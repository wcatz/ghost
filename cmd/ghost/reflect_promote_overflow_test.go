package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// A cross-project merge of more decision companions than one source_ref can
// link, applied with --promote-globals: the merge is refused and every
// companion stays live, and the round says so with the overflow note rather
// than with the warning that a candidate "could not be promoted and was kept in
// the project", which reads as though nothing was lost.
func TestRunReflectAnOverflowingPromotedMergeIsReportedAsAnOverflow(t *testing.T) {
	const project = "promote-overflow"
	const mergeText = "every repo records these thirteen choices the same way across all projects"
	dataHome, ids, stderr := reflectOpsRound(t, project,
		func(t *testing.T, ctx context.Context, s *memory.Store) []string {
			t.Helper()
			var ids []string
			for i := 0; i < 13; i++ {
				_, comp, _, err := s.RecordDecision(ctx, project, fmt.Sprintf("Decision %d", i), fmt.Sprintf("choice %d", i), "because", nil, nil)
				if err != nil {
					t.Fatalf("seed: %v", err)
				}
				ids = append(ids, comp)
			}
			// Ordinary facts the round keeps, so the round clears the
			// consolidator's output floor and has a project half.
			for i := 0; i < 10; i++ {
				id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5,
					Content: fmt.Sprintf("service %d listens on port %d", i, 8000+i)})
				if err != nil {
					t.Fatalf("seed: %v", err)
				}
				ids = append(ids, id)
			}
			return ids
		},
		func(ids []string) []string {
			ops := []string{"merge " + strings.Join(ids[:13], ",") + " -> " + mergeText}
			for _, id := range ids[13:] {
				ops = append(ops, "keep "+id)
			}
			return ops
		},
		"--promote-globals", "--allow-drops")

	if !strings.Contains(stderr, "were dropped with every merged memory left live") {
		t.Errorf("stderr does not carry the overflow note:\n%s", stderr)
	}
	if strings.Contains(stderr, "could not be promoted and were kept in the project") {
		t.Errorf("stderr reports the refused merge as kept in the project:\n%s", stderr)
	}
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	for _, id := range ids[:13] {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND resolved_at IS NULL`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("companion %s did not stay live", id)
		}
	}
	var merged int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE content = ?`, mergeText).Scan(&merged); err != nil {
		t.Fatal(err)
	}
	if merged != 0 {
		t.Errorf("%d row(s) hold the refused merge", merged)
	}
}

// The same refusal when the overflow is only found at the write: the _global
// row the promoted merge folds into already holds twelve decision links, and
// the companion's would be the thirteenth. The round reports the overflow, not
// a candidate kept in the project, and the companion stays live with its link.
func TestRunReflectAPromotionOverflowingItsFoldTargetIsReportedAsAnOverflow(t *testing.T) {
	const project = "promote-target-overflow"
	const mergeText = "every repo ships its events over nats and records that choice across all projects"
	dataHome, ids, stderr := reflectOpsRound(t, project,
		func(t *testing.T, ctx context.Context, s *memory.Store) []string {
			t.Helper()
			decisionID, comp, _, err := s.RecordDecision(ctx, project, "Event queue", "we use nats", "because", nil, nil)
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			ids := []string{comp}
			for i := 0; i < 11; i++ {
				id, err := s.Create(ctx, project, memory.Memory{Category: "fact", Source: "mcp", Importance: 0.5,
					Content: fmt.Sprintf("service %d listens on port %d", i, 8000+i)})
				if err != nil {
					t.Fatalf("seed: %v", err)
				}
				ids = append(ids, id)
			}
			if err := s.EnsureProject(ctx, memory.GlobalProjectID, memory.GlobalProjectID, "global"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			target, err := s.Create(ctx, memory.GlobalProjectID, memory.Memory{Category: "decision", Source: "mcp", Importance: 0.6,
				Content: mergeText})
			if err != nil {
				t.Fatalf("seed: %v", err)
			}
			var links []string
			for i := 0; i < 12; i++ {
				links = append(links, "decision:"+fmt.Sprintf("%0*d", len(decisionID), i))
			}
			db, err := openRawDB(os.Getenv("XDG_DATA_HOME"))
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer db.Close() //nolint:errcheck
			if _, err := db.Exec(`UPDATE memories SET source_ref = ? WHERE id = ?`, strings.Join(links, ","), target); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return ids
		},
		func(ids []string) []string {
			ops := []string{"merge " + ids[0] + "," + ids[1] + " -> " + mergeText}
			for _, id := range ids[2:] {
				ops = append(ops, "keep "+id)
			}
			return ops
		},
		"--promote-globals", "--allow-drops")

	if !strings.Contains(stderr, "were dropped with every merged memory left live") {
		t.Errorf("stderr does not carry the overflow note:\n%s", stderr)
	}
	if strings.Contains(stderr, "could not be promoted and were kept in the project") {
		t.Errorf("stderr reports the refused merge as kept in the project:\n%s", stderr)
	}
	db, err := openRawDB(dataHome)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	for _, id := range ids[:2] {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM memories WHERE id = ? AND resolved_at IS NULL`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("source %s did not stay live", id)
		}
	}
	var merged int
	if err := db.QueryRow(`SELECT count(*) FROM memories WHERE project_id = ? AND content = ?`, project, mergeText).Scan(&merged); err != nil {
		t.Fatal(err)
	}
	if merged != 0 {
		t.Errorf("%d project row(s) hold the refused merge", merged)
	}
}
