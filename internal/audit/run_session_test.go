package audit

// The retrieval audit judges one session's calls against that session's text.
//
// Before this scope existed `Run` read the project's newest calls whatever session made
// them and compared every kept memory with the one session the stop hook had just
// scanned. On a real store that filed 615 verdicts, every one of them `used`, across
// calls dated four different days.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// recordCallIn writes one call made in the given session ("" for a call that
// cannot name one) and returns its rowid.
func recordCallIn(t *testing.T, store *memory.Store, projectID, session string, kept ...string) int64 {
	t.Helper()
	verdicts := make([]memory.RowVerdict, 0, len(kept))
	for _, id := range kept {
		verdicts = append(verdicts, memory.RowVerdict{ID: id, Kept: true, Stage: "fit", Reason: "fit_response"})
	}
	if err := store.RecordRetrieval(context.Background(), memory.RetrievalRecord{
		ProjectID: projectID, SessionID: session, Source: "search", Outcome: "answerable", Verdicts: verdicts,
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	return newestCallRowID(t, store, projectID)
}

// TestRunJudgesOnlyThisSessionsCalls: a call made in another session, or in none,
// is not judged against this session's text, even when the text would match it.
func TestRunJudgesOnlyThisSessionsCalls(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "MINE", memContent)
	seedMemory(t, store, projectID, "THEIRS", memContent)
	seedMemory(t, store, projectID, "NOSESS", memContent)
	mine := recordCallIn(t, store, projectID, testSession, "MINE")
	_ = recordCallIn(t, store, projectID, "sess-other", "THEIRS")
	_ = recordCallIn(t, store, projectID, "", "NOSESS")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := res.byMemory()
	if len(got) != 1 || got["MINE"] != OutcomeUsed {
		t.Fatalf("verdicts = %v, want exactly MINE judged (the other two calls belong to another session or to none)", got)
	}
	rows, err := store.RetrievalAudits(context.Background(), projectID, "")
	if err != nil {
		t.Fatalf("RetrievalAudits: %v", err)
	}
	if len(rows) != 1 || rows[0].RecordRowID != mine || rows[0].SessionID != testSession {
		t.Fatalf("filed rows = %+v, want one verdict, on call %d, carrying the session id", rows, mine)
	}
	if n := len(res.Sources); n != 1 || res.Sources[0].Calls != 1 {
		t.Errorf("sources = %+v, want one source counting only this session's single call", res.Sources)
	}
}

// TestRunWithNoSessionJudgesNothing: a scan that could not name its session must not
// match every call that could not name one either. Both are "", and matching them
// would judge exactly the rows no session owns.
func TestRunWithNoSessionJudgesNothing(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "NOSESS", memContent)
	_ = recordCallIn(t, store, projectID, "", "NOSESS")

	s := NewWithHasher(testHasher) // no session id
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdicts != 0 {
		t.Fatalf("Verdicts = %d, want 0: a scan with no session id has no calls it can claim", res.Verdicts)
	}
	if !res.NoSession {
		t.Errorf("NoSession = false; the summary must say why nothing was judged")
	}
	if !strings.Contains(res.String(), "no session") {
		t.Errorf("the printed summary does not say the scan carried no session id:\n%s", res)
	}
	rows, _ := store.RetrievalAudits(context.Background(), projectID, "")
	if len(rows) != 0 {
		t.Errorf("%d verdict(s) filed for a scan with no session", len(rows))
	}
}

// TestRunFindsThisSessionsCallsBehindNewerOnesFromOthers: the window is applied AFTER
// the session predicate. With the filter in Go, CallWindow newer calls from other
// sessions evict this session's calls and the audit judges nothing.
func TestRunFindsThisSessionsCallsBehindNewerOnesFromOthers(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "MINE", memContent)
	seedMemory(t, store, projectID, "OTHER", memContent)
	_ = recordCallIn(t, store, projectID, testSession, "MINE")
	for i := 0; i < CallWindow+5; i++ {
		_ = recordCallIn(t, store, projectID, "sess-other", "OTHER")
	}

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.byMemory(); len(got) != 1 || got["MINE"] != OutcomeUsed {
		t.Fatalf("verdicts = %v, want MINE judged although %d newer calls belong to another session", got, CallWindow+5)
	}
}

