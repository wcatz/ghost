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

// The client half of the release-authenticity contract (#694): whether
// `ghost upgrade` may install a release archive that GitHub's attestation
// service has not vouched for, and what it says when it may not.
//
// The crypto is covered exhaustively in internal/selfupdate. What is untestable
// there is the DECISION, because that is here: which states the loud flag
// reaches, which it must never reach, and the order the check runs in relative
// to unpacking the archive.
//
// Every bundle below is minted from a virtual Fulcio and a virtual TSA, so no
// test in this file reaches GitHub or the Sigstore TUF repository.

// attestedTag is a release at or after selfupdate.FirstAttestedVersion, and
// therefore one the release workflow attests.
const attestedTag = "v0.43.0"

// preAttestationTag is a release published before the release workflow minted
// any attestation, and therefore one that cannot have one.
const preAttestationTag = "v0.42.0"

// mintBundle mints a wire-format Sigstore bundle over artifact, signed by
// identity under issuer, from a virtual Sigstore. It duplicates the minter in
// internal/selfupdate's tests rather than sharing it, because a Go test helper
// cannot cross a package boundary and the alternative — a real bundle from
// GitHub with a real Fulcio certificate — would pin the test to a trust root
// that has to be embedded and re-pinned every time the public one rotates.
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

	// The proto fields are `bytes` and protojson base64-encodes them, so the
	// base64 strings the DSSE envelope carries have to be decoded first.
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

// trustRootFrom turns a virtual Sigstore into the kind of value production
// gets from TUF. sigstore-go already models the trust root as an interface, so
// no test-only seam is needed to make it injectable.
func trustRootFrom(t *testing.T, vs *ca.VirtualSigstore) *root.TrustedRoot {
	t.Helper()
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		vs.FulcioCertificateAuthorities(), vs.CTLogs(), vs.TimestampingAuthorities(), vs.RekorLogs())
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	return tr
}

// attestedUpgrade is a fake release with a fake attestation service behind it:
// a release server serving the archive, and a verifier pointed at a local
// attestations endpoint and a virtual trust root. Everything the production
// path does is real except the network.
type attestedUpgrade struct {
	release *releaseServer
	vs      *ca.VirtualSigstore
	// mu guards the three fields below. The attestations handler runs on the
	// server's goroutine while the test sets them on its own, and CI runs this
	// package under -race.
	mu sync.Mutex
	// bundle is what the attestation service hands back, or nil for "holds
	// none". Set before the run.
	bundle []byte
	// serviceStatus, when non-zero, makes the attestations endpoint answer
	// with that status instead of a bundle.
	serviceStatus int
	// askedFor records the digests the client looked up, so a test can assert
	// the lookup used the bytes it downloaded rather than anything the
	// release said about them.
	askedFor []string
}

func newAttestedUpgrade(t *testing.T, tag string, binary []byte) *attestedUpgrade {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	return &attestedUpgrade{release: newReleaseServer(t, tag, binary), vs: vs}
}

// archiveBytes is the archive this fake release serves.
func (a *attestedUpgrade) archiveBytes() []byte {
	a.release.mu.Lock()
	defer a.release.mu.Unlock()
	return a.release.archive
}

// verifier builds the production-shaped attestation verifier, pointed at a
// local endpoint. The trust material is a function because a release that needs
// no attestation must not pay for a TUF round trip to discover that.
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
			"repository_id": 1,
			"initiator":     "github",
			"bundle":        json.RawMessage(bundle),
			"bundle_url":    nil,
		}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	v := &selfupdate.AttestationVerifier{
		APIBaseURL: srv.URL,
		TrustedMaterial: func(context.Context) (root.TrustedMaterial, error) {
			return trusted, nil
		},
	}
	return v.Check
}

// lookups lists the digests the attestation service was asked about, in order.
func (a *attestedUpgrade) lookups() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.askedFor...)
}

// setBundle is what the tests use to say what the service holds: a bundle, or
// nil for none.
func (a *attestedUpgrade) setBundle(b []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.bundle = b
}

// setServiceStatus makes the attestations endpoint answer with a status instead
// of a verdict, which is how an outage is simulated.
func (a *attestedUpgrade) setServiceStatus(status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.serviceStatus = status
}

