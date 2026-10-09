package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// #960: the advisory fired on conventions that name the files they apply to. In
// the save-quality audit (v0.48.0, 16 runs) two of the three advisories were
// false positives, and both fired on the same planted convention — a rule with
// its reason that happens to name a file. The one true positive was a bare
// "X is a no-op" claim, which is what the second half of this table is.
//
// Every QUIET row below would draw the advisory on its reference token and its
// containment predicate alone, so each one is the exemption doing the work
// rather than a note that was never flagged in the first place. That is what
// makes this a two-direction table: the same shape with the reason stripped
// off is the case that still fires.
func TestRepoFactHintLeavesAConventionThatNamesItsFileAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		category string
		content  string
	}{
		{
			// A reason connector with no rule word: the note is durable
			// because it says WHY, which the repository cannot restate.
			name:     "a convention naming the file its rule lives in, with the reason",
			category: "convention",
			content:  "Convention: the schema version is declared in internal/memory/schema.go, because an older build's store is migrated on open rather than read as it stands.",
		},
		{
			// A rule word ("always") under a rule category and no reason
			// connector anywhere: the half of the exemption that needs the
			// category, so a rule-shaped claim is not judged as a location.
			name:     "a decision that is a rule and names its file, with no connector at all",
			category: "decision",
			content:  "Decision: the repository-fact hint always reads the same two patterns, and the pair is defined in internal/mcpserver/durable.go beside the save path it judges.",
		},
		{
			// "on purpose" is a stated deliberate choice, which is the exact
			// thing a repository fact cannot carry.
			name:     "a gotcha naming the file, explained as deliberate",
			category: "gotcha",
			content:  "Gotcha: the sync check is defined in internal/mcpserver/ensure_project.go on purpose, because a bound path is the only address a session has.",
		},
		{
			// The reason sits in a LATER sentence than the claim it
			// explains, which is the scope of the connector half: a note
			// that carries a reason anywhere is a note that carries
			// knowledge the repository does not hold.
			name:     "a convention whose reason is a sentence of its own",
			category: "convention",
			content:  "The rule half is defined in internal/mcpserver/durable.go, and the advisory is appended to the save response. It reads the whole note, because a reason can sit in a sentence other than the one naming the file.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if hint := repoFactHint(tc.content, tc.category); hint != "" {
				t.Errorf("repoFactHint(%q, %q) = %q — a rule with its reason is durable knowledge, not a repository restatement", tc.content, tc.category, hint)
			}
		})
	}
}

// TestRepoFactHintStillFlagsABareRestatementOfAFile is the other direction, and
// the reason the exemption is scoped the way it is: strip the reason off a
// convention and what is left IS what the file holds, so the advisory is the
// right thing for it. A category alone never exempts a note — a bare location
// claim filed as a convention is still a bare location claim.
func TestRepoFactHintStillFlagsABareRestatementOfAFile(t *testing.T) {
	for _, tc := range []struct {
		name     string
		category string
		content  string
	}{
		{
			name:     "the issue's bad example, verbatim",
			category: "fact",
			content:  "foo.go contains HandleFoo()",
		},
		{
			name:     "a bare statement of what a file defines",
			category: "fact",
			content:  "internal/reflection/prompt.go defines the drop guard.",
		},
		{
			name:     "a bare statement of where a helper lives",
			category: "fact",
			content:  "The restore helper is in internal/memory/backup.go.",
		},
		{
			// The same sentence filed as a rule CATEGORY with no reason
			// connector and no rule word: the category half of the exemption
			// is not a blanket pardon, or every convention that cites a path
			// would go quiet.
			name:     "a bare restatement filed as a convention",
			category: "convention",
			content:  "The restore helper is in internal/memory/backup.go.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if hint := repoFactHint(tc.content, tc.category); hint == "" {
				t.Errorf("repoFactHint(%q, %q) = \"\" — a bare restatement of what a file does is the shape the advisory is for", tc.content, tc.category)
			}
		})
	}
}

