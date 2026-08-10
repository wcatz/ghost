# Seamless Ghost Install into Windows VM Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deploy ghost.exe + opencode.exe into the Windows 11 ARM64 guest (HVF, reachable via `ssh ghost@localhost:2222`), wire ghost as an MCP server for opencode via `ghost mcp init --client opencode`, point in-guest ghost at host Ollama for embeddings, verify end-to-end, and file a `wcatz/ghost` GitHub issue for every manual/friction step.

**Architecture:** Host cross-compiles `ghost.exe` (`GOOS=windows GOARCH=arm64`) and downloads the `opencode-windows-arm64.zip` release; both are scp'd into the guest to `C:\Users\ghost\bin\` and added to PATH via `setx`. `ghost mcp init --client opencode` deep-merges the `mcp.ghost` stdio entry into opencode's config. Guest `config.yaml` sets `embedding.ollama_url: http://10.0.2.2:11434` (QEMU user-net gateway → host loopback Ollama) for full hybrid search.

**Tech Stack:** Go (cross-compile), GitHub CLI (`gh`), QEMU HMP + screendump/OCR, expect (password SSH/SCP — this VM is password-auth only), Windows PowerShell/cmd (guest), GitHub issues (`gh issue create`).

## Global Constraints

- SSH/SCP into the guest is PASSWORD auth only (no keys): user `ghost`, password `GhostTest!2026`. `BatchMode=yes` and non-interactive SSH will FAIL — all host→guest operations MUST use expect wrappers.
- The guest's default remote shell is `cmd.exe`: use `&` as the command separator (NOT `;`), `2>NUL` for stderr suppression, `%VAR%` for env expansion.
- `system_powerdown` does NOT stop this VM (ACPI not honored) — never use it. VM must stay RUNNING for every task.
- `git add -A` sweeps `.opencode/` — use `git add -u` only, from `/Users/wayne/git/ghost`.
- Every task ends with the seamless-install gate: ANY manual/friction step encountered → `gh issue create` on `wcatz/ghost` with labels (use `windows`; add `mcp-init` if scoped there). Known-friction list: no one-command Windows install (manual binary transfer), manual `setx` PATH surgery, hand-editing `config.yaml` for Ollama gateway, any `ghost mcp init` failure.
- No repo code changes expected — this is deployment. Do not edit Go source.
- Guest data dir (fresh, independent): `C:\Users\ghost\.local\share\ghost`.

## Task 1: Stage artifacts on the host

**Files:**
- None in the repo. Staging dir `/tmp/win-deploy/` on the host.

**Interfaces:**
- Consumes: `gh` (authenticated as waltskinner → wcatz/ghost); Go 1.26 toolchain.
- Produces: `/tmp/win-deploy/ghost.exe` (windows/arm64), `/tmp/win-deploy/opencode.exe` (from release v1.18.16), both verified. Task 2 scp's these.

- [ ] **Step 1: Build ghost.exe for Windows/ARM64**

```bash
mkdir -p /tmp/win-deploy
cd /Users/wayne/git/ghost
GOOS=windows GOARCH=arm64 go build -o /tmp/win-deploy/ghost.exe ./cmd/ghost
```

- [ ] **Step 2: Verify the ghost.exe artifact**

Run: `file /tmp/win-deploy/ghost.exe`
Expected: `PE32+ executable (console) Aarch64, for MS Windows`

- [ ] **Step 3: Download and extract the opencode Windows ARM64 release binary**

```bash
cd /tmp/win-deploy
gh release download v1.18.16 --repo anomalyco/opencode --pattern "opencode-windows-arm64.zip" --dir /tmp/win-deploy
unzip -o opencode-windows-arm64.zip
```

- [ ] **Step 4: Verify the opencode.exe artifact**

Run: `ls -la /tmp/win-deploy/opencode.exe`
Expected: single `opencode.exe` (~174 MB uncompressed) present.

- [ ] **Step 5: Confirm the guest VM is still running**

Run: `ps -o pid,etime,stat -p 58827`
Expected: alive, STAT `S`/`N`. If not running, STOP and report BLOCKED (do not relaunch; the relaunch helper `/tmp/w11utm/relaunch.sh` exists but rebooting invalidates earlier task state).

- [ ] **Step 6: Seamless-install gate + commit**

If any step above needed manual invention beyond the exact commands given, file an issue (see the `gh issue create` format in Task 6 Step 5). Then:
```bash
cd /Users/wayne/git/ghost && git add -u && git commit -q -m "chore: stage windows deploy artifacts" 2>/dev/null || echo "no commit (clean tree)"
```
Expected: no commit (nothing staged — staging dir is in /tmp).

