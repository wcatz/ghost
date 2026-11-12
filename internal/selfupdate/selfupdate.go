// Package selfupdate provides self-update capability for ghost binaries
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
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

var (
	apiTimeout      = 30 * time.Second
	downloadTimeout = 10 * time.Minute
)

const (
	maxReleaseJSONBytes int64 = 4 << 20
	maxChecksumBytes    int64 = 1 << 20
	maxArchiveBytes     int64 = 200 << 20
	maxBinaryBytes      int64 = 128 << 20
)

var (
	archiveCap = maxArchiveBytes
	binaryCap  = maxBinaryBytes
)

var httpClient = &http.Client{}

// Release represents the subset of GitHub release API we need.
type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

// Asset represents a release asset. Digest is GitHub's own digest of the bytes
// it holds, in the form "sha256:<hex>" — see VerifyAssetDigest for why that
// field and not checksums.txt is the one an upgrade trusts.
type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

// LatestRelease fetches the latest release metadata from GitHub.
func LatestRelease(ctx context.Context) (*Release, error) {
	return fetchRelease(ctx, repoAPI)
}

func fetchRelease(ctx context.Context, url string) (*Release, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
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

func readCapped(r io.Reader, limit int64, what string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", what, err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is larger than the %d-byte cap", what, limit)
	}
	return data, nil
}

// ChecksumCap is the largest checksums.txt ReadChecksums will read. It is
// exported so a caller that has to serve a manifest larger than the cap — a
// test of the cap itself — names the same number this package refuses over,
// rather than a copy of it that can drift.
func ChecksumCap() int64 { return maxChecksumBytes }

// ReadChecksums reads a checksums.txt manifest, refusing anything over
// maxChecksumBytes.
func ReadChecksums(r io.Reader) ([]byte, error) {
	return readCapped(r, maxChecksumBytes, "checksums.txt")
}

// ReadArchive reads a release archive, refusing anything over maxArchiveBytes
// (200 MiB). The cap is on the compressed transfer; ExtractBinary applies its
// own cap to what the archive decompresses into, which this one cannot bound.
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

// Download fetches the asset and returns the reader. Caller must close: the
// request's deadline is ended there, and it has to outlive this function
// because the body is read after Download returns. The body is not bounded
// here — read it through ReadArchive or ReadChecksums so the size it is allowed
// to reach is named in one place.
func Download(ctx context.Context, url string) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("download: %w", err)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("download: %w", err)
	}
	if resp.StatusCode != 200 {
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("download returned %d", resp.StatusCode)
	}
	return &deadlineBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

type deadlineBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *deadlineBody) Close() error {
	err := b.ReadCloser.Close()
	b.cancel()
	return err
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

const sha256HexLen = 2 * sha256.Size

// VerifyAssetDigest checks that data matches the digest GitHub reports for this
// release asset.
func VerifyAssetDigest(data []byte, asset *Asset) error {
	if asset == nil {
		return fmt.Errorf("no release asset to verify")
	}
	algorithm, reported, found := strings.Cut(asset.Digest, ":")
	switch {
	case asset.Digest == "":
		return fmt.Errorf("github reports no digest for %s in this release — refusing to install an archive nothing vouches for", asset.Name)
	case !found:
		return fmt.Errorf("unsupported digest %q for %s: expected an algorithm and a value, as in \"sha256:<hex>\"", asset.Digest, asset.Name)
	case !strings.EqualFold(algorithm, "sha256"):
		return fmt.Errorf("unsupported digest algorithm %q for %s: only sha256 is verified", algorithm, asset.Name)
	case len(reported) != sha256HexLen:
		return fmt.Errorf("malformed digest %q for %s: a sha256 digest is %d hex characters", asset.Digest, asset.Name, sha256HexLen)
	}
	if _, err := hex.DecodeString(reported); err != nil {
		return fmt.Errorf("malformed digest %q for %s: %w", asset.Digest, asset.Name, err)
	}

	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(reported, got) {
		return fmt.Errorf("digest mismatch for %s: github reports %s, downloaded archive is %s", asset.Name, asset.Digest, got)
	}
	return nil
}

var zipMagic = []byte("PK")

// ExtractBinary extracts the ghost binary from a release archive: the .tar.gz
// every unix release ships, or the .zip every Windows release ships
// (goreleaser's format_overrides). Before #558 the zip was handed to a gzip
// reader, so `ghost upgrade` failed with "gzip: invalid header" on one of the
// two platforms ghost builds for.
func ExtractBinary(archive []byte) ([]byte, error) {
	if bytes.HasPrefix(archive, zipMagic) {
		return extractZipBinary(archive)
	}
	return extractTarGzBinary(archive)
}

func extractTarGzBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck

	tr := tar.NewReader(&inflatedReader{r: gz, limit: binaryCap})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Name != "ghost" && hdr.Name != "ghost.exe" {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return nil, fmt.Errorf("ghost in the archive is a %s, not a regular file", tarEntryKind(hdr))
		}
		binary, err := io.ReadAll(tr)
		if err != nil {
			return nil, fmt.Errorf("reading the ghost binary from the archive: %w", err)
		}
		if len(binary) == 0 {
			return nil, fmt.Errorf("ghost in the archive is empty — refusing to install a zero-byte binary")
		}
		return binary, nil
	}
	return nil, fmt.Errorf("ghost binary not found in archive")
}

func tarEntryKind(hdr *tar.Header) string {
	switch hdr.Typeflag {
	case tar.TypeDir:
		return "directory"
	case tar.TypeSymlink:
		return "symlink"
	case tar.TypeLink:
		return "hard link"
	case tar.TypeChar, tar.TypeBlock, tar.TypeFifo:
		return "device or fifo"
	default:
		return fmt.Sprintf("type %q", hdr.Typeflag)
	}
}

type inflatedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (c *inflatedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		return 0, fmt.Errorf("the release archive expands past the %d-byte cap", c.limit)
	}
	return n, err
}

func extractZipBinary(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || (f.Name != "ghost" && f.Name != "ghost.exe") {
			continue
		}
		if !zipEntryIsRegular(f) {
			return nil, fmt.Errorf("ghost in the archive is a %s, not a regular file", zipEntryKind(f))
		}
		rc, err := f.Open()
		if err != nil {
			return nil, fmt.Errorf("zip %s: %w", f.Name, err)
		}
		binary, readErr := readCapped(rc, binaryCap, "ghost binary")
		_ = rc.Close() //nolint:errcheck
		if readErr != nil {
			return nil, readErr
		}
		if len(binary) == 0 {
			return nil, fmt.Errorf("ghost in the archive is empty — refusing to install a zero-byte binary")
		}
		return binary, nil
	}
	return nil, fmt.Errorf("ghost binary not found in archive")
}

const (
	zipTypeMask      = 0o170000
	zipTypeRegular   = 0o100000
	zipTypeLink      = 0o120000
	zipTypeDirectory = 0o040000
	zipTypeFIFO      = 0o010000
	zipTypeSocket    = 0o140000
	zipTypeChar      = 0o020000
	zipTypeBlock     = 0o060000
)

func zipEntryType(f *zip.File) uint32 {
	return (f.ExternalAttrs >> 16) & zipTypeMask
}

func zipEntryIsRegular(f *zip.File) bool {
	switch zipEntryType(f) {
	case 0, zipTypeRegular:
		return true
	default:
		return false
	}
}

func zipEntryKind(f *zip.File) string {
	switch zipEntryType(f) {
	case zipTypeLink:
		return "link"
	case zipTypeDirectory:
		return "directory"
	case zipTypeFIFO:
		return "fifo"
	case zipTypeSocket:
		return "socket"
	case zipTypeChar, zipTypeBlock:
		return "device"
	default:
		return "special file"
	}
}

const asideSuffix = ".old"

var (
	removeFile = os.Remove
	renameFile = os.Rename
)

