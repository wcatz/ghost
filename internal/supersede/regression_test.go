package supersede

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// relationCase is one labeled pair: what the classifier should answer (want),
// the verdict a wrong run really produced (wrong), and each note's created_at
// — the orientation signal the prompt needs, and the only field separating a
// genuine later update from a stale claim re-asserted later.
type relationCase struct {
	key          string // short fixture id, used by the tests to look a case up
	name         string
	newer        string
	newerCreated string
	// newerUpdated is set only when the pass's updated_at ordering disagrees
	// with created_at (a reflection refresh re-asserting the stale note);
	// empty means created_at == updated_at.
	newerUpdated string
	older        string
	olderCreated string
	want         Relation
	wrong        Relation
}

// regressionRelationCases is the labeled regression set from issue #641: the
// four memory pairs a maintenance pass misjudged on a real database — a
// backwards supersession, two `causes` between contradicting status reports, and
// an event record superseded by a later unrelated event on the same host.
//
// The text is ANONYMIZED, not verbatim: hosts, the reporting agent's handle,
// repository and pull/issue numbers, pool names, slots and every date are
// replaced with neutral placeholders. What each pair has to keep is its shape,
// because the shape is what the classifier gets wrong:
//
//   - reversed: the endpoint the pass treats as newer (updated_at) is a stale
//     claim that was CREATED BEFORE the note that fixed it, which is the only
//     signal that separates it from a genuine later update. That inversion is
//     the regression.
//   - status-report-fix / status-report-divergence: two reports of ONE open
//     issue. `causes` is wrong for both. #641 labeled both SUPERSEDES and
//     said so itself — "supersede is at most right" — because the bug it was
//     fixing was the `causes` link. #779 relabeled the divergence half to
//     NEITHER, on the reasoning at that case: the fix half's later report
//     really does retire the earlier blocker, and the divergence half's does
//     not, so only the first is a supersession.
//   - parallel-events: two separate things that happened to the same host, in
//     opposite order to importance. Neither supersedes the other.
//
// The `created` timestamps are synthetic and only their RELATIVE order is
// meaningful. `newerUpdated` is set only for the reversed case, where the
// pass's updated_at ordering has to disagree with created_at.
var regressionRelationCases = []relationCase{
	{
		key:          "reversed",
		name:         "stale bug list vs the fix that closed it",
		newer:        "Three open tracker issues filed last week by the reporting agent, all real defects with cited line numbers: the balance lookup falls back to the first bucket that happens to have data instead of requiring the requested one, so every entry it writes is wrong; the undo path never rebuilds the tally after discarded rows are deleted, so the count drifts upward; and the third issue only shows up once the first is fixed, so a tally built on the bad lookup is never revalidated and keeps being served.",
		newerCreated: "2023-03-01 09:00:00",
		// The pass sees this stale note as newer, though it was written five
		// hours BEFORE the fix that superseded it.
		newerUpdated: "2023-03-02 06:00:00",
		older:        "Fixed 2 of the 3 tracker issues: the balance lookup now requires the requested bucket with no fallback, and the undo path rebuilds the tally from the upstream row set — but only when the delete actually removed something, because an undo also fires on ordinary short resyncs and an unconditional rebuild would put a rate-limited network call on the hot path. The cached half of the third issue is still pending.",
		olderCreated: "2023-03-01 14:00:00",
		want:         RelationReversed,
		wrong:        RelationSupersedes,
	},
	{
		key:          "status-report-fix",
		name:         "two status reports of one blocker: not shipped vs cleared",
		newer:        "The runtime evaluates scripts itself, so an encoding-fidelity fix has to land in all three layers at once: the shared library, the serializer and the runtime. The runtime rewrites wire-form script data into the canonical form before a script ever sees it. That was demonstrated against a real block: the issuing script derives its asset name as a hash over the serialized reference, and once the rewrite shipped the failure disappeared. The serializer on its own is insensitive to the wire form.",
		newerCreated: "2023-05-20 11:00:00",
		older:        "BLOCKER — the encoding-fidelity fix is in no released build of the shared library, so today's production build stops short of that block and never gets past it. Checked: the serializer change is merged and published, but the code that would invoke it lives in the library, and the newest published library version contains none of those invocation sites — the build pins precisely that version. So the published half is not sufficient on its own: the invocation sites are still missing, and landing them is what unblocks the upgrade.",
		olderCreated: "2023-05-16 08:00:00",
		want:         RelationSupersedes,
		wrong:        RelationCauses,
	},
	{
		key:          "status-report-divergence",
		name:         "two reports of the same open divergence",
		newer:        "OPEN: a second production script mismatch (a different transaction, a later block, the same error text) still reproduces with the witness-shape normalization pinned, and a separate zero-amount case turned up at yet another block. The upstream change that reshapes the witness before the second script context is built targets the first failure. The offending input payload is archived off-box.",
		newerCreated: "2023-05-20 11:05:00",
		older:        "OPEN: a second, distinct script-evaluation mismatch exists beyond the encoding bug already fixed — reproduced on live production as a rejected zero-amount script that the network accepts and our evaluation path does not. Confirmed still present with the witness-shape fix applied, so it is a separate cause, not yet run to completion. A full offending input was captured for later debugging.",
		olderCreated: "2023-05-17 16:00:00",
		// RELABELED to NEITHER by #779, deliberately and against #641's
		// SUPERSEDES. #641's own comment called this "at most right" — the
		// finding it fixed was the misused `causes` link, and SUPERSEDES was
		// chosen as a verdict the prompt could be made to reach, not because
		// anything in the pair is retired. Nothing is: BOTH notes are OPEN
		// reports of the SAME still-reproducing problem, and the newer adds a
		// second sighting plus a payload archive while the older's claim — a
		// distinct mismatch that is still present — stays true throughout. So
		// it is the coverage rule's own case, and it is also the shape of the
		// "a recurring defect is not a fix chain" bullet two paragraphs
		// below. #779's re-measurement is the reason this is not a hedge
		// either: every wrong edge it found joined two notes that were BOTH
		// still true, and an edge on this pair demotes a live OPEN report to
		// "resolved" — the one direction the KEEP-bias error argument does
		// not cover, since no ordinary pass will look at it again.
		want:  RelationNeither,
		wrong: RelationCauses,
	},
	{
		key:          "parallel-events",
		name:         "first block forged vs a later binary upgrade, same host",
		newer:        "The producer on host-a moved off an earlier partial backport onto the published build v1.2.0, which carries the complete rounding fix and everything else shipped since. Rollout: build on the host, copy the binary across, point the unit at the new path, reload the manager, restart. Verified afterwards: signing credentials accepted, registration still valid, the persisted schedule reloaded intact, and the head caught up.",
		newerCreated: "2023-06-30 07:00:00",
		older:        "The producer on host-a (build from the release branch) produced its first block at slot 41-120-400 with no errors and no double-sign, while both standby replicas stayed at leader_enabled=0. The remaining scheduled slots in that epoch were 41-180-900 and 41-260-300.",
		olderCreated: "2023-06-29 20:00:00",
		want:         RelationNeither,
		wrong:        RelationSupersedes,
	},
}

