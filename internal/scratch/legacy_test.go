package scratch

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// legacyFixture builds the two legacy locations with every candidate class the
// scan must classify correctly:
//   - ghost-tmp/: a strict-signature file, a NON-strict file, a strict-named
//     DIRECTORY (never removable), a strict file nested in a subdir (scan is
//     top-level only).
//   - shared tmp/: a strict-signature file, a -00000001 counter near-miss, a
//     .bun-*.so (bun's own JIT), and a plain file.
func legacyFixture(t *testing.T) (cacheDir, tmpDir string) {
	t.Helper()
	base := t.TempDir()
	cacheDir = filepath.Join(base, "cache", "ghost-tmp")
	tmpDir = filepath.Join(base, "tmp")
	for _, d := range []string{filepath.Join(cacheDir, "subdir"), filepath.Join(cacheDir, ".cccc000011112222-00000000.so"), tmpDir} {
		must(t, os.MkdirAll(d, 0o700))
	}
	files := map[string]int{
		filepath.Join(cacheDir, ".3cdfbefd9d76f95f-00000000.so"):           100,
		filepath.Join(cacheDir, "notes.txt"):                               5,
		filepath.Join(cacheDir, "subdir", ".bbbb000011112222-00000000.so"): 50,
		filepath.Join(tmpDir, ".3cdffa5eddfeef5f-00000000.so"):             200,
		filepath.Join(tmpDir, ".3cdffa7dbff6ef5b-00000001.so"):             300,
		filepath.Join(tmpDir, ".bun-1000-25bad600711fb661.so"):             400,
		filepath.Join(tmpDir, "some-ordinary-file"):                        8,
	}
	for path, size := range files {
		must(t, os.WriteFile(path, make([]byte, size), 0o600))
	}
	return cacheDir, tmpDir
}

// listTree returns every path under roots, sorted, so a before/after comparison
// proves what a run removed or created.
func listTree(t *testing.T, roots ...string) []string {
	t.Helper()
	var paths []string
	for _, root := range roots {
		must(t, filepath.WalkDir(root, func(path string, _ fs.DirEntry, err error) error {
			paths = append(paths, path)
			return err
		}))
	}
	slices.Sort(paths)
	return paths
}

// fakeTool writes an executable stand-in for lsof/fuser into a private dir and
// points PATH at exactly that dir: exit 0 reports the file in use, exit 1 not
// in use, exit 2 an error (cannot verify).
func fakeTool(t *testing.T, name string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake tool requires a POSIX shell")
	}
	dir := t.TempDir()
	must(t, os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)), 0o755))
	t.Setenv("PATH", dir)
}

// TestScanLegacyAndReport_ListStrictFilesOnlyRemovingNothing: the default
// report lists exactly the strict-signature regular files, never the
// near-misses, nested files, or strict-named directories, and removes nothing.
func TestScanLegacyAndReport_ListStrictFilesOnlyRemovingNothing(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	before := listTree(t, cacheDir, tmpDir)

	report := ScanLegacy(cacheDir, tmpDir)
	var buf bytes.Buffer
	if err := WriteLegacyReport(&buf, report); err != nil {
		t.Fatalf("WriteLegacyReport: %v", err)
	}
	out := buf.String()

	if len(report.CacheFiles) != 1 || len(report.TempFiles) != 1 {
		t.Fatalf("candidates = (%d cache, %d temp), want (1, 1)", len(report.CacheFiles), len(report.TempFiles))
	}
	if report.CacheFiles[0].Size != 100 || report.TempFiles[0].Size != 200 {
		t.Errorf("sizes = (%d, %d), want (100, 200)", report.CacheFiles[0].Size, report.TempFiles[0].Size)
	}
	for _, want := range []string{
		cacheDir, tmpDir,
		filepath.Join(cacheDir, ".3cdfbefd9d76f95f-00000000.so"),
		filepath.Join(tmpDir, ".3cdffa5eddfeef5f-00000000.so"),
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing %q:\n%s", want, out)
		}
	}
	for _, name := range []string{
		".3cdffa7dbff6ef5b-00000001.so", ".bun-1000-25bad600711fb661.so", "notes.txt",
		"some-ordinary-file", ".bbbb000011112222-00000000.so", ".cccc000011112222-00000000.so",
	} {
		if strings.Contains(out, "would remove "+filepath.Join(cacheDir, name)) ||
			strings.Contains(out, "would remove "+filepath.Join(tmpDir, name)) {
			t.Errorf("report lists %q as removable:\n%s", name, out)
		}
	}
	if after := listTree(t, cacheDir, tmpDir); !slices.Equal(before, after) {
		t.Errorf("report-only mutated the fixture:\nbefore %v\nafter  %v", before, after)
	}
}

