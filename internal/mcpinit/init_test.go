package mcpinit

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHandleSessionStartHook(t *testing.T) {
	var out bytes.Buffer
	HandleSessionStartHook(strings.NewReader(`{"event":"SessionStart"}`), &out)

	output := out.String()
	if output == "" {
		t.Error("hook output should not be empty")
	}
	if !strings.Contains(output, "ghost_memory_save") && !strings.Contains(output, "Ghost context") {
		t.Error("hook output should mention ghost_memory_save or Ghost context")
	}
}

func TestWriteRedirects_CreatesFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")

	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("expected redirect file to be created: %v", err)
	}

	content := string(data)
	if !strings.Contains(content, "stored in Ghost") {
		t.Error("redirect should contain 'stored in Ghost'")
	}
	if !strings.Contains(content, "myproject") {
		t.Error("redirect should contain project name")
	}

	output := out.String()
	if !strings.Contains(output, "created redirect") {
		t.Errorf("output should say 'created redirect', got: %s", output)
	}
}

func TestWriteRedirects_SkipsExisting(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Pre-create the redirect file.
	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("All project knowledge is stored in Ghost."), 0644); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	output := out.String()
	if !strings.Contains(output, "redirect exists") {
		t.Errorf("should report redirect exists, got: %s", output)
	}
}

func TestWriteRedirects_SkipsRelativePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projects := []projectInfo{
		{ID: "abc123", Path: "relative/path", Name: "rel"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	// Should produce no output for relative paths.
	if out.String() != "" {
		t.Errorf("expected no output for relative path, got: %s", out.String())
	}
}

func TestWriteRedirects_DoesNotClobber(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Pre-create a file with user content (not a Ghost redirect).
	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	dir := filepath.Join(home, ".claude", "projects", encoded, "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	original := "User's custom memory content here."
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(original), 0644); err != nil {
		t.Fatal(err)
	}

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, false)

	// Verify it was NOT overwritten.
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if string(data) != original {
		t.Error("writeRedirects clobbered existing non-Ghost MEMORY.md")
	}

	output := out.String()
	if !strings.Contains(output, "not overwriting") {
		t.Errorf("should say 'not overwriting', got: %s", output)
	}
}

func TestWriteRedirects_DryRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	projects := []projectInfo{
		{ID: "abc123", Path: "/home/test/git/myproject", Name: "myproject"},
	}

	var out bytes.Buffer
	writeRedirects(&out, projects, true)

	output := out.String()
	if !strings.Contains(output, "would create redirect") {
		t.Errorf("dry run should say 'would create redirect', got: %s", output)
	}

	// Verify no file was created.
	encoded := strings.ReplaceAll("/home/test/git/myproject", "/", "-")
	target := filepath.Join(home, ".claude", "projects", encoded, "memory", "MEMORY.md")
	if _, err := os.Stat(target); err == nil {
		t.Error("dry run should not create files")
	}
}

func TestEnsureAutoMemoryDisabled_SetsFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ensureAutoMemoryDisabled(&out, sf, false); err != nil {
		t.Fatalf("ensureAutoMemoryDisabled: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "set autoMemoryEnabled: false") {
		t.Errorf("expected 'set autoMemoryEnabled: false' in output, got: %s", output)
	}

	v, present := sf.getAutoMemoryEnabled()
	if !present || v {
		t.Errorf("expected autoMemoryEnabled=false, got present=%v value=%v", present, v)
	}
}

func TestEnsureAutoMemoryDisabled_Idempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"autoMemoryEnabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ensureAutoMemoryDisabled(&out, sf, false); err != nil {
		t.Fatalf("ensureAutoMemoryDisabled: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "already false") {
		t.Errorf("expected 'already false' in output, got: %s", output)
	}
}

func TestEnsureAutoMemoryDisabled_DryRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	path := filepath.Join(home, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}

	sf, err := loadSettings(path)
	if err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := ensureAutoMemoryDisabled(&out, sf, true); err != nil {
		t.Fatalf("ensureAutoMemoryDisabled dry run: %v", err)
	}

	output := out.String()
	if !strings.Contains(output, "would set autoMemoryEnabled: false") {
		t.Errorf("expected 'would set autoMemoryEnabled: false' in output, got: %s", output)
	}

	// In dry run, the in-memory state should not be modified.
	_, present := sf.getAutoMemoryEnabled()
	if present {
		t.Error("dry run should not modify settings in memory")
	}
}

func TestRetryHint(t *testing.T) {
	err := retryHint(fmt.Errorf("something broke"))
	msg := err.Error()
	if !strings.Contains(msg, "something broke") {
		t.Error("should preserve original error")
	}
	if !strings.Contains(msg, "ghost mcp init") {
		t.Error("should include retry hint")
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ghost", "'ghost'"},
		{"/usr/local/bin/ghost", "'/usr/local/bin/ghost'"},
		{"/path with spaces/ghost", "'/path with spaces/ghost'"},
		{"/path/with$dollar/ghost", "'/path/with$dollar/ghost'"},
		{"/path/with`backtick`/ghost", "'/path/with`backtick`/ghost'"},
		{"/path/it's/ghost", "'/path/it'\\''s/ghost'"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := shellQuote(tt.input)
			if got != tt.want {
				t.Errorf("shellQuote(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGhostPermissions_Complete(t *testing.T) {
	// Verify the canonical list has the expected count.
	if len(ghostPermissions) != 16 {
		t.Errorf("expected 16 ghost permissions, got %d", len(ghostPermissions))
	}

	// All should start with the correct prefix.
	for _, p := range ghostPermissions {
		if !strings.HasPrefix(p, "mcp__ghost__ghost_") {
			t.Errorf("permission %q has unexpected prefix", p)
		}
	}
}
