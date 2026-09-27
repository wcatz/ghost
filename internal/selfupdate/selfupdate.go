// Package selfupdate provides self-update capability for ghost binaries
// by downloading releases from GitHub.
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

// The two request deadlines. Both are vars so a test can shorten one to
// something shorter than a test can wait (see useTestDeadline); production
// never reassigns them.
//
// The bound is a context deadline carried by the request, not a
// http.Client.Timeout. Both would bound the same thing, but a deadline on the
// context travels with the request: the caller can shorten it or cancel it
// outright (a cancelled upgrade should stop waiting on a socket), and one
// mechanism cannot drift away from the other the way two bounds can.
var (
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
	// maxBinaryBytes bounds what one archive inflates into. The transfer cap
	// above bounds what arrives, which says nothing about how far that expands:
	// today's Windows binary is ~24 MiB inside a ~9 MiB zip, so a
	// decompression bomb is a small download with an unbounded result. For a
	// .tar.gz the bound covers every entry, since opening one inflates the
	// entries ahead of the binary too; 128 MiB is several times the ~23 MiB a
	// real release inflates to.
	maxBinaryBytes int64 = 128 << 20
)

// The caps are vars only so a test can lower them and prove each one holds
// without pushing hundreds of MiB through a socket or a decompressor.
var (
	archiveCap = maxArchiveBytes
	binaryCap  = maxBinaryBytes
)

// httpClient is the package's own client. It carries no Timeout: every
// request here is built with an explicit context deadline instead (see
// fetchRelease and Download), which is the only bound that also covers reading
// the body once the response headers have arrived.
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

// fetchRelease is LatestRelease against an explicit URL, so tests can point it
// at a local server instead of GitHub.
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
	// The deadline has to outlive this function — the caller reads the body
	// after Download returns — so it is ended by Close rather than deferred.
	return &deadlineBody{ReadCloser: resp.Body, cancel: cancel}, nil
}

// deadlineBody releases the request's context when the body is closed, so the
// download deadline governs the whole transfer and nothing is left holding a
// timer after the caller is done with it.
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

// sha256HexLen is the length of a hex-encoded SHA-256 digest.
const sha256HexLen = 2 * sha256.Size

// VerifyAssetDigest checks that data matches the digest GitHub reports for this
// release asset.
//
// The digest is the one GitHub computed for the asset it actually holds, taken
// from the releases API response rather than from a manifest uploaded next to
// it. That distinction is the whole point: checksums.txt is a second file in
// the same release, so whoever can replace the archive can replace the manifest
// that vouches for it, and a checksum comparison against it proves only that
// the two files agree.
//
// A missing digest is a refusal rather than a pass. An asset nothing vouched
// for is precisely the case that must not install, and GitHub computes a digest
// for every asset it holds — a response without one is not something this check
// can reason about, so the install stops instead of continuing on the strength
// of the manifest alone.
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
		// Not "skip the check". A digest this binary cannot compute is an
		// asset it cannot vouch for, and an algorithm it does not know is
		// also how a downgrade of the check would arrive. Case is not that:
		// the value compared below is the same digest either way.
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

// zipMagic is the first two bytes of any zip container — local file header,
// central directory and end-of-archive marker all begin "PK".
var zipMagic = []byte("PK")

// ExtractBinary extracts the ghost binary from a release archive: the .tar.gz
// every unix release ships, or the .zip every Windows release ships
// (goreleaser's format_overrides). Before #558 the zip was handed to a gzip
// reader, so `ghost upgrade` failed with "gzip: invalid header" on one of the
// two platforms ghost builds for.
//
// The container decides how to read the archive rather than the asset name, so
// a mislabelled asset still reads as what it is and a renamed one cannot
// present a different format to the parser.
//
// The cap bounds what the archive inflates into, not just the bytes that are
// kept: the transfer cap limits what arrived, which says nothing about how far
// one archive expands, and an uncapped read here was the last place a small
// hostile archive could spend unbounded memory and CPU.
func ExtractBinary(archive []byte) ([]byte, error) {
	if bytes.HasPrefix(archive, zipMagic) {
		return extractZipBinary(archive)
	}
	return extractTarGzBinary(archive)
}

