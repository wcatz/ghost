# Reflect No-Change Gate Implementation Plan

> **Historical record — not current documentation.** This file preserves the
> design or implementation state at the time it was written. For current Ghost
> behavior, start with [`docs/README.md`](../../README.md) and the source.

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Skip the full-corpus consolidation LLM call when the consolidatable memory set is unchanged since the last applied reflect.

**Architecture:** `ghost reflect` gains an opt-in `--skip-unchanged` flag (the auto lifecycle passes it). A SHA-256 fingerprint over the prompt-visible fields of the consolidatable set is stored in a new `ghost_state.reflect_input_sig` column after every successful apply; a later run with a matching fingerprint prints a skip line and exits without calling the model. Manual runs without the flag are unchanged.

**Tech Stack:** Go 1.26, `internal/reflection` (pure signature), `internal/memory` (schema v6 + store methods), `cmd/ghost` (flag, gate, lifecycle args).

---

## Background

reflect is one full-corpus LLM call per lifecycle pass (`BuildReflectionPrompt`, `internal/reflection/prompt.go:35`; up to 2000 consolidatable memories, each rendered with category/importance/source/access/tags/content). The **guarded-drop refusal that made rich projects retry forever is already fixed** — `cmd/ghost/main.go:956-983` now retains guarded-category drops verbatim (`RetainGuardedDrops`) so consolidation applies with zero loss. What remains is the no-change pass: the same corpus re-consolidated on every session stop even when nothing was saved.

Task: Ghost `777BB643` (scope updated 2026-09-21).

**Invariants:** no prompt change; no consolidation-quality change; a manual `ghost reflect <project>` (no flag) always runs; the flag only ever skips an `--apply` run whose stored fingerprint matches; any mutation to a consolidatable memory changes its `updated_at` (verified: `internal/memory/store.go:1074` and the pin/upsert paths all stamp `updated_at = datetime('now')`).

**Design decisions:**

