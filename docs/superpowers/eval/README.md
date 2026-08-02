# Ghost Real-World Eval Suite

A one-off diagnostic, not a CI gate. Run on demand to check how Ghost's
memory system performs against real usage. See
`docs/superpowers/specs/2026-07-27-ghost-eval-suite-design.md` for the design
rationale and `docs/superpowers/plans/2026-07-27-ghost-eval-suite.md` for how
it was built.

## Running

1. Build the binary: `make build` (from the repo root).
2. Ensure `ANTHROPIC_API_KEY` is set in the environment of the process that
   will invoke the Workflow tool call below — `ghost reflect --tier haiku`,
   `ghost resolve`, and `ghost supersede` all require it, and each run spends
   real API credits. Setting it in `~/.bashrc` or a terminal you open
   afterward is not sufficient: a running Claude Code session does not
   re-source shell rc files, so the key must already be exported in the
   environment this session actually has. The workflow's Setup phase fails
   fast with a clear error if the key is missing, rather than letting every
   Consolidation/storyline call fail individually deep into the run.
3. Invoke the Workflow tool with `scriptPath` set to
   `docs/superpowers/eval/workflows/ghost-eval.workflow.js` and `args`
   optionally overriding `replayProjects` (default:
   `['ghost', 'roller', 'infra']`), `repoPath`, `realDbPath`,
   `transcriptGlobRoot`, or `keepScratch` (set `true` to skip the final
   `rm -rf` and leave scratch data in place for post-mortem on failure).
4. The report lands at `docs/superpowers/reports/YYYY-MM-DD-ghost-eval.md`.

## Cost

Each full run: N replay agents (one per real project, each reading a full
transcript) + N consolidation gradings + 4 storylines x 4-5 agents each +
3 stress-test agents + 1 synthesis agent, plus the `resolve`/`supersede`
Haiku calls inside the storyline grading steps. Budget accordingly.

Only the `ghost reflect`/`resolve`/`supersede` CLI calls (Consolidation phase
+ storyline grading steps) spend real API credits — `internal/ai/client.go`
is a direct Anthropic HTTP client and has no subscription option.
`claude-eval-session.sh` unsets `ANTHROPIC_API_KEY` before launching each
`claude -p` actor subprocess (replay/storyline/stress), so those ride your
Claude Code subscription instead of API credits — do not remove that
`env -u` or every actor session bills as pay-per-token API usage too.

## Isolation mechanism

Every live-agent phase (replay, storyline, stress) does its actual work
inside a headless `claude -p` subprocess, launched via Bash from inside a
Workflow `agent()`'s own instructions — never by having that agent call
`ghost_*` MCP tools directly against its own (real, user-scoped) MCP
connection. Each `claude -p` subprocess is isolated with:

- `--mcp-config <unit>/mcp.json --strict-mcp-config` — scopes MCP tool
  calls to a scratch-wrapped `ghost mcp` server for that unit only.
- `--settings <unit>/settings.json --setting-sources project,local` —
  scopes the `SessionStart` hook to a scratch-wrapped `ghost hook
  session-start` command, and excludes the `user` settings source so the
  real, unwrapped hook registered in `~/.claude/settings.json` never loads.
  `--strict-mcp-config` alone does not scope hooks — both flags are
  required together.
- `--permission-mode bypassPermissions` — required for headless tool
  calls to execute at all.

**Isolation scope warning:** the flags above scope Ghost's own MCP server and
hook config to a scratch dir — they do NOT sandbox the `claude -p` subprocess
itself. `--permission-mode bypassPermissions` gives that subprocess full
filesystem and network access under the real `$HOME` (only `XDG_DATA_HOME`/
`XDG_CONFIG_HOME` are scratched). Storyline/replay/stress prompts are written
by this suite and are trusted, but only run this suite inside a trusted
OS-level sandbox (container/VM) — never point it at an untrusted prompt or
transcript source.

`docs/superpowers/eval/lib/make-unit-config.sh` writes the per-unit
`mcp.json`/`settings.json` (parameterized by a `unit-run-id`, plus the
`ghost-wrapped` and `ghost` binary paths); `claude-eval-session.sh` launches
one isolated session against that unit's scratch config. Every replay
project and stress scenario gets its own `unit-run-id`; a storyline's
sessions all share one `unit-run-id` (config built once before session 1)
so later sessions actually see what earlier sessions saved.

`claude-eval-session.sh` also takes a `<project-basename>` argument and
`cd`s into a scratch directory of that name before launching `claude -p`.
Ghost's `SessionStart` hook matches a project by cwd path or by
`name = basename(cwd)` (see `internal/mcpinit/hook.go`'s `lookupProject`),
and a project created via `ghost_memory_save(project_id=...)` alone has its
stored name set to that same id — so without this, every subprocess's cwd
would be this repo's own checkout, the automatic injection the storyline
module exists to test would never fire, and each subprocess's transcript
would land in this repo's real `~/.claude/projects/` directory instead of
staying scratch-isolated.

## If something breaks isolation

Check that no `/tmp/ghost-eval/<run-id>*` directories remain after the last
run — the cleanup glob removes the shared scratch root and every per-unit
sibling dir (e.g. `/tmp/ghost-eval/<run-id>-replay-ghost`). If a run crashed
mid-way, clean it up manually with `rm -rf /tmp/ghost-eval/<run-id>*`. Never
inspect or restore from it into the real DB.
