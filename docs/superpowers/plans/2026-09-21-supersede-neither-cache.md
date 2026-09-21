# Supersede NEITHER Cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop re-classifying unchanged candidate pairs that were already judged NEITHER, so repeat supersede passes on a converged project make ~zero harness calls.

**Architecture:** Fresh candidates whose two endpoints' content hashes match a stored NEITHER verdict are skipped and treated as `RelationNeither`; newly judged NEITHER pairs are recorded on `--apply` in a new `supersede_checked` table (schema v8) whose rows cascade-delete with their memories. Reclassify candidates (existing `supersedes`/`llm` links) are never cached — their existing skip-if-unchanged rules and self-healing stay exactly as they are.

**Tech Stack:** Go 1.26, `internal/supersede`, `internal/memory` (schema v8), `cmd/ghost`. Mirrors the resolve KEEP cache (PR #483) and the supersede batching (PR #480, v0.30.9).

---

## Background and design decisions

Supersede batching (v0.30.9) cut calls ~8x, but `Run` still re-classifies every fresh fresh candidate each pass (`internal/supersede/supersede.go:226`); skip-if-unchanged covers only existing links (`:270`). On dingo, whose 281 pairs are mostly NEITHER, every session stop still pays ~35 calls. Task Ghost `2354C85C`.

1. **Cache key:** pair `(newer_id, older_id)` + both endpoints' content hashes, hashed as `"v1\x00"+content` (same version-prefix reset lever as resolve's `ContentHash`). Content-only because the classification question is about the notes' text.
2. **Fresh only:** the cache applies to candidate pairs from `SelectCandidates` (tracked by the existing `freshKeys` map); reclassify candidates always classify so link self-healing is untouched.
3. **Apply-only writes:** dry-run stays side-effect-free.
4. **Dedicated table, not `memory_links`:** a `neither` edge in the link graph would leak into Obsidian export and ranking consumers; `supersede_checked` with cascading FKs deletes itself when either memory is replaced/merged by reflection.
5. **Stale rows are harmless:** if a pair's content changes, its stored hashes no longer match; a later SUPERSEDES verdict for the pair leaves the old row unused (it could only match again if the content reverted, in which case skipping is conservative — NEITHER writes nothing).
6. **Result observability:** `Result.Skipped` counts cached-pair skips; the CLI summary reports it alongside `classify call(s)`.

## File structure

| File | Change |
|---|---|
| `internal/memory/schema.go` | `supersede_checked` table (fresh DBs) |
| `internal/memory/migrate.go` | `schemaVersion = 8`, append `migrateV8` |
| `internal/memory/store.go` | `SupersedeCheck` type, `SupersedeChecked`, `MarkSupersedeNeither` |
| `internal/memory/store_test.go` + `migrate_test.go` | round-trip, cascade, project scope, v7→v8 migration |
| `internal/supersede/supersede.go` | `contentHash`, cache partition, record on apply, `Result.Skipped`; interface gains the two store methods |
| `internal/supersede/supersede_test.go` | skip/reclassify/dry-run/change tests |
| `cmd/ghost/main.go` | summary includes cached count |
| `CLAUDE.md` + `internal/supersede/supersede.go` package doc | describe the negative cache |

**Worktree:** `.worktrees/supersede-cache` on `feat/supersede-neither-cache`, based on `origin/main` at `d2cf5da` (schema v7 present; v8 appends after it).

---

### Task 1: Schema v8 and store methods

**Files:** `internal/memory/{schema.go,migrate.go,store.go,store_test.go,migrate_test.go}`.

- [ ] **Step 1:** add to `initSQL` (after `memory_links`):

```sql
CREATE TABLE IF NOT EXISTS supersede_checked (
    newer_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    older_id    TEXT NOT NULL REFERENCES memories(id) ON DELETE CASCADE,
    project_id  TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    newer_hash  TEXT NOT NULL,
    older_hash  TEXT NOT NULL,
    checked_at  TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (newer_id, older_id)
);
CREATE INDEX IF NOT EXISTS idx_supersede_checked_project ON supersede_checked(project_id);
```

- [ ] **Step 2:** `schemaVersion = 8`; append `migrateV8` to `migrations`; `migrateV8` creates the table + index with `CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` (new-table migrations copy the DDL, matching the existing pattern).
- [ ] **Step 3:** store methods (Lock/RLock per file convention):

```go
// SupersedeCheck is a cached NEITHER verdict for an ordered pair: the content
// hashes it was judged on. A pair is only skipped while both hashes still match.
type SupersedeCheck struct {
	NewerHash string
	OlderHash string
}

// SupersedeChecked returns the cached NEITHER verdicts for a project, keyed by
// {newerID, olderID}. Missing endpoints are absent from the map.
func (s *Store) SupersedeChecked(ctx context.Context, projectID string) (map[[2]string]SupersedeCheck, error)

// MarkSupersedeNeither records (or refreshes) NEITHER verdicts for a project.
// It is a no-op for an empty map.
func (s *Store) MarkSupersedeNeither(ctx context.Context, projectID string, checks map[[2]string]SupersedeCheck) error
```

