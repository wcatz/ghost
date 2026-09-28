package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/scratch"
	"github.com/wcatz/ghost/internal/supersede"
)

// The second half of a supersede repair, stated by the first half.
//
// Withdrawing a 'supersedes' edge is not the whole repair. `ghost resolve`'s
// piggyback stamped resolved_at on the older endpoint for free, and resolve's
// own repair pass deliberately HONOURS a live edge as a floor — so the memory
// the withdrawal just orphaned stays out of ranked injection until a second
// pass clears it, and until then the operator has no way to know that. The
// corpus reads as though the withdrawal did nothing, which is the invisibility
// #686 was opened to remove.
//
// So an --apply run that withdrew edges says which memories are now repairable
// and prints the command that repairs exactly them. Two forms, because the two
// populations are different sizes: the command, for the handful of targets a
// single wrong edge left behind, and a file under the data dir's scratch, for
// the run that withdrew forty. The file is a --only-file input, so an operator
// never has to retype a list the tool already computed, and the two lists cannot
// drift because one is rendered from the other's argument.
//
// Only an --apply run prints any of this. A dry run withdrew nothing, so naming
// its targets would be a follow-up command for a repair that has not happened
// yet — and resolve would refuse it anyway, because the edges are still live
// and that is exactly what its floor checks.

// withdrawnTargets returns the ids of the memories whose 'supersedes' edges the
// pass withdrew, deduplicated, in the order the edges were reported.
//
// Every row counts, including one this run did not itself invalidate: a
// concurrent pass that took an edge first left the same state behind, which is
// to say no live edge and a resolved_at nothing defends any more. The
// alternative — reporting only the edges this process wrote — would drop a
// memory that is just as repairable from a list the operator is about to run.
func withdrawnTargets(withdrawn []supersede.WithdrawnEdge) []string {
	var out []string
	seen := make(map[string]bool, len(withdrawn))
	for _, w := range withdrawn {
		if w.OlderID == "" || seen[w.OlderID] {
			continue
		}
		seen[w.OlderID] = true
		out = append(out, w.OlderID)
	}
	return out
}

// resolveFollowupCommand renders the follow-up as a command line the operator
// can paste. Full ids, not the eight-character abbreviations the reports use: a
// prefix is unambiguous now and may not be after the operator's next save, and
// this is a command about to be run rather than a line to read.
//
// The project name is a shell argument, and a project name is free text — the
// reason sanitizeFileNamePart exists is that a name can hold a separator or a
// space. So a name that needs no quoting is rendered as the positional the docs
// use everywhere, and anything else goes through --project in single quotes,
// which parseResolveArgs takes verbatim ("a dash-leading name is a name, not a
// flag"). Rendering a bare `ghost resolve my proj …` would be two positionals
// and a failed repair; rendering a name holding a shell metacharacter bare would
// execute it.
func resolveFollowupCommand(projectName string, ids []string) string {
	project := projectName
	if !bareShellWord.MatchString(projectName) {
		project = "--project " + shellQuote(projectName)
	}
	return fmt.Sprintf("ghost resolve %s --reassess --only %s --apply", project, strings.Join(ids, ","))
}

