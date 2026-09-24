package scratch

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// legacyFixture builds the two legacy locations with every candidate class the
// scan must classify correctly:
//   - ghost-tmp/: a strict-signature file, a NON-strict file, a strict-named
//     DIRECTORY (name matches, but it is a dir — never removable), a strict
//     file nested in a subdir (scan is top-level only), plus a foreign subdir.
//   - shared tmp/: a strict-signature file, a -00000001 counter near-miss, a
//     .bun-*.so (bun's own JIT), and a plain file.
//
// It returns the two dirs.
func legacyFixture(t *testing.T) (cacheDir, tmpDir string) {
	t.Helper()
	base := t.TempDir()
	cacheDir = filepath.Join(base, "cache", "ghost-tmp")
	tmpDir = filepath.Join(base, "tmp")
	for _, d := range []string{
		cacheDir,
		filepath.Join(cacheDir, "subdir"),
		tmpDir,
	} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		// cache dir
		filepath.Join(cacheDir, ".3cdfbefd9d76f95f-00000000.so"):           bytes.Repeat([]byte{1}, 100),
		filepath.Join(cacheDir, "notes.txt"):                               []byte("not a dropping"),
		filepath.Join(cacheDir, "subdir", ".bbbb000011112222-00000000.so"): bytes.Repeat([]byte{2}, 50), // nested: not scanned
		filepath.Join(cacheDir, "ghost-tmp-lookalike-dir"):                 nil,
		// shared tmp
		filepath.Join(tmpDir, ".3cdffa5eddfeef5f-00000000.so"): bytes.Repeat([]byte{3}, 200),
		filepath.Join(tmpDir, ".3cdffa7dbff6ef5b-00000001.so"): bytes.Repeat([]byte{4}, 300), // counter near-miss
		filepath.Join(tmpDir, ".bun-1000-25bad600711fb661.so"): bytes.Repeat([]byte{5}, 400), // bun's own
		filepath.Join(tmpDir, "some-ordinary-file"):            []byte("ordinary"),
	}
	for path, content := range files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The strict-NAME directory: same shape as a dropping but a directory.
	strictNamedDir := filepath.Join(cacheDir, ".cccc000011112222-00000000.so")
	if err := os.Mkdir(strictNamedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return cacheDir, tmpDir
}

// snapshotTree hashes the full fixture state: path → "dir" or content hash.
// Byte-level before/after diffs prove report-only never mutates and --apply
// touches exactly the strict regular files.
func snapshotTree(t *testing.T, roots ...string) map[string]string {
	t.Helper()
	state := map[string]string{}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				state[path] = "dir"
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				state[path] = "symlink"
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			state[path] = hex.EncodeToString(sum[:])
			return nil
		})
		if err != nil {
			t.Fatalf("snapshot walk %s: %v", root, err)
		}
	}
	return state
}

func diffSnapshots(before, after map[string]string) (removed, added []string, changed []string) {
	for path, hash := range before {
		newHash, ok := after[path]
		if !ok {
			removed = append(removed, path)
		} else if newHash != hash {
			changed = append(changed, path)
		}
	}
	for path := range after {
		if _, ok := before[path]; !ok {
			added = append(added, path)
		}
	}
	sort.Strings(removed)
	sort.Strings(added)
	sort.Strings(changed)
	return removed, added, changed
}