## Task 2: Transfer binaries into the guest

**Files:**
- None in the repo.
- Host staging: `/tmp/win-deploy/ghost.exe`, `/tmp/win-deploy/opencode.exe` (from Task 1).
- Guest target: `C:\Users\ghost\bin\`.

**Interfaces:**
- Consumes: artifacts from Task 1; VM running (pid 58827); password SSH/SCP.
- Produces: `C:\Users\ghost\bin\ghost.exe` + `C:\Users\ghost\bin\opencode.exe` on the guest. Task 3 adds them to PATH.

- [ ] **Step 1: Create the guest bin directory**

Write this expect script to `/tmp/scp-setup.exp`:

```expect
#!/usr/bin/expect -f
set timeout 30
spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/tmp/known_hosts.tst ghost@localhost -p 2222 {mkdir bin & echo MKDIR_DONE}
expect {
    "password:" { send "GhostTest!2026\r"; exp_continue }
    "MKDIR_DONE" { }
    timeout { puts "TIMEOUT"; exit 1 }
    eof { }
}
expect eof
```

Run: `expect /tmp/scp-setup.exp`
Expected: `MKDIR_DONE` (creates `C:\Users\ghost\bin`).

- [ ] **Step 2: scp ghost.exe into the guest**

Write `/tmp/scp-ghost.exp`:

```expect
#!/usr/bin/expect -f
set timeout 60
spawn scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/tmp/known_hosts.tst -P 2222 /tmp/win-deploy/ghost.exe ghost@localhost:bin/ghost.exe
expect {
    "password:" { send "GhostTest!2026\r"; exp_continue }
    timeout { puts "TIMEOUT"; exit 1 }
    eof { }
}
```

Run: `expect /tmp/scp-ghost.exp`
Expected: transfer completes without error.

- [ ] **Step 3: scp opencode.exe into the guest**

Write `/tmp/scp-opencode.exp` identical to Step 2 but sourcing `/tmp/win-deploy/opencode.exe` → `bin/opencode.exe`. Run: `expect /tmp/scp-opencode.exp`
Expected: transfer completes without error (this is a large file — allow up to 60s).

- [ ] **Step 4: Verify both files landed on the guest**

Write `/tmp/ssh-verify.exp`:

```expect
#!/usr/bin/expect -f
set timeout 30
spawn ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/tmp/known_hosts.tst ghost@localhost -p 2222 {dir bin & echo LIST_DONE}
expect {
    "password:" { send "GhostTest!2026\r"; exp_continue }
    "LIST_DONE" { }
    timeout { puts "TIMEOUT"; exit 1 }
    eof { }
}
expect eof
```

Run: `expect /tmp/ssh-verify.exp`
Expected: `bin` contains `ghost.exe` (~19.7 MB) and `opencode.exe` (~174 MB).

- [ ] **Step 5: Seamless-install gate**

This task (binary transfer via scp) is manual friction — the seamless install gap "no one-command Windows install path for ghost" exists. File the issue now (see the format in Task 6 Step 6) unless you determine an issue already covers it (`gh search issues --repo wcatz/ghost "windows install"` first). Record the issue URL.

- [ ] **Step 6: Commit**

```bash
cd /Users/wayne/git/ghost && git add -u && git commit -q -m "chore: transfer windows deploy binaries to guest" 2>/dev/null || echo "no commit (clean tree)"
```
Expected: no commit (nothing staged).

## Task 3: Add bin dir to guest PATH and smoke-test binaries

**Files:**
- None in the repo.
- Guest env: user PATH (persisted via `setx`).
- Guest binaries: `C:\Users\ghost\bin\ghost.exe`, `C:\Users\ghost\bin\opencode.exe` (from Task 2).

**Interfaces:**
- Consumes: binaries from Task 2.
- Produces: guest user PATH containing `C:\Users\ghost\bin`; verified `ghost --version` + `opencode --version` in the guest. Task 4 runs ghost config.

- [ ] **Step 1: Append bin dir to the guest user PATH via setx**

Write `/tmp/ssh-setx.exp` (same expect skeleton as Task 2 Step 4) running:

```
setx PATH "%PATH%;C:\Users\ghost\bin" & echo SETX_DONE
```

Note: `setx` prints its own confirmation; the `& echo SETX_DONE` confirms completion. Expected: `SETX_DONE`. (setx truncates PATH at 1024 chars — the guest PATH is short, safe.)

- [ ] **Step 2: Smoke-test ghost.exe in a NEW SSH session**

setx affects only new processes, so a fresh `ssh` invocation picks up the new PATH. Write `/tmp/ssh-smoke.exp` running:

```
ghost --version & echo VERSION_DONE
```

Expected: ghost version string then `VERSION_DONE`. If `'ghost' is not recognized`, the setx didn't apply — retry Step 1, then open a brand-new SSH session (each `expect` run is a new process, so this should be fine).

- [ ] **Step 3: Smoke-test opencode.exe in the guest**

Same helper, command: `opencode --version & echo OC_DONE`
Expected: opencode version then `OC_DONE`.

- [ ] **Step 4: Seamless-install gate**

Manual `setx` PATH surgery is a known friction gap — file a `wcatz/ghost` issue ("ghost on Windows: no PATH setup as part of install") unless one exists. Record the URL. If either smoke test failed, file an issue for the failure with the exact repro.

- [ ] **Step 5: Commit**

```bash
cd /Users/wayne/git/ghost && git add -u && git commit -q -m "chore: guest PATH + binary smoke tests" 2>/dev/null || echo "no commit (clean tree)"
```
Expected: no commit.

## Task 4: Point guest ghost at host Ollama

**Files:**
- None in the repo.
- Guest config: `C:\Users\ghost\.config\ghost\config.yaml` (created by ghost on first run if absent).
- Guest data dir: `C:\Users\ghost\.local\share\ghost`.

**Interfaces:**
- Consumes: ghost.exe on PATH (Task 3).
- Produces: guest `config.yaml` with `embedding.ollama_url: http://10.0.2.2:11434`; guest ghost reaching host Ollama. Task 5 runs `ghost mcp init`.

