package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeOpenCodeSessionCLI writes a fake `opencode` that answers `session list`
// with sessionsJSON, records every invocation in <dir>/calls.log, and records
// successful deletes in <dir>/deleted.log. A file <dir>/fail/<id> holding a
// positive number makes that id's next N delete attempts fail with a non-zero
// exit, so retry and failure paths are exercised without a real store.
//
// The child gets the shared harness environment allowlist, so a test-only
// variable cannot reach it: the log lives beside the binary, which the child
// can locate from its own argv[0]. Returns the directory holding the binary.
func fakeOpenCodeSessionCLI(t *testing.T, sessionsJSON string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake binary requires a POSIX shell")
	}
	// Pin the scratch root to this test's temp dir so the run never opens the
	// real data dir — and so the temp-root assertion has a path to compare
	// against.
	t.Setenv("GHOST_SCRATCH_DIR", t.TempDir())
	dir := t.TempDir()
	script := `#!/bin/sh
dir=$(dirname "$0")
printf '%s\n' "$*" >> "$dir/calls.log"
printf '%s\n' "$TMPDIR" >> "$dir/tmpdir.log"
case "$1:$2" in
  session:list)
    if [ -f "$dir/fail-list" ]; then
      echo "session list unavailable" >&2
      exit 1
    fi
    cat "$dir/sessions.json"
    exit 0
    ;;
  session:delete)
    id=$3
    f="$dir/fail/$id"
    if [ -f "$f" ]; then
      n=$(cat "$f")
      if [ "$n" -gt 0 ]; then
        echo "$((n - 1))" > "$f"
        echo "simulated delete failure for $id" >&2
        exit 1
      fi
    fi
    printf '%s\n' "$id" >> "$dir/deleted.log"
    exit 0
    ;;
esac
echo "unexpected invocation: $*" >&2
exit 3
`
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions.json"), []byte(sessionsJSON), 0o600); err != nil {
		t.Fatalf("write sessions fixture: %v", err)
	}
	// PATH override: the fake must win the lookup so no real opencode — and
	// above all no real `session delete` — can be reached from this test.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved, err := exec.LookPath("opencode")
	if err != nil || filepath.Dir(resolved) != dir {
		t.Fatalf("fake opencode not first on PATH: %q (err=%v)", resolved, err)
	}
	return dir
}

// sessionsFixture marshals the session objects the fake `session list` returns.
func sessionsFixture(t *testing.T, sessions ...map[string]any) string {
	t.Helper()
	data, err := json.Marshal(sessions)
	if err != nil {
		t.Fatalf("marshal sessions: %v", err)
	}
	return string(data)
}

// session builds one session object with created and updated stamped
// createdAgo/updatedAgo before now. Durations that are not both positive
// yield a session with no timestamps at all — the unknown-age case.
func session(id, title string, createdAgo, updatedAgo time.Duration, now time.Time) map[string]any {
	m := map[string]any{"id": id}
	if title != "" {
		m["title"] = title
	}
	if createdAgo > 0 && updatedAgo > 0 {
		m["created"] = now.Add(-createdAgo).UnixMilli()
		m["updated"] = now.Add(-updatedAgo).UnixMilli()
	}
	return m
}

// fakeCalls returns the lines the fake recorded in calls.log (nil when the
// binary was never invoked).
func fakeCalls(t *testing.T, dir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "calls.log"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read calls.log: %v", err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// fakeDeletes returns the ids the fake actually deleted, in order.
func fakeDeletes(t *testing.T, dir string) []string {
	t.Helper()
	return fakeLines(t, filepath.Join(dir, "deleted.log"))
}

func fakeLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Split(strings.TrimRight(string(data), "\n"), "\n")
}

