// Package selfupdate provides self-update capability for ghost binaries
// by downloading releases from GitHub.
package selfupdate

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const repoAPI = "https://api.github.com/repos/wcatz/ghost/releases/latest"

const (
	// apiTimeout bounds the release metadata lookup. It is a small JSON
	// request: either GitHub answers in seconds or the network is broken, and
	// an interactive command has no business waiting longer.
	apiTimeout = 30 * time.Second
	// downloadTimeout bounds one release-asset transfer, which is a
	// multi-megabyte body on a link of unknown speed rather than a metadata
	// lookup.
	downloadTimeout = 10 * time.Minute
)

const (
	// maxReleaseJSONBytes bounds the release metadata read from GitHub. A real
	// payload is a few KiB; 4 MiB is generous while still bounding what a
	// stalled-but-open connection can push into memory before the deadline.
	maxReleaseJSONBytes int64 = 4 << 20
	// maxChecksumBytes bounds checksums.txt. A real manifest is one 64-hex
	// digest per asset, well under a KiB.
	maxChecksumBytes int64 = 1 << 20
	// maxArchiveBytes bounds a release archive. Compressed ghost binaries are
	// tens of MiB, so this leaves room to grow while keeping an endless or
	// hostile response from exhausting memory.
	maxArchiveBytes int64 = 200 << 20
)

// archiveCap is the limit ReadArchive enforces. It is a var only so a test can
// lower it and prove the cap holds without pushing 200 MiB through a socket;
// production never reassigns it.
var archiveCap = maxArchiveBytes

// The clients carry a Timeout rather than relying on http.DefaultClient, which
// has none: an accept-then-stall connection would otherwise block
// `ghost upgrade` until the OS gave up, which can be minutes of nothing.
var (
	apiClient      = &http.Client{Timeout: apiTimeout}
	downloadClient = &http.Client{Timeout: downloadTimeout}
)

// Release represents the subset of GitHub release API we need.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a release asset.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// LatestRelease fetches the latest release metadata from GitHub.
func LatestRelease() (*Release, error) {
	return fetchRelease(repoAPI)
}

// fetchRelease is LatestRelease against an explicit URL, so tests can point it
// at a local server instead of GitHub.
func fetchRelease(url string) (*Release, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := apiClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("github API returned %d", resp.StatusCode)
	}

	body, err := readCapped(resp.Body, maxReleaseJSONBytes, "release metadata")
	if err != nil {
		return nil, err
	}

	var rel Release
	if err := json.Unmarshal(body, &rel); err != nil {
		return nil, fmt.Errorf("decode release: %w", err)
	}
	return &rel, nil
}

// readCapped reads r into memory and refuses anything larger than limit.
// Exceeding the cap is an error rather than a silent truncation: a short read
// of checksums.txt or of an archive is not a smaller valid file, it is a
// different file that must not reach Replace.
func readCapped(r io.Reader, limit int64, what string) ([]byte, error) {
	// One byte past the cap is what distinguishes "exactly at the cap" from
	// "over it".
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d-byte cap", what, limit)
	}
	return data, nil
}

// ReadChecksums reads a checksums.txt manifest, refusing anything over
// maxChecksumBytes.
func ReadChecksums(r io.Reader) ([]byte, error) {
	return readCapped(r, maxChecksumBytes, "checksums.txt")
}

// ReadArchive reads a release archive, refusing anything over maxArchiveBytes
// (200 MiB).
func ReadArchive(r io.Reader) ([]byte, error) {
	return readCapped(r, archiveCap, "release archive")
}

// AssetName returns the expected archive name for the current platform.
func AssetName(version string) string {
	os := runtime.GOOS
	arch := runtime.GOARCH
	ext := "tar.gz"
	if os == "windows" {
		ext = "zip"
	}
	return fmt.Sprintf("ghost_%s_%s_%s.%s", version, os, arch, ext)
}

