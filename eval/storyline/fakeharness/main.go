// Command fakeharness is the deterministic stand-in for the CLI harnesses a
// storyline run would otherwise spawn (eval/storyline -fake-harness). It never
// contacts a model or a network.
//
// It follows the contract the e2e suite's fake harness follows (e2e/harness_test.go):
// `--version` answers 2.0.15, so the opencode adapter takes its V2 argv; the
// operation is recognised from the request's own contract text ("learned_context"
// for reflect, "closed-by:" for resolve, "NEWER" for supersede); every diagnostic
// goes to stderr; and the opencode answer is delivered as one JSONL text event.
//
// What differs is the supersede verdict. The e2e fake answers one fixed verdict
// per kind; this one answers from an ORACLE — oracle.json beside the binary,
// written by the runner from the storyline's own SupersededBy and Lure
// annotations — so the classifier is right by construction about every pair the
// storyline describes. A failing storyline check is then a failure of Ghost's own
// pipeline (save, candidate generation, edges, resolve, assembly, search) rather
// than a model's judgement. Every other pair is NEITHER, and every resolve item
// is KEEP, which is the conservative answer of both rubrics.
//
// Only the opencode name answers. The storyline runner routes both arc phases
// with --source opencode, so a call that reaches the claude, codex or goose name
// is a routing defect, and it fails loudly rather than answering.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// OraclePair is one pair the fake classifier calls SUPERSEDES: the OLDER note
// carries OlderMark and the NEWER note carries NewerMark. Replaced is the
// retired claim the rubric's replaced: field must name.
type OraclePair struct {
	OlderMark string `json:"older_mark"`
	NewerMark string `json:"newer_mark"`
	Replaced  string `json:"replaced"`
}

func main() {
	os.Exit(run(os.Args, os.Stdin, os.Stdout, os.Stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	name := filepath.Base(args[0])
	dir := ownDir()
	logLine(dir, name+".argv", "ARGV "+strings.Join(args[1:], " "))

	if name != "opencode" {
		_, _ = fmt.Fprintf(stderr, "fake %s: the storyline runner routes every phase to opencode; this call is a routing defect\n", name)
		return 3
	}
	if len(args) > 1 {
		switch args[1] {
		case "--version":
			_, _ = fmt.Fprintln(stdout, "2.0.15")
			return 0
		case "session":
			_, _ = fmt.Fprint(stdout, "[]")
			return 0
		}
	}

	in, err := io.ReadAll(stdin)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fake opencode: read stdin: %v\n", err)
		return 2
	}
	text := string(in)
	kind := classify(text)
	logLine(dir, "opencode.kinds", kind)
	logLine(dir, "opencode.requests", "--- "+kind+" ---\n"+text)

	var answer string
	switch kind {
	case "resolve":
		answer = numbered(text, func(string) string { return "KEEP" })
	case "supersede":
		oracle, err := loadOracle(dir)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "fake opencode: %v\n", err)
			return 4
		}
		answer = numbered(text, func(item string) string { return supersedeVerdict(oracle, item) })
	default:
		_, _ = fmt.Fprintf(stderr, "fake opencode: unrecognised %s request (first 200 bytes: %q)\n", kind, head(text, 200))
		return 5
	}
	logLine(dir, "opencode.answers", "--- "+kind+" ---\n"+answer)

	event := map[string]any{"type": "text", "part": map[string]any{"type": "text", "text": answer}}
	b, err := json.Marshal(event)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fake opencode: encode answer: %v\n", err)
		return 2
	}
	_, _ = fmt.Fprintln(stdout, string(b))
	return 0
}

// classify reads the operation off the request's contract text, in the e2e
// fake's order: the reflect field first, then resolve's verdict field, then the
// supersede pair.
func classify(text string) string {
	switch {
	case strings.Contains(text, "learned_context"):
		return "reflect"
	case strings.Contains(text, "closed-by:"):
		return "resolve"
	case strings.Contains(text, "NEWER"):
		return "supersede"
	}
	return "unknown"
}

var itemNumber = regexp.MustCompile(`(?m)^([0-9]+)\.$`)

// numbered answers every item of a batched request ("1." on a line of its own
// opens an item) as "N: VERDICT", and a single-item request with the bare
// verdict. The item text handed to verdict is everything up to the next number.
func numbered(text string, verdict func(item string) string) string {
	locs := itemNumber.FindAllStringSubmatchIndex(text, -1)
	if len(locs) == 0 {
		return verdict(text)
	}
	var lines []string
	for i, loc := range locs {
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		n := text[loc[2]:loc[3]]
		lines = append(lines, n+": "+verdict(text[loc[1]:end]))
	}
	return strings.Join(lines, "\n")
}

// supersedeVerdict judges one OLDER/NEWER item against the oracle. The pair is
// read from the LAST "OLDER " and "NEWER " lines of the item, because a
// single-item request carries the rubric's own prose before the pair.
func supersedeVerdict(oracle []OraclePair, item string) string {
	older, newer := lastLine(item, "OLDER "), lastLine(item, "NEWER ")
	if older == "" || newer == "" {
		return "NEITHER"
	}
	for _, p := range oracle {
		if strings.Contains(older, p.OlderMark) && strings.Contains(newer, p.NewerMark) {
			return "SUPERSEDES | replaced: " + p.Replaced
		}
	}
	return "NEITHER"
}

func lastLine(text, prefix string) string {
	var found string
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			found = l
		}
	}
	return found
}

func loadOracle(dir string) ([]OraclePair, error) {
	b, err := os.ReadFile(filepath.Join(dir, "oracle.json"))
	if err != nil {
		return nil, fmt.Errorf("read oracle: %w", err)
	}
	var out []OraclePair
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("parse oracle: %w", err)
	}
	return out, nil
}

// ownDir is the directory the binary lives in: the oracle and the call logs sit
// beside it, which is how the runner and the fake share them without an
// environment variable the harness allowlist would drop.
func ownDir() string {
	exe, err := os.Executable()
	if err != nil {
		return "."
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}

func logLine(dir, file, line string) {
	f, err := os.OpenFile(filepath.Join(dir, file), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck
	_, _ = fmt.Fprintln(f, line)
}

func head(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
