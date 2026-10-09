package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// #674: nothing in the save path said what a memory is FOR, so agents saved
// facts the repository already holds authoritatively. These tests are the
// golden contract for the guidance that answers it — the server instructions
// and the description of each memory-writing SAVE tool — and for the advisory
// the response carries back.
//
// The surface map is the two SAVE tools plus the instructions, which is the set
// an agent reads BEFORE it chooses to store something, and the set the advisory
// fires on at the point of storing. It is NOT every tool that writes memory
// text: ghost_memory_update carries the advisory but no description guidance
// (an update names an existing memory rather than choosing a new one), and
// ghost_decision_record carries neither, because its companion memory's text is
// composed inside memory.RecordDecision and duplicating that format here would
// let the two drift. Both reach the rule through the server instructions.
//
// The guidance is prose in constants, so nothing but a test stops it from
// being reworded away; the strings below are the golden ones, copied from the
// issue, and every surface must carry all of them.

// durableGuidanceGolden is the issue's rule and its three examples, verbatim.
// Every surface states it in its own words, so the test asserts the CLAIM set
// rather than one shared paragraph: what must not be lost is the rule and the
// two-sided example, not a particular sentence.
var durableGuidanceGolden = []string{
	// the rule
	"durable knowledge",
	"survives the conversation",
	// good: a rule the repository does not state
	"Production schema changes require explicit approval.",
	// good: a deliberate choice, with the reason
	"Deployment keeps database migrations separate from application rollout",
	// bad: a fact the repository is authoritative for
	"foo.go contains HandleFoo()",
}

// saveToolDescription reads a save tool's description off the wire rather than
// out of the source, so the test pins what a client is actually handed.
func saveToolDescription(t *testing.T, session *mcp.ClientSession, tool string) string {
	t.Helper()
	tools, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tl := range tools.Tools {
		if tl.Name == tool {
			return tl.Description
		}
	}
	t.Fatalf("%s is missing from tools/list", tool)
	return ""
}

// TestDurableGuidanceStatesTheRuleOnEverySaveSurface pins the guidance an
// agent reads BEFORE it saves. The server instructions are the session-level
// contract and a tool description is what a client puts in front of the model
// at the call site; an agent that reads only one of them still has to learn
// that a memory is durable knowledge and not a restatement of the code.
func TestDurableGuidanceStatesTheRuleOnEverySaveSurface(t *testing.T) {
	_, session := newCapSession(t)

	// Both memory-writing SAVE tools are here, not just the project one: the
	// instructions send an agent to ghost_save_global for the cross-project
	// case, the advisory fires on that path too, and a description that carried
	// neither the rule nor an example would be the one surface where a save is
	// offered with no guidance attached.
	surfaces := map[string]string{
		"mcpInstructions":   mcpInstructions,
		"ghost_memory_save": saveToolDescription(t, session, "ghost_memory_save"),
		"ghost_save_global": saveToolDescription(t, session, "ghost_save_global"),
	}
	for name, text := range surfaces {
		for _, want := range durableGuidanceGolden {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing the durable-knowledge guidance %q — the rule was removed or reworded away", name, want)
			}
		}
	}
}

// TestSaveGuidanceSaysGhostOnlyGuides pins the boundary the issue draws: Ghost
// guides, it does not judge the save. Every surface has to say so, or an agent
// reads the rule as a filter it will be caught bypassing — and the
// deterministic hint below becomes a promise it must not make.
func TestSaveGuidanceSaysGhostOnlyGuides(t *testing.T) {
	_, session := newCapSession(t)

	for name, text := range map[string]string{
		"mcpInstructions":   mcpInstructions,
		"ghost_memory_save": saveToolDescription(t, session, "ghost_memory_save"),
		"ghost_save_global": saveToolDescription(t, session, "ghost_save_global"),
	} {
		if !strings.Contains(strings.ToLower(text), "never refuse") &&
			!strings.Contains(strings.ToLower(text), "never rejects") {
			t.Errorf("%s does not say the guidance never refuses a save — an agent must not read it as a filter that catches it out:\n%s", name, text)
		}
	}
}

