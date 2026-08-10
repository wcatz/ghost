# Seamless ghost install into the Windows VM

## Context

Ghost's MCP server is being tested on Windows. The UTM-native Windows 11 ARM64
VM (`ghost-win11-utm.utm`) is now running headless under HVF and reachable over
SSH (`ghost@localhost:2222`, password auth, OpenSSH Server running). The VM's
purpose is to let agents run ghost inside Windows.

The original goal — a reachable VM — is met. This spec covers the next step:
installing ghost in the guest so it *just works* as an MCP memory server for an
in-guest opencode client, and treating every manual/friction step as a defect
against the seamless-install experience (filed as a GitHub issue on
`wcatz/ghost`).

## Goal

A seamless ghost install inside the Windows guest:

- ghost.exe deployed to the guest and on PATH;
- opencode (Windows ARM64) installed and on PATH;
- `ghost mcp init --client opencode` registers ghost as an MCP server for
  opencode;
- in-guest ghost uses the host's Ollama for vector embeddings (full hybrid
  search);
- verified end-to-end: `ghost --version`, `ghost mcp status`, a memory
  save→search→list round-trip, and `opencode mcp ls` listing ghost;
- every step that required manual intervention becomes a `wcatz/ghost` issue.

## Key design decisions

1. **ghost.exe via cross-compile.** `GOOS=windows GOARCH=arm64 go build` from
   the host (verified: builds cleanly, ~19.7 MB, pure-Go SQLite). Land at
   `C:\Users\ghost\bin\ghost.exe` — no `%` in the path (ghost's `mcp init`
   warns on `%`-containing paths on Windows).

2. **Fresh independent DB.** The guest gets its own ghost memory store at
   `C:\Users\ghost\.local\share\ghost` (the default `DataDir`). It tests
   Windows functionality in isolation; host data is never touched.

3. **opencode from GitHub releases.** `opencode-windows-arm64.zip` (asset
   confirmed to exist). scp + unzip to `C:\Users\ghost\bin\`. opencode needs to
   be on PATH only so `opencode mcp ls` can verify the registration; `ghost mcp
   init --client opencode` itself requires only ghost and degrades gracefully
   without opencode.

4. **PATH via `setx`.** Append `C:\Users\ghost\bin` to the user PATH so both
   binaries survive reboot.

5. **Host Ollama via the user-net gateway.** The guest's default
   `embedding.ollama_url: http://localhost:11434` is unreachable inside the
   guest. QEMU user-net maps the guest's `10.0.2.2` to host loopback, and host
   Ollama is up with `nomic-embed-text:v1.5`, so the guest's `config.yaml` sets
   `embedding.ollama_url: http://10.0.2.2:11434` for full hybrid search.

6. **`ghost mcp init --client opencode` does the wiring.** It deep-merges the
   `mcp.ghost` entry (`stdio`, `ghost mcp`) into opencode's config and verifies
   via `opencode mcp ls`.

## Components

| Component | Host side | Guest side |
|---|---|---|
| ghost.exe | `GOOS=windows GOARCH=arm64 go build` | `C:\Users\ghost\bin\ghost.exe` + PATH |
| opencode | download `opencode-windows-arm64.zip` | `C:\Users\ghost\bin\opencode.exe` + PATH |
| config.yaml | — | `C:\Users\ghost\.config\ghost\config.yaml`, `embedding.ollama_url=http://10.0.2.2:11434` |
| MCP registration | — | opencode config `mcp.ghost = stdio "ghost mcp"` via `ghost mcp init --client opencode` |

## Data flow

1. Host cross-compiles ghost.exe.
2. Host downloads opencode-windows-arm64.zip.
3. Both are scp'd into the guest; PATH is extended via `setx`.
4. `ghost mcp init --client opencode` runs in the guest, merging `mcp.ghost`
   into opencode's config.
5. opencode spawns `ghost mcp` over stdio; ghost persists to its SQLite store
   under the guest data dir and embeds via host Ollama at `10.0.2.2:11434`.

## Seamless-install gate (defects → issues)

Every manual or friction step encountered becomes a `wcatz/ghost` GitHub issue
(`gh issue create`, label `windows`; `mcp-init` when scoped there). Candidates
already known:

- Manual binary transfer (no one-command Windows install path).
- Manual `setx` PATH surgery.
- Hand-editing `config.yaml` to reach Ollama (no built-in host-gateway config).
- Any `ghost mcp init` failure surfaced on Windows during this run.

Each issue includes title, repro, expected vs actual, and labels. Issues are
filed even when a workaround completes the install — the workaround is the
evidence the gap exists.

## Error handling / fallbacks

- opencode zip download/unzip fails → retry asset URL; fall back to PowerShell
  `Expand-Archive`; file an issue if the documented install path is broken.
- `opencode mcp ls` fails in a headless context → rely on the config merge +
  `ghost mcp status`; file an issue for the gap.
- Host-Ollama unreachable from the guest despite user-net → fall back to
  FTS5-only search (documented); file an issue for the discovery.

## Testing / verification

- Guest: `ghost --version`.
- Guest: `ghost mcp status --client opencode`.
- Guest: memory save → search → list round-trip over SSH (proves the DB and
  server path work on Windows).
- Guest: `opencode mcp ls` lists ghost (proves registration took).
- Issue list: every friction item recorded with its issue URL.
- No repo code changes expected; `git add -u` only if something appears.

## Out of scope

- Installing an LLM-backed agent provider or API key inside the guest (opencode
  is wired, not necessarily driven to a full agent session).
- Windows binary signing / installer (`.msi`) — the subject of potential future
  issues, not this spec.
- Modifying ghost's source to fix the issues filed — this spec stops at filing.
