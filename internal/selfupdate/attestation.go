package selfupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
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

// The client half of the release-authenticity contract: a release that
// publishes a GitHub artifact attestation for every asset (guarded by
// release_workflow_test.go on the producer side) is one `ghost upgrade` may
// install, and a release that does not is not.
//
// What the attestation adds over the two digests already checked is
// ATTRIBUTION. VerifyAssetDigest proves the archive is the bytes GitHub holds
// for that asset, and VerifyChecksum proves the manifest agrees — but both are
// checked against material published by whoever controls the release. A digest
// answers "is this the file the release is serving"; the attestation answers
// "did this repository's release workflow, running on the release tag, sign
// this file", which is the question a substitution of the whole release cannot
// answer. The bundle carries a Fulcio certificate whose identity is the
// workflow, and Fulcio only issues such a certificate to a GitHub Actions
// OIDC token — so the chain runs back to GitHub's issuer and to the workflow
// path in this repository, not to whoever uploaded the bytes.
//
// Nothing here is hand-rolled cryptography. The bundles are parsed and the
// certificates checked by sigstore-go, and the trust roots come from the
// Sigstore TUF repository through that same library. A hand-written check of a
// certificate chain is exactly the thing that is subtly wrong in ways no test
// of this package would catch, and the whole value of the attestation is that
// it is a signature nobody in this repository made.

// FirstAttestedVersion is the first ghost release whose assets the release
// workflow attests, and therefore the first one `ghost upgrade` refuses to
// install without an attestation.
//
// It is a constant rather than a computed property because it is a decision,
// and the decision is "where does the guarantee begin", not "what does this
// release look like". Everything at or after it is refused by default when it
// has no attestation; everything before it is left alone, because a release
// published before the release workflow minted any cannot have one and
// requiring it would break every upgrade to a pinned older release.
//
// The alternative — requiring an attestation from every version, with no
// boundary — was rejected: a user pinned to an old release would have to pass
// --allow-unattested forever, and the flag that exists for "this one release
// has no attestation" would become routine. The cost of a constant is that it
// has to be raised by hand when the producer side changes, which is why
// release_workflow_test.go exists: it fails the build when the workflow stops
// attesting an asset, so this constant cannot quietly outrun its producer.
const FirstAttestedVersion = "0.43.0"

// repoSlug is the repository every release and every attestation belongs to.
// The release lookup hard-codes it (see repoAPI) and so does the identity a
// bundle has to carry, which is the point: the two cannot name different
// repositories.
const repoSlug = "wcatz/ghost"

// The certificate identity a genuine release attestation carries. Both halves
// are required, and neither alone is enough.
//
// The SAN names the workflow file AND the ref it ran from. A ref of
// refs/heads/main is what an attestation from a manual run or from a merge on
// main looks like, and accepting it would mean proving that some workflow in
// this repository ran at some point — not that the release tag was built by
// the release workflow. Pinning the tag ref is what makes the claim a claim
// about the release rather than about the repository.
//
// The issuer pins the certificate to GitHub's OIDC provider. Without it, any
// Fulcio identity that happened to name this workflow path would pass, and
// Fulcio will happily issue one to anyone who can present the corresponding
// OIDC token from any provider.
const (
	releaseWorkflowSAN    = "https://github.com/" + repoSlug + "/.github/workflows/release.yml"
	gitHubOIDCIssuer      = "https://token.actions.githubusercontent.com"
	attestationAPIBaseURL = "https://api.github.com"
)

// AttestationRequiredFor reports whether a release at this version has to carry
// an attestation before `ghost upgrade` will install it.
//
// The comparison is against the release LINE, not the tag: "0.43.0-rc.1" is
// asked about as "0.43.0", so a candidate in the first attested line is checked
// like the release it leads to. The release workflow attests whatever it is
// triggered by, and a candidate is something a user can install with
// --allow-prerelease — so leaving candidates unchecked would hand the flag a
// second, entirely unadvertised way past this check.
//
// A version this package cannot order counts as required. "dev", "nightly" and
// "1.0" cannot be compared to the cutover, and "cannot tell" must not read as
// "old enough to skip": the only cost of being wrong here is a refusal the
// --allow-unattested flag exists to handle.
func AttestationRequiredFor(version string) bool {
	cmp, err := CompareVersions(coreVersion(version), FirstAttestedVersion)
	return err != nil || cmp >= 0
}

