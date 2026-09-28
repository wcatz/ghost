// Package memref resolves the refs a caller types when it names a memory.
//
// A ref is what an operator or an agent has on screen: every Ghost report
// shortens a memory id to eight characters, so anything that takes a memory by
// name has to accept that form, and 32 characters of hex pasted out of a report
// is a shape nobody has. Several surfaces now take refs — `ghost supersede
// --withdraw` names two, `ghost resolve --mark` names N (#714) — and every one
// of them is about to change the row it names, so the rules that decide what a
// ref may mean cannot live in whichever package happened to need them first.
// They live here.
//
// The rules, and why each one is what it is:
//
//   - A ref is a LITERAL PREFIX of a memory id, matched case-insensitively
//     inside the project (plus `_global`). One query answers both forms: a full
//     id of any shape matches itself, so an id an imported artifact wrote
//     verbatim — `ghost import` preserves the ids it reads, and nothing about
//     the column says they are hex — is nameable, while 8 or more characters of
//     one is the shortest prefix that can serve as an identity.
//   - The length floor applies to a PREFIX only, and is decided by comparing
//     the match with the ref rather than by inspecting the ref's characters.
//     A ref the store holds verbatim is a full id whatever its length or shape,
//     and refusing one would be a dead end on an imported corpus.
//   - An ambiguous ref is a refusal. Which memory a caller meant when the answer
//     has two is not a decision this package may make, and a guess here changes
//     a row nobody named.
//
// The refusals name which form failed, because they fail for different reasons
// and a reader who pasted a full id needs to be told the store does not hold it,
// not that the string they pasted is malformed.
package memref

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"
)

// MinRefLen is the shortest PREFIX a caller may pass. A full id is always
// accepted whatever its length or shape, because it names one row and the store
// holds it; a prefix has to be an identity rather than a class, and below this
// length it names a large slice of any corpus. Ids are hex(randomblob(16)), so
// eight characters is the first length at which a prefix of a real corpus is
// usually one row — and a corpus where it is not is answered with the ambiguity
// listing, which says so and asks for more characters.
const MinRefLen = 8

// MaxMatches caps how many ids an ambiguity refusal names. The point of the
// list is to let the reader type more characters, which the first few entries
// are enough to do; a corpus where one 8-character prefix names hundreds of ids
// would otherwise turn a refusal into a wall of them.
const MaxMatches = 10

// Store is the one store method ref resolution needs. It is the whole of
// *memory.Store's participation here, declared as an interface so a caller can
// pass the concrete store or a narrower view of it without either side knowing
// about the other.
type Store interface {
	// MemoryIDsByIDPrefix resolves one ref to the memory ids it can mean,
	// scoped to the project (plus _global).
	MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error)
}

// Resolve turns one ref into a memory id in the project.
//
// which names the operand the ref was written for ("source", "target", "id"),
// so a refusal says which of several refs on one command line was wrong rather
// than repeating a string the reader typed more than once.
//
// A byte-exact match wins at any length. `memories.id` is a unique key, so at
// most one stored id can equal the ref — which makes the spelling the caller
// typed decisive rather than a guess, and it is what lets a short full id that
// also happens to prefix a longer one resolve rather than be refused as
// ambiguous. This is the reading that makes the repair performable: a store
// holding both "abc" and "ABC" still names ONE of them per spelling.
//
// Then a single case-folded match, for a ref spelled in a case the store does
// not hold, which is one row and so not ambiguous.
//
// Two fold matches and no byte-exact one means the ref is a THIRD casing
// ("aBc") of ids that differ only in letter case. No casing OF THIS REF reaches
// either row, which is what makes it unaddressable — the two stored spellings
// do reach them, one row each, and that is why the message LISTS them instead of
// naming a remedy: the reader's next keystroke is a copy of one of them, and a
// command name would only send them somewhere that folds the spelling away
// again. Naming a remedy also belongs to the caller — this text is returned
// verbatim to an agent that may have no shell at all.
func Resolve(ctx context.Context, store Store, projectID, which, ref string) (string, error) {
	ids, err := store.MemoryIDsByIDPrefix(ctx, projectID, ref)
	if err != nil {
		return "", err
	}
	for _, id := range ids {
		if id == ref {
			return id, nil
		}
	}
	var folded []string
	for _, id := range ids {
		if strings.EqualFold(id, ref) {
			folded = append(folded, id)
		}
	}
	switch len(folded) {
	case 0:
		// No case-insensitive match either: the prefix rules below decide.
	case 1:
		return folded[0], nil
	default:
		return "", fmt.Errorf("the %s ref %q matches %d memories whose stored ids differ only in letter case (%s), and it is spelled as neither: ids are matched case-insensitively here, so this ref cannot address either of them",
			which, ref, len(folded), strings.Join(folded, ", "))
	}
	if utf8.RuneCountInString(ref) < MinRefLen {
		// Deliberately WITHOUT the match list. A ref this short names a slice of
		// the project rather than a row, and printing that slice would turn a
		// refusal into a dump of the project's id set — the one answer here that
		// hands out what the caller could not otherwise enumerate.
		return "", fmt.Errorf("the %s ref %q is %d character(s), too short to be a prefix of an id — a prefix needs %d or more, or the full id",
			which, ref, utf8.RuneCountInString(ref), MinRefLen)
	}
	switch len(ids) {
	case 0:
		return "", fmt.Errorf("no memory in project %s has an id starting with %q (%s)", projectID, ref, which)
	case 1:
		return ids[0], nil
	}
	// Every match is named, because the answer to an ambiguity is more characters
	// and the reader has to know what to type — up to the cap, so a pathological
	// corpus cannot turn a refusal into a wall of ids.
	shown, suffix := ids, ""
	if len(shown) > MaxMatches {
		suffix = fmt.Sprintf(", and %d more", len(ids)-MaxMatches)
		shown = shown[:MaxMatches]
	}
	return "", fmt.Errorf("the %s ref %q is ambiguous in project %s: %s%s — pass more characters of the id to choose one",
		which, ref, projectID, strings.Join(shown, ", "), suffix)
}

// Short is the report's id form: the first eight CHARACTERS, which is what every
// Ghost report prints and therefore what an operator will have on screen.
//
// By characters, not bytes, and that is the whole point of the function. Ids are
// not necessarily hex — `ghost import` writes an artifact's ids verbatim — so
// `id[:8]` on a 16-byte CJK id returns the first two runes plus two bytes of the
// third: invalid UTF-8 in a report line, and a string the prefix query can never
// match even with a rune bound. The report would then print a ref that cannot
// address the row it names, which is the one thing this package exists to make
// possible.
func Short(id string) string {
	if n := utf8.RuneCountInString(id); n > MinRefLen {
		return string([]rune(id)[:MinRefLen])
	}
	return id
}