// TestTheSidecarCarriesTheSession: the scan happens in the hook and the comparison in
// a detached child, so the session id has to cross the gap with the fingerprints.
func TestTheSidecarCarriesTheSession(t *testing.T) {
	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	back, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if back.SessionID() != testSession {
		t.Errorf("session after a round trip = %q, want %q", back.SessionID(), testSession)
	}
	if SidecarHeader == sidecarV2 {
		t.Errorf("the header was not bumped; a v2 file carries no session and would parse as one that judged nothing")
	}
}

// ---- the realistic fixture --------------------------------------------------------

// devSessionText builds the scanned text of a long coding session on one project:
// narrative turns, plus the bodies of Edit and Write tool calls (whole files), which is
// what makes it thousands of words long. Deterministic, so a failure is reproducible.
func devSessionText(t *testing.T, s *Signals) (words int) {
	t.Helper()
	return devSessionTextFrom(t, s, 0, true)
}

// devSessionTextFrom is devSessionText with an order: turn i is written i-callTurn
// minutes after the call the caller is about to record (so turns before callTurn were
// written before it). feedArgs false leaves the Edit/Write bodies out of the signals
// altogether, which is the variant "tool arguments stop feeding the token arm", measured
// here and not shipped.
func devSessionTextFrom(t *testing.T, s *Signals, callTurn int, feedArgs bool) (words int) {
	t.Helper()
	now := time.Now()
	narrative := []string{
		"I will read the assembler first and then change how the retrieval record is written by the sink",
		"The failing test shows the verdicts column holds stale rows, so the reader needs a session predicate",
		"Now the migration step: the schema string and one frozen step per version must change together",
		"Running the memory package tests with the race detector before touching the stop hook",
		"The hook payload carries the session id, and the server process carries it in its environment",
	}
	body := func(n int) string {
		var b strings.Builder
		for i := 0; i < n; i++ {
			fmt.Fprintf(&b, "// Package handler%d wires the retrieval record sink into the assembler stage %d.\n", i, i)
			fmt.Fprintf(&b, "func (s *Store) writeVerdicts%d(ctx context.Context, rec RetrievalRecord) error {\n", i)
			fmt.Fprintf(&b, "\t// the schema migration guards the verdicts column so the sink never fails the search %d\n", i)
			fmt.Fprintf(&b, "\tif err := s.sink.RecordRetrieval(ctx, rec); err != nil { return fmt.Errorf(\"record session %d: %%w\", err) }\n", i)
			fmt.Fprintf(&b, "\treturn s.assembler.Stage(ctx, rec.ProjectID, rec.SessionID, rec.Verdicts)\n}\n")
		}
		return b.String()
	}
	for i := 0; i < 40; i++ {
		// A minute between turns, offset half a minute so no turn falls in the second the
		// call is recorded in (the call is taken at the end of that second): a turn at or
		// after callTurn is clearly after it, one before is clearly before.
		s.SetAt(now.Add(time.Duration(i-callTurn)*time.Minute + 30*time.Second))
		text := narrative[i%len(narrative)]
		s.AddProse(text)
		words += len(strings.Fields(text))
		// Edit/Write-shaped tool arguments: one file body per call.
		args := body(4)
		if feedArgs {
			s.AddToolArgs(args)
		}
		words += len(strings.Fields(args))
	}
	return words
}

// unrelatedMemories are twenty memories from a different domain than the session.
func unrelatedMemories() []string {
	return []string{
		"Stake pool relays announce on port 3001 behind the tailscale mesh",
		"Grafana dashboards are generated from yaml with one panel per exporter",
		"KES keys rotate every ninety two days before the operational certificate expires",
		"Helmfile diff must run before apply on the production cluster",
		"The ogmios chart pins the node socket volume read only",
		"Prometheus scrape interval for cardano nodes is fifteen seconds",
		"SOPS age recipients live in the repository creation rules file",
		"Alertmanager routes block production alerts to the on call receiver",
		"The k3s agent joins with the tailscale auth key flag",
		"Mithril snapshots restore faster than syncing from genesis",
		"Cncli leaderlog needs the vrf signing key for epoch schedules",
		"Dingo block producer needs the opcert counter incremented on rotation",
		"Preprod faucet limits requests to one thousand test ada daily",
		"The relay topology file lists hot peers and local roots separately",
		"ARC runner scale sets use ephemeral pods labelled per repository",
		"Backups upload encrypted tarballs to object storage nightly",
		"Chrony keeps the block producer clock within ten milliseconds",
		"Ansible roles pin the docker compose version per host group",
		"Terraform state lives in a remote backend with locking enabled",
		"Cardano node release notes list the protocol parameter changes",
	}
}

