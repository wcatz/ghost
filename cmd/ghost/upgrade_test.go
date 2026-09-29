package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/selfupdate"
)

func TestDecideUpgrade(t *testing.T) {
	tests := []struct {
		name            string
		running, latest string
		opts            upgradeOptions
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
		{
			name: "--allow-downgrade permits an older patch", running: "0.32.1", latest: "0.32.0",
			opts: upgradeOptions{allowDowngrade: true}, want: upgradeProceed,
		},
		{
			name: "--allow-downgrade permits an older major", running: "1.0.0", latest: "0.32.0",
			opts: upgradeOptions{allowDowngrade: true}, want: upgradeProceed,
		},
		{
			name: "--allow-downgrade is not needed for an upgrade", running: "0.32.0", latest: "0.33.0",
			opts: upgradeOptions{allowDowngrade: true}, want: upgradeProceed,
		},
		{
			// The flag is about direction, not about skipping the comparison
			// entirely: already being on the release is still "up to date", so
			// a short-circuit that returned proceed whenever the flag was set
			// would reinstall the same build.
			name: "--allow-downgrade does not reinstall the release you have", running: "0.33.0", latest: "0.33.0",
			opts: upgradeOptions{allowDowngrade: true}, want: upgradeCurrent,
		},
		// A prerelease is refused whatever its direction. releases/latest
		// answers only with final releases, so nothing reaches these cases
		// from a plain `ghost upgrade` today; the guard is here so the refusal
		// is the command's own decision rather than a property of which
		// endpoint it happens to ask, and so a tag newer than the installed
		// build is not installed as though it were a release.
		{name: "a newer rc is refused", running: "0.33.0", latest: "0.34.0-rc.1", want: upgradeRefusePrerelease},
		{name: "an older rc is refused as a prerelease, not a downgrade", running: "0.33.0", latest: "0.33.0-rc.1", want: upgradeRefusePrerelease},
		{name: "a beta is refused", running: "0.33.0", latest: "0.34.0-beta", want: upgradeRefusePrerelease},
		{name: "a v-prefixed rc is refused", running: "0.33.0", latest: "v0.34.0-rc.1", want: upgradeRefusePrerelease},
		{
			// The two flags are independent. A prerelease is refused on what it
			// is, so a direction opt-in does not reach it: whatever the
			// numbers say, an unfinished release is not installed as a release.
			name: "--allow-downgrade does not permit a prerelease", running: "0.33.0", latest: "0.34.0-rc.1",
			opts: upgradeOptions{allowDowngrade: true}, want: upgradeRefusePrerelease,
		},
		{
			name: "--allow-prerelease permits a newer rc", running: "0.33.0", latest: "0.34.0-rc.1",
			opts: upgradeOptions{allowPrerelease: true}, want: upgradeProceed,
		},
		{
			// A prerelease that is also older needs both, and in this order:
			// the first to install something unfinished at all, the second to
			// install something older.
			name: "--allow-prerelease alone does not permit an older rc", running: "0.33.0", latest: "0.33.0-rc.1",
			opts: upgradeOptions{allowPrerelease: true}, want: upgradeRefuseDowngrade,
		},
		{
			name: "both flags permit an older rc", running: "0.33.0", latest: "0.33.0-rc.1",
			opts: upgradeOptions{allowPrerelease: true, allowDowngrade: true}, want: upgradeProceed,
		},
		{
			// Still a direction question, not a "skip the comparison" one.
			name: "--allow-prerelease does not reinstall the rc you have", running: "0.34.0-rc.1", latest: "0.34.0-rc.1",
			opts: upgradeOptions{allowPrerelease: true}, want: upgradeCurrent,
		},
		{
			// A tag this package cannot order is not a prerelease: nothing says
			// it is a candidate release, and refusing it would block a build
			// tagged something else entirely.
			name: "an unorderable tag is not treated as a prerelease", running: "0.32.0", latest: "nightly-rc.1", want: upgradeProceed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideUpgrade(tt.running, tt.latest, tt.opts); got != tt.want {
				t.Errorf("decideUpgrade(%q, %q, %+v) = %v, want %v", tt.running, tt.latest, tt.opts, got, tt.want)
			}
		})
	}
}

