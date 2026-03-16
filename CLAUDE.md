# Ghost — Memory Daemon & Personal Assistant

## Stack
- Go 1.25+ CLI application
- SQLite with FTS5 for memory persistence
- Claude API (manual HTTP client, not SDK)
- MCP server for Claude Code / Cursor / Goose integration

## Architecture
- `cmd/ghost/main.go` — CLI entrypoint
- `internal/ai/` — Claude API client with streaming + tool_use
- `internal/memory/` — SQLite CRUD, FTS5 search, time-decay scoring
- `internal/tool/` — Tool registry (memory_save, memory_search only)
- `internal/mcpserver/` — MCP server exposing memory tools
- `internal/orchestrator/` — Multi-project session manager
- `internal/reflection/` — Periodic memory consolidation (Haiku)
- `internal/prompt/` — 3-block system prompt construction
- `internal/mode/` — Operating mode (chat only)
- `internal/project/` — Project detection and context gathering
- `internal/config/` — YAML config + environment variables
- `internal/tui/` — Terminal REPL with streaming output
- `internal/telegram/` — Telegram bot frontend
- `internal/google/` — Google Calendar + Gmail integration
- `internal/github/` — GitHub notification monitor
- `internal/scheduler/` — Cron jobs + reminders
- `internal/briefing/` — Morning briefing generator

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

## Scope Lock — DO NOT VIOLATE

Ghost is a **memory daemon**, **MCP server**, and **personal assistant**.
Ghost is **NOT** a coding agent. Claude Code does coding. Ghost remembers things.

### Frozen packages — do not add features, do not resurrect:
- `internal/tool/` — ONLY memory_save and memory_search. No file/bash/git/glob/grep tools.
- `internal/voice/` — DELETED. Do not recreate until Phase C is explicitly started.
- `internal/mode/` — chat mode only. No code/debug/review/refactor modes.
- `vscode-ghost/` — DELETED. Do not recreate. Ghost is not an IDE extension.

### Do not add:
- File read/write/edit tools
- Shell/bash execution tools
- Git operation tools
- LSP or tree-sitter integration
- IDE extensions or coding UIs
- Sub-agent orchestration
- Any feature that duplicates Claude Code

### PR discipline:
- Every PR must reference a plan item (A.1, B.2, etc.) or be a bug fix
- Max 3 packages touched per PR
- No monolith PRs — one logical unit of work
- Run `go vet ./...` and `go test ./...` before every commit
