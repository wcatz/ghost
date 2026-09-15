package reflection

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitContextTimeout bounds the git invocation so a hung repository or a slow
// filesystem can never stall a reflection run — git context is best-effort
// grounding for the prompt, not a requirement.
const gitContextTimeout = 5 * time.Second

// maxGitCommits caps how many recent commit subjects reach the prompt.
const maxGitCommits = 20

// maxCommitSubjectLen bounds a single commit line, in runes. Very long
// subjects (merge bots, generated messages) would otherwise dominate the
// prompt.
const maxCommitSubjectLen = 300

// languageMarkers maps build/manifest files to a coarse language name. Order
// matters: the first match wins, so put the more specific marker first when a
// repository could carry several.
var languageMarkers = []struct {
	files []string
	name  string
}{
	{[]string{"go.mod"}, "Go"},
	{[]string{"Cargo.toml"}, "Rust"},
	{[]string{"package.json"}, "JavaScript/TypeScript"},
	{[]string{"pyproject.toml", "setup.py", "requirements.txt"}, "Python"},
	{[]string{"Gemfile"}, "Ruby"},
	{[]string{"pom.xml", "build.gradle", "build.gradle.kts"}, "Java/Kotlin"},
	{[]string{"*.csproj", "*.sln"}, "C#"},
	{[]string{"composer.json"}, "PHP"},
	{[]string{"mix.exs"}, "Elixir"},
	{[]string{"CMakeLists.txt"}, "C/C++"},
}

// CollectGitContext returns recent commit subjects ("<short-sha> <subject>")
// and a coarse language hint for dir. It is used to populate
// ReflectionInput.LastCommits and ProjectLanguage.
//
// Every failure path is non-fatal and returns whatever was determined:
// reflection must still run for a directory that is not a git repository, is
// unreadable, or where git is not installed. LastCommits is also the SHA
// whitelist dropFabricatedMemories consults, so returning an empty list keeps
// the guard strict rather than failing the run.
func CollectGitContext(dir string) (commits []string, language string) {
	language = detectLanguage(dir)
	if dir == "" {
		return nil, language
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, language
	}

	ctx, cancel := context.WithTimeout(context.Background(), gitContextTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "-C", dir, "log",
		"--no-merges", "--pretty=format:%h %s", "-n", strconv.Itoa(maxGitCommits))
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		// Not a repository, no commits yet, or git missing — all normal.
		return nil, language
	}

	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if r := []rune(line); len(r) > maxCommitSubjectLen {
			line = string(r[:maxCommitSubjectLen])
		}
		commits = append(commits, line)
	}
	return commits, language
}

// detectLanguage reports a coarse language name from the repository's build or
// manifest files. It is deliberately shallow — a prompt hint, not a classifier
// — and returns "" when nothing recognizable is present.
func detectLanguage(dir string) string {
	if dir == "" {
		return ""
	}
	for _, m := range languageMarkers {
		for _, f := range m.files {
			if strings.ContainsAny(f, "*?[") {
				if hits, _ := filepath.Glob(filepath.Join(dir, f)); len(hits) > 0 {
					return m.name
				}
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
				return m.name
			}
		}
	}
	return ""
}
