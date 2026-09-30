package followup

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/assemble"
)

// The ids are what the command judges, so they are never abbreviated: a prefix
// that is unambiguous now may not be after the next save, and this is a command
// about to be run rather than a line to read.
func TestResolveCommandUsesFullIDsInOrder(t *testing.T) {
	ids := []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}
	got, viaFile, unnameable := ResolveCommand("myproj", ids)
	if len(viaFile) != 0 || len(unnameable) != 0 {
		t.Errorf("ordinary ids were pushed out of the command: viaFile %v, unnameable %v", viaFile, unnameable)
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
			got, _, _ := ResolveCommand(tc.project, ids)
			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("ResolveCommand(%q) = %q, want it to start with %q", tc.project, got, tc.want)
			}
			if !strings.Contains(got, "--only 'aaaaaaaa1111111111111111111111' --apply") {
				t.Errorf("the selector list is missing from %q", got)
			}
		})
	}
}

// An id is quoted like a project name, for the same reason and because an id is
// caller-supplied text on a row a store may already hold. Unquoted, an id
// holding a space word-splits into two selectors the repair refuses, and one
// holding a `;` is a second command for whoever pastes the line.
//
// Since #791 `ImportMemory` refuses an id holding whitespace at all, so a NEW
// artifact cannot plant either of these; a store that holds one got it before
// the refusal landed, from a restored snapshot, or by hand. The quoting is not
// therefore redundant — it is the reason such an id is still nameable at all,
// and `ResolveCommand` is handed ids from the store rather than from an artifact
// this run is about to read.
func TestResolveCommandQuotesAnIDThatIsNotHex(t *testing.T) {
	got, viaFile, unnameable := ResolveCommand("myproj", []string{"imported note; rm -rf /"})
	if len(viaFile) != 0 || len(unnameable) != 0 {
		t.Errorf("an id holding a semicolon was pushed out of the command: viaFile %v, unnameable %v", viaFile, unnameable)
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

	cmd, viaFile, unnameable := ResolveCommand("myproj", []string{ordinary, commy})
	if len(viaFile) != 1 || viaFile[0] != commy {
		t.Errorf("viaFile = %v, want [%s]", viaFile, commy)
	}
	if len(unnameable) != 0 {
		t.Errorf("a comma is the FILE's problem, not one no surface can carry: %v", unnameable)
	}
	if !strings.Contains(cmd, "'"+ordinary+"'") {
		t.Errorf("the command dropped an id it can carry: %q", cmd)
	}
	if strings.Contains(cmd, commy) {
		t.Errorf("the command carries an id --only cannot name, which would judge the wrong rows: %q", cmd)
	}
}

// When EVERY id holds a comma there is no --only command, and the one thing this
// function must not then produce is the command without --only: that is the
// project-wide repair, which #698 measured proposing to un-hide 143 rows of which
// about 35% were stale, and both surfaces print what comes back here as THE repair
// for this withdrawal. An empty command forces the caller to say the file is the
// only way.
func TestResolveCommandNeverRendersTheUnscopedRepair(t *testing.T) {
	cmd, viaFile, unnameable := ResolveCommand("myproj", []string{"one,two", "three,four"})
	if cmd != "" {
		t.Errorf("every id is uncarrable, so the command must be empty, got %q", cmd)
	}
	if len(viaFile) != 2 || len(unnameable) != 0 {
		t.Errorf("viaFile %v unnameable %v, want both ids in viaFile", viaFile, unnameable)
	}
}

// A newline is the character the file cannot carry either: this format is one id
// per line, so a newline in an id becomes two selectors naming nothing. It is
// reported separately from a comma because no surface can reach it, and a caller
// that confuses the two tells an operator to write a file that will not help.
func TestResolveCommandSeparatesUnnameableFromFileOnly(t *testing.T) {
	cmd, viaFile, unnameable := ResolveCommand("myproj", []string{"aaaaaaaa1111111111111111111111", "two\nlines"})
	if cmd == "" {
		t.Fatal("an ordinary id is carriable, so there must be a command")
	}
	if strings.Contains(cmd, "two") {
		t.Errorf("the command carries an id holding a newline: %q", cmd)
	}
	if len(viaFile) != 0 {
		t.Errorf("a newline is not something the file can carry: viaFile %v", viaFile)
	}
	if len(unnameable) != 1 || unnameable[0] != "two\nlines" {
		t.Errorf("unnameable = %q, want the newline-bearing id", unnameable)
	}
	if !strings.Contains(cmd, "--only") {
		t.Errorf("the command lost its scope: %q", cmd)
	}
}

// The two supersede-side commands carry the same quoting rule as ResolveCommand
// and are here for the same reason: three print sites across two reports, one
// spelling. The cases that matter are the ones where a bare name is not one shell
// word, and where --apply is the difference between a repair and a prediction.
func TestSupersedeCommandsQuoteTheProjectAndCarryApply(t *testing.T) {
	if got, want := ReassessCommand("myproj"), "ghost supersede myproj --reassess --apply"; got != want {
		t.Errorf("ReassessCommand = %q, want %q", got, want)
	}
	if got := ReassessCommand("my proj"); got != `ghost supersede --project 'my proj' --reassess --apply` {
		t.Errorf("ReassessCommand for a name holding a space = %q, want the --project form a shell reads as one argument", got)
	}
	if got := ReassessCommand("proj; rm -rf /"); got != `ghost supersede --project 'proj; rm -rf /' --reassess --apply` {
		t.Errorf("ReassessCommand for a name holding a semicolon = %q, want it quoted: an unquoted name executes when pasted", got)
	}
	if got, nameable := WithdrawCommand("myproj", "abc123", "def456"); !nameable || got != "ghost supersede myproj --withdraw 'abc123' 'def456' --apply" {
		t.Errorf("WithdrawCommand = %q (nameable %v), want the --apply command", got, nameable)
	}
	// An id is caller-supplied text, and this one comes out of an import, so it is
	// quoted whatever it holds.
	if got, nameable := WithdrawCommand("myproj", "imported note; rm -rf /", "def456"); !nameable || !strings.Contains(got, `'imported note; rm -rf /'`) {
		t.Errorf("WithdrawCommand = %q (nameable %v), want the id quoted", got, nameable)
	}
	// A DASH-LEADING id is the one shape `ghost supersede` cannot be given:
	// parseSupersedeArgs refuses a --withdraw operand that looks like a flag, and
	// no amount of quoting changes the word a shell delivers. So the renderer
	// refuses to build the command and says so, and the caller names the ids and
	// the surface that CAN take them — the same split ResolveCommand makes.
	for _, id := range []string{"-imported-id", "-"} {
		if got, nameable := WithdrawCommand("myproj", id, "def456"); nameable || got != "" {
			t.Errorf("WithdrawCommand(%q) = %q (nameable %v), want no command and nameable=false: the parser refuses a dash-leading operand", id, got, nameable)
		}
	}
	// Both commands are repairs, and a repair without --apply is a dry run: the
	// flag is part of the command, not something the operator adds.
	withdrawCmd, _ := WithdrawCommand("p", "a", "b")
	for _, cmd := range []string{ReassessCommand("p"), withdrawCmd} {
		if !strings.Contains(cmd, "--apply") {
			t.Errorf("%q names a repair that predicts rather than performs", cmd)
		}
	}
}

// The buckets are the other half of what ResolveCommand answers, and they are
// printed by three surfaces — the CLI twice and each MCP tool once — so how an id
// is spelled is the one rule none of them may decide for itself. This function is
// that decision, and the test is that it makes the value DATA rather than text
// sitting at the head of a line.
//
// The hostile ids here hold a «, a backtick and a control character, which is the
// shape a comma bucket really contains: since #791 `ImportMemory` refuses a
// control character, whitespace, a backtick or a «, and a COMMA is deliberately
// not in that class — a comma breaks a selector rather than a line — so such an id
// still reaches the store, from an artifact imported before the refusal, a
// restored snapshot or a hand-edited row. The newline bucket holds one too, and
// that is the half %q got wrong: %q keeps a printable non-ASCII rune as itself,
// so a « stayed literally visible in the MCP surfaces while the CLI escaped it.
func TestRenderUncarriedIDsQuotesEveryStoredIDItPrints(t *testing.T) {
	commy := "AAAA,«bell\x07`x`"
	broken := "BBBB\n«second line»"
	viaFileText, unnameableText := RenderUncarriedIDs([]string{commy}, []string{broken})

	for _, c := range []struct {
		bucket, id, text string
	}{
		{"the comma bucket", commy, viaFileText},
		{"the newline bucket", broken, unnameableText},
	} {
		t.Run(c.bucket, func(t *testing.T) {
			if !strings.Contains(c.text, assemble.Token(c.id)) {
				t.Errorf("%s does not print the id through assemble.Token (%q):\n%q", c.bucket, assemble.Token(c.id), c.text)
			}
			// Line-anchored, and the reason this is not a substring test: the value
			// IS in a Token either way, and what a reader is fooled by is an id at
			// the head of a line, where it reads as Ghost's own.
			for _, line := range strings.Split(c.text, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), c.id) {
					t.Errorf("%s printed the id raw at the start of a line:\n%q", c.bucket, c.text)
				}
			}
			// Each line is the whole value and nothing else, so a second id on one
			// line cannot hide inside the first one's rendering.
			for _, line := range strings.Split(strings.TrimSuffix(c.text, "\n"), "\n") {
				if got := strings.TrimSpace(line); got != assemble.Token(c.id) {
					t.Errorf("%s line = %q, want exactly the tokenized id", c.bucket, got)
				}
			}
		})
	}

	// An honest id is printed bare and indented: Token quotes only what needs it,
	// so the 32 hex characters Ghost mints cost nothing on any real report.
	if got, _ := RenderUncarriedIDs([]string{"aaaaaaaa1111111111111111111111"}, nil); got != "    aaaaaaaa1111111111111111111111\n" {
		t.Errorf("an ordinary id rendered as %q, want it bare under four spaces of indent", got)
	}
	// An empty bucket renders to nothing, so a caller can write the result
	// unconditionally rather than guarding each one.
	if via, un := RenderUncarriedIDs(nil, nil); via != "" || un != "" {
		t.Errorf("RenderUncarriedIDs(nil, nil) = %q, %q, want two empty strings", via, un)
	}
}