// TestRunOnARealisticSessionDoesNotCallUnrelatedMemoriesUsed: a multi-thousand-word
// session with Edit/Write bodies, and twenty memories about something else. In the
// session whose calls these are, they are mostly ignored; in any other session, or
// none, they are not judged at all.
func TestRunOnARealisticSessionDoesNotCallUnrelatedMemoriesUsed(t *testing.T) {
	for _, tc := range []struct {
		name        string
		callSession string
		wantJudged  bool
	}{
		{"calls from another session", "sess-other", false},
		{"calls with no session", "", false},
		{"calls from this session", testSession, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, projectID := auditStore(t)
			var ids []string
			for i, c := range unrelatedMemories() {
				id := fmt.Sprintf("UNREL%02d", i)
				seedMemory(t, store, projectID, id, c)
				ids = append(ids, id)
			}
			_ = recordCallIn(t, store, projectID, tc.callSession, ids...)

			s := newTestSignals(t)
			words := devSessionText(t, s)
			if words < 3000 {
				t.Fatalf("fixture is only %d words; the point is a session of several thousand", words)
			}
			res, err := Run(context.Background(), store, projectID, s)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if !tc.wantJudged {
				if res.Verdicts != 0 {
					t.Fatalf("judged %d verdict(s) for %s; they belong to no call of this session", res.Verdicts, tc.name)
				}
				return
			}
			used := 0
			for _, o := range res.byMemory() {
				if o == OutcomeUsed {
					used++
				}
			}
			if res.Verdicts != len(ids) || used > len(ids)/4 {
				t.Errorf("%d of %d unrelated memories were judged `used` over %d words of session text; want mostly ignored", used, res.Verdicts, words)
			}
		})
	}
}

// devMemories are twenty memories from the SAME domain as the session, none of which
// the agent consulted: facts a developer would plausibly hold about the code being
// edited.
func devMemories() []string {
	return []string{
		"The retrieval record sink never fails the search when the write is refused",
		"A schema migration needs one frozen step per version and the schema string together",
		"The assembler stage order decides which verdicts reach the retrieval record",
		"The stop hook scans the session text and writes a sidecar of fingerprints",
		"Verdicts column holds a json array of kept and dropped rows per call",
		"Session id comes from the hook payload and the server environment",
		"The store refuses a schema newer than this build understands",
		"Record sink budget is a quarter of a second so the search is answered first",
		"Reflection consolidates memories and the resolve pass classifies them in batches",
		"The race detector run covers the memory package before every release",
		"Handler wiring passes the project id and session id down to the assembler",
		"Return errors wrapped with the operation name and the session id",
		"Context cancellation stops the write before the store lock is taken",
		"The package comment names the stage and the sink for the record",
		"Fingerprints are keyed hashes so a sidecar cannot be read as plain text",
		"An audit verdict is filed against the call that kept the memory",
		"Prose and tool arguments both feed the token arm of the comparison",
		"Tests build the real store because the write seam is the contract",
		"Assembler decisions record why a row was dropped at each stage",
		"Session scoped reads keep one session's calls apart from another's",
	}
}

