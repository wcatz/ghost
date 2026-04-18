# Ghost: Strip to MCP-Only

**Date:** 2026-04-18  
**Status:** Approved

## What and Why

Ghost is being repositioned as a focused MCP memory server for Claude Code. The TUI, Telegram bot, HTTP server, voice, Google integrations, scheduler, GitHub monitor, and VSCode extension are being removed. The standalone AI assistant persona goes away; the memory thesis stays.

A full copy of the current repo is preserved at `~/git/gertrude` as the starting point for a future standalone Telegram notification bot.

## What Stays

| Package | Why |
|---------|-----|
| `internal/memory/` | Core SQLite store — the whole point |
| `internal/mcpserver/` | MCP server exposing memory to Claude Code |
| `internal/mcpinit/` | `ghost mcp init`, `ghost mcp status`, `ghost hook session-start` |
| `internal/provider/` | Interfaces (MemoryStore, LLMProvider) |
| `internal/embedding/` | Hybrid vector search for MCP tools |
| `internal/reflection/` | Memory consolidation (Haiku) — keeps memories from rotting |
| `internal/claudeimport/` | One-time import of Claude Code auto-memory on first contact |
| `internal/config/` | YAML config + env vars |
| `internal/ai/` | Minimal Claude HTTP client (needed by reflection only) |
| `internal/selfupdate/` | `ghost upgrade` — users need to update the binary |

## What Goes

`internal/tui`, `internal/telegram`, `internal/voice`, `internal/google`, `internal/server`, `internal/scheduler`, `internal/github`, `internal/briefing`, `internal/calendar`, `internal/mdv2`, `internal/orchestrator`, `internal/mode`, `internal/project`, `internal/prompt`, `internal/tool`, `internal/simulation`, `vscode-ghost/`

## CLI After Strip

```
ghost mcp                    # Run MCP server on stdio (primary use)
ghost mcp init [--dry-run]   # Configure Claude Code integration
ghost mcp status             # Health check
ghost hook session-start     # SessionStart hook (called by Claude Code)
ghost reflect <project>      # Manual memory consolidation
ghost upgrade                # Self-update binary
ghost version                # Print version
ghost help                   # Print usage
```

No `ghost serve`, no `ghost [message]` interactive session, no flags for TUI/mode/model.

## Dependency Removals (go.mod)

Remove: `charm.land/*`, `go-telegram/bot`, `google.golang.org/api`, `cloud.google.com/*`, `go-chi/chi`, `gocron`, `go-github`, `go-webdav`, `olebedev/when`, `BourgeoisBear/rasterm`, `alecthomas/chroma`, `yuin/goldmark*`, `microcosm-cc/bluemonday`, `charmbracelet/*`, `golang.org/x/term`

Keep: `modelcontextprotocol/go-sdk`, `modernc.org/sqlite`, `koanf`, `golang.org/x/oauth2` (used by ai client), `coder/websocket`

## Dockerfile

Change `CMD ["serve"]` → `CMD ["mcp"]`.

## Release

Tag `v0.8.0` — minor bump (repositioning, not a breaking API change for existing MCP users). Goreleaser pipeline unchanged.

## Steps

1. Copy current repo to `~/git/gertrude` (full history)
2. Merge PR #148 to main on ghost
3. Create `refactor/mcp-only` branch on ghost
4. Delete removed packages
5. Rewrite `cmd/ghost/main.go` (remove runServe, interactive session, all removed imports)
6. Update go.mod: `go mod tidy`
7. Update Dockerfile CMD
8. `go build ./...`, `go vet ./...`, `go test ./...`
9. Update README
10. PR, CI green, merge to main
11. Tag `v0.8.0`, goreleaser cuts release
