package memref

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// fakeStore answers one prefix query from a fixed id set, which is all Resolve
// asks of the store. It records the project it was asked about so a test can pin
// the scope, and it is deliberately a fake rather than a real store: these are
// the rules, and the query that feeds them is pinned by the callers' tests
// against a real database.
type fakeStore struct {
	ids     []string
	project string
	calls   int
}

func (f *fakeStore) MemoryIDsByIDPrefix(_ context.Context, projectID, prefix string) ([]string, error) {
	f.calls++
	f.project = projectID
	var out []string
	for _, id := range f.ids {
		if strings.HasPrefix(strings.ToLower(id), strings.ToLower(prefix)) {
			out = append(out, id)
		}
	}
	return out, nil
}

// hexIDs builds n ids sharing a prefix, which is what makes a prefix ambiguous.
func hexIDs(prefix string, n int) []string {
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, prefix+string(rune('1'+i))+strings.Repeat("0", 23))
	}
	return out
}

// TestResolveAcceptsPrefixesAndFullIDs: the operator reads an id out of a report,
// and every Ghost report shortens it to eight characters. A command that only
// accepted a full 32-character id could not be driven from the report that says
// which memory is wrong.
func TestResolveAcceptsPrefixesAndFullIDs(t *testing.T) {
	ids := hexIDs("a1b2c3d4", 1)
	store := &fakeStore{ids: ids}

	for _, ref := range []string{ids[0], ids[0][:8], ids[0][:16], strings.ToUpper(ids[0][:8]), strings.ToUpper(ids[0])} {
		t.Run(ref, func(t *testing.T) {
			got, err := Resolve(context.Background(), store, "p", "id", ref)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", ref, err)
			}
			if got != ids[0] {
				t.Errorf("Resolve(%q) = %q, want %q", ref, got, ids[0])
			}
		})
	}
}

// TestResolveRefusesAnAmbiguousPrefix: a prefix naming two memories is a question
// with two answers, and the row it would change is not something to guess at. The
// refusal LISTS the matches so the reader can add characters, and it says how.
func TestResolveRefusesAnAmbiguousPrefix(t *testing.T) {
	ids := hexIDs("aaaaaaaa", 2)
	store := &fakeStore{ids: ids}

	_, err := Resolve(context.Background(), store, "p", "id", "aaaaaaaa")
	if err == nil {
		t.Fatal("Resolve accepted an ambiguous prefix")
	}
	for _, want := range append(append([]string{}, ids...), "more") {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not contain %q: %v", want, err)
		}
	}
	if !strings.Contains(err.Error(), "p") {
		t.Errorf("the refusal does not name the project it searched: %v", err)
	}
}

// TestResolveRefusesAShortRefWithoutListingTheProject: a ref below the floor
// names a slice of the project rather than a row, and printing that slice would
// hand out the very thing the caller could not otherwise enumerate. This is the
// one refusal that deliberately carries no id list.
func TestResolveRefusesAShortRefWithoutListingTheProject(t *testing.T) {
	// Every id shares "a", so a match set large enough to dump is available.
	ids := hexIDs("a", 12)
	store := &fakeStore{ids: ids}

	_, err := Resolve(context.Background(), store, "p", "id", "a")
	if err == nil {
		t.Fatal("Resolve accepted a one-character ref")
	}
	if !strings.Contains(err.Error(), "too short to be a prefix") {
		t.Errorf("the refusal does not say the ref is too short: %v", err)
	}
	if !strings.Contains(err.Error(), "character(s)") {
		t.Errorf("the refusal does not count the ref in characters: %v", err)
	}
	for _, id := range ids {
		if strings.Contains(err.Error(), id) {
			t.Errorf("the refusal for a too-short ref listed %s, turning it into a dump of the project's ids", id)
		}
	}
}