// TestRefusalMessagesAreDistinguishable keeps the two refusals telling the user
// which one happened, because the flags are different: a prerelease needs
// --allow-prerelease and a downgrade needs --allow-downgrade, so a message
// naming the wrong flag would send the user to re-run into the same refusal.
func TestRefusalMessagesAreDistinguishable(t *testing.T) {
	prerelease := prereleaseMessage("0.34.0-rc.1")
	for _, want := range []string{"0.34.0-rc.1", "prerelease", "--allow-prerelease"} {
		if !strings.Contains(prerelease, want) {
			t.Errorf("prerelease message %q should mention %q", prerelease, want)
		}
	}
	if strings.Contains(prerelease, "downgrade") {
		t.Errorf("prerelease message %q names the wrong refusal: a downgrade is a different guard with a different flag", prerelease)
	}

	downgrade := downgradeMessage("0.33.0", "0.32.0")
	for _, want := range []string{"0.33.0", "0.32.0", "downgrade", "--allow-downgrade"} {
		if !strings.Contains(downgrade, want) {
			t.Errorf("downgrade message %q should mention %q", downgrade, want)
		}
	}
	if strings.Contains(downgrade, "prerelease") {
		t.Errorf("downgrade message %q names the wrong refusal", downgrade)
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
		{name: "the prerelease opt-in", args: []string{"--allow-prerelease"}, want: upgradeOptions{allowPrerelease: true}},
		{name: "the attestation opt-in", args: []string{"--allow-unattested"}, want: upgradeOptions{allowUnattested: true}},
		{
			name: "both opt-ins",
			args: []string{"--allow-prerelease", "--allow-downgrade"},
			want: upgradeOptions{allowPrerelease: true, allowDowngrade: true},
		},
		{
			// The three are independent, so passing all three is a legal
			// command line rather than a contradiction. Whether installing an
			// older, prerelease AND unattested release is a good idea is the
			// user's call; the parser's job is to report what they asked for.
			name: "all three opt-ins",
			args: []string{"--allow-prerelease", "--allow-downgrade", "--allow-unattested"},
			want: upgradeOptions{allowPrerelease: true, allowDowngrade: true, allowUnattested: true},
		},
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
// consent. --allow-down, -allow-downgrade, --allow-downgrade=false and
// --allow-pre would each otherwise be ignored, and the refusal would come back
// with a message the user cannot connect to what they typed.
func TestParseUpgradeArgsRejectsAnythingElse(t *testing.T) {
	for _, arg := range []string{
		"--allow-down", "-allow-downgrade", "--allow-downgrade=false",
		"--allow-pre", "--allow-prerelease=true", "--allow-unatteste",
		"--allow-unattested=true", "--apply", "upgrade", "",
	} {
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
	err := installRelease(context.Background(), discardStreams(), fx.release, fx.asset, upgradeOptions{}, upgradeDeps{install: func(b []byte) error {
		calls++
		installed = b
		return nil
	}})
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
			err := installRelease(context.Background(), discardStreams(), fx.release, fx.asset, upgradeOptions{}, upgradeDeps{install: func(b []byte) error {
				calls++
				installed = b
				return nil
			}})
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

	err := installRelease(context.Background(), discardStreams(), fx.release, fx.asset, upgradeOptions{}, upgradeDeps{install: func([]byte) error {
		t.Error("the installer ran for an archive that was never verified")
		return nil
	}})
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
	err := installRelease(context.Background(), discardStreams(), fx.release, fx.asset, upgradeOptions{}, upgradeDeps{install: func([]byte) error {
		calls++
		return nil
	}})
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
	err := installRelease(context.Background(), discardStreams(), fx.release, fx.asset, upgradeOptions{}, upgradeDeps{install: func([]byte) error {
		return errors.New(want)
	}})
	if err == nil {
		t.Fatal("expected the installer's error to be reported")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q should carry the installer's reason", err)
	}
}

// The paths a release is served under by releaseServer, chosen to be the same
// shape as the real ones so a fixture cannot drift into a shape the real client
// would never ask for.
const (
	releaseMetadataPath = "/repos/wcatz/ghost/releases/latest"
	checksumAssetPath   = "/checksums.txt"
	archiveAssetPath    = "/archive"
)

// releaseServer is a fake GitHub: the release metadata, the checksum manifest
// and this platform's archive, all from one local server, with a record of what
// was asked for. The record is what makes "refused before anything was
// downloaded" assertable, and the per-path delay is what a total deadline has to
// cut.
type releaseServer struct {
	*httptest.Server

	mu       sync.Mutex
	tag      string
	asset    selfupdate.Asset
	checksum selfupdate.Asset
	manifest string
	archive  []byte
	delays   map[string]time.Duration
	asked    []string
}

// newReleaseServer serves a release tagged tag that carries binary on this
// platform, with every published digest agreeing until a test breaks one.
func newReleaseServer(t *testing.T, tag string, binary []byte) *releaseServer {
	t.Helper()

	version := strings.TrimPrefix(tag, "v")
	assetName := selfupdate.AssetName(version)
	archive := releaseArchive(t, assetName, binary)
	digest := sha256HexDigest(archive)

	rs := &releaseServer{
		tag:      tag,
		asset:    selfupdate.Asset{Name: assetName, Digest: "sha256:" + digest},
		checksum: selfupdate.Asset{Name: "checksums.txt"},
		manifest: digest + "  " + assetName + "\n",
		archive:  archive,
		delays:   map[string]time.Duration{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		rs.serve(w, r)
	})
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)

	// The asset URLs are only known once the server has a URL, so they are
	// filled in here rather than in the constructor.
	rs.asset.BrowserDownloadURL = rs.URL + archiveAssetPath
	rs.checksum.BrowserDownloadURL = rs.URL + checksumAssetPath
	return rs
}

func (rs *releaseServer) serve(w http.ResponseWriter, r *http.Request) {
	rs.mu.Lock()
	rs.asked = append(rs.asked, r.URL.Path)
	delay := rs.delays[r.URL.Path]
	manifest, archive := rs.manifest, rs.archive
	metadata := fmt.Sprintf(
		`{"tag_name":%q,"assets":[{"name":%q,"browser_download_url":%q,"digest":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
		rs.tag, rs.asset.Name, rs.asset.BrowserDownloadURL, rs.asset.Digest, rs.checksum.BrowserDownloadURL)
	rs.mu.Unlock()

	if delay > 0 {
		// Answering after the client has hung up is not a failure of the test:
		// a bounded client stops reading, and this handler exists to be cut
		// off mid-answer.
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	switch r.URL.Path {
	case releaseMetadataPath:
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, metadata)
	case checksumAssetPath:
		_, _ = io.WriteString(w, manifest)
	case archiveAssetPath:
		_, _ = w.Write(archive)
	default:
		http.NotFound(w, r)
	}
}

// fetch is the release lookup a test's run performs: a real request to the fake
// release, over the same socket every other download uses and under the same
// context. That is what makes askedFor a record of what the command really
// requested, and it leaves the lookup with no bound of its own — the budget is
// the only deadline on this call, which is the property under test.
func (rs *releaseServer) fetch(ctx context.Context) (*selfupdate.Release, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rs.URL+releaseMetadataPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fake release metadata returned %d", resp.StatusCode)
	}
	var rel selfupdate.Release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	return &rel, nil
}

// setDigest replaces the digest the release reports for its archive. It takes
// the lock because the server handler reads the same field on its own goroutine,
// and CI runs this package under -race.
func (rs *releaseServer) setDigest(digest string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.asset.Digest = digest
}

// askedFor lists the paths served so far, in order.
func (rs *releaseServer) askedFor() []string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]string(nil), rs.asked...)
}

// setDelay makes the handler for path answer only after d.
func (rs *releaseServer) setDelay(path string, d time.Duration) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.delays[path] = d
}

// swapArchive replaces the bytes served for the archive without touching
// anything the release says about them, which is exactly what a substituted
// release looks like from the client's side.
func (rs *releaseServer) swapArchive(other []byte) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.archive = other
}

// swapManifest replaces the bytes served for checksums.txt.
func (rs *releaseServer) swapManifest(text string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.manifest = text
}

// installedGhost stands in for the binary being replaced: a real file in a real
// directory, so a test can read back exactly what an upgrade left there and
// whether it left anything else behind.
func installedGhost(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ghost")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write the installed binary: %v", err)
	}
	return path
}

// installOver wires performUpgrade to a real selfupdate.Replace against target,
// so a test sees the file an upgrade produced rather than a recorded argument.
func installOver(target string) func([]byte) error {
	return func(binary []byte) error { return selfupdate.Replace(target, binary) }
}

// assertUnchanged checks that a refused upgrade left the installed binary
// exactly as it was, with nothing else in its directory: no staged file, and no
// aside. A refusal that wrote anything at all has already started replacing the
// binary it was supposed to be protecting.
func assertUnchanged(t *testing.T, target, want string) {
	t.Helper()
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read the installed binary: %v", err)
	}
	if string(got) != want {
		t.Errorf("installed binary holds %q, want the original %q", got, want)
	}
	entries, err := os.ReadDir(filepath.Dir(target))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != filepath.Base(target) {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the install directory holds %v, want only %s: a refused upgrade must leave no debris", names, filepath.Base(target))
	}
}

// TestPerformUpgradeRefusesADowngrade drives the whole command against a fake
// release, and the refusal has to happen before the archive is even fetched: a
// downgrade that downloads first and decides afterwards has already spent the
// transfer and, on a machine where the replace is attempted before the compare,
// the install.
func TestPerformUpgradeRefusesADowngrade(t *testing.T) {
	rs := newReleaseServer(t, "v0.33.0", []byte("the newer binary"))
	target := installedGhost(t, "the installed binary")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	installed, err := performUpgrade(ctx, discardStreams(), "0.34.0", upgradeOptions{}, upgradeDeps{
		fetch:   rs.fetch,
		install: installOver(target),
	})
	if err == nil {
		t.Fatalf("expected a refusal, but the downgrade installed %q", installed)
	}
	if !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Errorf("error %q should name the flag that permits a downgrade", err)
	}
	if got := rs.askedFor(); len(got) != 1 || got[0] != releaseMetadataPath {
		t.Errorf("the server was asked for %v, want only the release metadata: nothing may be downloaded for a refused downgrade", got)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeRefusesAPrereleaseUnlessAsked is the same shape for the
// other refusal, and it checks the opt-in is what turns it into an install —
// including that the install says on stderr that a prerelease is what was
// asked for, because the line below it reads like an ordinary upgrade.
func TestPerformUpgradeRefusesAPrereleaseUnlessAsked(t *testing.T) {
	t.Run("refused by default", func(t *testing.T) {
		rs := newReleaseServer(t, "v0.34.0-rc.1", []byte("the release candidate"))
		target := installedGhost(t, "the installed binary")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		streams := discardStreams()
		_, err := performUpgrade(ctx, streams, "0.33.0", upgradeOptions{}, upgradeDeps{
			fetch:   rs.fetch,
			install: installOver(target),
		})
		if err == nil {
			t.Fatal("expected a refusal: a prerelease is not a release")
		}
		if !strings.Contains(err.Error(), "--allow-prerelease") {
			t.Errorf("error %q should name the flag that permits a prerelease", err)
		}
		if got := rs.askedFor(); len(got) != 1 || got[0] != releaseMetadataPath {
			t.Errorf("the server was asked for %v, want only the release metadata: nothing may be downloaded for a refused prerelease", got)
		}
		assertUnchanged(t, target, "the installed binary")
	})

	t.Run("installed with --allow-prerelease", func(t *testing.T) {
		rs := newReleaseServer(t, "v0.34.0-rc.1", []byte("the release candidate"))
		target := installedGhost(t, "the installed binary")

		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		var out, errOut bytes.Buffer
		installed, err := performUpgrade(ctx, upgradeStreams{out: &out, err: &errOut}, "0.33.0",
			upgradeOptions{allowPrerelease: true}, upgradeDeps{
				fetch:   rs.fetch,
				install: installOver(target),
			})
		if err != nil {
			t.Fatalf("performUpgrade with --allow-prerelease: %v", err)
		}
		if installed != "0.34.0-rc.1" {
			t.Errorf("installed version = %q, want 0.34.0-rc.1", installed)
		}
		// Stated as intent, not as a completed act: this line is written before
		// the download and both digest checks, so a run that fails after it
		// would otherwise leave a warning describing an install that never
		// happened, sitting directly above the error saying so.
		if !strings.Contains(errOut.String(), "about to install the prerelease 0.34.0-rc.1") {
			t.Errorf("stderr %q should announce the prerelease as what is about to be installed", errOut.String())
		}
		if got, _ := os.ReadFile(target); string(got) != "the release candidate" {
			t.Errorf("installed binary holds %q, want the release candidate", got)
		}
	})
}

// TestPerformUpgradeStopsAtTheTotalBudget is the bound the per-request
// deadlines cannot express. Every request here answers well inside its own
// 10-minute transfer deadline and the whole run still has to stop, because the
// command has a budget of its own: a run that can spend it three times over is
// an interactive command that hangs for half an hour on a link that is
// progressing, not broken.
func TestPerformUpgradeStopsAtTheTotalBudget(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")

	// Long enough for a loopback server to answer every request, short enough
	// that the two delayed downloads together overrun it while each one on its
	// own is nowhere near the transfer deadline.
	const (
		budget = 300 * time.Millisecond
		delay  = 600 * time.Millisecond
	)
	original := upgradeBudget
	t.Cleanup(func() { upgradeBudget = original })
	upgradeBudget = budget
	rs.setDelay(checksumAssetPath, delay)
	rs.setDelay(archiveAssetPath, delay)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := performUpgrade(ctx, discardStreams(), "0.33.0", upgradeOptions{}, upgradeDeps{
		fetch:   rs.fetch,
		install: installOver(target),
	})
	if err == nil {
		t.Fatal("expected the run to stop at its total budget, but it completed")
	}
	// The bound has to be nameable: "context deadline exceeded" says which
	// request gave up, not that the command as a whole ran out of time.
	if !strings.Contains(err.Error(), "budget") {
		t.Errorf("error %q should name the total budget the run exceeded", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v should still wrap the deadline, so the cause survives the message", err)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeRefusesAnOversizedAsset is the cap at the command level: a
// release that answers with more than it should is refused, not buffered. The
// manifest is the asset here because the archive's cap is 200 MiB and a test
// cannot push that much through a socket; both are read by the same capped
// reader, and the archive cap is covered directly in internal/selfupdate.
func TestPerformUpgradeRefusesAnOversizedAsset(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	// Served at the real cap plus one byte, so the production number is the one
	// under test rather than a shrunken stand-in.
	rs.swapManifest(strings.Repeat("a", int(selfupdate.ChecksumCap())+1))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	_, err := performUpgrade(ctx, discardStreams(), "0.33.0", upgradeOptions{}, upgradeDeps{
		fetch:   rs.fetch,
		install: installOver(target),
	})
	if err == nil {
		t.Fatal("expected a refusal when an asset exceeds its cap")
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("error %q should name the cap it exceeded", err)
	}
	if got := rs.askedFor(); len(got) != 2 {
		t.Errorf("the server was asked for %v, want the metadata and the oversized manifest and nothing after it", got)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeRefusesASubstitutedArchiveAndLeavesTheOldBinary is #558's
// central property, end to end: the release publishes one digest and the server
// serves different bytes, and the file on disk afterwards is the binary that was
// installed before. Both ways of failing vouching are here — GitHub's digest
// for the asset, and the manifest beside it — because each is a substitute the
// other can be made to agree with.
//
// The substitute is a *well-formed* archive carrying a different binary, not
// bytes that fail to unpack. Bytes that are not an archive would be refused by
// the parser whether or not anything vouched for them, so a test built on those
// would pass against a build that verifies nothing at all.
func TestPerformUpgradeRefusesASubstitutedArchiveAndLeavesTheOldBinary(t *testing.T) {
	assetName := selfupdate.AssetName("0.34.0")
	// A real archive for this platform, holding a real-looking binary.
	substituted := releaseArchive(t, assetName, []byte("an attacker's binary"))

	tests := []struct {
		name    string
		corrupt func(rs *releaseServer)
	}{
		{
			// checksums.txt re-signed to match the substituted bytes: the only
			// witness left is the digest GitHub computed for the asset it holds.
			name: "the manifest is swapped to match",
			corrupt: func(rs *releaseServer) {
				rs.swapArchive(substituted)
				rs.swapManifest(sha256HexDigest(substituted) + "  " + rs.asset.Name + "\n")
			},
		},
		{
			// The API reports a digest for other bytes, which is what a
			// replace-the-asset-without-touching-the-API attack looks like.
			name: "the reported digest is for other bytes",
			corrupt: func(rs *releaseServer) {
				rs.swapArchive(substituted)
				rs.setDigest("sha256:" + sha256HexDigest(substituted))
			},
		},
		{
			// A release that vouches for nothing is not one ghost publishes.
			name: "the release reports no digest",
			corrupt: func(rs *releaseServer) {
				rs.swapArchive(substituted)
				rs.setDigest("")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
			target := installedGhost(t, "the installed binary")
			tt.corrupt(rs)

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_, err := performUpgrade(ctx, discardStreams(), "0.33.0", upgradeOptions{}, upgradeDeps{
				fetch:   rs.fetch,
				install: installOver(target),
			})
			if err == nil {
				t.Fatal("expected a refusal, but the substituted archive was installed")
			}
			// The refusal has to be a verification, not the parser. The
			// substitute is a valid archive, so an error from unpacking it would
			// mean the substitution was caught for the wrong reason — and a test
			// that accepted such an error would pass against a build that
			// verifies nothing at all. Which of the two witnesses refuses
			// differs per case and is not pinned here: in the second case the
			// manifest is still correct for the released bytes, so it is the
			// manifest that catches the substitute.
			if !strings.Contains(err.Error(), "digest") && !strings.Contains(err.Error(), "checksum") {
				t.Errorf("error %q should be a verification refusal, so a substituted archive is refused for what it is", err)
			}
			for _, parseFailure := range []string{"gzip: ", "zip: ", "tar: ", "ghost binary not found in archive"} {
				if strings.Contains(err.Error(), parseFailure) {
					t.Errorf("error %q came from unpacking the substitute, which is a well-formed archive: the substitution has to be caught before it is parsed", err)
				}
			}
			assertUnchanged(t, target, "the installed binary")
		})
	}
}

// TestPerformUpgradeInstallsTheVerifiedRelease is the case that has to keep
// working, over the same fake release and the same real install: every digest
// agrees, so the file on disk becomes the archive's binary.
func TestPerformUpgradeInstallsTheVerifiedRelease(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer
	installed, err := performUpgrade(ctx, upgradeStreams{out: &out, err: &out}, "0.33.0", upgradeOptions{}, upgradeDeps{
		fetch:   rs.fetch,
		install: installOver(target),
	})
	if err != nil {
		t.Fatalf("performUpgrade: %v", err)
	}
	if installed != "0.34.0" {
		t.Errorf("installed version = %q, want 0.34.0", installed)
	}
	if got, _ := os.ReadFile(target); string(got) != "the new binary" {
		t.Errorf("installed binary holds %q, want the release's binary", got)
	}
	if !strings.Contains(out.String(), "Downloading") {
		t.Errorf("output %q should report the download, so a wait is not silent", out.String())
	}
}

// TestPerformUpgradeReportsUpToDateWithoutInstalling keeps the quiet path quiet:
// nothing is downloaded and nothing is written when the installed build already
// is the release, however many opt-ins were given.
func TestPerformUpgradeReportsUpToDateWithoutInstalling(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var out bytes.Buffer
	installed, err := performUpgrade(ctx, upgradeStreams{out: &out, err: &out}, "0.34.0",
		upgradeOptions{allowDowngrade: true, allowPrerelease: true}, upgradeDeps{
			fetch:   rs.fetch,
			install: installOver(target),
		})
	if err != nil {
		t.Fatalf("performUpgrade: %v", err)
	}
	if installed != "" {
		t.Errorf("installed version = %q, want nothing: the binary is already the release", installed)
	}
	if !strings.Contains(out.String(), "Already up to date") {
		t.Errorf("output %q should report that there is nothing to do", out.String())
	}
	if got := rs.askedFor(); len(got) != 1 {
		t.Errorf("the server was asked for %v, want only the release metadata", got)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeDoesNotBlameTheBudgetForTheCallersDeadline keeps
// withBudget honest about which bound fired. performUpgrade documents that a
// caller may set an earlier one — and every test here hands it a 30-second
// parent while shortening the budget to a few hundred milliseconds, so a
// withBudget that reported the budget for any deadline error would be one edit
// away from telling a user their twelve-minute budget ran out when their own
// shorter bound did. The budget branch has its own test; this is the other half.
func TestPerformUpgradeDoesNotBlameTheBudgetForTheCallersDeadline(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")

	// Stalled long past both bounds. The production budget is left alone: the
	// point is that the caller's deadline is the earlier of the two and so is
	// the one that fires.
	rs.setDelay(archiveAssetPath, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := performUpgrade(ctx, discardStreams(), "0.33.0", upgradeOptions{}, upgradeDeps{
		fetch:   rs.fetch,
		install: installOver(target),
	})
	if err == nil {
		t.Fatal("expected the caller's own deadline to stop the run")
	}
	if strings.Contains(err.Error(), "budget") {
		t.Errorf("error %q blames the %s budget, but the deadline the caller set is what expired", err, upgradeBudget)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error %v should still wrap the deadline, so the cause survives the message", err)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestUpgradeFlagsAgreeAcrossBothHelpSurfaces pins the two lists against each
// other, because they are two statements of the same thing: the per-command
// usage handleHelp prints, and the top-level command list `ghost` with no
// arguments prints. A flag added to one and not the other does not merely look
// untidy — the summary goes on telling a user that `ghost upgrade` refuses
// pre-releases unconditionally, which is the opposite of what the command does.
// This is the drift review-sweeper found on this change; nothing pinned it.
//
// It is written as a per-flag check rather than one literal, because a single
// literal is satisfied by a PREFIX: a usage line ending at
// "[--allow-prerelease]" still contains "upgrade [--allow-downgrade]
// [--allow-prerelease]", so the literal passed while the newest flag was
// missing from the summary. Each flag has to be named in both surfaces.
func TestUpgradeFlagsAgreeAcrossBothHelpSurfaces(t *testing.T) {
	_, summary := captureStreams(t, printUsage)

	for _, flag := range []string{"--allow-downgrade", "--allow-prerelease", "--allow-unattested"} {
		if !strings.Contains(upgradeUsage, flag) {
			t.Errorf("the per-command help for `ghost upgrade` never mentions %s, so a user reading it cannot discover the opt-in:\n%s", flag, upgradeUsage)
		}
		if !strings.Contains(summary, flag) {
			t.Errorf("the top-level command list does not carry %s, so the two help surfaces disagree about what `ghost upgrade` accepts:\n%s", flag, summary)
		}
	}
}

// TestPerformUpgradeWarnsBeforeInstallingADowngrade is the sibling of the
// prerelease warning assertion, and it exists because the two warnings are the
// same shape: one pinned and the other free to drift is how a pair ends up
// disagreeing. The "newer" in the wording is only true because the warning is
// reached through isOlderRelease, so the release is provably older than the
// running one — an unorderable running version never gets here at all.
func TestPerformUpgradeWarnsBeforeInstallingADowngrade(t *testing.T) {
	rs := newReleaseServer(t, "v0.33.0", []byte("the older binary"))
	target := installedGhost(t, "the newer binary")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var errOut bytes.Buffer
	installed, err := performUpgrade(ctx, upgradeStreams{out: io.Discard, err: &errOut}, "0.34.0",
		upgradeOptions{allowDowngrade: true}, upgradeDeps{
			fetch:   rs.fetch,
			install: installOver(target),
		})
	if err != nil {
		t.Fatalf("performUpgrade with --allow-downgrade: %v", err)
	}
	if installed != "0.33.0" {
		t.Errorf("installed version = %q, want 0.33.0", installed)
	}
	// Intent, not a completed act: this line is written before the download and
	// both digest checks, so it cannot claim an install that has not happened.
	if !strings.Contains(errOut.String(), "about to install 0.33.0 over the newer 0.34.0") {
		t.Errorf("stderr %q should announce the downgrade as what is about to be installed", errOut.String())
	}
	if got, _ := os.ReadFile(target); string(got) != "the older binary" {
		t.Errorf("installed binary holds %q, want the older release's binary", got)
	}
}
