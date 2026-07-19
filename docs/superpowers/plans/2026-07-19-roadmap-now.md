# Roadmap "Now" Items — Execution Plan

**Date:** 2026-07-19. Source: docs/ROADMAP.md Parts 1 + 3.

| Item | How | Status |
|---|---|---|
| Repo About description | `gh repo edit --description` → README tagline | done 2026-07-19 |
| GitHub topics | `gh repo edit --add-topic` ×7 (mcp, mcp-server, memory, claude-code, sqlite, golang, local-first) | done 2026-07-19 |
| README "why not built-in memory" section | this PR | in review |
| Commit ROADMAP.md | this PR | in review |
| LongMemEval-S in CI | separate PR: `-floors` flag on bench/longmemeval + `.github/workflows/longmemeval.yml` (weekly + dispatch: fts+hybrid w/ Ollama; PR path-filtered: fts-only). Dataset + embed-cache via actions/cache. Floors = published numbers − small margin (assert floors, not exact rankings — RRF ties are unstable). | separate PR |
| MCP registry submission | `mcp-publisher` CLI: server.json (io.github.wcatz/ghost), GitHub device-flow login, publish. Requires interactive auth — prepared, awaiting maintainer login. | prepared |
