package secret

import "testing"

// TestDetectAllowsHelmAndKubernetesCredentialVocabulary is the false-positive
// half of the contract: the vocabulary of the tools Ghost's users run (Helm,
// k8s, CI, DSNs, pasted code) is mostly *about* credentials without recording a
// value, and a detector that refuses it gets switched off. Each text is checked
// through Detect, so the test states the requirement rather than the rule that
// used to catch it. Several rows exist because exactly one gate of the
// assigned-credential rule stops them.
func TestDetectAllowsHelmAndKubernetesCredentialVocabulary(t *testing.T) {
	cases := []struct{ name, text string }{
		{"helm secret entry name", "secretName: wildcard-tls-2024"},
		{"grafana provisioning secret name", "existingSecret: grafana-admin-v2"},
		{"oauth endpoint under a token key", "token_url: https://oauth2.googleapis.com/token"},
		{"password rotation timestamp", "password_changed_at: 2026-09-27T04:00:00Z"},
		{"secret reference by name", "token: secrets.GITHUB_TOKEN"},
		{"placeholder credentials in a dsn", "the dsn is postgres://USER:PASSWORD@host/db"},
		{"uppercase placeholder password", "postgres://ghost:HUNTER2@relay-1.example/ghost"},
		{"public plutus script, not a key", `{"type":"PlutusScriptV1","description":"always-true validator","cborHex":"` + plutusScriptHex + `"}`},
		{"bare plutus cborHex with no key envelope", `cborHex: "` + plutusScriptHex + `"`},
		{"service host that starts with sk", "the relay resolves sk-prod-cluster-node-01.example"},
		{"a longer host that starts with sk", "the relay resolves sk-production-cluster-node-01.example"},
		{"tx hash beside the skey that signed it", "signed with payment.skey, submitted tx " + cardano32[4:]},
		{"dsn with a substituted password", "postgres://app:${POSTGRES_PASSWORD}@db"},
		{"mongodb dsn with an angle-bracket password", "mongodb+srv://u:<db_password>@cluster0.example.net/orders"},
		{"default dev credential", "the dev stack is postgres://postgres:postgres@localhost"},
		{"client secret read from a vault helper", "ClientSecret: opts.OAuthClientSecretFromVault"},
		{"token digest", "token_sha: 9a3b7c1d5e2f40816b2d9c7e4a5f3b0d8c6e1a"},
		{"documentation naming both pem markers in one line", "the file must contain the lines -----BEGIN PRIVATE KEY----- and -----END PRIVATE KEY-----"},
		{"powershell variable assigned a cmdlet", `$domainAdminPassword = ConvertTo-SecureString "{1}" -AsPlainText -Force`},
		{"powershell variable assigned another variable", `$securePassword = ConvertTo-SecureString -String $domainPassword -AsPlainText -Force`},
		{"js handler assigning a request body field", "accessToken: req.body.accessToken,"},
		{"token assigned a function call", `token = PasswordResetTokenGenerator().make\_token(user) # Generate token`},
		{"credential field assigned a helper call", "api_key: fetchCredentialFromVault(ENV)"},
		{"random-looking value under a naming key", "secretName: Zq7Xn4Bt2Lm9Kc5Vr8Wd"},
		{"short-path url under a token key", "token: https://a.io/b.co/c.de"},
		{"redaction run with a visible tail", "api_key: ****Zq7Xn4Bt2Lm9Kc5Vr8"},
		{"quoted powershell cmdlet", `$domainAdminPassword = "ConvertTo-SecureString" "{1}" -AsPlainText -Force`},
		{"labelled kes verification key", `{"type":"KESVerificationKey","description":"cold vrf key","cborHex":"5820f2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f"}`},
		{"labelled payment verification key", `{"type":"PaymentVerificationKey","cborHex":"5820010df2429ae14536b3438abb84f7d3e8329ae48c3ecc9b1c1e5dbf1a1a5b8b4c2d1e0f"}`},
		{"alternating case that is over the length floor", "password: AbAbAbAbAbAbAbAbAbAb"},
		{"lower-case placeholder password in a dsn", "postgres://ghost:password@relay-1.example/ghost"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if finding, ok := Detect(tc.text); ok {
				t.Errorf("Detect(%q) flagged rule %q (%s), want no finding — credential vocabulary, not a credential",
					tc.text, finding.Rule, finding.Label)
			}
		})
	}
}

