// A newer note can retire an imperative, on the repair path only (#698).
//
// The deterministic veto protects a phrase, and a phrase is not a claim. A
// status note that says "always check X", a changelog entry that says "never Y"
// and a description of a retired code path that says "must Z" all carry an
// imperative, so the veto settles them KEEP — and on the REPAIR path KEEP means
// the opposite of what it means on the ordinary one. It means the note's
// resolved_at is wrong, and this pass is about to clear it, returning a
// completed changelog to ranked session-start injection in every future session.
// On a real store that is what proposed un-hiding 143 memories, of which about
// 35% of a judged sample were stale (#698).
//
// The evidence that such a note is dated is a newer memory in the same project
// about the same thing, and the cheap deterministic evidence of "the same thing"
// is shared key identifiers: a backticked span, an #NNN reference, a file name,
// a host name. Two of them is the floor. One is a coincidence — two notes in one
// project routinely name the same file — and the floor is what keeps this from
// deferring the veto on the whole corpus.
//
// It is scoped to the repair path, and the asymmetry is the reason. On the
// ordinary pass a false veto costs one noisy memory in the ranked surface: it
// stays visible, stays searchable, costs one injection slot, and a later pass
// can still bury it. On the repair pass the same false veto returns a stale
// note to injection permanently, in every session, with nothing left to bury it
// again. So the repair path pays a classifier call for the ambiguous case and
// the ordinary pass does not. The hide direction, the veto's own vocabulary, and
// supersede's reading of the same words are all untouched.
//
// The pool read is ResolveCandidates — the project's unresolved, unpinned,
// non-exempt memories — for the same reason the correction floor reads it and
// not the resolved pool: a memory that has itself been resolved is a row the
// next ordinary pass will not see, and a veto that deferred to it would send
// notes to a paid classifier call on the strength of evidence nothing will act
// on. A pinned note or a convention is excluded for the same reason resolve
// never changes those rows.
package resolve

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/wcatz/ghost/internal/memory"
)

// minSharedIdentifiers is how many key identifiers two notes must share before
// a newer one counts as evidence that the older is dated. Two, not one: in any
// real project two notes naming the same file is a coincidence several times a
// day, and a rule that fired on that would defer the veto on most of a corpus
// and hand the whole repair to the classifier.
const minSharedIdentifiers = 2

var (
	// backtickSpanRe pulls the body of a code span. A note that names a command,
	// a flag or a path in backticks has named the thing it is about, and no
	// other class in this file can see a flag at all.
	backtickSpanRe = regexp.MustCompile("`([^`\n]+)`")

	// issueRefRe pulls an #NNN reference. Ghost's own vocabulary: a changelog
	// entry, a PR locator and a review comment all name the work this way, and
	// two notes naming the same issue are about the same work. Keyed on the
	// digits, so "#698" and "698" are one identifier while a bare number
	// elsewhere in the text is not one at all.
	issueRefRe = regexp.MustCompile(`#(\d+)`)

	// identifierTokenRe matches the runs of characters a path, a host or a file
	// name can be made of. Everything else separates, so a path keeps its
	// slashes and a sentence keeps its words.
	identifierTokenRe = regexp.MustCompile(`[A-Za-z0-9][A-Za-z0-9_./+-]*`)
)

// keyIdentifiers returns the identifiers that carry a note's subject: its
// backticked spans, its #NNN references, its file names (by base name, so
// internal/resolve/reassess.go and reassess.go are one identifier) and its host
// names. Compared lowercased throughout — a path or a host restated in a
// different case is the same subject, not a new one.
//
// ONE identifier per thing, and that is the invariant the two-identifier floor
// rests on. A file named by its full path is a path, not a host (a host has no
// separator in it) and not a second identifier beside its own base name — either
// would let one shared file satisfy a floor of two, which is the coincidence the
// floor exists to reject. A backticked span that IS a path, a host or an #NNN
// reference is that same thing, so the class rules canonicalize it and the raw
// span adds nothing (carriesCanonicalIdentifier).
//
// Ordinary prose is deliberately not a class, and neither is a bare number or a
// version: those are the things that change when a note goes stale, so a rule
// that counted them would call every dated note the same subject as every other
// dated note.
func keyIdentifiers(content string) map[string]bool {
	canonical := make(map[string]bool)
	for _, tok := range identifierTokenRe.FindAllString(content, -1) {
		// A trailing full stop or comma is sentence punctuation, not part of
		// the name: "see reassess.go." and "see reassess.go" are one subject.
		tok = strings.TrimRight(tok, ".,")
		if tok == "" {
			continue
		}
		if base, ok := fileBaseName(tok); ok {
			canonical[strings.ToLower(base)] = true
			continue
		}
		if isHostToken(tok) {
			canonical[strings.ToLower(tok)] = true
		}
	}
	found := make(map[string]bool, len(canonical)+2)
	for id := range canonical {
		found[id] = true
	}
	for _, match := range backtickSpanRe.FindAllStringSubmatch(content, -1) {
		span := strings.TrimSpace(match[1])
		if span == "" {
			continue
		}
		// A span that already IS one of the other classes is that same thing,
		// under the one name that class gives it; adding the raw text beside it
		// is what would make one file or one issue reference worth two
		// identifiers — and a floor of two is what keeps one coincidence from
		// deferring the veto. The #NNN pass below runs over the whole content,
		// backticks included, so the inner reference is already in the set.
		if carriesCanonicalIdentifier(span) {
			continue
		}
		found[strings.ToLower(span)] = true
	}
	for _, ref := range issueRefRe.FindAllStringSubmatch(content, -1) {
		found[ref[1]] = true
	}
	return found
}

