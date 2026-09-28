package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/followup"
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
// The renderer is internal/followup's, because the MCP tool answers the same
// question and the quoting here is what decides whether the command RUNS: one
// implementation, two surfaces, no way for them to drift on the part that matters.
//
// The two returns after the command are the ids it could not carry, split by which
// surface can still reach them: those holding a comma, and those holding a newline
// that no surface can. supersedeReassessFollowup turns them into lines, because a
// command that silently named fewer memories than the block above lists would be the
// one lie this block must not tell — and an EMPTY command, which is what every id
// holding a comma produces, must never be filled in with the unscoped form.
func resolveFollowupCommand(projectName string, ids []string) (string, []string, []string) {
	return followup.ResolveCommand(projectName, ids)
}

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
	cmd, viaFileOnly, unnameable := resolveFollowupCommand(projectName, ids)
	// Never the unscoped command. When no id is carriable by --only the block says
	// so and points at the file, because `ghost resolve <project> --reassess
	// --apply` — the form with no --only — is the project-wide re-judge #698
	// measured, and printing it here under a heading that promises nothing outside
	// this list is judged would be the worst line in the report.
	if cmd != "" {
		fmt.Fprintf(&b, "  %s\n", cmd)
	}
	// The file line is printed only when the file can name something, which is
	// `len(ids) > len(unnameable)`: writeReassessTargets omits the newline-bearing
	// ids rather than writing two half-ids, so a file holding only those names no
	// selector at all and readOnlySelectors refuses it — pointing the operator at
	// it would be pointing at a command that cannot run.
	fileHoldsSomething := path != "" && len(unnameable) < len(ids)
	if fileHoldsSomething {
		// Quoted for the same reason as the project name, and because the path
		// is not Ghost's to control: $GHOST_SCRATCH_DIR and a data directory
		// under a spaced path both reach it.
		fmt.Fprintf(&b, "  (the same ids are in %s, for `ghost resolve --project %s --reassess --only-file %s --apply`)\n",
			path, shellQuote(projectName), shellQuote(path))
	}
	// Every branch is driven by the buckets, not by `cmd == ""`, which has two
	// causes: ids holding a comma (the file reaches them) and ids holding a
	// newline (nothing does). Keying the wording off the empty command made a
	// newline-only report claim the file was the only way and, three lines later,
	// that no surface could name the id.
	switch {
	case len(viaFileOnly) == 0 && len(unnameable) == 0:
		// Nothing to explain: every id is in the command above.
	case len(viaFileOnly) == len(ids):
		// Every id needs the file. The unscoped repair is DESCRIBED and not written
		// out: rendering it here, even inside a warning, puts a copy-pasteable line
		// that re-judges every resolved memory in the project directly under the
		// block promising that nothing outside this list is judged.
		if fileHoldsSomething {
			fmt.Fprintf(&b, "  (no --only command can name them — every id holds a comma, which --only splits on —\n"+
				"   so the file above is the only way to run this repair. Do NOT fall back on the same command\n"+
				"   without --only: that re-judges every resolved memory in the project)\n")
		} else {
			fmt.Fprintf(&b, "  (no --only command can name them and the id file could not be written, so they are\n"+
				"   named here: put each on its own line in a file and use --only-file. Do NOT fall back on the\n"+
				"   same command without --only: that re-judges every resolved memory in the project)\n")
			for _, id := range viaFileOnly {
				fmt.Fprintf(&b, "    %s\n", id)
			}
		}
	case len(viaFileOnly) > 0 && fileHoldsSomething:
		// `--only` splits on commas, so an id holding one is not nameable by that
		// flag however it is quoted, and the command above leaves it out rather
		// than splitting it into selectors that name nothing. The file is the only
		// surface that can carry it, so this is a warning about which line to run.
		fmt.Fprintf(&b, "  (%d id(s) hold a comma, which --only cannot carry, so the command above leaves them out;\n"+
			"   they are in the file, and the file is the only way to name them)\n", len(viaFileOnly))
	case len(viaFileOnly) > 0:
		// No file: the file write failed, so pointing at it would point at nothing
		// and the ids would be listed nowhere — the invisibility this whole block
		// exists to remove. They are named here instead, which is what the MCP
		// surface does for the same reason.
		fmt.Fprintf(&b, "  (%d id(s) hold a comma and the id file could not be written, so they are named here;\n"+
			"   no --only command can carry them — put each on its own line in a file and use --only-file)\n", len(viaFileOnly))
		for _, id := range viaFileOnly {
			fmt.Fprintf(&b, "    %s\n", id)
		}
	}
	if len(unnameable) > 0 {
		// No surface can name these: the file is one id per line, so a newline in
		// an id becomes two selectors. Saying so is the whole answer, because
		// pretending otherwise leaves a memory stamped resolved with nothing able
		// to clear it.
		fmt.Fprintf(&b, "  (%d id(s) hold a newline, which no --only or --only-file form can carry. No surface can\n"+
			"   name them, so these stay resolved until the row is rewritten — delete and re-save the memory,\n"+
			"   or re-import it under an id without a newline)\n", len(unnameable))
		for _, id := range unnameable {
			fmt.Fprintf(&b, "    %q\n", id)
		}
	}
	return b.String()
}

