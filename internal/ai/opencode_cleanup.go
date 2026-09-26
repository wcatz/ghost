package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/scratch"
)

// This file implements the one-shot backlog cleanup behind
// `ghost opencode cleanup-sessions` (#588): the sessions titled exactly
// "[ghost]" that lifecycle runs created before #568 pointed the child's data
// dir at an invocation-owned scratch tree.
//
// Unlike OpenCodeClient's children, this child deliberately does NOT get an
// invocation-owned home/config/data tree — the whole point is to address the
// user's real session store, the same one their own `opencode session list`
// shows. What it keeps is the shared harness environment allowlist
// (harnessEnv): PATH/HOME/XDG are enough for the CLI, no memory text or other
// untrusted content ever reaches the child (the arguments are session ids the
// selection already vetted), and the working directory stays the caller's —
// `opencode session list` is scoped to the current project, so moving the
// child would report on an empty one. The one thing the children do not
// inherit is the shared system temp: the run opens a single Ghost-owned
// scratch directory and pins TMPDIR/TMP/TEMP there, so opencode's ~5.7 MiB
// JIT-cache object is written once for the whole run instead of once per
// child, and is removed (or reaped) with that directory rather than left in
// /tmp.

// ghostSessionTitle is the exact title every lifecycle run passes to
// `opencode run --title`. Cleanup selects on this string and nothing else, so
// it is defined once and used by both the writer and the cleaner.
const ghostSessionTitle = "[ghost]"

const (
	// defaultSessionListLimit is how many sessions `session list` is asked
	// for when the caller passes no --limit. The CLI's own default is 100,
	// which would silently truncate a real pre-#568 backlog (measured in the
	// thousands); the list call is one process either way, so asking for far
	// more than any plausible backlog costs only the parse.
	defaultSessionListLimit = 20000

	// sessionCommandTimeout bounds one opencode invocation: the list, and
	// every delete attempt. A stalled CLI must not hang the command with no
	// output, and the bound keeps a hanging delete inside the retry budget.
	sessionCommandTimeout = 60 * time.Second

	// sessionDeleteAttempts is the total number of tries for one
	// `session delete` — the first attempt plus its retries. Deletion is
	// best-effort, so exhausting them is a reported failure, not an abort.
	sessionDeleteAttempts = 3

	// defaultSessionDeleteRetryDelay is the base of the linear backoff
	// between delete attempts: attempt n waits n-1 times this value.
	defaultSessionDeleteRetryDelay = 250 * time.Millisecond

	// sessionProgressEvery is how many processed sessions pass between
	// progress lines during a long --apply run (thousands of deletions at
	// one CLI process each take a while, and silence looks like a hang).
	sessionProgressEvery = 50

	// sessionFutureSkew is how far ahead of now a session timestamp may sit
	// and still be believed: a small allowance for clock skew between the
	// machine writing the store and the one reading it. Beyond it the value
	// is a unit problem (Unix nanoseconds land far in the future) or a clock
	// problem, and neither justifies a delete.
	sessionFutureSkew = 5 * time.Minute
)

// sessionTimestampFloor is the earliest instant a session timestamp may name
// and still be believed: 2020-01-01T00:00:00Z, well before any session this
// command targets (the backlog starts in 2026) and well after every unit a
// wrong reading can produce. Timestamps are read as Unix milliseconds on the
// CLI's word alone — nothing else in the payload states the unit — so a value
// in seconds would still be positive, map into 1970, and be older than every
// cutoff: every titled session, including one written seconds ago, would look
// eligible and --apply would delete the lot. An instant this far in the past
// is a unit problem, not an old session, so it is refused rather than acted
// on. time.Date is not a constant, hence var; nothing assigns to it.
var sessionTimestampFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// OpenCodeCleanupOptions configures one cleanup run.
type OpenCodeCleanupOptions struct {
	// Grace is how old a session must be before it is eligible, measured
	// from its most recent activity (created or updated, whichever is
	// later) — age from creation alone would call a session old while it is
	// still being written to. Zero means "any age". Negative is rejected.
	Grace time.Duration

	// Apply performs the deletes. Without it the run only lists and counts.
	Apply bool

	// Limit is how many sessions `session list` is asked for; zero uses
	// defaultSessionListLimit. Negative is rejected.
	Limit int

	// Now is the reference time for the grace period; zero means time.Now().
	// Tests pin it so eligibility is not a race with the clock.
	Now time.Time

	// RetryDelay is the base of the backoff between delete attempts; zero
	// uses defaultSessionDeleteRetryDelay. Negative is rejected.
	RetryDelay time.Duration
}

// OpenCodeCleanupResult reports what one cleanup run did.
type OpenCodeCleanupResult struct {
	Listed   int // sessions `session list` returned
	Eligible int // exact-title matches older than the grace period
	Deleted  int // sessions `session delete` confirmed
	Failed   int // eligible sessions whose deletion never succeeded
}

