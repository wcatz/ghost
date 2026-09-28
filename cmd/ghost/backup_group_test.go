package main

import (
	"strings"
	"testing"
)

// `ghost backup` became a command group to carry `verify`, and a group whose
// subcommand position also holds its own flags is a shape the CLI has not had
// before. The decision is in backupSubcommand rather than inline because both
// branches end in a run* that exits the process — a test cannot reach them
// through the dispatch at all — and the decision is the whole of what
// `ghost backup …` does. The typo path is covered end to end by
// TestRunCLI_UnknownSubcommandIsAUsageError, which drives every group in the
// table including this one.

func TestBackupSubcommand(t *testing.T) {
	for _, tc := range []struct {
		name         string
		word         string
		given        bool
		wantSub      string
		wantRoutable bool
	}{
		{name: "the subcommand", word: "verify", given: true, wantSub: "verify", wantRoutable: true},
		// The common case, and the one that would break if a flag were read as
		// a word: the backup's own --out.
		{name: "the backup's own flag", word: "--out", given: true, wantRoutable: true},
		{name: "a flag with no value", word: "-h", given: true, wantRoutable: true},
		// A bare `ghost backup` is the command, not a missing subcommand: it is
		// the invocation the docs lead with.
		{name: "no word at all", given: false, wantRoutable: true},
		// An EMPTY word is given, not absent: a wrapper running `ghost backup
		// "$SUB"` with an unset $SUB passes one, and taking it for the bare
		// invocation would run a backup over a path nobody chose. Refused, as
		// the mcp group already refuses the same thing (#691).
		{name: "an empty word", word: "", given: true, wantSub: "", wantRoutable: false},
		// A mistyped verb is a command line that cannot be acted on, and the
		// caller has to be able to name the word it could not route.
		{name: "a mistyped verb", word: "veriy", given: true, wantSub: "veriy", wantRoutable: false},
		{name: "a file where a verb was meant", word: "/backups/snap.db", given: true, wantSub: "/backups/snap.db", wantRoutable: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sub, routable := backupSubcommand(tc.word, tc.given)
			if sub != tc.wantSub || routable != tc.wantRoutable {
				t.Errorf("backupSubcommand(%q, %v) = (%q, %v), want (%q, %v)", tc.word, tc.given, sub, routable, tc.wantSub, tc.wantRoutable)
			}
			// The word comes back unmangled whenever it is reported, because
			// usageError quotes it and a reader has to recognise their own typo.
			if !routable && sub != tc.word {
				t.Errorf("the unroutable word came back as %q, want the word as typed (%q)", sub, tc.word)
			}
		})
	}
}

// TestBackupSubcommandIsRegisteredAsAGroup: `ghost backup verify` is only
// reachable if backup is a group that lists it, and only documented if the usage
// table registers the path. TestCommandGroupsMatchTheUsageTable holds the tables
// to each other; this says what has to be in them for the new subcommand to exist
// at all, so a future edit that drops the entry names itself here.
func TestBackupSubcommandIsRegisteredAsAGroup(t *testing.T) {
	var group *commandGroup
	for i := range commandGroups {
		if commandGroups[i].path == "backup" {
			group = &commandGroups[i]
		}
	}
	if group == nil {
		t.Fatal("backup is not in commandGroups, so `ghost backup verify` would fall through to the top-level answer")
	}
	if !listsWord(group.subs, "verify") {
		t.Errorf("commandGroups[backup] does not list \"verify\": %v", group.subs)
	}
	// defaultAction, because a bare `ghost backup` is the backup and not a
	// missing subcommand. Without it, TestRunCLI_MissingSubcommandIsAUsageError
	// would demand exit 2 for the invocation the docs lead with.
	if !group.defaultAction {
		t.Error("commandGroups[backup] has no defaultAction, so a bare `ghost backup` would be answered with a usage error")
	}
	if _, ok := usageByCommand["backup verify"]; !ok {
		t.Error("usageByCommand does not register \"backup verify\", so -h/--help on it would run the command instead of printing its usage")
	}
	if usageByCommand["backup verify"] != backupVerifyUsage {
		t.Error(`usageByCommand["backup verify"] is not backupVerifyUsage: the help text and the command's own can drift`)
	}
	// And the group's own usage has to mention the subcommand, so the word is
	// discoverable from `ghost backup --help` rather than only from the manual.
	if !strings.Contains(usageByCommand["backup"], "verify") {
		t.Error(`usageByCommand["backup"] does not mention "verify"`)
	}
}

// TestRunCLI_BackupVerifyHelpAsksRatherThanChecks: the help contract (#630) is
// one gate in front of the dispatch, so a registered subcommand inherits it
// without a check of its own. Registered wrongly, `ghost backup verify --help`
// would read a file and check it for a reader who only asked a question.
func TestRunCLI_BackupVerifyHelpAsksRatherThanChecks(t *testing.T) {
	roots := isolatedHelpFS(t)
	resetDetectRemote(t)

	var code int
	stdout, stderr := captureStreams(t, func() {
		code = runCLI([]string{"backup", "verify", "--help"}, dispatchCommand)
	})

	if code != 0 {
		t.Errorf("`ghost backup verify --help` exit code = %d, want 0: a question is not a mistake", code)
	}
	if !strings.Contains(stdout, "Usage: ghost backup verify") {
		t.Errorf("stdout = %q, want the verify usage on stdout", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want the usage on stdout and nothing on stderr", stderr)
	}
	assertNoFiles(t, roots)
}
