// Package secret detects credential-shaped values in text Ghost is about to
package secret

import (
	_ "embed"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Finding names the credential format Detect matched. Both fields are safe to
type Finding struct {
	Rule  string
	Label string
}

const longHexFloor = 132

const mnemonicWords = 24

const bip39WordCount = 2048

//go:embed bip39_english.txt
var bip39English string

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
		name:  "pem-private-key",
		label: "PEM private key block",
		re: regexp.MustCompile(
			`(?m)^[ \t]*-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[ \t]*\r?\n` +
				pemHeaders +
				`(?:[ \t]*[A-Za-z0-9+/=]{16,}[ \t]*\r?\n)+` +
				`[ \t]*-----END [A-Z0-9 ]*PRIVATE KEY-----[ \t\r]*$` +
				`|` +
				`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:\n|\\n|[ \t]+)` +
				pemHeadersLoose +
				`[A-Za-z0-9+/=]{16,}(?:\n|\\n|[ \t\r\n])+` +
				`-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	},
	{
		name:  "putty-private-key",
		label: "PuTTY private key file (PuTTY-User-Key-File)",
		re:    regexp.MustCompile(`\bPuTTY-User-Key-File-[0-9]:`),
	},
	{
		name:  "cardano-key-file",
		label: "Cardano private key file (operational/scaling/kes/evolving key)",
		re: regexp.MustCompile(
			`(?i)"type"\s*:\s*"[A-Za-z0-9_]*(?:Signing|Private)Key[A-Za-z0-9_]*"[^\n]*"?(?:cborHex|k,v)?"?\s*[:=]\s*"?[0-9a-f]{64,}` +
				`|"type"\s*:\s*"(?:KES|Evol|Scaling|VRF|Delegation)Key"[^\n]*"?(?:cborHex|k,v)?"?\s*[:=]\s*"?[0-9a-f]{64,}`),
	},
	{
		name:  "authorization-bearer",
		label: "Authorization bearer token",
		re:    regexp.MustCompile(`(?i)\b(?:proxy-)?authorization\s*[:=]\s*bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	},
}

var urlCredentialsRe = regexp.MustCompile(`://([^\s/:@]+):([^\s/@]{8,})@`)

const (
	urlCredentialUser     = 1
	urlCredentialPassword = 2
)

var urlPasswordAllCaps = regexp.MustCompile(`^[A-Z0-9_]+$`)

var urlCredentialPlaceholders = map[string]bool{
	"password": true, "passwd": true, "pass": true, "pwd": true,
	"secret": true, "token": true, "apikey": true, "api_key": true,
	"user": true, "username": true, "login": true, "admin": true,
	"root": true, "hint": true, "todo": true, "none": true, "null": true,
	"changeme": true, "change_me": true, "redacted": true, "example": true,
	"your_password": true, "my_password": true, "db_password": true,
	"the_password": true, "placeholder": true, "xxxxx": true,
	"your-password-here": true, "my-secret-here": true, "insert-here": true,
}

var urlDefaultAccounts = map[string]bool{
	"postgres": true, "mysql": true, "mariadb": true, "root": true,
	"mongo": true, "admin": true, "sa": true, "oracle": true,
	"db": true, "guest": true, "default": true, "test": true,
}

func looksLikeURLCredential(user, password string) bool {
	if strings.ContainsAny(password, placeholderChars) {
		return false
	}
	if urlPasswordAllCaps.MatchString(password) {
		return false
	}
	lower := strings.ToLower(password)
	if urlCredentialPlaceholders[lower] || urlDefaultAccounts[lower] {
		return false
	}
	return !strings.EqualFold(user, password)
}

var cardanoKeyWithContext = regexp.MustCompile(
	`(?i)\.skey\b|[A-Za-z0-9_]*(?:Signing|Private)Key[A-Za-z0-9_]*|signing[ _-]?key`)

var cardanoVerificationKey = regexp.MustCompile(`(?i)[A-Za-z0-9_-]*VerificationKey`)

var cardanoValueLabel = regexp.MustCompile(`(?i)"?(?:cborHex|k|v)"?\s*[:=]`)

const cardanoEnvelopeMax = 4096

func cardanoEnclosingObject(text string, offset int) string {
	lo := offset - cardanoEnvelopeMax
	if lo < 0 {
		lo = 0
	}
	open := -1
scan:
	for i := offset - 1; i >= lo; i-- {
		switch text[i] {
		case '{':
			open = i
			break scan
		case '}':
			return ""
		}
	}
	if open < 0 {
		return ""
	}
	end := strings.IndexByte(text[offset:], '}')
	if end < 0 {
		return ""
	}
	return text[open : offset+end+1]
}

func cardanoEnvelopeExempts(text string, offset int) bool {
	envelope := cardanoEnclosingObject(text, offset)
	if envelope == "" {
		return false
	}
	return cardanoVerificationKey.MatchString(envelope) &&
		cardanoValueLabel.MatchString(envelope)
}

const cardanoKeyMinRun = 68

var cardanoHexRunRe = regexp.MustCompile(`\b[0-9a-fA-F]{` + strconv.Itoa(cardanoKeyMinRun) + `,}\b`)

type cardanoCBORTag struct {
	bytes   int  // the decoded length
	twoByte bool // 0x59: the length is two bytes, so the value is > 255 bytes
	raw     string
}

func cardanoTagOf(run string) (cardanoCBORTag, bool) {
	if len(run) < 4 {
		return cardanoCBORTag{}, false
	}
	head := run[:2]
	hi, hiOK := hexByte(head[0], head[1])
	lo, loOK := hexByte(run[2], run[3])
	if !hiOK || !loOK {
		return cardanoCBORTag{}, false
	}
	switch hi {
	case 0x58:
		return cardanoCBORTag{bytes: int(lo), raw: run[:4]}, true
	case 0x59:
		if len(run) < 6 {
			return cardanoCBORTag{}, false
		}
		high, ok1 := hexByte(run[2], run[3])
		low, ok2 := hexByte(run[4], run[5])
		if !ok1 || !ok2 {
			return cardanoCBORTag{}, false
		}
		return cardanoCBORTag{bytes: high<<8 | low, twoByte: true, raw: run[:6]}, true
	}
	return cardanoCBORTag{}, false
}

func hexByte(a, b byte) (int, bool) {
	hi, ok1 := hexDigit(a)
	lo, ok2 := hexDigit(b)
	if !ok1 || !ok2 {
		return 0, false
	}
	return hi<<4 | lo, true
}

func hexDigit(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0'), true
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10, true
	}
	return 0, false
}

func cardanoKeyWalk(text string, li *lineIndex) (Finding, bool) {
	for _, loc := range cardanoHexRunRe.FindAllStringIndex(text, -1) {
		run := text[loc[0]:loc[1]]
		tag, ok := cardanoTagOf(run)
		if !ok {
			continue
		}
		line := li.line(loc[0])
		switch {
		case tag.twoByte:
			if tag.bytes == cardanoKESKeyBytes {
				return cardanoFinding(), true
			}
		case tag.bytes == 128:
			return cardanoFinding(), true
		case tag.bytes == 64:
			if cardanoEnvelopeExempts(text, loc[0]) {
				continue
			}
			return cardanoFinding(), true
		case tag.bytes == 32:
			if cardanoKeyWithContext.MatchString(line) {
				return cardanoFinding(), true
			}
		}
	}
	return Finding{}, false
}

const cardanoKESKeyBytes = 0x0260

func cardanoFinding() Finding {
	return Finding{Rule: cardanoCBRule, Label: cardanoCBLabel}
}

const pemHeaders = `(?:(?:Proc-Type|DEK-Info):[^\n]*\r?\n)*\r?\n?`

const pemHeadersLoose = `(?:(?:Proc-Type|DEK-Info):[^:\n]*[ \t]+)*`

var longHexRe = regexp.MustCompile(`\b[0-9a-fA-F]{` + strconv.Itoa(longHexFloor) + `,}\b`)

var cardanoCborHexPrefixRe = regexp.MustCompile(`(?i)cborhex"?\s*[:=]\s*"?[0-9a-f]*$`)

const cardanoCborHexPrefixWindow = 64

func isLabelledCardanoValue(text string, start int) bool {
	from := start - cardanoCborHexPrefixWindow
	if from < 0 {
		from = 0
	}
	return cardanoCborHexPrefixRe.MatchString(text[from:start])
}

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

var assignmentRe = regexp.MustCompile(
	`(?i)(\$?)\b([a-z0-9_.-]{1,64})\s*[:=]\s*["']?([^\s"',;]{` + strconv.Itoa(assignedSecretFloor) + `,})`)

const (
	assignmentShellVar  = 1
	assignmentKey       = 2
	assignmentValue     = 3
	assignedSecretFloor = 20
	longValueFloor      = 32
	minEntropyFloor     = 3.5
	minEntropyLoose     = 3.0
	wordSegmentFloor    = 6
)

const expressionChars = "()$"

const placeholderChars = "${}<>*"

var urlValuePrefix = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*://`)

var secretNouns = map[string]bool{
	"secret": true, "secrets": true,
	"password": true, "passwords": true, "passwd": true, "pwd": true,
	"token": true, "apikey": true,
}

var secretNounPairs = map[[2]string]bool{
	{"api", "key"}: true, {"private", "key"}: true, {"secret", "key"}: true,
	{"auth", "token"}: true, {"access", "token"}: true, {"bearer", "token"}: true,
	{"encryption", "key"}: true, {"signing", "key"}: true,
}

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
	"sha": true, "sha1": true, "sha256": true, "sha512": true, "md5": true,
	"digest": true, "checksum": true, "fingerprint": true, "etag": true,
	"printsha": true,
}

