package secret

import "testing"

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
// precision contract, and every row is a case where two DIFFERENT values share a
// field name and a length prefix.
//
// The negative corpus for this rule is not prose. It is the Cardano key file
// formats themselves, and they collide by construction: a signing key and a
// verification key are the same 32 bytes, an extended signing key and an
// extended verification key are the same 64 bytes, and the KES signing key
// carries the two-byte-length tag that a Plutus script also carries. The only
// things that tell them apart are the CBOR length byte and the `"type"`
// envelope, so those are what the rule reads.
//
// The rows that accept are the ones a block producer or a wallet operator must
// be able to record. A memory that cannot hold a published verification key
// cannot be a memory about a Cardano node.
func TestCardanoCBORTagsAreDiscriminatedOnShape(t *testing.T) {
	accepted := []struct {
		name string
		text string
		why  string
	}{
		{
			name: "extended verification key, shelley",
			text: `{"type":"StakeExtendedVerificationKeyShelley_ed25519_bip32","cborHex":"` + cardano64 + `"}`,
			why:  "a 32-byte key plus a 32-byte chain code, published by design",
		},
		{
			name: "extended verification key, cc-hot",
			text: `{"type":"cc-hot-StakeExtendedVerificationKey","cborHex":"` + cardano64 + `"}`,
			why:  "the same shape under a naming convention, still public",
		},
		{
			name: "kes verification key",
			text: `{"type":"KESVerificationKey","v":1,"cborHex":"` + cardano32 + `"}`,
			why:  "what cardano-node writes for a block production key",
		},
		{
			name: "payment verification key",
			text: `{"type":"PaymentVerificationKey","cborHex":"` + cardano32 + `"}`,
			why:  "published; refusing it would refuse a wallet memory",
		},
		{
			name: "a bare 32-byte cborHex with no envelope",
			text: "the cold key cborHex " + cardano32,
			why: "a 32-byte value is ambiguous with a verification key, so a bare one " +
				"is only refused when the line also names a signing key",
		},
		{
			name: "a public plutus script",
			text: `{"type":"PlutusScriptV1","cborHex":"` + plutusScript + `"}`,
			why:  "two-byte length, and not the KES one",
		},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if f, ok := Detect(tc.text); ok {
				t.Errorf("Detect flagged rule %q on a value that must be storable — %s",
					f.Rule, tc.why)
			}
		})
	}

	// Which rule reports a given key is an implementation detail — an
	// unlabelled run over the long-hex floor is caught by long-hex, and an
	// envelope carrying both a type and a value is caught by the type rule
	// first. What has to hold is that each of these is REFUSED, and that the
	// reason names a Cardano shape rather than a generic blob.
	refused := []struct {
		name  string
		text  string
		rules []string
	}{
		{
			name:  "kes signing key in a bare cborHex",
			text:  `{"cborHex":"` + kesKey + `"}`,
			rules: []string{"cardano-cbor-hex"},
		},
		{
			name:  "128-byte key in a bare cborHex",
			text:  `{"cborHex":"` + cardano128 + `"}`,
			rules: []string{"cardano-cbor-hex"},
		},
		{
			name:  "32-byte signing key named by its file",
			text:  "the cold key is in cold.skey, cborHex " + cardano32,
			rules: []string{"cardano-cbor-hex"},
		},
		{
			name:  "64-byte signing key under a signing type",
			text:  `{"type":"StakePoolSigningKey_ed25519","cborHex":"` + cardano64 + `"}`,
			rules: []string{"cardano-key-file", "cardano-cbor-hex"},
		},
		{
			name:  "64-byte key named in prose as a signing key",
			text:  "the pool signing key cborHex is " + cardano64,
			rules: []string{"long-hex", "cardano-cbor-hex"},
		},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			f, ok := Detect(tc.text)
			if !ok {
				t.Fatalf("Detect accepted a key: %q", tc.text)
			}
			hit := false
			for _, want := range tc.rules {
				if f.Rule == want {
					hit = true
				}
			}
			if !hit {
				t.Errorf("Detect = rule %q (%s), want one of %v", f.Rule, f.Label, tc.rules)
			}
		})
	}
}

// TestCardanoKeyFileTypeNeedsMaterial is the other half of the same envelope: a
// `"type"` field NAMES a key, and a sentence about rotating one is a sentence
// about a key. Only a value next to the name is a key.
//
// This was the second false positive the same review batch found, and it is the
// same error as the Heli=`secretName` cases in the other table: the identifier
// is not the thing. `"type": "StakePoolSigningKey_ed25519"` in a runbook is
// documentation, and a block producer writes that documentation constantly.
func TestCardanoKeyFileTypeNeedsMaterial(t *testing.T) {
	accepted := []string{
		`we rotate the "type": "StakePoolSigningKey_ed25519" on the pool host`,
		`the KES key lives at "type": "KESKey" in the node config`,
		`"type":"EvolVerificationKey" is what the genesis file carries`,
	}
	for _, text := range accepted {
		if f, ok := Detect(text); ok {
			t.Errorf("Detect flagged rule %q on a type name with no key material: %q", f.Rule, text)
		}
	}

	refused := []string{
		`{"type":"PaymentSigningKeyShelley_ed25519","cborHex":"` + cardano32 + `"}`,
		`{"type":"KESKey","cborHex":"` + kesKey + `"}`,
		`{"type":"EvolKey","cborHex":"` + cardano32 + `"}`,
	}
	for _, text := range refused {
		if _, ok := Detect(text); !ok {
			t.Errorf("Detect accepted a key beside its type name: %q", text)
		}
	}
}
