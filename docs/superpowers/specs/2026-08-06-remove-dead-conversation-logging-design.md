# Remove dead conversation-logging and unused reflection-prompt fields

## Context

`internal/memory/schema.go` defines `conversations` and `messages` tables, and
`internal/memory/store.go` defines `CreateConversation`/`AppendMessage` to
populate them. Neither method has a production caller — only test code calls
them — so in every real deployment both tables are permanently empty.

`store.GetRecentExchanges` reads from those empty tables and is called from
`cmd/ghost/main.go` to populate `ReflectionInput.RecentExchanges` for
`ghost reflect`. Since the source tables are always empty, this call always
returns an empty slice; the "Recent Exchanges" section of the reflection
prompt never renders in production.

Git archaeology confirms this is fallout from the v0.8.0 MCP-only scope lock
(PR #149, `docs/superpowers/specs/2026-04-18-ghost-mcp-only-strip.md`), which
removed every user-facing frontend (TUI, Telegram, VSCode extension) that used
to call `AppendMessage`, while leaving the write path itself and its
downstream plumbing in place. It was not a deliberately unfinished feature —
it's orphaned code from a deletion that didn't fully cascade.

A prior design explored resurrecting this by wiring conversation logging
through the stop hook's detached-spawn pattern. That was rejected on
privacy/security grounds (always-on transcript logging is a materially
riskier feature than what existed before — secrets in pasted transcripts, a
second uncurated copy of session data, no consent gate, reachable via
`ghost_memory_search`/`ghost obsidian export`). `ghost reflect`'s actual stated
job is consolidating `ExistingMemories` + `CurrentContext`, not mining raw
exchanges, and the role `RecentExchanges` used to serve is already covered by
the stop hook's mandatory-save nudge (`stopBlockMessage`), which ensures
curation happens upstream, live, by the agent. So the decision is to delete
the dead code rather than build new logging.

While reviewing `internal/reflection/prompt.go` for this cut, two more
dead-code items surfaced in the same file and are folded into this same pass:

- `ExtractionPrompt`: a system prompt const for a per-exchange extraction
  flow that no longer exists (zero callers anywhere in the repo).
- `ReflectionInput.LastCommits` / `ProjectLanguage`: fields read by
  `BuildReflectionPrompt` but never populated by `main.go` — always
  zero-value in production, same dead-weight category as `RecentExchanges`.

## Goal

Remove all three dead-code items in one pass: the conversations/messages
schema + `RecentExchanges` wiring, `ExtractionPrompt`, and
`LastCommits`/`ProjectLanguage`. No behavior change for any live code path on
a database where `conversations`/`messages` are empty — `ghost reflect`
already runs today with these inputs always empty/zero-valued. A database
where either table is non-empty is out of scope for this change (see
"Non-goals" below); it exists only if some code path wrote to them outside
this repo's own callers, since none of this repo's shipped code does.

## Changes

1. **`internal/memory/schema.go`** — remove the `conversations` and
   `messages` `CREATE TABLE IF NOT EXISTS` blocks and their two indexes
   (`idx_conversations_project`, `idx_messages_conv`) from `initSQL`.

2. **`internal/memory/migrate.go`** — add `migrateV4`, dropping `messages`
   then `conversations` (messages first, since it FKs to conversations).
   Append it to the `migrations` slice. Bump `schemaVersion` to `4`.

3. **`internal/memory/store.go`** — remove `CreateConversation`,
   `AppendMessage`, `GetRecentExchanges`, `GetLatestConversation`,
   `GetConversationMessages`, and the `ConversationMessage` type.

4. **`internal/provider/provider.go`** — remove the 5 corresponding methods
   from the `MemoryStore` interface.

5. **`internal/reflection/prompt.go`** — remove `RecentExchanges`,
   `LastCommits`, `ProjectLanguage` fields from `ReflectionInput`; remove the
   "Recent Exchanges" and "Recent Git Activity" prompt-building blocks;
   simplify the "## Project" line to just Name; remove the `ExtractionPrompt`
   const entirely.

6. **`cmd/ghost/main.go`** — remove the `store.GetRecentExchanges(...)` call,
   the `exchanges` var, and the `RecentExchanges:` field from the
   `ReflectionInput{}` literal.

7. **`docs/architecture.md`** — drop the `conversations`/`messages` rows from
   the SQLite Schema table; drop the `store.GetRecentExchanges()` line from
   the `ghost reflect` data-flow diagram.

8. **`internal/memory/store_test.go`** — remove `TestStoreGetRecentExchanges`,
   the conversation/message test block exercising
   Create/Append/GetConversationMessages/GetLatestConversation, and
   `TestStoreGetLatestConversationNoRows`.

9. **`internal/reflection/consolidator_test.go`** — drop the
   `ProjectLanguage: "go"` line from its `ReflectionInput{}` literal.

## Non-goals

- No data migration: the tables are always empty in production, so
  `migrateV4` is a plain drop, not a backfill-and-drop. As a safety check
  (not a migration path), `migrateV4` counts rows in both tables first and
  aborts the migration with an error, without dropping anything, if either
  is non-empty — this only guards against the "always empty" assumption
  being wrong on some deployment; it defines no preservation or export
  behavior, since one is deliberately out of scope.
- **Recovery policy for a non-empty table:** a deployment that hits the
  abort is explicitly unsupported by this migration — `ghost` ships no
  backfill, export, or backup tooling for `conversations`/`messages`. If it
  happens, the operator's options are: (a) inspect and manually export any
  rows they care about via `sqlite3 <db> ".dump conversations messages"` (or
  equivalent), then manually `DROP TABLE conversations; DROP TABLE
  messages;` and re-run `ghost` so the migration completes on the now-empty
  tables, or (b) open an issue if this occurs on a real deployment, since it
  would mean the "zero production callers" premise this whole change rests
  on is wrong and needs re-investigating before deleting anything further.
  This is a manual, one-time recovery step, not a maintained code path.
- No replacement logging mechanism. The stop hook's mandatory-save nudge
  already covers the curation role `RecentExchanges` used to serve.
- No change to `ghost resolve`/`ghost supersede` or any other subsystem.

## Testing

- `go vet ./...` and `go test ./...` before and after the change.
- Confirm via grep that no references to the removed identifiers remain
  outside this change set.
- Manually exercise `ghost reflect <project> --apply` against a dev DB to
  confirm consolidation still works with a normal existing-memories set.
