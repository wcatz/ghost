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

This installs one lifecycle adapter at `~/.config/opencode/plugins/ghost-opencode.ts`. The adapter registers the MCP server and bridges opencode session-idle events to Ghost's lifecycle contract. The same adapter supports opencode V1 and V2; it does not modify `opencode.json`. Restart opencode after setup.

For optional embeddings:

```bash
ollama pull nomic-embed-text:v1.5
```

### Codex

```bash
ghost mcp init --client codex
```

The initializer merges `[mcp_servers.ghost]` into `~/.codex/config.toml` textually, so comments and formatting survive: a repair rewrites only the table's own `command` and `args` keys, and leaves sub-tables such as `[mcp_servers.ghost.env]`, any other key, and the rest of the file untouched, adding only the managed comment above the table header and any `command`/`args` key the table is missing. It also writes the SessionStart, Stop, and SessionEnd entries in `~/.codex/hooks.json`.

If the server is registered as a dotted or inline key instead of a table, the initializer leaves the file unchanged and prints a warning: that form cannot be merged textually, and appending a table beside it would be a duplicate definition. Rewrite the entry as a `[mcp_servers.ghost]` table and re-run.

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

The script downloads the release, verifies its checksum, installs `ghost.exe` under `%LOCALAPPDATA%\ghost\bin`, and adds that directory to the user `PATH`. Open a new terminal, then run:

```powershell
ghost version
ghost mcp init --client claude
```

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
