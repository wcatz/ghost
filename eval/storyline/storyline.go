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
//	    [-results-dir eval/storyline/results]
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
// fact whose validity window has closed). The service-migration, on-call and
// config-clutter candidates from the design are follow-ups, and each is a
// Storyline literal here rather than a new mechanism.
func storylines() []Storyline {
	return []Storyline{ReversedDecision(), CorrectionReplay(), OpsFact(), StaleFact()}
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
	}
}
