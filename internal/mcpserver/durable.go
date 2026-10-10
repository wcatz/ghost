package mcpserver

import (
	"regexp"
	"strings"
)

// The deterministic half of #674: one advisory, appended to the response of a
// save whose content reads as a repository fact.
//
// The issue rules automatic rejection out of scope — "the agent decides what to
// save; Ghost only guides" — and that is what keeps a heuristic this crude
// tolerable. Two conditions must both hold INSIDE ONE SENTENCE, so the note has
// to both NAME something in the repository and CLAIM what is there, in the same
// breath: "cmd/ghost/lifecycle.go calls os.Exit, so the tier switch is
// unreachable from a test" gets no hint, because naming a file and then saying
// why it matters is durable knowledge. Same-sentence is the whole of it — a
// note-wide check pairs a predicate in one sentence with a path in another, and
// "internal/portable/portable.go holds the portable record. A dry run previews
// its own manifest." is a second copy of the durable shape that would then be
// flagged. The rule is per-sentence and not per-note, so a note whose FIRST
// sentence is a location claim is flagged whatever its later sentences add; that
// is the accepted cost, and it is a sentence of advice, not a refusal. The one
// exception is the reason connector, which is checked over the WHOLE note (#960):
// a reason can sit in a sentence of its own, and a note that carries one anywhere
// explains itself, which is the one property a repository fact cannot have.
//
// Measured residual, on the committed bench corpora (612 memories across
// internal/bench/testdata): 0 fired, down from 4/547 before the reference
// pattern below was tightened. What is left is bounded by the per-sentence rule
// above plus one thing this cannot see — a note that names a real file and
// states something about it that the code does not SAY is still a repository
// fact and will not be flagged. That is the direction the rule wants: over-
// flagging a true fact costs one sentence, under-flagging a stale one costs an
// injection slot in every later session.
//
// Even a false positive costs one sentence in a response the agent already read,
// and the response still reports the stored id, so the memory exists either way.
//
// It is a sentence in the response, not a rule in the store: nothing here can
// keep a memory out, and no future caller should be able to reach for a
// refusal on this basis.

// repoFactAdvisory is what the save response gains. It names the class of thing
// it matched rather than the token, says what a memory is for, and states its
// own effect — a warning in a save response has to be unambiguous about whether
// the write happened, or the agent re-saves and duplicates the memory.
const repoFactAdvisory = " — ADVISORY: this reads as a repository fact — it names a file, a path or a symbol and says what is in it. " +
	"The repository is authoritative for that, so the note goes stale silently and is cheap to re-read from the code. " +
	"A memory is for durable knowledge: a rule, a constraint, a decision, or a reason the code does not state. " +
	"Stored anyway — Ghost guides saves, it never refuses one on a heuristic."

// repoFactReference matches a token that locates a claim INSIDE the
// repository: a file or directory name carrying a source/config extension, or a
// call written the way code is written (`Store.Upsert(`, `HandleFoo(`).
//
// The identifier form needs its paren attached, which is what keeps ordinary
// prose out: "the schema (json)" is not a call, and a word before a space and a
// bracket never matches. The extension list is source and configuration only —
// no prose, no numbers, no hostnames — because a decimal or a version that
// matched here would fire the advisory on a memory about a port.
//
// The one requirement this pattern DOES impose is that the token be PATH-LIKE,
// because "it contains a dot" is not a property of code. An independent review
// measured five durable notes firing on Capitalised.Name tokens, four of them
// carrying a real containment predicate and none of them code:
//
//	"This rule is defined by U.S. regulators…", "Bob.Smith is where
//	escalations are routed…", "Node.js is where the build tooling lives…",
//	"Terraform.State is where locks are held", "…not in Jane.Doe's calendar"
//
// So a bare `Stem.ext` only counts when the stem is LOWERCASE (foo.go, not
// Node.js) or the token carries a directory separator (internal/foo/bar.go
// always counts, whatever its case); and a bare dotted identifier only counts
// when it looks Go-shaped, which the measured negatives are not: either a
// lowercase package qualifier with an exported name (`memory.ScopeMatches`) or
// a final segment carrying an internal capital (`Store.UpsertWithOptions`).
// Bob.Smith, Jane.Doe and U.S fail both — a personal name and an initialism
// are Capitalised.dot.Capitalised with no camelCase, and a product name is
// Capitalised.dot.lowercase. The test table pins all five.
//
// The cost of that cost: a real code reference this cannot see — a one-word
// package (`v1.Get(` is caught by the call form, but a bare `pkg.Symbol` where
// the symbol is a single word) — is missed. Missing is the right direction for
// an advisory that must not nag, and the first review round's lesson is that
// widening any branch of this pattern is what reintroduces the nags.
var repoFactReference = regexp.MustCompile(`(?:` +
	// A PATH — anything with a directory separator — ending in a code or config
	// extension: internal/foo/bar.go, cmd/ghost/lifecycle.go, web/src/app.ts.
	`\b[\w.-]*/[\w./-]*[\w-]+\.(?:go|mod|sum|py|pyi|ts|tsx|js|jsx|mjs|cjs|rs|java|kt|kts|rb|php|cs|c|h|cc|cpp|cxx|hpp|hxx|sh|bash|zsh|fish|sql|proto|ya?ml|toml|json|jsonc|ini|cfg|conf|env|tf|mk|cmake|gradle|lock)\b` +
	// A BARE filename, which only counts with a lowercase stem: foo.go, not
	// Node.js or Chart.js. Go and Rust names files in lower case, and the
	// measured false positives are all product names.
	`|\b[a-z0-9][\w-]*\.(?:go|mod|sum|py|pyi|ts|tsx|js|jsx|mjs|cjs|rs|java|kt|kts|rb|php|cs|c|h|cc|cpp|cxx|hpp|hxx|sh|bash|zsh|fish|sql|proto|ya?ml|toml|json|jsonc|ini|cfg|conf|env|tf|mk|cmake|gradle|lock)\b` +
	// A file with no extension worth naming.
	`|\b(?:Makefile|Dockerfile|Rakefile|Gemfile|Caddyfile)\b` +
	// A call written the way code is written, qualified or not.
	`|\b[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*\(` +
	// A package-qualified symbol named rather than called, in either Go shape:
	// a lowercase package with an exported name (`memory.ScopeMatches`), or a
	// final segment carrying an internal capital (`Store.UpsertWithOptions`,
	// `resolve.VetoKeep`). Both are absent from the measured negatives, which
	// are Capitalised.dot.Capitalised without camelCase (Bob.Smith, U.S,
	// Terraform.State) or Capitalised.dot.lowercase (Node.js).
	`|\b[a-z][\w]*\.[A-Z][\w]*\b` +
	`|\b[A-Za-z_][\w]*(?:\.[A-Za-z_][\w]*)*\.(?:[A-Z][a-z0-9]*[A-Z][\w]*|[A-Z]{2,})[\w]*` +
	`)`)

