package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strconv"
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

	"github.com/klauspost/compress/snappy"
)

// The consumer side of the release-authenticity contract (#694): the release
// mints a Sigstore attestation for every asset (guarded by
// release_workflow_test.go), and this file is what decides whether a given
// attestation may be installed. Every fixture here is minted locally from a
// virtual Fulcio and a virtual TSA, so nothing in this file reaches GitHub, the
// Sigstore TUF repository, or the network.

// attestationFixture is a virtual Sigstore plus the trust root that vouches
// for it. Minting against one and verifying against a trustRoot built from the
// SAME vs is the happy path; a test that wants a failure mints against a
// different one.
type attestationFixture struct {
	vs *ca.VirtualSigstore
	// trustRoot is built from vs.
	trustRoot *root.TrustedRoot
	// artifact is the release archive the attestation claims to cover.
	artifact []byte
}

func newAttestationFixture(t *testing.T, artifact []byte) *attestationFixture {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	return &attestationFixture{vs: vs, trustRoot: trustedRootFrom(t, vs), artifact: artifact}
}

// trustedRootFrom turns a virtual Sigstore into the same kind of value
// production gets from TUF. root.NewTrustedRoot is the concrete constructor;
// no test-only interface is needed for the trust root to be injectable, because
// sigstore-go already models it as one.
func trustedRootFrom(t *testing.T, vs *ca.VirtualSigstore) *root.TrustedRoot {
	t.Helper()
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		vs.FulcioCertificateAuthorities(), vs.CTLogs(), vs.TimestampingAuthorities(), vs.RekorLogs())
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	return tr
}

