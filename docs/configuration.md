# Configure Ghost

Ghost works with compiled defaults and no required configuration file. The first command that opens the store may create the user config file from the embedded example.

## Configuration precedence

Later layers override earlier layers:

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. The user config file
4. `GHOST_*` environment variables
5. Supported command-line flags, applied by the command after loading

## Invalid configuration

A config file that exists but does not parse is never ignored. The error names the file and the line, so it is reported differently depending on who asked for the configuration:

| Caller | Behaviour |
|---|---|
| CLI subcommands (`ghost reflect`, `ghost resolve`, `ghost supersede`, `ghost obsidian …`, `ghost maintenance status`, `ghost project …`) | Exit non-zero with the parse error. Nothing is run against half the intended configuration. |
| The `ghost mcp` server | Warn on its log channel — stderr, or `GHOST_LOG_FILE` when set — and serve the environment plus the compiled defaults. It does not exit: that would not fail a command, it would leave your editor with no Ghost tools at all, because of a typo in a file you may not know exists. The warning sink follows the server's log channel rather than raw stderr, so setting `GHOST_LOG_FILE` keeps it out of the MCP client's face. |
| Host-session hooks (SessionStart injection, the stop hook, obsidian auto-sync, session routing) | Report the same error on stderr and continue with the environment plus the compiled defaults. A typo in the config must not fail the session you are currently working in. |
| `ghost mcp status` | Prints the path as informational, then a `!` line carrying the parse error. The remaining checks then run on the defaults, so nothing is missing from the output — but the run is **not** marked unhealthy, because the `!` line is the pointer, not a verdict. |

"Environment plus the compiled defaults" means every layer that does not read a config file. The `GHOST_*` variables still apply, so a broken file cannot undo an opt-out you set in the environment (`GHOST_EMBEDDING_ENABLED=false`, `GHOST_SCRATCH_MAX_BYTES=0`). Only the file layers are lost.

```text
ghost: config: parse /home/you/.config/ghost/config.yaml: yaml: line 3: found unexpected end of stream — falling back to the environment and built-in defaults
```

## Unknown keys

A key in a config file that no setting binds — a typo such as `linking.thresholdd` — is a warning on stderr rather than a failure: it cannot affect anything, so it should not stop a session, and the other keys in the same file still load.

```text
ghost: config: /home/you/.config/ghost/config.yaml: unknown key(s) ignored: linking.thresholdd
```

This check covers **config files only**, and the limits are worth knowing:

- A misspelled `GHOST_*` variable is still ignored silently. Ghost cannot report it, because most of its variables are not config keys at all — `GHOST_DEBUG`, `GHOST_LOG_FILE`, `GHOST_SCRATCH_DIR`, `GHOST_PASSTHROUGH_ENV` and `GHOST_OPENCODE_MODEL` are documented here or in the harness section below, and none of them binds a `Config` field.
- A `GHOST_*` value that cannot be read as its key's type is an error naming the variable, but only for the explicit shortcuts in the table below. A generic-mapped value fails later, as a decode error naming the key (`'embedding.enabled' cannot parse value as 'bool'`), which is enough to identify the setting but not the variable that carried it.

## File locations

The user config file is normally:

| Platform | Path |
|---|---|
| Linux | `$XDG_CONFIG_HOME/ghost/config.yaml`, or `~/.config/ghost/config.yaml` when `XDG_CONFIG_HOME` is unset |
| macOS | `$XDG_CONFIG_HOME/ghost/config.yaml`, or `~/Library/Application Support/ghost/config.yaml` when `XDG_CONFIG_HOME` is unset |
| Windows | `%AppData%\ghost\config.yaml` when `XDG_CONFIG_HOME` is unset |

The data directory is separate from the config directory:

| Platform | Default |
|---|---|
| Any | `$XDG_DATA_HOME/ghost/ghost.db` |
| Any, when `XDG_DATA_HOME` is unset | `~/.local/share/ghost/ghost.db` |

The data path is intentionally consistent across operating systems. A Windows installation therefore commonly uses `%USERPROFILE%\.local\share\ghost\ghost.db` for the database, while the config file follows the platform convention above.

## Minimal example

```yaml
embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"
  dimensions: 768

linking:
  enabled: true
  threshold: 0.70
  demotion_threshold: 0.90
```

The complete annotated template is embedded in [`internal/config/config.example.yaml`](../internal/config/config.example.yaml). Ghost creates a copy at the user config path when needed.

## Embeddings

Embedding is enabled by default but degrades gracefully when Ollama is unavailable:

```yaml
embedding:
  enabled: true
  ollama_url: "http://localhost:11434"
  model: "nomic-embed-text:v1.5"
  dimensions: 768
```

