package secret

import (
	"slices"
	"testing"
)

// The fixtures below are the CBOR tags the Cardano rules discriminate on, as
// byte-string header + body, so a test reads as a format rather than as a hex
// run. `58` is a CBOR byte string with a one-byte length; `59` is the same with
// a two-byte length, which is what a Plutus script needs because a script is far
// larger than 255 bytes.
const (
	// A 32-byte value: a cold/payment/stake signing key AND every Cardano
	// verification key. The two are the same bytes, which is why a bare one is
	// ambiguous and a tagged one is not.
	cardano32 = "5820" +
		"010d9f2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b" +
		"8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9"
	// A 64-byte value: an EXTENDED key, which is a 32-byte key plus a 32-byte
	// chain code. The public half of that pair — StakeExtendedVerificationKey
	// and the cc-hot extended vkeys — is published, routinely recorded, and the
	// same tag as the private half. This is why "no Cardano public key is 64
	// bytes" was wrong, and why this tag cannot be refused on its own.
	cardano64 = "5840" +
		"f2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f" +
		"9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f9a" +
		"8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"
)

// The three below are `var` rather than more `const`s because Go has no
// constant string concatenation.
var (
	// A 128-byte value. No published Cardano value has this shape, so it is safe
	// to refuse on the tag alone.
	cardano128 = "5880" + cardano64[4:]
	// The KES signing key, which is a two-byte length and therefore looks
	// exactly like the wrapper around a Plutus script until you read the length.
	kesKey = "590260" + cardano64[4:]
	// A public Plutus script: a two-byte length, and a length that is not the
	// KES one.
	plutusScript = "5901a1" + cardano64[4:]
)

// TestCardanoCBORTagsAreDiscriminatedOnShape is the Cardano half of the
// precision contract: the Cardano key file formats collide by construction (a
// signing key and a verification key are the same 32 bytes, an extended pair the
// same 64, the KES key carries the tag a Plutus script also carries), so the
// CBOR length byte and the "type" envelope are what the rule reads. A memory
// that cannot hold a published verification key cannot be about a Cardano node.
// Which rule reports a refused key is an implementation detail; what must hold
// is that it is refused.
func TestCardanoCBORTagsAreDiscriminatedOnShape(t *testing.T) {
	accepted := map[string]string{
		"extended verification key, shelley": `{"type":"StakeExtendedVerificationKeyShelley_ed25519_bip32","cborHex":"` + cardano64 + `"}`,
		"extended verification key, cc-hot":  `{"type":"cc-hot-StakeExtendedVerificationKey","cborHex":"` + cardano64 + `"}`,
		"kes verification key":               `{"type":"KESVerificationKey","v":1,"cborHex":"` + cardano32 + `"}`,
		"payment verification key":           `{"type":"PaymentVerificationKey","cborHex":"` + cardano32 + `"}`,
		"a bare 32-byte cborHex":             "the cold key cborHex " + cardano32,
		"a public plutus script":             `{"type":"PlutusScriptV1","cborHex":"` + plutusScript + `"}`,
	}
	for name, text := range accepted {
		t.Run("accept/"+name, func(t *testing.T) {
			if f, ok := Detect(text); ok {
				t.Errorf("Detect flagged rule %q on a value that must be storable", f.Rule)
			}
		})
	}

	refused := []struct {
		name, text string
		rules      []string
	}{
		{"kes signing key in a bare cborHex", `{"cborHex":"` + kesKey + `"}`, []string{"cardano-cbor-hex"}},
		{"128-byte key in a bare cborHex", `{"cborHex":"` + cardano128 + `"}`, []string{"cardano-cbor-hex"}},
		{"32-byte signing key named by its file", "the cold key is in cold.skey, cborHex " + cardano32, []string{"cardano-cbor-hex"}},
		{"64-byte signing key under a signing type", `{"type":"StakePoolSigningKey_ed25519","cborHex":"` + cardano64 + `"}`, []string{"cardano-key-file", "cardano-cbor-hex"}},
		{"64-byte key named in prose as a signing key", "the pool signing key cborHex is " + cardano64, []string{"long-hex", "cardano-cbor-hex"}},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			f, ok := Detect(tc.text)
			if !ok {
				t.Fatalf("Detect accepted a key: %q", tc.text)
			}
			if !slices.Contains(tc.rules, f.Rule) {
				t.Errorf("Detect = rule %q (%s), want one of %v", f.Rule, f.Label, tc.rules)
			}
		})
	}
}

// TestCardanoKeyFileTypeNeedsMaterial: a "type" field NAMES a key, and a
// sentence about rotating one is a sentence about a key. Only a value next to
// the name is a key.
func TestCardanoKeyFileTypeNeedsMaterial(t *testing.T) {
	for _, text := range []string{
		`we rotate the "type": "StakePoolSigningKey_ed25519" on the pool host`,
		`the KES key lives at "type": "KESKey" in the node config`,
		`"type":"EvolVerificationKey" is what the genesis file carries`,
	} {
		if f, ok := Detect(text); ok {
			t.Errorf("Detect flagged rule %q on a type name with no key material: %q", f.Rule, text)
		}
	}
	for _, text := range []string{
		`{"type":"PaymentSigningKeyShelley_ed25519","cborHex":"` + cardano32 + `"}`,
		`{"type":"KESKey","cborHex":"` + kesKey + `"}`,
		`{"type":"EvolKey","cborHex":"` + cardano32 + `"}`,
	} {
		if _, ok := Detect(text); !ok {
			t.Errorf("Detect accepted a key beside its type name: %q", text)
		}
	}
}