- [ ] **Step 1: Prime the config file (first ghost run)**

Write `/tmp/ssh-prime.exp` running `ghost --version` (this triggers `EnsureConfigFile` which writes the default config.yaml). Expected: version string.

- [ ] **Step 2: Read the generated config to find the embedding key**

Write `/tmp/ssh-configcat.exp` running:
```
type "%USERPROFILE%\.config\ghost\config.yaml" & echo CONFIG_DONE
```
Expected: default config including `embedding.ollama_url` or the embedding section. (If `config.yaml` is not at this path, run `where ghost` and check the config dir reported by `ghost mcp status`.)

- [ ] **Step 3: Set the host-gateway Ollama URL in config.yaml**

The generated default `config.yaml` has the ENTIRE embedding section **commented out** (verified: `config.example.yaml` comments out `embedding.*`), so there is no `ollama_url` line to sed-replace — a `-replace 'localhost:11434'` would silently match nothing. Instead, **append** an uncommented `embedding:` block to the end of the file (koanf parses the whole file; a later block wins; commented lines are ignored). Write `/tmp/ssh-writecfg.exp` running this PowerShell one-liner:

```
powershell -Command "$cfg=Join-Path $env:USERPROFILE '.config\ghost\config.yaml'; $block=[Environment]::NewLine+'embedding:'+[Environment]::NewLine+'  enabled: true'+[Environment]::NewLine+'  ollama_url: \"http://10.0.2.2:11434\"'+[Environment]::NewLine+'  model: \"nomic-embed-text:v1.5\"'+[Environment]::NewLine+'  dimensions: 768'+[Environment]::NewLine; Add-Content $cfg $block; Get-Content $cfg | Select-Object -Last 6"
```

Expected: the last 6 lines of config.yaml are the uncommented embedding block with `ollama_url: "http://10.0.2.2:11434"`. Note the nested double quotes inside the PowerShell string are escaped with `\"` so the SSH/cmd layer passes them through — verify the printed config has literal `http://10.0.2.2:11434` without stray backslashes; if the quoting mangles, write the block with single quotes instead (`'http://10.0.2.2:11434'` — YAML accepts both).

- [ ] **Step 4: Verify the guest reaches host Ollama**

Same helper, running:
```
ghost mcp status --client opencode & echo STATUS_DONE
```
Expected: the Ollama/embedding check reports the model present (host Ollama at 10.0.2.2:11434 with `nomic-embed-text:v1.5`). If Ollama is unreachable (e.g. timeout), the QEMU user-net gateway may not map 10.0.2.2 → host loopback in this build — fall back to FTS5-only (document in report) and file an issue for the discovery.

- [ ] **Step 5: Seamless-install gate**

Hand-editing `config.yaml` to reach Ollama is a known friction gap — file a `wcatz/ghost` issue ("ghost on Windows: no built-in host-gateway Ollama config") unless one exists. Record the URL.

