package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// TestRunSupersedeEmbedsTheCorpusItHasToSearch is issue #716 at the layer the
// issue was filed against: the real `ghost supersede` path, over a store whose
// memories have no vectors.
//
// The candidate scan scores a pair by cosine, so a memory it has no vector for
// is proposed as no new candidate by that scan
// (internal/supersede.SelectCandidates) — a correct rule, applied to a state that
// is simply not there yet. The vectors are written by the embedding worker, and
// that worker lives in `ghost mcp`: a
// long-lived daemon this one-shot pass has no part in. A user who saves two
// notes and runs the pass in the same breath therefore gets a clean, empty
// report — "0 candidate pairs" — for a pair that was in front of them, and
// nothing anywhere says the index had not caught up.
//
// So the pass embeds what it cannot search for. That is not the daemon's
// business to do on the daemon's schedule: this process has to be able to answer
// the question it was asked, and the corpus it was asked about is the corpus
// the user just wrote.
//
// The two notes get ORTHOGONAL vectors from the stub (one per note's own
// keyword), so no pair clears the similarity threshold and the pass spends no
// classify call: this is about what the pass can search, not about a verdict,
// and a test that needed a harness answer would be testing two things at once.
// The stub `opencode` on PATH is only there because the pass resolves its
// classifier before it scans anything, and an unresolvable one is an exit.
func TestRunSupersedeEmbedsTheCorpusItHasToSearch(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)

	// The endpoint, and a harness the pass can resolve. Alive() at "/",
	// EmbedDocument() at "/api/embed"; the two notes are told apart by the
	// keyword in the text, so the vectors are [1,0] and [0,1] whatever prefix
	// the model would add.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusOK)
		case "/api/embed":
			var req struct {
				Input string `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			vec := "0,1"
			if strings.Contains(strings.ToLower(req.Input), "alpha") {
				vec = "1,0"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"embeddings":[[` + vec + `]]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	binDir := t.TempDir()
	harness := filepath.Join(binDir, "opencode")
	writeExecutable(t, harness, "#!/bin/sh\nexit 0\n")
	// Both the PATH and the configured override, because the isolation helper
	// pins the override at a path that does not exist and the pass resolves the
	// classifier through it before it scans anything.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("config file path: %v", err)
	}
	yaml := "embedding:\n  enabled: true\n  ollama_url: " + srv.URL + "\n  model: test-model\n  dimensions: 2\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Two notes, neither of them in the vector index — which is the state a
	// save leaves behind for as long as the daemon takes to notice it.
	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	ids := seedUnembeddedPair(t, dbPath)

	origArgs := os.Args
	os.Args = []string{origArgs[0], "supersede", "projy", "--source", "opencode"}
	t.Cleanup(func() { os.Args = origArgs })

	runSupersede()

	// What the candidate scan reads. GetEmbedding is the same call the scan
	// makes, identity check included, so a vector written under another model's
	// identity reads as absent here exactly as it would there — and so does a row
	// that was never written at all, which is the state this test is about.
	store := openStore(t, dbPath)
	for _, id := range ids {
		vec, err := store.GetEmbedding(context.Background(), id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("GetEmbedding(%s): %v", id, err)
		}
		if len(vec) == 0 {
			t.Fatalf("memory %s still has no vector after `ghost supersede`: the pass read a corpus it could not score and reported an empty result as if the corpus held no near-duplicate pair in it", id)
		}
	}
}

// TestSupersedeIndexNotesNamesTheRemedyThatApplies: the two lines the pass adds
// about the vector index are one story, and the story has three endings. A pass
// that cut ITSELF off on the pre-scan budget is not a daemon that is behind, and
// an index nobody is filling at all has no worker to go and look at — so the
// remedy in the line is the part that has to be right, and each shape is pinned.
//
// The scope of the claim is pinned here too: both lines are about NEW candidates,
// because the reclassified count printed above them is not bounded by the scan.
func TestSupersedeIndexNotesNamesTheRemedyThatApplies(t *testing.T) {
	// Nothing to say: an ordinary pass finds every memory scorable and wrote
	// nothing. An unconditional "0" line would bury the answer to the operator's
	// question, which is that there was nothing to do.
	if got := supersedeIndexNotes(supersedeIndexFacts{embeddingOn: true}); got != "" {
		t.Errorf("an ordinary pass printed %q", got)
	}

	// Wrote some, finished the batch: the vectors are the work this run did, and
	// the consequence is scoped to the scan.
	note := supersedeIndexNotes(supersedeIndexFacts{embedded: 2, embeddingOn: true})
	if !strings.Contains(note, "2 memories embedded for this pass") {
		t.Errorf("the line does not count what it wrote: %q", note)
	}
	if !strings.Contains(note, "no new candidate by the vector scan") {
		t.Errorf("the line does not scope its consequence to the scan: %q", note)
	}
	if strings.Contains(note, "not a candidate for anything") {
		t.Errorf("the line claims an unvectorised memory is in no pair at all, which a reclassified edge contradicts: %q", note)
	}

	// Cut off by its own budget: the remedy is a re-run, and the worker is not
	// named, because nothing here was waiting on the worker.
	cut := supersedeIndexNotes(supersedeIndexFacts{
		embedded: 40, unscored: 10, embeddingOn: true, budgetExpired: true,
	})
	if !strings.Contains(cut, "re-run `ghost supersede`") {
		t.Errorf("a pass that stopped early does not say the batch continues: %q", cut)
	}
	if strings.Contains(cut, "ghost mcp status") {
		t.Errorf("a pass that cut itself off sends the operator after a daemon that was never behind: %q", cut)
	}
	if !strings.Contains(cut, "10 memories had no vector") {
		t.Errorf("the line does not count what the scan could not read: %q", cut)
	}

	// The daemon is behind, which is the ordinary remaining case.
	behind := supersedeIndexNotes(supersedeIndexFacts{unscored: 143, embeddingOn: true})
	if !strings.Contains(behind, "embedding worker") || !strings.Contains(behind, "ghost mcp status") {
		t.Errorf("the line does not point at what fills the index: %q", behind)
	}
	// And with embedding off there is no worker to blame, which is a different
	// message rather than the same one with a missing tail.
	off := supersedeIndexNotes(supersedeIndexFacts{unscored: 2, embeddingOn: false})
	if !strings.Contains(off, "embedding is disabled") {
		t.Errorf("a pass with embedding off still points at the worker: %q", off)
	}
	if strings.Contains(off, "re-run `ghost supersede`") {
		t.Errorf("a re-run cannot help when nothing is filling the index: %q", off)
	}

	// The consequence is stated in the same words wherever it appears, so the two
	// lines cannot contradict each other or the count above them.
	for _, f := range []supersedeIndexFacts{
		{embedded: 2, embeddingOn: true},
		{embedded: 1, embeddingOn: true},
		{embedded: 40, unscored: 10, embeddingOn: true, budgetExpired: true},
		{unscored: 1, embeddingOn: true},
		{unscored: 1, embeddingOn: false},
	} {
		got := supersedeIndexNotes(f)
		if strings.Contains(got, "in no pair this run considered") || strings.Contains(got, "not a candidate for anything") {
			t.Errorf("%+v printed a claim the reclassify half can contradict: %q", f, got)
		}
	}
	// Singular and plural both have to read as English: a report line is prose.
	if one := supersedeIndexNotes(supersedeIndexFacts{embedded: 1, embeddingOn: true}); !strings.Contains(one, "1 memory embedded") {
		t.Errorf("the singular line reads wrong: %q", one)
	}
	if one := supersedeIndexNotes(supersedeIndexFacts{unscored: 1, embeddingOn: true}); !strings.Contains(one, "1 memory had no vector") {
		t.Errorf("the singular unscored line reads wrong: %q", one)
	}
}

// TestRunSupersedeSaysSoWhenItCannotReadTheCorpus is the same pass with the
// endpoint gone, and it is the case a formatter test cannot reach: an index that
// is merely current and a pass that could not refresh it both leave the embed
// call with nothing to report, and only the SCAN can tell them apart. So this
// drives the real command and reads what the operator would see.
//
// Without the line, a run like this prints "0 candidate pairs" over a project the
// pass could not read a single memory of — the same report a project with no
// near-duplicate pair produces, and the reason #716 was invisible.
func TestRunSupersedeSaysSoWhenItCannotReadTheCorpus(t *testing.T) {
	dataHome := isolatedLifecycleEnv(t)

	binDir := t.TempDir()
	harness := filepath.Join(binDir, "opencode")
	writeExecutable(t, harness, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

	// Port 1 has nothing listening, so Alive() fails exactly as an Ollama that is
	// down does — no stub to keep running and nothing to shut down afterwards.
	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("config file path: %v", err)
	}
	yaml := "embedding:\n  enabled: true\n  ollama_url: http://127.0.0.1:1\n  model: test-model\n  dimensions: 2\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	dbPath := filepath.Join(dataHome, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	ids := seedUnembeddedPair(t, dbPath)

	origArgs := os.Args
	os.Args = []string{origArgs[0], "supersede", "projy", "--source", "opencode"}
	t.Cleanup(func() { os.Args = origArgs })

	out := captureStdout(t, runSupersede)

	// The count is the scan's, so it names the two notes it could not read and
	// points at the thing that reports coverage.
	if !strings.Contains(out, fmt.Sprintf("%d memories had no vector", len(ids))) {
		t.Errorf("a pass that could read none of the corpus did not say so:\n%s", out)
	}
	if !strings.Contains(out, "ghost mcp status") {
		t.Errorf("the line does not point at the coverage report:\n%s", out)
	}
	// And the embed line must stay silent rather than claim the work was done.
	if strings.Contains(out, "embedded for this pass") {
		t.Errorf("the pass claims vectors it could not write:\n%s", out)
	}
	// The headline alone would be indistinguishable from an empty project.
	if !strings.Contains(out, "0 candidate pairs") {
		t.Errorf("the pass did not complete its report, so the line above was not the reason this failed:\n%s", out)
	}
}

// TestEmbedSupersedeCorpusStopsAtItsBudget: the batch bound is not a bound on
// TIME, and this is the shape that makes that matter — an endpoint that answers
// its liveness probe and then stalls every embed. The client allows 30s per
// request, so 50 memories would be half an hour of blocking before the candidate
// scan started, with nothing printed for any of it, in a command that used to
// begin scanning immediately.
//
// The budget is a parameter precisely so this can be measured in milliseconds:
// running out is not a failure and not a silence, it is the same honest outcome as
// a bound that was too small.
func TestEmbedSupersedeCorpusStopsAtItsBudget(t *testing.T) {
	isolatedLifecycleEnv(t)
	dbPath, ids := seedUnembeddedCorpus(t, 3)
	store := openStore(t, dbPath)

	// A wedge, not an error: the request is left hanging, which is exactly the
	// case the client's own 30s timeout is wide open for. The release channel
	// rather than r.Context(), because a handler that never reads the body gives
	// the server no way to notice the client walking away — so waiting on the
	// request context here wedges the FAKE, not just the product, and the
	// teardown would hang behind it. Defers run last-in-first-out, so the wedge
	// is released before the server is closed.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		<-release
	}))
	defer srv.Close()
	defer close(release)

	cfg := &config.Config{}
	cfg.Embedding.Enabled = true
	cfg.Embedding.OllamaURL = srv.URL
	cfg.Embedding.Model = "test-model"
	cfg.Embedding.Dimensions = 2

	const budget = 150 * time.Millisecond
	start := time.Now()
	embedded, expired := embedSupersedeCorpus(context.Background(), cfg, store, "projy", budget, slog.New(slog.DiscardHandler))
	elapsed := time.Since(start)

	if embedded != 0 {
		t.Errorf("embedded %d against a stalled endpoint, want 0", embedded)
	}
	// The expiry is the fact the report needs: it is what makes this a pass that
	// stopped early rather than a daemon that is behind, and the two have
	// different remedies.
	if !expired {
		t.Error("the budget expired and the caller was not told, so the report would blame the worker")
	}
	// Generously over the budget, and nowhere near the client's 30s per request:
	// a ceiling that only bites at the client timeout is not a ceiling.
	if elapsed > 5*time.Second {
		t.Errorf("the pre-scan took %v against its %v budget", elapsed, budget)
	}
	// And the pass is left in the state its report can describe: nothing vectored,
	// which is what supersedeUnscoredNote then reports.
	for _, id := range ids {
		if vec, err := store.GetEmbedding(context.Background(), id); err == nil && len(vec) > 0 {
			t.Errorf("memory %s was vectored by a stalled endpoint", id)
		}
	}
}

// seedUnembeddedCorpus writes n memories into a fresh project in a fresh store
// and returns the database path and their ids. None has a vector: that is the
// state a save leaves behind, and the only state in which the pre-scan has
// anything to do at all.
func seedUnembeddedCorpus(t *testing.T, n int) (string, []string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	store := openStore(t, dbPath)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	words := []string{"alpha", "beta", "gamma", "delta"}
	var ids []string
	for _, word := range words[:n] {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Source: "mcp", Importance: 0.7,
			Content: fmt.Sprintf("the %s relay holds its queue for thirty seconds before draining", word),
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	store.Close() //nolint:errcheck
	return dbPath, ids
}

// captureStdout runs fn with os.Stdout redirected and returns what it printed.
// The report is a few hundred bytes, far inside a pipe buffer, so nothing here
// has to read concurrently with the writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	done := make(chan string, 1)
	go func() {
		var sb strings.Builder
		_, _ = io.Copy(&sb, r)
		done <- sb.String()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// seedUnembeddedPair writes two memories into a fresh project at dbPath and
// returns their ids. Neither has a vector: seeding one would test a different
// state than the one a save leaves behind.
func seedUnembeddedPair(t *testing.T, dbPath string) []string {
	t.Helper()
	store := openStore(t, dbPath)
	ctx := context.Background()
	if err := store.EnsureProject(ctx, "projy", "/tmp/projy", "projy"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	var ids []string
	for _, content := range []string{
		"the alpha relay holds its queue for thirty seconds before draining",
		"the beta relay holds its queue for thirty seconds before draining",
	} {
		id, err := store.Create(ctx, "projy", memory.Memory{
			Category: "architecture", Content: content, Source: "mcp", Importance: 0.7,
		})
		if err != nil {
			t.Fatalf("create memory: %v", err)
		}
		ids = append(ids, id)
	}
	store.Close() //nolint:errcheck
	return ids
}

// openStore opens a writable store handle for a test, registered for closing.
func openStore(t *testing.T, dbPath string) *memory.Store {
	t.Helper()
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := memory.NewStore(db, nil)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// writeExecutable writes a shell script and makes it runnable, for the fake
// harness a command resolves before it does anything else.
func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
