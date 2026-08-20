# MemoryAgentBench Supersede Benchmark Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `bench/memoryagentbench/`, a standalone Go program (plus a small Python conversion script) that runs Ghost's real `ghost supersede` classifier against MemoryAgentBench's `Conflict_Resolution` (`fact_sh`) data and reports retrieval accuracy before/after supersede links exist.

**Architecture:** `convert.py` turns the HF parquet into JSONL (one demo per line: a flat fact list + single-hop questions + gold answers). The Go program seeds each demo's facts as memories with list-order timestamps, embeds them locally via Ollama, runs the real `internal/supersede` Haiku classifier with `apply=true`, then scores every question twice — once with `SupersedeDemote: false` (baseline) and once with `SupersedeDemote: true` (production default) — via substring match on the top-ranked memory's content. No LLM judge, no generation step.

**Tech Stack:** Go 1.26 (`github.com/wcatz/ghost/internal/{memory,supersede,ai,config}`), Python 3 + pyarrow (one-off conversion only), local Ollama (`nomic-embed-text:v1.5`), the real supersede classifier via the `opencode` CLI (subscription-billed, no `ANTHROPIC_API_KEY`).

**Spec:** `docs/superpowers/specs/2026-08-20-memoryagentbench-supersede-benchmark-design.md`

**Branch:** `feat/memoryagentbench-supersede-bench` (already checked out; the spec commit is already on it).

---

## File map

| File | Responsibility |
|---|---|
| `bench/memoryagentbench/dataset.go` | `Demo` type, JSONL loader, numbered-fact-line splitter |
| `bench/memoryagentbench/score.go` | Pure substring-match scoring (`answerHit`, `topKHit`) |
| `bench/memoryagentbench/seed.go` | Timestamp backdating + fact seeding in list order |
| `bench/memoryagentbench/embed.go` | Cached Ollama embedder (verbatim duplicate of `bench/longmemeval/embed.go`) |
| `bench/memoryagentbench/classifier.go` | Builds the real `supersede.HaikuClassifier` from config/env |
| `bench/memoryagentbench/evaluate.go` | Per-question search + outcome scoring against a seeded store |
| `bench/memoryagentbench/main.go` | CLI flags, per-demo orchestration, table/JSONL output |
| `bench/memoryagentbench/convert.py` | HF parquet → JSONL (`fact_sh` rows only) |
| `bench/memoryagentbench/README.md` | Setup, usage, cost, how to read the table |

---

### Task 1: `dataset.go` — Demo type, fact-line splitter, JSONL loader

**Files:**
- Create: `bench/memoryagentbench/dataset.go`
- Test: `bench/memoryagentbench/dataset_test.go`

- [ ] **Step 1: Write the failing test**

```go
// bench/memoryagentbench/dataset_test.go
package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSplitFacts(t *testing.T) {
	context := "Here is a list of facts:\n" +
		"0. Thomas Kyd was born in the city of London.\n" +
		"1. The chairperson of Fatah is Mahmoud Abbas.\n" +
		"16. Chanel was founded by Coco Chanel.\n" +
		"34. Chanel was founded by Andy Warhol.\n"
	facts := splitFacts(context)
	want := []string{
		"Thomas Kyd was born in the city of London.",
		"The chairperson of Fatah is Mahmoud Abbas.",
		"Chanel was founded by Coco Chanel.",
		"Chanel was founded by Andy Warhol.",
	}
	if len(facts) != len(want) {
		t.Fatalf("got %d facts, want %d: %v", len(facts), len(want), facts)
	}
	for i, f := range facts {
		if f != want[i] {
			t.Errorf("fact %d = %q, want %q", i, f, want[i])
		}
	}
}

func TestSplitFactsIgnoresNonNumberedLines(t *testing.T) {
	context := "Here is a list of facts:\n0. A fact.\nsome stray commentary line\n1. Another fact.\n"
	facts := splitFacts(context)
	if len(facts) != 2 {
		t.Fatalf("got %d facts, want 2: %v", len(facts), facts)
	}
}

func TestLoadDemos(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demos.jsonl")
	content := `{"source":"factconsolidation_sh_6k","context":"0. A.\n1. B.\n","questions":["q1"],"answers":[["B"]],"qa_pair_ids":["id0"]}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	demos, err := loadDemos(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(demos) != 1 {
		t.Fatalf("got %d demos, want 1", len(demos))
	}
	if demos[0].Source != "factconsolidation_sh_6k" {
		t.Errorf("source = %q", demos[0].Source)
	}
	facts := splitFacts(demos[0].Context)
	if len(facts) != 2 || facts[0] != "A." || facts[1] != "B." {
		t.Errorf("facts = %v", facts)
	}
}

func TestLoadDemosMismatchedLengths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demos.jsonl")
	content := `{"source":"bad","context":"0. A.\n","questions":["q1","q2"],"answers":[["A"]],"qa_pair_ids":["id0"]}` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDemos(path); err == nil {
		t.Fatal("expected error for mismatched questions/answers/qa_pair_ids lengths")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd bench/memoryagentbench && go test ./... -run 'TestSplitFacts|TestLoadDemos' -v`
Expected: FAIL — `splitFacts`/`Demo`/`loadDemos` undefined (no `dataset.go` yet, package `main` doesn't exist either — the build itself fails).

- [ ] **Step 3: Write the implementation**

```go
// bench/memoryagentbench/dataset.go

// dataset.go loads MemoryAgentBench Conflict_Resolution demos (converted to
// JSONL by convert.py) and splits each demo's numbered fact list into
// ordered fact sentences.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
)

// Demo is one row of the Conflict_Resolution split, as written by convert.py:
// a flat numbered fact list plus the single-hop questions probing it.
type Demo struct {
	Source    string     `json:"source"`
	Context   string     `json:"context"`
	Questions []string   `json:"questions"`
	Answers   [][]string `json:"answers"`
	QAPairIDs []string   `json:"qa_pair_ids"`
}

// factLine matches one numbered fact line, e.g. "16. Chanel was founded by
// Coco Chanel." — the captured group is the fact sentence without its
// leading index. Only spaces/tabs (not \s, which includes \n) are allowed
// between the dot and the body: an empty-body numbered line (e.g. "0.\n")
// then simply produces no match, instead of \s+ swallowing the newline and
// merging the next line's fact into this one. The capture excludes CR/LF
// directly so a CRLF-terminated line never leaves a trailing \r in the
// fact text.
var factLine = regexp.MustCompile(`(?m)^\d+\.[ \t]+([^\r\n]+)`)