- [ ] **Step 6: Commit**

```bash
cd /Users/wayne/git/ghost && git add -u && git commit -q -m "chore: guest ghost config points at host ollama" 2>/dev/null || echo "no commit (clean tree)"
```
Expected: no commit.

## Task 5: Wire ghost into opencode via ghost mcp init

**Files:**
- None in the repo.
- Guest config: opencode config file (created/merged by `ghost mcp init --client opencode`).

**Interfaces:**
- Consumes: ghost.exe + opencode.exe on PATH (Task 3); config.yaml with Ollama gateway (Task 4).
- Produces: opencode config containing `mcp.ghost` (stdio `ghost mcp`); verified via `opencode mcp ls`. Task 6 does the end-to-end round-trip.

- [ ] **Step 1: Run ghost mcp init targeting opencode**

Write `/tmp/ssh-mcpinit.exp` running:
```
ghost mcp init --client opencode & echo INIT_DONE
```
Expected output markers: `[1/2] Checking prerequisites...`, `✓ ghost binary at ...`, `[2/2] Registering MCP server...`, `✓ verified: ghost listed by opencode mcp ls`, then `INIT_DONE`. Capture the FULL output in the report.

- [ ] **Step 2: Handle the known fallback if opencode mcp ls fails headless**

If Step 1 printed "could not verify registration" (opencode CLI not on PATH or `mcp ls` failed in this headless context):
- First re-run `opencode --version` to confirm opencode is on PATH.
- If opencode is present but `opencode mcp ls` still fails, verify the config merge directly: run
  ```
  type "%USERPROFILE%\.config\opencode\opencode.json" & echo CFG_DONE
  ```
  (path may be `opencode.jsonc` or `opencode.json` — check both). Expected: a `"mcp"` object containing `"ghost": {"type": "local", "command": ["ghost", "mcp"], ...}`.
- Then run `ghost mcp status --client opencode` and confirm it reports the ghost registration.
- If the config merge itself failed, file an issue with the exact `ghost mcp init` output.

- [ ] **Step 3: Confirm ghost is listed by opencode mcp ls**

Run (same helper): `opencode mcp ls & echo LS_DONE`
Expected: output lists `ghost`. If `opencode mcp ls` errors headless (no TTY), this is the known fallback in Step 2 — document it and rely on the config check + `mcp status`.

- [ ] **Step 4: Seamless-install gate**

Any failure or awkwardness in `ghost mcp init --client opencode` on Windows → file a `wcatz/ghost` issue (labels `windows`, `mcp-init`) with the full output. If everything worked cleanly, no new issue (but record "no friction" in the report).

- [ ] **Step 5: Commit**

```bash
cd /Users/wayne/git/ghost && git add -u && git commit -q -m "chore: wire ghost MCP server into opencode in guest" 2>/dev/null || echo "no commit (clean tree)"
```
Expected: no commit.

## Task 6: End-to-end verification and issue ledger

**Files:**
- None in the repo.
- Consumes: complete install (Tasks 1-5).
- Produces: verified end-to-end ghost in-guest; complete issue list (URLs) for all friction found.

- [ ] **Step 1: ghost --version and mcp status**

Write `/tmp/ssh-final.exp` running:
```
ghost --version & ghost mcp status --client opencode & echo FINAL_DONE
```
Expected: version; `mcp status` reports ghost binary found, opencode registration OK, Ollama model present.

- [ ] **Step 2: Memory round-trip in the guest over the MCP protocol**

Ghost has NO `ghost memory` CLI — the memory tools exist only as MCP server tools (`ghost_memory_save`, `ghost_memory_search`, `ghost_memories_list`, etc.). Verify the round-trip by driving the guest's MCP server over stdio from the host, via SSH: the host's python3 pipes newline-delimited JSON-RPC into `ssh ghost@localhost -p 2222 "C:\Users\ghost\bin\ghost.exe mcp"` and reads responses from the SSH stdout. (Verified working: save→search→list round-trip returns the same memory ID.)

Write `/tmp/roundtrip.py`:

