# Invariants

Reference for the conventions that must not drift. The **Critical Rules** that
belong in an agent's standing context stay in [`CLAUDE.md`](../CLAUDE.md); this
file is the long form.

## Package map: `internal/ai/`

CLI-harness providers (`CLIClient`/`CodexClient`/`GooseClient`/`OpenCodeClient`)
used by reflection, resolve and supersede. There is no direct LLM API client.
Every child is spawned through the one `harnessCommand` funnel, which applies the
`harnessEnv` allowlist and confines the working directory and temp variables to an
invocation-owned scratch dir. The backend is part of the policy, so each harness
keeps only its own auth/config root (`CLAUDE_CONFIG_DIR`, `CODEX_HOME`, the safe
`GOOSE_*` set, `OPENCODE_API_KEY`). Two harnesses additionally get an isolated
config tree because their plugin/MCP discovery would otherwise find Ghost's own
registration: opencode gets a fresh home plus an `mcp: {}`/`plugin: []` config,
and goose gets an empty invocation-owned `HOME` (its plugin root is home-relative
by specification, so `XDG_CONFIG_HOME` alone cannot move it). The goose home
carries the config rather than restating where it lives — the inherited
`XDG_CONFIG_HOME` when set, otherwise reproducing each home-relative root goose may
resolve (`.config/goose`, `Library/Application Support/goose`; Windows uses
`%APPDATA%`, which the allowlist already passes through) — so goose's own
per-platform resolution is untouched and authentication still works.
`GOOSE_PATH_ROOT` is the exception: goose consults it before any home variable
(plugins resolve to `$GOOSE_PATH_ROOT/.agents/plugins`), so it is repointed at a
root inside the isolated tree with the real `config/` carried into it, and a
relative value is left alone because goose ignores one. Where a symlink is refused
(Windows without Developer Mode) the files are copied instead. If the home cannot
be established, the call fails rather than running with the real home. Codex needs
no tree — `--ignore-user-config` already covers it. OpenCode's tree also owns a
DATA root (`XDG_DATA_HOME`), which is what keeps a lifecycle run out of the user's
session list (#588): `opencode run` has no flag to skip saving a session, so the
store's location is the only control, and OpenCode resolves its session database
from that variable (verified on v2.0.15 with `opencode debug paths`). V2 needs
`--standalone` for this to hold — without it `run` attaches to the user's shared
background service, and the server holding the user's data dir is what creates the
session. No `session delete` follows a run: the store sits in the invocation-owned
directory whose deferred cleanup removes it, so a session cannot outlive its call,
and a delete would cost a second OpenCode process per harness call (hundreds per
lifecycle) to erase a row from a directory about to be deleted. The pre-isolation
backlog is `ghost opencode cleanup-sessions`.

## Key pattern: no harness child gets a tool

A harness child is given a **minimal environment AND no tool surface**, because
its prompt is memory text and memory text routinely carries third-party content.
The environment half is `harnessEnv`'s explicit allowlist; this is the tool half.
The rule is per-harness because the mechanism is, and the mechanism is whatever
that harness's own CLI documents — never a guessed flag.

**Every harness argv is a WHOLE-SLICE GOLDEN**, not a per-flag grep. A grep passes
when the flags it checks are present and says nothing about a flag that was never
added, which is exactly how goose shipped with an entire tool surface unchecked.
Each argv is built by a named function (`claudeInvocationArgs`,
`codexInvocationArgs`, `gooseInvocationArgs`, `openCodeInvocationArgs`,
`openCodePolicyFor`) so the whole slice is assertable;
`TestHarnessInvocationArgsAreGoldens` covers all four harnesses including both
opencode major versions, because V1 is the only shape that takes `--pure` and the
only one eligible for the deny policy.

A golden cannot check a flag against its upstream, so the shell-fake
harness-policy tests re-check what actually reaches the child, and two of them
cannot be checked by a fake at all. A fake that reads `$GOOSE_MODE` out of its own
environment proves only that Ghost WRITES the string — not that the harness accepts
the value, ranks it above its own config file, or acts on it. So the two upstream
facts this policy rests on have `GHOST_LIVE_TESTS=1`-gated tests that run the real
binary, pick a command that makes no LLM call, and report rather than assert where
the honest answer is "cannot settle".

**claude** is the only harness whose CLI can remove tools outright.
`claudeInvocationArgs` requires five capabilities probed from the INSTALLED
binary's own `--help` (`--tools`, `--disallowedTools`, `--safe-mode`,
`--restricted`, `--strict-mcp-config`) and REFUSES to run without them — a claude
that lacks one gets an error naming the upgrade, never a silently weaker policy.
`--tools ""` is the restriction (claude's own "disable all tools" spelling, which
unlike a list of names covers a tool shipped later). The child test walks its own
argv to assert the value is EMPTY, because `"$*"` renders a missing value and an
empty one identically — and a tool name there is both a grep hit and an open tool.

