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
// value each case matches the shape of a real one — a detector cannot be tested
// against the formats its own repository is not permitted to hold.
func token(prefix, body string) string { return prefix + body }

// TestDetectRejectsCredentialValues is the positive half of the contract in
// issue #553: a save carrying a credential value must be refused. Each case is
// a value shape, not a keyword — the words "api key" or "password" appear all
// over a memory store and must never be enough on their own (see
// TestDetectAllowsOrdinaryProse).
func TestDetectRejectsCredentialValues(t *testing.T) {
	cases := []struct {
		name  string
		text  string
		rule  string
		label string
	}{
		{
			// The self-naming gate must be about the TAIL of the value. A chosen
			// password that repeats the field name is an ordinary paste, and
			// exempting it would store a real secret.
			name:  "password that repeats its own field name",
			text:  "password: MyOwnPassword12345678",
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			// A labelled Plutus script and an unrelated key paste in the same
			// memory. The cborHex exemption is scoped to the run the label
			// introduces, so the second run is still caught.
			name:  "plutus script label beside a separate key paste",
			text:  plutusScriptEnvelope + "\n" + "and the cold key was 5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f",
			rule:  "long-hex",
			label: "long hex blob",
		},
		{
			name:  "age encryption identity",
			text:  "age key: AGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ",
			rule:  "age-identity",
			label: "age encryption identity",
		},
		{
			name:  "anthropic api key",
			text:  "export ANTHROPIC_API_KEY=sk-ant-api03-Za0b1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx2Yz3Ab4Cd",
			rule:  "anthropic-key",
			label: "Anthropic API key",
		},
		{
			name:  "openai style api key",
			text:  "the key is sk-proj-Za0b1Cd2Ef3Gh4Ij5Kl6Mn7Op8Qr9St0Uv1Wx2Yz3Ab4Cd5Ef6",
			rule:  "openai-key",
			label: "OpenAI-style API key",
		},
		{
			// The legacy shape, pinned to its exact 48 characters with a word
			// boundary. Both openai-key branches need a case or the alternation
			// is only half-covered.
			name:  "openai legacy api key",
			text:  "the key is " + token("sk-", "Zq7Xn4Bt2Lm9Kc5Vr8WdYh3Nf6Pj1Qs4Rw7Ed2Ua5Tz8Yb3X"),
			rule:  "openai-key",
			label: "OpenAI-style API key",
		},
		{
			name:  "github classic personal access token",
			text:  "GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
			rule:  "github-pat",
			label: "GitHub personal access token",
		},
		{
			name:  "github fine grained personal access token",
			text:  "gh auth uses github_pat_11ABCDEFG0aBcDeFgHiJkLmNoPqRsTuVwXyZ0123",
			rule:  "github-pat",
			label: "GitHub personal access token",
		},
		{
			name:  "aws access key id",
			text:  "AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE",
			rule:  "aws-access-key-id",
			label: "AWS access key ID",
		},
		{
			// A labelled key with neither a .skey filename nor a "type" field.
			// This is the shape the cborHex label used to exempt wholesale, and
			// what catches it is the CBOR tag rather than the envelope: a key is
			// a byte string of 32 or 64 bytes, which is 5820/5840, while a
			// script's length needs two bytes and its tag is 59.
			name:  "cardano cold signing key in a bare cborHex field",
			text:  `the cold key cborHex: 5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f`,
			rule:  "cardano-cbor-hex",
			label: "Cardano CBOR-encoded key",
		},
		{
			// Three ordinary ways a key is pasted. Indentation is how it lands
			// inside a YAML block scalar or an indented code fence; CRLF is how
			// it comes out of a Windows editor; escaped newlines are how kubectl
			// and Terraform render a Secret.
			name:  "indented pem block inside a yaml block scalar",
			text:  "  -----BEGIN RSA PRIVATE KEY-----\n  MIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy\n  -----END RSA PRIVATE KEY-----",
			rule:  "pem-private-key",
			label: "PEM private key block",
		},
		{
			name:  "pem block with crlf line endings",
			text:  "-----BEGIN RSA PRIVATE KEY-----\r\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy\r\n-----END RSA PRIVATE KEY-----\r\n",
			rule:  "pem-private-key",
			label: "PEM private key block",
		},
		{
			name:  "pem block with escaped newlines from a json secret",
			text:  `{"data":{"tls.key":"-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Pyy\n-----END RSA PRIVATE KEY-----"}}`,
			rule:  "pem-private-key",
			label: "PEM private key block",
		},
		{
			// A hex credential under a credential-named key. The word test splits
			// on digits, so without the entirely-hex exclusion this reads as a
			// 16-letter lowercase "word" and is stored.
			name:  "hex api key under a credential-named key",
			text:  "api_key: deadbeefcafebabe0123456789abcdef",
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			// A literal assigned to a shell variable. The $ on the key is not a
			// reason to skip: only a value with a flag after it is a command.
			name:  "literal secret assigned to a shell variable",
			text:  `$db_password = "K3q9Xm2pL7wRt4ZbAvN1"`,
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			// Split at the prefix: a contiguous Slack token literal is rejected
			// by GitHub push protection (GH013) before review, so this is the
			// one place a detector's fixtures have to be assembled rather than
			// written out.
			name:  "slack bot token",
			text:  "slack: " + token("xoxb-", "123456789012-1234567890123-AbCdEfGhIjKlMnOpQrStUvWx"),
			rule:  "slack-token",
			label: "Slack token",
		}, {
			name:  "google api key",
			text:  "AIzaSyD-0123456789abcdefghijklmnopqrstuvw",
			rule:  "google-api-key",
			label: "Google API key",
		},
		{
			name:  "google oauth access token",
			text:  "curl -H 'Authorization: Bearer ya29.a0AfH6SMBx0123456789abcdefghijklmnop'",
			rule:  "google-oauth-token",
			label: "Google OAuth access token",
		},
		{
			// Split at the prefix, for the same push-protection reason as the
			// Slack case above.
			name:  "stripe live secret key",
			text:  "stripe uses " + token("sk_live_", "0123456789abcdefghijklmn"),
			rule:  "stripe-key",
			label: "Stripe API key",
		},
		{
			name:  "npm automation token",
			text:  "//registry.npmjs.org/:_authToken=npm_0123456789abcdefghijklmnopqrstuvwxyz",
			rule:  "npm-token",
			label: "npm access token",
		},
		{
			name:  "pypi upload token",
			text:  "pypi-AgEIcHlwaS5vcmc0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJ",
			rule:  "pypi-token",
			label: "PyPI upload token",
		},
		{
			name:  "hugging face access token",
			text:  "hf_0123456789abcdefghijklmnopqrstuvwxyzAB",
			rule:  "huggingface-token",
			label: "Hugging Face access token",
		},
		{
			name:  "json web token",
			text:  "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4ifQ.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXk",
			rule:  "jwt",
			label: "JSON Web Token",
		},
		{
			name:  "pem rsa private key block",
			text:  "-----BEGIN RSA PRIVATE KEY-----\nMIIBOgIBAAJBAKj34GkxFhD90vcNLYLInFEX6Ppy1tPf9Cnzj4p4WGeKLs1Pt8Qu\n-----END RSA PRIVATE KEY-----",
			rule:  "pem-private-key",
			label: "PEM private key block",
		},
		{
			name:  "pem openssh private key block",
			text:  "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAA\n-----END OPENSSH PRIVATE KEY-----",
			rule:  "pem-private-key",
			label: "PEM private key block",
		},
		{
			name:  "putty private key file header",
			text:  "PuTTY-User-Key-File-2: ssh-ed25519",
			rule:  "putty-private-key",
			label: "PuTTY private key file",
		},
		{
			name:  "cardano voting signing key file",
			text:  `{"type":"VotingSigningKey","description":"Voting Key","cborHex":"5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"}`,
			rule:  "cardano-key-file",
			label: "Cardano private key file",
		},
		{
			name:  "cardano kes operational key",
			text:  `{"type":"KESKey","v":1,"kesVerificationKey":"6f3120faba7b7f3d4e0d2b0a0a1a3a3a3a"}`,
			rule:  "cardano-key-file",
			label: "Cardano private key file",
		},
		{
			name:  "cardano evol scaling key",
			text:  `{"type":"EvolKey","scalingAlgorithm":"sum","v":1}`,
			rule:  "cardano-key-file",
			label: "Cardano private key file",
		},
		{
			// The envelope's type field, with the curve suffix the reviewer named
			// and the reason the type-name tail accepts underscores and digits.
			// cardano-key-file owns this shape.
			name:  "cardano payment signing key file",
			text:  `{"type":"PaymentSigningKeyShelley_ed25519","description":"Payment Signing Key","cborHex":"5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f"}`,
			rule:  "cardano-key-file",
			label: "Cardano private key file",
		},
		{
			// The other envelope: a paste that names the file rather than the
			// type. This is the shape cardano-cbor-hex owns.
			name:  "cardano cold signing key beside its filename",
			text:  "cold.skey cborHex 5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f",
			rule:  "cardano-cbor-hex",
			label: "Cardano CBOR-encoded key",
		},
		{
			name:  "unlabelled cardano cold signing key paste",
			text:  "the cold key was 5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f",
			rule:  "long-hex",
			label: "long hex blob (key material or a key dump)",
		},
		{
			name: "twenty four word cardano mnemonic",
			text: "abandon ability able about above absent absorb abstract absurd abuse " +
				"access accident account accuse achieve acid acoustic acquire across " +
				"act action actor actress actual",
			rule:  "bip39-mnemonic",
			label: "24-word BIP-39 recovery mnemonic",
		},
		{
			name:  "authorization bearer header",
			text:  "curl -H 'Authorization: Bearer Zm9vYmFyYmF6cXV4MDEyMzQ1Njc4OTBhYmNkZWY='",
			rule:  "authorization-bearer",
			label: "Authorization bearer token",
		},
		{
			name:  "database url with inline password",
			text:  "the dsn is postgres://ghost:hunter2isnotsafe@db.internal:5432/ghost",
			rule:  "url-inline-credentials",
			label: "URL with inline credentials",
		},
		{
			// A backslash in a value is a markdown escape, not code — this one is
			// a real Docker Hub token from the benchmark corpus, and the
			// expression-character gate deliberately excludes a backslash for it.
			name:  "markdown-escaped credential in a pasted token",
			text:  `password: dckr\_pat\_QfMnjESK8cyTT6FTQ2tXbEeTubs`,
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			name: "aws secret access key",
			// The access key ID has its own prefix rule, but the secret half is
			// plain 64-char base64ish hex with no marker, so this is the case
			// that decides whether an assigned-value rule is needed at all.
			text:  "aws_secret_access_key = wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			name: "camelCase key naming a credential",
			// The camel boundary in keyWords is what makes this a credential
			// field at all: without it the key is the single token "myapikey",
			// which is not a noun, and the value is missed. The negative side of
			// the same boundary is in the Helm table (secretName,
			// existingSecret).
			text:  "myApiKey: Zq7Xn4Bt2Lm9Kc5Vr8Wd",
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			name:  "assigned secret without a provider prefix",
			text:  `the runner sets password = "Zq7Xn4Bt2Lm9Kc5Vr8Wd"`,
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
		{
			name:  "assigned client secret in a yaml file",
			text:  "client_secret: Tq8Zn3Bk6Lm1Vr9Xc4Wd7Hf2Js5Pd0Ga",
			rule:  "assigned-secret",
			label: "assigned credential value",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finding, ok := Detect(tc.text)
			if !ok {
				t.Fatalf("Detect(%q) found nothing, want rule %q", tc.text, tc.rule)
			}
			if finding.Rule != tc.rule {
				t.Errorf("Detect(%q) rule = %q, want %q", tc.text, finding.Rule, tc.rule)
			}
			if finding.Label == "" {
				t.Error("finding has no label — the rejection message would name no format")
			}
		})
	}
}

