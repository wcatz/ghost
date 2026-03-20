package tool

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileEdit_SingleReplace(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("fmt.Println(\"hello\")\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(fileEditInput{
		Path:      file,
		OldString: "fmt.Println",
		NewString: "log.Println",
	})

	r := execFileEdit(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "log.Println(\"hello\")\n" {
		t.Errorf("file content = %q", got)
	}
	if r.Content != "replaced 1 occurrence(s) in "+file {
		t.Errorf("result = %q", r.Content)
	}
}

func TestFileEdit_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("foo bar foo baz foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(fileEditInput{
		Path:       file,
		OldString:  "foo",
		NewString:  "qux",
		ReplaceAll: true,
	})

	r := execFileEdit(context.Background(), dir, input)
	if r.IsError {
		t.Fatalf("unexpected error: %s", r.Content)
	}
	got, _ := os.ReadFile(file)
	if string(got) != "qux bar qux baz qux\n" {
		t.Errorf("file content = %q", got)
	}
	if r.Content != "replaced 3 occurrence(s) in "+file {
		t.Errorf("result = %q", r.Content)
	}
}

func TestFileEdit_MultipleMatchesNoReplaceAll(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("foo foo foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(fileEditInput{
		Path:      file,
		OldString: "foo",
		NewString: "bar",
	})

	r := execFileEdit(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for multiple matches without replace_all")
	}
	if got := r.Content; got != "old_string found 3 times — set replace_all=true or provide more surrounding context to make it unique" {
		t.Errorf("error = %q", got)
	}
}

func TestFileEdit_OldStringNotFound(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "main.go")
	if err := os.WriteFile(file, []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	input, _ := json.Marshal(fileEditInput{
		Path:      file,
		OldString: "nonexistent",
		NewString: "replacement",
	})

	r := execFileEdit(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for old_string not found")
	}
	if r.Content != "old_string not found in file — read the file first to get exact content" {
		t.Errorf("error = %q", r.Content)
	}
}

func TestFileEdit_FileNotFound(t *testing.T) {
	dir := t.TempDir()

	input, _ := json.Marshal(fileEditInput{
		Path:      filepath.Join(dir, "nope.go"),
		OldString: "a",
		NewString: "b",
	})

	r := execFileEdit(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for missing file")
	}
}

func TestFileEdit_PathEscape(t *testing.T) {
	dir := t.TempDir()

	input, _ := json.Marshal(fileEditInput{
		Path:      "../../etc/passwd",
		OldString: "a",
		NewString: "b",
	})

	r := execFileEdit(context.Background(), dir, input)
	if !r.IsError {
		t.Fatal("expected error for path escape")
	}
}

func TestFileEdit_PreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(file, []byte("#!/bin/bash\necho hello\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Read actual permissions (umask may modify them).
	origInfo, _ := os.Stat(file)
	origPerm := origInfo.Mode().Perm()

	input, _ := json.Marshal(fileEditInput{
		Path:      file,
		OldString: "hello",
		NewString: "world",
	})

	execFileEdit(context.Background(), dir, input)

	info, _ := os.Stat(file)
	if info.Mode().Perm() != origPerm {
		t.Errorf("permissions = %o, want %o (original)", info.Mode().Perm(), origPerm)
	}
}

func TestFileEdit_InvalidJSON(t *testing.T) {
	r := execFileEdit(context.Background(), "/tmp", json.RawMessage(`{broken`))
	if !r.IsError {
		t.Fatal("expected error for invalid JSON")
	}
}