// bundleJSON mints a wire-format v0.3 Sigstore bundle for artifact, signed by
// identity under issuer. This mirrors what GitHub's attestation service returns
// for a release workflow: a DSSE envelope, the Fulcio leaf certificate, and one
// RFC3161 timestamp — and NO tlog entry, because a bundle the service serves
// from blob storage carries only those three things.
// bundleJSON mints a bundle for artifact. The optional at sets the trusted
// timestamp the bundle carries; the default is five minutes from now, which is
// the only value every other fixture in this file wants. It does NOT move the
// certificate — ca.VirtualSigstore hardcodes leaf validity to now±5h — so `at`
// can make the signature OLDER than the certificate, never the reverse.
func (f *attestationFixture) bundleJSON(t *testing.T, identity, issuer string, artifact []byte, at ...time.Time) []byte {
	t.Helper()
	stamp := time.Now().Add(5 * time.Minute)
	if len(at) > 0 {
		stamp = at[0]
	}
	sum := sha256.Sum256(artifact)
	statement := fmt.Sprintf(
		`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"ghost.tar.gz","digest":{"sha256":"%s"}}],`+
			`"predicateType":"https://slsa.dev/provenance/v1","predicate":{}}`,
		hex.EncodeToString(sum[:]))

	te, err := f.vs.AttestAtTime(identity, issuer, []byte(statement), stamp, false)
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

	// The proto's payload and sig fields are `bytes` and protojson base64s
	// them on the way out, so the base64 STRINGS the DSSE envelope carries
	// have to be decoded first. Copying them verbatim would encode a base64
	// string a second time, and RFC3161 verification would then fail against
	// a message imprint that covers the decoded signature.
	payload, err := base64.StdEncoding.DecodeString(envelope.Payload)
	if err != nil {
		t.Fatalf("decode envelope payload: %v", err)
	}
	sigs := make([]*protodsse.Signature, 0, len(envelope.Signatures))
	for _, s := range envelope.Signatures {
		raw, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			t.Fatalf("decode envelope signature: %v", err)
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

// --- The cutover boundary -------------------------------------------------

// TestAttestationRequiredFor pins the version constant, because it is the one
// decision in this feature that is a choice rather than a computation: a
// release at or after it must carry an attestation, one before it cannot,
// since it was published before the release workflow minted any. It also pins
// the two ends that a later editor gets wrong: the boundary is INCLUSIVE at
// FirstAttestedVersion, and a version this package cannot order counts as
// required, because "cannot tell" must not read as "old enough to skip".
func TestAttestationRequiredFor(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"0.42.0", false},
		{"0.42.9", false},
		{"0.43.0", true}, // the boundary is inclusive
		{"0.43.1", true},
		{"0.44.0", true},
		{"1.0.0", true},
		{"v0.43.0", true}, // a release tag arrives with its "v"
		{"0.43.0+build.5", true},
		{"dev", true},         // unorderable is not "older than the cutover"
		{"nightly", true},     // nor is a tag that is not a version at all
		{"0.43.0-rc.1", true}, // a candidate is not the release itself
	}
	for _, tc := range tests {
		if got := AttestationRequiredFor(tc.version); got != tc.want {
			t.Errorf("AttestationRequiredFor(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// TestFirstAttestedVersionIsOrderable keeps the constant usable: a cutover
// that CompareVersions cannot order would make every release either exempt or
// required by accident, and the constant is the only thing the boundary reads.
func TestFirstAttestedVersionIsOrderable(t *testing.T) {
	if !IsRelease(FirstAttestedVersion) {
		t.Fatalf("FirstAttestedVersion = %q, which is not a release version", FirstAttestedVersion)
	}
	if got := AttestationRequiredFor(FirstAttestedVersion); !got {
		t.Fatalf("FirstAttestedVersion = %q is not required by its own cutover", FirstAttestedVersion)
	}
}

// --- The identity the release workflow must prove -----------------------

// TestReleaseWorkflowIdentity pins the SAN and issuer the certificate has to
// carry. The SAN names the workflow by path AND by the tag ref it ran from, so
// an attestation minted by the same file running on a branch does not pass; the
// issuer pins it to GitHub's OIDC provider, so an identity nobody but GitHub
// can issue is not accepted.
func TestReleaseWorkflowIdentity(t *testing.T) {
	san, issuer := ReleaseWorkflowIdentity("0.43.0")
	wantSAN := "https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/v0.43.0"
	if san != wantSAN {
		t.Errorf("SAN = %q, want %q", san, wantSAN)
	}
	if issuer != "https://token.actions.githubusercontent.com" {
		t.Errorf("issuer = %q, want GitHub's OIDC issuer", issuer)
	}
}

// TestReleaseWorkflowIdentityUsesATagRef keeps the ref in the identity a TAG
// ref for every version, including a prerelease. The one shape that must never
// be produced is a branch ref, which is what an attestation from a manual run
// on main looks like and is exactly what the client must refuse.
func TestReleaseWorkflowIdentityUsesATagRef(t *testing.T) {
	for _, v := range []string{"0.43.0", "1.0.0", "0.44.0-rc.1"} {
		san, _ := ReleaseWorkflowIdentity(v)
		if !strings.HasPrefix(san, "https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/") {
			t.Errorf("ReleaseWorkflowIdentity(%q) SAN = %q, want a refs/tags/ ref", v, san)
		}
		if strings.Contains(san, "refs/heads/") {
			t.Errorf("ReleaseWorkflowIdentity(%q) SAN = %q, which names a branch", v, san)
		}
	}
}

// --- Verification ---------------------------------------------------------

// TestVerifyReleaseAttestation is the whole accept case: a bundle minted by
// this repository's release workflow on the release tag, over the bytes the
// client downloaded. It is the case that has to keep working, and it is the
// one that fails silently if the cutover or the identity is wrong.
// TestTheVerifierTakesTheTimestampNotTheWallClock guards the verifier's TIME
// POLICY, and it is a source assertion rather than a behavioural one. That is a
// deliberate fallback, and this comment is where the evidence lives.
//
// The natural behavioural test is an EXPIRED certificate verified through a
// timestamp inside its validity window — the case that only an
// observer-timestamp verifier can pass. It cannot be written here, and I tried
// both directions before concluding that rather than assuming it:
//
//   - ca.VirtualSigstore mints leaves with NotBefore/NotAfter hardcoded to
//     now±5h (pkg/testing/ca/ca.go), and AttestAtTime moves the integrated
//     timestamp, not the certificate. `GenerateLeafCert(subject, issuer,
//     expiration, …)` takes an expiration, but the virtual CA does not use it
//     for the leaf it issues.
//   - the only timestamp signer in the dependency graph stamps genTime with
//     time.Now() with no injectable time source
//     (github.com/sigstore/timestamp-authority/v2/pkg/api/timestamp.go:184), so
//     no bundle reachable from this package can carry a timestamp inside a PAST
//     certificate's window.
//
// What I then tried was the same disagreement run backwards — a certificate
// that is valid NOW with a thirty-day-old timestamp, where the observer
// verifier should refuse (the certificate did not exist then) and the wall-clock
// verifier should accept. Measured: BOTH accept. And in the other direction, a
// `WithCurrentTime()` verifier does not refuse a bundle that carries a trusted
// timestamp, despite its documentation. So sigstore-go's two policies are not
// separable by any bundle this package can mint, and a behavioural test would
// be a test that passes for reasons unrelated to the property.
//
// Hence the source assertion: the production verifier must be built from
// observer timestamps, and must not carry a wall-clock policy. The control
// proves the matcher is sensitive — a string that DOES contain the wall-clock
// option is rejected by the same code.
func TestTheVerifierTakesTheTimestampNotTheWallClock(t *testing.T) {
	raw, err := os.ReadFile("attestation.go")
	if err != nil {
		t.Fatalf("read attestation.go: %v", err)
	}
	body, ok := functionBody(t, string(raw), "newReleaseVerifier")
	if !ok {
		t.Fatal("no newReleaseVerifier in attestation.go, so the verifier's time policy is assembled somewhere ungoverned")
	}
	if usesWallClockPolicy(body) {
		t.Errorf("newReleaseVerifier configures a wall-clock time policy, so the certificate would be checked at time.Now() instead of at the bundle's timestamp. A Fulcio leaf is valid for about ten minutes, so every real release would then be refused as expired — AttestationUnverifiable, which no flag reaches:\n%s", body)
	}
	if !strings.Contains(body, "WithObserverTimestamps") {
		t.Errorf("newReleaseVerifier does not ask for an observer timestamp, so a signature with no trusted timestamp counts as observed:\n%s", body)
	}
	if !strings.Contains(body, strconv.Itoa(attestationObserverTimestamps)) {
		t.Errorf("newReleaseVerifier does not use attestationObserverTimestamps, so the tolerance is decided somewhere else:\n%s", body)
	}
	// "The only place this package configures a verifier" has to be true, not
	// merely said. A second call site would be a second time policy, assembled
	// where this test cannot see it — and the bypass is exactly the shape of
	// change this assertion exists to catch.
	if n := strings.Count(string(raw), "verify.NewVerifier("); n != 1 {
		t.Errorf("verify.NewVerifier appears %d times in attestation.go, want 1: every call site is a place the time policy can be assembled without newReleaseVerifier governing it", n)
	}

	// The control. Without it, a matcher that always returned false would make
	// every assertion above pass.
	for _, tc := range []struct {
		name string
		src  string
		want bool
	}{
		{"a verifier with a wall-clock policy", "return verify.NewVerifier(t, verify.WithCurrentTime())", true},
		{"a verifier with both policies", "return verify.NewVerifier(t, verify.WithObserverTimestamps(1), verify.WithCurrentTime())", true},
		{"the production verifier", "return verify.NewVerifier(t, verify.WithObserverTimestamps(attestationObserverTimestamps))", false},
		{
			// The reason the matcher strips comments: the production function
			// NAMES the option it must not use, in the comment explaining why.
			// A matcher that read prose would fail on its own documentation.
			name: "a comment naming the option it does not use",
			src:  "// not WithCurrentTime: the leaf lives ten minutes\nreturn verify.NewVerifier(t, verify.WithObserverTimestamps(1))",
			want: false,
		},
	} {
		if got := usesWallClockPolicy(tc.src); got != tc.want {
			t.Errorf("usesWallClockPolicy(%q) = %v, want %v", tc.src, got, tc.want)
		}
	}
}

// usesWallClockPolicy reports whether source configures a wall-clock
// verification time. The option is named rather than inferred from behaviour,
// because the two policies are not separable by any bundle this package can mint
// — see the test above for the measurements.
//
// Comments are stripped first, and that is not a convenience: the production
// function NAMES the option it must not use, in the comment explaining why it
// must not. Matching prose would fail on its own documentation.
func usesWallClockPolicy(source string) bool {
	for _, line := range strings.Split(source, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		if strings.Contains(line, "WithCurrentTime") {
			return true
		}
	}
	return false
}

// functionBody returns the body of the named function, braces balanced. It
// exists so the assertion above reads a function rather than the whole file,
// where an unrelated mention of the option would be a false alarm.
func functionBody(t *testing.T, source, name string) (string, bool) {
	t.Helper()
	start := strings.Index(source, "func "+name+"(")
	if start < 0 {
		return "", false
	}
	open := strings.Index(source[start:], "{")
	if open < 0 {
		return "", false
	}
	depth, i := 0, start+open
	for ; i < len(source); i++ {
		switch source[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start+open : i+1], true
			}
		}
	}
	return "", false
}

func TestVerifyReleaseAttestation(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	san, issuer := ReleaseWorkflowIdentity(version)
	bundle := fx.bundleJSON(t, san, issuer, artifact)

	if err := VerifyReleaseAttestation([][]byte{bundle}, version, artifact, fx.trustRoot); err != nil {
		t.Fatalf("VerifyReleaseAttestation: %v", err)
	}
}

// TestVerifyReleaseAttestationRefuses covers every way a present attestation
// can fail to prove what it claims. Each is fatal by design — the client has
// nothing to fall back to, and --allow-unattested does not reach them — so the
// test asserts an error and no silent pass.
//
// The three identity cases are the ones that matter most: a bundle signed by a
// DIFFERENT repository's workflow of the same name, one signed by a different
// workflow in this repository, and one whose cert is issued by an untrusted CA
// are all bundles that verify cryptographically and still must not install.
func TestVerifyReleaseAttestationRefuses(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	san, issuer := ReleaseWorkflowIdentity(version)
	otherSAN := "https://github.com/wcatz/other/.github/workflows/release.yml@refs/tags/v" + version
	otherWorkflow := "https://github.com/wcatz/ghost/.github/workflows/publish.yml@refs/tags/v" + version
	branchSAN := "https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/heads/main"

	tests := []struct {
		name     string
		identity string
		issuer   string
		artifact []byte
		version  string
		trusted  func(*attestationFixture) *root.TrustedRoot
	}{
		{
			name: "another repository's release workflow", identity: otherSAN, issuer: issuer,
			artifact: artifact, version: version, trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			name: "another workflow in this repository", identity: otherWorkflow, issuer: issuer,
			artifact: artifact, version: version, trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			name: "a branch ref instead of the tag", identity: branchSAN, issuer: issuer,
			artifact: artifact, version: version, trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			name: "an issuer that is not GitHub's", identity: san, issuer: "https://accounts.google.com",
			artifact: artifact, version: version, trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			// The archive the client downloaded is not the one the attestation
			// covers. This is the substitution the whole feature exists for,
			// and the digest is what catches it.
			name: "a bundle minted for different bytes", identity: san, issuer: issuer,
			artifact: []byte("a different archive"), version: version, trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			// The bundle is for this release, but the client is installing
			// another one: the same bytes released twice, the second time
			// under a tag no workflow ever signed for.
			name: "a bundle minted for another version", identity: san, issuer: issuer,
			artifact: artifact, version: "0.44.0", trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			// A bundle no trust root vouches for. The certificate chains to
			// nothing this client trusts, so the identity is never even
			// consulted.
			name: "an untrusted certificate authority", identity: san, issuer: issuer,
			artifact: artifact, version: version, trusted: func(f *attestationFixture) *root.TrustedRoot {
				other, err := ca.NewVirtualSigstore()
				if err != nil {
					t.Fatalf("NewVirtualSigstore: %v", err)
				}
				return trustedRootFrom(t, other)
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fx := newAttestationFixture(t, artifact)
			bundle := fx.bundleJSON(t, tc.identity, tc.issuer, tc.artifact)
			if err := VerifyReleaseAttestation([][]byte{bundle}, tc.version, artifact, tc.trusted(fx)); err == nil {
				t.Fatalf("VerifyReleaseAttestation accepted a bundle from %s", tc.name)
			}
		})
	}
}

// TestVerifyReleaseAttestationAcceptsOneGoodBundleAmongBad keeps the rule
// "any bundle that verifies is enough" honest. GitHub returns one attestation
// per workflow run, so a release legitimately carries more than one, and an
// attacker who can add a bundle must not be able to make the release
// uninstallable by poisoning it. The failure below is the opposite mistake and
// is the one that would strand a user: one bad bundle among good ones must not
// stop the install.
func TestVerifyReleaseAttestationAcceptsOneGoodBundleAmongBad(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	san, issuer := ReleaseWorkflowIdentity(version)

	good := fx.bundleJSON(t, san, issuer, artifact)
	wrongRepo := fx.bundleJSON(t, "https://github.com/attacker/ghost/.github/workflows/release.yml@refs/tags/v"+version, issuer, artifact)

	if err := VerifyReleaseAttestation([][]byte{wrongRepo, good}, version, artifact, fx.trustRoot); err != nil {
		t.Fatalf("a good bundle was rejected because another one failed: %v", err)
	}
	if err := VerifyReleaseAttestation([][]byte{good, wrongRepo}, version, artifact, fx.trustRoot); err != nil {
		t.Fatalf("a good bundle was rejected because another one failed: %v", err)
	}
}

// TestVerifyReleaseAttestationRefusesAnEmptyList keeps "nothing to check" from
// reading as "nothing wrong". The caller distinguishes absent from rejected,
// so this has to be an error rather than a nil.
func TestVerifyReleaseAttestationRefusesAnEmptyList(t *testing.T) {
	if err := VerifyReleaseAttestation(nil, "0.43.0", []byte("x"), newAttestationFixture(t, nil).trustRoot); err == nil {
		t.Fatal("VerifyReleaseAttestation accepted an empty bundle list")
	}
}

// TestVerifyReleaseAttestationNamesWhatFailed keeps a refusal debuggable. The
// user is told the upgrade cannot be verified and has nowhere to go but the
// loud flag or a manual install, so the reason has to be in the message rather
// than only in a log nobody reads. It must not, however, quote the rejected
// identity back: the identity is attacker-supplied text, and a message that
// echoes it invites a user to read it as a name to trust.
func TestVerifyReleaseAttestationNamesWhatFailed(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	_, issuer := ReleaseWorkflowIdentity(version)
	attacker := "https://github.com/attacker/ghost/.github/workflows/release.yml@refs/tags/v" + version
	bundle := fx.bundleJSON(t, attacker, issuer, artifact)

	err := VerifyReleaseAttestation([][]byte{bundle}, version, artifact, fx.trustRoot)
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if msg := err.Error(); strings.Contains(msg, "attacker/ghost") {
		t.Errorf("refusal quotes the rejected identity verbatim: %q", msg)
	} else if !strings.Contains(msg, "attestation") {
		t.Errorf("refusal does not say what was refused: %q", msg)
	}
	// The accepted identity is named instead, so the user can tell which
	// workflow the client was looking for.
	if msg := err.Error(); !strings.Contains(msg, "wcatz/ghost") {
		t.Errorf("refusal does not name the workflow it required: %q", msg)
	}
}

// TestVerifyReleaseAttestationRejectsAMalformedBundle keeps bytes that are not
// a bundle from reaching the parser's error as a panic, and keeps a truncated
// or rewritten body from being read as "no attestation to check".
func TestVerifyReleaseAttestationRejectsAMalformedBundle(t *testing.T) {
	fx := newAttestationFixture(t, []byte("a release archive"))
	artifact := []byte("a release archive")
	san, issuer := ReleaseWorkflowIdentity("0.43.0")
	good := fx.bundleJSON(t, san, issuer, artifact)

	for _, tc := range []struct {
		name string
		body []byte
	}{
		{"not json", []byte("this is not a bundle")},
		{"an empty body", nil},
		{"a truncated bundle", good[:len(good)/2]},
		{"a bundle with a rewritten payload", rewriteBundlePayload(t, good, []byte(`{"_type":"https://in-toto.io/Statement/v1"}`))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := VerifyReleaseAttestation([][]byte{tc.body}, "0.43.0", artifact, fx.trustRoot); err == nil {
				t.Fatalf("accepted a malformed bundle: %s", tc.name)
			}
		})
	}
}

// rewriteBundlePayload replaces the DSSE payload of a wire bundle and leaves
// the signature alone, which is what a tampered attestation looks like.
func rewriteBundlePayload(t *testing.T, bundle []byte, payload []byte) []byte {
	t.Helper()
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(bundle, &doc); err != nil {
		t.Fatalf("unmarshal bundle: %v", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(doc["dsseEnvelope"], &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	envelope["payload"] = encoded
	doc["dsseEnvelope"], err = json.Marshal(envelope)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return out
}

// sameJSON compares two bundle documents by value rather than by bytes.
// protojson deliberately varies its whitespace so that a client cannot depend
// on a bundle's exact serialisation, which means the bytes round-tripping
// through a server are not always the bytes that were minted — and a test that
// compared them would be asserting on the serialiser, not on the fetch.
func sameJSON(t *testing.T, got, want []byte, what string) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("unmarshal the %s: %v", what, err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("unmarshal the expected %s: %v", what, err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Errorf("the %s is not what was published:\n got %s\nwant %s", what, got, want)
	}
}

// --- Fetching -------------------------------------------------------------

// attestationServer is a fake GitHub attestations endpoint. The shape is the
// one the real service returns, both halves of it: `bundle` inline, and
// `bundle_url` pointing at a snappy-compressed copy, which is what the service
// actually serves for a GitHub-initiated attestation.
type attestationServer struct {
	// mu guards served and the two knobs a test turns after start: the handlers
	// run on the httptest server's goroutines while the test reads and writes
	// on its own, and a socket carries no happens-before edge the race detector
	// can see. CI runs this repository under -race.
	mu         sync.Mutex
	inline     json.RawMessage
	snappyBody []byte
	// served records the digests this server was asked about, so a test can
	// assert the lookup used the bytes the client downloaded.
	served []string
	// status and body override the 200 for a failure case.
	status int
	body   string
	// blobStatus, when non-zero, is the status the bundle blob answers with,
	// so a test can make the SECOND request fail while the index succeeds.
	blobStatus int
}

func (s *attestationServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		s.mu.Lock()
		body, status := s.snappyBody, s.blobStatus
		s.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/x-snappy")
		_, _ = w.Write(body)
	}))
	t.Cleanup(blob.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, r *http.Request) {
		digest := strings.TrimPrefix(r.URL.Path, "/repos/wcatz/ghost/attestations/")
		s.mu.Lock()
		s.served = append(s.served, digest)
		status, bodyText, inline := s.status, s.body, s.inline
		s.mu.Unlock()

		if status != 0 {
			w.WriteHeader(status)
			_, _ = io.WriteString(w, bodyText)
			return
		}
		member := map[string]any{
			"repository_id": 1,
			"initiator":     "github",
			"bundle_url":    blob.URL + "/bundle.sn",
			"bundle":        nil,
		}
		if inline != nil {
			member["bundle"] = inline
			member["bundle_url"] = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"attestations": []any{member}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// servedFor lists the digests this server was asked about, in order.
func (s *attestationServer) servedFor() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.served...)
}

// setBlobStatus makes the bundle blob answer with a status instead of a bundle,
// so a test can fail the SECOND request while the index succeeds — which is the
// case that must not be reported as a release with no attestation.
func (s *attestationServer) setBlobStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobStatus = status
}

// snappyFramed compresses data the way the attestation service's blob store
// serves it: the snappy framing format, not the bare block format.
func snappyFramed(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := snappy.NewBufferedWriter(&buf)
	if _, err := w.Write(data); err != nil {
		t.Fatalf("snappy write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("snappy close: %v", err)
	}
	return buf.Bytes()
}

// TestFetchAttestationBundles covers the two shapes the service really returns.
// `bundle` is null on a repository whose workflow used actions/attest-build-provenance
// — the action uploads to the store and GitHub links it — so the snappy path is
// the one that runs in production and the inline path is the one that must
// still work. Getting this wrong would make every upgrade report "no
// attestation" on a release that has one.
func TestFetchAttestationBundles(t *testing.T) {
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	san, issuer := ReleaseWorkflowIdentity("0.43.0")
	bundle := fx.bundleJSON(t, san, issuer, artifact)
	digest := sha256.Sum256(artifact)

	t.Run("inline bundle", func(t *testing.T) {
		srv := &attestationServer{inline: bundle}
		ts := srv.start(t)
		got, err := FetchAttestationBundles(context.Background(), ts.URL, hexDigest(digest[:]))
		if err != nil {
			t.Fatalf("FetchAttestationBundles: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("FetchAttestationBundles returned %d bundles, want 1", len(got))
		}
		sameJSON(t, got[0], bundle, "inline bundle")
		if looked := srv.servedFor(); len(looked) != 1 || looked[0] != "sha256:"+hexDigest(digest[:]) {
			t.Fatalf("looked up %v, want the digest the client downloaded", looked)
		}
	})

	t.Run("snappy bundle url", func(t *testing.T) {
		srv := &attestationServer{snappyBody: snappyFramed(t, bundle)}
		ts := srv.start(t)
		got, err := FetchAttestationBundles(context.Background(), ts.URL, hexDigest(digest[:]))
		if err != nil {
			t.Fatalf("FetchAttestationBundles: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("snappy bundle_url did not round-trip: got %d bundles, want 1", len(got))
		}
		sameJSON(t, got[0], bundle, "snappy bundle")
	})
}

// TestFetchAttestationBundlesReportsAbsence is the distinction the policy rests
// on. "GitHub holds no attestation for these bytes" is a fact about the
// release, and it is the only case --allow-unattested is meant to override. A
// 404 from the INDEX and an empty list are that same fact; neither is a fetch
// failure, and conflating the two would mean a release that never had an
// attestation reads like an outage.
func TestFetchAttestationBundlesReportsAbsence(t *testing.T) {
	const digest = "0000000000000000000000000000000000000000000000000000000000000000"

	t.Run("404", func(t *testing.T) {
		srv := &attestationServer{status: http.StatusNotFound, body: `{"message":"Not Found"}`}
		ts := srv.start(t)
		_, err := FetchAttestationBundles(context.Background(), ts.URL, digest)
		if !errors.Is(err, ErrNoAttestation) {
			t.Fatalf("404 gave %v, want ErrNoAttestation", err)
		}
	})

	t.Run("an empty list", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"attestations":[]}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		_, err := FetchAttestationBundles(context.Background(), ts.URL, digest)
		if !errors.Is(err, ErrNoAttestation) {
			t.Fatalf("empty list gave %v, want ErrNoAttestation", err)
		}
	})

	t.Run("a member with neither bundle nor url", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"attestations":[{"repository_id":1,"initiator":"github","bundle":null,"bundle_url":null}]}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		// NOT absence. The index listed an attestation, so the release has one
		// according to the service; the member is simply unreadable, and a user
		// told "this release has no attestation" would pass a flag that means
		// something quite different.
		_, err := FetchAttestationBundles(context.Background(), ts.URL, digest)
		if err == nil {
			t.Fatal("a member with no bundle was accepted as a readable attestation")
		}
		if errors.Is(err, ErrNoAttestation) {
			t.Fatalf("a member with no bundle read as absence: %v", err)
		}
		// The reason has to survive into the message, or the user is told the
		// check could not be completed and not why.
		if !strings.Contains(err.Error(), "carries neither") {
			t.Errorf("error %q does not say which member could not be read", err)
		}
	})
}