// splitFacts splits a demo's context into its ordered fact sentences. List
// order is temporal order: MemoryAgentBench encodes a fact update/contradiction
// as a later line restating an earlier subject+relation with a different
// object, so fact N is understood to have been "stated after" fact N-1.
func splitFacts(text string) []string {
	matches := factLine.FindAllStringSubmatch(text, -1)
	facts := make([]string, len(matches))
	for i, m := range matches {
		facts[i] = m[1]
	}
	return facts
}

// loadDemos reads convert.py's output: one JSON Demo per line.
func loadDemos(path string) ([]Demo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck

	var out []Demo
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<21)
	for sc.Scan() {
		line := sc.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var d Demo
		if err := json.Unmarshal(line, &d); err != nil {
			return nil, fmt.Errorf("decode demo: %w", err)
		}
		if len(d.Questions) != len(d.Answers) || len(d.Questions) != len(d.QAPairIDs) {
			return nil, fmt.Errorf("demo %s: %d questions, %d answer sets, %d qa_pair_ids (must match)",
				d.Source, len(d.Questions), len(d.Answers), len(d.QAPairIDs))
		}
		out = append(out, d)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
```

Also add these three tests to `dataset_test.go` alongside the four above (a code-quality review of this exact code caught the `\s+`/CRLF/blank-line issues this version already fixes — these tests pin the fix down):

```go
func TestSplitFactsEmptyBodyLine(t *testing.T) {
	facts := splitFacts("0.\n1. B.\n")
	if len(facts) != 1 || facts[0] != "B." {
		t.Fatalf("got %v, want exactly [\"B.\"] (line 0 has no body and must not merge into line 1)", facts)
	}
}

func TestSplitFactsCRLF(t *testing.T) {
	facts := splitFacts("0. A.\r\n1. B.\r\n")
	want := []string{"A.", "B."}
	if len(facts) != len(want) {
		t.Fatalf("got %d facts, want %d: %v", len(facts), len(want), facts)
	}
	for i, f := range facts {
		if f != want[i] {
			t.Errorf("fact %d = %q, want %q (no trailing CR)", i, f, want[i])
		}
	}
}

func TestLoadDemosInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "demos.jsonl")
	if err := os.WriteFile(path, []byte("not valid json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadDemos(path); err == nil {
		t.Fatal("expected error for invalid JSON line")
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd bench/memoryagentbench && go test ./... -run 'TestSplitFacts|TestLoadDemos' -v`
Expected: PASS (all 7 tests)

- [ ] **Step 5: Commit**

```bash
git add bench/memoryagentbench/dataset.go bench/memoryagentbench/dataset_test.go
git commit -s -m "feat(bench): add memoryagentbench dataset loader and fact splitter"
```

---

### Task 2: `score.go` — substring-match scoring

**Files:**
- Create: `bench/memoryagentbench/score.go`
- Test: `bench/memoryagentbench/score_test.go`

- [ ] **Step 1: Write the failing test**

```go
// bench/memoryagentbench/score_test.go
package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestAnswerHit(t *testing.T) {
	cases := []struct {
		content string
		answers []string
		want    bool
	}{
		{"Chanel was founded by Andy Warhol.", []string{"Andy Warhol"}, true},
		{"Chanel was founded by Andy Warhol.", []string{"andy warhol"}, true},
		{"Chanel was founded by Coco Chanel.", []string{"Andy Warhol"}, false},
		{"Chanel was founded by Coco Chanel.", []string{"Coco Chanel", "Andy Warhol"}, true},
	}
	for _, c := range cases {
		if got := answerHit(c.content, c.answers); got != c.want {
			t.Errorf("answerHit(%q, %v) = %v, want %v", c.content, c.answers, got, c.want)
		}
	}
}

func TestTopKHit(t *testing.T) {
	results := []memory.Memory{
		{Content: "Chanel was founded by Coco Chanel."},
		{Content: "Chanel was founded by Andy Warhol."},
	}
	if topKHit(results, []string{"Andy Warhol"}, 1) {
		t.Error("top-1 should miss: Andy Warhol is ranked second")
	}
	if !topKHit(results, []string{"Andy Warhol"}, 2) {
		t.Error("top-2 should hit")
	}
	if topKHit(results, []string{"nonexistent"}, 2) {
		t.Error("expected no hit for an absent answer")
	}
	if topKHit(nil, []string{"anything"}, 5) {
		t.Error("expected no hit against an empty result set")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd bench/memoryagentbench && go test ./... -run 'TestAnswerHit|TestTopKHit' -v`
Expected: FAIL — `answerHit`/`topKHit` undefined.

- [ ] **Step 3: Write the implementation**

```go
// bench/memoryagentbench/score.go
package main

import (
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

// answerHit reports whether content contains any of answers as a
// case-insensitive substring — the same metric shape MemoryAgentBench's own
// leaderboard uses (substring_exact_match).
func answerHit(content string, answers []string) bool {
	lc := strings.ToLower(content)
	for _, a := range answers {
		if a == "" {
			continue
		}
		if strings.Contains(lc, strings.ToLower(a)) {
			return true
		}
	}
	return false
}

// topKHit reports whether any of the top k results is an answerHit.
func topKHit(results []memory.Memory, answers []string, k int) bool {
	if k > len(results) {
		k = len(results)
	}
	for i := 0; i < k; i++ {
		if answerHit(results[i].Content, answers) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd bench/memoryagentbench && go test ./... -run 'TestAnswerHit|TestTopKHit' -v`
Expected: PASS (both tests)

- [ ] **Step 5: Commit**

```bash
git add bench/memoryagentbench/score.go bench/memoryagentbench/score_test.go
git commit -s -m "feat(bench): add memoryagentbench substring-match scoring"
```

---

### Task 3: `seed.go` — timestamp backdating and list-order fact seeding

**Files:**
- Create: `bench/memoryagentbench/seed.go`
- Test: `bench/memoryagentbench/seed_test.go`

This is the piece the design doc calls out as non-cosmetic: `supersede.orient()` (`internal/supersede/supersede.go:147`) decides newer/older by `updated_at`, and `store.Create` always stamps `datetime('now')` — a plain batch-insert would tie every fact to the same timestamp, making orientation undefined. `backdate` is a verbatim duplicate of `internal/bench/staleness.go`'s helper of the same name/shape (that package can't be imported here — it's `package bench` under `internal/`, and this is an unrelated `package main` under `bench/`; the shape is small enough to duplicate rather than export a shared helper for one caller).

- [ ] **Step 1: Write the failing test**

```go
// bench/memoryagentbench/seed_test.go
package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestSeedFactsOrdersTimestamps(t *testing.T) {
	ctx := context.Background()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer store.Close() //nolint:errcheck // store.Close() closes the underlying db too

	const project = "test-mabench-seed"
	if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
		t.Fatal(err)
	}

	facts := []string{"A is founded by X.", "A is founded by Y.", "A is founded by Z."}
	ids, err := seedFacts(ctx, store, db, project, facts)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != len(facts) {
		t.Fatalf("got %d ids, want %d", len(ids), len(facts))
	}

	mems, err := store.GetByIDs(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]memory.Memory, len(mems))
	for _, m := range mems {
		byID[m.ID] = m
	}
	for i := 1; i < len(ids); i++ {
		prev, cur := byID[ids[i-1]], byID[ids[i]]
		if !(prev.UpdatedAt < cur.UpdatedAt) {
			t.Errorf("fact %d (%q, updated_at=%s) is not strictly older than fact %d (%q, updated_at=%s)",
				i-1, prev.Content, prev.UpdatedAt, i, cur.Content, cur.UpdatedAt)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd bench/memoryagentbench && go test ./... -run TestSeedFactsOrdersTimestamps -v`
Expected: FAIL — `seedFacts` undefined.

- [ ] **Step 3: Write the implementation**

```go
// bench/memoryagentbench/seed.go
package main

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// backdate rewrites a memory's created_at/updated_at. store.Create always
// stamps now(); supersede.orient() (internal/supersede/supersede.go) decides
// newer/older by updated_at, so every seeded fact needs a distinct timestamp
// matching its list position. Duplicated from internal/bench/staleness.go's
// helper of the same name/shape — raw SQL on the harness-owned store only.
// This bypasses Store's internal mutex; it is safe only because seedFacts
// calls it sequentially, never concurrently.
func backdate(ctx context.Context, db *sql.DB, id string, ageDays int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE memories SET created_at = datetime('now', ?), updated_at = datetime('now', ?) WHERE id = ?`,
		fmt.Sprintf("-%d days", ageDays), fmt.Sprintf("-%d days", ageDays), id)
	return err
}

// seedFacts creates one memory per fact, oldest (facts[0]) to newest
// (facts[len(facts)-1]) — list order is temporal order in MemoryAgentBench's
// Conflict_Resolution data (see splitFacts). Returns store IDs in the same
// order. Source is "mcp" — the memories.source column has a CHECK constraint
// (internal/memory/schema.go) allowing only 'reflection', 'chat', 'manual',
// 'tool', 'mcp', 'onboarding', 'decision_log'; "mcp" matches the convention
// internal/bench/staleness.go and internal/bench/dataset.go already use for
// their own seeded benchmark memories.
func seedFacts(ctx context.Context, store *memory.Store, db *sql.DB, project string, facts []string) ([]string, error) {
	ids := make([]string, len(facts))
	n := len(facts)
	for i, fact := range facts {
		id, err := store.Create(ctx, project, memory.Memory{
			Category: "fact", Content: fact, Importance: 0.7, Source: "mcp",
		})
		if err != nil {
			return ids[:i], fmt.Errorf("seed fact %d: %w", i, err)
		}
		// The oldest fact gets the largest age; the newest gets age 1 (never
		// 0 — 0 would tie with "now" for anything created after this pass).
		if err := backdate(ctx, db, id, n-i); err != nil {
			return ids[:i], fmt.Errorf("backdate fact %d: %w", i, err)
		}
		ids[i] = id
	}
	return ids, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd bench/memoryagentbench && go test ./... -run TestSeedFactsOrdersTimestamps -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add bench/memoryagentbench/seed.go bench/memoryagentbench/seed_test.go
git commit -s -m "feat(bench): add list-order timestamp backdating for supersede orientation"
```

---

### Task 4: `embed.go` — cached Ollama embedder (verbatim duplicate)

**Files:**
- Create: `bench/memoryagentbench/embed.go`

This file is a byte-for-byte duplicate of `bench/longmemeval/embed.go` — same package shape (`package main`), same small self-contained-per-benchmark convention the `bench/` directory already uses. No new test: the source file being duplicated has none either (`bench/longmemeval/` has no `embed_test.go`), and its logic is a thin HTTP/cache wrapper exercised end-to-end by every real run.

- [ ] **Step 1: Create the file, byte-for-byte identical to the source**

```go
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

const embedModel = "nomic-embed-text:v1.5"
const embedBatchSize = 64

// cachedEmbedder resolves text embeddings through an append-only JSONL cache
// keyed by content hash, batching cache misses to Ollama. Shared sessions
// across questions (and across runs) are embedded exactly once.
type cachedEmbedder struct {
	ollamaURL string
	client    *http.Client
	cache     map[string][]float32
	cacheFile *os.File
	hits      int
	misses    int
}

type cacheLine struct {
	Hash   string    `json:"h"`
	Vector []float32 `json:"v"`
}

func newCachedEmbedder(ollamaURL, cachePath string) (*cachedEmbedder, error) {
	e := &cachedEmbedder{
		ollamaURL: ollamaURL,
		client:    &http.Client{Timeout: 5 * time.Minute},
		cache:     make(map[string][]float32),
	}
	if cachePath == "" {
		return e, nil
	}
	f, err := os.OpenFile(cachePath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open embed cache: %w", err)
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var line cacheLine
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			continue // torn tail line from an interrupted run; recomputed on miss
		}
		e.cache[line.Hash] = line.Vector
	}
	if err := sc.Err(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read embed cache: %w", err)
	}
	if _, err := f.Seek(0, 2); err != nil { // append from the end
		_ = f.Close()
		return nil, err
	}
	e.cacheFile = f
	return e, nil
}

func (e *cachedEmbedder) Close() error {
	if e.cacheFile != nil {
		return e.cacheFile.Close()
	}
	return nil
}

func (e *cachedEmbedder) Stats() (hits, misses int) { return e.hits, e.misses }

func hashContent(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// EnsureBatch resolves every text into the cache, batching misses to Ollama.
func (e *cachedEmbedder) EnsureBatch(ctx context.Context, texts []string) error {
	var missing []string
	seen := make(map[string]bool)
	for _, t := range texts {
		h := hashContent(t)
		if _, ok := e.cache[h]; ok || seen[h] {
			continue
		}
		seen[h] = true
		missing = append(missing, t)
	}
	for start := 0; start < len(missing); start += embedBatchSize {
		end := min(start+embedBatchSize, len(missing))
		batch := missing[start:end]
		vecs, err := e.embedRemote(ctx, batch)
		if err != nil {
			return err
		}
		for i, t := range batch {
			h := hashContent(t)
			e.cache[h] = vecs[i]
			e.misses++
			if e.cacheFile != nil {
				line, _ := json.Marshal(cacheLine{Hash: h, Vector: vecs[i]})
				if _, err := fmt.Fprintf(e.cacheFile, "%s\n", line); err != nil {
					return fmt.Errorf("append embed cache: %w", err)
				}
			}
		}
	}
	return nil
}

// Embed returns the cached vector for text, resolving it remotely on a miss.
func (e *cachedEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	if v, ok := e.cache[hashContent(text)]; ok {
		e.hits++
		return v, nil
	}
	if err := e.EnsureBatch(ctx, []string{text}); err != nil {
		return nil, err
	}
	return e.cache[hashContent(text)], nil
}

type ollamaEmbedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type ollamaEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
}

func (e *cachedEmbedder) embedRemote(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(ollamaEmbedRequest{Model: embedModel, Input: texts})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.ollamaURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("ollama embed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama embed: HTTP %d", resp.StatusCode)
	}
	var er ollamaEmbedResponse
	if err := json.NewDecoder(resp.Body).Decode(&er); err != nil {
		return nil, fmt.Errorf("ollama embed decode: %w", err)
	}
	if len(er.Embeddings) != len(texts) {
		return nil, fmt.Errorf("ollama embed: %d vectors for %d inputs", len(er.Embeddings), len(texts))
	}
	return er.Embeddings, nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd bench/memoryagentbench && go vet ./...`
Expected: succeeds (no output). Use `go vet`, not `go build`, here: this package has no `func main()` until Task 7 creates `main.go`, so `go build ./...` fails at the link step with "function main is undeclared in the main package" even though every file compiles fine — `go vet` type-checks without requiring a linkable binary, so it's the right check before Task 7 lands. It won't be exercised by a test until Task 7 wires it into `main`.

- [ ] **Step 3: Commit**

```bash
git add bench/memoryagentbench/embed.go
git commit -s -m "feat(bench): add cached Ollama embedder for memoryagentbench"
```

- [ ] **Step 4: Add a duplication pointer, in a separate follow-up commit**

A byte-for-byte duplicate with no cross-reference is a maintenance trap: unlike `seed.go` (which documents "Duplicated from `internal/bench/staleness.go`") and `dataset.go`, nothing in either `embed.go` copy says a twin exists. Add one to `bench/memoryagentbench/embed.go` only — **do not touch `bench/longmemeval/embed.go`**, a separate benchmark outside this plan's scope:

```go
// cachedEmbedder resolves text embeddings through an append-only JSONL cache
// keyed by content hash, batching cache misses to Ollama. Shared sessions
// across questions (and across runs) are embedded exactly once. Duplicated
// from bench/longmemeval/embed.go — see that file for the original; keep
// embedModel in sync between the two if it ever changes, since a cache file
// shared across both harnesses (--embed-cache pointed at the same path) is
// keyed by content hash alone, with no model tag.
type cachedEmbedder struct {
```

(Replaces the plain doc comment above `type cachedEmbedder struct` from Step 1 — everything else in the file is unchanged.)

- [ ] **Step 5: Verify**

Run: `cd bench/memoryagentbench && go vet ./... && gofmt -l .`
Expected: both clean (no output).

- [ ] **Step 6: Commit**

```bash
git add bench/memoryagentbench/embed.go
git commit -s -m "docs(bench): note embed.go's duplication source and cache-key coupling"
```

---

### Task 5: `classifier.go` — build the real supersede classifier via OpenCode

**Files:**
- Create: `bench/memoryagentbench/classifier.go`

This benchmark exists specifically to exercise the real classifier, but it runs the classifier through the `opencode` CLI (`ai.NewOpenCodeClientWithBinary`, the same client reflection's `--tier opencode` uses — see `cmd/ghost/main.go`'s `opencode` case), not `ANTHROPIC_API_KEY` — subscription-billed, no direct Anthropic API credits spent by this harness. `supersede.HaikuClassifier` is just a type name (a holdover from its original model target); it wraps whatever `ai.Provider` it's given, so no change is needed in `internal/supersede` itself. This is scoped to the benchmark only — production `ghost resolve`/`ghost supersede` (`cmd/ghost/main.go`'s `buildClassifyProvider`) is untouched and keeps its existing Anthropic-primary/CLI-fallback behavior.

No unit test: the interesting branch (`opencode` missing from PATH) depends on real machine state (`exec.LookPath`), and asserting on that from an automated test would be either a no-op (opencode is already on PATH on this machine) or require mutating `PATH`, which risks masking real lookups for other code running in the same test process. `go build`/`go vet` cover this file; the missing-binary branch is exercised naturally on any machine without `opencode` installed.

- [ ] **Step 1: Create the file**

```go
// bench/memoryagentbench/classifier.go
package main

import (
	"fmt"
	"os/exec"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/supersede"
)

// buildClassifier builds the real supersede classifier backed by the
// `opencode` CLI (ai.NewOpenCodeClientWithBinary) — subscription-billed, no
// ANTHROPIC_API_KEY required. cfg.CLI.OpenCodeBinary overrides the "opencode"
// PATH lookup, matching cmd/ghost/main.go's own opencode-tier resolution.
func buildClassifier() (*supersede.HaikuClassifier, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	binary := "opencode"
	if cfg.CLI.OpenCodeBinary != "" {
		binary = cfg.CLI.OpenCodeBinary
	}
	if _, err := exec.LookPath(binary); err != nil {
		hint := "set cli.opencode_binary in ~/.config/ghost/config.yaml"
		if cfg.CLI.OpenCodeBinary != "" {
			hint = fmt.Sprintf("check that cli.opencode_binary (%s) is a valid, executable path", binary)
		}
		return nil, fmt.Errorf("memoryagentbench requires the `%s` binary on PATH (or %s): %w", binary, hint, err)
	}
	provider := ai.NewFallbackProvider(ai.NewOpenCodeClientWithBinary(binary), nil, false)
	return supersede.NewHaikuClassifier(provider), nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd bench/memoryagentbench && go vet ./...`
Expected: succeeds (no output). Use `go vet`, not `go build`, until Task 7 adds `main.go` — see Task 4's note on why `go build ./...` fails at the link step (missing `func main()`) even when every file is correct.

- [ ] **Step 3: Commit**

```bash
git add bench/memoryagentbench/classifier.go
git commit -s -m "feat(bench): wire the real supersede classifier via opencode for memoryagentbench"
```

---

### Task 6: `evaluate.go` — per-question search and outcome scoring

**Files:**
- Create: `bench/memoryagentbench/evaluate.go`

No unit test here: `evaluateQuestions` needs a real embedding for every question (via `cachedEmbedder`, which calls Ollama on a cache miss), so a hermetic `go test` can't exercise it without either a live Ollama or a committed embedding fixture — neither is worth adding for one integration-shaped function. `answerHit`/`topKHit` (Task 2, already tested) and `seedFacts`/`backdate` (Task 3, already tested) cover the logic pieces that don't need a network call; this file is thin glue over those plus `store.SearchHybridParams`, verified by `go build`/`go vet` now and by a real run (Task 9) end-to-end.

- [ ] **Step 1: Create the file**

```go
// bench/memoryagentbench/evaluate.go
package main

import (
	"context"
	"fmt"

	"github.com/wcatz/ghost/internal/memory"
)

// searchLimit is the retrieval depth passed to SearchHybridParams — how many
// ranked results come back per question.
const searchLimit = 10

// hitK is the top-k cutoff for the "@5" scoring fields below. It must be
// strictly less than searchLimit: SupersedeDemote/recency only ever reorder
// the already-fetched searchLimit-sized result window (internal/memory/vector.go's
// fuseAndRank truncates to the window before either reranking step runs), so
// checking all searchLimit results for a hit is order-independent — baseline
// and with-supersede would always agree. Cutting off at a strictly smaller k
// makes the two conditions capable of actually differing: a reorder can push
// a hit across the hitK boundary even though it stays inside searchLimit.
const hitK = 5

// questionOutcome is one question's hit/miss under both ablation conditions.
type questionOutcome struct {
	QAPairID      string
	BaselineHit1  bool
	BaselineHit5  bool
	SupersedeHit1 bool
	SupersedeHit5 bool
}

// searchQuestion embeds question and runs Ghost's production hybrid search
// under params p.
func searchQuestion(ctx context.Context, store *memory.Store, project, question string, embedder *cachedEmbedder, p memory.SearchParams) ([]memory.Memory, error) {
	qv, err := embedder.Embed(ctx, question)
	if err != nil {
		return nil, fmt.Errorf("embed question: %w", err)
	}
	return store.SearchHybridParams(ctx, project, question, qv, searchLimit, p)
}

// evaluateQuestions scores every question in d twice: once under p with
// SupersedeDemote forced off (baseline) and once under p as given (the
// with-supersede condition — the caller passes memory.DefaultSearchParams(),
// which has SupersedeDemote true) — both against the SAME already-seeded
// store, so this must run after the real supersede pass has applied its
// links (see runDemo in main.go). Deriving baseline from p here, rather than
// accepting two independently-constructed SearchParams, guarantees the two
// conditions differ in exactly the one field these results are labeled
// against — a caller can't accidentally vary anything else and mislabel the
// comparison.
func evaluateQuestions(ctx context.Context, store *memory.Store, project string, d Demo, embedder *cachedEmbedder, p memory.SearchParams) ([]questionOutcome, error) {
	baseline := p
	baseline.SupersedeDemote = false

	out := make([]questionOutcome, len(d.Questions))
	for i, q := range d.Questions {
		baseResults, err := searchQuestion(ctx, store, project, q, embedder, baseline)
		if err != nil {
			return nil, fmt.Errorf("question %d baseline: %w", i, err)
		}
		supResults, err := searchQuestion(ctx, store, project, q, embedder, p)
		if err != nil {
			return nil, fmt.Errorf("question %d with-supersede: %w", i, err)
		}
		out[i] = questionOutcome{
			QAPairID:      d.QAPairIDs[i],
			BaselineHit1:  topKHit(baseResults, d.Answers[i], 1),
			BaselineHit5:  topKHit(baseResults, d.Answers[i], hitK),
			SupersedeHit1: topKHit(supResults, d.Answers[i], 1),
			SupersedeHit5: topKHit(supResults, d.Answers[i], hitK),
		}
	}
	return out, nil
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd bench/memoryagentbench && go vet ./...`
Expected: succeeds (no output). Use `go vet`, not `go build`, until Task 7 adds `main.go` — see Task 4's note on why `go build ./...` fails at the link step (missing `func main()`) even when every file is correct.

- [ ] **Step 3: Commit**

```bash
git add bench/memoryagentbench/evaluate.go
git commit -s -m "feat(bench): add memoryagentbench per-question ablation scoring"
```

---

### Task 7: `main.go` — CLI orchestration

**Files:**
- Create: `bench/memoryagentbench/main.go`

- [ ] **Step 1: Create the file**

```go
// bench/memoryagentbench/main.go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/supersede"
)

// defaultSources is the v1 scope: the two smallest fact_sh haystacks.
// sh_64k/sh_262k are reachable via --sources but not run by default — see
// README.md "Scope and cost".
var defaultSources = []string{"factconsolidation_sh_6k", "factconsolidation_sh_32k"}

// demoResult is the aggregate row printed per demo.
type demoResult struct {
	Source        string  `json:"source"`
	Questions     int     `json:"questions"`
	BaselineAcc1  float64 `json:"baseline_acc@1"`
	BaselineAcc5  float64 `json:"baseline_acc@5"`
	SupersedeAcc1 float64 `json:"supersede_acc@1"`
	SupersedeAcc5 float64 `json:"supersede_acc@5"`
	Candidates    int     `json:"candidates"`
	Confirmed     int     `json:"confirmed"`
	Created       int     `json:"created"`
}

// perQuestionLine is one line of the --out log.
type perQuestionLine struct {
	Source        string `json:"source"`
	QAPairID      string `json:"qa_pair_id"`
	BaselineHit1  bool   `json:"baseline_hit@1"`
	SupersedeHit1 bool   `json:"supersede_hit@1"`
}

func main() {
	dataPath := flag.String("data", "", "path to convert.py's JSONL output (required)")
	sourcesSpec := flag.String("sources", strings.Join(defaultSources, ","), "comma-separated demo sources to run")
	threshold := flag.Float64("threshold", 0.80, "min cosine similarity for a supersede candidate pair (matches ghost supersede's default)")
	ollamaURL := flag.String("ollama", "http://localhost:11434", "Ollama URL for embeddings")
	embedCache := flag.String("embed-cache", "", "append-only embedding cache JSONL")
	outPath := flag.String("out", "", "per-question JSONL results log")
	flag.Parse()

	if *dataPath == "" {
		fmt.Fprintln(os.Stderr, "error: --data is required")
		os.Exit(1)
	}
	wantSources := make(map[string]bool)
	for _, s := range strings.Split(*sourcesSpec, ",") {
		if s = strings.TrimSpace(s); s != "" {
			wantSources[s] = true
		}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	demos, err := loadDemos(*dataPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cls, err := buildClassifier()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	embedder, err := newCachedEmbedder(*ollamaURL, *embedCache)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer embedder.Close() //nolint:errcheck

	var outFile *os.File
	if *outPath != "" {
		outFile, err = os.Create(*outPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		defer outFile.Close() //nolint:errcheck
	}

	ctx := context.Background()
	var results []demoResult
	for _, d := range demos {
		if !wantSources[d.Source] {
			continue
		}
		start := time.Now()
		r, err := runDemo(ctx, d, cls, embedder, *threshold, logger, outFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: demo %s: %v\n", d.Source, err)
			os.Exit(1)
		}
		results = append(results, r)
		fmt.Fprintf(os.Stderr, "%s done in %s\n", d.Source, time.Since(start).Round(time.Second))
	}

	if len(results) == 0 {
		fmt.Fprintln(os.Stderr, "error: no demo matched --sources")
		os.Exit(1)
	}

	fmt.Printf("MemoryAgentBench Conflict_Resolution (fact_sh) — supersede ablation\n\n")
	fmt.Printf("%-28s %5s %10s %10s %12s %12s %6s %6s %6s\n",
		"source", "n", "base@1", "base@5", "supersede@1", "supersede@5", "cand", "conf", "made")
	for _, r := range results {
		fmt.Printf("%-28s %5d %10.3f %10.3f %12.3f %12.3f %6d %6d %6d\n",
			r.Source, r.Questions, r.BaselineAcc1, r.BaselineAcc5,
			r.SupersedeAcc1, r.SupersedeAcc5, r.Candidates, r.Confirmed, r.Created)
	}
}

// runDemo seeds one demo's facts into a fresh in-memory store (list-order
// backdated timestamps), embeds everything, runs the real supersede
// classifier with apply=true, then scores every question under both
// ablation conditions against that same post-supersede store.
func runDemo(ctx context.Context, d Demo, cls *supersede.HaikuClassifier, embedder *cachedEmbedder,
	threshold float64, logger *slog.Logger, outFile *os.File) (demoResult, error) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		return demoResult{}, err
	}
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck // store.Close() closes the underlying db too

	project := "mabench-" + d.Source
	if err := store.EnsureProject(ctx, project, "/bench/"+project, project); err != nil {
		return demoResult{}, err
	}

	facts := splitFacts(d.Context)
	ids, err := seedFacts(ctx, store, db, project, facts)
	if err != nil {
		return demoResult{}, err
	}

	texts := append(append([]string{}, facts...), d.Questions...)
	if err := embedder.EnsureBatch(ctx, texts); err != nil {
		return demoResult{}, fmt.Errorf("embed: %w", err)
	}
	for i, id := range ids {
		vec, err := embedder.Embed(ctx, facts[i])
		if err != nil {
			return demoResult{}, err
		}
		if err := store.StoreEmbedding(ctx, id, vec, embedModel); err != nil {
			return demoResult{}, err
		}
	}

	res, _, err := supersede.Run(ctx, store, cls, project, float32(threshold), true, logger)
	if err != nil {
		return demoResult{}, fmt.Errorf("supersede.Run: %w", err)
	}

	outcomes, err := evaluateQuestions(ctx, store, project, d, embedder, memory.DefaultSearchParams())
	if err != nil {
		return demoResult{}, err
	}

	if outFile != nil {
		for _, o := range outcomes {
			line, err := json.Marshal(perQuestionLine{
				Source: d.Source, QAPairID: o.QAPairID,
				BaselineHit1: o.BaselineHit1, SupersedeHit1: o.SupersedeHit1,
			})
			if err != nil {
				return demoResult{}, err
			}
			if _, err := fmt.Fprintf(outFile, "%s\n", line); err != nil {
				return demoResult{}, err
			}
		}
	}

	return aggregateOutcomes(d.Source, outcomes, res), nil
}

// aggregateOutcomes turns per-question outcomes into the printed summary row.
func aggregateOutcomes(source string, outcomes []questionOutcome, res supersede.Result) demoResult {
	r := demoResult{Source: source, Questions: len(outcomes),
		Candidates: res.Candidates, Confirmed: res.Confirmed, Created: res.Created}
	if len(outcomes) == 0 {
		return r
	}
	var b1, b5, s1, s5 float64
	for _, o := range outcomes {
		if o.BaselineHit1 {
			b1++
		}
		if o.BaselineHit5 {
			b5++
		}
		if o.SupersedeHit1 {
			s1++
		}
		if o.SupersedeHit5 {
			s5++
		}
	}
	n := float64(len(outcomes))
	r.BaselineAcc1, r.BaselineAcc5 = b1/n, b5/n
	r.SupersedeAcc1, r.SupersedeAcc5 = s1/n, s5/n
	return r
}
```

- [ ] **Step 2: Write a test for the one pure function this task adds**

`aggregateOutcomes` is pure (no DB/network) and easy to get an off-by-one wrong in — worth a real test even though the rest of `main.go` is integration-only.

```go
// bench/memoryagentbench/main_test.go
package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

func TestAggregateOutcomes(t *testing.T) {
	outcomes := []questionOutcome{
		{QAPairID: "q0", BaselineHit1: false, BaselineHit5: true, SupersedeHit1: true, SupersedeHit5: true},
		{QAPairID: "q1", BaselineHit1: true, BaselineHit5: true, SupersedeHit1: true, SupersedeHit5: true},
		{QAPairID: "q2", BaselineHit1: false, BaselineHit5: false, SupersedeHit1: false, SupersedeHit5: true},
		{QAPairID: "q3", BaselineHit1: false, BaselineHit5: false, SupersedeHit1: false, SupersedeHit5: false},
	}
	res := supersede.Result{Candidates: 10, Confirmed: 3, Created: 3}

	got := aggregateOutcomes("demo-x", outcomes, res)

	if got.Source != "demo-x" || got.Questions != 4 {
		t.Fatalf("got source=%q questions=%d", got.Source, got.Questions)
	}
	if got.BaselineAcc1 != 0.25 {
		t.Errorf("BaselineAcc1 = %v, want 0.25", got.BaselineAcc1)
	}
	if got.BaselineAcc5 != 0.5 {
		t.Errorf("BaselineAcc5 = %v, want 0.5", got.BaselineAcc5)
	}
	if got.SupersedeAcc1 != 0.5 {
		t.Errorf("SupersedeAcc1 = %v, want 0.5", got.SupersedeAcc1)
	}
	if got.SupersedeAcc5 != 0.75 {
		t.Errorf("SupersedeAcc5 = %v, want 0.75", got.SupersedeAcc5)
	}
	if got.Candidates != 10 || got.Confirmed != 3 || got.Created != 3 {
		t.Errorf("classifier counts not passed through: %+v", got)
	}
}

func TestAggregateOutcomesEmpty(t *testing.T) {
	got := aggregateOutcomes("demo-empty", nil, supersede.Result{})
	if got.Questions != 0 || got.BaselineAcc1 != 0 || got.SupersedeAcc5 != 0 {
		t.Errorf("expected all-zero result for no outcomes, got %+v", got)
	}
}
```

- [ ] **Step 3: Run the test**

Run: `cd bench/memoryagentbench && go test ./... -run TestAggregateOutcomes -v`
Expected: PASS (both tests)

- [ ] **Step 4: Full package build/vet/test**

Run: `cd bench/memoryagentbench && go build ./... && go vet ./... && go test ./...`
Expected: all succeed; `go test` reports `ok` with the full count of tests from Tasks 1–3 and this task (12 tests total: `TestSplitFacts`, `TestSplitFactsIgnoresNonNumberedLines`, `TestSplitFactsEmptyBodyLine`, `TestSplitFactsCRLF`, `TestLoadDemos`, `TestLoadDemosMismatchedLengths`, `TestLoadDemosInvalidJSON`, `TestAnswerHit`, `TestTopKHit`, `TestSeedFactsOrdersTimestamps`, `TestAggregateOutcomes`, `TestAggregateOutcomesEmpty`).

- [ ] **Step 5: Commit**

```bash
git add bench/memoryagentbench/main.go bench/memoryagentbench/main_test.go
git commit -s -m "feat(bench): add memoryagentbench CLI orchestration"
```

---

### Task 8: `convert.py` — HF parquet → JSONL

**Files:**
- Create: `bench/memoryagentbench/convert.py`

- [ ] **Step 1: Create the file**

```python
#!/usr/bin/env python3
"""Convert MemoryAgentBench's Conflict_Resolution parquet into the JSONL
format bench/memoryagentbench's Go harness reads: one demo per line,
single-hop (fact_sh) rows only. See
docs/superpowers/specs/2026-08-20-memoryagentbench-supersede-benchmark-design.md.

Usage:
    pip install pyarrow
    curl -sL -o Conflict_Resolution-00000-of-00001.parquet \
        "https://huggingface.co/datasets/ai-hyz/MemoryAgentBench/resolve/main/data/Conflict_Resolution-00000-of-00001.parquet"
    python3 convert.py --parquet Conflict_Resolution-00000-of-00001.parquet --out demos.jsonl
"""
import argparse
import json
import sys

import pyarrow.parquet as pq


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--parquet", required=True, help="path to the downloaded Conflict_Resolution parquet")
    ap.add_argument("--out", required=True, help="output JSONL path")
    args = ap.parse_args()

    table = pq.read_table(args.parquet)
    rows = table.to_pylist()

    written = 0
    with open(args.out, "w") as f:
        for row in rows:
            meta = row["metadata"] or {}
            source = meta.get("source") or ""
            if not source.startswith("factconsolidation_sh_"):
                continue  # single-hop only — multi-hop needs a generation/decomposition step, out of scope
            questions = row["questions"] or []
            answers = row["answers"] or []
            qa_pair_ids = meta.get("qa_pair_ids") or []
            if len(questions) != len(answers) or len(questions) != len(qa_pair_ids):
                print(f"warning: {source} has mismatched questions/answers/qa_pair_ids lengths, skipping", file=sys.stderr)
                continue
            demo = {
                "source": source,
                "context": row["context"] or "",
                "questions": questions,
                "answers": answers,
                "qa_pair_ids": qa_pair_ids,
            }
            f.write(json.dumps(demo) + "\n")
            written += 1

    print(f"wrote {written} demo(s) to {args.out}", file=sys.stderr)
    if written == 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
```

- [ ] **Step 2: Verify against a real download**

```bash
cd bench/memoryagentbench
pip install --quiet pyarrow
curl -sL -o /tmp/conflict_resolution.parquet \
    "https://huggingface.co/datasets/ai-hyz/MemoryAgentBench/resolve/main/data/Conflict_Resolution-00000-of-00001.parquet"
python3 convert.py --parquet /tmp/conflict_resolution.parquet --out /tmp/demos.jsonl
wc -l /tmp/demos.jsonl
python3 -c "
import json
with open('/tmp/demos.jsonl') as f:
    demos = [json.loads(l) for l in f]
sources = sorted(d['source'] for d in demos)
print('sources:', sources)
assert sources == [
    'factconsolidation_sh_262k', 'factconsolidation_sh_32k',
    'factconsolidation_sh_64k', 'factconsolidation_sh_6k',
], sources
for d in demos:
    assert len(d['questions']) == len(d['answers']) == len(d['qa_pair_ids']) == 100, d['source']
print('OK: 4 fact_sh demos, 100 questions each, lengths match')
"
```

Expected: `wc -l` reports `4`; the Python check prints `sources: ['factconsolidation_sh_262k', 'factconsolidation_sh_32k', 'factconsolidation_sh_64k', 'factconsolidation_sh_6k']` then `OK: 4 fact_sh demos, 100 questions each, lengths match`. (`mh_*` rows are correctly excluded — only 4 of the split's 8 rows survive the `factconsolidation_sh_` filter.)

- [ ] **Step 3: Commit**

```bash
git add bench/memoryagentbench/convert.py
git commit -s -m "feat(bench): add MemoryAgentBench parquet-to-JSONL converter"
```

---

### Task 9: `README.md`

**Files:**
- Create: `bench/memoryagentbench/README.md`

- [ ] **Step 1: Create the file**

```markdown
# MemoryAgentBench Conflict-Resolution — supersede ablation

Runs Ghost's real `ghost supersede` pipeline (the actual Haiku classifier,
real Anthropic API calls) against MemoryAgentBench's `Conflict_Resolution`
split ([HF: `ai-hyz/MemoryAgentBench`](https://huggingface.co/datasets/ai-hyz/MemoryAgentBench),
ICLR 2026), and scores retrieval before/after supersede links exist. See
[the design doc](../../docs/superpowers/specs/2026-08-20-memoryagentbench-supersede-benchmark-design.md)
for the full rationale.

Single-hop (`fact_sh`) only — multi-hop (`fact_mh`) needs a query-decomposition
step this harness deliberately doesn't have (retrieval-only, no generation).

## Setup

```bash
pip install pyarrow
curl -sL -o Conflict_Resolution-00000-of-00001.parquet \
    "https://huggingface.co/datasets/ai-hyz/MemoryAgentBench/resolve/main/data/Conflict_Resolution-00000-of-00001.parquet"
python3 convert.py --parquet Conflict_Resolution-00000-of-00001.parquet --out demos.jsonl
```

## Run

```bash
# requires the `opencode` CLI on PATH (or cli.opencode_binary in
# ~/.config/ghost/config.yaml) — no ANTHROPIC_API_KEY needed
go run ./bench/memoryagentbench --data demos.jsonl \
    --embed-cache ~/.cache/ghost-bench/mabench-embed-cache.jsonl \
    --out per-question.jsonl
```

Requires a local Ollama serving `nomic-embed-text:v1.5` (the same model Ghost
uses in production) — pass `--ollama <url>` if it's not on `localhost:11434`.

## Scope and cost

- Default `--sources` runs `factconsolidation_sh_6k,factconsolidation_sh_32k`
  only — a few hundred facts each, so the classifier sees at most
  `facts × 8` (`supersede.maxNeighbors`) candidate pairs, deduped: at most
  that many `opencode run` subprocess calls.
- `factconsolidation_sh_64k`/`factconsolidation_sh_262k` are reachable via
  `--sources factconsolidation_sh_64k` etc., but scale the haystack ~5x/40x —
  expect a proportional jump in candidate pairs and `opencode` calls (each is
  a subprocess spawn, so wall-clock — not API cost — is the constraint at
  that scale). Not run by default.
- No CI gate: every run shells out to `opencode` once per candidate pair.
- `ghost resolve` is not exercised here — its keyword prefilter
  (`"resolved"`, `"shipped"`, `"deprecated"`, etc.) doesn't match
  FactConsolidation's neutral factual sentences.

## Reading the table

```
source                          n     base@1     base@5  supersede@1  supersede@5   cand   conf   made
factconsolidation_sh_6k       100      0.XXX      0.XXX        0.XXX        0.XXX    NNN    NNN    NNN
```

`base@k` scores plain hybrid search (`SupersedeDemote: false`); `supersede@k`
scores the same questions with `SupersedeDemote: true` (production default)
after the real classifier's verdicts are applied — both against the same
post-supersede store, so the difference isolates what the ranking switch
alone contributes. `cand`/`conf`/`made` are `supersede.Result`'s
`Candidates`/`Confirmed`/`Created`: how many same-subject pairs the
classifier was offered, how many it called SUPERSEDES, and how many links
were actually written.

Numbers get published in `docs/benchmarks.md` under a new phase once there's
a real run to report — see the design doc.
```

- [ ] **Step 2: Commit**

```bash
git add bench/memoryagentbench/README.md
git commit -s -m "docs(bench): add memoryagentbench README"
```

---

### Task 10: Whole-repo verification

**Files:** none (verification only)

- [ ] **Step 1: Full build, vet, and test across the repo**

Run: `cd /home/wayne/git/ghost && go build ./... && go vet ./... && go test ./...`
Expected: all packages build; `go vet` reports nothing; `go test` reports `ok` for every package including the new `github.com/wcatz/ghost/bench/memoryagentbench` (9+ tests, per Task 7 Step 4's count).

- [ ] **Step 2: Confirm the new files are tracked and nothing stray was picked up**

Run: `git status --short`
Expected: clean (everything from Tasks 1–9 already committed); if anything shows here, it's a leftover from a step above — `git add` and commit it with a message matching that step, don't fold it into an unrelated commit.

- [ ] **Step 3: Review the commit sequence**

Run: `git log --oneline main..HEAD`
Expected: one commit per task (spec doc + 9 implementation commits), each scoped to one file/concern, no merge commits.

No further commit — this task only verifies what Tasks 1–9 already committed.
