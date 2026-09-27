package main

import (
	"context"
	"fmt"
	"regexp"
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

var storedIDPattern = regexp.MustCompile(`[0-9A-Z]{26}`)

// testID is a stored-id-shaped name for a fixture memory, so a report that
// quotes an id is quoted in the shape an operator would see in the database.
func accountingID(n int) string { return fmt.Sprintf("01J8Z%021d", n) }

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
	reportInputAccounting(&out, input, result, guarded, allowDrops, full)
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
		"Rewrites (",
		"Dropped (",
		"Absent from the result (",
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

// TestReflectSummaryAccountsForEveryInputID is the mixed case: one reply that
// keeps a row, merges three, rewrites one, drops one and never mentions the last.
// The five dispositions are the five the operations vocabulary has, and each has
// to be visible in the report — the input a merge consumed used to vanish from
// it entirely, which is what made a dry run unauditable.
//
// The merge takes THREE sources on purpose: that is the one place a header
// counting operations and a header counting ids disagree, and it is the case a
// reader has to be able to add up.
func TestReflectSummaryAccountsForEveryInputID(t *testing.T) {
	kept := accountFixture{accountingID(1), "the bastion answers ping on 443 and ssh on 2222"}
	mergeA := accountFixture{accountingID(2), "production runs in region fsn1 behind Cloudflare"}
	mergeB := accountFixture{accountingID(3), "Cloudflare fronts the production region fsn1"}
	mergeC := accountFixture{accountingID(4), "the production region fsn1 sits behind Cloudflare"}
	rewritten := accountFixture{accountingID(5), "the ledger ingests through the bastion on port 2222, not 22"}
	dropped := accountFixture{accountingID(6), "the bastion also answers ssh on port 2222 only"}
	unnamed := accountFixture{accountingID(7), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	fixtures := []accountFixture{kept, mergeA, mergeB, mergeC, rewritten, dropped, unnamed}
	input := accountingInput(t, fixtures...)

	const mergeText = "production in region fsn1 runs behind Cloudflare"
	const rewriteText = "the ledger ingests through the bastion on port 2222, never 22"
	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":[
			"keep %s",
			"merge %s,%s,%s -> %s",
			"rewrite %s -> %s",
			"drop %s reason: obsolete"]}`,
			kept.id, mergeA.id, mergeB.id, mergeC.id, mergeText, rewritten.id, rewriteText, dropped.id)
	}, false)

	if len(guarded) != 0 {
		t.Fatalf("the guard flagged %d rows, so this fixture is not the clean case it claims to be: %+v", len(guarded), guarded)
	}

	section := accountingSection(t, input, result, guarded, false, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures,
		// the merge's three sources, then the rewrite, then the drop
		accountingIDs(mergeA, mergeB, mergeC, rewritten, dropped),
		nil, accountingIDs(kept), accountingIDs(unnamed))

	// The merge line is the part the issue is about: a successor the dry run
	// can point at, the ids it consumed, and what it cost.
	if !strings.Contains(section, "new <- "+mergeA.id+", "+mergeB.id+", "+mergeC.id) {
		t.Errorf("the merge line does not name the successor and every id it folded in:\n%s", section)
	}
	sourceBytes := len(mergeA.content) + len(mergeB.content) + len(mergeC.content)
	if !strings.Contains(section, fmt.Sprintf("(%d B from %d B)", len(mergeText), sourceBytes)) {
		t.Errorf("the merge line does not report the bytes it produced against the bytes it consumed:\n%s", section)
	}
	// The drop line carries the reason the model gave, which nothing recorded
	// before: an obsolete drop names no successor, so the response said nothing
	// at all about the id it disposed of.
	if !strings.Contains(section, dropped.id+" reason: obsolete") {
		t.Errorf("the drop line does not carry the reason the response gave:\n%s", section)
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

// TestReflectSummaryAccountsForADropTheGuardRetains is the case where the drop
// guard overrides the model, and it is the one an operator most needs to audit:
// the response asked for a deletion, nothing in the result accounts for the
// row, so the guard put it back — and the report has to say the row survived
// rather than leaving the drop line to be read as a deletion.
func TestReflectSummaryAccountsForADropTheGuardRetains(t *testing.T) {
	dropped := accountFixture{accountingID(21), "the bastion firmware rollback procedure runs from a laptop with a serial cable"}
	other := accountFixture{accountingID(22), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	fixtures := []accountFixture{dropped, other}
	input := accountingInput(t, fixtures...)

	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["drop %s reason: obsolete"]}`, dropped.id)
	}, false)
	if len(guarded) != 1 || !resultCarries(result, dropped.content) {
		t.Fatalf("the guard did not retain the dropped row, so this is not the case it claims to be: %+v", guarded)
	}

	section := accountingSection(t, input, result, guarded, false, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures, accountingIDs(dropped), nil, nil, accountingIDs(other))
	// The outcome, not just the claim: a drop line read on its own is a
	// deletion, and this is the case where it is not one.
	if !strings.Contains(section, "the drop guard re-added 1 row verbatim") {
		t.Errorf("a drop the guard overrode is not reported as kept:\n%s", section)
	}
}

