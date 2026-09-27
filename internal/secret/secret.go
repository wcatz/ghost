// Package secret detects credential-shaped values in text Ghost is about to
// store.
//
// It answers one question — "does this value look like a secret?" — and it
// answers it from the *shape* of the value, never from the words around it.
// That distinction is the whole design. A memory store is prose about
// credentials: "rotate the deployment password quarterly", "the CI access token
// lives in the runner env", "the tokenizer keeps a 30k vocabulary" are ordinary
// knowledge about secrets, and a keyword heuristic refuses all three. The
// reflection package's looksLikeSecret, which does exactly that, is the right
// tool for its own question — is this text safe to *replay into every project*
// — and the wrong tool for this one: it must never reject a save, only narrow
// a scope. See internal/reflection/secrets.go.
//
// So every rule here needs a value: a fixed provider prefix at a realistic
// length, a complete PEM block, a labelled Cardano key field, an unbroken
// credential-length hex or base64 run, a whole word sequence that is a
// mnemonic, or a credential-shaped name assigned to a value that could not have
// been written by someone documenting the setting.
//
// A Finding names the format and nothing else. It never carries the matched
// text, because a Finding reaches the log file, the saving agent's context, and
// — for reflection — a prompt sent to a third-party model; a message quoting
// the value would relocate the secret rather than contain it.
//
// Precision is not optional. The rejection is loud and blocking, so a false
// positive trains a user to route around the control within a week, and every
// rule's length floor is chosen against the longest innocent value of the same
// shape (see longHexFloor, mnemonicWords).
package secret

