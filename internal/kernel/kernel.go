// Package kernel implements the Cognitive Kernel — the central intelligence
// layer that coordinates all bounded contexts to process a user request.
//
// The Kernel is deterministic by default: strategy selection is rule-based,
// not LLM-based. Every decision emits a structured log and, when a Bus is
// configured, a typed event so subscribers can audit, persist, or replay
// the cognitive pipeline. Memory and enrichment failures degrade gracefully —
// runs always proceed.
package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/felipemarques-rec/sandcode/internal/architect"
	"github.com/felipemarques-rec/sandcode/internal/brain"
	"github.com/felipemarques-rec/sandcode/internal/event"
	sclog "github.com/felipemarques-rec/sandcode/internal/log"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/strategy"
)

// ErrNoPlanner is returned by ForcePlan when the Kernel was constructed
// without WithPlanner. Callers (e.g. the DAG executor invoked via
// `sandcode run --dag`) can branch on this to surface a helpful error.
var ErrNoPlanner = errors.New("kernel: planner not configured")

// Option configures a Kernel. Use the With* helpers; do not construct
// Option values directly.
type Option func(*Kernel)

// WithBus wires the Kernel to publish run.classified, brain.lesson_recalled,
// and run.enriched events for each Process() call. If nil, event emission
// is silently disabled (the Kernel is still fully functional).
func WithBus(bus event.Bus) Option {
	return func(k *Kernel) { k.bus = bus }
}

// WithPlanner wires an LLM-based (or other) planner that decomposes
// high-complexity prompts into a TaskDAG. When nil, the planner step
// is skipped entirely and ProcessResult.Plan is the zero value. The
// planner is invoked ONLY when the classifier returns
// brain.ComplexityHigh — simple prompts skip decomposition by design
// (see master plan §9.1).
func WithPlanner(p planner.Planner) Option {
	return func(k *Kernel) { k.planner = p }
}

// WithArchitect wires an Architect that designs solution guidance for
// divergent or high-complexity prompts. When nil, the architect step is
// skipped entirely and the working prompt equals the raw prompt
// (byte-identical). Failures degrade gracefully.
func WithArchitect(a architect.Architect) Option {
	return func(k *Kernel) { k.architect = a }
}

// WithSelector wires a strategy selector that picks the execution
// shape (single/refine/parallel) from classification + plan. When nil,
// the strategy step is skipped and ProcessResult.Strategy is empty —
// callers can apply their own logic or default to single-agent.
func WithSelector(s strategy.Selector) Option {
	return func(k *Kernel) { k.selector = s }
}

// Kernel coordinates cognition, memory recall, prompt enrichment,
// and strategy selection for each run.
type Kernel struct {
	brain      brain.Brain
	classifier *brain.Classifier
	enricher   *brain.Enricher
	planner    planner.Planner
	architect  architect.Architect
	selector   strategy.Selector
	bus        event.Bus
	tracer     Tracer
	logger     *slog.Logger
}

// New creates a Kernel. brain may be nil for runs without learning.
// Pass kernel.WithBus(bus) to enable event emission.
func New(b brain.Brain, opts ...Option) *Kernel {
	k := &Kernel{
		brain:      b,
		classifier: brain.NewClassifier(),
		tracer:     noopTracer{},
		logger:     slog.Default(),
	}
	if b != nil {
		k.enricher = brain.NewEnricher(b)
	}
	for _, opt := range opts {
		opt(k)
	}
	return k
}

// ProcessRequest holds the inputs for a kernel processing cycle.
type ProcessRequest struct {
	Prompt string
	CWD    string
	RunID  string
}

// ProcessResult holds the kernel's decisions.
type ProcessResult struct {
	EnrichedPrompt string
	Classification brain.Classification
	LessonsUsed    int

	// Plan is the decomposition of the prompt when the planner ran.
	// Empty (zero-value TaskDAG) when the planner is not configured,
	// when complexity is below the threshold, or when decomposition
	// failed (graceful degradation — run still proceeds).
	Plan planner.TaskDAG

	// Arch is the Architect's design when the architect ran (divergent or
	// high-complexity + WithArchitect configured). Nil otherwise.
	Arch *architect.ArchPlan

	// Strategy is the deterministic execution shape chosen from the
	// classification + plan. Empty string when no selector was
	// configured. Reason is the human-readable rule that fired.
	Strategy       strategy.Strategy
	StrategyReason string
}