1. **Fingerprint fields:** `id | updated_at | category | importance | source | tags | content` — the fields the prompt renders plus `id` and `updated_at` as change proxies (not rendered), with learned_context, git commits, and access counts excluded (see #2), so any prompt-visible mutation invalidates the gate.
2. **Excluded:** `learned_context` (the consolidator's own output — including it would make every apply invalidate its own gate), git commits (best-effort grounding, moves on every commit), access counts (incremented by ordinary reads, so they would break the gate on sessions that saved nothing).
3. **Stored post-apply**, recomputed over the reloaded consolidatable set — `ReplaceNonManual` may merge/reuse rows, so only stored state is authoritative.
4. **Flag, not mode:** `--skip-unchanged` is explicit; `lifecyclePhases` adds it to the auto chain; manual runs never skip by accident.
5. **Schema v6** adds `ghost_state.reflect_input_sig TEXT NOT NULL DEFAULT ''` (fresh DB via `initSQL`, existing DB via `migrateV6` with the existing `columnExists` guard).

## File structure

| File | Change |
|---|---|
| `internal/reflection/signature.go` | New: `InputSignature([]memory.Memory) string` |
| `internal/reflection/signature_test.go` | New: stability + invalidation tests |
| `internal/memory/schema.go` | `ghost_state` gains `reflect_input_sig` |
| `internal/memory/migrate.go` | `schemaVersion = 6`, append `migrateV6` |
| `internal/memory/store.go` | `GetReflectInputSignature` / `SetReflectInputSignature` |
| `internal/memory/store_test.go` | Round-trip test |
| `cmd/ghost/main.go` | `--skip-unchanged` flag, `consolidatable()` helper, gate, store-on-apply, usage text |
| `cmd/ghost/main_test.go` | lifecycle args assertion + gate-decision test |
| `docs/superpowers/plans/2026-09-21-reflect-skip-unchanged.md` | This file |

**Worktree:** `.worktrees/reflect-skip` on `feat/reflect-skip-unchanged` (created from `origin/main` at `fd3f6fb`). All commands run from there.

---

### Task 0: Baseline and plan commit

- [ ] **Step 1:** `go test ./internal/reflection/ ./internal/memory/ ./cmd/... -count=1` → all `ok`
- [ ] **Step 2:** commit the plan:
```bash
git add docs/superpowers/plans/2026-09-21-reflect-skip-unchanged.md
git commit -m "docs: add reflect no-change gate plan"
```

---

### Task 1: Input signature

**Files:** create `internal/reflection/signature.go`; test `internal/reflection/signature_test.go`.

- [ ] **Step 1: failing tests**

```go
package reflection

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestInputSignatureStableAndOrderIndependent(t *testing.T) {
	a := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.7, Source: "mcp", Tags: []string{"x", "y"}, Content: "one"}
	b := memory.Memory{ID: "b", UpdatedAt: "2026-09-02 00:00:00", Category: "gotcha", Importance: 0.9, Source: "reflection", Tags: []string{"z"}, Content: "two"}

	sigAB := InputSignature([]memory.Memory{a, b})
	sigBA := InputSignature([]memory.Memory{b, a})
	if sigAB != sigBA {
		t.Errorf("signature must be order-independent: %s != %s", sigAB, sigBA)
	}
	if sigAB == "" {
		t.Fatal("signature must not be empty")
	}
}

func TestInputSignatureInvalidatesOnPromptVisibleChange(t *testing.T) {
	base := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.7, Source: "mcp", Tags: []string{"x"}, Content: "one"}
	baseSig := InputSignature([]memory.Memory{base})

	mutate := map[string]func(m *memory.Memory){
		"content":    func(m *memory.Memory) { m.Content = "changed" },
		"category":   func(m *memory.Memory) { m.Category = "decision" },
		"importance": func(m *memory.Memory) { m.Importance = 0.8 },
		"source":     func(m *memory.Memory) { m.Source = "manual" },
		"tags":       func(m *memory.Memory) { m.Tags = []string{"y"} },
		"updated_at": func(m *memory.Memory) { m.UpdatedAt = "2026-09-02 00:00:00" },
		"id":         func(m *memory.Memory) { m.ID = "b" },
	}
	for name, fn := range mutate {
		m := base
		fn(&m)
		if got := InputSignature([]memory.Memory{m}); got == baseSig {
			t.Errorf("changing %s must change the signature", name)
		}
	}
}

func TestInputSignatureIgnoresAccessCount(t *testing.T) {
	a := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.7, Content: "one", AccessCount: 0}
	b := a
	b.AccessCount = 42
	if InputSignature([]memory.Memory{a}) != InputSignature([]memory.Memory{b}) {
		t.Error("access count must not affect the signature (ordinary reads would break the gate)")
	}
}
```

- [ ] **Step 2:** `go test ./internal/reflection/ -run TestInputSignature -v` → FAIL (undefined)
- [ ] **Step 3: implementation**

```go
package reflection

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// InputSignature fingerprints the consolidation input so an unchanged corpus
// can skip a full-corpus LLM call (ghost reflect --skip-unchanged). It covers
// exactly the fields BuildReflectionPrompt renders for each consolidatable
// memory, so any prompt-visible mutation invalidates the gate.
//
// Deliberately excluded:
//   - learned_context: the consolidator's own output, so including it would
//     make every successful run invalidate its own gate;
//   - git commits: best-effort grounding that moves on every commit without
//     changing what the consolidator would produce;
//   - access counts: incremented by ordinary reads/injections, so including
//     them would make the signature change on sessions that saved nothing.
func InputSignature(mems []memory.Memory) string {
	lines := make([]string, 0, len(mems))
	for _, m := range mems {
		lines = append(lines, fmt.Sprintf("%s|%s|%s|%.2f|%s|%s|%s",
			m.ID, m.UpdatedAt, m.Category, m.Importance, m.Source,
			strings.Join(m.Tags, ","), m.Content))
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return hex.EncodeToString(sum[:])
}
```

- [ ] **Step 4:** `go test ./internal/reflection/ -count=1` → PASS; `go vet ./internal/reflection/` clean
- [ ] **Step 5:** commit:
```bash
git add internal/reflection/signature.go internal/reflection/signature_test.go
git commit -m "feat(reflection): add consolidation input signature"
```

---

### Task 2: Schema v6 and store methods

**Files:** modify `internal/memory/schema.go`, `internal/memory/migrate.go`, `internal/memory/store.go`; test `internal/memory/store_test.go`.

- [ ] **Step 1: schema** — in the `ghost_state` CREATE TABLE, after `reflection_summary`, add:
```sql
    reflect_input_sig   TEXT NOT NULL DEFAULT '',
```
- [ ] **Step 2: migration** — `migrate.go`: `const schemaVersion = 6`; append `migrateV6` to `migrations`; add at the end of the file:
```go
// migrateV6 adds ghost_state.reflect_input_sig: the fingerprint of the memory
// set that produced the last applied consolidation (reflection.InputSignature).
func migrateV6(tx *sql.Tx) error {
	exists, err := columnExists(tx, "ghost_state", "reflect_input_sig")
	if err != nil {
		return err
	}
	if exists {
		return nil
	}
	if _, err := tx.Exec(`ALTER TABLE ghost_state ADD COLUMN reflect_input_sig TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("add ghost_state.reflect_input_sig: %w", err)
	}
	return nil
}
```
- [ ] **Step 3: store methods** — in `internal/memory/store.go`, after `UpdateLearnedContext`:
```go
// GetReflectInputSignature returns the fingerprint of the memory set that
// produced the last applied consolidation for projectID, or "" when none was
// recorded.
func (s *Store) GetReflectInputSignature(ctx context.Context, projectID string) (string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var sig string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(reflect_input_sig, '') FROM ghost_state WHERE project_id = ?`, projectID).Scan(&sig)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get reflect signature: %w", err)
	}
	return sig, nil
}

// SetReflectInputSignature records the fingerprint of the memory set that just
// produced an applied consolidation.
func (s *Store) SetReflectInputSignature(ctx context.Context, projectID, sig string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, err := s.db.ExecContext(ctx, `
		UPDATE ghost_state
		SET reflect_input_sig = ?, updated_at = datetime('now')
		WHERE project_id = ?
	`, sig, projectID)
	if err != nil {
		return fmt.Errorf("set reflect signature: %w", err)
	}
	return nil
}
```
- [ ] **Step 4: store test** (in `internal/memory/store_test.go`, alongside the learned-context tests):
```go
func TestReflectInputSignatureRoundTrip(t *testing.T) {
	store, _ := newTestStore(t) // follow the existing helper in this file
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetReflectInputSignature(ctx, "p")
	if err != nil || got != "" {
		t.Fatalf("initial = (%q, %v), want empty", got, err)
	}
	if err := store.SetReflectInputSignature(ctx, "p", "sig123"); err != nil {
		t.Fatal(err)
	}
	got, err = store.GetReflectInputSignature(ctx, "p")
	if err != nil || got != "sig123" {
		t.Fatalf("round-trip = (%q, %v), want sig123", got, err)
	}
	if err := store.SetReflectInputSignature(ctx, "p", ""); err != nil {
		t.Fatal(err)
	}
	if got, _ = store.GetReflectInputSignature(ctx, "p"); got != "" {
		t.Fatalf("after clear = %q, want empty", got)
	}
}
```
(The implementer must use the file's actual store-construction helper name.)
- [ ] **Step 5:** `go test ./internal/memory/ -run 'TestReflectInputSignature|TestMigrate' -count=1 -v` → PASS; then the full `go test ./internal/memory/ -count=1`
- [ ] **Step 6:** commit:
```bash
git add internal/memory/schema.go internal/memory/migrate.go internal/memory/store.go internal/memory/store_test.go
git commit -m "feat(memory): store the reflect input signature (schema v6)"
```

---

### Task 3: CLI flag, gate, and store-on-apply

**Files:** modify `cmd/ghost/main.go`; test `cmd/ghost/main_test.go`.

- [ ] **Step 1: flag + usage.** Add `skipUnchanged` to the `var` block (`var apply, restore, requireLLM, allowDrops, skipUnchanged bool`), parse:
```go
		case os.Args[i] == "--skip-unchanged":
			skipUnchanged = true
```
and add to the usage text:
```
  --skip-unchanged Skip when the consolidatable set is unchanged since the last
                   applied consolidation (used by the auto lifecycle)
```
- [ ] **Step 2: helper + gate.** Replace the `live` filter loop (currently `for _, m := range existingMemories { ... }` producing `live` and `resolvedCount`) with:
```go
	live := consolidatable(existingMemories)
	resolvedCount := 0
	for _, m := range existingMemories {
		if m.ResolvedAt != nil {
			resolvedCount++
		}
	}
```
Add the helper near `runReflect`:
```go
// consolidatable returns the memories reflection may rewrite: non-resolved,
// unpinned, non-manual rows. ReplaceNonManual preserves exactly the excluded
// set, so this is the input the consolidator sees — and therefore the set the
// skip-unchanged fingerprint must cover.
func consolidatable(mems []memory.Memory) []memory.Memory {
	out := make([]memory.Memory, 0, len(mems))
	for _, m := range mems {
		if m.ResolvedAt != nil || m.Pinned || m.Source == "manual" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// reflectSkipDecision reports whether a --skip-unchanged run may skip: the
// flag is set on an apply run and the stored fingerprint matches the current
// input. Pure so the decision is testable without a store.
func reflectSkipDecision(skipUnchanged, apply bool, stored, current string) bool {
	return skipUnchanged && apply && stored != "" && stored == current
}
```
Immediately after `live` is built (before the `maxConsolidationInput` check), insert the gate:
```go
	if skipUnchanged && apply {
		currentSig := reflection.InputSignature(live)
		storedSig, err := store.GetReflectInputSignature(ctx, projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: read reflect signature: %v\n", err)
		} else if reflectSkipDecision(skipUnchanged, apply, storedSig, currentSig) {
			fmt.Printf("reflect: consolidatable set unchanged since the last applied consolidation — skipping (%d memories, no LLM call)\n", len(live))
			return
		}
	}
```
- [ ] **Step 3: store-on-apply.** At the very end of the apply path in `runReflect` (after the project `ReplaceNonManual` block and the learned-context update, once every write has succeeded), insert:
```go
	// Record the post-apply fingerprint so the next --skip-unchanged run can
	// skip without an LLM call. Re-read rather than reusing `live`: the replace
	// may merge/reuse rows, so only the stored state is authoritative.
	if postMemories, err := store.GetAll(ctx, projectID, -1); err != nil {
		fmt.Fprintf(os.Stderr, "warning: record reflect signature: reload memories: %v\n", err)
	} else if err := store.SetReflectInputSignature(ctx, projectID, reflection.InputSignature(consolidatable(postMemories))); err != nil {
		fmt.Fprintf(os.Stderr, "warning: record reflect signature: %v\n", err)
	}
```
- [ ] **Step 4: tests** in `cmd/ghost/main_test.go`:
```go
func TestReflectSkipDecision(t *testing.T) {
	cases := []struct {
		skip, apply bool
		stored, cur string
		want        bool
	}{
		{true, true, "abc", "abc", true},
		{true, true, "abc", "def", false},
		{true, true, "", "abc", false},   // nothing recorded yet
		{true, false, "abc", "abc", false}, // dry-run never skips
		{false, true, "abc", "abc", false}, // manual run always executes
	}
	for _, c := range cases {
		if got := reflectSkipDecision(c.skip, c.apply, c.stored, c.cur); got != c.want {
			t.Errorf("reflectSkipDecision(%v,%v,%q,%q) = %v, want %v", c.skip, c.apply, c.stored, c.cur, got, c.want)
		}
	}
}
```
- [ ] **Step 5:** `go build ./... && go vet ./... && go test ./cmd/... -count=1` → clean/green
- [ ] **Step 6:** commit:
```bash
git add cmd/ghost/main.go cmd/ghost/main_test.go
git commit -m "feat(reflect): skip unchanged consolidation with --skip-unchanged"
```

---

### Task 4: Lifecycle wiring, docs, and PR

**Files:** modify `cmd/ghost/main.go` (`lifecyclePhases`), `cmd/ghost/main_test.go`, `internal/reflection/prompt.go` (comment), `README.md` if it describes reflect.

- [ ] **Step 1:** in `lifecyclePhases`, change the reflect phase args to:
```go
		phases = append(phases, lifecyclePhase{"reflect", []string{"reflect", projectName, "--apply", "--require-llm", "--skip-unchanged"}, timeout})
```
- [ ] **Step 2:** update the exact-args assertion in `TestLifecyclePhasesOrder` to `[]string{"reflect", "proj", "--apply", "--require-llm", "--skip-unchanged"}`.
- [ ] **Step 3:** run `go vet ./... && go test ./... -count=1` (live tests ~1 min; must not be skipped).
- [ ] **Step 4:** commit:
```bash
git add cmd/ghost/main.go cmd/ghost/main_test.go
git commit -m "feat(lifecycle): pass --skip-unchanged to auto-reflect"
```
- [ ] **Step 5:** push and PR:
```bash
git push -u origin feat/reflect-skip-unchanged
gh pr create --title "feat(reflect): skip unchanged consolidation passes" --body "reflect re-sends the whole consolidatable corpus to the harness on every session stop, even when nothing was saved since the last applied pass. This adds \`ghost reflect --skip-unchanged\` (the auto lifecycle passes it): a fingerprint over the prompt-visible fields of the consolidatable set is stored after each successful apply, and a matching fingerprint skips the LLM call. Manual reflect runs without the flag are unchanged; the fingerprint excludes learned_context (the consolidator's own output), git commits, and access counts so ordinary reads cannot break the gate. Schema v6 adds ghost_state.reflect_input_sig. Related: Ghost task 777BB643."
```
- [ ] **Step 6:** `gh pr checks --watch` → all pass. Do NOT merge without the user.

---

## Self-review

**Spec coverage:** signature (Task 1), persistence + migration (Task 2), gate + store-on-apply + CLI semantics (Task 3), auto wiring + PR (Task 4). Backoff explicitly out of scope (obsolete — retention fix).

**Placeholder scan:** none; each code step carries complete code. The one environment-dependent detail is the store-test constructor helper name in Task 2 Step 4 — the implementer must use the file's existing helper.

**Type consistency:** `reflection.InputSignature([]memory.Memory) string`, `store.GetReflectInputSignature(ctx, projectID) (string, error)`, `store.SetReflectInputSignature(ctx, projectID, sig) error`, `consolidatable([]memory.Memory) []memory.Memory`, `reflectSkipDecision(bool,bool,string,string) bool` are used consistently across tasks.