**codex has NO "no tools" flag.** `--sandbox read-only` bounds what a tool may do,
not which tools exist, so the policy is a list. Both exec tools are named
(`features.shell_tool` AND `features.unified_exec`) because codex registers either
the unified PTY-backed tool or the one-shot exec, and disabling one leaves the
other. `view_image` is named because it is a plain local file read the sandbox's
write policy does not cover. The plugin and connector group (`apps`, `plugins`,
`tool_suggest`, `skill_mcp_dependency_install`) goes together because each
contributes a tool of its own and installing an MCP server on demand RUNS A
COMMAND. `agents.enabled` and the top-level `web_search="disabled"` are not
feature keys, so `features list` says nothing about them and they are never
filtered.

**The codex list is filtered against the installed binary and NEVER refuses.**
`codexFeaturesFor` probes with `codex features list`, keyed on path/size/mtime by
a `sync.Map` exactly as claude's capability probe is, and caches the result. The
negative verdict is cached too, for `codexFeatureRetry` (5 minutes), and that
asymmetry is the point rather than an oversight: an unanswering codex is the
OLD-codex install this design exists to support, so probing it per call would add
a process to each of a lifecycle's hundreds of calls — the doubling the cache
exists to prevent, for the whole run. Caching the negative for the life of the
process is the opposite failure, because a codex upgraded in place under a
long-lived MCP server would never be noticed. A positive answer does not expire at
all.

The filter matters because codex **silently IGNORES** a `-c` key it does not know
(the fail-OPEN direction): `-c` overrides are applied onto the config tree without
a `deny_unknown_fields` check, so a key renamed upstream leaves the tool on and
the call still succeeds. `--strict-config` is the strict reading, it is opt-in, and
Ghost does not pass it, so `TestLiveCodexDeclaresTheNoToolFeatureKeys` provokes an
unknown key and reports which way the installed codex actually goes.

`warnOnWeakerCodexPolicy` fires ONCE per process, naming the missing keys, and
never blocks. A probe that cannot answer is a DIFFERENT message saying
"unverified" rather than claiming the codex lacks features it was never asked
about. The refusal is the part deliberately absent and the reason is the blast
radius: `CLIProvider` selects codex by `exec.LookPath` with no version floor, so
refusing would fail every reflect, resolve and supersede call on a working install
over a surface that may not exist — a worse failure, and much larger, than the one
the policy prevents. An older codex therefore gets the keys it understands, a
weaker policy than a current one, and exactly one warning.

**opencode has no disabling flag either** — `run`'s only permission flag is
`--auto`, which moves in the WRONG direction and is never passed. The policy is a
permission deny/ask in the isolated config Ghost already writes, and the WILDCARD
is what makes it total: opencode defaults every unset permission to `allow`, so a
name it adds later is covered by `*` on the day it ships rather than the day
someone remembers it. V2 must use `ask` (a non-interactive `run` auto-declines
each ask) because a deny rule or a disabled tool strips tools from the request and
the free tier answers that with 403 `provider.auth`; V1 keeps `deny` since its
non-interactive ask behaviour is unverified.

**goose has NO flag for this at all** (its extension options only ADD extensions;
`--no-profile` governs the profile, not plugin discovery), so the restriction is
the `GOOSE_MODE=chat` environment variable goose's own reference documents as
"chat only, no tool calls", against a default of `auto` (approves tool calls). It
is SET rather than inherited, and `GOOSE_MODE` is deliberately absent from the
goose allowlist, because an inherited value could only weaken it while dropping it
returns the child to goose's autonomous default.

**Residuals, named rather than papered over.** codex's `apply_patch` has no
disabling flag or key (its registry entry is gated on the model catalog, and
`apply_patch_freeform` is marked Removed), so the read-only sandbox is its boundary
and a write attempt is refused rather than the tool withheld; the same holds for
codex's skills-extension read/list tools, which are off unless a skill exists and
the child's working directory is a neutral scratch dir holding none.

On the goose side two questions are open, and they are different in kind. Whether
`chat` mode SUPPRESSES EXTENSIONS in a given build is a property of the binary
that no configuration-printing command reports, so it is documented as unverified
rather than claimed as tested. Whether `chat` mode is COMPATIBLE WITH A HEADLESS
`goose run` is the question that decides whether this policy breaks the goose
backend at all, and `TestLiveGooseRunsATurnInChatMode` runs the production argv
under the production environment to answer it — behind `GHOST_LIVE_TESTS=1`,
because a real turn costs a model call. So the mode is confirmed end-to-end only
where goose is installed and a provider is configured, and not in CI.
