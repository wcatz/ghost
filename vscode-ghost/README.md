# Ghost for VSCode

Memory-first coding agent powered by Claude. Chat with Ghost, leverage persistent memories, and get context-aware assistance directly in VSCode.

![Ghost Icon](media/icon.png)

## Features

### 💬 **Intelligent Chat**
- Natural conversation with Claude (Sonnet 4)
- Context-aware responses based on your project
- Markdown rendering with syntax highlighting
- Code blocks with copy buttons

### 🧠 **Persistent Memory**
- Ghost remembers project patterns, conventions, and decisions
- Search through conversation history and memories
- Context builds over time for better assistance

### 🔧 **Tool Integration**
- File operations (read, write, edit)
- Git commands
- Bash execution (with approval)
- Collapsible tool output for clean UI

### 💰 **Cost Tracking**
- Real-time token usage and cost display
- Prompt caching savings shown in footer
- Per-message cost breakdown

### 🎨 **Modern UI**
- Collapsible thinking blocks (see Claude's reasoning)
- Expandable tool results
- Clean, minimal design
- Dark mode support

## Getting Started

### Prerequisites

1. **Ghost daemon must be running:**
   ```bash
   ghost serve
   ```

2. **Claude API key** configured in `~/.config/ghost/config.yaml`

### Installation

#### From VSIX (Local)
```bash
code --install-extension ghost-0.4.0.vsix
```

#### From Marketplace
Search for "Ghost" in VSCode Extensions

### Configuration

Open VSCode settings and configure:

```json
{
  "ghost.serverUrl": "http://127.0.0.1:2187",
  "ghost.authToken": "",
  "ghost.autoStart": false
}
```

## Usage

### Open Ghost Panel
- Click Ghost icon in Activity Bar (sidebar)
- Or press `Cmd/Ctrl+Shift+G`

### Send Messages
- Type your message in the input area
- Press `Enter` to send
- Use `Shift+Enter` for new lines

### Slash Commands
- `/cost` - Show session cost
- `/mode <mode>` - Switch modes (code, debug, review, plan, refactor)
- `/clear` - Clear conversation

### Attach Images
- Click 📎 button to attach screenshots
- Paste images directly from clipboard
- Ghost can analyze UI, diagrams, errors

## Keyboard Shortcuts

| Shortcut | Action |
|----------|--------|
| `Cmd/Ctrl+Shift+G` | Open Ghost chat in editor |
| `Enter` | Send message |
| `Shift+Enter` | New line |
| `Esc` | Cancel/close dialogs |

## Operating Modes

Ghost supports different modes for specialized assistance:

- **chat** - General conversation
- **code** - Feature implementation
- **debug** - Error investigation
- **review** - Code review
- **plan** - Architecture planning
- **refactor** - Code improvements

Switch modes: `/mode code` or use command palette

## Tool Approvals

When Ghost wants to execute tools:
- **Auto-approved:** File reads, git status
- **Requires approval:** File writes, bash commands
- **Approval UI:** Shows tool name, inputs, and preview

Press `y` to approve, `n` to deny, or provide custom instructions.

## Memory System

Ghost automatically extracts and stores:
- Architecture decisions
- Code patterns and conventions
- Project-specific gotchas
- Dependencies and tools

Memories are:
- Searchable via semantic similarity
- Time-decayed (situational knowledge fades)
- Project-scoped (per repository)

## Cost & Caching

Ghost uses prompt caching to reduce costs:
- Block 1: Static personality (cached ~90% savings)
- Block 2: Project context (stable cache)
- Block 3: Memories + recent commits (dynamic)

Footer shows: `$0.0023 | saved $0.0015 (65% cache)`

## Troubleshooting

### Ghost daemon not running
```bash
# Start daemon
ghost serve

# Check if running
curl http://127.0.0.1:2187/health
```

### Connection refused
- Ensure `ghost.serverUrl` matches daemon port
- Check firewall settings
- Verify daemon is running: `ps aux | grep ghost`

### No memories loading
- Check SQLite database: `~/.local/share/ghost/ghost.db`
- Verify project is detected: Check Ghost logs

### Console errors
- Open Developer Tools: `Cmd/Ctrl+Shift+I`
- Check Console tab for JavaScript errors
- Report issues on GitHub

## Development

### Build from source
```bash
cd vscode-ghost
npm install
npm run compile
```

### Debug extension
1. Open `vscode-ghost` folder in VSCode
2. Press `F5` to launch Extension Development Host
3. Make changes, reload window to test

### Package extension
```bash
npm run compile
npx @vscode/vsce package
```

## Privacy & Data

- All data stored locally in `~/.local/share/ghost/`
- API calls go directly to Anthropic (no proxy)
- Memories are project-scoped SQLite database
- No telemetry or tracking

## Links

- **GitHub:** [github.com/wcatz/ghost](https://github.com/wcatz/ghost)
- **Issues:** [github.com/wcatz/ghost/issues](https://github.com/wcatz/ghost/issues)
- **License:** Apache 2.0

## Changelog

### 0.4.0
- High-resolution color icon (512x512)
- Collapsible thinking and tool output
- Complete marketplace metadata

### 0.3.0
- Collapsible tool output with details/summary
- Extension icon added
- Approval UI polish

### 0.2.0
- Markdown rendering improvements
- Smart scroll behavior
- Cost tracking in footer

### 0.1.0
- Initial release
- Chat interface
- Memory integration
- Tool approvals

## Support

Found a bug? Have a feature request?

- Open an issue: [github.com/wcatz/ghost/issues](https://github.com/wcatz/ghost/issues)
- Discussions: [github.com/wcatz/ghost/discussions](https://github.com/wcatz/ghost/discussions)

---

**Built with ❤️ by Wayne Catz**
