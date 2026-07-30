# Contributing to IOLD

## Development setup

Go (version pinned in `go.mod`) is the only requirement for the core CLI.

```bash
go test -race ./...   # unit and component tests
go vet ./...
gofmt -l .            # must print nothing
```

## Before opening a pull request

- All tests pass with the race detector.
- `gofmt`, `go vet`, and `staticcheck` are clean (CI enforces all three plus `govulncheck` and secret scanning).
- New behavior comes with tests, including the failure paths listed in `docs/TESTING.md`.
- No secrets, tokens, or real endpoint URLs anywhere — `.env.example` holds placeholder names only.
- Update `docs/TASKS.md` when a backlog item changes status, and `docs/DECISIONS.md` when a decision is made or reversed.

## Design constraints to respect

- `docs/ARCHITECTURE.md` is the source of truth for the state machine, destroy semantics, and security rules.
- All system access in checks goes through injectable probe interfaces so tests never need a GPU.
- Deployment IDs are validated as single path segments before any filesystem operation.
- Every process signal must be preceded by a PID ownership check.
- Log output must pass through the `redact` package before reaching users.

## Reporting issues

Use the issue tracker for bugs and feature requests. For security issues, see `SECURITY.md` — do not open public issues.
