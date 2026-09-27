//go:build unix

package mcpinit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The user-owned files ghost rewrites outside the Claude settings.json and the
// codex config.toml pair — codex hooks.json, the opencode plugin, the goose
// package and the MEMORY.md redirect — are the half of #551 that #621 left
// open. Every one of them is rewritten with the whole document ghost wants, so
// an interrupted write is not a cosmetic diff: whatever host reads the file
// next sees a truncated config, and a user hook that was in it is gone with no
// copy anywhere.
//
// Two properties per file pin that none of them goes through a bare
// os.WriteFile, which opens the target for writing and truncates it before the
// first byte of the new content lands.
//
//   - read-only target: the target is never opened O_WRONLY, so the repair
//     lands by rename and the target keeps its exact mode. The mode is the
//     observable; the property behind it is that a refused or failed write
//     cannot leave a half-written file. This is the same contract
//     TestWriteFileAtomicReplacesReadOnlyFile pins for codex config.toml.
//   - open reader keeps its snapshot: a reader that already holds the file open
//     still reads the complete previous document after the install. A
//     truncate-in-place write makes that handle read the new document, or a
//     prefix of it, instead. Rename is what keeps an in-flight reader
//     consistent, which is the case a codex or goose process starting at the
//     same moment as `ghost mcp init` is in.
//
// Unix only: on Windows os.Chmod(0400) sets FILE_ATTRIBUTE_READONLY, which
// blocks the rename that replaces the file, and Go opens files there without
// FILE_SHARE_DELETE, so a rename cannot replace a file a reader holds open.
// Neither property is observable on that platform.

// seedUserFile writes content at path, locked read-only, and returns the bytes
// so a test can compare them against what an in-flight reader sees.
func seedUserFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	// MkdirAll/WriteFile are subject to the umask, so the mode is set
	// explicitly: the test needs a target the process genuinely cannot open
	// for writing, not one that happens to have come out read-only.
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
}

// holdReader opens path for reading and returns the handle, standing in for a
// host process that is reading its own config while init runs.
func holdReader(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// assertStillSnapshot fails when reader no longer holds the document that was
// at path when the reader opened it.
func assertStillSnapshot(t *testing.T, reader *os.File, want string) {
	t.Helper()
	var seen bytes.Buffer
	if _, err := seen.ReadFrom(reader); err != nil {
		t.Fatalf("read through the handle opened before the install: %v", err)
	}
	if seen.String() != want {
		t.Errorf("a reader holding the file open across the install must still read the whole previous document.\nwant:\n%s\ngot:\n%s", want, seen.String())
	}
}

// codexHooksSeed is a hooks.json a user wrote: their own description and their
// own SessionStart rule, which the merge must carry across untouched.
const codexHooksSeed = `{
  "description": "my personal hooks",
  "hooks": {
    "SessionStart": [
      {"hooks": [{"type": "command", "command": "/usr/bin/env fortune"}]}
    ]
  }
}`

func TestInstallCodexHooks_WritesAtomically(t *testing.T) {
	t.Run("read-only target", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
		path := codexHooksJSON(home)
		seedUserFile(t, path, codexHooksSeed)

		var out bytes.Buffer
		changed, err := installCodexHooks(&out, ghostBin, false)
		if err != nil {
			t.Fatalf("installCodexHooks over a read-only hooks.json: %v", err)
		}
		if !changed {
			t.Error("installCodexHooks reported no change, so the drift was not repaired")
		}

		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "/usr/bin/env fortune") {
			t.Errorf("the user's own SessionStart rule must survive the repair:\n%s", data)
		}
		if !strings.Contains(string(data), "hook session-start --source codex") {
			t.Errorf("ghost's own rule must be wired in:\n%s", data)
		}
		assertFileMode(t, path, 0400)
	})

	t.Run("open reader keeps its snapshot", func(t *testing.T) {
		home, _ := setupCodexTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
		path := codexHooksJSON(home)
		seedUserFile(t, path, codexHooksSeed)
		reader := holdReader(t, path)

		var out bytes.Buffer
		if _, err := installCodexHooks(&out, ghostBin, false); err != nil {
			t.Fatalf("installCodexHooks: %v", err)
		}
		assertStillSnapshot(t, reader, codexHooksSeed)
	})
}

// staleOpencodePluginSeed stands in for a plugin file ghost is replacing
// because it no longer matches the embedded source.
const staleOpencodePluginSeed = "// an older ghost-opencode.ts, left over from a previous release\n"

