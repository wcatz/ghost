package reflection

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/repo"
)

const gitContextTimeout = 5 * time.Second

const maxGitCommits = 20

const maxCommitSubjectLen = 300

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

// CollectGitContext returns recent commit subjects and a coarse language hint for dir.
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

	cmd := repo.GitCommand(ctx, "-C", dir, "log",
		"--no-merges", "--pretty=format:%h %s", "-n", strconv.Itoa(maxGitCommits))
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
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

const commitLineBreak = "⏎"

func commitLine(c string) string {
	return quoteData(foldCommitControls(c))
}

func foldCommitControls(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n' {
			b.WriteString(commitLineBreak)
			i += 2
			continue
		}
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case isCommitLineBreak(r):
			b.WriteString(commitLineBreak)
		case unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

func isCommitLineBreak(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1c, 0x1d, 0x1e, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}
