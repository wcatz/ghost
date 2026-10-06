package memory

// #648 slice 2: an agent's own negative evidence about ONE memory.
//
// A flag is a claim somebody made, not a verdict the store reached, so it is
// append-only and attributed, and it is stamped with the hash of the content it
// was about — the same rule retrieval_audit's content_hash follows (#879), for
// the same reason: text rewritten under a stable id makes the old claim a claim
// about words nobody can read any more.
//
// What it must NOT do is act. Nothing here resolves, deletes, demotes or
// re-ranks: the flag is a COUNT that reaches the classifier's context beside the
// note, and the classifier decides. The tests below hold the three properties
// that rot silently — the reason text never reaches a prompt-facing renderer,
// the flag never wins the "latest verdict" tuple, and a flag on rewritten
// content is withdrawn rather than silently counting against the new text.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// flagReasonMarker is deliberately unmistakable text. If any renderer starts
// carrying a flag's reason, this is the byte sequence that shows up in a prompt.
const flagReasonMarker = "ZZREASONNEVERLEAVESSTOREZZ"

// flagRows is the fixture's own read of the table, newest last. Tests assert on
// the ROWS rather than on a count returned by the writer, because "the write was
// refused" and "the write reported a refusal" are the same claim from where a
// caller stands and different claims from here.
type flagRows struct {
	rowid                            int64
	project, memory, kind, reason    string
	hash, agent, session, recordedAt string
}

