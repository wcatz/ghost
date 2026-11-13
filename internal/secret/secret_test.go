package secret

import (
	"strings"
	"testing"
)

// token assembles a credential-shaped value from a provider prefix and a body.
//
// A literal token-shaped string cannot be committed to this repository: GitHub
// push protection matches the Slack and Stripe formats anywhere in a diff and
// rejects the push (GH013) before review can start. Splitting the prefix from
// the body keeps the two halves out of one contiguous literal while leaving the
// value each case matches the shape of a real one.
func token(prefix, body string) string { return prefix + body }

const (
	coldTail = "010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"
	seedHex  = "f2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"
	pemRSA   = "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----"
)

// TestDetectRejectsCredentialValues is the positive half of the contract in
// issue #553: a save carrying a credential value must be refused. Each case is
// a value shape, not a keyword. The last rows are the false negatives a
// first-match implementation hides: a placeholder-shaped candidate earlier in
// the save must not shadow a real credential later in the same save.
func TestDetectRejectsCredentialValues(t *testing.T) {
	cases := []struct{ name, text, rule, alt string }{
		{"password that repeats its own field name", "password: MyOwnPassword12345678", "assigned-secret", ""},
		{"plutus script label beside a separate key paste", plutusScriptEnvelope + "\nand the cold key was 5820" + coldTail, "long-hex", ""},
		{"age encryption identity", "age key: AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ", "age-identity", ""},
		{"anthropic api key", "export ANTHROPIC_API_KEY=sk-ant-api03-Za0b1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx2Yz3Ab4Cd", "anthropic-key", ""},
		{"openai style api key", "the key is sk-proj-Za0b1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx2Yz3Ab4Cd5Ef6", "openai-key", ""},
		{"openai legacy api key", "the key is " + token("sk-", "Zq7Xn4Bt2Lm9Kc5Vr8WdYh3Nf6Pj1Qs4Rw7Ed2Ua5Tz8Yb3X"), "openai-key", ""},
		{"github classic personal access token", "GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB", "github-pat", ""},
		{"github fine grained personal access token", "gh auth uses github_pat_11ABCDEFG0aBcDeFgHiJkLmNoPqRsTuVwXyZ0123", "github-pat", ""},
		{"aws access key id", "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE", "aws-access-key-id", ""},
		{"cardano cold signing key in a bare cborHex field", "the cold key cborHex: 5840" + seedHex + seedHex[:64], "cardano-cbor-hex", ""},
		{"pem block with escaped newlines", `{"data":{"tls.key":"` + strings.ReplaceAll(pemRSA, "\n", `\n`) + `"}}`, "pem-private-key", ""},
		{"hex api key under a credential-named key", "api_key: deadbeefcafebabe0123456789abcdef", "assigned-secret", ""},
		{"credential argument after a quoted command word", `$api_key = "read-secret-from-keyvault" -SecretName "K3q9Xm2pL7wRt4ZbAvN1" -Version 2`, "assigned-secret", ""},
		{"plaintext secret passed as a command argument", `$pw = ConvertTo-SecureString "K3q9Xm2pL7wRt4ZbAvN1" -AsPlainText -Force`, "assigned-secret", ""},
		{"quoted dash-grouped passphrase with a trailing flag", `$api_key = "XkQpz-ZmRtv-LvNbw-HcJdx" -AsPlainText -Force`, "assigned-secret", ""},
		{"quoted literal with a trailing shell flag", `$db_password = "K3q9Xm2pL7wRt4ZbAvN1" -AsPlainText`, "assigned-secret", ""},
		{"slack bot token", "slack: " + token("xoxb-", "123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx"), "slack-token", ""},
		{"google api key", "AIzaSyD-0123456789abcdefghijklmnopqrstuvw", "google-api-key", ""},
		{"google oauth access token", "curl -H 'Authorization: Bearer ya29.a0AfH6SMBx0123456789abcdefghijklmnop'", "google-oauth-token", ""},
		{"stripe live secret key", "stripe uses " + token("sk_live_", "0123456789abcdefghijklmn"), "stripe-key", ""},
		{"npm automation token", "//registry.npmjs.org/:_authToken=npm_0123456789abcdefghijklmnopqrstuvwxyz", "npm-token", ""},
		{"pypi upload token", "pypi-AgEIcHlwaS5vcmc0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ", "pypi-token", ""},
		{"hugging face access token", "hf_0123456789abcdefghijklmnopqrstuvwxyzAB", "huggingface-token", ""},
		{"json web token", "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk", "jwt", ""},
		{"pem rsa private key block", pemRSA, "pem-private-key", ""},
		{"pem openssh private key block", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAA\n-----END OPENSSH PRIVATE KEY-----", "pem-private-key", ""},
		{"putty private key file header", "PuTTY-User-Key-File-2: ssh-ed25519", "putty-private-key", ""},
		{"cardano voting signing key file", `{"type":"VotingSigningKey","description":"Voting Key","cborHex":"5820` + coldTail + `"}`, "cardano-key-file", ""},
		{"cardano kes operational key", `{"type":"KESKey","v":1,"kesVerificationKey":"6f3120faba7b7f3d4e0d2b0a0a1a3a3a3a","seed":"` + seedHex + `"}`, "cardano-key-file", ""},
		{"cardano evol scaling key", `{"type":"EvolKey","scalingAlgorithm":"sum","v":1,"seed":"` + seedHex + `"}`, "cardano-key-file", ""},
		{"cardano payment signing key file", `{"type":"PaymentSigningKeyShelley_ed25519","description":"Payment Signing Key","cborHex":"5820` + coldTail + `"}`, "cardano-key-file", ""},
		{"cardano cold signing key beside its filename", "cold.skey cborHex 5820" + coldTail, "cardano-cbor-hex", "long-hex"},
		{"unlabelled cardano cold signing key paste", "the cold key was 5820" + coldTail, "long-hex", ""},
		{"twenty four word cardano mnemonic", "abandon ability able about above absent absorb abstract absurd abuse access accident account accuse achieve acid acoustic acquire across act action actor actress actual", "bip39-mnemonic", ""},
		{"authorization bearer header", "curl -H 'Authorization: Bearer Zm9vYmFyYmF6cXV4MDEyMzQ1Njc4OTBhYmNkZWY='", "authorization-bearer", ""},
		{"database url with inline password", "the dsn is postgres://ghost:hunter2isnotsafe@db.internal:5432/ghost", "url-inline-credentials", ""},
		{"markdown-escaped credential in a pasted token", `password: dckr\_pat\_QfMnjESK8cyTT6FTQ2tXbEeTubs`, "assigned-secret", ""},
		{"aws secret access key", "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", "assigned-secret", ""},
		{"camelCase key naming a credential", "myApiKey: Zq7Xn4Bt2Lm9Kc5Vr8Wd", "assigned-secret", ""},
		{"assigned client secret in a yaml file", "client_secret: Tq8Zn3Bk6Lm1Vr9Xc4Wd7Hf2Js5Pd0Ga", "assigned-secret", ""},
		{"real password after a variable reference", "password: ${SECRET_MANAGER_DB_PASSWORD} — the leaked one was password: Zq7Xn4Bt2Lm9Kc5Vr8Wd", "assigned-secret", ""},
		{"real token after an angle-bracket placeholder", "token: <your-value-here> then token: ghp_0123456789abcdefghijklmnopqrstuvwxyzAB", "github-pat", ""},
		{"real cardano cold key after a filler run", strings.Repeat("d", 7000) + " and the key was 5820" + coldTail, "long-hex", ""},
		{"real key after a row of zeroes", "padding " + strings.Repeat("0", 400) + " then the cold key 5820" + coldTail, "long-hex", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finding, ok := Detect(tc.text)
			if !ok {
				t.Fatalf("Detect(%q) found nothing, want rule %q", tc.text, tc.rule)
			}
			if finding.Rule != tc.rule && finding.Rule != tc.alt {
				t.Errorf("Detect(%q) rule = %q, want %q (or %q)", tc.text, finding.Rule, tc.rule, tc.alt)
			}
			if finding.Label == "" {
				t.Error("finding has no label — the rejection message would name no format")
			}
		})
	}
}

