// Command evalstoryline runs one multi-session storyline against a
// scratch-isolated Ghost and grades whether the arc held together (issue #339,
// design section 2 of docs/superpowers/specs/2026-07-27-ghost-eval-suite-design.md).
//
// A storyline is a chain of headless harness sessions that share ONE scratch data
// dir. Session N+1 is told its own stage script and nothing else: whatever it
// knows about session N arrived through Ghost's own session-start injection
// (the `ghost context` block the opencode plugin hands a session), because that
// is the property the module measures. Its records are seeded AFTER its session
// has answered, so a stage cannot be handed its own answer.
//
// Usage:
//
//	go run ./eval/storyline [-storyline reversed-decision|a,b|all] [-repo .] [-keep]
//	    [-without-ghost] [-runs 1] [-judge] [-model opencode/big-pickle]
//	    [-ollama http://localhost:11434]
//	    [-drain-timeout 3m] [-auth-file path/to/auth.json]
//	    [-results-dir eval/storyline/results] [-fake-harness]
//
// LOCAL-ONLY. There is no CI wiring (issue #338 is not done), because every
// session is a real model call and the stages are nondeterministic. The
// isolation, the harness and the credential handling are eval/cycle's
// mechanisms, not new ones; see docs/benchmarks.md.
package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Record is one memory a stage's agent wrote. Everything about it that is
// harness-only (Key, Mark, SupersededBy) never reaches a model: Key is the
// grading identity, Mark is the substring the grader looks for in an injected
// block, and SupersededBy is the same annotation eval/cycle's corpus uses, so
// both suites read the same way.
type Record struct {
	Key      string
	Category string
	Content  string
	Tags     []string
	// Mark is a distinctive substring of Content, quoted verbatim in the block.
	// It is required rather than derived because a grader that matched on a
	// prefix of the content would grade the first line of a memory.
	Mark string
	// SupersededBy names the Key of the LATER record that replaces this one. A
	// reversal is the whole point of the shipped storyline, and an edge that
	// pointed the wrong way would pass a direction-blind grade.
	SupersededBy string
	// Global saves the record through ghost_save_global, into the cross-project
	// bucket every project's session start reads. It is how a fact learned in one
	// repository reaches a session in another: the sessions run with no tools, so
	// ghost_search_all is not reachable and the global bucket in the injection is
	// the only cross-project path they have. A global record needs a project
	// record beside it (Validate) because the project is what binds the work dir
	// and what the embedding drain waits on.
	Global bool
	// ValidUntil is the record's valid_until, an RFC 3339 stamp or a date, passed
	// to the save tool as written. A record whose window has already closed is one
	// the session start must withhold, and the grade checks that it did and that
	// no answer used it.
	ValidUntil string
	// Scope is the record's scope, passed to the save tool as written: where the
	// claim applies, e.g. {"environment": "staging"}. A scoped record is the one
	// a session working somewhere else must not be handed as applying to it.
	Scope map[string]string
	// Pin saves the record pinned, guaranteeing it a slot on every passive
	// surface ahead of ranking. A pin that outnumbers the bucket is the
	// efficiency defect the pins-over-bucket arc measures.
	Pin bool
}

// Probe is one search a reader of the project would run once the arc is over,
// graded on the rows ghost_memory_search returns rather than on its verdict
// line. Want is the record the search must lead to, Over the records that may
// be returned but must rank BELOW Want (the claim Want replaced), and Absent the
// records the search must not return at all (a record scoped elsewhere when the
// probe carries a scope). Marked is the records whose row, when the search
// returns it together with Want, must carry the replaced marker at its field
// (the lexical guess's or the recorded relation's): the claim Want replaced,
// read by an agent that searched before acting.
type Probe struct {
	Name   string
	Query  string
	Scope  map[string]string
	Want   string
	Over   []string
	Absent []string
	Marked []string
}

// Lure is a pair a fallible classifier would call a supersession although both
// claims still hold — two values of one setting in two environments. The fake
// harness's oracle answers SUPERSEDES for it, so the lure-refused check measures
// whether Ghost's own guards keep the pair away from the classifier, not whether
// a model resisted it.
type Lure struct {
	Older, Newer string
}

// AnswerCheck is one deterministic reading of a session's ANSWER. Any is a list
// of alternative spellings, matched case-insensitively as substrings for Carries
// and as whole tokens for Avoids; Name is what the report keys on.
type AnswerCheck struct {
	Name string
	Any  []string
	// Needs is for an Avoids check: when the answer mentions an Any spelling at all
	// (rejected or not) it must also carry one of these, or the check fails.
	Needs []string
}

