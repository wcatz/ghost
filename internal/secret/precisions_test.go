package secret

import (
	"testing"
)

// TestDetectAllowsHelmAndKubernetesCredentialVocabulary is the false-positive
// half of the contract, and on this side of it the corpus is not Ghost's own
// database but the vocabulary of the tools Ghost's users actually run.
//
// A Helm chart, a k8s manifest, a Grafana provisioning file and a CI workflow are
// all mostly *about* credentials, and every one of them records a credential
// without ever recording a value: `secretName` names the entry,
// `existingSecret` names the one to rotate, `token_url` names the endpoint,
// `password_changed_at` names the timestamp, `secrets.GITHUB_TOKEN` names the
// lookup, `USER:PASSWORD` is the literal placeholder, a `cborHex` field is a
// public Plutus script. A detector that refuses those refuses the memory a
// user writes while debugging their own cluster, and a control that does that
// gets switched off — which is the same outcome as having no control, reached
// faster.
//
// Each case is checked through Detect, not through the rule that used to catch
// it, so the test states the requirement rather than the implementation.
func TestDetectAllowsHelmAndKubernetesCredentialVocabulary(t *testing.T) {
	cases := []struct {
		name string
		text string
		// was flags the rule that used to fire on this text, so a reader can see
		// which tightening each case is holding down.
		was string
	}{
		{
			name: "helm secret entry name",
			text: "secretName: wildcard-tls-2024",
			was:  "assigned-secret",
		},
		{
			name: "grafana provisioning secret name",
			text: "existingSecret: grafana-admin-v2",
			was:  "assigned-secret",
		},
		{
			name: "oauth endpoint under a token key",
			text: "token_url: https://oauth2.googleapis.com/token",
			was:  "assigned-secret",
		},
		{
			name: "password rotation timestamp",
			text: "password_changed_at: 2026-09-27T04:00:00Z",
			was:  "assigned-secret",
		},
		{
			name: "secret reference by name",
			text: "token: secrets.GITHUB_TOKEN",
			was:  "assigned-secret",
		},
		{
			name: "placeholder credentials in a dsn",
			text: "the dsn is postgres://USER:PASSWORD@host/db",
			was:  "url-inline-credentials",
		},
		{
			name: "uppercase placeholder password",
			text: "postgres://ghost:HUNTER2@relay-1.example/ghost",
			was:  "url-inline-credentials",
		},
		{
			name: "public plutus script, not a key",
			text: `{"type":"PlutusScriptV1","description":"always-true validator",` +
				`"cborHex":"` + plutusScriptHex + `"}`,
			was: "cardano-cbor-hex",
		},
		{
			name: "bare plutus cborHex with no key envelope",
			text: `cborHex: "` + plutusScriptHex + `"`,
			was:  "cardano-cbor-hex",
		},
		{
			name: "service host that starts with sk",
			text: "the relay resolves sk-prod-cluster-node-01.example",
			was:  "openai-key",
		},
		{
			name: "a longer host that starts with sk",
			// 27 characters after the prefix, which is over the old {24,} floor
			// and is the case the real key shape actually excludes.
			text: "the relay resolves sk-production-cluster-node-01.example",
			was:  "openai-key",
		},
		{
			name: "a host that looks exactly like the project-scoped prefix",
			text: "we deploy to sk-prod-eu-west-1-elasticsearch-ingress-gateway",
			was:  "openai-key",
		},

		// The four below are not Helm or Kubernetes vocabulary. They are here
		// because each is stopped by exactly ONE of the rule's gates, and a gate
		// no case depends on is a gate nobody has measured. Each was confirmed
		// RED against the version of the rule with that one gate removed.
		{
			// Only the key test stops this. The value is 20 characters of
			// high-entropy mixed case with no template, no URL and no word
			// segment — indistinguishable from a real password. What makes it
			// safe is that the key names a Helm entry rather than a secret.
			name: "random-looking value under a naming key",
			text: "secretName: Zq7Xn4Bt2Lm9Kc5Vr8Wd",
			was:  "assigned-secret, key test only",
		},
		{
			// Only the URL gate stops this: 22 characters, high entropy, and no
			// single-case word segment anywhere in it, because every path
			// component is short.
			name: "short-path url under a token key",
			text: "token: https://a.io/b.co/c.de",
			was:  "assigned-secret, url gate only",
		},
		{
			// Only the placeholder gate stops this: a redaction that leaves the
			// tail visible, a shape operators genuinely paste and which is still
			// a leak.
			name: "redaction run with a visible tail",
			text: "api_key: ****Zq7Xn4Bt2Lm9Kc5Vr8",
			was:  "assigned-secret, placeholder gate only",
		},
		{
			// Only the entropy bar stops this. 20 characters, two character
			// classes, mixed case so no single-case word run forms, no separator
			// and therefore no word segment, and exactly at the length floor — and
			// 1.0 bits per byte, because it is one letter repeated. A weak
			// password is still a password; this is the case that says the bar
			// measures randomness rather than spelling.
			name: "alternating case that is over the length floor",
			text: "password: AbAbAbAbAbAbAbAbAbAb",
			was:  "assigned-secret, entropy bar only",
		},
		{
			// Only the placeholder word list stops this: lower case, so the
			// all-caps test cannot see it, and exactly at the length floor.
			name: "lower-case placeholder password in a dsn",
			text: "postgres://ghost:password@relay-1.example/ghost",
			was:  "url-inline-credentials, word list only",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if finding, ok := Detect(tc.text); ok {
				t.Errorf("Detect(%q) flagged rule %q (%s), want no finding — this is credential vocabulary, not a credential (the %s rule used to catch it)",
					tc.text, finding.Rule, finding.Label, tc.was)
			}
		})
	}
}

