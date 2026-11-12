package mcpinit

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wcatz/ghost/internal/hostevent"
	"github.com/wcatz/ghost/internal/memory"
)

// A memory is covered by an instruction block when the tokens they share are at
// least instructionCoverSmaller of the smaller side AND at least
// instructionCoverMemory of the memory's own tokens. The second bound is what
// stops a short generic line ("Go 1.24.0+") from covering a long, specific
// memory that happens to contain its few words.
const (
	instructionCoverSmaller   = 0.6
	instructionCoverMemory    = 0.3
	instructionBlockMinTokens = 4
	instructionFileMaxBytes   = 256 << 10
)

// hostInstructionFiles lists the instruction files the given host loads on its
// own for a session in cwd, and only those that exist. A host Ghost has no list
// for gets none, so nothing is withheld for it: withholding a row over a file
// the host never read would hide that row's only copy.
func hostInstructionFiles(src hostevent.Source, cwd, home string) []string {
	var candidates []string
	switch src {
	case hostevent.SourceClaudeCode:
		// Claude Code: the user file, then CLAUDE.md, .claude/CLAUDE.md and
		// CLAUDE.local.md in the working directory and every parent below /.
		if envTrue("CLAUDE_CODE_DISABLE_CLAUDE_MDS") {
			return nil
		}
		userDir := os.Getenv("CLAUDE_CONFIG_DIR")
		if userDir == "" {
			userDir = filepath.Join(home, ".claude")
		}
		candidates = append(candidates, filepath.Join(userDir, "CLAUDE.md"))
		for dir := cwd; dir != ""; {
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			candidates = append(candidates,
				filepath.Join(dir, "CLAUDE.md"),
				filepath.Join(dir, ".claude", "CLAUDE.md"),
				filepath.Join(dir, "CLAUDE.local.md"))
			dir = parent
		}
	case hostevent.SourceOpencode:
		// opencode: AGENTS.md in its config directory, then AGENTS.md from the
		// working directory up to the home directory (or, outside home, up to the
		// repository root). It reads no CLAUDE.md.
		cfgDir := os.Getenv("OPENCODE_CONFIG_DIR")
		if cfgDir == "" {
			if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
				cfgDir = filepath.Join(xdg, "opencode")
			} else {
				cfgDir = filepath.Join(home, ".config", "opencode")
			}
		}
		candidates = append(candidates, filepath.Join(cfgDir, "AGENTS.md"))
		if cwd != "" && !envTrue("OPENCODE_DISABLE_PROJECT_CONFIG") && !envTrue("OPENCODE_CONFIG_PROJECT_DISABLE") {
			stop := opencodeWalkStop(cwd, home)
			for dir := cwd; ; {
				candidates = append(candidates, filepath.Join(dir, "AGENTS.md"))
				parent := filepath.Dir(dir)
				if dir == stop || parent == dir {
					break
				}
				dir = parent
			}
		}
	default:
		return nil
	}
	var files []string
	seen := make(map[string]bool, len(candidates))
	for _, p := range candidates {
		if seen[p] {
			continue
		}
		seen[p] = true
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			files = append(files, p)
		}
	}
	return files
}

// opencodeWalkStop is where opencode stops looking for AGENTS.md above cwd: the
// home directory when cwd is inside it, else the repository root, else cwd.
func opencodeWalkStop(cwd, home string) string {
	if home != "" {
		if rel, err := filepath.Rel(home, cwd); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return home
		}
	}
	for dir := cwd; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return cwd
		}
		dir = parent
	}
}

func envTrue(name string) bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return v != "" && v != "0" && v != "false"
}

var listItem = regexp.MustCompile(`^([-*+]|\d+[.)])\s`)

// instructionBlocks splits an instruction file into its rules: one block per
// list item or paragraph (with its continuation lines), one per table row.
// Headings and rules separate blocks and are not blocks themselves. A block too
// short to say anything is left out.
func instructionBlocks(text string) []map[string]bool {
	var blocks []map[string]bool
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			if tok := memory.DedupTokenize(strings.Join(cur, " ")); len(tok) >= instructionBlockMinTokens {
				blocks = append(blocks, tok)
			}
			cur = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		l := strings.TrimSpace(line)
		switch {
		case l == "" || strings.HasPrefix(l, "#") || strings.HasPrefix(l, "---"):
			flush()
			continue
		case strings.HasPrefix(l, "|"):
			flush()
			cur = []string{l}
			flush()
			continue
		case listItem.MatchString(l):
			flush()
		}
		cur = append(cur, l)
	}
	flush()
	return blocks
}

// coveredByInstructions reports whether one block covers the memory content.
func coveredByInstructions(content string, blocks []map[string]bool) bool {
	mem := memory.DedupTokenize(content)
	if len(mem) == 0 {
		return false
	}
	for _, b := range blocks {
		shared := 0
		for t := range mem {
			if b[t] {
				shared++
			}
		}
		smaller := min(len(mem), len(b))
		if float64(shared) >= instructionCoverSmaller*float64(smaller) &&
			float64(shared) >= instructionCoverMemory*float64(len(mem)) {
			return true
		}
	}
	return false
}

// hostInstructionCover returns the session-start Covered predicate for a host
// in cwd, or nil when the host loads no instruction file Ghost can read.
func hostInstructionCover(src hostevent.Source, cwd string) func(memory.Candidate) bool {
	cover := InstructionCover(src, cwd)
	if cover == nil {
		return nil
	}
	return func(c memory.Candidate) bool { return cover(c.Content) }
}

// InstructionCover reports whether a memory's content restates a rule from the
// instruction files the host loads for a session in dir. An empty dir reads
// only the host's user-level file, which is all a cross-project bucket can rely
// on. It returns nil when the host loads no file Ghost can read; an unreadable
// file is skipped, so it withholds nothing.
func InstructionCover(src hostevent.Source, dir string) func(content string) bool {
	home, _ := os.UserHomeDir()
	var blocks []map[string]bool
	for _, p := range hostInstructionFiles(src, dir, home) {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, instructionFileMaxBytes))
		_ = f.Close()
		if err != nil {
			continue
		}
		blocks = append(blocks, instructionBlocks(string(data))...)
	}
	if len(blocks) == 0 {
		return nil
	}
	return func(content string) bool { return coveredByInstructions(content, blocks) }
}