// Stage is one simulated session: the script the session is given, the records
// its agent wrote once it had answered, and the keys whose Mark that session
// must have been INJECTED with (see Storyline.Validate on why they must come
// from an earlier stage).
type Stage struct {
	Script  string
	Records []Record
	// Expect is the DELIVERY property: the keys whose Mark the session's block
	// must carry. It says nothing about what the agent did with the block.
	Expect []string
	// Carries is the ANSWER property, and the primary grade: each check passes
	// when the agent's answer contains one of its spellings. Every spelling must
	// come from an earlier record and must not appear in this stage's own script
	// (Validate), which is what makes a pass mean the answer was remembered
	// rather than read out of the prompt.
	Carries []AnswerCheck
	// Avoids lists claims the answer must not USE: a superseded or expired fact,
	// or the mistake a correction was about. A spelling counts as used when it
	// appears in a sentence with nothing marking it as old or wrong (see
	// answerAvoids), so "use X, not Y" is not a use of Y.
	Avoids []AnswerCheck
	// SessionScope is the scope this session works in: the block is rendered
	// with GHOST_INJECTION_SESSION_SCOPE set to it, which is how a host tells
	// Ghost's session start that it is, say, a production session.
	SessionScope map[string]string
	// Withheld lists records whose Mark this session's block must NOT carry, for
	// a reason other than a supersession — a record scoped to an environment the
	// session is not working in.
	Withheld []string
	// Lifecycle runs the arc stages (restamp, embedding drain, supersede,
	// resolve) after this stage's records are saved and before the next block
	// is rendered — the order a host's Stop hook gives a project whose lifecycle
	// is enabled. Without it the arc stages run once, after the last session, so
	// a session after a truth change is graded on a store no lifecycle has read.
	Lifecycle bool
	// Probes are searches run at this session's start, after its block is
	// rendered and before its records exist: what the session would get if it
	// searched before doing anything else. Every key must be an earlier stage's.
	Probes []Probe
}

// Storyline is a whole arc. Project is the Ghost project the run works in;
// Opening is what the project already held before the first session, seeded
// first so the block has real competition in it.
type Storyline struct {
	Key     string
	Title   string
	Project string
	Opening []Record
	Stages  []Stage
	// Judge is the yes/no question the opt-in judge asks about the final session's
	// answer, as a format string taking the expected record's mark and content.
	// Empty means defaultJudgeQuestion.
	Judge string
	// FinalScope is the scope the final block is rendered under, as
	// Stage.SessionScope is for a session's block.
	FinalScope map[string]string
	// Probes are the searches graded against the end state.
	Probes []Probe
	// Lures are the pairs the fake classifier is told to get wrong.
	Lures []Lure
	// KnownFailing names the checks this storyline is known to fail on main,
	// each with the one-line reason a report prints beside it. A listed check
	// that fails is reported as known and does not decide the run; a listed check
	// that passes is reported as an unexpected pass, so a fix shows up as a line
	// to delete rather than as silence.
	KnownFailing map[string]string
}

// validCategories mirrors memory's own CHECK constraint, so a typo'd category
// fails at the flag boundary rather than as a rejected INSERT in the middle of a
// run that has already spent model calls.
var validCategories = map[string]bool{
	"architecture": true, "decision": true, "pattern": true, "convention": true,
	"gotcha": true, "dependency": true, "preference": true, "fact": true,
}

// stageOf is the index of the stage a record belongs to; an opening record
// answers -1, which is earlier than every session.
func (s Storyline) stageOf(key string) (int, bool) {
	for _, r := range s.Opening {
		if r.Key == key {
			return -1, true
		}
	}
	for i, st := range s.Stages {
		for _, r := range st.Records {
			if r.Key == key {
				return i, true
			}
		}
	}
	return 0, false
}

// Order is every record in the order a real project accumulated them: the
// opening notes, then each stage's. The chronology restamp walks it, and it is
// what makes a supersedes direction a fact about the storyline rather than about
// how fast the saves happened to land.
func (s Storyline) Order() []Record {
	out := make([]Record, 0, len(s.Opening))
	out = append(out, s.Opening...)
	for _, st := range s.Stages {
		out = append(out, st.Records...)
	}
	return out
}

