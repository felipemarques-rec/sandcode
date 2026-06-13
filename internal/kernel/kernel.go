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
	"github.com/felipemarques-rec/sandcode/internal/costopt"
	"github.com/felipemarques-rec/sandcode/internal/event"
	sclog "github.com/felipemarques-rec/sandcode/internal/log"
	"github.com/felipemarques-rec/sandcode/internal/planner"
	"github.com/felipemarques-rec/sandcode/internal/reactor"
	"github.com/felipemarques-rec/sandcode/internal/stepback"
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

// WithStepBack wires the Step-Back Reasoner. When nil (default) the step-back
// stage is skipped entirely and the working prompt is unchanged (byte-identical).
// Gated on divergent OR high; failures degrade gracefully. Runs after the
// Architect stage so both guidances stack.
func WithStepBack(s stepback.StepBack) Option {
	return func(k *Kernel) { k.stepback = s }
}

// WithSelector wires a strategy selector that picks the execution
// shape (single/refine/parallel) from classification + plan. When nil,
// the strategy step is skipped and ProcessResult.Strategy is empty —
// callers can apply their own logic or default to single-agent.
func WithSelector(s strategy.Selector) Option {
	return func(k *Kernel) { k.selector = s }
}

// WithModelRouter wires the Cost Optimizer's deterministic model router. When nil
// (default) the model-route stage is skipped and ProcessResult.Model is empty
// (byte-identical). Runs after strategy-select; observation-only — the routed model
// is applied by the orchestrator.
func WithModelRouter(r costopt.Router) Option {
	return func(k *Kernel) { k.router = r }
}

// WithReactive routes the classify stage through the deterministic reactor
// (SP3.0) instead of calling the classifier directly — the proof-of-concept
// for bus-mediated, event-driven coordination. Requires a Bus (WithBus); with
// no bus the flag is inert and the direct path runs. Off by default ⇒ the
// direct call path is byte-identical to legacy. SP3.1+ invert further stages.
func WithReactive() Option {
	return func(k *Kernel) { k.reactive = true }
}

// Kernel coordinates cognition, memory recall, prompt enrichment,
// and strategy selection for each run.
type Kernel struct {
	brain      brain.Brain
	classifier *brain.Classifier
	enricher   *brain.Enricher
	planner    planner.Planner
	architect  architect.Architect
	stepback   stepback.StepBack
	selector   strategy.Selector
	router     costopt.Router
	bus        event.Bus
	tracer     Tracer
	logger     *slog.Logger
	reactive   bool // SP3.0: route classify through the reactor
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

	// Model is the agent model chosen by the Cost Optimizer (E3.2) from the
	// classification. Empty when no router is configured — the orchestrator then
	// leaves AgentOpts.Model unchanged (byte-identical).
	Model string
}

