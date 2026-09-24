// Legacy cleanup for the debris Ghost left before internal/scratch existed:
//
//   - ~/.cache/ghost-tmp — an old-style cache directory from the
//     pre-scratch-root era (hundreds of megabytes of JIT-cache droppings on
//     the machine that motivated #465/#468);
//   - the shared system temp dir — hidden `.<hex>-00000000.so` JIT-cache
//     droppings opencode wrote per invocation that leaked there only when a
//     process was SIGKILLed before #465 confined TMPDIR/TMP/TEMP to the
//     scratch root.
//
// Both paths are REPORT-ONLY by default: the report lists counts, bytes, and
// exact locations; removal happens only behind an explicit --apply, only for
// regular files matching the strict droppings signature, only after an
// open/mmap probe says the file is closed, and never for directories (Ghost
// did not create arbitrary directories and must not remove any).
package scratch

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
)

// legacyDropping is the strict signature of the #465-era droppings, derived
// from the files themselves (every one of the 69 files in the observed
// ~/.cache/ghost-tmp and every shared-temp match):
//
//	.<16 lowercase hex>-00000000.so
//
// The pattern is anchored and deliberately narrow: near-misses that exist on
// the same machines — a counter suffix other than 00000000 (e.g.
// .3cdffa7dbff6ef5b-00000001.so) and bun's own .bun-<pid>-<hex>.so — do NOT
// match, and neither does any name that is not a regular file at scan time.
var legacyDropping = regexp.MustCompile(`^\.[0-9a-f]{16}-00000000\.so$`)

// LegacyFile is one candidate: an absolute path and its lstat size in bytes.
type LegacyFile struct {
	Path string
	Size int64
}

// LegacyReport lists strict-signature droppings per legacy location. The scan
// is top-level only (no recursion): the droppings were written directly into
// these directories, and descending into foreign trees would widen the
// removal surface for no benefit.
type LegacyReport struct {
	CacheDir     string
	CacheFiles   []LegacyFile
	CacheMissing bool
	TempDir      string
	TempFiles    []LegacyFile
	TempMissing  bool
}

// ScanLegacy lists strict-signature regular files in the two legacy
// locations. Missing directories are flagged, not an error: most machines will
// have cleaned at least one already. Nothing is removed.
func ScanLegacy(cacheDir, tmpDir string) LegacyReport {
	cacheFiles, cacheMissing := scanLegacyDir(cacheDir)
	tempFiles, tempMissing := scanLegacyDir(tmpDir)
	return LegacyReport{
		CacheDir:     cacheDir,
		CacheFiles:   cacheFiles,
		CacheMissing: cacheMissing,
		TempDir:      tmpDir,
		TempFiles:    tempFiles,
		TempMissing:  tempMissing,
	}
}

// scanLegacyDir returns the strict-signature regular files directly inside
// dir. Entries are qualified with BOTH checks: the name must match the
// anchored droppings signature AND lstat must say regular file — a directory
// (or symlink) whose name mimics a dropping is never a candidate, because
// Ghost only ever removes files it can prove are droppings, never directories.
func scanLegacyDir(dir string) (files []LegacyFile, missing bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, os.IsNotExist(err)
	}
	for _, entry := range entries {
		if !legacyDropping.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info() // lstat: symlinks report as links, not targets
		if err != nil || !info.Mode().IsRegular() {
			continue // vanished, or a directory/symlink wearing a dropping's name
		}
		files = append(files, LegacyFile{Path: filepath.Join(dir, entry.Name()), Size: info.Size()})
	}
	return files, false
}

// WriteLegacyReport prints the report-only view: per-location counts and
// bytes, every candidate path, and totals. It removes nothing — with
// --apply absent this is the whole command.
func WriteLegacyReport(w io.Writer, r LegacyReport) error {
	if _, err := fmt.Fprintln(w, "legacy scratch leftovers (report only — nothing is removed without --apply):"); err != nil {
		return err
	}
	if err := writeLegacyCategory(w, "cache dir", r.CacheDir, r.CacheFiles, r.CacheMissing); err != nil {
		return err
	}
	if err := writeLegacyCategory(w, "system temp", r.TempDir, r.TempFiles, r.TempMissing); err != nil {
		return err
	}
	total := len(r.CacheFiles) + len(r.TempFiles)
	var bytes int64
	for _, f := range r.CacheFiles {
		bytes += f.Size
	}
	for _, f := range r.TempFiles {
		bytes += f.Size
	}
	_, err := fmt.Fprintf(w, "  total: %d file(s), %d bytes\n", total, bytes)
	return err
}