// fakeFail budgets N failing delete attempts for id.
func fakeFail(t *testing.T, dir, id string, attempts int) {
	t.Helper()
	failDir := filepath.Join(dir, "fail")
	if err := os.MkdirAll(failDir, 0o755); err != nil {
		t.Fatalf("mkdir fail dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(failDir, id), []byte(strconv.Itoa(attempts)), 0o644); err != nil {
		t.Fatalf("write fail budget: %v", err)
	}
}

// countCalls returns how many logged invocations start with prefix.
func countCalls(calls []string, prefix string) int {
	n := 0
	for _, c := range calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// countDeleteCalls returns how many logged invocations were `session delete`
// for exactly this id (exact, so ses_b never counts ses_b2).
func countDeleteCalls(calls []string, id string) int {
	line := "session delete " + id
	n := 0
	for _, c := range calls {
		if c == line {
			n++
		}
	}
	return n
}

// TestCleanupOpenCodeSessions_DryRunCountsAndDeletesNothing is the default
// mode: one `session list`, a count of what would go, and not a single delete
// — against a store that also holds sessions a deletion would corrupt if the
// exact-title rule slipped.
func TestCleanupOpenCodeSessions_DryRunCountsAndDeletesNothing(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		session("ses_old", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
		session("ses_recent", ghostSessionTitle, 30*time.Minute, 5*time.Minute, now),
		session("ses_own", "my own session", 10*time.Hour, 9*time.Hour, now),
		session("ses_suffixed", ghostSessionTitle+" follow-up", 10*time.Hour, 9*time.Hour, now),
		session("ses_case", "[Ghost]", 10*time.Hour, 9*time.Hour, now),
		session("ses_prefix", "ghost", 10*time.Hour, 9*time.Hour, now),
	))

	var out bytes.Buffer
	res, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now, Limit: 42}, &out)
	if err != nil {
		t.Fatalf("CleanupOpenCodeSessions: %v", err)
	}
	if res.Listed != 6 || res.Eligible != 1 {
		t.Errorf("Listed=%d Eligible=%d, want 6 and 1", res.Listed, res.Eligible)
	}
	if res.Deleted != 0 || res.Failed != 0 {
		t.Errorf("dry run Deleted=%d Failed=%d, want 0 and 0", res.Deleted, res.Failed)
	}
	if got := fakeDeletes(t, dir); len(got) != 0 {
		t.Errorf("dry run deleted %v, want none", got)
	}
	calls := fakeCalls(t, dir)
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "session list --format json --max-count 42") {
		t.Errorf("calls = %v, want exactly the one `session list` with --max-count 42", calls)
	}
	if !strings.Contains(out.String(), "dry run") {
		t.Errorf("dry-run report missing from output:\n%s", out.String())
	}
}

// TestCleanupOpenCodeSessions_ApplyDeletesOnlyEligible pins the other half of
// the selection contract: --apply deletes exactly the eligible ids, in list
// order, and leaves every other session — different title, title that only
// starts with "[ghost]", different case, still inside the grace period —
// untouched.
func TestCleanupOpenCodeSessions_ApplyDeletesOnlyEligible(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		session("ses_own", "my own session", 10*time.Hour, 9*time.Hour, now),
		session("ses_old1", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
		session("ses_recent", ghostSessionTitle, 30*time.Minute, 5*time.Minute, now),
		session("ses_suffixed", ghostSessionTitle+" follow-up", 10*time.Hour, 9*time.Hour, now),
		session("ses_old2", ghostSessionTitle, 5*time.Hour, 4*time.Hour, now),
	))

	var out bytes.Buffer
	res, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now, Apply: true}, &out)
	if err != nil {
		t.Fatalf("CleanupOpenCodeSessions: %v", err)
	}
	if res.Eligible != 2 || res.Deleted != 2 || res.Failed != 0 {
		t.Errorf("Eligible=%d Deleted=%d Failed=%d, want 2/2/0", res.Eligible, res.Deleted, res.Failed)
	}
	want := []string{"ses_old1", "ses_old2"}
	got := fakeDeletes(t, dir)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("deleted %v, want %v", got, want)
	}
	if calls := fakeCalls(t, dir); countCalls(calls, "session delete ") != 2 {
		t.Errorf("delete invocations = %v, want exactly 2", calls)
	}
	if !strings.Contains(out.String(), "deleted 2") {
		t.Errorf("final count missing from output:\n%s", out.String())
	}
}