// openCodeSession is the subset of one `opencode session list --format json`
// object the cleanup reads. Fields the CLI adds beyond these are ignored, and
// a row missing one of them fails closed: no title means no match, no
// timestamp means no provable age.
type openCodeSession struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
}

// lastActivity returns the newest of the session's timestamps as Unix
// milliseconds.
func (s openCodeSession) lastActivity() int64 {
	if s.Updated > s.Created {
		return s.Updated
	}
	return s.Created
}

// CleanupOpenCodeSessions lists the current project's OpenCode sessions and,
// with opts.Apply, deletes the ones titled exactly "[ghost]" that are older
// than opts.Grace. It is report-only by default: the count it prints is what
// --apply would remove.
//
// Every refusal is decided up front in selectCleanupSessions, so a session
// that is not eligible is never named in a delete — and one failed delete is
// counted and reported while the run continues with the rest. Fatal problems
// (an unusable list, unparseable output, a rejected option) return an error
// before anything is deleted.
//
// out receives the whole human report: the summary, progress, per-session
// failure lines, and the final deleted/failed counts.
func CleanupOpenCodeSessions(ctx context.Context, binary string, opts OpenCodeCleanupOptions, out io.Writer) (OpenCodeCleanupResult, error) {
	var res OpenCodeCleanupResult
	if opts.Grace < 0 {
		return res, fmt.Errorf("grace period must not be negative (got %s)", opts.Grace)
	}
	if opts.Limit < 0 {
		return res, fmt.Errorf("limit must not be negative (got %d)", opts.Limit)
	}
	if opts.RetryDelay < 0 {
		return res, fmt.Errorf("retry delay must not be negative (got %s)", opts.RetryDelay)
	}
	if binary == "" {
		binary = "opencode"
	}
	limit := opts.Limit
	if limit == 0 {
		limit = defaultSessionListLimit
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	// One Ghost-owned temp dir for the whole run: see the file's opening
	// comment. Opened before the first child so the list call already runs
	// there, released when the run ends (or reaped if this process dies).
	tempRoot, release := openCleanupTempRoot()
	defer release()
	runner := openCodeCleanupRunner{binary: binary, tempRoot: tempRoot}

	sessions, err := runner.listSessions(ctx, limit)
	if err != nil {
		return res, err
	}
	eligible := selectCleanupSessions(sessions, opts.Grace, now)
	res = OpenCodeCleanupResult{Listed: len(sessions), Eligible: len(eligible)}

	if _, err := fmt.Fprintf(out, "listed %d opencode session(s); %d titled %q older than %s\n",
		res.Listed, res.Eligible, ghostSessionTitle, opts.Grace); err != nil {
		return res, err
	}
	if res.Listed >= limit {
		if _, err := fmt.Fprintf(out, "warning: the list reached the --limit of %d sessions and may be truncated; re-run with a higher --limit if you expect more\n", limit); err != nil {
			return res, err
		}
	}
	if !opts.Apply {
		_, err := fmt.Fprintf(out, "dry run: nothing deleted — re-run with --apply to delete %d session(s)\n", res.Eligible)
		return res, err
	}

	delay := opts.RetryDelay
	if delay == 0 {
		delay = defaultSessionDeleteRetryDelay
	}
	for i, s := range eligible {
		if err := runner.deleteSession(ctx, s.ID, delay); err != nil {
			res.Failed++
			if _, err := fmt.Fprintf(out, "failed: %s: %v\n", s.ID, err); err != nil {
				return res, err
			}
		} else {
			res.Deleted++
		}
		if (i+1)%sessionProgressEvery == 0 && i+1 < len(eligible) {
			if _, err := fmt.Fprintf(out, "progress: %d/%d processed (deleted %d, failed %d)\n",
				i+1, len(eligible), res.Deleted, res.Failed); err != nil {
				return res, err
			}
		}
	}
	if _, err := fmt.Fprintf(out, "done: deleted %d, failed %d\n", res.Deleted, res.Failed); err != nil {
		return res, err
	}
	return res, nil
}

// selectCleanupSessions picks the sessions a run may delete. Each condition
// is a refusal rather than a preference: the title must be exactly
// ghostSessionTitle, the id must be usable as a plain argument, the session
// must carry a timestamp that proves it is strictly older than the grace
// period and that is itself an instant worth believing, and nothing else
// about a session can widen the selection.
func selectCleanupSessions(sessions []openCodeSession, grace time.Duration, now time.Time) []openCodeSession {
	cutoff := now.Add(-grace)
	out := make([]openCodeSession, 0, len(sessions))
	for _, s := range sessions {
		if s.Title != ghostSessionTitle {
			continue
		}
		// An id is forwarded as an argument to `session delete`, so one that
		// is empty or flag-shaped could be read as a flag instead of a
		// target. Neither is deletable as intended; refuse both.
		if s.ID == "" || strings.HasPrefix(s.ID, "-") {
			continue
		}
		activity := s.lastActivity()
		if activity <= 0 {
			continue // no provable age, so no provable passage of the grace period
		}
		at := time.UnixMilli(activity)
		if !plausibleSessionTime(at, now) {
			continue
		}
		if at.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out
}

// plausibleSessionTime reports whether at, a timestamp read as Unix
// milliseconds, is an instant this command can believe: no earlier than
// sessionTimestampFloor and no later than now plus sessionFutureSkew.
//
// The cutoff already excludes anything at or after now (grace is never
// negative), so today the future half only restates that invariant — it is
// spelled out anyway because the floor is worthless on its own: the unit
// assumption behind both bounds is the same one, and a change to how the
// cutoff is derived must not silently reopen it. Failing closed here costs a
// skipped session; believing the wrong unit costs every one of them.
func plausibleSessionTime(at, now time.Time) bool {
	if at.Before(sessionTimestampFloor) {
		return false
	}
	return !at.After(now.Add(sessionFutureSkew))
}

// openCodeCleanupRunner spawns the cleanup's opencode children: one resolved
// binary and one temp root shared by every child of the run.
type openCodeCleanupRunner struct {
	binary   string
	tempRoot string // empty when the scratch root was unusable
}

// openCleanupTempRoot opens the run's temp root under Ghost's owned scratch
// root and returns it with its release. An unusable scratch root is a WARN,
// not an error — the children then keep the inherited temp dir, which is what
// they would have used before this bookkeeping existed. Unlike harnessCommand
// this does not call scratch.EnforceBudget: that check opens the Ghost
// database to record itself, and this one-shot command opens at most a
// single directory that Release always removes, so keeping it database-free
// is worth more than one more budget row.
func openCleanupTempRoot() (string, func()) {
	dir, err := scratch.Open()
	if err != nil {
		slog.Warn("opencode cleanup: scratch dir unavailable; children keep the inherited temp dir", "error", err)
		return "", func() {}
	}
	return dir.Path(), dir.Release
}

// listSessions runs `opencode session list --format json` and parses it.
// Output that does not parse is an error, never an empty list: a garbled list
// must not read as a clean run.
func (r openCodeCleanupRunner) listSessions(ctx context.Context, limit int) ([]openCodeSession, error) {
	raw, err := r.run(ctx, []string{"session", "list", "--format", "json", "--max-count", strconv.Itoa(limit)})
	if err != nil {
		return nil, err
	}
	var sessions []openCodeSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		return nil, fmt.Errorf("opencode session list: unparseable output: %w", err)
	}
	return sessions, nil
}

// deleteSession runs `opencode session delete <id>` with a bounded number of
// attempts and a linear backoff between them. The last error is returned so
// the caller can count and report it; the run continues either way.
func (r openCodeCleanupRunner) deleteSession(ctx context.Context, id string, delay time.Duration) error {
	var last error
	for attempt := 1; attempt <= sessionDeleteAttempts; attempt++ {
		if attempt > 1 {
			if err := sleepContext(ctx, delay*time.Duration(attempt-1)); err != nil {
				return fmt.Errorf("interrupted before retry: %w", err)
			}
		}
		_, err := r.run(ctx, []string{"session", "delete", id})
		if err == nil {
			return nil
		}
		last = err
	}
	return fmt.Errorf("no success after %d attempts: %w", sessionDeleteAttempts, last)
}

// run executes one opencode subcommand with the shared harness environment
// allowlist (so the child reaches the same store and service the user's own
// CLI does, without inheriting unrelated credentials), the run's temp root
// pinned in place of the shared system temp, and a bounded timeout, returning
// its stdout.
func (r openCodeCleanupRunner) run(ctx context.Context, args []string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, sessionCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.binary, args...)
	cmd.Env = harnessEnv(os.Environ(), harnessOpencode)
	if r.tempRoot != "" {
		cmd.Env = scratchEnv(cmd.Env, r.tempRoot)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("opencode %s: timed out after %s", strings.Join(args, " "), sessionCommandTimeout)
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return nil, fmt.Errorf("opencode %s: %w", strings.Join(args, " "), err)
		}
		return nil, fmt.Errorf("opencode %s: %w: %s", strings.Join(args, " "), err, firstRunes(detail, 512))
	}
	return stdout.Bytes(), nil
}

// firstRunes trims s to at most n runes, so a chatty failing CLI cannot turn
// one error line into a wall of text.
func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

// sleepContext waits for d or until ctx is done, whichever comes first.
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
