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

const ghostSessionTitle = "[ghost]"

const (
	defaultSessionListLimit = 20000

	sessionDeleteAttempts = 3

	defaultSessionDeleteRetryDelay = 250 * time.Millisecond

	sessionProgressEvery = 50

	sessionFutureSkew = 5 * time.Minute
)

var sessionCommandTimeout = 60 * time.Second

var sessionTimestampFloor = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// OpenCodeCleanupOptions configures one cleanup run.
type OpenCodeCleanupOptions struct {
	Grace time.Duration

	Apply bool

	Limit int

	Now time.Time

	RetryDelay time.Duration
}

// OpenCodeCleanupResult reports what one cleanup run did.
type OpenCodeCleanupResult struct {
	Listed      int
	Eligible    int
	Implausible int
	Deleted     int
	Failed      int
}

type openCodeSession struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Created int64  `json:"created"`
	Updated int64  `json:"updated"`
}

func (s openCodeSession) lastActivity() int64 {
	if s.Updated > s.Created {
		return s.Updated
	}
	return s.Created
}

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

	tempRoot, release := openCleanupTempRoot()
	defer release()
	runner := openCodeCleanupRunner{binary: binary, tempRoot: tempRoot}

	sessions, err := runner.listSessions(ctx, limit)
	if err != nil {
		return res, err
	}
	eligible, implausible := selectCleanupSessions(sessions, opts.Grace, now)
	res = OpenCodeCleanupResult{Listed: len(sessions), Eligible: len(eligible), Implausible: implausible}

	if _, err := fmt.Fprintf(out, "listed %d opencode session(s); %d titled %q older than %s\n",
		res.Listed, res.Eligible, ghostSessionTitle, opts.Grace); err != nil {
		return res, err
	}

	if res.Implausible > 0 {
		if _, err := fmt.Fprintf(out, "warning: %d titled session(s) had a missing or out-of-window timestamp (outside [%s, now+%s]) and were skipped — opencode may have changed the timestamp unit or the session JSON shape; run GHOST_LIVE_TESTS=1 go test ./internal/ai/ -run TestOpenCodeSessionList_TimestampsAreMilliseconds to check before trusting this count\n",
			res.Implausible, sessionTimestampFloor.Format(time.RFC3339), sessionFutureSkew); err != nil {
			return res, err
		}
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

func selectCleanupSessions(sessions []openCodeSession, grace time.Duration, now time.Time) ([]openCodeSession, int) {
	cutoff := now.Add(-grace)
	out := make([]openCodeSession, 0, len(sessions))
	implausible := 0
	for _, s := range sessions {
		if s.Title != ghostSessionTitle {
			continue
		}

		if s.ID == "" || strings.HasPrefix(s.ID, "-") {
			continue
		}
		activity := s.lastActivity()
		at := time.UnixMilli(activity)

		if !plausibleSessionTime(at, now) {
			implausible++
			continue
		}
		if at.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out, implausible
}

func plausibleSessionTime(at, now time.Time) bool {
	if at.Before(sessionTimestampFloor) {
		return false
	}
	return !at.After(now.Add(sessionFutureSkew))
}

type openCodeCleanupRunner struct {
	binary   string
	tempRoot string
}

func openCleanupTempRoot() (string, func()) {
	dir, err := scratch.Open()
	if err != nil {
		slog.Warn("opencode cleanup: scratch dir unavailable; children keep the inherited temp dir", "error", err)
		return "", func() {}
	}
	return dir.Path(), dir.Release
}

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

func (r openCodeCleanupRunner) run(ctx context.Context, args []string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, sessionCommandTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.binary, args...)

	boundHarnessProcess(cmd)
	cmd.Env = harnessEnv(os.Environ(), harnessOpencode)
	if r.tempRoot != "" {
		cmd.Env = scratchEnv(cmd.Env, r.tempRoot)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	ctxErr := cctx.Err()
	sweepHarnessProcessGroup(cmd)
	if err != nil && ctxErr == context.DeadlineExceeded {
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

func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}

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
