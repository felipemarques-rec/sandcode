// Package metrics provides a small, pure-Go Prometheus-compatible
// metrics layer. It intentionally avoids prometheus/client_golang to
// keep the dependency surface minimal (see sandcode_no_deps_stance):
// the production scope here is a handful of counters, gauges, and
// histograms — well under the threshold where the upstream library
// would justify its size.
//
// Concepts:
//
//   - Counter: monotonically increasing float64 (Inc, Add).
//   - Gauge: bidirectional float64 (Set, Inc, Dec, Add).
//   - Histogram: fixed-bucket distribution (Observe).
//
// Each metric is a family parameterised by a fixed, ordered list of
// label names. WithLabels(values…) returns a child handle for the
// specific label tuple. Children are created lazily on first access
// and are safe for concurrent use.
package metrics

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"sync"
)

// collector is the internal interface every metric family implements
// so the Registry can render them uniformly.
type collector interface {
	name() string
	help() string
	typeName() string
	write(w io.Writer) error
}

// ---------------------------------------------------------------------
// Counter
// ---------------------------------------------------------------------

// Counter is a monotonically increasing float64 metric family.
type Counter struct {
	nm, hp   string
	labels   []string
	mu       sync.Mutex
	children map[string]*counterChild
}

type counterChild struct {
	values []string
	mu     sync.Mutex
	v      float64
}

// NewCounter constructs a Counter family with the given label names.
// labels may be nil for a counter without dimensions.
func NewCounter(name, help string, labels []string) *Counter {
	return &Counter{
		nm:       name,
		hp:       help,
		labels:   append([]string(nil), labels...),
		children: make(map[string]*counterChild),
	}
}

// With returns the child for the given label values, creating it on
// first access. Panics if len(values) != len(labels).
func (c *Counter) With(values ...string) *counterChild {
	if len(values) != len(c.labels) {
		panic(fmt.Sprintf("metrics: counter %q expects %d labels, got %d",
			c.nm, len(c.labels), len(values)))
	}
	key := labelKey(values)
	c.mu.Lock()
	defer c.mu.Unlock()
	ch, ok := c.children[key]
	if !ok {
		ch = &counterChild{values: append([]string(nil), values...)}
		c.children[key] = ch
	}
	return ch
}

// Inc adds 1 to the counter child identified by values. Convenience
// wrapper for hot paths that don't want to retain the child handle.
func (c *Counter) Inc(values ...string) { c.With(values...).Inc() }

// Add adds delta (must be >= 0) to the counter child identified by
// values. Negative deltas are silently dropped: counters must never
// go backwards.
func (c *Counter) Add(delta float64, values ...string) {
	c.With(values...).Add(delta)
}

func (cc *counterChild) Inc() { cc.Add(1) }

func (cc *counterChild) Add(delta float64) {
	if delta < 0 || math.IsNaN(delta) {
		return
	}
	cc.mu.Lock()
	cc.v += delta
	cc.mu.Unlock()
}

// Value returns the current value (test helper).
func (cc *counterChild) Value() float64 {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.v
}

func (c *Counter) name() string     { return c.nm }
func (c *Counter) help() string     { return c.hp }
func (c *Counter) typeName() string { return "counter" }

