package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// benchResolveStore records 200 projects under one root, 50 of whose
// directories are then removed, so the table has the mix a long-lived store has.
func benchResolveStore(b *testing.B) (s *Store, hit, miss string) {
	b.Helper()
	ctx := context.Background()
	db, err := OpenDB(":memory:")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	s = NewStore(db, nil)
	root, err := filepath.EvalSymlinks(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		dir := filepath.Join(root, fmt.Sprintf("workspace-%03d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			b.Fatal(err)
		}
		if err := s.EnsureProject(ctx, fmt.Sprintf("id-%03d", i), dir, fmt.Sprintf("name-%03d", i)); err != nil {
			b.Fatal(err)
		}
		if i%4 == 0 {
			_ = os.RemoveAll(dir)
		}
	}
	miss = filepath.Join(root, "unrelated-dir")
	if err := os.MkdirAll(miss, 0o755); err != nil {
		b.Fatal(err)
	}
	return s, filepath.Join(root, "workspace-101"), miss
}

func BenchmarkResolveProjectPathHit(b *testing.B) {
	s, hit, _ := benchResolveStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if id, _, err := s.ResolveProject(ctx, hit); err != nil || id == "" {
			b.Fatalf("hit = (%q, %v)", id, err)
		}
	}
}

func BenchmarkResolveProjectPathMiss(b *testing.B) {
	s, _, miss := benchResolveStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if id, _, err := s.ResolveProject(ctx, miss); err != nil || id != "" {
			b.Fatalf("miss = (%q, %v)", id, err)
		}
	}
}

// Cold: the memo is empty at every resolve, which is the cost of the first
// resolve after a project move or an expiry.
func BenchmarkResolveProjectPathMissCold(b *testing.B) {
	s, _, miss := benchResolveStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		s.canon.invalidate()
		if id, _, err := s.ResolveProject(ctx, miss); err != nil || id != "" {
			b.Fatalf("miss = (%q, %v)", id, err)
		}
	}
}

// AfterSave: a resolve that follows an ensure-project write, which is what a
// save does first. The memo must survive it.
func BenchmarkResolveProjectPathMissAfterSave(b *testing.B) {
	s, hit, miss := benchResolveStore(b)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := s.EnsureProject(ctx, "id-101", hit, "name-101"); err != nil {
			b.Fatal(err)
		}
		if id, _, err := s.ResolveProject(ctx, miss); err != nil || id != "" {
			b.Fatalf("miss = (%q, %v)", id, err)
		}
	}
}