// TestResolveCountsRefLengthInCharactersNotBytes: an id is not necessarily hex —
// `ghost import` writes an artifact's ids verbatim — so a floor measured in bytes
// would accept four CJK characters as a "prefix" and reject eight of them, and a
// report that shortened an id in bytes would print a ref the query can never
// match. The two lengths agree for the hex ids Ghost mints, which is exactly why
// a byte-counted floor would survive a normal test.
func TestResolveCountsRefLengthInCharactersNotBytes(t *testing.T) {
	// Eight CJK characters: 24 bytes, 8 runes. A byte-counted floor would refuse
	// this and accept a shorter one, in both directions.
	cjk := "日本語のメモ本文です" // 9 runes
	store := &fakeStore{ids: []string{cjk}}
	if got := utf8.RuneCountInString(cjk); got < MinRefLen {
		t.Fatalf("the fixture is %d runes, which does not clear the floor", got)
	}
	if len(cjk) == utf8.RuneCountInString(cjk) {
		t.Skip("the fixture has no multi-byte runes, so byte length and rune count agree")
	}
	got, err := Resolve(context.Background(), store, "p", "id", cjk)
	if err != nil {
		t.Fatalf("Resolve on a full multi-byte id: %v", err)
	}
	if got != cjk {
		t.Errorf("Resolve = %q, want %q", got, cjk)
	}
	// And the SHORT form of it: the first eight runes, which is what Short
	// prints, must still resolve.
	prefix := string([]rune(cjk)[:MinRefLen])
	if got, err := Resolve(context.Background(), store, "p", "id", prefix); err != nil {
		t.Errorf("Resolve on the eight-rune prefix %q: %v", prefix, err)
	} else if got != cjk {
		t.Errorf("Resolve(%q) = %q, want %q", prefix, got, cjk)
	}
	// One rune short of the floor is refused as a PREFIX — but this id is shorter
	// than the floor and the store holds it verbatim, so the full-id reading wins
	// and it resolves. That is the point of deciding the two forms by comparing
	// the match with the ref rather than by inspecting the ref's characters.
	short := string([]rune(cjk)[:MinRefLen-1])
	store2 := &fakeStore{ids: []string{short}}
	if got, err := Resolve(context.Background(), store2, "p", "id", short); err != nil {
		t.Errorf("Resolve on a stored id shorter than the floor: %v", err)
	} else if got != short {
		t.Errorf("Resolve = %q, want %q", got, short)
	}

	// The other direction, and the one a byte-counted floor gets wrong in the way
	// that matters: three CJK characters are NINE bytes, so a byte check clears
	// an 8-byte floor with a ref that is a third of an identity. It is a PREFIX
	// here (it matches, but does not equal, the stored id), so the floor has to
	// refuse it — and to say why. Without this the byte-counted floor resolves a
	// three-character ref to a unique row, and the same test above passes because
	// a full multi-byte id is byte-long enough to clear either floor.
	tooShort := string([]rune(cjk)[:3])
	store3 := &fakeStore{ids: []string{cjk}}
	_, floorErr := Resolve(context.Background(), store3, "p", "id", tooShort)
	if floorErr == nil {
		t.Fatalf("Resolve accepted %q (%d bytes, %d characters) as a prefix", tooShort, len(tooShort), utf8.RuneCountInString(tooShort))
	}
	if !strings.Contains(floorErr.Error(), "too short to be a prefix") {
		t.Errorf("a three-character ref was refused for the wrong reason: %v", floorErr)
	}
}

