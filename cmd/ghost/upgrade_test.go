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
	down := upgradeOptions{allowDowngrade: true}
	pre := upgradeOptions{allowPrerelease: true}
	tests := []struct {
		name            string
		running, latest string
		opts            upgradeOptions
		want            upgradeOutcome
	}{
		{"same release is already current", "0.32.0", "0.32.0", upgradeOptions{}, upgradeCurrent},
		{"newer minor proceeds", "0.32.0", "0.33.0", upgradeOptions{}, upgradeProceed},
		{"older patch is refused", "0.32.1", "0.32.0", upgradeOptions{}, upgradeRefuseDowngrade},
		{"older major is refused", "1.0.0", "0.32.0", upgradeOptions{}, upgradeRefuseDowngrade},
		{"0.10.0 is newer than 0.9.0", "0.9.0", "0.10.0", upgradeOptions{}, upgradeProceed},
		{"a v-prefixed tag is not a different release", "0.32.0", "v0.32.0", upgradeOptions{}, upgradeCurrent},
		{"a final release supersedes its rc", "0.33.0-rc.1", "0.33.0", upgradeOptions{}, upgradeProceed},
		{"dev build is never blocked", "dev", "0.0.1", upgradeOptions{}, upgradeProceed},
		{"unorderable release tag proceeds", "0.32.0", "nightly", upgradeOptions{}, upgradeProceed},
		{"identical unorderable version is current", "nightly", "nightly", upgradeOptions{}, upgradeCurrent},
		{"--allow-downgrade permits an older patch", "0.32.1", "0.32.0", down, upgradeProceed},
		{"--allow-downgrade does not reinstall the release you have", "0.33.0", "0.33.0", down, upgradeCurrent},
		{"a newer rc is refused", "0.33.0", "0.34.0-rc.1", upgradeOptions{}, upgradeRefusePrerelease},
		{"an older rc is refused as a prerelease, not a downgrade", "0.33.0", "0.33.0-rc.1", upgradeOptions{}, upgradeRefusePrerelease},
		{"a v-prefixed rc is refused", "0.33.0", "v0.34.0-rc.1", upgradeOptions{}, upgradeRefusePrerelease},
		{"--allow-downgrade does not permit a prerelease", "0.33.0", "0.34.0-rc.1", down, upgradeRefusePrerelease},
		{"--allow-prerelease permits a newer rc", "0.33.0", "0.34.0-rc.1", pre, upgradeProceed},
		{"--allow-prerelease alone does not permit an older rc", "0.33.0", "0.33.0-rc.1", pre, upgradeRefuseDowngrade},
		{"both flags permit an older rc", "0.33.0", "0.33.0-rc.1", upgradeOptions{allowPrerelease: true, allowDowngrade: true}, upgradeProceed},
		{"--allow-prerelease does not reinstall the rc you have", "0.34.0-rc.1", "0.34.0-rc.1", pre, upgradeCurrent},
		{"an unorderable tag is not treated as a prerelease", "0.32.0", "nightly-rc.1", upgradeOptions{}, upgradeProceed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := decideUpgrade(tt.running, tt.latest, tt.opts); got != tt.want {
				t.Errorf("decideUpgrade(%q, %q, %+v) = %v, want %v", tt.running, tt.latest, tt.opts, got, tt.want)
			}
		})
	}
}

func TestParseUpgradeArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want upgradeOptions
	}{
		{"no arguments", nil, upgradeOptions{}},
		{"the downgrade opt-in", []string{"--allow-downgrade"}, upgradeOptions{allowDowngrade: true}},
		{"the prerelease opt-in", []string{"--allow-prerelease"}, upgradeOptions{allowPrerelease: true}},
		{"the attestation opt-in", []string{"--allow-unattested"}, upgradeOptions{allowUnattested: true}},
		{"all three opt-ins", []string{"--allow-prerelease", "--allow-downgrade", "--allow-unattested"},
			upgradeOptions{allowPrerelease: true, allowDowngrade: true, allowUnattested: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseUpgradeArgs(tt.args)
			if err != nil || got != tt.want {
				t.Errorf("parseUpgradeArgs(%v) = %+v, %v, want %+v", tt.args, got, err, tt.want)
			}
		})
	}
}

