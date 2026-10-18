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

	cmd := repo.GitCommand(ctx, "-C", dir, "log",
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

// commitLineBreak is what a line break inside a commit line is rendered as. It
// is the glyph assemble.LineBreakEscape uses for the same fold, copied rather
// than imported because assemble imports memory and this package cannot reach
// it without a cycle; one spelling of a folded break across Ghost's surfaces is
// what lets a reader recognise it.
const commitLineBreak = "⏎"

// commitLine renders one LastCommits entry for the reflection input as data.
//
// A commit subject is text from outside the store: anyone who can push to a
// branch the user pulls writes it. So it is framed the way a memory's content
// is, through quoteData (« » delimited, any literal « » inside rewritten), and
// additionally folded to ONE physical line, which quoteData deliberately does
// not do for memories. The fold is safe here and not there because the model is
// never asked to rewrite a commit subject, so a visible escape cannot be copied
// back into stored text as a rewrite of it.
//
// Folded: every rune a reader ends a line at (LF, VT, FF, CR, FS, GS, RS, NEL,
// LS, PS — the set assemble's fold uses, with a CRLF pair as one break) becomes
// commitLineBreak, and every other C0 or C1 control becomes a space. With the
// line opened by the builder's own "- «", no subject can start a line with a
// heading or a memory-shaped "- id:".
//
// It is applied at render time rather than in CollectGitContext because
// LastCommits is also the SHA source for the fabrication and contamination
// guards and is set directly by callers that never run git; the builder is the
// one place every entry passes through.
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