// #959: the audit behind the issue measured 43 saves of which 32 were good —
// 4 duplicates, 2 repository restatements, 2 transient progress notes, and 4 of
// 8 global rows wrong. These tests pin the guidance that answers each, on the
// surfaces an agent reads BEFORE it saves: the server instructions and the two
// SAVE tool descriptions. Prose in constants, so nothing but a test stops it
// from being reworded away.

// saveGuidanceGolden is the claim set every save surface must carry. Each entry
// is a phrase that names one of the audit's findings; a surface that drops one
// has lost the answer to that finding.
var saveGuidanceGolden = []string{
	// one fact per memory, refined rather than re-saved (#959: 4 duplicates)
	"One memory per fact",
	"ghost_memory_update",
	// the three shapes that are not a memory (#959: 2 repository restatements,
	// 2 transient progress notes)
	"not what the repository says",
	"not the current task's progress",
	"never a key or token value",
	// a constraint has a home: convention, or dependency for a toolchain or
	// version limit (#959: two runs refused a save for using a category that
	// does not exist)
	"convention (naming/workflow, and any constraint that is not a toolchain or version limit)",
	"dependency (versions/API quirks, and toolchain and version limits)",
	// fact is the weakest category, not the default for everything
	"fact (general knowledge — the weakest category, for when nothing more specific fits)",
	// the durability knobs are a pair, not three interchangeable spellings
	// (#959: one planted invariant saved three different ways)
	"pass pin=true and retention='persistent' together",
	"omit both otherwise",
	// a stated date is a valid_until trigger (#959: one of two runs used it)
	"When the user states a date after which something changes",
}

// TestSaveGuidanceStatesTheAuditAnswersOnEverySaveSurface pins the claim set
// above on the server instructions and both SAVE tool descriptions.
func TestSaveGuidanceStatesTheAuditAnswersOnEverySaveSurface(t *testing.T) {
	_, session := newCapSession(t)

	surfaces := map[string]string{
		"mcpInstructions":   mcpInstructions,
		"ghost_memory_save": saveToolDescription(t, session, "ghost_memory_save"),
		"ghost_save_global": saveToolDescription(t, session, "ghost_save_global"),
	}
	for name, text := range surfaces {
		for _, want := range saveGuidanceGolden {
			if !strings.Contains(text, want) {
				t.Errorf("%s is missing the save guidance %q — the answer to an audit finding was removed or reworded away", name, want)
			}
		}
	}
}

// TestSaveToolExampleIsADurableRule pins the example the save description
// carries. The old one was a location fact — 'k3s-mini-1 runs Grafana on port
// 80' — which is the kind of memory the tool's own definition says not to
// save, so the one exemplar every reader saw taught the wrong shape.
func TestSaveToolExampleIsADurableRule(t *testing.T) {
	_, session := newCapSession(t)

	desc := saveToolDescription(t, session, "ghost_memory_save")
	for _, want := range []string{
		"Grafana stays on port 80 behind the ingress because the ops firewall only forwards 80/443; do not move it to 3000.",
		"category='convention'",
		"importance=0.8",
	} {
		if !strings.Contains(desc, want) {
			t.Errorf("ghost_memory_save's example lost %q — the exemplar teaches the wrong shape of memory:\n%s", want, desc)
		}
	}
	if strings.Contains(desc, "k3s-mini-1 runs Grafana on port 80") {
		t.Errorf("ghost_memory_save still carries the location-fact example its own definition says not to save:\n%s", desc)
	}
}

