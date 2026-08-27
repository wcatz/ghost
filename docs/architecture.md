# Ghost Architecture

## Runtime

Ghost runs as a single binary with one primary mode:

```
ghost mcp              MCP server on stdio (used by Claude Code, Cursor, Goose, opencode)
ghost mcp init         Configure MCP client integration (four hosts: claude, opencode, codex, goose)
ghost mcp status       Health check
ghost hook session-start --source <host>   SessionStart hook (called by MCP clients)
ghost hook stop --source <host>        Stop hook — save-nudge, blocks stop once (called by MCP clients)
ghost reflect <project>    Manual memory consolidation
ghost supersede <project>  LLM-classified 'supersedes' link creation
ghost obsidian export|sync One-way Markdown vault mirror
ghost bench [--sweep]      Retrieval-quality benchmark
ghost upgrade          Self-update from GitHub Releases (sha256-verified)
ghost version          Print version
ghost context [--cwd <dir>]       Print the passive session-start context block (for opencode)
ghost resolve <project>    Mark resolved-evidence memories (dry-run by default, --apply to write)
ghost project delete <name> [--apply]   Permanently delete a project and everything under it (dry-run by default)
ghost project merge <old> <new>   Merge one project into another; child records move to the survivor with memory IDs, links, and pin state preserved
```

## Package Map

```
cmd/ghost/main.go          CLI entrypoint + subcommand dispatch
internal/
  ai/                      LLM backends: Anthropic HTTP client + 4 CLI backends (claude, opencode, codex, goose) + sampling provider + source-aware routing; client.go: Reflect(); models.go: Message/TokenUsage; cost.go: per-model pricing; used by reflection, resolve, and supersede
    models.go              Message, ContentBlock, SystemBlock, TokenUsage
    cost.go                Per-model pricing, CostForUsage()
  config/                  Layered configuration (koanf)
    config.go              Config struct, Load(), EnsureConfigFile()
    config.example.yaml    Annotated defaults
  embedding/               Local vector embeddings
    client.go              Ollama HTTP client (/api/embed)
    worker.go              Async batch embedder
  linking/                 Memory auto-linking
    worker.go              Sweeps embedded memories, links cosine neighbors ≥ threshold
  supersede/               ghost supersede — 'supersedes' link creation
    supersede.go           Candidate selection (cosine proposes, created_at directs), Run()
    haiku.go               LLM classifier for 3-way SUPERSEDES/CAUSES/NEITHER classification
  bench/                   ghost bench — retrieval-quality benchmark harness
    dataset.go             JSONL dataset loading + seeding with embedding fixtures
    runner.go              Graded conditions (fts/vector/hybrid)
    metrics.go             Recall@k, MRR, NDCG
    sweep.go               Fusion-parameter grid search
    staleness.go           Fresh-fact-wins suite (supersede demote proof)
    recencytrap.go         Older-answer-correct suite (category-aware decay frontier)
  memory/                  Persistence layer
    store.go               SQLite CRUD, FTS5 search, time-decay scoring
    schema.go              DDL (embedded Go string constant — the single source of truth)
    vector.go              Cosine similarity, hybrid RRF search
    links.go               Memory links: edge CRUD (related/supersedes/contradicts/elaborates/causes)
  resolve/                 ghost resolve — keyword prefilter + LLM classify → resolved_at
  mcpserver/               MCP server (stdio transport)
    mcpserver.go           20 tools + 4 resources + 2 prompts via go-sdk
  hostevent/               Normalized host-event contract: envelope parse, capability matrix, transcript scanners
  mcpinit/                 Host integration setup — contract-v1 lifecycle dispatch, installers (Claude Code, opencode, codex, goose)
    init.go                ghost mcp init — registers server, imports memories, writes redirects
    status.go              ghost mcp status — health check
    opencode_ghost.ts      Embedded opencode lifecycle plugin (session.status→idle → contract)
    hook.go                ghost hook session-start — injects project context
    stophook.go            ghost hook stop — save-nudge, blocks stop once when nothing was saved
  obsidian/                One-way Markdown vault mirror (ghost export → PRAGMA data_version sync)
    export.go              Export memories to Markdown vault
    sync.go                Keep vault mirror fresh (PRAGMA data_version polling)
    render.go              Render memory blocks as Markdown
  linking/                 Memory auto-linking
    worker.go              Sweeps embedded memories, links cosine neighbors ≥ threshold
  claudeimport/            One-time import of Claude Code auto-memory files
    import.go              Scans ~/.claude/projects/*/memory/*.md, upserts into Ghost
  reflection/              Memory consolidation
    consolidator.go        Consolidator interface + TieredConsolidator
    tier_haiku.go          Haiku LLM consolidation (requires ANTHROPIC_API_KEY or use CLI tier)
    tier_sqlite.go         Local Jaccard similarity consolidation (free, always available)
    prompt.go              BuildReflectionPrompt()
  provider/                Interface contracts
    provider.go            LLMProvider, MemoryStore
  selfupdate/              Self-update from GitHub releases
    selfupdate.go          LatestRelease, Download, ExtractBinary, Replace
  eval/                    End-to-end pipeline grader
    eval/cycle             Pipeline grader: inject annotated corpus via real MCP save, run supersede/resolve/reflect, grade vs annotations, write Markdown scorecard
    eval/cycle/corpus      Load/validate JSONL corpus; harness-only annotations never reach the save path
```

