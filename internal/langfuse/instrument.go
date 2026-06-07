package langfuse

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
)

// InstrumentJudge creates a traced evaluation span around a judge ranking call.
// Usage:
//
//	ctx, finish := lf.InstrumentJudge(ctx, "judge_ranking", runID)
//	ranking, err := judge.Rank(ctx, ...)
//	finish(ranking.Scores[ranking.Winner], ranking.Rationale, err)
func (p *Provider) InstrumentJudge(ctx context.Context, evalName, runID string) (context.Context, func(score float64, comment string, err error)) {
	if !p.Enabled() {
		return ctx, func(float64, string, error) {} // no-op
	}

	ctx, span := p.SpanEvaluation(ctx, "sandcode.judge."+evalName, EvaluationOpts{
		Name:  evalName,
		RunID: runID,
	})

	return ctx, func(score float64, comment string, err error) {
		if err != nil {
			span.SetAttributes(attribute.String("error", err.Error()))
		}
		EndEvaluation(span, score, comment)

		// Also send the score to the REST API so it natively populates in Langfuse
		if err == nil {
			p.SubmitScore(span.SpanContext().TraceID().String(), evalName, score, comment)
		}
	}
}

// InstrumentLLMCall creates a traced generation span for any LLM API call.
// Usage:
//
//	ctx, finish := lf.InstrumentLLMCall(ctx, "brain.enrich", "anthropic", "claude-sonnet-4-20250514")
//	result, err := callLLM(ctx, ...)
//	finish(langfuse.GenerationResult{Model: "claude-sonnet-4-20250514", InputTokens: 1200, OutputTokens: 800})
func (p *Provider) InstrumentLLMCall(ctx context.Context, name, system, model string) (context.Context, func(GenerationResult)) {
	if !p.Enabled() {
		return ctx, func(GenerationResult) {} // no-op
	}

	ctx, span := p.SpanGeneration(ctx, "sandcode.llm."+name, GenerationOpts{
		System: system,
		Model:  model,
	})

	return ctx, func(result GenerationResult) {
		EndGeneration(span, result)
	}
}

// InstrumentBrainOp creates a traced span for brain operations (recall, enrich, learn).
// Usage:
//
//	ctx, finish := lf.InstrumentBrainOp(ctx, "recall", runID, 10)
//	lessons, err := brain.Recall(ctx, prompt, 10)
//	finish(len(lessons), err)
func (p *Provider) InstrumentBrainOp(ctx context.Context, operation, runID string, extraAttrs ...attribute.KeyValue) (context.Context, func(resultCount int, err error)) {
	if !p.Enabled() {
		return ctx, func(int, error) {} // no-op
	}

	attrs := append([]attribute.KeyValue{
		attribute.String("sandcode.run_id", runID),
	}, extraAttrs...)

	ctx, span := p.SpanBrain(ctx, operation, attrs...)

	return ctx, func(resultCount int, err error) {
		span.SetAttributes(attribute.Int("result_count", resultCount))
		if err != nil {
			span.SetAttributes(attribute.String("error", err.Error()))
		}
		span.End()
	}
}
