package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	protobundle "github.com/sigstore/protobuf-specs/gen/pb-go/bundle/v1"
	protocommon "github.com/sigstore/protobuf-specs/gen/pb-go/common/v1"
	protodsse "github.com/sigstore/protobuf-specs/gen/pb-go/dsse"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/testing/ca"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/wcatz/ghost/internal/selfupdate"
)

// The client half of the release-authenticity contract (#694): which states
// `ghost upgrade` may install past a missing attestation, and the order of the
// check relative to unpacking. The crypto is covered in internal/selfupdate;
// every bundle here is minted from a virtual Sigstore.

// attestedTag is at or after selfupdate.FirstAttestedVersion; preAttestationTag
// predates any attestation.
const (
	attestedTag       = "v0.43.0"
	preAttestationTag = "v0.42.0"
)

// mintBundle mints a wire-format Sigstore bundle over artifact, signed by
// identity under issuer. It duplicates the minter in internal/selfupdate's tests
// because a test helper cannot cross a package boundary.
func mintBundle(t *testing.T, vs *ca.VirtualSigstore, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(artifact)
	statement := fmt.Sprintf(
		`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"archive","digest":{"sha256":"%s"}}],`+
			`"predicateType":"https://slsa.dev/provenance/v1","predicate":{}}`,
		hex.EncodeToString(sum[:]))

	te, err := vs.AttestAtTime(identity, issuer, []byte(statement), time.Now().Add(5*time.Minute), false)
	if err != nil {
		t.Fatalf("AttestAtTime: %v", err)
	}
	content, err := te.VerificationContent()
	if err != nil {
		t.Fatalf("VerificationContent: %v", err)
	}
	sigContent, err := te.SignatureContent()
	if err != nil {
		t.Fatalf("SignatureContent: %v", err)
	}
	envelope := sigContent.EnvelopeContent().RawEnvelope()
	timestamps, err := te.Timestamps()
	if err != nil {
		t.Fatalf("Timestamps: %v", err)
	}

	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	sigs := make([]*protodsse.Signature, 0, len(envelope.Signatures))
	for _, s := range envelope.Signatures {
		raw, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			t.Fatalf("decode signature: %v", err)
		}
		sigs = append(sigs, &protodsse.Signature{Sig: raw, Keyid: s.KeyID})
	}
	rfc3161 := make([]*protocommon.RFC3161SignedTimestamp, 0, len(timestamps))
	for _, ts := range timestamps {
		rfc3161 = append(rfc3161, &protocommon.RFC3161SignedTimestamp{SignedTimestamp: ts})
	}

	wire, err := protojson.Marshal(&protobundle.Bundle{
		MediaType: "application/vnd.dev.sigstore.bundle.v0.3+json",
		Content: &protobundle.Bundle_DsseEnvelope{
			DsseEnvelope: &protodsse.Envelope{
				Payload:     payload,
				PayloadType: envelope.PayloadType,
				Signatures:  sigs,
			},
		},
		VerificationMaterial: &protobundle.VerificationMaterial{
			Content: &protobundle.VerificationMaterial_Certificate{
				Certificate: &protocommon.X509Certificate{RawBytes: content.Certificate().Raw},
			},
			TimestampVerificationData: &protobundle.TimestampVerificationData{Rfc3161Timestamps: rfc3161},
		},
	})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return wire
}

// trustRootFrom turns a virtual Sigstore into the trust root production gets
// from TUF.
func trustRootFrom(t *testing.T, vs *ca.VirtualSigstore) *root.TrustedRoot {
	t.Helper()
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		vs.FulcioCertificateAuthorities(), vs.CTLogs(), vs.TimestampingAuthorities(), vs.RekorLogs())
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	return tr
}

// attestedUpgrade is a fake release with a fake attestation service behind it
// and a verifier pointed at a virtual trust root; only the network is fake.
type attestedUpgrade struct {
	release *releaseServer
	vs      *ca.VirtualSigstore
	mu      sync.Mutex // guards the fields below (the handler runs on its own goroutine)
	bundle  []byte     // what the service hands back; nil for "holds none"
	// serviceStatus, when non-zero, makes the endpoint answer with that status.
	serviceStatus int
	askedFor      []string // digests the client looked up
}

func newAttestedUpgrade(t *testing.T, tag string, binary []byte) *attestedUpgrade {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	return &attestedUpgrade{release: newReleaseServer(t, tag, binary), vs: vs}
}

func (a *attestedUpgrade) archiveBytes() []byte {
	a.release.mu.Lock()
	defer a.release.mu.Unlock()
	return a.release.archive
}