// TestReflectSummaryAccountsForTheSQLiteTierAbsorptions covers the tier that
// names no ids at all. It folds a near-duplicate away and records nothing, so
// before this the report's only account of that row was a warning naming its
// text: an operator could not tell a dedup from a loss, which is the same hole
// the LLM path had.
func TestReflectSummaryAccountsForTheSQLiteTierAbsorptions(t *testing.T) {
	shorter := accountFixture{accountingID(31), "the bastion answers ping on 443"}
	longer := accountFixture{accountingID(32), "the bastion answers ping on 443 as well today"}
	other := accountFixture{accountingID(33), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	fixtures := []accountFixture{shorter, longer, other}
	input := accountingInput(t, fixtures...)

	result, err := reflection.NewSQLiteConsolidator().Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	guarded := reflection.AuditGuardedDrops(input, result)
	if len(guarded) != 0 {
		t.Fatalf("the guard flagged %d rows, so the absorbed row was not simply gone: %+v", len(guarded), guarded)
	}
	// The absorbed row is the one whose text the emission did not take: the
	// SQLite tier keeps the longer of a pair, so the row that loses is the
	// shorter one.
	absent := shorter
	for _, f := range fixtures {
		if !resultCarries(result, f.content) {
			absent = f
		}
	}

	section := accountingSection(t, input, result, guarded, false, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures, accountingIDs(absent), nil, nil, accountingIDs(longer, other))
	if !strings.Contains(section, "Absent from the result (1)") {
		t.Errorf("an input the result does not carry is not reported as absent:\n%s", section)
	}
}

// TestReflectFullPrintsUntruncatedText pins --full against the compact default
// it replaces. The complaint behind it was a merged memory's added text being
// cut off mid-sentence, so the flag has to reach the text of the operation
// lines, not only the result listing.
func TestReflectFullPrintsUntruncatedText(t *testing.T) {
	// Trimmed because the op parser trims the text it reads, so a fixture with a
	// trailing space would never be the text the report prints.
	long := strings.TrimSpace(strings.Repeat("the ledger syncs from the relay export directory. ", 12))
	if len(long) <= 120 {
		t.Fatalf("the fixture is %d bytes, which the compact default would not truncate", len(long))
	}
	source := accountFixture{accountingID(41), "the ledger syncs from the relay export directory today"}
	fixtures := []accountFixture{source}
	input := accountingInput(t, fixtures...)

	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["rewrite %s -> %s"]}`, source.id, long)
	}, false)

	compact := accountingSection(t, input, result, guarded, false, false)
	if strings.Contains(compact, long) {
		t.Errorf("the default render is not compact, so --full has nothing to add:\n%s", compact)
	}
	if !strings.Contains(compact, "...") {
		t.Errorf("the compact render does not mark its truncation:\n%s", compact)
	}

	full := accountingSection(t, input, result, guarded, false, true)
	if !strings.Contains(full, long) {
		t.Errorf("--full did not print the whole replacement text:\n%s", full)
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

// TestReflectSummaryPrintsTheStoredIDSpelling pins the id the report quotes to
// the spelling the DATABASE holds. The op parser strips the prompt's `id:`
// label and trims but does not upper-case, and the drop's successor target is
// upper-cased while the dropped id is not, so a model that lower-cases an id —
// which `memIDKey` exists to tolerate — would otherwise have its operation line
// print `01j8z…02` where a count line prints `01J8Z…02`. These ids are the keys
// the report asks an operator to look rows up by, and a copied id that does not
// match the store is the one kind of typo this section cannot afford.
func TestReflectSummaryPrintsTheStoredIDSpelling(t *testing.T) {
	source := accountFixture{accountingID(91), "the ledger syncs from the relay export directory"}
	unnamed := accountFixture{accountingID(92), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	fixtures := []accountFixture{source, unnamed}
	input := accountingInput(t, fixtures...)

	// The harness answers in the case the parser accepts and a stored row does
	// not spell: the id lower-cased. One id takes exactly one operation, so this
	// is the rewrite alone and the other fixture is the pass-through.
	lower := strings.ToLower(source.id)
	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["rewrite %s -> the ledger syncs from the relay export directory nightly"]}`, lower)
	}, false)
	if len(guarded) != 0 {
		t.Fatalf("the guard flagged %d rows, so this fixture is not the clean case it claims to be: %+v", len(guarded), guarded)
	}

	section := accountingSection(t, input, result, guarded, false, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures, accountingIDs(source), nil, nil, accountingIDs(unnamed))
	if !strings.Contains(section, source.id+" ->") {
		t.Errorf("the rewrite line does not quote the stored spelling of the id:\n%s", section)
	}
	if strings.Contains(section, lower) {
		t.Errorf("the report quotes the model's spelling of the id, which the database does not hold:\n%s", section)
	}
}

