# Install Ghost

This guide covers installing the binary, connecting an MCP client, and verifying the integration.

## Requirements

Choose one installation path:

- **Source:** Go 1.26 or newer.
- **Prebuilt binary:** no Go toolchain; download the appropriate archive from [GitHub Releases](https://github.com/wcatz/ghost/releases/latest).
- **Container:** Docker with access to the `ghcr.io/wcatz/ghost` image.

You also need one supported MCP client. Ollama is optional and is only needed for local vector embeddings:

```bash
ollama pull nomic-embed-text:v1.5
```

Without Ollama, Ghost still works with SQLite FTS5 full-text search.

## Install the binary

### From source

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
```

Make sure the Go `bin` directory is on your `PATH`. With `GOTOOLCHAIN=auto`, an older Go installation may fetch the required toolchain automatically.

Check the result:

```bash
ghost version
```

### Claude Code plugin

The Claude Code plugin bundles the Ghost binary and declares the MCP server, permissions, and lifecycle hooks. It is the shortest path for Claude Code users:

```text
/plugin marketplace add wcatz/ghost
/plugin install ghost@ghost                   # macOS and Linux, including WSL
/plugin install ghost-windows-amd64@ghost     # native Windows x64
/plugin install ghost-windows-arm64@ghost     # native Windows ARM
```

The plugin finalizes on its first session: it imports known Claude Code memories, disables the built-in file memory, and writes project redirects. Those first-run changes remain if the plugin is later uninstalled. Update a plugin-managed binary with `/plugin update`, not `ghost upgrade`.

## Initialize an MCP client

The standard setup command is:

```bash
ghost mcp init
```

With no `--client`, Ghost detects supported clients on `PATH` and configures the ones it finds. Use `--dry-run` to preview every change:

```bash
ghost mcp init --dry-run
```

Use an explicit target when you do not want auto-detection:

```bash
ghost mcp init --client claude
ghost mcp init --client opencode
ghost mcp init --client codex
ghost mcp init --client goose
ghost mcp init --client all
```

`--client all` attempts every supported installer, so each target client must be installed. `ghost mcp init` is idempotent and non-destructive. Re-running it repairs missing or outdated wiring.

### Claude Code

If the plugin is not installed:

```bash
ghost mcp init --client claude
```

The initializer registers the MCP server, installs the SessionStart and Stop hooks, grants the required tool permissions, disables Claude Code's built-in file memory, imports existing memories, and writes project redirects. Restart Claude Code after setup.

If the plugin is installed, the initializer detects that the plugin owns the integration and defers instead of double-wiring it.

### opencode

```bash
ghost mcp init --client opencode
```

This installs one lifecycle adapter at `<config-dir>/plugins/ghost-opencode.ts`, where `<config-dir>` is the directory opencode resolves as its config directory: `$OPENCODE_CONFIG_DIR` when set, else `$XDG_CONFIG_HOME/opencode` (which defaults to `~/.config/opencode`). The adapter registers the MCP server and bridges opencode session-idle events to Ghost's lifecycle contract. The same adapter supports opencode V1 and V2; it does not modify `opencode.json`. Restart opencode after setup.

`ghost mcp status --client opencode` verifies more than the adapter: it reads your user-level config sources and checks that `mcp.ghost` is present, enabled, and resolving to the ghost binary on your PATH. Those layers are opencode's config directory file — both spellings of it, `opencode.json` and `opencode.jsonc` (comments and trailing commas included), with `opencode.jsonc` winning a conflict exactly as opencode resolves them — then the file named by `$OPENCODE_CONFIG`, then inline `$OPENCODE_CONFIG_CONTENT`, with later sources overriding earlier ones key by key. A per-checkout `opencode.json` and the `.opencode` directory are deliberately not judged: they would make one run's verdict depend on the directory the command happened to be typed in, and status reports the config you carry between projects rather than the project you happen to be standing in.

The lifecycle adapter registers `mcp.ghost` at startup, so the file entry is the fallback for installs where the plugin is not loaded. A missing, disabled, or stale entry is therefore reported as a warning while the plugin is current, and the adapter keeps the server registered either way. It becomes an error when the plugin is missing too — that is the install where the file entry is the only thing that could register ghost — and then the failing line names the file to edit and the edit that repairs it (for an absent entry, the whole `mcp.ghost` block to paste). A config file status cannot read or parse is a second error, under either gate: opencode drops such a layer, so the entry in it is unknown rather than covered, and the failing line names the file to repair instead. The installer never writes that file in either case:

```jsonc
{
  "mcp": {
    "ghost": {
      "type": "local",
      "command": ["/home/you/.local/bin/ghost", "mcp"],
      "enabled": true
    }
  }
}
```

For optional embeddings:

```bash
ollama pull nomic-embed-text:v1.5
```

### Codex

```bash
ghost mcp init --client codex
```

The initializer merges `[mcp_servers.ghost]` into `~/.codex/config.toml` textually, so comments and formatting survive: a repair rewrites only the table's own `command` and `args` keys, and leaves sub-tables such as `[mcp_servers.ghost.env]`, any other key, and the rest of the file untouched, adding only the managed comment above the table header and any `command`/`args` key the table is missing. It also writes the SessionStart, Stop, and SessionEnd entries in `~/.codex/hooks.json`, merging them alongside the hooks you already have. Both files are written through a temporary file and a rename, so a run interrupted part way leaves your existing file as it was rather than half-written, and your permissions on it are kept. Ghost keeps a one-time backup of the file it merges into: `~/.claude/settings.json` gets a `.bak` on its first save, and `~/.codex/config.toml` gets one the first time Ghost writes to it — which is the first init, the run that appends `[mcp_servers.ghost]` to a file you already had. Neither `.bak` is ever rolled forward, so it stays your pre-ghost file rather than becoming Ghost's own previous output, and a later repair cannot overwrite it. A `config.toml` that did not exist before that run has nothing to copy and gets no `.bak`. `hooks.json` and the other written files get no backup, so if a merge there ever goes wrong, what was there has to be restored by hand.

Any spelling of the table header is recognised and repaired in place, including a trailing comment (`[mcp_servers.ghost] # mine`), quoted key parts (`["mcp_servers"."ghost"]`) and spacing (`[ mcp_servers . ghost ]`).

If the server is registered as a dotted or inline key instead of a table, or behind a header the initializer cannot parse with confidence (`[mcp_servers.ghost.]`, `[[mcp_servers.ghost]]`), the file is left unchanged and a warning names the line: that form cannot be merged textually, and appending a table beside it would be a duplicate definition codex rejects. Rewrite the entry as a plain `[mcp_servers.ghost]` table and re-run.

Codex requires an explicit trust step the first time:

```text
/hooks
```

Approve the Ghost entries before relying on automatic context injection or lifecycle events.

### Goose

```bash
ghost mcp init --client goose
```

This installs an Agent Plugins package under `~/.agents/plugins/ghost/`, including the MCP registration and Open Plugins hooks. Goose's event fields are normalized internally; no shell shim is required.

### Cursor and other MCP clients

Ghost speaks standard MCP over stdio. Add this to the client's MCP configuration:

```json
{
  "mcpServers": {
    "ghost": {
      "type": "stdio",
      "command": "ghost",
      "args": ["mcp"]
    }
  }
}
```

The exact configuration location and schema are client-specific. If the client supports lifecycle hooks, point them at the corresponding `ghost hook` commands described in the [CLI reference](cli.md#hooks).

## Windows

For a native Windows installation, use the PowerShell installer:

```powershell
irm https://github.com/wcatz/ghost/releases/latest/download/install.ps1 | iex
```

The script downloads the release, verifies its checksum, verifies the release's build attestation, installs `ghost.exe` under `%LOCALAPPDATA%\ghost\bin`, and adds that directory to the user `PATH`. Open a new terminal, then run:

```powershell
ghost version
ghost mcp init --client claude
```

A binary installed this way upgrades itself with `ghost upgrade` (see the [CLI reference](cli.md#ghost-upgrade)), which also handles the fact that Windows will not let a running executable be replaced in place.

### What the installer verifies, and what it needs to

**Two checks, and they are not the same kind of check.** The SHA256 in `checksums.txt` is an *integrity* check: it proves the bytes arrived intact and are the bytes the manifest names. It does not prove who published them, because `checksums.txt` is published in the same release as the archive it vouches for — anyone able to replace one can replace the other. The **build attestation** is the publisher check: a Sigstore bundle, signed by a certificate whose identity is this repository's release workflow on the release tag. From v0.43.0 every release asset carries one, `install.ps1` included.

**The attestation is verified by the GitHub CLI.** There is no Sigstore verification library in Windows PowerShell, and shipping one is a dependency nobody piping a script into `iex` has agreed to. So the script runs:

```powershell
gh attestation verify ghost_0.43.0_windows_amd64.zip `
  --repo wcatz/ghost `
  --cert-identity https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/v0.43.0
```

`--cert-identity` carries the repository, the workflow **and the tag** in one string, because those are the three things the check is for and `gh` will not let you state them separately (`--cert-identity`, `--signer-repo` and `--signer-workflow` are one exclusive group, and `--signer-workflow` matches a path — it would accept the same workflow run from a branch). If you would rather do this by hand, that command is the whole check, and `gh` prints the identity it verified against.

**So `gh` has to be installed and logged in.** `gh auth login` is required even for a public repository — that is `gh`'s own behaviour, not a choice this script makes — and there is no way around it from PowerShell, because the attestations API serves each bundle from a presigned URL whose body is snappy-compressed, and PowerShell cannot decompress snappy.

**What happens when it cannot check, and what happens when the check fails.** These are different, and the difference is the point:

| | what it means | what happens |
|---|---|---|
| **verified** | the bundle is this repository's release workflow on this tag | installs |
| **no attestation required** | the release predates v0.43.0, so none can exist | installs, and says so |
| **did not verify** | `gh` checked, and the attestation does not hold | **refused, and there is no override** |
| **could not check** | no `gh`, or `gh` is not logged in | refused unless you say otherwise (below) |

The asymmetry is deliberate and it matches [`ghost upgrade`](cli.md#attestations). The override means *"nobody could be asked"*, never *"the attestation did not check out"* — an attacker who can publish one bundle of their own would otherwise be handed the entire feature. So there is nothing to reach the third row, and its message names no flag, because there is no flag to name.

To install without the check having run, which accepts the archive on the strength of a checksum from the same release and nothing more:

```powershell
irm https://github.com/wcatz/ghost/releases/latest/download/install.ps1 | iex
# then, in the same session:
$env:GHOST_SKIP_ATTESTATION = 1
irm https://github.com/wcatz/ghost/releases/latest/download/install.ps1 | iex
```

or, if you saved the script, `.\install.ps1 -SkipAttestation`. The environment variable exists because the documented way to run the installer is a pipe, where a parameter cannot be passed at all.

**Where the check runs.** After the download, because `gh` keys its lookup on the digest of the bytes that actually arrived, and before the archive is unpacked, so nothing nobody has vouched for is ever handed to a decompressor. The cost is one wasted transfer on a release that turns out to be unattested; the property it buys is that reason enough.

Earlier releases publish no attestation at all, and the check is skipped for them — a real difference between the two eras, not a documentation quirk. A version the script cannot order (`dev`, a tag that is not a semver) is treated as *requiring* one, so "cannot tell" never reads as "old enough to skip".

If GitHub's own release attestation (`dotcom.releases.github.com`) is ever enabled for this repository, a release carrying only that one is reported as **did not verify** rather than as missing, and the third row is fatal. See [`ghost upgrade` → Attestations](cli.md#attestations) for what the operator does about that.

For the Claude Code plugin on native Windows, choose the architecture-specific entry shown above.

## Docker

The image is published for `linux/amd64` and `linux/arm64`. MCP uses stdio, so keep stdin attached and persist the data directory:

```bash
docker run -i \
  -e XDG_DATA_HOME=/data \
  -v ghost-data:/data \
  ghcr.io/wcatz/ghost:latest
```

The default command is `ghost mcp`. To run a command such as reflection against the same volume:

```bash
docker run -i \
  -e XDG_DATA_HOME=/data \
  -v ghost-data:/data \
  ghcr.io/wcatz/ghost:latest reflect myproject --apply
```

Ollama is optional inside the container. If embeddings are enabled, make the Ollama endpoint reachable from the container and configure it in Ghost's YAML or environment.

## Verify the installation

Check the binary first:

```bash
ghost version
```

Then check the specific client integration:

```bash
ghost mcp status --client claude
ghost mcp status --client opencode
ghost mcp status --client codex
ghost mcp status --client goose
```

The status command reports client registration, hooks or plugin ownership where applicable, database health, Ollama reachability, and embedding/link coverage. A generic MCP client has no Ghost-specific status command; verify that it can start `ghost mcp` and expose the `ghost_*` tools.

Start a session in a project directory. A healthy SessionStart event should provide a project context block, and the client should expose Ghost's MCP tools.

## Remove Ghost

To stop using Ghost for one client, remove its MCP registration and lifecycle hooks. For Claude Code, a plugin-managed installation can be removed through the plugin manager; do not edit its cached binary.

To remove local data, stop any running Ghost process and delete:

```text
$XDG_DATA_HOME/ghost/
# or
~/.local/share/ghost/
```

The directory contains the SQLite database and related local state. Back it up first if you want to preserve the memory store.

For configuration and environment-variable details, see [Configuration](configuration.md).