// Process runs the cognitive pipeline: classify → architect → plan → strategy
// → enrich. All steps degrade gracefully — errors produce warnings, not
// failures. When a Bus is configured, events are emitted at each stage.
//
// With WithReactive (SP3.0/SP3.1) each stage runs through the deterministic
// reactor: the kernel sequences the stages and applies the gating (kernel-
// conducted), while each stage is a reactor handler that consumes its
// observation-only command and publishes its result. The default (direct) path
// is byte-identical — same result events, same order, same payloads.
func (k *Kernel) Process(ctx context.Context, req ProcessRequest) ProcessResult {
	ctx = sclog.WithCorrelation(ctx, req.RunID)
	logger := sclog.Logger(ctx)

	// 1. Classify (always runs).
	var classification brain.Classification
	k.stage(ctx, req.RunID, event.ClassifyRequested,
		classifyRequestedPayload{PromptLen: len(req.Prompt)},
		func(wctx context.Context) []event.Event {
			cctx, end := k.tracer.Start(wctx, "kernel.classify", nil)
			classification = k.classifier.Classify(cctx, req.Prompt)
			end(nil)
			return []event.Event{k.buildEvent(wctx, event.RunClassified, req.RunID, classificationPayload{
				Type:       string(classification.Type),
				Complexity: string(classification.Complexity),
				Reasoning:  classification.Reasoning,
			})}
		})
	logger.Info("prompt classified",
		"type", string(classification.Type), "complexity", string(classification.Complexity))

	// 1.5 Architect (gated on divergent OR high). Designs guidance prepended to
	// the working prompt feeding plan + enrich. Failures degrade gracefully.
	workingPrompt := req.Prompt
	var arch *architect.ArchPlan
	if k.architect != nil &&
		(classification.Type == brain.Divergent || classification.Complexity == brain.ComplexityHigh) {
		k.stage(ctx, req.RunID, event.ArchitectRequested,
			architectRequestedPayload{Complexity: string(classification.Complexity), ProblemType: string(classification.Type)},
			func(wctx context.Context) []event.Event {
				actx, end := k.tracer.Start(wctx, "kernel.architect", nil)
				ap, err := k.architect.Design(actx, architect.DesignRequest{
					Prompt:      req.Prompt,
					ProblemType: string(classification.Type),
					Complexity:  string(classification.Complexity),
				})
				end(err)
				if err != nil {
					logger.Warn("architect design failed, proceeding without guidance", "error", err)
					return nil
				}
				arch = &ap
				workingPrompt = formatArchGuidance(ap) + req.Prompt
				logger.Info("architecture designed", "files", len(ap.Files), "risks", len(ap.Risks))
				return []event.Event{k.buildEvent(wctx, event.RunArchitected, req.RunID, architectedPayload{
					RunID:       req.RunID,
					ApproachLen: len(ap.Approach),
					FilesCount:  len(ap.Files),
					RisksCount:  len(ap.Risks),
					Architect:   ap.Architect,
				})}
			})
	}

	// 1.6 Step-Back (gated on divergent OR high). Distills reframing principles
	// prepended to the working prompt feeding plan + enrich. Runs AFTER architect
	// so both guidances stack (architect reassigns workingPrompt from req.Prompt).
	if k.stepback != nil &&
		(classification.Type == brain.Divergent || classification.Complexity == brain.ComplexityHigh) {
		k.stage(ctx, req.RunID, event.StepBackRequested,
			stepBackRequestedPayload{Complexity: string(classification.Complexity), ProblemType: string(classification.Type)},
			func(wctx context.Context) []event.Event {
				sctx, end := k.tracer.Start(wctx, "kernel.stepback", nil)
				res, err := k.stepback.Reason(sctx, stepback.ReasonRequest{
					Prompt:      req.Prompt,
					ProblemType: string(classification.Type),
					Complexity:  string(classification.Complexity),
				})
				end(err)
				if err != nil || len(res.Principles) == 0 {
					if err != nil {
						logger.Warn("step-back failed, proceeding without principles", "error", err)
					}
					return nil
				}
				workingPrompt = formatStepBackGuidance(res) + workingPrompt
				logger.Info("stepped back", "principles", len(res.Principles))
				return []event.Event{k.buildEvent(wctx, event.RunSteppedBack, req.RunID, steppedBackPayload{
					RunID:          req.RunID,
					PrincipleCount: len(res.Principles),
					Reasoner:       res.Reasoner,
				})}
			})
	}

	// 2. Plan (gated on high complexity to avoid the LLM round-trip on simple
	// prompts). Failures degrade gracefully (warn + empty plan).
	var plan planner.TaskDAG
	if k.planner != nil && classification.Complexity == brain.ComplexityHigh {
		k.stage(ctx, req.RunID, event.PlanRequested,
			planRequestedPayload{PromptLen: len(workingPrompt)},
			func(wctx context.Context) []event.Event {
				pctx, end := k.tracer.Start(wctx, "kernel.plan", nil)
				dag, err := k.planner.Decompose(pctx, workingPrompt)
				end(err)
				if err != nil {
					logger.Warn("planner decompose failed, proceeding without plan", "error", err)
					return nil
				}
				plan = dag
				logger.Info("prompt decomposed", "nodes", len(plan.Nodes), "roots", len(plan.Roots()))
				return []event.Event{k.buildEvent(wctx, event.RunPlanned, req.RunID, planPayload{
					NodeCount: len(plan.Nodes), RootCount: len(plan.Roots()),
				})}
			})
	}

	// 3. Strategy select (deterministic, rule-based; when a selector is wired).
	var chosen strategy.Strategy
	var reason string
	if k.selector != nil {
		k.stage(ctx, req.RunID, event.StrategyRequested, strategyRequestedPayload{},
			func(wctx context.Context) []event.Event {
				_, end := k.tracer.Start(wctx, "kernel.strategy", nil)
				chosen, reason = k.selector.Select(classification, plan)
				end(nil)
				logger.Info("strategy selected", "strategy", string(chosen), "reason", reason)
				return []event.Event{k.buildEvent(wctx, event.RunStrategySelected, req.RunID, strategyPayload{
					Strategy: string(chosen), Reason: reason,
				})}
			})
	}

	// 3.5 Model route (Cost Optimizer; when a router is wired). Picks the agent
	// model from the classification. Observation-only — applied by the orchestrator
	// via ProcessResult.Model.
	var routedModel string
	if k.router != nil {
		// In direct mode the stage helper only publishes result events; emit the
		// observation-only command explicitly so bus subscribers see it on both
		// paths (the reactive stage already publishes it via Dispatch).
		if !k.reactive {
			k.publish(ctx, event.ModelRouteRequested, req.RunID,
				modelRouteRequestedPayload{Complexity: string(classification.Complexity), ProblemType: string(classification.Type)})
		}
		k.stage(ctx, req.RunID, event.ModelRouteRequested,
			modelRouteRequestedPayload{Complexity: string(classification.Complexity), ProblemType: string(classification.Type)},
			func(wctx context.Context) []event.Event {
				_, end := k.tracer.Start(wctx, "kernel.modelroute", nil)
				model, reason := k.router.Route(classification)
				end(nil)
				routedModel = model
				logger.Info("model routed", "model", model, "reason", reason)
				return []event.Event{k.buildEvent(wctx, event.RunModelRouted, req.RunID, modelRoutedPayload{
					Model: model, Reason: reason,
				})}
			})
	}

	// 4. Enrich (recall + Grill with Docs; when an enricher is wired). Recall
	// emits brain.lesson_recalled (observation-only, published directly so it
	// precedes enrich exactly as the legacy path does) before run.enriched.
	enrichedPrompt := req.Prompt
	lessonsUsed := 0
	if k.enricher != nil {
		k.stage(ctx, req.RunID, event.EnrichRequested,
			enrichRequestedPayload{PromptLen: len(workingPrompt)},
			func(wctx context.Context) []event.Event {
				if k.brain != nil {
					if lessons, err := k.brain.Recall(wctx, req.Prompt, 10); err == nil {
						lessonsUsed = len(lessons)
						if lessonsUsed > 0 {
							k.publish(wctx, event.BrainLessonRecalled, req.RunID, lessonRecallPayload{
								Count: lessonsUsed, LessonIDs: lessonIDs(lessons),
							})
						}
					}
				}
				ectx, end := k.tracer.Start(wctx, "kernel.enrich", nil)
				ep, err := k.enricher.Enrich(ectx, workingPrompt, req.CWD)
				end(err)
				if err != nil {
					logger.Warn("enrichment failed, using raw prompt", "error", err)
					return nil
				}
				enrichedPrompt = ep
				logger.Info("prompt enriched", "original_len", len(req.Prompt),
					"enriched_len", len(enrichedPrompt), "lessons_used", lessonsUsed)
				return []event.Event{k.buildEvent(wctx, event.RunEnriched, req.RunID, enrichmentPayload{
					OriginalLen: len(req.Prompt), EnrichedLen: len(enrichedPrompt), LessonsUsed: lessonsUsed,
				})}
			})
	}

	return ProcessResult{
		EnrichedPrompt: enrichedPrompt,
		Classification: classification,
		LessonsUsed:    lessonsUsed,
		Plan:           plan,
		Arch:           arch,
		Strategy:       chosen,
		StrategyReason: reason,
		Model:          routedModel,
	}
}