// TestReflectFullIsParsed pins the flag as argv rather than as a field some
// other run path sets: parseReflectArgs silently ignores a flag it does not
// take, so a --full that was never added to the parser would be accepted on the
// command line and quietly do nothing, which is the worst shape a display flag
// can have.
func TestReflectFullIsParsed(t *testing.T) {
	p, err := parseReflectArgs([]string{"proj", "--full", "--apply"})
	if err != nil {
		t.Fatalf("parseReflectArgs: %v", err)
	}
	if !p.full {
		t.Error("--full was ignored: an unparsed flag is accepted and does nothing")
	}
	if p.apply != true || p.project != "proj" {
		t.Errorf("--full changed the rest of the parse: %+v", p)
	}
	// The default is the compact preview, so a run that did not ask for the
	// whole text must not get it.
	plain, err := parseReflectArgs([]string{"proj"})
	if err != nil {
		t.Fatalf("parseReflectArgs: %v", err)
	}
	if plain.full {
		t.Error("the compact preview is the default, so --full must be asked for")
	}
	if !strings.Contains(reflectUsage, "--full") {
		t.Errorf("the usage block does not document --full:\n%s", reflectUsage)
	}
}

// TestReflectSummaryNamesTheOperationsThatDidNotLand covers the case where an
// operation was accepted and then a post-filter removed what it produced. The
// ids it named are on their line, but the text it produced is in neither the
// result nor the store, so a line showing only the operation would read as a
// merge that happened — and the sources it consumed are then the only record
// that anything was lost.
func TestReflectSummaryNamesTheOperationsThatDidNotLand(t *testing.T) {
	sourceA := accountFixture{accountingID(51), "the ledger syncs from the relay export directory"}
	sourceB := accountFixture{accountingID(52), "the relay writes an export directory for the ledger"}
	fixtures := []accountFixture{sourceA, sourceB}
	input := accountingInput(t, fixtures...)

	// A result as a post-filter left it: the merge is recorded and the memory it
	// produced is in neither the result nor the store, and nothing else in the
	// corpus accounts for either source.
	result := reflection.ReflectionResult{
		Merges: []reflection.Merge{{
			IDs:  accountingIDs(sourceA, sourceB),
			Text: "the ledger syncs from the relay export staging directory",
		}},
	}
	guarded := reflection.AuditGuardedDrops(input, result)
	if len(guarded) != 2 {
		t.Fatalf("the guard flagged %d of the merge's sources, so this is not the case it claims to be: %+v", len(guarded), guarded)
	}

	// The operator accepted the deletions, so the clause has to be the accepting
	// one rather than a re-add.
	section := accountingSection(t, input, result, guarded, true, false)
	t.Logf("section:\n%s", section)

	assertAccountsFor(t, section, fixtures, accountingIDs(sourceA, sourceB), nil, nil, nil)
	if !strings.Contains(section, "the merged text is not in this result") {
		t.Errorf("a merge whose text a post-filter removed reads as one that happened:\n%s", section)
	}
	if !strings.Contains(section, "2 sources have no surviving output") {
		t.Errorf("the merge line does not say what became of its sources:\n%s", section)
	}
}

