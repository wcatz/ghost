# Ghost — Future Work & Strategic Roadmap

> Working notes from a July 2026 audit and strategy session. This is a planning
> document, not a commitment — items move between horizons as real usage and
> demand show up. For current behavior, start with [`README.md`](../README.md)
> and the [documentation index](README.md).

## How to read this

Horizons, roughly:

- **Now** — cheap, concrete, do these regardless of anything else
- **Next** — real work, only worth it once a specific trigger condition is met
- **Exploratory** — interesting, unproven, explicitly *not* a commitment
- **Debt** — things the current implementation already owes, tracked so they
  don't get rediscovered the hard way

---

## Part 1 — Now: low-effort, high-leverage fixes

> Description + topics applied 2026-07-19.

- [x] **Fix the repo's "About" description.** At the time of the July 2026
  audit, the GitHub meta description read *"Memory-first personal assistant
  daemon"*, which was stale relative to the README. The repository description
  should continue to lead with Ghost's local-first, cross-client positioning.
- [x] **Add GitHub topics**: `mcp`, `mcp-server`, `memory`, `claude-code`,
  `sqlite`, `golang`, `local-first`. Drives discovery via GitHub topic search
  and any registry that scrapes topics.
- [ ] **Submit to Anthropic's MCP registry/directory.** Lowest-effort,
  highest-relevance distribution channel available. (prepared: Dockerfile
  label + server.json in-repo; publish when the registry's current submission
  flow and release are ready)
- [x] **Add a short README section addressing "why not just use \[platform]'s
  built-in memory" head-on**, near the top rather than implied by the
  comparison table. The new **Why Ghost?** section states the cross-client,
  local-first differentiator directly; detailed comparisons now live in
  [`docs/benchmarks.md`](benchmarks.md).
- [x] **Wire LongMemEval-S into CI** — shipped PRs #193/#211/#213:
  `.github/workflows/longmemeval.yml` gates PRs on the fts floor
  (R@5 ≥ 0.74, NDCG@10 ≥ 0.72, path-filtered to `internal/memory/**`,
  `bench/longmemeval/**`); hybrid (R@5 ≥ 0.91) is manual
  `workflow_dispatch`-only because the cold embedding pass is too slow to
  gate or schedule. See docs/benchmarks.md "Phase 1".

---

## Part 2 — Now / Next: marketing & distribution

**Positioning:** lead with *"one memory, across every MCP client, on your own
disk"* — not *"better than Claude Code's built-in memory."* The first is a
gap nothing else fills. The second is a claim Anthropic itself can erode at
any time by upgrading Claude Code's own memory.

**Channels, in priority order for a solo maintainer:**

1. **Show HN** — the published negative result (graph-bonus regression,
   disabled after failing your own sweep) and per-question benchmark logs are
   exactly what lands there. Title around the cross-client gap, not the tech
   stack.
2. **r/LocalLLaMA + MCP-adjacent communities** — audience is pre-sold on
   "local-first" as a value.
3. **Outreach to existing comparison content** (Mem0-alternatives roundups,
   MCP server listicles) — offer the benchmark numbers; several of these are
   actively looking for genuinely local-first, rigorously-benchmarked
   entries.
4. **`awesome-mcp-servers`-style lists** — a PR, not a campaign.
5. **Cross-promotion with Superpowers** (already referenced as complementary
   in the README) — warmer audience than cold discovery.

**Explicitly skip:** Product Hunt (wrong audience for infra), paid ads (no
funnel to justify it), SEO content war against funded competitors (unwinnable
on volume — win on rigor and citations instead).

---

## Part 3 — Now: benchmark infrastructure

**Status: shipped.** `LongMemEval-S` now runs in CI (see the Part 1 checkbox
above). Phase 1 (retrieval-only), Phase 2 (`ghost bench`), Phase 3 (staleness
suite), and Phase 4 (end-to-end retrieve→generate→judge) numbers are all
published in docs/benchmarks.md. What remains below is the design rationale
and one optional alternative.

- Runs retrieval-only (no LLM judge) — **no paid API calls required**.
- Dataset is open on Hugging Face, no gating — pull `longmemeval_s_cleaned.json`.
- Embedding step (`nomic-embed-text`, ~137M params) runs fine on CPU.
- **GitHub Actions public-repository runners are the natural home for the job.**
  They are currently available at no charge to the repository under GitHub's
  applicable terms, but usage limits and policy can change, so do not treat
  "free" or "unlimited" as a permanent platform guarantee.