import (
	_ "embed"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Finding names the credential format Detect matched. Both fields are safe to
// log, return to a caller, and put in front of a model: Rule is a stable
// machine-comparable slug, Label is a short human-readable format name.
type Finding struct {
	Rule  string
	Label string
}

// longHexFloor is the shortest unbroken run of hex characters treated as key
// material rather than as a digest.
//
// The ceiling it is chosen against is 128: SHA-512 and BLAKE2b-512 are the
// longest standard single-digest hex encodings, and a Git object id is 40. A
// run longer than 128 therefore cannot be one digest — it is key material, or
// several digests concatenated, and the second is indistinguishable from the
// first by shape. Cardano's ed25519 cold signing key is the case that sets the
// floor: its cborHex is 64 bytes plus a 4-byte CBOR header, i.e. 136
// characters, and it is routinely pasted into a memory unlabelled and unquoted.
//
// It is deliberately above 64 as well, so a bare 32-byte public key, a payment
// credential, and a stake credential stay storable: they are not secrets, and
// block producers legitimately record them.
//
// The run must also mix digit and letter classes to count at all — see
// looksLikeKeyMaterial.
const longHexFloor = 132

// mnemonicWords is the length of the mnemonic this rule looks for. BIP-39
// allows 12, 15, 18, 21 and 24 words, but 12 is excluded here on purpose: a
// run of 12 lowercase dictionary words is something an ordinary memory
// contains, while a run of 24 consecutive words drawn from a fixed 2048-word
// list is not — the chance that prose lands there is not measurable. 24 is also
// what Cardano ledger and block-production tooling use in practice. The cost is
// a documented false negative on a 12-word mnemonic.
const mnemonicWords = 24

// bip39WordCount is the length of the English BIP-39 list, and the width of the
// mnemonic this rule detects. TestBip39WordListIsTheWholeList pins it.
const bip39WordCount = 2048

//go:embed bip39_english.txt
var bip39English string

// rule is one credential format. Rules are evaluated in order and the first
// match wins, so a more specific format must precede a general one: a
// GitHub token assigned to GITHUB_TOKEN is reported as the token, not as an
// assigned secret, because the specific label is the more useful diagnostic.
type rule struct {
	name  string
	label string
	re    *regexp.Regexp
}

var rules = []rule{
	{
		name:  "age-identity",
		label: "age encryption identity (AGE-SECRET-KEY-1)",
		re:    regexp.MustCompile(`\bAGE-SECRET-KEY-1[0-9A-Z]{20,}`),
	},
	{
		name:  "anthropic-key",
		label: "Anthropic API key (sk-ant-)",
		re:    regexp.MustCompile(`\bsk-ant-[A-Za-z0-9_-]{24,}`),
	},
	{
		// The two real OpenAI key shapes, and nothing looser. The earlier
		// `sk-` + 24 or more of [A-Za-z0-9_-] accepted every hostname that
		// happened to start with sk-, which is a naming convention an
		// infrastructure team uses: sk-prod-cluster-node-01.example is a
		// service, not a key. So the project-scoped form takes base62 only — a
		// real project key has no dashes — at the length one actually has, and
		// the legacy form is pinned to its exact 48 characters with a word
		// boundary, so a longer token cannot be truncated into a match.
		name:  "openai-key",
		label: "OpenAI-style API key (sk-)",
		re:    regexp.MustCompile(`\b(?:sk-proj-[A-Za-z0-9]{40,}|sk-[A-Za-z0-9]{48})\b`),
	},
	{
		name:  "github-pat",
		label: "GitHub personal access token",
		re:    regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})`),
	},
	{
		name:  "aws-access-key-id",
		label: "AWS access key ID",
		re:    regexp.MustCompile(`\b(?:AKIA|ASIA|ABIA|ACCA)[0-9A-Z]{16}\b`),
	},
	{
		name:  "slack-token",
		label: "Slack token (xox)",
		re:    regexp.MustCompile(`\bxox[abopsr]-[A-Za-z0-9-]{10,}`),
	},
	{
		name:  "google-api-key",
		label: "Google API key (AIza)",
		re:    regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35,}`),
	},
	{
		name:  "google-oauth-token",
		label: "Google OAuth access token (ya29)",
		re:    regexp.MustCompile(`\bya29\.[0-9A-Za-z_-]{20,}`),
	},
	{
		name:  "stripe-key",
		label: "Stripe API key (sk_live_/sk_test_)",
		re:    regexp.MustCompile(`\b[srp]k_(?:live|test)_[A-Za-z0-9]{16,}`),
	},
	{
		name:  "npm-token",
		label: "npm access token (npm_)",
		re:    regexp.MustCompile(`\bnpm_[A-Za-z0-9]{30,}`),
	},
	{
		name:  "pypi-token",
		label: "PyPI upload token (pypi-AgEIcHlwaS5vcmc)",
		re:    regexp.MustCompile(`\bpypi-AgEIcHlwaS5vcmc[0-9A-Za-z_-]{40,}`),
	},
	{
		name:  "huggingface-token",
		label: "Hugging Face access token (hf_)",
		re:    regexp.MustCompile(`\bhf_[A-Za-z0-9]{30,}`),
	},
	{
		name:  "jwt",
		label: "JSON Web Token (JWT)",
		re:    regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
	},
	{
		// A real block, not a mention of one. The earlier form matched
		// `-----BEGIN … -----` through to a later `-----END … -----` anywhere in
		// the text, which caught a Snowflake help page that says "replace
		// <PRIVATE_KEY> … the file must contain the lines -----BEGIN PRIVATE
		// KEY----- and -----END PRIVATE KEY-----". So BEGIN has to end its
		// line, END has to start one, and a base64 body line has to sit between
		// them — which is what a key file is and what a sentence about a key
		// file is not.
		name:  "pem-private-key",
		label: "PEM private key block",
		re: regexp.MustCompile(
			// A real block. Three ordinary ways of pasting one, all accepted:
			// indented inside a YAML block scalar or a code fence, with CRLF line
			// endings, and with its newlines escaped the way kubectl and
			// Terraform render one in JSON.
			`(?m)^[ \t]*-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[ \t]*\r?\n` +
				`(?:[ \t]*[A-Za-z0-9+/=]{16,}[ \t]*\r?\n)+` +
				`[ \t]*-----END [A-Z0-9 ]*PRIVATE KEY-----[ \t\r]*$` +
				`|` +
				`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----\\n` +
				`(?:[A-Za-z0-9+/=]{16,}\\n)+` +
				`-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	},
	{
		name:  "putty-private-key",
		label: "PuTTY private key file (PuTTY-User-Key-File)",
		re:    regexp.MustCompile(`\bPuTTY-User-Key-File-[0-9]:`),
	},
	{
		// Cardano's operational.json, scaling.json, evol.json and vrf.json name
		// the key they hold in a "type" field, and those names are only used for
		// private material: a public or verification key file is spelled
		// KESVerificationKey / EvolVerificationKey, which this does not match.
		// The tail is [A-Za-z0-9_]* rather than [A-Za-z0-9]* because the modern
		// key files name the curve too — PaymentSigningKeyShelley_ed25519 is a
		// signing key and PlutusScriptV1 is a public script, and only one of
		// those two contains the word.
		name:  "cardano-key-file",
		label: "Cardano private key file (operational/scaling/kes/evolving key)",
		re:    regexp.MustCompile(`(?i)"type"\s*:\s*"[A-Za-z0-9_]*(?:Signing|Private)Key[A-Za-z0-9_]*"|"type"\s*:\s*"(?:KES|Evol|Scaling|VRF|Delegation)Key"`),
	},
	{
		// The other side of the same envelope: a paste that names the file it
		// came from rather than carrying the JSON type field. cardano-key-file
		// owns the type field, and without this the cborHex rule would have no
		// shape of its own — every type name it could read is one that rule
		// already reads. So this matches the filename instead: a line naming a
		// .skey file and carrying a key-sized hex run is a key paste, and a
		// public Plutus script never names a .skey file.
		//
		// The second form catches a labelled key with neither filename nor
		// "type", and it works on the wire rather than by convention: both a key
		// and a script are a CBOR byte string, so both begin 0x58 or 0x59 followed
		// by the length, and a script is large enough to need two bytes (tag
		// `59`). A 64-byte byte string — tag `5840` — is private-only, because no
		// Cardano public key is 64 bytes, so that is the unambiguous form.
		//
		// A 32-byte `5820` is deliberately NOT matched, because that is also the
		// encoding of every Cardano *public* key: cardano-cli and cardano-node
		// write exactly `{"type":"KESVerificationKey","cborHex":"5820…"}` for a
		// block production key, and a block producer has to be able to record
		// one. So a labelled 32-byte cold signing key is a documented false
		// negative, and it is the price of not refusing published verification
		// keys. Unlabelled, it is still caught: 136 hex characters is over
		// longHexFloor.
		name:  "cardano-cbor-hex",
		label: "Cardano CBOR-encoded key (cborHex beside a .skey file)",
		re: regexp.MustCompile(
			`(?i)\.skey\b[^\n]*\b[0-9a-f]{64,}` +
				`|cborhex"?\s*[:=]\s*"?5840[0-9a-f]{8,}`),
	},
	{
		name:  "authorization-bearer",
		label: "Authorization bearer token",
		re:    regexp.MustCompile(`(?i)\b(?:proxy-)?authorization\s*[:=]\s*bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	},
}

