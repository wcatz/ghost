package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/snappy"
	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/tuf"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// FirstAttestedVersion is the first ghost release whose assets the release
const FirstAttestedVersion = "0.43.0"

const repoSlug = "wcatz/ghost"

const (
	releaseWorkflowSAN    = "https://github.com/" + repoSlug + "/.github/workflows/release.yml"
	gitHubOIDCIssuer      = "https://token.actions.githubusercontent.com"
	attestationAPIBaseURL = "https://api.github.com"
)

// AttestationRequiredFor reports whether a release at this version has to carry
func AttestationRequiredFor(version string) bool {
	cmp, err := CompareVersions(coreVersion(version), FirstAttestedVersion)
	return err != nil || cmp >= 0
}

func coreVersion(s string) string {
	v, err := parseVersion(s)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// ReleaseWorkflowIdentity returns the certificate SAN and issuer an attestation
func ReleaseWorkflowIdentity(version string) (san, issuer string) {
	return releaseWorkflowSAN + "@refs/tags/v" + strings.TrimPrefix(version, "v"), gitHubOIDCIssuer
}

// ErrNoAttestation reports that the attestation service holds no attestation
var ErrNoAttestation = errors.New("no attestation is published for this release asset")

var (
	attestationResponseCap int64 = 4 << 20
	attestationBundleCap   int64 = 8 << 20
	attestationTimeout           = 30 * time.Second
)

type attestationsResponse struct {
	Attestations []attestationEntry `json:"attestations"`
}

type attestationEntry struct {
	RepositoryID int64           `json:"repository_id"`
	BundleURL    string          `json:"bundle_url"`
	Initiator    string          `json:"initiator"`
	Bundle       json.RawMessage `json:"bundle"`
}

// FetchAttestationBundles returns the raw Sigstore bundles GitHub holds for
func FetchAttestationBundles(ctx context.Context, apiBaseURL, digestHex string) ([][]byte, error) {
	if len(digestHex) != sha256HexLen {
		return nil, fmt.Errorf("attestation lookup needs a %d-character sha256 digest, got %d characters", sha256HexLen, len(digestHex))
	}
	if _, err := hex.DecodeString(digestHex); err != nil {
		return nil, fmt.Errorf("malformed digest %q for the attestation lookup: %w", digestHex, err)
	}

	ctx, cancel := context.WithTimeout(ctx, attestationTimeout)
	defer cancel()

	url := strings.TrimSuffix(apiBaseURL, "/") + "/repos/" + repoSlug + "/attestations/sha256:" + digestHex
	body, err := attestationGet(ctx, url, attestationResponseCap, "attestations response", true)
	if err != nil {
		return nil, err
	}

	var parsed attestationsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode the attestations response: %w", err)
	}

	if len(parsed.Attestations) == 0 {
		return nil, fmt.Errorf("%w for sha256:%s", ErrNoAttestation, digestHex)
	}

	bundles := make([][]byte, 0, len(parsed.Attestations))
	var unreadable []string
	for i, entry := range parsed.Attestations {
		if inline := bytes.TrimSpace(entry.Bundle); len(inline) > 0 && !bytes.Equal(inline, []byte("null")) {
			bundles = append(bundles, inline)
			continue
		}
		if entry.BundleURL == "" {
			unreadable = append(unreadable, fmt.Sprintf("attestation %d carries neither an inline bundle nor a bundle_url", i))
			continue
		}
		compressed, err := attestationGet(ctx, entry.BundleURL, attestationBundleCap,
			fmt.Sprintf("the bundle for attestation %d", i), false)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("attestation %d: %v", i, err))
			continue
		}
		bundle, err := decodeSnappyBundle(compressed)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("attestation %d: %v", i, err))
			continue
		}
		bundles = append(bundles, bundle)
	}

	if len(bundles) > 0 {
		return bundles, nil
	}
	return nil, fmt.Errorf("the attestation service lists %d attestation(s) for sha256:%s and none of them could be read: %s",
		len(parsed.Attestations), digestHex, strings.Join(unreadable, "; "))
}

