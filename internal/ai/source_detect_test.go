package ai

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestDetectSourceFromEnv(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"opencode", map[string]string{"OPENCODE": "1"}, "opencode"},
		{"opencode pid", map[string]string{"OPENCODE_PID": "4242"}, ""},
		{"claudecode", map[string]string{"CLAUDECODE": "1"}, "claude-code"},
		{"claude entrypoint", map[string]string{"CLAUDE_CODE_ENTRYPOINT": "cli"}, "claude-code"},
		{"opencode wins over claude", map[string]string{"OPENCODE": "1", "CLAUDECODE": "1"}, "opencode"},
		{"empty values are not markers", map[string]string{"OPENCODE": "", "CLAUDECODE": ""}, ""},
		{"nothing set", nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookup := func(key string) string { return tt.env[key] }
			if got := detectSourceFromEnv(lookup); got != tt.want {
				t.Errorf("detectSourceFromEnv() = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeProcEntry creates a fake /proc/<pid> directory: stat with ppid in field
// 4 (after state) and a comm file. cmdline is empty unless provided.
func writeProcEntry(t *testing.T, root string, pid, ppid int, comm, cmdline string) {
	t.Helper()
	dir := filepath.Join(root, fmt.Sprintf("%d", pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	stat := fmt.Sprintf("%d (%s) S %d\n", pid, comm, ppid)
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
		t.Fatalf("write stat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm+"\n"), 0o644); err != nil {
		t.Fatalf("write comm: %v", err)
	}
	if cmdline != "" {
		if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(cmdline), 0o644); err != nil {
			t.Fatalf("write cmdline: %v", err)
		}
	}
}

func TestDetectSourceFromProc(t *testing.T) {
	t.Run("finds harness ancestor", func(t *testing.T) {
		root := t.TempDir()
		// ghost (500) <- bash (400) <- opencode (300) <- init (1)
		writeProcEntry(t, root, 500, 400, "ghost", "")
		writeProcEntry(t, root, 400, 300, "bash", "")
		writeProcEntry(t, root, 300, 1, "opencode", "")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 500); got != "opencode" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "opencode")
		}
	})

	t.Run("claude ancestor maps to claude-code", func(t *testing.T) {
		root := t.TempDir()
		writeProcEntry(t, root, 20, 10, "ghost", "")
		writeProcEntry(t, root, 10, 1, "claude", "")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 20); got != "claude-code" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "claude-code")
		}
	})

	t.Run("js runtime checks the script argument", func(t *testing.T) {
		root := t.TempDir()
		writeProcEntry(t, root, 30, 20, "ghost", "")
		writeProcEntry(t, root, 20, 1, "node", "node\x00/usr/local/bin/opencode\x00")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 30); got != "opencode" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "opencode")
		}
	})

	t.Run("nodejs runtime is a js runtime", func(t *testing.T) {
		root := t.TempDir()
		writeProcEntry(t, root, 30, 20, "ghost", "")
		writeProcEntry(t, root, 20, 1, "nodejs", "nodejs\x00/usr/local/bin/opencode\x00")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 30); got != "opencode" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "opencode")
		}
	})

	t.Run("no harness ancestor", func(t *testing.T) {
		root := t.TempDir()
		writeProcEntry(t, root, 50, 40, "ghost", "")
		writeProcEntry(t, root, 40, 30, "bash", "")
		writeProcEntry(t, root, 30, 20, "node", "/usr/local/bin/some-tool\x00")
		writeProcEntry(t, root, 20, 1, "konsole", "")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 50); got != "" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "")
		}
	})

	t.Run("stops at missing stat", func(t *testing.T) {
		root := t.TempDir()
		// 60 has no stat file at all.
		if got := detectSourceFromProc(root, 60); got != "" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "")
		}
	})
}
