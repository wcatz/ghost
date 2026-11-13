package main

import (
	"testing"
)

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