// TestSameDomainMemoriesInTheSameSessionRemainProblemB measures what this change does
// NOT fix, so the number is on record rather than assumed. Memories in the session's own
// domain, in a call of the session, meet text that mentions their words simply because
// the session edited the code they describe; the token arm cannot tell that from use.
// The count is logged, not asserted: it is the evidence for the follow-up, and tuning
// the fixture until it passes would erase it. Cross-session and unscoped calls are
// asserted unjudged.
func TestSameDomainMemoriesInTheSameSessionRemainProblemB(t *testing.T) {
	for _, session := range []string{"sess-other", ""} {
		store, projectID := auditStore(t)
		var ids []string
		for i, c := range devMemories() {
			id := fmt.Sprintf("DEV%02d", i)
			seedMemory(t, store, projectID, id, c)
			ids = append(ids, id)
		}
		_ = recordCallIn(t, store, projectID, session, ids...)
		s := newTestSignals(t)
		devSessionText(t, s)
		res, err := Run(context.Background(), store, projectID, s)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Verdicts != 0 {
			t.Errorf("session %q: judged %d same-domain memories against a session that did not make the call", session, res.Verdicts)
		}
	}

	store, projectID := auditStore(t)
	var ids []string
	for i, c := range devMemories() {
		id := fmt.Sprintf("DEV%02d", i)
		seedMemory(t, store, projectID, id, c)
		ids = append(ids, id)
	}
	_ = recordCallIn(t, store, projectID, testSession, ids...)
	s := newTestSignals(t)
	words := devSessionText(t, s)
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	used := 0
	for _, o := range res.byMemory() {
		if o == OutcomeUsed {
			used++
		}
	}
	t.Logf("remaining problem B: same session, same domain, %d words of text: %d of %d memories judged used", words, used, res.Verdicts)
}

// TestSameDomainResidualWithTheOrderInPlace measures the residual problem B after a call
// is judged only against the text written after it, for the call at the start of the
// session (a session-start injection: every turn is after it), in the middle, and late,
// and for both readings of the token arm: tool-call bodies feeding it (shipped) and not
// feeding it (a variant measured here and NOT shipped). The numbers are logged for the
// record; what is asserted is only that moving the call later, or taking the bodies out,
// never produces MORE `used`.
func TestSameDomainResidualWithTheOrderInPlace(t *testing.T) {
	measure := func(callTurn int, feedArgs bool) (used, judged int) {
		store, projectID := auditStore(t)
		var ids []string
		for i, c := range devMemories() {
			id := fmt.Sprintf("DEV%02d", i)
			seedMemory(t, store, projectID, id, c)
			ids = append(ids, id)
		}
		_ = recordCallIn(t, store, projectID, testSession, ids...)
		s := newTestSignals(t)
		devSessionTextFrom(t, s, callTurn, feedArgs)
		res, err := Run(context.Background(), store, projectID, s)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		for _, o := range res.byMemory() {
			if o == OutcomeUsed {
				used++
			}
		}
		return used, res.Verdicts
	}
	prev := map[bool]int{true: 21, false: 21}
	for _, callTurn := range []int{0, 20, 36, 40} {
		with, n := measure(callTurn, true)
		without, _ := measure(callTurn, false)
		t.Logf("call before turn %2d of 40: %d of %d used with tool bodies feeding the token arm, %d of %d without", callTurn, with, n, without, n)
		if with > prev[true] || without > prev[false] || without > with {
			t.Errorf("call before turn %d: used with=%d without=%d (previous %d / %d): a later call or fewer bodies must never add a `used`", callTurn, with, without, prev[true], prev[false])
		}
		prev[true], prev[false] = with, without
	}
}

// TestRunDoesNotJudgeCallsBeforeTheSessionBegins: a call is judged against the text
// written AFTER it. Here the text is written an hour before the call is recorded, so a
// memory the call kept cannot have been used in it. The same text written after the call
// is a use, which is what keeps the test from passing by judging nothing.
func TestRunDoesNotJudgeCallsBeforeTheSessionBegins(t *testing.T) {
	for _, tc := range []struct {
		name string
		at   time.Duration
		want Outcome
	}{
		{"written before the call", -time.Hour, OutcomeIgnored},
		{"written after the call", time.Hour, OutcomeUsed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, projectID := auditStore(t)
			seedMemory(t, store, projectID, "LATE", memContent)

			s := newTestSignals(t)
			s.SetAt(time.Now().Add(tc.at))
			s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
			_ = recordCallIn(t, store, projectID, testSession, "LATE")

			res, err := Run(context.Background(), store, projectID, s)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got := res.byMemory()["LATE"]; got != tc.want {
				t.Fatalf("LATE judged %q for text %s, want %q", got, tc.name, tc.want)
			}
		})
	}
}