func (c *Counter) write(w io.Writer) error {
	c.mu.Lock()
	children := make([]*counterChild, 0, len(c.children))
	for _, ch := range c.children {
		children = append(children, ch)
	}
	c.mu.Unlock()
	sortChildrenByLabels(children)
	for _, ch := range children {
		ch.mu.Lock()
		v := ch.v
		ch.mu.Unlock()
		if err := writeSample(w, c.nm, "", c.labels, ch.values, nil, v); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Gauge
// ---------------------------------------------------------------------

// Gauge is a bidirectional float64 metric family.
type Gauge struct {
	nm, hp   string
	labels   []string
	mu       sync.Mutex
	children map[string]*gaugeChild
}

type gaugeChild struct {
	values []string
	mu     sync.Mutex
	v      float64
}

// NewGauge constructs a Gauge family with the given label names.
func NewGauge(name, help string, labels []string) *Gauge {
	return &Gauge{
		nm:       name,
		hp:       help,
		labels:   append([]string(nil), labels...),
		children: make(map[string]*gaugeChild),
	}
}

// With returns the child for the given label values.
func (g *Gauge) With(values ...string) *gaugeChild {
	if len(values) != len(g.labels) {
		panic(fmt.Sprintf("metrics: gauge %q expects %d labels, got %d",
			g.nm, len(g.labels), len(values)))
	}
	key := labelKey(values)
	g.mu.Lock()
	defer g.mu.Unlock()
	ch, ok := g.children[key]
	if !ok {
		ch = &gaugeChild{values: append([]string(nil), values...)}
		g.children[key] = ch
	}
	return ch
}

func (g *Gauge) Set(v float64, values ...string) { g.With(values...).Set(v) }
func (g *Gauge) Inc(values ...string)            { g.With(values...).Add(1) }
func (g *Gauge) Dec(values ...string)            { g.With(values...).Add(-1) }
func (g *Gauge) Add(delta float64, values ...string) {
	g.With(values...).Add(delta)
}

func (gc *gaugeChild) Set(v float64) {
	if math.IsNaN(v) {
		return
	}
	gc.mu.Lock()
	gc.v = v
	gc.mu.Unlock()
}

func (gc *gaugeChild) Add(delta float64) {
	if math.IsNaN(delta) {
		return
	}
	gc.mu.Lock()
	gc.v += delta
	gc.mu.Unlock()
}

// Value returns the current value (test helper).
func (gc *gaugeChild) Value() float64 {
	gc.mu.Lock()
	defer gc.mu.Unlock()
	return gc.v
}

func (g *Gauge) name() string     { return g.nm }
func (g *Gauge) help() string     { return g.hp }
func (g *Gauge) typeName() string { return "gauge" }

func (g *Gauge) write(w io.Writer) error {
	g.mu.Lock()
	children := make([]*gaugeChild, 0, len(g.children))
	for _, ch := range g.children {
		children = append(children, ch)
	}
	g.mu.Unlock()
	sortChildrenByLabels(children)
	for _, ch := range children {
		ch.mu.Lock()
		v := ch.v
		ch.mu.Unlock()
		if err := writeSample(w, g.nm, "", g.labels, ch.values, nil, v); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// GaugeFunc — callback gauge whose value is computed at scrape time
// ---------------------------------------------------------------------

// GaugeFunc is a label-less gauge whose value is produced by a caller-
// supplied function at scrape time. Use it for "size of a live
// collection" metrics where pushing on every mutation is awkward
// (e.g. an in-memory map that's mutated from many goroutines).
//
// GaugeFunc takes no labels — adding labels would require a different
// fn signature per child and is intentionally out of scope here.
type GaugeFunc struct {
	nm, hp string
	fn     func() float64
}

// NewGaugeFunc constructs a callback gauge. fn is invoked on every
// Registry render; it MUST be safe for concurrent calls and MUST NOT
// block (a hung fn would stall /metrics scrapes).
func NewGaugeFunc(name, help string, fn func() float64) *GaugeFunc {
	if fn == nil {
		fn = func() float64 { return 0 }
	}
	return &GaugeFunc{nm: name, hp: help, fn: fn}
}

func (g *GaugeFunc) name() string     { return g.nm }
func (g *GaugeFunc) help() string     { return g.hp }
func (g *GaugeFunc) typeName() string { return "gauge" }

func (g *GaugeFunc) write(w io.Writer) error {
	v := g.fn()
	if math.IsNaN(v) {
		v = 0
	}
	return writeSample(w, g.nm, "", nil, nil, nil, v)
}

// ---------------------------------------------------------------------
// Histogram
// ---------------------------------------------------------------------

// DefaultBuckets matches Prometheus' default histogram buckets and
// works well for short LLM/agent latencies in seconds.
var DefaultBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

// Histogram is a fixed-bucket cumulative distribution family.
type Histogram struct {
	nm, hp   string
	labels   []string
	buckets  []float64 // strictly increasing upper bounds, no +Inf entry
	mu       sync.Mutex
	children map[string]*histogramChild
}

type histogramChild struct {
	values []string
	mu     sync.Mutex
	counts []uint64 // per-bucket flat counts; +Inf bucket appended at write time
	sum    float64
	count  uint64
}

// NewHistogram constructs a Histogram family. buckets must be strictly
// increasing and finite; the implicit +Inf bucket is appended on emit.
// If buckets is nil, DefaultBuckets is used.
func NewHistogram(name, help string, labels []string, buckets []float64) *Histogram {
	if buckets == nil {
		buckets = DefaultBuckets
	}
	for i := 1; i < len(buckets); i++ {
		if buckets[i] <= buckets[i-1] {
			panic(fmt.Sprintf("metrics: histogram %q buckets must be strictly increasing", name))
		}
	}
	for _, b := range buckets {
		if math.IsInf(b, 0) || math.IsNaN(b) {
			panic(fmt.Sprintf("metrics: histogram %q buckets must be finite", name))
		}
	}
	return &Histogram{
		nm:       name,
		hp:       help,
		labels:   append([]string(nil), labels...),
		buckets:  append([]float64(nil), buckets...),
		children: make(map[string]*histogramChild),
	}
}

// With returns the child for the given label values.
func (h *Histogram) With(values ...string) *histogramChild {
	if len(values) != len(h.labels) {
		panic(fmt.Sprintf("metrics: histogram %q expects %d labels, got %d",
			h.nm, len(h.labels), len(values)))
	}
	key := labelKey(values)
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.children[key]
	if !ok {
		ch = &histogramChild{
			values: append([]string(nil), values...),
			counts: make([]uint64, len(h.buckets)),
		}
		h.children[key] = ch
	}
	return ch
}

// Observe records v in the child identified by values.
func (h *Histogram) Observe(v float64, values ...string) {
	h.With(values...).Observe(v, h.buckets)
}

func (hc *histogramChild) Observe(v float64, buckets []float64) {
	if math.IsNaN(v) {
		return
	}
	hc.mu.Lock()
	hc.sum += v
	hc.count++
	// Find smallest bucket whose upper bound >= v. sort.SearchFloat64s
	// returns the leftmost index i where buckets[i] >= v.
	i := sort.SearchFloat64s(buckets, v)
	if i < len(hc.counts) {
		hc.counts[i]++
	}
	// Values exceeding the largest finite bucket go only into +Inf,
	// which is computed as `count` at write time — no slot to bump.
	hc.mu.Unlock()
}

// Snapshot returns (cumulativeBucketCounts, sum, count) for tests.
// cumulativeBucketCounts has len(buckets)+1, with the last entry equal
// to count (the +Inf bucket).
func (hc *histogramChild) Snapshot(buckets []float64) ([]uint64, float64, uint64) {
	hc.mu.Lock()
	defer hc.mu.Unlock()
	out := make([]uint64, len(buckets)+1)
	var acc uint64
	for i := range buckets {
		acc += hc.counts[i]
		out[i] = acc
	}
	out[len(buckets)] = hc.count
	return out, hc.sum, hc.count
}

func (h *Histogram) name() string     { return h.nm }
func (h *Histogram) help() string     { return h.hp }
func (h *Histogram) typeName() string { return "histogram" }

func (h *Histogram) write(w io.Writer) error {
	h.mu.Lock()
	children := make([]*histogramChild, 0, len(h.children))
	for _, ch := range h.children {
		children = append(children, ch)
	}
	h.mu.Unlock()
	sortChildrenByLabels(children)

	for _, ch := range children {
		cum, sum, count := ch.Snapshot(h.buckets)
		// Bucket samples: one line per upper bound + +Inf.
		for i, ub := range h.buckets {
			extra := []labelPair{{"le", formatFloat(ub)}}
			if err := writeSample(w, h.nm, "_bucket", h.labels, ch.values, extra, float64(cum[i])); err != nil {
				return err
			}
		}
		extra := []labelPair{{"le", "+Inf"}}
		if err := writeSample(w, h.nm, "_bucket", h.labels, ch.values, extra, float64(count)); err != nil {
			return err
		}
		// Sum and count.
		if err := writeSample(w, h.nm, "_sum", h.labels, ch.values, nil, sum); err != nil {
			return err
		}
		if err := writeSample(w, h.nm, "_count", h.labels, ch.values, nil, float64(count)); err != nil {
			return err
		}
	}
	return nil
}

// ---------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------

type labelPair struct{ Name, Value string }

// labelKey joins label values with a sentinel separator that cannot
// appear in normal label values, producing a stable map key.
func labelKey(values []string) string {
	return strings.Join(values, "\x00")
}

// sortChildrenByLabels sorts children lexicographically by their label
// tuple so test golden output is stable.
func sortChildrenByLabels(children any) {
	switch s := children.(type) {
	case []*counterChild:
		sort.SliceStable(s, func(i, j int) bool { return lessLabels(s[i].values, s[j].values) })
	case []*gaugeChild:
		sort.SliceStable(s, func(i, j int) bool { return lessLabels(s[i].values, s[j].values) })
	case []*histogramChild:
		sort.SliceStable(s, func(i, j int) bool { return lessLabels(s[i].values, s[j].values) })
	}
}

func lessLabels(a, b []string) bool {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// writeSample writes one Prometheus text-format sample line:
//
//	<name><suffix>{<labels>} <value>\n
//
// labels and values are zipped; extra is appended after the family
// labels (used for histogram "le").
func writeSample(w io.Writer, name, suffix string, labelNames, labelValues []string, extra []labelPair, value float64) error {
	var sb strings.Builder
	sb.WriteString(name)
	sb.WriteString(suffix)
	if len(labelNames) > 0 || len(extra) > 0 {
		sb.WriteByte('{')
		first := true
		for i, ln := range labelNames {
			if !first {
				sb.WriteByte(',')
			}
			sb.WriteString(ln)
			sb.WriteString(`="`)
			sb.WriteString(escapeLabel(labelValues[i]))
			sb.WriteByte('"')
			first = false
		}
		for _, lp := range extra {
			if !first {
				sb.WriteByte(',')
			}
			sb.WriteString(lp.Name)
			sb.WriteString(`="`)
			sb.WriteString(escapeLabel(lp.Value))
			sb.WriteByte('"')
			first = false
		}
		sb.WriteByte('}')
	}
	sb.WriteByte(' ')
	sb.WriteString(formatFloat(value))
	sb.WriteByte('\n')
	_, err := io.WriteString(w, sb.String())
	return err
}

// escapeLabel escapes a label value per the Prometheus text format:
// \ → \\, " → \", newline → \n.
func escapeLabel(v string) string {
	if !strings.ContainsAny(v, `\"`+"\n") {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v) + 4)
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '"':
			sb.WriteString(`\"`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// escapeHelp escapes a HELP string per the Prometheus text format:
// \ → \\, newline → \n. Quotes are NOT escaped (HELP is not quoted).
func escapeHelp(v string) string {
	if !strings.ContainsAny(v, "\\\n") {
		return v
	}
	var sb strings.Builder
	sb.Grow(len(v) + 4)
	for _, r := range v {
		switch r {
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		default:
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

// formatFloat formats v in the canonical Prometheus way: integers stay
// integral, otherwise use %g for compactness; ±Inf/NaN are spelled out.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return fmt.Sprintf("%d", int64(v))
	}
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%.6f", v), "0"), ".")
}
