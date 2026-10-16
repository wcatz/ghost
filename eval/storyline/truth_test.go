package main

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// gradedNames is every check name a storyline's grade can produce, read off a
// graded synthetic run: one session per stage, every probe answered, nothing in
// any block. The names do not depend on the verdicts, so this is the set a
// KnownFailing key has to come from.
func gradedNames(s Storyline) map[string]bool {
	res := &Result{Story: s, Opening: map[string]string{}, Probes: map[string]string{}}
	for _, r := range s.Opening {
		res.Opening[r.Key] = "id-" + r.Key
	}
	for i, st := range s.Stages {
		sess := Session{Index: i, Saved: map[string]string{}, Probes: map[string]string{}}
		for _, r := range st.Records {
			sess.Saved[r.Key] = "id-" + r.Key
		}
		for _, p := range st.Probes {
			sess.Probes[p.Name] = ""
		}
		res.Sessions = append(res.Sessions, sess)
	}
	for _, p := range s.Probes {
		res.Probes[p.Name] = ""
	}
	names := map[string]bool{}
	for _, c := range grade(res) {
		if names[c.Name] {
			// A duplicated name would make a KnownFailing entry ambiguous.
			names["DUPLICATE "+c.Name] = true
		}
		names[c.Name] = true
	}
	return names
}

// TestKnownFailingNamesAreRealChecks keeps the known-failing lists from rotting:
// an entry that names no check the storyline produces (a typo, a renamed check)
// would silently stop marking anything, and a real failure under that name
// would then gate a run the list was meant to keep green.
func TestKnownFailingNamesAreRealChecks(t *testing.T) {
	for _, s := range storylines() {
		names := gradedNames(s)
		for n := range names {
			if strings.HasPrefix(n, "DUPLICATE ") {
				t.Errorf("%s: two checks share the name %q", s.Key, strings.TrimPrefix(n, "DUPLICATE "))
			}
		}
		for name := range s.KnownFailing {
			if !names[name] {
				t.Errorf("%s: known-failing %q is not a check this storyline grades", s.Key, name)
			}
		}
	}
}

// TestKnownFailingIsReportedButNotGating covers both directions: a listed check
// that fails is Known and does not fail the run, and a listed check that passes
// is reported as an unexpected pass rather than silently.
func TestKnownFailingIsReportedButNotGating(t *testing.T) {
	s := ReversedDecision()
	s.KnownFailing = map[string]string{"a": "filed", "b": "filed"}
	checks := applyKnown(s, []Check{
		{Name: "a", Passed: false, Detail: "observed"},
		{Name: "b", Passed: true, Detail: "observed"},
		{Name: "c", Passed: true},
	})
	res := &Result{Story: s, Checks: checks}
	if !checks[0].Known || checks[0].UnexpectedPass {
		t.Errorf("failed listed check: Known=%v UnexpectedPass=%v, want known", checks[0].Known, checks[0].UnexpectedPass)
	}
	if !checks[1].UnexpectedPass || checks[1].Known {
		t.Errorf("passed listed check: Known=%v UnexpectedPass=%v, want an unexpected pass", checks[1].Known, checks[1].UnexpectedPass)
	}
	if !res.Passed() || len(res.FailedNames()) != 0 {
		t.Errorf("a run whose only failure is known must pass; failed=%v", res.FailedNames())
	}
	if got := res.KnownNames(); !slices.Equal(got, []string{"a"}) {
		t.Errorf("KnownNames = %v, want [a]", got)
	}
	if v := checks[0].verdict(); v != "FAIL (known)" {
		t.Errorf("known verdict = %q", v)
	}
	if v := checks[1].verdict(); !strings.Contains(v, "unexpected") {
		t.Errorf("unexpected-pass verdict = %q", v)
	}
	// An unlisted failure still gates.
	res.Checks = append(res.Checks, Check{Name: "d", Passed: false})
	if res.Passed() {
		t.Errorf("an unlisted failure must still fail the run")
	}
}

// TestValidateRefusesAnAbsenceMarkInAnotherRecord is the rule that keeps a
// rename or an upgrade note from failing its own arc: the superseded record's
// Mark must not be inside the record that replaced it.
func TestValidateRefusesAnAbsenceMarkInAnotherRecord(t *testing.T) {
	s := VersionUpgrade()
	s.Stages[0].Records[0].Mark = "pgxpool"
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "also appears in") {
		t.Fatalf("Validate = %v, want the mark-collision refusal", err)
	}
}

