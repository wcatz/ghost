# Memory and publish-readiness audit

**Date:** 2026-09-15 · **Branch:** `docs/audit-2026-09-15` · **Scope:** the live memory
database (`~/.local/share/ghost/ghost.db`, 580 memories across 15 projects), with a
close read of the `ghost`, `dingo`, and `infra` projects; plus `origin/main`, the
latest release tag, `server.json`, and the plugin distribution path.

This was a read-only inspection unless a change is called out below. Memory writes were
limited to deleting byte-identical duplicate rows and one correction to a routing
convention (both recorded under "Applied").

## Summary

- The **infra** project is the healthiest of the three: dense, operational, and
  honestly weighted.
- The **ghost** project is over-resolved (75% `resolved_at`) yet still carried literal
  duplicate rows that reflection had not merged.
- The **dingo** project is the real problem: 205 memories in ~3 weeks, 48% of them
  `gotcha`, each incident accumulating "root cause" and "CORRECTION" layers rather than
  being consolidated. Mechanical (SQLite-tier) reflection cannot fix this — only the
  LLM tier can, and it is not being applied.
- Publishing is blocked less by the code than by **release hygiene**: `main` is
  unreleased, and both the manifest and the plugin are behind it.

## Memory quality

### Strong

- **infra** — 24 gotchas at ~718 chars each, almost all carrying root cause, exact fix,
  and a verification command (`F2FE7A45` helmfile `condition` vs `installed`,
  `9C646095` minting a Tailscale authkey via the operator OAuth client, `53729522` the
  wrong-cluster-context trap). Category mix is balanced and importance is calibrated
  (mean 0.78 — not everything is a 1.0).
- **ghost** — durable architecture and decision memories that stay true
  (`2903A100` layout, `0AF55CEF` migrations, `D118DE18` ranking, `7200BE0E` the
  harness-only removal). The high-importance claims are, where checked, correct.
- **dingo** — contains knowledge that is genuinely hard to re-derive (the PlutusData
  definite/indefinite CBOR divergence, the missing `idx_utxo_transaction_id` halt, the
  committee-hot-key import defect). The value is real; the problem is its volume and
  layering, not its accuracy.

### Weak

| Project | n | resolved | gotchas | avg len | notes |
|---|---|---|---|---|---|
| ghost | 60 | 45 (75%) | 22 | 654 | literal duplicate clusters survived reflection |
| dingo | 205 | 27–28 | 98 (48%) | 983 | ~10.5 memories/day; 14 at the 2000-char cap |
| infra | 55 | 8 | 24 | 632 | best-shaped; one unresolved contradiction |

- **dingo is an append-only war log.** 28 memories independently claim a "root cause" of
  overlapping incidents, and 14 open with "CORRECTION to an earlier …". 35 memories
  self-describe as RESOLVED/FIXED/SOLVED but only ~28 carry `resolved_at` — the resolve
  stage is not clearing them. Fourteen sit exactly at the 2000-char truncation ceiling,
  so tail detail is already gone (tracked separately).
- **ghost had literal duplicates.** Three byte-identical clusters (3× a CodeRabbit
  reference stack, 2× a MemoryAgentBench note, 2× an mcpinit UX audit) plus near-variant
  copies. Reflection's Jaccard dedup thresholds are clearly not merging these.
- **infra has an un-superseded temporal contradiction.** `CE1C6CE9` (2026-09-14 PM: BP
  moved *back* to `cardano-node-mainnet-az2`) contradicts `A1C47B85`, `C3F98D70`,
  `ED70A277`, `7954527B`, and `189F8C71` (dingo on `mr-slave` is the live BP). Seventeen
  `supersedes` edges exist in the project but did not resolve the headline conflict, so
  both versions remain rankable.
- **Knowledge is split across two infrastructure projects.** `infra` (55, wcatz Star
  Forge) and `infrastructure` (44, blinklabs-io/demeter + homelab) are distinct repos,
  but the global convention `CB60D72F` called `infrastructure` the canonical bucket for
  cluster/SSH facts, which actively misroutes Star Forge facts. A search of one bucket
  misses half the story.
- **`_global` (87) carries stale claims** that inject into every session: two memorise
  the removed `ANTHROPIC_API_KEY` path (`42E11668`, `B25D9217`), one pins a specific
  opencode free model (`23601826`), and one asserts the project has no users besides its
  author (`A2327AA0`) — no longer true under a publish plan.

