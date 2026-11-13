package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shellSplit splits a command line the way a POSIX shell does for the quoting
// this package emits: a single-quoted run is literal, a backslash outside one
// escapes the next character, and whitespace outside a run separates.
func shellSplit(t *testing.T, line string) []string {
	t.Helper()
	var (
		out     []string
		cur     strings.Builder
		started bool
	)
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == '\\' && i+1 < len(line):
			started = true
			cur.WriteByte(line[i+1])
			i++
		case c == '\'':
			started = true
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				t.Fatalf("unterminated quote in %q", line)
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
		case c == ' ':
			if started {
				out = append(out, cur.String())
				cur.Reset()
				started = false
			}
		default:
			started = true
			cur.WriteByte(c)
		}
	}
	if started {
		out = append(out, cur.String())
	}
	return out
}

// TestSupersedeReassessFollowupNamesTheCommandAndTheFile: the block carries both
// forms. The command is what most operators run; the file is what an operator
// with forty withdrawn targets needs, because a --only argument that long is
// unreadable and easy to truncate.
func TestSupersedeReassessFollowupNamesTheCommandAndTheFile(t *testing.T) {
	ids := []string{"aaaaaaaa1111111111111111111111"}
	got := supersedeReassessFollowup("myproj", ids, "/data/ghost/scratch/supersede-reassess-myproj-20260927T220356Z.ids")
	if !strings.Contains(got, "ghost resolve myproj --reassess --only 'aaaaaaaa1111111111111111111111' --apply") {
		t.Errorf("the follow-up must print the exact command:\n%s", got)
	}
	if !strings.Contains(got, "/data/ghost/scratch/supersede-reassess-myproj-20260927T220356Z.ids") {
		t.Errorf("the follow-up must print the file it wrote:\n%s", got)
	}
	if !strings.Contains(got, "--only-file") {
		t.Errorf("the follow-up must show the --only-file form too:\n%s", got)
	}
	// The sentence has to say what the ids ARE, or a reader cannot tell a
	// follow-up command from a second list of withdrawn edges.
	if !strings.Contains(got, "resolutions those edges justified") {
		t.Errorf("the follow-up must say the ids are resolved memories to clear:\n%s", got)
	}
	if !strings.Contains(got, "another edge still holds is reported as still") {
		t.Errorf("the follow-up must not promise a clear a surviving edge prevents:\n%s", got)
	}
	if blank := supersedeReassessFollowup("myproj", nil, ""); blank != "" {
		t.Errorf("with nothing withdrawn there is no follow-up, got %q", blank)
	}
	commy := supersedeReassessFollowup("myproj", []string{"aaaaaaaa1111111111111111111111", "imported,note"}, "/data/x.ids")
	if !strings.Contains(commy, "hold a comma") || !strings.Contains(commy, "--only cannot carry") {
		t.Errorf("the follow-up does not say the command omits a comma-bearing id:\n%s", commy)
	}
	if strings.Contains(commy, "--only 'aaaaaaaa1111111111111111111111','imported,note'") {
		t.Errorf("the follow-up printed a command carrying an id --only cannot name:\n%s", commy)
	}
}

// When EVERY id holds a comma there is no --only command, and the block must not
// fall back to the command without --only: that is the project-wide re-judge #698
// measured, printed under a heading that promises nothing outside this list is
// judged. The file is the way, and the block says so.
func TestSupersedeReassessFollowupNeverPrintsTheUnscopedRepair(t *testing.T) {
	got := supersedeReassessFollowup("myproj", []string{"one,two", "three,four"}, "/data/x.ids")
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "ghost resolve") && !strings.Contains(line, "--only") && !strings.Contains(line, "--only-file") {
			t.Errorf("the follow-up printed an unscoped repair line: %q\n%s", line, got)
		}
	}
	if !strings.Contains(got, "Do NOT fall back on the same command") {
		t.Errorf("the follow-up does not warn the operator off the unscoped form:\n%s", got)
	}
	if !strings.Contains(got, "/data/x.ids") {
		t.Errorf("the follow-up does not point at the file that can name them:\n%s", got)
	}
}