// attestThisRelease signs the fake release's own archive with this
// repository's release workflow on this release's tag, which is the one
// attestation that has to be accepted.
func (a *attestedUpgrade) attestThisRelease(t *testing.T, tag string) {
	t.Helper()
	version := strings.TrimPrefix(tag, "v")
	san, issuer := selfupdate.ReleaseWorkflowIdentity(version)
	a.setBundle(mintBundle(t, a.vs, san, issuer, a.archiveBytes()))
}

// deps wires a run against this fixture.
func (a *attestedUpgrade) deps(t *testing.T, target string) upgradeDeps {
	t.Helper()
	return upgradeDeps{
		fetch:   a.release.fetch,
		install: installOver(target),
		attest:  a.verifier(t),
	}
}

// --- the happy path -------------------------------------------------------

// TestUpgradeInstallsAnAttestedRelease is the case that has to keep working, and
// it is an end-to-end one: the real verifier, over a real Sigstore bundle,
// against the bytes the fake release actually served. It is what catches the
// wiring being wrong — a lookup by the wrong digest, or a bundle fetched and
// then never checked — neither of which any state-level test would see.
func TestUpgradeInstallsAnAttestedRelease(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	au.attestThisRelease(t, attestedTag)
	target := installedGhost(t, "the old binary")

	installed, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, au.deps(t, target))
	if err != nil {
		t.Fatalf("performUpgrade: %v", err)
	}
	if installed != "0.43.0" {
		t.Errorf("installed version = %q, want 0.43.0", installed)
	}
	if got := readFileString(t, target); got != string(binary) {
		t.Errorf("installed binary holds %q, want %q", got, binary)
	}
	if lookups := au.lookups(); len(lookups) != 1 {
		t.Fatalf("the attestation service was asked %d times, want 1", len(lookups))
	}
	sum := sha256.Sum256(au.archiveBytes())
	if want := "sha256:" + hex.EncodeToString(sum[:]); au.lookups()[0] != want {
		t.Errorf("looked up %q, want the digest of the bytes the client downloaded (%q)", au.lookups()[0], want)
	}
}

// TestUpgradeLooksUpTheAttestationByTheDownloadedBytes keeps the lookup bound
// to the archive rather than to what the release claims about it. If the client
// asked by the reported digest instead, a substituted archive — refused by
// VerifyAssetDigest, so never reached here — is not the threat; the threat is a
// client that looks up the attacker's digest and finds an attestation for it.
// Asking by the downloaded bytes is what makes the lookup unforgeable.
func TestUpgradeLooksUpTheAttestationByTheDownloadedBytes(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	// A bundle over bytes that are NOT what the release serves.
	san, issuer := selfupdate.ReleaseWorkflowIdentity(strings.TrimPrefix(attestedTag, "v"))
	au.setBundle(mintBundle(t, au.vs, san, issuer, []byte("an attacker's archive")))
	target := installedGhost(t, "the old binary")

	calls := 0
	_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, upgradeDeps{
		fetch:   au.release.fetch,
		install: func([]byte) error { calls++; return nil },
		attest:  au.verifier(t),
	})
	if err == nil {
		t.Fatal("a bundle over different bytes was accepted")
	}
	if calls != 0 {
		t.Errorf("the installer ran %d time(s) for an unvouched archive", calls)
	}
	assertUnchanged(t, target, "the old binary")
}

// --- absence: the only state the flag reaches -----------------------------

// TestUpgradeRefusesAnUnattestedRelease is the default. From the first attested
// release onwards, a release with no attestation is refused, and the refusal
// names the one flag that permits it — the same contract the downgrade and
// prerelease guards keep.
func TestUpgradeRefusesAnUnattestedRelease(t *testing.T) {
	au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
	// No bundle: the service holds nothing for this digest.
	target := installedGhost(t, "the old binary")

	_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, au.deps(t, target))
	if err == nil {
		t.Fatal("an unattested release was installed with no flag given")
	}
	if !strings.Contains(err.Error(), "--allow-unattested") {
		t.Errorf("refusal %q does not name the flag that permits it", err)
	}
	if !strings.Contains(err.Error(), "0.43.0") {
		t.Errorf("refusal %q does not name the release it refused", err)
	}
	assertUnchanged(t, target, "the old binary")
}