- Workflow sketch: checkout → install Ollama → pull embed model → download
  dataset → run harness → fail PR on regression, same pattern as the existing
  dev-facts `ghost bench` CI gate. (Implemented; the PR gate runs
  fts-only and needs no Ollama, while hybrid stays manual
  (`workflow_dispatch`) because its cold embedding pass is hours-long.)
- **Optional persistent alternative (not pursued):** Oracle Cloud "Always Free" Ampere A1
  (4 OCPU / 24GB RAM, ARM, free indefinitely as of 2026) if a long-lived
  Ollama instance is preferred over ephemeral CI runs. Caveats: those
  instances are in high demand and may need a retry loop to provision, and
  there are scattered 2025–2026 reports of accounts flagged for heavy
  automation-style use — keep usage modest if this route is taken.

---

## Part 4 — Next: team-mode architecture (trigger: a team actually asks for this)

Core problem: current design is intentionally single-writer, local, and
pull-based (`SetMaxOpenConns(1)`, stdio transport, path-prefix project
resolution). Team mode is a second deployment mode alongside solo mode, not a
flag on top of it.

- [ ] **New `MemoryStore` implementation** behind the existing `provider.go`
  interface — networked backend (Postgres, or `libsql`/Turso for SQLite-like
  semantics with real multi-writer support) — instead of rewriting the MCP
  server layer.
- [ ] **`ghost serve`** — long-running daemon, MCP over HTTP/SSE instead of
  stdio. Solo mode keeps spawning a local process; team mode connects to a
  service.
- [ ] **Identity via Tailscale (`tsnet`)** rather than building API keys/OAuth
  — pull caller identity from `tailscale whois` on each connection. Fits the
  existing "no accounts" ethos and the fact that `audit_log.user` is already
  present but unused in solo mode.
- [ ] **Scope layer**: `project → team → _global`, with a visibility flag
  (private-to-me / shared-to-team) on memories. Likely defaults: `preference`
  / `gotcha` private, `decision` / `convention` shared — falls out of the
  existing category system.
- [ ] **Write-time conflict handling**: extend the existing save-time
  FTS-overlap duplicate detection and link behavior with conflict semantics
  for concurrent writers. Single-process saves already run through
  `MemoryStore.Upsert`; the missing piece is coordination when multiple writers
  can update the same project.
- [ ] **Review-gated `reflect --apply` for shared scope** — the
  snapshot/restore machinery already exists; the missing piece is a diff a
  team lead approves before a consolidation lands on shared memory, rather
  than an immediate atomic replace.
- [ ] **Deployment**: Helm chart, one pod in k3s, PVC or Postgres in place of
  the bare SQLite file, Tailscale for the network boundary, Grafana panels
  off `audit_log` and the reserved `token_usage` accounting schema (already
  shaped for this).

---

## Part 5 — Next (gated behind Part 4 demand): compliance controls

Only pursue if a team-mode customer's procurement actually requires it — these
are good practice regardless, but not worth building speculatively.

- [ ] Encryption at rest (SQLCipher, or native TDE if backend moves to Postgres)
- [ ] TLS at the application layer for `ghost serve` (don't rely on Tailscale
  alone — auditors want to see it explicitly)
- [ ] RBAC enforcement, not just the visibility flag from Part 4
- [ ] Tamper-evident audit log — hash-chained rows, or ship `audit_log` to an
  external immutable sink (Loki/Grafana stack is a legitimate answer here)
- [ ] Documented right-to-erasure path — note `memory_snapshots` is in direct
  tension with "final deletion" and needs an explicit carve-out
- [ ] Data Processing Agreement / subprocessor disclosure before offering the
  reflection tier to any enterprise customer

**Explicit non-goal until there's a paying customer requiring it:** actual
SOC 2 / ISO / HIPAA attestation. These are organizational, not code,
undertakings (3–12 months of operating under written policies before an
auditor will look, five-to-six-figure engagement cost, recurring annually).
Don't build toward this speculatively.

---

## Part 6 — Next: monetization (open-core, staying Apache-2.0)

**Hard constraint:** no relicensing bait-and-switch (no SSPL/BSL-style move
against cloud providers later) — that's explicitly off the table per the
"staying open source" requirement, and it's also just consistent with the
software's whole "own your data" pitch.

| Tier | Contents | License | Buyer |
|---|---|---|---|
| **OSS (free, forever)** | Solo binary, local SQLite, all current MCP tools, embedding/linking/consolidation, Obsidian mirror | Apache-2.0 | Individual devs — this stays complete on its own, no crippling |
| **Team (paid, self-hosted)** | `ghost serve`, RBAC, audit-to-SIEM export, reflect-diff review console | Can be closed/separate repo | Teams running it on their own infra |
| **Managed (paid, hosted)** | Same as Team, operated for the customer | — | Teams who don't want to run it themselves |

