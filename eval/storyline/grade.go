package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
)

// Check is one graded property of a finished run: a name a report can key on, a
// verdict, and a detail line that says what was observed. A check with no
// measurable observation behind it is not a check, which is why the judge check
// exists only when the judge ran (see judgedCheck).
type Check struct {
	Name   string
	Passed bool
	Detail string
	// Advisory marks a check that is reported but does not decide the run: the
	// judge, which is a model's opinion and not a reading of the run. Result.Passed
	// and FailedNames skip it, so an advisory column can disagree with the
	// deterministic grade without flipping a verdict or an exit code.
	Advisory bool
	// Known marks a failed check the storyline lists in KnownFailing: reported,
	// and not gating, because the failure is a filed finding rather than news.
	// UnexpectedPass marks a listed check that passed, which is news too.
	Known          bool
	UnexpectedPass bool
}

// grade is the whole deterministic grade, and it reads three sources and no
// others: the blocks the run rendered, the agent's answers, and the rows the
// store ended up holding.
//
// It is a pure function of the Result so it can be exercised against a hand-built
// arc without a ghost binary, an embedding endpoint or a model. Two properties
// matter for that to be worth anything:
//
//   - The STORE is what the arc stages are graded on. The CLI's prose is kept in
//     the report for a human, never parsed: a check that matched "superseded" in
//     stdout would pass on a wording change and fail on a rename.
//   - Every check names the record keys it is about, so a failing check says
//     which claim of the storyline broke rather than only that something did.
//
// There are two kinds of line per session and they answer different questions.
// The DELIVERY lines (injection-present, carry-forward, stale-original,
// expired-withheld) read the block: did Ghost hand the session the claim. The
// ANSWER lines (answer-carries, answer-avoids) read what the agent said: did the
// claim change what it did. Delivery without an answer is a block nobody used;
// an answer without delivery is a script that leaked, which is what the without-
// Ghost arm exists to expose.
//
// In the without-Ghost arm the block is empty by construction, so the delivery
// lines and the store lines say nothing the arm's definition does not already
// say, and only the answer lines are graded.
func grade(res *Result) []Check {
	var out []Check
	s := res.Story
	stages := s.Stages
	if len(res.Sessions) < len(stages) {
		stages = stages[:len(res.Sessions)]
	}
	for i, sess := range res.Sessions {
		if !res.WithoutGhost {
			out = append(out, injectionPresent(sess))
			for _, key := range stages[i].Expect {
				out = append(out, carryForward(sess, mustRecord(s, key)))
			}
			stales := supersededBefore(s, i)
			for _, stale := range stales {
				out = append(out, staleAbsentAs("stale-original:"+staleSite(sessionName(sess.Index), stale, len(stales)), sessionName(sess.Index), sess.Block, stale, mustRecord(s, stale.SupersededBy)))
			}
			for _, old := range expiredBefore(s, i) {
				out = append(out, expiredWithheld(sessionName(sess.Index), sess.Block, old))
			}
		}
		if !res.WithoutGhost {
			for _, key := range stages[i].Withheld {
				out = append(out, withheld(sessionName(sess.Index), sess.Block, mustRecord(s, key)))
			}
			for _, p := range stages[i].Probes {
				raw, ran := sess.Probes[p.Name]
				out = append(out, probeChecks(res, p, sessionName(sess.Index)+":"+p.Name, raw, ran)...)
			}
		}
		// Under the fake harness the answer is a fixed string, so an answer line
		// would grade the fake. Those runs grade the block and the store only.
		if res.FakeHarness {
			continue
		}
		for _, c := range stages[i].Carries {
			out = append(out, answerCarries(sess, c))
		}
		for _, c := range stages[i].Avoids {
			out = append(out, answerAvoids(sess, c))
		}
	}
	if res.WithoutGhost {
		return applyKnown(s, out)
	}
	// The final block is graded on its own, because it is the one a reader of the
	// report cares about: what the NEXT session of this project would be told.
	finalStale := supersededBefore(s, len(s.Stages))
	for _, stale := range finalStale {
		out = append(out, staleAbsentAs("stale-original:"+staleSite("final-block", stale, len(finalStale)), "final-block", res.FinalBlock, stale, mustRecord(s, stale.SupersededBy)))
	}
	for _, old := range expiredBefore(s, len(s.Stages)) {
		out = append(out, expiredWithheld("final-block", res.FinalBlock, old))
	}
	for _, pair := range reversals(s) {
		out = append(out, supersedeEdge(res, pair))
		// In a chain (a fix that later regressed) the middle record is itself
		// superseded, so "must stay live" and "the final block carries it" are
		// asked of the chain's head only; the middle one is graded stale instead.
		if pair.newer.SupersededBy == "" {
			out = append(out, reversalLive(res, pair))
			out = append(out, finalBlockCarries(res, pair.newer))
		}
		out = append(out, distinctSave(res, pair))
	}
	for _, l := range s.Lures {
		out = append(out, lureRefused(res, mustRecord(s, l.Older), mustRecord(s, l.Newer)))
	}
	for _, p := range s.Probes {
		raw, ran := res.Probes[p.Name]
		out = append(out, probeChecks(res, p, p.Name, raw, ran)...)
	}
	// Efficiency arcs for #1831, #1834, #1832.
	switch s.Key {
	case "pins-over-bucket":
		out = append(out, pinsOverflowNoticed(res))
	case "duplicate-never-merged":
		out = append(out, duplicateMergedBlock(res))
		out = append(out, duplicateMergedStore(res))
	case "dated-global-status":
		out = append(out, datedGlobalWithheld(res))
		out = append(out, datedGlobalAdvisory(res))
	}
	return applyKnown(s, out)
}

