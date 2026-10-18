package mcpinit

import (
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// pluginSessionDirName returns a name of the shape the opencode plugin's
// mkdtemp gives its session directory: its prefix followed by the six
// alphanumeric characters every runtime's mkdtemp substitutes for XXXXXX.
func pluginSessionDirName(t *testing.T) string {
	t.Helper()
	const alnum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("rand: %v", err)
	}
	for i := range b {
		b[i] = alnum[int(b[i])%len(alnum)]
	}
	return opencodeSessionDirPrefix + string(b)
}

// sweepTempBase points every temp variable the sweep consults at a fresh
// directory inside t.TempDir(), so nothing a case plants can be in the real
// temp directory and the case's base is the only one the hook recognises.
func sweepTempBase(t *testing.T) string {
	t.Helper()
	isolatedHome(t)
	base := filepath.Join(t.TempDir(), "hosttmp")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatalf("create host temp dir: %v", err)
	}
	unsetEnvForTest(t, "TMP")
	unsetEnvForTest(t, "TEMP")
	t.Setenv("TMPDIR", base)
	return base
}

// plantPluginSessionDir lays out exactly what the plugin writes — the mkdtemp
// directory holding one messages.jsonl — under base, and returns the directory.
func plantPluginSessionDir(t *testing.T, base string) string {
	t.Helper()
	return sweepableGhostDir(t, base, pluginSessionDirName(t))
}

// sweepWithFormat drives one stop through the hook for source/format and the
// given rollout path, the way the deferred cleanup runs in production.
func sweepWithFormat(t *testing.T, source, format, transcriptPath string) {
	t.Helper()
	input := contractInputFor(t, "Stop", source, format,
		fmt.Sprintf(`{"session_id":"sweep-guard","transcript_path":%q,"cwd":"/repo","stop_hook_active":false}`, transcriptPath))
	var out strings.Builder
	RunHostEvent("stop", source, strings.NewReader(input), &out, &strings.Builder{})
}

// treeUnder lists every path under root (relative), so a case can assert that
// nothing it planted was removed.
func treeUnder(t *testing.T, root string) []string {
	t.Helper()
	var paths []string
	err := filepath.Walk(root, func(p string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		paths = append(paths, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(paths)
	return paths
}

// assertNothingRemoved fails when any path present before the hook ran is gone
// after it. New entries are allowed (the hook may write its own sidecar).
func assertNothingRemoved(t *testing.T, root string, before []string) {
	t.Helper()
	for _, rel := range before {
		if _, err := os.Lstat(filepath.Join(root, rel)); err != nil {
			t.Errorf("the sweep removed %q, which the host did not materialise for this session: %v", rel, err)
		}
	}
}

// TestSweepRemovesTheDirectoryThePluginMaterialised: the one directory shape
// the plugin creates is still removed, for both plugin formats.
func TestSweepRemovesTheDirectoryThePluginMaterialised(t *testing.T) {
	for _, format := range []string{"opencode-messages", "opencode-v2-messages"} {
		t.Run(format, func(t *testing.T) {
			base := sweepTempBase(t)
			dir := plantPluginSessionDir(t, base)
			sweepWithFormat(t, "opencode", format, filepath.Join(dir, opencodeTranscriptName))
			if _, err := os.Lstat(dir); !os.IsNotExist(err) {
				t.Errorf("the plugin's session directory was not swept: %v", err)
			}
		})
	}
}

// TestSweepLeavesEverythingTheHostDidNotMaterialise holds the blast radius: a
// payload path pointing into any other entry of the temp directory removes
// nothing. Each case after the issue's own three fails exactly one check, so a
// mutation that drops one check fails a case of its own.
func TestSweepLeavesEverythingTheHostDidNotMaterialise(t *testing.T) {
	writeFile := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	type plant func(t *testing.T, base string) (transcriptPath string)
	cases := []struct {
		name   string
		source string
		format string
		plant  plant
	}{
		{"a pending audit sidecar file, codex", "codex", "codex-rollout", func(t *testing.T, base string) string {
			p := filepath.Join(base, "ghost-audit-111111.signals")
			writeFile(t, p, "ghost-audit v5\nid AAAA 1\n")
			return filepath.Join(p, "rollout.jsonl")
		}},
		{"a pending audit sidecar file, opencode", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			p := filepath.Join(base, "ghost-audit-111111.signals")
			writeFile(t, p, "ghost-audit v5\nid AAAA 1\n")
			return filepath.Join(p, opencodeTranscriptName)
		}},
		{"another session's directory, codex", "codex", "codex-rollout", func(t *testing.T, base string) string {
			d := filepath.Join(base, "ghost-otherSession")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(d, "transcript.jsonl"), "t\n")
			return filepath.Join(d, "rollout.jsonl")
		}},
		{"another session's directory, opencode", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			d := filepath.Join(base, "ghost-otherSession")
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(d, opencodeTranscriptName), lineUser+"\n")
			return filepath.Join(d, opencodeTranscriptName)
		}},
		{"a harness scratch directory", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			// The same call internal/ai/scratch.go makes for its fall-back,
			// holding a file of the plugin's name so only the name differs.
			d, err := os.MkdirTemp(base, "ghost-reflect-")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(d, opencodeTranscriptName), lineUser+"\n")
			return filepath.Join(d, opencodeTranscriptName)
		}},
		{"a bench temp directory", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			// The same call internal/bench/passive.go makes for one run,
			// holding a file of the plugin's name so only the name differs.
			d, err := os.MkdirTemp(base, "ghost-bench-passive-")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(d, opencodeTranscriptName), lineUser+"\n")
			return filepath.Join(d, opencodeTranscriptName)
		}},
		{"a codex payload into a plugin-shaped directory", "codex", "codex-rollout", func(t *testing.T, base string) string {
			// codex hands over its own rollout file and nothing materialises
			// one in the temp directory, so a codex payload never sweeps.
			return filepath.Join(plantPluginSessionDir(t, base), opencodeTranscriptName)
		}},
		{"a plugin-named regular file", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			p := filepath.Join(base, pluginSessionDirName(t))
			writeFile(t, p, "not a directory\n")
			return filepath.Join(p, opencodeTranscriptName)
		}},
		{"a plugin-named symlink to a plugin-shaped directory", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			elsewhere := filepath.Join(filepath.Dir(base), "elsewhere")
			if err := os.MkdirAll(elsewhere, 0o700); err != nil {
				t.Fatal(err)
			}
			target := plantPluginSessionDir(t, elsewhere)
			link := filepath.Join(base, pluginSessionDirName(t))
			if err := os.Symlink(target, link); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			return filepath.Join(link, opencodeTranscriptName)
		}},
		{"a plugin-shaped directory holding another file", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			d := plantPluginSessionDir(t, base)
			writeFile(t, filepath.Join(d, "keep.txt"), "mine\n")
			return filepath.Join(d, opencodeTranscriptName)
		}},
		{"a plugin-shaped directory named by another file", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			d := plantPluginSessionDir(t, base)
			writeFile(t, filepath.Join(d, "other.jsonl"), lineUser+"\n")
			// The payload names the second file, so the layout check is the
			// one that refuses; the name check alone would also refuse it.
			return filepath.Join(d, "other.jsonl")
		}},
		{"a plugin-shaped directory whose file is a symlink", "opencode", "opencode-messages", func(t *testing.T, base string) string {
			d := filepath.Join(base, pluginSessionDirName(t))
			if err := os.Mkdir(d, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(base), "elsewhere.jsonl")
			writeFile(t, target, lineUser+"\n")
			if err := os.Symlink(target, filepath.Join(d, opencodeTranscriptName)); err != nil {
				t.Skipf("symlink unavailable: %v", err)
			}
			return filepath.Join(d, opencodeTranscriptName)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			base := sweepTempBase(t)
			root := filepath.Dir(base)
			transcriptPath := tc.plant(t, base)
			before := treeUnder(t, root)
			sweepWithFormat(t, tc.source, tc.format, transcriptPath)
			assertNothingRemoved(t, root, before)
		})
	}
}

