package selfupdate

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// This file holds the tests that run against a RECORDED REAL bundle in testdata,
// because the bundles this package can mint cannot see the property they check.
//
// Everything in attestation_test.go builds its fixtures with ca.VirtualSigstore,
// which mints a bundle carrying an RFC3161 timestamp and no transparency-log
// entry. That is ONE of the two shapes a real release attestation comes in, so a
// policy can be wrong about the other one and every fixture stays green. It was.
// The policy asked for an observer timestamp without asking for the log entries
// that make one, and actions/attest-build-provenance — the action ghost's own
// release workflow runs — produces a bundle with a log entry and NO RFC3161
// timestamp. From the first attested release on, that is every real bundle, and
// the policy refused all of them with AttestationUnverifiable, which no flag can
// override.
//
// The fixture is a real bundle, downloaded from a public release and committed
// here with the trust root it was verified against, so the test runs offline
// with no TUF fetch and no network at all:
//
//	recorded-log-bundle.json  actions/attest-build-provenance, on
//	  artem-sedykh/mini-climate-card v3.4.0 — 1 transparency-log entry, 0 RFC3161
//	  timestamps, and a Fulcio leaf that EXPIRED a month before this test was
//	  written.
//
// The artifact bytes are deliberately not committed. verify.WithArtifactDigest
// takes the digest the client EXPECTED and compares it against the digest the
// statement claims; the bytes are hashed by the caller before that, so pinning
// the expected digest is the same check minus 80KB of vendor JavaScript.
//
// There is only one recorded fixture, and the absence of the second shape is a
// finding rather than an omission. The attestations service also mints bundles of
// its own — cli/cli v2.101.0's is committed nowhere here — and those are signed
// by a TSA whose certificate is NOT in the public Sigstore trust root, so they
// cannot be verified against the root this client fetches through TUF and must
// never be expected to. The RFC3161 branch of the policy is therefore covered by
// a minted fixture below rather than by a recorded one; what a recorded bundle
// buys here is the real Fulcio certificate, the real Rekor entry, the real
// inclusion proof, and an EXPIRED leaf, none of which anything in this package
// can mint.

// recordedTrustRoot loads the committed trusted root, so these tests never reach
// rekorBaseURL is the transparency log this package's attestations are entered
// in, and it is what identifies the trust root to use.
//
// It is named rather than a media type because BOTH lines of the committed file
// carry the trustedroot media type, and only one of them can verify anything
// this package produces. Selecting on the media type picks whichever line comes
// first, which is a coin toss that happens to land right today and would start
// failing the day the file is regenerated in a different order — or trimmed —
// with no diff to explain it.
const rekorBaseURL = "https://rekor.sigstore.dev"

// the network. The file is the JSONL `gh attestation trusted-root` prints, and it
// holds two roots, both carrying the trustedroot media type:
//
//	line 1  public-good Sigstore — rekor.sigstore.dev, Fulcio signed by
//	        sigstore.dev, and timestamp.sigstore.dev. This is the one this client
//	        fetches through TUF, so it is the only one that can verify an
//	        attestation made by actions/attest-build-provenance.
//	line 2  GitHub's own root — fulcio.githubapp.com and timestamp.githubapp.com,
//	        with no transparency log at all. It verifies the attestations service's
//	        own bundles, which is why a bundle carrying an RFC3161 timestamp signed
//	        by that TSA cannot be verified here: a TSA absent from the trust root
//	        is not a failure of policy, it is a signature this client was never
//	        given the key to check.
//
// So the selection is by the log, because that is the property that decides
// whether a bundle can be verified at all.
//
// recordedTrustRoot loads it, so these tests never reach the network.
func recordedTrustRoot(t *testing.T) root.TrustedMaterial {
	t.Helper()
	raw, err := os.ReadFile("testdata/recorded-trusted-root.jsonl")
	if err != nil {
		t.Fatalf("read the recorded trust root: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, rekorBaseURL) {
			continue
		}
		trusted, err := root.NewTrustedRootFromJSON([]byte(line))
		if err != nil {
			t.Fatalf("NewTrustedRootFromJSON: %v", err)
		}
		return trusted
	}
	t.Fatalf("no line in testdata/recorded-trusted-root.jsonl names the transparency log %s, so the fixture no longer holds the trust root this client uses", rekorBaseURL)
	return nil
}