// RecordByKey is the grading lookup: a key is the identity the annotations and
// the checks both speak.
func (s Storyline) RecordByKey(key string) (Record, bool) {
	for _, r := range s.Order() {
		if r.Key == key {
			return r, true
		}
	}
	return Record{}, false
}

// Validate rejects a storyline whose grade could not mean what it says, before
// any model call: a missing mark grades nothing, a duplicated key grades one
// record twice, and an expectation a session could only satisfy by reading its
// own stage's writes would turn the carry-forward check into a tautology.
func (s Storyline) Validate() error {
	if strings.TrimSpace(s.Key) == "" {
		return fmt.Errorf("storyline key is required")
	}
	if strings.TrimSpace(s.Project) == "" {
		return fmt.Errorf("storyline %s: project is required", s.Key)
	}
	if len(s.Stages) < 2 {
		return fmt.Errorf("storyline %s: %d stages, and a storyline needs 2 stages or more to have anything to carry forward", s.Key, len(s.Stages))
	}
	seen := map[string]bool{}
	for i, r := range s.Order() {
		// The position is in the message because a duplicate or an unmarked record
		// in a six-record storyline is otherwise found by counting lines.
		if err := validateRecord(r, seen); err != nil {
			return fmt.Errorf("storyline %s: record %d: %w", s.Key, i+1, err)
		}
	}
	for i, st := range s.Stages {
		if strings.TrimSpace(st.Script) == "" {
			return fmt.Errorf("storyline %s: stage %d has no script", s.Key, i+1)
		}
		if err := s.validateAnswerChecks(i, st); err != nil {
			return err
		}
		for _, want := range st.Expect {
			at, ok := s.stageOf(want)
			if !ok {
				return fmt.Errorf("storyline %s: stage %d expects no record %q", s.Key, i+1, want)
			}
			if at >= i {
				return fmt.Errorf("storyline %s: stage %d expects %q, which is its own or a later stage's record; a session is injected before it records anything, so only an earlier stage's record can have carried forward", s.Key, i+1, want)
			}
		}
	}
	if err := s.validateScope(); err != nil {
		return err
	}
	if err := s.validateTruthChecks(); err != nil {
		return err
	}
	for _, r := range s.Order() {
		if r.SupersededBy == "" {
			continue
		}
		at, ok := s.stageOf(r.SupersededBy)
		if !ok {
			return fmt.Errorf("storyline %s: record %s names superseded_by %q, which is not a record in this storyline", s.Key, r.Key, r.SupersededBy)
		}
		here, _ := s.stageOf(r.Key)
		if at <= here {
			return fmt.Errorf("storyline %s: record %s names superseded_by %q, which is not from a later stage; a supersedes edge only points newer to older, so this would invert the arc", s.Key, r.Key, r.SupersededBy)
		}
	}
	return nil
}

// validateScope refuses the shapes the project-scoped machinery cannot grade.
// A project record must exist (the bind and the embedding drain read the
// project), and a global record cannot take part in a reversal because the
// supersede stage and the graded state both read the project's rows only.
func (s Storyline) validateScope() error {
	project := false
	for _, r := range s.Order() {
		if !r.Global {
			project = true
		}
		if r.ValidUntil != "" {
			if _, err := parseValidUntil(r.ValidUntil); err != nil {
				return fmt.Errorf("storyline %s: record %s: %w", s.Key, r.Key, err)
			}
		}
		if !r.Global {
			continue
		}
		if r.SupersededBy != "" {
			return fmt.Errorf("storyline %s: global record %s cannot be superseded: the arc stages read the project's rows only", s.Key, r.Key)
		}
		for _, o := range s.Order() {
			if o.SupersededBy == r.Key {
				return fmt.Errorf("storyline %s: global record %s cannot supersede %s: the arc stages read the project's rows only", s.Key, r.Key, o.Key)
			}
		}
	}
	if !project {
		return fmt.Errorf("storyline %s: every record is global, and the run needs a project record to bind and to drain", s.Key)
	}
	return nil
}

// parseValidUntil reads a valid_until the way the save tool does: an RFC 3339
// stamp or a whole date. A bare date is the END of that day in the store, so it
// is returned as the last second of it.
func parseValidUntil(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.Add(24*time.Hour - time.Second), nil
	}
	return time.Time{}, fmt.Errorf("valid_until %q is neither RFC 3339 nor a date", v)
}