// TestCleanupOpenCodeSessions_FlagShapedIDIsNeverPassed: a malformed row whose
// id could be read as an opencode flag must never reach `session delete` — it
// is not deletable, so it is not eligible.
func TestCleanupOpenCodeSessions_FlagShapedIDIsNeverPassed(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		map[string]any{"id": "--standalone", "title": ghostSessionTitle,
			"created": now.Add(-5 * time.Hour).UnixMilli(), "updated": now.Add(-5 * time.Hour).UnixMilli()},
		session("ses_old", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
	))

	var out bytes.Buffer
	res, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now, Apply: true}, &out)
	if err != nil {
		t.Fatalf("CleanupOpenCodeSessions: %v", err)
	}
	if res.Eligible != 1 || res.Deleted != 1 {
		t.Errorf("Eligible=%d Deleted=%d, want 1/1", res.Eligible, res.Deleted)
	}
	for _, call := range fakeCalls(t, dir) {
		if strings.Contains(call, "--standalone") {
			t.Errorf("flag-shaped id reached the CLI: %q", call)
		}
	}
	if got := fakeDeletes(t, dir); len(got) != 1 || got[0] != "ses_old" {
		t.Errorf("deleted %v, want [ses_old]", got)
	}
}

// TestCleanupOpenCodeSessions_DeleteFailureNeverAbortsTheRest: one id that
// always fails is counted and reported, every other eligible session is still
// deleted, and the retry stays bounded at sessionDeleteAttempts tries.
func TestCleanupOpenCodeSessions_DeleteFailureNeverAbortsTheRest(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		session("ses_a", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
		session("ses_b", ghostSessionTitle, 4*time.Hour, 3*time.Hour, now),
		session("ses_c", ghostSessionTitle, 5*time.Hour, 4*time.Hour, now),
	))
	fakeFail(t, dir, "ses_b", 1000)

	var out bytes.Buffer
	res, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now, Apply: true, RetryDelay: time.Millisecond}, &out)
	if err != nil {
		t.Fatalf("CleanupOpenCodeSessions: %v", err)
	}
	if res.Deleted != 2 || res.Failed != 1 {
		t.Errorf("Deleted=%d Failed=%d, want 2/1", res.Deleted, res.Failed)
	}
	got := fakeDeletes(t, dir)
	if len(got) != 2 || got[0] != "ses_a" || got[1] != "ses_c" {
		t.Errorf("deleted %v, want [ses_a ses_c] — a failure must not stop the run", got)
	}
	if n := countDeleteCalls(fakeCalls(t, dir), "ses_b"); n != sessionDeleteAttempts {
		t.Errorf("ses_b delete attempts = %d, want the bounded %d", n, sessionDeleteAttempts)
	}
	if !strings.Contains(out.String(), "failed") || !strings.Contains(out.String(), "deleted 2") {
		t.Errorf("report must show both the failure and the totals:\n%s", out.String())
	}
}

// TestCleanupOpenCodeSessions_RetriesThenSucceeds: a transient failure costs
// one retry, not a failed count.
func TestCleanupOpenCodeSessions_RetriesThenSucceeds(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		session("ses_a", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
	))
	fakeFail(t, dir, "ses_a", 1)

	var out bytes.Buffer
	res, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now, Apply: true, RetryDelay: time.Millisecond}, &out)
	if err != nil {
		t.Fatalf("CleanupOpenCodeSessions: %v", err)
	}
	if res.Deleted != 1 || res.Failed != 0 {
		t.Errorf("Deleted=%d Failed=%d, want 1/0", res.Deleted, res.Failed)
	}
	if n := countDeleteCalls(fakeCalls(t, dir), "ses_a"); n != 2 {
		t.Errorf("delete attempts = %d, want 2 (first failed, retry succeeded)", n)
	}
}

// TestCleanupOpenCodeSessions_ListFailureReturnsError: without a trustworthy
// list there is nothing to select from, so the run fails loudly and deletes
// nothing at all.
func TestCleanupOpenCodeSessions_ListFailureReturnsError(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		session("ses_old", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
	))
	if err := os.WriteFile(filepath.Join(dir, "fail-list"), []byte("1"), 0o644); err != nil {
		t.Fatalf("write fail-list marker: %v", err)
	}

	var out bytes.Buffer
	_, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now, Apply: true}, &out)
	if err == nil {
		t.Fatal("expected an error when `session list` fails")
	}
	if !strings.Contains(err.Error(), "session list unavailable") {
		t.Errorf("error should carry the CLI's own report, got %v", err)
	}
	if got := fakeDeletes(t, dir); len(got) != 0 {
		t.Errorf("deleted %v despite an unusable list, want none", got)
	}
}

