package main

import (
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/secret"
)

// TestDisplayProposalNeverPrintsACredential covers every place `ghost reflect`
// echoes a proposal or a guarded drop back to the operator, in one helper.
//
// The report deliberately emits only format, category, scope and length for a
// proposal it refuses to store. That guarantee is worthless if the same command
// printed the value ninety lines earlier, and it did: the proposal listing and
// the drop-guard warning both printed `truncateForDisplay(content, 120)`, and
// 120 characters is far more than a GitHub PAT or a Docker Hub token needs. In
// the autonomous path that stdout is the append-only lifecycle.log, so the value
// would outlive the run — which is the exposure the credential refusal exists
// to prevent, arrived at from the other direction.
//
// So the detection runs at every print site rather than at the write boundary.
// A boundary-only guarantee is not a guarantee about the command's report.
func TestDisplayProposalNeverPrintsACredential(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the
	// PAT format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + rep("a1B2c3D4e5F6", 3) + "AbCd"

	clean := "the relay listens on 2222"
	if got := displayProposal(clean, "fact", 120); got != truncateForDisplay(clean, 120) {
		t.Errorf("displayProposal(%q) = %q, want the truncated content — a clean proposal must still be readable", clean, got)
	}

	got := displayProposal("the deploy token is "+credential, "fact", 120)
	if strings.Contains(got, credential) {
		t.Errorf("displayProposal printed the credential: %q", got)
	}
	if !strings.Contains(got, "withheld") || !strings.Contains(got, "GitHub personal access token") {
		t.Errorf("displayProposal did not name the format it withheld: %q", got)
	}
	// The category is still shown, so an operator can find the row.
	if !strings.Contains(got, "fact") {
		t.Errorf("displayProposal dropped the category: %q", got)
	}
	// And the length, so the operator can tell how much was withheld.
	if !strings.Contains(got, "bytes=") {
		t.Errorf("displayProposal dropped the length: %q", got)
	}
}

// TestDisplayProposalCoversEveryFlaggedShape is the corpus discipline applied
// to the report: the substitution must hold for every rule the detector has, not
// only the prefix rules a reviewer would think of.
func TestDisplayProposalCoversEveryFlaggedShape(t *testing.T) {
	shapes := []string{
		"the token is ghp_" + rep("a1B2c3D4e5F6", 3) + "AbCd",
		"postgres://ghost:hunter2isnotsafe@relay-1.example/ghost",
		"the cold key cborHex: 5840" + rep("f2429ae1", 4) + "abcd",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYL\n-----END RSA PRIVATE KEY-----",
		"password: Zq7Xn4Bt2Lm9Kc5Vr8Wd",
	}
	for _, s := range shapes {
		finding, ok := secret.Detect(s)
		if !ok {
			t.Errorf("fixture is not flagged, so it does not test the substitution: %q", s)
			continue
		}
		got := displayProposal(s, "fact", 120)
		if strings.Contains(got, "MIIBOGIB") || strings.Contains(got, "f2429ae1") || strings.Contains(got, "Zq7Xn4Bt2Lm9") ||
			strings.Contains(got, "hunter2isnotsafe") || strings.Contains(got, "a1B2c3D4e5F6") {
			t.Errorf("displayProposal leaked part of a %s value: %q", finding.Label, got)
		}
	}
}

func rep(s string, n int) string { return strings.Repeat(s, n) }
