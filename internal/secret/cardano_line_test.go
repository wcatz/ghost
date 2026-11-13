package secret

import "testing"

// TestIdentifierPathNeedsANameInIt pins the STRICT direction of the
// identifier-path exemption: isIdentifierPath runs before the length,
// character-class and entropy gates, so a human-chosen password with separators
// in it must not pass as a dotted reference. A NAME has a short lower-case
// segment (package, receiver, namespace); a random group of characters does not.
func TestIdentifierPathNeedsANameInIt(t *testing.T) {
	for v, want := range map[string]bool{
		"opts.OAuthClientSecretFromVault": true,
		"secrets.GITHUB_TOKEN":            true,
		"req.body.accessToken":            true,
		"ctx.token":                       true,
		"opts.Zq7Xn4Bt2Lm9Kc5Vr8WdQ3":     true,
		"Nq8e.Rt0y.Xk9q.Zm2r.Tv4b.Lp6w":   false,
		"a1B2.c3D4.e5F6.g7H8.i9J0.k1L2":   false,
		"Zm2r.Tv4b.Lp6w.Nq8e":             false,
		"opts":                            false,
		"opts..token":                     false,
	} {
		if got := isIdentifierPath(v); got != want {
			t.Errorf("isIdentifierPath(%q) = %v, want %v", v, got, want)
		}
	}
}

// TestTheChosenPasswordIsNotExemptThroughTheIdentifierPath is the same
// discriminator stated end to end, through Detect rather than the predicate.
func TestTheChosenPasswordIsNotExemptThroughTheIdentifierPath(t *testing.T) {
	const chosen = "Nq8e.Rt0y.Xk9q.Zm2r.Tv4b.Lp6w"
	f, ok := Detect("password: " + chosen)
	if !ok {
		t.Fatalf("Detect accepted %q — a chosen password with separators in it is not a reference", chosen)
	}
	if f.Rule != assignedSecretRule {
		t.Errorf("Detect = rule %q, want %q", f.Rule, assignedSecretRule)
	}
}