// TestValidateRefusesAStageProbeOnALaterRecord: a session-start probe runs
// before its own stage's records exist, so naming one would grade nothing.
func TestValidateRefusesAStageProbeOnALaterRecord(t *testing.T) {
	s := VersionUpgrade()
	s.Stages[1].Probes = []Probe{{Name: "early", Query: "pgx", Want: "pgx-upgrade"}}
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "does not exist yet") {
		t.Fatalf("Validate = %v, want the stage-probe refusal", err)
	}
}

// TestParseSearchRowsReadsRowsNotTheVerdict pins the row grammar the probes
// read: the shared item line, in rank order, with the resolved marker, and
// nothing from the outcome line.
func TestParseSearchRowsReadsRowsNotTheVerdict(t *testing.T) {
	raw := "- [dependency] `AAA` (0.8 [resolved] tags:[\"x\"] source=mcp) «go.mod pins (v4)»\n" +
		"- [fact] `BBB` (0.7 scope{environment=staging} source=mcp) «a fact»\n\n" +
		"[ghost:outcome=answerable reason=floor_met candidates=2 admitted=2]\n"
	got := parseSearchRows(raw)
	want := []searchRow{{ID: "AAA", Resolved: true}, {ID: "BBB"}}
	if !slices.Equal(got, want) {
		t.Fatalf("parseSearchRows = %+v, want %+v", got, want)
	}
}

// TestProbeChecksGradeRankAndExclusion: the replacement must be returned and
// above the record it replaced, and an excluded record must not be returned.
func TestProbeChecksGradeRankAndExclusion(t *testing.T) {
	s := ScopedSetting()
	res := &Result{Story: s, Sessions: []Session{
		{Saved: map[string]string{"pool-staging": "S"}},
		{Saved: map[string]string{"pool-production": "P"}},
	}}
	p := Probe{Name: "x", Query: "q", Want: "pool-production", Over: []string{"pool-staging"}, Absent: []string{"pool-staging"}}

	byName := func(cs []Check) map[string]bool {
		m := map[string]bool{}
		for _, c := range cs {
			m[c.Name] = c.Passed
		}
		return m
	}
	good := byName(probeChecks(res, p, "x", "- [fact] `P` (0.7) «a»\n", true))
	if !good["search-finds:x"] || !good["search-ranks:x:pool-staging"] || !good["search-excludes:x:pool-staging"] {
		t.Errorf("a clean answer graded %v", good)
	}
	bad := byName(probeChecks(res, p, "x", "- [fact] `S` (0.7) «b»\n- [fact] `P` (0.7) «a»\n", true))
	if !bad["search-finds:x"] || bad["search-ranks:x:pool-staging"] || bad["search-excludes:x:pool-staging"] {
		t.Errorf("an answer ranking the replaced record first graded %v", bad)
	}
	missing := byName(probeChecks(res, p, "x", "- [fact] `S` (0.7) «b»\n", true))
	if missing["search-finds:x"] || missing["search-ranks:x:pool-staging"] {
		t.Errorf("an answer without the replacement graded %v", missing)
	}
}

// TestDistinctSaveReadsTheDuplicateLink: the save-path check fails on an active
// 'duplicate' link between a replacement and the claim it replaces, in either
// direction, and passes when the link is invalidated or absent.
func TestDistinctSaveReadsTheDuplicateLink(t *testing.T) {
	s := VersionUpgrade()
	pair := reversals(s)[0]
	res := &Result{Story: s, Sessions: []Session{
		{Saved: map[string]string{"pgx-pin": "OLD"}},
		{Saved: map[string]string{"pgx-upgrade": "NEW"}},
	}}
	res.State.Links = []Link{{Source: "NEW", Target: "OLD", Relation: "duplicate"}}
	if distinctSave(res, pair).Passed {
		t.Errorf("an active duplicate link passed")
	}
	res.State.Links = []Link{{Source: "OLD", Target: "NEW", Relation: "duplicate"}}
	if distinctSave(res, pair).Passed {
		t.Errorf("a reversed duplicate link passed")
	}
	res.State.Links = []Link{{Source: "NEW", Target: "OLD", Relation: "duplicate", Invalidated: true}, {Source: "NEW", Target: "OLD", Relation: "supersedes"}}
	if !distinctSave(res, pair).Passed {
		t.Errorf("an invalidated duplicate link failed")
	}
}

