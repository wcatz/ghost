# VSCode Extension Regression Checklist (180 items)

Generated from pre-rewrite audit. Every item must work after changes.

## A. Commands (6)
1. ghost.sendMessage — input box, sends to chat
2. ghost.newSession — creates session for workspace
3. ghost.setMode — quick pick (chat only)
4. ghost.searchMemories — input box, searches, shows results
5. ghost.showChat — focuses sidebar
6. ghost.openEditor — opens editor panel (Ctrl+Shift+G)

## B. Views (6)
7. Activity bar with Ghost icon
8. Sidebar chat (ghost.chat)
9. Sidebar memories (ghost.memories)
10. Editor panel (singleton, retainContextWhenHidden)
11. Editor panel icon
12. Editor panel singleton behavior

## C. API Calls (14)
13-26. health, sessions CRUD, send (SSE), approve, mode, auto-approve, history, memories search/create/list/delete, projects

## D. SSE Events (12)
27-38. text, thinking, tool_use_start/delta/end, tool_diff, approval_required/resolved, done, error, generic event, close

## E. Config (4)
39. ghost.serverUrl
40. ghost.authToken
41. ghost.autoStart (declared, not implemented)
42. Config change listener

## F. Chat UI (30)
43-72. Connection dot, session info, mode badge, auto-approve toggle, cost display, messages area, user/assistant/system/error messages, message queue, markdown rendering, code blocks with copy, thinking blocks, tool indicators, slash commands (/mode, /clear, /cost, /auto-approve), input behavior, streaming state

## G. Memory Panel (8)
73-80. Project selector, search, memory cards, delete, empty state, error display, CSP, HTML escaping

## H. Status Bar (7)
81-87. Position, click action, connected/disconnected states, mode, token info, connected toggle

## I. Keyboard Shortcuts (10)
88-97. Ctrl+Shift+G, Enter, Shift+Enter, Escape, y/n approval, slash menu navigation

## J. Image Handling (9)
98-106. Attach dialog, base64 conversion, preview, remove, paste, drag-drop, pending image, image_data message, images array in POST

## K. Approval Flow (8)
107-114. Modal, auto-approve bypass, tool preview, deny with instructions, resolved event, server-side channel, notifier interface, concurrent protection

## L. Session Management (7)
115-121. Auto-session, ensureSession, resume, history loading, session message, independent sessions, singleton editor

## M. Error Handling (12)
122-133. HTTP errors, SSE errors, malformed JSON, network errors, session/approval/mode/memory failures, no workspace, health check, stream close

## N-T. Server routes (14), middleware (5), types (6), CSS (8), CSP (3), cost tracking (6), lifecycle (5)
134-180. Complete server API coverage, middleware chain, TypeScript interfaces, CSS variables, animations, CSP policies, cost calculation, activation/disposal
