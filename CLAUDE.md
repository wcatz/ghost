# Ghost — MCP Memory Server

## Stack
- Go 1.25+ CLI application
- SQLite with FTS5 for memory persistence (modernc.org/sqlite — pure Go, no CGO)
- Claude API (manual HTTP client) — used by reflection only
- MCP server via modelcontextprotocol/go-sdk (stdio transport)

## Architecture
- `cmd/ghost/main.go` — CLI entrypoint; subcommands: mcp, hook, reflect, upgrade, version
- `internal/ai/` — Claude API client with streaming (Reflect method for consolidation)
- `internal/memory/` — SQLite CRUD, FTS5 search, vector search, time-decay scoring
- `internal/mcpserver/` — MCP server: 16 tools + 4 resources
- `internal/mcpinit/` — `ghost mcp init`, `ghost mcp status`, `ghost hook session-start`
- `internal/claudeimport/` — One-time import of Claude Code auto-memory on first contact
- `internal/embedding/` — Ollama async vectorization worker
- `internal/reflection/` — Memory consolidation: HaikuConsolidator + SQLiteConsolidator
- `internal/provider/` — Interface contracts: LLMProvider, MemoryStore
- `internal/config/` — Layered YAML + env config (koanf)
- `internal/selfupdate/` — `ghost upgrade` self-update from GitHub Releases

## Key Patterns
- Memory categories: architecture, decision, pattern, convention, gotcha, dependency, preference, fact
- Time-decay scoring: convention/preference/fact never decay; architecture/pattern 45-day; decision/gotcha/dependency 30-day
- Empty-set guard: never replace all memories with empty reflection output
- Project lookup: path-prefix match (longest wins) OR basename name fallback
- Global memories: `_global` project, included in every project's context
- Hybrid search: 70% vector (cosine, Ollama) + 30% FTS5, RRF fusion — falls back to FTS5-only

## Critical Rules
- Always `go vet ./...` before committing
- Tests use `go test ./...`
- Never commit to main directly — feature branches + PRs
- SQLite schema is embedded as a Go string constant in `internal/memory/schema.go`
- `ghost mcp init` is idempotent and non-destructive — safe to re-run
