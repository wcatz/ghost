package memory

import "strings"

// shellQuote quotes s for a POSIX shell command line so a suggested argument
// pastes as exactly one word. It is mcpinit's hook quoting without the Windows
// split: this text is read by a person in a POSIX shell, where a quoted word is
// still legible.
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
