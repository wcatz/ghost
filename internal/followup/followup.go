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
// Both are real: `ghost import` writes an artifact's ids verbatim and
// ImportMemory refuses only an empty one.
//
// The command is EMPTY when no id is carriable, and that is deliberate in the
// other direction too. Printing `ghost resolve <project> --reassess --apply` — no
// `--only` — would be the unscoped project-wide repair, the one outcome #698
// measured (143 rows proposed, about 35% of them stale) and the one this command
// exists to prevent, and both surfaces would print it as THE repair. A caller
// that gets an empty command must say the file is the only way, or name the ids.
func ResolveCommand(projectName string, ids []string) (cmd string, viaFile, unnameable []string) {
	project := projectName
	if !bareShellWord.MatchString(projectName) {
		project = "--project " + shellQuote(projectName)
	}
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