// urlCredentialsRe finds a userinfo section that carries a password — which is
// what distinguishes "postgres://ghost@host/db" (a user, no secret) from
// "postgres://ghost:pw@host/db" (a credential in a connection string, and
// exactly what gets pasted into an incident note).
//
// It is checked outside the rules table because matching it is only half the
// test: the userinfo has to be looked at, because a template carries a
// password-shaped pair of words and a leak carries a value. Eight characters is
// a floor, not a test — "PASSWORD" is eight and "changeme" is eight.
var urlCredentialsRe = regexp.MustCompile(`://[^\s/:@]+:([^\s/@]{8,})@`)

// urlCredentialIndex is the submatch holding the userinfo password.
const urlCredentialIndex = 1

// urlPasswordAllCaps matches a userinfo password written in capitals. That is
// placeholder convention — postgres://USER:PASSWORD@host/db is what a template, a
// README and a docstring all say — and a real password made entirely of
// upper-case alphanumerics is not a shape worth refusing over: every format
// worth catching here (base64, base64url, hex, an AWS secret) is mixed case,
// because that is what encoding random bytes produces.
var urlPasswordAllCaps = regexp.MustCompile(`^[A-Z0-9_]+$`)

// urlCredentialPlaceholders are the literal userinfo values a template carries,
// matched in either case.
var urlCredentialPlaceholders = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true,
	"secret": true, "token": true, "apikey": true, "api_key": true,
	"user": true, "username": true, "login": true, "admin": true,
	"root": true, "hint": true, "todo": true, "none": true, "null": true,
	"changeme": true, "change_me": true, "redacted": true, "example": true,
	"your_password": true, "my_password": true, "db_password": true,
	"the_password": true, "placeholder": true, "xxxxx": true,
}

// looksLikeURLCredential reports whether a matched userinfo password is a value
// rather than a stand-in for one.
func looksLikeURLCredential(password string) bool {
	if urlPasswordAllCaps.MatchString(password) {
		return false
	}
	return !urlCredentialPlaceholders[strings.ToLower(password)]
}

// longHexRe is checked outside the rules table for the same reason the
// assignment and URL rules are: matching it is only half the test, because the
// matched run has to be inspected before it counts.
var longHexRe = regexp.MustCompile(`\b[0-9a-fA-F]{` + strconv.Itoa(longHexFloor) + `,}\b`)

// cardanoCborHexPrefixRe is the cborHex label anchored directly against the run
// it introduces, so the exemption below can be scoped to that one run. A
// text-wide label match would disable the long-hex rule for every run in the
// save, which is how a memory mentioning a script's cborHex and carrying an
// unrelated key paste would store the key.
var cardanoCborHexPrefixRe = regexp.MustCompile(`(?i)cborhex"?\s*[:=]\s*"?[0-9a-f]*$`)

// cardanoCborHexPrefixWindow is how far back from a run the label is looked for.
// Comfortably longer than `cborHex": "` with whitespace.
const cardanoCborHexPrefixWindow = 64

// isLabelledCardanoValue reports whether the hex run starting at start is the
// value of a Cardano cborHex field.
//
// A public Plutus script is what forces the exemption. Its cborHex runs 200 to
// 1000 characters — well past longHexFloor — uses the same alphabet as a signing
// key, and is quoted in ordinary memories about a validator, a fee, a datum or a
// redeemer. A Cardano project cannot be operated without recording its scripts,
// so refusing one refuses the knowledge. A *key* in the same field is not missed,
// because cardano-cbor-hex reads the CBOR tag — see its comment.
func isLabelledCardanoValue(text string, start int) bool {
	from := start - cardanoCborHexPrefixWindow
	if from < 0 {
		from = 0
	}
	return cardanoCborHexPrefixRe.MatchString(text[from:start])
}