Decide pricing/packaging only once a real team asks to pay for team mode —
right now the bottleneck is adoption, not pricing structure.

---

## Part 7 — Exploratory (not a commitment): beyond coding-agent memory

### 7a. "Everyday memory" generalization

- Technically feasible with moderate rework: category enum and
  path-prefix project resolution are the only two things actually
  coupled to "coding agent"; the store/decay/search/consolidation engine
  is already domain-agnostic.
- Strategically high-risk: as of 2026 all major consumer platforms
  (ChatGPT, Claude.ai, Gemini, Grok, Copilot) ship native memory, several
  now free-tier and human-editable — the exact trust pitch Ghost makes.
  Multiple dedicated cross-platform "memory bridge" startups already
  occupy the "portable, private memory" niche too.
- Only real differentiation left there is local-first + genuinely
  cross-platform — but capturing it means building a consumer app (sync,
  mobile client, non-developer trust model), not a fork of the current
  product. Treat as "new product that reuses the DB core," not a Ghost
  feature.
- **Note:** this assessment is about consumer-facing chat memory
  (ChatGPT/Claude.ai/Gemini). Whether Claude Code's own project memory
  specifically gets a comparable upgrade is a separate, open question —
  worth rechecking before leaning harder on the current competitive
  framing.

### 7b. Robotics / IoT memory

- Real, currently unsolved gap in the field: existing LLM-agent memory
  systems (Ghost's whole category, alongside Mem0/Zep) are built around
  text-based conversational agents and don't natively handle spatial
  coordinates or multimodal perception — which is what embodied agents
  actually need. This is still an active academic research area through
  2026, not a solved integration problem.
- Where Ghost's shape genuinely fits: the *cognitive/episodic* layer
  feeding a robot's task-planning LLM (decision log w/ rationale, learned
  fault patterns as `gotcha`-equivalents) — sitting *above* a separate
  spatial/perception stack (SLAM, 3D scene graphs), not replacing it.
- Fleet-sharing (many robots, one shared "this hallway has a step" fact)
  is architecturally the same problem as Part 4's team mode — same
  conflict-resolution/merge logic, robots instead of humans as writers.
- Real gaps: `content TEXT` has no home for spatial/multimodal data
  (schema extension required); realistic IoT target is SBC-class edge
  (Pi/Jetson/hub), not deeply embedded microcontrollers.
- **Framing:** treat as a research side-branch (possibly a fork/new store
  implementation under the existing `memory/` package boundary) if the
  problem itself is personally interesting — not a go-to-market move. It's
  a harder problem than current coding-agent memory, competing against
  active robotics research labs, not a bigger adjacent market to expand
  into cheaply.

---

## Part 8 — Technical debt / audit findings to track

- **Schema migrations are versioned, but migration coverage still needs
  discipline.** `internal/memory/migrate.go` carries the schema version and
  step-by-step upgrades, with foreign-key checks and derived-cache repair;
  `schema.go`'s `CREATE TABLE IF NOT EXISTS` statement alone still does not
  alter an existing database. Every future schema change must append and test
  a migration, and broader rollback/repair tooling remains future work.
- **Pure-Go SQLite (`modernc.org/sqlite`)** — right call for a static
  cross-platform binary, but worth watching for FTS5/write-throughput edge
  cases if usage patterns ever get more concurrent than "one dev, one
  binary."
- **`ghost supersede`'s relation classifier is validated on only 10 labeled
  examples (10/10 single-pair and batched in local runs).** Fine as an
  initial signal, too small an n to lean on as a benchmark claim — revisit
  with a larger labeled set before citing it more heavily.
- **Content schema is TEXT-only.** No path for spatial/multimodal data —
  relevant if Part 7b is ever pursued, irrelevant otherwise.

---

## Part 9 — Architecture direction: memory axes and context assembly (P0–P3)

