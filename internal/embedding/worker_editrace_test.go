package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

const editRaceProject = "race-project"

// editingEmbedServer is a fake endpoint that runs onFirst while the FIRST embed
// request is in flight (after the worker has read the content, before it can
// store the vector), then answers with vecOld. Every later request is answered
// with vecNew, the vector of the edited text. No sleeps: the edit happens inside
// the handler, which is exactly the window the worker is blocked in.
func editingEmbedServer(t *testing.T, vecOld, vecNew []float32, onFirst func()) *httptest.Server {
	t.Helper()
	var once sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/embed" {
			w.WriteHeader(http.StatusOK)
			return
		}
		vec := vecNew
		once.Do(func() {
			onFirst()
			vec = vecOld
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string][][]float32{"embeddings": {vec}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestEmbedEditDuringFlightKeepsRowPending: an edit that lands while the
// endpoint is computing a vector must not be overwritten by that vector. The
// edit removes the row's embedding; the in-flight vector was computed from the
// old text, so storing it would leave the wrong vector current, never selected
// for re-embedding. The write has to be refused, leaving the row pending, so the
// next pass embeds the new text.
func TestEmbedEditDuringFlightKeepsRowPending(t *testing.T) {
	vecOld := []float32{1, 0, 0}
	vecNew := []float32{0, 1, 0}

	for _, tc := range []struct {
		name string
		run  func(w *Worker, id string)
	}{
		{"EmbedPending", func(w *Worker, _ string) { w.EmbedPending(context.Background(), editRaceProject, 10) }},
		{"EmbedOne", func(w *Worker, id string) { w.EmbedOne(context.Background(), id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := memory.OpenDB(":memory:")
			if err != nil {
				t.Fatalf("OpenDB: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := memory.NewStore(db, discardLogger())
			if err := store.EnsureProject(ctx, editRaceProject, "/tmp/race", "race"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			id, err := store.Create(ctx, editRaceProject, memory.Memory{
				Category: "fact", Content: "the old text", Source: "manual", Importance: 0.5,
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}

			const newText = "the old text NEW"
			srv := editingEmbedServer(t, vecOld, vecNew, func() {
				c := newText
				if err := store.UpdateMemory(ctx, editRaceProject, id, &c, nil, nil, nil); err != nil {
					t.Errorf("UpdateMemory: %v", err)
				}
			})
			client := NewClient(srv.URL, "nomic-embed-text:v1.5", 3)
			worker := NewWorker(client, store, discardLogger(), time.Minute, t.TempDir())

			tc.run(worker, id)

			if got, _ := store.GetMemoryContent(ctx, id); got != newText {
				t.Fatalf("content = %q, want the edited %q", got, newText)
			}
			if vec, _ := store.GetEmbedding(ctx, id); slices.Equal(vec, vecOld) {
				t.Fatalf("stored vector is the OLD text's %v: the edit's deletion was overwritten", vec)
			}
			pending, err := store.UnembeddedMemoryIDs(ctx, editRaceProject, client.Identity(), 10)
			if err != nil {
				t.Fatalf("UnembeddedMemoryIDs: %v", err)
			}
			if !slices.Equal(pending, []string{id}) {
				t.Fatalf("pending = %v, want [%s]: the row must stay queued for the new text", pending, id)
			}

			// The next pass embeds the new text.
			if n, _ := worker.EmbedPending(ctx, editRaceProject, 10); n != 1 {
				t.Fatalf("second pass embedded %d, want 1", n)
			}
			if vec, _ := store.GetEmbedding(ctx, id); !slices.Equal(vec, vecNew) {
				t.Fatalf("vector after second pass = %v, want the new text's %v", vec, vecNew)
			}
		})
	}
}