// TestTheRecordedTrustRootIsTheOneThisClientFetches keeps that selection honest.
// The test that uses the root would fail loudly if the wrong line were picked —
// no Fulcio chain, so nothing verifies — but it would fail as a mysterious
// signature error, on one test, and the fixture would read as broken rather than
// as mis-selected. This says which root was chosen and why.
func TestTheRecordedTrustRootIsTheOneThisClientFetches(t *testing.T) {
	raw, err := os.ReadFile("testdata/recorded-trusted-root.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	var withLog, without int
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, root.TrustedRootMediaType01) {
			continue
		}
		if strings.Contains(line, rekorBaseURL) {
			withLog++
			continue
		}
		without++
	}
	// More than one root with the log, or none, and the selection above is
	// picking whichever matched first rather than the thing that is meant.
	if withLog != 1 {
		t.Errorf("%d lines in the trust-root fixture name %s, want exactly 1", withLog, rekorBaseURL)
	}
	if without < 1 {
		t.Error("no line in the trust-root fixture is a root WITHOUT the log, so the selection is not being tested against the alternative it has to tell apart")
	}
}

// The recorded bundle, with the identity it was issued to and the digest its
// statement claims for the artifact. The identity and the digest are written out
// rather than read out of the bundle, because a test that takes the expected
// identity from the thing it is checking asserts nothing.
const (
	recordedLogBundleSAN    = "https://github.com/artem-sedykh/mini-climate-card/.github/workflows/cd.yml@refs/tags/v3.4.0"
	recordedLogBundleIssuer = "https://token.actions.githubusercontent.com"
	recordedLogBundleDigest = "ddea1607f94c7114093b75127312899d03aa01f8c66be467d5be26d907f13ac0"
	recordedLogBundleFile   = "testdata/recorded-log-bundle.json"
)

func readRecordedLogBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	raw, err := os.ReadFile(recordedLogBundleFile)
	if err != nil {
		t.Fatalf("read %s: %v", recordedLogBundleFile, err)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		t.Fatalf("the recorded bundle no longer parses: %v", err)
	}
	return &b
}

func recordedLogPolicy(t *testing.T) verify.PolicyBuilder {
	t.Helper()
	digest, err := hex.DecodeString(recordedLogBundleDigest)
	if err != nil {
		t.Fatalf("the recorded digest is not hex: %v", err)
	}
	policy, err := releaseAttestationPolicy(recordedLogBundleSAN, recordedLogBundleIssuer, digest)
	if err != nil {
		t.Fatalf("releaseAttestationPolicy: %v", err)
	}
	return policy
}

// verifyRecorded runs the recorded bundle through EXACTLY the path production
// runs: releaseAttestationPolicy builds the policy, and verifyOneBundle — the
// production function — reads the bundle's shape, builds the verifier and
// verifies. Nothing here re-derives the shape or the policy, because a helper
// that did would go on passing when the production call site changed, which is
// how a wrong shape decision survives a test suite that appears to cover it.
func verifyRecorded(t *testing.T, trusted root.TrustedMaterial, b *bundle.Bundle, san, issuer, digestHex string) error {
	t.Helper()
	digest, err := hex.DecodeString(digestHex)
	if err != nil {
		t.Fatalf("the recorded digest is not hex: %v", err)
	}
	policy, err := releaseAttestationPolicy(san, issuer, digest)
	if err != nil {
		t.Fatalf("releaseAttestationPolicy: %v", err)
	}
	return verifyOneBundle(bRaw(t, b), policy, trusted)
}

// bRaw re-marshals a parsed bundle, so the production path — which takes wire
// bytes — is the one under test rather than a shortcut that skips the parse.
func bRaw(t *testing.T, b *bundle.Bundle) []byte {
	t.Helper()
	raw, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("re-marshal the recorded bundle: %v", err)
	}
	return raw
}

// TestTheRecordedReleaseActionBundleVerifies is the test that would have caught
// the refusal this package shipped, and it is behavioural: a real bundle, a real
// trust root, a real signature, a real inclusion proof, and the production
// constructors.
func TestTheRecordedReleaseActionBundleVerifies(t *testing.T) {
	trusted := recordedTrustRoot(t)
	b := readRecordedLogBundle(t)
	if err := verifyRecorded(t, trusted, b, recordedLogBundleSAN, recordedLogBundleIssuer, recordedLogBundleDigest); err != nil {
		t.Errorf("a REAL attestation, produced by a real release workflow and served by GitHub's own attestations service, did not verify under the policy production uses. Every release carrying a bundle of this shape would be refused as AttestationUnverifiable, which no flag can override: %v", err)
	}
}