// TestSaveGuidanceSaysWhatNotToSave pins the do-not-save list, which is the half
// of the guidance an agent reads as a filter. #959 added two shapes the audit
// measured: a release or pull-request status, and a measurement a later run
// replaces — both durable-looking, both wrong.
func TestSaveGuidanceSaysWhatNotToSave(t *testing.T) {
	for _, want := range []string{
		"ephemeral debug state",
		"info derivable from code/git",
		"content already in the project's agent instructions (CLAUDE.md, AGENTS.md)",
		"the status of a release or an open pull request",
		"a measurement a later run replaces",
	} {
		if !strings.Contains(mcpInstructions, want) {
			t.Errorf("mcpInstructions is missing %q in its do-not-save list — the guidance was removed or reworded away", want)
		}
	}
	// The old wording named CLAUDE.md alone, which opencode, codex and goose
	// users never see: their agent instructions are AGENTS.md or nothing.
	if strings.Contains(mcpInstructions, "content in CLAUDE.md.") {
		t.Errorf("mcpInstructions still names CLAUDE.md alone in its do-not-save list")
	}
}

// TestSaveGuidanceScopesGlobalSaves pins the rule that answers the audit's
// wrong-global rows: a global save is for what the user said applies everywhere,
// a codebase rule stays in the project, and a memory about Ghost's own tools is
// never one.
func TestSaveGuidanceScopesGlobalSaves(t *testing.T) {
	for _, want := range []string{
		"Global (ghost_save_global) only when the user says it applies to every repository",
		"A rule learned in this codebase stays in the project even when it sounds general",
		"promote later with ghost_memory_promote",
		"Never save a memory about how Ghost's own tools behaved",
	} {
		if !strings.Contains(mcpInstructions, want) {
			t.Errorf("mcpInstructions is missing %q — the global-save rule was removed or reworded away", want)
		}
	}
}

// TestRepoFactHintRecognisesOnlyContainmentClaims is the shape table for the
// deterministic hint. Two conditions, both required IN THE SAME SENTENCE: the
// note must NAME something in the repository and CLAIM what is there. Firing on
// durable knowledge would train agents to ignore the advisory, so the quiet
// half is the load-bearing half, and each case below isolates WHICH condition
// is missing, so dropping either one fails here rather than on a corpus nobody
// runs.
func TestRepoFactHintRecognisesOnlyContainmentClaims(t *testing.T) {
	fires := []string{
		"foo.go contains HandleFoo()", // the issue's bad example, verbatim
		"internal/reflection/prompt.go defines the drop guard.",
		"The Restore path in backup.go is where every copy of the store is written.",
		"`Store.UpsertWithOptions` is where the fold target is decided.",
		// A note that names a location in one sentence and the RULE for it in
		// the next still fires, and this case is here so the accepted cost is
		// pinned rather than implied: the rule is per-sentence, and a later
		// sentence cannot retract the first one's claim.
		"HandleFoo() lives in internal/foo/bar.go. The rule it enforces is that deploys need sign-off.",
	}
	for _, content := range fires {
		if repoFactHint(content) == "" {
			t.Errorf("repoFactHint(%q) = \"\" — a note naming the repository and what is in it is the shape the advisory is for", content)
		}
	}

	quiet := []string{
		// The issue's two good examples: durable knowledge, no repository anchor.
		"Production schema changes require explicit approval.",
		"Deployment keeps database migrations separate from application rollout, on purpose.",
		// BOTH conditions present across the note but never in one sentence: a
		// note-wide check pairs them, and this is the note that would flag.
		// Predicate first, reference second.
		"The session-start digest contains the scope label. It is printed by internal/mcpinit/hook.go.",
		// The same pairing the other way round, with the reference first and
		// the predicate in a later sentence of its own.
		"The fold target is decided by the store. `Store.UpsertWithOptions` owns the transaction that decides it.",
		// A path plus a BEHAVIOURAL claim, which is a reason and not a
		// repository fact: it has a reference and no containment predicate.
		// This is a real stored memory of this repository, and it is the case a
		// widened predicate list would break.
		"cmd/ghost/lifecycle.go calls os.Exit, so the tier switch is unreachable from a test and has to be pinned at the seam.",
		// A claim about the repository with nothing to locate it in.
		"The linker imports nothing from the harness package.",
		// Ordinary knowledge, no repository anchor at all.
		"SSH to the bastion goes through port 2222, not 22.",
		// The five probes an independent review measured firing on durable
		// notes. Every one is a CAPITALISED dotted token that is not a code
		// reference: a person's name, a product name, an initialism, and a
		// Go-looking identifier dropped into prose. Each carries a genuine
		// containment predicate ("is defined by", "is where", "are defined
		// in"), so only the REFERENCE half is wrong — which is why they are
		// the fixtures that pin it. Without them the qualified-identifier
		// branch is unpinned and a future edit can widen it back to any
		// Capital.Ident.
		"This rule is defined by U.S. regulators for production compliance.",
		"Bob.Smith is where escalations are routed after hours.",
		"Node.js is where the build tooling lives for the frontend, per team convention.",
		"The release manager is named per quarter, and Terraform.State is where locks are held.",
		"Escalation contacts are defined in the on-call rotation, not in Jane.Doe's calendar.",
		"",
	}
	for _, content := range quiet {
		if hint := repoFactHint(content); hint != "" {
			t.Errorf("repoFactHint(%q) = %q — durable knowledge must not be flagged as a repository fact", content, hint)
		}
	}
}

