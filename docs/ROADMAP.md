# Ghost — Future Work & Strategic Roadmap

> Working notes from a July 2026 audit and strategy session. This is a planning
> document, not a commitment — items move between horizons as real usage and
> demand show up. Suggested location: `docs/ROADMAP.md`.

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

- [x] **Fix the repo's "About" description.** GitHub's meta description for
  the repo currently reads *"Memory-first personal assistant daemon"* — stale
  relative to the README's actual tagline (*"MCP memory server for Claude
  Code, Cursor, and any MCP client. Pure Go. Single binary."*). This is what
  shows up in search snippets and social shares, ahead of anyone reading the
  README itself. Two-minute fix, outsized effect.
- [x] **Add GitHub topics**: `mcp`, `mcp-server`, `memory`, `claude-code`,
  `sqlite`, `golang`, `local-first`. Drives discovery via GitHub topic search
  and any registry that scrapes topics.
- [ ] **Submit to Anthropic's MCP registry/directory.** Lowest-effort,
  highest-relevance distribution channel available.
- [ ] **Add a short README section addressing "why not just use \[platform]'s
  built-in memory" head-on**, near the top rather than implied by the
  comparison table. The honest answer: native memory (ChatGPT, Claude, Gemini)
  is walled inside each product — none of them read each other. That's
  Ghost's actual differentiator (local + cross-client), and it should be
  stated outright, not left for the reader to infer from a feature table.
- [ ] **Wire LongMemEval-S into CI** (see Part 3) so the flagship benchmark
  claim is continuously regression-guarded, not just a one-time run.

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

Goal: `LongMemEval-S` runs in CI the same way `ghost bench --sweep` already
does for the smaller dataset.

- Runs retrieval-only (no LLM judge) — **no paid API calls required**.
- Dataset is open on Hugging Face, no gating — pull `longmemeval_s_cleaned.json`.
- Embedding step (`nomic-embed-text`, ~137M params) runs fine on CPU.
- **GitHub Actions is free and unlimited on public-repo standard runners** —
  no minute cap, unlike the 2,000 min/month private-repo allowance. This is
  the natural home for the job.
- Workflow sketch: checkout → install Ollama → pull embed model → download
  dataset → run harness → fail PR on regression, same pattern as the existing
  dev-facts `ghost bench` CI gate.
- **Optional persistent alternative:** Oracle Cloud "Always Free" Ampere A1
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
- [ ] **Write-time conflict handling**: extend the existing FTS-overlap
  dedup/upsert logic (currently a `reflect`-time operation) to run on every
  concurrent save, not just during consolidation.
- [ ] **Review-gated `reflect --apply` for shared scope** — the
  snapshot/restore machinery already exists; the missing piece is a diff a
  team lead approves before a consolidation lands on shared memory, rather
  than an immediate atomic replace.
- [ ] **Deployment**: Helm chart, one pod in k3s, PVC or Postgres in place of
  the bare SQLite file, Tailscale for the network boundary, Grafana panels
  off `token_usage` and `audit_log` (already shaped for this).

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
  Haiku reflection tier to any enterprise customer
- [ ] Secrets manager for `ANTHROPIC_API_KEY` in shared deployments (Vault, or
  k8s External Secrets) instead of a bare env var

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

- **No schema migration tooling.** `CREATE TABLE IF NOT EXISTS` never
  migrates an existing database — already flagged in `schema.go`'s own
  comments, but there's no versioned migration path or version table yet.
  Needs one before a schema change actually breaks an early adopter's DB.
- **Pure-Go SQLite (`modernc.org/sqlite`)** — right call for a static
  cross-platform binary, but worth watching for FTS5/write-throughput edge
  cases if usage patterns ever get more concurrent than "one dev, one
  binary."
- **`ghost supersede`'s Haiku classifier is validated on only 8/8 labeled
  examples.** Fine as an initial signal, too small an n to lean on as a
  benchmark claim — revisit with a larger labeled set before citing it
  more heavily.
- **Content schema is TEXT-only.** No path for spatial/multimodal data —
  relevant if Part 7b is ever pursued, irrelevant otherwise.

---

## Closing principle

Keep the free tier genuinely complete — it's the trust anchor, not a lure.
Don't chase compliance or enterprise packaging ahead of real demand. Don't
relicense. The project's actual edge right now is small, honest, and
well-benchmarked — protect that before reaching for a bigger market.
