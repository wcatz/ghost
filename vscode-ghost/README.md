# Ghost — VSCode Extension

Memory-first personal assistant with persistent context, voice input, and agentic tool use. Ghost remembers what matters across sessions so you never lose context.

## Features

- **Persistent Memory** — Ghost stores and recalls memories across sessions using SQLite with FTS5 full-text search and time-decay scoring
- **Agentic Tool Use** — File read/write/edit, grep, glob, git, and shell execution with an approval workflow for destructive operations
- **Voice Input** — Always-on listening with wake word activation ("ghost"), powered by AssemblyAI real-time WebSocket streaming
- **Streaming Chat** — Real-time streaming responses with inline thinking blocks, tool indicators, and syntax-highlighted code
- **Multiple Modes** — Switch between chat, code, debug, review, plan, and refactor modes
- **Project Context** — Automatically detects your project and loads relevant memories and context

## Getting Started

Ghost requires the `ghost` daemon running locally:

```bash
# Build and start the daemon
cd ghost && go build -o ghost ./cmd/ghost/
./ghost serve
```

The extension connects to `http://127.0.0.1:2187` by default. Configure `ghost.serverUrl`, `ghost.authToken`, and `ghost.autoStart` in settings.

## Usage

- **Sidebar**: Click the ghost icon in the activity bar
- **Editor Panel**: `Ctrl+Shift+Alt+G` / `Cmd+Shift+Alt+G` to open Ghost Chat as an editor tab
- **Voice**: Click the mic button — say "ghost" to activate, then speak your message
- **Slash Commands**: Type `/` in the input for available commands (`/mode`, `/continue`, `/compact`, `/clear`, `/cost`, `/export`)
- **Image Input**: Paste or drag-and-drop images into the chat
- **Tool Approval**: Review and approve/deny tool executions with optional instructions

## Stack

- Go daemon with Claude API (streaming + tool_use)
- TypeScript VSCode extension
- highlight.js syntax highlighting
- AssemblyAI WebSocket streaming STT