// looksLikeKeyMaterial reports whether a long hex run is worth refusing.
//
// A run of hex characters is only a candidate; the run also has to be hex that
// a key or a digest dump is made of, which means it must contain both a digit
// and an a-f letter. This is the same discipline internal/reflection's
// shaLikeToken applies before dropping a memory, and it earns its place: a and
// d and e and f are hex letters, so a padding or filler string built from them
// — strings.Repeat("d", 7000) in Ghost's own decision-cap test — is an
// unbroken hex run thousands of characters long. Single-class runs are also
// what a giant decimal number, a row of zeroes, and a spelled-out
// all-letters token look like. Real key material mixes the classes because it
// is base16 of random bytes.
func looksLikeKeyMaterial(run string) bool {
	var digit, letter bool
	for _, r := range run {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F'):
			letter = true
		}
	}
	return digit && letter
}

// assignmentRe finds the general case: a key token, an assignment, and a value
// long enough to be worth looking at. It deliberately does NOT try to decide
// whether the key names a secret — that is keyNamesSecret's job, and doing it
// here is what the first version got wrong.
//
// The first version required a secret-ish word anywhere in the key, and refused
// on that plus a long value. It refused a Helm chart, a Grafana provisioning
// file and a CI workflow, because all three are mostly about credentials and all
// three record the *name* of one: secretName, existingSecret, token_url,
// password_changed_at, token: secrets.GITHUB_TOKEN. So the key is captured whole
// and judged, and the value is judged separately, and BOTH have to hold.
//
// shellVarPrefix captures a `$` immediately before the key. A key that carries
// one is a shell or PowerShell *variable*. The `$` alone is not enough to skip
// the candidate — `$db_password = "K3q9Xm2pL7wRt4ZbAvN1"` is a leaked literal
// assigned to a variable and is the dangerous case — so the skip also requires
// valueIsCommand. From the same third-party corpus.
//
// The value class excludes whitespace, quotes, commas and semicolons, so a
// quoted scalar matches without its quotes and an English phrase after the colon
// never starts a match.
var assignmentRe = regexp.MustCompile(
	`(?i)(\$?)\b([a-z0-9_.-]{1,64})\s*[:=]\s*["']?([^\s"',;]{` + strconv.Itoa(assignedSecretFloor) + `,})`)

const (
	assignmentShellVar = 1
	assignmentKey      = 2
	assignmentValue    = 3
	// assignedSecretFloor is the shortest value worth looking at. It is 20
	// rather than 16 because every false positive that motivated the raise
	// landed between 16 and 19: grafana-admin-v2 is 16, wildcard-tls-2024 is 17,
	// secrets.GITHUB_TOKEN is 19. The true positives that matter are 20 and up.
	assignedSecretFloor = 20
	// longValueFloor is the length at which a value stops needing the strict
	// entropy bar. A 32-character credential is material whatever its character
	// distribution, because no description of a setting is 32 characters of
	// mixed-case alphanumerics.
	longValueFloor = 32
	// minEntropyFloor and minEntropyLoose are bits of Shannon entropy per
	// character, measured over the value's own bytes. Random base62 over 20
	// characters scores about 4.3; the placeholder-shaped false positives score
	// between 2.8 and 3.2. The gap is wide, and the thresholds sit inside it
	// rather than on either side of a case.
	minEntropyFloor = 3.5
	minEntropyLoose = 3.0
	// wordSegmentFloor is the length at which an all-lower or all-upper
	// alphabetic run inside a value is a word rather than a key. Six is where
	// "secret", "grafana" and "GITHUB" clear and "token", "tls" and "v2" do not.
	wordSegmentFloor = 6
)

// expressionChars are the characters a credential literal cannot contain and an
// expression always can. Base16, base64, base64url, hex and an alphanumeric
// password are made of letters, digits and `- _ . + / = ~`; a parenthesis or a
// dollar sign means the value is code — a call, a variable, a template
// expression — and code describes where a value lives rather than being one.
//
// The line that made this necessary came out of a third-party benchmark corpus:
// `token = PasswordResetTokenGenerator().make\_token(user)`, a code snippet
// someone pasted into a chat. A markdown-escaped `dckr\_pat\_…` also contains a
// backslash and is a real token, so a backslash is deliberately NOT in this set.
const expressionChars = "()$"

// placeholderChars are the characters that mark a value as a template rather
// than a secret: ${VAR} and {{VAR}} substitutions, <angle> placeholders, and
// the * runs that stand in for a redacted value. A config that says
// "password: ${DB_PASSWORD}" is documenting where the password comes from,
// which is the behaviour this control wants to encourage.
const placeholderChars = "${}<>*"

// urlValuePrefix matches a value that is a URL. A secret is not a URL, and
// token_url: https://oauth2.googleapis.com/token is a memory about a token
// endpoint rather than about a token.
var urlValuePrefix = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

// secretNouns are the single words that name a credential field. Absent on
// purpose: "credential", which names a role rather than a value — its concrete
// forms here are a Cardano payment or stake credential, both of which are
// published values an operator records on purpose.
var secretNouns = map[string]bool{
	"secret": true, "secrets": true,
	"password": true, "passwords": true, "passwd": true, "pwd": true,
	"token": true, "apikey": true,
}

