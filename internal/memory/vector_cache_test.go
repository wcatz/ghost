package memory

// #1031: the vector corpus cache and the vector_epoch counter that keeps it
// honest. The properties under test are the ones the cache can get wrong
// silently: a stale answer after a write (this process's or another's), a
// counter that moves on writes the vector query does not read, an answer that
// differs from the uncached path's, an unbounded copy, and the #556 rule that
// scoring runs with the store lock released.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	cacheProj  = "test-proj"
	cacheOther = "other-proj"
	cacheDims  = 8
)

// cacheVec is a deterministic vector of the corpus: distinct per seed, so no two
// rows tie on a cosine and the order of a result does not depend on the order
// the database yielded the rows in.
func cacheVec(seed uint64) []float32 {
	rng := rand.New(rand.NewPCG(seed, 7))
	v := make([]float32, cacheDims)
	for i := range v {
		// Biased positive so every pair of rows has a positive cosine: a row the
		// vector floor drops is not one a test can watch appear or disappear.
		v[i] = float32(rng.NormFloat64()) + 4
	}
	return v
}

// cacheStore opens a file-backed store (so a second handle can share the file),
// with the embedding identity configured and two projects.
func cacheStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cache.db")
	s := openCacheStoreAt(t, path)
	ctx := context.Background()
	for _, p := range []string{cacheProj, cacheOther} {
		if err := s.EnsureProject(ctx, p, "/"+p, p); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
	}
	return s, path
}

func openCacheStoreAt(t *testing.T, path string) *Store {
	t.Helper()
	db, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	s := NewStore(db, nil)
	s.SetEmbeddingIdentity(identityCurrent)
	return s
}

// cacheMemory creates a memory and, when seed is non-zero, embeds it.
func cacheMemory(t *testing.T, s *Store, project, content string, seed uint64, scope map[string]string) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, project, Memory{Category: "fact", Content: content, Source: "manual", Scope: scope})
	if err != nil {
		t.Fatalf("Create %q: %v", content, err)
	}
	if seed != 0 {
		if err := s.StoreEmbedding(ctx, id, cacheVec(seed), identityCurrent); err != nil {
			t.Fatalf("StoreEmbedding %q: %v", content, err)
		}
	}
	return id
}

// cacheCorpus seeds a handful of filler rows (seeds 101..) so the target is one
// row among several.
func cacheCorpus(t *testing.T, s *Store) {
	t.Helper()
	for i := range 6 {
		cacheMemory(t, s, cacheProj, fmt.Sprintf("filler %d", i), uint64(101+i), nil)
	}
}

func epochOf(t *testing.T, s *Store) int64 {
	t.Helper()
	e, ok := readVectorEpoch(context.Background(), s.db)
	if !ok {
		t.Fatal("the database has no readable vector_epoch")
	}
	return e
}

func cacheFind(res []ScoredMemory, id string) (ScoredMemory, bool) {
	for _, r := range res {
		if r.MemoryID == id {
			return r, true
		}
	}
	return ScoredMemory{}, false
}

// withCacheCap lowers (or raises) the cache bound for one test.
func withCacheCap(t *testing.T, n int64) {
	t.Helper()
	old := vectorCacheMaxBytes
	vectorCacheMaxBytes = n
	t.Cleanup(func() { vectorCacheMaxBytes = old })
}

