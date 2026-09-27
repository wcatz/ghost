package secret

import "testing"

// TestPEMBlocksSurviveTheWaysTheyArrive covers the two ordinary ways a real PEM
// block is not a textbook block, both of which the line-structure rule added for
// the false positives missed.
//
// The rule requires BEGIN to end its line, END to start one, and a base64 body
// between them, because a sentence that NAMES both markers is not a key — the
// false positive it was written for is a documentation page saying "the file must
// contain the lines -----BEGIN PRIVATE KEY----- and -----END PRIVATE KEY-----".
// Two real shapes do not fit that and were not considered:
//
//   - newlines lost. A block pasted through a log line, a shell argument, a
//     single-line YAML scalar or a chat message arrives with its line structure
//     gone, and the markers end up on one line with spaces between them.
//   - the traditional encrypted format, which puts `Proc-Type:` and `DEK-Info:`
//     headers and a blank line between BEGIN and the body. `ENCRYPTED PRIVATE
//     KEY` is matched by the marker's own `[A-Z0-9 ]*`, so the marker was never
//     the problem; the headers were.
//
// Both are still required to carry a base64 body, which is what keeps the
// documentation sentence out: between its two markers there is the word "and".
func TestPEMBlocksSurviveTheWaysTheyArrive(t *testing.T) {
	const body = "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy1tPf9Cnzj4p4WGeKLs1Pt8Qu"

	refused := []struct {
		name string
		text string
	}{
		{
			name: "newlines lost, on one line",
			text: "-----BEGIN RSA PRIVATE KEY----- " + body + " -----END RSA PRIVATE KEY-----",
		},
		{
			name: "newlines lost, tabs between",
			text: "-----BEGIN RSA PRIVATE KEY-----\t" + body + "\t-----END RSA PRIVATE KEY-----",
		},
		{
			name: "traditional encrypted block with headers",
			text: "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" +
				"Proc-Type: 4,ENCRYPTED\n" +
				"DEK-Info: AES-128-CBC,1234567890ABCDEF\n" +
				"\n" + body + "\n" +
				"-----END ENCRYPTED PRIVATE KEY-----",
		},
		{
			name: "encrypted block on one line with headers",
			text: "-----BEGIN ENCRYPTED PRIVATE KEY----- Proc-Type: 4,ENCRYPTED DEK-Info: AES-128-CBC,0123456789ABCDEF " +
				body + " -----END ENCRYPTED PRIVATE KEY-----",
		},
	}
	for _, tc := range refused {
		t.Run("refuse/"+tc.name, func(t *testing.T) {
			if _, ok := Detect(tc.text); !ok {
				t.Errorf("Detect accepted a key pasted in the %s shape:\n%q", tc.name, tc.text)
			}
		})
	}

	// The false positive the line-structure rule was written for, and the reason
	// neither shape above may be loosened into matching it: there is no base64
	// body between the two markers, only prose.
	accepted := []struct {
		name string
		text string
	}{
		{
			name: "a sentence naming both markers",
			text: "the file must contain the lines -----BEGIN PRIVATE KEY----- and -----END PRIVATE KEY-----",
		},
		{
			name: "a sentence naming an encrypted block's markers",
			text: "openssl reads -----BEGIN ENCRYPTED PRIVATE KEY----- through -----END ENCRYPTED PRIVATE KEY----- and asks for the passphrase",
		},
		{
			name: "a marker pair with a short filler between them",
			text: "-----BEGIN RSA PRIVATE KEY----- and -----END RSA PRIVATE KEY-----",
		},
		{
			name: "headers with no key body",
			text: "-----BEGIN ENCRYPTED PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,0123456789ABCDEF\n\n-----END ENCRYPTED PRIVATE KEY-----",
		},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			if f, ok := Detect(tc.text); ok {
				t.Errorf("Detect flagged rule %q on %s: %q", f.Rule, tc.name, tc.text)
			}
		})
	}
}