func keyWords(key string) []string {
	runes := []rune(key)
	marked := make([]rune, 0, len(runes))
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

func looksLikeCredentialMaterial(key, value string) bool {
	if strings.ContainsAny(value, placeholderChars) {
		return false
	}
	if isIdentifierPath(value) {
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

var shellFlagRe = regexp.MustCompile(`(?:^|\s)-[A-Za-z]`)

type lineIndex struct {
	text   string
	ends   []int // offset of each line's terminator, and len(text) as the last
	quoted []bool
}

func newLineIndex(text string) *lineIndex {
	ends := make([]int, 0, 8)
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\n':
			ends = append(ends, i)
		case '\r':
			ends = append(ends, i)
			if i+1 < len(text) && text[i+1] == '\n' {
				i++
			}
		}
	}
	ends = append(ends, len(text))
	return &lineIndex{text: text, ends: ends, quoted: quoteByLine(text, ends)}
}

func quoteByLine(text string, ends []int) []bool {
	out := make([]bool, len(ends))
	prev := 0
	for i, end := range ends {
		limit := end
		if limit > len(text) {
			limit = len(text)
		}
		out[i] = strings.ContainsAny(text[prev:limit], "\"'")
		prev = end + 1
	}
	return out
}

func (li *lineIndex) lineQuoted(offset int) bool {
	i := sort.SearchInts(li.ends, offset)
	if i >= len(li.quoted) {
		return false
	}
	return li.quoted[i]
}

func (li *lineIndex) rest(offset int) string {
	if offset < 0 || offset > len(li.text) {
		return ""
	}
	i := sort.SearchInts(li.ends, offset)
	if i >= len(li.ends) {
		return ""
	}
	return li.text[offset:li.ends[i]]
}

const shellStatementEnd = ";|&\n\r"

func (li *lineIndex) window(offset, windowEnd int) string {
	rest := li.rest(offset)
	if windowEnd < len(li.text) && windowEnd-offset < len(rest) {
		rest = rest[:windowEnd-offset]
	}
	if i := strings.IndexAny(rest, shellStatementEnd); i >= 0 {
		rest = rest[:i]
	}
	return rest
}

func (li *lineIndex) line(offset int) string {
	if offset <= 0 {
		return li.rest(0)
	}
	start := 0
	i := sort.SearchInts(li.ends, offset) - 1
	if i >= 0 {
		start = li.ends[i] + 1
	}
	return li.text[start:offset2end(li, offset)]
}

func offset2end(li *lineIndex, offset int) int {
	i := sort.SearchInts(li.ends, offset)
	if i >= len(li.ends) {
		return len(li.text)
	}
	return li.ends[i]
}

func valueIsCommand(li *lineIndex, valueEnd, windowEnd int) bool {
	return shellFlagRe.MatchString(li.window(valueEnd, windowEnd))
}

var commandWordRe = regexp.MustCompile(`^[A-Za-z]+([-./][A-Za-z]+)+$`)

const commandWordSegmentMin = wordSegmentFloor

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

func isCommandWord(value string) bool {
	return isCommandWordShape(value)
}

var quotedArgRe = regexp.MustCompile(`"([^"\n]{8,})"|'([^'\n]{8,})'`)

func detectQuotedArgument(li *lineIndex, offset, windowEnd int) (Finding, bool) {
	if !li.lineQuoted(offset) {
		return Finding{}, false
	}
	rest := li.window(offset, windowEnd)
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
	return needle != "" && strings.HasSuffix(hay, needle)
}

func isIdentifierPath(value string) bool {
	segments := strings.Split(value, ".")
	if len(segments) < 2 {
		return false
	}
	shortLower := false
	for _, segment := range segments {
		if segment == "" || !isIdentifier(segment) {
			return false
		}
		if len(segment) >= 3 && len(segment) <= 8 && isAllLower(segment) {
			shortLower = true
		}
	}
	return shortLower
}

func isAllLower(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 'a' || s[i] > 'z' {
			return false
		}
	}
	return true
}