// TestScanLegacyAndReport_ListStrictFilesOnlyRemovingNothing: the default
// report must list exactly the strict-signature regular files (with counts and
// bytes per location), never the near-misses, nested files, or strict-named
// directories — and it must remove nothing at all (byte-level fixture diff).
func TestScanLegacyAndReport_ListStrictFilesOnlyRemovingNothing(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	before := snapshotTree(t, cacheDir, tmpDir)

	report := ScanLegacy(cacheDir, tmpDir)
	var buf bytes.Buffer
	if err := WriteLegacyReport(&buf, report); err != nil {
		t.Fatalf("WriteLegacyReport: %v", err)
	}
	out := buf.String()

	// Counts + bytes per category.
	if len(report.CacheFiles) != 1 {
		t.Errorf("cache candidates = %d (%v), want 1 (strict regular only)", len(report.CacheFiles), report.CacheFiles)
	}
	if len(report.TempFiles) != 1 {
		t.Errorf("temp candidates = %d (%v), want 1 (strict regular only)", len(report.TempFiles), report.TempFiles)
	}
	if report.CacheFiles[0].Size != 100 || report.TempFiles[0].Size != 200 {
		t.Errorf("sizes = (%d, %d), want (100, 200)", report.CacheFiles[0].Size, report.TempFiles[0].Size)
	}
	// Where they are: both dirs named in the report.
	for _, want := range []string{cacheDir, tmpDir} {
		if !strings.Contains(out, want) {
			t.Errorf("report missing location %q:\n%s", want, out)
		}
	}
	// "would remove" lists exactly the two strict paths.
	if !strings.Contains(out, filepath.Join(cacheDir, ".3cdfbefd9d76f95f-00000000.so")) {
		t.Errorf("report missing strict cache candidate:\n%s", out)
	}
	if !strings.Contains(out, filepath.Join(tmpDir, ".3cdffa5eddfeef5f-00000000.so")) {
		t.Errorf("report missing strict temp candidate:\n%s", out)
	}
	// Near-misses, non-strict, nested, and strict-named dir are NOT removable.
	notRemovable := []string{
		".3cdffa7dbff6ef5b-00000001.so", // counter near-miss
		".bun-1000-25bad600711fb661.so", // bun's own
		"notes.txt",
		"some-ordinary-file",
		".bbbb000011112222-00000000.so", // nested in subdir
		".cccc000011112222-00000000.so", // strict NAME but a directory
	}
	for _, name := range notRemovable {
		if strings.Contains(out, "would remove "+filepath.Join(cacheDir, name)) ||
			strings.Contains(out, "would remove "+filepath.Join(tmpDir, name)) {
			t.Errorf("report lists %q as removable — must be strict regular files only:\n%s", name, out)
		}
	}

	// Report-only: byte-level fixture diff — nothing removed, nothing changed.
	after := snapshotTree(t, cacheDir, tmpDir)
	removed, added, changed := diffSnapshots(before, after)
	if len(removed)+len(added)+len(changed) != 0 {
		t.Errorf("report-only mutated the fixture: removed=%v added=%v changed=%v", removed, added, changed)
	}
}

// fakeTool writes an executable stand-in for lsof/fuser into a private dir and
// points PATH at exactly that dir, so apply-mode behavior is deterministic:
//   - exit 0 → tool reports the file as in use
//   - exit 1 → tool reports the file as not in use
//   - exit 2 → tool errors (cannot verify)
//   - empty PATH → tool unavailable
func fakeTool(t *testing.T, name string, exitCode int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake tool requires a POSIX shell")
	}
	dir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

// TestProbe_inUse_RealToolContract pins probe.inUse against the REAL lsof and
// fuser binaries, because the rest of the suite fakes them (fakeTool above) and
// therefore cannot notice if the exit-code contract those fakes assume is not
// what the shipped tools actually do.
//
// The contract inUse depends on, and which both tools satisfy: exit 0 means a
// process holds the file, exit 1 means nobody holds it, any other exit means
// the probe errored. Note in particular that lsof exits 1 — not 0 — when it
// finds no holder, so a probe that read lsof's exit as "always success" would
// report every candidate as in use and clean-scratch would remove nothing.
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

			dir := t.TempDir()
			candidate := filepath.Join(dir, ".0123456789abcdef-00000000.so")
			if err := os.WriteFile(candidate, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}

			// Unheld: the file exists but nothing has it open. This is the
			// case that decides whether apply-mode removes anything at all.
			inUse, ok := p.inUse(candidate)
			if !ok {
				t.Errorf("%s: unheld file reported cannot-verify; CleanLegacy would refuse to remove anything", tool)
			}
			if inUse {
				t.Errorf("%s: unheld file reported in use (exit 0); CleanLegacy would skip every removal", tool)
			}

			// Held: a real open descriptor must be detected, or apply-mode
			// would delete a file another process is still using.
			f, err := os.Open(candidate)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close() //nolint:errcheck
			inUse, ok = p.inUse(candidate)
			if !ok {
				t.Errorf("%s: held file reported cannot-verify", tool)
			}
			if !inUse {
				t.Errorf("%s: held file reported not in use; CleanLegacy would delete an in-use file", tool)
			}
		})
	}
}

