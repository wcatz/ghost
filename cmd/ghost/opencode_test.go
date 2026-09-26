package main

import (
	"strings"
	"testing"
	"time"
)

// TestParseCleanupSessionsArgs pins the flag surface: the grace period
// defaults to an hour, everything is report-only until --apply, and any
// argument the parser does not recognize is an error rather than something
// silently ignored (a dropped --grace would widen a deletion).
func TestParseCleanupSessionsArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		apply   bool
		grace   time.Duration
		limit   int
		wantErr string
	}{
		{name: "defaults", args: nil, grace: time.Hour},
		{name: "apply", args: []string{"--apply"}, apply: true, grace: time.Hour},
		{name: "grace with separate value", args: []string{"--grace", "30m"}, grace: 30 * time.Minute},
		{name: "grace with equals value", args: []string{"--grace=24h"}, grace: 24 * time.Hour},
		{name: "zero grace", args: []string{"--grace", "0s"}, grace: 0},
		{name: "limit with separate value", args: []string{"--limit", "5"}, grace: time.Hour, limit: 5},
		{name: "limit with equals value", args: []string{"--limit=5"}, grace: time.Hour, limit: 5},
		{name: "combined", args: []string{"--grace", "2h", "--apply", "--limit", "10"}, apply: true, grace: 2 * time.Hour, limit: 10},
		{name: "unknown argument", args: []string{"--force"}, grace: time.Hour, wantErr: "unknown argument"},
		{name: "positional junk", args: []string{"sessions"}, grace: time.Hour, wantErr: "unknown argument"},
		{name: "grace without value", args: []string{"--grace"}, grace: time.Hour, wantErr: "--grace needs a duration"},
		{name: "grace unparseable", args: []string{"--grace", "tomorrow"}, grace: time.Hour, wantErr: "invalid --grace"},
		{name: "grace negative", args: []string{"--grace", "-1h"}, grace: time.Hour, wantErr: "must not be negative"},
		{name: "limit without value", args: []string{"--limit"}, grace: time.Hour, wantErr: "--limit needs a session count"},
		{name: "limit unparseable", args: []string{"--limit", "many"}, grace: time.Hour, wantErr: "invalid --limit"},
		{name: "limit negative", args: []string{"--limit", "-3"}, grace: time.Hour, wantErr: "must not be negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts, err := parseCleanupSessionsArgs(tc.args)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected an error containing %q, got options %+v", tc.wantErr, opts)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
				}
				// An unrecognized argument is the one case where the reader
				// needs to be shown what the command accepts; the value
				// errors name the flag and the accepted form themselves.
				if tc.wantErr == "unknown argument" && !strings.Contains(err.Error(), "ghost opencode cleanup-sessions") {
					t.Errorf("error = %q, want it to carry the usage line", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("parseCleanupSessionsArgs: %v", err)
			}
			if opts.Apply != tc.apply || opts.Grace != tc.grace || opts.Limit != tc.limit {
				t.Errorf("Apply=%v Grace=%s Limit=%d, want Apply=%v Grace=%s Limit=%d",
					opts.Apply, opts.Grace, opts.Limit, tc.apply, tc.grace, tc.limit)
			}
		})
	}
}