// TestEveryArmJudgesOnlyTextAfterTheCall: the id, token, save and negation arms all read
// the view of the signals after the call, so none of them can be satisfied by earlier text.
func TestEveryArmJudgesOnlyTextAfterTheCall(t *testing.T) {
	const id = "0123456789ABCDEF0123456789ABCDEF"
	add := map[string]func(s *Signals){
		"identifier": func(s *Signals) { s.AddProse("as noted in " + id + " the plugin is fine") },
		"token":      func(s *Signals) { s.AddProse("the opencode plugin materializes its transcript under mkdtemp") },
		"save":       func(s *Signals) { s.AddSaveArgs("the opencode plugin materializes its transcript under mkdtemp") },
		"negation": func(s *Signals) {
			s.AddProse("That is wrong: the opencode plugin materializes its transcript under mkdtemp and rm-rfs the directory on hook close")
		},
	}
	for name, fn := range add {
		t.Run(name, func(t *testing.T) {
			judge := func(offset time.Duration) Outcome {
				store, projectID := auditStore(t)
				seedMemory(t, store, projectID, id, memContent)
				s := newTestSignals(t)
				s.SetAt(time.Now().Add(offset))
				fn(s)
				_ = recordCallIn(t, store, projectID, testSession, id)
				res, err := Run(context.Background(), store, projectID, s)
				if err != nil {
					t.Fatalf("Run: %v", err)
				}
				return res.byMemory()[id]
			}
			before, after := judge(-time.Hour), judge(time.Hour)
			if before != OutcomeIgnored {
				t.Errorf("%s arm: text written before the call gave %q, want %q", name, before, OutcomeIgnored)
			}
			if after == OutcomeIgnored {
				t.Errorf("%s arm: the same text written after the call gave %q; the arm never fires, so the check above proves nothing", name, after)
			}
		})
	}
}

// TestTextWithNoInstantIsNeverEvidence: a scanner that could not place a line carries its
// words but the run never counts them for any call, and a scan with no placed text at all
// judges nothing rather than filing every memory as ignored.
func TestTextWithNoInstantIsNeverEvidence(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "UNPLACED", memContent)
	_ = recordCallIn(t, store, projectID, testSession, "UNPLACED")

	s := NewWithHasher(testHasher)
	s.SetSessionID(testSession)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp") // no SetAt
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdicts != 0 || !res.NoOrder {
		t.Fatalf("verdicts = %d, NoOrder = %v: a scan with no instants must judge nothing and say so", res.Verdicts, res.NoOrder)
	}
	if !strings.Contains(res.String(), "no instants") {
		t.Errorf("the summary does not name the unplaced scan:\n%s", res)
	}

	// Mixed: the placed line is after the call, the unplaced one carries the memory's words.
	mixed := NewWithHasher(testHasher)
	mixed.SetSessionID(testSession)
	mixed.AddToolArgs("the opencode plugin materializes its transcript under mkdtemp") // unplaced
	mixed.SetAt(time.Now().Add(time.Hour))
	mixed.AddProse("unrelated narrative about the weather")
	res, err = Run(context.Background(), store, projectID, mixed)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.byMemory()["UNPLACED"]; got != OutcomeIgnored {
		t.Fatalf("an unplaced line counted as evidence: judged %q", got)
	}
}

// TestACallWithNoInstantIsLeftUnjudged: a record whose recorded_at is empty or unreadable
// has no "after", so it is not judged, whatever the text says.
func TestACallWithNoInstantIsLeftUnjudged(t *testing.T) {
	for _, stamp := range []string{"", "not a time"} {
		store, projectID, dbPath := reportStore(t)
		_ = recordCallIn(t, store, projectID, testSession, "USEDID")
		db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatalf("open a second handle: %v", err)
		}
		if _, err := db.Exec(`UPDATE retrieval_record SET recorded_at = ? WHERE project_id = ?`, stamp, projectID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		_ = db.Close()
		s := newTestSignals(t)
		s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
		res, err := Run(context.Background(), store, projectID, s)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if res.Verdicts != 0 || res.UnorderedCalls != 1 {
			t.Errorf("recorded_at %q: verdicts = %d, unordered calls = %d, want 0 and 1", stamp, res.Verdicts, res.UnorderedCalls)
		}
		if !strings.Contains(res.String(), "no readable instant") {
			t.Errorf("recorded_at %q: the summary does not say the call was left unjudged:\n%s", stamp, res)
		}
	}
}

