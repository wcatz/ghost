# ghost-opencode

[opencode](https://opencode.ai) plugin for [Ghost](https://github.com/wcatz/ghost), the MCP memory server. One npm package is the entire integration:

- **MCP self-registration** — the plugin's typed `config(cfg)` hook adds the ghost stdio MCP server to the SDK config directly. No `opencode.json[mcp]` edit needed.
- **Lifecycle bridge** — on `session.status` → idle (falling back to legacy `session.idle`), it materializes the session transcript as temp JSONL and spawns `ghost hook stop --source opencode`, driving reflection, resolve, and supersede after each turn.

Everything fails open: any error logs one line via `client.app.log` and never disturbs the session.

## Prerequisites

The `ghost` binary must be resolvable: on `PATH`, or set `GHOST_BIN` to an absolute path for hermetic setups. Install ghost separately (`go install github.com/wcatz/ghost/cmd/ghost@latest` or grab a release).

## Install

Add the package to your opencode config:

```jsonc
// ~/.config/opencode/opencode.json
{
  "plugin": ["ghost-opencode"]
}
```

## npm vs local-file variant

`ghost mcp init --client opencode` installs a *local-file* copy of this same plugin to `~/.config/opencode/plugins/ghost-opencode.ts`, managed and drift-checked by ghost itself.

| | npm (`"plugin": ["ghost-opencode"]`) | local-file (`ghost mcp init --client opencode`) |
| --- | --- | --- |
| Updates | Independent of ghost releases — bump via npm whenever the plugin changes | Re-run `ghost mcp init`; always matches your installed ghost binary |
| Network | Requires npm/Bun install | Zero-network; file written locally |
| Management | You own the dependency entry | `ghost mcp status --client opencode` verifies it byte-for-byte |

Pick npm if you want plugin updates decoupled from ghost releases; pick local-file if you prefer zero-network installs kept in lockstep with your ghost binary.

## Limitations

opencode cannot block stops or inject context, so save nudges degrade to log lines instead of prompts. Memory capture, reflection, and resolution all still run.
