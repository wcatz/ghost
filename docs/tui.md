# Ghost TUI

## Overview

Ghost's terminal UI is built with [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea) using the Model-Update-View (MVU) pattern. It replaces the original bufio.Scanner REPL with a full-featured terminal application.

## Components

### Layout

```
┌─ Header ─────────────────────────────────────────┐
│  ghost v0.1.0 | my-project (code)                │
├──────────────────────────────────────────────────┤
│                                                   │
│  [user] What does the auth middleware do?          │
│                                                   │
│  [ghost] The auth middleware in `server/auth.go`   │
│  handles JWT validation...                        │
│                                                   │
│  ```go                                            │
│  func AuthMiddleware(next http.Handler) ...        │
│  ```                                              │
│                                                   │
│  ⚙ file_read server/auth.go ✓ 142ms              │
│  ⚙ grep "AuthMiddleware" ✓ 89ms                   │
│                                                   │
├─ Tool Panel ─────────────────────────────────────┤
│  ⚙ bash ⠋ running...                             │
├─ Input ──────────────────────────────────────────┤
│  > _                                              │
│                                                   │
├─ Status Bar ─────────────────────────────────────┤
│  my-project | code | in:2.1k out:891 | $0.004     │
└──────────────────────────────────────────────────┘
```

### Message Bubbles

- **User messages**: right-aligned, bordered
- **Assistant messages**: left-aligned, markdown-rendered via glamour with syntax highlighting
- **Tool blocks**: collapsible, show tool name + duration + result summary

### Approval Dialog

When a tool requires approval, an overlay appears:

```
┌─────────────────────────────────────┐
│  bash                               │
│  command: git push origin feat/foo  │
│                                     │
│  [y] Allow  [n] Deny  [a] All      │
└─────────────────────────────────────┘
```

Keys: `y` approve, `n` deny, `a` approve all remaining.

### Command Palette

`Ctrl+K` opens the palette:

```
┌─────────────────────────────────────┐
│  > mo_                              │
│  ─────────────────────────────────  │
│  /mode chat                         │
│  /mode code                         │
│  /mode debug                        │
│  /mode review                       │
│  /mode plan                         │
│  /mode refactor                     │
└─────────────────────────────────────┘
```

### Image Support

Ghost renders images inline in terminals that support it:
- **Sixel**: WezTerm, mlterm, foot
- **Kitty protocol**: Kitty
- **iTerm2 protocol**: iTerm2, WezTerm

Fallback for unsupported terminals: `[Image: filename.png, 800x600]`

Send images to Claude: `/image path/to/screenshot.png`

## Key Bindings

| Key | Action |
|-----|--------|
| Enter | Send message |
| Shift+Enter | Insert newline |
| Ctrl+K | Open command palette |
| Ctrl+C | Cancel stream / quit |
| Ctrl+Space | Push-to-talk (Phase C) |
| Page Up/Down | Scroll viewport |
| Up/Down | Input history (when input empty) |
| Esc | Close overlay / clear input |

## Slash Commands

Available via command palette or typed directly:

| Command | Action |
|---------|--------|
| `/mode <name>` | Switch: chat, code, debug, review, plan, refactor |
| `/switch <project>` | Switch active project |
| `/projects` | List project sessions |
| `/memory` | List memories |
| `/memory search <q>` | Search memories |
| `/memory add <text>` | Add manual memory |
| `/reflect` | Force memory consolidation |
| `/context` | Show project context |
| `/cost` | Show token usage and cumulative cost |
| `/image <path>` | Send image to Claude |
| `/clear` | Clear conversation (keep memories) |
| `/quit` | Exit |

## Modes

Ghost supports three terminal modes:

| Mode | When | TUI |
|------|------|-----|
| Interactive | `ghost` (terminal) | Bubbletea |
| One-shot | `ghost "query"` | Lightweight streamer |
| Pipe | `echo ... \| ghost` | Lightweight streamer |
| Fallback | `--no-tui` or `TERM=dumb` | Legacy bufio REPL |

## Streaming Pipeline

```
Session.Send() → <-chan StreamEvent
                      ↓
              bridge.go: waitForEvent() tea.Cmd
                      ↓
              StreamEventMsg → Model.Update()
                      ↓
              text → messages.go (glamour render) → viewport
              tool_use_start → toolbar.go (add spinner)
              tool_use_end → toolbar.go (show ✓ + duration)
              approval → approval.go (show overlay)
              done → statusbar.go (update tokens/cost)
              error → error display (3s timeout)
```

## Cost Tracking

The status bar shows per-session cumulative cost. Token data comes from the Claude API response:

```
input_tokens:              Regular input (not cached)
output_tokens:             Generated output
cache_creation_tokens:     First-request cache write (1.25x input cost)
cache_read_tokens:         Cache hits (0.1x input cost)
```

Per-request cost stored in `token_usage` table, queryable per project.
