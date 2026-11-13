package portable

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestTheArtifactCarriesTheRetentionTier(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	_, _, _, _ = src.UpsertWithOptions(ctx, "p1", "fact", "a keep-forever fact", "mcp", 0.5, nil, memory.UpsertOptions{Retention: memory.RetentionPersistent})
	_, _, _, _ = src.UpsertWithOptions(ctx, "p1", "fact", "an ordinary fact", "mcp", 0.5, nil, memory.UpsertOptions{Retention: memory.RetentionProject})
	artifact := exportBytes(t, src, "")
	for _, want := range []string{`"retention":"persistent"`, `"retention":"project"`} {
		if !bytes.Contains(artifact, []byte(want)) {
			t.Errorf("missing %s", want)
		}
	}
	if got := bytes.Count(artifact, []byte(`"retention":`)); got != 2 {
		t.Errorf("retention keys=%d, want 2", got)
	}
}

func TestImportRestoresTheTierUnderTrustAndDowngradesItWithout(t *testing.T) {
	ctx := context.Background()
	src := newTestStore(t)
	_ = src.EnsureProject(ctx, "p1", "/src/p1", "one")
	_, _, _, _ = src.UpsertWithOptions(ctx, "p1", "fact", "a keep-forever fact", "mcp", 0.5, nil, memory.UpsertOptions{Retention: memory.RetentionPersistent})
	artifact := exportBytes(t, src, "")
	for _, tc := range []struct {
		name  string
		trust bool
		want  string
	}{
		{"trusted", true, memory.RetentionPersistent},
		{"untrusted", false, memory.RetentionProject},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dst := newTestStore(t)
			report, err := Import(ctx, dst, bytes.NewReader(artifact), ImportOptions{Apply: true, TrustProvenance: tc.trust}, nil)
			if err != nil || report.Rejected != 0 {
				t.Fatalf("err=%v rejected=%d", err, report.Rejected)
			}
			got, _ := dst.PortableMemories(ctx, nil)
			if len(got) != 1 || got[0].Retention != tc.want {
				t.Fatalf("got %+v want retention %q", got, tc.want)
			}
		})
	}
}

func TestAnArtifactRecordThatStatesNoRetentionStillImports(t *testing.T) {
	dst := newTestStore(t)
	artifact := strings.Join([]string{
		`{"type":"header","schema_version":2}`,
		`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		`{"type":"memory","memory":{"id":"mv","project_id":"pv","category":"fact","content":"a record that states no tier","source":"mcp","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
	}, "\n") + "\n"
	report, err := Import(context.Background(), dst, strings.NewReader(artifact), ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil || report.Rejected != 0 || report.Created["memory"] != 1 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	got, _ := dst.PortableMemories(context.Background(), nil)
	if len(got) != 1 || got[0].Retention != memory.RetentionProject {
		t.Fatalf("got %+v", got)
	}
}

func TestAnArtifactStatingAnUnknownRetentionIsRejectedByName(t *testing.T) {
	dst := newTestStore(t)
	artifact := strings.Join([]string{
		`{"type":"header","schema_version":2}`,
		`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		`{"type":"memory","memory":{"id":"bad","project_id":"pv","category":"fact","content":"a fourth tier","source":"mcp","retention":"forever","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		`{"type":"memory","memory":{"id":"good","project_id":"pv","category":"fact","content":"a real tier","source":"mcp","retention":"persistent","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
	}, "\n") + "\n"
	report, err := Import(context.Background(), dst, strings.NewReader(artifact), ImportOptions{Apply: true, TrustProvenance: true}, nil)
	if err != nil || report.Rejected != 1 || report.Created["memory"] != 1 {
		t.Fatalf("err=%v report=%+v", err, report)
	}
	got, _ := dst.PortableMemories(context.Background(), nil)
	if len(got) != 1 || got[0].ID != "good" || got[0].Retention != memory.RetentionPersistent {
		t.Fatalf("got %+v", got)
	}
}

func TestARejectedArtifactRecordNeverPrintsTheValueItRejected(t *testing.T) {
	const token = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	line := func(field string) string {
		rec := map[string]any{
			"id": "bad-" + field, "project_id": "pv", "category": "fact", "content": "x",
			"source": "mcp", "created_at": "2026-01-01 00:00:00", "updated_at": "2026-01-01 00:00:00",
			field: token,
		}
		b, _ := json.Marshal(map[string]any{"type": "memory", "memory": rec})
		return string(b)
	}
	artifact := strings.Join([]string{
		`{"type":"header","schema_version":2}`,
		`{"type":"project","project":{"id":"pv","path":"/src/pv","name":"v","created_at":"2026-01-01 00:00:00","updated_at":"2026-01-01 00:00:00"}}`,
		line("retention"), line("category"), line("source"),
	}, "\n") + "\n"
	report, err := Import(context.Background(), newTestStore(t), strings.NewReader(artifact), ImportOptions{Apply: true}, nil)
	if err != nil || report.Rejected != 3 {
		t.Fatalf("err=%v rejected=%d", err, report.Rejected)
	}
	for _, e := range report.Errors {
		if strings.Contains(e.Error(), token) || strings.Contains(fmt.Sprintf("%v", e), token) {
			t.Fatalf("report echoed credential: %v", e)
		}
	}
}
