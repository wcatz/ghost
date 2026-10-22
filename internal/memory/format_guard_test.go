package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// hiddenInvisibles are the invisible characters the detector must see
// through, written as escapes so the source file holds no invisible bytes of
// its own. Every listed code point is tested individually.
var hiddenInvisibles []struct {
	name string
	ch   string
}

func init() {
	hiddenInvisibles = []struct {
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
			hiddenInvisibles = append(hiddenInvisibles, struct {
				name string
				ch   string
			}{fmt.Sprintf("U+%04X (Cc)", r), string(r)})
		}
	}
	// So: braille blank
	hiddenInvisibles = append(hiddenInvisibles, struct {
		name string
		ch   string
	}{"braille blank (So)", "\u2800"})
}

// hiddenTokenShapes are token values and how to split them with one invisible
// character, mirroring internal/secret's own table.
var hiddenTokenShapes = []struct {
	name  string
	whole string
	split func(string) string
}{
	{
		name:  "anthropic api key",
		whole: "sk-ant-api03-ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnop",
		split: func(h string) string { return "sk-ant-api03-ABCDEFGHIJ" + h + "KLMNOPQRSTUVWXYZabcdefghijklmnop" },
	},
	{
		name:  "github personal access token",
		whole: "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
		split: func(h string) string { return "ghp_0123456789abcdef" + h + "ghijklmnopqrstuvwxyzAB" },
	},
	{
		name:  "aws access key id",
		whole: "AKIAIOSFODNN7EXAMPLE",
		split: func(h string) string { return "AKIAIOSFODNN7" + h + "EXAMPLE" },
	},
	{
		name:  "authorization bearer token",
		whole: "Authorization: Bearer Zm9vYmFyYmF6cXV4MDEyMzQ1Njc4OTBhYmNkZWY=",
		split: func(h string) string {
			return "Authorization: Bearer Zm9v" + h + "YmFyYmF6cXV4MDEyMzQ1Njc4OTBhYmNkZWY="
		},
	},
	{
		name:  "pem private key block",
		whole: "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----",
		split: func(h string) string {
			return "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy" + h + "1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----"
		},
	},
}

// TestTheWriteBoundaryRefusesATokenHiddenByAnInvisibleCharacter is the store's
// half of the rule the detector follows. A value that differs from a
// credential only by an invisible character is that credential to whoever reads
// it, so the save is refused and nothing is written — the refusal names the
// field and the format, and never the value.
func TestTheWriteBoundaryRefusesATokenHiddenByAnInvisibleCharacter(t *testing.T) {
	// One store for every case: each case asserts the store is still empty, and
	// a refused save writes nothing, so a case cannot see another's work.
	s := testStore(t)
	for _, inv := range hiddenInvisibles {
		for _, sh := range hiddenTokenShapes {
			t.Run(inv.name+" in "+sh.name, func(t *testing.T) {
				ctx := context.Background()
				content := "the staging note carries " + sh.split(inv.ch) + " and it is in the vault"

				_, _, _, err := s.Upsert(ctx, testProject, "fact", content, "mcp", 0.7, nil)
				if err == nil {
					t.Fatal("the save was accepted, want a refusal")
				}
				if !errors.Is(err, ErrSecretContent) {
					t.Errorf("errors.Is(err, ErrSecretContent) = false for %v", err)
				}
				if !strings.Contains(err.Error(), "content") {
					t.Errorf("refusal does not name the field: %v", err)
				}
				if n := projectMemoryCount(t, s); n != 0 {
					t.Errorf("a refused save stored %d memories, want 0", n)
				}
			})
		}
	}
}

// TestTheArgumentChecksRefuseATokenHiddenByAnInvisibleCharacter covers the two
// predicates a caller asks before it opens a project or writes a record.
func TestTheArgumentChecksRefuseATokenHiddenByAnInvisibleCharacter(t *testing.T) {
	for _, inv := range hiddenInvisibles {
		t.Run(inv.name, func(t *testing.T) {
			content := "the staging access key id is AKIAIOSFODNN7" + inv.ch + "EXAMPLE"
			if err := CheckUpsertArguments(content, nil, UpsertOptions{}); err == nil {
				t.Error("CheckUpsertArguments accepted the value, want a refusal")
			} else if !errors.Is(err, ErrSecretContent) {
				t.Errorf("CheckUpsertArguments refusal is not ErrSecretContent: %v", err)
			}
			record := PortableMemory{
				ID:        "0123456789abcdef0123456789abcdef",
				ProjectID: testProject,
				Category:  "fact",
				Content:   content,
				Source:    "mcp",
			}
			if err := CheckImportedMemory(record); err == nil {
				t.Error("CheckImportedMemory accepted the value, want a refusal")
			} else if !errors.Is(err, ErrSecretContent) {
				t.Errorf("CheckImportedMemory refusal is not ErrSecretContent: %v", err)
			}
		})
	}
}