// TestTheSidecarCarriesEveryInstant: the scan is in the hook and the comparison in the
// detached child, so the order has to cross the gap with the fingerprints, for all four
// kinds of signal.
func TestTheSidecarCarriesEveryInstant(t *testing.T) {
	const id = "0123456789ABCDEF0123456789ABCDEF"
	s := newTestSignals(t)
	at := time.Now().Add(-90 * time.Minute).Truncate(time.Millisecond)
	s.SetAt(at)
	s.AddProse("That is wrong: " + id + " is stale and should be ignored")
	s.AddSaveArgs("a saved sentence about the sidecar format")
	s.SetAt(at.Add(time.Minute))
	s.AddToolArgs("the materialized transcript lives under mkdtemp")

	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	back, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	for _, cutoff := range []time.Time{at, at.Add(30 * time.Second), at.Add(2 * time.Minute)} {
		want, got := s.Since(cutoff), back.Since(cutoff)
		if len(want.prose) != len(got.prose) || len(want.saves) != len(got.saves) ||
			len(want.ids) != len(got.ids) || len(want.negated) != len(got.negated) {
			t.Errorf("view at %v differs after a round trip: want %d/%d/%d/%d got %d/%d/%d/%d (prose/saves/ids/neg)",
				cutoff, len(want.prose), len(want.saves), len(want.ids), len(want.negated),
				len(got.prose), len(got.saves), len(got.ids), len(got.negated))
		}
	}
	if n := len(back.Since(at.Add(30 * time.Second)).prose); n == 0 || n == len(back.prose) {
		t.Errorf("the view after the first turn holds %d of %d prose tokens: the instants did not survive", n, len(back.prose))
	}
	if len(back.Since(at.Add(2*time.Minute)).prose) != 0 {
		t.Errorf("a cutoff after every line still holds prose")
	}
}

// TestTheSidecarCarriesEveryTurn: the scan is in the hook and the comparison in the
// detached child, so the turns have to cross the gap — a sidecar that flattened them
// back into one bag would let the union judge where a turn must (wcatz/ghost#932).
//
// Three turns: the first carries a whole memory, which both the turn and the union
// catch; the second and third carry three words each of a twelve-word memory, which
// only the union clears. Judged identically before and after the round trip, both
// ways.
func TestTheSidecarCarriesEveryTurn(t *testing.T) {
	at := time.Now().Add(-90 * time.Minute).Truncate(time.Millisecond)
	s := newTestSignals(t)
	s.SetAt(at)
	s.AddProse(memContent)
	s.SetAt(at.Add(time.Minute))
	s.AddProse("sweep migration assembler")
	s.SetAt(at.Add(2 * time.Minute))
	s.AddProse("verdicts kestrel gateway")

	whole := memTokens(t)
	split := testTokens("sweep migration assembler verdicts kestrel gateway rotates bearer nightly upstream proxies stale")
	if len(split) != 12 {
		t.Fatalf("split fixture = %d tokens, want 12", len(split))
	}
	if !clearsTokenBar(sharedTokens(s.prose, split), len(split)) {
		t.Fatalf("precondition: the union of the two later turns clears the bar, so only the union could call this used")
	}
	if s.matches(split) {
		t.Fatal("precondition: a turn already clears the split memory's bar, so this fixture cannot tell the readings apart")
	}
	if !s.matches(whole) {
		t.Fatal("precondition: the turn carrying the whole memory does not clear its bar")
	}

	path, err := WriteSidecar(t.TempDir(), s)
	if err != nil {
		t.Fatalf("WriteSidecar: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the file: %v", err)
	}
	if n := strings.Count(string(raw), "\nturn "); n != 3 {
		t.Errorf("the sidecar carries %d turn lines, want 3:\n%s", n, raw)
	}
	if strings.Contains(string(raw), "\nprose ") {
		t.Errorf("the sidecar still carries prose lines, which cannot tell one turn from another:\n%s", raw)
	}
	back, err := ReadSidecar(path, testHasher)
	if err != nil {
		t.Fatalf("ReadSidecar: %v", err)
	}
	if !back.matches(whole) {
		t.Errorf("a turn that carried a whole memory was judged ignored after the round trip")
	}
	if back.matches(split) {
		t.Errorf("a split only the union clears was judged used/%s after the round trip", SignalToken)
	}
	if !clearsTokenBar(sharedTokens(back.prose, split), len(split)) {
		t.Errorf("the union of the round-tripped prose no longer clears the split memory's bar: the turns did not survive")
	}
}