// TestResolveRefusesAThirdCasingOfTwoIds: ids that differ only in letter case are
// each reachable by their own stored spelling, and a ref spelled as neither
// reaches neither. The message LISTS the two rather than naming a remedy, because
// the reader's next keystroke is a copy of one of them.
func TestResolveRefusesAThirdCasingOfTwoIds(t *testing.T) {
	lower := "abcdef01" + strings.Repeat("0", 24)
	upper := "ABCDEF01" + strings.Repeat("0", 24)
	store := &fakeStore{ids: []string{lower, upper}}

	_, err := Resolve(context.Background(), store, "p", "id", "Abcdef01"+strings.Repeat("0", 24))
	if err == nil {
		t.Fatal("Resolve accepted a spelling that reaches neither stored id")
	}
	for _, want := range []string{lower, upper} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not list %s: %v", want, err)
		}
	}
	// Each stored spelling is still addressable, one row each.
	if got, err := Resolve(context.Background(), store, "p", "id", lower); err != nil || got != lower {
		t.Errorf("Resolve(%q) = %q, %v; want the lower row", lower, got, err)
	}
	if got, err := Resolve(context.Background(), store, "p", "id", upper); err != nil || got != upper {
		t.Errorf("Resolve(%q) = %q, %v; want the upper row", upper, got, err)
	}
}

// TestResolveRefusesAnEmptyRef: nothing was named, and a ref that names nothing
// has to be distinguishable from a ref naming a row nobody holds.
func TestResolveRefusesAnEmptyRef(t *testing.T) {
	store := &fakeStore{ids: hexIDs("aaaaaaaa", 1)}
	_, err := Resolve(context.Background(), store, "p", "id", "")
	if err == nil {
		t.Fatal("Resolve accepted an empty ref")
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("the refusal does not say why: %v", err)
	}
}

// TestResolveNamesWhichRefFailed: a command with several refs has to be able to
// say which of them was wrong, which is what the which argument is for.
func TestResolveNamesWhichRefFailed(t *testing.T) {
	store := &fakeStore{ids: hexIDs("aaaaaaaa", 1)}
	_, err := Resolve(context.Background(), store, "p", "source", "ffffffffffffffff")
	if err == nil {
		t.Fatal("Resolve accepted a ref naming nothing")
	}
	if !strings.Contains(err.Error(), "source") {
		t.Errorf("the refusal does not name the operand: %v", err)
	}
}

// TestResolveAsksTheStoreAboutTheGivenProject: the scope is the caller's project,
// and a caller that reached for another project's row is refused at the query. The
// fake records what it was asked, which is the only part of this a unit test can
// see — the query's own scope is pinned by the callers' tests against a real store.
func TestResolveAsksTheStoreAboutTheGivenProject(t *testing.T) {
	store := &fakeStore{ids: hexIDs("a1b2c3d4", 1)}
	if _, err := Resolve(context.Background(), store, "some-project", "id", "a1b2c3d4"); err != nil {
		t.Fatal(err)
	}
	if store.project != "some-project" {
		t.Errorf("the store was asked about %q, want some-project", store.project)
	}
	if store.calls != 1 {
		t.Errorf("the store was queried %d times, want 1", store.calls)
	}
}

// TestShortMeasuresEightCharactersNotEightBytes: the report form has to be a ref
// the query can accept, and on a multi-byte id `id[:8]` is neither — it is
// invalid UTF-8 in a report line and a prefix no query matches.
func TestShortMeasuresEightCharactersNotBytes(t *testing.T) {
	cjk := strings.Repeat("日", 12)
	got := Short(cjk)
	if utf8.RuneCountInString(got) != MinRefLen {
		t.Errorf("Short(%q) = %q, which is %d runes, want %d", cjk, got, utf8.RuneCountInString(got), MinRefLen)
	}
	if !utf8.ValidString(got) {
		t.Errorf("Short(%q) = %q, which is not valid UTF-8", cjk, got)
	}
	// And the printed form must resolve against the store that holds it, which is
	// the whole reason this function is measured in characters.
	store := &fakeStore{ids: []string{cjk}}
	if got2, err := Resolve(context.Background(), store, "p", "id", got); err != nil {
		t.Errorf("the id Short printed does not resolve: %v", err)
	} else if got2 != cjk {
		t.Errorf("the printed ref resolved to %q, want %q", got2, cjk)
	}
	// A short id is returned whole rather than truncated to nothing.
	if got := Short("abc"); got != "abc" {
		t.Errorf("Short(%q) = %q, want it whole", "abc", got)
	}
}
