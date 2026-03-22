package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	tests := []struct {
		name     string
		file     string
		expected string
	}{
		{"Go project", "go.mod", "Go"},
		{"Rust project", "Cargo.toml", "Rust"},
		{"Node project", "package.json", "JavaScript/TypeScript"},
		{"Python pyproject", "pyproject.toml", "Python"},
		{"Python setup", "setup.py", "Python"},
		{"Python requirements", "requirements.txt", "Python"},
		{"Java Maven", "pom.xml", "Java"},
		{"Java Gradle", "build.gradle", "Java"},
		{"Ruby project", "Gemfile", "Ruby"},
		{"Elixir project", "mix.exs", "Elixir"},
		{"Helm chart", "Chart.yaml", "Helm"},
		{"Helmfile", "helmfile.yaml", "Helmfile"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			testFile := filepath.Join(tmpDir, tt.file)
			if err := os.WriteFile(testFile, []byte("# test"), 0644); err != nil {
				t.Fatalf("write file: %v", err)
			}

			lang := detectLanguage(tmpDir)
			if lang != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, lang)
			}
		})
	}
}

func TestDetectLanguage_Unknown(t *testing.T) {
	tmpDir := t.TempDir()
	// No manifest files
	lang := detectLanguage(tmpDir)
	if lang != "unknown" {
		t.Errorf("expected 'unknown', got %q", lang)
	}
}

func TestDetectTestCommand(t *testing.T) {
	tests := []struct {
		lang        string
		wantCmd     string
		wantHasTest bool
	}{
		{"Go", "go test ./...", true},
		{"Rust", "cargo test", true},
		{"JavaScript/TypeScript", "npm test", true},
		{"Python", "pytest", true},
		{"Java", "mvn test", true},
		{"Ruby", "bundle exec rspec", true},
		{"unknown", "", false},
		{"C++", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			tmpDir := t.TempDir()
			cmd, hasTest := detectTestCommand(tmpDir, tt.lang)
			if cmd != tt.wantCmd {
				t.Errorf("expected cmd %q, got %q", tt.wantCmd, cmd)
			}
			if hasTest != tt.wantHasTest {
				t.Errorf("expected hasTest %v, got %v", tt.wantHasTest, hasTest)
			}
		})
	}
}

