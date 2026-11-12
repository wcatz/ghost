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
type Check struct {
	Name           string
	Passed         bool
	Detail         string
	Advisory       bool
	Known          bool
	UnexpectedPass bool
}

func grade(res *Result) []Check {
	var out []Check
	s := res.Story
	stages := s.Stages
	if len(res.Sessions) < len(stages) {
		stages = stages[:len(res.Sessions)]
	}
	for i, sess := range res.Sessions {
		if !res.baseline() {
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
		if !res.baseline() {
			for _, key := range stages[i].Withheld {
				out = append(out, withheld(sessionName(sess.Index), sess.Block, mustRecord(s, key)))
			}
			for _, p := range stages[i].Probes {
				raw, ran := sess.Probes[p.Name]
				out = append(out, probeChecks(res, p, sessionName(sess.Index)+":"+p.Name, raw, ran)...)
			}
		}
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
	if res.baseline() {
		return applyKnown(s, out)
	}
	finalStale := supersededBefore(s, len(s.Stages))
	for _, stale := range finalStale {
		out = append(out, staleAbsentAs("stale-original:"+staleSite("final-block", stale, len(finalStale)), "final-block", res.FinalBlock, stale, mustRecord(s, stale.SupersededBy)))
	}
	for _, old := range expiredBefore(s, len(s.Stages)) {
		out = append(out, expiredWithheld("final-block", res.FinalBlock, old))
	}
	for _, pair := range reversals(s) {
		out = append(out, supersedeEdge(res, pair))
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

func pinsOverflowNoticed(res *Result) Check {
	name := "pins-overflow-noticed:pin-result"
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

func duplicateMergedBlock(res *Result) Check {
	name := "duplicate-merged:block-shows-once"
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

func duplicateMergedStore(res *Result) Check {
	name := "duplicate-merged:store-holds-one"
	n := len(res.State.Stamps)
	if n == 2 {
		return Check{Name: name, Passed: true, Detail: "store holds one live row for the rule (2 memories total with the opening)"}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("store holds %d memories, want 2 (one live row for the rule)", n)}
}

func datedGlobalWithheld(res *Result) Check {
	name := "dated-global-withheld:final-block:rollout-status"
	mark := "Relay v2.3.1 is live in prod"
	if strings.Contains(res.FinalBlock, mark) {
		return Check{Name: name, Passed: false, Detail: "final block still carries the expired global rollout status"}
	}
	return Check{Name: name, Passed: true, Detail: "final block does not carry the expired global rollout status"}
}

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

func staleSite(where string, stale Record, n int) string {
	if n > 1 {
		return where + ":" + stale.Key
	}
	return where
}

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

type searchRow struct {
	ID       string
	Resolved bool
	Line     string
}

var searchRowRe = regexp.MustCompile(`(?m)^- \[[^\]]+\] ` + "`" + `([^` + "`" + `]+)` + "`" + ` \(([^\n]*)$`)

func parseSearchRows(raw string) []searchRow {
	var out []searchRow
	for _, m := range searchRowRe.FindAllStringSubmatch(raw, -1) {
		meta, _, _ := strings.Cut(m[2], "«")
		out = append(out, searchRow{ID: m[1], Resolved: strings.Contains(meta, "[resolved]"), Line: m[0]})
	}
	return out
}

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

func expiredWithheld(where, block string, old Record) Check {
	name := "expired-withheld:" + where + ":" + old.Key
	if !strings.Contains(block, old.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s (valid until %s) is not in %s", old.Key, old.ValidUntil, where)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf(
		"%s (%s) expired on %s and is still in %s", old.Key, old.Mark, old.ValidUntil, where)}
}

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

type reversal struct {
	older, newer Record
}

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

func mustRecord(s Storyline, key string) Record {
	r, ok := s.RecordByKey(key)
	if !ok {
		return Record{Key: key}
	}
	return r
}

func injectionPresent(sess Session) Check {
	name := "injection-present:" + sessionName(sess.Index)
	if strings.TrimSpace(sess.Block) == "" {
		return Check{Name: name, Passed: false, Detail: "the session was injected nothing"}
	}
	return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%d bytes of injected context", len(sess.Block))}
}

func carryForward(sess Session, want Record) Check {
	name := "carry-forward:" + sessionName(sess.Index)
	if strings.Contains(sess.Block, want.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("%s reached the session (%s)", want.Key, want.Mark)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("the block does not carry %s (mark %q)", want.Key, want.Mark)}
}

func staleAbsent(where, block string, stale, replacement Record) Check {
	return staleAbsentAs("stale-original:"+where, where, block, stale, replacement)
}

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

var replacedMarkerLine = regexp.MustCompile("^- \\[[^\\]]*\\] `[^`]*` \\([0-9]+\\.[0-9]")

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

func olderThan(st State, older, newer string) bool {
	o, okO := st.Stamps[older]
	n, okN := st.Stamps[newer]
	if !okO || !okN {
		return false
	}
	return o.CreatedAt < n.CreatedAt
}

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

func finalBlockCarries(res *Result, want Record) Check {
	name := "final-block-carries:" + want.Key
	if strings.Contains(res.FinalBlock, want.Mark) {
		return Check{Name: name, Passed: true, Detail: fmt.Sprintf("the final block carries %s (%s)", want.Key, want.Mark)}
	}
	return Check{Name: name, Passed: false, Detail: fmt.Sprintf("the final block does not carry %s (mark %q)", want.Key, want.Mark)}
}

func judgedCheck(followed bool, verdict string) Check {
	name := "judge:followed-reversal"
	return Check{Name: name, Passed: followed, Detail: strings.TrimSpace(verdict), Advisory: true}
}

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

func writeReport(dir string, res *Result, model string) (string, error) {
	return writeReportAs(dir, res.Story.Key+".md", res, model)
}

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

	if !res.baseline() {
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

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sessionName(i int) string { return "session-" + strconv.Itoa(i+1) }
