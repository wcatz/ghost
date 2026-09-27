package secret

import (
	"fmt"
	"strings"
	"testing"
)

// TestCardanoExemptionIsScopedToTheEnclosingObject pins the scope of the one
// exemption in the Cardano walk: the JSON object a value sits in, not the line,
// and not the whole memory.
//
// The 64-byte tag is refused unless a verification key is named, because a
// 64-byte value is an EXTENDED key and the published half of that pair is what a
// wallet records. Scoping that exemption to the LINE was a fix for a real
// regression — a memory naming a pool's published vkey must not thereby excuse
// an extended signing key pasted elsewhere in the same save — and it broke
// pretty-printed envelopes, where `"type"` and `"cborHex"` are on separate
// lines. That is the common shape: a JSON document indented by anything is
// pretty-printed by definition.
//
// So the scope is the ENVELOPE, bounded by its own braces. Three cases, and the
// third is the one the line-scoped fix was for — it must survive this widening.
func TestCardanoExemptionIsScopedToTheEnclosingObject(t *testing.T) {
	const (
		vkeyType  = `cc-hot-StakeExtendedVerificationKey`
		skeyType  = `cc-hot-StakeExtendedSigningKey`
		prettyFmt = "{\n  \"type\": %q,\n  \"cborHex\": %q\n}"
	)

	t.Run("pretty-printed verification key is stored", func(t *testing.T) {
		body := fmt.Sprintf(prettyFmt, vkeyType, cardano64)
		if f, ok := Detect("the pool's published key:\n" + body); ok {
			t.Errorf("a pretty-printed published extended verification key is "+
				"unstorable, so a wallet memory cannot be saved: rule %q", f.Rule)
		}
	})

	t.Run("pretty-printed signing key is refused", func(t *testing.T) {
		body := fmt.Sprintf(prettyFmt, skeyType, cardano64)
		if f, ok := Detect("the pool's signing key:\n" + body); !ok {
			t.Error("a pretty-printed extended SIGNING key is storable — the " +
				"exemption must not read a signing key's own envelope as " +
				"publishing it")
		} else if f.Rule != "cardano-cbor-hex" {
			t.Errorf("Detect = rule %q, want cardano-cbor-hex so the finding names "+
				"the Cardano walk rather than long-hex", f.Rule)
		}
	})

	t.Run("a vkey envelope does not excuse a separately labelled key", func(t *testing.T) {
		// The property the line-scoped fix was written for, in pretty-printed
		// form: the vkey's object CLOSES before the signing value, so the value
		// is not inside the envelope that names a verification key.
		//
		// The second value carries a real cborHex: label with a colon, which
		// matters twice. Unlabelled, 132 hex characters is over longHexFloor and
		// long-hex refuses it regardless of the exemption, so the case would pass
		// either way; and a prose label like "cborHex is …" is not a label to the
		// anchored matcher, with the same effect. With a real label the run is
		// exempt from long-hex and the Cardano walk is the only thing that can
		// catch it — which is the case under test.
		body := fmt.Sprintf(prettyFmt, vkeyType, cardano64) +
			"\nand the pool signing key cborHex: " + cardano64
		if _, ok := Detect(body); !ok {
			t.Error("a published verification key's envelope excused an extended " +
				"signing key pasted after it — the two objects are separate")
		}
	})

	t.Run("a name in an already-closed object does not exempt", func(t *testing.T) {
		// A `{` above the value with a `}` between them means that object has
		// already closed, so the value is not inside it. This is the difference
		// between TWO objects in one memory and one object in one memory, and it
		// is what the backward scan's stop condition is for.
		//
		// The trailing `}` is deliberate and the comment is here so nobody
		// tidies it away: without a closing brace after the value too, the
		// forward scan rejects it for being unterminated — the right answer for
		// the wrong reason, and it makes a scan that ignores `}` entirely
		// indistinguishable from this one. The document is malformed on purpose
		// for the same reason: the property under test is a property of the scan,
		// and well-formed JSON cannot express a value that is outside every
		// object while still inside a document.
		body := fmt.Sprintf(prettyFmt, vkeyType, cardano64) +
			" and a stray value cborHex: " + cardano64 + "}"
		if _, ok := Detect(body); !ok {
			t.Error("a closed object's verification-key name exempted a value that " +
				"is not inside it — the backward scan must stop at a closing brace")
		}
	})

	t.Run("a verification key name without a key label does not exempt", func(t *testing.T) {
		// Both conditions, or the envelope is not describing this value: it has
		// to name a verification key AND label something as key material. A
		// document that mentions a vkey in a note field while holding an
		// unrelated 64-byte blob is not publishing that blob.
		//
		// Asserted on the predicate rather than end to end, and the reason is
		// worth stating: give the blob a `cborHex` label and long-hex stands
		// down, which is exactly what lets the walk decide — but then the
		// envelope HAS a key label. Give it any other label and long-hex refuses
		// it first, so Detect is green for a reason that is not this predicate.
		// Neither end-to-end shape can fail when the predicate is wrong, so the
		// predicate is where the claim is pinned.
		body := "{\n  \"note\": \"the published vkey type is " + vkeyType + "\",\n" +
			"  \"hash\": " + fmt.Sprintf("%q", cardano64) + "\n}"
		at := strings.Index(body, cardano64)
		envelope := cardanoEnclosingObject(body, at)
		if envelope == "" {
			t.Fatal("the fixture has no enclosing object, so it is not testing the " +
				"condition")
		}
		if cardanoEnvelopeExempts(body, at) {
			t.Errorf("an envelope that names a verification key but does not label "+
				"the value as key material exempted it anyway:\n%s", envelope)
		}
	})
}