// verifier builds the production-shaped verifier, pointed at a local endpoint.
func (a *attestedUpgrade) verifier(t *testing.T) func(context.Context, []byte, string) selfupdate.AttestationResult {
	t.Helper()
	trusted := trustRootFrom(t, a.vs)
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.askedFor = append(a.askedFor, strings.TrimPrefix(r.URL.Path, "/repos/wcatz/ghost/attestations/"))
		bundle, status := a.bundle, a.serviceStatus
		a.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, `{"message":"the attestation service is not answering"}`)
			return
		}
		if bundle == nil {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"attestations": []any{map[string]any{
			"repository_id": 1, "initiator": "github", "bundle": json.RawMessage(bundle), "bundle_url": nil,
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	v := &selfupdate.AttestationVerifier{
		APIBaseURL:      srv.URL,
		TrustedMaterial: func(context.Context) (root.TrustedMaterial, error) { return trusted, nil },
	}
	return v.Check
}

func (a *attestedUpgrade) lookups() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.askedFor...)
}

func (a *attestedUpgrade) setBundle(b []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bundle = b
}

func (a *attestedUpgrade) setServiceStatus(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.serviceStatus = status
}

// attestThisRelease signs the fake release's archive with this repository's
// release workflow on this release's tag: the one attestation that is accepted.
func (a *attestedUpgrade) attestThisRelease(t *testing.T, tag string) {
	t.Helper()
	san, issuer := selfupdate.ReleaseWorkflowIdentity(strings.TrimPrefix(tag, "v"))
	a.setBundle(mintBundle(t, a.vs, san, issuer, a.archiveBytes()))
}

func (a *attestedUpgrade) deps(t *testing.T, target string) upgradeDeps {
	t.Helper()
	return upgradeDeps{fetch: a.release.fetch, install: installOver(target), attest: a.verifier(t)}
}

// upgradeFrom runs performUpgrade from the given running version.
func (a *attestedUpgrade) upgradeFrom(t *testing.T, target, running string, opts upgradeOptions) (string, error) {
	t.Helper()
	return performUpgrade(context.Background(), discardStreams(), running, opts, a.deps(t, target))
}

// TestUpgradeInstallsAnAttestedRelease is the end-to-end case that has to keep
// working: the real verifier over a real bundle, looked up by the digest of the
// bytes the client downloaded.
func TestUpgradeInstallsAnAttestedRelease(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	au.attestThisRelease(t, attestedTag)
	target := installedGhost(t, "the old binary")

	installed, err := au.upgradeFrom(t, target, "0.42.0", upgradeOptions{})
	if err != nil || installed != "0.43.0" {
		t.Fatalf("performUpgrade = %q, %v, want 0.43.0", installed, err)
	}
	if got := readFileString(t, target); got != string(binary) {
		t.Errorf("installed binary holds %q, want %q", got, binary)
	}
	sum := sha256.Sum256(au.archiveBytes())
	if lookups := au.lookups(); len(lookups) != 1 || lookups[0] != "sha256:"+hex.EncodeToString(sum[:]) {
		t.Errorf("lookups = %v, want one by the digest of the downloaded bytes", lookups)
	}
}

// TestUpgradeRefusesAnUnattestedRelease: from the first attested release on, a
// release with no attestation is refused, naming the release and the one flag.
func TestUpgradeRefusesAnUnattestedRelease(t *testing.T) {
	au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
	target := installedGhost(t, "the old binary")
	_, err := au.upgradeFrom(t, target, "0.42.0", upgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "--allow-unattested") || !strings.Contains(err.Error(), "0.43.0") {
		t.Fatalf("err = %v, want a refusal naming the flag and the release", err)
	}
	assertUnchanged(t, target, "the old binary")
}

// TestUpgradeInstallsAnUnattestedReleaseWithTheFlag: the escape hatch is loud,
// and says so before the install, not after.
func TestUpgradeInstallsAnUnattestedReleaseWithTheFlag(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	target := installedGhost(t, "the old binary")

	var out strings.Builder
	deps := au.deps(t, target)
	replace := deps.install
	var saidAtInstall string
	deps.install = func(b []byte) error {
		saidAtInstall = out.String()
		return replace(b)
	}
	installed, err := performUpgrade(context.Background(), upgradeStreams{out: &out, err: &out}, "0.42.0",
		upgradeOptions{allowUnattested: true}, deps)
	if err != nil || installed != "0.43.0" {
		t.Fatalf("performUpgrade with --allow-unattested = %q, %v", installed, err)
	}
	if got := readFileString(t, target); got != string(binary) {
		t.Errorf("installed binary holds %q, want %q", got, binary)
	}
	if !strings.Contains(saidAtInstall, "unattested") || !strings.Contains(saidAtInstall, "--allow-unattested") {
		t.Errorf("at the moment it installed, the run had said %q", saidAtInstall)
	}
}

