package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestImportUnguardedFieldsRefusesCredential reproduces the issue where
// ghost import stores task.branch, task.completed_at, memory.last_accessed,
// and decision.created_at without the token guard or timestamp validation.
func TestImportUnguardedFieldsRefusesCredential(t *testing.T) {
	ctx := context.Background()
	// A GitHub token shaped value that the secret guard recognizes.
	token := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	// A 200KB string for the branch size bound test.
	oversizeBranch := token + strings.Repeat("x", 200000)

	// Valid timestamp format for comparison
	validStamp := time.Date(2026, 1, 15, 10, 30, 0, 0, time.UTC).Format(StoredStampLayout)

	t.Run("task/branch with credential and oversize", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		// Test credential-shaped branch
		_, _, err := store.ImportTask(ctx, Task{
			ID: "t1", ProjectID: "p1", Title: "a task",
			Status: "pending", Branch: token,
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted a credential-shaped branch")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("refusal echoes the value: %v", err)
		}
		if !strings.Contains(err.Error(), "branch") {
			t.Errorf("refusal does not name 'branch': %v", err)
		}

		// Test oversize branch with credential - token guard runs first, so this
		// should be refused as a credential, not as oversize.
		_, _, err = store.ImportTask(ctx, Task{
			ID: "t2", ProjectID: "p1", Title: "a task",
			Status: "pending", Branch: oversizeBranch,
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted an oversize branch with credential")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("oversize token-bearing branch: errors.Is(err, ErrSecretContent) = false, got %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("refusal echoes the value: %v", err)
		}
		if !strings.Contains(err.Error(), "branch") {
			t.Errorf("refusal does not name 'branch': %v", err)
		}

		// Test oversize branch without credential - should be refused for size.
		plainOversize := strings.Repeat("x", MaxBranchLen+1)
		_, _, err = store.ImportTask(ctx, Task{
			ID: "t3", ProjectID: "p1", Title: "a task",
			Status: "pending", Branch: plainOversize,
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted an oversize branch without credential")
		}
		if !strings.Contains(err.Error(), "branch") {
			t.Errorf("refusal does not name 'branch': %v", err)
		}
	})

	t.Run("task/completed_at with credential", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		_, _, err := store.ImportTask(ctx, Task{
			ID: "t3", ProjectID: "p1", Title: "a task",
			Status: "done", CompletedAt: token,
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted a credential-shaped completed_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("refusal echoes the value: %v", err)
		}
		if !strings.Contains(err.Error(), "completed_at") {
			t.Errorf("refusal does not name 'completed_at': %v", err)
		}
	})

	t.Run("task/completed_at with invalid timestamp", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		_, _, err := store.ImportTask(ctx, Task{
			ID: "t4", ProjectID: "p1", Title: "a task",
			Status: "done", CompletedAt: "not-a-timestamp",
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted an invalid completed_at timestamp")
		}
		if !strings.Contains(err.Error(), "completed_at") {
			t.Errorf("refusal does not name 'completed_at': %v", err)
		}
	})

	t.Run("memory/last_accessed with credential", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", LastAccessed: &token,
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted a credential-shaped last_accessed")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("refusal echoes the value: %v", err)
		}
		if !strings.Contains(err.Error(), "last_accessed") {
			t.Errorf("refusal does not name 'last_accessed': %v", err)
		}
	})

	t.Run("memory/last_accessed with invalid timestamp", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		invalid := "not-a-timestamp"
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m2", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", LastAccessed: &invalid,
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted an invalid last_accessed timestamp")
		}
		if !strings.Contains(err.Error(), "last_accessed") {
			t.Errorf("refusal does not name 'last_accessed': %v", err)
		}
	})

	t.Run("decision/created_at with credential", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		_, _, err := store.ImportDecision(ctx, Decision{
			ID: "d1", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", CreatedAt: token,
		}, true)
		if err == nil {
			t.Fatal("ImportDecision accepted a credential-shaped created_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
		if strings.Contains(err.Error(), token) {
			t.Errorf("refusal echoes the value: %v", err)
		}
		if !strings.Contains(err.Error(), "created_at") {
			t.Errorf("refusal does not name 'created_at': %v", err)
		}
	})

	t.Run("decision/created_at with invalid timestamp", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		_, _, err := store.ImportDecision(ctx, Decision{
			ID: "d2", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", CreatedAt: "not-a-timestamp",
		}, true)
		if err == nil {
			t.Fatal("ImportDecision accepted an invalid created_at timestamp")
		}
		if !strings.Contains(err.Error(), "created_at") {
			t.Errorf("refusal does not name 'created_at': %v", err)
		}
	})

	t.Run("valid timestamps are accepted", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		// Task with valid completed_at
		created, _, err := store.ImportTask(ctx, Task{
			ID: "t5", ProjectID: "p1", Title: "a task",
			Status: "done", CompletedAt: validStamp,
		}, true)
		if err != nil {
			t.Fatalf("ImportTask refused valid completed_at: %v", err)
		}
		if !created {
			t.Error("ImportTask did not create the task")
		}

		// Memory with valid last_accessed
		created, _, _, err = store.ImportMemory(ctx, PortableMemory{
			ID: "m3", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", LastAccessed: &validStamp,
		}, ImportOptions{Apply: true})
		if err != nil {
			t.Fatalf("ImportMemory refused valid last_accessed: %v", err)
		}
		if !created {
			t.Error("ImportMemory did not create the memory")
		}

		// Decision with valid created_at
		created, _, err = store.ImportDecision(ctx, Decision{
			ID: "d3", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", CreatedAt: validStamp,
		}, true)
		if err != nil {
			t.Fatalf("ImportDecision refused valid created_at: %v", err)
		}
		if !created {
			t.Error("ImportDecision did not create the decision")
		}
	})

	t.Run("round-trip export/import with RFC3339 last_accessed", func(t *testing.T) {
		// This test ensures that a normal export (which uses Store.Touch and writes
		// last_accessed as RFC3339) can be imported without refusal.
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}

		// Create a memory and touch it so last_accessed is set to RFC3339
		id, _, _, err := src.Upsert(ctx, "p1", "fact", "round-trip memory", "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := src.Touch(ctx, []string{id}); err != nil {
			t.Fatalf("Touch: %v", err)
		}

		// Verify the stored last_accessed is RFC3339
		var storedLastAccessed string
		if err := src.db.QueryRowContext(ctx, `SELECT last_accessed FROM memories WHERE id = ?`, id).Scan(&storedLastAccessed); err != nil {
			t.Fatalf("read last_accessed: %v", err)
		}
		if storedLastAccessed == "" {
			t.Fatal("last_accessed is empty after Touch")
		}
		// Verify it parses as RFC3339
		if _, err := time.Parse(time.RFC3339, storedLastAccessed); err != nil {
			t.Fatalf("last_accessed %q is not RFC3339: %v", storedLastAccessed, err)
		}

		// Export the memory
		mems, err := src.PortableMemories(ctx, []string{"p1"})
		if err != nil {
			t.Fatalf("PortableMemories: %v", err)
		}
		if len(mems) != 1 {
			t.Fatalf("expected 1 memory, got %d", len(mems))
		}
		if mems[0].LastAccessed == nil || *mems[0].LastAccessed == "" {
			t.Fatal("exported memory has empty LastAccessed")
		}
		exportedLastAccessed := *mems[0].LastAccessed
		if _, err := time.Parse(time.RFC3339, exportedLastAccessed); err != nil {
			t.Fatalf("exported LastAccessed %q is not RFC3339: %v", exportedLastAccessed, err)
		}

		// Import into a fresh store
		dst := testStore(t)
		if err := dst.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject dst: %v", err)
		}

		created, _, _, err := dst.ImportMemory(ctx, mems[0], ImportOptions{Apply: true})
		if err != nil {
			t.Fatalf("ImportMemory refused RFC3339 last_accessed: %v", err)
		}
		if !created {
			t.Error("ImportMemory did not create the memory on round-trip")
		}

		// Verify the imported memory has the same last_accessed
		var importedLastAccessed string
		if err := dst.db.QueryRowContext(ctx, `SELECT last_accessed FROM memories WHERE id = ?`, id).Scan(&importedLastAccessed); err != nil {
			t.Fatalf("read imported last_accessed: %v", err)
		}
		if importedLastAccessed != storedLastAccessed {
			t.Errorf("imported last_accessed %q != original %q", importedLastAccessed, storedLastAccessed)
		}
	})

	// Six new timestamp fields: test token refusal, wrong layout refusal, and
	// round-trip acceptance for each.
	t.Run("memory/created_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", CreatedAt: token,
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted a credential-shaped created_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("memory/created_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", CreatedAt: "not-a-timestamp",
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted an invalid created_at timestamp")
		}
		if !strings.Contains(err.Error(), "created_at") {
			t.Errorf("refusal does not name 'created_at': %v", err)
		}
	})
	t.Run("memory/created_at round-trip", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := src.Upsert(ctx, "p1", "fact", "memory for created_at round-trip", "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		mems, err := src.PortableMemories(ctx, []string{"p1"})
		if err != nil {
			t.Fatalf("PortableMemories: %v", err)
		}
		dst := testStore(t)
		if err := dst.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject dst: %v", err)
		}
		created, _, _, err := dst.ImportMemory(ctx, mems[0], ImportOptions{Apply: true})
		if err != nil {
			t.Fatalf("ImportMemory refused valid created_at: %v", err)
		}
		if !created {
			t.Error("ImportMemory did not create the memory on round-trip")
		}
	})

	t.Run("memory/updated_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", UpdatedAt: token,
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted a credential-shaped updated_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("memory/updated_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", UpdatedAt: "not-a-timestamp",
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted an invalid updated_at timestamp")
		}
		if !strings.Contains(err.Error(), "updated_at") {
			t.Errorf("refusal does not name 'updated_at': %v", err)
		}
	})
	t.Run("memory/updated_at round-trip", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := src.Upsert(ctx, "p1", "fact", "memory for updated_at round-trip", "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		mems, err := src.PortableMemories(ctx, []string{"p1"})
		if err != nil {
			t.Fatalf("PortableMemories: %v", err)
		}
		dst := testStore(t)
		if err := dst.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject dst: %v", err)
		}
		created, _, _, err := dst.ImportMemory(ctx, mems[0], ImportOptions{Apply: true})
		if err != nil {
			t.Fatalf("ImportMemory refused valid updated_at: %v", err)
		}
		if !created {
			t.Error("ImportMemory did not create the memory on round-trip")
		}
	})

	t.Run("memory/resolved_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", ResolvedAt: &token,
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted a credential-shaped resolved_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("memory/resolved_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		invalid := "not-a-timestamp"
		_, _, _, err := store.ImportMemory(ctx, PortableMemory{
			ID: "m1", ProjectID: "p1", Category: "fact", Source: "mcp",
			Content: "a memory", ResolvedAt: &invalid,
		}, ImportOptions{Apply: true})
		if err == nil {
			t.Fatal("ImportMemory accepted an invalid resolved_at timestamp")
		}
		if !strings.Contains(err.Error(), "resolved_at") {
			t.Errorf("refusal does not name 'resolved_at': %v", err)
		}
	})
	t.Run("memory/resolved_at round-trip via SetResolved", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		id, _, _, err := src.Upsert(ctx, "p1", "fact", "memory for resolved_at round-trip", "mcp", 0.7, nil)
		if err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		// Use SetResolved to set resolved_at (this is what the real writer does)
		if _, err := src.SetResolved(ctx, []string{id}); err != nil {
			t.Fatalf("SetResolved: %v", err)
		}
		mems, err := src.PortableMemories(ctx, []string{"p1"})
		if err != nil {
			t.Fatalf("PortableMemories: %v", err)
		}
		dst := testStore(t)
		if err := dst.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject dst: %v", err)
		}
		created, _, _, err := dst.ImportMemory(ctx, mems[0], ImportOptions{Apply: true})
		if err != nil {
			t.Fatalf("ImportMemory refused valid resolved_at: %v", err)
		}
		if !created {
			t.Error("ImportMemory did not create the memory on round-trip")
		}
	})

	t.Run("task/created_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportTask(ctx, Task{
			ID: "t1", ProjectID: "p1", Title: "a task",
			Status: "pending", CreatedAt: token,
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted a credential-shaped created_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("task/created_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportTask(ctx, Task{
			ID: "t1", ProjectID: "p1", Title: "a task",
			Status: "pending", CreatedAt: "not-a-timestamp",
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted an invalid created_at timestamp")
		}
		if !strings.Contains(err.Error(), "created_at") {
			t.Errorf("refusal does not name 'created_at': %v", err)
		}
	})
	t.Run("task/created_at round-trip", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		id, err := src.CreateTask(ctx, "p1", "task for created_at round-trip", "desc", 2)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		got, err := src.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		_, _, err = src.ImportTask(ctx, got, true)
		if err != nil {
			t.Fatalf("ImportTask refused valid created_at: %v", err)
		}
	})

	t.Run("task/updated_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportTask(ctx, Task{
			ID: "t1", ProjectID: "p1", Title: "a task",
			Status: "pending", UpdatedAt: token,
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted a credential-shaped updated_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("task/updated_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportTask(ctx, Task{
			ID: "t1", ProjectID: "p1", Title: "a task",
			Status: "pending", UpdatedAt: "not-a-timestamp",
		}, true)
		if err == nil {
			t.Fatal("ImportTask accepted an invalid updated_at timestamp")
		}
		if !strings.Contains(err.Error(), "updated_at") {
			t.Errorf("refusal does not name 'updated_at': %v", err)
		}
	})
	t.Run("task/updated_at round-trip", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		id, err := src.CreateTask(ctx, "p1", "task for updated_at round-trip", "desc", 2)
		if err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		got, err := src.GetTask(ctx, id)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		_, _, err = src.ImportTask(ctx, got, true)
		if err != nil {
			t.Fatalf("ImportTask refused valid updated_at: %v", err)
		}
	})

	t.Run("decision/created_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportDecision(ctx, Decision{
			ID: "d1", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", CreatedAt: token,
		}, true)
		if err == nil {
			t.Fatal("ImportDecision accepted a credential-shaped created_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("decision/created_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportDecision(ctx, Decision{
			ID: "d1", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", CreatedAt: "not-a-timestamp",
		}, true)
		if err == nil {
			t.Fatal("ImportDecision accepted an invalid created_at timestamp")
		}
		if !strings.Contains(err.Error(), "created_at") {
			t.Errorf("refusal does not name 'created_at': %v", err)
		}
	})
	t.Run("decision/created_at round-trip", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := src.RecordDecision(ctx, "p1", "decision for created_at round-trip", "do it", "rationale", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
		decs, err := src.ListDecisions(ctx, "p1", "", 1)
		if err != nil {
			t.Fatalf("ListDecisions: %v", err)
		}
		if len(decs) == 0 {
			t.Fatal("no decision found")
		}
		dec := decs[0]
		_, _, err = src.ImportDecision(ctx, dec, true)
		if err != nil {
			t.Fatalf("ImportDecision refused valid created_at: %v", err)
		}
	})

	t.Run("decision/updated_at token refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportDecision(ctx, Decision{
			ID: "d1", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", UpdatedAt: token,
		}, true)
		if err == nil {
			t.Fatal("ImportDecision accepted a credential-shaped updated_at")
		}
		if !errors.Is(err, ErrSecretContent) {
			t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
		}
	})
	t.Run("decision/updated_at wrong layout refused", func(t *testing.T) {
		store := portableTestStore(t)
		if err := store.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, err := store.ImportDecision(ctx, Decision{
			ID: "d1", ProjectID: "p1", Title: "a decision", Status: "active",
			Decision: "do it", Rationale: "because", UpdatedAt: "not-a-timestamp",
		}, true)
		if err == nil {
			t.Fatal("ImportDecision accepted an invalid updated_at timestamp")
		}
		if !strings.Contains(err.Error(), "updated_at") {
			t.Errorf("refusal does not name 'updated_at': %v", err)
		}
	})
	t.Run("decision/updated_at round-trip", func(t *testing.T) {
		src := testStore(t)
		if err := src.EnsureProject(ctx, "p1", "/src/p1", "p1"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		_, _, _, err := src.RecordDecision(ctx, "p1", "decision for updated_at round-trip", "do it", "rationale", nil, nil)
		if err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
		decs, err := src.ListDecisions(ctx, "p1", "", 1)
		if err != nil {
			t.Fatalf("ListDecisions: %v", err)
		}
		if len(decs) == 0 {
			t.Fatal("no decision found")
		}
		dec := decs[0]
		_, _, err = src.ImportDecision(ctx, dec, true)
		if err != nil {
			t.Fatalf("ImportDecision refused valid updated_at: %v", err)
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
