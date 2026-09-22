package ai

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

func TestSourceFromScriptPath(t *testing.T) {
	tests := []struct {
		name string
		path string
		want string
	}{
		{"npm-installed codex", "/home/ada/.local/lib/node_modules/@openai/codex/bin/codex.js", "codex"},
		{"npm-installed claude-code", "/home/ada/.local/lib/node_modules/@anthropic-ai/claude-code/cli.js", "claude-code"},
		{"native binary", "/usr/local/bin/opencode", "opencode"},
		{"unknown script", "/usr/local/bin/some-tool.js", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sourceFromScriptPath(tt.path); got != tt.want {
				t.Errorf("sourceFromScriptPath(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}

// fakeProc is one ancestor served by fakePS. ppid is the raw `ps -o ppid=`
// output so tests can inject malformed values; command is the raw
// `ps -o command=` output, empty meaning the process vanished before it could
// be read.
type fakeProc struct {
	ppid    string
	comm    string
	command string
}

// fakePS serves a pid -> fakeProc table for the three ps invocations
// detectSourceFromPS makes. A pid absent from the table fails like ps does for
// a process that has already exited; a command request with no recorded
// command fails the same way.
func fakePS(t *testing.T, procs map[int]fakeProc) func(name string, args ...string) ([]byte, error) {
	t.Helper()
	return func(name string, args ...string) ([]byte, error) {
		if name != "ps" || len(args) != 4 || args[0] != "-o" || args[2] != "-p" {
			t.Fatalf("unexpected ps invocation: %s %v", name, args)
		}
		pid, err := strconv.Atoi(args[3])
		if err != nil {
			t.Fatalf("bad -p argument %q: %v", args[3], err)
		}
		proc, ok := procs[pid]
		if !ok {
			return nil, fmt.Errorf("ps: process %d not found", pid)
		}
		switch args[1] {
		case "ppid=":
			return []byte(proc.ppid + "\n"), nil
		case "comm=":
			return []byte(proc.comm + "\n"), nil
		case "command=":
			if proc.command == "" {
				return nil, fmt.Errorf("ps: command unavailable for %d", pid)
			}
			return []byte(proc.command + "\n"), nil
		}
		t.Fatalf("unexpected ps format %q", args[1])
		return nil, nil
	}
}

func TestDetectSourceFromPS(t *testing.T) {
	t.Run("codex script path ancestor", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "1", comm: "node", command: "node /home/ada/.local/lib/node_modules/@openai/codex/bin/codex.js"},
		})
		if got := detectSourceFromPS(run, 500); got != "codex" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "codex")
		}
	})

	t.Run("node script path ancestor", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "300", comm: "node", command: "node /home/ada/.local/lib/node_modules/@openai/codex/bin/codex.js"},
			300: {ppid: "1", comm: "bash", command: "bash"},
		})
		if got := detectSourceFromPS(run, 500); got != "codex" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "codex")
		}
	})

	t.Run("runtime flag before script", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "1", comm: "node", command: "node --experimental /usr/local/bin/opencode"},
		})
		if got := detectSourceFromPS(run, 500); got != "opencode" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "opencode")
		}
	})

	t.Run("later argument naming a harness is not the script", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "1", comm: "node", command: "node deploy.js /usr/local/bin/codex.js"},
		})
		if got := detectSourceFromPS(run, 500); got != "" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
		}
	})

	t.Run("walk continues past a non-harness script", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "300", comm: "node", command: "node deploy.js"},
			300: {ppid: "1", comm: "opencode", command: "opencode"},
		})
		if got := detectSourceFromPS(run, 500); got != "opencode" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "opencode")
		}
	})

	t.Run("native comm ancestor", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "300", comm: "claude", command: "claude"},
			300: {ppid: "1", comm: "bash", command: "bash"},
		})
		if got := detectSourceFromPS(run, 500); got != "claude-code" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "claude-code")
		}
	})

	// Regression pin: a non-JS-runtime command mentioning a harness path must
	// not be classified as that harness.
	t.Run("bash command mentioning codex path is not codex", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "1", comm: "bash", command: "bash /home/ada/codex/deploy.sh"},
		})
		if got := detectSourceFromPS(run, 500); got != "" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
		}
	})

	t.Run("plain bash chain", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "bash", command: "bash"},
			400: {ppid: "300", comm: "bash", command: "bash"},
			300: {ppid: "1", comm: "bash", command: "bash"},
		})
		if got := detectSourceFromPS(run, 500); got != "" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
		}
	})

	t.Run("self-parent stops the walk", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "500", comm: "bash", command: "bash"},
		})
		if got := detectSourceFromPS(run, 500); got != "" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
		}
	})

	t.Run("malformed output", func(t *testing.T) {
		for name, proc := range map[string]fakeProc{
			"empty ppid": {ppid: "", comm: "node"},
			"bad ppid":   {ppid: "not-a-pid", comm: "node"},
		} {
			t.Run(name, func(t *testing.T) {
				run := fakePS(t, map[int]fakeProc{500: proc})
				if got := detectSourceFromPS(run, 500); got != "" {
					t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
				}
			})
		}
	})

	t.Run("js runtime with unreadable command", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{
			500: {ppid: "400", comm: "ghost", command: "/usr/local/bin/ghost supersede"},
			400: {ppid: "1", comm: "node", command: ""},
		})
		if got := detectSourceFromPS(run, 500); got != "" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
		}
	})

	t.Run("exited process stops the walk", func(t *testing.T) {
		run := fakePS(t, map[int]fakeProc{})
		if got := detectSourceFromPS(run, 500); got != "" {
			t.Errorf("detectSourceFromPS() = %q, want %q", got, "")
		}
	})
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

	t.Run("npm-installed codex script path", func(t *testing.T) {
		root := t.TempDir()
		writeProcEntry(t, root, 30, 20, "ghost", "")
		writeProcEntry(t, root, 20, 1, "node", "node\x00/home/ada/.local/lib/node_modules/@openai/codex/bin/codex.js\x00")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 30); got != "codex" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "codex")
		}
	})

	t.Run("runtime flag before script is skipped", func(t *testing.T) {
		root := t.TempDir()
		writeProcEntry(t, root, 30, 20, "ghost", "")
		writeProcEntry(t, root, 20, 1, "node", "node\x00--experimental\x00/usr/local/bin/opencode\x00")
		writeProcEntry(t, root, 1, 0, "systemd", "")
		if got := detectSourceFromProc(root, 30); got != "opencode" {
			t.Errorf("detectSourceFromProc() = %q, want %q", got, "opencode")
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
