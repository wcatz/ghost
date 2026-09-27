package memory

import "strings"

// shellQuote quotes s for a POSIX shell command line, so an argument inside a
// command Ghost suggests to the reader pastes into their shell as exactly one
// argument and runs. It is the same shape internal/mcpinit uses for the hook
// commands it writes into a host's config, one platform split lighter: that one
// has to survive cmd.exe on Windows, while this text is read by a person (or an
// agent reading a person) in a POSIX shell, and a POSIX-quoted word is still
// legible as a word when it is read rather than run.
//
// A project id in this repository is frequently an absolute path, and Go's %q is
// not shell quoting: `C:\work\infra` comes out with its backslashes doubled
// (a path that then does not exist), a `$` or a backtick survives as itself
// inside the double quotes %q emits (so the shell expands it), and a control
// character comes out as a \u escape the shell does not interpret. Wrapping in
// single quotes and ending, escaping and re-opening the word around each
// embedded quote is the only spelling every POSIX shell reads back as the
// original string.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if strings.IndexFunc(s, needsShellQuote) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// needsShellQuote reports whether r is anything other than a character a POSIX
// shell passes through untouched inside an unquoted word. The safe set is the
// one shlex.quote uses, minus the shell's own metacharacters: a backslash is
// not in it, because an unquoted backslash is an escape rather than a character.
func needsShellQuote(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	}
	return !strings.ContainsRune("@%+=:,./-_", r)
}
