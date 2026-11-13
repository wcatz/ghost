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

// with it. That was the last gap in #694, and this is the file that closes it.

const installPS1Path = "../../install.ps1"

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
			".github/workflows/ci.yml runs this on windows-2025-vs2026 and windows-11-arm, which ship " +
			"PowerShell 7. To run it locally: " +
			"`pwsh -NoProfile -File internal/selfupdate/install_ps1_attestation_cases.ps1 -Script install.ps1`.")
	}
	return ""
}

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

func TestInstallPS1AttestationDecidesTheSameWayGhostUpgradeDoes(t *testing.T) {
	out := runInstallPS1Cases(t)
	if !strings.Contains(out, "checks passed") {
		t.Errorf("the case file did not report a pass count, so it may not have run:\n%s", out)
	}
	if strings.Contains(out, "FAIL:") {
		t.Errorf("the case file reported failures:\n%s", out)
	}
}

var boundaryRE = regexp.MustCompile(`(?m)^BOUNDARY (\[.*\])\r?$`)

type boundaryRow struct {
	Version  string
	Required bool
}

func parseEmittedBoundary(out string) ([]boundaryRow, error) {
	out = strings.ReplaceAll(out, "\r\n", "\n")
	out = strings.ReplaceAll(out, "\r", "\n")
	m := boundaryRE.FindStringSubmatch(out)
	if m == nil {
		return nil, fmt.Errorf("no BOUNDARY line in the case file's output")
	}
	var got []boundaryRow
	if err := json.Unmarshal([]byte(m[1]), &got); err != nil {
		return nil, fmt.Errorf("the emitted boundary is not JSON: %w", err)
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("the emitted boundary is empty, so comparing it would pass on nothing")
	}
	seen := make(map[string]bool, len(got))
	for _, row := range got {
		if seen[row.Version] {
			return nil, fmt.Errorf("the emitted boundary lists %q twice; a version that appears once cannot be compared against twice", row.Version)
		}
		seen[row.Version] = true
	}
	return got, nil
}

func TestTheTwoBoundariesAgree(t *testing.T) {
	out := runInstallPS1Cases(t)
	got, err := parseEmittedBoundary(out)
	if err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}

	for _, row := range got {
		version, want := row.Version, row.Required
		if have := AttestationRequiredFor(version); have != want {
			t.Errorf("install.ps1 and selfupdate disagree about whether %q needs an attestation: script says %v, Go says %v. "+
				"The two are the same rule, and a release at the boundary that one of them skips is a release nothing has vouched for", strconv.Quote(version), want, have)
		}
	}
}

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
