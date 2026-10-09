//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// e2eProject is the project every MCP test saves under. One name, so a test
// that forgets to say which project it meant is obvious.
const e2eProject = "e2e-proj"

// ready returns a sandbox with a live `ghost mcp` session and one saved
// memory, which is the state almost every tool needs before it can be asked
// anything meaningful. The saved id comes back so a test can address it.
func ready(t *testing.T) (*sandbox, *mcp.ClientSession, string) {
	t.Helper()
	s := newSandbox(t)
	cs := s.mcpSession(t)
	id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the relay listens on port 2222 in staging",
		"category":   "architecture",
		"importance": 0.8,
	}))
	return s, cs, id
}

// toolChecks is every tool the MCP server advertises, mapped to the subtest
// that drives it.
//
// The table is the coverage contract, not a convenience: TestMCPSurface fails
// when the server advertises a tool this map does not name, or names one the
// server stopped advertising. A tool added to the product without a check here
// is a tool nothing has ever called as a user calls it.
var toolChecks = map[string]func(t *testing.T, s *sandbox, cs *mcp.ClientSession, seeded string){
	"ghost_memory_save": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a second, unrelated note about the release process",
			"category":   "convention",
			"tags":       []string{"release", "process"},
			"scope":      map[string]any{"environment": "staging"},
			"importance": 0.6,
		})
		id := parseID(t, out)
		mustContain(t, "save", out, "Memory saved")
		if got := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND scope IS NOT NULL`, id); got != 1 {
			t.Fatalf("saved memory %s is not in the store with a scope", id)
		}
		// The tag list is stored, which is the part of the contract a caller
		// cannot see from the answer.
		tags := s.queryStrings(t, `SELECT tags FROM memories WHERE id = ?`, id)
		if len(tags) != 1 || !strings.Contains(tags[0], "release") {
			t.Fatalf("tags not stored: %v", tags)
		}
	},

	"ghost_memory_search": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, seeded string) {
		out := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "relay port",
		})
		mustContain(t, "search", out, "2222")
		mustNotContain(t, "search", out, "No matching memories found")

		// A query nothing matches must say so rather than answering with the
		// nearest thing, and it must not claim the store has nothing either:
		// the vector leg cannot vouch for rows it never saw, so the answer
		// states what it searched and stops there (#580).
		empty := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "zzzznotpresentzzzz",
		})
		mustNotContain(t, "search (miss)", empty, "relay port")
		mustNotContain(t, "search (miss)", empty, "No matching memories found")
		mustContain(t, "search (miss)", empty, "no match within the searched window")
		mustContain(t, "search (miss)", empty, "reason=no_candidates")
		_ = seeded
	},

	"ghost_memory_update": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, seeded string) {
		out := call(t, cs, "ghost_memory_update", map[string]any{
			"project_id": e2eProject,
			"memory_id":  seeded,
			"content":    "the relay listens on port 2222 in production",
		})
		mustContain(t, "update", out, "Memory updated")
		got := s.queryStrings(t, `SELECT content FROM memories WHERE id = ?`, seeded)
		if len(got) != 1 || !strings.Contains(got[0], "production") {
			t.Fatalf("update did not reach the store: %v", got)
		}
	},

	"ghost_memory_pin": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, seeded string) {
		out := call(t, cs, "ghost_memory_pin", map[string]any{
			"project_id": e2eProject,
			"memory_id":  seeded,
			"pinned":     true,
		})
		mustContain(t, "pin", out, "pinned")
		if n := s.queryInt(t, `SELECT pinned FROM memories WHERE id = ?`, seeded); n != 1 {
			t.Fatalf("pinned = %d, want 1", n)
		}
		off := call(t, cs, "ghost_memory_pin", map[string]any{
			"project_id": e2eProject,
			"memory_id":  seeded,
			"pinned":     false,
		})
		mustContain(t, "unpin", off, "unpinned")
		if n := s.queryInt(t, `SELECT pinned FROM memories WHERE id = ?`, seeded); n != 0 {
			t.Fatalf("pinned after unpin = %d, want 0", n)
		}
	},

	// An agent saying "this memory is wrong" without deleting it. The flag is
	// negative evidence and nothing else: it is appended, attributed, and hashed
	// against the content it was about — and the REASON it was given is the one
	// thing that must not come back, because this tool's answer lands in the same
	// agent context the reason came from.
	"ghost_memory_flag": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, seeded string) {
		const marker = "ZZREASONNEVERLEAVESSTOREZZ"
		out := call(t, cs, "ghost_memory_flag", map[string]any{
			"project_id": e2eProject,
			"memory_id":  seeded,
			"kind":       "wrong",
			"reason":     marker + " port 2222 was decommissioned in June",
		})
		mustContain(t, "flag", out, seeded)
		mustNotContain(t, "flag", out, marker)
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_flags WHERE memory_id = ?`, seeded); n != 1 {
			t.Fatalf("memory_flags rows for the flagged memory = %d, want 1", n)
		}
		// The stamp is what makes a flag answerable after a rewrite, so it is
		// checked here rather than only in the store's own tests: a hash that
		// does not match the stored content withdraws the flag silently.
		hash := s.queryStrings(t, `SELECT content_hash FROM memory_flags WHERE memory_id = ?`, seeded)
		if len(hash) != 1 || len(hash[0]) != 64 {
			t.Fatalf("content_hash = %v, want one 64-character digest", hash)
		}
		// A second flag appends rather than replacing the first.
		call(t, cs, "ghost_memory_flag", map[string]any{
			"project_id": e2eProject,
			"memory_id":  seeded,
			"kind":       "stale",
			"reason":     "the staging config moved to the new relay",
		})
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_flags WHERE memory_id = ?`, seeded); n != 2 {
			t.Fatalf("memory_flags rows after a second flag = %d, want 2", n)
		}
		// The contract's refusals, in the order an agent hits them: an id it
		// guessed, a kind outside the two, and a reason at neither edge of its
		// bound. Each is a refusal with a row not written behind it.
		for _, tc := range []struct {
			name string
			args map[string]any
		}{
			{"unknown id", map[string]any{
				"project_id": e2eProject, "memory_id": "ffffffffffffffffffffffffffffffff",
				"kind": "wrong", "reason": "it is wrong",
			}},
			{"unknown kind", map[string]any{
				"project_id": e2eProject, "memory_id": seeded,
				"kind": "useful", "reason": "it is right",
			}},
			{"no reason", map[string]any{
				"project_id": e2eProject, "memory_id": seeded,
				"kind": "stale", "reason": "",
			}},
		} {
			refused := callExpectingError(t, cs, "ghost_memory_flag", tc.args)
			mustNotContain(t, tc.name+" refusal", refused, marker)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_flags WHERE memory_id = ?`, seeded); n != 2 {
			t.Fatalf("memory_flags rows after three refusals = %d, want 2", n)
		}
	},

	// The repair for a wrong supersession an agent can SEE. Both repair passes
	// are CLI-only, and one of them only withdraws what the current rules reject,
	// so an edge the classifier still accepts had no path out of the graph at all
	// before this tool.
	"ghost_link_withdraw": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		// Saved stale-first and then a second later, because the pass orients a
		// pair by updated_at and a same-second pair ties — the direction this case
		// asserts on would then be whatever the query happened to return.
		older := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the relay backlog takes about forty minutes to drain",
		}))
		time.Sleep(1100 * time.Millisecond)
		newer := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the relay backlog drains in under a minute now",
		}))
		// The edge is created by the real pass rather than by a fixture row: this
		// suite's store is opened read-only, so an edge that exists here is one
		// the product wrote. The stub answer names the retired claim, which the
		// KEEP-biased rubric requires.
		//
		// claude, not opencode: the stub wraps an answer in one JSONL text event,
		// which cannot hold the multi-line answer a BATCH of pairs needs, and this
		// project's other notes make the scan propose more than one pair. The
		// claude backend takes bare stdout, so a numbered batch parses.
		s.setHarnessAnswer("supersede", "SUPERSEDES | replaced: the forty minute drain was the two-worker backlog")
		// The creation pass, retried a bounded number of times, because the
		// candidate scan reads every memory's embedding from the stub endpoint and
		// a read that fails leaves that memory with no similarity candidates BY
		// DESIGN (internal/supersede.SelectCandidates) — so one transient read
		// failure proposes nothing and the pass reports a clean no-op. The
		// failure message carries the pass's own report, so a genuine failure (a
		// verdict the parser refuses, a write error) is still diagnosable from
		// the output rather than hidden behind the retry.
		const attempts = 3
		for attempt := 1; ; attempt++ {
			created := s.mustRun("supersede", e2eProject, "--source", "claude-code", "--threshold", "0.1", "--apply")
			if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links
				WHERE relation = 'supersedes' AND invalidated_at IS NULL AND source_id = ? AND target_id = ?`, newer, older); n == 1 {
				break
			} else if attempt == attempts {
				t.Fatalf("the creation pass left %d edge(s) between these two notes, want 1, after %d attempts:\n%s",
					n, attempts, created.stdout+created.stderr)
			}
		}
		before, _ := s.harnessLog("claude")

		out := call(t, cs, "ghost_link_withdraw", map[string]any{
			"project_id": e2eProject,
			"source_id":  newer[:8],
			"target_id":  older[:8],
		})
		mustContain(t, "withdraw", out, "Withdrew 1")
		// The result names the memory the edge was burying and the step that
		// un-hides it. Without the second, an agent reads a finished repair where
		// half of one happened: the resolved_at the edge caused is still there.
		mustContain(t, "withdraw (target)", out, "about forty minutes")
		// The repair is a CLI COMMAND, SCOPED to the ids this call withdrew, and
		// named as a command: there is no MCP tool for it. `ghost_resolve` is the
		// forward pass — it stamps resolved_at on confirmed evidence — so an agent
		// pointed at it would bury MORE memories and pay a harness call.
		mustContain(t, "withdraw (follow-up)", out, "ghost resolve e2e-proj --reassess --only")
		mustContain(t, "withdraw (follow-up) scope", out, "SCOPED")
		mustContain(t, "withdraw (follow-up) target", out, older)
		mustContain(t, "withdraw (follow-up) not a tool", out, "no MCP tool for that repair")
		// Scoped to this pair: the stub answer above confirmed every candidate the
		// scan proposed, so the project holds other live edges this call was not
		// asked about and must not have touched.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_links
			WHERE relation = 'supersedes' AND invalidated_at IS NULL AND source_id = ? AND target_id = ?`, newer, older); n != 0 {
			t.Fatalf("the withdrawal left the named edge live (%d)", n)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'unsupersede'`, older); n != 1 {
			t.Fatalf("the withdrawal wrote %d unsupersede history row(s), want 1", n)
		}
		if after, _ := s.harnessLog("claude"); after != before {
			t.Fatalf("the tool asked the harness: it judges nothing, so a call here is billed for no judgment")
		}

		// A pair with no live edge is an error, not a quiet success, and an
		// unknown project never reaches another project's graph.
		fail := callExpectingError(t, cs, "ghost_link_withdraw", map[string]any{
			"project_id": e2eProject,
			"source_id":  newer,
			"target_id":  older,
		})
		mustContain(t, "withdraw (no live edge)", fail, "no live supersedes or causes link")
		fail = callExpectingError(t, cs, "ghost_link_withdraw", map[string]any{
			"project_id": "no-such-project-here",
			"source_id":  newer,
			"target_id":  older,
		})
		mustContain(t, "withdraw (unknown project)", fail, "not found")
	},

	// The mark an agent can make by NAME. Every other way of resolving a memory
	// goes through a pass that judges the whole project, and the case this tool
	// exists for is the one no pass can reach: a note whose claim a NEWER note in
	// the same project says was fixed, where the note itself holds no resolution
	// keyword and the prefilter never proposes it. An agent that has read both is
	// the only thing that can decide that.
	"ghost_resolve_mark": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		// No resolution keyword, so nothing here is a candidate for any pass.
		// That is what makes this a mark of a memory no pass would have buried.
		stale := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the relay firmware on the edge nodes runs build 4471",
		}))
		untouched := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging relay speaks QUIC on port 4471",
		}))
		before, _ := s.harnessLog("opencode")

		out := call(t, cs, "ghost_resolve_mark", map[string]any{
			"project_id": e2eProject,
			"memory_ids": []string{stale[:8]},
		})
		mustContain(t, "mark", out, "Marked 1")
		// The memory itself, so an agent reporting this to its user is quoting
		// the database rather than the call it made.
		mustContain(t, "mark (memory)", out, "the relay firmware on the edge nodes runs build 4471")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, stale); n != 1 {
			t.Fatalf("the tool did not stamp the named memory")
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, untouched); n != 0 {
			t.Fatalf("the tool touched a memory it was not asked about")
		}
		// The 'resolve' history row, with the calling client as the performer —
		// the same provenance every other mutating tool on this surface records.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'resolve'`, stale); n != 1 {
			t.Fatalf("the tool wrote %d resolve history row(s), want 1", n)
		}
		if got := s.queryStrings(t, `SELECT agent FROM memory_history WHERE memory_id = ? AND phase = 'resolve'`, stale); len(got) != 1 || got[0] != "claude-code" {
			t.Errorf("the resolve history row's agent = %v, want [claude-code]", got)
		}
		// The KEEP cache is dropped, so a later pass cannot report the row as
		// cached and bring it straight back.
		if got := s.queryStrings(t, `SELECT resolve_kept_hash FROM memories WHERE id = ?`, stale); len(got) != 1 || got[0] != "" {
			t.Errorf("resolve_kept_hash = %v, want [\"\"] — a cached KEEP would un-hide this row on the next pass", got)
		}
		if after, _ := s.harnessLog("opencode"); after != before {
			t.Fatalf("the tool asked the harness: it judges nothing, so a call here is billed for no judgment")
		}
		// The inverse is a CLI COMMAND, SCOPED to what this call stamped, and
		// named as a command: there is no MCP tool for clearing a resolved_at,
		// because ghost_resolve is the forward pass and would stamp MORE memories.
		mustContain(t, "mark (follow-up)", out, "ghost resolve e2e-proj --reassess --only")
		mustContain(t, "mark (follow-up) not a tool", out, "no MCP tool for it")
		mustContain(t, "mark (follow-up) target", out, stale)

		// Marking it again is a no-op, not a second stamp: nothing is written, so
		// nothing may be claimed.
		again := call(t, cs, "ghost_resolve_mark", map[string]any{
			"project_id": e2eProject,
			"memory_ids": []string{stale},
		})
		mustContain(t, "mark (no-op)", again, "already resolved")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ? AND phase = 'resolve'`, stale); n != 1 {
			t.Fatalf("the no-op wrote %d resolve history row(s) in total, want 1", n)
		}

		// A ref the project cannot resolve is an error, not a guess.
		fail := callExpectingError(t, cs, "ghost_resolve_mark", map[string]any{
			"project_id": e2eProject,
			"memory_ids": []string{"not-an-id-at-all"},
		})
		mustContain(t, "mark (unresolvable ref)", fail, "no memory in project")
		// And an empty list is a usage error, not a mark of nothing that reads
		// as a completed no-op.
		fail = callExpectingError(t, cs, "ghost_resolve_mark", map[string]any{
			"project_id": e2eProject,
			"memory_ids": []string{},
		})
		mustContain(t, "mark (no ids)", fail, "memory_ids is required")
	},

	"ghost_memory_promote": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note that belongs to every project",
			"category":   "preference",
		}))
		out := call(t, cs, "ghost_memory_promote", map[string]any{
			"project_id": e2eProject,
			"memory_id":  id,
		})
		mustContain(t, "promote", out, "global scope")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND project_id = '_global'`, id); n != 1 {
			t.Fatalf("memory %s was not moved to _global", id)
		}
	},

	"ghost_memory_delete": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a disposable note about the staging relay",
		}))
		out := call(t, cs, "ghost_memory_delete", map[string]any{
			"project_id": e2eProject,
			"memory_id":  id,
		})
		mustContain(t, "delete", out, "eleted")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, id); n != 0 {
			t.Fatalf("memory %s survived the delete", id)
		}
		// The default keeps the history: the row is gone, the record of what
		// it held is not. That asymmetry is the whole reason
		// purge_history exists, so it is asserted rather than assumed.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id); n == 0 {
			t.Fatalf("delete without purge_history erased the recorded history too")
		}
	},

	"ghost_memories_list": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a gotcha about the sqlite writer lock",
			"category":   "gotcha",
		})
		out := call(t, cs, "ghost_memories_list", map[string]any{
			"project_id": e2eProject,
			"category":   "gotcha",
		})
		mustContain(t, "memories_list", out, "writer lock")
		mustNotContain(t, "memories_list (category filter)", out, "relay listens")

		// A category that matches nothing must come back empty, not with the
		// unfiltered set.
		none := call(t, cs, "ghost_memories_list", map[string]any{
			"project_id": e2eProject,
			"category":   "dependency",
		})
		mustNotContain(t, "memories_list (no matches)", none, "writer lock")
	},

	"ghost_project_context": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, seeded string) {
		out := call(t, cs, "ghost_project_context", map[string]any{"project_id": e2eProject})
		mustContain(t, "project_context", out, "2222")
		_ = seeded
	},

	"ghost_list_projects": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_list_projects", nil)
		mustContain(t, "list_projects", out, e2eProject)
		// The builtin global memories are seeded by the store, so the list
		// always has at least the project and _global.
		mustContain(t, "list_projects", out, "_global")
	},

	"ghost_search_all": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "cross project note: the deploy runbook lives in the ops vault",
			"category":   "convention",
		})
		out := call(t, cs, "ghost_search_all", map[string]any{
			"query": "deploy runbook",
		})
		mustContain(t, "search_all", out, "runbook")
		// search_all is the cross-project surface: it must not require (or
		// accept) a project filter at all.
		mustNotContain(t, "search_all", out, "No matching memories found")
	},

	"ghost_save_global": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_save_global", map[string]any{
			"content":  "the user's own cross-project preference about commit style",
			"category": "preference",
		})
		id := parseID(t, out)
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND project_id = '_global'`, id); n != 1 {
			t.Fatalf("global save %s is not under _global", id)
		}
	},

	"ghost_task_create": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_task_create", map[string]any{
			"project_id":  e2eProject,
			"title":       "measure the relay port change",
			"description": "capture the before and after listener",
			"priority":    1,
		})
		mustContain(t, "task_create", out, "Task created")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM tasks WHERE project_id = ? AND title = ?`,
			e2eProject, "measure the relay port change"); n != 1 {
			t.Fatalf("the task is not in the store")
		}
	},

	"ghost_task_list": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		call(t, cs, "ghost_task_create", map[string]any{
			"project_id": e2eProject,
			"title":      "a pending task",
		})
		call(t, cs, "ghost_task_create", map[string]any{
			"project_id": e2eProject,
			"title":      "a blocked task",
		})
		// Complete one so the status filter has something to exclude.
		list := call(t, cs, "ghost_task_list", map[string]any{"project_id": e2eProject})
		mustContain(t, "task_list", list, "a pending task")
		mustContain(t, "task_list", list, "a blocked task")

		done := call(t, cs, "ghost_task_list", map[string]any{
			"project_id": e2eProject,
			"status":     "done",
		})
		mustNotContain(t, "task_list (status=done)", done, "a pending task")
	},

	"ghost_task_update": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_task_create", map[string]any{
			"project_id": e2eProject,
			"title":      "a task to reprioritise",
			"priority":   4,
		})
		id := taskIDFrom(t, out)
		call(t, cs, "ghost_task_update", map[string]any{
			"task_id": id,
			"status":  "active",
		})
		if n := s.queryInt(t, `SELECT COUNT(*) FROM tasks WHERE id LIKE ? AND status = 'active'`, id+"%"); n != 1 {
			t.Fatalf("task %s did not become active", id)
		}
		// A priority change and a description change on the same call.
		desc := "a longer description written by the update"
		call(t, cs, "ghost_task_update", map[string]any{
			"task_id":     id,
			"priority":    0,
			"description": desc,
		})
		if n := s.queryInt(t, `SELECT COUNT(*) FROM tasks WHERE id LIKE ? AND priority = 0 AND status = 'active' AND description = ?`,
			id+"%", desc); n != 1 {
			t.Fatalf("task %s is not priority 0 / active with the new description", id)
		}
	},

	"ghost_task_complete": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_task_create", map[string]any{
			"project_id": e2eProject,
			"title":      "a task to finish",
		})
		id := taskIDFrom(t, out)
		done := call(t, cs, "ghost_task_complete", map[string]any{
			"task_id": id,
			"notes":   "the relay port change is measured",
		})
		mustContain(t, "task_complete", done, "ompleted")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM tasks WHERE id LIKE ? AND status = 'done'`, id+"%"); n != 1 {
			t.Fatalf("task %s is not done", id)
		}
	},

	"ghost_decision_record": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_decision_record", map[string]any{
			"project_id":   e2eProject,
			"title":        "Use a single writer connection",
			"decision":     "the store pins MaxOpenConns to one",
			"rationale":    "a pool query inside an open transaction deadlocks",
			"alternatives": []string{"a read pool", "retry on busy"},
			"tags":         []string{"sqlite"},
		})
		mustContain(t, "decision_record", out, "Decision recorded")
		mustContain(t, "decision_record", out, "decision_id:")
		mustContain(t, "decision_record", out, "memory_id:")
		// The decision is stored twice on purpose: a decisions row and an
		// ordinary companion memory. Both are asserted, because a tool that
		// wrote only one of them would still print a success message.
		if n := s.queryInt(t, `SELECT COUNT(*) FROM decisions WHERE title = ?`, "Use a single writer connection"); n != 1 {
			t.Fatalf("the decision row is not in the store")
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE project_id = ? AND content LIKE '%single writer connection%'`, e2eProject); n == 0 {
			t.Fatalf("the companion memory is not in the store")
		}
	},

	"ghost_decisions_list": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		call(t, cs, "ghost_decision_record", map[string]any{
			"project_id": e2eProject,
			"title":      "Pin the pool to one connection",
			"decision":   "MaxOpenConns(1)",
			"rationale":  "an open transaction cannot see its own pool query",
		})
		out := call(t, cs, "ghost_decisions_list", map[string]any{
			"project_id": e2eProject,
			"status":     "active",
		})
		mustContain(t, "decisions_list", out, "Pin the pool to one connection")
		superseded := call(t, cs, "ghost_decisions_list", map[string]any{
			"project_id": e2eProject,
			"status":     "superseded",
		})
		mustNotContain(t, "decisions_list (superseded)", superseded, "Pin the pool to one connection")
	},

	"ghost_health": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		out := call(t, cs, "ghost_health", nil)
		mustContain(t, "health", out, "Ghost Health")
		mustContain(t, "health", out, "**Projects:**")
		mustContain(t, "health", out, "**Total memories:**")
		// Embedding is wired in the sandbox, and the endpoint answers, so the
		// report must say embeddings are on rather than leaving it to be
		// inferred from silence.
		mustContain(t, "health", out, "**Embeddings:** enabled")
		mustNotContain(t, "health", out, "Ollama unreachable")
		mustNotContain(t, "health", out, "not installed in Ollama")
		mustContain(t, "health", out, "**Memory links:**")
		// History growth rides along (#729), and this store has version rows in it
		// by now, so the block is present rather than the "no version rows" line.
		// What it SAYS is TestCLIMCPStatusHistoryGrowth's business, against a
		// store built to have a known share.
		mustContain(t, "health", out, "**History:**")
		mustNotContain(t, "health", out, "no version rows recorded yet")
	},

	"ghost_project_delete": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		victim := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": "doomed-proj",
			"content":    "a note in the project about to be deleted",
		}))
		dry := call(t, cs, "ghost_project_delete", map[string]any{"project": "doomed-proj"})
		mustContain(t, "project_delete (dry run)", dry, "doomed-proj")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, victim); n != 1 {
			t.Fatalf("a dry-run project delete removed a memory")
		}
		applied := call(t, cs, "ghost_project_delete", map[string]any{
			"project": "doomed-proj",
			"apply":   true,
		})
		mustContain(t, "project_delete (apply)", applied, "doomed-proj")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, victim); n != 0 {
			t.Fatalf("memory %s survived the project delete", victim)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM projects WHERE id = ?`, "doomed-proj"); n != 0 {
			t.Fatalf("the project row survived its own delete")
		}
	},

	"ghost_resolve": func(t *testing.T, s *sandbox, cs *mcp.ClientSession, _ string) {
		// A note shaped like resolved evidence: an intermediate finding with no
		// open action, which is exactly what the classifier is asked to bury.
		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the port 2222 experiment was abandoned after the relay change shipped",
			"category":   "fact",
		}))
		s.setHarnessAnswer("resolve", "RESOLVED | closed-by: the experiment was abandoned after the port change landed")

		argv, prompts := s.harnessLog("claude")
		if strings.Contains(argv, "ARGV run") {
			t.Fatalf("the claude harness was asked to run before resolve was called: %s", argv)
		}
		dry := call(t, cs, "ghost_resolve", map[string]any{"project": e2eProject})
		mustContain(t, "resolve (dry run)", dry, "would resolve")
		argv, prompts = s.harnessLog("claude")
		if !strings.Contains(argv, "ARGV -p") {
			t.Fatalf("resolve did not spawn the calling harness: %s", argv)
		}
		// The rubric travels on argv for this backend (--system-prompt) and the
		// note body on stdin, so the contract has to be looked for in both.
		if !strings.Contains(argv, "closed-by:") {
			t.Fatalf("the resolve rubric was not sent on argv: %s", argv)
		}
		if !strings.Contains(prompts, "was abandoned after") {
			t.Fatalf("the note body was not sent on stdin: %s", prompts)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id); n != 0 {
			t.Fatalf("a dry-run resolve stamped resolved_at")
		}

		applied := call(t, cs, "ghost_resolve", map[string]any{
			"project": e2eProject,
			"apply":   true,
		})
		mustContain(t, "resolve (apply)", applied, "resolved")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ? AND resolved_at IS NOT NULL`, id); n != 1 {
			t.Fatalf("resolve --apply did not stamp resolved_at on %s", id)
		}
		// A resolved memory must stop being injected, and stay searchable.
		gone := call(t, cs, "ghost_project_context", map[string]any{"project_id": e2eProject})
		mustNotContain(t, "project_context after resolve", gone, "was abandoned after")
		still := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "port 2222 experiment",
		})
		mustContain(t, "search after resolve", still, "was abandoned after")
	},
}

