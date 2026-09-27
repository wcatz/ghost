package secret

import "testing"

// TestCardanoVerificationKeyExemptionIsPerLine pins the scope of the one
// exemption in the Cardano walk.
//
// The 64-byte tag is refused unless a verification key is named, because a
// 64-byte value is an EXTENDED key and the published half of that pair is what a
// wallet records. That exemption was granted by matching the WHOLE text, which
// made it a property of the memory rather than of the value: one save that
// records a pool's published vkey — a phrase a block producer writes constantly —
// also stored an extended SIGNING key pasted anywhere else in the same content.
// One memory is exactly one ghost_memory_save, so the two are in the same
// record and the exemption reached both.
//
// The 32-byte branch already tested the line; this is the same scope for the
// 64-byte one.
func TestCardanoVerificationKeyExemptionIsPerLine(t *testing.T) {
	// A published vkey on its own line, a private key on another, one memory.
	// The second value carries a REAL cborHex label, with a colon, and that is
	// load-bearing twice over. An unlabelled 132-hex run is over longHexFloor and
	// long-hex refuses it on its own, so the test would pass whichever way the
	// exemption were scoped; a label the anchored matcher does not accept — prose
	// like "cborHex is …" rather than "cborHex: …" — has the same effect. With a
	// real label the run is exempt from long-hex, and the Cardano walk is the
	// only thing left that can catch it: which is the case under test.
	mixed := `{"type":"cc-hot-StakeExtendedVerificationKey","cborHex":"` + cardano64 + `"}` +
		"\nand the pool signing key cborHex: " + cardano64
	if f, ok := Detect(mixed); !ok {
		t.Error("a memory holding a published verification key stored an extended " +
			"SIGNING key pasted on another line — the exemption has to be per line")
	} else if f.Rule != "cardano-cbor-hex" && f.Rule != "long-hex" {
		t.Errorf("Detect = rule %q, want one of the Cardano or hex rules", f.Rule)
	}

	// And the same value with the envelope on its own line is still stored,
	// because there the envelope really does describe the value beside it.
	if _, ok := Detect(`{"type":"StakeExtendedVerificationKeyShelley_ed25519_bip32","cborHex":"` + cardano64 + `"}`); ok {
		t.Error("a published extended verification key is unstorable — " +
			"the exemption is per line and the envelope is on the value's line")
	}
}

// TestIdentifierPathNeedsANameInIt pins the STRICT direction of a new exemption,
// which is the half that is easy to leave unmeasured.
//
// isIdentifierPath exempts a value shaped like a dotted reference, and it runs
// before the length, character-class and entropy gates — so without a further
// condition any dotted value of any length or randomness was accepted. A
// human-chosen password with separators in it is exactly that value:
// `Nq8e.Rt0y.Xk9q.Zm2r.Tv4b.Lp6w` is 29 characters, mixed case, over the length
// floor and well above the entropy bar, and every segment is an identifier.
//
// The discriminator is that a NAME has a short lower-case segment in it — the
// package, the receiver, the namespace — and a random group of characters does
// not.
func TestIdentifierPathNeedsANameInIt(t *testing.T) {
	accepted := []string{
		// The case the gate was written for: a vault client reference.
		"opts.OAuthClientSecretFromVault",
		"secrets.GITHUB_TOKEN",
		"req.body.accessToken",
		"ctx.token",
		// And a dotted value that also clears every other gate, so this is
		// genuinely the identifier-path gate and not one of its neighbours.
		"opts.Zq7Xn4Bt2Lm9Kc5Vr8WdQ3",
	}
	for _, v := range accepted {
		if !isIdentifierPath(v) {
			t.Errorf("isIdentifierPath(%q) = false, want true — it names a place", v)
		}
	}

	refused := []struct {
		value string
		why   string
	}{
		{
			value: "Nq8e.Rt0y.Xk9q.Zm2r.Tv4b.Lp6w",
			why:   "a chosen password with separators in it, not a name — every segment is random",
		},
		{
			value: "a1B2.c3D4.e5F6.g7H8.i9J0.k1L2",
			why:   "the same, with digits leading each group",
		},
		{
			value: "Zm2r.Tv4b.Lp6w.Nq8e",
			why:   "no segment is a short lower-case name",
		},
		{
			value: "opts",
			why:   "a single segment is not a path",
		},
		{
			value: "opts..token",
			why:   "an empty segment is not an identifier",
		},
	}
	for _, tc := range refused {
		if isIdentifierPath(tc.value) {
			t.Errorf("isIdentifierPath(%q) = true, want false — %s", tc.value, tc.why)
		}
	}
}

// TestTheChosenPasswordIsNotExemptThroughTheIdentifierPath is the same
// discriminator stated end to end, through Detect rather than through the
// predicate, because the predicate's result is only ever one input to it.
func TestTheChosenPasswordIsNotExemptThroughTheIdentifierPath(t *testing.T) {
	const chosen = "Nq8e.Rt0y.Xk9q.Zm2r.Tv4b.Lp6w"
	// Written as an ASSIGNMENT, because the generic rule only ever reads a value
	// that sits next to a `:` or `=` — a value in running prose is not its
	// concern, and pinning that here would be asserting a different rule.
	f, ok := Detect("password: " + chosen)
	if !ok {
		t.Fatalf("Detect accepted %q — a chosen password with separators in it is not a reference", chosen)
	}
	if f.Rule != assignedSecretRule {
		t.Errorf("Detect = rule %q, want %q", f.Rule, assignedSecretRule)
	}
}
