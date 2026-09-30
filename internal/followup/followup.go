// Package followup renders the command that completes a resolve or supersede repair.
//
// Withdrawing a 'supersedes' edge is not the whole repair. `ghost resolve`'s
// piggyback stamps resolved_at on the older endpoint for free, and resolve's own
// repair pass deliberately honours a LIVE edge as a floor — so the memory the
// withdrawal orphaned stays out of ranked injection until a second pass clears
// it. The command that clears exactly those memories is therefore part of what a
// withdrawal has to say, and it has to be the SCOPED one: an unscoped repair
// re-judges every resolved memory in the project, and #698 measured that
// proposing to un-hide 143 rows on a real store, about 35% of them stale.
//
// It lives in its own package because THREE surfaces print it and none may
// render it differently. The CLI prints it under a supersede withdrawal's
// per-edge list; the CLI's `resolve --mark` report prints it for the memories
// that run stamped, which is the other direction of the same stamp and so the
// same command read backwards; and the MCP `ghost_link_withdraw` tool answers an
// agent that may have no shell at all, where the same string is a sentence rather
// than something to paste. One renderer means the project-name quoting — the part
// that decides whether the command RUNS — is decided once, and a change to it
// cannot leave the surfaces disagreeing about it.
//
// The two SUPERSEDE-side commands live here for that same reason rather than
// beside their callers, and they are the reason this is a package and not a
// helper file: `ReassessCommand` is printed by the creation pass (a pair the
// graph claims in both directions) and by the repair pass (a cycle it could not
// settle), and `WithdrawCommand` is printed per cycle edge. Three print sites
// between two reports, one spelling, and a project name that has to be rendered
// as a single shell argument or the pasted command runs against the wrong
// project.
//
// The two directions are why this is the ONE renderer rather than two that happen
// to agree. `--mark`'s follow-up is a supersede repair's follow-up read in the
// other direction: a withdrawal's report says "run this to clear the resolution
// the edge caused", and a mark's says "run this to clear the resolution this
// stamp caused". Two renderers for one command would drift on the quoting, and a
// command that does not run is worse than one whose wording is inconsistent.
package followup

import (
	"fmt"
	"regexp"
	"strings"
)

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

// ReassessCommand renders the project-wide supersede repair:
// `ghost supersede <project> --reassess --apply`, with the project spelled the
// way ResolveCommand spells it — a positional when that is one shell word, and
// --project in single quotes when it is not.
//
// It exists beside ResolveCommand because the quoting is the whole risk in a
// command that is about to be RUN, and it is decided in this package for exactly
// that reason. Two callers need it and neither may spell it differently: the
// creation pass's report, for a pair the graph claims in BOTH directions (#778),
// and the repair pass's own report, for a cycle whose two edges it could not
// settle. Both quote the APPLIED form, because --reassess without --apply is a
// dry run that withdraws nothing, and a report that named it would advertise a
// repair its own command cannot perform.
func ReassessCommand(projectName string) string {
	return "ghost supersede " + projectArg(projectName) + " --reassess --apply"
}

// WithdrawCommand renders the withdrawal of ONE named edge:
// `ghost supersede <project> --withdraw <source> <target> --apply`.
//
// It is the operator's own repair — no model, no classification, and the whole
// request settled before anything is written — and it is what a cycle the pass
// could not decide is handed as, because two contradictory edges are not a
// verdict question this pass can answer. The ids are full, for the reason
// ResolveCommand's are: this is a command about to be run, and an eight-character
// prefix that is unambiguous now may not be after the next save. --apply is on it
// for the same reason as everywhere else here: without it the command prints
// "would withdraw" and writes nothing.
//
// nameable is false when `ghost supersede` CANNOT be given this pair, and it is
// a separate return rather than a comment because the caller has to do something
// about it. parseSupersedeArgs refuses a --withdraw operand beginning with a
// dash — deliberately, since an id is caller-supplied text and a dash-leading one
// is a flag there — and quoting does not change that: the shell delivers the same
// word either way. `ghost import` writes ids verbatim, so such an id reaches
// memory_links and reaches here. A caller that printed the command regardless
// would print one that fails with a message naming neither the dash nor the id,
// which is ResolveCommand's `unnameable` bucket for the same class of problem: the
// answer is to name the ids and the surface that CAN take them
// (`ghost_link_withdraw`, which parses no flags), not to print a dead command.
func WithdrawCommand(projectName, sourceID, targetID string) (cmd string, nameable bool) {
	if !withdrawOperandNameable(sourceID) || !withdrawOperandNameable(targetID) {
		return "", false
	}
	return fmt.Sprintf("ghost supersede %s --withdraw %s %s --apply",
		projectArg(projectName), shellQuote(sourceID), shellQuote(targetID)), true
}