// FindAsset finds the matching asset for the current platform.
func FindAsset(rel *Release) (*Asset, error) {
	ver := strings.TrimPrefix(rel.TagName, "v")
	want := AssetName(ver)
	for i := range rel.Assets {
		if rel.Assets[i].Name == want {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("no release asset for %s/%s (expected %s)", runtime.GOOS, runtime.GOARCH, want)
}

// Download fetches the asset and returns the reader. Caller must close. The
// body is not bounded here — read it through ReadArchive or ReadChecksums so
// the size it is allowed to reach is named in one place.
func Download(url string) (io.ReadCloser, error) {
	resp, err := downloadClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download returned %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// FindChecksumAsset locates the checksums.txt asset published alongside the
// release binaries (goreleaser's default checksum manifest name — present on
// every ghost release).
func FindChecksumAsset(rel *Release) (*Asset, error) {
	for i := range rel.Assets {
		if rel.Assets[i].Name == "checksums.txt" {
			return &rel.Assets[i], nil
		}
	}
	return nil, fmt.Errorf("no checksums.txt asset in release %s — refusing to install an unverifiable binary", rel.TagName)
}

// VerifyChecksum checks that sha256(data) matches the entry for assetName in a
// checksums.txt manifest (goreleaser format: "<hex sha256>  <filename>" per
// line, with an optional binary-mode "*" filename prefix). It errors if the
// entry is missing or the digest mismatches, so a corrupted or substituted
// archive never reaches Replace.
func VerifyChecksum(data []byte, checksumsText, assetName string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])

	for _, line := range strings.Split(checksumsText, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		want, name := fields[0], strings.TrimPrefix(fields[1], "*")
		if name != assetName {
			continue
		}
		if !strings.EqualFold(want, got) {
			return fmt.Errorf("checksum mismatch for %s: manifest says %s, downloaded archive is %s", assetName, want, got)
		}
		return nil
	}
	return fmt.Errorf("no checksum entry for %s in checksums.txt", assetName)
}

// ExtractBinary extracts the "ghost" binary from a .tar.gz archive.
func ExtractBinary(r io.Reader) ([]byte, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		// The binary is at the root of the archive, named "ghost" or "ghost.exe".
		name := hdr.Name
		if name == "ghost" || name == "ghost.exe" {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("ghost binary not found in archive")
}

// Replace atomically replaces the binary at targetPath.
func Replace(targetPath string, newBinary []byte) error {
	// Resolve symlinks so we replace the actual file.
	resolved, err := resolveSymlinks(targetPath)
	if err != nil {
		return err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return fmt.Errorf("stat %s: %w", resolved, err)
	}

	// Write to temp file next to target, then rename.
	dir := dirOf(resolved)
	tmp, err := os.CreateTemp(dir, ".ghost-update-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(newBinary); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := os.Rename(tmpPath, resolved); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}

// maxSymlinkHops bounds chain resolution so a self-referential or
// mutually-referential symlink pair cannot spin forever.
const maxSymlinkHops = 32

// resolveSymlinks returns the real filesystem path behind path.
//
// os.Readlink alone is not sufficient here: it returns a *relative* target
// verbatim, and a relative target is only meaningful relative to the
// directory holding the symlink. Self-update runs from wherever the user
// invoked `ghost upgrade`, so resolving against the process working directory
// would point at a file that has nothing to do with the install. A single
// readlink also stops at the first hop of a chain and reports a symlink
// rather than the binary, and a broken link resolves "successfully" only to
// fail later with an error naming a path the user never typed.
//
// Every path returned is absolute, so the caller's working directory can
// never change where the update lands.
func resolveSymlinks(path string) (string, error) {
	// Anchor once, against the path the caller actually gave us. Everything
	// after this is derived from that anchor rather than from cwd.
	current, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make %s absolute: %w", path, err)
	}

	for hops := 0; ; hops++ {
		if hops >= maxSymlinkHops {
			return "", fmt.Errorf("resolve symlinks: %q exceeds %d hops — the chain may contain a loop", path, maxSymlinkHops)
		}

		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return "", fmt.Errorf("resolve symlinks: %q does not exist (missing file or broken symlink target): %w", current, err)
			}
			return "", fmt.Errorf("resolve symlinks: %w", err)
		}

		if info.Mode()&os.ModeSymlink == 0 {
			// A regular file or directory: this is the real target.
			return current, nil
		}

		target, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("readlink %s: %w", current, err)
		}
		if !filepath.IsAbs(target) {
			// Relative to the directory holding the link, never to cwd.
			target = filepath.Join(dirOf(current), target)
		}
		current = target
	}
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[:i]
		}
	}
	return "."
}