// TestTheRecordedBundleStillHasTheShapeItIsPinnedFor stops the fixture from
// quietly changing character. If a re-download produced a bundle that grew an
// RFC3161 timestamp, the test above would keep passing while no longer covering
// the branch its name claims, and the shape disagreement this file exists for
// would go back to being untested.
func TestTheRecordedBundleStillHasTheShapeItIsPinnedFor(t *testing.T) {
	vm := readRecordedLogBundle(t).GetVerificationMaterial()
	if got := len(vm.GetTlogEntries()); got != 1 {
		t.Errorf("the recorded bundle carries %d transparency-log entries, want 1: the fixture no longer covers the shape it is pinned for", got)
	}
	if got := len(vm.GetTimestampVerificationData().GetRfc3161Timestamps()); got != 0 {
		t.Errorf("the recorded bundle carries %d RFC3161 timestamps, want 0: the fixture no longer covers the shape it is pinned for, because the whole point is a bundle whose ONLY timestamp is a log entry's integrated time", got)
	}
}

// TestTheReleaseActionBundleLeavesNoPolicyThatWorksForIt pins WHY the verifier
// follows the bundle's shape, by showing that each shape refuses the other
// policy. Without these two assertions the shape argument is only a comment;
// with them, dropping the transparency-log expectation — the exact regression — is
// a failing test rather than a production outage.
func TestTheReleaseActionBundleLeavesNoPolicyThatWorksForIt(t *testing.T) {
	trusted := recordedTrustRoot(t)

	// The regression itself: the log-entry bundle with no transparency-log
	// expectation scores zero against the observer threshold, because Rekor's
	// integrated time is only counted when the log is asked about.
	logBundle := readRecordedLogBundle(t)
	withoutLog, err := verify.NewVerifier(trusted, verify.WithObserverTimestamps(attestationObserverTimestamps))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutLog.Verify(logBundle, recordedLogPolicy(t)); err == nil {
		t.Error("a bundle carrying a transparency-log entry verified WITHOUT asking for the log. So the shape needs no handling and the two policies were never mutually exclusive — which is the only reason newReleaseVerifier takes a shape argument")
	}

	// And the mirror image, so the fix is not simply "always demand a log
	// entry": that refuses a bundle whose timestamp came from a TSA. The
	// recorded bundle cannot serve here — see the note at the top of this file
	// about GitHub's own TSA not being in the public trust root — so this half
	// uses a minted bundle, whose trust root does carry the TSA that signed it.
	t.Run("a bundle whose timestamp is a TSA's is refused when a log entry is demanded", func(t *testing.T) {
		const version = "0.43.0"
		artifact := []byte("a release archive")
		fx := newAttestationFixture(t, artifact)
		san, issuer := ReleaseWorkflowIdentity(version)
		var b bundle.Bundle
		if err := b.UnmarshalJSON(fx.bundleJSON(t, san, issuer, artifact)); err != nil {
			t.Fatalf("UnmarshalJSON: %v", err)
		}
		if n := len(b.GetVerificationMaterial().GetTlogEntries()); n != 0 {
			t.Fatalf("the minted bundle carries %d log entries, so it is not the RFC3161-only shape this half needs", n)
		}
		if n := len(b.GetVerificationMaterial().GetTimestampVerificationData().GetRfc3161Timestamps()); n != 1 {
			t.Fatalf("the minted bundle carries %d RFC3161 timestamps, want 1", n)
		}
		// The one production step the recorded fixture replaces: the sha256 the
		// client computes from the bytes it downloaded, rather than 80KB of
		// vendor JavaScript committed to reproduce a hash.
		sum := sha256.Sum256(artifact)
		policy, err := releaseAttestationPolicy(san, issuer, sum[:])
		if err != nil {
			t.Fatal(err)
		}
		withLog, err := verify.NewVerifier(fx.trustRoot,
			verify.WithObserverTimestamps(attestationObserverTimestamps),
			verify.WithTransparencyLog(attestationTransparencyLogEntries))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := withLog.Verify(&b, policy); err == nil {
			t.Error("a bundle carrying an RFC3161 timestamp and no log entry verified WITH a transparency-log expectation, so the log expectation could be unconditional after all")
		}
		// The control, so the refusal above is the shape and not a broken
		// fixture: the same bundle under the policy that omits the expectation.
		ok, err := verify.NewVerifier(fx.trustRoot, verify.WithObserverTimestamps(attestationObserverTimestamps))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ok.Verify(&b, policy); err != nil {
			t.Errorf("the control failed, so the refusal above proves nothing: %v", err)
		}
	})
}

