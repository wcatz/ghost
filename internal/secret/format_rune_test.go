package secret

import (
	"fmt"
	"testing"
	"unicode"
)

// The invisible FORMAT class, pinned EXACTLY, and the detector's rule about it.
//
// A format character carries no width of its own and prints as nothing a
// reader can see, so a value that differs from another only by one of them
// prints as the other. That is a rendering problem the escape rules already
// answer (#1649, #1651, #1663), and it is a DETECTION problem this file is
// about: the rules matched the raw bytes of a value, so a single invisible
// character placed inside a token-shaped value broke the match and the value
// was stored.
//
// The predicate that names the class is pinned exhaustively over EVERY code
// point rather than against a table of hostile examples, because the failures
// are all in the characters nobody thought of. The reference is the standard
// library's category TABLE, walked here, rather than a list written out in this
// file: a written list is the rule this replaced, because it holds only what
// somebody thought of.

// hidden is the character the class is about, written as an escape so the
// source file holds no invisible bytes of its own.
const hidden = "\u200b"

// formatCodePoints lists the members of Unicode category Cf from the standard
// library's tables, so the reference and the implementation are two different
// paths to the same list.
func formatCodePoints() map[rune]bool {
	out := map[rune]bool{}
	for _, rng := range unicode.Cf.R16 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			out[r] = true
		}
	}
	for _, rng := range unicode.Cf.R32 {
		for r := rune(rng.Lo); r <= rune(rng.Hi); r += rune(rng.Stride) {
			out[r] = true
		}
	}
	return out
}

// TestTheFormatRuneClassIsExactlyUnicodeCategoryCf: the predicate admits every
// FORMAT character and nothing else, over every code point.
func TestTheFormatRuneClassIsExactlyUnicodeCategoryCf(t *testing.T) {
	want := formatCodePoints()
	var missing, extra []rune
	for r := rune(0); r <= 0x10FFFF; r++ {
		switch got := IsFormatRune(r); {
		case want[r] && !got:
			missing = append(missing, r)
		case !want[r] && got:
			extra = append(extra, r)
		}
	}
	if len(missing) > 0 {
		t.Errorf("%d FORMAT code point(s) are not recognised — the first %d %s. "+
			"An invisible character that is not recognised is one a detector cannot "+
			"see through and a renderer prints raw, so two values that differ in "+
			"bytes read alike",
			len(missing), min(8, len(missing)), sampleFormatRunes(missing))
	}
	if len(extra) > 0 {
		t.Errorf("%d code point(s) are recognised that are not FORMAT characters — the first %d %s. "+
			"A character that is not invisible is one a reader can see, so refusing or "+
			"escaping it changes a value a reader could read",
			len(extra), min(8, len(extra)), sampleFormatRunes(extra))
	}
}

// sampleFormatRunes renders the first few code points of a set for a failure
// message.
func sampleFormatRunes(rs []rune) string {
	out := ""
	for i, r := range rs {
		if i == 8 {
			break
		}
		if i > 0 {
			out += ", "
		}
		out += string(r)
	}
	return out
}

