package main

import (
	"strings"
	"testing"
)

// TestContextSourceFlag: --source names the host the block is for, so the block
// can leave out what that host already loaded; a block without it is unchanged.
func TestContextSourceFlag(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{nil, ""},
		{[]string{"--cwd", "/x"}, ""},
		{[]string{"--cwd", "/x", "--source", "opencode"}, "opencode"},
		{[]string{"--source=claude-code"}, "claude-code"},
	} {
		if _, _, err := parseContextBlockArgs(tc.argv); err != nil {
			t.Errorf("parseContextBlockArgs(%q) = %v, want no error", tc.argv, err)
			continue
		}
		got, err := contextSource(tc.argv)
		if err != nil || string(got) != tc.want {
			t.Errorf("contextSource(%q) = (%q, %v), want %q", tc.argv, got, err, tc.want)
		}
	}
	for _, argv := range [][]string{
		{"--source"},
		{"--source", "nosuchhost"},
		{"--source", "opencode", "--source", "opencode"},
	} {
		_, _, perr := parseContextBlockArgs(argv)
		_, serr := contextSource(argv)
		if perr == nil && serr == nil {
			t.Errorf("%q was accepted, want a refusal", argv)
		} else if err := firstErr(perr, serr); !strings.Contains(err.Error(), "--source") {
			t.Errorf("%q refusal %q does not name --source", argv, err)
		}
	}
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