// pinsOverflowNoticed checks that the owner was told the bucket overflowed.
// #1831 requires the pin result or the health output to carry the notice; the
// grade reads the save outputs for the pins that push the bucket over its cap.
// The block's own pinned-cut line is not the surface the issue grades, which is
// why the check looks at the save path rather than the block.
func pinsOverflowNoticed(res *Result) Check {
	name := "pins-overflow-noticed:pin-result"
	// The 16th and 17th pins are the ones that overflow the 15-slot bucket.
	for _, key := range []string{"pin-16", "pin-long"} {
		raw := res.SaveOutputs[key]
		if raw == "" {
			raw = res.OpeningRaw[key]
		}
		low := strings.ToLower(raw)
		if strings.Contains(low, "pinned") && strings.Contains(low, "exceed") {
			return Check{Name: name, Passed: true, Detail: "pin result reports that pinned rows exceed the cap"}
		}
		if strings.Contains(low, "overflow") || strings.Contains(low, "cut") && strings.Contains(low, "pinned") {
			return Check{Name: name, Passed: true, Detail: "pin result reports overflow"}
		}
	}
	return Check{Name: name, Passed: false, Detail: "pin result does not report that pinned rows exceed the cap"}
}

// duplicateMergedBlock checks that the rule saved twice in near-identical words
// appears once in the final block. The store holds the rule, the block is what
// the next session is handed, and showing it twice is the same defect as
// holding it twice.
func duplicateMergedBlock(res *Result) Check {
	name := "duplicate-merged:block-shows-once"
	// The shared phrase both records contain (lower-cased for the count).
	needle := "explicit approval"
	block := strings.ToLower(res.FinalBlock)
	count := strings.Count(block, needle)
	if count == 1 {
		return Check{Name: name, Passed: true, Detail: "final block shows the rule once"}
	}
	if count == 0 {
		return Check{Name: name, Passed: false, Detail: "final block does not show the rule at all"}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("final block shows the rule %d times, want once", count)}
}

// duplicateMergedStore checks that the store holds one live row for the rule
// rather than two. The two saves are near-identical, so the save path should
// have folded the second into the first; the fake-harness lifecycle runs, then
// the graded store read counts what is there.
func duplicateMergedStore(res *Result) Check {
	name := "duplicate-merged:store-holds-one"
	// Total live memories: opening (1) plus the one merged rule = 2. Two live
	// duplicate rows would be 3. The count is the store's own, via State.Stamps.
	n := len(res.State.Stamps)
	if n == 2 {
		return Check{Name: name, Passed: true, Detail: "store holds one live row for the rule (2 memories total with the opening)"}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("store holds %d memories, want 2 (one live row for the rule)", n)}
}

// datedGlobalWithheld checks that an expired global rollout status is not in the
// final block. The global is visible to every project, so a later session in a
// different project must not carry it once its valid_until has passed.
func datedGlobalWithheld(res *Result) Check {
	name := "dated-global-withheld:final-block:rollout-status"
	mark := "Relay v2.3.1 is live in prod"
	if strings.Contains(res.FinalBlock, mark) {
		return Check{Name: name, Passed: false, Detail: "final block still carries the expired global rollout status"}
	}
	return Check{Name: name, Passed: true, Detail: "final block does not carry the expired global rollout status"}
}

