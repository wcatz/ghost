package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plantAtomicExportRow(t *testing.T) (dir, path string, run func() error) {
	t.Helper()
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, created_at, updated_at)
	                        VALUES (?, 'p1', 'gotcha', 'kept', 'mcp', datetime('now'), datetime('now'))`, "m-good")
	dir = t.TempDir()
	path = filepath.Join(dir, "artifact.jsonl")
	run = func() error {
		var summary, warn strings.Builder
		return runExportCore(context.Background(), store, &summary, &warn, path, "")
	}
	return dir, path, run
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

// An existing destination is still refused and left untouched, with no temp file.
func TestExportStillRefusesExistingDestination(t *testing.T) {
	dir, path, run := plantAtomicExportRow(t)
	if err := os.WriteFile(path, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run()
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "precious" {
		t.Fatalf("existing file changed: %q", b)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("stray files: %v", names)
	}
}

// A destination that appears between the check and the publish is not replaced.
func TestExportPublishDoesNotReplaceARacingFile(t *testing.T) {
	dir, path, run := plantAtomicExportRow(t)
	orig := exportPublish
	exportPublish = func(tmp, dest string) error {
		if err := os.WriteFile(dest, []byte("raced"), 0o600); err != nil {
			t.Fatal(err)
		}
		return orig(tmp, dest)
	}
	t.Cleanup(func() { exportPublish = orig })
	if err := run(); err == nil {
		t.Fatal("export replaced a file that appeared mid-run")
	}
	if b, _ := os.ReadFile(path); string(b) != "raced" {
		t.Fatalf("racing file replaced: %q", b)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("stray files: %v", names)
	}
}

func forceNoLinks(t *testing.T) {
	t.Helper()
	orig := exportLink
	exportLink = func(string, string) error { return errors.New("operation not supported") }
	t.Cleanup(func() { exportLink = orig })
}

// On a filesystem without hard links the fallback must still refuse a file that
// appeared after the up-front check, and leave it untouched.
func TestExportCopyFallbackDoesNotReplaceARacingFile(t *testing.T) {
	dir, path, run := plantAtomicExportRow(t)
	forceNoLinks(t)
	orig := exportPublish
	exportPublish = func(tmp, dest string) error {
		if err := os.WriteFile(dest, []byte("raced"), 0o600); err != nil {
			t.Fatal(err)
		}
		return orig(tmp, dest)
	}
	t.Cleanup(func() { exportPublish = orig })
	err := run()
	if err == nil || !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if b, _ := os.ReadFile(path); string(b) != "raced" {
		t.Fatalf("racing file replaced: %q", b)
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("stray files: %v", names)
	}
}

// The fallback publishes the same bytes a link would.
func TestExportCopyFallbackPublishesSameBytes(t *testing.T) {
	_, path, run := plantAtomicExportRow(t)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	want, _ := os.ReadFile(path)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	forceNoLinks(t)
	dir := filepath.Dir(path)
	if err := run(); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != string(want) || len(want) == 0 {
		t.Fatalf("fallback bytes differ")
	}
	if names := dirNames(t, dir); len(names) != 1 {
		t.Fatalf("stray files: %v", names)
	}
}