// bareShellWord matches a token that can be pasted into a shell unquoted and
// still be one argument: it starts with a word character, so it cannot be read
// as a flag, and carries nothing a shell would interpret. Everything else is
// quoted.
var bareShellWord = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]*$`)

// shellQuote renders one POSIX shell argument, single-quoted. An embedded single
// quote is closed, backslash-escaped and reopened, which is the only spelling a
// POSIX shell reads back as one literal quote — and a project name is operator
// text, so it is not guaranteed to hold none.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// supersedeReassessFollowup renders the block an --apply run prints below the
// per-edge list. path is the file the ids were written to, and is empty when the
// write failed — the command is the primary form and never depends on it.
//
// The wording says "was held by", not "can now be cleared", because a note two
// newer notes both supersede is still held after one of the two edges is
// withdrawn (#697): the floor is counted per edge, so that repair reports the
// row as still asserted and clears nothing. Promising a clear the pass will
// refuse would be the one kind of lie this block must not tell.
func supersedeReassessFollowup(projectName string, ids []string, path string) string {
	if len(ids) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nFollow-up: these are the targets of the edges this run withdrew, and the\n" +
		"resolutions those edges justified. One that no live 'supersedes' edge still\n" +
		"holds can now be cleared; one another edge still holds is reported as still\n" +
		"asserted rather than cleared. Nothing outside this list is judged:\n")
	fmt.Fprintf(&b, "  %s\n", resolveFollowupCommand(projectName, ids))
	if path != "" {
		// Quoted for the same reason as the project name, and because the path
		// is not Ghost's to control: $GHOST_SCRATCH_DIR and a data directory
		// under a spaced path both reach it.
		fmt.Fprintf(&b, "  (the same ids are in %s, for `ghost resolve --project %s --reassess --only-file %s --apply`)\n",
			path, shellQuote(projectName), shellQuote(path))
	}
	return b.String()
}

// writeReassessTargets writes the follow-up's id list under the data dir's
// scratch root and returns its path, so the printed command and the file can
// never name different lists.
//
// The root is Ghost's own scratch, not the system temp: it is created 0700
// beneath the data dir, it is already exempt from the per-invocation reaping
// (which only collects directories in Open's <pid>-<hex> shape), and an operator
// who wants to read the list days later still can. No id file is removed by a
// later pass, which is deliberate — a repair an operator meant to run and did
// not should still be there to run.
//
// Nothing is ever written OVER. The root can be a directory someone else shares
// ($GHOST_SCRATCH_DIR, or a root an operator pointed at), the name is derived
// from a project name and a clock, and a name derived from a clock is not
// ownership: two runs of the same project in the same second agree on it, and
// whoever else can write to the root can plant a symlink at it. So the name
// carries a per-run token (scratch.UniqueSuffix), the file is opened O_EXCL — a
// name that is already taken fails instead of being followed or truncated — and
// a collision is retried under a fresh name. The two failure modes this buys
// are a symlink not followed and another run's list not replaced, and the
// operator's command can only ever point at the list this run wrote.
//
// writtenBy names the command that produced the list, and it is a parameter
// because two commands write this file: `ghost supersede --reassess --apply` and
// `ghost supersede --withdraw … --apply` orphan the same kind of resolution. A
// file claiming the other command wrote it would be read days later by whoever
// runs the repair, and that header is the only line in it saying where the ids
// came from.
func writeReassessTargets(projectName string, ids []string, writtenBy string) (string, error) {
	if len(ids) == 0 {
		return "", fmt.Errorf("no withdrawn targets to write")
	}
	root, err := scratch.Root()
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n", resolveFollowupCommand(projectName, ids))
	fmt.Fprintf(&b, "# Written by `%s`: the targets of the edges it\n", writtenBy)
	b.WriteString("# withdrew, one id per line. Use with `ghost resolve <project> --reassess --only-file`.\n")
	for _, id := range ids {
		b.WriteString(id + "\n")
	}
	body := b.String()

	// Three attempts, because a collision now means something else wrote between
	// the name and the open — a second repair, or a name we guessed wrong. It
	// does not mean the path is unusable, and giving up on the first would make
	// the follow-up's file a coin flip.
	for attempt := 0; attempt < reassessTargetsAttempts; attempt++ {
		name, err := reassessTargetsName(projectName)
		if err != nil {
			return "", err
		}
		path := filepath.Join(root, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, fs.ErrExist) {
			// Not ours. Ask for another name rather than reporting a failure the
			// operator can do nothing about, and without touching what is there.
			continue
		}
		if err != nil {
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if _, err := f.WriteString(body); err != nil {
			_ = f.Close()
			_ = os.Remove(path)
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			_ = os.Remove(path)
			return "", fmt.Errorf("write %s: %w", path, err)
		}
		return path, nil
	}
	return "", fmt.Errorf("no free id file name under %s after %d attempts", root, reassessTargetsAttempts)
}

// reassessTargetsAttempts bounds the name search in writeReassessTargets.
const reassessTargetsAttempts = 3

// defaultReassessTargetsName builds the id file's name: the repair, the project,
// the UTC second for a human reading a directory listing, and a per-run token so
// no two runs — or anything else in the root — can hold the same name.
//
// It is a variable so a test can pin the name and ask what the writer does when
// that name is already taken, which is the only way to observe the refusal
// without racing a clock.
var defaultReassessTargetsName = func(projectName string) (string, error) {
	suffix, err := scratch.UniqueSuffix()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("supersede-reassess-%s-%s-%s.ids",
		sanitizeFileNamePart(projectName), time.Now().UTC().Format("20060102T150405Z"), suffix), nil
}

var reassessTargetsName = defaultReassessTargetsName

// sanitizeFileNamePart renders a project name safe to embed in a file name. The
// name is free text — `ghost resolve --project` takes the next argument verbatim
// so a dash-leading project name works — and a name holding a separator would
// otherwise write the file outside the scratch root, or not at all.
func sanitizeFileNamePart(s string) string {
	const maxLen = 40
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= maxLen {
			break
		}
	}
	if b.Len() == 0 {
		return "project"
	}
	return b.String()
}