// regressionCase looks a labeled case up by its fixture key, so a failure names
// the shape it is about.
func regressionCase(t *testing.T, key string) relationCase {
	t.Helper()
	for _, c := range regressionRelationCases {
		if c.key == key {
			return c
		}
	}
	t.Fatalf("no labeled case with key %q", key)
	return relationCase{}
}

// seedRegressionCases loads every labeled pair into the store, one orthogonal
// embedding axis per pair so only the intended pairs clear the similarity
// floor, and returns each case's {newerID, olderID} in table order. The
// updated_at stamps reproduce the pass's orientation, which for the first case
// disagrees with created_at on purpose.
func seedRegressionCases(t *testing.T, store *memory.Store, db *sql.DB) [][2]string {
	t.Helper()
	axes := [][]float32{
		{1, 0, 0, 0}, {0, 1, 0, 0}, {0, 0, 1, 0}, {0, 0, 0, 1},
	}
	if len(regressionRelationCases) != len(axes) {
		t.Fatalf("seedRegressionCases covers %d case(s) with %d axes", len(regressionRelationCases), len(axes))
	}
	out := make([][2]string, 0, len(regressionRelationCases))
	for i, c := range regressionRelationCases {
		updated := c.newerUpdated
		if updated == "" {
			updated = c.newerCreated
		}
		newer := addStamped(t, store, db, c.newer, axes[i], c.newerCreated, updated)
		older := addStamped(t, store, db, c.older, nudge(axes[i]), c.olderCreated, c.olderCreated)
		out = append(out, [2]string{newer, older})
	}
	return out
}

