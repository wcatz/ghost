# Configure Ghost

Ghost works with compiled defaults and no required configuration file. The first command that opens the store may create the user config file from the embedded example.

## Configuration precedence

Later layers override earlier layers:

1. Compiled defaults
2. `/etc/ghost/config.yaml`
3. The user config file
4. `GHOST_*` environment variables
5. Supported command-line flags, applied by the command after loading

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

## Data-dir retention

Ghost keeps a bounded safety history and bounds its lifecycle and Obsidian logs:

```yaml
retention:
  backup_count: 3
  log_max_bytes: 10485760
```

`backup_count` keeps that many newest `ghost.db.pre-migrate-*` copies; the live database and the newest copy are never removed. `log_max_bytes` retains the newest tail of each known data-dir log when it exceeds the cap. A value of `0` (or a negative value) disables that cleanup. Ghost reaps only dead retired per-phase PID, temporary, and lock claims; current lifecycle/Obsidian claim protocols are left untouched. The probe uses `lsof`/`fuser` when present and otherwise falls back to a native Linux procfs or Windows handle probe; a file whose holder cannot be verified is deferred. Quarantine tombstones left by an interrupted rotation are themselves bounded and reaped after a one-hour grace period.

The retention pass runs after a database opens or migrates and at the start of a detached lifecycle. The detached lifecycle and Obsidian-sync launch paths also rotate their logs file-only immediately before opening them; neither path performs synchronous database maintenance. It is best-effort and never creates the data directory just to clean it. `ghost maintenance status` uses the no-retention database-open path and remains a read-only report.

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
| `GHOST_CLI_CLAUDE_BINARY` | `cli.claude_binary` |
| `GHOST_CLI_OPENCODE_BINARY` | `cli.opencode_binary` |
| `GHOST_CLI_CODEX_BINARY` | `cli.codex_binary` |
| `GHOST_CLI_GOOSE_BINARY` | `cli.goose_binary` |
| `GHOST_CLI_MODEL_REFLECT` | `cli.model_reflect` |
| `GHOST_CLI_MODEL_RESOLVE` | `cli.model_resolve` |
| `GHOST_CLI_MODEL_SUPERSEDE` | `cli.model_supersede` |
| `GHOST_SEARCH_MIN_SIMILARITY` | `search.min_similarity` |
| `GHOST_RETENTION_BACKUP_COUNT` | `retention.backup_count` |
| `GHOST_RETENTION_LOG_MAX_BYTES` | `retention.log_max_bytes` |
| `GHOST_ROUTING_DEFAULT_PROJECT` | `routing.default_project` |

Other useful variables:

- `GHOST_OPENCODE_MODEL` — inherited model pin for opencode-backed operations. If unset, Ghost passes `opencode/big-pickle` explicitly because the isolated child does not load the user's global OpenCode config.
- `GHOST_DEBUG` — enable debug logging.
- `GHOST_LOG_FILE` — redirect MCP logs to a file; useful when a client surfaces stderr as protocol noise.

## After changing configuration

Run the relevant health check:

```bash
ghost mcp status --client claude
```

For a new embedding model, stop and restart the MCP client after the model is available. See the [installation guide](installation.md#verify-the-installation) for client-specific status commands.
