// Legacy cleanup for the debris Ghost left before internal/scratch existed:
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

var legacyDropping = regexp.MustCompile(`^\.[0-9a-f]{16}-00000000\.so$`)

// LegacyFile is one candidate: an absolute path and its lstat size in bytes.
type LegacyFile struct {
	Path string
	Size int64
}

// LegacyReport lists strict-signature droppings per legacy location. The scan
type LegacyReport struct {
	CacheDir     string
	CacheFiles   []LegacyFile
	CacheMissing bool
	TempDir      string
	TempFiles    []LegacyFile
	TempMissing  bool
}

// ScanLegacy lists strict-signature regular files in the two legacy
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

type probe struct {
	tool string // "lsof" or "fuser"
	path string
}

func detectOpenProbe() (probe, bool) {
	if p, err := exec.LookPath("lsof"); err == nil {
		return probe{tool: "lsof", path: p}, true
	}
	if p, err := exec.LookPath("fuser"); err == nil {
		return probe{tool: "fuser", path: p}, true
	}
	return probe{}, false
}

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
func CleanLegacy(w io.Writer, r LegacyReport) (removed int, removedBytes int64, skipped int, err error) {
	note := func(format string, args ...any) {
		if err != nil {
			return
		}
		_, err = fmt.Fprintf(w, format, args...)
	}

	candidates := make([]LegacyFile, 0, len(r.CacheFiles)+len(r.TempFiles))
	candidates = append(candidates, r.CacheFiles...)
	candidates = append(candidates, r.TempFiles...)
	if len(candidates) == 0 {
		return 0, 0, 0, nil
	}

	p, found := detectOpenProbe()
	if !found {
		note("cannot verify that files are closed: neither lsof nor fuser is available; skipping removal of %d file(s)\n", len(candidates))
		return 0, 0, len(candidates), err
	}

	for _, c := range candidates {
		name := filepath.Base(c.Path)
		info, statErr := os.Lstat(c.Path)
		if statErr != nil || !info.Mode().IsRegular() || !legacyDropping.MatchString(name) {
			note("skipping %s: no longer a strict-signature regular file\n", c.Path)
			skipped++
			continue
		}
		if inUse, ok := p.inUse(c.Path); !ok {
			note("cannot verify %s is closed (%s errored); skipping\n", c.Path, p.tool)
			skipped++
			continue
		} else if inUse {
			note("skipping open file: %s\n", c.Path)
			skipped++
			continue
		}
		if rmErr := os.Remove(c.Path); rmErr != nil {
			note("skipping %s: %v\n", c.Path, rmErr)
			skipped++
			continue
		}
		note("removed %s (%d bytes)\n", c.Path, c.Size)
		removed++
		removedBytes += c.Size
	}
	note("removed %d file(s), %d bytes; skipped %d\n", removed, removedBytes, skipped)
	return removed, removedBytes, skipped, err
}
