// Package followup renders the command that completes a supersede repair.
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
// It lives in its own package because two surfaces print it and neither may
// render it differently. The CLI prints it under a per-edge list; the MCP tool
// answers an agent that may have no shell at all, where the same string is a
// sentence rather than something to paste. One renderer means the project-name
// quoting — the part that decides whether the command RUNS — is decided once, and
// a change to it cannot leave the two surfaces disagreeing about it.
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
// It also returns the ids the command cannot carry, which is not a defensive
// nicety: `--only` takes a COMMA-separated list and splits on commas, so an id
// holding one is not nameable by that flag however it is quoted — the quoting
// makes it one shell word, and the parser then splits it into two selectors that
// name nothing. Rendering it anyway would produce a command that runs, judges the
// wrong rows and reports a repair that did not happen. `ghost import` writes an
// artifact's ids verbatim and ImportMemory refuses only an empty one, so such an
// id is real. Those ids go in the --only-file instead, which reads one per line
// and never splits.
func ResolveCommand(projectName string, ids []string) (cmd string, viaFileOnly []string) {
	project := projectName
	if !bareShellWord.MatchString(projectName) {
		project = "--project " + shellQuote(projectName)
	}
	// The ids are quoted for the same reason the project is, and it is NOT a
	// formality: `ghost import` writes an artifact's ids verbatim and
	// ImportMemory refuses only an empty one, so an id can hold a space, a `;` or a
	// backtick. Unquoted, such an id word-splits into selectors the repair then
	// refuses — and pasted into a shell, it executes. A POSIX shell concatenates
	// adjacent quoted words, so `--only 'a','b'` is ONE argument holding "a,b",
	// which is exactly what parseResolveArgs splits on.
	quoted := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.Contains(id, ",") {
			viaFileOnly = append(viaFileOnly, id)
			continue
		}
		quoted = append(quoted, shellQuote(id))
	}
	if len(quoted) == 0 {
		// Every id needs the file, so a command naming none would look like an
		// unscoped repair — the very thing scoping exists to avoid. It is printed
		// without --only and the caller must say the file is the only way to run it.
		return fmt.Sprintf("ghost resolve %s --reassess --apply", project), viaFileOnly
	}
	return fmt.Sprintf("ghost resolve %s --reassess --only %s --apply", project, strings.Join(quoted, ",")), viaFileOnly
}
