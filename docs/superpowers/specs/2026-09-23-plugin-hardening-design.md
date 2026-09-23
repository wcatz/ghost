# Plugin Hardening Pass — Design

## Context

The 2026-09-23 Claude Code plugin audit (memory `9C497323942A8B3A56A31E04AFFC41E5`) found three gaps that cluster into one hardening PR:

- **`34CD5E1E`** — the Windows plugin name is only correct after a build-time rewrite. `plugin-windows/.claude-plugin/plugin.json` says `ghost-windows` (matching no marketplace entry), `scripts/assemble-plugin-windows.sh` jq-rewrites it to `ghost-windows-$ARCH`, and `internal/mcpinit/plugin.go` hardcodes the final set `{ghost, ghost-windows-amd64, ghost-windows-arm64}`. Only the Go side is tested (`plugin_test.go:57`); nothing asserts the three agree, so a rename on any side silently breaks `ghost mcp init`'s deferral.
- **`A5E6A856`** — the first-session finalize mutates `~/.claude/settings.json` (disables `autoMemoryEnabled`), imports memories, and writes MEMORY.md redirects into known repos, but only README line 42 discloses any of it; `isUnderPluginCache` is an unanchored substring match on `/.claude/plugins/`; the checked-in manifests carry version `0.0.0` with no explanation.
- **`7FFBDD03`** — native Windows support landed in PR #471 (per-arch marketplace entries, `ghost.exe` in `.mcp.json` and both hooks), but everything past local `claude plugin validate` still needs a real Windows host: `ghost.exe` as an MCP stdio server, hook execution, `ghost mcp init` deferral, the archive install path, and Windows ARM64 native behavior.

## Goals

- One test enforcing **set equality** across the three name sources: `.claude-plugin/marketplace.json` entries == assembled manifest names == the `PluginInstalled` allow-list.
- Home-anchored plugin-cache path detection that cannot false-positive on an unrelated path, while the safety gate stays no less strict when the home directory cannot be resolved.
- Finalize side effects and the consolidation opt-in defaults disclosed in every user-visible plugin description, plus the `0.0.0` placeholder explained where maintainers look.
- Automated Windows verification — x64 **and** ARM64 — of the MCP stdio server, both hooks, init deferral, and a zip round-trip of the plugin tree.

## Non-goals

- **Out of scope** (separate audit tasks, untouched): archive sha256 pinning (`D02DC277`), release publish ordering (`27E62189`), `claude plugin validate --strict` in CI (`80106B70`), the `plugin/` vs `plugin-windows/` sync test and the unanchored sed in `assemble-plugin.sh` (`DD648CDB`).
- No interactive consent prompt for finalize — disclosure only; the 2026-08-20 design keeps settings-restore-on-uninstall as a documented v1 non-goal (spec:201).
- No changes to hook behavior, MCP tool surface, memory schema, or search.

## Design

### 1. Three-way name-coupling test (`34CD5E1E`)

`plugin.go`'s switch becomes a package-level slice so the allow-list itself is assertable:

```go
// managedPluginNames is the exact set of Ghost plugin names that
// PluginInstalled defers to — must equal the marketplace entries and the
// assemble scripts' output names (see the coupling test).
var managedPluginNames = []string{"ghost", "ghost-windows-amd64", "ghost-windows-arm64"}
```

`PluginInstalled` loops over it with the existing case-insensitive compare; behavior is unchanged.

The new test in `internal/mcpinit` (repo files located relative to the package dir):

1. Parses `.claude-plugin/marketplace.json` → the `plugins[].name` set.
2. **Executes the real scripts**, not a reimplementation of their rewrite:
   - `PLATFORMS=linux-amd64 scripts/assemble-plugin.sh 0.0.0 <tmpdir>` → read the stamped `.claude-plugin/plugin.json` name;
   - `scripts/assemble-plugin-windows.sh amd64 0.0.0 <tmpdir>` and `... arm64 ...` → read both stamped names.
3. Asserts **set equality** across all three sources — equality, not subset: a marketplace entry the allow-list misses means `ghost mcp init` double-wires instead of deferring, and an allow-list name no entry produces means dead deferral.

Guards: skip with a clear message when `bash` is not on PATH (`exec.LookPath`).

**Forced portability fix:** both assemble scripts resolve `python3 || python` for their JSON validation step. The Windows runner image ships Python 3.12 as `python` only (no `python3` on PATH in Git Bash), and the scripts must run there for §4.

### 2. `isUnderPluginCache` tightening (`A5E6A856c`)

```go
func isUnderPluginCache(p string) bool {
    s := strings.ReplaceAll(filepath.ToSlash(p), `\`, "/")
    if home, err := os.UserHomeDir(); err == nil {
        root := strings.ReplaceAll(filepath.ToSlash(filepath.Join(home, ".claude", "plugins")), `\`, "/")
        if runtime.GOOS == "windows" { // exe casing can differ from %USERPROFILE%
            s, root = strings.ToLower(s), strings.ToLower(root)
        }
        return strings.HasPrefix(s, root+"/")
    }
    // Fail-safe: home unresolved — keep the historical substring match, so
    // behavior in that degenerate case is exactly today's.
    return strings.Contains(s, pluginCacheDir)
}
```

