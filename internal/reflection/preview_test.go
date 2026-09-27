package reflection

import (
	"strings"
	"testing"
)

// TestPreviewContentNeverEchoesACredential is the log-side half of the same
// guarantee cmd/ghost's displayProposal makes for stdout, and it is here rather
// than at the three call sites because all three share this one function.
//
// A preview is a log line, and in the autonomous path the log is an append-only
// file nothing prunes. 160 runes is far more than a GitHub PAT or a Docker Hub
// token needs. The fabrication, contamination and grounding guards all run
// INSIDE the tier — before cmd/ghost sees anything, and therefore before the
// credential drop at the write boundary — so a proposal that trips one of them
// used to have its value written to the log verbatim and never reach the
// boundary at all.
func TestPreviewContentNeverEchoesACredential(t *testing.T) {
	// Assembled rather than written out: GitHub push protection matches the PAT
	// format anywhere in a diff and rejects the push (GH013) before review.
	credential := "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"

	clean := "the relay listens on 2222 and answers ping on 443"
	if got := previewContent(clean); got != clean {
		t.Errorf("previewContent(%q) = %q, want it unchanged — a clean preview must still be readable", clean, got)
	}

	for _, text := range []string{
		"the deploy token is " + credential,
		"postgres://ghost:hunter2isnotsafe@relay-1.example/ghost",
		"password: Zq7Xn4Bt2Lm9Kc5Vr8Wd",
		"-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy\n-----END RSA PRIVATE KEY-----",
		"the cold key cborHex: 5840f2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0ff2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f",
	} {
		got := previewContent(text)
		for _, leak := range []string{"a1B2c3D4e5F6", "hunter2isnotsafe", "Zq7Xn4Bt2Lm9", "MIIBOgIB", "f2429ae1"} {
			if strings.Contains(got, leak) {
				t.Errorf("previewContent leaked %q from %q: %q", leak, text, got)
			}
		}
		if !strings.Contains(got, "withheld") {
			t.Errorf("previewContent(%q) = %q, want a withheld marker — an operator must be able to tell this was withheld rather than short", text, got)
		}
		if !strings.Contains(got, "bytes=") {
			t.Errorf("previewContent(%q) = %q, want the length kept, so an operator can tell how much was withheld", text, got)
		}
	}
}

// TestPreviewContentStillTruncatesCleanText keeps the substitution from being a
// second reason the preview is short: the fabrication guard reads these strings
// to show a human what the model produced, so a clean one is still truncated.
func TestPreviewContentStillTruncatesCleanText(t *testing.T) {
	long := strings.Repeat("the relay answers ping on port 443 for every subnet it fronts. ", 4)
	got := previewContent(long)
	if len([]rune(got)) > 161 {
		t.Errorf("previewContent returned %d runes, want a truncated preview", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("previewContent did not mark the truncation: %q", got[len(got)-10:])
	}
}