`MarkSupersedeNeither` uses one transaction and an upsert: `INSERT INTO supersede_checked (newer_id, older_id, project_id, newer_hash, older_hash) VALUES (?,?,?,?,?) ON CONFLICT(newer_id, older_id) DO UPDATE SET newer_hash = excluded.newer_hash, older_hash = excluded.older_hash, checked_at = datetime('now')`.

- [ ] **Step 4:** tests: round-trip (mark two, read map including project scoping); cascade (delete one memory → its row disappears; reflection's `ReplaceNonManual` delete path covers this via FK); upsert refresh updates hashes; empty map no-op; `migrateV8` test using the raw-DB pattern from `TestMigrateV7AddsResolveKeptHash` (v7-shaped DB without the table, `PRAGMA user_version = 7`, `migrate(db, 7)`, assert table exists); fresh-DB assertion that `supersede_checked` exists from `initSQL`.
- [ ] **Step 5:** `go test ./internal/memory/ -count=1` green; commit `feat(memory): persist supersede NEITHER checks (schema v8)`.

---

### Task 2: `Run` cache integration

**Files:** `internal/supersede/supersede.go`, `internal/supersede/supersede_test.go`.

- [ ] **Step 1:** add the versioned content hash helper (mirror `internal/resolve.ContentHash`):

```go
// contentHash is the NEITHER-cache key component. The version prefix is the
// reset lever for a future prompt/rubric change: bump it and every stored
// verdict stops matching.
func contentHash(content string) string {
	sum := sha256.Sum256([]byte("v1\x00" + content))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 2:** extend `vectorStore` with `SupersedeChecked` and `MarkSupersedeNeither`. Add `Skipped int // fresh pairs skipped via the NEITHER cache` to `Result`.
- [ ] **Step 3:** in `Run`, after the existence re-check (where `all` and `freshKeys` are final):
  - load `checked, err := store.SupersedeChecked(ctx, projectID)` (fatal on error);
  - partition: a pair is cache-skipped iff `freshKeys[key]` (fresh, not reclassify) and the stored check's hashes both equal `contentHash` of the current endpoint contents; count them in `res.Skipped`;
  - call `cls.ClassifyBatch` with only the non-skipped pairs and build a `verdictByKey map[[2]string]Relation` from the returned slice (keep the existing verdict-count guard);
  - iterate `all` as today: cache-skipped pairs get `RelationNeither` (they write nothing and affect no counter), classified pairs use `verdictByKey`; preserve the exact `Unclassified`/`Confirmed`/`CausesCreated`/`Reclassified` logic and the empty `all` early return. If every pair is cache-skipped, make **no** classifier call at all.
- [ ] **Step 4:** on `apply`, after the write loop, record hashes for freshly classified NEITHER pairs only (not cache skips, not reclassify pairs, not SUPERSEDES/CAUSES): `MapSupersedeNeither` with `{newer, older} -> {contentHash(newer), contentHash(older)}`. A store error is fatal like the link writes (or warn-and-continue, matching resolve's cache choice — pick one and test it).
- [ ] **Step 5:** tests (mirror the resolve cache tests): cached pair skipped with zero classifier calls when all pairs cached; a changed endpoint re-classifies; dry-run records nothing; reclassify pairs are classified even with a matching cache row; `Result.Skipped` counts; a mixed cached/uncached pass gets one batch call for the uncached pairs with correct link writes.
- [ ] **Step 6:** `go vet ./... && go test ./internal/supersede/ ./internal/memory/ -count=1` green; commit `perf(supersede): skip cached NEITHER candidate pairs`.

---

### Task 3: CLI summary, docs, PR

**Files:** `cmd/ghost/main.go`, `CLAUDE.md`, `internal/supersede/supersede.go` (doc).

- [ ] **Step 1:** extend the `runSupersede` summary to include `res.Skipped` (keep one line; read the current format first).
- [ ] **Step 2:** update the supersede package doc and the CLAUDE.md supersede bullet to mention the content-keyed NEITHER cache (schema v8) alongside batching.
- [ ] **Step 3:** `go build ./... && go vet ./... && go test ./... -count=1` (live tests run, not skipped).
- [ ] **Step 4:** commit `docs(supersede): describe the NEITHER cache` (or fold the doc changes into the CLI commit and use one message that covers both).
- [ ] **Step 5:** push, open the PR, `gh pr checks --watch`:
```
gh pr create --title "perf(supersede): skip cached NEITHER candidate pairs" --body "Batching cut supersede to ~8x fewer calls, but every fresh candidate pair was still re-classified on every pass; skip-if-unchanged only covered existing links. This caches NEITHER verdicts by pair and both content hashes in a new supersede_checked table (schema v8, cascading FKs), so a converged project makes zero classify calls. Fresh candidates only (reclassify pairs still validate), apply-only writes, dry-run untouched. Related: Ghost task 2354C85C."
```
Do NOT merge without the user.

---

## Self-review

**Spec coverage:** persistence (Task 1), Run behavior (Task 2), observability/docs/PR (Task 3). Per-phase model tiering (`B1F8C738`) remains separate.

**Type consistency:** `SupersedeCheck{NewerHash,OlderHash}`, `SupersedeChecked(ctx, projectID) (map[[2]string]SupersedeCheck, error)`, `MarkSupersedeNeither(ctx, projectID, map[[2]string]SupersedeCheck) error`, `contentHash(string) string`, `Result.Skipped` used consistently.