// TestUpgradeRefusesWhenTheAttestationServiceIsUnreachable: an outage is not
// absence. It refuses by default, names the flag, and must not claim the release
// publishes no attestation.
func TestUpgradeRefusesWhenTheAttestationServiceIsUnreachable(t *testing.T) {
	au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
	au.setServiceStatus(http.StatusInternalServerError)
	target := installedGhost(t, "the old binary")
	_, err := au.upgradeFrom(t, target, "0.42.0", upgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "--allow-unattested") || strings.Contains(err.Error(), "publishes no build attestation") {
		t.Fatalf("err = %v, want an outage refusal naming the flag", err)
	}
	assertUnchanged(t, target, "the old binary")
	if _, err := au.upgradeFrom(t, target, "0.42.0", upgradeOptions{allowUnattested: true}); err != nil {
		t.Fatalf("the outage is not recoverable with the flag: %v", err)
	}
}

// TestUpgradeRefusesARejectedBundleWithAndWithoutTheFlag: --allow-unattested
// means "no attestation", never "the attestation did not check out", so a bundle
// signed by another repository's workflow, or by a different workflow of this
// one, is refused with or without the flag and never offers it.
func TestUpgradeRefusesARejectedBundleWithAndWithoutTheFlag(t *testing.T) {
	version := strings.TrimPrefix(attestedTag, "v")
	_, issuer := selfupdate.ReleaseWorkflowIdentity(version)
	for _, identity := range map[string]string{
		"another repository": "https://github.com/attacker/ghost/.github/workflows/release.yml@refs/tags/v" + version,
		"another workflow":   "https://github.com/wcatz/ghost/.github/workflows/publish.yml@refs/tags/v" + version,
	} {
		for _, flag := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s flag=%v", identity, flag), func(t *testing.T) {
				au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
				au.setBundle(mintBundle(t, au.vs, identity, issuer, au.archiveBytes()))
				target := installedGhost(t, "the old binary")
				_, err := au.upgradeFrom(t, target, "0.42.0", upgradeOptions{allowUnattested: flag})
				if err == nil {
					t.Fatal("a bundle signed by the wrong identity was accepted")
				}
				if strings.Contains(err.Error(), "--allow-unattested") {
					t.Errorf("refusal %q names the flag, which does not reach a rejected bundle", err)
				}
				assertUnchanged(t, target, "the old binary")
			})
		}
	}
}

// TestUpgradeSaysSoWhenItSkipsTheAttestationBeforeTheCutover: a pre-cutover
// release installs without a lookup, and the user is told nothing vouched for
// it; a genuinely verified release stays silent.
func TestUpgradeSaysSoWhenItSkipsTheAttestationBeforeTheCutover(t *testing.T) {
	run := func(t *testing.T, tag, running string, attest bool) (*attestedUpgrade, string, string) {
		au := newAttestedUpgrade(t, tag, []byte("pretend executable"))
		if attest {
			au.attestThisRelease(t, tag)
		}
		target := installedGhost(t, "the old binary")
		var warn strings.Builder
		installed, err := performUpgrade(context.Background(), upgradeStreams{out: io.Discard, err: &warn}, running, upgradeOptions{}, au.deps(t, target))
		if err != nil || installed != tag[1:] {
			t.Fatalf("performUpgrade = %q, %v, want %s installed", installed, err, tag[1:])
		}
		return au, warn.String(), readFileString(t, target)
	}
	t.Run("a pre-cutover release says nothing vouched for it", func(t *testing.T) {
		au, warn, installed := run(t, preAttestationTag, "0.41.0", false)
		if installed != "pretend executable" || len(au.lookups()) != 0 {
			t.Errorf("installed %q with lookups %v, want the binary installed with no lookup", installed, au.lookups())
		}
		if !strings.Contains(warn, selfupdate.FirstAttestedVersion) || !strings.Contains(warn, preAttestationTag[1:]) {
			t.Errorf("the warning %q does not name the first attested release and the release installed", warn)
		}
	})
	t.Run("a genuinely verified release is silent", func(t *testing.T) {
		if _, warn, _ := run(t, attestedTag, "0.42.0", true); warn != "" {
			t.Errorf("a release whose bundle verified printed %q", warn)
		}
	})
}