func writeLegacyCategory(w io.Writer, label, dir string, files []LegacyFile, missing bool) error {
	if missing {
		_, err := fmt.Fprintf(w, "  %s %s: not present\n", label, dir)
		return err
	}
	var bytes int64
	for _, f := range files {
		bytes += f.Size
	}
	if _, err := fmt.Fprintf(w, "  %s %s: %d file(s), %d bytes\n", label, dir, len(files), bytes); err != nil {
		return err
	}
	for _, f := range files {
		if _, err := fmt.Fprintf(w, "    would remove %s (%d bytes)\n", f.Path, f.Size); err != nil {
			return err
		}
	}
	return nil
}

// probe names an available open-file probe tool and its resolved path.
type probe struct {
	tool string // "lsof" or "fuser"
	path string
}

// detectOpenProbe finds a tool that can tell whether a file is open or
// mmap'd: lsof first (definitive, covers mmap), fuser as the fallback. Both
// are looked up on PATH at runtime — no build tags — so the code compiles and
// behaves identically on Windows, where neither exists and callers degrade to
// "cannot verify → skip removal" by construction.
func detectOpenProbe() (probe, bool) {
	if p, err := exec.LookPath("lsof"); err == nil {
		return probe{tool: "lsof", path: p}, true
	}
	if p, err := exec.LookPath("fuser"); err == nil {
		return probe{tool: "fuser", path: p}, true
	}
	return probe{}, false
}

// inUse reports whether path is currently open/mmap'd. Three states:
//
//	(inUse=true,  ok=true) — some process holds the file
//	(inUse=false, ok=true) — probe ran cleanly, nobody holds it
//	(ok=false)             — probe failed or errored: CANNOT VERIFY
//
// Exit-code contract (both tools): 0 = holders found, 1 = none found,
// anything else = error. "Cannot verify" is never treated as "safe to delete"
// — the caller skips such files with a message instead.
func (p probe) inUse(path string) (inUse, ok bool) {
	cmd := exec.Command(p.path, path)
	err := cmd.Run() // output discarded: only the exit code carries the verdict
	if err == nil {
		return true, true
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, true
	}
	return false, false
}

// CleanLegacy removes the report's candidates under --apply. It returns the
// count of files removed, bytes removed, and candidates skipped.
//
// Safety rails, in order per candidate:
//  1. no open-file probe on PATH (Windows CI, minimal containers) → remove
//     NOTHING and say so explicitly — cannot verify must never mean delete;
//  2. probe says the file is open/mmap'd → skip with a message;
//  3. probe errored → skip with a message ("cannot verify");
//  4. re-verify at removal time that the path is still a regular file with
//     the strict signature (defense against TOCTOU swaps);
//  5. os.Remove — a single file, never RemoveAll, never a directory.
//
// What it never touches: non-strict names, directories (even ones named like
// droppings), symlinks, and nested files the top-level scan never saw.
func CleanLegacy(w io.Writer, r LegacyReport) (removed int, removedBytes int64, skipped int) {
	candidates := make([]LegacyFile, 0, len(r.CacheFiles)+len(r.TempFiles))
	candidates = append(candidates, r.CacheFiles...)
	candidates = append(candidates, r.TempFiles...)
	if len(candidates) == 0 {
		return 0, 0, 0
	}

	p, found := detectOpenProbe()
	if !found {
		fmt.Fprintf(w, "cannot verify that files are closed: neither lsof nor fuser is available; skipping removal of %d file(s)\n", len(candidates))
		return 0, 0, len(candidates)
	}

	for _, c := range candidates {
		name := filepath.Base(c.Path)
		// Re-verify immediately before removal: the scan's verdict could be
		// stale if the path was swapped between report and apply.
		info, err := os.Lstat(c.Path)
		if err != nil || !info.Mode().IsRegular() || !legacyDropping.MatchString(name) {
			fmt.Fprintf(w, "skipping %s: no longer a strict-signature regular file\n", c.Path)
			skipped++
			continue
		}
		if inUse, ok := p.inUse(c.Path); !ok {
			fmt.Fprintf(w, "cannot verify %s is closed (%s errored); skipping\n", c.Path, p.tool)
			skipped++
			continue
		} else if inUse {
			fmt.Fprintf(w, "skipping open file: %s\n", c.Path)
			skipped++
			continue
		}
		if err := os.Remove(c.Path); err != nil {
			fmt.Fprintf(w, "skipping %s: %v\n", c.Path, err)
			skipped++
			continue
		}
		fmt.Fprintf(w, "removed %s (%d bytes)\n", c.Path, c.Size)
		removed++
		removedBytes += c.Size
	}
	fmt.Fprintf(w, "removed %d file(s), %d bytes; skipped %d\n", removed, removedBytes, skipped)
	return removed, removedBytes, skipped
}