func readFlagRows(t *testing.T, s *Store) []flagRows {
	t.Helper()
	rows, err := s.db.Query(`SELECT rowid, project_id, memory_id, kind, reason, content_hash, agent, session_id, recorded_at
		FROM memory_flags ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read memory_flags: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	var out []flagRows
	for rows.Next() {
		var r flagRows
		if err := rows.Scan(&r.rowid, &r.project, &r.memory, &r.kind, &r.reason, &r.hash, &r.agent, &r.session, &r.recordedAt); err != nil {
			t.Fatalf("scan memory_flags: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate memory_flags: %v", err)
	}
	return out
}

// seedFlaggable stores one memory in testProject and returns its id and content.
func seedFlaggable(t *testing.T, s *Store, content string) (id string) {
	t.Helper()
	ctx := context.Background()
	created, err := s.Create(ctx, testProject, Memory{
		Category: "fact", Content: content, Source: "mcp", Importance: 0.5,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// moveMemoryTo puts a memory under another project. There is no store method for
// it because no product path moves a memory between projects — this fixture
// needs the state, and a fixture that needed a writer nobody calls would be
// inventing one.
func moveMemoryTo(t *testing.T, s *Store, id, project string) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`UPDATE memories SET project_id = ? WHERE id = ?`, project, id); err != nil {
		t.Fatalf("move %s to %s: %v", id, project, err)
	}
}

// TestFlagMemoryWritesAnAttributedRowStampedWithTheContentHash: the write
// itself, and the three things a later reader needs from it — WHO said it, WHEN,
// and about WHICH text.
//
// The stamp is the part a caller cannot see and the part that makes every other
// property here possible: without it a flag is a permanent objection to an id,
// and Ghost rewrites content under a stable id all the time.
func TestFlagMemoryWritesAnAttributedRowStampedWithTheContentHash(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "the relay listens on port 2222 in staging"
	id := seedFlaggable(t, s, content)

	err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject,
		MemoryID:  id,
		Kind:      FlagKindWrong,
		Reason:    "port 2222 was decommissioned in the June migration",
		Agent:     "claude-code",
		SessionID: "ses_flag_1",
	})
	if err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}

	rows := readFlagRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("memory_flags holds %d rows, want 1", len(rows))
	}
	got := rows[0]
	if got.memory != id || got.project != testProject {
		t.Errorf("row names memory %q in project %q, want %q in %q", got.memory, got.project, id, testProject)
	}
	if got.kind != FlagKindWrong {
		t.Errorf("kind = %q, want %q", got.kind, FlagKindWrong)
	}
	if got.agent != "claude-code" || got.session != "ses_flag_1" {
		t.Errorf("attribution = agent %q session %q, want claude-code/ses_flag_1", got.agent, got.session)
	}
	if got.recordedAt == "" {
		t.Error("recorded_at is empty: a flag with no stamp cannot be ordered against anything")
	}
	if got.hash != ContentHash(content) {
		t.Errorf("content_hash = %q, want the hash of the content the flag was about (%q)",
			got.hash, ContentHash(content))
	}
	// The reason IS stored — it is free text the operator will want when they
	// read the table back, and refusing to keep it would make every refusal above
	// unexplainable after the fact.
	if !strings.Contains(got.reason, "June migration") {
		t.Errorf("reason = %q, want the caller's own text stored verbatim", got.reason)
	}
}

// TestAFlagIsAppendedNotOverwritten: two flags on one memory are two claims, and
// the second must not erase the first. The whole point of "append-only" is that
// a later agent cannot retract an earlier agent's objection by flagging the same
// row again.
func TestAFlagIsAppendedNotOverwritten(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedFlaggable(t, s, "the deploy runs from the ops repository")

	for i, kind := range []string{FlagKindWrong, FlagKindStale} {
		if err := s.FlagMemory(ctx, FlagMemoryRequest{
			ProjectID: testProject, MemoryID: id, Kind: kind,
			Reason: "objection number " + itoa(i+1), Agent: "codex", SessionID: "ses_" + itoa(i),
		}); err != nil {
			t.Fatalf("FlagMemory(%d): %v", i, err)
		}
	}

	rows := readFlagRows(t, s)
	if len(rows) != 2 {
		t.Fatalf("memory_flags holds %d rows, want 2 — a second flag appends rather than replacing", len(rows))
	}
	if rows[0].reason != "objection number 1" || rows[1].reason != "objection number 2" {
		t.Errorf("reasons = %q, %q; want both preserved in the order they were filed",
			rows[0].reason, rows[1].reason)
	}
	if rows[0].rowid == rows[1].rowid {
		t.Error("both flags share an id, so the second overwrote the first rather than appending")
	}
}

// TestFlagMemoryRefusalsWriteNothing: every reason to say no, and the guarantee
// that saying no left the table alone.
//
// The refusals are distinct SENTINELS rather than one generic error because the
// caller has different fixes available: an unknown id means the agent guessed,
// another project means it guessed about ownership, a bad kind means the tool's
// contract was ignored, and a bad reason means the text has to change. A single
// error would make all four unactionable.
func TestFlagMemoryRefusalsWriteNothing(t *testing.T) {
	const reason = "this claims a port that no longer exists"
	otherProject := "other-project"

	for _, tc := range []struct {
		name string
		req  FlagMemoryRequest
		want error
	}{
		{
			name: "unknown memory id",
			req:  FlagMemoryRequest{ProjectID: testProject, MemoryID: "NOSUCHID00", Kind: FlagKindWrong, Reason: reason},
			want: ErrFlagNoMemory,
		},
		{
			// The memory is moved to otherProject below, so this request — which
			// claims testProject — is wrong about ownership while its id is fine.
			// That is the whole distinction the sentinel exists to make.
			name: "memory in another project",
			req:  FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: FlagKindWrong, Reason: reason},
			want: ErrFlagWrongProject,
		},
		{
			name: "kind the tool does not define",
			req:  FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: "useful", Reason: reason},
			want: ErrFlagKind,
		},
		{
			name: "no kind at all",
			req:  FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: "", Reason: reason},
			want: ErrFlagKind,
		},
		{
			name: "no reason at all",
			req:  FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: FlagKindWrong, Reason: ""},
			want: ErrFlagReason,
		},
		{
			name: "a reason that is only whitespace",
			req:  FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: FlagKindWrong, Reason: "   \n\t "},
			want: ErrFlagReason,
		},
		{
			name: "a reason past the bound",
			req: FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: FlagKindStale,
				Reason: strings.Repeat("r", FlagReasonMax+1)},
			want: ErrFlagReason,
		},
		{
			name: "a credential-shaped reason",
			req: FlagMemoryRequest{ProjectID: testProject, MemoryID: "THEDUMMYID", Kind: FlagKindWrong,
				Reason: "the token is ghp_" + strings.Repeat("a", 36) + " and it is still live"},
			want: ErrSecretContent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			ctx := context.Background()
			// A real memory in a real OTHER project, so "wrong project" and
			// "unknown id" are decided by two different facts rather than by
			// neither memory existing.
			if err := s.EnsureProject(ctx, otherProject, "/tmp/"+otherProject, otherProject); err != nil {
				t.Fatalf("EnsureProject: %v", err)
			}
			real := seedFlaggable(t, s, "the memory the flag is actually about")
			donor := seedFlaggable(t, s, "a memory the other project owns")
			moveMemoryTo(t, s, donor, otherProject)
			req := tc.req
			if req.MemoryID == "THEDUMMYID" {
				req.MemoryID = real
			}
			if tc.want == ErrFlagWrongProject {
				req.MemoryID = donor
			}

			err := s.FlagMemory(ctx, req)
			if err == nil {
				t.Fatalf("FlagMemory accepted a request it must refuse (%+v)", req)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want it to unwrap to %v", err, tc.want)
			}
			if rows := readFlagRows(t, s); len(rows) != 0 {
				t.Errorf("a refused flag left %d row(s) behind: %+v", len(rows), rows)
			}
			// The refusal must not quote the value it refused — that is the whole
			// of what a credential-shaped reason has to guarantee, and the reason
			// the guard exists on every other write path.
			if tc.want == ErrSecretContent && strings.Contains(err.Error(), "ghp_") {
				t.Errorf("the refusal quoted the credential back: %q", err.Error())
			}
		})
	}
}

// TestFlagMemoryAcceptsAReasonAtTheBound: the bound is 500 RUNES and the value
// at it is accepted. A bound only pinned from the refusing side is one a reader
// cannot tell from a bound set one lower, so both edges are asserted.
func TestFlagMemoryAcceptsAReasonAtTheBound(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedFlaggable(t, s, "the cache key includes the schema version")

	atBound := strings.Repeat("r", FlagReasonMax)
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: id, Kind: FlagKindStale, Reason: atBound,
	}); err != nil {
		t.Fatalf("FlagMemory at the %d-rune bound: %v", FlagReasonMax, err)
	}
	rows := readFlagRows(t, s)
	if len(rows) != 1 || rows[0].reason != atBound {
		t.Fatalf("stored reason is %d bytes, want exactly the %d runes the caller sent",
			len(rows[0].reason), FlagReasonMax)
	}
	// A rune, not a byte: an accented character is two bytes and one rune, so a
	// byte-counted bound would refuse text this contract allows.
	accented := strings.Repeat("é", FlagReasonMax)
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: id, Kind: FlagKindStale, Reason: accented,
	}); err != nil {
		t.Fatalf("FlagMemory with a %d-rune (2-byte) reason: %v", FlagReasonMax, err)
	}
}

// TestAFlagIsWithdrawnWhenTheContentChangesButSurvivesAMetadataEdit: #879's
// hash rule, applied to the flag.
//
// The rewrite arm errs toward silence (the objection is about text that is
// gone), and the metadata arm errs toward keeping it (a retag says nothing about
// the claim). Both directions matter: the first stops an old objection following
// an id into new text, and the second stops a retag burying a live one — which
// is exactly the silent failure updated_at caused before content_hash existed.
func TestAFlagIsWithdrawnWhenTheContentChangesButSurvivesAMetadataEdit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Withdrawn: the content is rewritten under the same id after the flag.
	rewritten := seedFlaggable(t, s, "the scheduler runs every six hours")
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: rewritten, Kind: FlagKindWrong, Reason: flagReasonMarker,
	}); err != nil {
		t.Fatalf("FlagMemory (rewrite arm): %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, rewritten,
		strPtr("the scheduler runs every fifteen minutes"), nil, nil, nil); err != nil {
		t.Fatalf("UpdateMemory (rewrite): %v", err)
	}

	// Kept: a metadata-only edit moves updated_at but not one byte of the text.
	retagged := seedFlaggable(t, s, "the queue drains on the hour")
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: retagged, Kind: FlagKindStale, Reason: "the queue moved to the worker pool",
	}); err != nil {
		t.Fatalf("FlagMemory (metadata arm): %v", err)
	}
	if err := s.UpdateMemory(ctx, testProject, retagged, nil, nil, nil, []string{"ops"}); err != nil {
		t.Fatalf("UpdateMemory (retag): %v", err)
	}

	got, err := s.UsefulnessByMemory(ctx, testProject)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	if ev, ok := got[rewritten]; ok {
		t.Errorf("the rewritten memory still carries flag evidence %+v: the flag was stamped over text the "+
			"store no longer holds", ev)
	}
	if ev := got[retagged]; ev.Flagged != 1 {
		t.Errorf("the retagged memory's Flagged = %d, want 1 — a metadata-only write must not withdraw a "+
			"live objection (#879's rule, applied here)", ev.Flagged)
	}
}

// TestTheFlaggedCountReachesTheEvidenceLineWithoutTheReason: the whole effect of
// a flag is one number in one fixed-format line.
//
// The reason is stored — see above — and must appear NOWHERE in what this
// package renders. It is free text an agent wrote, it lands in a prompt sent to
// a third-party model if it ever escapes, and its whole value is being read by a
// human in the store, not by the classifier. The marker makes the absence a
// measurement rather than a claim.
func TestTheFlaggedCountReachesTheEvidenceLineWithoutTheReason(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedFlaggable(t, s, "the failover target is the secondary region")

	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: id, Kind: FlagKindWrong,
		Reason: flagReasonMarker + " because the primary took over in March",
		Agent:  "codex", SessionID: "ses_line",
	}); err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}

	got, err := s.UsefulnessByMemory(ctx, testProject)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	ev, ok := got[id]
	if !ok {
		t.Fatalf("a flagged memory carries no evidence at all: %+v", got)
	}
	if ev.Flagged != 1 {
		t.Errorf("Flagged = %d, want 1", ev.Flagged)
	}
	line := ev.Line()
	if line != "audit: verdicts flagged=1" {
		t.Errorf("Line() = %q, want the fixed format %q", line, "audit: verdicts flagged=1")
	}
	if strings.Contains(line, flagReasonMarker) {
		t.Errorf("the reason text reached the evidence line:\n%s", line)
	}
	// And nothing else in the row leaked either: the line is counts and the
	// session id of the LATEST VERDICT, and a flag is not a verdict.
	if strings.Contains(line, "codex") || strings.Contains(line, "ses_line") {
		t.Errorf("the flag's attribution reached the evidence line, which is a claim about a verdict:\n%s", line)
	}
}

// TestAFlagDoesNotWinTheLatestVerdictTuple: the flag is not a verdict, and the
// one place the difference is observable is "latest session / latest verdict".
//
// Rowids do not compare across two tables, and a flag must never be reported as
// the moment this memory was last DOUBTED by a retrieval — a retrieval is what
// the stamp is a stamp of. So the tuple stays audit-only while the flag still
// counts.
func TestAFlagDoesNotWinTheLatestVerdictTuple(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const content = "the retry budget is three attempts"
	id := seedFlaggable(t, s, content)

	// The verdict is stamped OLD and the flag is stamped now, so a reader that
	// let flags into the tuple would report the flag's instant instead.
	usefulnessVerdictHashed(t, s.db, testProject, id, VerdictOutcomeContradicted,
		"ses_verdict", "2026-09-24 10:00:00", ContentHash(content))
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: id, Kind: FlagKindStale, Reason: flagReasonMarker,
		Agent: "codex", SessionID: "ses_flag_later",
	}); err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}

	got, err := s.UsefulnessByMemory(ctx, testProject)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	ev := got[id]
	if ev.Flagged != 1 || ev.Contradicted != 1 {
		t.Fatalf("evidence = %+v, want one contradiction and one flag", ev)
	}
	if ev.LastSession != "ses_verdict" || ev.LastAt != "2026-09-24 10:00:00" {
		t.Errorf("latest = session %q at %q, want the VERDICT's (ses_verdict at 2026-09-24 10:00:00): a flag "+
			"is not a retrieval verdict and must not be reported as the moment this memory was last doubted",
			ev.LastSession, ev.LastAt)
	}
}

// TestAPureFlagStillRendersALine: the flag-only case, which is the one that
// decides whether a flag is negative evidence AT ALL rather than a footnote
// attached to a contradiction. There is no verdict here, so there is no latest
// tuple to report — and the line still says something.
func TestAPureFlagStillRendersALine(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedFlaggable(t, s, "the retention window is ninety days")

	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: id, Kind: FlagKindStale, Reason: flagReasonMarker,
	}); err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}
	got, err := s.UsefulnessByMemory(ctx, testProject)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	line := got[id].Line()
	if line != "audit: verdicts flagged=1" {
		t.Errorf("Line() = %q, want %q", line, "audit: verdicts flagged=1")
	}
}

// TestAPurgeTakesTheFlagsWithTheMemory: a purge exists to erase what is recorded
// ABOUT a memory. A flag row is exactly that — an agent's objection, with its
// reason — and a purge that left it would report success over the row it was
// asked to remove. The delete is explicit because the purge keeps the memory, so
// no foreign key ever fires.
func TestAPurgeTakesTheFlagsWithTheMemory(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id := seedFlaggable(t, s, "the backup runs at 02:00 UTC")
	other := seedFlaggable(t, s, "the restore point is kept for a week")

	for _, mem := range []string{id, other} {
		if err := s.FlagMemory(ctx, FlagMemoryRequest{
			ProjectID: testProject, MemoryID: mem, Kind: FlagKindWrong, Reason: flagReasonMarker,
		}); err != nil {
			t.Fatalf("FlagMemory: %v", err)
		}
	}

	if _, err := s.PurgeMemoryHistory(ctx, id); err != nil {
		t.Fatalf("PurgeMemoryHistory: %v", err)
	}

	rows := readFlagRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("memory_flags holds %d rows after a purge of one of two flagged memories, want 1", len(rows))
	}
	if rows[0].memory != other {
		t.Errorf("the surviving flag belongs to %q, want the un-purged memory %q", rows[0].memory, other)
	}
}

// TestMergeProjectReassignsTheFlags: memory_flags.project_id is deliberately not
// a foreign key (the project purge reaches it with a plain predicate), so
// nothing cascades it and a merge that forgot it would orphan every flag of the
// outgoing project — under a project id that no longer exists, in a table only
// the project read reaches.
func TestMergeProjectReassignsTheFlags(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const doomed = "doomed-project"
	if err := s.EnsureProject(ctx, doomed, "/tmp/"+doomed, doomed); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id := seedFlaggable(t, s, "the canary checks the scheduler every minute")
	moveMemoryTo(t, s, id, doomed)
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: doomed, MemoryID: id, Kind: FlagKindStale, Reason: flagReasonMarker,
	}); err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}

	if err := s.MergeProject(ctx, doomed, testProject); err != nil {
		t.Fatalf("MergeProject: %v", err)
	}

	rows := readFlagRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("memory_flags holds %d rows after the merge, want 1 — the flags were dropped with the project",
			len(rows))
	}
	if rows[0].project != testProject {
		t.Errorf("flag's project_id = %q, want the survivor %q", rows[0].project, testProject)
	}
	// And the reader still finds it, which is what the reassignment is for: the
	// evidence is read per project, so an orphan here is invisible rather than
	// merely misattributed.
	got, err := s.UsefulnessByMemory(ctx, testProject)
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	if ev := got[id]; ev.Flagged != 1 {
		t.Errorf("the merged project's evidence for the flagged memory = %+v, want Flagged 1", ev)
	}
}

// TestDeleteProjectTakesTheFlagsWithIt: the same rows from the other side. The
// project purge deletes the memories (so the foreign key cascades) AND has to
// reach the flags directly, because the dry-run/apply pair counts rows before
// anything is deleted and a table it never names would keep rows whose memory
// and project are both gone.
func TestDeleteProjectTakesTheFlagsWithIt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	const doomed = "doomed-delete"
	if err := s.EnsureProject(ctx, doomed, "/tmp/"+doomed, doomed); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id := seedFlaggable(t, s, "the notifier retries three times")
	moveMemoryTo(t, s, id, doomed)
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: doomed, MemoryID: id, Kind: FlagKindWrong, Reason: flagReasonMarker,
	}); err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}
	// A second project's flag, so "the delete reached this table" is a statement
	// about SCOPING and not about a table being emptied.
	keep := seedFlaggable(t, s, "the notifier's dead letter queue is kept for a day")
	if err := s.FlagMemory(ctx, FlagMemoryRequest{
		ProjectID: testProject, MemoryID: keep, Kind: FlagKindStale, Reason: flagReasonMarker,
	}); err != nil {
		t.Fatalf("FlagMemory (survivor): %v", err)
	}

	if _, err := s.DeleteProject(ctx, doomed, true); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}

	rows := readFlagRows(t, s)
	if len(rows) != 1 {
		t.Fatalf("memory_flags holds %d rows after deleting one of two projects, want 1", len(rows))
	}
	if rows[0].project != testProject {
		t.Errorf("the surviving flag belongs to project %q, want %q", rows[0].project, testProject)
	}
}