// TestTheRecordedBundleNeedsItsTimestampNotTheWallClock is the wall-clock check,
// and it is behavioural here where it could not be before.
//
// The recorded release action bundle's Fulcio leaf expired a month before this
// test was written, so a verifier that trusted the clock cannot verify it and one
// that verifies at a trusted timestamp can. That is the exact property the
// ca.VirtualSigstore fixtures cannot express, because a fixture's certificate is
// always valid now — which is why the source-level assertion that used to stand
// in for this was replaced rather than kept.
func TestTheRecordedBundleNeedsItsTimestampNotTheWallClock(t *testing.T) {
	trusted := recordedTrustRoot(t)
	b := readRecordedLogBundle(t)
	policy := recordedLogPolicy(t)

	leaf, err := x509.ParseCertificate(b.GetVerificationMaterial().GetCertificate().GetRawBytes())
	if err != nil {
		t.Fatalf("read the recorded certificate: %v", err)
	}
	if !leaf.NotAfter.Before(time.Now()) {
		t.Fatalf("the recorded certificate is valid until %s, so it is no longer the EXPIRED certificate this test exists to use: its NotAfter has to stay in the past for the wall-clock check to mean anything", leaf.NotAfter)
	}

	wallClock, err := verify.NewVerifier(trusted,
		verify.WithObserverTimestamps(attestationObserverTimestamps),
		verify.WithTransparencyLog(attestationTransparencyLogEntries),
		verify.WithCurrentTime())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wallClock.Verify(b, policy); err == nil {
		t.Error("a verifier that trusted the WALL CLOCK verified a bundle whose certificate expired a month ago, so the observer timestamp in the policy is not load-bearing and a wall-clock policy would break every real release")
	}
}

// TestAReleaseIdentityAndDigestAreStillPinned keeps the recorded bundle honest in
// the other direction: it verifies because the production policy pins the signing
// identity and the artifact digest, and a bundle that proved neither would be no
// proof at all.
func TestAReleaseIdentityAndDigestAreStillPinned(t *testing.T) {
	trusted := recordedTrustRoot(t)
	b := readRecordedLogBundle(t)

	t.Run("a different workflow's identity is refused", func(t *testing.T) {
		other := "https://github.com/someone-else/some-repo/.github/workflows/release.yml@refs/tags/v9.9.9"
		if err := verifyRecorded(t, trusted, b, other, recordedLogBundleIssuer, recordedLogBundleDigest); err == nil {
			t.Error("a bundle verified for a workflow that did not sign it")
		}
	})

	t.Run("a different issuer is refused", func(t *testing.T) {
		if err := verifyRecorded(t, trusted, b, recordedLogBundleSAN, "https://accounts.google.com", recordedLogBundleDigest); err == nil {
			t.Error("a bundle verified against an issuer that did not issue its certificate")
		}
	})

	t.Run("a different artifact digest is refused", func(t *testing.T) {
		// The sha256 of the recorded artifact, which this bundle does not
		// attest to.
		if err := verifyRecorded(t, trusted, b, recordedLogBundleSAN, recordedLogBundleIssuer, "e6c02d5b422fc856a55537c5c1278f4e5ced53e40f0efbf5e745e0707a3272f8"); err == nil {
			t.Error("a bundle verified for bytes other than the ones it attests to")
		}
	})

	t.Run("the identity and digest it does carry are accepted", func(t *testing.T) {
		if err := verifyRecorded(t, trusted, b, recordedLogBundleSAN, recordedLogBundleIssuer, recordedLogBundleDigest); err != nil {
			t.Errorf("the control failed, so the refusals above prove nothing: %v", err)
		}
	})
}

// The second recorded bundle, attested the way THIS repository's releases are:
// one statement covering many subjects.
//
// release.yml hands actions/attest-build-provenance four subject-path globs in a
// single step, so one run mints ONE bundle whose statement names every published
// asset. That is the shape the client actually meets, and it is not the shape the
// single-subject fixture above has — so a policy that matched a statement's
// subjects against a required digest could read a many-subject statement as
// ambiguous, or take the first subject and never reach the one being installed.
// Nothing here could see that with a one-subject fixture.
//
// Recorded from EhTagTranslation/EhSyringe v3.5.0: SLSA provenance v1, seven
// subjects, one transparency-log entry, no RFC3161 timestamp, and a leaf that
// expired five days before this commit.
const (
	multiBundleSAN       = "https://github.com/EhTagTranslation/EhSyringe/.github/workflows/ci.yml@refs/tags/v3.5.0"
	multiBundleIssuer    = "https://token.actions.githubusercontent.com"
	multiBundleDigest    = "e6c02d5b422fc856a55537c5c1278f4e5ced53e40f0efbf5e745e0707a3272f8"
	multiBundleFile      = "testdata/recorded-multi-subject-bundle.json"
	multiBundleSubjectNo = 7
)