// TestUpgradeInstallsAnUnattestedReleaseWithTheFlag is the escape hatch, and
// it has to be loud: the flag is how a user proceeds past a check, so the run
// says on stderr, before the install, that the archive it is about to install
// has no attestation. Silence would leave the flag looking like a formality.
func TestUpgradeInstallsAnUnattestedReleaseWithTheFlag(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	target := installedGhost(t, "the old binary")

	var out strings.Builder
	deps := au.deps(t, target)
	replace := deps.install
	// What the run had said at the instant it replaced the binary. Asserting on
	// the stream afterwards cannot tell a warning printed before the install
	// from one printed after it, which is the whole content of this test.
	var saidAtInstall string
	deps.install = func(b []byte) error {
		saidAtInstall = out.String()
		return replace(b)
	}

	installed, err := performUpgrade(context.Background(), upgradeStreams{out: &out, err: &out}, "0.42.0",
		upgradeOptions{allowUnattested: true}, deps)
	if err != nil {
		t.Fatalf("performUpgrade with --allow-unattested: %v", err)
	}
	if installed != "0.43.0" {
		t.Errorf("installed version = %q, want 0.43.0", installed)
	}
	if got := readFileString(t, target); got != string(binary) {
		t.Errorf("installed binary holds %q, want %q", got, binary)
	}
	if !strings.Contains(saidAtInstall, "unattested") {
		t.Errorf("at the moment it installed, the run had said %q, which does not report that it was installing an unattested release", saidAtInstall)
	}
	if !strings.Contains(saidAtInstall, "--allow-unattested") {
		t.Errorf("at the moment it installed, the run had said %q, which does not name the flag that let it proceed", saidAtInstall)
	}
}

// TestUpgradeRefusesWhenTheAttestationServiceIsUnreachable is the outage case,
// and it is the one that must not be mistaken for absence. A 500 means nobody
// could be asked, not that the release has no attestation; the run still
// refuses by default, because refusing is what "offline" has to mean when the
// alternative is installing on the strength of a digest alone. The flag is
// named, so the user is not stranded by a transient fault.
func TestUpgradeRefusesWhenTheAttestationServiceIsUnreachable(t *testing.T) {
	au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
	au.setServiceStatus(http.StatusInternalServerError)
	target := installedGhost(t, "the old binary")

	_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, au.deps(t, target))
	if err == nil {
		t.Fatal("an upgrade proceeded with the attestation service down")
	}
	if !strings.Contains(err.Error(), "--allow-unattested") {
		t.Errorf("refusal %q does not name the flag that permits it", err)
	}
	// The wording has to keep this apart from a release that has no
	// attestation: a service that answered 500 established nothing, and a user
	// who reads it as a permanent fact about the release will pass the flag on
	// every future upgrade for no reason.
	if strings.Contains(err.Error(), "publishes no build attestation") {
		t.Errorf("refusal %q reports an outage as a release with no attestation", err)
	}
	assertUnchanged(t, target, "the old binary")
}

// TestUpgradeInstallsWhenTheAttestationServiceIsDownAndTheFlagIsGiven keeps
// the outage recoverable: a machine behind a proxy that blocks the attestation
// service must still be able to upgrade, deliberately and noisily.
func TestUpgradeInstallsWhenTheAttestationServiceIsDownAndTheFlagIsGiven(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	au.setServiceStatus(http.StatusInternalServerError)
	target := installedGhost(t, "the old binary")

	var out strings.Builder
	if _, err := performUpgrade(context.Background(), upgradeStreams{out: &out, err: &out}, "0.42.0",
		upgradeOptions{allowUnattested: true}, au.deps(t, target)); err != nil {
		t.Fatalf("performUpgrade with --allow-unattested: %v", err)
	}
	if got := readFileString(t, target); got != string(binary) {
		t.Errorf("installed binary holds %q, want %q", got, binary)
	}
	if said := out.String(); !strings.Contains(said, "--allow-unattested") {
		t.Errorf("the run said %q, which does not name the flag that let it proceed", said)
	}
}

// --- rejection: the state the flag must never reach -----------------------