func TestDetectLintCommand(t *testing.T) {
	tests := []struct {
		lang string
		want string
	}{
		{"Go", "golangci-lint run"},
		{"Rust", "cargo clippy"},
		{"JavaScript/TypeScript", "npx eslint ."},
		{"Python", "ruff check ."},
		{"unknown", ""},
		{"Ruby", ""},
	}

	for _, tt := range tests {
		t.Run(tt.lang, func(t *testing.T) {
			got := detectLintCommand(tt.lang)
			if got != tt.want {
				t.Errorf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func TestDetect_BasicFields(t *testing.T) {
	tmpDir := t.TempDir()

	// Create a go.mod to detect as Go project
	goMod := filepath.Join(tmpDir, "go.mod")
	if err := os.WriteFile(goMod, []byte("module test\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	if ctx.ID == "" {
		t.Error("expected non-empty ID")
	}
	if len(ctx.ID) != 12 {
		t.Errorf("expected ID length 12, got %d", len(ctx.ID))
	}
	if ctx.Name == "" {
		t.Error("expected non-empty Name")
	}
	if ctx.Path != tmpDir {
		t.Errorf("expected Path %q, got %q", tmpDir, ctx.Path)
	}
	if ctx.Language != "Go" {
		t.Errorf("expected Language 'Go', got %q", ctx.Language)
	}
	if ctx.TestCommand != "go test ./..." {
		t.Errorf("expected test command 'go test ./...', got %q", ctx.TestCommand)
	}
	if !ctx.HasTests {
		t.Error("expected HasTests true for Go project")
	}
	if ctx.LintCommand != "golangci-lint run" {
		t.Errorf("expected lint command 'golangci-lint run', got %q", ctx.LintCommand)
	}
}

func TestDetect_ID_Consistency(t *testing.T) {
	tmpDir := t.TempDir()

	ctx1, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	ctx2, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	if ctx1.ID != ctx2.ID {
		t.Errorf("expected consistent IDs, got %q and %q", ctx1.ID, ctx2.ID)
	}
}

func TestDetect_ClaudeMD(t *testing.T) {
	tmpDir := t.TempDir()
	claudeMD := filepath.Join(tmpDir, "CLAUDE.md")
	content := "# Test instructions\nTest content"

	if err := os.WriteFile(claudeMD, []byte(content), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	if ctx.ClaudeMD != content {
		t.Errorf("expected CLAUDE.md content %q, got %q", content, ctx.ClaudeMD)
	}
}

func TestDetect_GhostMD(t *testing.T) {
	tmpDir := t.TempDir()
	ghostMD := filepath.Join(tmpDir, ".ghost.md")
	content := "# Ghost config\nAlternate config"

	if err := os.WriteFile(ghostMD, []byte(content), 0644); err != nil {
		t.Fatalf("write .ghost.md: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	if ctx.ClaudeMD != content {
		t.Errorf("expected .ghost.md content %q, got %q", content, ctx.ClaudeMD)
	}
}

func TestDetect_ClaudeMD_Precedence(t *testing.T) {
	tmpDir := t.TempDir()

	// Create both files
	claudeMD := filepath.Join(tmpDir, "CLAUDE.md")
	ghostMD := filepath.Join(tmpDir, ".ghost.md")

	claudeContent := "# CLAUDE instructions"
	ghostContent := "# Ghost instructions"

	if err := os.WriteFile(claudeMD, []byte(claudeContent), 0644); err != nil {
		t.Fatalf("write CLAUDE.md: %v", err)
	}
	if err := os.WriteFile(ghostMD, []byte(ghostContent), 0644); err != nil {
		t.Fatalf("write .ghost.md: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	// CLAUDE.md should take precedence
	if ctx.ClaudeMD != claudeContent {
		t.Errorf("expected CLAUDE.md content, got %q", ctx.ClaudeMD)
	}
}

func TestDetect_ReadmeSummary(t *testing.T) {
	tmpDir := t.TempDir()
	readme := filepath.Join(tmpDir, "README.md")

	content := "# Test Project\nThis is a test project with some content."
	if err := os.WriteFile(readme, []byte(content), 0644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	if ctx.ReadmeSummary != content {
		t.Errorf("expected summary %q, got %q", content, ctx.ReadmeSummary)
	}
}

func TestDetect_ReadmeSummary_Truncation(t *testing.T) {
	tmpDir := t.TempDir()
	readme := filepath.Join(tmpDir, "README.md")

	// Create content longer than 500 chars
	content := strings.Repeat("a", 600)
	if err := os.WriteFile(readme, []byte(content), 0644); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	expected := content[:500] + "..."
	if ctx.ReadmeSummary != expected {
		t.Errorf("expected truncated summary (len=%d), got len=%d", len(expected), len(ctx.ReadmeSummary))
	}
	if !strings.HasSuffix(ctx.ReadmeSummary, "...") {
		t.Error("expected summary to end with '...'")
	}
}

func TestDetect_InvalidPath(t *testing.T) {
	// Test with a path that doesn't exist
	ctx, err := Detect("/nonexistent/path/that/does/not/exist")
	if err != nil {
		// It's okay if this errors, but let's check the behavior
		t.Logf("Got expected error for nonexistent path: %v", err)
		return
	}

	// If no error, context should still be created
	if ctx == nil {
		t.Error("expected non-nil context even for nonexistent path")
	}
}

func TestGitOutput_NotGitRepo(t *testing.T) {
	tmpDir := t.TempDir()
	// Not a git repo
	output := gitOutput(tmpDir, "status")
	if output != "" {
		t.Errorf("expected empty output for non-git repo, got %q", output)
	}
}

func TestDetect_Name(t *testing.T) {
	tmpDir := t.TempDir()

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	expectedName := filepath.Base(tmpDir)
	if ctx.Name != expectedName {
		t.Errorf("expected name %q, got %q", expectedName, ctx.Name)
	}
}

func TestDetect_MultipleLanguageFiles(t *testing.T) {
	tmpDir := t.TempDir()

	// Create multiple language files - first match wins
	goMod := filepath.Join(tmpDir, "go.mod")
	cargo := filepath.Join(tmpDir, "Cargo.toml")

	if err := os.WriteFile(goMod, []byte("module test\n"), 0644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(cargo, []byte("[package]\n"), 0644); err != nil {
		t.Fatalf("write Cargo.toml: %v", err)
	}

	ctx, err := Detect(tmpDir)
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}

	// Go should be detected first (due to order in checks)
	if ctx.Language != "Go" {
		t.Errorf("expected language 'Go', got %q", ctx.Language)
	}
}

func TestContext_AllFieldsAccessible(t *testing.T) {
	// Test that all Context fields are accessible and can be set
	ctx := &Context{
		ID:            "test123",
		Name:          "testproject",
		Path:          "/test/path",
		Language:      "Go",
		GitBranch:     "main",
		GitStatus:     "clean",
		LastCommits:   []string{"abc123 commit 1"},
		HasTests:      true,
		TestCommand:   "go test",
		LintCommand:   "golangci-lint",
		FileTree:      "file1.go\nfile2.go",
		ClaudeMD:      "# Instructions",
		ReadmeSummary: "# README",
	}

	if ctx.ID != "test123" {
		t.Error("ID field not accessible")
	}
	if ctx.Name != "testproject" {
		t.Error("Name field not accessible")
	}
	if len(ctx.LastCommits) != 1 {
		t.Error("LastCommits field not accessible")
	}
}

func TestHashID(t *testing.T) {
	id := HashID("/home/wayne/git/ghost")
	if len(id) != 12 {
		t.Errorf("expected 12-char ID, got len=%d: %q", len(id), id)
	}
	// Deterministic.
	if id2 := HashID("/home/wayne/git/ghost"); id != id2 {
		t.Errorf("not deterministic: %q vs %q", id, id2)
	}
	// Different inputs produce different IDs.
	if id3 := HashID("/home/wayne/git/other"); id == id3 {
		t.Error("different inputs produced same ID")
	}
}
