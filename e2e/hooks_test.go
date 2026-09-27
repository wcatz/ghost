//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// hookHosts is every source the contract knows. The payload differs per host
// where the hosts genuinely differ (goose sends its own field names), and the
// capability matrix means the same event produces different output per host —
// so the loop is over the whole set rather than one representative.
var hookHosts = []string{"claude-code", "opencode", "codex", "goose"}

// hookEvent is one fixture payload for one host and event.
type hookEvent struct {
	host  string
	event string
	// payload is the JSON a real host sends. Fields the contract requires are
	// present; the host-specific extras are included where the host sends them,
	// because tolerating them is part of the contract.
	payload string
	// injectsContext is whether this host honors an injected context block.
	// goose and opencode have no injection surface, so the hook must produce no
	// output for them.
	injectsContext bool
	// blocksStop is whether the host can render a Stop decision. Not used as an
	// assertion on the block protocol, because the reminder is emitted as a
	// non-blocking approve for every host on purpose.
	blocksStop bool
}

// sessionStartPayload is the SessionStart fixture for a host. Every host sends
// hook_event_name, session_id and cwd; the source field is the session-start
// REASON for the hosts that use it (startup/resume/clear/compact), which is a
// different meaning from the envelope's source, and that overloading is exactly
// why the envelope is nested.
func sessionStartPayload(host, cwd string) string {
	switch host {
	case "goose":
		// goose follows the Open Plugins hooks spec and sends `event` and
		// `working_dir` instead of the dialect's names. The hook must accept
		// those natively, with no shim.
		return fmt.Sprintf(`{"event":"SessionStart","working_dir":%q,"session_id":"e2e-%s"}`, cwd, host)
	case "opencode":
		return fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e-%s","cwd":%q,"source":"startup"}`, host, cwd)
	case "codex":
		return fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e-%s","cwd":%q,"model":"gpt-5"}`, host, cwd)
	default:
		return fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e-%s","cwd":%q,"source":"startup"}`, host, cwd)
	}
}

// stopPayload is the Stop fixture for a host, with a transcript path the hook
// will scan.
//
// The envelope is left to be completed from argv wherever the product has a
// native format for the source, because that is what a real host's payload looks
// like and it keeps the test from asserting its own belief about the default.
// goose and opencode are the exceptions: neither has a native format in the v1
// contract, so their adapters materialize a transcript and name the format
// explicitly. An empty format means "let the product decide".
func stopPayload(host, cwd, transcript, format string) string {
	contract := ""
	if format != "" && format != defaultFormatFor(host) {
		contract = fmt.Sprintf(`"contract":{"version":1,"source":%q,"transcript_format":%q},`, host, format)
	}
	switch host {
	case "goose":
		return fmt.Sprintf(`{%s"event":"Stop","working_dir":%q,"session_id":"e2e-%s","stop_hook_active":false,"transcript_path":%q}`,
			contract, cwd, host, transcript)
	default:
		return fmt.Sprintf(`{%s"hook_event_name":"Stop","session_id":"e2e-%s","cwd":%q,"stop_hook_active":false,"transcript_path":%q}`,
			contract, host, cwd, transcript)
	}
}

// defaultFormatFor is the transcript format the contract completes for a
// source. It is the product's table, restated here so stopPayload can tell
// "let the product decide" from "name this one explicitly"; a host added to
// internal/hostevent's defaultFormatFor without a row here would have its
// fixture written in a format the contract never asks for, and the nudge would
// stop firing for it.
func defaultFormatFor(host string) string {
	switch host {
	case "claude-code":
		return "claude-jsonl"
	case "codex":
		return "codex-rollout"
	}
	return "none"
}

