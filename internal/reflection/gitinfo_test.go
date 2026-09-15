package reflection

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectLanguage(t *testing.T) {
	cases := []struct {
		file string
		want string
	}{
		{"go.mod", "Go"},
		{"Cargo.toml", "Rust"},
		{"package.json", "JavaScript/TypeScript"},
		{"pyproject.toml", "Python"},
		{"Gemfile", "Ruby"},
		{"composer.json", "PHP"},
	}
	for _, tc := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, tc.file), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := detectLanguage(dir); got != tc.want {
			t.Errorf("detectLanguage with %s = %q, want %q", tc.file, got, tc.want)
		}
	}
	if got := detectLanguage(t.TempDir()); got != "" {
		t.Errorf("detectLanguage(empty dir) = %q, want empty", got)
	}
	if got := detectLanguage(""); got != "" {
		t.Errorf(`detectLanguage("") = %q, want empty`, got)
	}
}

func TestCollectGitContextNonRepo(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	commits, language := CollectGitContext(dir)
	if language != "Go" {
		t.Errorf("language = %q, want Go", language)
	}
	if len(commits) != 0 {
		t.Errorf("commits = %v, want none for a non-repo directory", commits)
	}
	if commits, _ := CollectGitContext(""); commits != nil {
		t.Errorf("empty dir should yield nil commits, got %v", commits)
	}
}

// TestCollectGitContextCommits pins the producer that was missing entirely:
// LastCommits and ProjectLanguage used to be dead fields, so the prompt's
// Recent Git Activity section never rendered and the fabrication guard had no
// legitimate SHA source beyond ExistingMemories.
func TestCollectGitContextCommits(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	runGit(t, dir, "init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", ".")
	runGit(t, dir, "-c", "user.email=test@example.com", "-c", "user.name=Test",
		"commit", "-q", "-m", "add module file")

	commits, language := CollectGitContext(dir)
	if language != "Go" {
		t.Errorf("language = %q, want Go", language)
	}
	if len(commits) != 1 {
		t.Fatalf("commits = %v, want exactly 1 entry", commits)
	}
	if !strings.Contains(commits[0], "add module file") {
		t.Errorf("commit line %q is missing the subject", commits[0])
	}
	sha := strings.Fields(commits[0])[0]
	if !shaLikeToken(sha) {
		t.Errorf("commit SHA %q is not recognized as a SHA token, so it would not whitelist anything", sha)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