// datedGlobalAdvisory checks that saving a dated rollout status without a
// valid_until advises the caller to add one. The save without a window is the
// defect that lets undated status linger forever; the advisory is the pin that
// makes the next save carry a window. The grade reads the tool's raw output for
// that record, which the runner now records per key.
func datedGlobalAdvisory(res *Result) Check {
	name := "dated-global-advisory:rollout-status-undated"
	raw := res.SaveOutputs["rollout-status-undated"]
	if raw == "" {
		raw = res.OpeningRaw["rollout-status-undated"]
	}
	low := strings.ToLower(raw)
	if strings.Contains(low, "valid_until") {
		return Check{Name: name, Passed: true, Detail: "save without valid_until advises to add one"}
	}
	return Check{Name: name, Passed: false, Detail: "save without valid_until does not advise to add valid_until"}
}

// staleSite names a stale-original line. One superseded record per site keeps
// the name the line has always had; a site with several names each record, so
// every line has a name of its own for a report, a summary and KnownFailing to
// key on.
func staleSite(where string, stale Record, n int) string {
	if n > 1 {
		return where + ":" + stale.Key
	}
	return where
}

// applyKnown turns the storyline's KnownFailing list into verdicts: a listed
// check that failed is Known (reported, not gating), and a listed check that
// passed is an UnexpectedPass, reported so the entry can be deleted.
func applyKnown(s Storyline, checks []Check) []Check {
	for i, c := range checks {
		why, ok := s.KnownFailing[c.Name]
		if !ok || c.Advisory {
			continue
		}
		if c.Passed {
			checks[i].UnexpectedPass = true
			checks[i].Detail += " (listed as known-failing: " + why + ")"
			continue
		}
		checks[i].Known = true
		checks[i].Detail += " (known: " + why + ")"
	}
	return checks
}

// withheld is a delivery line for a record that must not be in a block for a
// reason the storyline names (Stage.Withheld): a record scoped elsewhere.
func withheld(where, block string, r Record) Check {
	name := "withheld:" + where + ":" + r.Key
	if !strings.Contains(block, r.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s is not in %s", r.Key, where)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
		"%s (%s%s) is in %s, which is rendered for a session it does not apply to", r.Key, r.Mark, scopeNote(r.Scope), where)}
}

func scopeNote(scope map[string]string) string {
	if len(scope) == 0 {
		return ""
	}
	keys := make([]string, 0, len(scope))
	for k := range scope {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+scope[k])
	}
	return ", scoped " + strings.Join(parts, ",")
}

// distinctSave grades the save path on a reversal: the newer record must not
// have been filed as a near-duplicate of the claim it replaces. A 'duplicate'
// link says the two rows are one claim in two wordings, which is the opposite
// of what a replacement is, and it is read off the store's own links.
func distinctSave(res *Result, pair reversal) Check {
	name := "distinct-save:" + pair.newer.Key
	newer, older := res.idOf(pair.newer.Key), res.idOf(pair.older.Key)
	if newer == "" || older == "" {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf("no id reached the grade for %s/%s", pair.newer.Key, pair.older.Key)}
	}
	for _, l := range res.State.Links {
		if l.Relation != "duplicate" || l.Invalidated {
			continue
		}
		if (l.Source == newer && l.Target == older) || (l.Source == older && l.Target == newer) {
			return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
				"%s was saved as a likely duplicate of %s, the claim it replaces", pair.newer.Key, pair.older.Key)}
		}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s is not linked as a duplicate of %s", pair.newer.Key, pair.older.Key)}
}

// lureRefused grades a lure: no active supersedes edge from the newer record to
// the older one may exist, and the older one must not have been resolved. The
// fake classifier calls the pair SUPERSEDES, so only Ghost's own guards can have
// kept it apart.
func lureRefused(res *Result, older, newer Record) Check {
	name := "lure-refused:" + newer.Key
	n, o := res.idOf(newer.Key), res.idOf(older.Key)
	if n == "" || o == "" {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf("no id reached the grade for %s/%s", newer.Key, older.Key)}
	}
	for _, l := range res.State.Links {
		if l.Source == n && l.Target == o && l.Relation == "supersedes" && !l.Invalidated {
			return Check{Name: name, Passed: false, Detail: fmt.Sprintf("%s supersedes %s although both still hold", newer.Key, older.Key)}
		}
	}
	if st, ok := res.State.Stamps[o]; ok && st.ResolvedAt != "" {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf("%s was resolved at %s although it still holds", older.Key, st.ResolvedAt)}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s and %s are both live and unlinked by supersedes", newer.Key, older.Key)}
}

