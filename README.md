# sandcode

**A Go orchestrator for coding agents (Claude Code, Codex, Cursor) running in Docker/Podman sandboxes.**

Inspired by [mattpocock/sandcastle](https://github.com/mattpocock/sandcastle), rewritten in Go with native parallelism, SQLite persistence, an LLM judge, and a single static binary — no Node dependency in CI.

## Features

- **Multiple agents** side by side: Claude Code, Codex, Cursor (extensible).
- **Sandboxes** on Docker and Podman, with an isolated git-worktree bind-mount.
- **Flexible auth**: bind-mount `~/.claude` (uses your Pro/Max subscription) or `ANTHROPIC_API_KEY`.
- **Parallel execution** with errgroup + semaphore (`--parallel N`, `--max-concurrency`).
- **LLM judge** (Claude Haiku 4.5 by default, prompt caching) picks the winner among N parallel runs.
- **Git worktrees** with `merge-to-head` and `branch` strategies. Per-repo locks avoid races on concurrent creation.
- **SQLite persistence** (pure-Go, no CGO) — `sandcode list/show/logs` reproduce any run.
- **Secret redaction** (provider API keys, private keys, JWTs, credential URLs, `key=value`) before anything is logged, persisted, or sent to an LLM.
- **LLM features via your subscription** — `--llm-auth subscription` routes the judge/reviewer/architect/planner through the `claude` CLI, so no `ANTHROPIC_API_KEY` is required.
- **Runtime limits**: CPU, memory, PIDs, wall-clock timeout, network mode.

## Architecture

```
┌──────────────────────────────────────────────────────────────┐
│                    cmd/sandcode (CLI)                        │
│   init • run [--parallel --judge] • list • show • logs       │
└────────────────────┬─────────────────────────────────────────┘
                     │
         ┌───────────▼────────────┐
         │   orchestrator         │  Run() / ParallelRun()
         │   (errgroup, ctx,      │  ─ stream events
         │    timeout, redact)    │  ─ apply judge → merge winner
         └─┬───┬────┬───────┬─────┘
           │   │    │       │
   ┌───────▼┐  │  ┌─▼────┐ ┌▼──────┐
   │ agent  │  │  │ git  │ │ judge │
   │ Claude │  │  │ work │ │  LLM  │
   │ Codex  │  │  │ tree │ │(Haiku)│
   │ Cursor │  │  └──────┘ └───────┘
   └───┬────┘  │
       │   ┌───▼─────┐
       │   │  auth   │  bind-mount ~/.claude  |  api-key ($ANTHROPIC_API_KEY)
       │   └───┬─────┘
       │       │
   ┌───▼───────▼────┐    ┌─────────────┐
   │   sandbox      │    │   store     │
   │ Docker|Podman  │    │ SQLite (WAL)│
   │ NoSandbox(test)│    │  runs +     │
   └────────────────┘    │  events +   │
                         │  rankings   │
                         └─────────────┘
```

## Quick start

```bash
# 1. Install
go install github.com/felipemarques-rec/sandcode/cmd/sandcode@latest
# or build from source:
#   git clone https://github.com/felipemarques-rec/sandcode && cd sandcode
#   go build -o sandcode ./cmd/sandcode

# 2. In your project — must be a git repo with at least one commit
cd ~/my-project
git init && git add -A && git commit -m "init"

# 3. Scaffold .sandcode/{Dockerfile,config.yaml,.gitignore}
sandcode init --lang go --agents claude-code --sandbox docker

# 4. Build the sandbox image (uses the scaffolded Dockerfile)
docker build -t sandcode-default:latest .sandcode

# 5. Run a single agent (uses your Claude subscription via ~/.claude)
sandcode run "add a hello world function in main.go" --sandbox docker
```

## Examples

### 1) Single agent

```bash
sandcode run "refactor the auth middleware to remove the legacy session token branch"
```

Creates a worktree at `.sandcode/work/<runID>/0/`, runs Claude Code inside a container that mounts `~/.claude` read-only, and merges into the current branch HEAD on success.

### 2) Parallel with an LLM judge

```bash
sandcode run \
    --parallel 3 \
    --agent claude-code,codex,cursor \
    --judge llm \
    --strategy merge-to-head \
    --max-concurrency 2 \
    "implement a least-recently-used cache with O(1) get/put"
```

Runs the three agents in separate worktrees, lets the judge rank the diffs, and merges **only** the winner — the others stay available as branches (`git branch | grep sandcode/`).

### 3) Custom Dockerfile + strict limits

Edit `.sandcode/Dockerfile` to include your project's tools (e.g. `pnpm`, `pytest`, `cargo`). Then:

```bash
sandcode run \
    --image sandcode-default:latest \
    --cpus 1 --memory 1g --pids-limit 256 \
    --network none \
    --timeout 5m \
    --auth-mode api-key \
    "fix the failing test in cache_test.go"
```

`--network none` creates a network-less sandbox; `--auth-mode api-key` injects `ANTHROPIC_API_KEY` instead of bind-mounting.

## CLI

| Command | Purpose |
|---|---|
| `sandcode init` | Create `.sandcode/{Dockerfile,config.yaml,.gitignore}` |
| `sandcode run "<prompt>"` | Run one (or N) agents |
| `sandcode serve` | Start the HTTP API (see Security) |
| `sandcode list` | Table of recent runs (filters: `--status`, `--agent`, `--all`) |
| `sandcode show <run-id>` | Run details + ranking if it's a parallel parent |
| `sandcode logs <run-id> [--follow]` | Stream events from the store |

Relevant `run` flags:

```
--sandbox       docker|podman|nosandbox       (default docker)
--agent         claude-code|codex|cursor      (comma-separated for fan-out)
--role          role=agent                    (repeatable; opt-in)
--parallel N    replicate single agent N times
--judge         none|llm
--strategy      merge-to-head|branch          (default merge-to-head)
--auth-mode     bindmount|api-key             (default bindmount; AGENT auth)
--llm-auth      auto|subscription|api-key     (default auto; Go-side LLM-feature auth)
--cpus / --memory / --pids-limit / --timeout / --network
--keep-worktree --no-store
--report               write a deterministic REPORT.md after the run (to the worktree;
                       persists only with --keep-worktree)
--review               LLM code review of the diff (adds a ## Review section + event)
--architect            design guidance before the run (kernel-stage; requires --learn;
                       runs on divergent/high-complexity prompts)
--security-review      deterministic secret scan of the diff (no key)
--security-review-llm  LLM vuln+secret review (overrides --security-review)
--perf-review          LLM performance review of the diff
--refactor-review      LLM refactoring review of the diff (checks SOLID / clean
                       architecture / 12-Factor)
```

The single-run review/report flags apply to the single-run path only (ignored with `--dag`, multi-agent `--parallel`, or `--learn`).

### Auth: agent vs. Go-side LLM features

There are **two** independent auth planes:

- **`--auth-mode`** controls how the **coding agent** (Claude Code/Codex) inside the sandbox authenticates: `bindmount` mounts `~/.claude` (your subscription) or `api-key` injects `ANTHROPIC_API_KEY`.
- **`--llm-auth`** controls the **Go-side LLM features** (`--judge`, `--review`, `--architect`, `--perf-review`, `--refactor-review`, `--security-review-llm`, and the `--dag` planner):
  - `api-key` — calls the Anthropic Messages API directly with `ANTHROPIC_API_KEY`.
  - `subscription` — routes through the `claude` CLI (`claude --print --output-format json`), reusing the **same subscription** as the agent — **no `ANTHROPIC_API_KEY` needed**.
  - `auto` (default) — uses `api-key` when `ANTHROPIC_API_KEY` is set, otherwise `subscription` when the `claude` CLI is on PATH.

So with the subscription logged in (`~/.claude`) and no `ANTHROPIC_API_KEY`, every feature works by default.

## Security

sandcode runs agents that execute code. Key points (full detail in [`SECURITY.md`](SECURITY.md)):

- **Sandbox:** use `--sandbox docker|podman` (isolated). `--sandbox nosandbox` runs **on the host with no isolation** — for tests only, never with untrusted prompts.
- **`sandcode serve`:** binds to `127.0.0.1` by default; a non-loopback bind **requires** `--api-token` (or `SANDCODE_API_TOKEN`), enforced on every endpoint except `/healthz`. The request `cwd` is constrained to the server's `--cwd`.
- **Secrets:** diffs/prompts/output pass through a best-effort redactor before being persisted, logged, or sent to an LLM. It is not foolproof — keep real secrets out of the agent's workspace.
- `.sandcode/` databases and `REPORT.md` are created owner-only and git-ignored.

## Testing

```bash
go test -race ./...   # all packages, no data races
```

The suite (25 packages) runs without Docker or an API key — it uses the `nosandbox` provider and fake agents.

## License

[MIT](LICENSE).
