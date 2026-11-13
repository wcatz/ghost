package main

import (
	"strings"
	"testing"
)

// TestParseMCPClientRejectsUnknownArguments pins that parseMCPClient refuses
// anything it does not recognize, including the misspelled forms that used to
// be silently dropped, while still accepting both --client spellings.
func TestParseMCPClientRejectsUnknownArguments(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr bool
	}{
		{name: "dry-run equals form", args: []string{"--dry-run=true"}, wantErr: true},
		{name: "dryrun", args: []string{"--dryrun"}, wantErr: true},
		{name: "misspelled client flag", args: []string{"--clien"}, wantErr: true},
		{name: "bare client name", args: []string{"codex"}, wantErr: true},
		{name: "client separate", args: []string{"--client", "codex"}, wantErr: false},
		{name: "client equals", args: []string{"--client=codex"}, wantErr: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseMCPClient(tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseMCPClient(%v) = nil, want an error", tc.args)
				}
				if !strings.Contains(err.Error(), "unknown") {
					t.Errorf("error = %q, want it to name an unknown argument", err)
				}
			} else if err != nil {
				t.Fatalf("parseMCPClient(%v) = %v, want no error", tc.args, err)
			}
		})
	}
}