// TestKeyAndValueGatesAreIndependentlyLoadBearing measures each gate of the
// assigned-credential rule on its own: several gates overlap on realistic
// input, so removing one changes no corpus verdict. Each refused row is a key
// or value exactly one gate refuses; the accepted rows show no predicate is
// simply reversed.
func TestKeyAndValueGatesAreIndependentlyLoadBearing(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		for key, want := range map[string]bool{
			"secretName": false, "existingSecret": false, "token_url": false,
			"password_changed_at": false, "secret_ref": false, "api_key_path": false,
			"notsecret": false, "tokenizer_key": false, "passwording": false,
			"password": true, "client_secret": true, "api_key": true,
			"aws_secret_access_key": true, "myApiKey": true, "GITHUB_TOKEN": true,
			"db_password": true, "private_key": true,
		} {
			if got := keyNamesSecret(key); got != want {
				t.Errorf("keyNamesSecret(%q) = %v, want %v", key, got, want)
			}
		}
	})

	t.Run("value", func(t *testing.T) {
		for value, want := range map[string]bool{
			"****Zq7Xn4Bt2Lm9Kc5Vr8": false, "https://a1.io/B2/c3d/e4f.gh": false,
			"secrets.GITHUB_TOKEN": false, "grafana-admin-v2": false,
			"AbAbAbAbAbAbAbAbAbAb": false, "!@#%^&()-_=+.,;/|~?` :": false,
			"12345678901234567890": false,
			"Zq7Xn4Bt2Lm9Kc5Vr8Wd": true, "Tq8Zn3Bk6Lm1Vr9Xc4Wd7Hf2Js5Pd0Ga": true,
			"wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY": true,
		} {
			if got := looksLikeCredentialMaterial("some_field", value); got != want {
				t.Errorf("looksLikeCredentialMaterial(%q) = %v, want %v", value, got, want)
			}
		}
	})

	t.Run("url password", func(t *testing.T) {
		for _, tc := range []struct {
			user, password string
			want           bool
		}{
			{"ghost", "HUNTER2", false}, {"ghost", "K7MDENG", false},
			{"ghost", "password", false}, {"ghost", "changeme", false},
			{"app", "${POSTGRES_PASSWORD}", false}, {"u", "<db_password>", false},
			{"someapp", "postgres", false},
			{"postgres", "postgres", false}, {"admin", "Admin", false},
			{"appuser", "appuser", false}, {"svc_account", "SVC_ACCOUNT", false},
			{"ghost", "hunter2isnotsafe", true}, {"ghost", "K7mDeng9Xq4Bt2", true},
			{"someapp", "hunter2isnotsafe", true},
		} {
			if got := looksLikeURLCredential(tc.user, tc.password); got != tc.want {
				t.Errorf("looksLikeURLCredential(%q, %q) = %v, want %v", tc.user, tc.password, got, tc.want)
			}
		}
	})
}

// plutusScriptHex is a public Plutus V1 script in the double-CBOR form a
// plutus.json carries, starting at the `59` byte-string tag. It is long enough
// and hex enough to be a key's cborHex, which is the whole point: a script and a
// signing key are the same shape, and only the envelope around them differs.
// plutusScriptEnvelope is a public Plutus script the way plutus.json writes it.
const plutusScriptEnvelope = `{"type":"PlutusScriptV1","cborHex":"59` + plutusScriptHex + `"}`

const plutusScriptHex = "59" +
	"014301323589010029800" +
	"450141323355454501000002590101291323355450142" +
	"b0054645001445473374555279" +
	"0100298933554540014700" +
	"4545001233554545001470" + "001323355454001323355" +
	"45001473755f0100294533" + "55a12f45534f0f0f" +
	"0a22222a32335545450" + "0141f1f1f1f12" +
	"3233554545" + "00"