// TestFetchAttestationBundlesReportsAFetchFailure is the other half of the
// distinction: an outage is not absence. If a 500 read as ErrNoAttestation the
// user would be told a release has no attestation when the truth is that
// nobody could be asked, and the loud flag would become the only way through a
// transient network fault.
func TestFetchAttestationBundlesReportsAFetchFailure(t *testing.T) {
	const digest = "0000000000000000000000000000000000000000000000000000000000000000"

	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"a 500", http.StatusInternalServerError, `{"message":"boom"}`},
		{"a 403 from a rate limit", http.StatusForbidden, `{"message":"rate limited"}`},
		{"unparseable JSON", http.StatusOK, `not json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &attestationServer{status: tc.status, body: tc.body}
			ts := srv.start(t)
			_, err := FetchAttestationBundles(context.Background(), ts.URL, digest)
			if err == nil {
				t.Fatal("expected an error")
			}
			if errors.Is(err, ErrNoAttestation) {
				t.Fatalf("%s read as absence: %v", tc.name, err)
			}
		})
	}

	t.Run("a dead server", func(t *testing.T) {
		ts := &attestationServer{}
		srv := ts.start(t)
		srv.Close() // nothing is listening now
		_, err := FetchAttestationBundles(context.Background(), srv.URL, digest)
		if err == nil {
			t.Fatal("expected an error")
		}
		if errors.Is(err, ErrNoAttestation) {
			t.Fatalf("a dead server read as absence: %v", err)
		}
	})

	// The one that is NOT a fetch failure of the index, and the one the whole
	// policy hangs on. The index answered 200 and said this release has an
	// attestation; the blob it then pointed at is gone. That is "could not
	// check", and it must never be reported as "this release has no
	// attestation" — the user would read it as a fact about the release and
	// pass the flag meant for a release that never had one.
	t.Run("a 404 on the bundle the index pointed at", func(t *testing.T) {
		srv := &attestationServer{}
		srv.setBlobStatus(http.StatusNotFound)
		ts := srv.start(t)
		_, err := FetchAttestationBundles(context.Background(), ts.URL, digest)
		if err == nil {
			t.Fatal("a member whose bundle is gone was accepted")
		}
		if errors.Is(err, ErrNoAttestation) {
			t.Fatalf("a dead bundle blob read as absence: %v", err)
		}
		// The SENTENCE matters as much as the classification, because the
		// classification is right by construction here (a formatted string does
		// not wrap ErrNoAttestation) while the text is quoted verbatim into the
		// message a user reads. A blob 404 leaking the absence wording into it
		// is the same defect as classifying it as absence, told differently.
		if strings.Contains(err.Error(), "no attestation is published") {
			t.Errorf("a dead bundle blob produced absence wording in the message: %v", err)
		}
		if !strings.Contains(err.Error(), "none of them could be read") {
			t.Errorf("error %q should say the index listed an attestation that could not be read", err)
		}
	})
}

// TestAttestationVerifierCheckKeepsAGoodBundleWhenAnotherIsUnreadable is the
// same fault seen from the other side, and it is the one an attacker could aim
// for. A release legitimately has several attestations — one per workflow run —
// so the index is a list, and a list where one member's presigned link has
// expired must still yield the bundles the other members carried. Refusing the
// whole lookup would let anyone able to publish one extra (unreadable)
// attestation make a release uninstallable, which is the same denial the "any
// verifying bundle is enough" rule exists to prevent.
func TestAttestationVerifierCheckKeepsAGoodBundleWhenAnotherIsUnreadable(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	san, issuer := ReleaseWorkflowIdentity(version)
	good := fx.bundleJSON(t, san, issuer, artifact)

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/wcatz/ghost/attestations/") {
			// The dead presigned link. 404, and a 404 that is a fault here.
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = fmt.Fprintf(w,
			`{"attestations":[{"repository_id":1,"initiator":"user","bundle":%s,"bundle_url":null},`+
				`{"repository_id":1,"initiator":"github","bundle":null,"bundle_url":"%s/gone"}]}`,
			good, ts.URL)
	}))
	t.Cleanup(ts.Close)

	v := &AttestationVerifier{
		APIBaseURL:      ts.URL,
		TrustedMaterial: staticTrustMaterial(fx.trustRoot),
	}
	if got := v.Check(context.Background(), artifact, version); got.State != AttestationVerified {
		t.Fatalf("a release with one readable attestation reported %s, want verified: %s", got.State, got.Detail)
	}
}

// TestAttestationVerifierCheckBoundsTheTrustRootFetch keeps the TUF round trip
// on the same clock as the two requests before it. In production Check's
// context is the whole 12-minute upgrade budget, so a stalled Sigstore endpoint
// with no bound of its own would hold the command open for minutes and then
// report the budget — a long wait followed by the wrong explanation.
func TestAttestationVerifierCheckBoundsTheTrustRootFetch(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")

	old := attestationTimeout
	attestationTimeout = 200 * time.Millisecond
	t.Cleanup(func() { attestationTimeout = old })

	// The index answers and points at a bundle, so the check gets as far as
	// needing a trust root, and then blocks until it is told to stop.
	index := &attestationServer{snappyBody: snappyFramed(t, []byte("not really a bundle"))}
	ts := index.start(t)

	v := &AttestationVerifier{
		APIBaseURL: ts.URL,
		TrustedMaterial: func(ctx context.Context) (root.TrustedMaterial, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	start := time.Now()
	got := v.Check(context.Background(), artifact, version)
	elapsed := time.Since(start)

	if got.State != AttestationUnreachable {
		t.Errorf("a stalled trust-root fetch reported %s, want unreachable", got.State)
	}
	if elapsed > 10*time.Second {
		t.Errorf("the trust-root fetch took %s, so it was bounded by the run budget rather than attestationTimeout", elapsed)
	}
}

// TestAttestationVerifierCheckReportsADeadBundleAsUnreachable is the policy
// statement of the same fault: an attestation the index listed and the store
// could not serve is "could not check", never "this release has no
// attestation". The two states are the difference between a fact about the
// release and a fact about the moment, and only the first is something a user
// can permanently do anything about.
func TestAttestationVerifierCheckReportsADeadBundleAsUnreachable(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)

	index := &attestationServer{}
	index.setBlobStatus(http.StatusNotFound)
	ts := index.start(t)

	v := &AttestationVerifier{
		APIBaseURL:      ts.URL,
		TrustedMaterial: staticTrustMaterial(fx.trustRoot),
	}
	got := v.Check(context.Background(), artifact, version)
	if got.State != AttestationUnreachable {
		t.Errorf("a dead bundle blob reported %s, want unreachable: %s", got.State, got.Detail)
	}
	if strings.Contains(got.Detail, "no attestation is published") {
		t.Errorf("the detail a user reads reports an outage as a release with no attestation: %s", got.Detail)
	}
}

// staticTrustMaterial is a trust root that is already in hand, for a test whose
// subject is something other than fetching one.
func staticTrustMaterial(tr root.TrustedMaterial) func(context.Context) (root.TrustedMaterial, error) {
	return func(context.Context) (root.TrustedMaterial, error) { return tr, nil }
}

// TestFetchAttestationBundlesRefusesOversizedResponses keeps the service from
// deciding how much memory an upgrade spends. A bundle is a few KiB; the caps
// here are generous, and both the attestations response and the snappy body
// are bounded, because the second is a URL the response pointed at and would
// otherwise be a fetch whose size nothing in this package chose.
func TestFetchAttestationBundlesRefusesOversizedResponses(t *testing.T) {
	const digest = "0000000000000000000000000000000000000000000000000000000000000000"
	oldIndex, oldBundle := attestationResponseCap, attestationBundleCap
	t.Cleanup(func() { attestationResponseCap, attestationBundleCap = oldIndex, oldBundle })

	t.Run("the attestations response", func(t *testing.T) {
		attestationResponseCap = 512
		mux := http.NewServeMux()
		mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"attestations":[{"repository_id":1,"initiator":"github","bundle":{"padding":"`+strings.Repeat("A", 4096)+`"},"bundle_url":null}]}`)
		})
		ts := httptest.NewServer(mux)
		t.Cleanup(ts.Close)
		if _, err := FetchAttestationBundles(context.Background(), ts.URL, digest); err == nil {
			t.Fatal("an oversized attestations response was accepted")
		}
	})

	t.Run("the snappy bundle body", func(t *testing.T) {
		attestationBundleCap = 256
		srv := &attestationServer{snappyBody: snappyFramed(t, []byte(strings.Repeat("B", 8192)))}
		ts := srv.start(t)
		if _, err := FetchAttestationBundles(context.Background(), ts.URL, digest); err == nil {
			t.Fatal("an oversized bundle body was accepted")
		}
	})

	// The bare block encoding is a second path with its own bound, and the bound
	// is on the CLAIM rather than the outcome. The body below compresses to a
	// few hundred bytes and expands to 8 KiB, and the cap sits between the two.
	t.Run("a bare snappy block that expands past the cap", func(t *testing.T) {
		encoded := snappy.Encode(nil, bytes.Repeat([]byte("C"), 8192))
		attestationBundleCap = 1024
		srv := &attestationServer{snappyBody: encoded}
		ts := srv.start(t)
		if int64(len(encoded)) > attestationBundleCap {
			t.Fatalf("the compressed body is %d bytes, over the %d-byte cap; this case is meant to be refused for expanding, not for arriving large", len(encoded), attestationBundleCap)
		}
		if _, err := FetchAttestationBundles(context.Background(), ts.URL, digest); err == nil {
			t.Fatal("a bundle that expands past the cap was accepted")
		}
	})

	// The bare block format leads with a varint declaring the DECOMPRESSED
	// length, and snappy.Decode allocates a destination of that size BEFORE it
	// discovers the source is too short to be that long. So a body of a handful
	// of bytes claiming four gibibytes makes the decoder attempt a four-
	// gibibyte allocation, and a cap applied to the RESULT bounds nothing.
	//
	// This is the one place in the upgrade path where a remote service chooses
	// both what a client decompresses and what that costs, so the test has to
	// show the claim is refused rather than merely that a big body is: the
	// difference is a few bytes on the wire against gigabytes of allocation.
	t.Run("a bare block claiming far more than the cap", func(t *testing.T) {
		old := attestationBundleCap
		attestationBundleCap = 8 << 20
		t.Cleanup(func() { attestationBundleCap = old })

		// varint(4 GiB) followed by nothing: the smallest body that claims the
		// largest allocation a 64-bit host would make from it.
		var body []byte
		var claim [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(claim[:], 4<<30)
		body = append(body, claim[:n]...)

		// A cap of 8 MiB and a claim of 4 GiB: the check has to read the varint.
		start := time.Now()
		_, err := decodeSnappyBundle(body)
		elapsed := time.Since(start)

		if err == nil {
			t.Fatal("a block claiming 4 GiB of output was accepted")
		}
		if !strings.Contains(err.Error(), "declares") {
			t.Errorf("error %q does not report the claimed size, so a reader cannot tell a refused claim from a decode failure", err)
		}
		if !strings.Contains(err.Error(), "4294967296") {
			t.Errorf("error %q does not name the claimed length, so the bound it applied is not visible", err)
		}
		// The allocation the bug would have made is ~4 GiB; a cap that ran
		// after it could not complete inside a second on most hosts.
		if elapsed > 5*time.Second {
			t.Errorf("refusing a 4 GiB claim took %s, so the size was checked after the decoder had already allocated for it", elapsed)
		}
	})

	// A body that is neither encoding has to be reported as such, naming both
	// attempts, because which one the store switched to is the first thing
	// anyone debugging this needs to know. Note what this does NOT pin: the
	// specific wording for a body whose leading varint is unreadable. The
	// decoder's own error is an equally good sentence there, so a test
	// demanding the exact text would be pinning a distinction nothing depends
	// on — the guard there is about not handing a nonsense claim to the
	// decoder, not about diagnosing it.
	t.Run("a body that is neither encoding", func(t *testing.T) {
		// Too short to hold a varint, and not a framed stream: the framing
		// format opens with a chunk type byte, so this is neither.
		_, err := decodeSnappyBundle([]byte{0x07, 0x41})
		if err == nil {
			t.Fatal("a body that is not snappy at all was decoded")
		}
		if !strings.Contains(err.Error(), "as a framed stream") || !strings.Contains(err.Error(), "as a block") {
			t.Errorf("refusal %q does not name both encodings it tried, so a reader cannot tell which one the store switched to", err)
		}
	})
}