// Both buckets come back from ONE call, so the two cannot be spelled differently
// — which is the whole reason the signature takes both. The order is the buckets'
// order and nothing reorders it, because the caller prints the comma list and the
// unnameable list under prose that says which is which.
func TestRenderUncarriedIDsKeepsBothBucketsApart(t *testing.T) {
	ids := []string{"plain", "with,comma", "with\nnewline"}
	_, viaFile, unnameable := ResolveCommand("proj", ids)
	if got, want := viaFile, []string{"with,comma"}; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("ResolveCommand bucketed %v as viaFile %v, want %v — the renderer's input is not what this test means to check", ids, got, want)
	}
	viaFileText, unnameableText := RenderUncarriedIDs(viaFile, unnameable)
	if n := strings.Count(viaFileText, "\n"); n != 1 {
		t.Errorf("the comma bucket rendered %d lines, want one:\n%q", n, viaFileText)
	}
	if n := strings.Count(unnameableText, "\n"); n != 1 {
		t.Errorf("the newline bucket rendered %d lines, want one:\n%q", n, unnameableText)
	}
	if strings.Contains(viaFileText, "newline") || strings.Contains(unnameableText, "comma") {
		t.Errorf("a bucket printed another's id: %q / %q", viaFileText, unnameableText)
	}
}