// TestSaveAdvisesAndStillStoresARepositoryFact is the load-bearing pair: the
// save response carries the advisory AND the memory is stored, byte-exact. The
// issue rules automatic rejection out of scope ("the agent decides what to
// save; Ghost only guides"), so the hint may only ever be a sentence appended
// to a response that reports a stored id.
func TestSaveAdvisesAndStillStoresARepositoryFact(t *testing.T) {
	srv, session := newCapSession(t)

	content := "internal/mcpserver/mcpserver.go contains HandleFoo()"
	resp, stored := saveAndFetchContent(t, srv, session, content)

	if !strings.Contains(resp, "ADVISORY") {
		t.Errorf("save of a repository fact must carry the advisory; got %q", resp)
	}
	if !strings.Contains(stored, content) {
		t.Errorf("the advisory is advice, not a refusal: stored content = %q, want the note verbatim", stored)
	}
}

// TestSaveResponseSaysItStoredTheNoteAnyway keeps the advisory honest about
// its own effect. An agent that reads a warning in a save response has to be
// able to tell whether the write happened, or it will re-save and duplicate the
// memory.
func TestSaveResponseSaysItStoredTheNoteAnyway(t *testing.T) {
	srv, session := newCapSession(t)

	resp, _ := saveAndFetchContent(t, srv, session, "foo.go contains HandleFoo()")
	if !strings.Contains(resp, "Memory saved (id: ") {
		t.Errorf("an advised save must still report the stored id; got %q", resp)
	}
	if !strings.Contains(resp, "Stored anyway") {
		t.Errorf("the advisory must say the save happened anyway; got %q", resp)
	}
}

// TestGlobalSaveAdvisesToo: the instructions promise a repository-fact save is
// stored "with a note saying so" and then send an agent to ghost_save_global for
// the cross-project case, so the advisory that promise describes has to be on
// both write paths that store a memory. A global save is the one that gets read
// by every later project, which is the more expensive place to leave a stale
// code-location fact.
func TestGlobalSaveAdvisesToo(t *testing.T) {
	srv, session := newCapSession(t)
	_ = srv

	for _, tc := range []struct {
		content      string
		wantAdvisory bool
	}{
		// The issue's bad example, verbatim, saved as a global memory.
		{"foo.go contains HandleFoo()", true},
		{"Always use 2-space YAML indentation", false},
	} {
		res := callTool(t, session, "ghost_save_global", map[string]any{
			"content":  tc.content,
			"category": "fact",
		})
		resp := resultText(res)
		if !strings.Contains(resp, "Global memory saved (id: ") {
			t.Fatalf("global save response reports no stored id: %q", resp)
		}
		if got := strings.Contains(resp, "ADVISORY"); got != tc.wantAdvisory {
			t.Errorf("global save of %q: advisory present = %v, want %v; got %q", tc.content, got, tc.wantAdvisory, resp)
		}
		// Advisory or not, the memory exists and is findable — the advisory is
		// advice, never a refusal, on this path too.
		found := callTool(t, session, "ghost_memory_search", map[string]any{
			"project_id": "test-project",
			"query":      tc.content,
			"limit":      5,
		})
		if !strings.Contains(resultText(found), "«"+tc.content+"»") {
			t.Errorf("global save of %q was not stored: %q", tc.content, resultText(found))
		}
	}
}