// TestUpgradeRefusesARejectedBundleWithAndWithoutTheFlag is the most important
// test in this file. --allow-unattested means "this release has no attestation",
// and it must never come to mean "the attestation did not check out": that
// would hand an attacker who can mint one bundle of their own the whole
// feature. So the flag is set in one case and not the other, and both refuse.
func TestUpgradeRefusesARejectedBundleWithAndWithoutTheFlag(t *testing.T) {
	version := strings.TrimPrefix(attestedTag, "v")
	attacker := "https://github.com/attacker/ghost/.github/workflows/release.yml@refs/tags/v" + version
	_, issuer := selfupdate.ReleaseWorkflowIdentity(version)

	for _, flag := range []bool{false, true} {
		name := "without the flag"
		if flag {
			name = "with the flag"
		}
		t.Run(name, func(t *testing.T) {
			binary := []byte("pretend executable")
			au := newAttestedUpgrade(t, attestedTag, binary)
			// A bundle that is present, well-formed, and signed by somebody
			// else's release workflow.
			au.setBundle(mintBundle(t, au.vs, attacker, issuer, au.archiveBytes()))
			target := installedGhost(t, "the old binary")

			_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0",
				upgradeOptions{allowUnattested: flag}, au.deps(t, target))
			if err == nil {
				t.Fatal("a bundle signed by another repository's workflow was accepted")
			}
			if strings.Contains(err.Error(), "--allow-unattested") {
				t.Errorf("refusal %q names the flag, so it reads as a state the flag permits — a rejected bundle is not one", err)
			}
			assertUnchanged(t, target, "the old binary")
		})
	}
}

// TestUpgradeRefusesABundleForAnotherWorkflow makes the same point about the
// narrower failure: a bundle this repository's OWN GitHub account produced, but
// from a workflow that is not the release workflow. It is the bundle an attacker
// with a pull request could get minted, and it is refused identically.
func TestUpgradeRefusesABundleForAnotherWorkflow(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, attestedTag, binary)
	otherWorkflow := "https://github.com/wcatz/ghost/.github/workflows/publish.yml@refs/tags/v0.43.0"
	_, issuer := selfupdate.ReleaseWorkflowIdentity(strings.TrimPrefix(attestedTag, "v"))
	au.setBundle(mintBundle(t, au.vs, otherWorkflow, issuer, au.archiveBytes()))
	target := installedGhost(t, "the old binary")

	_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, au.deps(t, target))
	if err == nil {
		t.Fatal("a bundle from a workflow that is not the release workflow was accepted")
	}
	if strings.Contains(err.Error(), "--allow-unattested") {
		t.Errorf("refusal %q names the flag, which does not reach a rejected bundle", err)
	}
	assertUnchanged(t, target, "the old binary")
}

// --- the cutover ----------------------------------------------------------

// TestUpgradeSkipsTheAttestationBeforeTheCutover is the other half of the
// boundary. A release published before the release workflow minted any
// attestation cannot have one, so requiring it would make every upgrade to a
// pinned old release fail — and the fix a user reaches for would be
// --allow-unattested, which is a worse habit to teach than the one refusal it
// would prevent.
//
// It runs the REAL verifier rather than a stub that errors if called, so the
// assertion is the one that matters: the skip is a decision the production
// verifier makes on the version alone, and it spends no request doing it.
func TestUpgradeSkipsTheAttestationBeforeTheCutover(t *testing.T) {
	binary := []byte("pretend executable")
	au := newAttestedUpgrade(t, preAttestationTag, binary)
	// No bundle exists for this release, and none could.
	target := installedGhost(t, "the old binary")

	if _, err := performUpgrade(context.Background(), discardStreams(), "0.41.0", upgradeOptions{}, au.deps(t, target)); err != nil {
		t.Fatalf("performUpgrade on a pre-cutover release: %v", err)
	}
	if got := readFileString(t, target); got != string(binary) {
		t.Errorf("installed binary holds %q, want %q", got, binary)
	}
	if lookups := au.lookups(); len(lookups) != 0 {
		t.Errorf("the attestation service was asked about %v", lookups)
	}
}

