package selfupdate

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// install.ps1 verifies release digests only, which is an integrity check and not
// a publisher check: checksums.txt is published in the same release as the archive
// it vouches for, so anyone able to replace the archive can replace the manifest
// with it. That was the last gap in #694, and this is the file that closes it.
//
// The script cannot reuse this package's verifier — there is no Sigstore
// verification library in Windows PowerShell, and installing one is a dependency
// nobody piping a script into `iex` has agreed to. It shells out to
// `gh attestation verify`, which is why the properties here are about DECISIONS
// rather than about cryptography: the same three questions are asked of the same
// evidence, the same answers are allowed, and the same override reaches the same
// case and no other. install_ps1_attestation_cases.ps1 exercises the decisions
// with a stubbed gh, and the tables below hold this side of the contract.

// installPS1Path is the script under test, relative to this package.
const installPS1Path = "../../install.ps1"

// requirePwsh skips rather than fails when PowerShell is absent, which is every
// machine this repository's CI runs on — the workflows are ubuntu, and there is no
// Windows job. A skip is a gap someone will close; the cost of not skipping is a
// red build on a developer laptop for a test about a script that cannot run there
// anyway. The cost of skipping silently is a script whose logic nobody has ever
// executed, which is why the skip message says so and the local run is recorded in
// the PR that introduced this.
func requirePwsh(t *testing.T) string {
	t.Helper()
	pwsh, err := exec.LookPath("pwsh")
	if err != nil {
		t.Skip("pwsh is not on PATH, so install.ps1's decision logic cannot be executed here. " +
			"This is a real gap: nothing in CI runs it. Run it locally with " +
			"`pwsh -NoProfile -File internal/selfupdate/install_ps1_attestation_cases.ps1 -Script install.ps1`.")
	}
	return pwsh
}

// runInstallPS1Cases executes the PowerShell case file and returns its stdout. A
// non-zero exit is a failure with the script's own report, because the case file
// prints which check failed and a bare exit status says nothing.
func runInstallPS1Cases(t *testing.T) string {
	t.Helper()
	pwsh := requirePwsh(t)
	script, err := filepath.Abs(installPS1Path)
	if err != nil {
		t.Fatalf("resolve %s: %v", installPS1Path, err)
	}
	if _, err := os.Stat(script); err != nil {
		t.Fatalf("the script under test is missing: %v", err)
	}

	cmd := exec.Command(pwsh, "-NoProfile", "-NonInteractive", "-File", "install_ps1_attestation_cases.ps1", "-Script", script)
	cmd.Dir = "."
	out, runErr := cmd.CombinedOutput()
	t.Logf("install.ps1 cases:\n%s", out)
	if runErr != nil {
		t.Fatalf("install.ps1's own cases failed: %v", runErr)
	}
	return string(out)
}

// TestInstallPS1AttestationDecidesTheSameWayGhostUpgradeDoes is the whole of it:
// the script's decisions, executed.
func TestInstallPS1AttestationDecidesTheSameWayGhostUpgradeDoes(t *testing.T) {
	out := runInstallPS1Cases(t)
	if !strings.Contains(out, "checks passed") {
		t.Errorf("the case file did not report a pass count, so it may not have run:\n%s", out)
	}
	if strings.Contains(out, "FAIL:") {
		t.Errorf("the case file reported failures:\n%s", out)
	}
}

// boundaryRE matches the line install_ps1_attestation_cases.ps1 emits so this side
// can hold its own boundary function to the same answers, instead of trusting a
// second table that agrees by inspection.
var boundaryRE = regexp.MustCompile(`(?m)^BOUNDARY (\{.*\})$`)