// searchRow is one row of a ghost_memory_search answer, in rank order.
type searchRow struct {
	ID       string
	Resolved bool
	// Line is the row's whole line, which the marker check reads at its field.
	Line string
}

// searchRowRe reads one formatted search row: the shared item line
// assemble.Item.Line renders, "- [category] `id` (importance [resolved] …) «…»".
var searchRowRe = regexp.MustCompile(`(?m)^- \[[^\]]+\] ` + "`" + `([^` + "`" + `]+)` + "`" + ` \(([^\n]*)$`)

// parseSearchRows reads the ranked rows of a search answer. Only the row lines
// are read — never the outcome line — so the probe grades what was returned,
// not what the tool says about it.
func parseSearchRows(raw string) []searchRow {
	var out []searchRow
	for _, m := range searchRowRe.FindAllStringSubmatch(raw, -1) {
		meta, _, _ := strings.Cut(m[2], "«")
		out = append(out, searchRow{ID: m[1], Resolved: strings.Contains(meta, "[resolved]"), Line: m[0]})
	}
	return out
}

// probeChecks grades one probe against the rows it returned: search-finds (Want
// is returned), search-ranks (Want is above every Over record that is returned)
// search-excludes (no Absent record is returned) and search-marks (a Marked
// record returned together with Want carries the replaced marker at its field).
//
// site is the name the lines are keyed under: the probe's own name for an
// end-state probe, qualified by its session for a stage probe.
func probeChecks(res *Result, p Probe, site, raw string, ran bool) []Check {
	rows := parseSearchRows(raw)
	rank := map[string]int{}
	for i, r := range rows {
		if _, ok := rank[r.ID]; !ok {
			rank[r.ID] = i
		}
	}
	var out []Check
	want := mustRecord(res.Story, p.Want)
	wantID := res.idOf(p.Want)
	wantAt, found := rank[wantID]
	name := "search-finds:" + site
	switch {
	case !ran:
		out = append(out, Check{Name: name, Passed: false, Detail: "the probe did not run"})
	case wantID == "":
		out = append(out, Check{Name: name, Passed: false, Detail: "no id reached the grade for " + p.Want})
	case found:
		out = append(out, Check{Name: name, Passed: true, Detail: fmt.Sprintf("%q returns %s at rank %d of %d", p.Query, want.Key, wantAt+1, len(rows))})
	default:
		out = append(out, Check{Name: name, Passed: false, Detail: fmt.Sprintf("%q does not return %s (%d rows)", p.Query, want.Key, len(rows))})
	}
	for _, k := range p.Over {
		name := "search-ranks:" + site + ":" + k
		at, there := rank[res.idOf(k)]
		switch {
		case !there:
			out = append(out, Check{Name: name, Passed: found, Detail: fmt.Sprintf("%s is not returned; %s returned: %v", k, want.Key, found)})
		case found && wantAt < at:
			out = append(out, Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s (rank %d) is above %s (rank %d)", want.Key, wantAt+1, k, at+1)})
		case found:
			out = append(out, Check{Name: name, Passed: false, Detail: fmt.Sprintf(
				"%s (rank %d, resolved=%v) is above %s (rank %d), the record that replaced it", k, at+1, rows[at].Resolved, want.Key, wantAt+1)})
		default:
			out = append(out, Check{Name: name, Passed: false, Detail: fmt.Sprintf(
				"%s is returned at rank %d (resolved=%v) and %s is not returned at all", k, at+1, rows[at].Resolved, want.Key)})
		}
	}
	for _, k := range p.Marked {
		name := "search-marks:" + site + ":" + k
		at, there := rank[res.idOf(k)]
		switch {
		case !ran:
			out = append(out, Check{Name: name, Passed: false, Detail: "the probe did not run"})
		case !there:
			out = append(out, Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s is not returned", k)})
		case !found:
			// The marker names a row on the same page; with Want absent there is
			// nothing for it to point at, and search-finds already fails.
			out = append(out, Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s is returned without %s, so there is no replacement on the page to name", k, want.Key)})
		case carriesReplacedMarker(rows[at].Line):
			out = append(out, Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s (rank %d) is marked as replaced, with %s at rank %d", k, at+1, want.Key, wantAt+1)})
		default:
			out = append(out, Check{Name: name, Passed: false, Detail: fmt.Sprintf(
				"%s (rank %d) is returned with %s (rank %d) and nothing on its line marks it as replaced", k, at+1, want.Key, wantAt+1)})
		}
	}
	for _, k := range p.Absent {
		name := "search-excludes:" + site + ":" + k
		r := mustRecord(res.Story, k)
		if at, there := rank[res.idOf(k)]; there {
			out = append(out, Check{Name: name, Passed: false, Detail: fmt.Sprintf(
				"%q%s returns %s (%s%s) at rank %d", p.Query, probeScope(p), k, r.Mark, scopeNote(r.Scope), at+1)})
			continue
		}
		out = append(out, Check{Name: name, Passed: ran, Detail: fmt.Sprintf("%s is not returned", k)})
	}
	return out
}

func probeScope(p Probe) string {
	if len(p.Scope) == 0 {
		return ""
	}
	return " (scope" + strings.TrimPrefix(scopeNote(p.Scope), ", scoped") + ")"
}

// expiredBefore is every record whose valid_until had passed by the time session
// i was rendered and which already existed then: an opening record, or one an
// earlier session wrote. A record is expired by the clock, not by a later record,
// so it is the validity filter this grades and not supersede.
func expiredBefore(s Storyline, session int) []Record {
	var out []Record
	for _, r := range s.Order() {
		if r.ValidUntil == "" {
			continue
		}
		until, err := parseValidUntil(r.ValidUntil)
		if err != nil || !until.Before(time.Now()) {
			continue
		}
		if at, ok := s.stageOf(r.Key); ok && at < session {
			out = append(out, r)
		}
	}
	return out
}

// expiredWithheld is a delivery line: a record whose window has closed must not
// be in the block. Its wording does not say it is old, so only the validity
// filter can have kept it out.
func expiredWithheld(where, block string, old Record) Check {
	name := "expired-withheld:" + where + ":" + old.Key
	if !strings.Contains(block, old.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s (valid until %s) is not in %s", old.Key, old.ValidUntil, where)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
		"%s (%s) expired on %s and is still in %s", old.Key, old.Mark, old.ValidUntil, where)}
}

// containsToken reports whether needle appears in hay with no identifier
// character on either side, so "float" is not found in "floats" and
// "Idempotency-Key" is not found in "Idempotency-Keys". Both arguments are
// already lower-cased.
func containsToken(hay, needle string) bool {
	if needle == "" {
		return false
	}
	for from := 0; ; {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			return false
		}
		start, end := from+i, from+i+len(needle)
		if !identRuneAt(hay, start-1) && !identRuneAt(hay, end) {
			return true
		}
		from = start + 1
	}
}

func identRuneAt(s string, i int) bool {
	if i < 0 || i >= len(s) {
		return false
	}
	c := s[i]
	return c == '_' || c == '-' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// reversal pairs a record with the one that replaced it. The arc is expressed in
// the storylines' annotations and resolved to ids by the run, so the grade asks
// about the pair the storyline claims and not about whatever edge happened to be
// in the store.
type reversal struct {
	older, newer Record
}

// reversals is every annotated reversal, in storyline order.
func reversals(s Storyline) []reversal {
	var out []reversal
	for _, r := range s.Order() {
		if r.SupersededBy == "" {
			continue
		}
		out = append(out, reversal{older: r, newer: mustRecord(s, r.SupersededBy)})
	}
	return out
}

// supersededBefore is every record already replaced by the time session i is
// rendered — an earlier session's claim whose replacement an earlier session had
// already written. Those are the only records a session can be stale about: the
// reversal itself was recorded after the session that reverses it, so grading
// session 2 on it would grade the runner's own timeline.
func supersededBefore(s Storyline, session int) []Record {
	var out []Record
	for _, r := range s.Order() {
		if r.SupersededBy == "" {
			continue
		}
		if at, ok := s.stageOf(r.SupersededBy); ok && at < session {
			out = append(out, r)
		}
	}
	return out
}

// mustRecord is the graded lookup for a key Validate has already checked, so a
// miss here is a bug in the runner rather than a storyline to report on.
func mustRecord(s Storyline, key string) Record {
	r, ok := s.RecordByKey(key)
	if !ok {
		return Record{Key: key}
	}
	return r
}

// injectionPresent says a session was injected anything at all. A session with an
// empty block is a cold session with no memory at all, which is a different
// finding from one that was injected the wrong thing.
func injectionPresent(sess Session) Check {
	name := "injection-present:" + sessionName(sess.Index)
	if strings.TrimSpace(sess.Block) == "" {
		return Check{Name: name, Passed: false, Detail: "the session was injected nothing"}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%d bytes of injected context", len(sess.Block))}
}

// carryForward grades what a session had to be told and was not. The block is
// compared against the record's Mark — a distinctive verbatim substring of the
// content — rather than against the whole content, because the block renders a
// truncated or reworded line and the question is whether the CLAIM reached the
// session at all.
func carryForward(sess Session, want Record) Check {
	name := "carry-forward:" + sessionName(sess.Index)
	if strings.Contains(sess.Block, want.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s reached the session (%s)", want.Key, want.Mark)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("the block does not carry %s (mark %q)", want.Key, want.Mark)}
}

// staleAbsent is the finding this module exists to produce: a session told a
// claim had been reversed, and handed the original claim again with nothing
// marking it as old. Mark matching is a deliberately narrow test — it cannot see
// a paraphrase of the stale claim, which is what the opt-in judge is for — but
// what it does see is exactly the rendered row, which is the real failure mode.
//
// The line passes in one of two shapes. The stale claim is absent, or it is
// present and says so: every line carrying its Mark carries
// assemble.MaybeReplacedMarker (the lexical guess) or assemble.SupersededMarker
// (a live supersedes edge, which switches the guess off for its pair) in the
// field Ghost renders them in (carriesReplacedMarker), and the replacement's
// Mark appears on an earlier line. An unmarked stale line fails wherever it
// sits, and so does a marked one whose replacement is missing or comes after
// it: a marker pointing at a claim the reader meets second is the failure with
// a label on it.
func staleAbsent(where, block string, stale, replacement Record) Check {
	return staleAbsentAs("stale-original:"+where, where, block, stale, replacement)
}

// staleAbsentAs is staleAbsent with the line's name given separately from the
// block it reads, for a site that names each of several superseded records.
func staleAbsentAs(name, where, block string, stale, replacement Record) Check {
	if !strings.Contains(block, stale.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s is not in %s", stale.Key, where)}
	}
	lines := strings.Split(block, "\n")
	first := -1
	if replacement.Mark != "" {
		for i, l := range lines {
			if strings.Contains(l, replacement.Mark) {
				first = i
				break
			}
		}
	}
	marked := true
	for i, l := range lines {
		if !strings.Contains(l, stale.Mark) {
			continue
		}
		if !carriesReplacedMarker(l) {
			return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
				"%s (%s) is superseded by %s and is still in %s with nothing marking it as old",
				stale.Key, stale.Mark, stale.SupersededBy, where)}
		}
		if first < 0 || first >= i {
			marked = false
		}
	}
	if !marked {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
			"%s (%s) is in %s marked as maybe replaced, but its replacement %s (%s) does not appear before it",
			stale.Key, stale.Mark, where, stale.SupersededBy, replacement.Mark)}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf(
		"%s is in %s marked as maybe replaced, after its replacement %s", stale.Key, where, stale.SupersededBy)}
}

// replacedMarkerLine is the head of a memory line as assemble.Item.Line renders
// it, up to the importance: `- [category] `+"`id`"+` (0.7`.
var replacedMarkerLine = regexp.MustCompile("^- \\[[^\\]]*\\] `[^`]*` \\([0-9]+\\.[0-9]")

// carriesReplacedMarker reports whether a memory line carries the
// maybe-replaced or the superseded marker AT ITS FIELD: Item.Line's prefix is
// the importance, then an optional ` [pinned]`, an optional ` tags:[…]` JSON
// list, an optional ` [resolved]`, and then the marker (the superseded one first
// when a line carries both). Looking for the marker's text anywhere
// before the first « is not enough, because a tag is stored text and a tag
// holding the marker's words renders there too (JSON-quoted, inside the tag
// list). The list is decoded as JSON, so its length is the list's own and a
// bracket inside a tag cannot end it early.
func carriesReplacedMarker(line string) bool {
	head := replacedMarkerLine.FindString(line)
	if head == "" {
		return false
	}
	rest := line[len(head):]
	rest = strings.TrimPrefix(rest, " [pinned]")
	if strings.HasPrefix(rest, " tags:") {
		dec := json.NewDecoder(strings.NewReader(rest[len(" tags:"):]))
		var tags []string
		if err := dec.Decode(&tags); err != nil {
			return false
		}
		rest = rest[len(" tags:")+int(dec.InputOffset()):]
	}
	rest = strings.TrimPrefix(rest, " [resolved]")
	return strings.HasPrefix(rest, " "+assemble.MaybeReplacedMarker) ||
		strings.HasPrefix(rest, " "+assemble.SupersededMarker)
}

// supersedeEdge grades the arc stage's own work: an ACTIVE supersedes edge from
// the newer record to the older one. Direction is read from the store's own
// chronology rather than from the ids' order, so an edge pointing the wrong way
// fails even when the two ids exist and the relation is right. An invalidated
// edge stands for nothing — supersede withdraws an edge by invalidating it — so
// it is not half a pass.
func supersedeEdge(res *Result, pair reversal) Check {
	name := "supersede-edge:" + pair.newer.Key
	newer, older := res.idOf(pair.newer.Key), res.idOf(pair.older.Key)
	if newer == "" || older == "" {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
			"no id reached the grade for %s/%s", pair.newer.Key, pair.older.Key)}
	}
	var found string
	for _, l := range res.State.Links {
		if l.Source != newer || l.Target != older {
			continue
		}
		switch {
		case l.Invalidated:
			found = "the edge exists but was invalidated (unsuperseded)"
			continue
		case l.Relation != "supersedes":
			found = fmt.Sprintf("the edge's relation is %q, not supersedes", l.Relation)
			continue
		case !olderThan(res.State, older, newer):
			found = "the edge points the newer record at the older one, which is not what it was written for"
			continue
		default:
			return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s supersedes %s", pair.newer.Key, pair.older.Key)}
		}
	}
	if found == "" {
		found = "no edge joins the two records"
	}
	return Check{Name: name, Passed: false, Detail: found}
}

