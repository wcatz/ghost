package main

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
)

// #684: `ghost reflect`'s preview listed the resulting memories and nothing
// else, so an input that a merge consumed simply disappeared from the report
// and a reviewer could not tell a merge from a loss. These tests pin the
// section that replaces it: every input id appears in exactly one line or one
// count, and a line kind that stops being printed fails the accounting.

var storedIDPattern = regexp.MustCompile(`\b[0-9A-F]{32}\b`)

// accountingID is a stored-id-shaped name for a fixture memory, and the shape is
// load-bearing rather than cosmetic. Ghost's ids are `hex(randomblob(16))`, which
// SQLite renders as 32 UPPER-CASE hex digits — measured on a real store, not read
// off the schema, because two of the three spellings this file once used were
// wrong in opposite directions and both hid a case defect. The `CAFE` prefix is
// here for the same reason one notch down: an all-digit id is unchanged by a case
// conversion, so it cannot tell a resolved id from a raw one either.
func accountingID(n int) string { return fmt.Sprintf("CAFE%028X", n) }

// reflectRoundForTest runs the real LLM tier — prompt, op parser, grounding
// check, post-filters — and then the drop guard, returning the result and the
// guard's audit in the state runReflect holds them in when it prints the
// accounting: the guard has run, and unless --allow-drops the rows it flagged
// have been re-added verbatim. Building a result by hand would skip every rule
// the section reports on, which is the whole reason it exists.
func reflectRoundForTest(t *testing.T, input reflection.ReflectionInput, reply func() string, allowDrops bool) (reflection.ReflectionResult, []reflection.DroppedGuarded) {
	t.Helper()
	llm := reflection.NewLlmConsolidator(stubHarness{reply: reply})
	result, err := llm.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	guarded := reflection.AuditGuardedDrops(input, result)
	if len(guarded) > 0 && !allowDrops {
		result.Memories = append(result.Memories, reflection.RetainGuardedDrops(guarded)...)
	}
	return result, guarded
}

// accountingSection renders the section runReflect prints after the result
// list, for a dry run and for an apply alike.
func accountingSection(t *testing.T, input reflection.ReflectionInput, result reflection.ReflectionResult, guarded []reflection.DroppedGuarded, allowDrops, full bool) string {
	t.Helper()
	var out strings.Builder
	reportInputAccounting(&out, reflectRun{
		input: input, result: result, guarded: guarded, allowDrops: allowDrops, full: full,
	})
	return out.String()
}

// accountFixture is one input memory, with a known id and content.
type accountFixture struct {
	id      string
	content string
}

func (f accountFixture) stored() memory.Memory {
	return memory.Memory{ID: f.id, Content: f.content, Category: "fact", Importance: 0.5, Tags: []string{"accounting"}, Source: "mcp"}
}

func accountingInput(t *testing.T, fixtures ...accountFixture) reflection.ReflectionInput {
	t.Helper()
	input := reflection.ReflectionInput{ProjectName: "proj"}
	for _, f := range fixtures {
		input.ExistingMemories = append(input.ExistingMemories, f.stored())
	}
	return input
}

func accountingIDs(fixtures ...accountFixture) []string {
	out := make([]string, 0, len(fixtures))
	for _, f := range fixtures {
		out = append(out, f.id)
	}
	return out
}

// storedIDsIn returns the stored ids the report quotes.
func storedIDsQuoted(section string) []string {
	return storedIDPattern.FindAllString(section, -1)
}

// countIn reads one "label N" count out of the section.
func countReported(t *testing.T, section, label string) int {
	t.Helper()
	re := regexp.MustCompile(regexp.QuoteMeta(label) + `(\d+)`)
	m := re.FindStringSubmatch(section)
	if m == nil {
		t.Fatalf("the section does not report %q at all:\n%s", label, section)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("%q count %q: %v", label, m[1], err)
	}
	return n
}

