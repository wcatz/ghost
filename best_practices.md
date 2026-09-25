# Ghost Best Practices

## Code Style
- Go 1.26+, no CGO (modernc.org/sqlite)
- `go vet ./...` must pass before commit
- Tests: `go test ./...`
- Embed schema as Go string constants in `internal/memory/schema.go`
- Provider pattern: implement `cliBackend` interface (Reflect + Classify) for new CLI backends
- Error sentinels: use `var errXxx = errors.New(...)` for domain errors, not string matching

## Architecture Patterns
- CLI subprocess clients (CLIClient, OpenCodeClient, CodexClient, GooseClient) all follow the same shape: struct with `binary` field, `Reflect`/`Classify` methods, `run` helper with timeout + env stripping
- `harnessCommand` is the single spawn funnel: it applies the backend-specific `harnessEnv` allowlist, scratch confinement, and no-tools policy inputs; probes and new clients must use it
- `harnessEnv` keeps only reviewed common and backend-specific variables; `GHOST_PASSTHROUGH_ENV` is an explicit, security-sensitive escape hatch
- `stripLLMKeys` strips provider API keys from subprocess env — add new always-stripped keys here when adding CLI backends
- Stop hook spawns reflect/resolve/supersede as detached processes with `--source` forwarding
- SourceProvider maps host source strings to CLI backends; `CLIProvider` is the best-on-PATH availability probe, not source-aware routing
- There is no `FallbackProvider` in the current architecture: a missing or undetectable CLI harness fails fast, while offline consolidation is an explicit SQLite tier

## Commit Conventions
- Prefix: `feat(component):`, `fix(component):`, `chore(component):`, `docs:`
- DCO sign-off required (`git commit -s`)
- No AI attribution in commit messages
- Feature branches + PRs only — never commit to main

## Testing
- Table-driven tests with `t.Run` subtests
- Use `t.TempDir()` for filesystem tests
- Check `os.WriteFile` errors in test setup
- Test both available and unavailable binary paths for CLI providers
- SQLite schema migrations: each step wrapped in tx with foreign_key_check

## Security
- Strip API keys from subprocess environments (ANTHROPIC_API_KEY, OPENAI_API_KEY, GOOSE_PROVIDER__API_KEY)
- Keep harness tool/MCP surfaces disabled; do not rely on a scratch working directory as a sandbox
- Pass only reviewed environment variables; document any new public configuration or escape hatch
- Never log or commit secrets
- SOPS-encrypted secrets only — never git restore encrypted files

## Project Invariants
- The `_global` project is protected: every destructive or reassigning project operation (`DeleteProject`, `MergeProject`, any future op that deletes project rows or moves child records) must refuse `_global` on either side, with the refusal implemented at the store layer so all callers inherit it
- New callers of existing store/provider APIs inherit that API's guard clauses — when exposing one through a new CLI command or MCP tool, read the full implementation first and preserve its refusals at the deepest layer
- Error-handling replacements at call sites must cover the same failure modes they replace (an empty-string miss-check is only equivalent to `err != nil` if every error path also returns empty)