// TestParseUpgradeArgsRejectsAnythingElse keeps a mistyped flag from reading as
// consent.
func TestParseUpgradeArgsRejectsAnythingElse(t *testing.T) {
	for _, arg := range []string{
		"--allow-down", "-allow-downgrade", "--allow-downgrade=false",
		"--allow-pre", "--allow-unattested=true", "--apply", "upgrade", "",
	} {
		if _, err := parseUpgradeArgs([]string{arg}); err == nil {
			t.Errorf("parseUpgradeArgs(%q) accepted an argument it does not implement", arg)
		}
	}
}

func sha256HexDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// releaseArchive builds the archive a release of this name would carry: a
// .tar.gz everywhere except Windows, which ships a .zip.
func releaseArchive(t *testing.T, assetName string, binary []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	if !strings.HasSuffix(assetName, ".zip") {
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

// The paths releaseServer serves, shaped like the real ones.
const (
	releaseMetadataPath = "/repos/wcatz/ghost/releases/latest"
	checksumAssetPath   = "/checksums.txt"
	archiveAssetPath    = "/archive"
)

// releaseServer is a fake GitHub serving the release metadata, the checksum
// manifest and this platform's archive, recording what was asked for. Its fields
// are locked because httptest answers on its own goroutine and CI runs -race.
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
	assetName := selfupdate.AssetName(strings.TrimPrefix(tag, "v"))
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
	mux.HandleFunc("/", rs.serve)
	rs.Server = httptest.NewServer(mux)
	t.Cleanup(rs.Close)
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

// fetch is the release lookup a test's run performs, under the caller's context,
// so the total budget is the only deadline on it.
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

func (rs *releaseServer) setDelay(path string, d time.Duration) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.delays[path] = d
}

// swapArchive replaces the served archive without touching what the release
// says about it, which is what a substituted release looks like to the client.
func (rs *releaseServer) swapArchive(other []byte) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.archive = other
}

func (rs *releaseServer) swapManifest(text string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.manifest = text
}

// installedGhost stands in for the binary being replaced: a real file in a real
// directory.
func installedGhost(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ghost")
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write the installed binary: %v", err)
	}
	return path
}

// installOver wires performUpgrade to a real selfupdate.Replace against target.
func installOver(target string) func([]byte) error {
	return func(binary []byte) error { _, err := selfupdate.Replace(target, binary); return err }
}

// assertUnchanged checks that a refused upgrade left the installed binary as it
// was, with nothing else (no staged file, no aside) in its directory.
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
		t.Errorf("the install directory holds %v, want only %s", names, filepath.Base(target))
	}
}

// upgradeAgainst runs performUpgrade over rs, installing over target.
func upgradeAgainst(t *testing.T, rs *releaseServer, target, running string, opts upgradeOptions, streams upgradeStreams) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return performUpgrade(ctx, streams, running, opts, upgradeDeps{fetch: rs.fetch, install: installOver(target)})
}

// TestPerformUpgradeRefusesBeforeDownloading: a refused downgrade or prerelease
// is decided before the archive is fetched, names the flag that permits it, and
// leaves the installed binary alone.
func TestPerformUpgradeRefusesBeforeDownloading(t *testing.T) {
	for _, tc := range []struct{ name, tag, running, flag string }{
		{"downgrade", "v0.33.0", "0.34.0", "--allow-downgrade"},
		{"prerelease", "v0.34.0-rc.1", "0.33.0", "--allow-prerelease"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newReleaseServer(t, tc.tag, []byte("the other binary"))
			target := installedGhost(t, "the installed binary")
			installed, err := upgradeAgainst(t, rs, target, tc.running, upgradeOptions{}, discardStreams())
			if err == nil || !strings.Contains(err.Error(), tc.flag) {
				t.Fatalf("performUpgrade = %q, %v, want a refusal naming %s", installed, err, tc.flag)
			}
			if got := rs.askedFor(); len(got) != 1 || got[0] != releaseMetadataPath {
				t.Errorf("the server was asked for %v, want only the release metadata", got)
			}
			assertUnchanged(t, target, "the installed binary")
		})
	}
}