// The mixed set: an empty command with comma ids AND newline ids. The header must
// not claim every id holds a comma — the newline id is omitted from the file a few
// lines later, so that claim is false about a row the file does not carry.
func TestWriteReassessTargetsHeaderNamesBothBuckets(t *testing.T) {
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	path, err := writeReassessTargets("myproj", []string{"one,two", "three\nfour"}, "ghost supersede --withdraw --apply")
	if err != nil {
		t.Fatalf("writeReassessTargets: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	head := string(raw)
	if strings.Contains(head, "every one of them holds a comma") {
		t.Errorf("the header claims every id holds a comma, which is false for the newline id:\n%s", head)
	}
	if !strings.Contains(head, "1 hold a comma") || !strings.Contains(head, "1 hold a newline") {
		t.Errorf("the header must name both buckets and their counts:\n%s", head)
	}
	// And the file carries the id it can, and omits the one it cannot.
	if !strings.Contains(head, "\none,two\n") {
		t.Errorf("the comma-bearing id is missing from the file:\n%s", head)
	}
	if strings.Contains(head, "three") {
		t.Errorf("the newline-bearing id was written into a one-id-per-line file:\n%s", head)
	}
}

// writeReassessTargets refuses a set it cannot name, rather than writing a list
// readRefSelectors rejects — the file is a --only-file INPUT, so a file that
// names nothing is not a degraded list, it is a list the repair refuses.
func TestWriteReassessTargetsRefusesASetItCannotName(t *testing.T) {
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	path, err := writeReassessTargets("myproj", []string{"two\nlines"}, "ghost supersede --reassess --apply")
	if err == nil {
		t.Fatalf("wrote %q, want a refusal: no id in that set is nameable through --only-file", path)
	}
	if !strings.Contains(err.Error(), "newline") {
		t.Errorf("error %q must say why, in the one word that explains it", err)
	}
}

// TestWriteReassessTargetsNeverWritesOverSomethingElse: the name is derived
// from the project and a clock, and a scratch root may be a directory someone
// else can write to ($GHOST_SCRATCH_DIR, or a shared root), so a name that
// already exists is not ours. os.WriteFile follows a symlink planted at that
// name and truncates whatever it points at, and overwrites a real file another
// run left. Both are refused: the write is O_EXCL, and a taken name is retried
// under a fresh one.
func TestWriteReassessTargetsNeverWritesOverSomethingElse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{"a symlink planted at the name", func(t *testing.T, path string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(filepath.Dir(path), "victim.txt"), path); err != nil {
				t.Fatalf("plant the symlink: %v", err)
			}
		}},
		{"another run's file", func(t *testing.T, path string) {
			t.Helper()
			if err := os.WriteFile(path, []byte("the first run's ids\n"), 0o600); err != nil {
				t.Fatalf("write the first run's file: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("GHOST_SCRATCH_DIR", root)
			if err := os.WriteFile(filepath.Join(root, "victim.txt"), []byte("do not touch\n"), 0o600); err != nil {
				t.Fatalf("write the victim: %v", err)
			}
			taken := "supersede-reassess-myproj-20260101T000000Z-pinned.ids"
			reassessTargetsName = func(string) (string, error) { return taken, nil }
			t.Cleanup(func() { reassessTargetsName = defaultReassessTargetsName })
			tc.setup(t, filepath.Join(root, taken))

			path, err := writeReassessTargets("myproj", []string{"aaaaaaaa1111111111111111111111"}, "ghost supersede --reassess --apply")
			if err == nil {
				t.Fatalf("writeReassessTargets returned %q for a name that was already taken", path)
			}
			if body, rerr := os.ReadFile(filepath.Join(root, "victim.txt")); rerr != nil {
				t.Fatalf("read the victim: %v", rerr)
			} else if string(body) != "do not touch\n" {
				t.Errorf("the victim was rewritten through the symlink: %q", body)
			}
			if body, rerr := os.ReadFile(filepath.Join(root, taken)); rerr == nil && strings.Contains(string(body), "aaaaaaaa") {
				t.Error("the entry already at that name was overwritten")
			}
		})
	}
}

// TestWriteReassessTargetsFailsWithoutAnId: nothing withdrawn means no file. An
// empty ids file handed to --only-file would be refused there, and leaving one
// behind is a report that claims a repair nobody can run.
func TestWriteReassessTargetsFailsWithoutAnId(t *testing.T) {
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	if _, err := writeReassessTargets("myproj", nil, "ghost supersede --reassess --apply"); err == nil {
		t.Fatal("writeReassessTargets with no ids must not write a file")
	}
}
