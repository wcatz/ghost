package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileWrite_NewFile(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "new.txt")

	input, _ := json.Marshal(fileWriteInput{
		Path:    file,
		Content: "hello world",
	})

	r := execFileWrite(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("content = %q", got)
	}
	if r.Content != "wrote 11 bytes to "+file {
		t.Errorf("result = %q", r.Content)
	}
}

func TestFileWrite_Overwrite(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "existing.txt")
	if err := os.WriteFile(file, []byte("old content"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(fileWriteInput{
		Path:    file,
		Content: "new content",
	})

	r := execFileWrite(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}

	got, _ := os.ReadFile(file)
	if string(got) != "new content" {
		t.Errorf("content = %q", got)
	}
}

func TestFileWrite_CreatesParentDirs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "a", "b", "c", "deep.txt")

	input, _ := json.Marshal(fileWriteInput{
		Path:    file,
		Content: "deep content",
	})

	r := execFileWrite(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "deep content" {
		t.Errorf("content = %q", got)
	}
}

func TestFileWrite_PathEscape(t *testing.T) {
	dir := t.TempDir()

	input, _ := json.Marshal(fileWriteInput{
		Path:    "../../etc/evil",
		Content: "bad",
	})

	r := execFileWrite(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for path escape")
	}
}

func TestFileWrite_RelativePath(t *testing.T) {
	dir := t.TempDir()

	input, _ := json.Marshal(fileWriteInput{
		Path:    "subdir/file.txt",
		Content: "relative write",
	})

	r := execFileWrite(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}

	got, _ := os.ReadFile(filepath.Join(dir, "subdir", "file.txt"))
	if string(got) != "relative write" {
		t.Errorf("content = %q", got)
	}
}

func TestFileWrite_EmptyContent(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "empty.txt")

	input, _ := json.Marshal(fileWriteInput{
		Path:    file,
		Content: "",
	})

	r := execFileWrite(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	if r.Content != "wrote 0 bytes to "+file {
		t.Errorf("result = %q", r.Content)
	}
}

func TestFileWrite_InvalidJSON(t *testing.T) {
	r := execFileWrite(context.Background(), "/tmp", json.RawMessage(`{bad`))
	if !r.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateAncestors_NoSymlinks(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	err := validateAncestors(dir, filepath.Join(sub, "file.txt"))
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateAncestors_NewDirPath(t *testing.T) {
	dir := t.TempDir()
	// Path where parent directories don't exist yet — should be fine.
	err := validateAncestors(dir, filepath.Join(dir, "x", "y", "z", "file.txt"))
	if err != nil {
		t.Errorf("unexpected error for new dir path: %v", err)
	}
}
