# Ghost — MCP Memory Server

## Stack
- Go 1.26+ CLI application
- SQLite with FTS5 for memory persistence (modernc.org/sqlite — pure Go, no CGO)
- CLI harness callers (claude/opencode/codex/goose subprocesses) — used by reflection, resolve, and supersede. Each harness owns its authentication and billing; Ghost does not add a direct API client. `cli.model_reflect` / `model_resolve` / `model_supersede` pin the opencode harness model per phase (each phase is its own process; ignored by claude/codex/goose which have no model flag). Empty = any inherited `GHOST_OPENCODE_MODEL`, else Ghost's explicit `opencode/big-pickle` default; a configured pin overrides an inherited env pin for that phase.
- MCP server via modelcontextprotocol/go-sdk (stdio transport)

## Architecture
- `cmd/ghost/main.go` — CLI entrypoint; subcommands: mcp, hook, reflect, resolve, supersede, project, obsidian, bench, upgrade, version, context
- `internal/ai/` — CLI-harness providers (`CLIClient`/`OpenCodeClient`/`SourceProvider`) used by reflection + resolve + supersede — no Anthropic HTTP API client
- `internal/memory/` — SQLite CRUD, FTS5 search, vector search, time-decay scoring
- `internal/mcpserver/` — MCP server: 20 tools + 4 resources + 2 prompts (`recall_project`, `record_decision`)
- `internal/mcpinit/` — `ghost mcp init`, `ghost mcp status`, `ghost hook <event> --source <host>` (contract-v1 lifecycle dispatch; installers: claude-code, opencode plugin, codex TOML+hooks.json, goose Agent-Plugins package; goose field aliasing lives in hostevent.Parse)
- `internal/claudeimport/` — One-time import of Claude Code auto-memory on first contact
- `internal/embedding/` — Ollama async vectorization worker
- `internal/linking/` — Background worker linking similar memories into a graph
- `internal/resolve/` — `ghost resolve`: LLM-classified de-weighting of resolved-evidence memories (drops from ranked injection, stays searchable); classifies via CLI harness (see Classifier backend below)
- `internal/supersede/` — `ghost supersede`: LLM-classified 'supersedes'/'causes' link creation over live memories, batched 8 candidate pairs per harness call with a content-keyed NEITHER cache (`supersede_checked`, schema v8, cascading FKs) so a converged project's fresh candidates make zero calls (live-link pairs are still validated each pass)
- `internal/bench/` — `ghost bench`: retrieval-quality benchmark harness (graded dataset + sweep + staleness/recency suites)
- `internal/obsidian/` — One-way Markdown vault mirror (`ghost obsidian export|sync`)
- `internal/reflection/` — Memory consolidation: LlmConsolidator + SQLiteConsolidator
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
- Memory links: `memory_links` edge table auto-populated by cosine similarity (internal/linking worker); links cascade-delete with memories and self-heal after reflection. A graph-expansion ranking bonus was evaluated and removed — dominated by a deeper vector-k (links and the vector leg are both cosine); the link graph is retained for Obsidian export and supersedes ranking (see `docs/superpowers/specs/2026-07-20-graph-expansion-stays-off-design.md`).
- Resolution classifier: `ghost resolve` runs a keyword prefilter then batches up to 8 notes per KEEP-biased CLI-harness classify call (`claude`/`opencode`/`codex`/`goose` subprocess, numbered `N: RESOLVED|KEEP` reply judged by a strict first-field line parser) to mark `resolved_at`, dropping resolved-evidence memories (changelogs, cost estimates, closed experiment notes) from ranked injection while keeping them searchable. KEEP verdicts are cached by content hash in `memories.resolve_kept_hash` (schema v7; the hash key carries a version prefix so a prompt/rubric change can reset the cache), so a converged project makes no calls. Dry-run writes nothing, `--apply` writes resolved_at and the cache; a partial classify pass fails fatally rather than applying incomplete results. Never invoked from the stop hook's synchronous path (that path forbids DB access); the stop hook's detached `ghost lifecycle` spawn (whose resolve phase runs `--apply`) and the standalone batch command are the only callers. See `docs/superpowers/specs/2026-07-26-resolution-classifier-design.md`.
- Classifier backend: `resolve`/`supersede` classify via the calling session's own CLI harness — the backend is picked from the MCP client's reported identity (an opencode session uses the opencode binary, claude uses claude, etc.) via `ai.SourceProvider`, or — on the headless CLI path — from `--source`, else `ai.DetectSource()` (env markers plus process ancestors); if no calling harness can be determined it fails fast with an actionable error. There is no claude-first cascade: `NewSourceProviderForSource("")` is unavailable, and `ai.CLIProvider` is an availability probe only. MCP sampling was retired from this path per SEP-2577 — see `docs/superpowers/specs/2026-08-24-resolve-sampling-path-design.md`.

## Critical Rules
- Always `go vet ./...` before committing
- Tests use `go test ./...`
- Never commit to main directly — feature branches + PRs
- SQLite schema is embedded as a Go string constant in `internal/memory/schema.go` (currently v14; `migrate.go` carries one frozen step per version)
- `ghost mcp init` is idempotent and non-destructive — safe to re-run
