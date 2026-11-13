package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestImportUnguardedFieldsRefusesCredential: task.branch, task.completed_at,
// memory.last_accessed, memory.created_at/updated_at/resolved_at,
// task.created_at/updated_at and decision.created_at/updated_at are token-guarded
// and stamp-validated on import. A token-shaped value is refused as a token and
// never echoed, an unreadable stamp is refused naming the field, a readable one
// is accepted.
func TestImportUnguardedFieldsRefusesCredential(t *testing.T) {
	ctx := context.Background()
	token := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	validStamp := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC).Format(StoredStampLayout)

	mem := func(f func(m *PortableMemory, v string)) func(*Store, string) error {
		return func(s *Store, v string) error {
			m := PortableMemory{ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp", Content: "a memory"}
			f(&m, v)
			_, _, _, err := s.ImportMemory(ctx, m, ImportOptions{Apply: true})
			return err
		}
	}
	task := func(f func(t *Task, v string)) func(*Store, string) error {
		return func(s *Store, v string) error {
			tk := Task{ID: "t1", ProjectID: "p1", Title: "a task", Status: "done"}
			f(&tk, v)
			_, _, err := s.ImportTask(ctx, tk, true)
			return err
		}
	}
	dec := func(f func(d *Decision, v string)) func(*Store, string) error {
		return func(s *Store, v string) error {
			d := Decision{ID: "d1", ProjectID: "p1", Title: "a decision", Status: "active", Decision: "do it", Rationale: "because"}
			f(&d, v)
			_, _, err := s.ImportDecision(ctx, d, true)
			return err
		}
	}
	fields := []struct {
		name string
		do   func(*Store, string) error
	}{
		{"branch", task(func(t *Task, v string) { t.Branch = v })},
		{"completed_at", task(func(t *Task, v string) { t.CompletedAt = v })},
		{"created_at", task(func(t *Task, v string) { t.CreatedAt = v })},
		{"updated_at", task(func(t *Task, v string) { t.UpdatedAt = v })},
		{"last_accessed", mem(func(m *PortableMemory, v string) { m.LastAccessed = &v })},
		{"resolved_at", mem(func(m *PortableMemory, v string) { m.ResolvedAt = &v })},
		{"created_at", mem(func(m *PortableMemory, v string) { m.CreatedAt = v })},
		{"updated_at", mem(func(m *PortableMemory, v string) { m.UpdatedAt = v })},
		{"created_at", dec(func(d *Decision, v string) { d.CreatedAt = v })},
		{"updated_at", dec(func(d *Decision, v string) { d.UpdatedAt = v })},
	}
	for i, f := range fields {
		t.Run(f.name+"/"+string(rune('a'+i)), func(t *testing.T) {
			store := portableTestStore(t)
			if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			err := f.do(store, token)
			if !errors.Is(err, ErrSecretContent) {
				t.Fatalf("token-shaped %s: err = %v, want ErrSecretContent", f.name, err)
			}
			if strings.Contains(err.Error(), token) || !strings.Contains(err.Error(), f.name) {
				t.Errorf("refusal must name %s and not echo the value: %v", f.name, err)
			}
			bad := "not-a-timestamp"
			if f.name == "branch" {
				// A token-bearing oversize branch is still refused as a token;
				// a plain oversize one is refused for its size.
				if err := f.do(store, token+strings.Repeat("x", 200000)); !errors.Is(err, ErrSecretContent) {
					t.Errorf("oversize token-bearing branch: err = %v, want ErrSecretContent", err)
				}
				bad = strings.Repeat("x", MaxBranchLen+1)
			}
			if err := f.do(store, bad); err == nil || !strings.Contains(err.Error(), f.name) {
				t.Errorf("unreadable %s: err = %v, want a refusal naming it", f.name, err)
			}
			if f.name == "branch" {
				return
			}
			if err := f.do(store, validStamp); err != nil {
				t.Errorf("readable %s refused: %v", f.name, err)
			}
		})
	}

	t.Run("export round-trip keeps every stamp", func(t *testing.T) {
		src := testStore(t)
		dst := testStore(t)
		for _, s := range []*Store{src, dst} {
			if err := s.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
		}
		id, _, _, err := src.Upsert(ctx, "p1", "fact", "round-trip memory", "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := src.Touch(ctx, []string{id}); err != nil {
			t.Fatalf("Touch: %v", err)
		}
		if _, err := src.SetResolved(ctx, []string{id}); err != nil {
			t.Fatalf("SetResolved: %v", err)
		}
		var stored string
		if err := src.db.QueryRowContext(ctx, `SELECT last_accessed FROM memories WHERE id = ?`, id).Scan(&stored); err != nil {
			t.Fatalf("read last_accessed: %v", err)
		}
		if _, err := time.Parse(time.RFC3339, stored); err != nil {
			t.Fatalf("last_accessed %q is not RFC3339: %v", stored, err)
		}
		mems, err := src.PortableMemories(ctx, []string{"p1"})
		if err != nil || len(mems) != 1 {
			t.Fatalf("PortableMemories = %d, %v", len(mems), err)
		}
		if created, _, _, err := dst.ImportMemory(ctx, mems[0], ImportOptions{Apply: true}); err != nil || !created {
			t.Fatalf("ImportMemory refused an exported memory: created=%v err=%v", created, err)
		}
		var got string
		if err := dst.db.QueryRowContext(ctx, `SELECT last_accessed FROM memories WHERE id = ?`, id).Scan(&got); err != nil || got != stored {
			t.Errorf("imported last_accessed %q (%v) != original %q", got, err, stored)
		}

		tid, err := src.CreateTask(ctx, "p1", "task for round-trip", "desc", 2)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		tk, err := src.GetTask(ctx, tid)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if _, _, err := dst.ImportTask(ctx, tk, true); err != nil {
			t.Errorf("ImportTask refused an exported task: %v", err)
		}
		if _, _, _, err := src.RecordDecision(ctx, "p1", "decision for round-trip", "do it", "rationale", nil, nil); err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
		decs, err := src.ListDecisions(ctx, "p1", "", 1)
		if err != nil || len(decs) == 0 {
			t.Fatalf("ListDecisions = %d, %v", len(decs), err)
		}
		if _, _, err := dst.ImportDecision(ctx, decs[0], true); err != nil {
			t.Errorf("ImportDecision refused an exported decision: %v", err)
		}
	})
}

