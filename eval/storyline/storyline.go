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
//	go run ./eval/storyline [-storyline reversed-decision] [-repo .] [-keep]
//	    [-judge] [-model opencode/big-pickle] [-ollama http://localhost:11434]
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
}

// Stage is one simulated session: the script the session is given, the records
// its agent wrote once it had answered, and the keys whose Mark that session
// must have been INJECTED with (see Storyline.Validate on why they must come
// from an earlier stage).
type Stage struct {
	Script  string
	Records []Record
	Expect  []string
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

// storylines is the registry of shipped storylines. One ships in this change
// (reversed-decision); the service-migration, on-call and config-clutter
// candidates from the design are follow-ups, and each is a Storyline literal
// here rather than a new mechanism.
func storylines() []Storyline {
	return []Storyline{ReversedDecision()}
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
			},
		},
	}
}
