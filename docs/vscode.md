# Ghost VSCode Extension

## Overview

The Ghost VSCode extension provides IDE-integrated access to Ghost's memory and chat capabilities. It communicates with `ghost serve` via HTTP + SSE.

## Architecture

```
┌─ VSCode ──────────────────────────────┐
│  Extension Host (Node.js)             │
│    ├── ghost-client.ts (HTTP + SSE)   │
│    ├── chat-panel.ts (WebviewProvider)│
│    ├── memory-panel.ts (WebviewProvider)│
│    └── status-bar.ts (StatusBarItem)  │
│         │                             │
│         │  HTTP/SSE                   │
│         ▼                             │
│  ghost serve (127.0.0.1:2187)         │
│    ├── POST /api/v1/sessions          │
│    ├── POST /api/v1/sessions/{id}/send│
│    ├── POST /api/v1/sessions/{id}/approve│
│    ├── GET  /api/v1/memories/{id}     │
│    └── GET  /api/v1/health            │
└───────────────────────────────────────┘
```

## Panels

### Chat (Sidebar)

- Markdown rendering (VSCode built-in)
- Streaming responses via SSE
- Tool progress indicators
- Inline approval buttons (Allow / Deny)
- Image display (native `<img>` in webview)
- Input box with Enter-to-send
- Mode selector dropdown

### Memory Browser (Sidebar)

- Browse memories by category
- Search bar (FTS5)
- Click to expand memory content
- Delete button per memory
- Filter by project
- Importance score display

### Status Bar

```
Ghost: my-project/code | 2.1k tokens | $0.004
```

Click to open command palette with Ghost commands.

## API Endpoints Required

| Method | Path | Purpose |
|--------|------|---------|
| POST | `/api/v1/sessions` | Start session for workspace |
| GET | `/api/v1/sessions` | List active sessions |
| DELETE | `/api/v1/sessions/{id}` | Stop session |
| POST | `/api/v1/sessions/{id}/send` | Send message → SSE stream |
| POST | `/api/v1/sessions/{id}/approve` | Respond to approval |
| POST | `/api/v1/sessions/{id}/mode` | Change operating mode |
| GET | `/api/v1/sessions/{id}/context` | Get project context |
| POST | `/api/v1/memories/search` | Search memories |
| GET | `/api/v1/memories/{projectID}` | List memories |
| POST | `/api/v1/memories` | Create memory |
| DELETE | `/api/v1/memories/{id}` | Delete memory |
| GET | `/api/v1/health` | Health check |

## SSE Event Format

```
data: {"type":"text","text":"Here is the answer."}
data: {"type":"tool_use_start","tool":{"id":"t1","name":"file_read"}}
data: {"type":"tool_input_delta","tool":{"id":"t1","input_delta":"{\"path\":\"/foo\"}"}}
data: {"type":"tool_use_end","tool":{"id":"t1","duration_ms":142}}
data: {"type":"approval_required","tool":"bash","input":{"command":"git push"}}
data: {"type":"done","usage":{"input_tokens":2100,"output_tokens":891,"cache_read":1800},"cost_usd":0.004}
data: {"type":"error","message":"context canceled"}
```

## Extension Commands

| Command | Title | Trigger |
|---------|-------|---------|
| `ghost.send` | Ghost: Send Message | Chat panel Enter |
| `ghost.switchMode` | Ghost: Switch Mode | Command palette |
| `ghost.approve` | Ghost: Approve Tool | Inline button |
| `ghost.deny` | Ghost: Deny Tool | Inline button |
| `ghost.searchMemory` | Ghost: Search Memory | Memory panel |
| `ghost.openChat` | Ghost: Open Chat | Activity bar icon |

## Connection Management

1. Extension checks `GET /api/v1/health` on activation
2. If `ghost serve` is not running, offers to spawn it as child process
3. SSE connections auto-reconnect with exponential backoff (1s, 2s, 4s, max 30s)
4. Status bar shows connection state (connected / reconnecting / offline)

## Development

```bash
# Install dependencies
cd vscode-ghost
npm install

# Build
npm run compile

# Package
npx vsce package

# Install locally
code --install-extension ghost-*.vsix
```