// coreVersion returns version without its prerelease identifier, so a
// comparison can ask about the release line a tag belongs to. A version this
// package cannot parse is returned unchanged, so the caller's error is the one
// that decides.
func coreVersion(s string) string {
	v, err := parseVersion(s)
	if err != nil {
		return s
	}
	return fmt.Sprintf("%d.%d.%d", v.major, v.minor, v.patch)
}

// ReleaseWorkflowIdentity returns the certificate SAN and issuer an attestation
// for a ghost release has to carry: this repository's release workflow, run
// from the tag for that version, under GitHub's OIDC issuer.
//
// It is exported so the identity the client requires is a nameable value
// rather than a string assembled at the call site, and so a test can mint a
// bundle that matches it without copying the format.
func ReleaseWorkflowIdentity(version string) (san, issuer string) {
	return releaseWorkflowSAN + "@refs/tags/v" + strings.TrimPrefix(version, "v"), gitHubOIDCIssuer
}

// ErrNoAttestation reports that the attestation service holds no attestation
// for the bytes it was asked about.
//
// It is a distinct error, not a string to match on, because it is the ONLY
// state the loud override is meant to reach. A release that genuinely has no
// attestation is a fact about the release; a service that answered 500, a
// response that would not parse, and a trust root that could not be fetched are
// all facts about the moment, and treating them the same would let a network
// fault turn into an install with no provenance check at all.
var ErrNoAttestation = errors.New("no attestation is published for this release asset")

// The two response caps, and the deadline on the attestation lookups. They are
// vars for the same reason the transfer caps in selfupdate.go are: a test has
// to prove each one holds without pushing hundreds of MiB through a socket.
var (
	// attestationResponseCap bounds the attestations response. It is a
	// metadata request that in practice carries a few KiB, but it also
	// carries a presigned blob URL, and nothing in it should be able to
	// decide how much memory an upgrade spends.
	attestationResponseCap int64 = 4 << 20
	// attestationBundleCap bounds one fetched bundle, both compressed and
	// decompressed. A v0.3 bundle is a DSSE payload, one certificate and
	// one RFC3161 timestamp — a couple of KiB — but a build-provenance
	// predicate can carry a whole SBOM, and this bound is what keeps that
	// from being unbounded.
	attestationBundleCap int64 = 8 << 20
	// attestationTimeout bounds the whole attestation lookup, both requests.
	// It matches apiTimeout: these are small JSON requests, and an
	// interactive command has no business waiting longer for a verdict that
	// decides whether to start a transfer.
	attestationTimeout = 30 * time.Second
)

