# Ghost Real-World Eval Suite

> **Historical record — not current documentation.** This workflow predates
> Ghost's CLI-harness migration. Use [`../../README.md`](../../README.md) and
> [`eval/cycle/`](../../../eval/cycle/) for the current graded evaluation; the commands below are kept
> for historical reproducibility and may require removed components.

This was a one-off diagnostic, not a CI gate. The archived workflow was used
on demand to check how Ghost's memory system performed against real usage. See
`docs/superpowers/specs/2026-07-27-ghost-eval-suite-design.md` for the design
rationale and `docs/superpowers/plans/2026-07-27-ghost-eval-suite.md` for how
it was built.

## Current replacement

The maintained graded staleness evaluation is `go run ./eval/cycle`; see
[`docs/benchmarks.md`](../../benchmarks.md) for its methodology and
results. It uses the current source-aware CLI routing and does not require
`ANTHROPIC_API_KEY`.

The Workflow tool invocation described below is retained only for historical
reproduction. It expects the old Claude actor setup and the former direct-API
Ghost tier, so it is not a supported current setup guide. In particular,
`ghost reflect --tier haiku` and `internal/ai/client.go` are no longer valid
current commands or source paths.

## Historical cost note

The archived workflow spawned multiple headless Claude actor sessions and
graded memory maintenance with the then-current provider. Its cost depended on
the authentication and billing configuration of those subprocesses; it is not a
current Ghost cost estimate. Do not set an Anthropic key to run current Ghost
maintenance. The independent Phase 4 benchmark may use a direct provider key
only when explicitly reproducing that benchmark.

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