// assertAccountsFor is the invariant #684 asks for, checked against the reply
// the test itself wrote rather than against the accounting's own idea of what
// it did. lineIDs are the ids the section must name, refs are the ids it may
// quote as somebody ELSE's successor (a `superseded by` reason names the memory
// that replaces the dropped row, and that one is accounted for by its own
// bucket), and kept and passed are the two counts' worth. Every input id has to
// appear in exactly one of lineIDs, kept and passed.
func assertAccountsFor(t *testing.T, section string, fixtures []accountFixture, lineIDs, refs, kept, passed []string) {
	t.Helper()

	// The caller's own model of the round: one account per input id.
	accounted := make(map[string]int)
	claim := func(ids []string) {
		for _, id := range ids {
			accounted[id]++
		}
	}
	claim(lineIDs)
	claim(kept)
	claim(passed)
	for _, f := range fixtures {
		if accounted[f.id] != 1 {
			t.Errorf("input %s is accounted for %d times across the lines, the two counts and the inputs — exactly once is the contract\n%s",
				f.id, accounted[f.id], section)
		}
	}
	if len(accounted) != len(fixtures) {
		t.Errorf("the section accounts for %d distinct ids over %d inputs:\n%s", len(accounted), len(fixtures), section)
	}
	for _, id := range refs {
		if accounted[id] != 1 {
			t.Errorf("the section references %s as a successor, but that id is not accounted for exactly once itself\n%s", id, section)
		}
	}

	// The quoted ids are exactly the ones the line kinds name, once each.
	referenced := make(map[string]bool, len(refs))
	for _, id := range refs {
		referenced[id] = true
	}
	quoted := make(map[string]int)
	for _, id := range storedIDsQuoted(section) {
		if referenced[id] {
			continue
		}
		quoted[id]++
	}
	for id, n := range quoted {
		if n != 1 {
			t.Errorf("id %s is quoted %d times — every input id appears in exactly one line or one count:\n%s", id, n, section)
		}
		if accounted[id] != 1 {
			t.Errorf("the section quotes %s, which no line kind should have named:\n%s", id, section)
		}
	}

	// The counts are the counts of the two buckets, and the header is the input
	// total: a section that printed the right ids under the wrong numbers would
	// still not account for the input set.
	if got := countReported(t, section, "Kept verbatim: "); got != len(kept) {
		t.Errorf("Kept verbatim: %d, want %d (%v)\n%s", got, len(kept), kept, section)
	}
	if got := countReported(t, section, "Passed through (not named): "); got != len(passed) {
		t.Errorf("Passed through (not named): %d, want %d (%v)\n%s", got, len(passed), passed, section)
	}
	if got := countReported(t, section, "Inputs ("); got != len(fixtures) {
		t.Errorf("the section reports %d inputs, want %d:\n%s", got, len(fixtures), section)
	}
	// The check the report itself tells the reader to make, made here: every
	// header is a count of IDS, so the section's own numbers add up to the input
	// total. This is the assertion a count of merge OPERATIONS fails, which is
	// why the mixed fixture merges three sources rather than two.
	sum := len(kept) + len(passed)
	for _, header := range []string{
		"Merges (",
		"Refused by the grounding check (",
		"Refused because the merge folds more decision links than one source_ref can hold (",
		"Refused because the sources' scopes disagree (",
		"Rewrites (",
		"Dropped (",
		"Deleted (",
	} {
		sum += countReported(t, section, header)
	}
	if sum != len(fixtures) {
		t.Errorf("the section's own counts sum to %d over %d inputs, so the check it asks the reader to make does not come out:\n%s",
			sum, len(fixtures), section)
	}
	if quoted, counted := len(quoted), len(kept)+len(passed); quoted+counted != len(fixtures) {
		t.Errorf("the section names %d ids and counts %d more, which is not the %d inputs:\n%s", quoted, counted, len(fixtures), section)
	}
}

// TestReflectSummaryAccountsForASupersession covers the one place a line quotes
// an id it does not account for: a `superseded by` reason names the memory that
// replaces the dropped row, and that successor belongs to its own bucket. The
// reason is worth printing whole — which row took over is the first question
// asked of a supersession — and the accounting still has to be exactly once.
func TestReflectSummaryAccountsForASupersession(t *testing.T) {
	stale := accountFixture{accountingID(81), "the ledger syncs from the relay export staging directory"}
	successor := accountFixture{accountingID(82), "the ledger syncs from the relay export directory"}
	fixtures := []accountFixture{stale, successor}
	input := accountingInput(t, fixtures...)

	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["keep %s","drop %s reason: superseded by %s"]}`,
			successor.id, stale.id, successor.id)
	}, false)
	if len(guarded) != 0 {
		t.Fatalf("the guard flagged %d rows, so the drop did not take effect: %+v", len(guarded), guarded)
	}
	if resultCarries(result, stale.content) {
		t.Fatalf("the superseded row is still in the result, so this is not the case it claims to be: %+v", result.Memories)
	}

	section := accountingSection(t, input, result, guarded, false, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures, accountingIDs(stale), accountingIDs(successor),
		accountingIDs(successor), nil)
	if !strings.Contains(section, "reason: superseded by "+successor.id) {
		t.Errorf("the drop line does not name the successor that took over:\n%s", section)
	}
}

// TestReflectSummaryAccountsForARefusedMerge pins the grounding check's half of
// the contract. A merge whose text invents an identifier is rejected and its
// sources are emitted unchanged — but before this the refusal existed only as a
// log line inside the tier, so the report showed two memories and said nothing
// about the merge the model had asked for, or about the ids it named.
func TestReflectSummaryAccountsForARefusedMerge(t *testing.T) {
	sourceA := accountFixture{accountingID(11), "the ledger syncs from the relay's export directory"}
	sourceB := accountFixture{accountingID(12), "the relay writes an export directory for the ledger"}
	unnamed := accountFixture{accountingID(13), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	fixtures := []accountFixture{sourceA, sourceB, unnamed}
	input := accountingInput(t, fixtures...)

	// The replacement names /opt/ledger/cache, which neither source carries.
	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["merge %s,%s -> the ledger syncs from /opt/ledger/cache"]}`,
			sourceA.id, sourceB.id)
	}, false)
	if len(guarded) != 0 {
		t.Fatalf("the guard flagged %d rows, so the sources were not carried through untouched: %+v", len(guarded), guarded)
	}
	// The behavioural half: the refusal keeps the sources.
	for _, f := range fixtures[:2] {
		if !resultCarries(result, f.content) {
			t.Fatalf("a refused merge did not keep %s verbatim: %+v", f.id, result.Memories)
		}
	}

	section := accountingSection(t, input, result, guarded, false, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures, accountingIDs(sourceA, sourceB), nil, nil, accountingIDs(unnamed))
	if !strings.Contains(section, "/opt/ledger/cache") {
		t.Errorf("the refusal does not name the identifier that caused it:\n%s", section)
	}
	if !strings.Contains(section, "Refused by the grounding check (2):") {
		t.Errorf("a refused merge is not reported as one:\n%s", section)
	}
}