// attestationsResponse is GitHub's attestations API. The inline Bundle is
// json.RawMessage because it is either an object or null, and the difference is
// the whole point of the two fetch paths below.
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
// digestHex, a sha256 digest as hex.
//
// Both shapes the service really returns are handled. `bundle` is inline for
// some attestations and null for others — the ones a GitHub-hosted action
// produced, where the bundle was uploaded to the attestation store and GitHub
// links it — and a null one is fetched from `bundle_url` as snappy. A client
// that implemented only one of the two would report every release the release
// workflow attests as having no attestation at all.
//
// It returns ErrNoAttestation when the service holds none, and a different error
// for every way of failing to find out. The distinction is not cosmetic and it
// is not made at the call site either: ONLY the index 404 means absence. A 404
// on a `bundle_url` is a blob the store could not serve — the index said this
// release DOES have an attestation, so reporting absence there would tell a user
// their release is unattested and let --allow-unattested install an archive
// whose attestation was never checked, when the truth is that the check could
// not be completed.
//
// One member that cannot be read does not abort the lookup, either. The index
// is an index: a release legitimately has several attestations, and a member
// with a dead or expired presigned URL must not discard the bundle an earlier
// member carried in full. It becomes a reason on the error instead, which is
// what the caller sees when NO member could be read.
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
	// absenceOn404: only the index can answer the question "does this release
	// have an attestation", so only the index's 404 is an answer.
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
		// The inline bundle wins, and no request is made when it is there.
		// bundle_url is a presigned blob link, and spending one on bytes
		// already in hand would be a request and a signed URL for nothing.
		if inline := bytes.TrimSpace(entry.Bundle); len(inline) > 0 && !bytes.Equal(inline, []byte("null")) {
			bundles = append(bundles, inline)
			continue
		}
		if entry.BundleURL == "" {
			// A member with neither is a fact about the response, not a
			// failure of the fetch, and it is the one case that cannot be
			// blamed on the service being down.
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
		// Whatever else went wrong, the caller has a bundle to check and one
		// that verifies is a proof. Reporting the unreadable members here
		// would make a release with one good attestation and one dead blob link
		// look like a failure, which is the outcome a second attacker-supplied
		// bundle could aim for.
		return bundles, nil
	}
	return nil, fmt.Errorf("the attestation service lists %d attestation(s) for sha256:%s and none of them could be read: %s",
		len(parsed.Attestations), digestHex, strings.Join(unreadable, "; "))
}

// attestationGet fetches url and reads the body through readCapped, so the size
// a response may reach is decided in one place.
//
// absenceOn404 decides what a 404 means, and it is a parameter because the two
// callers are asking different questions. The index asks "does this release have
// an attestation", where 404 is the answer. A bundle blob is a presigned object
// whose lifetime has nothing to do with whether the release is attested, and a
// 404 there is a fault like any other. Every non-200 other than an index 404 is
// a failure to find out, and none of them may be confused with absence.
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

// decodeSnappyBundle inflates a bundle the attestation store served as
// application/x-snappy.
//
// Both snappy encodings are tried. The framing format is what the store
// actually sends and what snappy.NewReader reads; the bare block format is a
// one-off blob encoder's choice and snappy.Decode reads it. Accepting only one
// would mean a change in the store turns every attestation into a decode error
// — which reads as "unverifiable", i.e. a hard refusal, rather than as the
// transport hiccup it is.
//
// The cap applies to the result, because that is what reaches memory: a small
// compressed body could otherwise expand without bound, and this is the one
// place a service chooses what a client decompresses.
func decodeSnappyBundle(compressed []byte) ([]byte, error) {
	framed, framedErr := readCapped(snappy.NewReader(bytes.NewReader(compressed)), attestationBundleCap, "attestation bundle")
	if framedErr == nil {
		return framed, nil
	}
	decoded, blockErr := snappy.Decode(nil, compressed)
	if blockErr == nil {
		if int64(len(decoded)) > attestationBundleCap {
			return nil, fmt.Errorf("attestation bundle expands past the %d-byte cap", attestationBundleCap)
		}
		return decoded, nil
	}
	// Neither encoding parsed. Report both, because which one the store
	// switched to is the first thing anyone debugging this needs to know.
	return nil, fmt.Errorf("not a snappy bundle: as a framed stream: %v; as a block: %w", framedErr, blockErr)
}

