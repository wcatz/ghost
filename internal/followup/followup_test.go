package followup

import (
	"strings"
	"testing"
)

// The ids are what the command judges, so they are never abbreviated: a prefix
// that is unambiguous now may not be after the next save, and this is a command
// about to be run rather than a line to read.
func TestResolveCommandUsesFullIDsInOrder(t *testing.T) {
	ids := []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}
	got, viaFile := ResolveCommand("myproj", ids)
	if len(viaFile) != 0 {
		t.Errorf("ordinary ids were pushed to the file form: %v", viaFile)
	}
	// Quoted, because an id is caller-supplied text (`ghost import` writes an
	// artifact's ids verbatim), and a POSIX shell concatenates adjacent quoted
	// words, so this is ONE argument holding "a,b" — which is what the parser
	// splits on.
	want := "ghost resolve myproj --reassess --only 'aaaaaaaa1111111111111111111111','bbbbbbbb2222222222222222222222' --apply"
	if got != want {
		t.Errorf("ResolveCommand() = %q, want %q", got, want)
	}
}

// The project name is a shell argument, and a project name is free text. A bare
// name is the positional every document uses; anything a shell would interpret —
// a space, a metacharacter, a leading dash that would read as a flag — goes
// through --project in single quotes, which parseResolveArgs takes verbatim.
func TestResolveCommandQuotesAnythingButABareWord(t *testing.T) {
	ids := []string{"aaaaaaaa1111111111111111111111"}
	for _, tc := range []struct {
		name, project, want string
	}{
		{name: "bare word", project: "my-project_2.0", want: "ghost resolve my-project_2.0 --reassess"},
		{name: "a space", project: "my project", want: `ghost resolve --project 'my project' --reassess`},
		{name: "a metacharacter", project: "a;rm -rf /", want: `ghost resolve --project 'a;rm -rf /' --reassess`},
		{name: "a dollar", project: "proj$HOME", want: `ghost resolve --project 'proj$HOME' --reassess`},
		{name: "a leading dash", project: "-dashy", want: `ghost resolve --project '-dashy' --reassess`},
		{name: "an embedded quote", project: "it's", want: `ghost resolve --project 'it'\''s' --reassess`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ResolveCommand(tc.project, ids)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("ResolveCommand(%q) = %q, want it to start with %q", tc.project, got, tc.want)
			}
			if !strings.Contains(got, "--only 'aaaaaaaa1111111111111111111111' --apply") {
				t.Errorf("the selector list is missing from %q", got)
			}
		})
	}
}

// An id is quoted like a project name, for the same reason and because an
// imported artifact can hold anything: `ghost import` writes ids verbatim and
// ImportMemory refuses only an empty one. Unquoted, an id holding a space
// word-splits into two selectors the repair refuses, and one holding a `;` is a
// second command for whoever pastes the line.
func TestResolveCommandQuotesAnIDThatIsNotHex(t *testing.T) {
	got, viaFile := ResolveCommand("myproj", []string{"imported note; rm -rf /"})
	if len(viaFile) != 0 {
		t.Errorf("an id holding a semicolon was pushed to the file form: %v", viaFile)
	}
	want := "--only 'imported note; rm -rf /' --apply"
	if !strings.Contains(got, want) {
		t.Errorf("ResolveCommand() = %q, want it to contain %q", got, want)
	}
}

// `--only` splits its value on commas, so an id holding one is not nameable by
// that flag however it is quoted: the quoting makes it one shell word, and the
// parser then splits it into two selectors that name nothing. The command must
// leave such an id OUT and report it, because emitting it produces a command that
// runs, judges the wrong rows and reports a repair that did not happen. The
// --only-file is the only surface that can carry it — it reads one id per line
// and never splits.
func TestResolveCommandSaysWhichIDsOnlyTheFileCanCarry(t *testing.T) {
	commy := "imported,note"
	ordinary := "aaaaaaaa1111111111111111111111"

	cmd, viaFile := ResolveCommand("myproj", []string{ordinary, commy})
	if len(viaFile) != 1 || viaFile[0] != commy {
		t.Errorf("viaFileOnly = %v, want [%s]", viaFile, commy)
	}
	if !strings.Contains(cmd, "'"+ordinary+"'") {
		t.Errorf("the command dropped an id it can carry: %q", cmd)
	}
	if strings.Contains(cmd, commy) {
		t.Errorf("the command carries an id --only cannot name, which would judge the wrong rows: %q", cmd)
	}

	// Every id needing the file: a command naming none of them must not be printed
	// without --only, because that reads as the unscoped project-wide repair this
	// command exists to avoid.
	all, viaFile := ResolveCommand("myproj", []string{commy, "another,one"})
	if len(viaFile) != 2 {
		t.Errorf("viaFileOnly = %v, want both", viaFile)
	}
	if strings.Contains(all, "--only") {
		t.Errorf("with every id uncarrable the command still scopes: %q", all)
	}
}