// TestUpgradeSaysSoWhenItSkipsTheAttestationBeforeTheCutover is the other half
// of the previous test. That one pins that the install happens; this pins that
// the user is TOLD, because AttestationVerified covers two different facts here
// and only one of them is a proof: a pre-cutover release is verified because
// nothing could be checked, not because anything vouched for it.
//
// The detail is what distinguishes them, and it used to be dropped, so the
// upgrade reported the same silent success for a release nothing vouched for as
// for one a signed bundle proved. A warning with nothing in it would be noise on
// every upgrade from the cutover onwards, so the second half of this test holds
// that a genuinely verified release stays silent.
func TestUpgradeSaysSoWhenItSkipsTheAttestationBeforeTheCutover(t *testing.T) {
	t.Run("a pre-cutover release says nothing vouched for it", func(t *testing.T) {
		binary := []byte("pretend executable")
		au := newAttestedUpgrade(t, preAttestationTag, binary)
		target := installedGhost(t, "the old binary")

		var warn strings.Builder
		streams := upgradeStreams{out: io.Discard, err: &warn}
		// The floor is below the release, so the upgrade actually proceeds and
		// reaches the attestation check. The warning names the RELEASE being
		// installed, not this argument, which is why the assertion below is
		// against the tag and not against the string here.
		if _, err := performUpgrade(context.Background(), streams, "0.41.0", upgradeOptions{}, au.deps(t, target)); err != nil {
			t.Fatalf("performUpgrade on a pre-cutover release: %v", err)
		}
		got := warn.String()
		if got == "" {
			t.Fatal("the upgrade installed a release nothing vouched for and said nothing, so \"verified\" stands for a release where no check could happen")
		}
		// It has to name the boundary, or the user cannot tell which releases
		// are affected or when this changes.
		if !strings.Contains(got, selfupdate.FirstAttestedVersion) {
			t.Errorf("the warning %q does not name the first attested release, so it does not say what to compare the version against", got)
		}
		if !strings.Contains(got, preAttestationTag[1:]) {
			t.Errorf("the warning %q does not name the release being installed", got)
		}
	})

	t.Run("a genuinely verified release is silent", func(t *testing.T) {
		binary := []byte("pretend executable")
		au := newAttestedUpgrade(t, attestedTag, binary)
		target := installedGhost(t, "the old binary")

		var warn strings.Builder
		streams := upgradeStreams{out: io.Discard, err: &warn}
		if _, err := performUpgrade(context.Background(), streams, strings.TrimPrefix(attestedTag, "v"), upgradeOptions{}, au.deps(t, target)); err != nil {
			t.Fatalf("performUpgrade on an attested release: %v", err)
		}
		if got := warn.String(); got != "" {
			t.Errorf("a release whose bundle verified printed %q, so the pre-cutover warning is not keyed on anything real", got)
		}
	})
}

// TestUpgradeRefusesToSkipTheCheckBeforeTheCutoverWithNoTrustMaterial covers
// the other direction: the cutover is decided from the version alone, and
// nothing about the running binary, the flag, or the absence of a trust root
// can talk the client out of it. A release at the boundary with a verifier that
// would fail on sight is still checked, and still refused when it is absent.
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
	if err == nil {
		t.Fatalf("the release at the cutover (v%s) was installed with no attestation", selfupdate.FirstAttestedVersion)
	}
	if calls != 1 {
		t.Errorf("the attestation check ran %d times at the cutover, want exactly 1", calls)
	}
	assertUnchanged(t, target, "the old binary")
}

// TestUpgradeRefusesWhenThereIsNoVerifierAtAll covers the one branch the
// fixture-based tests above cannot reach: a caller that wires no attestation
// check at all. It is a fail-closed refusal for a release that needs one, never
// a skip — "nobody checked" must not be able to present as "there is nothing to
// check", because that single substitution turns the feature into a no-op
// anywhere the wiring is forgotten.
//
// The flag does not reach it, and the message must not offer it. This branch
// returns before the switch that consults --allow-unattested, so a message
// ending "re-run with --allow-unattested" would send the user into an
// identical refusal — and this is the branch whose entire purpose is to be a
// legible dead end, because it is only reached when ghost's own wiring is
// broken.
func TestUpgradeRefusesWhenThereIsNoVerifierAtAll(t *testing.T) {
	for _, flag := range []bool{false, true} {
		name := "without the flag"
		if flag {
			name = "with the flag"
		}
		t.Run(name, func(t *testing.T) {
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
				upgradeOptions{allowUnattested: flag}, upgradeDeps{
					install: func([]byte) error { calls++; return nil },
				})
			if err == nil {
				t.Fatal("a post-cutover release was installed with no attestation check wired in at all")
			}
			if strings.Contains(err.Error(), "--allow-unattested") {
				t.Errorf("refusal %q names a flag that this branch never consults, so it sends the user into an identical refusal", err)
			}
			// It is a bug in ghost, not a property of the release, and the user
			// is the only one who can report it.
			if !strings.Contains(err.Error(), "issue") {
				t.Errorf("refusal %q does not say this is a ghost bug worth reporting, so a broken build is a dead end with no way out", err)
			}
			if calls != 0 {
				t.Errorf("the installer ran %d time(s) with no verifier in place", calls)
			}
			assertUnchanged(t, target, "the old binary")
		})
	}
}

