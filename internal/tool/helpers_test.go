package tool

import (
	"testing"
)

func TestIsBlockedCleanArg(t *testing.T) {
	tests := []struct {
		name string
		arg  string
		want bool
	}{
		// Blocked: short flags containing 'f'
		{name: "-f is blocked", arg: "-f", want: true},
		{name: "-fd is blocked", arg: "-fd", want: true},
		{name: "-xf is blocked", arg: "-xf", want: true},
		{name: "-fdx is blocked", arg: "-fdx", want: true},
		{name: "-xfd is blocked", arg: "-xfd", want: true},

		// Not blocked: long flags (-- prefix) are not checked by this function
		{name: "--force is not blocked", arg: "--force", want: false},
		{name: "--force-clean is not blocked", arg: "--force-clean", want: false},

		// Not blocked: short flags without 'f'
		{name: "-n is not blocked", arg: "-n", want: false},
		{name: "-d is not blocked", arg: "-d", want: false},
		{name: "-x is not blocked", arg: "-x", want: false},
		{name: "-dx is not blocked", arg: "-dx", want: false},

		// Not blocked: bare words (no dash prefix)
		{name: "bare f is not blocked", arg: "f", want: false},
		{name: "bare word is not blocked", arg: "file.txt", want: false},

		// Edge cases
		{name: "empty string is not blocked", arg: "", want: false},
		{name: "single dash is not blocked", arg: "-", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isBlockedCleanArg(tt.arg)
			if got != tt.want {
				t.Errorf("isBlockedCleanArg(%q) = %v, want %v", tt.arg, got, tt.want)
			}
		})
	}
}

func TestReadOnlyGitCmds(t *testing.T) {
	// Verify known read-only commands are in the map.
	readOnly := []string{"status", "diff", "log", "show", "branch", "tag", "remote", "rev-parse", "ls-files", "ls-tree", "blame", "shortlog"}
	for _, cmd := range readOnly {
		if !readOnlyGitCmds[cmd] {
			t.Errorf("expected %q to be in readOnlyGitCmds", cmd)
		}
	}

	// Verify known write commands are NOT in the map.
	writeOps := []string{"add", "commit", "push", "reset", "clean", "checkout", "stash", "merge", "rebase"}
	for _, cmd := range writeOps {
		if readOnlyGitCmds[cmd] {
			t.Errorf("expected %q to NOT be in readOnlyGitCmds", cmd)
		}
	}
}

func TestBlockedGitOps(t *testing.T) {
	tests := []struct {
		name       string
		subcommand string
		wantFlags  []string
	}{
		{name: "push has --force and -f", subcommand: "push", wantFlags: []string{"--force", "-f"}},
		{name: "reset has --hard", subcommand: "reset", wantFlags: []string{"--hard"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			flags, ok := blockedGitOps[tt.subcommand]
			if !ok {
				t.Fatalf("expected %q to be in blockedGitOps", tt.subcommand)
			}
			if len(flags) != len(tt.wantFlags) {
				t.Fatalf("blockedGitOps[%q] has %d flags, want %d", tt.subcommand, len(flags), len(tt.wantFlags))
			}
			for i, flag := range flags {
				if flag != tt.wantFlags[i] {
					t.Errorf("blockedGitOps[%q][%d] = %q, want %q", tt.subcommand, i, flag, tt.wantFlags[i])
				}
			}
		})
	}

	// Verify non-destructive commands are not blocked.
	safe := []string{"add", "commit", "checkout", "stash"}
	for _, cmd := range safe {
		if _, ok := blockedGitOps[cmd]; ok {
			t.Errorf("expected %q to NOT be in blockedGitOps", cmd)
		}
	}
}

func TestBuildGrepArgs(t *testing.T) {
	tests := []struct {
		name       string
		in         grepInput
		searchPath string
		maxResults int
		want       []string
	}{
		{
			name:       "basic pattern only",
			in:         grepInput{Pattern: "TODO"},
			searchPath: "/project",
			maxResults: 50,
			want:       []string{"-n", "--no-heading", "--color=never", "-m50", "-e", "TODO", "--", "/project"},
		},
		{
			name:       "with context lines",
			in:         grepInput{Pattern: "func main", Context: 3},
			searchPath: "/project",
			maxResults: 50,
			want:       []string{"-n", "--no-heading", "--color=never", "-m50", "-C3", "-e", "func main", "--", "/project"},
		},
		{
			name:       "with glob filter",
			in:         grepInput{Pattern: "import", Glob: "*.go"},
			searchPath: "/project",
			maxResults: 25,
			want:       []string{"-n", "--no-heading", "--color=never", "-m25", "--glob", "*.go", "-e", "import", "--", "/project"},
		},
		{
			name:       "with context and glob",
			in:         grepInput{Pattern: "error", Context: 2, Glob: "*.ts"},
			searchPath: "/app",
			maxResults: 10,
			want:       []string{"-n", "--no-heading", "--color=never", "-m10", "-C2", "--glob", "*.ts", "-e", "error", "--", "/app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildGrepArgs(tt.in, tt.searchPath, tt.maxResults)
			if len(got) != len(tt.want) {
				t.Fatalf("buildGrepArgs() returned %d args, want %d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("buildGrepArgs()[%d] = %q, want %q\nfull got:  %v\nfull want: %v", i, got[i], tt.want[i], got, tt.want)
				}
			}
		})
	}
}

func TestBuildGrepFallbackArgs(t *testing.T) {
	tests := []struct {
		name       string
		in         grepInput
		searchPath string
		maxResults int
		want       []string
	}{
		{
			name:       "basic pattern only",
			in:         grepInput{Pattern: "TODO"},
			searchPath: "/project",
			maxResults: 50,
			want:       []string{"-rn", "--color=never", "-m50", "-e", "TODO", "--", "/project"},
		},
		{
			name:       "with context lines",
			in:         grepInput{Pattern: "func main", Context: 3},
			searchPath: "/project",
			maxResults: 50,
			want:       []string{"-rn", "--color=never", "-m50", "-C3", "-e", "func main", "--", "/project"},
		},
		{
			name:       "with glob filter uses --include",
			in:         grepInput{Pattern: "import", Glob: "*.go"},
			searchPath: "/project",
			maxResults: 25,
			want:       []string{"-rn", "--color=never", "-m25", "--include", "*.go", "-e", "import", "--", "/project"},
		},
		{
			name:       "with context and glob",
			in:         grepInput{Pattern: "error", Context: 2, Glob: "*.ts"},
			searchPath: "/app",
			maxResults: 10,
			want:       []string{"-rn", "--color=never", "-m10", "-C2", "--include", "*.ts", "-e", "error", "--", "/app"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildGrepFallbackArgs(tt.in, tt.searchPath, tt.maxResults)
			if len(got) != len(tt.want) {
				t.Fatalf("buildGrepFallbackArgs() returned %d args, want %d\ngot:  %v\nwant: %v", len(got), len(tt.want), got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("buildGrepFallbackArgs()[%d] = %q, want %q\nfull got:  %v\nfull want: %v", i, got[i], tt.want[i], got, tt.want)
				}
			}
		})
	}
}

