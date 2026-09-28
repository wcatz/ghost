package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/supersede"
)

// TestWithdrawnTargetsDedupesInListOrder: the follow-up names the memories whose
// resolution the withdrawal left behind, and two withdrawn edges can point at
// the same one. The list is deduplicated in the order the edges were reported,
// because a --only argument with the same id twice is a command the operator
// reads as a mistake.
func TestWithdrawnTargetsDedupesInListOrder(t *testing.T) {
	edges := []supersede.WithdrawnEdge{
		{NewerID: "n1", OlderID: "aaaaaaaa1111111111111111111111", Written: true},
		{NewerID: "n2", OlderID: "bbbbbbbb2222222222222222222222", Written: true},
		{NewerID: "n3", OlderID: "aaaaaaaa1111111111111111111111", Written: true},
	}
	got := withdrawnTargets(edges)
	want := []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}
	if len(got) != len(want) {
		t.Fatalf("withdrawnTargets = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("withdrawnTargets[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestWithdrawnTargetsIsEmptyForNoWithdrawal: with nothing withdrawn there is
// no follow-up, and printing one would be an operator running a repair against
// ids nothing here touched.
func TestWithdrawnTargetsIsEmptyForNoWithdrawal(t *testing.T) {
	if got := withdrawnTargets(nil); len(got) != 0 {
		t.Errorf("withdrawnTargets(nil) = %v, want nothing", got)
	}
}

// TestResolveFollowupCommandIsCopyable: the whole point of printing the command
// is that the operator runs it verbatim, so it carries full ids (a prefix could
// go ambiguous the moment another memory is written) and ends in --apply, which
// is the half that actually clears resolved_at.
func TestResolveFollowupCommandIsCopyable(t *testing.T) {
	ids := []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}
	want := "ghost resolve myproj --reassess --only aaaaaaaa1111111111111111111111,bbbbbbbb2222222222222222222222 --apply"
	if got := resolveFollowupCommand("myproj", ids); got != want {
		t.Errorf("resolveFollowupCommand() = %q, want %q", got, want)
	}
}

// TestResolveFollowupCommandQuotesAnAwkwardProjectName: a project name is free
// text — `--project` takes the next argument verbatim precisely so a
// dash-leading name works — so the printed command has to quote it. Unquoted, a
// name with a space is two positionals and the repair fails with "expected
// exactly one project", a dash-leading name is an unknown flag, and a name
// holding a metacharacter is executed when pasted.
func TestResolveFollowupCommandQuotesAnAwkwardProjectName(t *testing.T) {
	ids := []string{"aaaaaaaa1111111111111111111111"}
	for _, tc := range []struct {
		name    string
		project string
		want    string
	}{
		{"plain", "myproj", "ghost resolve myproj --reassess --only aaaaaaaa1111111111111111111111 --apply"},
		{"dotted and dashed", "platform-ops.v2", "ghost resolve platform-ops.v2 --reassess --only aaaaaaaa1111111111111111111111 --apply"},
		{"with a space", "my proj", "ghost resolve --project 'my proj' --reassess --only aaaaaaaa1111111111111111111111 --apply"},
		{"leading dash", "-myproj", "ghost resolve --project '-myproj' --reassess --only aaaaaaaa1111111111111111111111 --apply"},
		{"looks like a flag", "--apply", "ghost resolve --project '--apply' --reassess --only aaaaaaaa1111111111111111111111 --apply"},
		{"single quote", "o'brien proj", `ghost resolve --project 'o'\''brien proj' --reassess --only aaaaaaaa1111111111111111111111 --apply`},
		{"shell metacharacters", "a; rm -rf /", "ghost resolve --project 'a; rm -rf /' --reassess --only aaaaaaaa1111111111111111111111 --apply"},
		{"substitution", "$(id)", "ghost resolve --project '$(id)' --reassess --only aaaaaaaa1111111111111111111111 --apply"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveFollowupCommand(tc.project, ids); got != tc.want {
				t.Errorf("resolveFollowupCommand(%q) = %q, want %q", tc.project, got, tc.want)
			}
		})
	}
}

// TestPrintedFollowupRuns is the anti-drift test: the printed command is only
// useful if a shell hands it back to the parser as the operator typed it. This
// splits the string the way a POSIX shell does and feeds it to the real
// parseResolveArgs, so a quoting change that reads fine and parses wrong fails
// here rather than in a repair.
func TestPrintedFollowupRuns(t *testing.T) {
	for _, project := range []string{"myproj", "my proj", "-myproj", "o'brien proj", "a; rm -rf /"} {
		t.Run(project, func(t *testing.T) {
			ids := []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}
			// argv[2:], exactly as main hands the parser a command's own words.
			words := shellSplit(t, resolveFollowupCommand(project, ids))
			if len(words) < 2 || words[1] != "resolve" {
				t.Fatalf("the printed command does not start with the command word: %v", words)
			}
			parsed, err := parseResolveArgs(words[2:])
			if err != nil {
				t.Fatalf("the printed command does not parse: %v", err)
			}
			if parsed.project != project {
				t.Errorf("parsed project = %q, want %q", parsed.project, project)
			}
			if !parsed.reassess || !parsed.apply {
				t.Errorf("the printed command lost --reassess/--apply: %+v", parsed)
			}
			if len(parsed.only) != 2 || parsed.only[0] != ids[0] || parsed.only[1] != ids[1] {
				t.Errorf("parsed --only = %v, want both full ids from the one comma-joined argument", parsed.only)
			}
		})
	}
}

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
	if !strings.Contains(got, "ghost resolve myproj --reassess --only aaaaaaaa1111111111111111111111 --apply") {
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
	// And it must not promise a clear the repair pass will refuse: the floor is
	// counted per edge (#697), so a note two newer notes both supersede is still
	// held after one of the two edges is withdrawn.
	if !strings.Contains(got, "another edge still holds is reported as still") {
		t.Errorf("the follow-up must not promise a clear a surviving edge prevents:\n%s", got)
	}
	if blank := supersedeReassessFollowup("myproj", nil, ""); blank != "" {
		t.Errorf("with nothing withdrawn there is no follow-up, got %q", blank)
	}
}