// carriesCanonicalIdentifier reports whether a backticked span is already
// represented by one of the other classes: a file name, a host name or an #NNN
// reference. All three are read from the whole text, so a span that holds one of
// them adds nothing of its own.
func carriesCanonicalIdentifier(span string) bool {
	if _, ok := fileBaseName(span); ok {
		return true
	}
	if isHostToken(span) {
		return true
	}
	return issueRefRe.MatchString(span)
}

// subjectNote is one pool memory's subject: the field that decides freshness
// and the identifiers that decide sameness. The id is the map key.
type subjectNote struct {
	updated     string
	identifiers map[string]bool
}

// subjectIndex is the pool a repair pass compares against: every unresolved
// memory's key identifiers, keyed by memory id, built once per pass so a pool
// of a few hundred notes is tokenized once rather than once per judged note.
type subjectIndex map[string]subjectNote

func indexSubjects(mems []memory.Memory) subjectIndex {
	idx := make(subjectIndex, len(mems))
	for _, m := range mems {
		idx[m.ID] = subjectNote{updated: m.UpdatedAt, identifiers: keyIdentifiers(m.Content)}
	}
	return idx
}

// shadowing reports the id of a newer note in the pool that shares at least
// minSharedIdentifiers of m's key identifiers, and how many it shares.
//
// The freshness test is the same isOlder the correction pairing uses, ties
// broken by id, so "newer" means one thing in this package: a note written
// before the one that replaces it is not evidence that it is stale, and a note
// written after it is.
func (idx subjectIndex) shadowing(m memory.Memory) (id string, shared int, ok bool) {
	mine := keyIdentifiers(m.Content)
	if len(mine) < minSharedIdentifiers {
		return "", 0, false
	}
	for otherID, other := range idx {
		if !isOlder(m, memory.Memory{ID: otherID, UpdatedAt: other.updated}) {
			continue
		}
		n := 0
		for ident := range mine {
			if other.identifiers[ident] {
				n++
			}
		}
		if n >= minSharedIdentifiers && (!ok || otherID < id) {
			// The lowest id wins among equally good matches, so the log line
			// does not depend on map order.
			id, shared, ok = otherID, n, true
		}
	}
	return id, shared, ok
}

// isHostToken reports a dotted name whose last label is alphabetic and at least
// two characters: node-3.example.com, mr-slave.example.net. The length floor
// keeps the abbreviations prose is full of ("e.g", "i.e") out — a host rule that
// flagged them would match half a corpus. A purely numeric tail is a version or
// an address, not a host, which also keeps 1.2.3 out.
//
// It is the SECOND shape a token can have, never the first: keyIdentifiers asks
// fileBaseName before it asks this, and a file that also answered here would
// contribute its whole path and its base name as two identifiers — which is one
// shared file satisfying a floor of two on its own. Nothing outside this file
// has to know that ordering, and nothing else may ask the two questions in the
// other order.
func isHostToken(tok string) bool {
	parts := strings.Split(tok, ".")
	if len(parts) < 2 || len(parts[len(parts)-1]) < 2 || !isAlpha(parts[len(parts)-1]) {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	return true
}

// fileBaseName returns the base name of a token that ends in a file extension,
// and whether it has one. Only the final segment's last dot can be the
// extension: "v1.2" is a version and a directory named pkg.io inside a longer
// path must not supply one. The extension needs two to eight alphanumerics,
// which is what separates reassess.go from and/or and read/write.
func fileBaseName(tok string) (string, bool) {
	base := tok
	if idx := strings.LastIndex(tok, "/"); idx >= 0 {
		base = tok[idx+1:]
	}
	idx := strings.LastIndex(base, ".")
	if idx < 0 {
		return "", false
	}
	ext := base[idx+1:]
	if len(ext) < 2 || len(ext) > 8 || !isAlphaNum(ext) {
		return "", false
	}
	return base, true
}

func isAlpha(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func isAlphaNum(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}