func isIdentifier(s string) bool {
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func hasWordSegment(value string) bool {
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
func Detect(text string) (Finding, bool) {
	if text == "" {
		return Finding{}, false
	}
	if visible, stripped := withoutInvisibleForDetection(text); stripped {
		if f, ok := detect(visible); ok {
			return f, true
		}
	}
	return detect(text)
}

func detect(text string) (Finding, bool) {
	for _, r := range rules {
		if r.re.MatchString(text) {
			return Finding{Rule: r.name, Label: r.label}, true
		}
	}
	li := newLineIndex(text)
	for _, loc := range longHexRe.FindAllStringIndex(text, -1) {
		if looksLikeKeyMaterial(text[loc[0]:loc[1]]) && !isLabelledCardanoValue(text, loc[0]) {
			return Finding{Rule: longHexRule, Label: longHexLabel}, true
		}
	}
	if f, ok := cardanoKeyWalk(text, li); ok {
		return f, true
	}
	for _, m := range urlCredentialsRe.FindAllStringSubmatch(text, -1) {
		if looksLikeURLCredential(m[urlCredentialUser], m[urlCredentialPassword]) {
			return Finding{Rule: urlCredentialsRule, Label: urlCredentialsLabel}, true
		}
	}
	matches := assignmentRe.FindAllStringSubmatchIndex(text, -1)
	for i, m := range matches {
		windowEnd := len(text)
		if i+1 < len(matches) && matches[i+1][0] < windowEnd {
			windowEnd = matches[i+1][0]
		}
		key := text[m[2*assignmentKey]:m[2*assignmentKey+1]]
		value := text[m[2*assignmentValue]:m[2*assignmentValue+1]]
		if text[m[2*assignmentShellVar]:m[2*assignmentShellVar+1]] != "" &&
			valueIsCommand(li, m[2*assignmentValue+1], windowEnd) {
			if f, ok := detectQuotedArgument(li, m[2*assignmentValue+1], windowEnd); ok {
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
	cardanoCBRule       = "cardano-cbor-hex"
	cardanoCBLabel      = "Cardano CBOR-encoded private key (signing key by CBOR length)"
	urlCredentialsRule  = "url-inline-credentials"
	urlCredentialsLabel = "URL with inline credentials"
	mnemonicWordsLabel  = "24-word BIP-39 recovery mnemonic"
	longHexRule         = "long-hex"
	longHexLabel        = "long hex blob (key material, or several digests concatenated)"
	assignedSecretRule  = "assigned-secret"
	assignedSecretLabel = "credential name assigned a value"
)

func hasMnemonic(text string) bool {
	if len(text) < mnemonicWords*3 {
		return false
	}
	words := bip39Words()
	run := 0
	mnemonicCandidates(text, func(w string) bool {
		if _, ok := words[w]; !ok {
			run = 0
			return false
		}
		run++
		return run == mnemonicWords
	})
	return run == mnemonicWords
}

func mnemonicCandidates(text string, yield func(string) bool) {
	rest := text
	for {
		skip := strings.IndexFunc(rest, notSpace)
		if skip < 0 {
			return // all that is left is separators
		}
		rest = rest[skip:]
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			yield(mnemonicWord(rest)) // the last field; the loop is over either way
			return
		}
		if yield(mnemonicWord(rest[:end])) {
			return // the caller has what it came for
		}
		rest = rest[end:]
	}
}

func notSpace(r rune) bool { return !unicode.IsSpace(r) }

func mnemonicWord(field string) string {
	return strings.ToLower(strings.Trim(field, "\"'`*_.,;:()[]{}<>-–—"))
}

var bip39Words = sync.OnceValue(func() map[string]struct{} {
	words := make(map[string]struct{}, bip39WordCount)
	for _, w := range strings.Fields(bip39English) {
		words[w] = struct{}{}
	}
	return words
})
