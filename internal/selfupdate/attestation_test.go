package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
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
func (f *attestationFixture) bundleJSON(t *testing.T, identity, issuer string, artifact []byte) []byte {
	t.Helper()
	sum := sha256.Sum256(artifact)
	statement := fmt.Sprintf(
		`{"_type":"https://in-toto.io/Statement/v1","subject":[{"name":"ghost.tar.gz","digest":{"sha256":"%s"}}],`+
			`"predicateType":"https://slsa.dev/provenance/v1","predicate":{}}`,
		hex.EncodeToString(sum[:]))

	te, err := f.vs.AttestAtTime(identity, issuer, []byte(statement), time.Now().Add(5*time.Minute), false)
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
	inline     json.RawMessage
	snappyBody []byte
	// served records the digests this server was asked about, so a test can
	// assert the lookup used the bytes the client downloaded.
	served []string
	// status and body override the 200 for a failure case.
	status int
	body   string
}

func (s *attestationServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	blob := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/x-snappy")
		_, _ = w.Write(s.snappyBody)
	}))
	t.Cleanup(blob.Close)

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/wcatz/ghost/attestations/", func(w http.ResponseWriter, r *http.Request) {
		digest := strings.TrimPrefix(r.URL.Path, "/repos/wcatz/ghost/attestations/")
		s.served = append(s.served, digest)
		if s.status != 0 {
			w.WriteHeader(s.status)
			_, _ = io.WriteString(w, s.body)
			return
		}
		member := map[string]any{
			"repository_id": 1,
			"initiator":     "github",
			"bundle_url":    blob.URL + "/bundle.sn",
			"bundle":        nil,
		}
		if s.inline != nil {
			member["bundle"] = s.inline
			member["bundle_url"] = nil
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"attestations": []any{member}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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
		if len(srv.served) != 1 || srv.served[0] != "sha256:"+hexDigest(digest[:]) {
			t.Fatalf("looked up %v, want the digest the client downloaded", srv.served)
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
// 404, an empty list, and a member with neither bundle nor bundle_url are all
// that same fact; none of them is a fetch failure, and conflating the two
// would mean a release that never had an attestation reads like an outage.
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
		_, err := FetchAttestationBundles(context.Background(), ts.URL, digest)
		if !errors.Is(err, ErrNoAttestation) {
			t.Fatalf("a member with no bundle gave %v, want ErrNoAttestation", err)
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

	// The bare block encoding is a second path with its own bound, and a bound
	// that is only on the framed path is no bound at all on the day the store
	// switches. The body below compresses to a few hundred bytes and expands to
	// 8 KiB, and the cap sits between the two: nothing about the SIZE OF THE
	// RESPONSE can refuse it, so only the check on what it expands to can.
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