// TestPerformUpgradeAnnouncesAnOptedInInstall: an opted-in prerelease or
// downgrade installs and says on stderr, before the download, what is about to
// be installed (intent, not a completed act).
func TestPerformUpgradeAnnouncesAnOptedInInstall(t *testing.T) {
	for _, tc := range []struct {
		name, tag, running, warn string
		opts                     upgradeOptions
	}{
		{"prerelease", "v0.34.0-rc.1", "0.33.0", "about to install the prerelease 0.34.0-rc.1", upgradeOptions{allowPrerelease: true}},
		{"downgrade", "v0.33.0", "0.34.0", "about to install 0.33.0 over the newer 0.34.0", upgradeOptions{allowDowngrade: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rs := newReleaseServer(t, tc.tag, []byte("the other binary"))
			target := installedGhost(t, "the installed binary")
			var errOut bytes.Buffer
			installed, err := upgradeAgainst(t, rs, target, tc.running, tc.opts, upgradeStreams{out: io.Discard, err: &errOut})
			if err != nil || installed != strings.TrimPrefix(tc.tag, "v") {
				t.Fatalf("performUpgrade = %q, %v", installed, err)
			}
			if !strings.Contains(errOut.String(), tc.warn) {
				t.Errorf("stderr %q should announce %q", errOut.String(), tc.warn)
			}
			if got, _ := os.ReadFile(target); string(got) != "the other binary" {
				t.Errorf("installed binary holds %q, want the release's binary", got)
			}
		})
	}
}