func readMultiSubjectBundle(t *testing.T) *bundle.Bundle {
	t.Helper()
	raw, err := os.ReadFile(multiBundleFile)
	if err != nil {
		t.Fatalf("read %s: %v", multiBundleFile, err)
	}
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		t.Fatalf("the recorded bundle no longer parses: %v", err)
	}
	return &b
}

// TestARecordedBundleWithManySubjectsVerifiesAnyOfThem is the many-subject shape,
// through the production constructors. Every other recorded fixture has exactly
// one subject, and a client that installs one release asset out of a
// seven-asset statement is the ordinary case, not an edge one.
func TestARecordedBundleWithManySubjectsVerifiesAnyOfThem(t *testing.T) {
	trusted := recordedTrustRoot(t)
	b := readMultiSubjectBundle(t)

	subjects, err := statementSubjects(t, b)
	if err != nil {
		t.Fatalf("read the recorded statement: %v", err)
	}
	if len(subjects) != multiBundleSubjectNo {
		t.Fatalf("the recorded statement names %d subjects, want %d: the fixture no longer covers the many-subject shape it is pinned for", len(subjects), multiBundleSubjectNo)
	}

	digest, err := hex.DecodeString(multiBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := releaseAttestationPolicy(multiBundleSAN, multiBundleIssuer, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyOneBundle(bRaw(t, b), policy, trusted); err != nil {
		t.Errorf("a bundle whose statement names %d subjects refused a digest that IS one of them. This is the shape this repository's own release workflow produces — release.yml passes four subject-path globs to a single attest step — so every real upgrade would be AttestationUnverifiable: %v", len(subjects), err)
	}
}

// TestARecordedBundleRefusesADigestThatIsNotOneOfItsSubjects is the other
// direction, and it is the one that matters most: a many-subject statement names
// a great many digests, and "any of them will do" is only a safe rule because
// the rest are refused. A digest the statement does not name is bytes this
// attestation says nothing about.
func TestARecordedBundleRefusesADigestThatIsNotOneOfItsSubjects(t *testing.T) {
	trusted := recordedTrustRoot(t)
	b := readMultiSubjectBundle(t)

	digest, err := hex.DecodeString(recordedLogBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := releaseAttestationPolicy(multiBundleSAN, multiBundleIssuer, digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyOneBundle(bRaw(t, b), policy, trusted); err == nil {
		t.Error("a bundle verified for a digest its statement does not name, so \"any subject will do\" is doing the work \"this exact subject\" should be doing")
	}

	// The control, so the refusal above is about the digest and not about the
	// bundle being unverifiable for some other reason.
	good, err := hex.DecodeString(multiBundleDigest)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := releaseAttestationPolicy(multiBundleSAN, multiBundleIssuer, good)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyOneBundle(bRaw(t, b), ok, trusted); err != nil {
		t.Errorf("the control failed, so the refusal above proves nothing: %v", err)
	}
}

// statementSubjects returns the digests a bundle's DSSE payload claims. It is read
// here rather than through the verifier because the NUMBER of subjects is the
// property under test and the verifier does not report it.
func statementSubjects(t *testing.T, b *bundle.Bundle) ([]string, error) {
	t.Helper()
	raw, err := b.SignatureContent()
	if err != nil {
		return nil, err
	}
	payload, err := base64.StdEncoding.DecodeString(raw.EnvelopeContent().RawEnvelope().Payload)
	if err != nil {
		return nil, fmt.Errorf("decode the DSSE payload: %w", err)
	}
	var stmt struct {
		Subject []struct {
			Name   string            `json:"name"`
			Digest map[string]string `json:"digest"`
		} `json:"subject"`
	}
	if err := json.Unmarshal(payload, &stmt); err != nil {
		return nil, fmt.Errorf("parse the in-toto statement: %w", err)
	}
	out := make([]string, 0, len(stmt.Subject))
	for _, s := range stmt.Subject {
		out = append(out, s.Digest["sha256"])
	}
	return out, nil
}
