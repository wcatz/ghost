package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/selfupdate"
)

func TestDecideUpgrade(t *testing.T) {
	tests := []struct {
		name            string
		running, latest string
		allowDowngrade  bool
		want            upgradeOutcome
	}{
		{name: "same release is already current", running: "0.32.0", latest: "0.32.0", want: upgradeCurrent},
		{name: "newer patch proceeds", running: "0.32.0", latest: "0.32.1", want: upgradeProceed},
		{name: "newer minor proceeds", running: "0.32.0", latest: "0.33.0", want: upgradeProceed},
		{name: "newer major proceeds", running: "0.32.0", latest: "1.0.0", want: upgradeProceed},
		// The three refusal cases are the guard itself.
		{name: "older patch is refused", running: "0.32.1", latest: "0.32.0", want: upgradeRefuseDowngrade},
		{name: "older minor is refused", running: "0.33.0", latest: "0.32.0", want: upgradeRefuseDowngrade},
		{name: "older major is refused", running: "1.0.0", latest: "0.32.0", want: upgradeRefuseDowngrade},
		// String comparison calls all three of these a downgrade (or an
		// upgrade); only the last one is one.
		{name: "0.10.0 is newer than 0.9.0", running: "0.9.0", latest: "0.10.0", want: upgradeProceed},
		{name: "10.0.0 is newer than 9.0.0", running: "9.0.0", latest: "10.0.0", want: upgradeProceed},
		{name: "a v-prefixed tag is not a different release", running: "0.32.0", latest: "v0.32.0", want: upgradeCurrent},
		{name: "a final release supersedes its rc", running: "0.33.0-rc.1", latest: "0.33.0", want: upgradeProceed},
		{name: "an rc is older than its final release", running: "0.33.0", latest: "0.33.0-rc.1", want: upgradeRefuseDowngrade},
		// An unorderable version keeps the pre-guard behaviour: go ahead, and
		// let the checksum decide.
		{name: "dev build proceeds against a real release", running: "dev", latest: "0.32.0", want: upgradeProceed},
		{name: "dev build is never blocked", running: "dev", latest: "0.0.1", want: upgradeProceed},
		{name: "empty running version proceeds", running: "", latest: "0.32.0", want: upgradeProceed},
		{name: "unorderable release tag proceeds", running: "0.32.0", latest: "nightly", want: upgradeProceed},
		// Unorderable but identical is still the release already installed,
		// flag or no flag: --allow-downgrade is about direction, not about
		// re-installing what is already installed.
		{name: "identical unorderable version is current", running: "nightly", latest: "nightly", want: upgradeCurrent},
		{name: "identical unorderable version with v prefix is current", running: "nightly", latest: "vnightly", want: upgradeCurrent},
		// --allow-downgrade is the escape hatch for a release that was
		// withdrawn: it turns each refusal into a deliberate install, and
		// changes nothing else.
		{name: "--allow-downgrade permits an older patch", running: "0.32.1", latest: "0.32.0", allowDowngrade: true, want: upgradeProceed},
		{name: "--allow-downgrade permits an older major", running: "1.0.0", latest: "0.32.0", allowDowngrade: true, want: upgradeProceed},
		{name: "--allow-downgrade permits an older rc", running: "0.33.0", latest: "0.33.0-rc.1", allowDowngrade: true, want: upgradeProceed},
		{name: "--allow-downgrade is not needed for an upgrade", running: "0.32.0", latest: "0.33.0", allowDowngrade: true, want: upgradeProceed},
		// The flag is about direction, not about skipping the comparison
		// entirely: already being on the release is still "up to date", so a
		// short-circuit that returned proceed whenever the flag is set would
		// reinstall the same build.
		{name: "--allow-downgrade does not reinstall the release you have", running: "0.33.0", latest: "0.33.0", allowDowngrade: true, want: upgradeCurrent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideUpgrade(tt.running, tt.latest, tt.allowDowngrade); got != tt.want {
				t.Errorf("decideUpgrade(%q, %q, %v) = %v, want %v", tt.running, tt.latest, tt.allowDowngrade, got, tt.want)
			}
		})
	}
}

func TestDowngradeMessageNamesBothVersions(t *testing.T) {
	msg := downgradeMessage("0.33.0", "0.32.0")
	for _, want := range []string{"0.33.0", "0.32.0", "downgrade", "--allow-downgrade"} {
		if !strings.Contains(msg, want) {
			t.Errorf("downgrade message %q should mention %q", msg, want)
		}
	}
}

func TestParseUpgradeArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want upgradeOptions
	}{
		{name: "no arguments", args: nil, want: upgradeOptions{}},
		{name: "the downgrade opt-in", args: []string{"--allow-downgrade"}, want: upgradeOptions{allowDowngrade: true}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUpgradeArgs(tt.args)
			if err != nil {
				t.Fatalf("parseUpgradeArgs(%v): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parseUpgradeArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// TestParseUpgradeArgsRejectsAnythingElse keeps a mistyped flag from reading as
// consent. --allow-down, -allow-downgrade and --allow-downgrade=false would
// each otherwise be ignored, and the downgrade refused with a message the user
// cannot connect to what they typed.
func TestParseUpgradeArgsRejectsAnythingElse(t *testing.T) {
	for _, arg := range []string{"--allow-down", "-allow-downgrade", "--allow-downgrade=false", "--apply", "upgrade", ""} {
		if _, err := parseUpgradeArgs([]string{arg}); err == nil {
			t.Errorf("parseUpgradeArgs(%q) accepted an argument it does not implement", arg)
		}
	}
}

// upgradeFixture is a release whose every download is served by a local
// httptest server, so the install path can be exercised end to end without a
// network or the binary the test runner is executing. Every digest agrees
// until a test deliberately breaks one.
type upgradeFixture struct {
	release  *selfupdate.Release
	asset    *selfupdate.Asset
	binary   []byte
	payloads *releasePayloads
}

// releasePayloads is what the fixture's server hands out, read at request time
// so a test can substitute what the release publishes without a second server.
// The lock is not decoration: httptest answers each request on its own
// goroutine, a test rewrites these values from the goroutine running the
// command under test, and nothing orders the two — CI runs this package under
// -race, which reports the pair as a data race.
type releasePayloads struct {
	mu       sync.Mutex
	manifest string
	archive  []byte
}

func (p *releasePayloads) setManifest(manifest string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.manifest = manifest
}

func (p *releasePayloads) manifestText() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.manifest
}

func (p *releasePayloads) setArchive(archive []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.archive = archive
}

func (p *releasePayloads) archiveBytes() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.archive
}

func newUpgradeFixture(t *testing.T, binary []byte) *upgradeFixture {
	t.Helper()

	const version = "0.33.0"
	assetName := selfupdate.AssetName(version)
	archive := releaseArchive(t, assetName, binary)
	digest := sha256HexDigest(archive)
	payloads := &releasePayloads{
		manifest: digest + "  " + assetName + "\n",
		archive:  archive,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/checksums.txt", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, payloads.manifestText())
	})
	mux.HandleFunc("/archive", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payloads.archiveBytes())
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	rel := &selfupdate.Release{
		TagName: "v" + version,
		Assets: []selfupdate.Asset{
			{Name: "checksums.txt", BrowserDownloadURL: srv.URL + "/checksums.txt"},
			{
				Name:               assetName,
				BrowserDownloadURL: srv.URL + "/archive",
				Digest:             "sha256:" + digest,
			},
		},
	}
	asset, err := selfupdate.FindAsset(rel)
	if err != nil {
		t.Fatalf("FindAsset: %v", err)
	}
	return &upgradeFixture{release: rel, asset: asset, binary: binary, payloads: payloads}
}

func sha256HexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// releaseArchive builds the archive a release of this name would carry: a
// .tar.gz everywhere except Windows, which goreleaser ships as a .zip.
func releaseArchive(t *testing.T, assetName string, binary []byte) []byte {
	t.Helper()
	if !strings.HasSuffix(assetName, ".zip") {
		return tarGzArchive(t, binary)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("ghost.exe")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "ghost", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestInstallReleaseInstallsTheVerifiedArchive is the happy path, and the one
// that has to keep working: both published digests agree, so the binary is
// extracted and handed to the installer.
func TestInstallReleaseInstallsTheVerifiedArchive(t *testing.T) {
	binary := []byte("pretend executable")
	fx := newUpgradeFixture(t, binary)

	var installed []byte
	calls := 0
	err := installRelease(context.Background(), io.Discard, fx.release, fx.asset, func(b []byte) error {
		calls++
		installed = b
		return nil
	})
	if err != nil {
		t.Fatalf("installRelease: %v", err)
	}
	if calls != 1 {
		t.Fatalf("installer ran %d times, want 1", calls)
	}
	if !bytes.Equal(installed, binary) {
		t.Errorf("installed %q, want the archive's binary %q", installed, binary)
	}
}

// TestInstallReleaseRefusesBeforeInstalling is the security property of #558.
// Whatever the release says about the archive, an archive whose bytes do not
// match what the release vouched for must not reach the file that replaces the
// running binary. Each case is a different way that vouching can fail: GitHub
// reports a digest for other bytes, reports none at all, reports one this
// binary cannot check, or the uploaded manifest disagrees.
func TestInstallReleaseRefusesBeforeInstalling(t *testing.T) {
	binary := []byte("pretend executable")
	otherSum := sha256.Sum256([]byte("an attacker's archive"))
	otherHex := hex.EncodeToString(otherSum[:])

	tests := []struct {
		name    string
		corrupt func(fx *upgradeFixture)
	}{
		{
			// The substituted archive with checksums.txt swapped to match it:
			// the only defence left is the digest GitHub computed for the
			// asset it actually holds.
			name: "the asset digest is for other bytes",
			corrupt: func(fx *upgradeFixture) {
				fx.asset.Digest = "sha256:" + otherHex
			},
		},
		{
			name: "the release reports no digest",
			corrupt: func(fx *upgradeFixture) {
				fx.asset.Digest = ""
			},
		},
		{
			name: "the digest uses an algorithm ghost cannot check",
			corrupt: func(fx *upgradeFixture) {
				fx.asset.Digest = "sha512:" + strings.Repeat("a", 128)
			},
		},
		{
			// Both digests are genuine, but for a different file: the bytes
			// that arrived are not the ones that were released.
			name: "the manifest is for other bytes",
			corrupt: func(fx *upgradeFixture) {
				fx.payloads.setManifest(otherHex + "  " + fx.asset.Name + "\n")
			},
		},
		{
			name: "the manifest has no entry for the asset",
			corrupt: func(fx *upgradeFixture) {
				fx.payloads.setManifest(otherHex + "  some_other_asset.tar.gz\n")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newUpgradeFixture(t, binary)
			tt.corrupt(fx)

			var installed []byte
			calls := 0
			err := installRelease(context.Background(), io.Discard, fx.release, fx.asset, func(b []byte) error {
				calls++
				installed = b
				return nil
			})
			if err == nil {
				t.Fatal("expected a refusal, but the archive was accepted")
			}
			if calls != 0 {
				t.Errorf("the installer ran %d time(s) with %q after a refused verification; nothing unverified may reach the binary", calls, installed)
			}
		})
	}
}

// TestInstallReleaseVerifiesTheDigestBeforeParsingTheArchive pins the order,
// which is the part of the property the refusals above cannot see. The server
// here returns bytes that are not an archive at all, and a manifest that
// correctly vouches for them: the manifest check passes, so only a digest check
// that runs first can refuse. Without the order, a substituted release gets
// parsed by ghost before anything has vouched for it.
func TestInstallReleaseVerifiesTheDigestBeforeParsingTheArchive(t *testing.T) {
	notAnArchive := []byte("substituted bytes, not an archive")
	fx := newUpgradeFixture(t, []byte("pretend executable"))

	// The manifest vouches for the substituted bytes, and the reported digest
	// still vouches for the release's real archive: exactly one of the two
	// can pass.
	fx.payloads.setManifest(sha256HexDigest(notAnArchive) + "  " + fx.asset.Name + "\n")
	fx.payloads.setArchive(notAnArchive)

	err := installRelease(context.Background(), io.Discard, fx.release, fx.asset, func([]byte) error {
		t.Error("the installer ran for an archive that was never verified")
		return nil
	})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "digest") {
		t.Errorf("error %q should name the digest check, so the archive is never parsed before something vouches for it", err)
	}
	for _, parseFailure := range []string{"gzip: ", "zip: ", "tar: ", "ghost binary not found in archive"} {
		if strings.Contains(err.Error(), parseFailure) {
			t.Errorf("error %q came from parsing the archive, which must happen only after the digest agrees", err)
		}
	}
}

// TestInstallReleaseRefusesAMissingManifest keeps the fail-closed property of
// the published checksum: a release with no checksums.txt is not one ghost
// publishes, and treating it as "nothing to check" would skip a check. The
// refusal has to be the missing-asset one rather than any error at all, or a
// later download failure would pass for the check having happened.
func TestInstallReleaseRefusesAMissingManifest(t *testing.T) {
	fx := newUpgradeFixture(t, []byte("pretend executable"))
	fx.release.Assets = []selfupdate.Asset{*fx.asset}

	calls := 0
	err := installRelease(context.Background(), io.Discard, fx.release, fx.asset, func([]byte) error {
		calls++
		return nil
	})
	if err == nil {
		t.Fatal("expected a refusal for a release with no checksums.txt")
	}
	if !strings.Contains(err.Error(), "no checksums.txt asset") {
		t.Errorf("error %q should report the missing manifest itself, not a later failure", err)
	}
	if calls != 0 {
		t.Errorf("the installer ran %d time(s) for a release with no checksum manifest", calls)
	}
}

// TestInstallReleaseReportsAnInstallerFailure keeps the last step's error
// visible instead of reporting a successful upgrade that never happened.
func TestInstallReleaseReportsAnInstallerFailure(t *testing.T) {
	fx := newUpgradeFixture(t, []byte("pretend executable"))

	want := "the target directory is read-only"
	err := installRelease(context.Background(), io.Discard, fx.release, fx.asset, func([]byte) error {
		return errors.New(want)
	})
	if err == nil {
		t.Fatal("expected the installer's error to be reported")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should carry the installer's reason", err)
	}
}