// TestSweepNamesMatchThePluginSource couples the Go constants to the plugin's
// own naming code: every mkdtemp the embedded plugin makes uses the prefix the
// sweep matches, and names the file the sweep expects. A plugin that renamed
// either would otherwise leak its directories without any test noticing.
func TestSweepNamesMatchThePluginSource(t *testing.T) {
	re := regexp.MustCompile(`mkdtemp\(join\(tmpdir\(\), "([^"]*)"\)\)\s*\n\s*transcriptPath = join\(dir, "([^"]*)"\)`)
	matches := re.FindAllStringSubmatch(opencodeGhostPluginTS, -1)
	if got := strings.Count(opencodeGhostPluginTS, "mkdtemp("); got != len(matches) || got == 0 {
		t.Fatalf("plugin has %d mkdtemp call(s), %d in the materialisation shape the sweep knows", got, len(matches))
	}
	for _, m := range matches {
		if m[1] != opencodeSessionDirPrefix {
			t.Errorf("plugin mkdtemp prefix %q, sweep matches %q", m[1], opencodeSessionDirPrefix)
		}
		if m[2] != opencodeTranscriptName {
			t.Errorf("plugin writes %q, sweep expects %q", m[2], opencodeTranscriptName)
		}
	}
}

// TestSweepNameMatchesARealRuntimeMkdtemp runs the plugin's own mkdtemp call
// under each JavaScript runtime installed here (opencode runs on bun; node is
// the reference implementation) and asserts the sweep accepts the name it
// produced. Skips when neither runtime is on PATH.
func TestSweepNameMatchesARealRuntimeMkdtemp(t *testing.T) {
	ran := false
	for _, rt := range []string{"bun", "node"} {
		bin, err := exec.LookPath(rt)
		if err != nil {
			continue
		}
		ran = true
		t.Run(rt, func(t *testing.T) {
			base := t.TempDir()
			script := fmt.Sprintf(`const fs=require("node:fs"),p=require("node:path");for(let i=0;i<20;i++)console.log(p.basename(fs.mkdtempSync(p.join(process.argv[process.argv.length-1], %q))))`, opencodeSessionDirPrefix)
			cmd := exec.Command(bin, "-e", script, base)
			cmd.Env = append(os.Environ(), "TMPDIR="+base)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s mkdtemp: %v", rt, err)
			}
			names := strings.Fields(string(out))
			if len(names) != 20 {
				t.Fatalf("%s printed %d names, want 20: %q", rt, len(names), out)
			}
			for _, name := range names {
				if !isPluginSessionDirName(name) {
					t.Errorf("%s mkdtemp produced %q, which the sweep does not recognise", rt, name)
				}
			}
		})
	}
	if !ran {
		t.Skip("neither bun nor node is on PATH")
	}
}