// taskIDFrom pulls a task id out of a ghost_task_create answer. It shares
// parseID's shape check because the answer leads with the id; the difference is
// that a task id is the only hex in the line.
func taskIDFrom(t *testing.T, text string) string {
	t.Helper()
	for _, f := range strings.Fields(text) {
		c := strings.Trim(f, "\"'`,.;:[]()")
		if looksLikeID(c) {
			return c
		}
	}
	t.Fatalf("no task id in %q", text)
	return ""
}

// TestMCPSurface drives the whole advertised MCP surface of the built binary:
// every tool, every resource and template, and both prompts.
//
// The tool coverage is enforced in both directions against the server's own
// tools/list, so a tool added on the product side without a check here fails
// this test rather than quietly going untested forever.
func TestMCPSurface(t *testing.T) {
	s := newSandbox(t)
	cs := s.mcpSession(t)
	// The surface checks below read this session, so it has to hold the state
	// they assert about — a resource that answers from an empty project would
	// prove nothing about the resource.
	seeded := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
		"project_id": e2eProject,
		"content":    "the relay listens on port 2222 in staging",
		"category":   "architecture",
	}))
	_ = seeded

	listed := listToolNames(t, cs)
	if len(listed) == 0 {
		t.Fatal("the server advertised no tools")
	}
	for _, name := range listed {
		if _, ok := toolChecks[name]; !ok {
			t.Errorf("the server advertises %q and this suite has no subtest for it — add one to toolChecks", name)
		}
	}
	for _, name := range sortedKeys(toolChecks) {
		if !containsString(listed, name) {
			t.Errorf("toolChecks drives %q, which the server does not advertise", name)
		}
	}

	for _, name := range listed {
		t.Run("tool/"+name, func(t *testing.T) {
			// A fresh sandbox per tool: a check that inherited another's
			// memories would be asserting about the wrong store.
			ts := newSandbox(t)
			tcs := ts.mcpSession(t)
			seeded := parseID(t, call(t, tcs, "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "the relay listens on port 2222 in staging",
				"category":   "architecture",
			}))
			toolChecks[name](t, ts, tcs, seeded)
		})
	}

	t.Run("resolve routes through the calling client's harness", func(t *testing.T) {
		// The routing rule is that an agent's resolve call is billed to the
		// harness it is already running inside, so each client name must reach
		// its own binary and no other. Asserted by giving each client a
		// different answer and checking which fake was asked — the strongest
		// available statement, because a test that only checked the outcome
		// could not tell one harness's answer from another's.
		for _, tc := range []struct {
			client string
			fake   string
		}{
			{"claude-code", "claude"},
			{"opencode", "opencode"},
			{"codex", "codex"},
			{"goose", "goose"},
		} {
			t.Run(tc.client, func(t *testing.T) {
				s := newSandbox(t)
				call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
					"project_id": e2eProject,
					"content":    "the failed migration was reverted on the staging relay",
				})
				s.setHarnessAnswer("resolve", "RESOLVED | closed-by: the migration was reverted upstream")

				ctx, cancel := contextWithTimeout(t)
				defer cancel()
				client := mcp.NewClient(&mcp.Implementation{Name: tc.client, Version: "e2e"}, nil)
				cs, err := client.Connect(ctx, commandTransport(t, s), nil)
				if err != nil {
					t.Fatalf("connect as %s: %v", tc.client, err)
				}
				defer cs.Close() //nolint:errcheck

				out := call(t, cs, "ghost_resolve", map[string]any{
					"project": e2eProject,
					"apply":   true,
				})
				mustContain(t, "resolve as "+tc.client, out, "resolved")

				argv, _ := s.harnessLog(tc.fake)
				if !strings.Contains(argv, "ARGV ") {
					t.Fatalf("client %q did not spawn its own harness (%s); argv log: %q",
						tc.client, tc.fake, argv)
				}
				// No other fake may have been asked. This is the half that makes
				// the test about routing rather than about working binaries.
				for _, other := range harnessBinaries {
					if other == tc.fake {
						continue
					}
					if argv, _ := s.harnessLog(other); strings.Contains(argv, "ARGV ") {
						t.Fatalf("client %q spawned %s as well: %q", tc.client, other, argv)
					}
				}
				// And the answer has to have reached the store, or the routing
				// proved nothing.
				if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE resolved_at IS NOT NULL`); n == 0 {
					t.Fatalf("client %q resolved nothing", tc.client)
				}
			})
		}
	})

	t.Run("resources", func(t *testing.T) {
		ctx, cancel := contextWithTimeout(t)
		defer cancel()

		res, err := cs.ListResources(ctx, nil)
		if err != nil {
			t.Fatalf("resources/list: %v", err)
		}
		var globalURI string
		for _, r := range res.Resources {
			if strings.HasPrefix(r.URI, "ghost://memories/") {
				globalURI = r.URI
			}
		}
		if globalURI == "" {
			t.Fatalf("resources/list carries no global-memories resource: %+v", res.Resources)
		}
		read, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: globalURI})
		if err != nil {
			t.Fatalf("resources/read %s: %v", globalURI, err)
		}
		text := resourceText(read)
		if !strings.Contains(text, "Global") {
			t.Fatalf("the global-memories resource is empty or unlabelled: %q", text)
		}

		tmpl, err := cs.ListResourceTemplates(ctx, nil)
		if err != nil {
			t.Fatalf("resources/templates/list: %v", err)
		}
		wantTemplates := map[string]string{
			"ghost://project/{project_id}/context":   "2222",
			"ghost://project/{project_id}/decisions": "",
			"ghost://project/{project_id}/tasks":     "",
		}
		for _, want := range sortedKeys(wantTemplates) {
			if !templateAdvertised(t, tmpl.ResourceTemplates, want) {
				t.Errorf("resources/templates/list does not advertise %q", want)
			}
		}
		// The context template has to carry a real memory, so the seeded save
		// has to be visible through the resource surface too.
		got, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{
			URI: "ghost://project/" + e2eProject + "/context",
		})
		if err != nil {
			t.Fatalf("resources/read project context: %v", err)
		}
		mustContain(t, "project context resource", resourceText(got), "2222")

		// The other two templates must answer rather than error, even with
		// nothing in them: an empty project is a real state.
		for _, uri := range []string{
			"ghost://project/" + e2eProject + "/decisions",
			"ghost://project/" + e2eProject + "/tasks",
		} {
			if _, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri}); err != nil {
				t.Errorf("resources/read %s: %v", uri, err)
			}
		}
	})

	t.Run("prompts", func(t *testing.T) {
		ctx, cancel := contextWithTimeout(t)
		defer cancel()
		list, err := cs.ListPrompts(ctx, nil)
		if err != nil {
			t.Fatalf("prompts/list: %v", err)
		}
		advertised := map[string]bool{}
		for _, p := range list.Prompts {
			advertised[p.Name] = true
		}
		for _, want := range []string{"recall_project", "record_decision"} {
			if !advertised[want] {
				t.Errorf("prompts/list does not advertise %q (got %v)", want, sortedKeys(advertised))
			}
		}
		if len(advertised) != 2 {
			t.Errorf("prompts/list advertises %d prompts, want exactly 2: %v", len(advertised), sortedKeys(advertised))
		}

		recall, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{
			Name:      "recall_project",
			Arguments: map[string]string{"project_id": e2eProject},
		})
		if err != nil {
			t.Fatalf("prompts/get recall_project: %v", err)
		}
		rt := promptText(recall)
		mustContain(t, "recall_project", rt, "2222")

		rec, err := cs.GetPrompt(ctx, &mcp.GetPromptParams{
			Name: "record_decision",
			Arguments: map[string]string{
				"project_id": e2eProject,
				"topic":      "choosing the storage engine",
			},
		})
		if err != nil {
			t.Fatalf("prompts/get record_decision: %v", err)
		}
		dt := promptText(rec)
		mustContain(t, "record_decision", dt, "choosing the storage engine")
	})
}

// TestMCPLifecycles covers the multi-step flows the per-tool checks cannot:
// a save that is searched, updated and then read back out of its history.
func TestMCPLifecycles(t *testing.T) {
	t.Run("save search update and history", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)

		id := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the relay listens on port 2222 in staging",
			"category":   "architecture",
			"importance": 0.5,
		}))

		found := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "relay port",
		})
		mustContain(t, "search after save", found, "2222")

		call(t, cs, "ghost_memory_update", map[string]any{
			"project_id": e2eProject,
			"memory_id":  id,
			"content":    "the relay listens on port 2222 in production",
			"importance": 0.9,
		})

		// Both versions have to be in the history, and the current one has to be
		// what a read returns. A history that only kept the latest write would
		// pass the second assertion and lose the first.
		hist := s.mustRun("history", id, "--json")
		if !strings.Contains(hist.stdout, "staging") {
			t.Fatalf("history lost the first version: %s", hist)
		}
		mustContain(t, "history", hist.stdout, "production")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, id); n < 2 {
			t.Fatalf("memory_history holds %d entries for %s, want at least 2", n, id)
		}
		cur := s.queryRow(t, `SELECT content, importance FROM memories WHERE id = ?`, id)
		if !strings.Contains(cur.text, "production") {
			t.Fatalf("the stored memory is not the updated one: %q", cur.text)
		}
		if got := fmt.Sprintf("%.1f", cur.num); got != "0.9" {
			t.Fatalf("stored importance = %s, want 0.9", got)
		}

		// The human-readable form has to agree with the JSON one.
		human := s.mustRun("history", id)
		mustContain(t, "history (human)", human.stdout, "production")
	})

	t.Run("delete with and without purge_history", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)

		kept := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note whose recorded history is meant to survive",
		}))
		purged := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note that is being redacted for good",
		}))

		call(t, cs, "ghost_memory_delete", map[string]any{
			"project_id": e2eProject,
			"memory_id":  kept,
		})
		call(t, cs, "ghost_memory_delete", map[string]any{
			"project_id":    e2eProject,
			"memory_id":     purged,
			"purge_history": true,
		})

		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, kept); n == 0 {
			t.Fatalf("a plain delete erased the recorded history of %s", kept)
		}
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memory_history WHERE memory_id = ?`, purged); n != 0 {
			t.Fatalf("purge_history left %d history rows for %s", n, purged)
		}
		for _, id := range []string{kept, purged} {
			if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE id = ?`, id); n != 0 {
				t.Fatalf("memory %s survived its delete", id)
			}
		}
		// The redaction is only complete if the text is gone from the history
		// the CLI can still read, so the CLI's own view is the assertion.
		still := s.mustRun("history", purged)
		mustNotContain(t, "history after purge", still.stdout, "redacted for good")
	})

	t.Run("scope on save and on search", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)

		prod := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout is 30s in production",
			"category":   "architecture",
			"scope":      map[string]any{"environment": "production"},
		}))
		dev := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout is 5s on the dev pool",
			"category":   "architecture",
			"scope":      map[string]any{"environment": "development"},
		}))
		unscoped := parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout knob is set in the config file",
			"category":   "convention",
		}))

		// A scope that matches nothing must not become "return everything".
		prodOnly := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "pool timeout",
			"scope":      map[string]any{"environment": "production"},
		})
		mustContain(t, "scoped search (production)", prodOnly, "30s in production")
		mustNotContain(t, "scoped search (production)", prodOnly, "5s on the dev pool")
		// A memory that says nothing about the key still applies — that is the
		// documented rule, and it is the half a naive AND-filter gets wrong.
		mustContain(t, "scoped search (unscoped row survives)", prodOnly, "config file")

		devOnly := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "pool timeout",
			"scope":      map[string]any{"environment": "development"},
		})
		mustContain(t, "scoped search (development)", devOnly, "5s on the dev pool")
		mustNotContain(t, "scoped search (development)", devOnly, "30s in production")

		// A conflicting scope is a contradiction, and the search has to say so
		// rather than quietly drop the row.
		conflict := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "pool timeout",
			"scope":      map[string]any{"environment": "staging"},
		})
		mustNotContain(t, "scoped search (staging)", conflict, "30s in production")
		mustNotContain(t, "scoped search (staging)", conflict, "5s on the dev pool")

		// A scope naming a value nobody has excludes every CONTRADICTING row
		// and keeps every row that says nothing about the key. That is the
		// documented rule — a filter, not a gate — so the assertion is that the
		// two scoped memories go and the unscoped one stays, and that the
		// answer says the filter ran on a finite window rather than implying the
		// store holds nothing else.
		absent := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "pool timeout",
			"scope":      map[string]any{"environment": "nowhere"},
		})
		mustNotContain(t, "scoped search (nowhere)", absent, "30s in production")
		mustNotContain(t, "scoped search (nowhere)", absent, "5s on the dev pool")
		mustContain(t, "scoped search (unscoped row survives an unmatched scope)", absent, "config file")

		// A scope that contradicts EVERY candidate in the project is a genuine
		// absence, and it has to read as one.
		onlyScoped := newSandbox(t)
		ocs := onlyScoped.mcpSession(t)
		call(t, ocs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the pool timeout is 30s in production",
			"scope":      map[string]any{"environment": "production"},
		})
		empty := call(t, ocs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "pool timeout",
			"scope":      map[string]any{"environment": "staging"},
		})
		// The scope filter is what emptied this one, so the answer has to say so
		// and hand back the knob that would widen it — and it must not read as
		// "the store has nothing", because the vector leg cannot vouch for rows
		// it never saw (#580).
		mustNotContain(t, "scoped search (staging, only a production row)", empty, "30s in production")
		mustNotContain(t, "scoped search (staging, only a production row)", empty, "No matching memories found")
		mustContain(t, "scoped search (staging, only a production row)", empty, "drop the scope filter")

		// And the scopes are on disk as asked, not just in the answer.
		for _, tc := range []struct{ id, want string }{{prod, "production"}, {dev, "development"}} {
			got := s.queryStrings(t, `SELECT scope FROM memories WHERE id = ?`, tc.id)
			if len(got) != 1 || !strings.Contains(got[0], tc.want) {
				t.Fatalf("memory %s scope = %v, want it to name %q", tc.id, got, tc.want)
			}
		}
		if got := s.queryStrings(t, `SELECT COALESCE(scope, '') FROM memories WHERE id = ?`, unscoped); len(got) != 1 || got[0] != "" {
			t.Fatalf("the unscoped save has scope %v, want none", got)
		}
	})

	t.Run("explain returns the ranking diagnosis", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the relay listens on port 2222 in staging",
			"category":   "architecture",
		})
		out := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "relay port",
			"explain":    true,
		})
		var ex struct {
			ProjectID       string `json:"project_id"`
			Query           string `json:"query"`
			VectorAvailable bool   `json:"vector_available"`
			Notes           []string
			Rows            []struct {
				ID           string   `json:"memory_id"`
				Included     bool     `json:"included"`
				Rank         int      `json:"rank"`
				FTSRank      int      `json:"fts_rank"`
				VectorRank   int      `json:"vector_rank"`
				VectorScore  float64  `json:"vector_score"`
				RRFScore     float64  `json:"rrf_score"`
				ProjectMatch bool     `json:"project_match"`
				RowProject   string   `json:"row_project"`
				ScopeMatched bool     `json:"scope_matched"`
				ScopeKeys    []string `json:"scope_keys_compared"`
				Validity     string   `json:"validity_state"`
				ConfWeight   string   `json:"provenance_weight"`
			} `json:"rows"`
		}
		if err := json.Unmarshal([]byte(out), &ex); err != nil {
			t.Fatalf("explain did not return JSON: %v\n%s", err, out)
		}
		if ex.ProjectID != e2eProject {
			t.Fatalf("explain project_id = %q, want %q", ex.ProjectID, e2eProject)
		}
		if !ex.VectorAvailable {
			t.Fatalf("explain reports the vector leg as unavailable, but the sandbox wires an embedder")
		}
		// Every leg's contribution has to be reported, not only the winner's:
		// the whole reason to ask for explain is to see which signal decided.
		// fts_rank is 0 for a row the FTS leg found first (its base score, not
		// an absent signal), and -1 for a leg that did not match it at all, so
		// the assertion is on the rank and the fused score being present.
		var found bool
		for _, r := range ex.Rows {
			if r.Included && r.Rank > 0 && r.RRFScore > 0 && r.FTSRank >= 0 {
				found = true
			}
			// The axes an agent reads to decide WHICH signal to trust have to be
			// populated on every row, including the sentinels. Two invariants, and
			// only two: a row in the answer passed scope narrowing, so it is
			// scope-matched; and a row in the answer belongs either to the searched
			// project or to _global, which the legs admit by the shared-row
			// predicate and status_factor then demotes — so project_match=false
			// with row_project="_global" is a legitimate admitted row, NOT a
			// contradiction.
			if r.Included && !r.ScopeMatched {
				t.Fatalf("row %s is in the answer but reports scope_matched=false, which is the "+
					"verdict that decided membership", r.ID)
			}
			if r.Included && !r.ProjectMatch && r.RowProject != "_global" {
				t.Fatalf("row %s is in the answer but reports project_match=false for row_project=%q, "+
					"which is neither the searched project nor the shared-row predicate",
					r.ID, r.RowProject)
			}
			if r.RowProject == "" {
				t.Fatalf("row %s carries no row_project, so project_match=%v cannot be checked",
					r.ID, r.ProjectMatch)
			}
			if r.Validity == "" {
				t.Fatalf("row %s carries no validity_state", r.ID)
			}
			if r.ConfWeight != "off" {
				t.Fatalf("row %s reports provenance_weight %q, want \"off\": nothing in the search "+
					"ranking multiplies by provenance, and any number would be an invented weight",
					r.ID, r.ConfWeight)
			}
		}
		if !found {
			t.Fatalf("explain returned no included row with a rank and a score: %s", out)
		}
		// explain and the formatted answer must describe the same window, so
		// the memory the explain names has to be the one the listing shows.
		listed := call(t, cs, "ghost_memory_search", map[string]any{
			"project_id": e2eProject,
			"query":      "relay port",
		})
		for _, r := range ex.Rows {
			if r.Included && !strings.Contains(listed, r.ID) {
				t.Fatalf("explain includes %s but the formatted answer omits it", r.ID)
			}
		}
	})

	t.Run("a credential-shaped save is refused", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		const key = "AKIAIOSFODNN7EXAMPLE"
		out := callExpectingError(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the staging access key id is " + key + " and it is in the vault",
		})
		mustContain(t, "credential refusal", out, "credential")
		// The refusal must name the value as unprintable too: the message
		// reaches a harness prompt, so quoting it back would relocate it.
		mustNotContain(t, "credential refusal", out, key)
		if n := s.queryInt(t, `SELECT COUNT(*) FROM memories WHERE content LIKE '%' || ? || '%'`, key); n != 0 {
			t.Fatalf("the refused content reached the store")
		}
		// The refusal must be the guard, not a validation error about some
		// other field: the guard names the field it refused.
		mustContain(t, "credential refusal", out, "content")
	})

	t.Run("the decision tool refuses a credential in a nested field", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		// The save is what puts the project on record, and it is kept for a
		// reason that outlives #956: the tool now opens a project it has never
		// heard of, so without this the refusal under test would be about a
		// project that did not exist yet rather than about the credential, and
		// the assertions below would be reading an answer about something else.
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a note that creates the project the decision is aimed at",
		})
		const key = "AKIAIOSFODNN7EXAMPLE"
		out := callExpectingError(t, cs, "ghost_decision_record", map[string]any{
			"project_id":   e2eProject,
			"title":        "Store the relay credential",
			"decision":     "keep the key in the vault",
			"rationale":    "the operator needs it to cut over",
			"alternatives": []string{"paste the key " + key + " into the runbook"},
		})
		mustNotContain(t, "decision refusal", out, key)
		mustContain(t, "decision refusal", out, "alternatives[0]")
		if n := s.queryInt(t, `SELECT COUNT(*) FROM decisions WHERE title = ?`, "Store the relay credential"); n != 0 {
			t.Fatalf("a decision with a refused alternative was stored anyway")
		}
	})
}