// TestUpdateAdvisesOnAContentRewrite: ghost_memory_update is the third path
// that writes caller-supplied memory text and reports a real stored id, and an
// agent correcting a memory INTO a repository fact is the same mistake as
// saving one. The advisory is gated on content being present, so this also pins
// that a tag-only edit — which stores no text and cannot be judged — stays quiet
// rather than advising about a note the caller never rewrote.
func TestUpdateAdvisesOnAContentRewrite(t *testing.T) {
	srv, session := newCapSession(t)
	ctx := context.Background()

	id, err := srv.store.Create(ctx, "abc123", memory.Memory{
		Category: "fact", Content: "an older note about the same subject", Source: "mcp", Importance: 0.7, Tags: []string{},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	cases := []struct {
		name         string
		args         map[string]any
		wantAdvisory bool
	}{
		{
			name:         "rewriting the content into a repository fact advises",
			args:         map[string]any{"content": "foo.go contains HandleFoo()"},
			wantAdvisory: true,
		},
		{
			name:         "rewriting the content into a durable rule does not",
			args:         map[string]any{"content": "Production schema changes require explicit approval."},
			wantAdvisory: false,
		},
		{
			name:         "a tag-only edit has no text to judge",
			args:         map[string]any{"tags": []any{"a", "b"}},
			wantAdvisory: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.args["project_id"] = "test-project"
			tc.args["memory_id"] = id
			resp := resultText(callTool(t, session, "ghost_memory_update", tc.args))
			if !strings.Contains(resp, "Memory updated (id: "+id+")") {
				t.Fatalf("update response reports no stored id: %q", resp)
			}
			if got := strings.Contains(resp, "ADVISORY"); got != tc.wantAdvisory {
				t.Errorf("advisory present = %v, want %v; got %q", got, tc.wantAdvisory, resp)
			}
		})
	}
}

// TestDurableSavesCarryNoAdvisory is the false-positive guard on the live path
// rather than on the shape function: durable knowledge is saved through the
// real tool and must come back with a response that says nothing about
// durability. A hint that fires on durable knowledge is noise on every save,
// and noise is how an advisory stops being read.
//
// The last two cases are the ones that make this a two-condition test rather
// than a "does the note mention a file" test: one carries a containment claim
// and a path in DIFFERENT sentences, the other a path and a behavioural claim
// and no containment claim at all. The middle one is a real stored memory of
// this repository.
func TestDurableSavesCarryNoAdvisory(t *testing.T) {
	srv, session := newCapSession(t)

	for _, content := range []string{
		"Production schema changes require explicit approval.",
		"Deployment keeps database migrations separate from application rollout, on purpose.",
		"cmd/ghost/lifecycle.go calls os.Exit, so the tier switch is unreachable from a test and has to be pinned at the seam.",
		"The session-start digest contains the scope label. It is printed by internal/mcpinit/hook.go.",
	} {
		resp, stored := saveAndFetchContent(t, srv, session, content)
		if strings.Contains(resp, "ADVISORY") {
			t.Errorf("durable knowledge must be saved without an advisory; got %q for %q", resp, content)
		}
		if stored != content {
			t.Errorf("stored content = %q, want %q verbatim", stored, content)
		}
	}
}
