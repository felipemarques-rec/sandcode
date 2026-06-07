package metrics

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
)

// Registry collects metric families and renders them in Prometheus
// text format. The zero value is unusable; construct with NewRegistry.
type Registry struct {
	mu      sync.Mutex
	metrics map[string]collector
}

// NewRegistry constructs an empty Registry.
func NewRegistry() *Registry {
	return &Registry{metrics: make(map[string]collector)}
}

// Register adds m to the registry. Returns an error if a metric with
// the same name is already registered.
func (r *Registry) Register(m collector) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.metrics[m.name()]; dup {
		return fmt.Errorf("metrics: %q already registered", m.name())
	}
	r.metrics[m.name()] = m
	return nil
}

// MustRegister adds m and panics if registration fails. Intended for
// program-init metric declarations where collision indicates a bug.
func (r *Registry) MustRegister(m collector) {
	if err := r.Register(m); err != nil {
		panic(err)
	}
}

// NewCounter is a convenience: construct a Counter and register it.
func (r *Registry) NewCounter(name, help string, labels []string) *Counter {
	c := NewCounter(name, help, labels)
	r.MustRegister(c)
	return c
}

// NewGauge is a convenience: construct a Gauge and register it.
func (r *Registry) NewGauge(name, help string, labels []string) *Gauge {
	g := NewGauge(name, help, labels)
	r.MustRegister(g)
	return g
}

// NewHistogram is a convenience: construct a Histogram and register it.
func (r *Registry) NewHistogram(name, help string, labels []string, buckets []float64) *Histogram {
	h := NewHistogram(name, help, labels, buckets)
	r.MustRegister(h)
	return h
}

// NewGaugeFunc is a convenience: construct a GaugeFunc and register it.
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) *GaugeFunc {
	g := NewGaugeFunc(name, help, fn)
	r.MustRegister(g)
	return g
}

// Render emits every registered metric family in Prometheus text
// format (v0.0.4). Families are emitted in name-sorted order so output
// is byte-stable for tests.
//
// Each family produces:
//
//	# HELP <name> <help>
//	# TYPE <name> <type>
//	<sample lines…>
func (r *Registry) Render(w io.Writer) error {
	r.mu.Lock()
	names := make([]string, 0, len(r.metrics))
	for n := range r.metrics {
		names = append(names, n)
	}
	families := make([]collector, 0, len(r.metrics))
	for _, n := range sortStrings(names) {
		families = append(families, r.metrics[n])
	}
	r.mu.Unlock()

	for _, m := range families {
		if _, err := fmt.Fprintf(w, "# HELP %s %s\n", m.name(), escapeHelp(m.help())); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", m.name(), m.typeName()); err != nil {
			return err
		}
		if err := m.write(w); err != nil {
			return err
		}
	}
	return nil
}

// Handler returns an http.Handler that serves the registry in text
// format on GET. Use as `mux.Handle("/metrics", reg.Handler())`.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_ = r.Render(w)
	})
}

func sortStrings(s []string) []string {
	sort.Strings(s)
	return s
}