// TestUpgradeRefusesToSkipTheCheckBeforeTheCutoverWithNoTrustMaterial: the
// cutover is decided from the version alone; the release at the boundary is
// still checked, exactly once, and refused when absent.
func TestUpgradeRefusesToSkipTheCheckBeforeTheCutoverWithNoTrustMaterial(t *testing.T) {
	au := newAttestedUpgrade(t, selfupdate.FirstAttestedVersion, []byte("pretend executable"))
	target := installedGhost(t, "the old binary")
	calls := 0
	_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, upgradeDeps{
		fetch:   au.release.fetch,
		install: func([]byte) error { calls++; return nil },
		attest: func(context.Context, []byte, string) selfupdate.AttestationResult {
			calls++
			return selfupdate.AttestationResult{State: selfupdate.AttestationAbsent}
		},
	})
	if err == nil || calls != 1 {
		t.Errorf("err = %v with %d calls, want a refusal after exactly one attestation check", err, calls)
	}
	assertUnchanged(t, target, "the old binary")
}

// TestUpgradeRefusesWhenThereIsNoVerifierAtAll and the unrecognised-state test
// below are fail-closed branches that never consult --allow-unattested, so
// neither may offer it; "nobody checked" must not read as "nothing to check".
func TestUpgradeRefusesWhenThereIsNoVerifierAtAll(t *testing.T) {
	for _, flag := range []bool{false, true} {
		t.Run(fmt.Sprintf("flag=%v", flag), func(t *testing.T) {
			rs := newReleaseServer(t, attestedTag, []byte("pretend executable"))
			rel, err := rs.fetch(context.Background())
			if err != nil {
				t.Fatalf("fetch the fake release: %v", err)
			}
			asset, err := selfupdate.FindAsset(rel)
			if err != nil {
				t.Fatalf("FindAsset: %v", err)
			}
			target := installedGhost(t, "the old binary")
			calls := 0
			err = installRelease(context.Background(), discardStreams(), rel, asset,
				upgradeOptions{allowUnattested: flag}, upgradeDeps{install: func([]byte) error { calls++; return nil }})
			if err == nil || strings.Contains(err.Error(), "--allow-unattested") || !strings.Contains(err.Error(), "issue") || calls != 0 {
				t.Errorf("err = %v with %d installs, want a refusal that reports a ghost bug and does not offer the flag", err, calls)
			}
			assertUnchanged(t, target, "the old binary")
		})
	}
}

func TestUpgradeRefusesAnUnrecognisedAttestationState(t *testing.T) {
	for _, flag := range []bool{false, true} {
		t.Run(fmt.Sprintf("flag=%v", flag), func(t *testing.T) {
			au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
			target := installedGhost(t, "the old binary")
			calls := 0
			_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0",
				upgradeOptions{allowUnattested: flag}, upgradeDeps{
					fetch:   au.release.fetch,
					install: func([]byte) error { calls++; return nil },
					attest: func(context.Context, []byte, string) selfupdate.AttestationResult {
						return selfupdate.AttestationResult{State: selfupdate.AttestationState(99)}
					},
				})
			if err == nil || strings.Contains(err.Error(), "--allow-unattested") || !strings.Contains(err.Error(), "unknown") || calls != 0 {
				t.Errorf("err = %v with %d installs, want a refusal naming an unknown state without the flag", err, calls)
			}
			assertUnchanged(t, target, "the old binary")
		})
	}
}

// TestUpgradeVerifiesTheAttestationBeforeUnpackingTheArchive pins the order: the
// served archive is not an archive and both published digests vouch for it, so
// only an attestation check that runs before parsing can refuse. The archive IS
// downloaded first, because the lookup is keyed on the downloaded bytes.
func TestUpgradeVerifiesTheAttestationBeforeUnpackingTheArchive(t *testing.T) {
	notAnArchive := []byte("substituted bytes, not an archive")
	au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
	au.release.swapManifest(sha256HexDigest(notAnArchive) + "  " + selfupdate.AssetName("0.43.0") + "\n")
	au.release.swapArchive(notAnArchive)
	au.release.setDigest("sha256:" + sha256HexDigest(notAnArchive))
	target := installedGhost(t, "the old binary")

	_, err := au.upgradeFrom(t, target, "0.42.0", upgradeOptions{})
	if err == nil || !strings.Contains(err.Error(), "attestation") {
		t.Fatalf("err = %v, want a refusal from the attestation check", err)
	}
	for _, parseFailure := range []string{"gzip: ", "zip: ", "tar: ", "ghost binary not found in archive"} {
		if strings.Contains(err.Error(), parseFailure) {
			t.Errorf("error %q came from unpacking the archive", err)
		}
	}
	downloaded := false
	for _, p := range au.release.askedFor() {
		downloaded = downloaded || p == archiveAssetPath
	}
	if !downloaded {
		t.Error("the archive was never downloaded, so the refusal may be for the wrong reason")
	}
	assertUnchanged(t, target, "the old binary")
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}