- Only `$HOME/.claude/plugins/**` counts; a `.claude/plugins/` segment anywhere else (the audit's false-positive `ghost upgrade` refusal) no longer matches.
- The fallback preserves today's exact behavior when `UserHomeDir` fails, so the gate never silently returns `false` in that case.
- Tests: existing cases rebuilt from a set test home (`HOME` and `USERPROFILE`, so the table also runs on the Windows leg); new false case for another root; new fallback case with both env vars emptied.

### 3. Disclosure and `0.0.0` (`A5E6A856a/b/d`)

Disclosure-only. The following text is appended to all three `.claude-plugin/marketplace.json` entry descriptions, both `plugin.json` descriptions (`plugin/` and `plugin-windows/`), and — as the same two sentences in prose — to the README plugin paragraph after line 42:

> First-run setup is automatic and persistent: it disables Claude Code's built-in file memory, imports existing Claude memories, and writes MEMORY.md redirects in known projects — these changes remain if you uninstall the plugin. Memory save on session stop is on by default; the LLM consolidation passes (reflect/resolve/supersede) are opt-in and off by default.

(The consolidation defaults are verified against `config.go`: `reflection.auto_reflect` and siblings default `false`; `stophook.go` only spawns the lifecycle when a phase is enabled.)

`0.0.0`: **accepted and documented.** The checked-in manifests keep `0.0.0` so a source-checkout `claude --plugin-dir` install is honest about having no release version; releases are stamped by the assemble scripts. Documented in this spec's Context-adjacent note and in the `assemble-plugin.sh` header comment. No comment keys added to the JSON — both manifests must stay schema-clean.

### 4. Windows CI verification (`7FFBDD03`)

A platform-independent **e2e test** in `internal/mcpinit` (runs in the ordinary `go test ./...` leg everywhere, and is the point of the Windows legs):

1. Build `ghost(.exe)` once into a temp dir (`go build ./cmd/ghost`, native GOOS/GOARCH).
2. **Archive round-trip**: assemble the tree matching the test host — `scripts/assemble-plugin-windows.sh <runtime.GOARCH>` on Windows hosts, `PLATFORMS=$GOOS-$GOARCH scripts/assemble-plugin.sh` elsewhere — zip it at root layout with Go's `archive/zip` (stored modes `0755` for entry points, matching `zip -qr .` from a directory), extract to a fresh dir, and run everything below **from the extracted tree** — proving the archive layout is installable as published.
3. **MCP stdio handshake**: real `initialize` JSON-RPC request/response against the subprocess, then `tools/list` — proving `ghost(.exe) mcp` speaks the protocol on that OS/arch.
4. **Both hooks**: `hook session-start` and `hook stop` with `CLAUDE_PLUGIN_ROOT`/`CLAUDE_PLUGIN_DATA` set to the extracted tree / a temp data dir and `HOME`+`USERPROFILE` isolated to a temp dir. Assert exit 0; context markdown on stdout; the finalize banner (`finalizing`) **never** on stdout (output discipline, spec:183); finalize marker written on first run and honored on the second.
5. **Init deferral**: an `installed_plugins.json` fixture containing `ghost-windows-amd64@ghost` (and the POSIX `ghost@ghost` case) → `mcpinit.Run` prints the defer message and creates no `~/.claude.json`.

New `ci.yml` job:

```yaml
windows:
  strategy:
    fail-fast: false
    matrix:
      os: [windows-latest, windows-11-arm]
  runs-on: ${{ matrix.os }}
  steps:
    - uses: actions/checkout@v7.0.1
    - uses: actions/setup-go@v7
      with: { go-version-file: go.mod }
    - run: go test -count=1 ./internal/mcpinit/
```

Grounding (runner-images Windows Server 2025 manifest, verified 2026-09-23): Bash 5.3, jq 1.8.1, Python 3.12 are preinstalled; `windows-11-arm` is GA (free for public repos), which closes the task's ARM64-native remainder alongside x64. `fail-fast: false` keeps one architecture's result visible while the other runs.

**Contingency:** if `windows-11-arm` proves ineligible or its tooling fails in ways the portability fixes don't cover, trim the matrix to `windows-latest` and record Windows ARM64 as the one remaining manual verification item in the task's completion notes.

The GitHub *download* path (`releases/latest/download/*.zip` fetched by the marketplace) cannot be exercised against a pre-merge artifact; the zip round-trip covers the local mechanics of what that download produces.

## Error handling

- Coupling test: missing `bash` or `go` → `t.Skip` with the reason, never a silent pass.
- E2E under isolated HOME: finalize is fully hermetic (no `~/.claude` exists → settings created in the temp home); a missing marker or finalize output on stdout fails the test, as does a nonzero hook exit.
- `isUnderPluginCache`: home-unresolvable fallback keeps historical semantics (documented above).

## Testing

- New: coupling test; `isUnderPluginCache` other-root/fallback/case table rows; the e2e suite (zip, MCP, hooks, deferral).
- Existing: `plugin_test.go` registry-name table and finalize-marker tests must stay green unchanged (except paths rebuilt from the test home where §2 requires it).
- Local: `go test` scoped to `./internal/mcpinit`, `go vet ./...`; full `ci.yml` (including both new Windows legs and actionlint over the workflow change) after push.