// extractTarGzBinary reads the binary out of a gzipped tar. The binary is at
// the archive root, named "ghost" or "ghost.exe".
func extractTarGzBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close() //nolint:errcheck

	// Bounded on the way out, not only on what is kept. archive/tar reads an
	// entry in full to reach the next header — a gzip stream is not seekable,
	// so Next() copies the skipped body to io.Discard — and the entries ahead
	// of the binary are real (LICENSE, README.md). A cap on the returned bytes
	// alone would leave a few hundred KiB of compressible data in any other
	// entry free to expand without limit, spending CPU that no deadline here
	// can stop: the archive is already downloaded and in memory by now.
	tr := tar.NewReader(&inflatedReader{r: gz, limit: binaryCap})
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tar: %w", err)
		}
		if hdr.Name == "ghost" || hdr.Name == "ghost.exe" {
			return io.ReadAll(tr)
		}
	}
	return nil, fmt.Errorf("ghost binary not found in archive")
}

// inflatedReader fails once the stream it wraps has produced more than limit
// bytes. It is the bound on what an archive costs to open, whether or not the
// bytes are kept: gzip expands as it is read, so the work is done before
// anything can decide whether it wanted the result.
type inflatedReader struct {
	r     io.Reader
	limit int64
	read  int64
}

func (c *inflatedReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.read > c.limit {
		// n discarded along with the rest: the archive is being refused, so
		// the bytes this call produced have no further reader.
		return 0, fmt.Errorf("the release archive expands past the %d-byte cap", c.limit)
	}
	return n, err
}

// extractZipBinary reads the binary out of a zip. Only root entries count:
// goreleaser puts the binary at the archive root, so an entry that could only
// be reached by traversing out of the archive is not a file ghost published.
//
// A zip is random-access, so an entry this loop skips is never inflated at
// all; the one entry it opens is bounded directly, by the same cap.
func extractZipBinary(archive []byte) ([]byte, error) {
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("zip: %w", err)
	}
	for _, f := range zr.File {
		if f.FileInfo().IsDir() || (f.Name != "ghost" && f.Name != "ghost.exe") {
			continue
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
		return binary, nil
	}
	return nil, fmt.Errorf("ghost binary not found in archive")
}

// asideSuffix is the fixed name the binary being replaced moves to when the
// platform cannot rename over it. Fixed rather than unique because the file
// cannot be deleted until this process exits, so a per-attempt name would
// leave one file behind per upgrade.
const asideSuffix = ".old"

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
	if err := installNewBinary(tmpPath, resolved, asideRunningTarget); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

// installNewBinary puts the staged file at targetPath.
//
// asideFirst is the platform policy, and it is the only build-tagged decision
// in the install path. On Windows the loader holds a running executable's
// image open without delete sharing, so renaming over it fails with "Access is
// denied" — the command whose whole job is repairing an install could never
// repair a Windows one. Windows does allow renaming that image, so the old
// binary moves aside first and the staged file then takes a path nothing holds.
// Unix renames atomically over the target, and doing the same dance there would
// only open a window in which the install path does not exist.
func installNewBinary(tmpPath, targetPath string, asideFirst bool) error {
	if !asideFirst {
		return os.Rename(tmpPath, targetPath)
	}

	aside := targetPath + asideSuffix
	// Best effort: a leftover from a previous upgrade is this same name, and
	// reusing it is the point. If the file is genuinely in the way, the rename
	// below reports that with a real error instead of guessing.
	_ = os.Remove(aside)
	if err := os.Rename(targetPath, aside); err != nil {
		return fmt.Errorf("move the current binary aside: %w", err)
	}

	if err := os.Rename(tmpPath, targetPath); err != nil {
		// Put the old binary back before reporting. An upgrade that fails
		// with no binary at the install path is worse than one that never
		// started, so the restore is attempted even though it can fail.
		if restoreErr := os.Rename(aside, targetPath); restoreErr != nil {
			return fmt.Errorf("install the new binary: %w (the previous binary is at %s and could not be moved back: %v)", err, aside, restoreErr)
		}
		return fmt.Errorf("install the new binary: %w", err)
	}

	// Removed when the platform allows it. On Windows the aside is the image
	// this process is still running from, so the delete fails until the
	// process exits and the file waits here for the next upgrade to reuse it.
	_ = os.Remove(aside) //nolint:errcheck
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
