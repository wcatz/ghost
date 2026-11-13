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
	"reflect"
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

type attestationFixture struct {
	vs        *ca.VirtualSigstore
	trustRoot *root.TrustedRoot
	artifact  []byte
}

func newAttestationFixture(t *testing.T, artifact []byte) *attestationFixture {
	t.Helper()
	vs, err := ca.NewVirtualSigstore()
	if err != nil {
		t.Fatalf("NewVirtualSigstore: %v", err)
	}
	return &attestationFixture{vs: vs, trustRoot: trustedRootFrom(t, vs), artifact: artifact}
}

func trustedRootFrom(t *testing.T, vs *ca.VirtualSigstore) *root.TrustedRoot {
	t.Helper()
	tr, err := root.NewTrustedRoot(root.TrustedRootMediaType01,
		vs.FulcioCertificateAuthorities(), vs.CTLogs(), vs.TimestampingAuthorities(), vs.RekorLogs())
	if err != nil {
		t.Fatalf("NewTrustedRoot: %v", err)
	}
	return tr
}

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

	material := &protobundle.VerificationMaterial{
		Content: &protobundle.VerificationMaterial_Certificate{
			Certificate: &protocommon.X509Certificate{RawBytes: content.Certificate().Raw},
		},
		TimestampVerificationData: &protobundle.TimestampVerificationData{Rfc3161Timestamps: rfc3161},
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
		VerificationMaterial: material,
	})
	if err != nil {
		t.Fatalf("marshal bundle: %v", err)
	}
	return wire
}

// --- The cutover boundary -------------------------------------------------

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

func TestFirstAttestedVersionIsOrderable(t *testing.T) {
	if !IsRelease(FirstAttestedVersion) {
		t.Fatalf("FirstAttestedVersion = %q, which is not a release version", FirstAttestedVersion)
	}
	if got := AttestationRequiredFor(FirstAttestedVersion); !got {
		t.Fatalf("FirstAttestedVersion = %q is not required by its own cutover", FirstAttestedVersion)
	}
}

// --- The identity the release workflow must prove -----------------------

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
			name: "a bundle minted for different bytes", identity: san, issuer: issuer,
			artifact: []byte("a different archive"), version: version, trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
			name: "a bundle minted for another version", identity: san, issuer: issuer,
			artifact: artifact, version: "0.44.0", trusted: func(f *attestationFixture) *root.TrustedRoot { return f.trustRoot },
		},
		{
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

func TestVerifyReleaseAttestationRefusesAnEmptyList(t *testing.T) {
	if err := VerifyReleaseAttestation(nil, "0.43.0", []byte("x"), newAttestationFixture(t, nil).trustRoot); err == nil {
		t.Fatal("VerifyReleaseAttestation accepted an empty bundle list")
	}
}

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

type attestationServer struct {
	mu         sync.Mutex
	inline     json.RawMessage
	snappyBody []byte
	served     []string
	status     int
	body       string
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

func (s *attestationServer) servedFor() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.served...)
}

func (s *attestationServer) setBlobStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blobStatus = status
}

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
		if err == nil {
			t.Fatal("a member with no bundle was accepted as a readable attestation")
		}
		if errors.Is(err, ErrNoAttestation) {
			t.Fatalf("a member with no bundle read as absence: %v", err)
		}

	})
}

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

	})
}

func TestAttestationVerifierCheckKeepsAGoodBundleWhenAnotherIsUnreadable(t *testing.T) {
	const version = "0.43.0"
	artifact := []byte("a release archive")
	fx := newAttestationFixture(t, artifact)
	san, issuer := ReleaseWorkflowIdentity(version)
	good := fx.bundleJSON(t, san, issuer, artifact)

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/wcatz/ghost/attestations/") {
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

}

func staticTrustMaterial(tr root.TrustedMaterial) func(context.Context) (root.TrustedMaterial, error) {
	return func(context.Context) (root.TrustedMaterial, error) { return tr, nil }
}

func TestFetchAttestationBundlesRefusesOversizedResponses(t *testing.T) {
	const digest = "0000000000000000000000000000000000000000000000000000000000000000"
	oldIndex, oldBundle := attestationResponseCap, attestationBundleCap
	t.Cleanup(func() { attestationResponseCap, attestationBundleCap = oldIndex, oldBundle })

	t.Run("the snappy bundle body", func(t *testing.T) {
		attestationBundleCap = 256
		srv := &attestationServer{snappyBody: snappyFramed(t, []byte(strings.Repeat("B", 8192)))}
		ts := srv.start(t)
		if _, err := FetchAttestationBundles(context.Background(), ts.URL, digest); err == nil {
			t.Fatal("an oversized bundle body was accepted")
		}
	})
	t.Run("a bare block claiming far more than the cap", func(t *testing.T) {
		old := attestationBundleCap
		attestationBundleCap = 8 << 20
		t.Cleanup(func() { attestationBundleCap = old })
		var body []byte
		var claim [binary.MaxVarintLen64]byte
		n := binary.PutUvarint(claim[:], 4<<30)
		body = append(body, claim[:n]...)
		_, err := decodeSnappyBundle(body)
		if err == nil {
			t.Fatal("accepted")
		}
	})
}

func hexDigest(sum []byte) string { return hex.EncodeToString(sum) }