Install the model once:

```bash
ollama pull nomic-embed-text:v1.5
```

To use FTS5 only, set:

```yaml
embedding:
  enabled: false
```

The embedding worker runs asynchronously. A newly saved memory may appear in full-text search before its vector is available.

## Linking

Linking is active when embeddings are enabled:

```yaml
linking:
  enabled: true
  threshold: 0.70
  demotion_threshold: 0.90
```

- `threshold` is the cosine similarity required for a `related` edge.
- `demotion_threshold` is the stronger similarity required before a related edge can demote a near-duplicate during injection.

The link graph is used by the Obsidian mirror and supersession ranking. It is not a graph-expansion bonus in production search.

## Search ranking

The vector leg can use a cosine floor before results enter Reciprocal Rank Fusion:

```yaml
search:
  min_similarity: 0.0
```

FTS candidates are not subject to this floor. Raise it only after testing against your corpus with `ghost bench`; a higher value can remove weak semantic matches, while `0.0` preserves the historical behavior of dropping only non-positive cosine scores.

## Session injection

The SessionStart hook injects a bounded context digest. The default category bias reserves slots for high-signal behavioral notes:

```yaml
injection:
  behavior_floor: 8
  behavior_categories:
    - gotcha
    - convention
    - preference
    - decision
  category_weights: {}
  category_caps:
    gotcha: 4
```

Set `behavior_floor: 0` to disable the category bias and use rank-only selection. `category_weights` can give a category a small ordering boost. `category_caps` prevents one behavioral category from consuming every reserved slot.

## Lifecycle and reflection

Automatic lifecycle phases are off by default:

```yaml
reflection:
  auto_reflect: false
  auto_resolve: false
  auto_supersede: false
  lifecycle_timeout_minutes: 60
  consolidation_timeout_minutes: 10
```

When enabled, the Stop hook spawns one detached lifecycle process and runs the phases in this order:

```text
reflect → resolve → supersede
```

- `consolidation_timeout_minutes` bounds one `ghost reflect` consolidation call.
- `lifecycle_timeout_minutes` bounds each lifecycle phase. Set it to `0` to remove the bound, but keep it above the consolidation timeout when using both settings.
- The lifecycle process is fire-and-forget. Failures are logged in the Ghost data directory and do not block the Stop hook.
- The unattended reflect path requires a real CLI harness; it does not silently use the offline fallback for an automatic rewrite.

## CLI harness paths and model pins

LLM-backed maintenance uses the calling session's CLI harness. Ghost resolves binaries from `PATH` unless a path is configured explicitly:

```yaml
cli:
  claude_binary: ""
  opencode_binary: ""
  codex_binary: ""
  goose_binary: ""
  model_reflect: ""
  model_resolve: ""
  model_supersede: ""
```

Set an explicit path when a binary is installed somewhere unavailable to the Stop hook process, for example:

```yaml
cli:
  opencode_binary: "/home/you/.opencode/bin/opencode"
```

The three `model_*` settings apply only to the opencode backend. Other harnesses do not receive a model flag. An empty value uses an inherited `GHOST_OPENCODE_MODEL` value, or Ghost's explicit `opencode/big-pickle` default when no environment pin is set. A configured phase pin overrides the inherited value for that phase.

Ghost does not contain a direct Anthropic HTTP client. Each selected CLI harness uses its own configured authentication and billing; Ghost does not add a second provider or API key.

## Obsidian

```yaml
obsidian:
  vault_dir: ""
  interval: "30s"
  auto_sync: false
```

- `vault_dir` defaults to `~/Documents/GhostVault` when empty.
- `interval` accepts a Go duration such as `30s`, `1m`, or `5m`.
- `auto_sync` starts a background `ghost obsidian sync` process from SessionStart. Leave it off unless you want that process and vault.