// TestCleanupOpenCodeSessions_MalformedListOutputIsAnError: output the parser
// cannot vouch for must not be silently treated as "no sessions" (nor as a
// selection), because a partial or garbled list would otherwise look like a
// clean, empty run.
func TestCleanupOpenCodeSessions_MalformedListOutputIsAnError(t *testing.T) {
	fakeOpenCodeSessionCLI(t, "not json at all")

	var out bytes.Buffer
	_, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour}, &out)
	if err == nil {
		t.Fatal("expected an error for unparseable `session list` output")
	}
	if !strings.Contains(err.Error(), "unparseable") {
		t.Errorf("error = %v, want it to name the parse failure", err)
	}
}

// TestSelectCleanupSessions pins the eligibility rules on their own, without
// a subprocess, including the boundary of the grace period.
func TestSelectCleanupSessions(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	cases := []struct {
		name    string
		session openCodeSession
		grace   time.Duration
		want    bool
	}{
		{name: "old ghost session", session: openCodeSession{ID: "ses_a", Title: ghostSessionTitle, Updated: now.Add(-2 * time.Hour).UnixMilli()}, grace: time.Hour, want: true},
		{name: "exactly at the grace boundary is not older", session: openCodeSession{ID: "ses_b", Title: ghostSessionTitle, Updated: now.Add(-time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "one tick past the boundary", session: openCodeSession{ID: "ses_c", Title: ghostSessionTitle, Updated: now.Add(-time.Hour - time.Millisecond).UnixMilli()}, grace: time.Hour, want: true},
		{name: "zero grace takes any age", session: openCodeSession{ID: "ses_d", Title: ghostSessionTitle, Updated: now.Add(-time.Millisecond).UnixMilli()}, grace: 0, want: true},
		{name: "still inside the grace period", session: openCodeSession{ID: "ses_e", Title: ghostSessionTitle, Updated: now.Add(-30 * time.Minute).UnixMilli()}, grace: time.Hour, want: false},
		{name: "title only prefixed", session: openCodeSession{ID: "ses_f", Title: ghostSessionTitle + " x", Updated: now.Add(-9 * time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "different case", session: openCodeSession{ID: "ses_g", Title: "[Ghost]", Updated: now.Add(-9 * time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "padded title", session: openCodeSession{ID: "ses_h", Title: " [ghost]", Updated: now.Add(-9 * time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "untitled", session: openCodeSession{ID: "ses_i", Updated: now.Add(-9 * time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "no known timestamp", session: openCodeSession{ID: "ses_j", Title: ghostSessionTitle}, grace: time.Hour, want: false},
		{name: "empty id", session: openCodeSession{Title: ghostSessionTitle, Updated: now.Add(-9 * time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "flag-shaped id", session: openCodeSession{ID: "--version", Title: ghostSessionTitle, Updated: now.Add(-9 * time.Hour).UnixMilli()}, grace: time.Hour, want: false},
		{name: "updated later than created counts as recent activity", session: openCodeSession{ID: "ses_k", Title: ghostSessionTitle, Created: now.Add(-9 * time.Hour).UnixMilli(), Updated: now.Add(-time.Minute).UnixMilli()}, grace: time.Hour, want: false},
		{name: "a seconds-unit timestamp lands in 1970 and is refused", session: openCodeSession{ID: "ses_l", Title: ghostSessionTitle, Updated: now.Add(-2 * time.Hour).Unix()}, grace: time.Hour, want: false},
		{name: "a nanosecond-unit timestamp lands far in the future and is refused", session: openCodeSession{ID: "ses_m", Title: ghostSessionTitle, Updated: now.Add(-2 * time.Hour).UnixNano()}, grace: time.Hour, want: false},
		{name: "a timestamp before the floor epoch is refused", session: openCodeSession{ID: "ses_n", Title: ghostSessionTitle, Updated: sessionTimestampFloor.Add(-time.Millisecond).UnixMilli()}, grace: 0, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selectCleanupSessions([]openCodeSession{tc.session}, tc.grace, now)
			if tc.want && len(got) != 1 {
				t.Errorf("selected %d, want the session eligible", len(got))
			}
			if !tc.want && len(got) != 0 {
				t.Errorf("selected %v, want refused", got)
			}
		})
	}
}

// TestPlausibleSessionTime pins the bounds that keep a wrong timestamp unit
// from turning --apply into a mass delete (#588): the value is read as Unix
// milliseconds on the CLI's word alone, so anything outside a sane window is
// refused instead of being read as "very old".
func TestPlausibleSessionTime(t *testing.T) {
	now := time.Unix(1_800_000_000, 0) // 2027-01-15, after the floor
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{name: "a session from a day ago", at: now.Add(-24 * time.Hour), want: true},
		{name: "exactly the floor epoch", at: sessionTimestampFloor, want: true},
		{name: "one tick before the floor epoch", at: sessionTimestampFloor.Add(-time.Millisecond), want: false},
		{name: "the absent-timestamp sentinel", at: time.UnixMilli(0), want: false},
		{name: "a 1970 instant, which a seconds-unit value produces", at: time.UnixMilli(1_799_992_800), want: false},
		{name: "now itself", at: now, want: true},
		{name: "exactly the future skew allowance", at: now.Add(sessionFutureSkew), want: true},
		{name: "one tick past the future skew allowance", at: now.Add(sessionFutureSkew + time.Millisecond), want: false},
		{name: "a nanosecond-unit value, which lands millennia out", at: time.UnixMilli(now.Add(-time.Hour).UnixNano()), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := plausibleSessionTime(tc.at, now); got != tc.want {
				t.Errorf("plausibleSessionTime(%v, %v) = %v, want %v", tc.at, now, got, tc.want)
			}
		})
	}
}

// TestCleanupOpenCodeSessions_TempStaysInsideGhostScratchRoot: opencode
// writes a ~5.7 MiB JIT-cache object into TMPDIR on every invocation, and
// this run can spawn thousands of children — left on the shared system temp
// that is exactly the debris #542 documents. Every child must instead see the
// run's Ghost-owned scratch directory as its temp.
func TestCleanupOpenCodeSessions_TempStaysInsideGhostScratchRoot(t *testing.T) {
	now := time.Now()
	dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t,
		session("ses_old", ghostSessionTitle, 3*time.Hour, 2*time.Hour, now),
	))
	inherited := t.TempDir()
	t.Setenv("TMPDIR", inherited)
	t.Setenv("TMP", inherited)
	t.Setenv("TEMP", inherited)
	root := os.Getenv("GHOST_SCRATCH_DIR")

	var out bytes.Buffer
	if _, err := CleanupOpenCodeSessions(context.Background(), "opencode",
		OpenCodeCleanupOptions{Grace: time.Hour, Now: now}, &out); err != nil {
		t.Fatalf("CleanupOpenCodeSessions: %v", err)
	}
	recorded := fakeLines(t, filepath.Join(dir, "tmpdir.log"))
	if len(recorded) == 0 {
		t.Fatal("fake binary never recorded its TMPDIR")
	}
	for _, tmp := range recorded {
		if !strings.HasPrefix(tmp, root+string(os.PathSeparator)) {
			t.Errorf("child TMPDIR = %q, want it inside the scratch root %q", tmp, root)
		}
		if tmp == inherited {
			t.Errorf("child TMPDIR = %q, want the run's scratch dir instead of the inherited temp", tmp)
		}
	}
}

// TestCleanupOpenCodeSessions_InvalidOptionsAreRejected: a negative grace or
// limit is a mistake that would silently widen the deletion, so it fails
// before any subprocess runs.
func TestCleanupOpenCodeSessions_InvalidOptionsAreRejected(t *testing.T) {
	for name, opts := range map[string]OpenCodeCleanupOptions{
		"negative grace": {Grace: -time.Second},
		"negative limit": {Limit: -1},
	} {
		t.Run(name, func(t *testing.T) {
			dir := fakeOpenCodeSessionCLI(t, sessionsFixture(t))
			var out bytes.Buffer
			if _, err := CleanupOpenCodeSessions(context.Background(), "opencode", opts, &out); err == nil {
				t.Fatal("expected an error")
			}
			if calls := fakeCalls(t, dir); len(calls) != 0 {
				t.Errorf("invoked %v before rejecting the options", calls)
			}
		})
	}
}
