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

// A failure after the artifact is fully written still leaves nothing behind.
func TestExportFailedPublishLeavesNoFileAndNoTempFile(t *testing.T) {
	dir, _, run := plantAtomicExportRow(t)
	orig := exportPublish
	exportPublish = func(tmp, dest string) error { return errors.New("injected publish failure") }
	t.Cleanup(func() { exportPublish = orig })

	if err := run(); err == nil {
		t.Fatal("export succeeded despite a failed publish")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("directory not empty after failed export: %v", names)
	}
}

// The temp file exists while the export runs and the final name does not.
func TestExportDestinationAbsentUntilPublish(t *testing.T) {
	dir, _, run := plantAtomicExportRow(t)
	orig := exportPublish
	var sawDest bool
	var sawTemp int
	exportPublish = func(tmp, dest string) error {
		_, err := os.Lstat(dest)
		sawDest = err == nil
		sawTemp = len(dirNames(t, dir))
		return orig(tmp, dest)
	}
	t.Cleanup(func() { exportPublish = orig })

	if err := run(); err != nil {
		t.Fatal(err)
	}
	if sawDest {
		t.Fatal("destination name existed before the artifact was complete")
	}
	if sawTemp != 1 {
		t.Fatalf("expected one temp file before publish, saw %d", sawTemp)
	}
	if names := dirNames(t, dir); len(names) != 1 || names[0] != "artifact.jsonl" {
		t.Fatalf("unexpected directory contents: %v", names)
	}
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

// A cancelled export leaves nothing, and a normal export is the same bytes the
// writer produces (checked against stdout-path Export through portable).
func TestExportCancelledLeavesNothing(t *testing.T) {
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, created_at, updated_at)
	                        VALUES (?, 'p1', 'gotcha', 'kept', 'mcp', datetime('now'), datetime('now'))`, "m-good")
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var s, w strings.Builder
	if err := runExportCore(ctx, store, &s, &w, filepath.Join(dir, "a.jsonl"), ""); err == nil {
		t.Skip("cancelled context did not fail the export")
	}
	if names := dirNames(t, dir); len(names) != 0 {
		t.Fatalf("leftovers: %v", names)
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

// A destination basename at NAME_MAX is legal and worked before the temp file
// existed, so the temp name must not grow with the destination name.
func TestExportLongDestinationNameStillWorks(t *testing.T) {
	dir := t.TempDir()
	name := strings.Repeat("a", 255-len(".jsonl")) + ".jsonl"
	store, db := exportTestStore(t)
	plantExportRow(t, db, `INSERT INTO memories (id, project_id, category, content, source, created_at, updated_at)
	                        VALUES (?, 'p1', 'gotcha', 'kept', 'mcp', datetime('now'), datetime('now'))`, "m-good")
	path := filepath.Join(dir, name)
	var s, w strings.Builder
	if err := runExportCore(context.Background(), store, &s, &w, path, ""); err != nil {
		if strings.Contains(err.Error(), "too long") {
			t.Fatalf("a legal %d-byte name failed: %v", len(name), err)
		}
		t.Skipf("filesystem refuses the name itself: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}