// TestPerformUpgradeStopsAtTheTotalBudget: every request answers inside its own
// transfer deadline and the whole run still has to stop at the command's budget.
func TestPerformUpgradeStopsAtTheTotalBudget(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	original := upgradeBudget
	t.Cleanup(func() { upgradeBudget = original })
	upgradeBudget = 300 * time.Millisecond
	rs.setDelay(checksumAssetPath, 600*time.Millisecond)
	rs.setDelay(archiveAssetPath, 600*time.Millisecond)

	_, err := upgradeAgainst(t, rs, target, "0.33.0", upgradeOptions{}, discardStreams())
	if err == nil || !strings.Contains(err.Error(), "budget") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a budget error wrapping the deadline", err)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeDoesNotBlameTheBudgetForTheCallersDeadline: a caller's
// earlier deadline is not reported as the total budget.
func TestPerformUpgradeDoesNotBlameTheBudgetForTheCallersDeadline(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	rs.setDelay(archiveAssetPath, 30*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()

	_, err := performUpgrade(ctx, discardStreams(), "0.33.0", upgradeOptions{}, upgradeDeps{fetch: rs.fetch, install: installOver(target)})
	if err == nil || strings.Contains(err.Error(), "budget") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the caller's deadline, not the budget", err)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeRefusesAnOversizedAsset: an asset past its cap is refused,
// not buffered. The manifest stands in because the archive cap is 200 MiB and
// is covered in internal/selfupdate.
func TestPerformUpgradeRefusesAnOversizedAsset(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	rs.swapManifest(strings.Repeat("a", int(selfupdate.ChecksumCap())+1))

	_, err := upgradeAgainst(t, rs, target, "0.33.0", upgradeOptions{}, discardStreams())
	if err == nil || !strings.Contains(err.Error(), "cap") {
		t.Fatalf("err = %v, want a refusal naming the cap", err)
	}
	if got := rs.askedFor(); len(got) != 2 {
		t.Errorf("the server was asked for %v, want the metadata and the manifest only", got)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestPerformUpgradeRefusesASubstitutedArchiveAndLeavesTheOldBinary is #558
// end to end: whatever the release says about the archive, bytes that do not
// match never reach the installed file. The substitute is a well-formed archive,
// so a parser error would mean nothing vouched for it first.
func TestPerformUpgradeRefusesASubstitutedArchiveAndLeavesTheOldBinary(t *testing.T) {
	assetName := selfupdate.AssetName("0.34.0")
	substituted := releaseArchive(t, assetName, []byte("an attacker's binary"))
	otherHex := sha256HexDigest([]byte("other bytes"))

	tests := []struct {
		name    string
		corrupt func(rs *releaseServer)
	}{
		{"the manifest is swapped to match", func(rs *releaseServer) {
			rs.swapArchive(substituted)
			rs.swapManifest(sha256HexDigest(substituted) + "  " + rs.asset.Name + "\n")
		}},
		{"the reported digest is for other bytes", func(rs *releaseServer) {
			rs.swapArchive(substituted)
			rs.setDigest("sha256:" + sha256HexDigest(substituted))
		}},
		{"the release reports no digest", func(rs *releaseServer) { rs.swapArchive(substituted); rs.setDigest("") }},
		{"the digest uses an algorithm ghost cannot check", func(rs *releaseServer) {
			rs.setDigest("sha512:" + strings.Repeat("a", 128))
		}},
		{"the manifest is for other bytes", func(rs *releaseServer) { rs.swapManifest(otherHex + "  " + rs.asset.Name + "\n") }},
		{"the manifest has no entry for the asset", func(rs *releaseServer) { rs.swapManifest(otherHex + "  some_other_asset.tar.gz\n") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
			target := installedGhost(t, "the installed binary")
			tt.corrupt(rs)

			_, err := upgradeAgainst(t, rs, target, "0.33.0", upgradeOptions{}, discardStreams())
			if err == nil {
				t.Fatal("expected a refusal, but the substituted archive was installed")
			}
			if !strings.Contains(err.Error(), "digest") && !strings.Contains(err.Error(), "checksum") {
				t.Errorf("error %q should be a verification refusal", err)
			}
			for _, parseFailure := range []string{"gzip: ", "zip: ", "tar: ", "ghost binary not found in archive"} {
				if strings.Contains(err.Error(), parseFailure) {
					t.Errorf("error %q came from unpacking, not from verification", err)
				}
			}
			assertUnchanged(t, target, "the installed binary")
		})
	}
}

// TestPerformUpgradeRefusesAMissingManifest keeps the check fail-closed: a
// release with no checksums.txt is refused for that reason, not skipped.
func TestPerformUpgradeRefusesAMissingManifest(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := performUpgrade(ctx, discardStreams(), "0.33.0", upgradeOptions{}, upgradeDeps{
		fetch: func(ctx context.Context) (*selfupdate.Release, error) {
			rel, err := rs.fetch(ctx)
			if err == nil {
				rel.Assets = rel.Assets[:1]
			}
			return rel, err
		},
		install: installOver(target),
	})
	if err == nil || !strings.Contains(err.Error(), "no checksums.txt asset") {
		t.Fatalf("err = %v, want the missing-manifest refusal", err)
	}
	assertUnchanged(t, target, "the installed binary")
}

func TestPerformUpgradeInstallsTheVerifiedRelease(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	var out bytes.Buffer
	installed, err := upgradeAgainst(t, rs, target, "0.33.0", upgradeOptions{}, upgradeStreams{out: &out, err: &out})
	if err != nil || installed != "0.34.0" {
		t.Fatalf("performUpgrade = %q, %v, want 0.34.0", installed, err)
	}
	if got, _ := os.ReadFile(target); string(got) != "the new binary" {
		t.Errorf("installed binary holds %q, want the release's binary", got)
	}
	if !strings.Contains(out.String(), "Downloading") {
		t.Errorf("output %q should report the download", out.String())
	}
}

func TestPerformUpgradeReportsUpToDateWithoutInstalling(t *testing.T) {
	rs := newReleaseServer(t, "v0.34.0", []byte("the new binary"))
	target := installedGhost(t, "the installed binary")
	var out bytes.Buffer
	installed, err := upgradeAgainst(t, rs, target, "0.34.0", upgradeOptions{allowDowngrade: true, allowPrerelease: true}, upgradeStreams{out: &out, err: &out})
	if err != nil || installed != "" {
		t.Fatalf("performUpgrade = %q, %v, want nothing installed", installed, err)
	}
	if !strings.Contains(out.String(), "Already up to date") {
		t.Errorf("output %q should report that there is nothing to do", out.String())
	}
	if got := rs.askedFor(); len(got) != 1 {
		t.Errorf("the server was asked for %v, want only the release metadata", got)
	}
	assertUnchanged(t, target, "the installed binary")
}

// TestUpgradeFlagsAgreeAcrossBothHelpSurfaces: the per-command usage and the
// top-level command list must each name every flag (a single literal would be
// satisfied by a prefix).
func TestUpgradeFlagsAgreeAcrossBothHelpSurfaces(t *testing.T) {
	_, summary := captureStreams(t, printUsage)
	for _, flag := range []string{"--allow-downgrade", "--allow-prerelease", "--allow-unattested"} {
		if !strings.Contains(upgradeUsage, flag) {
			t.Errorf("the per-command help for `ghost upgrade` never mentions %s", flag)
		}
		if !strings.Contains(summary, flag) {
			t.Errorf("the top-level command list does not carry %s", flag)
		}
	}
}
