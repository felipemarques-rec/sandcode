# Security Policy

## Reporting a vulnerability

Please report security issues privately via **GitHub Security Advisories**
("Report a vulnerability" on the repository's *Security* tab) rather than a
public issue. We aim to acknowledge reports within a few days.

## Threat model & security posture

sandcode runs AI coding agents that execute code. Treat every prompt as
potentially leading to arbitrary code execution **inside the sandbox**.

### Sandbox isolation
- **`--sandbox docker` / `--sandbox podman`** (default `docker`) run the agent in
  a container with a git-worktree bind-mount. Credentials (`~/.claude`) are
  mounted **read-only** and scoped to the credential directory only.
- **`--sandbox nosandbox` runs the agent directly on the host with no isolation
  and full host access.** It exists for tests/dry-runs. **Never use it with
  untrusted prompts.** It must be selected explicitly; it is never a default.
- Default resource guard: `--pids-limit 1024` (fork-bomb guard). Set
  `--cpus`/`--memory`/`--timeout` for stricter limits. `--network none` disables
  sandbox networking.

### HTTP server (`sandcode serve`)
`POST /v1/runs` executes an agent, so the API is treated as a code-execution
surface:
- **Binds to `127.0.0.1:8080` by default.** A non-loopback bind is **refused**
  unless an auth token is set.
- **Bearer-token auth** (`--api-token` or `SANDCODE_API_TOKEN`) is required on
  every endpoint except `GET /healthz`. Constant-time comparison.
- The client-supplied `cwd` is constrained to the server's configured `--cwd`
  subtree; `network: host` is rejected over the API.
- Run a single-tenant, trusted-network deployment. The server is not designed
  for multi-tenant or public exposure.

### Secret redaction
- A best-effort redactor (`internal/redact`) strips common secret shapes
  (provider API keys, private keys, JWTs, credential URLs, `key=value`
  secrets) from agent diffs, verify output, prompts, event payloads and the
  run store **before** they are persisted, written to `REPORT.md`, or sent to
  an LLM helper. Verified end-to-end.
- Redaction is **regex-based and best-effort** — it will not catch every secret
  format. Do not rely on it as your only control; keep real secrets out of the
  workspace the agent operates on.
- When LLM features are enabled (`--judge`, `--review`, `--architect`,
  `--perf-review`, `--refactor-review`, `--security-review-llm`, `--dag`
  planner), the (redacted) prompt and diff are sent to Anthropic (API key) or
  through the `claude` CLI (subscription). This egress is by design.
- Data-at-rest: `.sandcode/` databases and `REPORT.md` are created with
  owner-only permissions (dir `0700`, report `0600`) and are git-ignored.

## Known residual items
- SSE event replay (`?from=` / `Last-Event-ID`) loads a run's events into
  memory per connection; it is bounded by request size and gated behind auth,
  but very large event histories can amplify memory use.
- `internal/sandbox/nosandbox` intentionally provides no isolation (see above).