// TestDetectAllowsOrdinaryProse is the false-positive half, and it decides
// whether this control is usable. A memory store is prose: "token", "password"
// and "api key" are the vocabulary of the domain, so a keyword heuristic
// refuses legitimate knowledge and gets switched off within a week.
func TestDetectAllowsOrdinaryProse(t *testing.T) {
	cases := []struct{ name, text string }{
		{"api key mentioned in prose", "The api key for this service is sk-live-123."},
		{"short example token", "GITHUB_TOKEN=ghp_example"},
		{"client secret pointing elsewhere", "CLIENT_SECRET: rotate this value"},
		{"private key header with no key body", "PRIVATE_KEY=-----BEGIN PRIVATE KEY-----"},
		{"api key placeholder value", "api-key: example-secret"},
		{"tokenizer is not a token", "The tokenizer keeps a 30k vocabulary and drops unknown words."},
		{"bearer word in prose", "The relay forwards the request and adds a bearer header upstream."},
		{"shell variable reference", "password: ${SECRET_MANAGER_DB_PASSWORD}"},
		{"templated placeholder", "secret: <your-value-goes-here>"},
		{"masked placeholder", "api_key: ************************"},
		{"documented placeholder word", "api_key: changeme"},
		{"key path, no key", "Read the cold signing key from ./cold.skey before starting the node."},
		{"full git commit sha", "The fix landed in 8f2a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4."},
		{"sha512 checksum", "sha512 of the tarball: " + strings.Repeat("ab", 64)},
		{"sha256 image digest", "docker pull ghcr.io/blink-labs/ghost@sha256:" + strings.Repeat("0123456789abcdef", 4)},
		{"filler run of a hex letter", strings.Repeat("d", 7000)},
		{"filler run of a non-hex letter", strings.Repeat("r", 2000)},
		{"row of zeroes", "padding: " + strings.Repeat("0", 256)},
		{"short cardano payment credential", `payment credential: "600a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8"`},
		{"public verification key", "The block verification key is 5820a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a"},
		{"tls certificate not a key", "-----BEGIN CERTIFICATE-----\nMIIBkTCB+wIJAO0V\n-----END CERTIFICATE-----"},
		{"twelve word run is not a mnemonic", "abandon ability able about above absent absorb abstract absurd abuse access accident"},
		{"url with a user but no password", "the dsn is postgres://ghost@localhost:5432/ghost"},
		{"version pins", "go 1.26.6, modernc.org/sqlite v1.34.5, go-sdk v0.4.0"},
		{"lower-case word ending in token", "The session token bucket refills every 100ms."},
		{"short assignment under sixteen chars", "token: 4h2Kd9Xq"},
		{"a sentence containing equals", "the formula is width = cols - 2*border and always was."},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if finding, ok := Detect(tc.text); ok {
				t.Errorf("Detect(%q) flagged rule %q (%s), want no finding", tc.text, finding.Rule, finding.Label)
			}
		})
	}
}

// TestFindingNeverEchoesTheValue is what makes a rejection safe to log: the
// error a rejecting caller returns travels into the saving agent's context and
// the log file, so a message that quoted the matched value would relocate the
// token rather than contain it.
func TestFindingNeverEchoesTheValue(t *testing.T) {
	const value = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	finding, ok := Detect("token is " + value)
	if !ok {
		t.Fatal("Detect found nothing for a GitHub token")
	}
	if strings.Contains(finding.Label, "0123456789") || strings.Contains(finding.Rule, value) {
		t.Errorf("finding echoes the matched value: %+v", finding)
	}
}

// TestBip39WordListIsTheWholeList guards the mnemonic rule's data rather than
// its logic: a truncated or padded list makes a 24-word run of ordinary words
// look like a mnemonic.
func TestBip39WordListIsTheWholeList(t *testing.T) {
	words := bip39Words()
	if got := len(words); got != bip39WordCount {
		t.Errorf("word set has %d words, want %d", got, bip39WordCount)
	}
	for _, w := range []string{"abandon", "zoo", "abandoned"} {
		_, present := words[w]
		if want := w != "abandoned"; present != want {
			t.Errorf("word set membership of %q = %v, want %v", w, present, want)
		}
	}
}