func attestationGet(ctx context.Context, url string, limit int64, what string, absenceOn404 bool) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch the %s: %w", what, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch {
	case absenceOn404 && resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s answered 404", ErrNoAttestation, what)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("fetch the %s: github returned %d", what, resp.StatusCode)
	}
	return readCapped(resp.Body, limit, what)
}

func decodeSnappyBundle(compressed []byte) ([]byte, error) {
	framed, framedErr := readCapped(snappy.NewReader(bytes.NewReader(compressed)), attestationBundleCap, "attestation bundle")
	if framedErr == nil {
		return framed, nil
	}
	declared, n := binary.Uvarint(compressed)
	switch {
	case n <= 0:
		return nil, fmt.Errorf("not a snappy bundle: as a framed stream: %v; as a block: the leading uncompressed-length varint is unreadable", framedErr)
	case declared > uint64(attestationBundleCap):
		return nil, fmt.Errorf("not a snappy bundle: as a framed stream: %v; as a block: it declares %d decompressed bytes, over the %d-byte cap",
			framedErr, declared, attestationBundleCap)
	}
	decoded, blockErr := snappy.Decode(make([]byte, 0, declared), compressed)
	if blockErr != nil {
		return nil, fmt.Errorf("not a snappy bundle: as a framed stream: %v; as a block: %w", framedErr, blockErr)
	}
	if int64(len(decoded)) > attestationBundleCap {
		return nil, fmt.Errorf("not a snappy bundle: as a framed stream: %v; as a block: it decoded to %d bytes, over the %d-byte cap",
			framedErr, len(decoded), attestationBundleCap)
	}
	return decoded, nil
}

const (
	attestationObserverTimestamps     = 1
	attestationTransparencyLogEntries = 1
)

func newReleaseVerifier(trusted root.TrustedMaterial, hasTransparencyLogEntries bool) (*verify.Verifier, error) {
	opts := []verify.VerifierOption{verify.WithObserverTimestamps(attestationObserverTimestamps)}
	if hasTransparencyLogEntries {
		opts = append(opts, verify.WithTransparencyLog(attestationTransparencyLogEntries))
	}
	return verify.NewVerifier(trusted, opts...)
}

func verifyOneBundle(raw []byte, policy verify.PolicyBuilder, trusted root.TrustedMaterial) error {
	var b bundle.Bundle
	if err := b.UnmarshalJSON(raw); err != nil {
		return errors.New("one bundle is not a readable Sigstore bundle")
	}
	verifier, err := newReleaseVerifier(trusted, len(b.GetVerificationMaterial().GetTlogEntries()) > 0)
	if err != nil {
		return errors.New("the attestation verifier could not be configured")
	}
	if _, err := verifier.Verify(&b, policy); err != nil {
		return errors.New(classifyAttestationFailure(err))
	}
	return nil
}

func releaseAttestationPolicy(san, issuer string, artifactDigest []byte) (verify.PolicyBuilder, error) {
	identity, err := verify.NewShortCertificateIdentity(issuer, "", san, "")
	if err != nil {
		return verify.PolicyBuilder{}, fmt.Errorf("build the release workflow identity: %w", err)
	}
	return verify.NewPolicy(
		verify.WithArtifactDigest("sha256", artifactDigest),
		verify.WithCertificateIdentity(identity),
	), nil
}

// VerifyReleaseAttestation reports whether any of bundles proves that this
func VerifyReleaseAttestation(bundles [][]byte, version string, artifact []byte, trusted root.TrustedMaterial) error {
	if len(bundles) == 0 {
		return fmt.Errorf("there is no attestation bundle to check for %s", version)
	}
	if trusted == nil {
		return fmt.Errorf("there is no Sigstore trust root to check %s's attestation against", version)
	}
	sum := sha256.Sum256(artifact)
	san, issuer := ReleaseWorkflowIdentity(version)
	policy, err := releaseAttestationPolicy(san, issuer, sum[:])
	if err != nil {
		return err
	}

	var reasons []string
	for _, raw := range bundles {
		if err := verifyOneBundle(raw, policy, trusted); err != nil {
			reasons = append(reasons, err.Error())
			continue
		}
		return nil
	}

	return fmt.Errorf("none of the %d attestation bundle(s) published for %s prove that this repository's release workflow signed these bytes. "+
		"Each must carry the certificate identity %s issued by %s, and must cover the archive's digest. %s",
		len(bundles), version, san, issuer, strings.Join(reasons, "; "))
}

