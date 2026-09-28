// Package memref resolves the refs a caller types when it names a memory.
//
// A ref is what an operator or an agent has on screen: every Ghost report
// shortens a memory id to eight characters, so anything that takes a memory by
// name has to accept that form, and 32 characters of hex pasted out of a report
// is a shape nobody has. Several surfaces now take refs — `ghost supersede
// --withdraw` names two, `ghost resolve --mark` names N (#714), `ghost history`
// names one (#720) — so the rules that decide what a ref may mean cannot live in
// whichever package happened to need them first. They live here.
//
// Two entry points, one set of rules. `Resolve` reads a PROJECT's live ids and is
// for a caller about to change a row it names, where the project is both the
// scope and something the refusal must name. `ResolveIn` takes the caller's own id
// set, for a caller whose set is not that — `ghost history` resolves against every
// id `memory_history` records plus every id still live, because the question it is
// asked is most often about a memory that has been deleted and the tombstone is in
// the history table alone. The difference is WHICH ids, never how a ref is judged.
//
// The rules, and why each one is what it is:
//
//   - A ref is a LITERAL PREFIX of a memory id, matched case-insensitively
//     inside the caller's id set. One query answers both forms: a full id of any
//     shape matches itself, so an id an imported artifact wrote verbatim —
//     `ghost import` preserves the ids it reads, and nothing about the column
//     says they are hex — is nameable, while 8 or more characters of one is the
//     shortest prefix that can serve as an identity.
//   - The length floor applies to a PREFIX only, and is decided by comparing
//     the match with the ref rather than by inspecting the ref's characters.
//     A ref the store holds verbatim is a full id whatever its length or shape,
//     and refusing one would be a dead end on an imported corpus.
//   - An ambiguous ref is a refusal. Which memory a caller meant when the answer
//     has two is not a decision this package may make, and a guess here either
//     changes a row nobody named or prints one memory's recorded text as though it
//     were another's.
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

// FullIDLen is how many CHARACTERS long the id Ghost mints: hex(randomblob(16)).
//
// It is NOT a rule about what an id may be — `ghost import` writes an artifact's
// ids verbatim, so a store can hold ids of any length and this package accepts a
// full id of any shape (see the floor above, which is why the two constants
// disagree in purpose rather than in value). It answers one narrower question, and
// only a caller facing a MISS needs to ask it: a ref shorter than this could be a
// TRUNCATION of an id, so "no id starts with what you typed" is the useful answer
// and "this memory was never written" is the misleading one; a ref at least this
// long is not a truncation of anything, so the miss is about the id itself and the
// caller's own report of it is the better answer.
//
// A caller must not turn it into a test. What decides whether a ref IS a whole id
// is whether the store holds it — a store full of imported notes holds
// eight-character ids, and those eight characters are whole — so membership is the
// test and this constant is only ever the boundary between two honest sentences
// about a miss.
const FullIDLen = 32

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
	return resolveIn(ids, projectID, which, ref)
}

// ResolveIn turns one ref into one of the ids the CALLER already holds. It is
// Resolve's rules — the floor, the byte-exact and fold precedence, the refusal to
// choose between two matches, the refusal to list a match set for a ref below the
// floor — with the id set as an argument instead of a project-scoped read.
//
// The split is about WHICH ids, never about how a ref is judged. A caller's set
// here is not "this project's live memories": `ghost history` (#720) resolves
// against every id memory_history records plus every id still live, because a
// history lookup is most often asked about a memory that has been DELETED, and
// the tombstone is in the history table alone. Copying the rules for that set
// would be the failure this package exists to prevent — two implementations
// eventually disagree about which id one spelling addresses, and the disagreement
// shows up as a command that names a different memory than the one the operator
// read out of a report.
//
// There is no scope argument, and that is the same decision `Resolve`'s callers
// make rather than a limitation: a caller whose set is confined to a project reads
// it THROUGH a store and calls Resolve, so its refusals name the project they
// searched. A caller's set here is not confined to one, and a refusal naming a
// project would be a claim about a scope that is false — and the kind of claim a
// reader acts on, going looking in a project the set never searched. A caller that
// holds a set it CAN describe is describing it wrong, and the way to fix that is a
// scoped read.
func ResolveIn(ids []string, which, ref string) (string, error) {
	return resolveIn(ids, "", which, ref)
}