// TestFetchAttestationBundlesSkipsTheBlobFetchForAnInlineBundle keeps a
// response that carries the bundle inline from also fetching bundle_url. The
// URL is a presigned blob link; fetching it when the bundle is already in hand
// spends a request and a signed URL on nothing.
func TestFetchAttestationBundlesSkipsTheBlobFetchForAnInlineBundle(t *testing.T) {
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	san, issuer := ReleaseWorkflowIdentity("0.43.0")
	bundle := fx.bundleJSON(t, san, issuer, artifact)

	var blobHits int
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"attestations": []any{map[string]any{
			"repository_id": 1, "initiator": "github",
			"bundle": json.RawMessage(bundle),
			// Present but unused: a fetch here would 404 in this fixture and
			// fail a test that is only about the inline path.
			"bundle_url": "http://127.0.0.1:1/never-fetched",
		}}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { blobHits++ })
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	sum := sha256.Sum256(artifact)
	if _, err := FetchAttestationBundles(context.Background(), ts.URL, hexDigest(sum[:])); err != nil {
		t.Fatalf("FetchAttestationBundles: %v", err)
	}
	if blobHits != 0 {
		t.Fatalf("fetched bundle_url %d times when the bundle was inline", blobHits)
	}
}

func hexDigest(sum []byte) string { return hex.EncodeToString(sum) }
