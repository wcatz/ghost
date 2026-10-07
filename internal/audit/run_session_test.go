package audit

// The retrieval audit judges one session's calls against that session's text.
//
// Before this scope existed `Run` read the project's newest calls whatever session made
// them and compared every kept memory with the one session the stop hook had just
// scanned. On a real store that filed 615 verdicts, every one of them `used`, across
// calls dated four different days.

import (
	"context"
	"fmt"
	"strings"
	"testing"

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
		text := narrative[i%len(narrative)]
		s.AddProse(text)
		words += len(strings.Fields(text))
		// Edit/Write-shaped tool arguments: one file body per call.
		args := body(4)
		s.AddToolArgs(args)
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

// TestRunDoesNotJudgeCallsBeforeTheSessionBegins is the documented follow-up to the
// session scope: a call is judged against the text written AFTER it, and the scanned
// signals carry no order or timestamp to say which text that is. Here the text is
// scanned (written) before the call is recorded, so a memory the call kept cannot have
// been used in it; today it is judged `used`.
func TestRunDoesNotJudgeCallsBeforeTheSessionBegins(t *testing.T) {
	t.Skip("follow-up to #648: scanned signals have no order or timestamp, so a call is still judged against the whole session's text, including text written before it")
	store, projectID := auditStore(t)
	seedMemory(t, store, projectID, "LATE", memContent)

	s := newTestSignals(t)
	s.AddProse("the opencode plugin materializes its transcript under mkdtemp") // written first
	_ = recordCallIn(t, store, projectID, testSession, "LATE")                  // retrieved afterwards

	res, err := Run(context.Background(), store, projectID, s)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := res.byMemory()["LATE"]; got == OutcomeUsed {
		t.Fatalf("LATE judged %q from text written before the call that kept it", got)
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