// TestLureRefusedFailsOnAnEdgeOrAResolution: both values of a scoped setting
// still hold, so an edge between them or a resolution of the older one fails.
func TestLureRefusedFailsOnAnEdgeOrAResolution(t *testing.T) {
	s := ScopedSetting()
	older, newer := mustRecord(s, "pool-staging"), mustRecord(s, "pool-production")
	res := &Result{Story: s, Sessions: []Session{
		{Saved: map[string]string{"pool-staging": "S"}},
		{Saved: map[string]string{"pool-production": "P"}},
	}, State: State{Stamps: map[string]Stamp{"S": {}, "P": {}}}}
	if !lureRefused(res, older, newer).Passed {
		t.Errorf("an unlinked pair failed")
	}
	res.State.Links = []Link{{Source: "P", Target: "S", Relation: "supersedes"}}
	if lureRefused(res, older, newer).Passed {
		t.Errorf("a supersedes edge between the two values passed")
	}
	res.State.Links = nil
	res.State.Stamps["S"] = Stamp{ResolvedAt: "2026-01-01 00:00:00"}
	if lureRefused(res, older, newer).Passed {
		t.Errorf("a resolved older value passed")
	}
}

// TestRunLifecycleRunsBetweenSessions is the production ordering: a stage marked
// Lifecycle has its records restamped, drained, superseded and resolved before
// the NEXT block is rendered, and the last stage's lifecycle is the arc's own.
func TestRunLifecycleRunsBetweenSessions(t *testing.T) {
	g := &fakeGhost{}
	runFixtureStory(t, RenamedEndpoint(), g, &fakeAgent{})
	var seq []string
	for _, c := range g.calls {
		switch {
		case strings.HasPrefix(c, "context "):
			seq = append(seq, "context")
		case c == "drain", c == "settle", c == "restamp":
			seq = append(seq, c)
		case strings.HasPrefix(c, "supersede "):
			seq = append(seq, "supersede")
		case strings.HasPrefix(c, "resolve "):
			seq = append(seq, "resolve")
		}
	}
	want := []string{
		"context", "context", // sessions 1 and 2
		"restamp", "drain", "supersede", "resolve", // stage 2's lifecycle
		"context",                                   // session 3
		"restamp", "settle", "supersede", "resolve", // the arc's own
		"context", // the final block
	}
	if !slices.Equal(seq, want) {
		t.Fatalf("call order\n got %v\nwant %v", seq, want)
	}
}

// TestRunPassesTheSessionScopeToTheBlock: a stage's SessionScope reaches the
// block render, and the final block gets the storyline's FinalScope.
func TestRunPassesTheSessionScopeToTheBlock(t *testing.T) {
	g := &scopeRecordingGhost{}
	var scopes []string
	r := &Run{Story: ScopedSetting(), WorkDir: "/w", Ghost: g, Agent: &fakeAgent{}, Out: testWriter{t}}
	if _, err := r.Execute(context.Background()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, sc := range g.scopes {
		scopes = append(scopes, sessionScopeEnv(sc))
	}
	want := []string{"environment=staging", "environment=production", "environment=production", "environment=staging", "environment=production"}
	if !slices.Equal(scopes, want) {
		t.Fatalf("block scopes = %v, want %v", scopes, want)
	}
}

type scopeRecordingGhost struct {
	fakeGhost
	scopes []map[string]string
}

func (g *scopeRecordingGhost) Context(ctx context.Context, workDir string, scope map[string]string) (string, error) {
	g.scopes = append(g.scopes, scope)
	return g.fakeGhost.Context(ctx, workDir, scope)
}

// TestOraclePairsComeFromTheAnnotations: the fake classifier's answer key is
// the storyline's own reversals and lures, and nothing else.
func TestOraclePairsComeFromTheAnnotations(t *testing.T) {
	got := oraclePairs(RegressedFix())
	if len(got) != 2 || got[0]["older_mark"] != "the warehouse misses that night's load" || got[1]["newer_mark"] != "commit 9be71d0" {
		t.Fatalf("oraclePairs(regressed-fix) = %v", got)
	}
	if got := oraclePairs(ScopedSetting()); len(got) != 1 || got[0]["older_mark"] != "max_conns=24" {
		t.Fatalf("oraclePairs(scoped-setting) = %v, want the lure", got)
	}
}

// TestPrependPathReplacesThePathEntry: one PATH, the fake directory first.
func TestPrependPathReplacesThePathEntry(t *testing.T) {
	got := prependPath([]string{"A=1", "PATH=/usr/bin:/bin"}, "/fake")
	if !slices.Equal(got, []string{"A=1", "PATH=/fake:/usr/bin:/bin"}) {
		t.Fatalf("prependPath = %v", got)
	}
}
