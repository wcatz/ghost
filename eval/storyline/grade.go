package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Check is one graded property of a finished run: a name a report can key on, a
// verdict, and a detail line that says what was observed. A check with no
// measurable observation behind it is not a check, which is why the judge check
// exists only when the judge ran (see judgedCheck).
type Check struct {
	Name   string
	Passed bool
	Detail string
}

// grade is the whole deterministic grade, and it reads two sources and no
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
// The primary grade for carried-forward knowledge is now the agent's ANSWER,
// not the block. Block checks are kept as "delivery:" diagnostics to show
// whether the block carried the mark. The without-Ghost arm has an empty block
// so its answer grade measures the agent's own knowledge.
func grade(res *Result) []Check {
	var out []Check
	s := res.Story
	stages := s.Stages
	if len(res.Sessions) < len(stages) {
		stages = stages[:len(res.Sessions)]
	}
	for i, sess := range res.Sessions {
		// Delivery diagnostics: what the block actually carried
		out = append(out, injectionPresent(sess))
		for _, key := range stages[i].Expect {
			out = append(out, deliveryBlockCarries(sess, mustRecord(s, key)))
		}
		for _, stale := range supersededBefore(s, i) {
			out = append(out, deliveryStaleAbsent(sessionName(sess.Index), sess.Block, stale))
		}
		// Primary grade: did the ANSWER act on the carried-forward mark?
		for _, key := range stages[i].Expect {
			out = append(out, answerCarries(sess, mustRecord(s, key)))
		}
		// Paraphrase check via judge (non-gating column): does the answer
		// contain a paraphrase of the expected mark, not just verbatim?
		// This is a separate check that doesn't gate the run.
		for _, key := range stages[i].Expect {
			out = append(out, answerParaphrases(sess, mustRecord(s, key)))
		}
	}
	// The final block is graded on its own, because it is the one a reader of the
	// report cares about: what the NEXT session of this project would be told.
	for _, stale := range supersededBefore(s, len(s.Stages)) {
		out = append(out, deliveryStaleAbsent("final-block", res.FinalBlock, stale))
	}
	// Final block answer check (for the final session's answer)
	lastIdx := len(res.Sessions) - 1
	if lastIdx >= 0 {
		lastStage := stages[lastIdx]
		for _, key := range lastStage.Expect {
			out = append(out, answerCarries(res.Sessions[lastIdx], mustRecord(s, key)))
			out = append(out, answerParaphrases(res.Sessions[lastIdx], mustRecord(s, key)))
		}
	}
	for _, pair := range reversals(s) {
		out = append(out, supersedeEdge(res, pair))
		out = append(out, reversalLive(res, pair))
		out = append(out, finalBlockCarries(res, pair.newer))
	}
	return out
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
		return Check{name, false, "the session was injected nothing"}
	}
	return Check{name, true, fmt.Sprintf("%d bytes of injected context", len(sess.Block))}
}

// deliveryBlockCarries is a DELIVERY diagnostic: did the block carry the mark?
// This is NOT the primary grade — it shows whether Ghost delivered the claim.
func deliveryBlockCarries(sess Session, want Record) Check {
	name := "delivery:block-carries:" + sessionName(sess.Index) + ":" + want.Key
	if strings.Contains(sess.Block, want.Mark) {
		return Check{name, true, fmt.Sprintf("%s reached the block (%s)", want.Key, want.Mark)}
	}
	return Check{name, false, fmt.Sprintf("the block does not carry %s (mark %q)", want.Key, want.Mark)}
}

// deliveryStaleAbsent is a DELIVERY diagnostic: is the stale mark absent from the block?
// This is NOT the primary grade — it shows whether Ghost stopped delivering the stale claim.
func deliveryStaleAbsent(where, block string, stale Record) Check {
	name := "delivery:stale-absent:" + where + ":" + stale.Key
	if !strings.Contains(block, stale.Mark) {
		return Check{name, true, fmt.Sprintf("%s is not in %s", stale.Key, where)}
	}
	return Check{name, false, fmt.Sprintf(
		"%s (%s) is superseded by %s and is still in %s with nothing marking it as old",
		stale.Key, stale.Mark, stale.SupersededBy, where)}
}

// answerCarries is the PRIMARY grade: did the agent's ANSWER act on the carried-forward mark?
// This grades the agent's words, not the block. In the without-Ghost arm the block is
// empty, so this measures the agent's own knowledge.
func answerCarries(sess Session, want Record) Check {
	name := "answer-carries:" + sessionName(sess.Index) + ":" + want.Key
	// Check if the answer contains the mark (verbatim substring match)
	if strings.Contains(sess.Answer, want.Mark) {
		return Check{name, true, fmt.Sprintf("%s appears in answer (%s)", want.Key, want.Mark)}
	}
	return Check{name, false, fmt.Sprintf("the answer does not carry %s (mark %q)", want.Key, want.Mark)}
}

