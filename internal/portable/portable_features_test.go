package portable

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestPortableFeatures(t *testing.T) {
	t.Run("reject unknown and leave store unchanged", func(t *testing.T) {
		store := newTestStore(t)
		ctx := context.Background()
		_ = store.EnsureProject(ctx, "p1", "/src/p1", "one")
		body := `{"type":"header","schema_version":2,"features":["frob"]}` + "\n" +
			`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
			`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"a fact","source":"manual","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n"
		before, _ := store.PortableMemories(ctx, []string{"p1"})
		_, err := Import(ctx, store, strings.NewReader(body), ImportOptions{Apply: true}, nil)
		if err == nil || !strings.Contains(err.Error(), "frob") {
			t.Fatalf("err=%v, want unknown feature named", err)
		}
		after, _ := store.PortableMemories(ctx, []string{"p1"})
		if len(after) != len(before) {
			t.Fatalf("wrote rows on refusal: before=%d after=%d", len(before), len(after))
		}
	})

	t.Run("read old and known headers", func(t *testing.T) {
		for _, tc := range []struct {
			name, body string
		}{
			{"no features", `{"type":"header","schema_version":1}` + "\n" +
				`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
				`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"a fact from an old artifact","source":"manual","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n"},
			{"known feature", `{"type":"header","schema_version":2,"features":["retention"]}` + "\n" +
				`{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one"}}` + "\n" +
				`{"type":"memory","memory":{"id":"m1","project_id":"p1","category":"fact","content":"a fact","source":"manual","retention":"project","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}` + "\n"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				store := newTestStore(t)
				report, err := Import(context.Background(), store, strings.NewReader(tc.body), ImportOptions{Apply: true}, nil)
				if err != nil {
					t.Fatalf("Import: %v", err)
				}
				if report.Created["memory"] != 1 {
					t.Fatalf("memory created=%d, want 1", report.Created["memory"])
				}
			})
		}
	})

	t.Run("export declares features and round trips", func(t *testing.T) {
		src := newTestStore(t)
		ctx := context.Background()
		_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
		_, _ = src.Create(ctx, "p1", memory.Memory{Category: "fact", Content: "a fact", Source: "manual"})
		data := exportBytes(t, src, "")
		if !strings.Contains(string(data), `"features":["links","retention"]`) {
			t.Fatalf("header missing declared features: %s", strings.SplitN(string(data), "\n", 2)[0])
		}
		dst := newTestStore(t)
		report, err := Import(ctx, dst, bytes.NewReader(data), ImportOptions{Apply: true, TrustProvenance: true}, nil)
		if err != nil || report.Created["memory"] != 1 {
			t.Fatalf("err=%v created=%v", err, report.Created)
		}
	})
}
