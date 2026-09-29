package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fakeGhost is the Ghost a unit test runs against. It records every call in
// order, hands out ids in seed order, renders a block from what exists so far, and
// can carry one supersedes edge and one resolved row into the end state. No binary,
// no store, no model.
type fakeGhost struct {
	saved []savedRecord
	calls []string
	links []Link
	// edge names a supersedes edge by RECORD KEY and is resolved to ids when the
	// end state is read. A test cannot write the ids itself — they do not exist
	// until the run's saves have happened — so an edge expressed as ids would have
	// to be assembled after the fact, which is the very wiring
	// TestRunGradedStateComesFromTheStore exists to check.
	edge     *fakeEdge
	resolved map[string]string // id -> resolved_at
	restamp  []string
	bindErr  error
	saveErr  error
	ctxErr   error
	supErr   error
	setErr   error
	supOut   string
	resOut   string
}

type savedRecord struct {
	key   string
	rec   Record
	order int
}

// fakeEdge is one supersedes edge named by record key, resolved to ids by the
// fake's State read — see fakeGhost.edge.
type fakeEdge struct {
	source, target string
	relation       string
	invalidated    bool
}

func (f *fakeGhost) Bind(_ context.Context, project, workDir string) error {
	f.calls = append(f.calls, fmt.Sprintf("bind %s %s", project, workDir))
	return f.bindErr
}

func (f *fakeGhost) Save(_ context.Context, project string, rec Record) (string, error) {
	f.calls = append(f.calls, "save "+rec.Key)
	if f.saveErr != nil {
		return "", f.saveErr
	}
	id := fmt.Sprintf("ID%04d", len(f.saved)+1)
	f.saved = append(f.saved, savedRecord{key: rec.Key, rec: rec, order: len(f.saved) + 1})
	return id, nil
}

// Context renders the marks of whatever has been saved SO FAR — the one property
// the real block has that the carry-forward grade reads, and the reason a stage
// that seeds its own records too early is observable here rather than only in a
// real run.
//
// It renders EVERY saved record, including one a later record supersedes, because
// that is what the store's own render does today: the block carries no superseded
// marker. A fake that dropped the stale claim would pass stale-original by
// construction, which is the finding this module exists to report.
func (f *fakeGhost) Context(_ context.Context, workDir string) (string, error) {
	f.calls = append(f.calls, "context "+workDir)
	if f.ctxErr != nil {
		return "", f.ctxErr
	}
	var b strings.Builder
	b.WriteString("## Ghost context: acme\n")
	for _, s := range f.saved {
		fmt.Fprintf(&b, "- [%s] «%s»\n", s.rec.Category, s.rec.Content)
	}
	return b.String(), nil
}

func (f *fakeGhost) Settle(_ context.Context) error {
	f.calls = append(f.calls, "settle")
	return f.setErr
}

func (f *fakeGhost) Supersede(_ context.Context, project string) (string, error) {
	f.calls = append(f.calls, "supersede "+project)
	return f.supOut, f.supErr
}

func (f *fakeGhost) Resolve(_ context.Context, project string) (string, error) {
	f.calls = append(f.calls, "resolve "+project)
	return f.resOut, nil
}

func (f *fakeGhost) Restamp(_ context.Context, order []string) error {
	f.calls = append(f.calls, "restamp")
	f.restamp = append(f.restamp, order...)
	return nil
}

func (f *fakeGhost) State(_ context.Context) (State, error) {
	f.calls = append(f.calls, "state")
	st := State{Stamps: f.stamps(), Links: f.links}
	if f.edge != nil {
		st.Links = append(st.Links, Link{
			Source:      f.idOf(f.edge.source),
			Target:      f.idOf(f.edge.target),
			Relation:    f.edge.relation,
			Invalidated: f.edge.invalidated,
		})
	}
	return st, nil
}

// stamps gives every saved row a created_at that increases with its seed order —
// the chronology the runner's restamp writes, so an edge's direction is readable
// from the store rather than from the order the ids happen to have. A stamp missing
// for one side of a pair is what an unreadable chronology looks like, and the
// direction check refuses it rather than assuming.
func (f *fakeGhost) stamps() map[string]Stamp {
	out := make(map[string]Stamp, len(f.saved))
	for _, s := range f.saved {
		id := fmt.Sprintf("ID%04d", s.order)
		out[id] = Stamp{CreatedAt: fmt.Sprintf("2026-01-01 00:%02d:00", s.order)}
		if at, ok := f.resolved[id]; ok {
			out[id] = Stamp{CreatedAt: out[id].CreatedAt, ResolvedAt: at}
		}
	}
	return out
}