// validateAnswerChecks is the anti-leak rule for the answer grade. A Carries
// spelling that the stage's own script contains would be graded as remembered
// when the prompt handed it over, and one that no earlier record contains could
// only be matched by luck. Avoids spellings are not leak-checked: a script may
// name the thing it asks the agent to avoid.
func (s Storyline) validateAnswerChecks(i int, st Stage) error {
	earlier := strings.Builder{}
	for _, r := range s.Opening {
		earlier.WriteString(strings.ToLower(r.Content) + "\n")
	}
	for j := 0; j < i; j++ {
		for _, r := range s.Stages[j].Records {
			earlier.WriteString(strings.ToLower(r.Content) + "\n")
		}
	}
	script := strings.ToLower(st.Script)
	for _, c := range st.Carries {
		if strings.TrimSpace(c.Name) == "" || len(c.Any) == 0 {
			return fmt.Errorf("storyline %s: stage %d has a carries check with no name or no spelling", s.Key, i+1)
		}
		for _, a := range c.Any {
			a = strings.ToLower(strings.TrimSpace(a))
			if a == "" {
				return fmt.Errorf("storyline %s: stage %d carries %q has an empty spelling", s.Key, i+1, c.Name)
			}
			if strings.Contains(script, a) {
				return fmt.Errorf("storyline %s: stage %d carries %q, but its own script contains %q; the answer could be read out of the prompt", s.Key, i+1, c.Name, a)
			}
			if !strings.Contains(earlier.String(), a) {
				return fmt.Errorf("storyline %s: stage %d carries %q, but no earlier record contains %q; nothing could have carried it forward", s.Key, i+1, c.Name, a)
			}
		}
	}
	for _, c := range st.Avoids {
		if strings.TrimSpace(c.Name) == "" || len(c.Any) == 0 {
			return fmt.Errorf("storyline %s: stage %d has an avoids check with no name or no spelling", s.Key, i+1)
		}
		for _, a := range c.Any {
			if strings.TrimSpace(a) == "" {
				return fmt.Errorf("storyline %s: stage %d avoids %q has an empty spelling", s.Key, i+1, c.Name)
			}
		}
	}
	return nil
}

// validateTruthChecks refuses the shapes of the truth-change checks that would
// grade the storyline's wording instead of Ghost:
//
//   - A record whose Mark is checked for ABSENCE (a superseded record, a
//     withheld one) must not have that Mark inside any other record's content.
//     A rename or an upgrade note naturally restates the old name or pin, and a
//     stale-original check matching it would fail on the current record.
//   - Every key a probe, a lure or a Withheld list names must be a record here.
//   - A known-failing entry must say why, so a report line is never bare.
func (s Storyline) validateTruthChecks() error {
	absent := map[string]bool{}
	for _, r := range s.Order() {
		if r.SupersededBy != "" {
			absent[r.Key] = true
		}
	}
	for i, st := range s.Stages {
		for _, k := range st.Withheld {
			if _, ok := s.RecordByKey(k); !ok {
				return fmt.Errorf("storyline %s: stage %d withholds no record %q", s.Key, i+1, k)
			}
			at, _ := s.stageOf(k)
			if at >= i {
				return fmt.Errorf("storyline %s: stage %d withholds %q, which does not exist yet when its block is rendered", s.Key, i+1, k)
			}
			absent[k] = true
		}
		if err := validScope(st.SessionScope); err != nil {
			return fmt.Errorf("storyline %s: stage %d session scope: %w", s.Key, i+1, err)
		}
	}
	for _, p := range s.allProbes() {
		for _, k := range p.Absent {
			absent[k] = true
		}
	}
	for key := range absent {
		r, _ := s.RecordByKey(key)
		for _, o := range s.Order() {
			if o.Key != key && strings.Contains(o.Content, r.Mark) {
				return fmt.Errorf("storyline %s: %s's mark %q also appears in %s, so a check for its absence would match the other record", s.Key, key, r.Mark, o.Key)
			}
		}
	}
	for _, r := range s.Order() {
		if err := validScope(r.Scope); err != nil {
			return fmt.Errorf("storyline %s: record %s scope: %w", s.Key, r.Key, err)
		}
	}
	if err := validScope(s.FinalScope); err != nil {
		return fmt.Errorf("storyline %s: final scope: %w", s.Key, err)
	}
	for i, st := range s.Stages {
		for _, p := range st.Probes {
			for _, k := range append(append(append([]string{p.Want}, p.Over...), p.Absent...), p.Marked...) {
				if at, ok := s.stageOf(k); ok && at >= i {
					return fmt.Errorf("storyline %s: stage %d probe %s names %q, which does not exist yet when the probe runs", s.Key, i+1, p.Name, k)
				}
			}
		}
	}
	names := map[string]bool{}
	for _, p := range s.allProbes() {
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.Query) == "" {
			return fmt.Errorf("storyline %s: a probe needs a name and a query", s.Key)
		}
		if names[p.Name] {
			return fmt.Errorf("storyline %s: probe name %q is a duplicate", s.Key, p.Name)
		}
		names[p.Name] = true
		if err := validScope(p.Scope); err != nil {
			return fmt.Errorf("storyline %s: probe %s scope: %w", s.Key, p.Name, err)
		}
		keys := append(append(append([]string{p.Want}, p.Over...), p.Absent...), p.Marked...)
		for _, k := range keys {
			if _, ok := s.RecordByKey(k); !ok {
				return fmt.Errorf("storyline %s: probe %s names no record %q", s.Key, p.Name, k)
			}
		}
	}
	for _, l := range s.Lures {
		older, ok1 := s.stageOf(l.Older)
		newer, ok2 := s.stageOf(l.Newer)
		if !ok1 || !ok2 {
			return fmt.Errorf("storyline %s: lure %s/%s names a record that is not here", s.Key, l.Newer, l.Older)
		}
		if newer <= older {
			return fmt.Errorf("storyline %s: lure %s must be from a later stage than %s", s.Key, l.Newer, l.Older)
		}
	}
	for name, why := range s.KnownFailing {
		if strings.TrimSpace(why) == "" {
			return fmt.Errorf("storyline %s: known-failing %s has no reason", s.Key, name)
		}
	}
	return nil
}