// resolveIn holds the rules, once, for both entry points: Resolve reads the
// project's ids and hands them here with that project as the scope its refusals
// name, and ResolveIn hands over the caller's unscoped set. Every comment in the
// body below is about the rules rather than about where the ids came from, because
// neither caller may decide one of them differently.
//
// The prefix match is applied HERE rather than trusted from the caller, and that is
// load-bearing for ResolveIn. `Resolve`'s store query is already a prefix filter,
// so filtering its answer again is a no-op; a caller that collected its ids for
// some other reason is not obliged to have filtered them, and treating an
// unfiltered set as a match set would resolve a ref to a memory that does not
// begin with it. One rule in one place, then: `ids` is every id the caller can
// name, and which of them this ref means is decided here.
func resolveIn(ids []string, scope, which, ref string) (string, error) {
	matches := make([]string, 0, len(ids))
	for _, id := range ids {
		if hasPrefixFold(id, ref) {
			matches = append(matches, id)
		}
	}
	for _, id := range matches {
		if id == ref {
			return id, nil
		}
	}
	var folded []string
	for _, id := range matches {
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
		// the set rather than a row, and printing that slice would turn a
		// refusal into a dump of the ids — the one answer here that hands out
		// what the caller could not otherwise enumerate. A caller's set can be
		// the whole store rather than one project, which is a larger thing to
		// dump, not a smaller one.
		return "", fmt.Errorf("the %s ref %q is %d character(s), too short to be a prefix of an id — a prefix needs %d or more, or the full id",
			which, ref, utf8.RuneCountInString(ref), MinRefLen)
	}
	switch len(matches) {
	case 0:
		// The phrase carries its own preposition so the sentence reads the same
		// whether there is a scope or not: "no memory has an id starting with"
		// against "no memory in project p has an id starting with". A separate
		// "project %s" placeholder would leave a double space in the second case
		// and read as a hole in the sentence.
		return "", fmt.Errorf("no memory%s has an id starting with %q (%s)", inProject(scope), ref, which)
	case 1:
		return matches[0], nil
	}
	// Every match is named, because the answer to an ambiguity is more characters
	// and the reader has to know what to type — up to the cap, so a pathological
	// corpus cannot turn a refusal into a wall of ids.
	shown, suffix := matches, ""
	if len(shown) > MaxMatches {
		suffix = fmt.Sprintf(", and %d more", len(matches)-MaxMatches)
		shown = shown[:MaxMatches]
	}
	return "", fmt.Errorf("the %s ref %q is ambiguous%s: %s%s — pass more characters of the id to choose one",
		which, ref, inProject(scope), strings.Join(shown, ", "), suffix)
}

// inProject is the prepositional phrase a refusal uses for WHERE it looked, so a
// caller whose set is not confined to a project is described rather than left to
// invent one. It carries its own "in" and its own leading space, because a
// placeholder that only held the project id would leave "no memory  has an id
// starting with" — a double space, which is how a missing clause announces itself
// in output a caller is reading for meaning.
//
// The empty string is the answer for an unscoped set, and it claims nothing about a
// project the set never searched. That matters: a reader told a memory is not in
// some project will go and look there.
func inProject(scope string) string {
	if scope == "" {
		return ""
	}
	return " in project " + scope
}

// hasPrefixFold is the case-insensitive prefix test, measured in CHARACTERS on both
// sides so it agrees with the store's `substr(id, 1, runeCount(ref))` bound: a
// byte-counted test on a multi-byte id compares the wrong length and reports a
// match, or misses one, for a ref the store would have answered. It exists because
// resolveIn applies the match itself rather than trusting the caller's set, and a
// copy of this rule is what a second copy of the whole rule looks like.
func hasPrefixFold(id, ref string) bool {
	ir, rr := []rune(id), []rune(ref)
	if len(ir) < len(rr) {
		return false
	}
	return strings.EqualFold(string(ir[:len(rr)]), ref)
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