// olderThan reads the direction off the store's own stamps. A pair whose
// chronology cannot be read is NOT given the benefit of the doubt: the arc's
// direction is the one thing a direction-blind pass would hide.
func olderThan(st State, older, newer string) bool {
	o, okO := st.Stamps[older]
	n, okN := st.Stamps[newer]
	if !okO || !okN {
		return false
	}
	return o.CreatedAt < n.CreatedAt
}

// reversalLive grades the other half of the arc: `ghost resolve` must not have
// withdrawn the claim that is supposed to be current. A resolve stage that took
// the reversal with it leaves every later session with no session store at all,
// and that is a different bug from failing to demote the old one.
func reversalLive(res *Result, pair reversal) Check {
	name := "reversal-live:" + pair.newer.Key
	id := res.idOf(pair.newer.Key)
	if id == "" {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf("no id reached the grade for %s", pair.newer.Key)}
	}
	stamp, ok := res.State.Stamps[id]
	if !ok {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf("no stamp reached the grade for %s", pair.newer.Key)}
	}
	if stamp.ResolvedAt != "" {
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf("%s was resolved at %s, so the reversal is not live",
			pair.newer.Key, stamp.ResolvedAt)}
	}
	return Check{Name: name, Passed: true, Detail: pair.newer.Key + " is unresolved"}
}

