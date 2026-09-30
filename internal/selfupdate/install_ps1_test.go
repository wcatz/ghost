package selfupdate

import (
	"encoding/json"
	"fmt"
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
// linux and macOS machine this repository's tests run on, and a developer laptop
// that has not installed it. A skip is a gap someone will close; the cost of not
// skipping is a red build for a test about a Windows script on a machine that
// cannot run it.
//
// It is NOT a gap in CI. An earlier version of this comment claimed "the workflows
// are ubuntu, and there is no Windows job", which was simply false: build-and-test
// has a `windows-plugin` job on windows-latest and windows-11-arm, both of which
// ship PowerShell 7, and it now runs these tests. The skip is for local runs
// only, and it says so rather than implying the suite is never executed.
// requirePwshEnv, when set, turns the skip below into a failure. `go test` exits 0
// when every selected test SKIPS, so a step whose only job is to run this suite
// would stay green while running nothing — which is the exact gap the CI step was
// added to close, reopening itself the day a runner image drops PowerShell 7. This
// is the same concern as the lint job's assertion on a literal `Ran [1-9][0-9]*
// test` line, and the same reason: zero tests must not read as a pass.
const requirePwshEnv = "GHOST_REQUIRE_PWSH"

func requirePwsh(t *testing.T) string {
	t.Helper()
	pwsh, err := exec.LookPath("pwsh")
	if err == nil {
		return pwsh
	}
	if os.Getenv(requirePwshEnv) != "" {
		t.Fatalf("%s is set, so a missing pwsh is a failure and not a skip: %v. "+
			"This step exists to run install.ps1's decision logic, and a green step that ran "+
			"nothing is the failure mode it was added to prevent", requirePwshEnv, err)
	}
	{
		t.Skip("pwsh is not on PATH, so install.ps1's decision logic cannot be executed here. " +
			"CI does run it: the `install.ps1 decision tests` step of the `windows-plugin` job in " +
			".github/workflows/ci.yml runs this on windows-latest and windows-11-arm, which ship " +
			"PowerShell 7. To run it locally: " +
			"`pwsh -NoProfile -File internal/selfupdate/install_ps1_attestation_cases.ps1 -Script install.ps1`.")
	}
	return ""
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
var boundaryRE = regexp.MustCompile(`(?m)^BOUNDARY (\{.*\})\r?$`)

// parseEmittedBoundary reads the script's own answers out of its output.
//
// The \r? in the pattern is not decoration. Go's `$` in a multiline expression
// does not match before the CR of a CRLF pair, so the original pattern matched on
// linux and matched nothing on Windows — which is where this suite now runs. The
// failure read "the case file emitted no BOUNDARY line, so there is nothing to
// compare", on a run whose log plainly showed the line a few lines above it. Line
// endings are a property of the platform, not of the data, so they are normalised
// away here rather than being allowed to decide whether the two implementations
// get compared at all.
//
// Two mechanisms, because the first was not enough on its own: the pattern tolerates
// a trailing CR, AND the line endings are normalised before it runs. A test row
// using a bare CR with no LF caught that — `\r?$` cannot match when `$` has no
// newline to anchor to, so the pattern alone would still miss an output that some
// other host produced. Neither mechanism is load-bearing alone.
func parseEmittedBoundary(out string) (map[string]bool, error) {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "\n")
	m := boundaryRE.FindStringSubmatch(out)
	if m == nil {
		return nil, fmt.Errorf("no BOUNDARY line in the case file's output")
	}
	var got map[string]bool
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
		return nil, fmt.Errorf("the emitted boundary is not JSON: %w", err)
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("the emitted boundary is empty, so comparing it would pass on nothing")
	}
	return got, nil
}

// TestParseEmittedBoundaryIsNotFooledByLineEndings is a pure-Go test on purpose.
// The bug it pins was Windows-only and could ONLY be found by the Windows job, so
// it is worth checking on every platform that the parser is indifferent to the
// platform's line endings — which is the whole fix.
func TestParseEmittedBoundaryIsNotFooledByLineEndings(t *testing.T) {
	const line = `BOUNDARY {"0.42.9":false,"0.43.0":true}`
	for _, tc := range []struct {
		name  string
		out   string
		wantN int
	}{
		{"unix", line + "\nall 176 checks passed\n", 2},
		{"windows", line + "\r\nall 176 checks passed\r\n", 2},
		{"no trailing newline", line, 2},
		{"cr only", line + "\rall 1 checks passed", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEmittedBoundary(tc.out)
			if err != nil {
				t.Fatalf("parseEmittedBoundary: %v", err)
			}
			if len(got) != tc.wantN {
				t.Fatalf("parsed %d versions, want %d: %v", len(got), tc.wantN, got)
			}
			if !got["0.43.0"] {
				t.Error("0.43.0 parsed as not required; the value the JSON carries was dropped")
			}
			if got["0.42.9"] {
				t.Error("0.42.9 parsed as required; the value the JSON carries was inverted")
			}
		})
	}
	for _, tc := range []struct {
		name string
		out  string
	}{
		{"no boundary at all", "all 176 checks passed\n"},
		{"empty boundary", "BOUNDARY {}\n"},
		{"malformed json", "BOUNDARY {not json}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseEmittedBoundary(tc.out); err == nil {
				t.Error("accepted output it should have refused, so a broken run could compare as though it agreed")
			}
		})
	}
}

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
	got, err := parseEmittedBoundary(out)
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
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
