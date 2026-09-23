package mcpinit

import (
	"archive/zip"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// entryPoint returns the executable hooks.json names for the current OS:
// the POSIX launcher, or ghost.exe on native Windows.
func entryPoint(t *testing.T, tree string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		return filepath.Join(tree, "bin", "ghost.exe")
	}
	return filepath.Join(tree, "bin", "ghost-launcher")
}

// hookWiring is what hooks.json declares for one event.
type hookWiring struct {
	Command string   // e.g. ${CLAUDE_PLUGIN_ROOT}/bin/ghost-launcher
	Args    []string // e.g. [hook session-start --source claude-code]
}

// loadHookWiring parses hooks/hooks.json from an installed tree for the
// SessionStart and Stop events; it fails the test if the file is missing or
// malformed (a zip that drops the wiring must not pass).
func loadHookWiring(t *testing.T, tree string) map[string]hookWiring {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(tree, "hooks", "hooks.json"))
	if err != nil {
		t.Fatalf("installed tree is missing hooks/hooks.json: %v", err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Type    string   `json:"type"`
				Command string   `json:"command"`
				Args    []string `json:"args"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse hooks/hooks.json: %v", err)
	}
	w := map[string]hookWiring{}
	for _, event := range []string{"SessionStart", "Stop"} {
		entries := doc.Hooks[event]
		if len(entries) == 0 || len(entries[0].Hooks) == 0 {
			t.Fatalf("hooks/hooks.json declares no %s hook", event)
		}
		h := entries[0].Hooks[0]
		if h.Type != "command" {
			t.Fatalf("%s hook type = %q, want command", event, h.Type)
		}
		w[event] = hookWiring{Command: h.Command, Args: h.Args}
	}
	return w
}

// executable resolves the declared command against the install root.
func (w hookWiring) executable(tree string) string {
	return filepath.Join(tree, filepath.FromSlash(strings.TrimPrefix(w.Command, "${CLAUDE_PLUGIN_ROOT}/")))
}

// writeRootZip zips src with entries rooted at the archive top level — the
// layout release.yml produces with `(cd dir && zip -qr ../x.zip .)`.
func writeRootZip(src, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	walkErr := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		name := filepath.ToSlash(rel)
		if d.IsDir() {
			_, err := zw.Create(name + "/")
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(info.Mode())
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		srcf, err := os.Open(p)
		if err != nil {
			return err
		}
		defer srcf.Close()
		_, err = io.Copy(w, srcf)
		return err
	})
	if walkErr != nil {
		_ = zw.Close()
		return walkErr
	}
	return zw.Close()
}

// extractRootZip extracts an archive written by writeRootZip. Names come from
// our own archive, so no foreign-path guard is needed.
func extractRootZip(src, dst string) error {
	r, err := zip.OpenReader(src)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, h := range r.File {
		p := filepath.Join(dst, filepath.FromSlash(h.Name))
		if h.FileInfo().IsDir() || strings.HasSuffix(h.Name, "/") {
			if err := os.MkdirAll(p, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		rc, err := h.Open()
		if err != nil {
			return err
		}
		mode := h.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			_ = rc.Close()
			return err
		}
		if _, err := io.Copy(f, rc); err != nil {
			_ = rc.Close()
			_ = f.Close()
			return err
		}
		if err := rc.Close(); err != nil {
			_ = f.Close()
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// zipRoundTrip zips the assembled tree and extracts it fresh: everything the
// e2e test executes comes from the extracted, installable layout.
func zipRoundTrip(t *testing.T, tree string) string {
	t.Helper()
	zipPath := filepath.Join(t.TempDir(), "ghost-plugin.zip")
	if err := writeRootZip(tree, zipPath); err != nil {
		t.Fatalf("zip assembled tree: %v", err)
	}
	extract := filepath.Join(t.TempDir(), "extracted")
	if err := extractRootZip(zipPath, extract); err != nil {
		t.Fatalf("extract plugin zip: %v", err)
	}
	return extract
}

// insertE2EMemory adds the marker memory the session-start assertion looks
// for, in the project seedProject created.
func insertE2EMemory(t *testing.T, dataHome string) {
	t.Helper()
	db, err := memory.OpenDB(filepath.Join(dataHome, "ghost", "ghost.db"))
	if err != nil {
		t.Fatalf("open seeded db: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(`INSERT INTO memories (id, project_id, category, content, source, importance)
		VALUES ('e2emem01', 'e2e-proj', 'fact', 'E2E_SEED_MARKER context from disk', 'manual', 0.99)`); err != nil {
		t.Fatalf("insert e2e memory: %v", err)
	}
}

// runE2EHook runs the installed tree's hook exactly as hooks.json wires it —
// executable and argv both come from the parsed wiring, with plugin env
// pointing at the tree. home isolates ~/.claude; dataDir is shared across
// runs so the finalize marker persists.
func runE2EHook(t *testing.T, tree string, w hookWiring, dataDir, home, stdin string) (stdout, stderr string) {
	t.Helper()
	// Bound the hook so a hang fails cleanly instead of tripping the
	// package-level go-test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, w.executable(tree), w.Args...)
	cmd.Env = append(os.Environ(),
		"CLAUDE_PLUGIN_ROOT="+tree,
		"CLAUDE_PLUGIN_DATA="+dataDir,
		"HOME="+home,
		"USERPROFILE="+home,
	)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("hook %q failed: %v\nstderr: %s", strings.Join(w.Args, " "), err, errb.String())
	}
	return out.String(), errb.String()
}

// TestPluginE2E drives an installed plugin tree end to end: zip round-trip,
// MCP stdio handshake, both lifecycle hooks, and mcp init deferral. It runs
// on every platform's CI (linux proof) and on the windows-latest /
// windows-11-arm legs (native ghost.exe proof).
func TestPluginE2E(t *testing.T) {
	requireBash(t)

	// Assemble the host's tree, then execute only from the zip round-trip.
	var tree string
	if runtime.GOOS == "windows" {
		tree = runAssembleWindows(t, runtime.GOARCH)
	} else {
		tree = runAssemblePOSIX(t, runtime.GOOS+"-"+runtime.GOARCH)
	}
	installed := zipRoundTrip(t, tree)

	// The installable layout must carry its wiring and manifest: loadHookWiring
	// fails if hooks/hooks.json is missing/malformed/missing an event, and
	// manifestName fails on a missing, malformed, or nameless manifest.
	wiring := loadHookWiring(t, installed)
	if name := manifestName(t, installed); name == "" {
		t.Fatalf("installed manifest has an empty name")
	}
	exe := entryPoint(t, installed)
	info, err := os.Stat(exe)
	if err != nil {
		t.Fatalf("entry point missing after round-trip: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode()&0o111 == 0 {
		t.Fatalf("entry point lost its exec bit through the round-trip: mode %v", info.Mode())
	}
	// Pin the ${CLAUDE_PLUGIN_ROOT}/bin/... template in hooks.json to the path
	// this test execs: if they disagree, the subprocess is not proof of the
	// shipped wiring.
	for _, event := range []string{"SessionStart", "Stop"} {
		if got, want := wiring[event].executable(installed), exe; got != want {
			t.Fatalf("%s hook command resolves to %q, but the test execs %q", event, got, want)
		}
	}

	// Hermetic home + data dir, one seeded project whose path is the cwd the
	// session-start payload will report.
	dataHome := isolatedHome(t)
	home := filepath.Dir(dataHome)
	rawCwd := t.TempDir()
	cwd := rawCwd
	if resolved, err := filepath.EvalSymlinks(rawCwd); err == nil {
		cwd = resolved
	}
	seedProject(t, dataHome, "e2e-proj", cwd, "e2e-project")
	insertE2EMemory(t, dataHome)

	t.Run("session-start finalizes and injects context", func(t *testing.T) {
		dataDir := t.TempDir() // fresh CLAUDE_PLUGIN_DATA: marker must be created
		payload := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e","cwd":%q,"source":"startup"}`, cwd)

		out1, err1 := runE2EHook(t, installed, wiring["SessionStart"], dataDir, home, payload)
		if !strings.Contains(err1, "finalizing") {
			t.Errorf("first run should finalize on stderr, got %q", err1)
		}
		if strings.Contains(out1, "finalizing") || strings.Contains(out1, "ghost plugin finalize") {
			t.Errorf("finalize banner leaked to stdout: %q", out1)
		}
		if !strings.Contains(out1, "E2E_SEED_MARKER") {
			t.Errorf("expected seeded context injected on stdout, got %q", out1)
		}
		if _, err := os.Stat(filepath.Join(dataDir, finalizeMarkerName)); err != nil {
			t.Errorf("finalize marker not written: %v", err)
		}

		out2, err2 := runE2EHook(t, installed, wiring["SessionStart"], dataDir, home, payload)
		if strings.Contains(err2, "finalizing") {
			t.Errorf("marker must skip finalize on the second run, got %q", err2)
		}
		if !strings.Contains(out2, "E2E_SEED_MARKER") {
			t.Errorf("second run should still inject context, got %q", out2)
		}
	})

	t.Run("stop nudges an unsaved session", func(t *testing.T) {
		transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
		body := lineUser + "\n" + lineToolBash + "\n" + lineText + "\n"
		if err := os.WriteFile(transcript, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		payload := fmt.Sprintf(`{"hook_event_name":"Stop","session_id":"e2e","transcript_path":%q,"cwd":%q,"stop_hook_active":false}`, transcript, cwd)
		out, errOut := runE2EHook(t, installed, wiring["Stop"], t.TempDir(), home, payload)
		if !strings.Contains(out, "ghost_memory_save") {
			t.Errorf("expected save nudge on stdout, got %q", out)
		}
		if errOut != "" {
			t.Errorf("stop hook should be silent on stderr, got %q", errOut)
		}
	})

	t.Run("mcp stdio handshake", func(t *testing.T) {
		cmd := exec.Command(entryPoint(t, installed), "mcp")
		// Neutralize the plugin env so the MCP path is exercised exactly like
		// a standalone install (uniform with the deferral subtest); appended
		// keys win under exec's last-wins env semantics.
		cmd.Env = append(os.Environ(), "HOME="+home, "USERPROFILE="+home, "CLAUDE_PLUGIN_ROOT=", "CLAUDE_PLUGIN_DATA=")
		stdin, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		var errb strings.Builder
		cmd.Stderr = &errb
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		}()

		send := func(v any) {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshal request: %v", err)
			}
			if _, err := stdin.Write(append(b, '\n')); err != nil {
				t.Fatalf("write to mcp stdin: %v", err)
			}
		}
		send(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "initialize",
			"params": map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{},
				"clientInfo":      map[string]any{"name": "ghost-e2e", "version": "0"},
			},
		})

		lines := make(chan string, 16)
		// Non-blocking drain: trailing lines after the loop (or Fatalf paths)
		// must not strand the reader goroutine; the drain exits when the
		// reader closes the channel.
		defer func() {
			go func() {
				for range lines {
				}
			}()
		}()
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				lines <- sc.Text()
			}
			close(lines)
		}()
		timeout := time.After(30 * time.Second)
		gotInit, gotTools := false, false
		for !gotTools {
			select {
			case line, ok := <-lines:
				if !ok {
					t.Fatalf("mcp stdout ended early (init=%v tools=%v); stderr: %s", gotInit, gotTools, errb.String())
				}
				var msg struct {
					ID     *float64        `json:"id"`
					Result json.RawMessage `json:"result"`
				}
				if err := json.Unmarshal([]byte(line), &msg); err != nil || msg.ID == nil {
					continue // notification or non-JSON log line
				}
				switch int(*msg.ID) {
				case 1:
					if len(msg.Result) == 0 {
						t.Fatalf("initialize returned an error response: %s", line)
					}
					gotInit = true
					send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
					send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
				case 2:
					var r struct {
						Tools []struct {
							Name string `json:"name"`
						} `json:"tools"`
					}
					if err := json.Unmarshal(msg.Result, &r); err != nil {
						t.Fatalf("parse tools/list result: %v", err)
					}
					found := false
					for _, tl := range r.Tools {
						if tl.Name == "ghost_memory_save" {
							found = true
						}
					}
					if !found {
						t.Errorf("tools/list lacks ghost_memory_save: %s", msg.Result)
					}
					gotTools = true
				}
			case <-timeout:
				t.Fatalf("timed out waiting for MCP responses (init=%v); stderr: %s", gotInit, errb.String())
			}
		}

		_ = stdin.Close() // server should exit on stdin EOF
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("mcp server exited non-zero: %v; stderr: %s", err, errb.String())
			}
		case <-time.After(15 * time.Second):
			t.Error("mcp server did not exit after stdin close")
		}
	})

	t.Run("mcp init defers to an installed plugin", func(t *testing.T) {
		for _, key := range []string{"ghost@ghost", "ghost-windows-amd64@ghost"} {
			t.Run(key, func(t *testing.T) {
				h := t.TempDir()
				t.Setenv("HOME", h)
				t.Setenv("USERPROFILE", h)
				t.Setenv(pluginRootEnv, "")
				reg := filepath.Join(h, ".claude", "plugins")
				if err := os.MkdirAll(reg, 0o755); err != nil {
					t.Fatal(err)
				}
				doc := `{"version":2,"plugins":{"` + key + `":[]}}`
				if err := os.WriteFile(filepath.Join(reg, "installed_plugins.json"), []byte(doc), 0o644); err != nil {
					t.Fatal(err)
				}
				var buf strings.Builder
				if err := Run(&buf, false); err != nil {
					t.Fatalf("Run: %v", err)
				}
				if !strings.Contains(buf.String(), "plugin manages this integration") {
					t.Errorf("expected the defer message, got %q", buf.String())
				}
				if _, err := os.Stat(filepath.Join(h, ".claude.json")); err == nil {
					t.Error("init must not write ~/.claude.json while deferring")
				}
			})
		}
	})
}
