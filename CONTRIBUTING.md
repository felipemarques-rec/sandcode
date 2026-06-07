# Contributing to sandcode

Thanks for your interest in contributing! sandcode is a pure-Go orchestrator
for coding agents — no CGO, no runtime dependencies beyond the Go module graph.

## Development setup

```bash
git clone https://github.com/felipemarques-rec/sandcode
cd sandcode
go build -o sandcode ./cmd/sandcode
```

You need Go 1.23+ and, to run agents for real, Docker or Podman plus a coding
agent CLI (`claude`, `codex`, ...). The test suite runs without either —
it uses the `nosandbox` provider and fake agents.

## Before opening a PR

Run the same gates CI runs:

```bash
gofmt -l .            # must print nothing
go vet ./...
go build ./...
go test -race ./...   # all packages must pass, no data races
```

- Keep changes focused; one logical change per PR.
- Add tests for new behavior. Security-sensitive code (sandbox, redaction,
  the HTTP server) must come with tests.
- Match the surrounding style and comment density.
- Never commit secrets or real credentials. `.sandcode/` databases and
  `.env*` are git-ignored — keep it that way.

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md). Do not
open public issues for security problems.

## License

By contributing, you agree that your contributions are licensed under the
MIT License (see [LICENSE](LICENSE)).
