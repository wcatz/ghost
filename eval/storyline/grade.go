package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
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
			for _, stale := range supersededBefore(s, i) {
				out = append(out, staleAbsent(sessionName(sess.Index), sess.Block, stale))
			}
			for _, old := range expiredBefore(s, i) {
				out = append(out, expiredWithheld(sessionName(sess.Index), sess.Block, old))
			}
		}
		for _, c := range stages[i].Carries {
			out = append(out, answerCarries(sess, c))
		}
		for _, c := range stages[i].Avoids {
			out = append(out, answerAvoids(sess, c))
		}
	}
	if res.WithoutGhost {
		return out
	}
	// The final block is graded on its own, because it is the one a reader of the
	// report cares about: what the NEXT session of this project would be told.
	for _, stale := range supersededBefore(s, len(s.Stages)) {
		out = append(out, staleAbsent("final-block", res.FinalBlock, stale))
	}
	for _, old := range expiredBefore(s, len(s.Stages)) {
		out = append(out, expiredWithheld("final-block", res.FinalBlock, old))
	}
	for _, pair := range reversals(s) {
		out = append(out, supersedeEdge(res, pair))
		out = append(out, reversalLive(res, pair))
		out = append(out, finalBlockCarries(res, pair.newer))
	}
	return out
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

// answerCarries is the primary grade: the agent's answer contains one of the
// check's spellings, matched case-insensitively and as a whole token (so 6432 is
// not found in 16432). It reads the answer and never the block, so a session
// handed the claim and ignoring it fails, and a session that never saw it and
// guessed it right passes — which is why the script may not contain the
// spelling (Validate).
func answerCarries(sess Session, c AnswerCheck) Check {
	name := "answer-carries:" + sessionName(sess.Index) + ":" + c.Name
	low := strings.ToLower(sess.Answer)
	for _, a := range c.Any {
		if containsToken(low, strings.ToLower(a)) {
			return Check{Name: name, Passed: true, Detail: fmt.Sprintf("the answer carries %q", a)}
		}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("the answer carries none of %q", c.Any)}
}

// answerAvoids is the negative answer grade: the answer must not USE the stale,
// expired or mistaken claim. A mention of a spelling is a use unless a rejecting
// word SCOPES it: one within staleBefore words before it ("not X", "instead of
// X", "the old X"), or one of staleAfter right after it ("X is wrong", "X is
// not used"). A rejecting word elsewhere in the sentence does not excuse it, so
// "I don't have project info, but the standard header is X" is a use of X.
//
// When the check names Needs, an answer that mentions the spelling AT ALL
// without carrying one of the Needs spellings fails too, marked or not: naming
// the natural wrong answer while not supplying the right one is not a pass.
//
// It is a deliberately narrow reading: verbatim, and blind to a paraphrase,
// which is what the judge column is for.
func answerAvoids(sess Session, c AnswerCheck) Check {
	name := "answer-avoids:" + sessionName(sess.Index) + ":" + c.Name
	low := strings.ToLower(sess.Answer)
	var mentioned string
	for _, sentence := range splitSentences(sess.Answer) {
		ls := strings.ToLower(sentence)
		for _, a := range c.Any {
			la := strings.ToLower(a)
			for _, at := range tokenIndexes(ls, la) {
				mentioned = a
				if !scopedRejection(ls, at, len(la)) {
					return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
						"the answer uses %q with nothing rejecting it: %s", a, oneLine(sentence))}
				}
			}
		}
	}
	if mentioned != "" && len(c.Needs) > 0 {
		for _, n := range c.Needs {
			if containsToken(low, strings.ToLower(n)) {
				return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%q is rejected and %q is carried", mentioned, n)}
			}
		}
		return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
			"the answer names %q without carrying any of %q", mentioned, c.Needs)}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("the answer does not use %q", c.Any)}
}

const (
	staleBefore = 4
	staleAfter  = 4
)

var (
	wordRe       = regexp.MustCompile(`[a-z0-9_'’-]+`)
	rejectBefore = regexp.MustCompile(`\b(not|never|no|without|avoid|avoiding|instead of|rather than|no longer|stop|stopped|old|older|previous|previously|former|formerly|obsolete|outdated|deprecated|expired|stale|wrong|incorrect|ignore|ignores|ignored|ignoring|replaced|replaces|replacing|superseded|don't|doesn't|isn't|shouldn't|won't)\b`)
	rejectAfter  = regexp.MustCompile(`\b(wrong|incorrect|deprecated|obsolete|outdated|replaced|superseded|expired|stale|ignored|unused|not|isn't|no longer)\b`)
)

// scopedRejection reports whether a rejecting word is attached to the mention at
// [at, at+n) of the lower-cased sentence.
func scopedRejection(sentence string, at, n int) bool {
	before := wordRe.FindAllString(sentence[:at], -1)
	if len(before) > staleBefore {
		before = before[len(before)-staleBefore:]
	}
	after := wordRe.FindAllString(sentence[at+n:], -1)
	if len(after) > staleAfter {
		after = after[:staleAfter]
	}
	return rejectBefore.MatchString(strings.Join(before, " ")) || rejectAfter.MatchString(strings.Join(after, " "))
}

// tokenIndexes is every offset at which needle appears in hay as a whole token
// (see containsToken). Both arguments are already lower-cased.
func tokenIndexes(hay, needle string) []int {
	var out []int
	if needle == "" {
		return nil
	}
	for from := 0; from < len(hay); {
		i := strings.Index(hay[from:], needle)
		if i < 0 {
			break
		}
		start, end := from+i, from+i+len(needle)
		if !identRuneAt(hay, start-1) && !identRuneAt(hay, end) {
			out = append(out, start)
		}
		from = start + 1
	}
	return out
}

var sentenceEnd = regexp.MustCompile(`[.!?]+(?:\s+|$)|\n+`)

// splitSentences splits on sentence punctuation followed by space or the end,
// and on newlines, so a dot inside a hostname does not end a sentence.
func splitSentences(s string) []string {
	var out []string
	for _, p := range sentenceEnd.Split(s, -1) {
		if strings.TrimSpace(p) != "" {
			out = append(out, p)
		}
	}
	return out
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
func staleAbsent(where, block string, stale Record) Check {
	name := "stale-original:" + where
	if !strings.Contains(block, stale.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s is not in %s", stale.Key, where)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
		"%s (%s) is superseded by %s and is still in %s with nothing marking it as old",
		stale.Key, stale.Mark, stale.SupersededBy, where)}
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
	return v
}

// oneLine keeps a detail on one report line: a check's detail names records, and
// a memory's content is a paragraph.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sessionName(i int) string { return "session-" + strconv.Itoa(i+1) }