// TestImportedStampsAcceptAWholeDay pins that every stamp column takes the
// layouts its readers take: ParseStamp reads a whole-day stamp, and Export runs
// the same predicates, so a refused layout would leave the row out of a backup.
func TestImportedStampsAcceptAWholeDay(t *testing.T) {
	const day = "2026-01-04"
	stamp := time.Date(2026, 1, 4, 10, 0, 0, 0, time.UTC).Format(StoredStampLayout)

	for _, field := range []string{"created_at", "updated_at", "resolved_at", "last_accessed"} {
		m := PortableMemory{ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp", Content: "a memory", CreatedAt: stamp, UpdatedAt: stamp}
		v := day
		switch field {
		case "created_at":
			m.CreatedAt = day
		case "updated_at":
			m.UpdatedAt = day
		case "resolved_at":
			m.ResolvedAt = &v
		case "last_accessed":
			m.LastAccessed = &v
		}
		if err := CheckImportedMemory(m); err != nil {
			t.Errorf("memory %s %q refused: %v", field, day, err)
		}
	}
	for _, field := range []string{"created_at", "updated_at", "completed_at"} {
		tk := Task{ID: "t1", ProjectID: "p1", Title: "a task", Status: "done", Priority: 2, CreatedAt: stamp, UpdatedAt: stamp, CompletedAt: stamp}
		switch field {
		case "created_at":
			tk.CreatedAt = day
		case "updated_at":
			tk.UpdatedAt = day
		case "completed_at":
			tk.CompletedAt = day
		}
		if err := CheckImportedTask(tk); err != nil {
			t.Errorf("task %s %q refused: %v", field, day, err)
		}
	}
	for _, field := range []string{"created_at", "updated_at"} {
		d := Decision{ID: "d1", ProjectID: "p1", Title: "a decision", Decision: "do it", Rationale: "because", Status: "active", CreatedAt: stamp, UpdatedAt: stamp}
		if field == "created_at" {
			d.CreatedAt = day
		} else {
			d.UpdatedAt = day
		}
		if err := CheckImportedDecision(d); err != nil {
			t.Errorf("decision %s %q refused: %v", field, day, err)
		}
	}
}

// TestImportedTokenGuardRunsBeforeTheValidityCheck pins the order of the two
// refusals: export picks its advice from the refusal's kind, so a row that holds
// a token-shaped field AND an unreadable stamp must be refused as the former, or
// the report would only ever tell the operator to repair the stamp.
func TestImportedTokenGuardRunsBeforeTheValidityCheck(t *testing.T) {
	token := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	until := "next tuesday"
	err := CheckImportedMemory(PortableMemory{
		ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
		Content: "holds " + token, ValidUntil: &until,
	})
	if err == nil {
		t.Fatal("CheckImportedMemory accepted a token-shaped content with an unreadable valid_until")
	}
	if !errors.Is(err, ErrSecretContent) {
		t.Errorf("refused as %T (%v), want the token guard's refusal", err, err)
	}
}