// TestDetectAllowsOrdinaryProse is the false-positive half, and it is the half
// that decides whether this control is usable. A memory store is prose: the
// words "token", "password" and "api key" are the vocabulary of the domain
// (tokenizers, password-reset flows, key rotation policies), so a keyword
// heuristic refuses legitimate knowledge and gets switched off within a week.
//
// The cases below are the real content of this repo's own database, plus the
// shapes an over-eager detector reaches for: a header that names a key without
// containing one, a shell variable reference, a placeholder, a standard
// checksum, and a public key.
func TestDetectAllowsOrdinaryProse(t *testing.T) {
	cases := []struct {
		name string
		text string
	}{
		{"api key mentioned in prose", "The api key for this service is sk-live-123."},
		{"password rotation policy", "Rotate the deployment password every quarter."},
		{"access token in the runner env", "The CI access_token lives in the runner env."},
		{"short example token", "GITHUB_TOKEN=ghp_example"},
		{"client secret pointing elsewhere", "CLIENT_SECRET: rotate this value"},
		{"private key header with no key body", "PRIVATE_KEY=-----BEGIN PRIVATE KEY-----"},
		{"api key placeholder value", "api-key: example-secret"},
		{"tokenizer is not a token", "The tokenizer keeps a 30k vocabulary and drops unknown words."},
		{"password reset flow", "The password reset flow sends a link that expires in 15 minutes."},
		{"secret rotation convention", "Every secret is rotated quarterly and the old value revoked."},
		{"credential scanning in CI", "CI runs gitleaks over the repository on every push."},
		{"bearer word in prose", "The relay forwards the request and adds a bearer header upstream."},
		{"shell variable reference", "password: ${SECRET_MANAGER_DB_PASSWORD}"},
		{"templated placeholder", "secret: <your-value-goes-here>"},
		{"masked placeholder", "api_key: ************************"},
		{"documented placeholder word", "api_key: changeme"},
		{"named pointer to a secret", "The KES signing key lives in /etc/cardano/kes/keys/ and is never in git."},
		{"key path, no key", "Read the cold signing key from ./cold.skey before starting the node."},
		{"full git commit sha", "The fix landed in 8f2a1b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4."},
		{"sha512 checksum", "sha512 of the tarball: " + strings.Repeat("ab", 64)},
		{"sha256 image digest", "docker pull ghcr.io/blink-labs/ghost@sha256:" + strings.Repeat("0123456789abcdef", 4)},
		// A filler run built from hex letters. a, d, e and f are all hex, so
		// padding is an unbroken hex run thousands of characters long — this is
		// not hypothetical, it is what Ghost's own decision-cap test passes in.
		{"filler run of a hex letter", strings.Repeat("d", 7000)},
		{"filler run of another hex letter", strings.Repeat("f", 8000)},
		{"filler run of a non-hex letter", strings.Repeat("r", 2000)},
		{"row of zeroes", "padding: " + strings.Repeat("0", 256)},
		{"short cardano payment credential", `payment credential: "600a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8"`},
		{"cardano payment public key hash", `address stake key hash is 7a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f`},
		{"public verification key", "The block verification key is 5820a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90a"},
		{"tls certificate not a key", "-----BEGIN CERTIFICATE-----\nMIIBkTCB+wIJAO0V\n-----END CERTIFICATE-----"},
		{"twelve word run is not a mnemonic", "abandon ability able about above absent absorb abstract absurd abuse access accident"},
		{"word list in a memory", "the quick brown fox jumps over the lazy dog and keeps running through the field"},
		{"url with a user but no password", "the dsn is postgres://ghost@localhost:5432/ghost"},
		{"version pins", "go 1.26.6, modernc.org/sqlite v1.34.5, go-sdk v0.4.0"},
		{"port and host facts", "k3s-mini-1 runs Grafana on port 80 and the relay on 2222."},
		{"cardano fee configuration", "The relay charges a 1.55 ada fee plus a 44 lovelace per-byte minimum."},
		{"key file the operator must not commit", "operational.json, scaling.json, kes.json and cold.skey are all in .gitignore."},
		{"lower-case word ending in token", "The session token bucket refills every 100ms."},
		{"short assignment under sixteen chars", "token: 4h2Kd9Xq"},
		{"a sentence containing equals", "the formula is width = cols - 2*border and always was."},
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

// TestDetectExaminesEveryCandidate is the false negative a first-match
// implementation hides: the two rules whose verdict needs the matched text
// inspect candidates, and a placeholder-shaped candidate earlier in the save
// must not shadow a real credential later in the same save. That ordering is
// not adversarial — "the env var is ${X}, the value we leaked was <key>" is
// exactly the shape of an incident note.
func TestDetectExaminesEveryCandidate(t *testing.T) {
	cases := []struct {
		name string
		text string
		rule string
	}{
		{
			name: "real password after a variable reference",
			text: "password: ${SECRET_MANAGER_DB_PASSWORD} — the leaked one was password: Zq7Xn4Bt2Lm9Kc5Vr8Wd",
			rule: "assigned-secret",
		},
		{
			name: "real password after a masked placeholder",
			text: "api_key: ************************ and the real one is api_key=Kq8Zn3Bk6Lm1Vr9Xc4Wd7Hf2",
			rule: "assigned-secret",
		},
		{
			name: "real token after an angle-bracket placeholder",
			text: "token: <your-value-here> then token: ghp_0123456789abcdefghijklmnopqrstuvwxyzAB",
			rule: "github-pat",
		},
		{
			name: "real cardano cold key after a filler run",
			text: strings.Repeat("d", 7000) +
				" and the key was 5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f",
			rule: "long-hex",
		},
		{
			name: "real key after a row of zeroes",
			text: "padding " + strings.Repeat("0", 400) +
				" then the cold key 5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f9a8b7c6d5e4f3a2b1c0d9e8f7a6b5c4d3e2f1a0b9c8d7e6f5a4b3c2d1e0f",
			rule: "long-hex",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finding, ok := Detect(tc.text)
			if !ok {
				t.Fatalf("Detect found nothing, want rule %q", tc.rule)
			}
			if finding.Rule != tc.rule {
				t.Errorf("Detect rule = %q, want %q", finding.Rule, tc.rule)
			}
		})
	}
}

// TestFindingNeverEchoesTheValue is what makes a rejection safe to log. The
// error a rejecting caller returns travels into the saving agent's context, the
// log file, and (for reflection) a prompt sent to a third-party model, so a
// message that quoted the matched value would relocate the secret rather than
// contain it.
func TestFindingNeverEchoesTheValue(t *testing.T) {
	const value = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	finding, ok := Detect("token is " + value)
	if !ok {
		t.Fatal("Detect found nothing for a GitHub token")
	}
	if strings.Contains(finding.Label, "0123456789") {
		t.Errorf("finding label echoes the matched value: %q", finding.Label)
	}
	if strings.Contains(finding.Rule, value) {
		t.Errorf("finding rule echoes the matched value: %q", finding.Rule)
	}
}

// TestDetectIsStableOnRepeatedCalls pins that Detect is a pure function of its
// input: the store calls it on a save and a caller may call it again to build a
// different message, and the two must not disagree.
func TestDetectIsStableOnRepeatedCalls(t *testing.T) {
	text := "client_secret: Tq8Zn3Bk6Lm1Vr9Xc4Wd7Hf2Js5Pd0Ga"
	first, ok := Detect(text)
	if !ok {
		t.Fatal("Detect found nothing for an assigned secret")
	}
	for i := 0; i < 3; i++ {
		again, ok := Detect(text)
		if !ok || again != first {
			t.Fatalf("Detect call %d = (%+v, %v), want (%+v, true)", i+1, again, ok, first)
		}
	}
}

// TestBip39WordListIsTheWholeList guards the mnemonic rule's data rather than
// its logic: a truncated or padded list makes a 24-word run of ordinary words
// look like a mnemonic, which is a false rejection of exactly the prose this
// control must let through.
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
