# Ghost — MCP Memory Server

Go 1.26+ MCP memory server: SQLite + FTS5 (modernc.org/sqlite, pure Go, no CGO), hybrid vector search via Ollama, maintenance passes (reflect, resolve, supersede) run through the calling session's CLI harness (claude/opencode/codex/goose). No direct model API client.

## Read before changing code
- `docs/invariants.md` — the package map and every load-bearing invariant (credential guard, memory history, drop guard, scope rules, schema refusal, spawn guards). Check the bullets for the packages you touch.
- `docs/architecture.md` — the full design, the concurrency contract, backup/transfer and the context-assembly target design.
- `docs/cli.md`, `docs/mcp.md` — the command and tool surfaces; keep them and the tool count in sync with code.
- New invariants go in `docs/invariants.md`, not here: this file holds only the rules every change must follow.

## Critical Rules
- Always `go vet ./...` before committing
- Tests use `go test ./...`
- Never commit to main directly — feature branches + PRs
- SQLite schema is embedded as a Go string constant in `internal/memory/schema.go` (currently v18; `migrate.go` carries one frozen step per version)
- `ghost mcp init` is idempotent and non-destructive — safe to re-run
- A test that needs a real *program* builds it with `go build` into a temp dir rather than re-exec'ing its own binary: under `go test` the test binary is the suite, so a self-spawn re-runs every test in the package once per spawn and a bug in the barrier logic becomes a fork bomb. A `-test.run`-bounded child test is fine (`internal/ai/harness_policy_test.go` does exactly that)
- The end-to-end suite (`e2e/`, build tag `e2e`) runs the BUILT binary: `make test-e2e`, or `go test -tags e2e ./e2e/ -count=1`. The tag is what keeps `go test ./...` and CI unchanged — with it absent the `./...` pattern skips `e2e/` silently, so do not add a file there without `//go:build e2e` (and `e2e/doc.go` deliberately carries `!e2e` so `go test ./e2e/` is a clean "no test files" rather than a build-constraint error). Rules for writing into it: every test runs in a sandbox whose environment is BUILT from an allowlist (never filtered from `os.Environ()`), the LLM harnesses and the embedding endpoint are fakes, and a failing case is a finding — fix the product in its own commit with a regression test if it is small, otherwise `t.Skip("known: #NNN")` plus a filed issue. Coverage is enforced against the product's own `tools/list` and against `cmd/ghost/help.go`'s `usageByCommand`, both read at run time, so a new tool or subcommand fails the suite. See [architecture.md](docs/architecture.md#testing)
- The concurrency contract in `docs/architecture.md` is enforced by `TestMultiProcessSharedDatabase` (`internal/memory/multiproc_process_test.go`, helper in `internal/memory/testdata/multiproc`): MCP server, CLI child, read-only handle, lifecycle writer, pinned-snapshot reader and sampler processes against one database. It reads `memory.OpenDB`'s and `memory.readOnlyDSN`'s settings back from a live connection, so dropping WAL, `foreign_keys`, `busy_timeout`, the pool pin or `_txlock=immediate` fails it. `mcpinit`'s own DSNs are not covered. Skips under `-short`; makes no LLM call