// TestTheTwoBoundariesAgree compares install.ps1's answer to
// AttestationRequiredFor's, for the same inputs.
//
// Two implementations of one rule is two things to keep in step, and this is the
// check that keeps them in step: the PowerShell side emits what it decided and
// this compares it against the Go function rather than against a table written
// twice. The rule is the interesting one — an unorderable version REQUIRES an
// attestation — and it is the row most likely to be got wrong on either side.
func TestTheTwoBoundariesAgree(t *testing.T) {
	out := runInstallPS1Cases(t)
	m := boundaryRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("the case file emitted no BOUNDARY line, so there is nothing to compare:\n%s", out)
	}
	var got map[string]bool
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
		t.Fatalf("the emitted boundary is not JSON: %v\n%s", err, m[1])
	}
	if len(got) == 0 {
		t.Fatal("the emitted boundary is empty, so the comparison would pass on nothing")
	}

	for version, want := range got {
		if have := AttestationRequiredFor(version); have != want {
			t.Errorf("install.ps1 and selfupdate disagree about whether %q needs an attestation: script says %v, Go says %v. "+
				"The two are the same rule, and a release at the boundary that one of them skips is a release nothing has vouched for", strconv.Quote(version), want, have)
		}
	}
}

// TestFirstAttestedVersionIsTheSameConstantInBoth is the other half of keeping
// one rule in one place: the boundary VALUE, not just the comparison. install.ps1
// carries its own copy because a PowerShell script cannot import a Go constant,
// and a copy that drifts would skip or demand the check on the wrong releases.
func TestFirstAttestedVersionIsTheSameConstantInBoth(t *testing.T) {
	raw, err := os.ReadFile(installPS1Path)
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	re := regexp.MustCompile(`\$script:FirstAttestedVersion\s*=\s*'([^']+)'`)
	m := re.FindSubmatch(raw)
	if m == nil {
		t.Fatal("install.ps1 declares no $script:FirstAttestedVersion, so it decides the boundary from somewhere else — or nowhere")
	}
	if got := string(m[1]); got != FirstAttestedVersion {
		t.Errorf("install.ps1 puts the first attested release at %q and selfupdate at %q. "+
			"One of them will skip the check on releases the other checks, and the gap is a release that installs with no attestation at all", got, FirstAttestedVersion)
	}
}

// TestTheReleaseWorkflowIdentityIsTheSameStringInBoth does the same for the
// identity. This one is worth more than the boundary: the identity is what the
// check is FOR, and a drifted path or a lost ref would either refuse every
// release (loud) or accept a different workflow (quiet, and the actual bug).
func TestTheReleaseWorkflowIdentityIsTheSameStringInBoth(t *testing.T) {
	raw, err := os.ReadFile(installPS1Path)
	if err != nil {
		t.Fatalf("read install.ps1: %v", err)
	}
	repo := reExec(`\$script:Repo\s*=\s*'([^']+)'`, raw)
	workflow := reExec(`\$script:ReleaseWorkflow\s*=\s*'([^']+)'`, raw)
	if repo == "" || workflow == "" {
		t.Fatalf("install.ps1 declares no repository or release workflow: repo=%q workflow=%q", repo, workflow)
	}
	if repo != repoSlug {
		t.Errorf("install.ps1 installs from %q and selfupdate from %q", repo, repoSlug)
	}

	san, issuer := ReleaseWorkflowIdentity("0.44.1")
	const want = "https://github.com/" + repoSlug + "/.github/workflows/release.yml@refs/tags/v0.44.1"
	if san != want {
		t.Errorf("ReleaseWorkflowIdentity(\"0.44.1\") = %q, and install.ps1 builds %q from its own constants", san, want)
	}
	if issuer != gitHubOIDCIssuer {
		t.Errorf("issuer %q does not match the constant the script leaves to gh's default", issuer)
	}
	// The two constants together are the SAN, and install.ps1 joins them with the
	// ref. If the Go side ever stops appending the ref, the script's identity and
	// the client's stop being the same string — which is this whole change.
	got := "https://github.com/" + repo + "/" + workflow + "@refs/tags/v0.44.1"
	if got != san {
		t.Errorf("install.ps1's identity %q is not the string selfupdate requires %q", got, san)
	}
}

func reExec(pattern string, src []byte) string {
	m := regexp.MustCompile(pattern).FindSubmatch(src)
	if m == nil {
		return ""
	}
	return string(m[1])
}
