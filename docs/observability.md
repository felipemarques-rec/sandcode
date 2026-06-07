# Observability — Langfuse Cognitive Trace

Sandcode emits one connected OpenTelemetry trace per run to Langfuse,
covering the cognitive pipeline and the dispatched execution (single,
refine, parallel, or DAG).

## Enable

Set these environment variables before `sandcode run --learn` or
`sandcode serve` (auto-detected; disabled by default — zero overhead
and byte-identical behaviour when unset):

| Variable | Default | Meaning |
|---|---|---|
| `LANGFUSE_ENABLED` | `false` | Set `true` to turn tracing on |
| `LANGFUSE_HOST` | `https://cloud.langfuse.com` | Langfuse endpoint |
| `LANGFUSE_PUBLIC_KEY` | — | Public API key |
| `LANGFUSE_SECRET_KEY` | — | Secret API key |
| `SANDCODE_ENV` | `development` | `deployment.environment` resource attr |

Tracing only attaches a root span when the kernel is active
(`sandcode run --learn`, or `sandcode serve` which always builds the
kernel). Without the kernel the run executes untraced (by design).

## Trace shape

```
sandcode.brain.execute                     (root; sandcode.run_id, dispatch.kind/reason, strategy)
├── kernel.classify
├── kernel.plan          (only when complexity=high + planner configured)
├── kernel.strategy      (only when a selector is configured)
├── kernel.enrich        (only when a brain is configured)
├── sandcode.brain.dispatch                (dispatch decision; near-zero duration)
└── one of (the dispatched execution):
    ├── sandcode.brain.run                              (Single / Refine)
    │
    ├── sandcode.brain.run × N                          (Parallel: one per arm)
    │   └── sandcode.judge.ranking                      (when a judge is configured)
    │
    └── sandcode.brain.dag                              (DAG; run_id, dag.nodes/roots/chains)
        ├── sandcode.brain.dag.chain × C                (one per root chain)
        ├── sandcode.judge.ranking                      (multi-root only; real evaluation.score)
        └── sandcode.brain.dag.synthesizer              (multi-root + synthesizer enabled)
```

Degenerate single-node DAG: no `sandcode.brain.dag` grouping span — it
delegates to `Run`, whose `sandcode.brain.run` span is the container
(adding a dag span would misrepresent the execution model).

Every `event.Event` on the bus carries `trace_id` (empty when tracing
is disabled) so bus events correlate back to the Langfuse trace.

## Scheduler (queued phase) events

When the optional in-process scheduler is enabled (`sandcode serve
--max-concurrent-runs N --queue-capacity M`), an admitted run emits two
observation-only bus events **before** `orchestrator.Execute` starts:
`run.scheduled` (payload: `priority`, 0-based `queue_position`) on
enqueue, then `run.dequeued` (payload: `wait_ms`) when a pool slot frees.
Neither changes run phase (the run stays `submitted`, same precedent as
`run.strategy_selected`). These events carry an **empty `trace_id`** by
construction: the per-run Langfuse trace is created inside
`orchestrator.Execute`, which has not started while the run is queued —
so the queued→running gap is observed via **`run_id` correlation on the
SSE stream** (`GET /v1/runs/{id}/events`), not via Langfuse.

Operator note: a queued run is visible on `/v1/runs/{id}/events`
(streaming `run.scheduled`) **before** it appears in `GET /v1/runs/{id}`
or the run list — the run snapshot/StateCache is populated by
orchestrator events, which only fire once the run is dequeued and
`Execute` runs. A `404` on `GET /v1/runs/{id}` for a freshly-accepted
run means "still queued", not "unknown"; the event stream is
authoritative during the queued phase.

## Reporter events

### `report.generated` (observation-only)

Emitted after a Reporter produces a run report (e.g. `sandcode run --report`).
Like the scheduler events, it carries **no `trace_id`** by construction —
correlate it to a run via `run_id` on the SSE stream. Payload:
`{ run_id, path, bytes, status }` (`path` empty when content-only). It does
NOT trigger a state-machine transition (observation-only).