## Consolidation dry-runs

Run against the live database; nothing was applied.

| Project | Tier | Before | After | Note |
|---|---|---|---|---|
| dingo | sqlite | 200 | 172 | mechanical dedup only — the war log is not near-duplicate text |
| dingo | opencode | 200 | 25 | real consolidation, coherent summaries; resolves the CORRECTION layers |
| infra | sqlite | 55 | 47 | mechanical only |
| infra | opencode | 55 | 25 | merges the contradictory BP-state memories into one current entry |

The SQLite tier is a floor, not a fix: it removes ~14% from dingo while leaving every
"root cause" and "CORRECTION" layer intact. The LLM tier collapses dingo 200 → 25 and
infra 55 → 25, which is the lever that actually addresses the problem — but 200 → 25 is
aggressive, so it should be reviewed as a diff and applied deliberately
(`ghost reflect dingo --tier opencode --apply`), not left to the autonomous path.

## Publishing blockers

1. **The plugin does not exist yet.** `docs/superpowers/specs/2026-08-20-ghost-claude-plugin-design.md`
   designs a Claude Code plugin (`.claude-plugin/plugin.json`, `.mcp.json`, `hooks/`,
   bundled per-platform binaries), but there is no `.claude-plugin/` directory in the
   tree. Publishing is blocked on building it.
2. **`main` is unreleased.** The latest tag is `v0.29.0` (2026-09-09); `origin/main` is
   15 commits ahead, including the harness-only change that removed the Anthropic HTTP
   API tier (#418) and the README rewrite (#435/#436). `go install …@latest`,
   `install.ps1`, the Docker `:latest` tag, and the registry OCI ref therefore all serve
   `v0.29.0` — whose `--tier auto` still builds a Haiku API tier when
   `ANTHROPIC_API_KEY` is set, and whose help advertises `--tier haiku`. That contradicts
   the current README's "no key, $0/month" claim and can silently bill a new user.
3. **`server.json` was 14 minor versions stale** (`0.15.0`) and no workflow read it.
   Fixed for the current release in PR #440, which also gates future releases on it.
4. **README headline advertises Cursor** as a first-class client while `ghost mcp init`
   has no Cursor installer; clarified in PR #440.

## New-user onboarding friction

- The released binary documents `--tier haiku` / `ANTHROPIC_API_KEY`; the docs no longer
  do. Resolved by shipping a release from `main`.
- `ghost mcp status` prints `✗ permissions: 19/20` on an otherwise current install until
  `ghost mcp init` re-runs — the new `mcp__ghost__ghost_project_delete` permission is only
  granted by init. Recoverable (the tool says what to run) but it is a red mark on first
  run after an upgrade.
- Codex needs a manual `/hooks` trust step; documented, easy to miss.
- Positives: no Ollama is a graceful degradation rather than an error; `mcp init` is
  idempotent and non-destructive with `--dry-run`; `ghost mcp status` gives actionable
  remediation.

## Applied

- Deleted four byte-identical duplicate memories in the `ghost` project
  (`15EEC6CD`, `C7060319`, `EBAE2BDC`, `3D1C0E04`), keeping the highest-importance copy
  of each cluster.
- Rewrote global routing convention `CB60D72F` to name `infra` (wcatz Star Forge) and
  `infrastructure` (blinklabs-io/demeter + homelab) separately, replacing the
  "`infrastructure` is canonical" wording that caused the misrouting.

## Recommended, not applied

1. **Cut a release from `main`** so the published artifacts match the documented
   behavior, then build the Claude Code plugin against it.
2. **Consolidate dingo and infra with the LLM tier**, reviewing the proposed diff
   (`ghost reflect <project> --tier opencode` is a dry run by default) before `--apply`.
3. **Run `ghost resolve --apply` on dingo** to stamp the ~35 memories that already say
   RESOLVED/FIXED but carry no `resolved_at`.
4. **Move the misrouted `infrastructure` memories** that belong to Star Forge into `infra`.
5. **Correct the stale `_global` memories** (`42E11668`, `B25D9217`, `23601826`,
   `A2327AA0`).
6. **Raise the 2000-char content cap** so long operational memories stop truncating
   mid-detail.
