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

// runSupersedeOverUnembeddedPair is the real `ghost supersede` path over a store
// whose two notes have no vectors, with the embedding endpoint at ollamaURL and a
// harness stub the pass can resolve (an unresolvable classifier is an exit). The
// pass is about what it can search, so no classify call is expected.
func runSupersedeOverUnembeddedPair(t *testing.T, ollamaURL string) (out, dbPath string, ids []string) {
	t.Helper()
	dataHome := isolatedLifecycleEnv(t)
	binDir := t.TempDir()
	harness := filepath.Join(binDir, "opencode")
	writeExecutable(t, harness, "#!/bin/sh\nexit 0\n")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GHOST_CLI_OPENCODE_BINARY", harness)

	cfgPath, err := config.ConfigFilePath()
	if err != nil {
		t.Fatalf("config file path: %v", err)
	}
	yaml := "embedding:\n  enabled: true\n  ollama_url: " + ollamaURL + "\n  model: test-model\n  dimensions: 2\n"
	if err := os.WriteFile(cfgPath, []byte(yaml), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	dbPath = filepath.Join(dataHome, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatalf("mkdir data dir: %v", err)
	}
	ids = seedUnembeddedPair(t, dbPath)

	origArgs := os.Args
	os.Args = []string{origArgs[0], "supersede", "projy", "--source", "opencode"}
	t.Cleanup(func() { os.Args = origArgs })
	return captureStdout(t, runSupersede), dbPath, ids
}

// TestRunSupersedeEmbedsTheCorpusItHasToSearch is issue #716: the candidate scan
// scores by cosine and proposes nothing for a memory with no vector, and the
// vectors are written by the embedding worker in `ghost mcp`, so notes saved a
// moment ago produced a clean "0 candidate pairs". The pass embeds what it
// cannot search for. The stub gives the notes orthogonal vectors (by keyword),
// so no pair clears the threshold.
func TestRunSupersedeEmbedsTheCorpusItHasToSearch(t *testing.T) {
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

	_, dbPath, ids := runSupersedeOverUnembeddedPair(t, srv.URL)
	// GetEmbedding is the call the scan makes, identity check included.
	store := openStore(t, dbPath)
	for _, id := range ids {
		vec, err := store.GetEmbedding(context.Background(), id)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("GetEmbedding(%s): %v", id, err)
		}
		if len(vec) == 0 {
			t.Fatalf("memory %s still has no vector after `ghost supersede`", id)
		}
	}
}

// TestSupersedeIndexNotesNamesTheRemedyThatApplies: the index lines have three
// endings and the remedy must be the one that applies. A pass cut off by its own
// budget is not a daemon that is behind; with embedding off there is no worker to
// blame. Both lines are scoped to NEW candidates (a reclassified edge is not
// bounded by the scan).
func TestSupersedeIndexNotesNamesTheRemedyThatApplies(t *testing.T) {
	if got := supersedeIndexNotes(supersedeIndexFacts{embeddingOn: true}); got != "" {
		t.Errorf("an ordinary pass printed %q", got)
	}
	for _, tc := range []struct {
		name  string
		facts supersedeIndexFacts
		want  []string
		avoid []string
	}{
		{"wrote some", supersedeIndexFacts{embedded: 2, embeddingOn: true},
			[]string{"2 memories embedded for this pass", "no new candidate by the vector scan"}, []string{"not a candidate for anything"}},
		{"singular", supersedeIndexFacts{embedded: 1, embeddingOn: true}, []string{"1 memory embedded"}, nil},
		{"cut off by its budget", supersedeIndexFacts{embedded: 40, unscored: 10, embeddingOn: true, budgetExpired: true},
			[]string{"re-run `ghost supersede`", "10 memories had no vector"}, []string{"ghost mcp status"}},
		{"the daemon is behind", supersedeIndexFacts{unscored: 143, embeddingOn: true},
			[]string{"embedding worker", "ghost mcp status"}, nil},
		{"singular unscored", supersedeIndexFacts{unscored: 1, embeddingOn: true}, []string{"1 memory had no vector"}, nil},
		{"embedding off", supersedeIndexFacts{unscored: 2, embeddingOn: false},
			[]string{"embedding is disabled"}, []string{"re-run `ghost supersede`", "in no pair this run considered"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := supersedeIndexNotes(tc.facts)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("the line does not say %q: %q", w, got)
				}
			}
			for _, w := range tc.avoid {
				if strings.Contains(got, w) {
					t.Errorf("the line says %q: %q", w, got)
				}
			}
		})
	}
}

// TestRunSupersedeSaysSoWhenItCannotReadTheCorpus: with the endpoint gone (port 1
// has nothing listening) only the SCAN can tell "index current" from "pass could
// not refresh it", so the real command must say so; without the line this printed
// the same "0 candidate pairs" as an empty project (the reason #716 was invisible).
func TestRunSupersedeSaysSoWhenItCannotReadTheCorpus(t *testing.T) {
	out, _, ids := runSupersedeOverUnembeddedPair(t, "http://127.0.0.1:1")
	for _, want := range []string{fmt.Sprintf("%d memories had no vector", len(ids)), "ghost mcp status", "0 candidate pairs"} {
		if !strings.Contains(out, want) {
			t.Errorf("the report does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "embedded for this pass") {
		t.Errorf("the pass claims vectors it could not write:\n%s", out)
	}
}

// TestEmbedSupersedeCorpusStopsAtItsBudget: the batch bound is not a bound on
// time. An endpoint that answers its liveness probe and stalls every embed would
// block 30s per request before the scan started, so running out of budget is
// reported (not silent) and measured in milliseconds.
func TestEmbedSupersedeCorpusStopsAtItsBudget(t *testing.T) {
	isolatedLifecycleEnv(t)
	dbPath, ids := seedUnembeddedCorpus(t, 3)
	store := openStore(t, dbPath)

	// A release channel rather than r.Context(): a handler that never reads the
	// body cannot notice the client leaving, and teardown would hang behind it.
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

	start := time.Now()
	embedded, expired := embedSupersedeCorpus(context.Background(), cfg, store, "projy", 150*time.Millisecond, slog.New(slog.DiscardHandler))
	if embedded != 0 || !expired {
		t.Errorf("embedded/expired = %d/%v, want 0/true so the report blames the pass, not the worker", embedded, expired)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("the pre-scan took %v against a 150ms budget", elapsed)
	}
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
