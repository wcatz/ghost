package memory

// negationTokens are the words that invert an instruction. One of them turns a
// restatement into the opposite claim, and the token difference is so small that
// Jaccard cannot see it.
var negationTokens = map[string]bool{
	"never": true, "not": true, "no": true, "none": true, "avoid": true,
	"without": true, "disable": true, "disabled": true, "stop": true,
	"dont": true, "cannot": true,
}

// environmentQualifiers name a place. Two memories that differ only here are
// about different deployments, and folding one into the other loses whichever
// qualifier the survivor did not carry.
var environmentQualifiers = map[string]bool{
	"staging": true, "production": true, "prod": true, "dev": true,
	"development": true, "test": true, "testing": true, "preview": true,
	"localhost": true, "mainnet": true, "testnet": true,
}

// contradictoryInstruction reports whether two token sets state incompatible
// claims: a negation present in exactly one of them, an environment qualifier
// that differs, or a numeric value that differs.
//
// It is deliberately narrow and lexical. Deciding that two sentences really
// contradict needs semantics this package does not have, and a wrong "yes"
// would stop a legitimate fold — so each rule fires only on a token whose whole
// job is to carry that conflict. Ordinary prose differences ("the" vs "a",
// "release" vs nothing) are left to the similarity score, which is what
// decides whether two texts are the same claim at all.
func contradictoryInstruction(a, b map[string]bool) bool {
	if exclusiveIn(a, b, negationTokens) {
		return true
	}
	if exclusiveIn(a, b, environmentQualifiers) {
		return true
	}
	return exclusiveNumeric(a, b)
}

// exclusiveIn reports whether a token from set is present in exactly one of the
// two inputs.
func exclusiveIn(a, b, set map[string]bool) bool {
	for token := range a {
		if set[token] && !b[token] {
			return true
		}
	}
	for token := range b {
		if set[token] && !a[token] {
			return true
		}
	}
	return false
}

// exclusiveNumeric reports whether the two sets carry different numbers. The
// value itself is the claim, so a differing number is a different rule rather
// than a rewording — and prose words are excluded, which is what keeps a plain
// restatement foldable.
func exclusiveNumeric(a, b map[string]bool) bool {
	for token := range a {
		if isNumericToken(token) && !b[token] {
			return true
		}
	}
	for token := range b {
		if isNumericToken(token) && !a[token] {
			return true
		}
	}
	return false
}

func isNumericToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