// answerParaphrases checks if the answer paraphrases the expected mark.
// This is a non-gating diagnostic column — the judge provides the authoritative
// paraphrase check when -judge runs. This deterministic version uses a simple
// token overlap heuristic as a proxy.
func answerParaphrases(sess Session, want Record) Check {
	name := "answer-paraphrases:" + sessionName(sess.Index) + ":" + want.Key
	// Simple token overlap: split mark and answer into words, check overlap
	markTokens := tokenize(want.Mark)
	answerTokens := tokenize(sess.Answer)
	if len(markTokens) == 0 {
		return Check{name, false, "empty mark"}
	}
	overlap := 0
	for _, mt := range markTokens {
		for _, at := range answerTokens {
			if strings.EqualFold(mt, at) {
				overlap++
				break
			}
		}
	}
	// Require at least half the mark tokens to appear in the answer
	threshold := (len(markTokens) + 1) / 2
	if overlap >= threshold {
		return Check{name, true, fmt.Sprintf("%d/%d mark tokens in answer", overlap, len(markTokens))}
	}
	return Check{name, false, fmt.Sprintf("%d/%d mark tokens in answer (need %d)", overlap, len(markTokens), threshold)}
}

func tokenize(s string) []string {
	// Simple word tokenization: split on non-alphanumeric, lowercase
	var tokens []string
	var current strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			current.WriteRune(r)
		} else {
			if current.Len() > 0 {
				tokens = append(tokens, strings.ToLower(current.String()))
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, strings.ToLower(current.String()))
	}
	return tokens
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
		return Check{name, false, fmt.Sprintf(
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
			return Check{name, true, fmt.Sprintf("%s supersedes %s", pair.newer.Key, pair.older.Key)}
		}
	}
	if found == "" {
		found = "no edge joins the two records"
	}
	return Check{name, false, found}
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
		return Check{name, false, fmt.Sprintf("no id reached the grade for %s", pair.newer.Key)}
	}
	stamp, ok := res.State.Stamps[id]
	if !ok {
		return Check{name, false, fmt.Sprintf("no stamp reached the grade for %s", pair.newer.Key)}
	}
	if stamp.ResolvedAt != "" {
		return Check{name, false, fmt.Sprintf("%s was resolved at %s, so the reversal is not live",
			pair.newer.Key, stamp.ResolvedAt)}
	}
	return Check{name, true, pair.newer.Key + " is unresolved"}
}

// finalBlockCarries grades what a reader of the report actually wants to know: a
// session starting after the arc must still be handed the decision that is
// current.
func finalBlockCarries(res *Result, want Record) Check {
	name := "final-block-carries:" + want.Key
	if strings.Contains(res.FinalBlock, want.Mark) {
		return Check{name, true, fmt.Sprintf("the final block carries %s (%s)", want.Key, want.Mark)}
	}
	return Check{name, false, fmt.Sprintf("the final block does not carry %s (mark %q)", want.Key, want.Mark)}
}

// judgedCheck is the one check that is not a store or a block reading, and it is
// added only when -judge ran. An opt-in judge that did not run leaves no check
// behind rather than a check that always passes: a report line for a measurement
// nobody took is a number with no evidence behind it, and it is the one output
// this module is not allowed to produce.
func judgedCheck(followed bool, verdict string) Check {
	name := "judge:followed-reversal"
	if followed {
		return Check{name, true, strings.TrimSpace(verdict)}
	}
	return Check{name, false, strings.TrimSpace(verdict)}
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
	fmt.Fprintf(&b, "- model: %s\n", model)
	fmt.Fprintf(&b, "- work dir: %s\n", res.WorkDir)
	fmt.Fprintf(&b, "- judged: %v\n", res.Judged)
	if failed := res.FailedNames(); len(failed) > 0 {
		fmt.Fprintf(&b, "- failed checks: %s\n", strings.Join(failed, ", "))
	}

	b.WriteString("\n## checks\n\n")
	if len(res.Checks) == 0 {
		b.WriteString("no checks were graded\n")
	}
	for _, c := range res.Checks {
		mark := "FAIL"
		if c.Passed {
			mark = "PASS"
		}
		fmt.Fprintf(&b, "- %s %s — %s\n", mark, c.Name, oneLine(c.Detail))
	}

	b.WriteString("\n## sessions\n")
	for _, sess := range res.Sessions {
		fmt.Fprintf(&b, "\n### session %d\n\n", sess.Index+1)
		fmt.Fprintf(&b, "injection (%d bytes):\n\n```\n%s```\n\n", len(sess.Block), sess.Block)
		fmt.Fprintf(&b, "answer:\n\n```\n%s\n```\n\n", strings.TrimSpace(sess.Answer))
	}

	fmt.Fprintf(&b, "\n## stage: ghost supersede --apply\n\n```\n%s```\n", res.Supersede)
	fmt.Fprintf(&b, "\n## stage: ghost resolve --apply\n\n```\n%s```\n", res.Resolve)

	fmt.Fprintf(&b, "\n## final injection (%d bytes)\n\n```\n%s```\n", len(res.FinalBlock), res.FinalBlock)
	if res.Judged {
		fmt.Fprintf(&b, "\n## judge\n\n```\n%s\n```\n", strings.TrimSpace(res.Verdict))
	}

	path := filepath.Join(dir, res.Story.Key+".md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	return path, nil
}

// oneLine keeps a detail on one report line: a check's detail names records, and
// a memory's content is a paragraph.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sessionName(i int) string { return "session-" + strconv.Itoa(i+1) }
