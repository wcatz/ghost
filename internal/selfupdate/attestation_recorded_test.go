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

const rekorBaseURL = "https://rekor.sigstore.dev"

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

func bRaw(t *testing.T, b *bundle.Bundle) []byte {
	t.Helper()
	raw, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("re-marshal the recorded bundle: %v", err)
	}
	return raw
}

func TestTheRecordedReleaseActionBundleVerifies(t *testing.T) {
	trusted := recordedTrustRoot(t)
	b := readRecordedLogBundle(t)
	if err := verifyRecorded(t, trusted, b, recordedLogBundleSAN, recordedLogBundleIssuer, recordedLogBundleDigest); err != nil {
		t.Errorf("a REAL attestation, produced by a real release workflow and served by GitHub's own attestations service, did not verify under the policy production uses. Every release carrying a bundle of this shape would be refused as AttestationUnverifiable, which no flag can override: %v", err)
	}
}

func TestTheReleaseActionBundleLeavesNoPolicyThatWorksForIt(t *testing.T) {
	trusted := recordedTrustRoot(t)

	logBundle := readRecordedLogBundle(t)
	withoutLog, err := verify.NewVerifier(trusted, verify.WithObserverTimestamps(attestationObserverTimestamps))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutLog.Verify(logBundle, recordedLogPolicy(t)); err == nil {
		t.Error("a bundle carrying a transparency-log entry verified WITHOUT asking for the log. So the shape needs no handling and the two policies were never mutually exclusive — which is the only reason newReleaseVerifier takes a shape argument")
	}

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
		ok, err := verify.NewVerifier(fx.trustRoot, verify.WithObserverTimestamps(attestationObserverTimestamps))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ok.Verify(&b, policy); err != nil {
			t.Errorf("the control failed, so the refusal above proves nothing: %v", err)
		}
	})
}

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