// finalBlockCarries grades what a reader of the report actually wants to know: a
// session starting after the arc must still be handed the decision that is
// current.
func finalBlockCarries(res *Result, want Record) Check {
	name := "final-block-carries:" + want.Key
	if strings.Contains(res.FinalBlock, want.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("the final block carries %s (%s)", want.Key, want.Mark)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("the final block does not carry %s (mark %q)", want.Key, want.Mark)}
}

// judgedCheck is the one check that is not a store or a block reading, and it is
// added only when -judge ran. An opt-in judge that did not run leaves no check
// behind rather than a check that always passes: a report line for a measurement
// nobody took is a number with no evidence behind it, and it is the one output
// this module is not allowed to produce.
func judgedCheck(followed bool, verdict string) Check {
	name := "judge:followed-reversal"
	return Check{Name: name, Passed: followed, Detail: strings.TrimSpace(verdict), Advisory: true}
}

// judgeVerdict reads the judge's answer. The verdict is its FIRST FIELD being
// exactly yes or no, and nothing else counts: a prefix match turns the opening
// of an ordinary sentence into a verdict ("Yesterday's block…" is not a yes, and
// "Nothing in the block names the new store" is not a no), which would put a
// number in the report that nothing observed.
//
// The field is split on WHITESPACE, not on one byte, and the word is stripped of
// the punctuation a sentence can put around it. Both halves are load-bearing
// because the alternative is not a failed check but a dead run: a judge that
// answers "yes" on its own line, or "Yes." with a full stop, is answering the
// question, and refusing it would throw away a three-session run's report and
// every deterministic check in it over formatting.
func judgeVerdict(answer string) (bool, error) {
	trimmed := strings.TrimSpace(answer)
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return false, fmt.Errorf("unreadable judge verdict %q: answer yes or no", trimmed)
	}
	switch strings.ToUpper(strings.Trim(fields[0], "\"'`.,;:!?()[]{}")) {
	case "YES":
		return true, nil
	case "NO":
		return false, nil
	}
	return false, fmt.Errorf("unreadable judge verdict %q: answer yes or no", trimmed)
}

