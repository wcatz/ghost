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
- `stripLLMKeys` strips provider API keys from subprocess env — add new keys here when adding CLI backends
- Stop hook spawns reflect/resolve/supersede as detached processes with `--source` forwarding
- SourceProvider maps host source strings to CLI backends; fallbackCLI cascades through all backends
- FallbackProvider wraps primary + optional secondary; credit-exhaustion triggers fallover

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
- Never log or commit secrets
- SOPS-encrypted secrets only — never git restore encrypted files

## Project Invariants
- The `_global` project is protected: every destructive or reassigning project operation (`DeleteProject`, `MergeProject`, any future op that deletes project rows or moves child records) must refuse `_global` on either side, with the refusal implemented at the store layer so all callers inherit it
- New callers of existing store/provider APIs inherit that API's guard clauses — when exposing one through a new CLI command or MCP tool, read the full implementation first and preserve its refusals at the deepest layer
- Error-handling replacements at call sites must cover the same failure modes they replace (an empty-string miss-check is only equivalent to `err != nil` if every error path also returns empty)
