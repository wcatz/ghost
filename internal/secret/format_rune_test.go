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

// TestTheFormatRuneClassNamesTheInvisibleMembers: the members the class exists
// for are named one by one, because each is a character some future change
// would plausibly reach for. The exhaustive test above proves the SET matches;
// this says which members are the point.
func TestTheFormatRuneClassNamesTheInvisibleMembers(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    rune
	}{
		{"zero-width space", 0x200B},
		{"zero-width non-joiner", 0x200C},
		{"zero-width joiner", 0x200D},
		{"word joiner", 0x2060},
		{"byte-order mark", 0xFEFF},
		{"soft hyphen", 0x00AD},
		{"left-to-right mark", 0x200E},
		{"right-to-left override", 0x202E},
		{"left-to-right isolate", 0x2066},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !IsFormatRune(tc.r) {
				t.Errorf("%U is not recognised as a FORMAT character, so a detector "+
					"cannot see through it and two values that differ in bytes read alike", tc.r)
			}
		})
	}
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
		// Cf: format characters
		{"zero-width space (Cf)", "\u200b"},
		{"soft hyphen (Cf)", "\u00ad"},
		// Mn/Me: combining marks
		{"combining grapheme joiner (Mn)", "\u034f"},
		{"combining acute accent (Mn)", "\u0301"},
		// Mn: variation selectors (U+FE00..U+FE0F)
		{"variation selector 1 (Mn)", "\ufe00"},
		{"variation selector 2 (Mn)", "\ufe01"},
		{"variation selector 3 (Mn)", "\ufe02"},
		{"variation selector 4 (Mn)", "\ufe03"},
		{"variation selector 5 (Mn)", "\ufe04"},
		{"variation selector 6 (Mn)", "\ufe05"},
		{"variation selector 7 (Mn)", "\ufe06"},
		{"variation selector 8 (Mn)", "\ufe07"},
		{"variation selector 9 (Mn)", "\ufe08"},
		{"variation selector 10 (Mn)", "\ufe09"},
		{"variation selector 11 (Mn)", "\ufe0a"},
		{"variation selector 12 (Mn)", "\ufe0b"},
		{"variation selector 13 (Mn)", "\ufe0c"},
		{"variation selector 14 (Mn)", "\ufe0d"},
		{"variation selector 15 (Mn)", "\ufe0e"},
		{"variation selector 16 (Mn)", "\ufe0f"},
		// Mn: Mongolian free variation selectors (U+180B..U+180F)
		{"mongolian free variation selector 1 (Mn)", "\u180b"},
		{"mongolian free variation selector 2 (Mn)", "\u180c"},
		{"mongolian free variation selector 3 (Mn)", "\u180d"},
		{"mongolian free variation selector 4 (Mn)", "\u180e"},
		{"mongolian free variation selector 5 (Mn)", "\u180f"},
		// Mn: Khmer vowel inherent (U+17B4, U+17B5)
		{"khmer vowel inherent (Mn)", "\u17b4"},
		{"khmer vowel inherent 2 (Mn)", "\u17b5"},
		// Hangul fillers (Letter category but invisible)
		{"hangul choseong filler (L)", "\u115f"},
		{"hangul jungseong filler (L)", "\u1160"},
		{"hangul filler (L)", "\u3164"},
		{"halfwidth hangul filler (L)", "\uffa0"},
		// So: object replacement character
		{"object replacement character (So)", "\ufffc"},
		// Cc: control characters (detection-only, all of Cc except \t \n \r)
		// Named rows for four notable control characters.
		{"null (Cc)", "\x00"},
		{"backspace (Cc)", "\x08"},
		{"delete (Cc)", "\x7f"},
		{"next line (Cc)", "\u0085"},
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
	// So: braille blank
	invisibles = append(invisibles, struct {
		name string
		ch   string
	}{"braille blank (So)", "\u2800"})
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
			name:  "aws access key id",
			whole: "AKIAIOSFODNN7EXAMPLE",
			split: func(h string) string { return "AKIAIOSFODNN7" + h + "EXAMPLE" },
			rule:  "aws-access-key-id",
		},
		{
			name:  "authorization bearer token",
			whole: "Authorization: Bearer Zm9vYmFyYmF6cXV4MDEyMzQ1Njc4OTBhYmNkZWY=",
			split: func(h string) string {
				return "Authorization: Bearer Zm9v" + h + "YmFyYmF6cXV4MDEyMzQ1Njc4OTBhYmNkZWY="
			},
			rule: "authorization-bearer",
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
	cases := []struct {
		name string
		text string
	}{
		{"a word split by an invisible character", "the re" + hidden + "lay listens on 2222"},
		{"a sentence broken across a line", "rotate the deployment pass" + hidden + "word every quarter"},
		{"a short example token", "GITHUB_TOKEN=ghp" + hidden + "_example"},
		{"a prose mention of a key", "the api key for this service is sk" + hidden + "-live-123"},
		{"an accented word", "the cafe\u0301 serves coffee every morning"},
		{"a soft hyphen in prose", "soft\u00adhyphenated word in a note"},
		{"an emoji with a variation selector", "love \u2764\ufe0f you"},
		{"an arabic phrase", "the relay \u0627\u0644\u0645\u0631\u062d\u0644\u0629 listens on 2222"},
		// Prose with tabs and newlines: these are visible line structure and
		// must NOT be flagged as credentials.
		{"prose with tabs", "the relay\tlistens on 2222"},
		{"prose with newlines", "the relay\nlistens on 2222"},
		{"prose with carriage return", "the relay\rlistens on 2222"},
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