// The four tests below run a REAL tier over a REAL store and then check the
// report against what the replace actually did, which is the only thing that
// makes a claim about rows sound. `present` used to be keyed by content text
// alone, so two byte-identical inputs both read as "carried" — while
// `ReplaceNonManual` claims exactly ONE of them, because reuse is content-keyed
// and every unclaimed candidate is deleted. The report then said "passed
// through" about a row the apply had removed, which is the one thing this section
// exists to stop saying.

// storeRound seeds a project, runs a tier over its own rows, renders the
// accounting, applies it through the real write boundary, and returns the section
// with the ids the store holds afterwards.
//
// The last value is the point. A report's account of which rows an apply removes
// is only worth having if something compares it with the rows the apply removed,
// and the only thing that knows is the store.
type storeRoundResult struct {
	// fixtures are the seeded rows with the ids the STORE gave them, so the
	// printed-id scan in assertAccountsFor runs against the real keys.
	fixtures []accountFixture
	// categories maps each of those ids to the category it was seeded with, which
	// the reuse pick is decided by.
	categories map[string]string
	// ids are the stored ids in seeding order, for a reply that names one.
	ids       []string
	result    reflection.ReflectionResult
	section   string
	surviving map[string]bool
}

// seedRow is a row for storeRound: content, and the category to store it under.
// An empty category is "fact", so a test that does not care writes only content.
type seedRow struct {
	// id and createdAt pin a row's place in the claim order, which is
	// (created_at, id) — the store's candidate order. A row that leaves them to
	// the store cannot be used to pin WHICH row the replace keeps, because the
	// consolidator's input order is GetAll's `importance DESC, created_at DESC`
	// and the two are not the same order. Set both and the row is imported, which
	// is the only way a caller can choose an id at all.
	id        string
	createdAt string
	content   string
	category  string
}

func (s seedRow) storedCategory() string {
	if s.category == "" {
		return "fact"
	}
	return s.category
}

// storeRound runs the offline SQLite tier when reply is nil, and the real LLM
// tier with a canned reply otherwise. promoteGlobals is off, which is the
// default and the case where the cross-project candidates are folded into the
// project before the replace.
func storeRound(t *testing.T, project string, seeded []seedRow, reply func(ids []string) string) storeRoundResult {
	t.Helper()
	store, ctx := newReflectStore(t, project)
	out := storeRoundResult{categories: map[string]string{}, surviving: map[string]bool{}}
	for _, f := range seeded {
		id := f.id
		if id == "" {
			created, err := store.Create(ctx, project, memory.Memory{
				Category: f.storedCategory(), Content: f.content, Importance: 0.6, Source: "mcp", Tags: []string{"accounting"},
			})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			id = created
		} else {
			if _, _, _, err := store.ImportMemory(ctx, memory.PortableMemory{
				ID: id, ProjectID: project, Category: f.storedCategory(),
				Content: f.content, Source: "mcp", CreatedAt: f.createdAt,
			}, memory.ImportOptions{Apply: true, TrustProvenance: true}); err != nil {
				t.Fatalf("ImportMemory(%s): %v", id, err)
			}
		}
		out.ids = append(out.ids, id)
		out.fixtures = append(out.fixtures, accountFixture{id: id, content: f.content})
		out.categories[id] = f.storedCategory()
	}
	input := reflectInputFor(t, store, project)

	var result reflection.ReflectionResult
	var guarded []reflection.DroppedGuarded
	if reply == nil {
		var err error
		if result, err = reflection.NewSQLiteConsolidator().Consolidate(ctx, input); err != nil {
			t.Fatalf("Consolidate: %v", err)
		}
		guarded = reflection.AuditGuardedDrops(input, result)
		if len(guarded) > 0 {
			result.Memories = append(result.Memories, reflection.RetainGuardedDrops(guarded)...)
		}
	} else {
		result, guarded = reflectRoundForTest(t, input, func() string { return reply(out.ids) }, false)
	}
	out.result = result

	out.section = accountingSection(t, input, result, guarded, false, false)
	projectMems, globalMems := splitByScope(result.Memories)
	if _, err := applyReflection(ctx, store, project, projectMems, globalMems, "", false, nil, nil); err != nil {
		t.Fatalf("applyReflection: %v", err)
	}
	after, err := store.GetAll(ctx, project, -1)
	if err != nil {
		t.Fatalf("GetAll after apply: %v", err)
	}
	for _, m := range after {
		out.surviving[m.ID] = true
	}
	return out
}

