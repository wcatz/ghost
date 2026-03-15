# Change Log

All notable changes to the Ghost VSCode extension will be documented in this file.

## [0.4.0] - 2026-03-15

### Added
- High-resolution color icon (512x512 RGBA) for better visibility
- Comprehensive README with features, setup, and troubleshooting
- CHANGELOG for tracking version history

### Changed
- Upgraded icon from grayscale to full-color vibrant Ghost logo
- Improved marketplace discoverability with complete metadata

## [0.3.0] - 2026-03-15

### Added
- Collapsible tool output using `<details>`/`<summary>` elements
- Tool results now expandable/collapsible for cleaner UI
- Extension icon in Activity Bar
- Complete marketplace metadata (repository, homepage, keywords, license)

### Improved
- Approval UI polish with better visual feedback
- Tool indicator styling and interaction

## [0.2.0] - 2026-03-14

### Added
- Cost tracking in footer showing session cost and cache savings
- Cache hit percentage display
- Smart scroll behavior (doesn't auto-scroll when user scrolled up)

### Fixed
- Markdown rendering regex issues
- Tool result XML tag filtering
- Scroll position preservation during streaming

## [0.1.0] - 2026-03-13

### Added
- Initial release of Ghost VSCode extension
- Chat interface with markdown rendering
- Memory search and display
- Tool approval workflow
- Project context integration
- Slash commands (/cost, /mode, /clear)
- Image attachment support (clipboard paste)
- Multiple operating modes (chat, code, debug, review, plan, refactor)
- Session management (new session, resume conversations)
- Real-time streaming responses
- Syntax highlighting for code blocks
- Copy button for code snippets

### Features
- Persistent memory system integration
- SQLite-backed conversation history
- Prompt caching for cost optimization
- Tool execution with approval guards
- Multi-modal support (text + images)
- Dark mode support
- Keyboard shortcuts (Cmd/Ctrl+Shift+G)

---

## Release Process

1. Update version in `package.json`
2. Update this CHANGELOG with changes
3. Commit: `git commit -m "chore: release vX.X.X"`
4. Tag: `git tag vscode-vX.X.X`
5. Push: `git push && git push --tags`
6. Build: `npm run compile && npx @vscode/vsce package`
7. Publish: `npx @vscode/vsce publish` (if publishing to marketplace)

## Version Scheme

We follow [Semantic Versioning](https://semver.org/):
- **MAJOR** version for incompatible API changes
- **MINOR** version for new functionality (backward compatible)
- **PATCH** version for bug fixes (backward compatible)