// repoFactPredicate matches the CLAIM half: a statement about what is AT that
// location — a containment, a definition, a declaration, or a statement of
// where the thing lives. A location phrase ("is in", "is at", "is where")
// counts, because a note saying where something lives is a repository fact
// whatever verb it uses.
//
// Deliberately LOCATION verbs only. A behavioural verb ("calls", "imports",
// "handles", "exports") is not a claim about what the repository HOLDS but
// about what code does, and a path-anchored behavioural claim is usually the
// opposite of a repository fact: "cmd/ghost/lifecycle.go calls os.Exit, so the
// tier switch is unreachable from a test and has to be pinned at the seam" is a
// note worth more than the file it names. Widening this list is how an advisory
// starts appearing on ordinary durable knowledge, which is how it stops being
// read.
var repoFactPredicate = regexp.MustCompile(`(?i)\b(?:` +
	`contains?|defines?|defined|declares?|declared|implements?|implemented|named|` +
	`lives?|resides?|located|found|` +
	`is in|is at|is where|are in|are at|are where` +
	`)\b`)

// sentenceBoundary splits on a terminator followed by whitespace, never on the
// dot inside "foo.go" or "v0.9.3" — the same dot that ends a sentence ends a
// filename, and a split there would separate the reference from the predicate
// that gives it meaning.
var sentenceBoundary = regexp.MustCompile(`[.!?:;]\s|\n`)

// repoFactReason matches the half of a note that makes it durable knowledge
// rather than a restatement: a REASON connector. A note that says why is
// carrying something the repository does not hold, whatever file it names —
// "the schema version is declared in internal/memory/schema.go, because an
// older build's store is migrated on open" is a rule with its reason, and the
// reason is the part worth keeping.
//
// The scope is the NOTE and not the sentence, which is the one place this rule
// is deliberately wider than the reference/predicate pair above. A reason can
// sit in a sentence of its own, and a note that carries one anywhere is a note
// that explains itself; the per-sentence rule exists to stop a predicate in one
// sentence pairing with a path in another, which is a different question from
// "does this note explain itself". #960 measured the cost of the narrow
// reading: two of three advisories in the save-quality audit were false
// positives on one planted convention, a rule with its reason that happens to
// name a file.
var repoFactReason = regexp.MustCompile(`(?i)\b(?:` +
	`because|so that|on purpose|never|must` +
	`)\b`)

// repoFactRule matches an imperative inside ONE sentence: the claim being
// judged is a rule, and a rule is durable knowledge whatever it names. Narrower
// than the connector list on purpose — "always" is here and not above, because
// a note that merely says "always" without saying why is not excused by the
// reason half, only by the category half below.
var repoFactRule = regexp.MustCompile(`(?i)\b(?:must|never|always)\b`)

// repoFactRuleCategory is the set of categories whose notes are rules by
// nature. A rule word only exempts a sentence filed under one of these: the
// category is the caller's own statement that this note is a rule, and a bare
// location claim filed as a `convention` is still a bare location claim.
// The note-level reason check (`repoFactReason`) covers `must` and `never`
// in every category, so only `always` reaches the category-gated rule check
// below. This design ensures that a convention or decision that merely says
// "must" or "never" is still exempt as durable knowledge, regardless of
// its category, while `always` is the only rule word that is category-gated.
var repoFactRuleCategory = map[string]bool{
	"convention": true,
	"decision":   true,
	"gotcha":     true,
}

// repoFactHint returns the advisory sentence for content that reads as a
// repository fact, or "" for everything else. It is called with the content
// the save has just stored and the category it was filed under, and it never
// decides the save.
//
// Two exemptions, both of which leave the note stored and only drop the
// sentence (#960): a note carrying a reason connector anywhere, and a sentence
// that is a rule filed under a rule category. The justification is that an
// advisory that fires on durable knowledge trains agents to ignore it; the
// false positives #960 measured were exactly that — a convention with its
// reason was flagged, and the note was worth keeping. Neither exemption can
// refuse a save.
func repoFactHint(content, category string) string {
	if repoFactReason.MatchString(content) {
		return ""
	}
	for _, sentence := range sentenceBoundary.Split(content, -1) {
		if !repoFactPredicate.MatchString(sentence) || !repoFactReference.MatchString(sentence) {
			continue
		}
		if repoFactRuleCategory[strings.ToLower(strings.TrimSpace(category))] && repoFactRule.MatchString(sentence) {
			continue
		}
		return repoFactAdvisory
	}
	return ""
}