func TestInstallOpencodePlugin_WritesAtomically(t *testing.T) {
	t.Run("read-only target", func(t *testing.T) {
		home, xdg := setupOpencodeTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
		path := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")
		seedUserFile(t, path, staleOpencodePluginSeed)

		var out bytes.Buffer
		changed, err := installOpencodePlugin(&out, ghostBin, false)
		if err != nil {
			t.Fatalf("installOpencodePlugin over a read-only plugin file: %v", err)
		}
		if !changed {
			t.Error("installOpencodePlugin reported no change, so the stale plugin was not replaced")
		}
		assertFileContent(t, path, renderOpencodeGhostPlugin(ghostBin))
		assertFileMode(t, path, 0400)
	})

	t.Run("open reader keeps its snapshot", func(t *testing.T) {
		home, xdg := setupOpencodeTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
		path := filepath.Join(xdg, "opencode", "plugins", "ghost-opencode.ts")
		seedUserFile(t, path, staleOpencodePluginSeed)
		reader := holdReader(t, path)

		var out bytes.Buffer
		if _, err := installOpencodePlugin(&out, ghostBin, false); err != nil {
			t.Fatalf("installOpencodePlugin: %v", err)
		}
		assertStillSnapshot(t, reader, staleOpencodePluginSeed)
	})
}

func TestInstallGoosePackage_WritesAtomically(t *testing.T) {
	// Spelled out rather than ranged over goosePackageFiles, so the test cannot
	// pass vacuously if that list ever comes back empty or short.
	gooseFiles := func(ghostBin string) []struct{ rel, want string } {
		return []struct{ rel, want string }{
			{"plugin.json", renderGoosePluginManifest()},
			{"mcp.json", renderGooseMCPConfig(ghostBin)},
			{filepath.Join("hooks", "hooks.json"), renderGooseHooksConfig(ghostBin)},
		}
	}

	t.Run("read-only target", func(t *testing.T) {
		home, _ := setupGooseTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
		files := gooseFiles(ghostBin)
		for _, f := range files {
			seedUserFile(t, goosePackagePath(home, f.rel), "stale\n")
		}

		var out bytes.Buffer
		if _, err := installGoosePackage(&out, ghostBin, false); err != nil {
			t.Fatalf("installGoosePackage over read-only package files: %v", err)
		}
		for _, f := range files {
			assertFileContent(t, goosePackagePath(home, f.rel), f.want)
			assertFileMode(t, goosePackagePath(home, f.rel), 0400)
		}
	})

	t.Run("open reader keeps its snapshot", func(t *testing.T) {
		home, _ := setupGooseTestEnv(t)
		ghostBin := stubPath(filepath.Join(home, "bin"), "ghost")
		path := goosePackagePath(home, "mcp.json")
		seedUserFile(t, path, "stale\n")
		reader := holdReader(t, path)

		var out bytes.Buffer
		if _, err := installGoosePackage(&out, ghostBin, false); err != nil {
			t.Fatalf("installGoosePackage: %v", err)
		}
		assertStillSnapshot(t, reader, "stale\n")
	})
}

// staleRedirectSeed is an older ghost redirect. It has to carry both
// "stored in Ghost" and a tool call to ghost_list_projects: a file with the
// first but not the second is already the current wording and is left alone,
// and a file with neither belongs to the user, so only this shape reaches the
// write that refreshes ghost's own redirect.
const staleRedirectSeed = "# Myproject Project Memory\n\nAll project knowledge is stored in Ghost.\nCall `ghost_list_projects` to see what is known.\n"

func TestWriteRedirects_WritesAtomically(t *testing.T) {
	projectPath := absProjectPath(t)
	projects := []projectInfo{{ID: "abc123", Path: projectPath, Name: "myproject"}}

	t.Run("read-only target", func(t *testing.T) {
		home := t.TempDir()
		setHome(t, home)
		target := filepath.Join(home, ".claude", "projects", projectSlug(projectPath), "memory", "MEMORY.md")
		seedUserFile(t, target, staleRedirectSeed)

		var out bytes.Buffer
		writeRedirects(&out, projects, false)

		if strings.Contains(out.String(), "write error") {
			t.Fatalf("writeRedirects failed to refresh a read-only redirect: %s", out.String())
		}
		if !strings.Contains(out.String(), "created redirect") {
			t.Errorf("output should say 'created redirect', got: %s", out.String())
		}
		data, err := os.ReadFile(target)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "ghost_list_projects") {
			t.Errorf("the stale tool-call instructions should have been replaced:\n%s", data)
		}
		if !strings.Contains(string(data), "ghost_memory_save") {
			t.Errorf("the refreshed redirect should point at ghost_memory_save:\n%s", data)
		}
		assertFileMode(t, target, 0400)
	})

	t.Run("open reader keeps its snapshot", func(t *testing.T) {
		home := t.TempDir()
		setHome(t, home)
		target := filepath.Join(home, ".claude", "projects", projectSlug(projectPath), "memory", "MEMORY.md")
		seedUserFile(t, target, staleRedirectSeed)
		reader := holdReader(t, target)

		var out bytes.Buffer
		writeRedirects(&out, projects, false)
		if strings.Contains(out.String(), "write error") {
			t.Fatalf("writeRedirects failed to refresh the redirect: %s", out.String())
		}
		assertStillSnapshot(t, reader, staleRedirectSeed)
	})
}