// TestUnplacedTextIsNeverATurnThatCounts: a turn under SetAt(time.Time{}) is counted
// by Unplaced() — the scan is partly unplaced and says so — and never clears the
// token bar, however much of a memory it carries (wcatz/ghost#932). The union check
// keeps the fixture honest: the words are there and would clear it.
func TestUnplacedTextIsNeverATurnThatCounts(t *testing.T) {
	s := NewWithHasher(testHasher)
	s.SetAt(time.Time{})
	s.AddProse(memContent)

	toks := memTokens(t)
	if !clearsTokenBar(sharedTokens(s.prose, toks), len(toks)) {
		t.Fatalf("precondition: the unplaced text alone would clear the bar, so this fixture proves nothing")
	}
	if n := s.Unplaced(); n == 0 {
		t.Errorf("Unplaced() = 0, want the prose written with no instant")
	}
	if s.matches(toks) {
		t.Errorf("a turn with no instant cleared the token bar: unplaced text was judged evidence")
	}
	// Placed text clears, so the two differ by the instant and nothing else.
	s.SetAt(time.Now())
	s.AddProse(memContent)
	if !s.matches(toks) {
		t.Errorf("the same words under an instant did not clear the bar")
	}
}

// TestASidecarOfAnOlderVersionIsRefusedByName: a v4 file carried no turns, so reading
// it as v5 would leave the token arm with one bag over the whole session — the very
// union this build refuses. It is refused by name, alongside the older formats.
func TestASidecarOfAnOlderVersionIsRefusedByName(t *testing.T) {
	if SidecarHeader == sidecarV4 {
		t.Fatal("the header was not bumped")
	}
	path := t.TempDir() + "/ghost-audit-old.signals"
	if err := writeFileString(path, sidecarV3+"\nprose 0123456789abcdef 1\nsession \"s\"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := ReadSidecar(path, testHasher)
	if err == nil {
		t.Fatal("ReadSidecar accepted a v3 file; every signal in it would be unplaced")
	}
	for _, want := range []string{SidecarHeader, sidecarV4, sidecarV3} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not name %q", err, want)
		}
	}
	// A v4 file: readable by the build that wrote it, and refused here because its
	// prose lines say nothing about which turn any word arrived in.
	if err := writeFileString(path, sidecarV4+"\nprose 0123456789abcdef 1\nsession \"s\"\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadSidecar(path, testHasher); err == nil {
		t.Errorf("ReadSidecar accepted a %s file; its prose lines are a union over the whole session", sidecarV4)
	} else {
		for _, want := range []string{SidecarHeader, sidecarV4} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
	}
	// A v5 turn line with no instant is refused, not defaulted.
	if err := writeFileString(path, SidecarHeader+"\nturn\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadSidecar(path, testHasher); err == nil {
		t.Error("a turn line with no instant was accepted")
	}
	// A v5 file still carrying a prose line is refused: prose is not a field this
	// build writes, and guessing at it would rebuild the union.
	if err := writeFileString(path, SidecarHeader+"\nprose 0123456789abcdef 1\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := ReadSidecar(path, testHasher); err == nil {
		t.Error("a prose line in a v5 file was accepted")
	}
}