// Process runs the cognitive pipeline: classify → recall → enrich.
// All steps degrade gracefully — errors produce warnings, not failures.
// When a Bus is configured, events are emitted at each stage.
func (k *Kernel) Process(ctx context.Context, req ProcessRequest) ProcessResult {
	ctx = sclog.WithCorrelation(ctx, req.RunID)
	logger := sclog.Logger(ctx)

	// 1. Classify
	cctx, endClassify := k.tracer.Start(ctx, "kernel.classify", nil)
	classification := k.classifier.Classify(cctx, req.Prompt)
	endClassify(nil)
	logger.Info("prompt classified",
		"type", string(classification.Type),
		"complexity", string(classification.Complexity),
	)
	k.publish(ctx, event.RunClassified, req.RunID, classificationPayload{
		Type:       string(classification.Type),
		Complexity: string(classification.Complexity),
		Reasoning:  classification.Reasoning,
	})

	// 1.5 Architect (opt-in; gated on divergent OR high). Designs guidance
	// that is prepended to the working prompt feeding plan + enrich + execute.
	// Failures degrade gracefully (warn + raw prompt). nil ⇒ byte-identical.
	workingPrompt := req.Prompt
	var arch *architect.ArchPlan
	if k.architect != nil &&
		(classification.Type == brain.Divergent || classification.Complexity == brain.ComplexityHigh) {
		actx, endArch := k.tracer.Start(ctx, "kernel.architect", nil)
		ap, err := k.architect.Design(actx, architect.DesignRequest{
			Prompt:      req.Prompt,
			ProblemType: string(classification.Type),
			Complexity:  string(classification.Complexity),
		})
		endArch(err)
		if err != nil {
			logger.Warn("architect design failed, proceeding without guidance", "error", err)
		} else {
			arch = &ap
			workingPrompt = formatArchGuidance(ap) + req.Prompt
			logger.Info("architecture designed",
				"files", len(ap.Files), "risks", len(ap.Risks))
			k.publish(ctx, event.RunArchitected, req.RunID, architectedPayload{
				RunID:       req.RunID,
				ApproachLen: len(ap.Approach),
				FilesCount:  len(ap.Files),
				RisksCount:  len(ap.Risks),
				Architect:   ap.Architect,
			})
		}
	}

	// 2. Plan (high-complexity decomposition into TaskDAG).
	// Gated on complexity to avoid the LLM round-trip for simple
	// prompts; failures degrade gracefully (warn + empty plan).
	var plan planner.TaskDAG
	if k.planner != nil && classification.Complexity == brain.ComplexityHigh {
		pctx, endPlan := k.tracer.Start(ctx, "kernel.plan", nil)
		dag, err := k.planner.Decompose(pctx, workingPrompt)
		endPlan(err)
		if err != nil {
			logger.Warn("planner decompose failed, proceeding without plan", "error", err)
		} else {
			plan = dag
			logger.Info("prompt decomposed",
				"nodes", len(plan.Nodes),
				"roots", len(plan.Roots()),
			)
			k.publish(ctx, event.RunPlanned, req.RunID, planPayload{
				NodeCount: len(plan.Nodes),
				RootCount: len(plan.Roots()),
			})
		}
	}

	// 3. Strategy select (deterministic, rule-based). Pure function
	// of classification + plan; no LLM in this hot path. The empty
	// Strategy is the conservative default when no selector is wired.
	var chosen strategy.Strategy
	var reason string
	if k.selector != nil {
		_, endStrat := k.tracer.Start(ctx, "kernel.strategy", nil)
		chosen, reason = k.selector.Select(classification, plan)
		endStrat(nil)
		logger.Info("strategy selected",
			"strategy", string(chosen),
			"reason", reason,
		)
		k.publish(ctx, event.RunStrategySelected, req.RunID, strategyPayload{
			Strategy: string(chosen),
			Reason:   reason,
		})
	}

	// 4. Enrich (includes recall + Grill with Docs)
	enrichedPrompt := req.Prompt
	lessonsUsed := 0
	if k.enricher != nil {
		// Recall first so we can emit a lesson_recalled event with the count
		// — the Enricher will recall again internally; this is cheap (SQL).
		if k.brain != nil {
			if lessons, err := k.brain.Recall(ctx, req.Prompt, 10); err == nil {
				lessonsUsed = len(lessons)
				if lessonsUsed > 0 {
					k.publish(ctx, event.BrainLessonRecalled, req.RunID, lessonRecallPayload{
						Count:     lessonsUsed,
						LessonIDs: lessonIDs(lessons),
					})
				}
			}
		}

		ectx, endEnrich := k.tracer.Start(ctx, "kernel.enrich", nil)
		ep, err := k.enricher.Enrich(ectx, workingPrompt, req.CWD)
		endEnrich(err)
		if err != nil {
			logger.Warn("enrichment failed, using raw prompt", "error", err)
		} else {
			enrichedPrompt = ep
			logger.Info("prompt enriched",
				"original_len", len(req.Prompt),
				"enriched_len", len(enrichedPrompt),
				"lessons_used", lessonsUsed,
			)
			k.publish(ctx, event.RunEnriched, req.RunID, enrichmentPayload{
				OriginalLen: len(req.Prompt),
				EnrichedLen: len(enrichedPrompt),
				LessonsUsed: lessonsUsed,
			})
		}
	}

	return ProcessResult{
		EnrichedPrompt: enrichedPrompt,
		Classification: classification,
		LessonsUsed:    lessonsUsed,
		Plan:           plan,
		Arch:           arch,
		Strategy:       chosen,
		StrategyReason: reason,
	}
}