### `review.generated` (observation-only)

Emitted by `Coordinate` after an opt-in LLM reviewer (`sandcode run --review`)
scores the run's diff against the prompt. Like `report.generated` it carries
**no `trace_id`** — correlate via `run_id`. Payload: `{ run_id, score, reviewer }`
(`score` in `[0,1]`, `reviewer` e.g. `llm:claude-haiku-4-5-20251001`). The review
is purely observational: it feeds the REPORT.md `## Review` section but **never**
changes run status, and absence of `--review` is byte-identical to before. It does
NOT trigger a state-machine transition.

### `run.architected` (observation-only)

Emitted by the kernel after an opt-in Architect (`sandcode run --learn --architect`)
designs solution guidance for a divergent or high-complexity prompt. Payload:
`{ run_id, approach_len, files_count, risks_count, architect }` (structural metadata;
the full ArchPlan is on `ProcessResult.Arch`). The guidance is injected into the prompt
that feeds the planner/enricher/implementer, so unlike review it *does* shape execution —
but only when opted in; absence of `--architect` is byte-identical. Correlate via
`run_id`. It does NOT trigger a state-machine transition.

### `security.reviewed` (observation-only)

Emitted by `Coordinate` after an opt-in Security Reviewer (`sandcode run --security-review` for the
deterministic secret scan, or `--security-review-llm` for LLM vuln+secret review) scans the run's diff.
Payload: `{ run_id, findings_count, high_count, reviewer }` (`reviewer` is `deterministic:secrets` or
`llm:<model>`). The findings feed the REPORT.md `## Security` section; the review is purely
observational and **never** changes run status. Absence of the flags is byte-identical. Correlate via
`run_id`. It does NOT trigger a state-machine transition.

### `perf.reviewed` / `refactor.reviewed` (observation-only)

Emitted by `Coordinate` after an opt-in advisory lens scores the run's diff: `sandcode run --perf-review`
(Performance Reviewer) and `--refactor-review` (Refactoring Specialist). Both reuse the
`{ run_id, score, reviewer }` payload — the event **type** distinguishes the lens. Each feeds its own
REPORT.md section (`## Performance` / `## Refactoring`); both are purely observational and **never**
change run status. Absence of the flags is byte-identical. Correlate via `run_id`. Neither triggers a
state-machine transition.

## Known fidelity limitations (deferred, tracked)

None currently. The two previously-tracked items were closed in W13.3:
the DAG `sandcode.judge.ranking` span now carries the real
`evaluation.score` (`runJudgeOverChains` exposes `Ranking.Scores`), and
the DAG synthesizer pass is now its own `sandcode.brain.dag.synthesizer`
span nested under `sandcode.brain.dag`.

## LLM auth (subscription vs. API key)

The Go-side LLM features behind these events (`review.generated`,
`run.architected`, `security.reviewed`, `perf.reviewed`, `refactor.reviewed`,
and the `--judge=llm` ranking) pick their transport via `--llm-auth`:

- `api-key` — direct Anthropic Messages API with `ANTHROPIC_API_KEY`.
- `subscription` — routes through the `claude` CLI (`internal/llmcli`), reusing the
  host's Claude subscription; no `ANTHROPIC_API_KEY` needed.
- `auto` (default) — `api-key` when the key is set, else `subscription` when the
  `claude` CLI is on PATH.

The transport choice does not change any event shape — the same observation-only
events are emitted either way.

## Notes

- Disabled is the default and is byte-identical to no tracing (no root
  span, no allocations on the hot path, `trace_id` omitted from events).
- The `internal/kernel` package depends on neither OpenTelemetry nor
  `internal/langfuse`; tracing is injected via the `kernel.Tracer` seam.
- Span lifetime contract: spans ended inside goroutines (run, dag, the
  per-arm/per-chain children) end **before** the channel/`resultCh`
  signal the caller waits on, so a caller that drains events then awaits
  the result is guaranteed the spans are already exported. Do not
  convert these explicit `End()` calls to deferred ones in goroutine
  bodies (see the W13 happens-before lesson in the session handoff).
