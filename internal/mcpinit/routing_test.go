package mcpinit

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestResolveSessionProject covers the home-dir/root fallback routing added
// for issue #391: an unmatched cwd normally resolves to nothing, but with
// routing.default_project configured a $HOME or "/" session falls back to it.
func TestResolveSessionProject(t *testing.T) {
	setup := func(t *testing.T, defaultProject string) *memory.Store {
		t.Helper()
		t.Setenv("GHOST_ROUTING_DEFAULT_PROJECT", defaultProject)
		db, err := memory.OpenDB(":memory:")
		if err != nil {
			t.Fatalf("OpenDB: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		store := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := store.EnsureProject(context.Background(), "infra", "/home/wayne/git/infra", "infrastructure"); err != nil {
			t.Fatalf("EnsureProject: %v", err)
		}
		return store
	}

	t.Run("direct path hit needs no routing", func(t *testing.T) {
		store := setup(t, "")
		id, name := resolveSessionProject(context.Background(), store, "/home/wayne/git/infra/internal/x")
		if id != "infra" || name != "infrastructure" {
			t.Fatalf("got (%q, %q), want (infra, infrastructure)", id, name)
		}
	})

	t.Run("home cwd falls back to configured default", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		store := setup(t, "infrastructure")
		id, name := resolveSessionProject(context.Background(), store, home)
		if id != "infra" || name != "infrastructure" {
			t.Fatalf("got (%q, %q), want (infra, infrastructure)", id, name)
		}
	})

	t.Run("filesystem root falls back to configured default", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		store := setup(t, "infrastructure")
		id, _ := resolveSessionProject(context.Background(), store, "/")
		if id != "infra" {
			t.Fatalf("got %q, want infra", id)
		}
	})

	t.Run("no default configured behaves as today", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		store := setup(t, "")
		id, name := resolveSessionProject(context.Background(), store, home)
		if id != "" || name != "" {
			t.Fatalf("got (%q, %q), want empty", id, name)
		}
	})

	t.Run("non-home unmatched cwd never routes", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		store := setup(t, "infrastructure")
		other := filepath.Join(home, "unrelated")
		if err := os.MkdirAll(other, 0o755); err != nil {
			t.Fatal(err)
		}
		id, name := resolveSessionProject(context.Background(), store, other)
		if id != "" || name != "" {
			t.Fatalf("got (%q, %q), want empty — only HOME and / may route", id, name)
		}
	})

	t.Run("configured but nonexistent default degrades to miss", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", home)
		store := setup(t, "does-not-exist")
		id, name := resolveSessionProject(context.Background(), store, home)
		if id != "" || name != "" {
			t.Fatalf("got (%q, %q), want empty", id, name)
		}
	})
}
