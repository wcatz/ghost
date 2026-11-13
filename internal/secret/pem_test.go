package secret

import "testing"

// TestPEMBlocksSurviveTheWaysTheyArrive covers the ordinary ways a real PEM
// block is not a textbook block: newlines lost (a log line, a shell argument, a
// single-line YAML scalar) and the traditional encrypted format with Proc-Type
// and DEK-Info headers. Both must still carry a base64 body, which is what keeps
// a sentence that merely NAMES both markers from being refused.
func TestPEMBlocksSurviveTheWaysTheyArrive(t *testing.T) {
	const body = "MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy1tPf9Cnzj4p4WGeKLs1Pt8Qu"
	const hdrs = "Proc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,1234567890ABCDEF\n"

	for name, text := range map[string]string{
		"newlines lost, on one line":               "-----BEGIN RSA PRIVATE KEY----- " + body + " -----END RSA PRIVATE KEY-----",
		"newlines lost, tabs between":              "-----BEGIN RSA PRIVATE KEY-----\t" + body + "\t-----END RSA PRIVATE KEY-----",
		"traditional encrypted block with headers": "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" + hdrs + "\n" + body + "\n-----END ENCRYPTED PRIVATE KEY-----",
		"indented, CRLF block":                     "  -----BEGIN RSA PRIVATE KEY-----\r\n  " + body + "\r\n  -----END RSA PRIVATE KEY-----\r\n",
	} {
		t.Run("refuse/"+name, func(t *testing.T) {
			if _, ok := Detect(text); !ok {
				t.Errorf("Detect accepted a key pasted in the %s shape:\n%q", name, text)
			}
		})
	}

	for name, text := range map[string]string{
		"a sentence naming both markers":                 "the file must contain the lines -----BEGIN PRIVATE KEY----- and -----END PRIVATE KEY-----",
		"a marker pair with a short filler between them": "-----BEGIN RSA PRIVATE KEY----- and -----END RSA PRIVATE KEY-----",
		"headers with no key body":                       "-----BEGIN ENCRYPTED PRIVATE KEY-----\n" + hdrs + "\n-----END ENCRYPTED PRIVATE KEY-----",
	} {
		t.Run("accept/"+name, func(t *testing.T) {
			if f, ok := Detect(text); ok {
				t.Errorf("Detect flagged rule %q on %s: %q", f.Rule, name, text)
			}
		})
	}
}
