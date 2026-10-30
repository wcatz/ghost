package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestI1894LifecycleExecutableFailureReleasesClaim(t *testing.T) {
	root := t.TempDir()
	data := filepath.Join(root, "data")
	cfg := filepath.Join(root, "config")
	tmp := filepath.Join(root, "tmp")
	for _, d := range []string{data, cfg, tmp, filepath.Join(cfg, "ghost")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	dbPath := filepath.Join(data, "ghost", "ghost.db")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := memory.OpenDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	store := memory.NewStore(db, nil)
	if err := store.EnsureProject(t.Context(), "proj", root, "proj"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})

	t.Setenv("XDG_DATA_HOME", data)
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("HOME", root)
	t.Setenv("TMPDIR", tmp)

	orig := lifecycleExecutable
	lifecycleExecutable = func() (string, error) { return "", fmt.Errorf("boom") }
	t.Cleanup(func() { lifecycleExecutable = orig })

	oldArgs := os.Args
	os.Args = []string{"ghost", "lifecycle", "--project", "proj"}
	t.Cleanup(func() { os.Args = oldArgs })

	code := runLifecycle()
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(data, "ghost", "lifecycle-proj.pid")); err == nil {
		t.Fatal("claim file not removed")
	}
}
