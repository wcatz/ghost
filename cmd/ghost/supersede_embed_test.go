package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
)

// TestRunSupersedeEmbedsTheCorpusItHasToSearch is issue #716 at the layer the
// issue was filed against: the real `ghost supersede` path, over a store whose
// memories have no vectors.
//
// The candidate scan scores a pair by cosine, so a memory it has no vector for
// is not a candidate for anything (internal/supersede.SelectCandidates) — a
// correct rule, applied to a state that is simply not there yet. The vectors are
// written by the embedding worker, and that worker lives in `ghost mcp`: a
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

// TestSupersedeEmbedNoteSaysNothingWhenThereWasNothingToDo: the report line is
// about work this run did, and a pass that found the index already current must
// print nothing at all — an unconditional line would put "embedded 0 memories"
// on every ordinary pass, where the answer to the operator's question is that
// there was nothing to do.
func TestSupersedeEmbedNoteSaysNothingWhenThereWasNothingToDo(t *testing.T) {
	if note := supersedeEmbedNote(0); note != "" {
		t.Errorf("a pass that embedded nothing printed %q", note)
	}
	note := supersedeEmbedNote(2)
	if !strings.Contains(note, "2") {
		t.Errorf("the note does not count what it did: %q", note)
	}
	// And it must not read as an error: the vectors are there now, which is the
	// whole point of doing the work inside the pass.
	if !strings.Contains(strings.ToLower(note), "embedded") {
		t.Errorf("the note does not say what it did: %q", note)
	}
}

// TestSupersedeUnscoredNoteNamesWhatThePassCouldNotRead: the two counts on a
// report answer different questions, and only the second one closes the gap a
// reader cannot see. "embedded 2" says what the run did; "2 had no vector when
// the pass scanned" says how much of the project the totals beside it cover. A
// pass that found every memory scorable says nothing at all, and one running
// with embedding disabled has to name THAT rather than send the operator to a
// worker that is not running.
func TestSupersedeUnscoredNoteNamesWhatThePassCouldNotRead(t *testing.T) {
	if note := supersedeUnscoredNote(0, true); note != "" {
		t.Errorf("a pass that scored every memory printed %q", note)
	}
	note := supersedeUnscoredNote(2, true)
	if !strings.Contains(note, "2") {
		t.Errorf("the note does not count what was unscored: %q", note)
	}
	// It must name the CONSEQUENCE correctly and narrowly. The consequence is
	// about NEW candidates: a memory with no vector cannot be proposed by the
	// scan, but it CAN still turn up in a reclassified pair, because that half of
	// the pass re-reads live edges from their link rows. Claiming the stronger
	// "in no pair this run considered" is what would contradict the
	// "N reclassified" line printed just above it.
	if !strings.Contains(note, "proposed no new candidate") {
		t.Errorf("the note does not say what an unscored memory means for the pass: %q", note)
	}
	if strings.Contains(note, "in no pair this run considered") {
		t.Errorf("the note claims an unscored memory is in no pair at all, which a reclassified edge above it can contradict: %q", note)
	}
	// With embedding off, naming the worker is a dead end: nothing is running to
	// keep the index current, and that is the thing to say.
	if off := supersedeUnscoredNote(2, false); !strings.Contains(off, "embedding is disabled") {
		t.Errorf("a pass with embedding off still points at the worker that maintains the index: %q", off)
	}
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