// TestRepoFactHintGatesTheRuleHalfOnTheCategory pins the asymmetry between the
// two halves: the reason connector is a property of the NOTE, so it exempts
// whatever category the save was filed under, while a rule word is a property
// of the SENTENCE being judged and only counts in a category whose notes are
// rules by nature (convention, decision, gotcha). A note filed as a `fact` is
// not excused for containing "always", because `fact` makes no such claim
// about the note and the savings must not depend on a caller's spelling.
func TestRepoFactHintGatesTheRuleHalfOnTheCategory(t *testing.T) {
	content := "Decision: the repository-fact hint always reads the same two patterns, and the pair is defined in internal/mcpserver/durable.go beside the save path it judges."

	for _, category := range []string{"convention", "decision", "gotcha", "Convention"} {
		if hint := repoFactHint(content, category); hint != "" {
			t.Errorf("repoFactHint(..., %q) = %q — category %q is a rule category", content, category, hint)
		}
	}
	for _, category := range []string{"fact", "architecture", "pattern", "dependency", "preference", ""} {
		if hint := repoFactHint(content, category); hint == "" {
			t.Errorf("repoFactHint(..., %q) = \"\" — category %q is not a rule category, so the rule word exempts nothing", content, category)
		}
	}
}

// TestASavedConventionThatNamesAFileDrawsNoAdvisory is the live-path half of
// #960: the false positive was measured on saves, so the exemption has to be
// reachable from the tool and not only from the shape function. The category
// arrives with the save, so the rule half is judged against what the caller
// filed rather than against nothing.
func TestASavedConventionThatNamesAFileDrawsNoAdvisory(t *testing.T) {
	srv, session := newCapSession(t)

	for _, tc := range []struct {
		name         string
		category     string
		content      string
		wantAdvisory bool
	}{
		{
			name:         "a convention with its reason names its file",
			category:     "convention",
			content:      "Convention: the schema version is declared in internal/memory/schema.go, because an older build's store is migrated on open rather than read as it stands.",
			wantAdvisory: false,
		},
		{
			name:         "a gotcha carrying a reason connector is exempt whatever its category",
			category:     "fact",
			content:      "Gotcha: the sync check is defined in internal/mcpserver/ensure_project.go on purpose, because a bound path is the only address a session has.",
			wantAdvisory: false,
		},
		{
			// The other direction, through the same tool: a location claim
			// with no reason and no rule word is still what the advisory is
			// for, and it is stored either way.
			name:         "a bare restatement of what a file holds",
			category:     "fact",
			content:      "The restore helper is in internal/memory/backup.go.",
			wantAdvisory: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := callTool(t, session, "ghost_memory_save", map[string]any{
				"project_id": "test-project",
				"content":    tc.content,
				"category":   tc.category,
			})
			resp := resultText(res)
			if !strings.Contains(resp, "Memory saved (id: ") {
				t.Fatalf("save response reports no stored id: %q", resp)
			}
			if got := strings.Contains(resp, "ADVISORY"); got != tc.wantAdvisory {
				t.Errorf("advisory present = %v, want %v; got %q", got, tc.wantAdvisory, resp)
			}
			// Advisory or not, the save landed: the hint guides and never
			// refuses, which is the whole of what keeps #960 safe to fix
			// this way.
			id, ok := extractID(resp)
			if !ok || id == "" {
				t.Fatalf("save response carries no id: %q", resp)
			}
			mems, err := srv.store.GetByIDs(context.Background(), []string{id})
			if err != nil || len(mems) != 1 {
				t.Fatalf("GetByIDs(%q): err=%v n=%d", id, err, len(mems))
			}
			if mems[0].Content != tc.content {
				t.Errorf("stored content = %q, want the note verbatim", mems[0].Content)
			}
		})
	}
}

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
		// Every case here is filed as a `fact`, which is the category that
		// exempts nothing: the rule half needs a rule category, and none of
		// these notes carries a reason connector, so the shape alone decides.
		if repoFactHint(content, "fact") == "" {
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
		if hint := repoFactHint(content, "fact"); hint != "" {
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