func classifyAttestationFailure(err error) string {
	var mismatch *verify.ErrNoMatchingCertificateIdentity
	if errors.As(err, &mismatch) {
		return "one bundle was signed by a workflow that is not " + repoSlug + "'s release workflow"
	}
	return "one bundle did not verify against the Sigstore trust root: its signature, its timestamp, or the archive it covers did not check out"
}

// AttestationState is what the attestation service said about a release asset.
type AttestationState int

const (
	AttestationVerified AttestationState = iota
	AttestationAbsent
	AttestationUnverifiable
	AttestationUnreachable
)

func (s AttestationState) String() string {
	switch s {
	case AttestationVerified:
		return "verified"
	case AttestationAbsent:
		return "absent"
	case AttestationUnverifiable:
		return "unverifiable"
	case AttestationUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// AttestationResult is a state and the reason for it. Detail is safe to print:
type AttestationResult struct {
	State  AttestationState
	Detail string
}

// AttestationVerifier checks a release asset against GitHub's attestation
type AttestationVerifier struct {
	APIBaseURL      string
	TrustedMaterial func(context.Context) (root.TrustedMaterial, error)
}

// Check decides the whole question for one release asset: whether the bytes the
func (v *AttestationVerifier) Check(ctx context.Context, archive []byte, version string) AttestationResult {
	if !AttestationRequiredFor(version) {
		return AttestationResult{
			State:  AttestationVerified,
			Detail: fmt.Sprintf("%s is older than the first attested release (%s), so it has no attestation and none is required", version, FirstAttestedVersion),
		}
	}

	sum := sha256.Sum256(archive)
	bundles, err := FetchAttestationBundles(ctx, v.APIBaseURL, hex.EncodeToString(sum[:]))
	if err != nil {
		if errors.Is(err, ErrNoAttestation) {
			return AttestationResult{State: AttestationAbsent, Detail: err.Error()}
		}
		return AttestationResult{State: AttestationUnreachable, Detail: err.Error()}
	}

	trustCtx, cancel := context.WithTimeout(ctx, attestationTimeout)
	trusted, err := v.TrustedMaterial(trustCtx)
	cancel()
	if err != nil {
		return AttestationResult{
			State:  AttestationUnreachable,
			Detail: fmt.Sprintf("fetch the Sigstore trust root: %v", err),
		}
	}
	if err := VerifyReleaseAttestation(bundles, version, archive, trusted); err != nil {
		return AttestationResult{State: AttestationUnverifiable, Detail: err.Error()}
	}
	return AttestationResult{State: AttestationVerified}
}

// LiveAttestationCheck returns the Check a real `ghost upgrade` uses: GitHub's
func LiveAttestationCheck(dataDir func() (string, error)) func(context.Context, []byte, string) AttestationResult {
	v := &AttestationVerifier{
		APIBaseURL:      attestationAPIBaseURL,
		TrustedMaterial: sigstoreTrustMaterial(dataDir),
	}
	return v.Check
}

func sigstoreTrustMaterial(dataDir func() (string, error)) func(context.Context) (root.TrustedMaterial, error) {
	return func(ctx context.Context) (root.TrustedMaterial, error) {
		dir, err := dataDir()
		if err != nil {
			return nil, fmt.Errorf("the ghost data directory is not usable: %w", err)
		}
		opts := tuf.DefaultOptions().
			WithCacheValidity(7).
			WithCachePath(filepath.Join(dir, "sigstore-tuf")).
			WithContext(ctx)
		return root.FetchTrustedRootWithOptions(opts)
	}
}