// TestCleanLegacy_ApplyRemovesStrictRegularFilesOnly: with a tool present that
// reports every file as closed, --apply removes exactly the strict-signature
// regular files; the non-strict files, the strict-named directory (with its
// content), the nested strict file, and all foreign dirs survive byte-identically.
func TestCleanLegacy_ApplyRemovesStrictRegularFilesOnly(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	before := snapshotTree(t, cacheDir, tmpDir)
	report := ScanLegacy(cacheDir, tmpDir)
	fakeTool(t, "lsof", 1) // not in use → removable

	var buf bytes.Buffer
	removed, removedBytes, skipped, err := CleanLegacy(&buf, report)
	if err != nil {
		t.Fatalf("CleanLegacy: %v", err)
	}

	if removed != 2 || removedBytes != 300 || skipped != 0 {
		t.Errorf("CleanLegacy = (%d files, %d bytes, %d skipped), want (2, 300, 0): output:\n%s",
			removed, removedBytes, skipped, buf.String())
	}
	after := snapshotTree(t, cacheDir, tmpDir)
	gotRemoved, added, changed := diffSnapshots(before, after)
	wantRemoved := []string{
		filepath.Join(cacheDir, ".3cdfbefd9d76f95f-00000000.so"),
		filepath.Join(tmpDir, ".3cdffa5eddfeef5f-00000000.so"),
	}
	if strings.Join(gotRemoved, "\n") != strings.Join(wantRemoved, "\n") {
		t.Errorf("removed files =\n%v\nwant\n%v", gotRemoved, wantRemoved)
	}
	if len(added) != 0 || len(changed) != 0 {
		t.Errorf("apply mutated or created files: added=%v changed=%v", added, changed)
	}
	// The strict-named DIRECTORY and everything non-strict still exist.
	for _, keep := range []string{
		filepath.Join(cacheDir, ".cccc000011112222-00000000.so"), // strict NAME, but a directory
		filepath.Join(cacheDir, "notes.txt"),
		filepath.Join(cacheDir, "subdir", ".bbbb000011112222-00000000.so"),
		filepath.Join(tmpDir, ".3cdffa7dbff6ef5b-00000001.so"),
		filepath.Join(tmpDir, ".bun-1000-25bad600711fb661.so"),
	} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("apply removed a file it must never touch: %s (%v)", keep, err)
		}
	}
}

// TestCleanLegacy_SkipsOpenFiles: a tool that reports files as in use makes
// --apply skip them — with a message — leaving every byte in place.
func TestCleanLegacy_SkipsOpenFiles(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	before := snapshotTree(t, cacheDir, tmpDir)
	report := ScanLegacy(cacheDir, tmpDir)
	fakeTool(t, "lsof", 0) // in use → must skip

	var buf bytes.Buffer
	removed, _, skipped, err := CleanLegacy(&buf, report)
	if err != nil {
		t.Fatalf("CleanLegacy: %v", err)
	}
	out := buf.String()

	if removed != 0 || skipped != 2 {
		t.Errorf("CleanLegacy = (removed %d, skipped %d), want (0, 2): output:\n%s", removed, skipped, out)
	}
	if !strings.Contains(out, "open") {
		t.Errorf("skip message must say the file is open:\n%s", out)
	}
	after := snapshotTree(t, cacheDir, tmpDir)
	removedPaths, added, changed := diffSnapshots(before, after)
	if len(removedPaths) != 0 || len(added) != 0 || len(changed) != 0 {
		t.Errorf("open files were mutated: removed=%v added=%v changed=%v", removedPaths, added, changed)
	}
}

