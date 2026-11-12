// Command evalstoryline runs one multi-session storyline against a
// scratch-isolated Ghost and grades whether the arc held together (issue #339,
// design section 2 of docs/superpowers/specs/2026-07-27-ghost-eval-suite-design.md).
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
	Key          string
	Category     string
	Content      string
	Tags         []string
	Mark         string
	SupersededBy string
	Global       bool
	ValidUntil   string
	Scope        map[string]string
	Pin          bool
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
	Name  string
	Any   []string
	Needs []string
}

// Stage is one simulated session: the script the session is given, the records
// its agent wrote once it had answered, and the keys whose Mark that session
// must have been INJECTED with (see Storyline.Validate on why they must come
// from an earlier stage).
type Stage struct {
	Script       string
	Records      []Record
	Expect       []string
	Carries      []AnswerCheck
	Avoids       []AnswerCheck
	SessionScope map[string]string
	Withheld     []string
	Lifecycle    bool
	Probes       []Probe
}

// Storyline is a whole arc. Project is the Ghost project the run works in;
// Opening is what the project already held before the first session, seeded
// first so the block has real competition in it.
type Storyline struct {
	Key          string
	Title        string
	Project      string
	Opening      []Record
	Stages       []Stage
	Judge        string
	FinalScope   map[string]string
	Probes       []Probe
	Lures        []Lure
	KnownFailing map[string]string
}

var validCategories = map[string]bool{
	"architecture": true, "decision": true, "pattern": true, "convention": true,
	"gotcha": true, "dependency": true, "preference": true, "fact": true,
}

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

func parseValidUntil(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t.Add(24*time.Hour - time.Second), nil
	}
	return time.Time{}, fmt.Errorf("valid_until %q is neither RFC 3339 nor a date", v)
}

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
						Key:          "session-store-redis",
						Category:     "decision",
						Content:      "Per-session state lives in Redis: the bootstrap sets SESSION_STORE=redis and every session key carries a 3600s TTL.",
						Tags:         []string{"northwind-api", "session"},
						Mark:         "SESSION_STORE=redis",
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
				Expect: []string{"session-store-redis"},
			},
			{
				Script: "Third session on northwind-api. You are writing the service bootstrap and the reviewer wants a short summary to go with it.\n\n" +
					"Answer in at most three sentences, using only what this session's context already holds:\n" +
					"- which store per-session state goes in,\n" +
					"- the exact environment variable the bootstrap must set, and\n" +
					"- what a reader still working from the old choice would get wrong.",
				Expect:  []string{"session-store-postgres"},
				Carries: []AnswerCheck{{Name: "current-store", Any: []string{"SESSION_STORE=postgres"}}},
				Avoids:  []AnswerCheck{{Name: "superseded-store", Any: []string{"SESSION_STORE=redis"}}},
			},
		},
	}
}
