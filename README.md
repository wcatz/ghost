# Ghost

A memory-first coding agent that learns your codebase over time. Ghost uses Claude's API with persistent memory to provide context-aware assistance across sessions.

## What Makes Ghost Different

Most coding agents start fresh every session. Ghost remembers:

- **Architecture decisions** — why things are built a certain way
- **Conventions** — formatting, testing patterns, commit style
- **Gotchas** — bugs you've hit, tricky behavior, edge cases
- **Patterns** — recurring code structures and naming conventions
- **Dependencies** — key libraries, versions, integration notes

Memories are automatically extracted from conversations, consolidated during periodic reflection, and ranked using time-decay scoring so stale information fades while core architecture knowledge persists.

## Features

- **Persistent memory** — SQLite-backed with FTS5 full-text search and category-aware time decay
- **Multi-project** — Work on multiple codebases in parallel with isolated memory spaces
- **Native tool use** — Claude's tool_use API for file operations, code search, git, and shell commands
- **3-block prompt caching** — ~90% input token savings on repeated system prompts
- **Automatic reflection** — Haiku consolidates memories every N interactions
- **6 operating modes** — chat, code, debug, review, plan, refactor
- **Safety controls** — Approval levels for dangerous operations, destructive git commands blocked

## Install

```bash
go install github.com/wcatz/ghost/cmd/ghost@latest
```

Or build from source:

```bash
git clone https://github.com/wcatz/ghost.git
cd ghost
go build -o ghost ./cmd/ghost
```

## Quick Start

```bash
# Set your API key
export ANTHROPIC_API_KEY="sk-ant-..."

# Start Ghost in your project directory
cd ~/my-project
ghost

# Or one-shot mode
ghost "explain the authentication flow"

# Work on multiple projects
ghost --project ~/project-a --project ~/project-b
```

## Usage

### Interactive REPL

```
ghost                              # Start in current directory
ghost --mode review                # Start in review mode
ghost --model claude-opus-4-6-20250514   # Use a specific model
ghost --yolo                       # Skip tool approval prompts
ghost --continue                   # Resume last conversation
```

### REPL Commands

```
/mode <name>       Switch mode: chat, code, debug, review, plan, refactor
/switch <project>  Switch active project (multi-project mode)
/projects          List active project sessions
/memory            List all memories for current project
/memory search <q> Search memories
/memory add <text> Add a manual memory
/reflect           Force memory consolidation
/context           Show project context
/cost              Show token usage
/clear             Clear conversation (keep memories)
/quit              Exit
```

### Pipe Mode

```bash
echo "explain this error" | ghost
cat error.log | ghost "what went wrong?"
```

## Configuration

### Global Config

`~/.config/ghost/config.toml`:

```toml
[api]
key = ""                                   # or use ANTHROPIC_API_KEY env var
model_quality = "claude-sonnet-4-5-20250929"
model_fast = "claude-haiku-4-5-20251001"

[defaults]
mode = "code"
reflection_interval = 10
auto_memory = true
approval_mode = "normal"                   # "normal", "yolo", "strict"

[display]
show_token_usage = true
show_cost = true
```

### Per-Project Config

`.ghost.toml` in your project root:

```toml
[project]
name = "my-project"

[conventions]
test_command = "go test ./..."
lint_command = "golangci-lint run"
build_command = "go build ./..."

[context]
include_files = ["CLAUDE.md", "ARCHITECTURE.md"]
ignore_patterns = ["vendor/", "node_modules/"]
```

## Memory System

Ghost uses 8 memory categories with different decay behaviors:

| Category | Decay | Purpose |
|----------|-------|---------|
| architecture | none | How the codebase is organized |
| decision | 30-day | Why things were done a certain way |
| pattern | 45-day | Recurring code patterns |
| convention | none | Formatting, naming, testing style |
| gotcha | 30-day | Bugs, edge cases, tricky behavior |
| dependency | 30-day | Libraries, versions, integration |
| preference | none | Developer's preferred approaches |
| fact | none | Durable project facts |

Memories are scored using: `importance * category_decay * pinned_boost`

## Tools

Ghost has 10 built-in tools with safety controls:

| Tool | Auto-approve | Purpose |
|------|-------------|---------|
| file_read | yes | Read files with line numbers |
| file_write | confirm | Create or overwrite files |
| file_edit | warn | Search-and-replace edits |
| grep | yes | Regex code search (ripgrep) |
| glob | yes | File pattern matching |
| bash | confirm | Shell command execution |
| git | varies | Git operations (read=auto, write=confirm) |
| memory_save | yes | Save a project memory |
| memory_search | yes | Search project memories |

## Architecture

```
cmd/ghost/main.go          CLI entrypoint
internal/
  ai/                      Claude API client + streaming + tool_use
  memory/                  SQLite persistence, FTS5, time-decay scoring
  tool/                    Tool registry + 10 built-in executors
  orchestrator/            Multi-project session manager
  reflection/              Memory extraction + consolidation engine
  prompt/                  3-block system prompt construction
  mode/                    Operating mode definitions
  project/                 Project detection + context gathering
  config/                  TOML config + environment variables
  tui/                     Terminal REPL with streaming output
```

## License

Apache License 2.0 — see [LICENSE](LICENSE).