The mirror is one-way. See [the usage guide](usage.md#obsidian-vault-mirror).

## Default project routing

Sessions in a home directory or filesystem root may fall back to a configured project:

```yaml
routing:
  default_project: ""
```

Set this only when those sessions should receive a deliberate memory context. An empty value disables the fallback.

## Environment variables

Most keys use the generic mapping:

```text
GHOST_EMBEDDING_ENABLED=true
GHOST_REFLECTION_AUTO_REFLECT=true
```

The generic transformer replaces underscores with dots. Keys whose actual names contain underscores have explicit shortcuts, including:

| Variable | Key |
|---|---|
| `GHOST_OLLAMA_URL` | `embedding.ollama_url` |
| `GHOST_OBSIDIAN_VAULT_DIR` | `obsidian.vault_dir` |
| `GHOST_OBSIDIAN_AUTO_SYNC` | `obsidian.auto_sync` |
| `GHOST_CLI_CLAUDE_BINARY` | `cli.claude_binary` |
| `GHOST_CLI_OPENCODE_BINARY` | `cli.opencode_binary` |
| `GHOST_CLI_CODEX_BINARY` | `cli.codex_binary` |
| `GHOST_CLI_GOOSE_BINARY` | `cli.goose_binary` |
| `GHOST_CLI_MODEL_REFLECT` | `cli.model_reflect` |
| `GHOST_CLI_MODEL_RESOLVE` | `cli.model_resolve` |
| `GHOST_CLI_MODEL_SUPERSEDE` | `cli.model_supersede` |
| `GHOST_LINKING_DEMOTION_THRESHOLD` | `linking.demotion_threshold` |
| `GHOST_INJECTION_BEHAVIOR_FLOOR` | `injection.behavior_floor` |
| `GHOST_INJECTION_BEHAVIOR_CATEGORIES` | `injection.behavior_categories` |
| `GHOST_INJECTION_CATEGORY_WEIGHTS` | `injection.category_weights` |
| `GHOST_INJECTION_CATEGORY_CAPS` | `injection.category_caps` |
| `GHOST_SEARCH_MIN_SIMILARITY` | `search.min_similarity` |
| `GHOST_ROUTING_DEFAULT_PROJECT` | `routing.default_project` |

The four `injection.*` variables take structured values, so they use a comma-separated form rather than YAML syntax. Whitespace around the separators is ignored:

```bash
GHOST_INJECTION_BEHAVIOR_CATEGORIES="gotcha,decision"
GHOST_INJECTION_CATEGORY_WEIGHTS="gotcha=1.2,decision=1.5"
GHOST_INJECTION_CATEGORY_CAPS="gotcha=4,decision=2"
```

A value that cannot be read as its key's type is an error naming the variable, not a silently ignored setting.

Other useful variables:

- `GHOST_OPENCODE_MODEL` — inherited model pin for opencode-backed operations. If unset, Ghost passes `opencode/big-pickle` explicitly because the isolated child does not load the user's global OpenCode config.
- `GHOST_DEBUG` — enable debug logging.
- `GHOST_LOG_FILE` — redirect MCP logs to a file; useful when a client surfaces stderr as protocol noise.
- `GHOST_LIVE_TESTS=1` — opt into the live LLM tests; without it, `go test ./...` skips billable harness calls.
- `GHOST_TEST_SOURCE` — select the harness source used by opt-in live tests (`claude-code`, `opencode`, `codex`, or `goose`).

### Harness subprocess environment

Ghost gives each LLM harness child a case-insensitive environment allowlist. The common set contains process/home variables (including `USERPROFILE`, `APPDATA`, `LOCALAPPDATA`, `PATHEXT`, and `COMSPEC` on Windows), locale, proxy/CA settings, the owned scratch temp variables, and the documented Ghost configuration variables. It does not pass arbitrary credentials or an entire `GHOST_*` namespace. In particular, `GHOST_API_KEY`, `GHOST_DATABASE_URL`, unknown `GHOST_*_TOKEN`/`*_SECRET` names, and SSH agent/askpass variables are dropped by default.

Backend-specific configuration is selected only for the selected child:

- Claude receives `CLAUDE_CONFIG_DIR` for its configured authentication store.
- Codex receives `CODEX_HOME` for its configured state and credentials.
- Goose receives its documented safe `GOOSE_*` model/provider settings and `GOOSE_PATH_ROOT`.
- OpenCode receives `OPENCODE_API_KEY` when configured. If authentication is file-based, Ghost copies only the existing `auth.json` into the invocation-owned data directory; the child still uses an invocation-owned home/config tree and a deny-all tool/MCP policy, so Ghost does not load the user's OpenCode config or plugins.

`GHOST_PASSTHROUGH_ENV=NAME1,NAME2` is an explicit escape hatch for an additional variable. Names are matched case-insensitively. Opting in can re-expose credentials to the selected harness; use it only for a value whose exposure you intend. `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, and `GOOSE_PROVIDER__API_KEY` are always removed, even when named in the hatch.

The harness commands also disable their tool surfaces explicitly: Claude runs restricted/safe mode with no built-in tools or MCP, Codex ignores user config/rules and disables shell, web, app, hook, and agent features, Goose uses `--no-profile --no-session`, and OpenCode receives a deny-all config. The allowlist is applied inside the shared spawn helper, including OpenCode's version probe.

## After changing configuration

Run the relevant health check:

```bash
ghost mcp status --client claude
```

For a new embedding model, stop and restart the MCP client after the model is available. See the [installation guide](installation.md#verify-the-installation) for client-specific status commands.