// idOf is the fake's id for a record key, so a test can assert on graded state
// without hard-coding the fake's id scheme.
func (f *fakeGhost) idOf(key string) string {
	for i, s := range f.saved {
		if s.key == key {
			return fmt.Sprintf("ID%04d", i+1)
		}
	}
	return ""
}

func (f *fakeGhost) callsBefore(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

// fakeAgent is the harness a unit test runs against: it records the prompt of
// every session and answers from a fixed script. No model is spawned.
type fakeAgent struct {
	prompts []string
	answers []string
	err     error
}

func (a *fakeAgent) Ask(_ context.Context, prompt string) (string, error) {
	a.prompts = append(a.prompts, prompt)
	if a.err != nil {
		return "", a.err
	}
	if len(a.answers) < len(a.prompts) {
		return fmt.Sprintf("answer %d", len(a.prompts)), nil
	}
	return a.answers[len(a.prompts)-1], nil
}

func runFixture(t *testing.T, g *fakeGhost, a *fakeAgent) *Result {
	t.Helper()
	return runFixtureStory(t, ReversedDecision(), g, a)
}

// runFixtureStory is runFixture for a caller-chosen storyline, so a test can
// exercise a shape the shipped arc does not have — an opening record that is
// itself reversed, say — without the shipped storyline growing an arc nobody
// asked for.
func runFixtureStory(t *testing.T, s Storyline, g *fakeGhost, a *fakeAgent) *Result {
	t.Helper()
	r := &Run{
		Story:   s,
		WorkDir: "/scratch/work/acme",
		Ghost:   g,
		Agent:   a,
		Out:     testWriter{t},
	}
	res, err := r.Execute(context.Background())
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return res
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// TestRunSeedsEachStageAfterItsSession is the property the whole module rests on.
// A session's block is rendered BEFORE that stage's own records exist, so the
// block a session received is exactly what sessions 1..N-1 left behind. A runner
// that seeds first would hand every session its own answer and the carry-forward
// grade would pass for the wrong reason.
func TestRunSeedsEachStageAfterItsSession(t *testing.T) {
	s := ReversedDecision()
	g := &fakeGhost{}
	res := runFixture(t, g, &fakeAgent{})
	if len(res.Sessions) != len(s.Stages) {
		t.Fatalf("ran %d sessions, want %d", len(res.Sessions), len(s.Stages))
	}
	for i, sess := range res.Sessions {
		stage := s.Stages[i]
		for _, rec := range stage.Records {
			if strings.Contains(sess.Block, rec.Mark) {
				t.Errorf("session %d was injected its OWN record %q — seeded before the block was rendered", i+1, rec.Key)
			}
		}
		for j := 0; j < i; j++ {
			for _, prev := range s.Stages[j].Records {
				if !strings.Contains(sess.Block, prev.Content) {
					t.Errorf("session %d is missing session %d's record %q", i+1, j+1, prev.Key)
				}
			}
		}
	}
}

// TestRunGivesEachSessionOnlyItsOwnScript pins the other half of the isolation:
// a stage is told its own work and nothing about the other stages, so a session
// cannot answer from the storyline's script and pass as if it had recalled it.
func TestRunGivesEachSessionOnlyItsOwnScript(t *testing.T) {
	s := ReversedDecision()
	a := &fakeAgent{}
	runFixture(t, &fakeGhost{}, a)
	if len(a.prompts) != len(s.Stages) {
		t.Fatalf("ran %d sessions, want %d", len(a.prompts), len(s.Stages))
	}
	for i, p := range a.prompts {
		if !strings.Contains(p, s.Stages[i].Script) {
			t.Errorf("session %d prompt is missing its own script", i+1)
		}
		for j, other := range s.Stages {
			if j == i || other.Script == "" {
				continue
			}
			if strings.Contains(p, other.Script) {
				t.Errorf("session %d prompt carries session %d's script", i+1, j+1)
			}
		}
	}
}

// TestRunBindsTheProjectToTheWorkDirOnce: the session-start block is resolved
// from a working directory, so a project that records no location renders no
// project half at all and every session reads as a first session.
func TestRunBindsTheProjectToTheWorkDirOnce(t *testing.T) {
	// The project's name comes from the storyline rather than a literal, so the
	// assertion is about the SEQUENCE (bind once, before any block) and not about
	// a name the shipped storyline is free to change.
	story := ReversedDecision()
	g := &fakeGhost{}
	runFixture(t, g, &fakeAgent{})
	bind := fmt.Sprintf("bind %s /scratch/work/acme", story.Project)
	if binds := countCall(g.calls, bind); binds != 1 {
		t.Fatalf("bound %d times, want 1 (calls: %v)", binds, g.calls)
	}
	if idx(g.calls, bind) > idx(g.calls, "context /scratch/work/acme") {
		t.Fatalf("bind ran after the first block was rendered (calls: %v)", g.calls)
	}
}

// TestRunRestampsRecordsInStorylineOrder: created_at has second granularity, so
// two records seeded inside one second tie and the supersedes direction between
// them becomes arbitrary. The restamp is what makes the direction a fact about
// the storyline.
func TestRunRestampsRecordsInStorylineOrder(t *testing.T) {
	s := ReversedDecision()
	g := &fakeGhost{}
	runFixture(t, g, &fakeAgent{})
	want := make([]string, 0, len(s.Order()))
	for _, r := range s.Order() {
		want = append(want, g.idOf(r.Key))
	}
	if strings.Join(g.restamp, ",") != strings.Join(want, ",") {
		t.Fatalf("restamped %v, want %v", g.restamp, want)
	}
}

func idx(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

// TestArcPhasesClassifyThroughTheRunsOwnHarness: `ghost supersede` and `ghost
// resolve` classify by CALLING A CLI HARNESS, and with no --source they pick one
// by walking the process ancestry of whoever launched the runner. That is the
// wrong answer for an eval on both counts: a run launched from inside Claude Code
// would bill Claude for verdicts about an opencode-driven arc (and fail outright
// when the sandbox holds only opencode's credential), and the report's model
// attribution would name a model that never saw the sessions. The phases name
// opencode, which is the harness the run's own sessions run on.
func TestArcPhasesClassifyThroughTheRunsOwnHarness(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"supersede", arcPhaseArgs("supersede", "northwind-api")},
		{"resolve", arcPhaseArgs("resolve", "northwind-api")},
	} {
		i := slices.Index(tc.args, "--source")
		if i < 0 {
			t.Errorf("%s: no --source in %v — the phase picks a harness by process ancestry", tc.name, tc.args)
			continue
		}
		if tc.args[i+1] != "opencode" {
			t.Errorf("%s: classifies through %q, want opencode", tc.name, tc.args[i+1])
		}
		if !slices.Contains(tc.args, "--apply") {
			t.Errorf("%s: %v does not apply, so the run would grade a dry run", tc.name, tc.args)
		}
		if !slices.Contains(tc.args, "northwind-api") {
			t.Errorf("%s: %v does not name the project", tc.name, tc.args)
		}
	}
}

func countCall(calls []string, want string) int {
	n := 0
	for _, c := range calls {
		if c == want {
			n++
		}
	}
	return n
}

// TestRunOrdersTheArcAfterTheLastSession: embeddings, supersede and resolve all
// read what the sessions wrote, so running the arc before the last session has
// recorded anything would grade a half-finished arc.
func TestRunOrdersTheArcAfterTheLastSession(t *testing.T) {
	story := ReversedDecision()
	g := &fakeGhost{}
	runFixture(t, g, &fakeAgent{})
	settle, sup, res := idx(g.calls, "settle"),
		idx(g.calls, "supersede "+story.Project), idx(g.calls, "resolve "+story.Project)
	if settle < 0 || sup < 0 || res < 0 {
		t.Fatalf("missing call in %v", g.calls)
	}
	// Every save, not one chosen save: the last session's records are what the arc
	// stages read, and the shipped storyline's final stage records nothing, so a
	// test naming that stage's first record would grade a storyline that has one.
	lastSave := -1
	for _, st := range story.Stages {
		for _, rec := range st.Records {
			if i := idx(g.calls, "save "+rec.Key); i > lastSave {
				lastSave = i
			}
		}
	}
	if lastSave < 0 {
		t.Fatalf("no save happened in %v", g.calls)
	}
	if lastSave >= settle || settle >= sup || sup >= res {
		t.Fatalf("arc out of order in %v", g.calls)
	}
}

func TestRunFailsWhenASessionFails(t *testing.T) {
	g := &fakeGhost{}
	a := &fakeAgent{err: errors.New("harness exploded")}
	r := &Run{Story: ReversedDecision(), WorkDir: "/scratch/work/acme", Ghost: g, Agent: a, Out: testWriter{t}}
	if _, err := r.Execute(context.Background()); err == nil {
		t.Fatal("Execute succeeded with a failing harness")
	}
	if g.callsBefore("supersede " + ReversedDecision().Project) {
		t.Fatal("the arc ran after a session failed")
	}
}

func TestRunFailsWhenTheSupersedeStageFails(t *testing.T) {
	g := &fakeGhost{supErr: errors.New("no candidates")}
	r := &Run{Story: ReversedDecision(), WorkDir: "/scratch/work/acme", Ghost: g, Agent: &fakeAgent{}, Out: testWriter{t}}
	if _, err := r.Execute(context.Background()); err == nil {
		t.Fatal("Execute succeeded with a failing supersede stage")
	}
}

// TestJudgeRefusesAStorylineWithNoReversalToAskAbout: the judge is handed the
// final stage's expected record, and Validate does NOT require a final stage to
// expect anything — it requires each Expect to name an EARLIER stage's record,
// never its own. So a storyline whose last stage expects nothing is valid, and
// indexing into that empty list would panic inside a run that already spent
// three model sessions. The judge has nothing to ask about, so it says so.
func TestJudgeRefusesAStorylineWithNoReversalToAskAbout(t *testing.T) {
	s := ReversedDecision()
	s.Stages[len(s.Stages)-1].Expect = nil // still Validate-clean: Expect may be empty
	if err := s.Validate(); err != nil {
		t.Fatalf("a storyline with an empty final Expect must be valid: %v", err)
	}
	g := &fakeGhost{}
	r := &Run{Story: s, WorkDir: "/scratch/work/acme", Ghost: g, Agent: &fakeAgent{},
		Judge: &fakeAgent{}, Out: testWriter{t}}
	if _, err := r.Execute(context.Background()); err == nil {
		t.Fatal("Execute succeeded with a judge and no record to judge")
	}
}

// TestRunGradedStateComesFromTheStore: the arc is graded from the rows the
// stages wrote, so the ids the saves returned have to travel all the way to the
// grade. An id lost here makes every supersede check unresolvable and every
// reversal look live.
func TestRunGradedStateComesFromTheStore(t *testing.T) {
	s := ReversedDecision()
	g := &fakeGhost{}
	var original, reversal Record
	for _, r := range s.Order() {
		switch r.Key {
		case "session-store-redis":
			original = r
		case "session-store-postgres":
			reversal = r
		}
	}
	if original.Key == "" || reversal.Key == "" {
		t.Fatal("reversed-decision no longer names both sides of its reversal")
	}
	g.edge = &fakeEdge{source: reversal.Key, target: original.Key, relation: "supersedes"}
	res := runFixture(t, g, &fakeAgent{})

	// The edge is expressed by key and resolved by the FAKE, so this asserts the
	// thing it claims to: the ids the run's saves returned reached the grade. If
	// the runner dropped Session.Saved, the edge would resolve to empty ids and
	// supersede-edge would fail with "no id reached the grade" — which is a
	// different failure from "the arc produced no edge", and the two must not be
	// reported as the same finding.
	if c := checkNamed(res, "supersede-edge:"+reversal.Key); !c.Passed {
		t.Errorf("the store's edge did not resolve: %s", c.Detail)
	}
	if c := checkNamed(res, "reversal-live:"+reversal.Key); !c.Passed {
		t.Errorf("the reversal's stamp did not resolve: %s", c.Detail)
	}
	// The stale-original checks FAIL against this fake, on purpose: it renders a
	// superseded claim with no marker, exactly as the store does today. Asserting
	// that here is what makes the passing grade in grade_test.go meaningful — the
	// clean fixture there has to differ in the BLOCK, not in the wiring.
	for _, name := range []string{"stale-original:session-3", "stale-original:final-block"} {
		if c := checkNamed(res, name); c.Passed {
			t.Errorf("%s passed against a block that still quotes the superseded claim", name)
		}
	}
}

// TestRunGradesAReversalOfAnOpeningRecord: the opening notes are saved BEFORE
// the first session, and their ids used to live only in a local slice, so
// idOf could not resolve them. A storyline may supersede an OPENING record —
// Validate accepts it, because stageOf answers -1 for an opening record and the
// SupersededBy rule only demands a LATER stage — and the supersede-edge check
// names BOTH keys, so it would fail with "no id reached the grade" on a store
// that had the edge perfectly right. A runner defect must never surface as a
// check a reader takes as a finding about Ghost.
func TestRunGradesAReversalOfAnOpeningRecord(t *testing.T) {
	s := goodStory()
	s.Opening[0].SupersededBy = "reversal"
	if err := s.Validate(); err != nil {
		t.Fatalf("a reversal of an opening record must validate: %v", err)
	}
	g := &fakeGhost{}
	g.edge = &fakeEdge{source: "reversal", target: "opening", relation: "supersedes"}
	res := runFixtureStory(t, s, g, &fakeAgent{})
	if c := checkNamed(res, "supersede-edge:reversal"); !c.Passed {
		t.Errorf("the edge on an opening record did not resolve: %s", c.Detail)
	}
}

// checkNamed is grade_test.go's checkByName for a result whose checks exist by
// construction; it says which check is missing rather than returning a zero Check
// that would report as a failure of something else.
func checkNamed(res *Result, name string) Check {
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Detail: "no check with this name was graded"}
}