// TestSweepSidecarsDeletesOlderVersionSidecars: a file an older build wrote can never be
// read again, so the sweep takes it (once stale) instead of leaving it behind for good.
func TestSweepSidecarsDeletesOlderVersionSidecars(t *testing.T) {
	dir := t.TempDir()
	for name, header := range map[string]string{
		"ghost-audit-v1.signals": sidecarV1, "ghost-audit-v2.signals": sidecarV2,
		"ghost-audit-v3.signals": sidecarV3, "ghost-audit-v4.signals": sidecarV4,
		"ghost-audit-v5.signals": SidecarHeader,
	} {
		p := dir + "/" + name
		if err := writeFileString(p, header+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
		if err := touchOlderThan(p, 2*sidecarStaleAfter); err != nil {
			t.Fatalf("age: %v", err)
		}
	}
	foreign := dir + "/ghost-audit-foreign.signals"
	if err := writeFileString(foreign, "# somebody else's file\n"); err != nil {
		t.Fatal(err)
	}
	if err := touchOlderThan(foreign, 2*sidecarStaleAfter); err != nil {
		t.Fatal(err)
	}
	n, err := SweepSidecars(dir, sidecarStaleAfter)
	if err != nil {
		t.Fatalf("SweepSidecars: %v", err)
	}
	if n != 5 {
		t.Errorf("swept %d, want the 5 sidecars of every version", n)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("a file that is not a sidecar was removed: %v", err)
	}
}

// TestTheReportKeepsUnscopedVerdictsOutOfEveryFigure: a verdict filed with no session
// (every one filed before the audit was session-scoped) was judged against a session
// that may not have made its call. The report names it and counts it in no figure, and
// the row stays in the store.
func TestTheReportKeepsUnscopedVerdictsOutOfEveryFigure(t *testing.T) {
	store, projectID, _ := reportStore(t)
	rowid := recordCall(t, store, projectID, "search", "USEDID", "IGNID")
	fileVerdict(t, store,
		memory.RetrievalAuditRow{ProjectID: projectID, SessionID: "", Source: "search", MemoryID: "USEDID",
			Outcome: string(OutcomeUsed), Signal: "token", RecordRowID: rowid},
		memory.RetrievalAuditRow{ProjectID: projectID, SessionID: testSession, Source: "search", MemoryID: "IGNID",
			Outcome: string(OutcomeIgnored), RecordRowID: rowid},
	)
	for name, build := range map[string]func(context.Context, *memory.Store, ReportOptions) (Report, error){
		"BuildReport": BuildReport, "BuildStoreReport": BuildStoreReport,
	} {
		rep, err := build(context.Background(), store, ReportOptions{ProjectID: projectID})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checkUnscoped(t, name, rep)
	}
	rows, _ := store.RetrievalAudits(context.Background(), projectID, "")
	if len(rows) != 2 {
		t.Errorf("%d rows in the store, want 2: nothing is deleted", len(rows))
	}
}

func checkUnscoped(t *testing.T, name string, rep Report) {
	t.Helper()
	search := rep.Source("search")
	if search.Scored != 1 || search.Used != 0 || search.Ignored != 1 || search.Unscoped != 1 {
		t.Fatalf("%s: scored=%d used=%d ignored=%d unscoped=%d, want 1/0/1/1: the unscoped `used` must be in no figure", name, search.Scored, search.Used, search.Ignored, search.Unscoped)
	}
	if !strings.Contains(rep.String(), "carry no session") {
		t.Errorf("%s: the report does not name the unscoped verdicts:\n%s", name, rep.String())
	}
}

// TestARunThatFindsNoSessionOnTheRecordsSaysSo: the mirror of NoSession. A host whose
// server cannot name its session records "" on every call, so a scan WITH a session id
// matches none of them, and the summary must not print that as a clean zero: "the agent
// used nothing" and "this host's calls carry no session, so the audit never ran" are
// different claims.
func TestARunThatFindsNoSessionOnTheRecordsSaysSo(t *testing.T) {
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "NOSESS", memContent)
	_ = recordCallIn(t, store, projectID, "", "NOSESS")
	_ = recordCallIn(t, store, projectID, "", "NOSESS")

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp")
	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Verdicts != 0 {
		t.Fatalf("Verdicts = %d, want 0", res.Verdicts)
	}
	if res.UnscopedCalls != 2 {
		t.Errorf("UnscopedCalls = %d, want the 2 calls that name no session", res.UnscopedCalls)
	}
	if !strings.Contains(res.String(), "carry no session id") {
		t.Errorf("the summary prints a confident zero instead of saying the project's calls name no session:\n%s", res)
	}

	// And a project with no calls at all says nothing of the kind.
	empty, _ := auditStore(t)
	res2, err := Run(context.Background(), empty, "p1", s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res2.UnscopedCalls != 0 || strings.Contains(res2.String(), "carry no session id") {
		t.Errorf("a project with no calls reported unscoped calls: %+v", res2)
	}
}