// containsLineBreak reports whether an id cannot be written as one line of an
// --only-file. It is the one character class no surface can carry: --only splits
// on commas, the file splits on newlines, and an id holding one is nameable by
// neither.
func containsLineBreak(id string) bool { return strings.ContainsAny(id, "\n\r") }

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
	head, viaFileOnly, unnameable := resolveFollowupCommand(projectName, ids)
	if len(unnameable) == len(ids) {
		// Nothing this file could name. Writing it anyway produces a list
		// readOnlySelectors refuses with "names no memory ids or prefixes", and the
		// report would point the operator at a command that cannot run. The ids are
		// named in the report instead, which is the only place they can be.
		return "", fmt.Errorf("no id in this set is nameable through --only-file: all %d hold a newline", len(ids))
	}
	var b strings.Builder
	// The header carries the command, and an id the command cannot carry makes it
	// name fewer ids than the list below — so the header says so rather than
	// letting the operator compare the two and wonder which was dropped. Which
	// claim is made is driven by the buckets and not by `head == ""`, because an
	// empty command has two causes and only one of them is a comma: an id holding
	// a newline is one no surface can name, and calling that a comma says the
	// file is the answer when the file is not carrying it.
	// The format is unchanged otherwise, because this file is a --only-file input
	// and readOnlySelectors reads one id per line whatever the comment says.
	switch {
	case head != "":
		fmt.Fprintf(&b, "# %s\n", head)
	case len(viaFileOnly) > 0:
		fmt.Fprintf(&b, "# No --only command can name these ids: every one of them holds a comma,\n"+
			"# which --only splits on. This file is the only surface that can, so run\n"+
			"# `ghost resolve <project> --reassess --only-file <this file> --apply`.\n")
	default:
		fmt.Fprintf(&b, "# No --only command can name these ids (some hold a comma, which --only splits on, and\n"+
			"# none of those are nameable any other way). The ids this file CAN name are below; run\n"+
			"# `ghost resolve <project> --reassess --only-file <this file> --apply`.\n")
	}
	if len(viaFileOnly) > 0 {
		fmt.Fprintf(&b, "# (%d id(s) below hold a comma and are not in that command; --only cannot carry them,\n"+
			"#  which is what this file is for)\n", len(viaFileOnly))
	}
	fmt.Fprintf(&b, "# Written by `%s`: the targets of the edges it\n", writtenBy)
	b.WriteString("# withdrew, one id per line. Use with `ghost resolve <project> --reassess --only-file`.\n")
	// A newline in an id is the one character the file cannot carry either: this
	// format is one id per line, so writing it verbatim produces two selectors,
	// both naming nothing. The id is left out and named in the report instead,
	// because a corrupt file that looks right is worse than a listed id with an
	// honest note that no surface can reach it.
	for _, id := range ids {
		if containsLineBreak(id) {
			continue
		}
		b.WriteString(id + "\n")
	}
	if len(unnameable) > 0 {
		fmt.Fprintf(&b, "# (%d id(s) hold a newline, which no surface can carry, and are NOT below;\n"+
			"#  they are named in the command's own output and must be cleared by rewriting the row)\n", len(unnameable))
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