// ForcePlan invokes the configured planner directly, bypassing the
// complexity gate that Process applies. Returns ErrNoPlanner when no
// planner has been configured. Used by the DAG executor (W12 Slice 4)
// when the user explicitly opts into multi-node execution via --dag —
// the gate that protects Process from billing LLM round-trips on
// trivial prompts is bypassed because the user has already declared
// intent.
func (k *Kernel) ForcePlan(ctx context.Context, prompt string) (planner.TaskDAG, error) {
	if k.planner == nil {
		return planner.TaskDAG{}, ErrNoPlanner
	}
	plan, err := k.planner.Decompose(ctx, prompt)
	if err != nil {
		return planner.TaskDAG{}, fmt.Errorf("kernel: force plan: %w", err)
	}
	return plan, nil
}

// Learn delegates to the brain's learning pipeline.
func (k *Kernel) Learn(ctx context.Context, outcome brain.Outcome) (int, error) {
	if k.brain == nil {
		return 0, nil
	}
	return k.brain.Learn(ctx, outcome)
}

// Stats returns brain statistics, or empty stats if no brain.
func (k *Kernel) Stats(ctx context.Context) (brain.Stats, error) {
	if k.brain == nil {
		return brain.Stats{}, nil
	}
	return k.brain.Stats(ctx)
}

// publish emits an event on the configured bus. No-op when bus is nil.
// Marshal failures are logged but never fail the cognitive pipeline.
func (k *Kernel) publish(ctx context.Context, typ event.Type, runID string, payload any) {
	if k.bus == nil {
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		sclog.Logger(ctx).Warn("kernel: event marshal failed",
			"error", err, "event_type", string(typ), "run_id", runID)
		raw = []byte("{}")
	}
	ev := event.New(typ, runID, raw).
		WithCorrelation(runID).
		WithTrace(k.tracer.TraceID(ctx))
	if err := k.bus.Publish(ctx, ev); err != nil {
		sclog.Logger(ctx).Warn("kernel: event publish failed",
			"error", err, "event_type", string(typ), "run_id", runID)
	}
}

// classificationPayload is the JSON shape of run.classified events.
type classificationPayload struct {
	Type       string `json:"type"`
	Complexity string `json:"complexity"`
	Reasoning  string `json:"reasoning,omitempty"`
}

// lessonRecallPayload is the JSON shape of brain.lesson_recalled events.
type lessonRecallPayload struct {
	Count     int      `json:"count"`
	LessonIDs []string `json:"lesson_ids,omitempty"`
}

// enrichmentPayload is the JSON shape of run.enriched events.
type enrichmentPayload struct {
	OriginalLen int `json:"original_len"`
	EnrichedLen int `json:"enriched_len"`
	LessonsUsed int `json:"lessons_used"`
}

// planPayload is the JSON shape of run.planned events. Carries only
// structural metadata — the full DAG lives on ProcessResult.Plan and
// can be persisted separately by callers that need the body.
type planPayload struct {
	NodeCount int `json:"node_count"`
	RootCount int `json:"root_count"`
}

// architectedPayload is the JSON shape of run.architected events. Carries
// only structural metadata — the full ArchPlan lives on ProcessResult.Arch.
type architectedPayload struct {
	RunID       string `json:"run_id"`
	ApproachLen int    `json:"approach_len"`
	FilesCount  int    `json:"files_count"`
	RisksCount  int    `json:"risks_count"`
	Architect   string `json:"architect"`
}

// strategyPayload is the JSON shape of run.strategy_selected events.
type strategyPayload struct {
	Strategy string `json:"strategy"`
	Reason   string `json:"reason"`
}

// formatArchGuidance renders an ArchPlan as a compact Markdown block that is
// prepended to the working prompt. Deterministic.
func formatArchGuidance(ap architect.ArchPlan) string {
	var b strings.Builder
	b.WriteString("## Architecture guidance\n\n")
	if ap.Approach != "" {
		b.WriteString("Approach: ")
		b.WriteString(ap.Approach)
		b.WriteString("\n\n")
	}
	if len(ap.Files) > 0 {
		b.WriteString("Likely files:\n")
		for _, f := range ap.Files {
			b.WriteString("- ")
			b.WriteString(f)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	if len(ap.Risks) > 0 {
		b.WriteString("Risks:\n")
		for _, r := range ap.Risks {
			b.WriteString("- ")
			b.WriteString(r)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}
	b.WriteString("---\n\n")
	return b.String()
}

func lessonIDs(ls []brain.Lesson) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}