// secretNounPairs are the two-word names a single-word list cannot express.
var secretNounPairs = map[[2]string]bool{
	{"api", "key"}: true, {"private", "key"}: true, {"secret", "key"}: true,
	{"auth", "token"}: true, {"access", "token"}: true, {"bearer", "token"}: true,
	{"encryption", "key"}: true, {"signing", "key"}: true,
}

// descriptorWords turn a credential noun into a reference to one. This is the
// half of the key test that the noun list cannot do: "secret" is a whole word in
// `secretName` and in `existingSecret` just as much as it is in
// `client_secret`, and only the presence of a descriptor word beside it says
// that the key points at a credential rather than holding one.
var descriptorWords = map[string]bool{
	"name": true, "names": true, "keyname": true, "label": true, "field": true,
	"url": true, "uri": true, "endpoint": true, "host": true, "hostname": true,
	"at": true, "ref": true, "refs": true, "reference": true,
	"path": true, "paths": true, "file": true, "files": true, "dir": true,
	"id": true, "ids": true, "type": true, "kind": true, "class": true,
	"provider": true, "issuer": true, "algorithm": true, "version": true,
	"hash": true, "len": true, "length": true, "count": true,
	"expiry": true, "expires": true, "expiration": true, "ttl": true,
	"rotation": true, "changed": true, "updated": true, "created": true,
	"existing": true, "source": true, "store": true, "backend": true,
	"location": true, "prefix": true, "pattern": true, "format": true,
	"example": true, "placeholder": true, "hint": true,
}

// keyWords splits a config key into lower-cased words on the separators a key
// uses — _, -, . — and on camelCase boundaries, so `existingSecret` yields
// [existing, secret] and `token_url` yields [token, url]. Both spellings occur
// in the same corpus: snake_case in a Helm values file, camelCase in a Java or
// Go config struct, which is why the camel boundary is inserted before
// lowercasing rather than by inspecting the already-lowered string.
func keyWords(key string) []string {
	runes := []rune(key)
	marked := make([]rune, 0, len(runes)+4)
	for i, r := range runes {
		if i > 0 && isUpper(r) {
			prev := runes[i-1]
			nextIsLower := i+1 < len(runes) && isLower(runes[i+1])
			if isLower(prev) || isDigit(prev) || (isUpper(prev) && nextIsLower) {
				marked = append(marked, '_')
			}
		}
		marked = append(marked, r)
	}
	fields := strings.FieldsFunc(string(marked), func(r rune) bool {
		return r == '_' || r == '-' || r == '.' || !isLower(r) && !isDigit(r) && !isUpper(r)
	})
	for i, f := range fields {
		fields[i] = strings.ToLower(f)
	}
	return fields
}

// keyNamesSecret reports whether a key names a credential field rather than
// pointing at one.
//
// Three tests, and any can refuse. The key must contain a secret noun as a
// whole word — `client_secret` and `api_key` pass, `notsecret` does not, because
// there the noun is part of a longer word rather than being one. The key must
// contain no descriptor word anywhere: `secretName` and `existingSecret` each
// carry `secret` as a whole word, and are refused because what they name is a
// name. And the key must not be a shell variable, which assignmentRe reports
// separately.
func keyNamesSecret(key string) bool {
	words := keyWords(key)
	for _, w := range words {
		if descriptorWords[w] {
			return false
		}
	}
	for _, w := range words {
		if secretNouns[w] {
			return true
		}
	}
	for i := 0; i+1 < len(words); i++ {
		if secretNounPairs[[2]string{words[i], words[i+1]}] {
			return true
		}
	}
	return false
}

// looksLikeCredentialMaterial reports whether an assigned value is a credential
// rather than a reference to one, a placeholder for one, a piece of code, or a
// description of where one lives.
//
// Five tests, all required:
//
//   - not a template (see placeholderChars);
//   - not a URL, because a secret is not a URL;
//   - not code: a parenthesis or a dollar sign means a call or a variable (see
//     expressionChars);
//   - not a name. A value split on its separators that contains an all-lower or
//     all-upper run of wordSegmentFloor or more letters is naming something:
//     `secrets.GITHUB_TOKEN`, `grafana-admin-v2`, `wildcard-tls-2024`. Key
//     material is base16 or base64 of random bytes and mixes case, so it does
//     not produce a six-letter single-case run;
//   - and it has to clear the length/entropy bar, which is what separates a
//     20-character random password from a 20-character word chain.
//
// key is the field the value was assigned to, because one of the reference
// shapes is only visible against it: a handler that assigns
// `accessToken: req.body.accessToken` is wiring a property, not storing one.
func looksLikeCredentialMaterial(key, value string) bool {
	if strings.ContainsAny(value, placeholderChars) {
		return false
	}
	if urlValuePrefix.MatchString(value) {
		return false
	}
	if strings.ContainsAny(value, expressionChars) {
		return false
	}
	if namesItsOwnKey(key, value) {
		return false
	}
	if hasWordSegment(value) {
		return false
	}
	if !hasTwoCharClasses(value) {
		return false
	}
	entropy := entropyPerChar(value)
	return (len(value) >= assignedSecretFloor && entropy >= minEntropyFloor) ||
		(len(value) >= longValueFloor && entropy >= minEntropyLoose)
}

