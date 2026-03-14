# Ghost — Memory-First Coding Agent

## Stack
- Go 1.24+ CLI application
- SQLite with FTS5 for memory persistence
- Claude API (manual HTTP client, not SDK)
- Native tool_use for file operations, search, git, and shell

## Architecture
- `cmd/ghost/main.go` — CLI entrypoint
- `internal/ai/` — Claude API client with streaming + tool_use
- `internal/memory/` — SQLite CRUD, FTS5 search, time-decay scoring
- `internal/tool/` — Tool registry + 10 built-in tools
- `internal/orchestrator/` — Multi-project session manager
- `internal/reflection/` — Periodic memory consolidation (Haiku)
- `internal/prompt/` — 3-block system prompt construction
- `internal/mode/` — Operating modes (chat, code, debug, review, plan, refactor)
- `internal/project/` — Project detection and context gathering
- `internal/config/` — TOML config + environment variables
- `internal/tui/` — Terminal REPL with streaming output

## Key Patterns
- 3-block prompt caching: Block 1 (static, cached), Block 2 (project context, cached), Block 3 (memories, dynamic)
- Agentic tool loop: send -> tool_use -> execute -> send results -> repeat until end_turn
- Memory categories: architecture, decision, pattern, convention, gotcha, dependency, preference, fact
- Time-decay scoring: identity never decays, behavioral 45-day half-life, situational 30-day
- Empty-set guard: never replace all memories with empty reflection output

## Critical Rules
- Always `go vet ./...` before committing
- Tests use `go test ./...`
- Never commit to main directly — feature branches + PRs
- SQLite migrations are embedded via go:embed
- Tool approval levels: None (read ops), Warn (writes), Require (bash/destructive)