var writeStaged = func(f *os.File, newBinary []byte) error {
	_, err := f.Write(newBinary)
	return err
}

// Replace atomically replaces the binary at targetPath.
func Replace(targetPath string, newBinary []byte) (string, error) {
	resolved, err := resolveSymlinks(targetPath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", resolved, err)
	}

	dir := dirOf(resolved)
	tmp, err := os.CreateTemp(dir, ".ghost-update-*")
	if err != nil {
		return "", fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if err := writeStaged(tmp, newBinary); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("close temp: %w", err)
	}
	if err := os.Chmod(tmpPath, info.Mode()); err != nil {
		_ = os.Remove(tmpPath)
		return "", fmt.Errorf("chmod temp: %w", err)
	}
	note, err := installNewBinary(tmpPath, resolved, asideRunningTarget)
	if err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return note, nil
}

func installNewBinary(tmpPath, targetPath string, asideFirst bool) (string, error) {
	if !asideFirst {
		if err := renameFile(tmpPath, targetPath); err != nil {
			return "", err
		}
		return "", nil
	}

	sweepOldAsides(targetPath)

	fixedAside := targetPath + asideSuffix
	aside := fixedAside
	var lockedErr error
	if err := removeFile(aside); err != nil && !os.IsNotExist(err) {
		lockedErr = err
		aside = fmt.Sprintf("%s%s-%d", targetPath, asideSuffix, time.Now().UnixNano())
	}

	if err := renameFile(targetPath, aside); err != nil {
		if lockedErr == nil {
			return "", fmt.Errorf("move the current binary aside to %s: %w", aside, err)
		}
		return "", fmt.Errorf("move the current binary aside to %s: %w; removing the fixed %s also failed: %v (a running ghost mcp client may hold the old binary, and restarting the client releases it)", aside, err, fixedAside, lockedErr)
	}

	if err := renameFile(tmpPath, targetPath); err != nil {
		if restoreErr := renameFile(aside, targetPath); restoreErr != nil {
			msg := fmt.Errorf("install the new binary: %w (the previous binary is at %s and could not be moved back: %v)", err, aside, restoreErr)
			if lockedErr != nil {
				msg = fmt.Errorf("%v; removing the fixed %s also failed: %v (a running ghost mcp client may hold the old binary, and restarting the client releases it)", msg, fixedAside, lockedErr)
			}
			return "", msg
		}
		if lockedErr != nil {
			return "", fmt.Errorf("install the new binary: %w; removing the fixed %s also failed: %v (a running ghost mcp client may hold the old binary, and restarting the client releases it)", err, fixedAside, lockedErr)
		}
		return "", fmt.Errorf("install the new binary: %w", err)
	}

	var notes []string
	if lockedErr != nil {
		notes = append(notes, fmt.Sprintf("could not remove %s: %v (a running ghost mcp client may hold the old binary, and restarting the client releases it)", fixedAside, lockedErr))
	}
	_ = removeFile(aside) //nolint:errcheck
	sweepOldAsides(targetPath)
	return strings.Join(notes, "; "), nil
}

func sweepOldAsides(targetPath string) {
	dir := filepath.Dir(targetPath)
	base := filepath.Base(targetPath)
	prefix := base + asideSuffix + "-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		suffix := name[len(prefix):]
		if suffix == "" {
			continue
		}
		digits := true
		for i := 0; i < len(suffix); i++ {
			if suffix[i] < '0' || suffix[i] > '9' {
				digits = false
				break
			}
		}
		if !digits {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		_ = removeFile(filepath.Join(dir, name))
	}
}

const maxSymlinkHops = 32

func resolveSymlinks(path string) (string, error) {
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
			return current, nil
		}

		target, err := os.Readlink(current)
		if err != nil {
			return "", fmt.Errorf("readlink %s: %w", current, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dirOf(current), target)
		}
		current = target
	}
}

func dirOf(path string) string {
	return filepath.Dir(path)
}