// TestUpgradeRefusesAnUnrecognisedAttestationState is the same shape for a state
// this build has no policy for. It is refused, nothing is offered that would
// change the answer, and the state is named so a report is actionable.
func TestUpgradeRefusesAnUnrecognisedAttestationState(t *testing.T) {
	for _, flag := range []bool{false, true} {
		name := "without the flag"
		if flag {
			name = "with the flag"
		}
		t.Run(name, func(t *testing.T) {
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
			if err == nil {
				t.Fatal("an unrecognised attestation state was treated as a pass")
			}
			if strings.Contains(err.Error(), "--allow-unattested") {
				t.Errorf("refusal %q names a flag that an unrecognised state never reaches", err)
			}
			if !strings.Contains(err.Error(), "unknown") {
				t.Errorf("refusal %q does not name the state it saw, so a report could not be acted on: %v", err, err)
			}
			if calls != 0 {
				t.Errorf("the installer ran %d time(s) for an unrecognised attestation state", calls)
			}
			assertUnchanged(t, target, "the old binary")
		})
	}
}

// TestUpgradeSkipsTheCheckBeforeTheCutoverWithNoVerifier is the boundary from
// the other side, and it is why the branch above is a refusal rather than an
// unconditional one: a release that predates the attestation is not made
// uninstallable by the absence of a verifier, because none could have been
// consulted for it anyway.
func TestUpgradeSkipsTheCheckBeforeTheCutoverWithNoVerifier(t *testing.T) {
	rs := newReleaseServer(t, preAttestationTag, []byte("pretend executable"))
	rel, err := rs.fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch the fake release: %v", err)
	}
	asset, err := selfupdate.FindAsset(rel)
	if err != nil {
		t.Fatalf("FindAsset: %v", err)
	}
	target := installedGhost(t, "the old binary")

	if err := installRelease(context.Background(), discardStreams(), rel, asset, upgradeOptions{}, upgradeDeps{
		install: installOver(target),
	}); err != nil {
		t.Fatalf("installRelease on a pre-cutover release with no verifier: %v", err)
	}
	if got := readFileString(t, target); got != "pretend executable" {
		t.Errorf("installed binary holds %q, want the release's binary", got)
	}
}

// --- ordering -------------------------------------------------------------

// TestUpgradeVerifiesTheAttestationBeforeUnpackingTheArchive pins the order,
// which is the part of the property the refusals above cannot see. The archive
// served here is not an archive, and both published digests vouch for it
// correctly, so the only check that can refuse is one that runs before the
// bytes are parsed. Without the order, an archive nothing vouched for is handed
// to a decompressor.
//
// It also pins the half that could be got wrong the other way: the archive IS
// downloaded first, because the lookup is keyed on the downloaded bytes and
// asking by the digest the release reports would consult the attacker's own
// metadata about the attacker's own bytes. A check that could refuse before the
// transfer would be a check that trusts the release's own claim about the
// archive, which is the thing being checked. The transfer is the cost of doing
// this without trusting anything; refusing before the unpack is what it buys.
func TestUpgradeVerifiesTheAttestationBeforeUnpackingTheArchive(t *testing.T) {
	notAnArchive := []byte("substituted bytes, not an archive")
	au := newAttestedUpgrade(t, attestedTag, []byte("pretend executable"))
	// Both published digests agree with the substituted bytes, so they cannot
	// be what refuses.
	au.release.swapManifest(sha256HexDigest(notAnArchive) + "  " + selfupdate.AssetName("0.43.0") + "\n")
	au.release.swapArchive(notAnArchive)
	au.release.setDigest("sha256:" + sha256HexDigest(notAnArchive))
	// And the attestation service has nothing for those bytes either.
	target := installedGhost(t, "the old binary")

	_, err := performUpgrade(context.Background(), discardStreams(), "0.42.0", upgradeOptions{}, au.deps(t, target))
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if !strings.Contains(err.Error(), "attestation") {
		t.Errorf("error %q should name the attestation check, so the archive is never unpacked before something vouches for it", err)
	}
	for _, parseFailure := range []string{"gzip: ", "zip: ", "tar: ", "ghost binary not found in archive"} {
		if strings.Contains(err.Error(), parseFailure) {
			t.Errorf("error %q came from unpacking the archive, which must happen only after the attestation agrees", err)
		}
	}
	var downloaded bool
	for _, p := range au.release.askedFor() {
		if p == archiveAssetPath {
			downloaded = true
		}
	}
	if !downloaded {
		t.Error("the archive was never downloaded, so this test would pass on a client that refused for the wrong reason")
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
