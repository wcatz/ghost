package project

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Context holds detected metadata about a project.
type Context struct {
	ID            string // sha256(abs_path)[:12]
	Name          string // basename or config override
	Path          string // absolute path
	Language      string // detected from manifest files
	GitBranch     string
	GitStatus     string // "clean", "3 files modified", etc.
	LastCommits   []string
	HasTests      bool
	TestCommand   string
	LintCommand   string
	FileTree      string // truncated ls-tree
	ClaudeMD      string // contents of CLAUDE.md if present
	ReadmeSummary string // first 500 chars of README.md
}

// Detect scans a project directory and builds context.
func Detect(path string) (*Context, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve path: %w", err)
	}
	// Resolve symlinks to get a canonical path (mitigates path traversal).
	absPath, err = filepath.EvalSymlinks(absPath)
	if err != nil {
		return nil, fmt.Errorf("resolve symlinks: %w", err)
	}

	h := sha256.Sum256([]byte(absPath))
	ctx := &Context{
		ID:   fmt.Sprintf("%x", h[:6]),
		Name: filepath.Base(absPath),
		Path: absPath,
	}

	ctx.Language = detectLanguage(absPath)
	ctx.TestCommand, ctx.HasTests = detectTestCommand(absPath, ctx.Language)
	ctx.LintCommand = detectLintCommand(ctx.Language)

	// Git info.
	ctx.GitBranch = gitOutput(absPath, "branch", "--show-current")
	status := gitOutput(absPath, "status", "--porcelain")
	if status == "" {
		ctx.GitStatus = "clean"
	} else {
		lines := strings.Split(strings.TrimSpace(status), "\n")
		ctx.GitStatus = fmt.Sprintf("%d files modified", len(lines))
	}

	// Recent commits.
	logOutput := gitOutput(absPath, "log", "--oneline", "-5")
	if logOutput != "" {
		ctx.LastCommits = strings.Split(strings.TrimSpace(logOutput), "\n")
	}

	// File tree.
	tree := gitOutput(absPath, "ls-tree", "-r", "--name-only", "HEAD")
	if tree != "" {
		lines := strings.Split(tree, "\n")
		if len(lines) > 100 {
			lines = append(lines[:100], fmt.Sprintf("... and %d more files", len(lines)-100))
		}
		ctx.FileTree = strings.Join(lines, "\n")
	}

	// CLAUDE.md or .ghost.md.
	for _, name := range []string{"CLAUDE.md", ".ghost.md"} {
		if p := safeJoin(absPath, name); p != "" {
			content, err := os.ReadFile(p)
			if err == nil {
				ctx.ClaudeMD = string(content)
				break
			}
		}
	}

	// README summary.
	readme, err := os.ReadFile(safeJoin(absPath, "README.md"))
	if err == nil {
		s := string(readme)
		if len(s) > 500 {
			s = s[:500] + "..."
		}
		ctx.ReadmeSummary = s
	}

	return ctx, nil
}

// safeJoin joins base and name, ensuring the result stays within base.
func safeJoin(base, name string) string {
	joined := filepath.Join(base, filepath.Clean(name))
	if !strings.HasPrefix(joined, base+string(filepath.Separator)) && joined != base {
		return ""
	}
	return joined
}

func detectLanguage(path string) string {
	checks := []struct {
		file string
		lang string
	}{
		{"go.mod", "Go"},
		{"Cargo.toml", "Rust"},
		{"package.json", "JavaScript/TypeScript"},
		{"pyproject.toml", "Python"},
		{"setup.py", "Python"},
		{"requirements.txt", "Python"},
		{"pom.xml", "Java"},
		{"build.gradle", "Java"},
		{"Gemfile", "Ruby"},
		{"mix.exs", "Elixir"},
		{"helmfile.yaml", "Helmfile"},
		{"Chart.yaml", "Helm"},
	}
	for _, c := range checks {
		if p := safeJoin(path, c.file); p != "" {
			if _, err := os.Stat(p); err == nil {
				return c.lang
			}
		}
	}
	return "unknown"
}

func detectTestCommand(path, lang string) (string, bool) {
	switch lang {
	case "Go":
		return "go test ./...", true
	case "Rust":
		return "cargo test", true
	case "JavaScript/TypeScript":
		return "npm test", true
	case "Python":
		return "pytest", true
	case "Java":
		return "mvn test", true
	case "Ruby":
		return "bundle exec rspec", true
	default:
		return "", false
	}
}

func detectLintCommand(lang string) string {
	switch lang {
	case "Go":
		return "golangci-lint run"
	case "Rust":
		return "cargo clippy"
	case "JavaScript/TypeScript":
		return "npx eslint ."
	case "Python":
		return "ruff check ."
	default:
		return ""
	}
}

func gitOutput(dir string, args ...string) string {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