// shellFlagRe is a `-Word` token, which is what a shell command's flags look
// like. Anywhere in the rest of the line, not only immediately after the value:
// `$x = ConvertTo-SecureString "{1}" -AsPlainText -Force` puts a quoted argument
// in between. That is safe because this only runs when the key is already
// `$`-prefixed, so the line is shell context and a `-Word` in it is a flag.
var shellFlagRe = regexp.MustCompile(`(?:^|\s)-[A-Za-z]`)

// valueIsCommand reports whether an assignment to a shell variable is really a
// command invocation — a flag appears later on the same line.
//
// Both corpus cases are this shape:
// `$domainAdminPassword = ConvertTo-SecureString "{1}" -AsPlainText -Force` and
// `$securePassword = ConvertTo-SecureString -String $domainPassword …`. What
// they share is not the variable but the command: a value with a `-Flag` after
// it is an argument list, and a leaked literal assigned to a variable has
// nothing after it.
func valueIsCommand(text string, valueEnd int) bool {
	rest := text[valueEnd:]
	if i := strings.IndexAny(rest, "\n\r"); i >= 0 {
		rest = rest[:i]
	}
	return shellFlagRe.MatchString(rest)
}

// commandWordRe splits a candidate command word from a path or an executable:
// two or more segments of letters only, separated by -, . or /.
var commandWordRe = regexp.MustCompile(`^[A-Za-z]+([-./][A-Za-z]+)+$`)

// commandWordSegmentMin is the shortest segment that makes a word-join a name
// rather than a random string.
//
// It is wordSegmentFloor, deliberately the same constant the value test uses, so
// the two cannot drift: a shape the value test is willing to call a name is a
// shape the skip may exempt, and anything else goes to the value test. Five
// letter groups (`XkQpz-ZmRtv-LvNbw-HcJdx`, a recovery code or a passphrase in
// dash groups) has no six-letter segment, so hasWordSegment cannot see a name in
// it and neither may this.
const commandWordSegmentMin = wordSegmentFloor

// isCommandWordShape reports whether value is a hyphen/dot/slash join of
// letters-only segments, at least one of which is long enough to be a word.
func isCommandWordShape(value string) bool {
	if !commandWordRe.MatchString(value) {
		return false
	}
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '.' || r == '/'
	}) {
		if len(segment) >= commandWordSegmentMin {
			return true
		}
	}
	return false
}

// isCommandWord reports whether a value names a command rather than a secret.
//
// The shape is the vendor's: a cmdlet or executable path is WORDS joined by
// separators, so `ConvertTo-SecureString`, `Get-SecretValue` and
// `/usr/bin/openssl` match. `K3q9Xm2pL7wRt4ZbAvN1` has no separator and has
// digits, so it does not, and `XkQpz-ZmRtv-LvNbw-HcJdx` has five letter groups of
// five — no wordSegmentFloor-sized segment, which is exactly the condition the
// value test applies, so the value test is what decides it.
//
// This exists because "is the value a literal" cannot be answered by looking for
// quotes: PowerShell allows `$x = "ConvertTo-SecureString" …`, and refusing that
// would be a false positive on exactly the corpus line the precision table
// already pins.
func isCommandWord(value string) bool {
	return isCommandWordShape(value)
}

// quotedArgRe finds a quoted token, which is where a command's secret lives.
var quotedArgRe = regexp.MustCompile(`"([^"\n]{8,})"|'([^'\n]{8,})'`)

// detectQuotedArgument reports whether the rest of a shell command line carries a
// credential in one of its quoted arguments.
//
// The key is deliberately not consulted: `$pw = ConvertTo-SecureString
// "K3q9Xm2pL7wRt4ZbAvN1" -AsPlainText -Force` names no secret in its variable, and
// the value is in the argument. Both corpus cases that this gate exists for pass
// it, because `{1}` carries placeholderChars and `$domainPassword` is not quoted.
func detectQuotedArgument(rest string) (Finding, bool) {
	// Only the same line: a flag on the next line is a different statement.
	if i := strings.IndexAny(rest, "\n\r"); i >= 0 {
		rest = rest[:i]
	}
	// The caller passes text starting at the END of the value, so a quoted value
	// leaves its own closing quote first. Left in place it pairs with the next
	// argument's opening quote and `quotedArgRe` reads
	// `"read-secret" -SecretName "K3q9…"` as one argument named ` -SecretName `
	// and never sees the credential. Dropping exactly one leading quote is what
	// the caller means.
	if strings.HasPrefix(rest, `"`) {
		rest = rest[1:]
	} else if strings.HasPrefix(rest, `'`) {
		rest = rest[1:]
	}
	for _, m := range quotedArgRe.FindAllStringSubmatch(rest, -1) {
		arg := m[1] + m[2]
		if looksLikeCredentialMaterial("", arg) {
			return Finding{Rule: assignedSecretRule, Label: assignedSecretLabel}, true
		}
	}
	return Finding{}, false
}