// splitBySurvival is the ground truth every duplicate test asserts against, read
// from the store rather than from the accounting: the ids the replace removed
// and the ids it left. deleted is what the section must report as its Deleted
// lines; passed is what it must count as carried.
func (r storeRoundResult) splitBySurvival() (deleted, passed []string) {
	for _, f := range r.fixtures {
		if r.surviving[f.id] {
			passed = append(passed, f.id)
			continue
		}
		deleted = append(deleted, f.id)
	}
	return deleted, passed
}

// deletedLinesQuoted are the ids on the section's Deleted lines, and reading them
// is a check assertAccountsFor cannot make: that helper asks whether the quoted
// ids are a subset of the accounted set with the right counts, and an id that
// belongs in one bucket but is printed in another of the SAME SIZE passes it. A
// claim pass that picked the wrong row of an identical pair prints exactly that —
// the right count and the wrong ids.
func deletedLinesQuoted(section string) []string {
	var ids []string
	inBlock := false
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "Deleted (") {
			inBlock = true
			continue
		}
		if !inBlock {
			continue
		}
		if !strings.HasPrefix(line, "  ") {
			break
		}
		ids = append(ids, storedIDsQuoted(line)...)
	}
	return ids
}

// assertDeletedLinesAreTheRemovedRows is the invariant every store-backed test
// shares: the ids on the Deleted lines are the ids the store stopped holding, no
// more and no fewer. The section's own arithmetic cannot catch a wrong row of an
// identical pair, and the store is the only thing that knows which one it was.
func assertDeletedLinesAreTheRemovedRows(t *testing.T, section string, removed []string) {
	t.Helper()
	quoted := deletedLinesQuoted(section)
	if len(quoted) != len(removed) {
		t.Fatalf("the section's Deleted lines name %v, want the %d row(s) the store removed (%v):\n%s",
			quoted, len(removed), removed, section)
	}
	got := append([]string(nil), quoted...)
	want := append([]string(nil), removed...)
	sort.Strings(got)
	sort.Strings(want)
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("the section names %s as deleted where the store removed %s:\n%s", got[i], want[i], section)
		}
	}
}

// byte-identical inputs, no operation naming either of them, one emission
// between them. The content IS in the result, so the pre-claim accounting called
// both "passed through"; the store claims one row for the emission and deletes
// the other, so one of them was a lie.
func TestReflectSummaryNamesTheRowAnIdenticalTwinReuses(t *testing.T) {
	const text = "the relay rolls back to the previous firmware over the serial console"
	out := storeRound(t, "twin", []seedRow{
		{content: text},
		{content: text},
		{content: "the ingest pipeline writes run manifests under /var/lib/ghost"},
	}, nil)
	t.Logf("section:\n%s", out.section)

	deleted, passed := out.splitBySurvival()
	if len(deleted) != 1 {
		t.Fatalf("the store removed %v, want exactly one of the identical pair: the reuse is content-keyed, so one row is claimed and one is deleted", deleted)
	}
	if len(passed) != 2 {
		t.Fatalf("the store kept %v, want the surviving twin and the unrelated row", passed)
	}
	assertAccountsFor(t, out.section, out.fixtures, deleted, nil, nil, passed)
	assertDeletedLinesAreTheRemovedRows(t, out.section, deleted)
	// The reason, because "deleted" and "deduplicated" are different outcomes
	// and the row's text is still in the corpus either way.
	if !strings.Contains(out.section, "an identical row is reused in its place") {
		t.Errorf("the deleted row is not reported as a duplicate rather than a loss:\n%s", out.section)
	}
}

func resultCarries(result reflection.ReflectionResult, content string) bool {
	for _, m := range result.Memories {
		if m.Content == content {
			return true
		}
	}
	return false
}