// sessionEndPayload is the SessionEnd fixture. It never carries a transcript
// path, because the host fires it once the session is over.
func sessionEndPayload(host, cwd string) string {
	if host == "goose" {
		return fmt.Sprintf(`{"event":"SessionEnd","working_dir":%q,"session_id":"e2e-%s"}`, cwd, host)
	}
	return fmt.Sprintf(`{"hook_event_name":"SessionEnd","session_id":"e2e-%s","cwd":%q}`, host, cwd)
}

// TestHookSessionStartInjectsContext drives the SessionStart event for every
// host, and asserts the injected block is the product's own: the project's
// memories, quoted as data, with the scope labels it carries.
func TestHookSessionStartInjectsContext(t *testing.T) {
	for _, host := range hookHosts {
		t.Run("host/"+host, func(t *testing.T) {
			s := newSandbox(t)
			cs := s.mcpSession(t)
			// Two memories, one scoped and one not, so the assertion can tell a
			// block that carries the scope label from one that ignores it.
			parseID(t, call(t, cs, "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "the pool timeout is 30s in production",
				"category":   "architecture",
				"scope":      map[string]any{"environment": "production"},
			}))
			call(t, cs, "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "the pool timeout knob is set in the config file",
				"category":   "convention",
			})
			// A task and a decision, so the block is shown to carry them too.
			call(t, cs, "ghost_task_create", map[string]any{
				"project_id": e2eProject,
				"title":      "measure the relay port change",
			})
			call(t, cs, "ghost_decision_record", map[string]any{
				"project_id": e2eProject,
				"title":      "Use a single writer connection",
				"decision":   "the store pins MaxOpenConns to one",
				"rationale":  "a pool query inside an open transaction deadlocks",
			})
			// Bind the checkout, or the directory resolves to no project and the
			// block would be empty for a reason that has nothing to do with the
			// injection.
			s.mustRun("project", "bind", e2eProject, s.work)

			payload := sessionStartPayload(host, s.work)
			r := s.mustRunStdin(payload, "hook", "session-start", "--source", host)
			if r.code != 0 {
				t.Fatalf("hook session-start --source %s: %s", host, r)
			}

			// goose and opencode have no context-injection surface, so the
			// documented behaviour is silence. Anything they printed would be
			// output their host cannot consume, which is worse than nothing.
			injects := host != "goose" && host != "opencode"
			if !injects {
				if strings.TrimSpace(r.stdout) != "" {
					t.Fatalf("hook session-start --source %s printed output to a host with no injection surface:\n%s", host, r.stdout)
				}
				return
			}

			mustContain(t, "session-start (host "+host+")", r.stdout, "Ghost context: "+e2eProject)
			mustContain(t, "session-start (host "+host+")", r.stdout, `Use project_id: "`+e2eProject+`"`)
			mustContain(t, "session-start (host "+host+")", r.stdout, "30s in production")
			mustContain(t, "session-start (host "+host+")", r.stdout, "config file")
			mustContain(t, "session-start (host "+host+")", r.stdout, "measure the relay port change")
			mustContain(t, "session-start (host "+host+")", r.stdout, "Use a single writer connection")
			// The scope label is the same one every other surface prints, so a
			// row that carries scope has to say so here: this is the one surface
			// where an agent would otherwise not see the axis at all. The
			// assertion is positional — label and content on the SAME line — so
			// a block that mentioned the value somewhere else would not pass.
			mustMatch(t, "the scope label is on the row it describes (host "+host+")", r.stdout,
				`(?m)^- \[architecture\] scope\{environment=production\} «the pool timeout is 30s in production»$`)
			// The unscoped row carries no label at all, which is what keeps the
			// block byte-identical for a store written before the column existed.
			mustMatch(t, "an unscoped row carries no scope label (host "+host+")", r.stdout,
				`(?m)^- \[convention\] «the pool timeout knob`)
			// Stored text is quoted as data. The delimiter is the product's, and
			// its presence is what keeps the block from reading as instructions.
			mustContain(t, "session-start quotes stored data", r.stdout, "«")
			// The session counter moved: this fire was counted as a session.
			mustMatch(t, "session-start counts the session (host "+host+")", r.stdout, `\*\*Session #\d+\*\*`)
		})
	}

	t.Run("a resume and a compact do not re-inject", func(t *testing.T) {
		// The original injection is already in the transcript, and a compaction
		// may not have carried it verbatim — re-emitting is pure waste either
		// way, and on compaction the product points at the tool instead.
		s := newSandbox(t)
		call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory the resume must not re-inject",
		})
		s.mustRun("project", "bind", e2eProject, s.work)

		resume := s.mustRunStdin(
			fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e-resume","cwd":%q,"source":"resume"}`, s.work),
			"hook", "session-start", "--source", "claude-code")
		if strings.TrimSpace(resume.stdout) != "" {
			t.Fatalf("a resume re-injected the context block:\n%s", resume.stdout)
		}

		compact := s.mustRunStdin(
			fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e-compact","cwd":%q,"source":"compact"}`, s.work),
			"hook", "session-start", "--source", "claude-code")
		mustContain(t, "compact", compact.stdout, "ghost_project_context")
		mustNotContain(t, "compact", compact.stdout, "a memory the resume must not re-inject")
	})

	t.Run("a subagent session gets no block", func(t *testing.T) {
		// A subagent already receives its working context in-band from the
		// parent's prompt; a second independent dump is token cost for nothing.
		s := newSandbox(t)
		call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory the subagent must not be handed",
		})
		s.mustRun("project", "bind", e2eProject, s.work)
		sub := s.mustRunStdin(
			fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":"e2e-sub","cwd":%q,"source":"startup","agent_id":"researcher","agent_type":"Explore"}`, s.work),
			"hook", "session-start", "--source", "claude-code")
		if strings.TrimSpace(sub.stdout) != "" {
			t.Fatalf("a subagent session was given the context block:\n%s", sub.stdout)
		}
	})
}