```python
import subprocess, json, time, sys, os, getpass
SSH = ['ssh', '-o', 'StrictHostKeyChecking=no', '-o', 'UserKnownHostsFile=/tmp/known_hosts.tst',
       '-p', '2222', 'ghost@localhost', r'C:\Users\ghost\bin\ghost.exe mcp']
p = subprocess.Popen(SSH, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
buf = bytearray()
def send(m):
    p.stdin.write((json.dumps(m)+'\n').encode()); p.stdin.flush()
def drain(t=4):
    time.sleep(t); buf.extend(p.stdout.read1(65536)); return buf.decode(errors='replace')
def call(mid, tool, args):
    send({"jsonrpc":"2.0","id":mid,"method":"tools/call","params":{"name":tool,"arguments":args}})
    return drain(2)
send({"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"drivetest","version":"1.0"}}})
drain(2)
send({"jsonrpc":"2.0","method":"notifications/initialized"})
out = call(2, "ghost_memory_save", {"project_id":"win-test","content":"seamless-windows-install verification marker","category":"fact","importance":0.5})
print("SAVE:", out)
out = call(3, "ghost_memory_search", {"project_id":"win-test","query":"seamless verification marker"})
print("SEARCH:", out)
out = call(4, "ghost_memories_list", {"project_id":"win-test"})
print("LIST:", out)
p.kill()
```

Run: `python3 /tmp/roundtrip.py` with password `GhostTest!2026` — if the SSH pipe blocks waiting for the password (no key auth), run it under an expect wrapper that answers the password prompt first, OR add `-o BatchMode=no` and use `sshpass` if installed. Simplest robust path: wrap in expect — write `/tmp/roundtrip.exp` that spawns `python3 /tmp/roundtrip.py` and on `password:` sends `GhostTest!2026`. (python3 must be present on the HOST — it is on macOS.)

Expected:
- `SAVE:` → `"Memory saved (id: <HEXID>)"`
- `SEARCH:` → a line `- [fact] ... (0.5) «seamless-windows-install verification marker»`
- `LIST:` → the same memory listed.

If the SSH pipe corrupts the newline framing (cmd.exe layer), fall back to running the python driver entirely in the guest via PowerShell's `Invoke-WebRequest`-free approach: scp the script to the guest and run `C:\Users\ghost\bin\ghost.exe mcp` with the driver, OR — last resort — write the driver in PowerShell in the guest. Record whichever worked.

- [ ] **Step 3: Confirm opencode lists ghost**

Run: `opencode mcp ls & echo LS_DONE`
Expected: `ghost` in the list (or the documented Step 2 fallback of Task 5 applies).

- [ ] **Step 4: Clean up the test memory**

Use the MCP protocol (extend `/tmp/roundtrip.py` with a 5th call) to delete the round-trip test memory via `ghost_memory_delete` (project_id `win-test`, memory_id from the SAVE result). This leaves the fresh DB clean. If the delete call fails, leave the memory and note it.

- [ ] **Step 5: Compile the issue ledger**

Run `gh search issues --repo wcatz/ghost "windows" --state all` and list any related issues (pre-existing: #252 "Windows mcp init: follow-ups from #251", #245 "mcp init breaks on Windows: hook quoting, project-path encoding, PID liveness" — both closed; check for any duplicate that would cover new findings). Then for every friction item recorded across Tasks 1-5 that does NOT already have an issue, create one:

```bash
gh issue create --repo wcatz/ghost --title "<specific friction title>" --label "windows" --body "## Repro ... ## Expected ... ## Actual ..."
```

Add `--label "mcp-init"` for mcp-init-scoped friction. The `ghost` repo labels may not have `windows`/`mcp-init` yet — `gh label create windows --repo wcatz/ghost --force` (and same for `mcp-init`) before creating issues, or omit the label if label management is out of scope (note it in the report).

- [ ] **Step 6: Write the final report and ledger**

Write `/Users/wayne/git/ghost/.superpowers/sdd/2026-08-10-seamless-windows-install/progress.md` recording: each task's result, every friction item, every issue URL (title + number), the end-to-end verification outputs, and the exact "how to reproduce install" steps for the user.

- [ ] **Step 7: Commit**

```bash
cd /Users/wayne/git/ghost && git add -u && git commit -q -m "chore: windows deploy issue ledger" 2>/dev/null || echo "no commit (clean tree)"
```
Expected: no commit.

---

## Self-review notes

- **Spec coverage:** every spec decision maps to a task — cross-compile (T1), opencode release download (T1), fresh independent DB (T6 round-trip, guest default path), PATH via setx (T3), host Ollama gateway (T4), `mcp init --client opencode` (T5), end-to-end verify (T6), seamless-install gate → issues (all tasks, ledger T6). Out-of-scope items (provider API key, msi signing, source fixes) are not tasks.
- **Verification is evidence-based:** each task ends with an observable check, and the report captures the actual command output rather than claiming success.
- **Known-friction issues are filed even when the workaround completes the install** — the workaround IS the evidence the seamless gap exists.