// nudge returns v tilted towards the next axis: near-identical to v (cosine
// ~1) and orthogonal to every other pair's axis.
func nudge(v []float32) []float32 {
	out := make([]float32, len(v))
	copy(out, v)
	for i, f := range out {
		if f == 1 {
			out[i] = 0.999
			out[(i+1)%len(out)] = 0.001
			break
		}
	}
	return out
}

// TestRunRefusesReversedSupersedesOnRealData is the code half of #641. The
// labeled verdict for the "reversed" pair is REVERSED — the fix note is the
// current truth, the bug list was re-saved after the fix was written — and Run
// must refuse it: no link in either direction, and no NEITHER
// cache row (which would skip the pair forever and freeze the staleness bug
// the pass exists to fix).
//
// A bare "SUPERSEDES" cannot be refused in code — one word carries no
// direction, which is exactly why the prompt now offers REVERSED and shows
// each note's created_at. The tests below pin that the prompt offers the
// escape; this one pins that Run takes it.
func TestRunRefusesReversedSupersedesOnRealData(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	c := regressionCase(t, "reversed")
	newer := addStamped(t, store, db, c.newer, []float32{1, 0, 0, 0}, c.newerCreated, c.newerUpdated)
	older := addStamped(t, store, db, c.older, nudge([]float32{1, 0, 0, 0}), c.olderCreated, c.olderCreated)

	// Single-pair path (one candidate), so the lone-tail prompt and the
	// one-word parser are the ones under test. The pair carries no link, so
	// the pass must not report dropping one.
	var buf bytes.Buffer
	cls := NewRelationClassifier(&fakeProvider{resp: "REVERSED"})
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(buf.String(), "dropped the links of a reversed pair") {
		t.Errorf("logged a link drop for a pair that had no link:\n%s", buf.String())
	}

	if res.Reversed != 1 {
		t.Errorf("Reversed = %d, want 1 (%s)", res.Reversed, c.name)
	}
	if res.Confirmed != 0 || res.Created != 0 {
		t.Errorf("a refused verdict must not count as confirmed: confirmed=%d created=%d", res.Confirmed, res.Created)
	}
	rels := make([]Relation, 0, len(classified))
	for _, cl := range classified {
		rels = append(rels, cl.Relation)
	}
	if len(rels) != 1 || rels[0] != RelationReversed {
		t.Errorf("classified relations = %v, want [%q]", rels, RelationReversed)
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Errorf("refused verdict wrote %d supersedes link(s); the stale bug list must not supersede its own fix", len(pairs))
	}
	links, _ := store.GetLinks(ctx, newer)
	for _, l := range links {
		if l.Relation == string(RelationSupersedes) {
			t.Errorf("backwards supersedes link written: %+v", l)
		}
	}
	checked, err := store.SupersedeChecked(ctx, "p")
	if err != nil {
		t.Fatalf("SupersedeChecked: %v", err)
	}
	if _, ok := checked[[2]string{newer, older}]; ok {
		t.Errorf("refused verdict cached as NEITHER: %v — the pair would never be re-asked", checked)
	}
}