// VerifyReleaseAttestation reports whether any of bundles proves that this
// repository's release workflow, running from the tag for version, signed
// artifact — the bytes the client downloaded.
//
// One bundle that verifies is enough. GitHub holds one attestation per workflow
// run, so a release legitimately has several, and a client that required all of
// them would let anyone who can add one bundle make a release uninstallable.
// Equally, one that verifies is a proof; the rest being unreadable is not a
// reason to doubt it.
//
// The error is a reason, not a decision: it says what the bundles failed to
// prove and never what to do about it, because that is the caller's to choose
// (a rejection is fatal, an absence is not) and a reason that arrives pre-
// packaged with a verdict cannot be classified at all.
//
// It does not quote what a rejected bundle claimed. A certificate identity is
// attacker-supplied text, and sigstore-go's own mismatch error prints the value
// it got — so echoing it would put a name an attacker chose into a message a
// user reads as a verdict. Each failure is classified into this package's own
// words, and the identity the client required is named instead, because that
// one is not attacker-supplied.
func VerifyReleaseAttestation(bundles [][]byte, version string, artifact []byte, trusted root.TrustedMaterial) error {
	if len(bundles) == 0 {
		return fmt.Errorf("there is no attestation bundle to check for %s", version)
	}
	if trusted == nil {
		return fmt.Errorf("there is no Sigstore trust root to check %s's attestation against", version)
	}
	sum := sha256.Sum256(artifact)
	san, issuer := ReleaseWorkflowIdentity(version)
	identity, err := verify.NewShortCertificateIdentity(issuer, "", san, "")
	if err != nil {
		return fmt.Errorf("build the release workflow identity: %w", err)
	}
	// WithObserverTimestamps(1) and not WithCurrentTime: a Fulcio leaf is
	// valid for about ten minutes, so the certificate is checked at the time
	// a trusted timestamp authority says the signature was made, not at the
	// wall clock. Every release ever attested would otherwise be expired.
	verifier, err := verify.NewVerifier(trusted, verify.WithObserverTimestamps(1))
	if err != nil {
		return fmt.Errorf("configure the attestation verifier: %w", err)
	}

	policy := verify.NewPolicy(
		verify.WithArtifactDigest("sha256", sum[:]),
		verify.WithCertificateIdentity(identity),
	)

	var reasons []string
	for _, raw := range bundles {
		var b bundle.Bundle
		if err := b.UnmarshalJSON(raw); err != nil {
			reasons = append(reasons, "one bundle is not a readable Sigstore bundle")
			continue
		}
		if _, err := verifier.Verify(&b, policy); err != nil {
			reasons = append(reasons, classifyAttestationFailure(err))
			continue
		}
		return nil
	}

	return fmt.Errorf("none of the %d attestation bundle(s) published for %s prove that this repository's release workflow signed these bytes. "+
		"Each must carry the certificate identity %s issued by %s, and must cover the archive's digest. %s",
		len(bundles), version, san, issuer, strings.Join(reasons, "; "))
}

// classifyAttestationFailure turns a verifier error into this package's own
// words, for the reason given on VerifyReleaseAttestation: the underlying error
// quotes the identity that was actually in the certificate.
//
// Only one classification is reliable, and it is the one worth making. A
// certificate-identity mismatch is a typed error sigstore-go exports, and it
// means the bundle is genuine and simply not ours — which is the difference
// between "re-sign the release" and "someone is publishing attestations for
// this repository". Everything else is reported as one thing, because the
// library reports a bad signature, a bad timestamp and a statement that covers
// different bytes as untyped strings, and matching on their text would break on
// any release of the library.
func classifyAttestationFailure(err error) string {
	var mismatch *verify.ErrNoMatchingCertificateIdentity
	if errors.As(err, &mismatch) {
		return "one bundle was signed by a workflow that is not " + repoSlug + "'s release workflow"
	}
	return "one bundle did not verify against the Sigstore trust root: its signature, its timestamp, or the archive it covers did not check out"
}

// AttestationState is what the attestation service said about a release asset.
// It is the shape the decision is made on, so the four outcomes are named
// separately rather than collapsed into an error: the difference between "this
// release has no attestation" and "nobody could be asked" is the difference
// between a fact about the release and a fact about the moment, and only the
// first is something a user can permanently do anything about.
type AttestationState int

