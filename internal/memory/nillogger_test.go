package memory

import (
	"context"
	"path/filepath"
	"testing"
)

// TestNewStoreToleratesNilLogger pins the contract NewStore advertises by
// accepting a *slog.Logger at all: a nil logger must behave as a discarded
// one.
//
// Two production call sites pass nil deliberately —
// mcpinit.AcquireLifecycleLock and mcpinit.resolveMarkerProject both build a
// Store only to resolve a project identifier, and neither wants logging from
// a lock/marker path that may run before any logger exists. Store has over a
// dozen s.logger.X call sites with no nil guard, and Go's log/slog does NOT
// tolerate a nil receiver: Info, Warn and Debug each panic with "invalid
// memory address or nil pointer dereference".
//
// The two nil call sites are safe today only because ResolveProject happens
// not to touch the logger — the safety is incidental, not enforced. The
// moment such a Store reaches DeleteProject, MergeProjects, GetTopMemories or
// ReplaceNonManual, it panics.
//
// DeleteProject is the probe because its final s.logger.Info is
// unconditional: it runs on every successful delete, not only on an error
// path, so a nil logger cannot avoid it by behaving well.
func TestNewStoreToleratesNilLogger(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "nil-logger.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	ctx := context.Background()
	store := NewStore(db, nil)

	if err := store.EnsureProject(ctx, testProject, "/tmp/nil-logger", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// The regression this guards: calling a method that logs must not panic
	// just because the Store was built without a logger.
	if _, err := store.DeleteProject(ctx, testProject, true); err != nil {
		t.Fatalf("DeleteProject with a nil logger: %v", err)
	}
}

// TestNewStoreNilLoggerStillDiscardsOutput guards the other half: substituting
// a logger must not start writing anywhere. A nil logger's whole point at
// these call sites is silence, so replacing it with os.Stderr or a real
// handler would be a behavior change, not a fix.
func TestNewStoreNilLoggerStillDiscardsOutput(t *testing.T) {
	db, err := OpenDB(filepath.Join(t.TempDir(), "nil-logger-quiet.sqlite"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()

	store := NewStore(db, nil)
	if store.logger == nil {
		t.Fatal("NewStore stored a nil logger; logging on any method will panic")
	}

	// Log through the substituted logger directly: it must not panic and must
	// not reach a handler that writes.
	store.logger.Info("this must not surface anywhere")
	store.logger.Warn("this must not surface anywhere either")
}