> Added 2026-09-25 from an architecture-direction review of `main` (`cab4236`).
> The target design is documented in [`architecture.md`](architecture.md#memory-axes)
> and [`architecture.md`](architecture.md#context-assembly-target-design). This
> section is the sequence; the issues are the work. Nothing here is a commitment
> beyond P0, which is correctness work against already-shipped behavior.

Verified as already delivered and therefore not repeated below: repository
identity across checkouts ([#539](https://github.com/wcatz/ghost/pull/539)),
machine-readable scope ([#562](https://github.com/wcatz/ghost/pull/562),
[#563](https://github.com/wcatz/ghost/pull/563)), identity-based snapshot restore
([#564](https://github.com/wcatz/ghost/pull/564)), and the GhostMem rebrand
(`672fb42`, [#525](https://github.com/wcatz/ghost/pull/525)).

### P0 — correctness first (make the axes mean something)

The four axes are named in [`architecture.md`](architecture.md#memory-axes). Validity and confidence are inert today; scope is partially implemented because it is persisted and searchable, but session-start injection still ignores it. P0 is the smallest set of changes that makes those axes mean what the schema says they mean, and stops two active defects.

| Issue | Why it is P0 |
|---|---|
| [#575](https://github.com/wcatz/ghost/issues/575) | Validity is preserved by snapshot replacement/restore but is not exposed or consulted by normal retrieval; confidence is written but not read by ranking |
| [#573](https://github.com/wcatz/ghost/issues/573) | Scope post-filter runs over an unnarrowed window, so filtered results are already lossy |
| [#574](https://github.com/wcatz/ghost/issues/574) | The linker's `related` edges bypass the scope exemption added in #563 |
| [#571](https://github.com/wcatz/ghost/issues/571) | Explain mode reports rows the tool would exclude |
| [#577](https://github.com/wcatz/ghost/issues/577) | Session-start injection neither renders nor filters scope — the surface that most needs it |
| [#579](https://github.com/wcatz/ghost/issues/579) | Define the four axes and their invariants; the documentation half can land first and this section already starts it |
| [#580](https://github.com/wcatz/ghost/issues/580) | Retrieval never abstains: weak matches are returned as if authoritative |
| [#588](https://github.com/wcatz/ghost/issues/588) | **Bug.** Every lifecycle call leaks a `[ghost]` OpenCode session into the user's session list (6,397 measured); needs an isolated data dir plus post-call deletion |

### P1 — one context assembler, explainable

| Issue | Why it is P1 |
|---|---|
| [#581](https://github.com/wcatz/ghost/issues/581) | The target pipeline: retrieve → validity → scope → provenance → conflicts → dedup → diversity → budget → render, with a per-stage trace |
| [#583](https://github.com/wcatz/ghost/issues/583) | Explain reports the pipeline's own computations (validity, confidence, scope, contradiction, diversity) instead of only RRF mechanics |
| [#578](https://github.com/wcatz/ghost/issues/578) | Append-only `memory_provenance`: who changed what, when — provenance is a mutable column with no history today |
| [#584](https://github.com/wcatz/ghost/issues/584) | Stress the documented multi-process contract with the real entry points (MCP + CLI + maintenance), including read-snapshot-across-write and batch atomicity |
| [#585](https://github.com/wcatz/ghost/issues/585) | Adversarial fixtures for the import, host-event, and Obsidian parse paths, defending the fixes in #545/#546/#552/#553 |

### P2 — data ownership

| Issue | Why it is P2 |
|---|---|
| [#586](https://github.com/wcatz/ghost/issues/586) | First-class backup/export/import (online backup API, inspectable artifact, verified restore) instead of "copy the data dir"; prerequisite for merging two machines' databases |
| [#587](https://github.com/wcatz/ghost/issues/587) | Retention/ownership tiers: `session` expires, `project` is the default, `persistent` is exempt from consolidation and pruning |
| [#582](https://github.com/wcatz/ghost/issues/582) | Context-quality metrics (precision, contamination, budget adherence, diversity, token cost) — coordinate with [#561](https://github.com/wcatz/ghost/issues/561) rather than duplicating its methodology fixes |

P2 depends on P1: export must serialise the axes, pruning needs provenance to
audit it, and context metrics only make sense once stages 2–8 are one pipeline.

### P3 — gated hardening

| Issue | Why it is P3 |
|---|---|
| [#558](https://github.com/wcatz/ghost/issues/558) | Release signing: checksums are published but never signed, so `ghost upgrade` cannot verify provenance; gated on the upgrade path being worth a signature format |
| [#542](https://github.com/wcatz/ghost/issues/542) | Data-dir growth (pre-migrate backups, unrotated logs, stale lock files) — becomes tractable once #586 gives backups a retention policy |
| [#552](https://github.com/wcatz/ghost/issues/552) / [#545](https://github.com/wcatz/ghost/issues/545) / [#546](https://github.com/wcatz/ghost/issues/546) | Harness env allowlist and the two reflection/resolution privilege findings; the adversarial fixtures in #585 are the regression net |

Team mode (Part 4) and compliance controls (Part 5) remain gated on real demand
and are unaffected by this sequence.

---

## Closing principle

Keep the free tier genuinely complete — it's the trust anchor, not a lure.
Don't chase compliance or enterprise packaging ahead of real demand. Don't
relicense. The project's actual edge right now is small, honest, and
well-benchmarked — protect that before reaching for a bigger market.