// withdrawOperandNameable mirrors the one rule in parseSupersedeArgs that decides
// whether an id can be an operand of --withdraw. It is restated here rather than
// imported because that parser is in package main and this package must not depend
// on it — the two are pinned together by TestSupersedeRepairCommandsParse, which
// runs every rendered command through the real parser, so a rule that drifts is a
// failing test rather than a command that does not run.
func withdrawOperandNameable(id string) bool {
	return id != "" && !strings.HasPrefix(id, "-")
}

// projectArg is the project as ONE shell argument: the bare name when that
// parses as a single word, and --project with the name quoted when it does not.
// The quoted form is not a formality — `ghost supersede my proj` is two
// positionals and the second overwrites the first, and a name holding a `;`
// pasted unquoted executes.
func projectArg(projectName string) string {
	if bareShellWord.MatchString(projectName) {
		return projectName
	}
	return "--project " + shellQuote(projectName)
}

// ResolveCommand renders the repair for the given target ids: a `ghost resolve`
// SCOPED to them, with the project as a positional when that is one shell word
// and through --project in single quotes when it is not.
//
// The ids are full, not the eight-character abbreviations the reports print: this
// is a command about to be RUN rather than a line to read, and a prefix that is
// unambiguous now may not be after the next save.
//
// The project name is a shell argument and a project name is free text — the
// reason internal/mcpinit sanitizes one before using it as a filename. Rendering
// a bare `ghost resolve my proj …` would be two positionals and a repair that
// refuses; rendering a name holding a shell metacharacter bare would execute it.
// --project takes its value verbatim, which is also why a dash-leading name works
// there and not as a bare positional. The ids are quoted for the same reason and
// with the same rule, because an id is caller-supplied text too (see the loop).
//
// It also returns the ids it could not put in the command, split by which surface
// can still reach them, because "carried a shell" is not the same as "nameable":
//
//   - viaFile holds an id holding a COMMA. `--only` splits its value on commas, so
//     such an id is not nameable by that flag however it is quoted — the quoting
//     makes it one shell word, and the parser then splits it into two selectors
//     that name nothing. The --only-file reads one id per line and never splits,
//     so it carries them.
//   - unnameable holds an id holding a NEWLINE. The file is one id per line, so it
//     would become two selectors, and no surface can carry it: the memory stays
//     stamped resolved until the row itself is rewritten.
//
// Both are real. `ghost import` writes an artifact's ids verbatim, and
// ImportMemory refuses an empty one and — since #791 — one holding a control
// character, whitespace, a backtick or a «». A COMMA is not in that class, which
// is the point of the split: the refused characters are the ones that can end a
// rendered line, and a comma breaks a selector rather than a line. A NEWLINE is
// refused now, so the unnameable case above is a row a store already held
// rather than one a file can plant — a store written before the refusal landed,
// a restored snapshot, a hand edit. Either way the memory stays stamped, and
// this is the sentence an operator holding one needs.
//
// The command is EMPTY when no id is carriable, and that is deliberate in the
// other direction too. Printing `ghost resolve <project> --reassess --apply` — no
// `--only` — would be the unscoped project-wide repair, the one outcome #698
// measured (143 rows proposed, about 35% of them stale) and the one this command
// exists to prevent, and both surfaces would print it as THE repair. A caller
// that gets an empty command must say the file is the only way, or name the ids.
func ResolveCommand(projectName string, ids []string) (cmd string, viaFile, unnameable []string) {
	project := projectArg(projectName)
	// The ids are quoted for the same reason the project is, and it is NOT a
	// formality: an id can hold a space, a `;` or a backtick. Unquoted, such an id
	// word-splits into selectors the repair then refuses — and pasted into a shell,
	// it executes. A POSIX shell concatenates adjacent quoted words, so
	// `--only 'a','b'` is ONE argument holding "a,b", which is exactly what
	// parseResolveArgs splits on.
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		switch {
		case strings.ContainsAny(id, "\n\r"):
			unnameable = append(unnameable, id)
		case strings.Contains(id, ","):
			viaFile = append(viaFile, id)
		default:
			quoted = append(quoted, shellQuote(id))
		}
	}
	if len(quoted) == 0 {
		return "", viaFile, unnameable
	}
	return fmt.Sprintf("ghost resolve %s --reassess --only %s --apply", project, strings.Join(quoted, ",")), viaFile, unnameable
}