// TestCleanLegacy_ToolErrorSkips: when the probe tool errors (exit 2), the
// file cannot be verified — skip it with a message rather than risk deleting
// an open file.
func TestCleanLegacy_ToolErrorSkips(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	report := ScanLegacy(cacheDir, tmpDir)
	fakeTool(t, "lsof", 2) // error → cannot verify

	var buf bytes.Buffer
	removed, _, skipped, err := CleanLegacy(&buf, report)
	if err != nil {
		t.Fatalf("CleanLegacy: %v", err)
	}
	if removed != 0 || skipped != 2 {
		t.Errorf("CleanLegacy = (removed %d, skipped %d), want (0, 2): output:\n%s", removed, skipped, buf.String())
	}
	if !strings.Contains(buf.String(), "cannot verify") {
		t.Errorf("skip message must say the file cannot be verified:\n%s", buf.String())
	}
}

// TestCleanLegacy_FuserFallback: lsof absent, fuser present → the fuser probe
// decides (exit 1 = closed → removable).
func TestCleanLegacy_FuserFallback(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	report := ScanLegacy(cacheDir, tmpDir)
	fakeTool(t, "fuser", 1) // only fuser on PATH; reports closed

	var buf bytes.Buffer
	removed, _, skipped, err := CleanLegacy(&buf, report)
	if err != nil {
		t.Fatalf("CleanLegacy: %v", err)
	}
	if removed != 2 || skipped != 0 {
		t.Errorf("CleanLegacy = (removed %d, skipped %d), want (2, 0): output:\n%s", removed, skipped, buf.String())
	}
}

// TestCleanLegacy_ToolUnavailableSkipsRemoval: with neither lsof nor fuser on
// PATH (Windows CI, minimal containers — reproduced here by an empty PATH),
// --apply must refuse to remove anything and say so explicitly. This is the
// portable degradation path: no build tags, a runtime PATH check.
func TestCleanLegacy_ToolUnavailableSkipsRemoval(t *testing.T) {
	cacheDir, tmpDir := legacyFixture(t)
	before := snapshotTree(t, cacheDir, tmpDir)
	report := ScanLegacy(cacheDir, tmpDir)
	t.Setenv("PATH", "") // no lsof, no fuser anywhere

	var buf bytes.Buffer
	removed, _, skipped, err := CleanLegacy(&buf, report)
	if err != nil {
		t.Fatalf("CleanLegacy: %v", err)
	}
	out := buf.String()

	if removed != 0 {
		t.Errorf("removed %d files with no open-file probe available, want 0", removed)
	}
	if skipped != 2 {
		t.Errorf("skipped = %d, want 2 (every candidate accounted for)", skipped)
	}
	if !strings.Contains(out, "cannot verify") || !strings.Contains(out, "lsof") {
		t.Errorf("message must name the missing capability (cannot verify / lsof):\n%s", out)
	}
	after := snapshotTree(t, cacheDir, tmpDir)
	removedPaths, added, changed := diffSnapshots(before, after)
	if len(removedPaths) != 0 || len(added) != 0 || len(changed) != 0 {
		t.Errorf("fixture mutated without a probe tool: removed=%v added=%v changed=%v", removedPaths, added, changed)
	}
}

// TestScanLegacy_MissingDirsAreEmpty: both legacy locations are optional —
// absent dirs yield empty candidates, not errors.
func TestScanLegacy_MissingDirsAreEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope")
	report := ScanLegacy(missing, filepath.Join(t.TempDir(), "alsonope"))
	if len(report.CacheFiles) != 0 || len(report.TempFiles) != 0 {
		t.Errorf("missing dirs produced candidates: %+v", report)
	}
	if !report.CacheMissing || !report.TempMissing {
		t.Errorf("missing dirs must be flagged: %+v", report)
	}
}
