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
// there and not as a bare positional.
func ResolveCommand(projectName string, ids []string) string {
	project := projectName
	if !bareShellWord.MatchString(projectName) {
		project = "--project " + shellQuote(projectName)
	}
	return fmt.Sprintf("ghost resolve %s --reassess --only %s --apply", project, strings.Join(ids, ","))
}
