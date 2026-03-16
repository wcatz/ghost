package orchestrator

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/tool"
)

func testOrchestrator(t *testing.T) (*Orchestrator, *memory.Store) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	registry := tool.NewRegistry()
	tool.RegisterAll(registry, store)

	cfg := &config.Config{}
	cfg.Defaults.Mode = "chat"
	cfg.API.ModelQuality = "claude-sonnet-4-5-20250929"

	o := New(nil, store, registry, cfg, logger)
	return o, store
}

func TestOrchestrator_New(t *testing.T) {
	o, _ := testOrchestrator(t)
	if o == nil {
		t.Fatal("New returned nil")
	}
	if o.sessions == nil {
		t.Error("sessions map should be initialized")
	}
}

func TestOrchestrator_StartSession(t *testing.T) {
	o, _ := testOrchestrator(t)

	// Use /tmp as a valid directory.
	s, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if s == nil {
		t.Fatal("StartSession returned nil session")
	}
	if !s.Active {
		t.Error("session should be active")
	}
	if s.ProjectPath == "" {
		t.Error("ProjectPath should not be empty")
	}
}

func TestOrchestrator_StartSession_Idempotent(t *testing.T) {
	o, _ := testOrchestrator(t)

	s1, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession first: %v", err)
	}

	s2, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession second: %v", err)
	}

	if s1 != s2 {
		t.Error("StartSession should return same session for same path")
	}
}

func TestOrchestrator_GetSession(t *testing.T) {
	o, _ := testOrchestrator(t)

	// Before starting, should return nil.
	s := o.GetSession("/tmp")
	if s != nil {
		t.Error("GetSession before start should return nil")
	}

	// Start a session.
	started, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	// Now GetSession should return it.
	s = o.GetSession("/tmp")
	if s != started {
		t.Error("GetSession should return the started session")
	}
}

func TestOrchestrator_GetSessionByID(t *testing.T) {
	o, _ := testOrchestrator(t)

	started, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	s := o.GetSessionByID(started.ProjectID)
	if s != started {
		t.Error("GetSessionByID should return the started session")
	}

	// Unknown ID should return nil.
	s = o.GetSessionByID("nonexistent")
	if s != nil {
		t.Error("GetSessionByID with unknown ID should return nil")
	}
}

func TestOrchestrator_ListSessions(t *testing.T) {
	o, _ := testOrchestrator(t)

	sessions := o.ListSessions()
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions initially, got %d", len(sessions))
	}

	_, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	sessions = o.ListSessions()
	if len(sessions) != 1 {
		t.Errorf("expected 1 session, got %d", len(sessions))
	}
}

func TestOrchestrator_StopSession(t *testing.T) {
	o, _ := testOrchestrator(t)

	_, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := o.StopSession("/tmp"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	sessions := o.ListSessions()
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after stop, got %d", len(sessions))
	}
}

func TestOrchestrator_Shutdown(t *testing.T) {
	o, _ := testOrchestrator(t)

	s, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if err := o.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	if s.Active {
		t.Error("session should be inactive after shutdown")
	}

	sessions := o.ListSessions()
	if len(sessions) != 0 {
		t.Errorf("expected 0 sessions after shutdown, got %d", len(sessions))
	}
}

func TestOrchestrator_AutoApproveYolo(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	store := memory.NewStore(db, logger)
	registry := tool.NewRegistry()

	cfg := &config.Config{}
	cfg.Defaults.Mode = "chat"
	cfg.Defaults.ApprovalMode = "yolo"
	cfg.API.ModelQuality = "claude-sonnet-4-5-20250929"

	o := New(nil, store, registry, cfg, logger)

	s, err := o.StartSession("/tmp")
	if err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	s.mu.Lock()
	auto := s.autoApprove
	s.mu.Unlock()

	if !auto {
		t.Error("session should have autoApprove=true with yolo approval mode")
	}
}