// TestRunReversedVerdictLeavesPairOpenForReclassification pins the half of the
// refusal above that a cache row would defeat: the next pass must classify the
// pair again rather than skip it, so a prompt improvement can still land the
// link Ghost could not write this time.
func TestRunReversedVerdictLeavesPairOpenForReclassification(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	c := regressionCase(t, "reversed")
	addStamped(t, store, db, c.newer, []float32{1, 0, 0, 0}, c.newerCreated, c.newerUpdated)
	addStamped(t, store, db, c.older, nudge([]float32{1, 0, 0, 0}), c.olderCreated, c.olderCreated)

	first := NewRelationClassifier(&fakeProvider{resp: "REVERSED"})
	if _, _, err := Run(ctx, store, first, "p", 0.9, true, slog.Default()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	second := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, _, err := Run(ctx, store, second, "p", 0.9, true, slog.Default())
	if err != nil {
		t.Fatalf("Run (second): %v", err)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0: a refused verdict must not become a cache hit", res.Skipped)
	}
	if second.Calls() != 1 {
		t.Errorf("second pass made %d classify call(s), want 1 (the pair must be re-asked)", second.Calls())
	}
}

// TestRunReversedVerdictInvalidatesBackwardsLink: a reclassify pair arrives
// carrying the backwards link #641 found on real data. The current verdict
// contradicts it, so the link goes — the same self-healing the NEITHER verdict
// already does, and the only way a wrong-direction link ever leaves the graph.
func TestRunReversedVerdictInvalidatesBackwardsLink(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	c := regressionCase(t, "reversed")
	// Orthogonal embeddings: the pair must reach the classifier through the
	// reclassify path (an existing llm link), not as a fresh candidate, so this
	// also covers the created_at copy that path makes.
	newer := addStamped(t, store, db, c.newer, []float32{1, 0, 0, 0}, c.newerCreated, c.newerUpdated)
	older := addStamped(t, store, db, c.older, []float32{0, 1, 0, 0}, c.olderCreated, c.olderCreated)
	if err := store.CreateLink(ctx, newer, older, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	// A previous pass judged the pair CAUSES; the reversed verdict contradicts
	// that too, so this link goes as well.
	if err := store.CreateLink(ctx, older, newer, "causes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	// Backdate the link so the reclassify path (not skip-if-unchanged) fires.
	if _, err := db.ExecContext(ctx,
		`UPDATE memory_links SET created_at = '2020-01-01 00:00:00' WHERE source_id = ? AND target_id = ?`,
		newer, older,
	); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cls := NewRelationClassifier(&fakeProvider{resp: "REVERSED"})
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reversed != 1 {
		t.Errorf("Reversed = %d, want 1", res.Reversed)
	}
	// This pair carried both relations, so the drop is a real graph change and
	// has to be visible at Info in lifecycle.log.
	if !strings.Contains(buf.String(), "dropped the links of a reversed pair") {
		t.Errorf("a real link drop must be logged at Info:\n%s", buf.String())
	}
	if pairs, _ := store.SupersedesWithin(ctx, []string{newer, older}); len(pairs) != 0 {
		t.Errorf("backwards link survived a reversed verdict: %d supersedes pair(s) remain", len(pairs))
	}
	links, err := store.GetLinks(ctx, older)
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	for _, l := range links {
		if l.Relation == string(RelationCauses) {
			t.Errorf("causes link survived a reversed verdict: %+v", l)
		}
	}
	// The reclassify path loads its content from the store, so it has to copy
	// created_at across too — that is the pair where updated_at is newest and
	// created_at oldest, and the model needs both ends to see it.
	user := cls.client.(*fakeProvider).lastUserContent
	if !strings.Contains(user, c.newerCreated) {
		t.Errorf("reclassify pair reached the classifier without the newer note's created_at:\n%s", user)
	}
	if !strings.Contains(user, c.olderCreated) {
		t.Errorf("reclassify pair reached the classifier without the older note's created_at:\n%s", user)
	}
}

// TestRunAppliesLabeledRealDataVerdicts drives all four real pairs through one
// batched call with the verdicts the fixed prompt is supposed to produce, and
// pins the resulting graph: the ONE status-report pair whose later report really
// does retire the earlier one supersedes, and the reversed pair, the
// still-open-report pair and the two parallel events on one host get nothing.
//
// One of them no longer reaches the classifier at all. "status-report-fix"'s
// older note says the build "never gets past" the missing fix, which reads as a
// rule to the imperative vocabulary the veto shares with resolve, and its newer
// note never retires one — so the veto settles it as no-edge, for free, before
// any call (#686). The cost is a missed staleness link: that older note stays
// ranked beside its replacement. It is the cheap direction (a duplicate pair
// visible in search) rather than a buried memory, it is what the issue's rule
// asks for, and the expectation is pinned BY NAME here so narrowing the veto
// has to be a deliberate change to this test.
func TestRunAppliesLabeledRealDataVerdicts(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	ids := seedRegressionCases(t, store, db)

	cls := NewRelationClassifier(newLabeledProvider(func(c relationCase) Relation { return c.want }, replacedClaim))
	res, classified, err := Run(ctx, store, cls, "p", 0.9, true, slog.Default())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	vetoed := map[string]bool{"status-report-fix": true}
	judged := len(regressionRelationCases) - len(vetoed)
	if res.Candidates != len(regressionRelationCases) {
		t.Fatalf("Candidates = %d, want %d (one pair per labeled case)", res.Candidates, len(regressionRelationCases))
	}
	if res.Vetoed != len(vetoed) {
		t.Errorf("Vetoed = %d, want %d", res.Vetoed, len(vetoed))
	}
	if cls.Calls() != 1 {
		t.Errorf("classify calls = %d, want 1 batched call for the %d un-vetoed pair(s)", cls.Calls(), judged)
	}
	if res.Reversed != 1 {
		t.Errorf("Reversed = %d, want 1 (%s)", res.Reversed, regressionCase(t, "reversed").name)
	}
	// Zero, and stated as zero rather than as the count that happened to be
	// true. NEITHER status-report pair reaches a confirmed edge:
	// status-report-fix is settled by the imperative veto before any call (see
	// the test's comment), and status-report-divergence is NEITHER by label
	// since #779, because its later report is a second sighting of a still-open
	// problem rather than a retirement of the first. Before #779 the divergence
	// half confirmed, and that edge pointed at a live OPEN report — the one
	// direction the KEEP-bias error argument does not cover. The per-pair
	// assertions below still pin the graph case by case, so this count is a
	// statement about the SHAPE of #641's two status-report pairs rather than
	// the only thing holding the graph down.
	if res.Confirmed != 0 {
		t.Errorf("Confirmed = %d, want 0: one of the two status-report pairs is vetoed and the other is NEITHER by label, so a confirmed edge here points at a live OPEN report", res.Confirmed)
	}
	// Which pair got which verdict, by identity: the per-pair verdicts are the
	// fixture, so this fails if a reply ever lands on the wrong pair — which a
	// per-case link count alone would miss whenever two cases share a verdict.
	gotVerdict := make(map[string]Relation, len(classified))
	for _, cl := range classified {
		gotVerdict[cl.NewerID] = cl.Relation
	}
	for i, c := range regressionRelationCases {
		newer, older := ids[i][0], ids[i][1]
		want := c.want
		wantLink := c.want == RelationSupersedes
		if vetoed[c.key] {
			want = ""
			wantLink = false
			if _, settled := VetoSupersede(Candidate{OlderContent: c.older, NewerContent: c.newer}); !settled {
				t.Errorf("%s: the fixture claims the veto settles this pair, but VetoSupersede did not", c.name)
			}
		}
		if got := gotVerdict[newer]; got != want {
			t.Errorf("%s: verdict = %q, want %q", c.name, got, want)
		}
		pairs, err := store.SupersedesWithin(ctx, []string{newer, older})
		if err != nil {
			t.Fatalf("SupersedesWithin: %v", err)
		}
		if wantLink && len(pairs) != 1 {
			t.Errorf("%s: want one supersedes link, got %d", c.name, len(pairs))
		}
		if !wantLink && len(pairs) != 0 {
			t.Errorf("%s (%s): want no link, got %d", c.name, want, len(pairs))
		}
		// No labeled case in this set is labeled CAUSES — the prompt rules
		// those out — so ANY causes link here is one the pass wrote against the
		// fixture. The assertion is unconditional on purpose: narrowing it to
		// the cases whose own `wrong` is CAUSES would stop catching a spurious
		// edge on any other pair.
		links, _ := store.GetLinks(ctx, older)
		for _, l := range links {
			if l.Relation == string(RelationCauses) {
				t.Errorf("%s: a causes link was written: %+v", c.name, l)
			}
		}
	}
}

// TestRunLeavesTheThreePromptOnlyPairsToThePrompt records where #641's fix
// stops and the prompt starts. The other three wrong verdicts are semantic
// (a misused `causes`, a superseded event record) and no code can tell them
// from a correct verdict of the same shape, so the pass writes whatever it is
// told; the rules that stop those three live in the prompt, pinned by the
// TestClassifierPromptRefuses* tests below.
//
// It is the "status-report-divergence" pair that carries the misused `causes`,
// not its "status-report-fix" sibling: the latter's older note says the build
// "never gets past" the missing fix, so the imperative veto settles it before
// the model is asked anything (see TestRunAppliesLabeledRealDataVerdicts). One
// misused `causes` out of two labeled status reports is the shape the prompt
// rule has to catch.
func TestRunLeavesTheThreePromptOnlyPairsToThePrompt(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	ids := seedRegressionCases(t, store, db)

	cls := NewRelationClassifier(newLabeledProvider(func(c relationCase) Relation { return c.wrong }, replacedClaim))
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.Default())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reversed != 0 {
		t.Errorf("Reversed = %d, want 0: a bare SUPERSEDES names no direction for code to refuse", res.Reversed)
	}
	// The reversed case's wrong verdict is SUPERSEDES, so this pass writes the
	// backwards link #641 reported — the exact harm the prompt rules remove by
	// offering REVERSED. Asserting it here would pin the bug, so the count is
	// only reported.
	t.Logf("with the pre-fix verdict set: confirmed=%d created=%d causes=%d reversed=%d vetoed=%d (the first case is the backwards link #641 found)",
		res.Confirmed, res.Created, res.CausesCreated, res.Reversed, res.Vetoed)

	// The misused causes link is the one the prompt now forbids, so it must be
	// the one the pass still writes when the prompt does not forbid it.
	divergence := regressionCase(t, "status-report-divergence")
	causes, err := store.GetLinks(ctx, ids[2][1])
	if err != nil {
		t.Fatalf("GetLinks: %v", err)
	}
	found := false
	for _, l := range causes {
		if l.Relation == string(RelationCauses) && l.SourceID == ids[2][1] && l.TargetID == ids[2][0] {
			found = true
		}
	}
	if !found {
		t.Errorf("expected the misused causes link for %s to be written when the prompt does not forbid it (see TestClassifierPromptRefusesCausesBetweenStatusReports)", divergence.name)
	}
}

// batchPairPattern matches one rendered pair of the batch content. The label
// between "OLDER " and the « is skipped with [^«\n]* because a timestamp
// carries colons; quoteData rewrites any « or » inside the note text, so the
// first « and the » after it are the pair's own.
var batchPairPattern = regexp.MustCompile(`(?s)OLDER [^«\n]*«(.*?)»\nNEWER [^«\n]*«(.*?)»`)

// labeledProvider answers a call with the verdict each pair deserves, matched by
// the note text the prompt actually carries. It replaces a canned numbered
// reply because batch numbering is POSITIONAL: SelectCandidates emits
// candidates in GetAll's order (importance DESC, created_at DESC), not the
// fixture table's order, so a fixed "1: …, 2: …, 3: …" reply grades whichever
// pairs happen to come first — an accidentally green test.
//
// A SUPERSEDES line also carries claim(case) in its required `replaced:` field
// (issue #686), because a verdict that cannot name what the older note claimed
// is NEITHER — so a fixture graded through this provider has to speak the
// current output contract or it grades the parser, not the pass.
type labeledProvider struct {
	byNewer map[string]Relation
	claim   func(relationCase) string
	last    string
}

// newLabeledProvider builds a provider that answers each labeled pair with
// verdict(case), in whatever order the pass emits the pairs, carrying
// claim(case) as the `replaced:` value of every SUPERSEDES line.
func newLabeledProvider(verdict func(relationCase) Relation, claim func(relationCase) string) *labeledProvider {
	p := &labeledProvider{
		byNewer: make(map[string]Relation, len(regressionRelationCases)),
		claim:   claim,
	}
	for _, c := range regressionRelationCases {
		p.byNewer[c.newer] = verdict(c)
	}
	return p
}

// replacedClaim is a fixed, synthetic `replaced:` value standing for "the claim
// this case's older note made". The parser judges only that a value is present
// and non-empty, never what it says, so the exact wording carries nothing — and
// a fixed wording keeps these fixtures free of any real memory text.
func replacedClaim(relationCase) string {
	return "the claim the older note made"
}

// Classify answers one call. An unknown note is a hard error, not a silent
// NEITHER: a fixture the lookup cannot place means the test is grading
// something it did not set up.
func (p *labeledProvider) Classify(_ context.Context, _, userContent string) (string, error) {
	matches := batchPairPattern.FindAllStringSubmatch(userContent, -1)
	if len(matches) == 0 {
		return "", errors.New("labeled provider: no pairs in the rendered content")
	}
	lines := make([]string, 0, len(matches))
	for _, m := range matches {
		rel, ok := p.byNewer[m[2]]
		if !ok {
			return "", fmt.Errorf("labeled provider: no verdict for newer note %.60q", m[2])
		}
		line := strconv.Itoa(len(lines)+1) + ": " + strings.ToUpper(string(rel))
		if rel == RelationSupersedes {
			line += " | replaced: " + p.claim(regressionCaseByNewer(m[2]))
		}
		lines = append(lines, line)
	}
	p.last = strings.Join(lines, "\n")
	return p.last, nil
}

// regressionCaseByNewer looks a labeled case up by its newer note's text, the
// key labeledProvider matches on.
func regressionCaseByNewer(newer string) relationCase {
	for _, c := range regressionRelationCases {
		if c.newer == newer {
			return c
		}
	}
	return relationCase{}
}

// promptFor classifies one labeled case and returns the system prompt and the
// rendered user content the harness saw. One pair means the lone-tail path, so
// this is the SINGLE-pair prompt; the batch contract is asserted separately in
// TestRelationClassifierBatchMapsNumberedLines, which is why the
// "both contracts" claim in TestClassifierPromptOffersReversedVerdict holds.
func promptFor(t *testing.T, c relationCase) (system, user string) {
	t.Helper()
	fp := &fakeProvider{resp: "1: NEITHER"}
	cls := NewRelationClassifier(fp)
	if _, err := cls.ClassifyBatch(context.Background(), []Candidate{{
		NewerID: "n", NewerContent: c.newer, NewerCreatedAt: c.newerCreated,
		OlderID: "o", OlderContent: c.older, OlderCreatedAt: c.olderCreated,
	}}); err != nil {
		t.Fatalf("ClassifyBatch: %v", err)
	}
	return fp.lastSystem, fp.lastUserContent
}

// TestClassifierPromptCarriesBothCreatedAt: the classifier cannot see which
// note is older in substance — the pass only tells it which endpoint is newer
// by updated_at, and updated_at is exactly the signal that is wrong when a
// stale claim gets re-asserted (the "reversed" fixture). created_at is the only
// ordering that says which note the fix landed in.
func TestClassifierPromptCarriesBothCreatedAt(t *testing.T) {
	c := regressionCase(t, "reversed")
	_, user := promptFor(t, c)
	// Asserted against the case's own stamps, so the test does not restate them.
	for _, stamp := range []string{c.newerCreated, c.olderCreated} {
		if !strings.Contains(user, stamp) {
			t.Errorf("created_at %q missing from the prompt:\n%s", stamp, user)
		}
	}
}

// TestClassifierPromptOffersReversedVerdict: a four-way answer is the only way
// a model can decline a direction, and Run refuses exactly that verdict.
// promptFor renders the single-pair contract; the batch contract's "VERDICT is
// SUPERSEDES, CAUSES, NEITHER, or REVERSED" is asserted in
// TestRelationClassifierBatchMapsNumberedLines.
//
// The single-pair contract prints one answer per line rather than the old
// "respond with exactly one word: SUPERSEDES, CAUSES, NEITHER, or REVERSED",
// because a SUPERSEDES answer now carries a `replaced:` claim (#686) — so the
// assertion is on REVERSED being offered as a bare answer line, which is what
// "offers the verdict" means for that shape.
func TestClassifierPromptOffersReversedVerdict(t *testing.T) {
	system, _ := promptFor(t, regressionCase(t, "reversed"))
	if !strings.Contains(system, "REVERSED") {
		t.Errorf("prompt does not offer a REVERSED verdict:\n%s", system)
	}
	if !strings.Contains(system, "\nREVERSED\n") {
		t.Errorf("output contract does not list REVERSED alongside the other verdicts:\n%s", system)
	}
	if !strings.Contains(system, "never writes a supersedes link backwards") {
		t.Errorf("prompt does not say Ghost refuses a backwards link:\n%s", system)
	}
}

// TestClassifierPromptRefusesCausesBetweenStatusReports: #641's second wrong
// link was a CAUSES verdict between two status reports of one open issue. No
// code can tell a status report from a rationale, so the rule has to live in
// the prompt — and the test pins the text so a later rubric edit cannot drop it.
func TestClassifierPromptRefusesCausesBetweenStatusReports(t *testing.T) {
	system, _ := promptFor(t, regressionCase(t, "status-report-fix"))
	if !strings.Contains(system, "never CAUSES") {
		t.Errorf("prompt does not forbid CAUSES between two status reports of one issue:\n%s", system)
	}
	if !strings.Contains(system, "status report") {
		t.Errorf("prompt no longer names the status-report case:\n%s", system)
	}
}

// TestClassifierPromptRefusesSupersedingParallelEvents: #641's fourth wrong
// link superseded a forged block with a later binary upgrade on the same host.
// A host name is not a shared fact, and an event record is never obsolete.
func TestClassifierPromptRefusesSupersedingParallelEvents(t *testing.T) {
	system, _ := promptFor(t, regressionCase(t, "parallel-events"))
	if !strings.Contains(system, "event record") {
		t.Errorf("prompt no longer names the event-record case:\n%s", system)
	}
	if !strings.Contains(system, "same host") {
		t.Errorf("prompt does not rule out the shared-host confusion:\n%s", system)
	}
}

// TestRunReclassifiesPairsCachedUnderTheOldRubric: the NEITHER cache keys on a
// content hash whose version prefix is bumped when the rubric changes, so every
// verdict judged by the old rules is re-asked in one step — the same reset
// resolve's "v2" prefix performed. Seeding a v1 row must therefore NOT skip the
// pair; with the prefix still at v1 the row matches and the pair is silently
// frozen for the life of the text.
func TestRunReclassifiesPairsCachedUnderTheOldRubric(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()
	c := regressionCase(t, "status-report-divergence")
	newer := add(t, store, db, c.newer, []float32{1, 0, 0, 0}, c.newerCreated)
	older := add(t, store, db, c.older, []float32{0.999, 0.001, 0, 0}, c.olderCreated)

	// A row written by a pass running the pre-#686 rubric, whose prefix is v2.
	// The prefix moved to v3 with the `replaced:` rule, so this row no longer
	// matches and the pair is re-asked.
	old := func(content string) string {
		sum := sha256.Sum256([]byte("v2\x00" + content))
		return hex.EncodeToString(sum[:])
	}
	if err := store.MarkSupersedeNeither(ctx, "p", map[[2]string]memory.SupersedeCheck{
		{newer, older}: {NewerHash: old(c.newer), OlderHash: old(c.older)},
	}); err != nil {
		t.Fatalf("seed cache: %v", err)
	}

	cls := NewRelationClassifier(&fakeProvider{resp: "NEITHER"})
	res, _, err := Run(ctx, store, cls, "p", 0.9, true, slog.Default())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Skipped != 0 {
		t.Errorf("Skipped = %d, want 0: a verdict cached under the old rubric must be re-asked", res.Skipped)
	}
	if cls.Calls() != 1 {
		t.Errorf("classify calls = %d, want 1 (the stale cache row did not match)", cls.Calls())
	}
}