// TestProbe_inUse_RealToolContract pins probe.inUse against the REAL lsof and
// fuser binaries, because the rest of the suite fakes them. Exit 0 means a
// process holds the file, exit 1 nobody does, any other exit an error. lsof
// exits 1 — not 0 — when it finds no holder, so a probe reading it as "always
// success" would report every candidate in use and remove nothing.
func TestProbe_inUse_RealToolContract(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("neither lsof nor fuser exists on Windows; CleanLegacy degrades by construction")
	}
	for _, tool := range []string{"lsof", "fuser"} {
		t.Run(tool, func(t *testing.T) {
			path, err := exec.LookPath(tool)
			if err != nil {
				t.Skipf("%s not installed", tool)
			}
			p := probe{tool: tool, path: path}
			candidate := filepath.Join(t.TempDir(), ".0123456789abcdef-00000000.so")
			must(t, os.WriteFile(candidate, []byte("x"), 0o600))

			if inUse, ok := p.inUse(candidate); !ok || inUse {
				t.Errorf("%s: unheld file = (inUse %v, verified %v), want (false, true)", tool, inUse, ok)
			}
			f, err := os.Open(candidate)
			must(t, err)
			defer f.Close() //nolint:errcheck
			if inUse, ok := p.inUse(candidate); !ok || !inUse {
				t.Errorf("%s: held file = (inUse %v, verified %v), want (true, true)", tool, inUse, ok)
			}
		})
	}
}

// TestCleanLegacy_ApplyRemovesStrictRegularFilesOnly: with a tool reporting
// every file closed, --apply removes exactly the strict-signature regular
// files; everything else, including the strict-named directory, survives.
func TestCleanLegacy_ApplyRemovesStrictRegularFilesOnly(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	before := listTree(t, cacheDir, tmpDir)
	report := ScanLegacy(cacheDir, tmpDir)
	fakeTool(t, "lsof", 1)

	var buf bytes.Buffer
	removed, removedBytes, skipped, err := CleanLegacy(&buf, report)
	if err != nil {
		t.Fatalf("CleanLegacy: %v", err)
	}
	if removed != 2 || removedBytes != 300 || skipped != 0 {
		t.Errorf("CleanLegacy = (%d files, %d bytes, %d skipped), want (2, 300, 0): output:\n%s", removed, removedBytes, skipped, buf.String())
	}
	want := slices.DeleteFunc(slices.Clone(before), func(p string) bool {
		return p == filepath.Join(cacheDir, ".3cdfbefd9d76f95f-00000000.so") || p == filepath.Join(tmpDir, ".3cdffa5eddfeef5f-00000000.so")
	})
	if after := listTree(t, cacheDir, tmpDir); !slices.Equal(want, after) {
		t.Errorf("tree after apply =\n%v\nwant\n%v", after, want)
	}
}

// TestCleanLegacy_NoRemovalWithoutAVerifiedClosedFile: a tool reporting the
// files in use, a tool that errors, or no tool at all (an empty PATH, as on
// Windows CI) makes --apply skip every candidate with a message, rather than
// risk deleting an open file. With only fuser on PATH it decides instead.
func TestCleanLegacy_NoRemovalWithoutAVerifiedClosedFile(t *testing.T) {
	tests := []struct {
		name        string
		tool        string
		exit        int
		wantRemoved int
		wantMsg     []string
	}{
		{"in use", "lsof", 0, 0, []string{"open"}},
		{"tool error", "lsof", 2, 0, []string{"cannot verify"}},
		{"no tool", "", 0, 0, []string{"cannot verify", "lsof"}},
		{"fuser fallback closed", "fuser", 1, 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheDir, tmpDir := legacyFixture(t)
			report := ScanLegacy(cacheDir, tmpDir)
			if tt.tool == "" {
				t.Setenv("PATH", "")
			} else {
				fakeTool(t, tt.tool, tt.exit)
			}
			before := listTree(t, cacheDir, tmpDir)

			var buf bytes.Buffer
			removed, _, skipped, err := CleanLegacy(&buf, report)
			if err != nil {
				t.Fatalf("CleanLegacy: %v", err)
			}
			if removed != tt.wantRemoved || removed+skipped != 2 {
				t.Errorf("CleanLegacy = (removed %d, skipped %d), want %d removed of 2: output:\n%s", removed, skipped, tt.wantRemoved, buf.String())
			}
			for _, m := range tt.wantMsg {
				if !strings.Contains(buf.String(), m) {
					t.Errorf("message missing %q:\n%s", m, buf.String())
				}
			}
			if tt.wantRemoved == 0 {
				if after := listTree(t, cacheDir, tmpDir); !slices.Equal(before, after) {
					t.Errorf("fixture mutated: before %v after %v", before, after)
				}
			}
		})
	}
}

// TestScanLegacy_MissingDirsAreEmpty: both legacy locations are optional —
// absent dirs yield empty candidates, not errors.
func TestScanLegacy_MissingDirsAreEmpty(t *testing.T) {
	report := ScanLegacy(filepath.Join(t.TempDir(), "nope"), filepath.Join(t.TempDir(), "alsonope"))
	if len(report.CacheFiles) != 0 || len(report.TempFiles) != 0 || !report.CacheMissing || !report.TempMissing {
		t.Errorf("missing dirs must yield no candidates and be flagged: %+v", report)
	}
}