// allProbes is every probe in the storyline, the stages' and the end state's,
// with each stage probe's name qualified by its session so the names are
// unique across the storyline.
func (s Storyline) allProbes() []Probe {
	var out []Probe
	for i, st := range s.Stages {
		for _, p := range st.Probes {
			p.Name = sessionName(i) + ":" + p.Name
			out = append(out, p)
		}
	}
	return append(out, s.Probes...)
}

// validScope refuses an empty key or value, which the save tool and the
// session-scope setting both refuse too; refusing it here keeps the refusal at
// the flag boundary instead of in the middle of a run.
func validScope(scope map[string]string) error {
	for k, v := range scope {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("empty key or value in %v", scope)
		}
	}
	return nil
}

func validateRecord(r Record, seen map[string]bool) error {
	if r.Key == "" || r.Content == "" {
		return fmt.Errorf("key and content are required")
	}
	if seen[r.Key] {
		return fmt.Errorf("record key %q is a duplicate", r.Key)
	}
	seen[r.Key] = true
	if !validCategories[r.Category] {
		return fmt.Errorf("%s: invalid category %q", r.Key, r.Category)
	}
	if r.Mark == "" || !strings.Contains(r.Content, r.Mark) {
		return fmt.Errorf("%s: mark %q must appear verbatim in the content it grades", r.Key, r.Mark)
	}
	return nil
}

// storylines is the registry of shipped storylines: the reversed decision, and
// the three arcs the usefulness measurement needs (a correction that must not
// be repeated, a fact learned in one repository and needed in another, and a
// fact whose validity window has closed), and the four truth-change arcs in
// truth.go (a rename, a regressed fix, a version upgrade and a scoped setting),
// and the six default-install arcs in default_install.go (a retired dependency,
// a reverted change, a plain-word change, a replacement in another category,
// repeated tuning of one setting and a superseded convention), and the three
// efficiency arcs in efficiency_arcs.go (pins over the bucket, a duplicate never
// merged, a dated global status) for #1831, #1834 and #1832.
// The service-migration, on-call and
// config-clutter candidates from the design are follow-ups, and each is a
// Storyline literal here rather than a new mechanism.
func storylines() []Storyline {
	return []Storyline{
		ReversedDecision(), CorrectionReplay(), OpsFact(), StaleFact(),
		RenamedEndpoint(), RegressedFix(), VersionUpgrade(), ScopedSetting(),
		RetiredDependency(), RevertedChange(), ProseOnlyChange(),
		RecategorisedReplacement(), RepeatedTuning(), SupersededConvention(),
		PinsOverBucket(), DuplicateNeverMerged(), DatedGlobalStatus(),
	}
}