// TestDetectSeesThroughAnInvisibleFormatCharacter is the hole this package
// had. The rules matched the raw bytes of a value, so one invisible character
// placed inside a token-shaped value broke the match: the value was reported
// as nothing, and every writer that answers from Detect stored it.
//
// The rule the fix follows is that the value the detector judges is the value a
// READER sees. A zero-width space is invisible, so a token that differs from a
// real one only by an invisible character IS that token to whoever reads it.
//
// The split value must report the SAME format as the whole one, because they
// are the same credential: a guard that reports a different format for the same
// value teaches an agent that the format name is not the thing to act on.
func TestDetectSeesThroughAnInvisibleFormatCharacter(t *testing.T) {
	// The invisible characters the detector must see through, written as
	// escapes so the source file holds no invisible bytes of its own.
	// Every listed code point is tested individually.
	invisibles := []struct {
		name string
		ch   string
	}{
		{"zero-width space (Cf)", "\u200b"},
		{"soft hyphen (Cf)", "\u00ad"},
		{"combining grapheme joiner (Mn)", "\u034f"},
		{"variation selector 1 (Mn)", "\ufe00"},
		{"variation selector 16 (Mn)", "\ufe0f"},
		{"mongolian free variation selector 1 (Mn)", "\u180b"},
		{"khmer vowel inherent (Mn)", "\u17b4"},
		{"hangul filler (L)", "\u3164"},
		{"object replacement character (So)", "\ufffc"},
		{"braille blank (So)", "\u2800"},
	}
	// Loop over every Cc code point except tab, newline, carriage return.
	// This covers the remaining 56 control characters.
	for r := rune(0); r <= 0x10FFFF; r++ {
		if unicode.Is(unicode.Cc, r) && r != '\t' && r != '\n' && r != '\r' {
			// Skip the four we already named.
			if r == 0x0000 || r == 0x0008 || r == 0x007F || r == 0x0085 {
				continue
			}
			invisibles = append(invisibles, struct {
				name string
				ch   string
			}{fmt.Sprintf("U+%04X (Cc)", r), string(r)})
		}
	}
	// Token shapes: a whole value and how to split it with one invisible
	// character. Each rule must fire on the split value exactly as it fires on
	// the whole one, because the two are the same credential to a reader.
	shapes := []struct {
		name  string
		whole string
		split func(string) string
		rule  string
	}{
		{
			name:  "anthropic api key",
			whole: "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop",
			split: func(h string) string { return "sk-ant-api03-ABCDEFGHIJ" + h + "KLMNOPQRSTUVWXYZabcdefghijklmnop" },
			rule:  "anthropic-key",
		},
		{
			name:  "github personal access token",
			whole: "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
			split: func(h string) string { return "ghp_0123456789abcdef" + h + "ghijklmnopqrstuvwxyzAB" },
			rule:  "github-pat",
		},
		{
			name:  "pem private key block",
			whole: "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----",
			split: func(h string) string {
				return "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy" + h + "1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----"
			},
			rule: "pem-private-key",
		},
	}
	for _, inv := range invisibles {
		for _, sh := range shapes {
			t.Run(inv.name+" in "+sh.name, func(t *testing.T) {
				whole, ok := Detect(sh.whole)
				if !ok {
					t.Fatalf("Detect found nothing for the whole value, want rule %q", sh.rule)
				}
				if whole.Rule != sh.rule {
					t.Fatalf("Detect rule for the whole value = %q, want %q", whole.Rule, sh.rule)
				}
				split := sh.split(inv.ch)
				got, ok := Detect(split)
				if !ok {
					t.Fatalf("Detect found nothing for the value carrying an invisible character, "+
						"want rule %q — the two values are the same credential to a reader", sh.rule)
				}
				if got.Rule != sh.rule {
					t.Errorf("Detect rule for the value carrying an invisible character = %q, want %q — "+
						"the two values are the same credential to a reader", got.Rule, sh.rule)
				}
				if got.Label == "" {
					t.Error("finding has no label — the rejection message would name no format")
				}
			})
		}
	}
}

func TestDetectKeepsRefusingWhatTheBytesRefused(t *testing.T) {
	value := "x" + hidden + "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop"
	finding, ok := Detect(value)
	if !ok {
		t.Fatal("Detect found nothing, want the Anthropic format — the bytes alone recognise it")
	}
	if finding.Rule != "anthropic-key" {
		t.Errorf("Detect rule = %q, want %q", finding.Rule, "anthropic-key")
	}
}

// TestDetectAllowsProseCarryingAnInvisibleCharacter is the false-positive half
// of the same rule: an invisible character in ordinary prose is not a
// credential, and the guard must not start refusing memories because a word
// carries one.
//
// The cases are the shapes the class is found in: a word split in two, a
// sentence broken across a line, and a value that only LOOKS like a credential
// once the invisible characters are removed.
func TestDetectAllowsProseCarryingAnInvisibleCharacter(t *testing.T) {
	cases := []struct{ name, text string }{
		{"a word split by an invisible character", "the re" + hidden + "lay listens on 2222"},
		{"a short example token", "GITHUB_TOKEN=ghp" + hidden + "_example"},
		{"an accented word", "the cafe\u0301 serves coffee every morning"},
		{"a soft hyphen in prose", "soft\u00adhyphenated word in a note"},
		{"an emoji with a variation selector", "love \u2764\ufe0f you"},
		{"prose with mixed whitespace", "the relay\t\n\rlistens on 2222"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if finding, ok := Detect(tc.text); ok {
				t.Errorf("Detect(%q) flagged rule %q (%s), want no finding",
					tc.text, finding.Rule, finding.Label)
			}
		})
	}
}