// writeReport is the run's artifact: the storyline, the model that answered, every
// graded check with its detail, each session's injection and answer verbatim, the
// two arc stages' raw output, and the final block. The pass/fail verdict is the
// FIRST line, because a report is read for its conclusion.
func writeReport(dir string, res *Result, model string) (string, error) {
	return writeReportAs(dir, res.Story.Key+".md", res, model)
}

// writeReportAs is writeReport under a caller-chosen file name, so several runs
// of one storyline (two arms, n runs each) do not overwrite each other.
func writeReportAs(dir, file string, res *Result, model string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create results dir: %w", err)
	}
	verdict := "FAIL"
	if res.Passed() {
		verdict = "PASS"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s — %s\n\n", verdict, res.Story.Title)
	fmt.Fprintf(&b, "- storyline: %s\n", res.Story.Key)
	fmt.Fprintf(&b, "- project: %s\n", res.Story.Project)
	fmt.Fprintf(&b, "- arm: %s\n", res.Arm())
	fmt.Fprintf(&b, "- model: %s\n", model)
	fmt.Fprintf(&b, "- work dir: %s\n", res.WorkDir)
	fmt.Fprintf(&b, "- judged: %v\n", res.Judged)
	if res.FakeHarness {
		b.WriteString("- harness: fake (oracle classifier; answer lines not graded)\n")
	}
	if known := res.KnownNames(); len(known) > 0 {
		fmt.Fprintf(&b, "- known-failing checks that failed: %s\n", strings.Join(known, ", "))
	}
	if failed := res.FailedNames(); len(failed) > 0 {
		fmt.Fprintf(&b, "- failed checks: %s\n", strings.Join(failed, ", "))
	}

	b.WriteString("\n## checks\n\n")
	if len(res.Checks) == 0 {
		b.WriteString("no checks were graded\n")
	}
	for _, c := range res.Checks {
		fmt.Fprintf(&b, "- %s %s — %s\n", c.verdict(), c.Name, oneLine(c.Detail))
	}

	b.WriteString("\n## sessions\n")
	for _, sess := range res.Sessions {
		fmt.Fprintf(&b, "\n### session %d\n\n", sess.Index+1)
		fmt.Fprintf(&b, "injection (%d bytes):\n\n```\n%s```\n\n", len(sess.Block), sess.Block)
		fmt.Fprintf(&b, "answer:\n\n```\n%s\n```\n\n", strings.TrimSpace(sess.Answer))
	}

	if !res.WithoutGhost {
		fmt.Fprintf(&b, "\n## stage: ghost supersede --apply\n\n```\n%s```\n", res.Supersede)
		fmt.Fprintf(&b, "\n## stage: ghost resolve --apply\n\n```\n%s```\n", res.Resolve)
		fmt.Fprintf(&b, "\n## final injection (%d bytes)\n\n```\n%s```\n", len(res.FinalBlock), res.FinalBlock)
		for i, st := range res.Story.Stages {
			if i >= len(res.Sessions) {
				break
			}
			for _, p := range st.Probes {
				fmt.Fprintf(&b, "\n## probe %s:%s: ghost_memory_search %q%s\n\n```\n%s\n```\n", sessionName(i), p.Name, p.Query, probeScope(p), strings.TrimSpace(res.Sessions[i].Probes[p.Name]))
			}
		}
		for _, p := range res.Story.Probes {
			fmt.Fprintf(&b, "\n## probe %s: ghost_memory_search %q%s\n\n```\n%s\n```\n", p.Name, p.Query, probeScope(p), strings.TrimSpace(res.Probes[p.Name]))
		}
	}
	if res.Judged {
		fmt.Fprintf(&b, "\n## judge\n\n```\n%s\n```\n", strings.TrimSpace(res.Verdict))
	}

	path := filepath.Join(dir, file)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	return path, nil
}

// verdict is the report word for a check; an advisory line says so, because a
// FAIL that does not decide the run is otherwise read as one that does.
func (c Check) verdict() string {
	v := "FAIL"
	if c.Passed {
		v = "PASS"
	}
	if c.Advisory {
		v += " (advisory)"
	}
	if c.Known {
		v += " (known)"
	}
	if c.UnexpectedPass {
		v += " (unexpected: listed known-failing)"
	}
	return v
}

// oneLine keeps a detail on one report line: a check's detail names records, and
// a memory's content is a paragraph.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sessionName(i int) string { return "session-" + strconv.Itoa(i+1) }