// transcriptFor writes a transcript in the format the given host's adapter
// declares, containing tool calls and — when withGhostSave is set — one Ghost
// save among them. opencode and goose have no transcript format in the v1
// contract (their adapters materialize their own, so the default format is
// "none"), so they get the opencode-messages format spelled out in the payload's
// contract, which is the only way to reach their scanner.
//
// The formats are the ones internal/hostevent registers; each is written to look
// like what the real host writes, because a scanner that reads a file no host
// produces would be a scanner nothing exercises.
func transcriptFor(t *testing.T, host string, withGhostSave bool) (path, format string) {
	t.Helper()
	format = "claude-jsonl"
	var lines []string

	switch host {
	case "codex":
		// codex's rollout: timestamped envelopes, model traffic under
		// response_item, and codex flattens the namespace onto the name.
		format = "codex-rollout"
		call := `{"type":"response_item","payload":{"type":"function_call","name":"shell","namespace":"functions"}}`
		if withGhostSave {
			call = `{"type":"response_item","payload":{"type":"function_call","name":"ghost_memory_save","namespace":"mcp__ghost"}}`
		}
		lines = []string{
			`{"type":"session_meta","payload":{}}`,
			call,
			`{"type":"response_item","payload":{"type":"message","role":"assistant"}}`,
		}
	case "goose", "opencode":
		format = "opencode-messages"
		tool := "bash"
		if withGhostSave {
			tool = "ghost_memory_save"
		}
		lines = []string{
			`{"info":{"role":"user"},"parts":[{"type":"text","text":"run the build"}]}`,
			fmt.Sprintf(`{"info":{"role":"assistant"},"parts":[{"type":"tool","tool":%q}]}`, tool),
			`{"info":{"role":"assistant"},"parts":[{"type":"text","text":"done"}]}`,
		}
	default:
		// The dialect Claude Code writes: JSONL whose assistant lines carry
		// tool_use blocks.
		tool := "Bash"
		if withGhostSave {
			// The server-qualified spelling, because that is what a real session
			// records and the scanner has to recognise a save under any host's
			// qualification convention.
			tool = "mcp__ghost__ghost_memory_save"
		}
		lines = []string{
			`{"type":"user","message":{"content":[{"type":"text","text":"run the build"}]}}`,
			fmt.Sprintf(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":%q}]}}`, tool),
			`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		}
	}

	path = filepath.Join(t.TempDir(), host+"-transcript.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write %s transcript: %v", host, err)
	}
	return path, format
}

// TestHookStop drives the Stop event: the save-nudge, and the lifecycle spawn
// behind its lock and its min_interval cooldown.
func TestHookStop(t *testing.T) {
	for _, host := range hookHosts {
		t.Run("nudge/host/"+host, func(t *testing.T) {
			s := newSandbox(t)
			call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "a memory so the project exists for the stop hook",
			})
			s.mustRun("project", "bind", e2eProject, s.work)

			// A session that used a tool but saved nothing to Ghost: exactly the
			// case the reminder exists for.
			//
			// The transcript is written in the format THIS host's adapter
			// declares, not one format for all four. The scanner is selected by
			// the envelope's transcript_format, and each host has its own native
			// format — so a fixture in the wrong format would scan clean, count
			// zero tool calls, and the nudge would correctly not fire. Asserting
			// the nudge for a host fed another host's transcript would be
			// asserting that a scanner reads a format it was not written for.
			transcript, format := transcriptFor(t, host, false)
			r := s.mustRunStdin(stopPayload(host, s.work, transcript, format),
				"hook", "stop", "--source", host)
			mustContain(t, "stop nudge (host "+host+")", r.stdout, "ghost_memory_save")
			// It is a non-blocking approve, not a block: a host that rendered it
			// as a Stop failure would make every turn look broken. The decision
			// is JSON, so it is parsed rather than pattern-matched.
			var decision struct {
				Decision string `json:"decision"`
				Reason   string `json:"reason"`
			}
			line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(r.stdout), "\n", 2)[0])
			if err := jsonUnmarshal([]byte(line), &decision); err != nil {
				t.Fatalf("the stop reminder is not JSON: %v\n%s", err, line)
			}
			if decision.Decision != "approve" {
				t.Fatalf("the stop reminder is a %q, want a non-blocking approve: %s", decision.Decision, line)
			}
			if !strings.Contains(decision.Reason, "ghost_memory_save") {
				t.Fatalf("the reminder does not name the tool to use: %q", decision.Reason)
			}
		})
	}

	t.Run("a session that saved is not nudged", func(t *testing.T) {
		s := newSandbox(t)
		call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory the transcript records a save of",
		})
		s.mustRun("project", "bind", e2eProject, s.work)
		transcript, format := transcriptFor(t, "claude-code", true)
		r := s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
			"hook", "stop", "--source", "claude-code")
		if strings.Contains(r.stdout, "ghost_memory_save") {
			t.Fatalf("a session that saved to Ghost was still nudged:\n%s", r.stdout)
		}
	})

	t.Run("a re-stop after the reminder is silent", func(t *testing.T) {
		// stop_hook_active means the host is re-stopping immediately after our
		// own reminder; nudging again would loop.
		s := newSandbox(t)
		call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory for the re-stop case",
		})
		s.mustRun("project", "bind", e2eProject, s.work)
		transcript, format := transcriptFor(t, "claude-code", false)
		payload := fmt.Sprintf(
			`{"hook_event_name":"Stop","session_id":"e2e-restop","cwd":%q,"stop_hook_active":true,"transcript_path":%q}`,
			s.work, transcript)
		_ = format
		r := s.mustRunStdin(payload, "hook", "stop", "--source", "claude-code")
		if strings.TrimSpace(r.stdout) != "" {
			t.Fatalf("a re-stop produced output:\n%s", r.stdout)
		}
	})

	t.Run("lifecycle spawns behind its lock and min_interval", func(t *testing.T) {
		s := newSandbox(t)
		cs := s.mcpSession(t)
		// A candidate the resolve phase will actually ask about, so the spawned
		// chain does real work rather than exiting immediately.
		call(t, cs, "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "the failed migration was reverted on the staging relay",
		})
		s.mustRun("project", "bind", e2eProject, s.work)
		// Auto-resolve on, and a cooldown long enough that a second stop inside
		// the test is definitely inside the window.
		s.reconfigure(configOpts{autoResolve: true, minInterval: "30m"})

		dataDir := s.dataDir()
		stamp := filepath.Join(dataDir, "lifecycle-"+e2eProject+".last")
		lock := filepath.Join(dataDir, "lifecycle-"+e2eProject+".pid.lock")
		logPath := filepath.Join(dataDir, "lifecycle.log")
		mustNotExist(t, "the lifecycle stamp before the first stop", stamp)

		// The first stop spawns the chain. The stamp is written by the spawned
		// process itself, once it holds the lock — deliberately not by the hook,
		// so a spawn that failed to start cannot burn the window.
		transcript, format := transcriptFor(t, "claude-code", true)
		s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
			"hook", "stop", "--source", "claude-code")
		waitForFile(t, stamp, 60*time.Second, "the lifecycle start stamp")
		firstStamp := modTime(t, stamp)
		// The lock is taken and released around the run, so the pid file itself
		// is gone by the time the chain finishes. The .lock sibling is the
		// durable evidence that the claim happened: it is created once and
		// never removed, and a kernel-owned lock on it is what serialized the
		// two hooks below.
		waitForFile(t, lock, 60*time.Second, "the lifecycle pid lock")
		// And the chain really ran the phase it was configured for, which is
		// what makes the stamp a record of work rather than of a spawn.
		waitForFileContaining(t, logPath, "resolve completed", 60*time.Second, "the resolve phase to finish")

		// The second stop, inside the window, must not spawn. The evidence is
		// the log line the skip leaves behind — the hook's own stderr is the
		// host's, and one line per turn is noise nobody can act on.
		s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
			"hook", "stop", "--source", "claude-code")
		waitForFileContaining(t, logPath, "min_interval", 30*time.Second, "the cooldown skip line")
		if got := modTime(t, stamp); !got.Equal(firstStamp) {
			t.Fatalf("the second stop advanced the lifecycle stamp (%s -> %s), so it spawned inside the window",
				firstStamp.Format(time.RFC3339Nano), got.Format(time.RFC3339Nano))
		}
		// And the skip is a skip, not a second run: the phase ran once.
		if n := countOccurrences(string(mustReadFile(t, logPath)), "resolve completed"); n != 1 {
			t.Fatalf("the lifecycle log records %d completed resolve phases, want 1 — the cooldown did not hold", n)
		}

		// A zero interval is the documented opt-out, and it has to actually
		// re-enable the spawn: an opt-out that did not work would leave the
		// cooldown permanently on, which is the failure a user cannot see.
		//
		// The wait for the pid lock to clear is load-bearing. The two guards are
		// separate — a running chain blocks the spawn regardless of the
		// interval, and only the cooldown is what min_interval controls — so
		// firing the stop while the previous chain is still winding down would
		// be blocked by the lock and would prove nothing about the interval.
		waitForFileGone(t, filepath.Join(dataDir, "lifecycle-"+e2eProject+".pid"),
			60*time.Second, "the first lifecycle chain to release its lock")
		s.reconfigure(configOpts{autoResolve: true, minInterval: "0"})
		s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
			"hook", "stop", "--source", "claude-code")
		// Counted, not waited-for: the marker is already in the log from the
		// first run, so a "wait until the log contains it" would return
		// immediately and assert nothing.
		waitForCount(t, logPath, "resolve completed", 2, 60*time.Second,
			"the phase to run again with the cooldown off")
		if n := countOccurrences(string(mustReadFile(t, logPath)), "resolve completed"); n != 2 {
			t.Fatalf("the lifecycle log records %d completed resolve phases, want 2 — min_interval=0 did not re-enable the spawn", n)
		}
	})

	t.Run("auto-consolidation off means no spawn", func(t *testing.T) {
		// The default is off. A stop hook that spawned a chain nobody asked for
		// would spend a subscription call per turn.
		s := newSandbox(t)
		call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
			"project_id": e2eProject,
			"content":    "a memory for the no-spawn case",
		})
		s.mustRun("project", "bind", e2eProject, s.work)
		transcript, format := transcriptFor(t, "claude-code", true)
		s.mustRunStdin(stopPayload("claude-code", s.work, transcript, format),
			"hook", "stop", "--source", "claude-code")
		mustNotExist(t, "the lifecycle stamp with auto-consolidation off",
			filepath.Join(s.dataDir(), "lifecycle-"+e2eProject+".last"))
	})
}

// TestHookFailOpen covers the fail-open contract: every malformed input must
// still let the host proceed, with one diagnostic line on stderr and nothing on
// stdout.
func TestHookFailOpen(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		// wantStderr is a pattern the single diagnostic line must match. Empty
		// means the assertion is only on the exit code and the silence.
		wantStderr string
	}{
		{
			name:       "oversized payload",
			payload:    oversizedPayload(),
			wantStderr: "(?i)fail-open|payload|ceiling|size",
		},
		{
			name:       "malformed JSON",
			payload:    `{"hook_event_name":"Stop","session_id":`,
			wantStderr: "(?i)fail-open|parse",
		},
		{
			name:       "not JSON at all",
			payload:    "this is not json",
			wantStderr: "(?i)fail-open|parse",
		},
		{
			name:       "empty payload",
			payload:    "",
			wantStderr: "(?i)fail-open|parse|contract",
		},
		{
			name:       "unknown event name in the payload",
			payload:    `{"hook_event_name":"TeaTime","session_id":"x","cwd":"/tmp"}`,
			wantStderr: "(?i)fail-open|not a v1 event|event",
		},
		{
			name:       "an explicit but invalid contract",
			payload:    `{"contract":{},"hook_event_name":"Stop","session_id":"x"}`,
			wantStderr: "(?i)fail-open|contract",
		},
		{
			name:       "a contract version this build does not speak",
			payload:    `{"contract":{"version":99,"source":"claude-code"},"hook_event_name":"Stop","session_id":"x"}`,
			wantStderr: "(?i)fail-open|version",
		},
		{
			name:       "a contract that disagrees with argv",
			payload:    `{"contract":{"version":1,"source":"codex"},"hook_event_name":"Stop","session_id":"x"}`,
			wantStderr: "(?i)fail-open|disagree",
		},
		{
			name:       "a payload event that disagrees with argv",
			payload:    `{"hook_event_name":"SessionStart","session_id":"x","cwd":"/tmp"}`,
			wantStderr: "(?i)fail-open|disagree",
		},
		{
			name:       "an unknown transcript format",
			payload:    `{"contract":{"version":1,"source":"claude-code","transcript_format":"telepathy"},"hook_event_name":"Stop","session_id":"x"}`,
			wantStderr: "(?i)fail-open|transcript_format",
		},
		{
			// A raw invalid byte is NOT what refuses this payload:
			// encoding/json decodes it to U+FFFD, so the parse succeeds. What
			// refuses it is the EVENT NAME, because argv decides the event and a
			// name holding a replacement character is not a v1 event. Putting the
			// byte in the session_id instead would produce a payload the hook
			// accepts, which is the documented behaviour and not this case.
			name:       "invalid UTF-8 in the event name",
			payload:    "{\"hook_event_name\":\"St" + string([]byte{0xff}) + "op\",\"session_id\":\"x\",\"cwd\":\"/tmp\"}",
			wantStderr: "(?i)fail-open|not a v1 event|event",
		},
	}

	// The table is a guard as well as a set of cases, and it needs its own
	// guard. Every row is supposed to be a payload the contract REFUSES, and a
	// fixture that quietly became well-formed would then assert that a VALID
	// payload produces a diagnostic — so the row would fail for the wrong reason,
	// and worse, a hook that started accepting malformed input would be
	// indistinguishable from a fixture that stopped being malformed.
	//
	// The control for each row is the same envelope with the defect removed. It
	// carries a real transcript and a real project, so it reaches the whole Stop
	// path — lifecycle spawn decision, capability lookup, transcript scan — and
	// is silent. The scanner finding no save is also silent, so the control's
	// silence is about the ENVELOPE being accepted rather than about the nudge
	// having nothing to say; that is the part the rows are testing.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newSandbox(t)
			call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "a memory so the control resolves a real project",
			})
			transcript, format := transcriptFor(t, "claude-code", false)

			control := s.runStdin(stopPayload("claude-code", s.work, transcript, format),
				"hook", "stop", "--source", "claude-code")
			if len(nonEmptyLines(control.stderr)) != 0 {
				t.Fatalf("the well-formed control for %q produced a diagnostic, so this row cannot be "+
					"proving the defect is what is refused:\n%s", tc.name, control.stderr)
			}

			r := s.runStdin(tc.payload, "hook", "stop", "--source", "claude-code")
			// Fail-open is absolute: the host proceeds.
			if r.code != 0 {
				t.Fatalf("a malformed payload exited %d, want 0 — the host must be allowed to stop:\n%s", r.code, r)
			}
			// Nothing on stdout, ever: a host parsing that output would be
			// misled about a decision it did not receive.
			if strings.TrimSpace(r.stdout) != "" {
				t.Fatalf("a malformed payload produced stdout:\n%s", r.stdout)
			}
			// One diagnostic line, and it names what failed.
			lines := nonEmptyLines(r.stderr)
			if len(lines) == 0 {
				t.Fatalf("a malformed payload produced no diagnostic on stderr")
			}
			if len(lines) > 1 {
				t.Fatalf("a malformed payload produced %d diagnostic lines, want exactly 1:\n%s", len(lines), r.stderr)
			}
			mustMatch(t, "the fail-open line for "+tc.name, lines[0], tc.wantStderr)
		})
	}

	t.Run("a missing or unknown --source fails open too", func(t *testing.T) {
		s := newSandbox(t)
		payload := `{"hook_event_name":"Stop","session_id":"x","cwd":"/tmp"}`

		// Missing --source: the contract has no legacy mode, so the diagnostic
		// points at the command that repairs the wiring.
		missing := s.mustRunStdin(payload, "hook", "stop")
		if strings.TrimSpace(missing.stdout) != "" {
			t.Fatalf("a hook with no --source produced stdout:\n%s", missing.stdout)
		}
		mustMatch(t, "the missing-source fail-open line", missing.stderr, "(?i)fail-open|--source|mcp init")

		// An unknown source is the same outcome.
		unknown := s.mustRunStdin(payload, "hook", "stop", "--source", "emacs")
		if strings.TrimSpace(unknown.stdout) != "" {
			t.Fatalf("a hook with an unknown --source produced stdout:\n%s", unknown.stdout)
		}
		mustMatch(t, "the unknown-source fail-open line", unknown.stderr, "(?i)fail-open|unknown")

		// An unknown event on argv is refused the same way.
		badEvent := s.mustRunStdin(payload, "hook", "teatime", "--source", "claude-code")
		if badEvent.code != 0 {
			t.Fatalf("an unknown event exited %d, want 0", badEvent.code)
		}
		if strings.TrimSpace(badEvent.stdout) != "" {
			t.Fatalf("an unknown event produced stdout:\n%s", badEvent.stdout)
		}
		mustMatch(t, "the unknown-event fail-open line", badEvent.stderr, "(?i)fail-open|unknown event")
	})

	t.Run("a missing transcript is fail-open, not a crash", func(t *testing.T) {
		s := newSandbox(t)
		r := s.mustRunStdin(
			stopPayload("claude-code", s.work, filepath.Join(t.TempDir(), "gone.jsonl"), "claude-jsonl"),
			"hook", "stop", "--source", "claude-code")
		if strings.TrimSpace(r.stdout) != "" {
			t.Fatalf("a missing transcript produced stdout:\n%s", r.stdout)
		}
		mustMatch(t, "the missing-transcript line", r.stderr, "(?i)fail-open|transcript")
	})
}

// TestHookSessionEnd covers the third event: the spawns run, and nothing is
// emitted. It fires once per real session end, so it does not read
// stop_hook_active.
func TestHookSessionEnd(t *testing.T) {
	for _, host := range hookHosts {
		t.Run("host/"+host, func(t *testing.T) {
			s := newSandbox(t)
			call(t, s.mcpSession(t), "ghost_memory_save", map[string]any{
				"project_id": e2eProject,
				"content":    "a memory for the session-end case",
			})
			s.mustRun("project", "bind", e2eProject, s.work)
			r := s.mustRunStdin(sessionEndPayload(host, s.work),
				"hook", "session-end", "--source", host)
			if r.code != 0 {
				t.Fatalf("hook session-end --source %s: %s", host, r)
			}
			// session-end never scans a transcript and never emits output: there
			// is no turn left to nudge about.
			if strings.TrimSpace(r.stdout) != "" {
				t.Fatalf("hook session-end --source %s produced output:\n%s", host, r.stdout)
			}
		})
	}
}

// oversizedPayload builds a valid envelope padded past the contract's one
// mebibyte ceiling. It is a real envelope, not garbage: the ceiling is checked
// before the JSON is parsed, so a payload that were ALSO malformed would prove
// nothing about the ceiling.
func oversizedPayload() string {
	const ceiling = 1 << 20
	envelope := `{"hook_event_name":"Stop","session_id":"big","cwd":"/tmp","pad":""}`
	padding := ceiling + 1024 - len(envelope)
	if padding < 0 {
		panic("the envelope fixture grew past the ceiling")
	}
	return `{"hook_event_name":"Stop","session_id":"big","cwd":"/tmp","pad":"` +
		strings.Repeat("A", padding) + `"}`
}

// nonEmptyLines returns the non-blank lines of s.
func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

// modTime returns a file's modification time, failing the test if it is absent.
func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st.ModTime()
}

// waitForFile polls until path exists, or fails the test naming what it waited
// for. The spawn is detached, so the file appears some time after the hook
// returns and there is no signal to wait on.
func waitForFile(t *testing.T, path string, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s at %s", within, what, path)
}

// waitForFileGone polls until path is absent, or fails the test.
func waitForFileGone(t *testing.T, path string, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s (%s still exists)", within, what, path)
}

// waitForFileContaining polls until path holds want, or fails the test. Used
// for the detached lifecycle chain's own log, which is the only place a skip
// leaves a record.
func waitForFileContaining(t *testing.T, path, want string, within time.Duration, what string) {
	t.Helper()
	waitForCount(t, path, want, 1, within, what)
}

// waitForCount polls until want appears in path at least n times, or fails the
// test. Counting rather than containing is what makes a SECOND occurrence
// waitable at all: a detached chain appends to a log the first run already
// wrote, so "wait until the log contains X" would return immediately and assert
// nothing.
func waitForCount(t *testing.T, path, want string, n int, within time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last string
	best := 0
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			last = string(b)
			if best = countOccurrences(last, want); best >= n {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s (%q x%d in %s, saw %d); the file holds:\n%s",
		within, what, want, n, path, best, last)
}
