package secret

import (
	"fmt"
	"strings"
	"testing"
)

// TestCardanoExemptionIsScopedToTheEnclosingObject pins the scope of the one
// exemption in the Cardano walk: the JSON object a value sits in, not the line
// and not the whole memory. A 64-byte value is an EXTENDED key and the published
// half of that pair is what a wallet records, so the exemption must reach a
// pretty-printed envelope (type and cborHex on separate lines) without letting a
// vkey's name excuse a signing key pasted elsewhere in the same save.
func TestCardanoExemptionIsScopedToTheEnclosingObject(t *testing.T) {
	const (
		vkeyType  = `cc-hot-StakeExtendedVerificationKey`
		skeyType  = `cc-hot-StakeExtendedSigningKey`
		prettyFmt = "{\n  \"type\": %q,\n  \"cborHex\": %q\n}"
	)

	t.Run("pretty-printed verification key is stored", func(t *testing.T) {
		body := fmt.Sprintf(prettyFmt, vkeyType, cardano64)
		if f, ok := Detect("the pool's published key:\n" + body); ok {
			t.Errorf("a published extended verification key is unstorable: rule %q", f.Rule)
		}
	})

	t.Run("pretty-printed signing key is refused", func(t *testing.T) {
		body := fmt.Sprintf(prettyFmt, skeyType, cardano64)
		if f, ok := Detect("the pool's signing key:\n" + body); !ok {
			t.Error("a pretty-printed extended SIGNING key is storable")
		} else if f.Rule != "cardano-cbor-hex" {
			t.Errorf("Detect = rule %q, want cardano-cbor-hex", f.Rule)
		}
	})

	// The second value carries a real cborHex: label, so it is exempt from
	// long-hex and the Cardano walk is the only thing that can catch it. The
	// closed-object row leaves a stray closing brace on purpose: without it the
	// forward scan rejects the value as unterminated, which would pass for the
	// wrong reason.
	for name, tail := range map[string]string{
		"a vkey envelope does not excuse a separately labelled key": "\nand the pool signing key cborHex: " + cardano64,
		"a name in an already-closed object does not exempt":        " and a stray value cborHex: " + cardano64 + "}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, ok := Detect(fmt.Sprintf(prettyFmt, vkeyType, cardano64) + tail); !ok {
				t.Error("a verification-key envelope excused a value outside it")
			}
		})
	}

	// Asserted on the predicate: with any end-to-end shape either long-hex
	// refuses first or the envelope has a key label, so Detect cannot fail when
	// the predicate is wrong.
	t.Run("a verification key name without a key label does not exempt", func(t *testing.T) {
		body := "{\n  \"note\": \"the published vkey type is " + vkeyType + "\",\n" +
			"  \"hash\": " + fmt.Sprintf("%q", cardano64) + "\n}"
		at := strings.Index(body, cardano64)
		if cardanoEnclosingObject(body, at) == "" {
			t.Fatal("the fixture has no enclosing object, so it is not testing the condition")
		}
		if cardanoEnvelopeExempts(body, at) {
			t.Error("an envelope that names a verification key but does not label the value as key material exempted it")
		}
	})
}