// stage runs one cognitive stage. work does the stage's computation (setting the
// caller's captured result variables) and returns the result event(s) to emit,
// in order. work ALWAYS runs (even with no bus) so the kernel's result is
// populated. Direct mode publishes each result event. Reactive mode
// (WithReactive + bus) publishes an observation-only command (cmdType/payload),
// runs work inside a per-call reactor handler, and lets the reactor publish the
// results — adding only the command event vs the direct path.
func (k *Kernel) stage(ctx context.Context, runID string, cmdType event.Type, cmdPayload any, work func(context.Context) []event.Event) {
	if k.reactive && k.bus != nil {
		r := reactor.New(k.bus)
		r.Register(cmdType, func(hctx context.Context, _ event.Event) ([]event.Event, error) {
			return work(hctx), nil
		})
		if _, err := r.Dispatch(ctx, runID, k.buildEvent(ctx, cmdType, runID, cmdPayload)); err != nil {
			sclog.Logger(ctx).Warn("reactor stage failed", "cmd", string(cmdType), "error", err)
		}
		return
	}
	for _, ev := range work(ctx) {
		if k.bus == nil {
			continue
		}
		if err := k.bus.Publish(ctx, ev); err != nil {
			sclog.Logger(ctx).Warn("kernel: event publish failed",
				"error", err, "event_type", string(ev.Type), "run_id", runID)
		}
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

// buildEvent constructs a fully-stamped event (correlation + trace) without
// publishing. Shared by publish (direct path) and the reactor handlers (SP3.0
// reactive path), so a reactor-produced event is byte-identical to one publish
// would emit. Marshal failures degrade to an empty payload, never panic.
func (k *Kernel) buildEvent(ctx context.Context, typ event.Type, runID string, payload any) event.Event {
	raw, err := json.Marshal(payload)
	if err != nil {
		sclog.Logger(ctx).Warn("kernel: event marshal failed",
			"error", err, "event_type", string(typ), "run_id", runID)
		raw = []byte("{}")
	}
	return event.New(typ, runID, raw).
		WithCorrelation(runID).
		WithTrace(k.tracer.TraceID(ctx))
}

// publish emits an event on the configured bus. No-op when bus is nil.
// Marshal failures are logged but never fail the cognitive pipeline.
func (k *Kernel) publish(ctx context.Context, typ event.Type, runID string, payload any) {
	if k.bus == nil {
		return
	}
	ev := k.buildEvent(ctx, typ, runID, payload)
	if err := k.bus.Publish(ctx, ev); err != nil {
		sclog.Logger(ctx).Warn("kernel: event publish failed",
			"error", err, "event_type", string(typ), "run_id", runID)
	}
}

// Reactor command payloads (SP3.0/SP3.1). They deliberately carry only metadata
// (lengths, classification labels) — never raw prompt content — so command
// events leak nothing to bus observers.
type classifyRequestedPayload struct {
	PromptLen int `json:"prompt_len"`
}

type architectRequestedPayload struct {
	Complexity  string `json:"complexity"`
	ProblemType string `json:"problem_type"`
}

type stepBackRequestedPayload struct {
	Complexity  string `json:"complexity"`
	ProblemType string `json:"problem_type"`
}

type planRequestedPayload struct {
	PromptLen int `json:"prompt_len"`
}

type strategyRequestedPayload struct{}

type modelRouteRequestedPayload struct {
	Complexity  string `json:"complexity"`
	ProblemType string `json:"problem_type"`
}

// modelRoutedPayload is the JSON shape of run.model_routed events.
type modelRoutedPayload struct {
	Model  string `json:"model"`
	Reason string `json:"reason"`
}

type enrichRequestedPayload struct {
	PromptLen int `json:"prompt_len"`
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

// steppedBackPayload is the JSON shape of run.stepped_back events. Carries only
// structural metadata — the principles themselves are folded into the working prompt.
type steppedBackPayload struct {
	RunID          string `json:"run_id"`
	PrincipleCount int    `json:"principle_count"`
	Reasoner       string `json:"reasoner"`
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

// formatStepBackGuidance renders step-back principles as a compact Markdown block
// prepended to the working prompt. Deterministic.
func formatStepBackGuidance(res stepback.Result) string {
	var b strings.Builder
	b.WriteString("## Step-back principles\n\n")
	for _, p := range res.Principles {
		b.WriteString("- ")
		b.WriteString(p)
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n")
	return b.String()
}

func lessonIDs(ls []brain.Lesson) []string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, l.ID)
	}
	return out
}