const (
	// AttestationVerified means a bundle proved the release workflow signed
	// these bytes. It also covers a release published before the cutover,
	// which has no attestation because none could exist for it.
	AttestationVerified AttestationState = iota
	// AttestationAbsent means the service holds no attestation for the bytes.
	AttestationAbsent
	// AttestationUnverifiable means a bundle exists and did not verify. This
	// is the state no override may reach.
	AttestationUnverifiable
	// AttestationUnreachable means the service or the trust root could not be
	// consulted. The check did not happen, which is not the same as passing.
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
// it never contains a value read out of a certificate.
type AttestationResult struct {
	State  AttestationState
	Detail string
}

// AttestationVerifier checks a release asset against GitHub's attestation
// service. Its fields are the two seams: where the service is, and where the
// trust root comes from.
//
// TrustedMaterial is a function rather than a value because a TUF round trip is
// not free and most upgrades never need one. A release before the cutover, and
// a release the service holds nothing for, both stop before it.
type AttestationVerifier struct {
	// APIBaseURL is the root of the GitHub API. The attestations path is
	// built from it, so a test points this at a local server.
	APIBaseURL string
	// TrustedMaterial returns the Sigstore trust root. Production fetches it
	// from the Sigstore TUF repository; sigstore-go models it as an
	// interface, so a test substitutes a virtual CA with no seam of its own.
	TrustedMaterial func(context.Context) (root.TrustedMaterial, error)
}

// Check decides the whole question for one release asset: whether the bytes the
// client downloaded may be installed, and why not if they may not.
//
// The cutover is applied here, not by the caller, so that no call site can
// forget it. A caller that wants to know why a check was skipped reads Detail.
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

	// Only now, with a bundle in hand, is the trust root worth fetching.
	//
	// Its own deadline, and not the caller's: in production that caller is the
	// 12-minute run budget, and a stalled Sigstore TUF endpoint would otherwise
	// hold the whole command open until the budget expires and then report the
	// budget, which is both a long wait and the wrong explanation. The two
	// requests above and this one are each metadata fetches that should answer
	// in seconds; a bound that only exists for two of the three is not a bound.
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
// attestations service, and the Sigstore trust root fetched from the public TUF
// repository and cached under the ghost data directory.
//
// dataDir is a function rather than a path because the cache directory is
// resolved only once a bundle is in hand. Resolving it at wiring time would put
// the data-directory lookup — and, on a development build, the guard that
// refuses to write into a data directory this build must not touch — in the path
// of every `ghost upgrade`, including the pre-cutover ones that never fetch a
// trust root. A failure to resolve it is then reported as what it is: the check
// could not be carried out, which is a failure to find out, not a pass.
//
// The cache is not a performance detail. The trust root is what every
// certificate is checked against, so re-fetching it on every run would put a
// Sigstore outage in the path of every upgrade; a cached root is also what
// makes the check work offline, which is the state --allow-unattested exists
// for.
func LiveAttestationCheck(dataDir func() (string, error)) func(context.Context, []byte, string) AttestationResult {
	v := &AttestationVerifier{
		APIBaseURL:      attestationAPIBaseURL,
		TrustedMaterial: sigstoreTrustMaterial(dataDir),
	}
	return v.Check
}

// sigstoreTrustMaterial fetches the Sigstore trusted root through TUF, caching
// the repository metadata under <dataDir>/sigstore-tuf and reusing it for a
// week.
//
// A week is long enough that a typical upgrade makes no network request to
// Sigstore at all, and short enough that a rotated root is picked up without
// anyone having to think about it. A negative cache is deliberately not
// configured: a failed fetch is retried on the next run rather than remembered,
// because a cached failure would keep refusing upgrades long after the fault.
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
		// FetchTrustedRoot rather than NewLiveTrustedRoot: the live variant
		// starts a refresh goroutine and a 24 hour timer, and `ghost upgrade`
		// is a process that exits seconds later. One fetch, no goroutine, and
		// nothing left running to leak.
		return root.FetchTrustedRootWithOptions(opts)
	}
}