// namesItsOwnKey reports whether a value contains the name of the field it is
// assigned to, which means the value is a reference to that field rather than
// something stored in it.
//
// The comparison is on the alphanumerics alone, so `accessToken` and
// `req.body.accessToken` match, and so would `api-key` and `MY_API_KEY`. A
// credential is not a superset of its own field name; a property being threaded
// through a handler always ends in it. From the same third-party corpus.
func namesItsOwnKey(key, value string) bool { //nolint:revive // the value is the subject here
	strip := func(s string) string {
		var b strings.Builder
		for _, r := range strings.ToLower(s) {
			if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	needle, hay := strip(key), strip(value)
	// The name has to be the END of the value, not merely inside it. A handler
	// threading a property puts it last — `req.body.accessToken`, `ctx.token` —
	// and a chosen password that happens to repeat the field name
	// (`MyOwnPassword12345678`) does not, which is the distinction that keeps a
	// real secret from being exempted by this gate.
	return needle != "" && strings.HasSuffix(hay, needle)
}

// hasWordSegment reports whether value names something: a separator-delimited
// run of wordSegmentFloor or more letters that is entirely one case.
//
// Three exclusions, and each was a real false negative. An entirely-hex value
// is never a name (see isHex). A mixed-case run is not a word: base64 of random
// bytes produces those routinely, and `wJalrXUtnFEMI` is thirteen letters with
// no digit. And the run is measured on letters, so a segment that exists only
// because digits split a longer token is not a word either.
func hasWordSegment(value string) bool {
	// Hex is the alphabet of digests and keys, not of names. Without this the
	// letter runs inside a hex value are judged on their own — a 32-character
	// hex key splits into a 16-letter lowercase "word" and reads as a name,
	// which is a false negative on the commonest secret shape there is.
	if isHex(value) {
		return false
	}
	for _, segment := range strings.FieldsFunc(value, func(r rune) bool {
		return !isLetter(r)
	}) {
		if len(segment) < wordSegmentFloor {
			continue
		}
		allLower, allUpper := true, true
		for _, r := range segment {
			if isLower(r) {
				allUpper = false
			} else {
				allLower = false
			}
		}
		if allLower || allUpper {
			return true
		}
	}
	return false
}

// hasTwoCharClasses reports whether value draws on at least two of lower case,
// upper case and digits. A single-class run is a hostname, a flag name, a long
// filename or a spelled-out word — a note about a setting, not a key.
//
// On today's thresholds this gate is mostly redundant with the other two, and
// that is worth stating rather than rediscovering. A single-class value of
// letters is a word segment, which hasWordSegment refuses; one of digits is
// capped by the alphabet at log2(10) = 3.32 bits, under minEntropyFloor, so the
// entropy bar refuses that. The shape it alone reaches is a long run of
// punctuation, and the test suite says exactly that.
//
// It is kept anyway, because the two gates that duplicate it are thresholds
// rather than invariants: lower minEntropyFloor to 3.0 to catch a 19-character
// key, and a digits-only value starts passing with nothing left to stop it. A
// second, differently-founded condition is what makes that retune safe.
func hasTwoCharClasses(value string) bool {
	var lower, upper, digit bool
	for _, r := range value {
		switch {
		case isLower(r):
			lower = true
		case isUpper(r):
			upper = true
		case isDigit(r):
			digit = true
		}
	}
	classes := 0
	for _, present := range []bool{lower, upper, digit} {
		if present {
			classes++
		}
	}
	return classes >= 2
}

// entropyPerChar is the Shannon entropy of value's bytes, in bits per byte. It
// is the standard measure for "this looks random", and it is what separates a
// generated key from a phrase someone typed: the same length, the same
// character set, a completely different distribution.
func entropyPerChar(value string) float64 {
	if value == "" {
		return 0
	}
	var counts [256]int
	for i := 0; i < len(value); i++ {
		counts[value[i]]++
	}
	total := float64(len(value))
	entropy := 0.0
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		entropy -= p * math.Log2(p)
	}
	return entropy
}

// isHex reports whether every character of s is a hex digit. Hex is the
// alphabet of digests and keys, not of names — see hasWordSegment.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}

func isLower(r rune) bool  { return r >= 'a' && r <= 'z' }
func isUpper(r rune) bool  { return r >= 'A' && r <= 'Z' }
func isDigit(r rune) bool  { return r >= '0' && r <= '9' }
func isLetter(r rune) bool { return isLower(r) || isUpper(r) }

// Detect reports whether text contains a credential-shaped value, and which
// format it matched. It is a pure function of its input and is cheap enough to
// call on every save: text is bounded by memory.MaxContentLen and the rules are
// linear scans.
//
// Three rules are checked after the table rather than in it, because matching
// them is only half the test: longHexRe, urlCredentialsRe and assignmentRe all
// need the matched text inspected (looksLikeKeyMaterial,
// looksLikeURLCredential, keyNamesSecret + looksLikeCredentialMaterial) rather
// than a boolean. All three therefore walk EVERY candidate in the text, not the
// leftmost one. A first-match implementation is a false negative in the exact
// case this control exists for: a placeholder-shaped candidate earlier in the
// save — `${DB_PASSWORD}`, `****`, a filler run — shadows a real credential
// later in the same save, and "the env var is ${X}, the value we leaked was
// <key>" is not an adversarial ordering, it is what an incident note reads
// like.
func Detect(text string) (Finding, bool) {
	if text == "" {
		return Finding{}, false
	}
	for _, r := range rules {
		if r.re.MatchString(text) {
			return Finding{Rule: r.name, Label: r.label}, true
		}
	}
	for _, loc := range longHexRe.FindAllStringIndex(text, -1) {
		if looksLikeKeyMaterial(text[loc[0]:loc[1]]) && !isLabelledCardanoValue(text, loc[0]) {
			return Finding{Rule: longHexRule, Label: longHexLabel}, true
		}
	}
	for _, m := range urlCredentialsRe.FindAllStringSubmatch(text, -1) {
		if looksLikeURLCredential(m[urlCredentialIndex]) {
			return Finding{Rule: urlCredentialsRule, Label: urlCredentialsLabel}, true
		}
	}
	// SubmatchIndex rather than Submatch: the shell-variable test has to know
	// where the value ENDS, not how long it is, to see what follows it.
	for _, m := range assignmentRe.FindAllStringSubmatchIndex(text, -1) {
		key := text[m[2*assignmentKey]:m[2*assignmentKey+1]]
		value := text[m[2*assignmentValue]:m[2*assignmentValue+1]]
		if text[m[2*assignmentShellVar]:m[2*assignmentShellVar+1]] != "" &&
			valueIsCommand(text, m[2*assignmentValue+1]) {
			// A shell line with a flag after the value, so whatever follows is an
			// argument list. Two things have to be true at once and neither
			// replaces the other.
			//
			// The ARGUMENTS are always scanned, quoted or not: in
			// `ConvertTo-SecureString "K3q9…" -AsPlainText` the secret is the
			// argument and the value is the cmdlet, and -AsPlainText is precisely
			// the flag that says the plaintext argument IS the password.
			//
			// The VALUE is skipped only when it is command-shaped. A cmdlet or a
			// path is one whether or not it is quoted — `$x = "ConvertTo-
			// SecureString" …` is still assigning a cmdlet — whereas a quoted
			// literal is a literal however much is flagged after it:
			// `$db_password = "K3q9Xm2pL7wRt4ZbAvN1" -AsPlainText` is the secret.
			if f, ok := detectQuotedArgument(text[m[2*assignmentValue+1]:]); ok {
				return f, true
			}
			if isCommandWord(value) {
				continue
			}
		}
		if keyNamesSecret(key) && looksLikeCredentialMaterial(key, value) {
			return Finding{Rule: assignedSecretRule, Label: assignedSecretLabel}, true
		}
	}
	if hasMnemonic(text) {
		return Finding{Rule: mnemonicRule, Label: mnemonicWordsLabel}, true
	}
	return Finding{}, false
}

const (
	mnemonicRule        = "bip39-mnemonic"
	urlCredentialsRule  = "url-inline-credentials"
	urlCredentialsLabel = "URL with inline credentials"
	mnemonicWordsLabel  = "24-word BIP-39 recovery mnemonic"
	longHexRule         = "long-hex"
	longHexLabel        = "long hex blob (key material, or several digests concatenated)"
	assignedSecretRule  = "assigned-secret"
	assignedSecretLabel = "credential name assigned a value"
)

// hasMnemonic reports whether text contains mnemonicWords consecutive words
// that are all in the BIP-39 English list. Word membership does the work: the
// list is 2048 words, so the chance that 24 consecutive words of English prose
// are all on it is vanishing, while any run of real mnemonic words is.
func hasMnemonic(text string) bool {
	words := bip39Words()
	run := 0
	for _, w := range mnemonicCandidates(text) {
		if _, ok := words[w]; ok {
			run++
			if run == mnemonicWords {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// mnemonicCandidates splits text into the lower-cased words a mnemonic could
// be written as: whitespace-delimited, and stripped of surrounding punctuation
// so a memory that lists a mnemonic one per line, or wraps it in quotes or
// backticks, is read the same way. Anything with a digit or an interior
// punctuation mark is not a BIP-39 word and breaks the run, which is why
// "abandon ability able ... actual." terminates rather than extending.
func mnemonicCandidates(text string) []string {
	fields := strings.FieldsFunc(text, unicode.IsSpace)
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, strings.ToLower(strings.Trim(f, "\"'`*_.,;:()[]{}<>-–—")))
	}
	return out
}

// bip39Words parses the embedded English list once, on first use. The file is
// the BIP-39 specification's own list, unmodified; embedding it is what makes a
// run of 24 consecutive words checkable at all without a network call on a save
// path. Once, because Detect runs on every save and 2048 map inserts per call
// would be a cost with no reason to pay it twice.
var bip39Words = sync.OnceValue(func() map[string]struct{} {
	words := make(map[string]struct{}, bip39WordCount)
	for _, w := range strings.Fields(bip39English) {
		words[w] = struct{}{}
	}
	return words
})
