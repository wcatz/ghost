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
		name:  "openai-key",
		label: "OpenAI-style API key (sk-)",
		re:    regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9_-]{24,}`),
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
		// The BEGIN and END markers are both required, and the body is matched
		// across newlines. A lone BEGIN line is how a memory refers to a key
		// ("the node reads PRIVATE_KEY=-----BEGIN PRIVATE KEY----- from
		// disk"), and refusing that would be refusing the documentation of
		// where the key lives. A block is a key.
		name:  "pem-private-key",
		label: "PEM private key block",
		re:    regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
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
		// The label is the reason this rule is not a keyword heuristic — the
		// word "key" appears in every Cardano memory, the type name does not.
		name:  "cardano-key-file",
		label: "Cardano private key file (operational/scaling/kes/evolving key)",
		re:    regexp.MustCompile(`(?i)"type"\s*:\s*"[A-Za-z]*(?:Signing|Private)Key"|"type"\s*:\s*"(?:KES|Evol|Scaling|VRF|Delegation)Key"`),
	},
	{
		// A cardano-cli signing key file is cborHex and nothing else, so the
		// field name is the only signal available. The floor sits above a
		// payment or stake credential (28 bytes, 56 hex) and a bare public key
		// (32 bytes, 64 hex), which are published values, and below a 64-byte
		// private scalar (128 hex) — the size every Cardano secret key has.
		name:  "cardano-cbor-hex",
		label: "Cardano CBOR-encoded key (cborHex)",
		re:    regexp.MustCompile(`(?i)cborhex"?\s*[:=]\s*"?[0-9a-f]{96,}`),
	},
	{
		name:  "authorization-bearer",
		label: "Authorization bearer token",
		re:    regexp.MustCompile(`(?i)\b(?:proxy-)?authorization\s*[:=]\s*bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	},
	{
		// A userinfo section carrying a colon. "postgres://ghost@host/db" has a
		// user and no password and is left alone; "postgres://ghost:pw@host/db"
		// is a credential in a connection string, which is exactly what ends up
		// pasted into an incident note. The eight-character floor on the password
		// keeps a bare "scheme://a:b@host" from counting, which is a URL nobody
		// writes and a shape some prose reaches.
		name:  "url-inline-credentials",
		label: "URL with inline credentials",
		re:    regexp.MustCompile(`://[^\s/:@]+:[^\s/@]{8,}@`),
	},
}

// assignedSecretValue is the index of the assigned value in
// assignedSecretRe's submatches, and assignedSecretFloor is the shortest value
// its pattern accepts.
const (
	assignedSecretValue = 1
	// A value has to be at least this long to be a credential rather than a
	// word: 16 is past the length of every spelling of "changeme" and every
	// "example-secret" a config or a doc reaches for, and short of none of the
	// real formats above.
	assignedSecretFloor = 16
)

// longHexRe is checked outside the rules table for the same reason
// assignedSecretRe is: matching it is only half the test, because the matched
// run has to be inspected before it counts.
var longHexRe = regexp.MustCompile(`\b[0-9a-fA-F]{` + strconv.Itoa(longHexFloor) + `,}\b`)

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

// assignedSecretRe is the general case: a credential-shaped name, an
// assignment, and a value long and character-class-rich enough that it could
// not be prose about the setting. Those three requirements are what separate
// "client_secret: rotate this value" from a leak, and none of them is a keyword
// alone. The value floor is assignedSecretFloor, applied in the pattern.
//
// The value class excludes whitespace, quotes, commas and semicolons, so a
// quoted scalar matches without its quotes and an English phrase after the
// colon never starts a match. The remaining shape test is in
// looksAssignedSecret.
//
// "credential" is deliberately absent from the name list while "secret",
// "password", "token" and "apikey" are present, and the asymmetry is the point.
// Those name a value; "credential" names a role. Its concrete forms in this
// domain are public — a Cardano payment or stake credential, a wallet
// credential — and those are exactly the values an operator records. A
// provider's own secret field still matches, because AWS's is
// aws_secret_access_key, Stripe's is sk_live_, GitHub's is ghp_: the "secret"
// and the provider prefix reach what the bare word cannot.
var assignedSecretRe = regexp.MustCompile(
	`(?i)\b[a-z0-9_.-]*(?:api[_-]?key|apikey|secret|password|passwd|pwd|token|` +
		`private[_-]?key|auth[_-]?token|access[_-]?token)[a-z0-9_.-]*` +
		`\s*[:=]\s*["']?([^\s"',;]{` + strconv.Itoa(assignedSecretFloor) + `,})`)

// placeholderChars are the characters that mark a value as a template rather
// than a secret: ${VAR} and {{VAR}} substitutions, <angle> placeholders, and
// the * runs that stand in for a redacted value. A config that says
// "password: ${DB_PASSWORD}" is documenting where the password comes from,
// which is the behaviour this control wants to encourage.
const placeholderChars = "${}<>*"

// Detect reports whether text contains a credential-shaped value, and which
// format it matched. It is a pure function of its input and is cheap enough to
// call on every save: text is bounded by memory.MaxContentLen and the rules are
// linear scans.
//
// Two rules are checked after the table rather than in it, because matching
// them is only half the test: longHexRe and assignedSecretRe both need the
// matched text inspected (looksLikeKeyMaterial, looksAssignedSecret) rather
// than a boolean. Each is evaluated once, no matter how many candidates the
// text contains.
func Detect(text string) (Finding, bool) {
	if text == "" {
		return Finding{}, false
	}
	for _, r := range rules {
		if r.re.MatchString(text) {
			return Finding{Rule: r.name, Label: r.label}, true
		}
	}
	if m := longHexRe.FindString(text); m != "" && looksLikeKeyMaterial(m) {
		return Finding{Rule: longHexRule, Label: longHexLabel}, true
	}
	if m := assignedSecretRe.FindStringSubmatch(text); m != nil {
		if looksAssignedSecret(m[assignedSecretValue]) {
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

// looksAssignedSecret reports whether an assigned value is a value rather than
// a description. The regex has already established a credential-shaped name and
// a value of at least assignedSecretFloor characters; two tests remain:
//
//   - it is not a template (see placeholderChars);
//   - it draws on at least two of the three character classes, so a
//     single-class run of letters or digits — a hostname, a flag name, a long
//     filename, a spelled-out word — is a note about a setting, not a key.
func looksAssignedSecret(value string) bool {
	if strings.ContainsAny(value, placeholderChars) {
		return false
	}
	var lower, upper, digit bool
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
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
