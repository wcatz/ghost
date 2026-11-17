package main

import (
	"strings"
	"testing"
)

func TestParseReplayArgsReadsTheReportAndRankModes(t *testing.T) {
	opts, err := parseReplayArgs([]string{"--snapshot", "snap.db", "--baseline", "/old/ghost", "--limit", "7", "--top-k", "4", "--max-queries", "12"})
	if err != nil {
		t.Fatalf("parseReplayArgs: %v", err)
	}
	if opts.snapshot != "snap.db" || opts.baseline != "/old/ghost" {
		t.Errorf("snapshot/baseline = %q/%q", opts.snapshot, opts.baseline)
	}
	if opts.limit != 7 || opts.topK != 4 || opts.maxQueries != 12 {
		t.Errorf("limits = %+v", opts)
	}
	if opts.rank {
		t.Error("report mode was read as rank mode")
	}

	rank, err := parseReplayArgs([]string{"--rank", "--snapshot=snap.db", "--record=q.json"})
	if err != nil {
		t.Fatalf("parseReplayArgs(rank): %v", err)
	}
	if !rank.rank || rank.record != "q.json" || rank.snapshot != "snap.db" {
		t.Errorf("rank options = %+v", rank)
	}
}

func TestParseReplayArgsRefusesMistakesBeforeOpeningAnything(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no snapshot", nil, "--snapshot"},
		{"unknown flag", []string{"--snapshot", "s", "--wat"}, "--wat"},
		{"empty snapshot", []string{"--snapshot", ""}, "--snapshot requires a value"},
		{"bad number", []string{"--snapshot", "s", "--top-k", "x"}, "--top-k"},
		{"duplicate", []string{"--snapshot", "s", "--snapshot", "t"}, "--snapshot"},
		{"rank without record", []string{"--rank", "--snapshot", "s"}, "--record"},
		{"record in report mode", []string{"--snapshot", "s", "--record", "q"}, "--record"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseReplayArgs(tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("parseReplayArgs(%v) = %v, want a refusal naming %q", tc.args, err, tc.want)
			}
		})
	}
}

func TestReplayUsageNamesEveryFlag(t *testing.T) {
	for _, flag := range []string{"--snapshot", "--baseline", "--rank", "--record", "--limit", "--top-k", "--max-queries"} {
		if !strings.Contains(replayUsage, flag) {
			t.Errorf("replayUsage does not mention %s, so the mode is undiscoverable", flag)
		}
	}
}