// TestReflectSummaryTellsADeletedDropFromARetainedOne is the other half of the
// drop line: --allow-drops is the operator accepting the deletions, so the same
// audited drop has to read differently under it. A line that said "kept" in both
// cases would be worse than no outcome at all.
func TestReflectSummaryTellsADeletedDropFromARetainedOne(t *testing.T) {
	dropped := accountFixture{accountingID(61), "the bastion firmware rollback procedure runs from a laptop with a serial cable"}
	other := accountFixture{accountingID(62), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	fixtures := []accountFixture{dropped, other}
	input := accountingInput(t, fixtures...)

	result, guarded := reflectRoundForTest(t, input, func() string {
		return fmt.Sprintf(`{"learned_context":"ctx","ops":["drop %s reason: obsolete"]}`, dropped.id)
	}, true)
	if resultCarries(result, dropped.content) {
		t.Fatalf("--allow-drops accepted the deletion, so the row must not be back: %+v", result.Memories)
	}

	section := accountingSection(t, input, result, guarded, true, false)
	t.Logf("section:\n%s", section)
	assertAccountsFor(t, section, fixtures, accountingIDs(dropped), nil, nil, accountingIDs(other))
	if !strings.Contains(section, "1 row has no surviving output, and --allow-drops accepts the deletion") {
		t.Errorf("a deletion the operator accepted is not reported as one:\n%s", section)
	}
	if strings.Contains(section, "re-added") {
		t.Errorf("a deletion under --allow-drops is reported as a re-add:\n%s", section)
	}
	// The exact clause, singular verb and all: this is the wording an operator
	// reads on every accepted deletion, and "1 row have" is what pluralising the
	// noun alone produces.
	if !strings.Contains(section, "; 1 row has no surviving output") {
		t.Errorf("the guard clause does not read as a sentence:\n%s", section)
	}
}

// TestReflectSummaryNamesARepeatedInputIDOnce keeps the one-account contract
// where it is easiest to break: the total is a count of ids, so an id the input
// happens to carry twice is named once and the buckets still add up to the
// header.
func TestReflectSummaryNamesARepeatedInputIDOnce(t *testing.T) {
	one := accountFixture{accountingID(71), "the ingest pipeline writes run manifests under /var/lib/ghost"}
	two := accountFixture{accountingID(72), "the ledger ingests through the bastion on port 2222"}
	input := accountingInput(t, one, two, one)

	var out strings.Builder
	reportInputAccounting(&out, input, reflection.ReflectionResult{}, nil, false, false)
	section := out.String()
	t.Logf("section:\n%s", section)

	if got := countReported(t, section, "Inputs ("); got != 2 {
		t.Errorf("the section reports %d inputs, want the 2 distinct ids:\n%s", got, section)
	}
	// Both rows are named by nothing and carried by nothing, so they are the
	// absent pair — and named once each.
	if got := countReported(t, section, "Absent from the result ("); got != 2 {
		t.Errorf("Absent from the result (%d), want 2:\n%s", got, section)
	}
	quoted := storedIDsQuoted(section)
	if len(quoted) != 2 {
		t.Errorf("the section names %v, want one line per distinct id:\n%s", quoted, section)
	}
}