// TestKeyAndValueGatesAreIndependentlyLoadBearing measures each gate of the
// assigned-credential rule on its own, which the table above cannot do.
//
// The reason it needs doing: several of the gates overlap on realistic input, so
// a corpus case is usually stopped by two or three of them at once. That is a
// good property — defence in depth — but it means removing any single gate
// changes no corpus verdict, and a gate nobody can observe being load-bearing is
// a gate nobody has measured. Each row below is a value or a key that exactly
// one gate refuses, and the removed-gate mutations were confirmed RED against
// this table.
func TestKeyAndValueGatesAreIndependentlyLoadBearing(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		cases := []struct {
			key       string
			namesIt   bool
			refusedBy string
		}{
			// The descriptor list is the only thing that separates these from
			// a field that holds a value. `secret` is a whole word in all of
			// them, exactly as it is in the accepted pair below.
			{key: "secretName", namesIt: false, refusedBy: "descriptorWords"},
			{key: "existingSecret", namesIt: false, refusedBy: "descriptorWords"},
			{key: "token_url", namesIt: false, refusedBy: "descriptorWords"},
			{key: "password_changed_at", namesIt: false, refusedBy: "descriptorWords"},
			{key: "secret_ref", namesIt: false, refusedBy: "descriptorWords"},
			{key: "api_key_path", namesIt: false, refusedBy: "descriptorWords"},
			// The word-boundary test: the noun has to BE a word. A key that
			// merely contains the letters is a different field.
			{key: "notsecret", namesIt: false, refusedBy: "secretNouns word test"},
			{key: "tokenizer_key", namesIt: false, refusedBy: "secretNouns word test"},
			{key: "passwording", namesIt: false, refusedBy: "secretNouns word test"},
			// Accepted, so the refusal above is not the predicate being simply
			// reversed.
			{key: "password", namesIt: true},
			{key: "client_secret", namesIt: true},
			{key: "api_key", namesIt: true},
			{key: "aws_secret_access_key", namesIt: true},
			{key: "myApiKey", namesIt: true},
			{key: "GITHUB_TOKEN", namesIt: true},
			{key: "db_password", namesIt: true},
			{key: "private_key", namesIt: true},
		}
		for _, tc := range cases {
			if got := keyNamesSecret(tc.key); got != tc.namesIt {
				t.Errorf("keyNamesSecret(%q) = %v, want %v (refused by %s)",
					tc.key, got, tc.namesIt, tc.refusedBy)
			}
		}
	})

	t.Run("value", func(t *testing.T) {
		cases := []struct {
			value     string
			material  bool
			refusedBy string
		}{
			// Placeholder gate alone. 24 characters, three character classes,
			// over the floor — and a redaction that leaves the tail visible,
			// which is both a real paste and a real leak.
			{value: "****Zq7Xn4Bt2Lm9Kc5Vr8", material: false, refusedBy: "placeholderChars"},
			// URL gate alone. Every path component is under the word floor and
			// the value carries three character classes, so the word test and
			// the class test both pass it through; only "it is a URL" stops it.
			{value: "https://a1.io/B2/c3d/e4f.gh", material: false, refusedBy: "urlValuePrefix"},
			// Word-segment gate alone: two references whose every component is
			// a word, so the entropy bar is never consulted.
			{value: "secrets.GITHUB_TOKEN", material: false, refusedBy: "hasWordSegment"},
			{value: "grafana-admin-v2", material: false, refusedBy: "hasWordSegment"},
			// Entropy bar alone: right length, two character classes, mixed case
			// so no single-case run forms, and 1.0 bits per byte.
			{value: "AbAbAbAbAbAbAbAbAbAb", material: false, refusedBy: "entropy bar"},
			// Character-class gate alone. 20 punctuation characters: no letters,
			// so the word-segment test has nothing to look at, and none of
			// placeholderChars, so the template test passes it through. This is
			// the only shape the class gate reaches on its own, which is what the
			// comment on the gate says.
			{value: "!@#%^&()-_=+.,;/|~?` :", material: false, refusedBy: "hasTwoCharClasses"},
			// Digits only, which the class test also refuses — and which the
			// entropy bar would refuse on its own, because a ten-symbol alphabet
			// caps at log2(10) = 3.32 bits, under the floor. The row exists to
			// pin that the value stays acceptable rather than becoming a
			// finding, not because this is the only gate that would say so.
			{value: "12345678901234567890", material: false, refusedBy: "hasTwoCharClasses, and the entropy ceiling too"},
			// Accepted, so none of the rows above is the predicate reversed.
			{value: "Zq7Xn4Bt2Lm9Kc5Vr8Wd", material: true},
			{value: "Tq8Zn3Bk6Lm1Vr9Xc4Wd7Hf2Js5Pd0Ga", material: true},
			{value: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY", material: true},
		}
		for _, tc := range cases {
			if got := looksLikeCredentialMaterial(tc.value); got != tc.material {
				t.Errorf("looksLikeCredentialMaterial(%q) = %v, want %v (refused by %s)",
					tc.value, got, tc.material, tc.refusedBy)
			}
		}
	})

	t.Run("url password", func(t *testing.T) {
		cases := []struct {
			password  string
			value     bool
			refusedBy string
		}{
			// All-caps gate alone: lower-case in the word list? no. So the case
			// test is what refuses it.
			{password: "HUNTER2", value: false, refusedBy: "urlPasswordAllCaps"},
			{password: "K7MDENG", value: false, refusedBy: "urlPasswordAllCaps"},
			// Word list alone: lower case, so the case test cannot see it.
			{password: "password", value: false, refusedBy: "urlCredentialPlaceholders"},
			{password: "changeme", value: false, refusedBy: "urlCredentialPlaceholders"},
			// Accepted, so neither gate is simply inverted.
			{password: "hunter2isnotsafe", value: true},
			{password: "K7mDeng9Xq4Bt2", value: true},
		}
		for _, tc := range cases {
			if got := looksLikeURLCredential(tc.password); got != tc.value {
				t.Errorf("looksLikeURLCredential(%q) = %v, want %v (refused by %s)",
					tc.password, got, tc.value, tc.refusedBy)
			}
		}
	})
}

// plutusScriptHex is a public Plutus V1 script in the double-CBOR form a
// plutus.json carries, starting at the `59` byte-string tag. It is long enough
// and hex enough to be a key's cborHex, which is the whole point: a script and a
// signing key are the same shape, and only the envelope around them differs.
const plutusScriptHex = "59" +
	"014301323589010029800" +
	"450141323355454501000002590101291323355450142" +
	"b0054645001445473374555279" +
	"0100298933554540014700" +
	"4545001233554545001470" + "001323355454001323355" +
	"45001473755f0100294533" + "55a12f45534f0f0f" +
	"0a22222a32335545450" + "0141f1f1f1f12" +
	"3233554545" + "00"