// A vector-affecting write of every kind invalidates the cache: the next search
// reflects it, the counter moved, and the corpus was reloaded. Each kind runs
// twice — written through the searching store's own handle, and through a second
// handle on the same file, which is what another process's write looks like to
// this one.
func TestVectorEpochInvalidatesOnEveryVectorAffectingWrite(t *testing.T) {
	ctx := context.Background()
	q := cacheVec(1)

	type kind struct {
		name string
		// setup creates the target in its "before" state and returns its id.
		setup func(t *testing.T, s *Store) string
		// search is the query whose answer the write changes.
		search func(s *Store) ([]ScoredMemory, error)
		// write performs the vector-affecting write through w.
		write func(t *testing.T, w *Store, id string)
		// reflects checks the answer after the write.
		reflects func(t *testing.T, before, after []ScoredMemory, id string)
	}
	inProject := func(s *Store) ([]ScoredMemory, error) { return s.SearchVector(ctx, cacheProj, q, 20) }
	rawExec := func(t *testing.T, w *Store, query string, args ...any) {
		t.Helper()
		if _, err := w.db.ExecContext(ctx, query, args...); err != nil {
			t.Fatalf("%s: %v", query, err)
		}
	}

	kinds := []kind{
		{
			name:   "embedding insert",
			setup:  func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 0, nil) },
			search: inProject,
			write: func(t *testing.T, w *Store, id string) {
				if err := w.StoreEmbedding(ctx, id, cacheVec(2), identityCurrent); err != nil {
					t.Fatal(err)
				}
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				if _, ok := cacheFind(before, id); ok {
					t.Error("the target was searchable before it had an embedding")
				}
				if _, ok := cacheFind(after, id); !ok {
					t.Error("a new embedding is not in the next search: the cache served the old corpus")
				}
			},
		},
		{
			name:   "embedding update",
			setup:  func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 3, nil) },
			search: inProject,
			write: func(t *testing.T, w *Store, id string) {
				if err := w.StoreEmbedding(ctx, id, cacheVec(4), identityCurrent); err != nil {
					t.Fatal(err)
				}
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				b, _ := cacheFind(before, id)
				a, ok := cacheFind(after, id)
				if !ok {
					t.Fatal("the target vanished after its embedding was rewritten")
				}
				if want := cosineSimilarity(q, cacheVec(4)); a.Score != want || a.Score == b.Score {
					t.Errorf("score after the rewrite = %v (before %v), want %v: the cache served the old vector", a.Score, b.Score, want)
				}
			},
		},
		{
			name:   "embedding delete",
			setup:  func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 5, nil) },
			search: inProject,
			write: func(t *testing.T, w *Store, id string) {
				if err := w.DeleteEmbedding(ctx, id); err != nil {
					t.Fatal(err)
				}
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				if _, ok := cacheFind(before, id); !ok {
					t.Fatal("the target was not searchable before the delete")
				}
				if _, ok := cacheFind(after, id); ok {
					t.Error("a deleted embedding is still returned: the cache served the old corpus")
				}
			},
		},
		{
			name:   "resolve",
			setup:  func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 6, nil) },
			search: inProject,
			write: func(t *testing.T, w *Store, id string) {
				if got, err := w.MarkResolved(ctx, cacheProj, []string{id}, Provenance{}); err != nil || len(got) != 1 {
					t.Fatalf("MarkResolved = %v, %v", got, err)
				}
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				b, _ := cacheFind(before, id)
				a, ok := cacheFind(after, id)
				if !ok || b.Resolved || !a.Resolved {
					t.Errorf("resolved flag before/after = %v/%v (found %v), want false/true: the cache served the old status", b.Resolved, a.Resolved, ok)
				}
			},
		},
		{
			name: "scope change",
			setup: func(t *testing.T, s *Store) string {
				return cacheMemory(t, s, cacheProj, "target", 7, map[string]string{"env": "prod"})
			},
			search: func(s *Store) ([]ScoredMemory, error) {
				return s.SearchVectorScoped(ctx, cacheProj, q, 20, map[string]string{"env": "dev"})
			},
			write: func(t *testing.T, w *Store, id string) {
				rawExec(t, w, `UPDATE memories SET scope = '{"env":"dev"}' WHERE id = ?`, id)
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				if _, ok := cacheFind(before, id); ok {
					t.Fatal("a prod-scoped row was returned for a dev query before the write")
				}
				if _, ok := cacheFind(after, id); !ok {
					t.Error("a rescoped row is not in the next search: the cache served the old scope")
				}
			},
		},
		{
			name:  "project move",
			setup: func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 8, nil) },
			search: func(s *Store) ([]ScoredMemory, error) {
				return s.SearchVector(ctx, cacheOther, q, 20)
			},
			write: func(t *testing.T, w *Store, id string) {
				rawExec(t, w, `UPDATE memories SET project_id = ? WHERE id = ?`, cacheOther, id)
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				if _, ok := cacheFind(before, id); ok {
					t.Fatal("another project's row was returned before it moved")
				}
				if _, ok := cacheFind(after, id); !ok {
					t.Error("a moved row is not in its new project's next search: the cache served the old project")
				}
			},
		},
		{
			name:   "memory delete",
			setup:  func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 9, nil) },
			search: inProject,
			write: func(t *testing.T, w *Store, id string) {
				if err := w.Delete(ctx, id); err != nil {
					t.Fatal(err)
				}
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				if _, ok := cacheFind(before, id); !ok {
					t.Fatal("the target was not searchable before the delete")
				}
				if _, ok := cacheFind(after, id); ok {
					t.Error("a deleted memory is still returned: the cache served the old corpus")
				}
			},
		},
		{
			// A connection with foreign keys off deletes the memory and leaves its
			// embedding row behind (a migration step runs like this). The orphan
			// no longer joins, so the answer changes, and only the memory-delete
			// trigger can tell: the cascade that would have fired the embedding
			// trigger did not run.
			name:   "memory delete without the cascade",
			setup:  func(t *testing.T, s *Store) string { return cacheMemory(t, s, cacheProj, "target", 10, nil) },
			search: inProject,
			write: func(t *testing.T, w *Store, id string) {
				conn, err := w.db.Conn(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close() //nolint:errcheck
				if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
					t.Fatal(err)
				}
				defer conn.ExecContext(ctx, `PRAGMA foreign_keys = ON`) //nolint:errcheck
				if _, err := conn.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
				var orphans int
				if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM memory_embeddings WHERE memory_id = ?`, id).Scan(&orphans); err != nil || orphans != 1 {
					t.Fatalf("the embedding row was not left behind (%d, %v): this case no longer isolates the memory-delete trigger", orphans, err)
				}
			},
			reflects: func(t *testing.T, before, after []ScoredMemory, id string) {
				if _, ok := cacheFind(before, id); !ok {
					t.Fatal("the target was not searchable before the delete")
				}
				if _, ok := cacheFind(after, id); ok {
					t.Error("a deleted memory is still returned: the cache served the old corpus")
				}
			},
		},
	}

	for _, k := range kinds {
		for _, via := range []string{"same handle", "second handle"} {
			t.Run(k.name+"/"+via, func(t *testing.T) {
				s, path := cacheStore(t)
				cacheCorpus(t, s)
				id := k.setup(t, s)

				before, err := k.search(s)
				if err != nil {
					t.Fatal(err)
				}
				// A second search is a hit, so what the write is up against is a
				// warm cache and not a cold one.
				if _, err := k.search(s); err != nil {
					t.Fatal(err)
				}
				loads := s.vcache.loads.Load()
				if loads != 1 {
					t.Fatalf("corpus loads after two searches = %d, want 1: the cache is not warm", loads)
				}
				epoch := epochOf(t, s)

				w := s
				if via == "second handle" {
					w = openCacheStoreAt(t, path)
				}
				k.write(t, w, id)

				if got := epochOf(t, s); got <= epoch {
					t.Errorf("vector_epoch = %d after the write, was %d: no trigger fired for it", got, epoch)
				}
				after, err := k.search(s)
				if err != nil {
					t.Fatal(err)
				}
				k.reflects(t, before, after, id)
				if got := s.vcache.loads.Load(); got != loads+1 {
					t.Errorf("corpus loads = %d after the write, want %d: the next search did not reload", got, loads+1)
				}
			})
		}
	}
}

// A write the vector query does not read leaves the counter alone, and the next
// search is a hit. This is the other half of the design: a counter that moved on
// the retrieval record every search writes would invalidate every other
// process's copy on every query.
func TestVectorEpochIgnoresWritesTheVectorQueryDoesNotRead(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	target := cacheMemory(t, s, cacheProj, "target", 3, nil)
	q := cacheVec(1)

	if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
		t.Fatal(err)
	}
	loads, epoch := s.vcache.loads.Load(), epochOf(t, s)

	imp := float32(0.9)
	cases := []struct {
		name  string
		write func() error
	}{
		{"access count and last_accessed", func() error { return s.Touch(ctx, []string{target}) }},
		{"tags and importance", func() error { return s.UpdateMemory(ctx, cacheProj, target, nil, nil, &imp, []string{"a", "b"}) }},
		{"a retrieval record", func() error {
			return s.RecordRetrieval(ctx, RetrievalRecord{ProjectID: cacheProj, Source: "search", Outcome: "empty", Reason: "no_candidates"})
		}},
		{"a memory with no embedding", func() error {
			_, err := s.Create(ctx, cacheProj, Memory{Category: "fact", Content: "no vector yet", Source: "manual"})
			return err
		}},
		{"a link between memories", func() error {
			other := cacheMemory(t, s, cacheProj, "linked", 0, nil)
			return s.CreateLink(ctx, target, other, "related", 0.5, "auto")
		}},
	}
	for _, c := range cases {
		if err := c.write(); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := epochOf(t, s); got != epoch {
			t.Errorf("%s moved vector_epoch %d -> %d: a write the vector query does not read invalidated every copy", c.name, epoch, got)
			epoch = got
		}
		if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
		if got := s.vcache.loads.Load(); got != loads {
			t.Errorf("%s: the next search reloaded the corpus (%d loads, want %d)", c.name, got, loads)
			loads = got
		}
	}
}

// Another PROCESS's write is seen. The writer is a real child process built with
// go build (never a self-exec of the test binary), writing through memory.OpenDB
// to the file this process holds a warm cache on.
func TestVectorEpochSeesAnotherProcessWrite(t *testing.T) {
	ctx := context.Background()
	helper := helperBinary(t)
	home := t.TempDir()

	run := func(t *testing.T, path string, args ...string) {
		t.Helper()
		cmd := exec.Command(helper, append([]string{"-role", "vecwrite", "-db", path, "-project", cacheProj}, args...)...)
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home,
			"XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_DATA_HOME="+filepath.Join(home, "data"),
			"XDG_STATE_HOME="+filepath.Join(home, "state"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("vecwrite %v: %v\n%s", args, err, out)
		}
		var rep struct{ KV map[string]string }
		if err := json.Unmarshal(out, &rep); err != nil || rep.KV["op"] == "" {
			t.Fatalf("vecwrite %v reported nothing: %v\n%s", args, err, out)
		}
	}

	q := cacheVec(1)
	vecArg := func(v []float32) string {
		parts := make([]string, len(v))
		for i, x := range v {
			parts[i] = fmt.Sprintf("%v", x)
		}
		return strings.Join(parts, ",")
	}

	t.Run("embedding", func(t *testing.T) {
		s, path := cacheStore(t)
		cacheCorpus(t, s)
		id := cacheMemory(t, s, cacheProj, "target", 0, nil)
		if res, _ := s.SearchVector(ctx, cacheProj, q, 20); len(res) == 0 {
			t.Fatal("empty corpus")
		}
		run(t, path, "-op", "embed", "-target", id, "-label", identityCurrent, "-vec", vecArg(cacheVec(2)))
		res, err := s.SearchVector(ctx, cacheProj, q, 20)
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := cacheFind(res, id); !ok || got.Score != cosineSimilarity(q, cacheVec(2)) {
			t.Errorf("another process's embedding is not in this process's next search (found=%v)", ok)
		}
	})
	t.Run("resolve", func(t *testing.T) {
		s, path := cacheStore(t)
		cacheCorpus(t, s)
		id := cacheMemory(t, s, cacheProj, "target", 3, nil)
		if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
		run(t, path, "-op", "resolve", "-target", id)
		res, _ := s.SearchVector(ctx, cacheProj, q, 20)
		if got, ok := cacheFind(res, id); !ok || !got.Resolved {
			t.Errorf("another process's resolve is not seen (found=%v resolved=%v)", ok, got.Resolved)
		}
	})
	t.Run("delete", func(t *testing.T) {
		s, path := cacheStore(t)
		cacheCorpus(t, s)
		id := cacheMemory(t, s, cacheProj, "target", 3, nil)
		if res, _ := s.SearchVector(ctx, cacheProj, q, 20); len(res) == 0 {
			t.Fatal("empty corpus")
		}
		run(t, path, "-op", "delete", "-target", id)
		res, _ := s.SearchVector(ctx, cacheProj, q, 20)
		if _, ok := cacheFind(res, id); ok {
			t.Error("a memory another process deleted is still returned")
		}
	})
	t.Run("writes the vector query does not read keep the copy", func(t *testing.T) {
		s, path := cacheStore(t)
		cacheCorpus(t, s)
		id := cacheMemory(t, s, cacheProj, "target", 3, nil)
		if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
		loads := s.vcache.loads.Load()
		run(t, path, "-op", "touch", "-target", id)
		run(t, path, "-op", "retrieval")
		if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
		if got := s.vcache.loads.Load(); got != loads {
			t.Errorf("another process's touch and retrieval record invalidated this process's copy (%d loads, want %d)", got, loads)
		}
	})
}

// A change of embedding identity drops the copy: the key is (epoch, identity),
// and the foreign rows are still reported on the reload.
func TestVectorEpochIdentityChangeInvalidates(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	q := cacheVec(1)

	res, facts, err := s.searchVectorLeg(ctx, cacheProj, q, 20, nil)
	if err != nil || len(res) == 0 || facts.foreign != 0 {
		t.Fatalf("baseline search = %d rows, %+v, %v", len(res), facts, err)
	}
	loads := s.vcache.loads.Load()
	epoch := epochOf(t, s)

	s.SetEmbeddingIdentity("another-model:v1")
	res, facts, err = s.searchVectorLeg(ctx, cacheProj, q, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 || facts.foreign != 6 {
		t.Errorf("after the identity change: %d rows, facts %+v; want 0 rows and 6 foreign: the old identity's copy was served", len(res), facts)
	}
	if got := s.vcache.loads.Load(); got != loads+1 {
		t.Errorf("loads = %d, want %d: the identity change did not reload", got, loads+1)
	}
	if got := epochOf(t, s); got != epoch {
		t.Errorf("an in-process identity change moved the database's counter (%d -> %d)", epoch, got)
	}

	// And back: the held copy is the other identity's, so this reloads too.
	s.SetEmbeddingIdentity(identityCurrent)
	res, _, err = s.searchVectorLeg(ctx, cacheProj, q, 20, nil)
	if err != nil || len(res) == 0 {
		t.Fatalf("search after restoring the identity = %d rows, %v", len(res), err)
	}
	if got := s.vcache.loads.Load(); got != loads+2 {
		t.Errorf("loads = %d, want %d after switching back", got, loads+2)
	}
}

// A search served from the cache scores with the store lock released, exactly as
// an uncached one: TestVectorSearchScoresWithTheStoreLockReleased is the
// uncached path's, and this is the same assertion with the cache warm and the
// search proven to be a hit.
func TestCachedVectorSearchScoresWithTheStoreLockReleased(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	q := cacheVec(1)

	if _, err := s.SearchVector(ctx, cacheProj, q, 10); err != nil {
		t.Fatal(err)
	}
	loads, hits := s.vcache.loads.Load(), s.vcache.hits.Load()

	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unpark := func() { once.Do(func() { close(release) }) }
	setDuringVectorSeams(nil, func() {
		close(entered)
		<-release
	})
	t.Cleanup(func() { setDuringVectorSeams(nil, nil) })
	t.Cleanup(unpark)

	searched := make(chan error, 1)
	go func() {
		_, err := s.SearchVector(ctx, cacheProj, q, 10)
		searched <- err
	}()
	select {
	case <-entered:
	case err := <-searched:
		t.Fatalf("the search returned before scoring (%v)", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the search never reached its scoring pass")
	}
	if s.vcache.hits.Load() != hits+1 || s.vcache.loads.Load() != loads {
		t.Fatal("the parked search was not served from the cache, so this test is not about the cached path")
	}

	wrote := make(chan struct{})
	go func() {
		s.SetEmbeddingIdentity(identityCurrent) // takes the store's write lock
		close(wrote)
	}()
	select {
	case <-wrote:
	case <-time.After(30 * time.Second):
		unpark()
		t.Fatal("a writer could not take the store lock while a cached search was scoring")
	}
	unpark()
	if err := <-searched; err != nil {
		t.Fatal(err)
	}
}

// The cached path's answer is the uncached path's: same rows, same order, same
// float32 scores (compared with ==, not a tolerance), same facts, for the project
// leg, the scoped leg and the cross-project leg, over a corpus with global rows,
// scoped rows, resolved rows, another project's rows, rows from another vector
// space and rows of another width. The corpus has no tied cosines, which is what
// makes the order a function of the data and not of the scan order (see
// vecCandidate.betterThan).
func TestCachedVectorSearchMatchesTheUncachedPath(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	if err := s.EnsureProject(ctx, globalProject, "/g", "g"); err != nil {
		t.Logf("EnsureProject(global): %v", err)
	}
	var wantResolved []string
	for i := range 60 {
		var scope map[string]string
		switch i % 3 {
		case 1:
			scope = map[string]string{"env": "prod"}
		case 2:
			scope = map[string]string{"env": "dev", "region": "eu"}
		}
		proj := cacheProj
		if i%5 == 4 {
			proj = cacheOther
		}
		id := cacheMemory(t, s, proj, fmt.Sprintf("row %d", i), uint64(200+i), scope)
		if i%7 == 0 && proj == cacheProj {
			wantResolved = append(wantResolved, id)
		}
	}
	if _, err := s.MarkResolved(ctx, cacheProj, wantResolved, Provenance{}); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		g := cacheMemory(t, s, cacheProj, fmt.Sprintf("global %d", i), uint64(400+i), nil)
		if err := s.PromoteToGlobal(ctx, cacheProj, g); err != nil {
			t.Fatal(err)
		}
	}
	// Another vector space, and another width.
	foreign := cacheMemory(t, s, cacheProj, "foreign", 0, nil)
	if err := s.StoreEmbedding(ctx, foreign, cacheVec(500), "retired-model:v0"); err != nil {
		t.Fatal(err)
	}
	narrow := cacheMemory(t, s, cacheProj, "narrow", 0, nil)
	if err := s.StoreEmbedding(ctx, narrow, cacheVec(501)[:4], identityCurrent); err != nil {
		t.Fatal(err)
	}

	type answer struct {
		rows  []ScoredMemory
		facts vectorLegFacts
	}
	legs := map[string]func(q []float32, limit int) (answer, error){
		"project": func(q []float32, limit int) (answer, error) {
			r, f, err := s.searchVectorLeg(ctx, cacheProj, q, limit, nil)
			return answer{r, f}, err
		},
		"other project": func(q []float32, limit int) (answer, error) {
			r, f, err := s.searchVectorLeg(ctx, cacheOther, q, limit, nil)
			return answer{r, f}, err
		},
		"scoped": func(q []float32, limit int) (answer, error) {
			r, f, err := s.searchVectorLeg(ctx, cacheProj, q, limit, map[string]string{"env": "dev"})
			return answer{r, f}, err
		},
		"all projects": func(q []float32, limit int) (answer, error) {
			r, f, err := s.searchVectorAllLeg(ctx, q, limit)
			return answer{r, f}, err
		},
	}
	for name, leg := range legs {
		for _, limit := range []int{1, 5, 200} {
			for _, seed := range []uint64{1, 2, 3} {
				q := cacheVec(seed)

				withCacheCap(t, 0)
				want, err := leg(q, limit)
				if err != nil {
					t.Fatal(err)
				}
				withCacheCap(t, maxRetainedVectorBytes)
				loadsBefore := s.vcache.loads.Load()
				miss, err := leg(q, limit)
				if err != nil {
					t.Fatal(err)
				}
				hit, err := leg(q, limit)
				if err != nil {
					t.Fatal(err)
				}
				if s.vcache.loads.Load() > loadsBefore+1 {
					t.Fatalf("%s: the cached legs read the corpus more than once", name)
				}
				if len(want.rows) == 0 && name != "other project" {
					t.Fatalf("%s limit %d seed %d: the reference answer is empty, so nothing is being compared", name, limit, seed)
				}
				if !reflect.DeepEqual(want, miss) {
					t.Errorf("%s limit %d seed %d: the first cached search differs from the uncached one:\n cached   %+v\n uncached %+v", name, limit, seed, miss, want)
				}
				if !reflect.DeepEqual(want, hit) {
					t.Errorf("%s limit %d seed %d: a cache hit differs from the uncached search:\n hit      %+v\n uncached %+v", name, limit, seed, hit, want)
				}
			}
		}
	}
	// The facts really were exercised: one foreign row and one narrow row sit in
	// the project, so a path that dropped either count would match nothing above.
	_, f, _ := s.searchVectorLeg(ctx, cacheProj, cacheVec(1), 5, nil)
	if f.foreign != 1 || f.mismatched != 1 {
		t.Errorf("facts = %+v, want 1 foreign and 1 mismatched in the corpus under test", f)
	}
}

// Above the bound nothing is cached, and the search is answered as it always was.
func TestVectorCacheHoldsNothingAboveItsBound(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	q := cacheVec(1)
	// Six rows of eight float32s are 192 bytes of embeddings.
	withCacheCap(t, 191)

	for range 3 {
		res, err := s.SearchVector(ctx, cacheProj, q, 20)
		if err != nil || len(res) != 6 {
			t.Fatalf("search = %d rows, %v; want the 6 the uncached path returns", len(res), err)
		}
	}
	if _, ok := s.vcache.held(); ok {
		t.Error("a corpus above the bound is held")
	}
	if n := s.vcache.loads.Load(); n != 0 {
		t.Errorf("a corpus above the bound was read for the cache %d times: the bound should be checked before the load", n)
	}

	// At the bound it is cached.
	withCacheCap(t, 192)
	for range 2 {
		if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := s.vcache.held(); !ok || s.vcache.loads.Load() != 1 {
		t.Errorf("a corpus at the bound was not cached once (held=%v loads=%d)", func() bool { _, ok := s.vcache.held(); return ok }(), s.vcache.loads.Load())
	}
}

// The bound's pre-check counts only the rows the cache would keep: the configured
// identity's, at the query's width. A corpus that fits the bound must still be
// cached when the table also holds rows of another vector space — the loader
// files those as foreign pairs, and the uncached count already excludes them — so
// counting the whole table made a corpus that fits look over the bound and sent
// every search down the uncached path (#1257).
func TestVectorCacheBoundCountsOnlyTheConfiguredSpace(t *testing.T) {
	ctx := context.Background()
	q := cacheVec(1)
	// Four configured rows of eight float32s are 128 bytes of embeddings, under
	// the cap below, so a working pre-check loads the corpus once and hits after.
	const cap = 150
	four := func(t *testing.T, s *Store) {
		t.Helper()
		for i := range 4 {
			cacheMemory(t, s, cacheProj, fmt.Sprintf("configured %d", i), uint64(200+i), nil)
		}
	}

	// Control: the same four rows and no rows of another identity.
	control, _ := cacheStore(t)
	four(t, control)
	withCacheCap(t, cap)
	for range 2 {
		if _, err := control.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
	}
	if control.vcache.loads.Load() != 1 || control.vcache.hits.Load() != 1 {
		t.Errorf("control: loads=%d hits=%d, want 1 and 1 (four configured rows fit the bound)",
			control.vcache.loads.Load(), control.vcache.hits.Load())
	}

	// Three more rows embedded under another identity do not change what the cache
	// keeps, so they must not change the bound's answer either.
	s, _ := cacheStore(t)
	four(t, s)
	for i := range 3 {
		id := cacheMemory(t, s, cacheProj, fmt.Sprintf("foreign %d", i), 0, nil)
		if err := s.StoreEmbedding(ctx, id, cacheVec(uint64(300+i)), "other-model:8"); err != nil {
			t.Fatalf("StoreEmbedding foreign: %v", err)
		}
	}
	withCacheCap(t, cap)
	for range 2 {
		if _, err := s.SearchVector(ctx, cacheProj, q, 20); err != nil {
			t.Fatal(err)
		}
	}
	if s.vcache.loads.Load() != 1 || s.vcache.hits.Load() != 1 {
		t.Errorf("loads=%d hits=%d, want 1 and 1: rows of another vector space pushed a corpus that fits the bound past it",
			s.vcache.loads.Load(), s.vcache.hits.Load())
	}
}

// A copy that grew past the bound after the count was taken is used by the search
// that read it and is not kept.
func TestVectorCacheDoesNotKeepACopyOverItsBound(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	// The pre-check estimates at the query's width, so a query narrower than the
	// stored rows passes it while the copy (192 bytes) exceeds the bound.
	withCacheCap(t, 150)
	narrowQ := cacheVec(1)[:4]
	if _, err := s.SearchVector(ctx, cacheProj, narrowQ, 20); err != nil {
		t.Fatal(err)
	}
	if s.vcache.loads.Load() != 1 {
		t.Fatalf("the load the estimate allowed did not happen (%d loads)", s.vcache.loads.Load())
	}
	if _, ok := s.vcache.held(); ok {
		t.Error("a copy over the bound was published")
	}
}

// An unused copy is released after the idle period.
func TestVectorCacheIsReleasedWhenIdle(t *testing.T) {
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	s.vcache.setIdle(50 * time.Millisecond)
	if _, err := s.SearchVector(context.Background(), cacheProj, cacheVec(1), 5); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.vcache.held(); !ok {
		t.Fatal("nothing held after a search")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, ok := s.vcache.held(); !ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the copy was still held long after the idle period")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Candidates reads the epoch inside its pinned transaction and uses the cache
// only on a match. A transaction whose snapshot is older than the held copy
// scores its own snapshot and does not publish over the newer copy; a newer
// snapshot reloads.
func TestVectorCacheRespectsAPinnedSnapshot(t *testing.T) {
	ctx := context.Background()
	s, path := cacheStore(t)
	cacheCorpus(t, s)
	target := cacheMemory(t, s, cacheProj, "target", 3, nil)
	q := cacheVec(1)

	readDB, err := OpenReadDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	rs := NewStoreWithRead(s.db, readDB, nil)
	rs.SetEmbeddingIdentity(identityCurrent)
	if rs.vcache == nil {
		t.Fatal("a store has no cache")
	}

	// Pin a snapshot at the current state, the way Candidates does, and bind a
	// store to it.
	tx, err := readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	pinnedEpoch, ok := readVectorEpoch(ctx, tx)
	if !ok {
		t.Fatal("no epoch inside the pinned transaction")
	}
	pinned := &Store{db: s.db, snapshot: tx, logger: rs.logger,
		foreignWarned: rs.foreignWarned, scratch: rs.scratch, vcache: rs.vcache}
	pinned.embeddingIdentity.Store(identityPtr(identityCurrent))

	// Another handle deletes the target, and a plain search publishes the newer
	// state to the shared cache.
	if err := s.Delete(ctx, target); err != nil {
		t.Fatal(err)
	}
	res, err := rs.SearchVector(ctx, cacheProj, q, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheFind(res, target); ok {
		t.Fatal("the plain search returned a deleted row")
	}
	newer, ok := rs.vcache.held()
	if !ok || newer <= pinnedEpoch {
		t.Fatalf("held epoch = %d (%v), want a copy newer than the pinned snapshot's %d", newer, ok, pinnedEpoch)
	}

	// The pinned snapshot still sees the row, and does not replace the newer copy.
	res, _, err = pinned.searchVectorLeg(ctx, cacheProj, q, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheFind(res, target); !ok {
		t.Error("a search inside a pinned snapshot was served the newer cache: it did not score its own snapshot")
	}
	if held, _ := rs.vcache.held(); held != newer {
		t.Errorf("a pinned older snapshot replaced the held copy (epoch %d, was %d)", held, newer)
	}

	// A snapshot at the held epoch is a hit. The read handle's pool holds one
	// connection, so the first snapshot is released before the second is taken.
	_ = tx.Rollback()
	hits := rs.vcache.hits.Load()
	tx2, err := readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx2.Rollback() //nolint:errcheck
	current := &Store{db: s.db, snapshot: tx2, logger: rs.logger,
		foreignWarned: rs.foreignWarned, scratch: rs.scratch, vcache: rs.vcache}
	current.embeddingIdentity.Store(identityPtr(identityCurrent))
	res, _, err = current.searchVectorLeg(ctx, cacheProj, q, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheFind(res, target); ok {
		t.Error("a current snapshot returned the deleted row")
	}
	if rs.vcache.hits.Load() != hits+1 {
		t.Error("a snapshot at the held epoch did not use the cache")
	}
}

// Concurrent searches and writes through several handles, under -race: every
// search completes, and once the writers stop, the cached answer is the
// uncached one.
func TestVectorCacheUnderConcurrentWrites(t *testing.T) {
	ctx := context.Background()
	s, path := cacheStore(t)
	cacheCorpus(t, s)
	w := openCacheStoreAt(t, path)
	q := cacheVec(1)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for g := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := s.SearchVector(ctx, cacheProj, q, 5); err != nil {
					t.Errorf("searcher %d: %v", g, err)
					return
				}
			}
		}()
	}
	for i := range 15 {
		id, err := w.Create(ctx, cacheProj, Memory{Category: "fact", Content: fmt.Sprintf("racer %d", i), Source: "manual"})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.StoreEmbedding(ctx, id, cacheVec(uint64(600+i)), identityCurrent); err != nil {
			t.Fatal(err)
		}
		if i%3 == 0 {
			if err := w.DeleteEmbedding(ctx, id); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()

	got, err := s.SearchVector(ctx, cacheProj, q, 50)
	if err != nil {
		t.Fatal(err)
	}
	withCacheCap(t, 0)
	want, err := s.SearchVector(ctx, cacheProj, q, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("after concurrent writes the cached search differs from the uncached one:\n cached   %v\n uncached %v", idsOf(got), idsOf(want))
	}
}

// The uncached path keeps its own guards. The three tests below were written for
// it (#556) and, with the cache on, now meet a corpus small enough to cache and
// exercise the cache-miss path instead. Running each with the cache bound at zero
// makes them cover snapshotVectors, which a corpus above the bound still uses.
func TestUncachedVectorSearchCopiesTheCorpusUnderTheStoreReadLock(t *testing.T) {
	withCacheCap(t, 0)
	TestVectorSearchCopiesTheCorpusUnderTheStoreReadLock(t)
}

func TestUncachedVectorSearchScoresWithTheStoreLockReleased(t *testing.T) {
	withCacheCap(t, 0)
	TestVectorSearchScoresWithTheStoreLockReleased(t)
}

func TestUncachedVectorSearchRankingMatchesTheCollectAndSortBaseline(t *testing.T) {
	withCacheCap(t, 0)
	TestSearchVectorRankingMatchesTheCollectAndSortBaseline(t)
}

// A copy read after another process's write but labelled with the epoch read
// before it is newer than its label, and must not be published: a pinned
// snapshot at the older epoch would otherwise be served rows it cannot see. The
// write lands in the one window that makes the label wrong, between the epoch
// read and the corpus query.
func TestVectorCacheDoesNotPublishACopyANewerWriteOverlapped(t *testing.T) {
	ctx := context.Background()
	s, path := cacheStore(t)
	cacheCorpus(t, s)
	late := cacheMemory(t, s, cacheProj, "late", 0, nil)
	q := cacheVec(1)

	readDB, err := OpenReadDB(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = readDB.Close() })
	rs := NewStoreWithRead(s.db, readDB, nil)
	rs.SetEmbeddingIdentity(identityCurrent)

	// A snapshot pinned at the state before the write.
	tx, err := readDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback() //nolint:errcheck
	if _, ok := readVectorEpoch(ctx, tx); !ok {
		t.Fatal("no epoch in the pinned snapshot")
	}
	pinned := &Store{db: s.db, snapshot: tx, logger: rs.logger,
		foreignWarned: rs.foreignWarned, scratch: rs.scratch, vcache: rs.vcache}
	pinned.embeddingIdentity.Store(identityPtr(identityCurrent))

	writer := openCacheStoreAt(t, path)
	var wrote bool
	beforeVectorCorpusLoadFn.Store(func() {
		if wrote {
			return
		}
		wrote = true
		if err := writer.StoreEmbedding(ctx, late, cacheVec(2), identityCurrent); err != nil {
			t.Errorf("the overlapping write: %v", err)
		}
	})
	t.Cleanup(func() { beforeVectorCorpusLoadFn.Store(func() {}) })

	res, err := rs.SearchVector(ctx, cacheProj, q, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !wrote {
		t.Fatal("the overlapping write never ran, so this test proved nothing")
	}
	if _, ok := cacheFind(res, late); !ok {
		t.Fatal("the loading search did not see the row written before its corpus query")
	}
	if _, ok := rs.vcache.held(); ok {
		t.Error("a copy a newer write overlapped was published under the older epoch")
	}

	res, _, err = pinned.searchVectorLeg(ctx, cacheProj, q, 20, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheFind(res, late); ok {
		t.Error("a pinned snapshot older than the write was served a row it cannot see")
	}
}

// Tied cosines. Rows with identical embeddings (duplicate content embeds to the
// same bytes) tie exactly, and a tie is broken by creation order (the older memory first), not by the order the
// database yielded the rows in. The uncached project leg is answered through a
// project index and the cache scans the embeddings table once, so the two scan in
// different orders; the answers must still be identical, row for row, at every
// limit, including limits that cut through a tied group.
func TestCachedVectorSearchAgreesWithTheUncachedPathOnTies(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	var created []string // creation order of the tied rows
	for i := range 4 {
		created = append(created, cacheMemory(t, s, cacheProj, fmt.Sprintf("dup %d", i), 50, nil))
	}
	for i := range 3 {
		g := cacheMemory(t, s, cacheProj, fmt.Sprintf("global dup %d", i), 50, nil)
		if err := s.PromoteToGlobal(ctx, cacheProj, g); err != nil {
			t.Fatal(err)
		}
		created = append(created, g)
	}
	rank := map[string]int{}
	for i, id := range created {
		rank[id] = i
	}
	for i := range 6 {
		cacheMemory(t, s, cacheProj, fmt.Sprintf("distinct %d", i), uint64(70+i), nil)
	}
	q := cacheVec(1)

	for limit := 1; limit <= 14; limit++ {
		withCacheCap(t, 0)
		want, err := s.SearchVector(ctx, cacheProj, q, limit)
		if err != nil {
			t.Fatal(err)
		}
		withCacheCap(t, maxRetainedVectorBytes)
		got, err := s.SearchVector(ctx, cacheProj, q, limit)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("limit %d: cached and uncached differ on ties:\n cached   %v\n uncached %v", limit, idsOf(got), idsOf(want))
		}
		for i := 1; i < len(got); i++ {
			a, aok := rank[got[i-1].MemoryID]
			b, bok := rank[got[i].MemoryID]
			if got[i].Score == got[i-1].Score && aok && bok && b < a {
				t.Errorf("limit %d: tied rows %d and %d are not in creation order", limit, i-1, i)
			}
		}
	}
}

// A memory inserted under an id that already has an embedding row joins the
// vector query again, and only vector_epoch_mem_ai can tell. The orphan is left
// by a delete made with foreign keys off (what a migration step does); the
// cached search must return the row once it is reinserted.
func TestVectorEpochSeesAnOrphanedEmbeddingReattach(t *testing.T) {
	ctx := context.Background()
	s, _ := cacheStore(t)
	cacheCorpus(t, s)
	id := cacheMemory(t, s, cacheProj, "target", 3, nil)
	q := cacheVec(1)

	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	res, err := s.SearchVector(ctx, cacheProj, q, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheFind(res, id); ok {
		t.Fatal("the orphaned embedding was returned without its memory")
	}
	if _, ok := s.vcache.held(); !ok {
		t.Fatal("nothing cached, so the reattach would not be tested against a copy")
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO memories (id, project_id, category, content, source) VALUES (?, ?, 'fact', 'reattached', 'manual')`,
		id, cacheProj); err != nil {
		t.Fatal(err)
	}
	res, err = s.SearchVector(ctx, cacheProj, q, 20)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cacheFind(res, id); !ok {
		t.Error("a memory reinserted over an orphaned embedding is not in the next search: the cache served the old corpus")
	}
}