// StorylineByKey resolves a -storyline value, naming the ones it does not have
// rather than falling back to a default nobody asked for.
func StorylineByKey(key string) (Storyline, error) {
	for _, s := range storylines() {
		if s.Key == key {
			return s, nil
		}
	}
	var keys []string
	for _, s := range storylines() {
		keys = append(keys, s.Key)
	}
	sort.Strings(keys)
	return Storyline{}, fmt.Errorf("unknown storyline %q (have: %s)", key, strings.Join(keys, ", "))
}

// ReversedDecision is candidate (d) from the design: a long-lived project whose
// early architectural choice is later contradicted. Session 1 records the
// choice, session 2 records its reversal, and session 3 has to act on the
// reversal from a cold start — which is the only place the module's question
// ("does injection carry forward what matters") can be answered about something
// that actually matters.
func ReversedDecision() Storyline {
	return Storyline{
		Key:     "reversed-decision",
		Title:   "Long-lived project with a reversed early decision",
		Project: "northwind-api",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "northwind-api is the checkout service for the Northwind order pipeline; four engineers work on it.",
				Tags:     []string{"northwind-api"},
				Mark:     "Northwind order pipeline",
			},
			{
				Key:      "deploy-target",
				Category: "architecture",
				Content:  "northwind-api deploys to Fly.io, configured in fly.toml; the app listens on port 8080.",
				Tags:     []string{"northwind-api", "deploy"},
				Mark:     "deploys to Fly.io",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on northwind-api. You are settling where per-session state lives.\n\n" +
					"Answer in at most three sentences, using only what this session's context already holds:\n" +
					"- which store per-session state goes in, and\n" +
					"- what breaks if the request path blocks on it.",
				Records: []Record{
					{
						Key:      "session-store-redis",
						Category: "decision",
						Content:  "Per-session state lives in Redis: the bootstrap sets SESSION_STORE=redis and every session key carries a 3600s TTL.",
						Tags:     []string{"northwind-api", "session"},
						Mark:     "SESSION_STORE=redis",
						// The arc: this claim is what session 3 must NOT be told is
						// current, and the edge is what supersede has to find.
						SupersededBy: "session-store-postgres",
					},
					{
						Key:      "redis-single-thread",
						Category: "gotcha",
						Content:  "The Redis client is single-threaded: a blocking BRPOP in the request path stalls every worker behind it.",
						Tags:     []string{"northwind-api", "session"},
						Mark:     "single-threaded",
					},
				},
			},
			{
				Script: "Second session on northwind-api. The month-end bill landed and the session store is the line item nobody can explain. " +
					"You have just been handed the decision the team took about it.\n\n" +
					"Answer in at most three sentences, using only what this session's context already holds:\n" +
					"- which store per-session state goes in now, and\n" +
					"- what changed about the previous choice.",
				Records: []Record{
					{
						Key:      "session-store-postgres",
						Category: "decision",
						Content:  "Per-session state lives in Postgres, not Redis: the bootstrap sets SESSION_STORE=postgres and rows in the public.sessions table are the sessions.",
						Tags:     []string{"northwind-api", "session"},
						Mark:     "SESSION_STORE=postgres",
					},
					{
						Key:      "redis-cost",
						Category: "fact",
						Content:  "The Redis bill for April was $1,840, of which 90% was idle replica memory for keys that were never read.",
						Tags:     []string{"northwind-api"},
						Mark:     "$1,840",
					},
				},
				// Session 2 knows the original only if session 1's record was
				// carried forward into its block.
				Expect: []string{"session-store-redis"},
			},
			{
				Script: "Third session on northwind-api. You are writing the service bootstrap and the reviewer wants a short summary to go with it.\n\n" +
					"Answer in at most three sentences, using only what this session's context already holds:\n" +
					"- which store per-session state goes in,\n" +
					"- the exact environment variable the bootstrap must set, and\n" +
					"- what a reader still working from the old choice would get wrong.",
				// The cold-start session the whole storyline exists for: it has to
				// find the reversal in its block, and it has to find it unmarked
				// as the current answer.
				Expect: []string{"session-store-postgres"},
				// The answer grade. Nothing in this stage's script names a store or a
				// variable value; both come from the block or from nowhere.
				Carries: []AnswerCheck{{Name: "current-store", Any: []string{"SESSION_STORE=postgres"}}},
				Avoids:  []AnswerCheck{{Name: "superseded-store", Any: []string{"SESSION_STORE=redis"}}},
			},
		},
		// This arc runs its lifecycle after the last session only, which is the
		// default install's shape (automatic lifecycle phases are off), so
		// session 3 is graded on what the save path and the read side alone left
		// behind: the reversal first, the original marked as maybe replaced.
	}
}
