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
	got := ResolveCommand("myproj", ids)
	want := "ghost resolve myproj --reassess --only aaaaaaaa1111111111111111111111,bbbbbbbb2222222222222222222222 --apply"
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
			got := ResolveCommand(tc.project, ids)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("ResolveCommand(%q) = %q, want it to start with %q", tc.project, got, tc.want)
			}
			if !strings.Contains(got, "--only aaaaaaaa1111111111111111111111 --apply") {
				t.Errorf("the selector list is missing from %q", got)
			}
		})
	}
}