// TestWriteReassessTargetsIsReadableAsOnlyFile: the file the supersede repair
// writes and the flag resolve reads are one format, and the way to hold them
// together is for the writer's own output to parse. A header comment names the
// command; every other line is an id.
func TestWriteReassessTargetsIsReadableAsOnlyFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	ids := []string{"aaaaaaaa1111111111111111111111", "bbbbbbbb2222222222222222222222"}
	path, err := writeReassessTargets("myproj", ids, "ghost supersede --reassess --apply")
	if err != nil {
		t.Fatalf("writeReassessTargets: %v", err)
	}
	if dir := filepath.Dir(path); dir != root {
		t.Errorf("wrote %s, want it under the scratch root %s", path, root)
	}
	if !strings.HasPrefix(filepath.Base(path), "supersede-reassess-myproj-") {
		t.Errorf("file name %q must name the project and the repair", filepath.Base(path))
	}
	got, err := readOnlySelectors(path)
	if err != nil {
		t.Fatalf("the file resolve is pointed at must parse: %v", err)
	}
	if len(got) != len(ids) {
		t.Fatalf("readOnlySelectors = %v, want %v", got, ids)
	}
	for i := range ids {
		if got[i] != ids[i] {
			t.Errorf("selector %d = %q, want %q", i, got[i], ids[i])
		}
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(body), "ghost resolve myproj --reassess --only") {
		t.Errorf("the file must carry the command it is for:\n%s", body)
	}
	if !strings.HasPrefix(string(body), "#") {
		t.Errorf("the file must open with a comment, not with a selector:\n%s", body)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %v, want 0600 — the file lists memory ids", perm)
	}
}

// TestWriteReassessTargetsSanitizesTheProjectName: a project name is free text
// (see --project, which takes the next argument verbatim so a dash-leading name
// works), so a name carrying a path separator must not be able to steer the
// write out of the scratch root.
func TestWriteReassessTargetsSanitizesTheProjectName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	path, err := writeReassessTargets("../../escape/odd name", []string{"aaaaaaaa1111111111111111111111"}, "ghost supersede --reassess --apply")
	if err != nil {
		t.Fatalf("writeReassessTargets: %v", err)
	}
	if dir := filepath.Dir(path); dir != root {
		t.Errorf("wrote %s, want it under the scratch root %s", path, root)
	}
	if strings.ContainsAny(filepath.Base(path), `/\ `) {
		t.Errorf("file name %q must not carry a separator or a space", filepath.Base(path))
	}
}

// TestWriteReassessTargetsGivesEachRunItsOwnFile: the printed block tells the
// operator "(the same ids are in <path>)", so a second run must not land on the
// first run's path and replace the list that sentence points at. The name
// carries a per-run token for that reason, and a whole-second timestamp alone is
// not one — two repairs of the same project in the same second collide, and the
// operator would then run a repair for the wrong set of memories.
func TestWriteReassessTargetsGivesEachRunItsOwnFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GHOST_SCRATCH_DIR", root)

	first, err := writeReassessTargets("myproj", []string{"aaaaaaaa1111111111111111111111"}, "ghost supersede --reassess --apply")
	if err != nil {
		t.Fatalf("first write: %v", err)
	}
	second, err := writeReassessTargets("myproj", []string{"bbbbbbbb2222222222222222222222"}, "ghost supersede --reassess --apply")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if first == second {
		t.Fatalf("two runs wrote the same path %q: the second list replaced the first", first)
	}
	for path, want := range map[string]string{first: "aaaaaaaa1111111111111111111111", second: "bbbbbbbb2222222222222222222222"} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(body), want) {
			t.Errorf("%s holds the wrong run's ids:\n%s", path, body)
		}
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
			// Pin the name so the planted entry is the one the writer asks for,
			// and make every attempt collide: what is under test is the refusal,
			// not the retry.
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