## Data Flow

### MCP Server (primary mode)
```
Claude Code / Cursor → stdio JSON-RPC → mcpserver
                                          ↓
                        Tools (pull-based, Claude must call):
                          ghost_memory_search → store.SearchHybrid() or SearchFTS()
                          ghost_memory_save   → store.Upsert()
                          ghost_project_context → store.GetTopMemories()
                          ghost_save_global   → store.Upsert("_global")
                          ghost_task_create/update/complete → store.CreateTask()...
                          ghost_decision_record → store.RecordDecision()
                          ghost_health        → store metadata query
                          ... 20 tools total
                                          ↓
                        Resources (pinnable, survive context compaction):
                          ghost://project/{id}/context   → GetTopMemories + GetLearnedContext
                          ghost://project/{id}/decisions → ListDecisions
                          ghost://project/{id}/tasks     → ListTasks
                          ghost://memories/global        → GetTopMemories("_global")
                                          ↓
                               SQLite (no LLM calls in hot path)
```

### SessionStart Hook
```
MCP client session opens
  → ghost hook session-start --source claude-code (stdin: JSON with cwd + projectPath)
  → lookupProject(db, cwd)           # path-prefix match OR name fallback
  → buildProjectContext(store, id)   # top memories + tasks + decisions + globals
  → writes markdown to stdout
  → Claude Code injects into system prompt
```

### Memory Consolidation (ghost reflect)
```
ghost reflect <project> --apply
  → store.GetAll()           # existing memories
  → TieredConsolidator.Consolidate()
      → HaikuConsolidator (if configured with Anthropic key or via CLI)
          → Anthropic API (haiku model)
      → SQLiteConsolidator (fallback)
          → Jaccard token similarity, merge >50% overlap
  → quality gate: reject if < 30% of existing memories returned
  → store.ReplaceNonManual()  # atomic replace of non-manual memories
  → store.UpdateLearnedContext()
```

## Embedding (optional, Ollama)

```
embedding.Worker goroutine:
  every 2min → store.UnembeddedMemoryIDs()
             → embedding.Client.Embed(content)  # Ollama /api/embed
             → store.StoreEmbedding(id, vec)
             
Search with embeddings enabled:
  store.SearchHybrid() → 70% vector (cosine) + 30% FTS5, RRF fusion (k=60)

Search without embeddings:
  store.SearchFTS() → FTS5 only (porter unicode61 tokenizer)

linking.Worker goroutine:
  every 2min → store.UnscannedEmbeddedMemoryIDs()
             → store.SearchVector(own embedding)   # top cosine neighbors
             → store.CreateLink(≥ threshold, 'related')
             → store.MarkLinkScanned()
  Links cascade-delete with memories and are rebuilt after reflection
  rewrites them — same self-healing lifecycle as embeddings.
```

A link-graph expansion bonus was evaluated and removed (dominated by a deeper vector-k; links and the vector leg are both cosine). The memory_links graph is retained for Obsidian export and supersedes ranking.

## SQLite Schema

| Table | Purpose |
|-------|---------|
| `projects` | Project registry (id, path, name) |
| `memories` | Core store (category, content, importance, tags, source, pinned) |
| `memories_fts` | FTS5 virtual table (porter unicode61 tokenizer) |
| `memory_embeddings` | Vector embeddings (float32 blob) |
| `ghost_state` | Per-project state (interaction count, learned context) |
| `token_usage` | Per-request token + cost tracking |
| `tasks` | Task tracker (title, status, priority, description) |
| `decisions` | Architectural decisions (rationale, alternatives, status) |
| `memory_links` | Memory graph edges (related/supersedes/contradicts/elaborates/causes; soft-invalidated, cascade-delete) |
| `link_scans` | Tracks which embedded memories the linking worker has scanned |
| `memory_snapshots` | Pre-replace backups consumed by `ghost reflect --restore` |
| `audit_log` | Append-only record of destructive/consolidation operations |

The schema lives solely in `internal/memory/schema.go` (embedded Go constant).
Note that `CREATE TABLE IF NOT EXISTS` never migrates an existing database —
schema changes only reach databases created after the change.

## Time-Decay Scoring

Memories are scored by `importance × decay_factor`, where
`decay_factor = max(floor, 1 / (1 + age_days / scale))`:

| Category | Scale (half-life) | Floor |
|----------|-------------------|-------|
| preference, convention, fact | none (no decay) | — |
| architecture, pattern | 45-day | 0.3 |
| decision, gotcha, dependency | 30-day | 0.15 |

Pinned memories are fully exempt from decay — `decay_factor` is forced to `1.0`
regardless of category or age, so a pinned memory always scores at its raw
importance. This is a no-op for preference/convention/fact, which already never
decay. See `DecayRankingSQL` / `GetTopMemories` in `internal/memory/store.go`.

## Build

```bash
# Pure Go — no CGO (modernc.org/sqlite with FTS5 built-in)
go build -o ghost ./cmd/ghost

# Release (goreleaser — triggered by git tag)
# Targets: linux/{amd64,arm64}, darwin/{amd64,arm64}, windows/{amd64,arm64}
# ldflags: -s -w -X main.version={{.Version}}
```
