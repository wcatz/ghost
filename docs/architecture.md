# Ghost architecture

This document is for contributors and maintainers. For installation and everyday use, start with [`installation.md`](installation.md), [`usage.md`](usage.md), and [`cli.md`](cli.md).

## Design goals

Ghost is intentionally:

- **Multi-process and local-first:** one SQLite database that any number of Ghost processes may open concurrently; SQLite is the synchronization layer. See [Concurrency contract](#concurrency-contract).
- **Pull-based during normal MCP use:** the server exposes tools and resources; it does not inject an LLM call into ordinary memory reads.
- **CGO-free:** `modernc.org/sqlite` supplies SQLite and FTS5 without a C toolchain.
- **Host-aware:** lifecycle events are normalized into a small contract shared by Claude Code, opencode, Codex, and Goose.
- **Source-aware for maintenance:** reflect, resolve, and supersede route through the calling session's CLI harness rather than silently selecting another harness or billing path.

## Runtime modes

The same binary provides several modes:

```text
ghost mcp                         MCP server over stdio
ghost mcp init                    Configure MCP clients
ghost mcp status                  Check client and store health, and list
                                   projects with no bound checkout
ghost hook <event> --source <host> Normalize a host lifecycle event
ghost reflect <project>           Consolidate memories
ghost resolve <project>           Mark resolved evidence
                                   (--reassess re-judges the
                                   resolutions already made; --only
                                   narrows that to named ids)
ghost supersede <project>         Classify replacement relationships
                                   (--reassess re-judges the edges
                                   already in the graph; --withdraw
                                   removes one named edge)
ghost lifecycle <project>         Run the detached maintenance phases
ghost project delete|merge        Manage project records
ghost project bind <id> <path>     Record a project checkout so a session in
                                   that directory resolves it
ghost obsidian export|sync        Mirror the store to Markdown
ghost opencode cleanup-sessions   One-shot cleanup of lifecycle sessions
                                   titled exactly "[ghost]"
ghost backup                      Snapshot the live database (VACUUM INTO)
                                   plus a sidecar manifest describing it
ghost backup verify <file>        Check a backup against that manifest
ghost export                      Write the store as a portable JSONL artifact
ghost import <file> [--apply]     Load a JSONL artifact (dry-run by default)
ghost context                     Render passive session context
ghost context --as-of <RFC3339>   Render it as the store stood at an instant
ghost history <memory-id>         Print one memory's append-only history
ghost history purge <memory-id>   Erase a memory and every recorded version of it
ghost history compact [--apply]   Remove history versions that changed nothing (bounded by --before)
ghost bench [--sweep|--context|--passive]   Run the built-in benchmark
ghost upgrade                     Update a standalone binary
ghost version                     Print the version
```

The MCP server is the primary mode. It starts embedding and linking workers when enabled, but those workers do not make LLM calls.

## Package map

```text
cmd/ghost/                         CLI entrypoint and command dispatch

internal/ai/                        Source-aware CLI harness adapters
  provider.go                      Provider and TokenUsage contracts
  source_detect.go                 Environment/process source detection
  source_provider.go               Routes to the caller's harness
  cli_client.go                    Claude-compatible CLI adapter
  opencode_client.go               OpenCode V1/V2 adapter
  opencode_cleanup.go              One-shot "[ghost]" session backlog cleanup
  codex_client.go                  Codex adapter
  goose_client.go                  Goose adapter
  scratch.go                       Harness scratch/temp handling

internal/adversarial/               Test-only: shared hostile-input corpus and
                                     the inert-content invariant (#585)
internal/bench/                     Built-in retrieval benchmark and sweeps
internal/claudeimport/              One-time Claude Code memory import
internal/config/                    Layered YAML/environment configuration
internal/embedding/                 Optional Ollama embedding client/worker
internal/hostevent/                 Normalized host-event contract and scanners
internal/linking/                   Background related-memory linking worker
internal/mcpinit/                   Client installers, status checks, hooks
internal/mcpserver/                 MCP server, tools, resources, prompts
internal/memory/                    SQLite store, FTS5, vectors, links, schema
internal/obsidian/                  One-way Markdown vault exporter/sync
internal/portable/                  Portable JSONL artifact: read (export) and
                                    write (import) of the portable record format
internal/procstat/                  Cross-platform process liveness/start time
internal/provider/                  MemoryStore and LLMProvider interfaces
internal/reflection/                Tiered memory consolidation
internal/resolve/                   Resolved-evidence classifier and cache
internal/scratch/                   Ghost-owned scratch root cleanup
internal/secret/                    Credential-shape detector for stored text
internal/selfupdate/                Checksum-verified GitHub release updater
internal/supersede/                 Directed supersession relation classifier

internal/followup/                  The command that completes a supersede repair, and the one renderer for the ids it cannot carry
```

The database schema is an embedded Go string constant in `internal/memory/schema.go`; it is the single source of truth for the store schema. `OpenDB` reads `PRAGMA user_version` and refuses a store from a newer Ghost **before** it runs any DDL against it, not after: `initSQL` is `CREATE … IF NOT EXISTS`, so on a store that merely has more than this build knows it is nearly a no-op — which is exactly what hid the ordering — but any object the newer build renamed, replaced or dropped is recreated here, in a store the same call then declares unreadable. A refusal that has already written is not a refusal. A fresh store has no stamp to read and is initialized and stamped instead; a store behind is backed up, then migrated.

## Host integration

`internal/mcpinit` owns the host-specific setup paths:

- Claude Code: MCP registration, permissions, SessionStart/Stop hooks, file-memory migration, and redirects.
- opencode: one lifecycle TypeScript adapter that registers MCP and bridges idle events.
- Codex: `config.toml` registration plus `hooks.json` entries that require user trust.
- Goose: an Agent Plugins package with MCP and Open Plugins hooks.

All hook paths converge on `internal/hostevent`, which parses the versioned event envelope and dispatches normalized events. The `scratch` and `procstat` packages keep harness scratch and detached-process liveness handling separate from host adapters.

Every user-owned config write — the Claude `settings.json`, the codex `config.toml` and `hooks.json`, the opencode plugin, the goose package and the MEMORY.md redirect — goes through one atomic path (temp file in the same directory, then rename, under a random hidden name), so an interrupted write cannot leave a half-written host config, and a host reading a config while init repairs it reads a whole document rather than a truncated one. The rename alone only orders that for a *reader*; a *power cut* is answered by the two flushes around it: the temp file is fsynced (its data and its mode together) before the rename, and the parent directory is fsynced after it, because a rename that reaches the disk before the bytes it points at leaves a config that reads as empty or truncated, and a directory entry that never reaches the disk leaves the file gone even though its contents were written. A temp-file flush that fails stops the write and is reported — nothing has been published yet, so the user's config is still the one that was there. A directory flush that fails is not reported: the rename already published the document, there is nothing left to refuse, and the alternative is failing an init that did its job on a filesystem that cannot flush a directory at all (Windows has no equivalent — `FlushFileBuffers` on a directory handle fails and NTFS orders that metadata itself — so the directory flush is a no-op there). A symlinked config is written through to its target rather than replaced by it; a link whose target does not exist yet is the one case that cannot be, and is replaced by a regular file. A rename swaps in a new inode, so the permission bits are copied across but the owner, POSIX ACLs, xattrs and hardlinks are not: a config owned by another account changes owner, its ACL entries are dropped for the directory's defaults, and a config hardlinked into a dotfiles repo instead of symlinked silently diverges from its repo copy. An existing file keeps its own permissions, so a repair never widens or narrows a mode the user chose, except on the Claude side: `settings.json` is clamped so it can never be group- or world-readable whatever mode the user's copy had. That clamp is backwards compatibility, not a credential policy — it is the ceiling every save enforced before the refactor, and a copy narrower than `0600` is left at its own narrower mode. The other files keep a wider mode on purpose, including a codex `config.toml` whose `[mcp_servers.ghost.env]` sub-table can hold a credential. The Claude `settings.json.bak` backup is captured once, by the first save that finds a file to back up, so repeated `init` runs never overwrite the pristine original with ghost's own output. No other user-owned config is backed up: a `hooks.json` repair that merged wrongly can only be repaired by hand, and a once-only `.bak` for the merged hosts is still open.

If a host reports an unknown source, `internal/ai` does not cascade to a default harness. The caller must provide a source or the operation fails with an actionable error. This prevents an opencode or Claude session from silently using the wrong harness or billing path. OpenCode children receive an explicit `opencode/big-pickle` model unless a per-phase pin or `GHOST_OPENCODE_MODEL` override is supplied; the child home/config tree is invocation-owned, only a configured `auth.json` is carried into its data root, and its tool/MCP policy is deny-all, so the user's global OpenCode model, plugins, and MCP servers are not inherited.

**The prompt travels on stdin, never as an argv element.** Every adapter spawns its harness with the prompt on the child's standard input and no positional message, because the kernel caps a single argument at 32 pages — 128 KiB on a 4 KiB-page x86, 512 KiB on a 16 KiB-page one — and a reflect prompt is assembled from up to 2000 memories of 8000 bytes, so a large project produced a prompt that failed the spawn outright with `E2BIG` rather than an answer. Each harness reads it the same way: `claude -p` and `opencode run` take the message from a pipe when no positional message is given, and codex and goose are given their explicit stdin sentinels (`codex exec -`, `goose run -i -`). A flag argument stays an argument where it is small, fixed, and must not be confusable with user content: `claude --system-prompt` carries the classify rubric while the untrusted user content is piped.

## Data flow

### Normal MCP session

```text
MCP client
  → stdio JSON-RPC
  → internal/mcpserver
  → provider.MemoryStore
  → SQLite / FTS5
```

The MCP server registers 23 tools, 4 resources, and 2 prompts. Core memory CRUD and search tools do not invoke an LLM; the maintenance-oriented `ghost_resolve` and lifecycle paths can invoke the selected CLI harness. Resources expose project context, decisions, tasks, and global memories for clients that support resource pinning.

### Session start

```text
host SessionStart
  → ghost hook session-start --source <host>
  → resolve project by longest path prefix/name
  → rank and bound the context digest
  → write the digest to the host
```

opencode uses `ghost context` because it cannot consume the hook's stdout injection directly. The OpenCode adapter injects that rendered block as instructions instead.

### Stop and maintenance

```text
host Stop
  → ghost hook stop --source <host>
  → save reminder / bounded host behavior
  → cooldown check (lifecycle.min_interval vs lifecycle-<project>.last mtime)
  → optional detached ghost lifecycle <project>
       → writes lifecycle-<project>.last
       → reflect
       → resolve
       → supersede
```

The lifecycle is opt-in. A phase failure is logged and does not prevent later phases from running. The reflect phase can use a source-matched CLI harness or an explicitly selected offline tier; the autonomous path requires a real harness when it is configured to rewrite memories.

The cooldown exists because the hook fires once per turn, so a chain per turn is a chain per turn. Two independent guards bound it: the per-project PID file (`AcquireLifecycleLock` — a run is not already in progress) and `lifecycle.min_interval` (a run did not start too recently). The second is a file's mtime rather than a row, because the hook's synchronous path must not open the database to ask the question. The `ghost lifecycle` process writes it, once it has passed every precondition that can exit non-zero — so a run that dies before doing any work does not burn the window, and does not add to the silent-death class the failure marker exists to catch. A foreground run writes it too, which is what a user retrying the alert's command wants.

### The supersede pass, and why it is KEEP-biased

`ghost supersede` proposes newer→older `supersedes` edges over cosine-similar pairs and asks the calling session's CLI harness to judge each one, four ways: `supersedes`, `causes`, `reversed` (refused — the older note is the current one) and `neither`. The edge is not informational, and that is the whole design constraint: `SupersedePenalties` demotes the older endpoint in ranking, and `ghost resolve`'s supersedes piggyback stamps `resolved_at` on it for free, so a wrong edge takes a live memory out of every later session's context, and — the pair being held quiet by skip-if-unchanged until an endpoint moves — nothing in the ordinary pass will look at it again.

An independent judge graded the edges one real v0.35.0 dry run proposed over a real project store — 253 candidate pairs, 32 proposed edges, **43% precision** ([#686](https://github.com/wcatz/ghost/issues/686)). There were no direction errors any more; every wrong edge joined two notes that were **both still true**, sharing a topic (a follow-up, an addendum, a restatement, a partial fix of one detail) with neither replacing the other. Three rules answer that, and they are KEEP-biased for resolve's reason — a missed supersession leaves a stale note ranked and a later pass can still link it, while a false one buries a memory:

- **The falsity question, asked.** The rubric asks whether the older note's claim is false or no longer applicable *after* the newer note, and says outright that two notes both still true are `neither` even on the same topic.
- **A named retired claim.** A `supersedes` answer must carry `replaced: <the older note's claim that no longer holds>`; a missing, empty or placeholder value reads `neither`, which is a decision and therefore cacheable — and so does a value that OPENS by declining to name one ("not applicable", "no such claim", "cannot name it", "unclear"), because a refusal is the likeliest way a model says it cannot tell you what stopped being true, and a nine-word placeholder list does not catch it. `replaced:` is the twin of resolve's `closed-by:` and is read by the same grammar (`resolve.ReasonedField`, `resolve.ReasonlessValue`), so a value that keeps a note in one pass's injection cannot bury it in the other's ranking. The NEITHER cache key prefix moved with the rule: a cache hit is a permanent skip for the life of that text, so verdicts judged without these rules are re-asked.
- **A free veto, before the call.** `VetoSupersede` settles a pair whose older note states a standing rule — an imperative from resolve's own vocabulary — and whose newer note never names that rule as retired or changed. No call, no link, no cache row, and the count is on the report. Its second half is what makes the first safe: "the no-merge rule is retired" is a real supersession of an imperative and reaches the classifier, because whether the retirement covers the *same* rule is the judgement the veto refuses to make. A false veto costs a stale-but-visible note, which is the cheap direction, and the labeled regression set pins that cost on a real [#641](https://github.com/wcatz/ghost/issues/641) fixture whose older note says a build "never gets past" a missing fix.

**Those three rules answered #686's question and left #779's.** Re-measured over three dry-run passes each on copies of three more real stores, the pass proposed 108 distinct edges at **55% precision** — 0.79 among edges proposed in all three runs, 0.33 among those proposed in one, which is the shape of an unstable classifier rather than a wrong one. Every wrong edge was again a pair whose two notes are both still true, in four classes, and all four are cases where the newer note is *plainly about* the older one, so a model reading for "did this change that?" answers `supersedes`:

1. **Partial-claim.** An older note states several independent claims; the newer one resolves one of them and is credited with retiring the whole note.
2. **A log read as a chain.** "vX released" / "vY released", status lines, incident entries — every one still true, none made untrue by the next.
3. **A recurring defect read as a fix chain.** The same failure seen twice, which is one still-open problem, not a bug and its fix.
4. **Parallel investigation read as a linear chain.** Two notes on one problem, each about a different layer, with the timestamps implying an order the content does not have.

The rules answer them as *coverage*, not as a fifth verdict, because they are all the #686 question asked more precisely:

- **A supersession must retire EVERY claim of the older note, not one of them.** A newer note that retires some of them and leaves the rest standing is `neither` — **never `causes`**, even where it reads as an elaboration acting on the older one, because the `causes` criterion requires the older note's content to remain independently true and a partly-retired note by definition does not. A newer note that acts on an older one and retires *nothing* is `causes`; one that retires part of it and leaves the rest is not. The reason either way is the harm, restated: the edge demotes the WHOLE note, so an unretired claim leaves an agent's view in the same instant as the retired one. The `replaced:` field may then list several claims, semicolon-separated — which is a widening of the value, not a change of the format, so the parser and its golden reply contract are untouched. (What the parser cannot do is check that *every* claim was named: `requireReplaced` asks whether **a** claim is named, so a value truncated at the first semicolon reads the same as a complete one. The coverage requirement is the model's to follow.)
- **Three shapes are named outright**, because each of them is separately false and a general rule does not catch it: *a log entry is not a chain* (a note that is one entry in a release, status, changelog or incident log does not supersede an earlier entry in it **merely by being the next one**), *a recurring defect is not a fix chain* (only a note that says it is fixed, and fixes it, supersedes the note that reported it), and *parallel investigation is not a chain* (neither note retired the other; the timestamps say only which was written last).

  Each rule states its own exception to its own default, and the log rule's is the one worth reading twice: **the exception is an entry that reports the open issue CLOSED, and it overrides the log rule only — not the coverage rule above, which holds everywhere and outranks every bullet.** A status entry saying the fix shipped retires the earlier entry saying the build never got past it. **NARROWING IS NOT CLOSING**, stated explicitly because a partial retirement is the case the coverage rule already decides: a report that fixes one component while the rest of the blocker stands, or that is a second sighting of a problem still reproducing, is `neither` under the coverage rule rather than an exception here. `#641`'s labeled set is what forced that distinction, and it changed a label to match. Its own comment called the status-report pairs "supersede is at most right" — the bug it was fixing was the misused `causes` link, and `SUPERSEDES` was chosen as a verdict the prompt could be made to reach, not because anything was retired. `status-report-fix`'s later report genuinely does retire the earlier blocker and stays `SUPERSEDES`; **`status-report-divergence` was relabeled to `NEITHER` by this change**, because both its notes are OPEN reports of one still-reproducing problem and the later one only adds a second sighting. Leaving it `SUPERSEDES` would have meant the rubric contradicting its own labeled set, and the edge would point at a live OPEN report — again the one direction the KEEP-bias error argument does not cover. The precedence itself has to be *stated* rather than inferred, because a permissive clause beside an absolute one is a contradiction the model resolves by reading order, and resolving it the wrong way is not a recall miss: `Run` invalidates a live edge on a `neither` verdict and `--reassess` withdraws it. The two-status-reports case is therefore restated as **SUPERSEDES at most, never CAUSES** — never `causes`, which is the misused edge `#641` measured. `TestClassifyRubricAgreesWithItselfAboutTheStatusReportPair` holds every clause of that chain in both prompts, and holds each labeled case to a clause that can *reach* its label — a check the first version of that guard lacked, since it only re-read the labels and so passed while the prompt's own narrowing clause made the divergence pair unreachable.
- **The NEITHER cache key prefix moved again** (`v4`), for the same reason it moved for `#686`: every one of the three rules above only ever *narrowed* the set of answers that are `supersedes`, so every `#686`-era row is a verdict the current rubric would not necessarily give.
- **One labeled fixture per class**, added to the eval set, so the live eval scores the four shapes and not only the older ones. `TestClassifyRubricCarriesEverySupersedeRule` holds the shipped text to the four clauses and `TestEveryRubricRuleIsMeasuredByALabeledPair` holds the clauses to the fixtures in both directions — a clause with no fixture is a rule nothing scores, and a fixture with no clause is a pair the prompt says nothing about.

No deterministic rule was added alongside them, and that is a decision rather than an omission. A log-shaped or fix-chain-shaped pair *could* be recognised without a model, and the veto proves the shape of such a rule — but the veto's error argument is directional, and it does not survive being copied: on the repair path (`--reassess`) a false veto deletes a correct edge rather than costing recall, and it is deterministic, so the pair stays unlinked until a note changes. A 55%-precision classifier is a reason to ask the question again rather than to hard-code a second judgement about prose, and the rest of #779 is about which of those two the number supports.

**The other half of #779's proposal is the gate, and the measurement says what it has to be.** Read by agreement rather than by count, the same 108 proposals are **0.79** correct among the edges proposed in all three runs, **0.56** among those proposed in two, and **0.33** among those proposed in one. The number that predicted correctness was *how many runs proposed the edge*, so the gate requires **unanimity** over N passes rather than a majority: a 2-of-3 majority writes exactly the middle row. Each pass is an independent classify call over the same candidate set in the same order, so the only thing that can differ between two passes is the model's own answer to the same question — which is what "independent" has to mean here for the number to mean anything.

**Where the gate sits is what makes it affordable.** The N passes run *after* every free filter: the orientation refusals, the existence check, the scope and keep-forever exemptions, the imperative veto, the NEITHER cache and skip-if-unchanged. All of those are questions about a pair's *text and identity*, and none of them can answer differently on a second look at the same text, so re-running them per pass would buy nothing and the multiplier would land on pairs that were never going to be asked. The cost is therefore N × (pairs that survived), and on a converged project — every fresh pair cached, every live edge unchanged — the gate is close to free, which is the only reason an N-fold multiplier is a reasonable thing to put on the automatic path at all.

**And each pass is a real re-ask.** The cache is read once, before the first pass, and nothing writes a cache row until every pass is in hand and the apply block runs. Persisting pass 1's NEITHERs and letting pass 2 be served from them would be invisible in the report — the run would look like a model that changed its mind twice — and it would make the gate measure the cache rather than the model. `TestConsensusAsksEveryPass` exists for exactly that: the fake says NEITHER on pass 1 and SUPERSEDES on passes 2 and 3, and if any pass were served from a row the previous one wrote, the result would change.

Three outcomes come out of the gate, and keeping them apart is what makes it diagnosable. **Agreed** — every pass gave the same verdict, and the pass's ordinary handling of that verdict applies unchanged: a unanimous `supersedes` links, a unanimous `causes` links, a unanimous `neither` is cached (every pass agreeing *is* a decision, and caching it is what makes the next pass free), and a unanimous `reversed` is still refused and still never cached. **Not agreed** — the passes split, so nothing is written, nothing is withdrawn and nothing is cached; the pair is asked again next pass. It is deliberately not a withdrawal and not a reversal: N different answers is a statement about the model's stability, not about which endpoint is current. **Undecided** — no pass produced a readable verdict, which is `Result.Unclassified` and the pair is re-asked; calling that "not agreed" would report a prompt or harness fault as model instability and send an operator to raise the quorum for it. A gated dry run's extra hint is gated on the middle outcome alone, for the same reason: the other two reasons a gated run writes nothing — every pair vetoed, every pass unreadable — already have their own line and their own remedy on the page, and a block claiming instability for either would send an operator after the wrong thing.

A disagreement is reported with its evidence, not just counted: the ids, and the tally of what each pass said, **in descending vote count** so the line leads with the majority and a tie falls back to the fixed verdict order (supersedes, causes, neither, reversed, unreadable) and stays reproducible. "2 supersedes, 1 neither" is a different finding from "1 causes, 1 reversed, 1 unreadable", and an operator deciding whether to keep a gate needs to see which it is. The advice that goes with it is bounded by what the gate can actually do: **raising N makes unanimity harder, not easier**, because a pair that split 2-1 at N=3 has to satisfy one more pass at N=4, so the report offers the two remedies that are real — re-run, since fresh passes may land on the same answer, or drop the flag and write what the first pass said. The multiplier itself is reported on *every* return path, including the ones with nothing to ask, so a gated run on a quiet or fully-vetoed project is never indistinguishable on the page from an ungated one. The gate is off unless asked for on the CLI (`--consensus N`, `N` ≥ 2) and on for the automatic phase through `reflection.supersede_consensus`, which is read only when `auto_supersede` is true — and `auto_supersede` stays off, because a gate raises the precision of an unmeasured classifier and does not make it measured.



One pass judges one **pair**, not one direction, and the pair is the unordered one. That is the same error argument as above, read on the graph rather than on a verdict: a demotion lands on the target alone, so a cycle of two `supersedes` edges sinks BOTH memories of the pair and neither edge withdraws the other. A pass that carries `{A,B}` and `{B,A}` therefore proposes and can write both directions of one pair — measured on copies of three real projects, one pass proposed both `02EA044F supersedes 74CE9D10` and its reverse, and the same pair came back "reversed" in both orientations across passes, never once with only the right direction. `Run` reconciles the two sources on the unordered pair, because they disagree about direction by construction: the scan orients by timestamp, a live edge keeps the direction it was written with, and the old key was the ordered pair, so a disagreement matched nothing.

The live edge's direction wins, and that is chosen rather than arbitrary. The edge is the claim the graph already makes, and a `reversed` verdict has to be shown *it*: that is the wrong edge, and it is the one both this pass and `--reassess` have to reach in order to withdraw it. Judging the scan's orientation instead hands the classifier the other direction, so the verdict that would have withdrawn the #641 backwards link confirms the forward one and leaves the wrong edge in place. A scan proposal that contradicts a live edge is refused as a **direction**, and counted as such (`Result.OppositeLive`): the pair continues in the link's direction, and whether it is re-judged at all is then the ordinary skip-if-unchanged question, exactly as for any live edge whose endpoints have not moved. A proposal that *agrees* with a live edge is not refused and buys nothing either, and that is the ordinary case rather than an edge case: a `supersedes` edge is only ever written for a pair the scan found as cosine neighbours above the threshold, and writing it retires no vector, so every later pass proposes it again. The edge's own `created_at` is the freshness reference, not the scan, and it moves when a verdict re-confirms the edge — so an edge costs one classifier call per **endpoint edit** and none on a pass where nothing changed.

Two more refusals, both free, both counted because a pass that declined work and printed the totals of one that found nothing reads as "nothing was skipped". A pair the graph already claims in **both** directions — the state a pre-fix pass leaves — has no third direction to try, so the creation pass is not judged at all, and its report names `ghost supersede <project> --reassess --consensus 3 --apply` as the repair, since withdrawing an edge is not this pass's job (and the flagless `--reassess` is a dry run that withdraws nothing).

**Which means the repair has to be able to do it, and the first version of it could not.** `Reassess` loaded every live edge and judged each as an *independent candidate*, so a pair claimed in both directions was asked the same question twice, and a classifier that cannot decline a direction answered `supersedes` to both: `Confirmed 2, Withdrawn 0`, the cycle untouched, nothing said. The pass that exists to remove wrong edges was keeping the worst one there, silently, and the command the creation pass printed as its repair was a promise with nothing behind it. `Reassess` now groups the live edges by the unordered pair first, so a cycle is **one** question asked in the direction the timestamps give it — never the store's row order, which is not a promise — and reads the verdict as a *direction*: `supersedes` names one of the two live edges as current, so that edge stands and its reverse is withdrawn; `reversed` names the other one, so the other stands; `neither` and `causes` deny the replacement in either direction, so both go. Anything else — a failed call, an unparseable reply, or the #778 tie where both notes share every timestamp — decides nothing, and **nothing is withdrawn**: withdrawing half a cycle on a hunch would leave a live edge this pass never judged, which is exactly the state the cycle is a report about. The veto is skipped for a cycle on purpose, because it asks one orientation's question and a cycle has two over the same two bodies. And a pair whose two rows share **both** `updated_at` and `created_at` is not proposed: a bulk import stamps a whole batch at once (33 dingo rows share one `2026-09-20 09:26:05`), so the chronology is genuinely absent, and the old fallback — order by id — decided the direction of a real supersession by which of two random hex strings sorted higher. `created_at` is the tiebreak and never the primary ordering, because it mislabels a note re-saved long after it was created, which is exactly #641. A live edge naming a tied pair is still revalidated: the tie rules out *proposing* a direction, not *judging* one, because the edge already carries a direction and withholding that would freeze a wrong edge forever.

The measurement is a labeled eval, not an assertion: **32 synthetic pairs — 14 true supersessions and 18 both-true pairs**, covering every class the judge named plus one fixture per `#779` class — scored through the shipped decision, off by default like resolve's (`GHOST_LIVE_TESTS=1`). The free `opencode/big-pickle` read 1.00 precision and 1.00 recall on both the single-pair and the batched path, 2 both-true pairs settled by the veto without a call; before the many-fact clause was added the same set read 0.93/1.00. Precision is the gated number (≥ 0.90), recall is reported without a gate.

**Those figures are for the 28-pair set that preceded `#779`, and the four `#779` fixtures are not in them.** They were added with the rubric clauses meant to answer their classes, and a figure measured before a fixture existed says nothing about how the shipped rubric handles it — so quoting 1.00/1.00 as the KEEP-bias justification reads as a measurement of a set the four hardest NEITHER pairs were never scored against, and the figure **is not a measurement of the set that ships**. The live eval is what re-measures it, on the 32 pairs, and re-running it is a separate exercise from this change: the numbers belong in `docs/benchmarks.md` and the person measuring owns them. Until then the honest reading is "the rubric asks the question and the four classes are labeled and scored by a real harness on demand", not "1.00 on the shipped set".

Two repair paths exist because the ordinary pass will not re-open a decision whose endpoints have not changed: it holds an untouched edge quiet, so the graph keeps a wrong edge for as long as nobody edits it, and nothing short of an operator can end that. `ghost supersede <project> --reassess [--apply]` re-judges every live `supersedes` edge under these rules and withdraws the ones that come back `neither`, vetoed, `causes` or `reversed`, through `InvalidateLink` — and it takes `--consensus N`, so with that flag an edge the classifier decided moves only when all N passes name the same outcome — the VETOED ones do not, because that half spends no classify call — and a disagreement is reported with its tally instead (the same gate and the same measurement the creation pass uses; the default stays one pass so existing scripts are unchanged) — which writes the `unsupersede` history row, so an audit that shows a supersession with no withdrawal cannot read as though the stale claim is still live. A pair live in **both** directions is one question rather than two, and its report block is a list of ids because a cycle's repair is one `--withdraw` command per edge — see the paragraph above on why it cannot simply drop both. It prints each withdrawn edge with the rule that withdrew it and which of the two decided it (`veto, no harness call` for the deterministic half), a dry run says "would withdraw" per edge and reports the count it is about to print, and a failed invalidation still reports the edges that landed before it — each one is its own transaction, and a later pass will not see them again. The withdrawal also sweeps the other relation's edge, as the ordinary pass does on the same self-contradicting verdicts, because a `causes` link pointing into a note the pass just decided is still current asserts the opposite. That is a second graph row per withdrawal, so it is reported too: a `[+1 causes edge]` on the row and a sweep count on the summary line, predicted in a dry run and *observed* under `--apply` — the row carries what the sweep moved, never what it was going to move, so a concurrent pass that took the edge first reports 0 rather than claiming a deletion, and a sweep that *errored* is reported as unknown rather than as a count, because after a failed write the count is not knowable. The prediction is read through each pair's own endpoints, not through a project-scoped query, because the sweep deletes by id: a pair whose older note has been promoted to `_global` or moved by `ghost project merge` is still swept, and a project-scoped read cannot see it. A `causes` verdict is the one withdrawal that sweeps nothing — it affirms that relation instead.

One asymmetry is worth stating, because it is the only place the creation pass's error argument does not carry over. On the ordinary pass a false veto costs recall — a stale note stays ranked, and a later pass can still link it. On the repair pass it costs a correct edge, and since the veto is deterministic on the same two note bodies, the ordinary pass will re-fire it on every later run: the pair stays unlinked until one of the notes changes. That is the trade the operator is being asked to make when they pass `--apply`, and it is why the report marks those rows. Then `ghost resolve <project> --reassess` clears the `resolved_at` those edges caused: that repair pass deliberately honours a live edge as a floor, so the withdrawal has to come first or the memory stays out of injection. Until both run, the safe direction holds — a duplicated stale note beats a memory nobody is reminded of.

That leaves the case neither repair can reach, and it is a real one: a pair that is wrong for a reason no rubric can see — the newer note is not a replacement of the older one at all (the same fact recorded either side of a release), or the note the edge calls "newer" is itself the stale one by content. A classifier re-asked about that pair has not made a mistake by its own lights, so `--reassess` confirms the edge and the wrong edge keeps burying its target. `ghost supersede <project> --withdraw <source-id> <target-id> [--apply]` is the operator's undo for that, and `ghost_link_withdraw` is the same core over MCP, so an agent that can see the wrong edge can name it. Nothing is judged: the caller's judgement is the decision, which is why no harness is involved and nothing is billed.

Its rules are the ones a delete needs. The ids are refs: a full id, or **8 or more characters** of one, because every Ghost report abbreviates to eight and a withdrawal that insisted on the full 32 characters could not be driven from the report that names the wrong edge. One literal-prefix query answers both forms, so a full id is accepted whatever its shape — `ghost import` writes an artifact's ids verbatim and the id column only *defaults* to hex, so a ref check that insisted on hex would make an imported endpoint unnameable — while the 8-character floor applies to a prefix alone, which names a class of ids rather than one, and is applied by comparing the match with the ref rather than by inspecting the ref's characters. The match is literal and case-insensitively scoped to the project (plus `_global`, which a promotion moves a memory into without touching its links) — except under `_global` itself, where a ref resolves against the whole store's live rows, because a pair whose source was promoted there can bury a memory in any project and both of its endpoints have to be nameable from the one command that can withdraw it; and **an ambiguous ref is a refusal that lists the matches**, never a choice: guessing which memory to unlink is not a decision the tool may make. A pair with no live edge is an error, and the refusal names the target's live edges of BOTH relations (e.g. "no live supersedes or causes link A→B"), because "no live supersedes link A→B" is a dead end and "B is superseded by C and D" is an answer — and a pair whose only edge is a 'causes' one needs that naming to tell an operator what to withdraw. Several pairs may be given in one command, and **the whole request is settled before anything is written** — a request of five pairs with one bad pair withdraws none of them, because withdrawing four and reporting an error about the fifth is a graph state nobody asked for. The edge is found through the project that owns **either** endpoint of it, and `_global` is owned by every project. The target's half is what makes the undo survive a promotion: `ghost_memory_promote` moves a memory into `_global` and keeps its links, so the project that owns the memory being buried must still be able to name the edge burying it — otherwise a promotion quietly deletes the repair for a wrong edge, and the target stays demoted and `resolved_at`-stamped by a claim no command can name. An edge with both endpoints in another project is that project's edge, and a project-scoped command cannot even name its source, so one project still cannot withdraw or learn about another's. This is also what keeps the repair surfaces agreeing with each other and with the ranking: `SupersedePenalties` has no project predicate at all, so it demotes a target for ANY live edge, and the two project-scoped reads now cover every edge with an endpoint in the caller's project or in `_global` — the cases where the ranking and the repair could otherwise disagree about the same edge. The passes that judge and write whole pairs require **both** endpoints rather than either, because judging an edge acts on both of them; a targeted withdrawal, whose subject is the one memory the caller named, is the one read that takes either. The write is the ordinary `InvalidateLink`, so it leaves the `unsupersede` history row, and it is soft: a later pass that still judges the pair a supersession re-creates the edge. Every report ends by naming the step that un-hides the target, and it is the SCOPED one — the `ghost resolve <project> --reassess --only <the withdrawn edges' target ids> --apply` that `internal/followup.ResolveCommand` builds, with the `--only-file` beside it — because withdrawing the edge is half the repair and the resolution it caused keeps the target out of injection until that runs. The UNSCOPED `ghost resolve <project> --reassess --apply` is never printed: it is the project-wide re-judge, so a report naming it would name the one command that does the most harm. The report also prints the target's own first line: an operator withdrawing an edge they believe is wrong has to be able to confirm from the output that it was the right edge.

### Scoping a repair, and why the unscoped repair is the wrong tool

`ghost resolve --reassess` re-judges every already-resolved memory in a project, and used as the second half of a supersede repair that is a much larger claim than the operator made. On a real store it proposed un-hiding **143** memories, and an independent judge sampling 40 found about **35%** of them stale: completed changelogs, PR and host status snapshots, notes a newer memory in the same project had already superseded, a description of a retired code path ([#698](https://github.com/wcatz/ghost/issues/698)). The repair that was meant to undo a handful of wrong resolutions proposed undoing dozens of right ones.

**Prefer `--only`/`--only-file` for any repair.** `--only <id>[,<id>…]` takes a full memory id **of any shape** or an 8+ character hex prefix of one — the shape rules are rules about a *prefix*, and `ghost import` writes an artifact's ids verbatim, so a stored id can hold a space, a `;` or a leading dash (`internal/resolve/scope.go` reads the full id before either rule, the same order `internal/supersede.resolveRef` uses, because two id-resolution paths that disagree about the same id are the defect). `--only-file <path>` takes the same one per line with `#` starting a comment; both may be given and the list is the union, and the file is the only form that can carry an id holding a comma, since `--only` splits its value on commas. A '#' inside an id is data, not a comment: a comment is a '#' that begins the line or FOLLOWS WHITESPACE, so `<id>   # note` still annotates and `<id>#note` is one id. The rule has to keep the annotated form working, because a selector holding a trailing comment is not a prefix and the repair would refuse the whole file over its own annotation; the one shape that costs is an id containing ' #'. A newline in an id is the one shape **no** surface can carry (`--only` splits on commas, the file is one id per line), and a supersede withdrawal says so rather than claiming a repair exists: such a memory stays resolved until the row is rewritten, the id file is REFUSED when every id in the set is one of those (a file naming nothing is a list the `--only-file` reader rejects, so the printed repair would not run), and the follow-up's wording is driven by which bucket an id fell in rather than by whether a command came out empty — an empty command has two causes and only one of them is a comma. When no id is carriable by `--only`, the printed command is **empty** — never the command without `--only`, which is the project-wide re-judge this scoping exists to prevent — and both surfaces say the file is the only way to run it. Both are refused without `--reassess`, because the ordinary pass has no resolved pool to narrow. Scoping changes *which* rows are judged and nothing else — the supersedes-edge floor, the correction pairing and the KEEP cache all still hold a judged row back — and a scoped report says so (`3 of 143 already resolved judged`) rather than describing a pool it never read. The two ways a selector can fail are not the same kind of failure: one that matches nothing is a mistyped id, another project's row, or one an earlier repair already cleared, and it is **reported and skipped** so the rest of the scope is still judged; one that matches **two** rows is an error, because nothing can be said about which rows were meant and judging either of them is the failure the flag exists to prevent.

The scoping is not a convenience, it is the second half of the chain. Both supersede repairs — `ghost supersede --reassess --consensus 3 --apply` and `ghost supersede --withdraw … --apply` — therefore print their own follow-up — the exact `ghost resolve <project> --reassess --only <ids> --apply` over the withdrawn edges' targets, plus the same id list written under the data dir's scratch as a `--only-file` input, for the run that withdrew forty edges and cannot type forty ids. A dry run prints neither: it withdrew nothing, and resolve would refuse the command anyway while the edges are live. The block says a resolution "was held by" those edges rather than "can now be cleared", because the floor is counted per edge: a note two newer notes both supersede is still held after one of them is withdrawn, and that repair reports the row as still asserted and clears nothing.

The second cause of the same 143 was the KEEP veto itself, which protects a phrase rather than a claim: a status note that says "always check X", a changelog entry that says "never Y" and a description of a retired code path that says "must Z" all carry an imperative, and on the **repair** path a vetoed KEEP is an un-hiding. So on that path alone a veto no longer settles a note when the same project holds a **newer unresolved** memory sharing at least **two** of its key identifiers — a backticked span, an `#NNN` reference, a file name, a host name (`resolve.keyIdentifiers`). The note goes to the classifier, which is the only thing that can tell a completed changelog from a standing rule. Two details are load-bearing. The identifiers are deliberately **not** a bare number or a version, because those are exactly what changes when a note goes stale and counting them would make every dated note the same subject as every other one. And the pool read is `ResolveCandidates`, the same pool the correction floor reads, because a memory that is itself resolved is a row the next ordinary pass will not see. `Run`'s own veto is untouched, and the asymmetry is the reason: on the ordinary pass a false veto keeps a note *out* of the resolution, which costs one noisy memory in the ranked surface, while on the repair pass it returns a stale note to injection permanently, in every session. So the repair path pays a classifier call for the ambiguous case and the ordinary pass does not.

## Persistence and search

`internal/memory` owns:

- Project and memory CRUD
- FTS5 indexing and query sanitization
- Optional vector storage and cosine similarity
- Reciprocal Rank Fusion for hybrid results, with the result window chosen by
  `Store.fuseAndRank` delegating to `selectWindow` (see
  [Hybrid fusion and window selection](#hybrid-fusion-and-window-selection))
- Category-aware time-decay ordering
- Pinned and near-duplicate handling. A pin is a SLOT GUARANTEE on every passive surface (session start, `ghost_project_context`, the project and global resources), reserved ahead of ranking by the passive slice policy: the fetch orders pinned rows first, so a pinned row is in the window whatever it ranks, and the selection puts the pinned rows at the head of the set the demotions run over, so they are never outside the pool and never the loser of a near-duplicate pair. Validity, scope and resolution still withhold a pinned row. A supersede may reorder a pinned row behind its replacement but never out of its slot. When pinned rows exceed a bucket's cap they are ranked among themselves, the cap cuts the rest, and the block reports how many pinned rows were cut.
- Directed memory links
- Snapshots, audit history, tasks, decisions, and usage data

### The vector scan

Both vector legs are brute force over the project's embeddings, and they run as
two phases in `internal/memory/vector_scan.go`. `snapshotVectors` copies every
candidate row out of SQLite under the store's read lock and its single
connection, and returns; `vectorRows.search` then scores the copy with both
released. The copy is the only part that needs either — the cosine pass is
O(corpus × dims) of float arithmetic over rows the store has already handed over,
so holding a read lock across it blocked a writer on the store for the length of
the whole corpus. It matters most for `ghost supersede`, which runs one search
per memory.

A writer is still kept out for the duration of the copy, and releasing the mutex
is not what fixes that: `OpenDB` pins the pool at one connection, so a write
cannot start while a query is streaming whether or not a mutex says so. The
connection, not the lock, is what a corpus scan contends on — which is why the
next change here is a cache that stops asking SQLite for the bytes, not finer
lock work.

The copy keeps the bytes as the database handed them over rather than decoding
them, and the score loop reads the little-endian `float32`s back out of the
blob in place (`cosineFromBytes`, bit-identical to decoding first). The scope
column is kept as text and parsed only for a row that wins a slot. A search
decides scope eligibility per row with a `scopeProbe`, which skips the
`json.Unmarshal` for a row whose stored text cannot mention a requested key —
`ScopeMatches` only ever excludes a row that names a requested key and disagrees
with it, so the substring test can save a parse but never decides membership.

Ranking holds a bounded window rather than the corpus: candidates are offered to
a min-heap whose root is the worst row held, so a row that cannot win a slot
costs one comparison, and the order is imposed once at the end over at most
`limit` rows. The order is total — cosine descending, exact ties broken by scan
position — so the answer is a function of the stored data *and* of the order the
scan yielded it in, rather than of the heap's internals. That is not independence
from the scan order and does not claim to be: the query has no `ORDER BY`, so a
`VACUUM` or an index rebuild can reorder a tie. What it does buy is that two
searches over the same rows in the same order return the same rows, which the
unstable `sort.Slice` it replaces did not promise either.

The scratch is recycled through a `sync.Pool` on the store, so a steady-state
search refills buffers it grew last time; an oversized embedding buffer is
dropped on return rather than retained for a query that may not return. What a
query still allocates is the database driver: `modernc.org/sqlite` copies every
BLOB and every TEXT column into a fresh Go allocation per row, which is the
floor for any `database/sql` scan and is the reason the next step here is a
decoded-vector cache invalidated by `PRAGMA data_version` rather than more work
on the scoring loop ([#556](https://github.com/wcatz/ghost/issues/556)).

### Hybrid fusion and window selection

Window selection lives in `internal/memory/vector.go` as three steps:
`fuseCandidatePool` fuses the FTS5 and vector legs into one ranking,
`scopeEligiblePool` narrows it, and `selectWindow` decides which memories
form the result window. They stay together because window selection is not
separable from fusion — the rule that keeps a keyword hit has to be stated in
terms of the scores fusion produced. Every production search reaches them
through `Store.fuseAndRank`, which fuses once and then cuts;
`FuseAndSelectWindow` is the exported entry point over the same seam, called
only from the fusion tests.

Fusion is Reciprocal Rank Fusion, weighted 0.3 FTS / 0.7 vector with k=60.
A memory retrieved by both legs accumulates both contributions, so a two-leg
match always outranks a single-leg one.

Before that fused score is sorted and cut, `demoteStatus`
(`internal/memory/demotion.go`) multiplies it by a status factor: a resolved
row, and a `_global` row when a specific project is being searched, score
`× 0.5`. The factor runs inside `fuseCandidatePool`, so it decides membership
too — a live project memory a raw-score cut would have lost to a demoted row
takes that slot. It only ever scales, so the demotion itself never excludes a
row: whether a demoted row comes back is the window's ordinary question of
rank, not a filter. With RRF k=60 the factor effectively ranks a demoted row
below every live candidate in the pool rather than below one comparable
neighbour: every fused score lives in a band of a few thousandths — the best
any row can earn is a rank-1 hit in both legs, 0.3/61 + 0.7/61 ≈ 0.0164 — so
halving one lands at ≈ 0.0082, under the ≈ 0.0088 that the deepest vector-leg
row in a default `limit`-10 window still scores and under the 0.0125 of the
deepest row in a keyword-only search. The only live rows a demoted leader can
still outrank are ones the vector leg never fetched (no embedding yet), whose
keyword-only score is capped at 0.3/61 ≈ 0.0049. That is what keeps resolved
memories and shared rules findable while stopping them from leading every
result.

The demotion only reorders what the legs already fetched: each leg pulls
`limit*2` rows from the project *plus* `_global`, so `_global` rows count
against that budget and neither leg drops them. When a project matches fewer
rows than the limit, the demoted `_global` rows are the only candidates left
and they fill the remainder — demotion decides which fetched rows lead, never
which rows are eligible. The two passive surfaces are outside all of this: they
rank in SQL on two separate paths — `loadSessionPassive`
(`internal/mcpinit/session_passive.go`) assembles the session-start digest
through the seam's passive branch, and `projectContextBudget`
(`internal/mcpserver/project_context.go`) is the policy behind
`ghost_project_context`, the `ghost://project/{id}/context` resource and the
`recall_project` prompt — neither reaches fusion,
and both queries already filter `resolved_at IS NULL`, so no status factor
changes what is injected.

The project-context read is the one passive bucket that admits `_global` into a
project bucket (`SlicePolicy.IncludeGlobal`), because it is the one that has
always read `project_id = ? OR project_id = '_global'` under a single cap — the
whole-project listing. Two buckets cannot express that: capped at the caller's
`limit` each, they admit twice the rows the caller asked for. The consequence
is that a bucket name and a row's own project stop being the same thing, so the
retriever reports which policy admitted each row (`Candidate.FetchedBy`) and the
assembler's stage 8 keys its per-slice cap on that. A request that both mixes
`_global` into one bucket and fetches it in another is refused at both seams: the
two row sets overlap, so every global row would be admitted twice under two
different caps. A caller that wants both — the resource's Global section is
exactly that caller — runs them as two **requests**, which is also what keeps
their two verdicts separate.

A cross-project
search leaves `_global` undemoted (there is no project whose own memories it
could be padding), and explain mode reports the factor per row
as `status_factor`, the one `statusDemotionFactor` recorded when the ranking
used it.

## Explain is a record of the ranking, not a second one

`ghost_memory_search` with `explain:true` answers "why did I get this memory"
by projecting the run that produced the answer. The handler builds ONE
`assemble.Request`, sets `Explain` on it, and calls `assemble.Run` once; the
formatted answer and the payload are two readings of that one `Result`
([#898](https://github.com/wcatz/ghost/issues/898), closing the deferral to
[#583](https://github.com/wcatz/ghost/issues/583) and
[#571](https://github.com/wcatz/ghost/issues/571)). Before it, explain called a
second ranking diagnosis (`Store.ExplainSearchScoped`) that was not the
assembler: it marked an expired row included, applied no category or retention
filter, and had no budget, so it could report a row as included that the
formatted answer had withheld. That function is gone; nothing else reached it.

Two things make the projection honest.

**Membership is the answer's.** A row is `included` exactly when it is in
`Result.Items`, after the validity stage, the category, retention and scope
predicates, the item budget and the response-fit pass against the byte cap. An
excluded row carries the reason the stage that withheld it gave (`p.dropped`), or,
for a candidate the retriever saw and did not return, the ranking fact that left it
out: the vector floor, scope narrowing, or the retrieval window. Rows come from the
candidate set's own account of what the ranking saw: `Rows` (the window and its
tail), `Excluded` (the pool candidates the returned set does not carry, in fused
order) and `FloorDropped` (the candidates the vector floor removed outright).

**Every ranking number is recorded where it is computed.** Asked to explain,
`Store.Candidates` creates the ranking's trace (`internal/memory/ranktrace.go`,
carried on `SearchParams.trace`, an unexported pointer that is nil on every
request that did not ask) before the legs run, and hands the per-candidate
`RankFact` map and the fusion knobs back on the `CandidateSet`. The flag is a
request to RECORD: it changes what the retriever returns and nothing about how it
ranks, scopes or windows, and
`TestCandidatesExplainChangesNothingAboutTheRetrieval` holds the rows, their
order and scores, the leg statuses and the edges equal with it on and off. The
assembler adds the facts only it knows: the validity state at the run's clock, the
scope verdict its predicate stage reached, and the weight its provenance stage
pinned. The trace is a byproduct, not a decision path: nothing reads it to decide
anything, and a projection that re-derived a number would be a second chance to
disagree with the ranking.

Which stage records what:

| Stage | Records |
|---|---|
| `fuseCandidatePool` | each leg's 0-based rank, and the vector cosine |
| `demoteStatus` | the fused base and the status factor — the two sides of the one multiplication, so a reader can perform it |
| `scopeEligiblePool` | the scope verdict, for the dropped candidates as well as the survivors |
| `selectWindow` | the keyword reservation, naming both the promoted row and the row whose slot it took |
| `decayRank` | the decay factor and the age at the ranking's own clock, for the rows it orders |
| `Candidates` (explain request) | the factor a row `decayRank` never ordered (the window's tail, the excluded pool, the floor drops) would have carried, at the request's own clock |
| `runCandidateLegs` | the vector floor's per-candidate verdict (which vector contribution it cut, and the cosine), and the candidates only the floor removed |
| `supersedeVerdicts` / `nearDuplicateVerdicts` | the penalty count and the id of the memory that decided it |
| `assemble` validity, predicates, provenance | the validity state, the scope verdict, the provenance weight and the two contributions, per row |

Recording the scope verdict inside `scopeEligiblePool` — for the rows it drops as
well as the rows it keeps — is the structural fix for [#571](https://github.com/wcatz/ghost/issues/571),
where explain reported rows the tool would exclude because the filter ran after
the ranking had already described them. There is no longer a second place that
decides whether a row is in scope. The two penalty functions each return the count
and the counterpart ids from ONE edge read, so the id explain attributes a demotion
to is the id the demotion actually chose.

**Every stored string is rendered as the formatted answer renders it.** Content is
`assemble.Data` over its 120-rune snippet; ids, the project, scope keys and
values, the keyword-reservation and demotion counterparts and a leg's error text go
through `assemble.Token` or `assemble.Data`. The payload is JSON, which escapes a
newline and a quote but not « or », and an agent reads the string rather than the
bytes, so a stored delimiter or forged verdict line cannot reconstruct itself in it.

**One request, no second budget.** `explain` shares the answer's budget, filters,
clock and byte cap. It is refused with `as_of`: an `as_of` read selects among
recorded versions and ranks nothing, so there is no ranking to project, and
answering it with the current ranking would be a payload that cannot be told from a
historical one. It is refused without a query for the same reason (a passive
retrieval scores no candidate). An explain call writes **no retrieval record**: the
record counts answers delivered to a caller, and an explanation is a diagnostic of
one, which also keeps the audit's denominator what it was before explain went
through `Run`. A retrieval leg that failed while another answered is reported in the payload's
notes rather than converted to an error, because the caller asked for the diagnosis
of this run; a run in which every applicable leg failed is still the retrieval error
the formatted path returns, because nothing was searched.

### Signals the ranking does not apply

Fields that report a contribution the ranking does not act on say so with a zero
rather than an invented value. `confidence_contribution`,
`provenance_contribution` and `validity_penalty` are `0`, read from the signals the
assembler's stages recorded. `provenance_weight` is `"off"`: stage 4 pins an inert
`1.0` that no score is multiplied by, and publishing it would read as "weighed and
found neutral". Beside them, `confidence`, `provenance` and `validity_state` report the
row's own stored values, which is a different thing: those are readable, not
applied. One note per payload says which is which.

That is a deliberate choice, not a gap. A contribution invented to fill a field is
a ranking factor nobody measured. Validity is the clearest case: the assembler's
validity stage DROPS an expired or not-yet-valid row rather than ranking it lower,
so such a row is excluded with its reason and `validity_penalty` is 0 rather than a
fraction. A multiplier here needs a measured threshold first, the same bar the
evidence weight cleared before it shipped. The validity state is read through
`memory.ValidityState`, the one rule in the tree.

`rrf_score` is the fused base before status demotion: the sum, over the legs that
retrieved the row, of `weight/(RRFK+rank+1)`, with the weights and the floor the
ranking ran with named in a note (`CandidateSet.ExplainKnobs`, copied from the
parameters, not re-resolved). A request with no usable vector leg fuses with the
configured keyword weight, which is what the answer was ranked on.

### Size budget

An explanation is bounded at 150 CANDIDATE rows (`memory.ExplainMaxRows`) and each
content snippet at 120 runes (`memory.ExplainSnippetRunes`) — the answer is
not part of the budget, because a caller that asked for a window needs it back. The
candidate set is the
union of both legs' results, so it grows with the caller's limit rather than with
the corpus — each leg fetches `limit*2` — while the window itself is capped at 100
by the tool, which is what makes a fixed row budget possible at all.

The cut spends itself on **candidates and never on the answer**: every row the
search returned is kept wherever it falls in the list, and only excluded
candidates are dropped, from the far end. Positional order is not a safe proxy
for that, because the list runs leg by leg and a vector-leg rank-0 row sits after
every keyword-leg row however it ranked.

A payload that hit the budget carries a `truncation` object — `rows_omitted`,
`max_rows` and a sentence — rather than only a note, because a note list is
bounded from the end and a short candidate list that LOOKS complete is the exact
failure the marker prevents.

Window selection reserves real estate for the keyword leg. A plain cut on the
fused score could not admit a keyword-only hit at all: the keyword leg's rank-1
row scores 0.3/61 ≈ 0.0049, while the vector leg's 20th row — still well
inside the fetched window — scores 0.7/80 ≈ 0.0088. A full vector leg
therefore outranked the best keyword match every time, and an exact identifier
match could never reach the results, which is the case FTS exists for.

So the best `limit/5` keyword hits the vector leg did **not** retrieve are
guaranteed a place in the window, evicting the weakest admitted rows for them
— unless the hit's status factor is below 1. The reservation reads raw
keyword rank while the demotion writes the fused score, so reserving a
demoted `_global` or resolved rank-1 hit would hand back exactly the slot the
factor had just taken from it; such a row still enters on its demoted score
like any other candidate — it comes back when the window has room and its
demoted score clears the cut, and drops out of a full window by either of the
window's two ordinary paths: enough candidates outscore it (at default
limits, any full window of live candidates the weights already put above a
halved keyword score), or the reservation's score-blind eviction hands its
slot to a top-`limit/5` keyword hit that scored below it — eviction replaces
the weakest admitted row that is not itself reserved, and a demoted row is
never reserved, so it is always an eviction target. Position is left to the
fused score: admission is
the defect, and the stronger
interventions were built and measured against the built-in dataset first.
Reordering the selected slice does nothing beyond admission, because
`decayRank` re-sorts by score on the way out. Flooring a reserved hit's score
does work, but the floor cannot be made safe — at the top of the window it
costs hybrid R@1 0.507 → 0.366 and NDCG@10 0.812 → 0.738, and at the median it
sits close enough to the row below that decay, which multiplies scores by a
category- and age-dependent factor, reorders it and breaks the invariant that
uniform timestamps leave the graded ranking untouched.

The window's width is `limit`, or twice that under `DecayReselect`, where decay
still has to narrow the set afterwards. Scope constraints are narrowed from the
combined candidate pool before this cut, including when one leg is unavailable,
so an out-of-scope row cannot consume a result slot and force the tool to report
absence for an eligible row that was retrieved but not selected. The hydration
backfill after the cut draws from that same narrowed pool, so a row that
disappears between the leg queries and hydration is replaced by the next
strongest *in-scope* candidate rather than shortening the result. Explain
mode is a reading of the same run, so its included rows and
scope-exclusion reasons describe the answer rather than an unscoped
ranking. Ordering is deterministic (ties broken by ID), because the demotion
penalties applied downstream depend on order.

That makes a store's *contents* part of what a measurement of it means, which is
why the benchmark derives the ids it seeds: `internal/bench` writes every corpus
row under `bench:<project>:<key>` and stamps the whole corpus with one
`created_at`, so a tie is settled by the dataset's own key order — and, after
`decayRank` re-sorts the window by base × decay factor, a tied pair in different
categories by their categories — rather than by a draw from
`hex(randomblob(16))` or by which side of a wall-clock second a seeded row landed
on ([#708](https://github.com/wcatz/ghost/issues/708)). Production rows are
untouched: `Store.Create` still mints every id a live save gets, and the only
caller that hands the store a key it computed for a fresh row is the benchmark.
The two writers that do take an id from a caller are preserving one Ghost already
held rather than being given a new one — `ImportMemory` writes the id an artifact
carries, and `RestoreSnapshot` brings a row back under the id it had.

The formatted `ghost_memory_search` path does not call that entry point
directly. It goes through `assemble.Run` (see [Context assembly](#context-assembly-target-design)),
which asks `Store.Candidates` for the same legs, parameters and configured
vector floor, and then applies the caller's category filter to the widened
candidate set before the window closes. Retrieval is unchanged; what moved is
which set the filter sees.

The main schema tables are:

| Table | Purpose |
|---|---|
| `projects` | Project names, IDs, and paths |
| `memories` | Core memory content, category, importance, tags, and state |
| `memories_fts` | FTS5 virtual table |
| `memory_embeddings` | Float32 embedding vectors, each stamped with the identity of the space that produced it (model, dimensions, task prefix), so a model change retires the old vectors instead of comparing across spaces |
| `memory_links` | Related, supersedes, causes, and other graph edges |
| `tasks` | Cross-session work items |
| `decisions` | Decisions, rationale, alternatives, and status |
| `ghost_state` | Per-project learned context and interaction state |
| `memory_snapshots` | Reflection rollback snapshots, including `scope` and its `scope_captured` marker (schema v14) so a restore can put scope back — and leave a live scope alone when the snapshot predates scope |
| `memory_history` | Append-only per-memory CHANGE LOG (schema v17, [#578](https://github.com/wcatz/ghost/issues/578)) — one row per write, each holding the content, category, importance, `resolved_at` and source the memory had once that write landed, plus the other memory the event is about (`related_id`) and the text a merge folded in (`merged_content`). Its `memory_id` deliberately has no foreign key. A version row that changed nothing is removable by `ghost history compact` ([#730](https://github.com/wcatz/ghost/issues/730)); see [Memory history](#memory-history) |
| `memory_provenance` | Append-only per-memory EVIDENCE records (schema v18, [#673](https://github.com/wcatz/ghost/issues/673)) — SEVERAL rows per memory, one per observation, each naming the agent, session, reference and confidence that supported it, with `observed_at`/`verified_at`, and `carried_from` when a consolidation carried the record off the memory it was consolidated from. A different question from the change log, and the name `memory_history` deliberately did not take. Its `memory_id` cascades, and the table is in the migration's derived-row whitelist so an orphan cannot brick a newer build |
| `memory_snapshot_evidence` | The evidence a reflection snapshot carries, so a restore brings the support back with the row (schema v18). Pruned with the snapshots themselves |
| `retrieval_record` | One row per retrieval CALL (schema v20, [#646](https://github.com/wcatz/ghost/issues/646)) — the query as an HMAC-SHA256 digest under a per-install key kept OUTSIDE the database, the surface, the session, `as_of`, the outcome/reason, and a JSON array of the per-memory kept/dropped verdicts with the stage and reason. The grain is the call and not the (call, memory) pair, because the audit's denominator is calls. **Written by every retrieval surface, not only by search** ([#850](https://github.com/wcatz/ghost/issues/850)): `ghost_memory_search` under `search`, the session-start block under `session_start`, and the project-context surfaces under `project_context`, which since [#581](https://github.com/wcatz/ghost/issues/581) includes the standalone `ghost://memories/global` resource. Two things about those rows are the rule rather than a list of shapes, and the rule is what a reader computing a per-source figure needs: attribution follows the read, so a row carries the `ProjectID` its own request carried, and a read that named the `_global` bucket carries `project_id=_global` rather than the requesting project — a project-keyed read is a statement about that PROJECT's retrievals, and attributing another bucket's window to it would compute that project's precision over rows it never held. And the grain is per CALL, so one call can write more than one row: the `ghost://project/{id}/context` resource and the `recall_project` prompt cap each SECTION, so their `## Global` is a read and a row of its own rather than a slice of the window above it, which is why a resolved-project read of theirs is two rows where the tool's is one — the tool renders its Global section out of the window it already read. A count of calls is therefore not a count of rows. WHICH shapes make which read — the tool at the caller's `limit`, the resource and prompt at `projectContextMemoriesCap`, and the shapes that skip the project-keyed read or the second one — is per-shape detail belonging to the surfaces, and it is stated once, in [docs/mcp.md](mcp.md#project-context), rather than here: this cell's grain is the table, and a list of branches duplicated into a schema row is a list that goes stale without failing anything. `query_hash` is empty on both passive surfaces (they carry no question, so the per-install key is never resolved for them), and `session_id` is the host's session id where the host names one (Claude Code's `CLAUDE_CODE_SESSION_ID` for the MCP server, the hook payload's `session_id` for the session-start block; see [Retrieval audit: which session a verdict is about](#retrieval-audit-which-session-a-verdict-is-about)) and empty otherwise, which leaves the call unjudged. No text, and `query_hash` is CHECK-constrained to a 64-character hex digest or empty, so the column cannot hold a question. The digest is keyed, not a bare hash: a short question is confirmable against the store's own memories by anyone holding it, so the key is a sibling 0600 file in the data directory, which is also why `ghost backup` (a `VACUUM INTO` of the database) carries the records without the key — and a restore on another machine re-keys every row it brings with it. Oldest-first 5000-ROW cap, so a call that writes two rows spends two of them and the retained window is a few weeks of searching in ROWS rather than in calls; `ghost history purge` removes the rows naming a purged memory |
| `retrieval_audit` | One row per (call, KEPT memory) (schema v21, [#646](https://github.com/wcatz/ghost/issues/646); `content_hash` added in schema v22, [#879](https://github.com/wcatz/ghost/issues/879)) — the verdict the comparison reached (`used`, `ignored`, `superseded_in_session`, `contradicted`), the signal that proved a positive one, the scanner's degradation reason, and the hash of the CONTENT that verdict judged, so `UsefulnessByMemory` can tell a real rewrite (withdraw the verdict) from a retag, a re-weight or a `verified` flip (keep it): `updated_at` cannot tell them apart, because metadata-only writes move it too. The grain is the pair, so a second pass over the same call REPLACES exactly that call's verdicts, which is what lets a hook that fires after every turn keep this table proportional to calls. `record_rowid` is the `retrieval_record` rowid it belongs to, read rather than inferred because `recorded_at` is second-precision. IDs, outcomes and reasons only: no transcript text, no query, no memory content — what went in is read, compared against fingerprints and dropped. `content_hash` is a 64-character hex DIGEST of the memory's text and never that text: it fingerprints what was judged, is `''` on every row written before v22, and nothing is backfilled onto those rows (nobody holds the text they judged, and hashing against today's text would make a stale row look current) — they are read by the NAMED LEGACY RULE instead, `recorded_at >= updated_at`, the pre-v22 rule verbatim, which `TestAVerdictWithNoHashFollowsTheLegacyStampRule` pins. Oldest-first 50000-row cap; `ghost history purge` removes the rows naming a purged memory AND the verdicts filed against the calls it removes, in the same transaction and by the record delete's own predicate rather than by memory id — a freed `record_rowid` is reused (the record table has no AUTOINCREMENT), so a verdict left behind would be re-attributed to a call that never admitted its memory — and the pairing is TWO-SIDED, so the write refuses it too: a verdict naming a call that has since been purged is DROPPED (a hole in the report, which is a lost pair) rather than filed against the rowid the purge freed, because a re-filed verdict is a WRONG pair and is indistinguishable from a real one afterwards; the check is on the call's CONTENT — did it KEPT that memory — and not on the rowid, since a freed rowid can be re-let by the next call before the late write lands, and it RETURNS the rows it refused so `audit.Run` can take them out of the figures it already counted and print the loss beside them (`Summary.Unfiled`) rather than print a number the table does not hold; the same guard also gates the batch's replace-DELETE, so a refused row cannot wipe the verdicts a successor holding the freed rowid already filed; the same pairing rides with the record cap's eviction, which takes them by its rowid bound and leaves `record_rowid = 0` alone; a project MERGE reassigns them and `ghost project delete` removes them (and counts them in its dry run), like every other `project_id`-bearing child table |
| `token_usage` | Reserved schema for future harness usage and cost records; current CLI adapters report zero token counts |
| `audit_log` | Destructive and consolidation operations |

### Retrieval

When embeddings are available, search combines:

- SQLite FTS5 for lexical and exact-identifier matches
- Cosine-similarity vector candidates for paraphrases
- Reciprocal Rank Fusion with the shipped 70% vector / 30% FTS weighting
- Category-aware decay applied to the surviving result window
- Targeted demotion when a present memory is superseded by another present memory — an edge that only exists if the older note's claim is no longer true, which is what `ghost supersede`'s KEEP-biased rubric, its required `replaced:` claim and its imperative veto are for (see [The supersede pass](#the-supersede-pass-and-why-it-is-keep-biased))

Without Ollama, the same API remains available with FTS5-only results. Search membership is not discarded solely because of age; decay changes ordering.

A search may also read a past instant instead of the present (`as_of`); that path
retrieves from `memory_history` and runs no vector leg. See
[Historical retrieval](#historical-retrieval-as_of).

### Retrieval audit: which session a verdict is about

A verdict says "this session did or did not use this memory", so it can only be filed
for a call and a session that belong together. Four pieces make that hold
([#648](https://github.com/wcatz/ghost/issues/648)).

- **The call names its session.** `retrieval_record.session_id` (no schema change; the
  column and `retrieval_audit.session_id` already existed, and were empty on every row written before this change) is
  the host's id for the session. The MCP server over stdio has no transport id, so it
  reads `CLAUDE_CODE_SESSION_ID` from its own environment once at startup, which is the
  id Claude Code also sends in every hook payload as `session_id` and names its session
  record after; the session-start hook records its payload's id directly. Hosts whose
  server environment names no session (codex, opencode, goose, a bridge such as `mcpo`)
  record `""`.
- **The scan names its session.** The stop hook stamps the payload's `session_id` on the
  signals and the sidecar (a `session` line; header v4 since the order below). `audit.Run` judges only what
  `RetrievalRecordsForSession` returns for that id, with the predicate in the SQL ahead of
  the `CallWindow` limit, and an empty id judges nothing. When that read finds nothing it
  makes one more bounded, project-wide read, only to count the recent calls that carry no
  session id (`Summary.UnscopedCalls`) and say so, without asserting why; it never judges
  from it. A call with no session id is
  unjudged, never guessed.
- **Old verdicts are set aside, not deleted.** Every verdict filed before this change
  has an empty session and was judged against whichever session the hook had scanned.
  `UsefulnessByMemory` skips audit rows with no session; the reports count them as
  `Unscoped` (named, in no figure). Nothing is removed from the store.

- **A call is judged only against text written after it.** Every scanner stamps what it
  reads with the line's own instant (Claude and codex `timestamp`, opencode part or
  message `time`), the signals keep the latest instant of each fingerprint and id, and the
  sidecar (header v4) carries them; v3, v2 and v1 are refused by name. `Run` takes a
  call's `recorded_at` as the end of its second (the store stamps to the second, a line to
  the millisecond, so a same-second line may predate the call) and judges it against
  `Signals.Since(that instant)`, for all four arms. Anything unplaced can never make a
  memory `used`: an entry with no instant never counts, a scan with none judges nothing
  (`Summary.NoOrder`), a call with an empty or unreadable `recorded_at` is skipped
  (`Summary.UnorderedCalls`), and a scan that carried unplaced lines is marked degraded.
  `SweepSidecars` removes stale sidecars of any version.

What this does not fix: the bodies of Edit, Write and Bash tool arguments are still in the
token set, which clears the token bar (three shared distinctive words and a third of the
memory's words) on ordinary development text. Measured on the test fixture (about 10,000
words, twenty memories from the session's own domain, all in one call of that session),
`TestSameDomainResidualWithTheOrderInPlace`:

| the call comes | `used` of 20, bodies feed the token arm (shipped) | `used` of 20, bodies do not (variant, not shipped) |
|---|---|---|
| before turn 0 of 40 (session start) | 10 | 9 |
| before turn 20 | 10 | 9 |
| before turn 36 | 10 | 5 |
| after the last turn | 0 | 0 |

Ordering helps only for a call made late; a session-start injection still sees the whole
session. Excluding the bodies changes little on this fixture, because its narrative turns
are themselves in the memories' domain, so the evidence does not support removing them
from the token arm. Known limits that all fail toward unjudged: a session id that changes
inside one long-lived server process keeps the old id, and subagent calls are attributed
to whatever session id their server process carries (not verified either way).

### Memory history

`memory_history` is the append-only CHANGE LOG of how a memory reached its
current state ([#578](https://github.com/wcatz/ghost/issues/578)). `memories`
holds only the last value of everything, so "which reflection run changed this",
"what did it say before" and "why is this here with this confidence" were all
unanswerable from the database. Every write that changes a memory's state appends one row, in
the **same transaction** as the write, so history cannot diverge from state:

**A history row's content is written through a redaction filter, and it is a replacement rather than a refusal.** The history is the one place Ghost keeps text it holds nowhere else — a memory row is overwritten by the next edit and gone by the next delete — so a credential redacted from the row by an edit or a delete has to be redacted from here too, or the redaction leaves a LONGER-LIVED copy than the row it was made to remove. `ghost_history_content` is a registered SQLite scalar function that every statement writing a history row reaches its content through (the batched append and the baseline's `NOT EXISTS` insert; they cannot each decide for themselves, because one of them forgetting is exactly how an unredacted credential lands while the append path claims to redact it). The filter is installed by `internal/memory/history_redactor.go` and the append path's gate is DERIVED from the redactor rather than kept beside it, so the two cannot disagree.

The redaction is whole-content, not a span, and the reason is worth stating because it looks like laziness and is not. `secret.Finding` carries a rule name and a human-readable label and no offsets, deliberately — its other consumer is a refusal message that must not quote the value — so a span-precise redactor would need a capture group per rule across the whole rules table plus offsets from all three post-table passes. The honest options today are "replace the content" and "keep the credential", and only one of those is a redaction. The trade is that a **false positive now costs one history entry's text**, where before the guard landed it cost nothing: the live row still holds the current text, and the entry still records that a change happened, when and by whom. And it is defence in depth rather than a live control — every writer refuses a credential on the way in, so a credential can only be in a history row if it was stored before that guard existed, which means the filter fires on nothing at all in a store that has never held one.

The filter runs on **every** appended row whether or not anything is redacted, because the only sound way to know is to look, and that cost is on the write path's critical section: measured at **~1.1 ms per 2 KB row and ~3.6 ms at the 8 KB content cap**, inside the transaction, so a batched append pays it once per id. Skipping the detector on rows that look clean is not available — a prefilter with a false negative is a silent leak, which is the same lesson `internal/secret` already teaches once in its own comments.

**A version row is removable only when it changed nothing, and "nothing" is every column a version records.**
`ghost history compact` (#730) is the repair for the rows a pre-#727 build left behind: #727 stopped every applied
reflection from appending a byte-identical `reflect` version per kept memory and from moving that memory's
`updated_at` to the run's own time, which measured at 80% of this table on one real store. A version row goes only
when it records the same state as the row before it of the same memory, in rowid order, compared over **every**
column a version stores — and it is a `reflect` version recorded before a bound. Five things never go: a memory's
first version, its newest version, the phases that record a claim the state does not, any row naming another memory,
and any version at or after the bound. A sixth rule keeps ALL of a memory's versions, and it is a scope rule rather
than a sixth: a memory whose newest version is a `delete` tombstone is not compacted at all. A deleted memory has no
`memories` row, so `CreatedAt` falls back to the version that answers and removing a version from it would change the
AGE a past read computes, for a memory nobody can edit and nobody can restore. It costs nothing by construction —
a retired memory's history is frozen — and it is stable under the deletion, because the rule reads the newest
version and no removable row is ever the newest.

The bound is the interesting one, and it exists because a **current** build still files a byte-identical `reflect`
version on purpose: a consolidation merge whose survivor is one of its own sources carries the union of that source's
tags, `ReplaceNonManual`'s `reusePreservesAge` branch writes them, and the version restates every column this table
stores, because the tags are not a column of it. So the recorded state cannot tell a deliberate retag from the
pre-#727 flood, and the repair does not pretend otherwise — the default bound is the instant #727 reached main
(`--before` widens it for a store whose clock is behind), so a row a current build wrote is a current writer's
business and a row written before the fix shipped is this repair's. `save` and `update` are not compacted at all,
which is the same fact from the other side: a tags-only `ghost memory update` is a real change this table cannot see.

The same fact decides `--fix-updated-at`'s **anchor**, and this is where the design went wrong twice. The anchor is
the newest version the repair will **not** remove **and** whose WRITER bumped `updated_at` in the same statement
that filed it — `stampMovingPhases`, three phases long. The first attempt read "not the newest version that
*changed state*", and that was wrong in the direction that invents: `SetResolved` writes `resolved_at` and says in
as many words that it leaves `updated_at` alone, `Upsert`'s unsaturated fold writes `importance` and moves nothing,
and a memory's FIRST version passes the state comparison trivially because it has no predecessor. Any of them
became the anchor, so the repair set a memory's stamp to an instant the store had never held on that column — 49 of
288 restored stamps on one real store, and one pre-v17 memory whose stamp was set to the moment its unresolve row
was written. There is nothing else to check the claim against, and that is why the rule is a phase list and not a
cleverer predicate: this table records no `updated_at` at all, so a version cannot be compared with the stamp it is
supposed to account for. The only evidence a stamp write happened is the identity of the writer, and the writer is
what the phase names. The newest-version guard is the one clause deliberately left out of the anchor, for an
arithmetic reason rather than a judgement one: a removable row is never the newest, so an anchor that always
included the newest row would sit above every removable row and the repair would never fire at all.

The obvious over-correction is to make *every* non-removable row an anchor, and that is worse than the bug: a
`supersede` version is non-removable, and `CreateLink` writes no `memories` row at all, so as an anchor it closes
the gate for that memory permanently — on exactly the store where the supersede landed after the damage. So the
anchor takes both clauses and neither alone. Getting `stampMovingPhases` wrong in the permissive direction is
silent, and getting it wrong in the *other* direction is silent too: a memory whose history holds no version written
by a stamp-moving writer has no anchor at all, and its stamp is left exactly where the damage put it rather than set
to a guess. That is a third outcome, counted under its own name (`no recorded stamp write`) because it is a
different fault from an unreadable stamp — an unreadable stamp is a value that exists and no layout reads, and this
is a value whose AUTHOR does not exist anywhere in the table — and because a report of `0 updated_at restored` beside
a store full of removed versions otherwise reads as a finished repair. A pre-v17 memory is the reachable case: no
`save` version, so a `ClearResolved` above the flood leaves nothing that moved the stamp.

The rules, the reasons, the writers whose deliberate restatements forced the bound, and the gate `--fix-updated-at`
needs are stated in [invariants.md](invariants.md#ghost-invariants) under "Memory history"; this section is the
design narrative, that file the checklist a change is held to.

**The name is a distinction, not a description.** This is a change log — one
row per write, holding the state the memory had once that write landed. Evidence
provenance is a separate concept and is not this table: that is
`memory_provenance`, created by `migrateV18` ([#673](https://github.com/wcatz/ghost/issues/673)),
holding SEVERAL evidence records per memory (kind, agent, `session_id`,
`source_ref`, confidence, `observed_at`, `verified_at`), answering "who or what
supports this memory" rather than "how did this row change". The two are easy to
confuse in prose and unrelated in fact, and a schema name is permanent once
released, so this table is named for what it is.
A development store built from a pre-rename commit of #664 can hold a
`memory_provenance` table from that build, holding the CHANGE LOG's shape under
the reserved name. `initSQL` cannot stop that — the change log has a `memory_id`
too, so its `CREATE INDEX` succeeds against the wrong table — so the check runs
twice: in `migrateV18`, which owns the precondition, and in `OpenDB` *before*
`backupBeforeMigrate` (a refusal that costs a full `VACUUM INTO` copy on every
open of a store that cannot be opened, and whose same-second retry reports a
backup collision naming neither the table nor the remedy). It **refuses** a
mismatch, naming the command rather than warning: a store that will not open is
the operator's to fix, and a warning nobody may see leaves every writer failing
on the same missing column. Nothing is converted, and nothing is dropped
automatically — the rows in that table are a dev build's change log under the
wrong name, there is no shape to convert them into, no release ever wrote one,
and the pre-migration copy `OpenDB` has already taken is the net a conversion
would be guessing past.

The check is about **identity, not version**: it tests for `id`, `memory_id` and
`kind` and nothing else. `kind` alone settles it, since no shape of the change log
has one, and a probe that compared the *full* column set would be a version check
wearing an identity check's clothes — the next schema change to add a column here
would then refuse every store already at that version, with a remedy that drops a
table full of evidence records. An older shape of the real table is therefore left
alone with its rows intact, and the column it lacks is reported by the statement
that reads it: a repairable message about one column. A table of the current shape
is likewise left exactly as it is — the seed adds a `legacy` row only where a
memory has none, so re-running the step cannot double them.

| Phase | Appended by | What the row records |
|---|---|---|
| `save` | `Create`, `Upsert` (new row or linked copy), the decision companion memory, the shipped seeds | the row as inserted |
| `merge` | `Upsert`'s near-duplicate fold | the target after its importance and access count were raised |
| `update` | `UpdateMemory` | the edited row (a content change may also clear `resolved_at`) |
| `reflect` | `ReplaceNonManual` — a rewrite, a reuse that restated a field, or a fresh insert | the row as the consolidation left it |
| `resolve` / `unresolve` | `SetResolved` / `ClearResolved` | the row with the new `resolved_at`, or without it |
| `supersede` | `CreateLink` with a `supersedes` edge, when the edge becomes active | the **target**'s state, `related_id` naming the superseding memory |
| `unsupersede` | `InvalidateLink` on a `supersedes` edge, when a live edge is withdrawn — by the repair pass or by `--withdraw` / `ghost_link_withdraw` | the target's state again — a withdrawal is a change, and a history that shows a claim and no withdrawal reads as though it is still live |
| `baseline` | the first write to a memory that predates the table, before that write | what the memory said when this build had never seen it |
| `restore` | `RestoreSnapshot` | the row as the snapshot put it back |
| `import` | `ImportMemory` | the imported row, attributed to the artifact's agent |
| `delete` | `Delete`, the replace's bulk delete, the restore's cleanup | the state the row held immediately before it went |

**`ghost history` takes a ref, and the ids it resolves over are BOTH tables**
([#720](https://github.com/wcatz/ghost/issues/720)). Every Ghost report shortens
an id to eight characters, so `ghost history <id>` taking only a full id meant the
one command whose output is those eight characters could not be driven from them —
and `ghost resolve --mark` and `ghost supersede --withdraw`, which print exactly
that form, already took the short one. It now goes through `internal/memref`, so a
ref means here exactly what it means on every other surface: a full id of any
shape, or a prefix of 8 or more CHARACTERS naming one memory, with an ambiguous
prefix refused rather than guessed. The rules are not restated, which is the point
— two implementations eventually disagree about which id one spelling addresses,
and this is the command that would be most expensive to get wrong, since it prints
a memory's whole recorded text.

The id set is `Store.AnyMemoryIDsByIDPrefix`: every `memory_history.memory_id`
UNION every `memories.id`, the whole store, no project predicate. Each half is
load-bearing, and the first is the one the issue is about. A deleted memory has no
row in `memories` — the `delete` history row **is** the tombstone and carries the
text the memory held — so a set read from the live rows alone reports a deleted
memory as never written, which is the exact false claim #720 removes. The live half
is the easy miss in the other direction: a memory predating this table has NO
history row, because `migrateV17` deliberately does not backfill, so a history-only
set refuses a prefix of an id the store plainly holds. It is a `UNION` and not a
concatenation for a third reason: the same id is in both tables as soon as a memory
has any history, and a set listing it twice makes its own prefix **ambiguous** —
the memory's own history would make it unnameable.

The scope is the whole store, deliberately, and it is the reason this is `ResolveIn`
rather than `Resolve` with a project. `memref.Resolve`'s project scoping exists
because its caller is about to change the row it names. This caller changes
nothing, and `ghost history` has no project operand: a full id has always reached
any row in the store, so scoping a PREFIX to a project would make the two forms of
one ref disagree about where a memory may be looked for. `ResolveIn` therefore
takes the id set as an argument and its refusals name **no** project — printing one
would be a claim about a scope the set never searched, and the kind of claim a
reader acts on. The same store read is why `resolveIn` applies the prefix match
itself rather than trusting the caller's set: `Resolve`'s query is already a prefix
filter so re-filtering it is a no-op, and a caller that collected ids for another
reason is then not obliged to have filtered them. `memref.ErrNoMatch` is the
sentinel for the one refusal that says the set holds nothing this ref can mean, so
a caller can tell that from the ones that say it holds too much — `TestOnlyAMissCarriesErrNoMatch`
holds the line in both directions, because the distinction is worth nothing if a
second refusal acquires the sentinel.

Two arguments do not go through the refusal, and both are about what a full id
means. One is a **miss** on a ref at least as long as a whole id (32 characters,
what `hex(randomblob(16))` mints): it cannot be a truncation, so the answer is
about the id and `readHistoryView` reports it as it always has — "never written, or
its history has been pruned" — rather than as a prefix of nothing. "Miss" is the
whole of the exception, and that is what `memref.ErrNoMatch` is for: an ambiguity
and a third casing are refusals whatever the ref's length, and a gate on length
alone would let either through. Both are reachable here, because `ghost import`
writes an artifact's ids verbatim, so a store can hold two 40-character ids sharing
32 characters, and the read would then report two memories the store plainly holds
as never written. `ErrNoMatch` is the distinction memref already draws between the
refusal that says the set holds nothing and the one that says it holds too much, so
branching on it here is not a second implementation of a rule.

The other argument is `purge`, which takes the **whole id only** and refuses a
prefix. It is the one place in Ghost that erases recorded text for good, where a
mistyped argument is not a message but an unprintable memory; echoing the resolved
full id would not make that safe, because the announcement and the transaction are
one breath apart and a single-character slip in a pasted prefix is still a unique
match. So the prefix is refused and the full id it names is printed, and the way
through is `ghost history <prefix>`, which resolves the ref and shows the id. Two
consequences of the asymmetry, both tested. An id the store never held is a
**miss**, not a prefix — which is also what re-running a successful purge looks
like, since a purge removes the id from both tables. And the whole-id test asks
`memref.ResolveIn` rather than testing membership itself, which is the one gate in
this path and the subject of the second review round. A case-insensitive scan is
wrong twice over. It answers a BOOL, and every read a purge then makes compares
case-SENSITIVELY (neither `memories.id` nor `memory_history.memory_id` carries
`COLLATE NOCASE`), so a folded spelling passes the gate and then erases nothing —
"nothing to purge" on the redaction path, with the text still in the database. And
taking the FIRST match is wrong in a store holding two ids differing only in letter
case, which `ghost import` can produce (verbatim ids, case-sensitive presence
probe): SQLite's BINARY collation sorts the upper case first, so a third casing
that memref refuses as addressing neither row would instead destroy the upper-case
memory's text irreversibly while the memory the operator named kept it. Asking
`ResolveIn` gets byte-exact precedence, the ambiguity and third-casing refusals and
the stored spelling from the one place that already implements them, and the gate's
own question — is the resolved id THIS ref — is a single `EqualFold`. The id-set
read is propagated when it fails, because a store that cannot be read has told us
nothing about the argument, and a prefix sentence there would send the operator
after a full id they may already have.

Three properties are deliberate:

- **Each row is a version, not a diff.** It holds the content, category,
  importance, `resolved_at` and source the memory *had once that write landed*.
  The prior content of any write is therefore the previous row's content, and
  "what did Ghost know at time T" is the newest row at or before T — a query
  `memories` cannot answer at all. Recording the state *after* the write is also
  the only shape an insert can have: an inserted row has no prior state.
- **`agent` and `session_id` are the performer, not the memory's own stored
  provenance.** The save, merge and import paths carry a `Provenance`; the
  lifecycle passes (`reflect`, `resolve`, `supersede`) and `Delete` know no
  session and leave both empty. An empty field is an admission, not a claim that
  nobody acted.
- **A `delete` row names its successor.** A consolidation rewrite or merge gives
  the row a new id, so without a pointer the old id's history simply stops, and a
  reader following one memory — what [#648](https://github.com/wcatz/ghost/issues/648)
  will do with a usefulness verdict — never learns the memory it holds a verdict
  about is now a different row. `ReplaceNonManual` stamps it from
  `Memory.ReplacesIDs`, which the reflection operations (#659) fill in; a row
  deleted for any other reason is left without one rather than given a fabricated
  successor.
- **`memory_id` has no foreign key.** A hard `DELETE` takes the row with it, so
  a cascading history table would be empty exactly when the audit is asked — the
  `delete` row is the tombstone, and it carries the text the memory held.
  `project_id` *does* cascade: deleting a project is meant to take its corpus
  with it. That makes `project_id` reassignment on a project *merge* load-bearing
  rather than cosmetic — a merge keeps the memories and deletes only the
  `projects` row, so a `memory_history` row left behind would be taken by that
  cascade while the memory it describes survived with no recorded past. Both of
  main's merge reassignment lists (there are two, one per implementation) carry
  the table.

A write that changes none of a memory's own recorded columns appends no row: one
repeating the previous state would record that nothing happened, at the cost of a
row per recall. `Touch` (`access_count`, `last_accessed`), `TogglePin` and the
resolve KEEP cache are in that class.

One writer deliberately breaks that rule, because the row is not a state change
but a record that a *claim* now stands against a memory, which is not visible in
the row's own columns. A `supersedes` edge moves none of the target's columns but
does change its standing — "this is no longer current" — and an audit blind to
that is blind to the corpus's main staleness signal. It costs a slot under the
cap below, which is what bounds it; it is not a licence to repeat. The
`supersede` row in particular is written when the edge *becomes* active and not
on every re-link, because `ghost supersede` re-writes a pair on every pass whose
endpoint moved, and a re-write of a live edge records no run and asserts no new
claim. That re-write also moves `memory_links.created_at`, which is the judgement
stamp the *next* pass reads to decide whether the pair moved at all — so the
column answers "when was this pair last judged?" rather than "when did this edge
first enter the graph?", it has exactly one reader, and no history row keys on it.
The stamp is the freshness of the content the verdict was made against, not the
moment the row landed: the classify call sits between the pass reading the
endpoints and its write landing, so a clock here would cover an edit made in
between and leave the pair quiet against text nothing judged.

**A consolidation that re-emits a memory verbatim no longer breaks it
([#727](https://github.com/wcatz/ghost/issues/727)), which reverses a rule this
section previously stated.** It used to append a `reflect` row byte-identical to
the previous version, on the reasoning that "which reflection run touched this"
is the question the table exists to answer. The measurement said otherwise.
About fifteen hours after the v17 upgrade, a real store held 3,340 history rows
of which **2,669 (80%) were `reflect` re-emissions**; one memory already had 17
versions of itself, and each run wrote 140 rows inside a single second. At that
rate the per-memory cap of 50 began evicting real events — saves, updates,
supersede and unsupersede, resolve — within about two days, and the 20,000-row
store cap within about four, so `as_of` and `ghost history` lost exactly the
events they exist for. The same write also set `updated_at = datetime('now')` on
rows it did not change, and that timestamp is not only a freshness hint:

- **`ghost supersede` orients each candidate pair by `updated_at`.** Once every
  row reflect had merely looked at shared one timestamp, the newer-versus-older
  direction came from the id tie-break instead of from either memory's age. A
  `REVERSED` verdict is *refused* rather than flipped, so a pair oriented the
  wrong way costs a classify call on every pass and is never written: a dry run
  on the measured store logged dozens of refusals on pairs whose `created_at`
  order was never in doubt.
- **`--skip-unchanged` fingerprints the consolidatable set by `updated_at`**
  (`reflection.InputSignature`, which carries it as a change proxy). An
  all-keep apply therefore moved a field the gate reads, so the fingerprint
  stops describing the corpus and starts describing when reflect last ran. On
  a corpus nothing else touches this is self-cancelling rather than visible:
  `runReflect` records the fingerprint *after* the apply, so the stamps it
  wrote are already in the stored value and the next round matches and skips
  (measured — `TestRunReflectSkipUnchangedSkipsAfterAnAllKeepApply` records
  both halves). It matters when anything else moves the corpus in between, and
  a pin or a `PromoteToGlobal` does exactly that: the gate is then invalidated
  by reflect having done nothing, and the next round pays for a full-corpus
  consolidation. Fixing it at the source is what makes the fingerprint mean
  "the corpus", which is the only thing a fingerprint can mean.

The rule now is that a re-emission that leaves **content, category, importance,
tags and scope** unchanged is a no-op: no `UPDATE`, so `updated_at` stays where
the memory last really changed, and no history row, because there is no new state
to record. `reuseChangesNothing` decides it, comparing every field the reuse
`UPDATE` would write. A change to **any** of them is a real update and is recorded
as before. The comparison is by value rather than by the bytes `json.Marshal`
produced for each side — a row saved untagged holds `"null"` where a keep that
normalises nil to an empty list holds `"[]"`, and both read back as the same empty
list, so comparing the column text would report a difference on every run of an
untagged corpus. **importance is compared at the precision the emission carries**,
which is a float32 because the read already narrows the column into a `Memory` —
and that read is the whole reason this is not a full-width `==`: the fold's
strengthen is `SET importance = MIN(1.0, importance + ?)` with `importance*0.2`
bound as a float32, so the multiply is Go arithmetic on the caller's own value and
only the ADD is the column's own float64 — and that sum is a value float32 cannot
name (0.55 strengthens to 0.6600000187754631), so a keep of a
folded row reported a change on every run and reinstated the flood this rule
removes (#750). Narrowing is the one lossy direction and it is the one no reader
can observe; what it cannot absorb is a reweight, which is a different float32
however close. A stored value that cannot be read counts as *changed*, never as
unchanged, so a broken `tags` column is repaired loudly rather than skipped
quietly.

"Which run last looked at this memory" is no longer a question the change log
answers, deliberately: it is not state, and the run itself is already in
`lifecycle.log`. One consequence is worth stating because it is a narrowing
rather than a fix — a memory written before v17 and thereafter only ever carried
through keeps no version at all, so an `as_of` read reports it in `Unknown` for as
long as nothing changes it. That is the honest answer (the store cannot say what
it said at T because nothing ever wrote a version of it) and `Unknown`'s
disclosure is designed for exactly this, but before this change such a memory
acquired a version merely by being looked at.

The already-flooded stores are not repaired by this: the byte-identical
consecutive `reflect` rows on disk are still there. A one-time cleanup that drops
a version equal to the one before it is a **separate follow-up**, and it should be
a reported, dry-run-first command rather than an automatic migration.

Two more writers can produce a row byte-identical to the previous version
without meaning to, and neither earns a special case: `UpdateMemory` called with
the values a row already holds (the MCP tool passes the caller's arguments
straight through, and its "changed" list is which arguments were supplied, not
what differed), and a fold whose `importance` increment has saturated at 1.0.
Both cost a slot under the cap, which is what bounds them, and both record a write
somebody asked for.

`MergeProject` and `PromoteToGlobal` are in the no-append class — both change
only `project_id` — and they are the writers that show the other half: appending
nothing is not the same as ignoring them. A write that moves a memory between
projects has to carry its history rows' `project_id` with it, because that column
is what the project-delete cascade follows, and both do. Without it, deleting the
project a memory was promoted out of takes the recorded past of a memory that is
still live in `_global`, and `ghost history <id>` reports that it was never
written.

**Redaction.** The history is the one place Ghost keeps text it no longer holds
anywhere else, which makes it a liability as well as an asset: a memory row is
overwritten by the next edit and gone by the next delete, while its earlier
versions sit here. So a secret that was "removed" by deleting its memory is not
removed — it is in a table with a longer life than the row, and `ghost history`
prints it. Three things follow:

- **A purge path, in both directions.** `ghost history purge <id>` and
  `ghost_memory_delete`'s `purge_history` argument delete a live row and every
  history row for it in one transaction, so a memory and its history cannot come
  apart. A memory that is ALREADY deleted has its history erased on its own
  (`Store.PurgeMemoryHistory`, reachable from both entry points), which is the
  case a delete-time purge cannot cover: the tombstone is the feature, so a
  redaction asked after the delete would otherwise report the memory as not found
  and leave the text on disk. A plain delete deliberately keeps the history —
  that is what makes the table worth having — and an id whose history still exists
  is one `ghost import` refuses to write into, because the artifact's ids are
  verbatim and the two records would splice under one id. The second direction is
  `Store.PurgeMemoryHistory` alone, and it differs in kind from the two entry
  points above: it erases recorded text and leaves a live memory row exactly as it
  was, because "erase the history of this memory" and "delete this memory" are
  different requests. The entry points do not have that choice — they are a
  delete, and the live row goes with its history.
- **A purge reaches every copy this database holds**, not just the history table,
  and that includes the *evidence* copies added later — `memory_provenance` and
  `memory_snapshot_evidence` both name the agents, sessions and references that
  reported a memory, so a redaction that left either behind would have erased the
  text and kept the story of who said it. The snapshot table is the one an earlier
  version of this list would have missed: a snapshot is the rollback point for a
  whole *project*, so its evidence rows are keyed on the memory alone and would
  otherwise outlive the purge by however many reflects it takes to prune that
  snapshot — or forever if none runs.
  `memory_snapshots` is the one that matters: every applied reflection copies each
  non-manual memory's full content into a snapshot, the column has no foreign key,
  and `ghost reflect --restore` re-inserts the row from it under the memory's
  original id — so a purge that left the snapshot behind would report success on a
  secret one `ghost reflect --restore` away from being readable again, with no
  history to show it came back. A FoldOnly fold's discarded wording is the other
  copy: it sits in `merged_content` on the *target's* row, so those cells are
  redacted rather than their rows deleted (the event is worth keeping; the text is
  what the purge is for). Snapshots are matched by content as well as by id, because a
  pre-v13 snapshot recorded no id and `ghost reflect --restore` matches those by
  content — an id-keyed delete would have left the one snapshot that can resurrect
  the row. What a purge still cannot reach is a backup taken before it, or another
  machine's copy of the store, so the command reports what its transaction covered
  rather than that the text has ceased to exist.
- **A redaction seam on the way in.** `ghost_history_content` is a SQLite
  function called by the append statement itself, so content is rewritten inside
  the one statement that copies the state out of `memories` rather than in a
  second pass that would leave an unredacted copy on disk. It is wired to
  `internal/secret`'s `Detect` ([#656](https://github.com/wcatz/ghost/pull/656) has
  landed), and the plumbing is tested.
- **The pre-v17 gap is not a purge's job.** `migrateV17` records no starting row,
  so a memory that predates the table has no history until something writes one.
  The first write that would destroy its text files a `baseline` row first
  (`recordBaselineHistoryTx`), and a backfill is the documented alternative that
  was not taken: it would assert what every existing row said at the moment of
  the upgrade, which nobody recorded, on a store with a large corpus.

**Growth policy.** The table is append-only, not unbounded, and both bounds are
applied in the appending transaction — so neither needs a background job or a
clock, and neither can be lost to a crash between the write and its cleanup:

1. **Per memory**, only the newest 50 versions survive
   (`historyVersionsPerMemory`), ranked by `rowid` rather than `recorded_at` —
   which is second-precision, so every write one reflection makes in a pass shares
   a timestamp and the order among them would be a tie-break. Ranking by `rowid`
   also means the newest row can never be taken, so a memory always has one
   statement of what it says now, and a memory under the cap keeps everything —
   which is why a rarely-changed memory never loses its `baseline`. One
   `ROW_NUMBER()`-windowed `DELETE` covers the whole batch, not one statement per
   id.
2. **Across the store**, only the newest 20 000 rows survive (`historyRowsCap`).
   The per-memory bound cannot do this job — every created-then-dropped memory is
   a distinct `memory_id` with a couple of rows of its own, and one applied
   reflection can churn the whole non-manual corpus. The cap is a rowid window
   rather than an `ORDER BY` over the table: an implicit rowid is `max(rowid)+1`
   and is never reused, so rowid order *is* insertion order.

Both questions are asked in **one** statement, and each `DELETE` behind them runs
only when the answer says a cap is actually exceeded (`pruneHistoryTx`). That is
not tidiness. This runs inside the caller's write transaction, so everything it
adds is time the write lock is held, and the write lock is the one resource
concurrent writers queue for — see the concurrency contract above. A first
version that ran the windowed `DELETE` and a separate `max(rowid)` probe on every
append added three statements to every save, and `TestConcurrentProcessesMixedReadWrite`
began failing with `SQLITE_BUSY` at `BEGIN IMMEDIATE`: `busy_timeout(5000)` is a
bound, not a guarantee, and a per-upsert statement that looks free in a
one-process test is what reaches it. A memory under the cap and a table under the
global cap are the normal state, and then the only statement is the probe.

Two more decisions on that same critical section, both measured rather than
assumed:

- **One index, not two.** `idx_history_memory(memory_id, recorded_at)` is the
  only one, and it serves every reader this build has: the per-memory cap ranks
  by rowid (the implicit index, free), `MemoryHistory` filters on `memory_id`, and
  an `as_of` read (#647) — "the newest row for this memory at or before T" — is
  the same index with a bound. A standalone `recorded_at` index would have no
  reader, and every append would pay a second b-tree insert to keep it current.
  The migration test asserts the *absence*, so adding one back "just in case"
  takes an edit rather than happening quietly.
- **The content filter is called only when one is installed.**
  `ghost_history_content` is a Go function reached through the driver, so calling
  it unconditionally means a cross-language call per appended row to do nothing.
  In production the redactor is always installed — `history_redactor.go`'s
  `init()` registers it unconditionally — so every build pays the call; the only
  no-filter state is the one `setHistoryRedactor(nil)` creates for the seam's
  own tests, and `history.go:166-170` documents this explicitly.

`Store.MemoryHistory` reads one memory's history oldest first — a changelog, not
a log tail — and `ghost history <memory-id>` prints it.

### Historical retrieval (`as_of`)

[`#647`](https://github.com/wcatz/ghost/issues/647) adds a read of a project's
memory set **as it stood at an instant T**. It exists because the live tables can
only answer "what does Ghost know now": `reflect` replaces rows and
`ghost_memory_update` overwrites content in place, so a benchmark cannot replay
what a past session was given. The history table can, because each of its rows is
a VERSION rather than a diff — the state a memory held once that write landed — so
the newest row at or before T *is* the state at T. There is no reconstruction step
anywhere in the read, and no inference: it is a selection.

`Store.MemoriesAsOf(ctx, projectID, T)` returns an `AsOfSet` in **one statement**,
`internal/memory/asof.go`:

- **The version set.** The newest history row at or before T for every in-scope
  memory, partitioned with `ROW_NUMBER() OVER (PARTITION BY memory_id ORDER BY
  recorded_at DESC, rowid DESC)`. `rowid` is the second key because
  `recorded_at` is second-precision and every write one reflection makes shares a
  timestamp, so the newest among them would be a tie-break rather than an answer.
  The driving table is `memory_history`, **not** `memories`: a consolidation's
  delete takes the row and leaves a tombstone, and a read that started from the
  live tables would find nothing for a memory the store held for months.
- **Liveness from the sequence.** A row whose newest version at or before T is a
  `delete` is dropped (it was already gone), and a memory whose first recorded
  version is after T is dropped (it did not exist yet). `resolved_at` comes from
  the version row, so a `resolve` / `unresolve` pair is read as a sequence rather
  than as a single current value. The supersede **claim** is read separately,
  from the newest `supersede`/`unsupersede` row at or before T, because a
  supersede records a claim about a memory rather than a change to it: the
  target's own state columns are identical whether the claim is live or
  withdrawn.
- **The gap, reported.** A live in-scope memory with no recorded version at or
  before T is **unknown**, not guessed. `migrateV17` backfills nothing, so every
  memory written before the table existed reads this way until something writes
  one. It is left out of the set and counted in `AsOfSet.Unknown`, and
  `AsOfUnknownNote` turns the count into the sentence a surface must show — a
  shorter set that says nothing about the gap reads as the whole truth.
- **Which columns are historical.** `content`, `category`, `importance`,
  `resolved_at`, `source` and `project_id` come from the version row. Tags,
  scope, pin, access count, confidence, provenance and the validity triple were
  never versioned, so they are read from the row as it stands, and a deleted
  memory has no row at all — those fields are zero for it, which is the honest
  reading rather than a guess. `valid_from` / `valid_until` / `verified_at` are the sharpest
  case: [#575](https://github.com/wcatz/ghost/issues/575) ships the writers, but
  the change log still records no validity, so an `as_of` read takes the window's
  bounds from the current row. It judges them **at T**, the way
  `ghost_memory_search` does when it binds the assembler's clock to `as_of`: a
  row whose window had closed or had not yet opened at T is withheld, and a row
  valid at T is shown as valid at T even if its window has closed since. The two
  `as_of` **listing** surfaces — `ghost_project_context`'s `as_of` branch and the
  `ghost context --as-of` session block — and search all reach that verdict
  through one helper, `memory.ValidityAt` (stage 2 drops on the same
  `memory.ValidityWithheld`). The two listings apply it before the limit so a
  withheld row takes no slot, and state `memory.AsOfValidityNote` once at block
  level, saying that validity was judged at T, how many rows it withheld
  (counted, so an all-withheld block is not mistaken for a project that held
  nothing) and that the bounds, like the row's other unversioned fields, are the
  current row's. Search reaches the same verdict through stage 2 but does not
  state that note or a count. A bound exactly at T is inside the window, and an unreadable
  bound is no bound, as in stage 2. The `unverified` marker is drawn as on any
  line, because `verified_at` is a flag rather than a predicate. One imprecision
  is recorded rather than worked around: because the bounds are the current
  row's, a window edited after T is judged as the edited one, so a row can be
  withheld from (or kept in) a past answer on the strength of a bound that was
  not set then ([#910](https://github.com/wcatz/ghost/issues/910)); and a
  promotion **rewrites** the history rows' `project_id` (it has to, or the
  project-delete cascade takes a memory's past with it), so a pre-promotion
  instant reports a promoted memory under `_global`.
- **Which timestamp the age is measured from.** The row's own `created_at`, which
  is what the current read decays on, whenever it can answer — it is set on
  INSERT and never rewritten (a snapshot restore carries the snapshot's
  `created_at` back on both its UPDATE and its INSERT). Two cases it cannot, and
  both fall back to the version row's own `recorded_at`, which is at or before T
  by definition: **the row is gone** (a delete takes the memory, so every current
  column is NULL, and an empty `created_at` reads as ~56,000 days — a memory
  deleted yesterday sat at the decay floor in every listing covering the past
  year, on the strength of a column that says nothing about it), and
  **`created_at` is later than T** (no current writer produces that, and it is
  refused because `ageDays` clamps a negative age at 0, which makes a
  future-dated column the most favourable value a row could carry into a ranking
  of the past). The composite is otherwise the same rule at T, so a historical and
  a current listing stay ordered by one thing.
- **The gap is never silent.** The two halves of the read partition the in-scope
  set, and the "no version recorded" half is bounded by T on **both** its tests —
  `created_at <= T` and "no history row at or before T" — because an unbounded
  `NOT EXISTS` answered a different question ("does this memory have history at
  all") and a pre-v17 memory the lifecycle touched after the upgrade *does* have
  history. A resolve, a supersede, a delete or a reflection reuse is enough, and
  the memory then fell out of both halves and vanished from an answer it belongs
  to. The half that reports tombstones exists for the same reason: a pre-v17
  memory deleted after T has no live row either, so its gap is reachable only
  through the tombstone's own project id. Two things make that half count a memory
  once, and both are needed: it **excludes ids that are live again** (a snapshot
  restore reinstates a row under the id it recorded, so "tombstoned" does not imply
  "not live", and a delete → restore → delete memory would otherwise be in both
  halves), and it is **ranked per memory** keeping its newest tombstone (the same
  `(recorded_at DESC, rowid DESC)` the version set uses, because a second delete
  after a restore is a second row and `UNION ALL` does not dedup). Either defect
  alone reaches `CandidateSet.Unrecorded` and every surface's count. (No writer
  needs to file a baseline
  there: every one of those first writes appends a row read out of the memories row
  in the same transaction, so the text it is about to stop holding is recorded
  anyway — which is why `recordBaselineHistoryTx` is needed only on
  `UpdateMemory`, the one write that overwrites the text in place.)

`as_of` (RFC 3339) is a parameter on `ghost_memory_search` and
`ghost_project_context`, and `--as-of` on `ghost context`. In the assembler it is
a **binding, not a filter**: `Run` moves `Now` to it, so a row's age, its validity
window and the trace that describes the block are all decided against T rather
than against the wall clock. The store treats `AsOf` as authoritative over
`Now` for the same reason.

A historical retrieval is **keyword-only**, and says so in the answer, the trace
and the store's leg status. Every statement in the read names `memory_history` or
`memories` and nothing else, so a store that predates v18 — or one whose evidence
table a later migration has not created yet — answers an `as_of` read in full
(`TestCandidatesAsOfDoesNotDependOnMemoryProvenance` drops the table and reads on):

| | current | `as_of` |
|---|---|---|
| keyword leg | FTS5 over `memories_fts` | term match over the versions' own text — `memories_fts` holds current content only |
| vector leg | cosine, when an embedding is available | **not applicable** — an embedding is a vector of the text as it is *now*, and the vectors for the versions being chosen between were never computed. `Condition: vector_only` with `as_of` is refused rather than downgraded, and so is `explain` with `as_of` (`explain` is a projection of the current ranking, and an `as_of` read ranks nothing — so a historical request would otherwise have come back as a present-day payload with nothing to say so; the handler refuses the pair with a message naming both, and `assemble.Run` and `Store.Candidates` refuse it too, as defence in depth for a caller that reaches them directly). The two refusals live at different layers, and the difference matters to anyone auditing the contract: the `explain` one is on an input the tool accepts and is **advertised in the tool's own `as_of` and `explain` argument descriptions**, while the `vector_only` one is a store-level guard on `CandidateRequest.Condition` — a condition `ghost_memory_search` never sets, because its handler hardcodes `Condition: assemble.CondHybrid`. The tool's published schema therefore has no argument that can trigger it |
| ranking | RRF fusion, then decay | matched query terms, then the same decay composite at T — not bm25, so it is not comparable with a current order. The window and the tail are cut the way the current path cuts them, tail included, so a set is at most about twice the window however many versions matched |
| link graph | supersede and near-duplicate demotion, `contradicts` edges | **not read** — `memory_links` records when an edge was invalidated, never what the graph looked like at T. The supersede demotion uses the recorded sequence instead, and the edge status is `not_applicable` so the conflict stage makes no claim in either direction |
| evidence counts (`memory_provenance`, v18) | read on the same snapshot as the rows, recorded in the trace | **not read** — an evidence record is one *observation* of a memory, not a version of it, so nothing in that table can be placed at an instant and a count taken now would be a present-day claim about a past memory. The counts therefore stay zero, and a zero renders as "no recorded evidence", which is why the `as_of` disclosure says the counts were not read rather than leaving that phrase to stand as a claim |
| edges status | `ok` / `unavailable` / `err` | adds `not_applicable`: "no edge joins these candidates" is a claim, and a retrieval that made none must not render it |

The term extraction is the **same** one the FTS leg uses (`ftsQueryTerms`, so the
10-term budget, the identifier ranking and the stopword rule are one rule), and a
term is matched as the PHRASE FTS5 would make it — `ghost_windows_arm64` is three
adjacent tokens — with a trailing `*` still meaning a prefix match.

Every surface states the instant and what did not run, in one shared sentence
(`memory.AsOfSourceNote` plus the assembler's retrieval half) and one shared
omission note (`memory.AsOfUnversionedNote`, for tasks, decisions and learned
context, none of which is versioned). They are `Result.Qualifiers` rather than
`Result.Notes`: a note list is a diagnostic list shown for the empty answer and
bounded from the end, where the first thing to go is exactly the sentence that
changes what the answer *means*.

`ghost context --as-of` runs **none** of the startup side effects the current
path runs. The Obsidian mirror and the session-count bump exist because that
command backs a session start; a past reading is a diagnostic a person asked for,
and counting it would move the present's session number to answer a question about
the past.

Restoring or rolling the store back to a past state is explicitly **not** this —
that is export/import ([#586](https://github.com/wcatz/ghost/issues/586)).

A vector candidate is only scored when its stored vector belongs to the vector
space this process embeds into. Every embedding records that identity — model,
dimensions, and whether the model needs a task prefix (`nomic-embed-text` takes
`search_document: ` on stored text and `search_query: ` on queries, which is why
both halves have to move together) — as
`<model>[:<dimensions>][+prefix]` in `memory_embeddings.model`. A row recorded
under a different identity is a vector from another space, so it is excluded
from the leg and handed back to the embedding worker as unembedded rather than
compared with a query it cannot be compared to. `SetEmbeddingIdentity` supplies
the configured side from `embedding.VectorIdentity`, and the rules that read it
are four, deliberately separate by caller:

| Rule | Site | Applies to |
|---|---|---|
| may this stored vector enter a search? | `loadVectorRows` | both vector scans (`SearchVector`, `SearchVectorAll`), which share one snapshot and one scoring pass |
| may this stored vector act as a *query* vector? | `GetEmbedding` (returns nil) | the link worker and `ghost supersede`, which both search from a stored vector |
| may this memory be compared at all yet? | `UnscannedEmbeddedMemoryIDs` | the link worker's queue, so a foreign row is neither paired across spaces nor marked scanned |
| is this memory covered? | `EmbeddingStats` | `ghost mcp status` and `ghost_health`, which must not report full coverage mid-re-embed — and split the uncovered rows into stale (a vector under a retired identity) and unembedded (no vector at all), since only the first kind has something to rewrite |

Rewriting a row under a new identity also clears the `link_scans` slot the
memory earned in the old space (in `StoreEmbedding`, before the upsert), so the
memory is re-queued for linking and the linker compares it again in the new
space. The re-scan adds current-space edges alongside the ones the old space
produced — nothing deletes the old `related` rows (only `supersede` calls
`InvalidateLink`), and `CreateLink` upserts with `MAX(strength, ...)`, so an
edge whose new-space similarity is lower keeps the strength the old space gave
it; retiring those edges is a separate decision this change does not make. The
failure ordering of the delete and the upsert fails safe in both directions, so
they need no transaction. The foreign-vector warning in `loadVectorRows` is
likewise logged once per retired identity rather than once per search — a
process that reconfigures twice warns about both retirements.

The query-side rules matter because the filter only guards the *rows*: a stale
vector used as a query would be a cosine between two spaces, and the number it
produces becomes a `related` edge or a `supersedes` candidate. The derived snapshot store
`Candidates` runs on inherits the identity too, or an explain request's recorded
facts would describe a ranking the search did not produce. A retired vector never hides its memory: the
text stays in the keyword leg until the row is rewritten.

### Evidence provenance

`memory_provenance` is the table whose name v17 reserved, and it answers the
question the change log cannot ([#673](https://github.com/wcatz/ghost/issues/673)):
**what supports this memory**, as opposed to how it changed. `memories` holds one
set of mutable provenance columns, so a fact claude-code reported in one session,
codex reported in another, and a human verified kept only the last of the three.
Here they are three rows, and none of them is overwritten.

Three concepts stay distinct, and prose that blurs them is a bug in the prose:

| Concept | Where it lives | Question |
|---|---|---|
| **Source** | `memories.source` (`mcp`, `reflection`, `onboarding`, `manual`, …) | How did Ghost *receive* this? |
| **Agent** | `agent` on a row, and on the memory | Which agent produced or observed it? |
| **Provenance** | this table | Which *observations* support it, and when? |

A row is one observation: `observed` (a write path recorded what the host
reported), `imported` (the fact arrived through a portable artifact), `verified`
(somebody checked the fact, written by the validity writers,
[#575](https://github.com/wcatz/ghost/issues/575)) and `legacy` (the migration's seed). A CHECK, not a
convention: a kind no reader knows is a kind no reader can filter.

| Writer | Appends | To which memory |
|---|---|---|
| `Create`, `Upsert` insert | `observed` | the new row |
| `Upsert` fold, including `FoldOnly` | `observed` | the **surviving** memory — see below |
| `RecordDecision` | `observed` | the companion memory, empty of provenance (the tool reports none) |
| `ReplaceNonManual`, fresh insert | a **carry** of its sources' records | the row the rewrite or merge becomes — see below |
| `ImportMemory` | the artifact's own records, then `imported` | the imported row — and the arrival record is stamped when the artifact held a `verified_at`, so an imported fact still counts as checked |
| `RestoreSnapshot` | the snapshot's records, verbatim | a re-created row — see below |
| `migrateV18` | `legacy` | a memory that already recorded a provenance value |
| `Create`, any `UpsertWithOptions` branch, `UpdateMemoryWithOptions`, when the call states a `verified_at` | `verified` | the row it wrote, or the fold target — stamped with the store's own clock, never the caller's |
| `CreateFromCorpus` | `observed` only, **never a stamp** | the ingested row; its `verified_at` is a value in the dataset with no observation behind it, so a record would have to invent the check. The column keeps the dataset's own value |

Two writers deliberately append nothing, and both are cases where a record would be
a claim nobody made. `ReplaceNonManual`'s **reuse** branch updates a row in place, so
that row's evidence never left it. And the *carried* records keep the kind they
were: the observation really was made, by that agent, about that content, at that
time; what it was not made about is the wording now on the row.

**A fold is the case the table exists for.** A near-duplicate save used to be a
discard: the incoming wording became a linked copy and the agent, session and
reference that reported it went with it. The fold now appends the second report
to the row the corpus actually kept. `FoldOnly` — the promotion path, which stores
no row of its own — needs this more, not less: without it the report would leave
no trace at all.

**A consolidation carries, because the foreign key would otherwise forget.** A
rewrite or a merge mints a new id, and the evidence of the row it replaces dies with
it — so every consolidated memory would read "no recorded evidence" from then on,
on the one write path that runs unattended over the whole corpus. The new row
therefore gets a verbatim copy of each source's records, with `carried_from` naming
the memory each came from. A merge names every one of its sources, so two agents
agreeing on one fact survives the merge as two records rather than one.

`carried_from` is the honest half. The copy keeps the kind, the agent, the session,
the reference, the confidence and both stamps — a consolidation is not an
observation and gets to invent none of them — and the pointer says what it is: an
agent reported *something this was consolidated from*, not this wording. It is also
the way back, because the id it names is gone while the change log kept that id's
whole past.

The carry is **one statement for the whole set** (an engine-dependent row order
would put a diff between two exports of an unchanged corpus on the card, so it
carries an `ORDER BY memory_id, rowid`), and it runs *before* the delete that
removes the sources — that is the ordering constraint the whole feature rests on,
and `TestConsolidationCarriesEvidenceToTheRowThatSurvives` fails if the two blocks
swap.

**A restore brings the support back, because the snapshot carries it.** The same
cascade that takes a rewritten row's evidence takes it on restore, and a restore
that returned the text alone would hand back a memory that reads as never observed
in a database that still had the evidence to return. `memory_snapshot_evidence`
holds it beside the snapshot row — a table rather than a column, because a memory
has several records — and is pruned with the snapshots it belongs to, so it cannot
outlive what it describes.

Only the **re-created** rows are restored. A row the replace never deleted is
updated in place, and the snapshot holds an *older* copy of its evidence: restoring
that too would replace a row's own support with what it held at snapshot time, and
silently drop every corroboration recorded since. A pre-v18 snapshot carries no
evidence, and a pre-v13 one recorded no `memory_id` to attribute it to, so a row
restored from either comes back with none — which is the truth.

**Nothing is invented.** Every column except `kind` is NULL when the host did not
report it, and `nullIfEmpty` is what keeps "" from being stored in place of an
absence. A guessed agent or session is a provenance claim nobody made, which is
the same rule the memory columns follow ([v10](#retrieval)) and the reason
`Upsert` with no `Provenance` records a row with every field NULL rather than
skipping the append: the save *did* happen, and "observed by nobody in particular"
is a true statement. `observed_at` is NULL only for the migration's seed, which can
say what the columns hold now and cannot say when the fact was first observed.

**The foreign key cascades, and that is the difference from the change log.**
`memory_history.memory_id` has no foreign key on purpose — the audit of a deletion
must outlive the row, and the `delete` tombstone is the whole point of it. Evidence
has the opposite requirement: "supported by 3 observations" is a claim *about a
memory*, and once the memory is gone the claim is about nothing. So a hard `DELETE`
takes the records with it. A purge also deletes them **explicitly**
(`purgeHistoryTx`), because `Store.PurgeMemoryHistory` leaves the memory in place
and no cascade fires for a row that stays — a purge whose completeness depended
on a connection's `foreign_keys` pragma would be a purge that can report success
over rows still in the table.

**No growth prune, on purpose.** The append is one statement inside the caller's
write transaction, and that is the whole budget: the change log's growth policy is
three statements, and it is what took `TestConcurrentProcessesMixedReadWrite` to
`SQLITE_BUSY` at `BEGIN IMMEDIATE` (see the growth policy above). A second
statement per save to bound a table the cascade already keeps in step with the
corpus is the wrong trade. The growth that remains is bounded in practice: records
die with their memory, so the table holds at most one observation per save for
every *live* memory, and a store folding the same fact ten thousand times has a
change log at its cap first.

**The seed is conditional.** `migrateV18` writes one `legacy` row per memory that
recorded at least one of `agent`, `session_id`, `source_ref` or `confidence`, and
**none** for a memory that recorded nothing. A row with every field NULL would
still say "this was observed", and seeding one per memory would make a store
report "supported by 1 observation" about rows nobody ever attributed — the same
fabrication as inventing a session id, just spread over the corpus.
`verified_at` comes across when the memory holds one (a hand verification is
corroboration, and dropping it would understate the support already recorded) but
does not on its own justify a row.

**Readers.** `Store.MemoryProvenance(id)` returns the records oldest first (by
`rowid`, because `observed_at` is second-precision and several observations in one
session share a timestamp). `Store.MemoryEvidenceCounts(id)` returns
`EvidenceCounts{Observations, Verified}`, whose `Label()` renders the compact form
a trace shows: `supported by 2 observations, 1 verified`. The verified clause is
absent when it is zero, and a memory with no records reads `no recorded evidence`
rather than `supported by 0 observations` — the first says Ghost holds nothing, the
second reads as a measurement of zero support.

**Nothing ranks on this yet.** The counts ride on `memory.Candidate` because
`Candidates` is the assembler's *only* route to the store, and stage 4 records
them in `Signals.Evidence` while its multiplier stays pinned at `1.0`. A memory
with three observations and one with none keep the retriever's order. That is the
deliberate half of [#673](https://github.com/wcatz/ghost/issues/673): ship the
model and the reading, and let a measured change justify the weighting later.

**The portable artifact carries them; the Obsidian mirror does not.** The artifact
is v2, and a v1 file still imports — the change is a nested list, which is a shape
change worth a version bump, and the readable range is an explicit list of the
versions whose rules still hold rather than "everything below". The records nest
under the memory they support, because a bare evidence line cannot be attributed.
An import is deliberately **not** a fixed point: it keeps the records the artifact
carried and adds one of its own for the arrival, because a destination that
adopted the source's records verbatim would say nothing about how the fact reached
*it*. One field is deliberately NOT carried: `carried_from`, which names a memory
that was deleted — the reason the record was carried — so it resolves against
nothing on the far side and the change log that could explain it is not in the
artifact either. The destination holds those records as its own direct support,
which is what they are. The Obsidian vault is a human-readable mirror, not a
transfer format, and its front matter is about the memory.

### Memory lifecycle

`reflect` replaces non-manual/non-builtin memories through a tiered consolidator. It snapshots before replacement, rejects empty results, preserves manual and Ghost-shipped builtin memories, and can restore the latest snapshot.

The LLM tier does not return a rewritten memory list. It returns operations on the ids the prompt renders: `keep <id>`, `merge <id>,<id> -> <text>`, `rewrite <id> -> <text>`, `drop <id> reason: obsolete | superseded by <id>`. A memory the harness has nothing to change is carried through by `keep`, which re-emits the stored row byte for byte, so the row is updated in place and keeps its id, embedding, link graph, age and source. That is the whole point: under the previous free-text contract only a byte-identical re-emission kept an identity, so retyping — including retyping in order to improve — gave the memory a fresh id and cascaded its embedding and links away with it, which a maintenance benchmark over 865 real memories measured as 12 of 13 new ids in one project and 15 of 17 in another. A merge derives its category from the first id it names, its importance from the strongest source, its tags from their union and its scope from the same heuristic the SQLite tier uses: a consolidation run does not get to relabel what a memory is, least of all unattended and into `_global`. A merge or rewrite whose text carries a path, hash, version, hostname or number appearing in none of its sources is rejected and its sources are kept unchanged — the model was measured turning `2.BeXIAhbj.js` into `2.BeXIAhbq.js` while only copying, and Ghost cannot know which side is true, so it declines to write either. That is also why a `rewrite` corrects a claim and never a specific: the corrected value would be an identifier the source does not contain, so the prompt asks for `keep` (or `drop`) instead, and the check is never asked a question it has to refuse. Anything Ghost cannot read — an unreadable operation, an id it did not supply, an id claimed by two operations, a supersession whose target the same response drops, a response carrying no operations — fails the LLM result outright and falls through to the deterministic tier, because applying the readable half of an operation list is a partial consolidation.

**A rejection costs one re-read, not the whole pass.** Failing the response is the only correct reading of it, but discarding it is not the only consequence, and the measured difference was about a third of all runs on v0.35.0 with the free `opencode` model: a model mistyping one ULID — one extra hex character in an otherwise correct answer — failed the strict reader, and a refusal was then the end of the pass. It cost differently depending on the invocation, and the two are easy to conflate. `--require-llm`, which is what the unattended lifecycle phase passes, omits the SQLite tier entirely, so the run exited non-zero with nothing written. A manual run on the `auto` tier fell through to the Jaccard-only tier instead, replacing a consolidation of the whole corpus with a mechanical dedup that consolidates nothing the model judged. So the LLM tier sends the same prompt once more, with the reader's own complaint attached, and reads the second answer under the same rules (`LlmConsolidator.Consolidate`, `buildRepairPrompt`). Three things that are deliberately *not* true of it: no fuzzy id matching, no prefix completion and no application of the answer just rejected — a re-read, not a repair, so a response the reader would refuse is still refused a turn later, and the pass-through is what makes discarding it lossless rather than what makes it safe to keep. Two rejections fail the run with the **second** complaint, the one describing the answer a reader of the log will see; the first is only on the WARN line. The bound is one extra call on one condition: a transport failure is not a rejected response and is never retried here, and both calls share the run's one `consolidation_timeout_minutes` deadline, so a repair spends what is left of the budget rather than extending it.

The complaint has two renderings, because it has two audiences. The one a person reads and the one the repair prompt quotes back to the model that wrote the line carries that line, clipped — it is how anyone can see WHICH operation was wrong. The one a log may carry withholds it (`opRefusal.Safe`) and keeps the reason and the id, because an operation's replacement text is model prose over stored memory and a log outlives the run, which is why the three other lines the tier writes go through `previewContent`. Two log sites go through that seam: the tier's own WARN (`readerComplaintForLog`, fail-closed) and the tiered wrapper's `consolidator failed, trying next tier` (`safeTierError`), the second being the one that fires on a manual `auto` run which then succeeds on SQLite, so its line lands in the log with no failure anywhere to explain it. `safeTierError` redacts a refusal and passes anything else through, because the other error on that line is a harness failure, and `opencode run: signal: killed` is worth telling apart from a refused answer.

Withholding the line is not the whole job, because SIX reasons quote a model-supplied fragment rather than describing one — `parseOpLine`'s unreadable drop tail and unknown operation verb, and from `executeOps` an id that is not one of the input, a superseded-by target that is not, an id claimed twice, and a supersession whose target this response does not carry forward. All six are routed through `clipOpText`, at six call sites and eight invocations, and the source enumerates them so the list can be audited against the code. That gate probes **all three spellings** — as written, lower-cased, upper-cased — and the two folds are what cover the case-SENSITIVE rules, which split by literal case: `gh[pousr]_` is lower, `AKIA|ASIA|ABIA|ACCA`+16 and `AGE-SECRET-KEY-1…` are upper. All three are reachable from model text and each hides a token from the others: the parser upper-cases a supersession target so it matches the stored spelling, hiding a `ghp_…` from an as-written-only probe, while the free-form drop tail is probed in the spelling the model wrote, hiding a lower-cased `AKIA…` from a lower-fold-only probe (and folding that one down is a no-op). The as-written probe is not redundant either: it is the only one that reaches the MIXED-case literals below, which is why `parseOpLine` keeps a raw copy of the verb rather than gating the folded one. A stored id is safe here for two separate reasons, both from the detector's own constants: 32 hex characters is under the two bare-hex floors (`cardanoKeyMinRun` 68, `longHexFloor` 132) though above `assignedSecretFloor` (20), and it carries no provider prefix and no `key: value` assignment, which is what every remaining rule needs. `TestClipOpTextStillGatesARealId` pins that. Each of the three probes is pinned by a test that fails when it is removed, and `TestReaderComplaintGatesAMixedCaseLiteral` covers the as-written one and the raw verb together.

**A known residual, stated rather than implied closed:** the MIXED-case literals — google-api-key `AIza…`, pypi `pypi-AgEIcHlwaS5vcmc…`, JWT `eyJ…`, PuTTY `PuTTY-User-Key-File-` — match only the as-written probe, so a model that re-spells one (`aizasyd-…`) is caught by no fold. Closing THAT would mean lower-casing the rules themselves, which would change what `internal/secret` matches everywhere it is called, including the write boundary where a re-spelled credential has to be judged the same way. It is a gap in the rules rather than in this probe set, and closing it is a change to `internal/secret` with its own callers. A `drop <id> reason: <value>` is refused precisely BECAUSE the tail is free-form, so the value is in the reason — and 60 runes of it holds a whole short-format token (gh[pousr]_, AKIA…, npm_/hf_) and a large part of a long one — an ed25519 cborHex, a PEM body, a mnemonic, a JWT all run past the clip — so without the gate a fragment of a key would sit in the log rather than nothing. `clipOpText` therefore runs the same `secret.Detect` gate `previewContent` applies, before the clip, so a value straddling the boundary is judged whole. Without that this seam was the one in-tier log sink with no value gate, and the one fixture that would have caught it — a rewrite — could not: its replacement text sits inside the line that *is* withheld.

**What that last exemption does not cover, stated rather than glossed.** A harness failure is *not* value-free. `internal/ai` builds those from the child's own output — `harnessFailureOutput` falls back to stdout, because opencode reports on its JSON stream, and that stream carries `text` events holding the model's answer — so up to 1200 bytes of model prose reach that WARN and `lifecycle.log` with it. That is pre-existing and deliberate: the child's explanation is what made the 357 undiagnosable `opencode run: exit status 1: ` entries fixable in the first place (#540), and hiding it again would restore that. Redacting it means changing what `internal/ai` puts in an error, which has its own callers. One refusal sink is also still unredacted, and it is not new: the tiered wrapper's final `all consolidation tiers failed (last: %w)` is printed by `runReflect` on its `error: consolidation failed:` line, and on the unattended path that line reaches `lifecycle.log` whole and the phase's 1200-byte output tail. It does **not** reach the next session's maintenance alert. That alert renders the marker's first line, and the marker is the phase's output tail — so what it shows is the first line the phase printed (`Project: <name> (<id>)` on the unattended path, `DRY RUN …` on a manual dry run), not the refusal, which is the LAST thing `runReflect` prints, after `Consolidator:`, `Memories:` and `Running consolidation...`. Head-trimming the tail to 1200 bytes only pulls earlier lines in. The phase name is there; the exit status is not, because `phaseFailureCause` returns the child's output verbatim whenever the child produced any and folds the code in only on the no-output branch. The full rendering is what a person debugging a stubborn model wants on that line, which is why it was not changed here; a redacted rendering at the command boundary is the change to make, and it belongs to whoever decides what that diagnostic is worth.

Every model-supplied fragment inside a complaint is clipped where it is interpolated (`clipOpText`), so a `drop <id> reason: <a paragraph>` cannot put a paragraph into a prompt, a log and an error message — and that clip is also where the value-shape gate runs, so a fragment that IS a credential is withheld in the log rendering too.

The first complaint is logged at WARN, and the turn is counted in the summary as `repair: 1` on the `Result:` line — which is the append-only `lifecycle.log` on the unattended path, so the token is grepped, not scraped from two streams that would each count it. The count is per RUN rather than per tier (`TieredConsolidator.Consolidate` carries a discarded tier's repair turns onto whichever result survives), because the run that repairs and then falls through to SQLite is exactly the one that has to be visible. That counter's denominator is the `Result:` lines in the same log: a run that repaired and then failed outright never reaches the summary, and its rejection is on the WARN line, which is written whether the run goes on to succeed or not.

A memory leaves the corpus only when its id is named as a merge source, named for a rewrite, or named in a drop with a reason — **and** a surviving output accounts for it, which is the drop guard's 45% token containment with a merge source measured against its own merge — or `--allow-drops` accepts the deletion. Naming an id is necessary but not sufficient: a rewrite whose replacement does not carry the old row's substance leaves that row in the corpus verbatim, and a `superseded by` drop naming a successor that shares nothing with the row disposes of nothing. Every other input is carried through verbatim, and that pass-through is what makes the tier zero-loss rather than merely careful. It is also what an earlier draft got wrong: an unnamed memory used to be emitted by nobody, so it survived only if the token guard *failed* to recognise a survivor — and a guard false positive in the "absorbed" direction is a silent deletion. A measured run lost "SSH to the bastion goes through port 2222, not 22" that way, with no warning and no `--allow-drops`, because an unrelated survivor shared 45% of its tokens. A guard can only re-add what it flags, so an id the harness never mentioned needs no inference at all. An operation may echo the `id:` label the prompt prints in front of each memory; the label is stripped rather than failing the pass.


An LLM tier's answer is additionally bounded by a scale-aware quality gate, from six memories up: a small input must retain 30% of itself, and a large backlog must return at least an absolute handful — `gateBacklogMinOutput`, value 5. The floor is per TIER, so it only bounds a run with nowhere else to go: the `auto` default drops to the mechanical SQLite tier, which is exempt because Jaccard dedup cannot truncate or hallucinate, while `--require-llm`, which omits that fallback, reports a rejection as a failed run. It is absolute rather than a fraction because the prompt asks for a corpus of high-quality memories rather than a fixed count, and a percentage floor would demand more output than that ever asks for on a large backlog. `--tier cli` and `--tier opencode` are held to it too: naming a backend used to hand the bare LLM tier straight to the apply path, where the gate does not live, so those invocations had no floor at all; they now run through the same one-tier wrapper, which adds no fallback and so keeps "exactly this backend" meaning exactly that. The wrapper does not change the reported tier name, so `Consolidator: cli` still prints `cli` rather than `tiered:cli`.

Before replacement, every input memory is audited for a surviving merge target, in all eight categories: an input under 45% token containment in the output it is compared against is re-added verbatim rather than deleted, and `--allow-drops` accepts the deletions instead. With the pass-through above the guard no longer has omissions to rescue, and what remains is narrow. A **merge's** sources are measured against the text of **that merge**, which is the only witness available: a merge source is consumed by its merge, so its own text is never in the result, and an id claimed by two operations is rejected, so a sibling cannot be there either. It used to be measured against the union of the outputs, which was right when the union was a handful of survivors and which the pass-through turned into the whole project's vocabulary — every id the response never named is emitted verbatim — so a source whose substance its merge discarded passed containment on the strength of an unrelated memory that happened to share its words. That is the same class of loss the pass-through exists to remove, reintroduced through the merge branch. A merge that is not in the result has no witness at all, and its sources fall back to the strict per-output test. Everything else — an explicit `obsolete` drop, and every input of the offline SQLite tier, which names no ids — is measured against a **single** output, because there the guard is the only thing between a claim and a deletion and it has to be answered by one survivor. **There is no exemption.** An input the response **disposed of** — named for a rewrite, or named in a `superseded by` drop — is audited exactly like an `obsolete` drop: the corpus has to be able to show the row is gone. An unattended reflect never deletes a memory on the model's say-so alone. The result does still *record* the ids a response disposed of and the text it says took their place, and `ghost reflect` prints each of them under `Disposed of (model's claim)`, so a person deciding whether to pass `--apply` can see that the model tried to drop something; the guarded-drop report beside it says what was actually retained or deleted. The record is not consulted to decide anything, and the report deliberately does not predict the outcome — a second opinion about it would be a second implementation of the guard's decision waiting to disagree with the first. The carve-out that used to exist was the hole: a `superseded by` drop disposed of a row whenever the named successor's text was in the result, and the parser's only rule is that the target survives the response — so naming a neighbour was a valid operation, and two ordinary deployment notes in the same project, paired because they sat adjacent in the prompt, was a silent deletion with no warning, no `--allow-drops` and a zero exit status. Requiring the witness to be *related* closed that and then turned out to be redundant, because the witness text is itself an output and a row at 45% containment against it also passes the per-output scan; the whole branch was dead code. The rule is **keep-biased** on purpose, and the reason is repairability: a stale row that is kept can be demoted, because the lifecycle runs `resolve` and `supersede` immediately after `reflect` and demoting a genuinely stale row is their job, while a deleted row is gone. The failure direction is therefore a duplicate — the old text back beside its replacement — never a silent deletion. The cost is real and worth naming: a supersession whose successor is reworded below the containment bar leaves the stale row behind (this project's own superseded note about calling the Anthropic API client directly scores 0.429 against its replacement), and it stays visible until `resolve` or `supersede` demote it, so a corpus of those grows the input each pass has to read. That is the right way round for a command that rewrites a memory store with no human in the loop.

The result list above it says what the corpus became; the accounting says what became of the rows the run was **given**, which is the question a reviewer or an independent judge is actually asking, and it is the same section for a dry run and for an apply, printed before the write so a dry run previews it exactly. Every input id appears in exactly one line or one count, every count is a count of **ids** — a merge may name any number of sources, so `Merges (3)` is three ids folded into one row — which is what makes the section checkable by adding its numbers up to the input total. Ids are printed in the spelling the store holds rather than the one the response used, because `memIDKey` compares them case-insensitively and these ids are what an operator copies out of the report to look a row up. The sixth bucket, **deleted**, is the one that makes the section an audit: the rows an apply removes, one line each with its reason, decided by the replace's own reuse pass rather than by whether the row's text is still in the result. `ReplaceNonManual` reuses stored rows by byte-identical content and claims ONE per emission — the row in the same category, else the oldest, in `(created_at, id)` order — and deletes every candidate left in the bucket, so two inputs holding the same bytes both have their text in the result and only one of them is still there afterwards; keying on the text called both of them carried, which is this report's own defect pointed the other way. The two reasons are different outcomes and the section has to be able to tell them: a row nothing carries is a loss, a row whose identical twin was reused is a deduplication whose knowledge is still in the project. On the LLM path the bucket is empty or holds one row a post-filter removed the carrier for; on the SQLite tier, which names no ids, it is every duplicate the tier absorbed. The reuse rule is re-implemented in `claimedRows` rather than shared — the store's copy runs against an open transaction — and the two are held together by tests that assert the id the section reports as deleted is the id a real store stopped holding. `--promote-globals` deliberately does NOT reach the accounting: an input no operation named is always carried by its own pass-through, and a pass-through states no scope, so it is project-scoped under either setting; a named input is the one a promotion could change, and a named input is owned by its own line. A merge prints `new <- <ids> (<bytes> B from <bytes> B)`, a rewrite prints the text it wrote, a drop prints the reason the response gave and what the drop guard decided about the row, a refused merge or rewrite prints the ids the grounding check kept and the identifiers that caused the refusal, and two counts cover the ids a `keep` named and the ids nothing named. A merge names no successor id because the row it produces does not exist until the apply writes it — `memory_history`'s `related_id` is where the identity becomes visible, and a preview that had to guess at it would not be a preview. A `superseded by` reason names the successor that replaces the dropped row, so that id is quoted on the drop line while its own bucket accounts for it: the accounting is per input id, not per mention. `reflection.AccountInputs` computes the partition as a pure function of the result, and `cmd/ghost`'s `reportInputAccounting` only formats it. Three of the buckets are the result's own records, which is why `ReflectionResult` grew them: `Kept` (an explicit `keep` is otherwise indistinguishable from the pass-through), `Drops` (an `obsolete` drop names no successor, so the result previously said **nothing** about the id it disposed of) and `Refusals` (a grounding rejection was a log line inside the tier, invisible to every reader of the result). All three are records; nothing acts on them, and the drop guard still audits every disposed row on its own evidence. A guard verdict annotates the line that named the id and says whether the row was re-added or `--allow-drops` accepted the deletion, so a drop line cannot be read as a deletion that did not happen. `ghost reflect --full` prints the memory text whole; the default stays the 120-byte preview.

A round is also reported when it compresses hard: a corpus of six consolidatable memories or more that keeps less than half of them prints a `>50% reduction` warning, counted as the memories the project ends up **holding** rather than the ones emitted under the project scope. A cross-project candidate is counted as retained in two of the three promotion states and not in the third, which is why the count has to be taken where it is known. With promotion off every candidate is folded back into the project, so all of them are survivors; with promotion on, a candidate that could not become a `_global` row is written back into the project too and is likewise a survivor; but a candidate that *did* become a `_global` row has left the project deliberately and is not counted as retained, because counting it would overstate survival and hide a compression that did happen. Only that third state is knowable after the write, so an apply with promotion on reports after the write rather than before — a count built from the project-scoped memories alone would understate retention by the whole candidate set, and a run where every promotion failed would report a far larger reduction than actually occurred. A dry run has no write to wait for, so it reports before. With promotion on that is the optimistic count and the note says so, since a candidate it cannot yet place is counted as though it had left the project; with promotion off every candidate is folded back into the project either way, so the count there is already the final one. It is a warning, not a refusal: the number of operations a response carries says nothing about how much it folded, and on the unattended lifecycle path it is the only report a compression gets, on the stderr of a detached process, with the exit status still 0.

### Retention tiers

`memories.retention` is one of `session`, `project`, `persistent` (a CHECK, `NOT NULL DEFAULT 'project'`), and `memories.expires_at` is derived for a session save and stated by nobody else. `migrateV19` adds the columns and updates no row: a memory that persisted until somebody resolved it was a `project` memory, which is what every pre-v19 row reads as afterwards, so adopting tiers costs an existing corpus nothing.

`expires_at` has exactly one source. A caller cannot state an expiry on a save, so there is no way for a save to schedule the memory it just wrote for deletion, and a NULL expiry is never a prune candidate.

Several readers cannot name a column a migration added, because they open the store
read-only and cannot migrate it: the session-start block's retrieval, `ghost context
--as-of`, and anything else on `memory.OpenReadDB`. Two shapes answer that, and which
one applies depends on whether the reader has the row already.

A reader that has to *select* the tier substitutes for the column rather than
failing on it, and the substitution is owned in ONE place —
`internal/memory`'s `passiveColumnsFor`, which selects `memoryColumnNames` with
`retention` replaced by `'' AS retention` below v19 and `scope` by
`NULL AS scope` below v12. The two literals are not interchangeable, and the reason
is the scanner rather than taste: `retention` is bound as a plain `string`, so a
NULL there is a `Scan` error, while `scope` is bound as `sql.NullString` and NULL is
the honest value for "no scope stated". The ORDER BY follows the substitution for
the reason a scan failure used to: a statement that selected a literal while
ordering by the real column orders by nothing, and a passive bucket states its own
order instead. The two loaders that each carried a private copy of this probe are
gone — the session-start block reads through the seam, and a second version floor
in a caller is a second answer to "which columns does this store have".

A reader that is *handed ids* takes the protection from its caller instead, which is
what `DemotionPenalties` already did and what `SupersedePenalties` does now: both
demotion lookups are called on a read-only handle (`GetTopMemories`, the `Candidates` snapshot an explain request records from, and
the assembler's near-duplicate stage over a passive bucket), so a statement naming
`target_mem.retention` fails in full —
a superseded memory outranks its replacement and a spurious diagnostic reaches stderr
on every session start. The caller already holds the hydrated rows, and a caller
holding a row it did not read is a caller that cannot protect it, so every one of
them builds the map from the rows it already read. The historical reader's tier was
removed for the same reason plus one of its own: see below.

A tier is a claim about a memory's owner, not part of its text, and nothing that replays text carries one. The portable artifact does not (an imported memory arrives as `project` — consolidatable, never pruned, which is the direction that keeps a memory rather than the one that schedules it); a consolidation's own output is a `project` row, because a tier the model never chose is not a protection; and `ghost reflect --restore` revives a deleted memory as `project`, because neither `memory_snapshots` nor `memory_history` records a tier and there is nothing to restore one from. All three resolve to the durable default, which is the direction that cannot surprise a user by ending a memory's life.

`persistent` is the one tier a default may not take back, and the exemption is asked in two places, not one. At the write: `ReplaceNonManual` (snapshot, replaceable set, concurrent-save set, output delete), `RestoreSnapshot`'s UPDATE, `ResolveCandidates`/`ResolvedCandidates`/`SetResolved`, and `supersede.SelectCandidates` — before the classify call, so a verdict that could only produce the refused edge is never paid for. And at the consequence: `SupersedePenalties` and `DemotionPenalties` both spare it, because an edge written before the tier was declared would otherwise sink the row on the next search. A protection that survives one pass and is lost in the next is not a protection, and a user cannot be asked to re-save a memory to defend it. Both take the caller's *protection* map (pin or tier) rather than a list of pins, for the read-only reason above — and every caller builds it, since all five of them were handed a pin list at one point or another. `SupersedePenalties` does NOT carry that through to a pinned row: its map is tier-only, so a pinned superseded target still sinks below its replacement exactly as it did before #587. A pin keeps a row visible without declaring its claim current — `DecayFactor` and the near-duplicate demotion treat it as brand new, but a `supersedes` edge asserts that one claim REPLACED another, which is a claim about the text rather than about the row's visibility. The pin-sparing supersede variant is deferred (issue #739); `TestAPinDoesNotStopASupersedesEdge` pins the pin boundary and `TestASupersedesEdgeDoesNotSinkAPersistentRow` pins the tier boundary.

`session` rows carry a bounded decay — tau 7 days, floor 0.5 — inside `DecayFactor`, whose SQL mirror `DecayRankingSQL` is formatted from the same constants. The bound has two ends and both matter: the factor never exceeds 1.0, so recency — the strongest signal in the composite score — can never on its own put a conversation-scoped row above the durable memory it duplicates, and it never falls below the floor, so an old session memory is dim rather than unfindable. `explain` reports the tier and the factor per row (`retention`, `retention_factor`) and names the signal in a note when any candidate carries one.

A near-duplicate save **raises** the surviving row's tier and never lowers it, because a protection the caller asked for and did not get is a false report, while lowering would let a `session`-scoped restatement of somebody else's durable memory schedule that memory for deletion. The save result names the row that carries it.

A tier filter and an `as_of` read are refused together, at `assemble.validateRequest`, for the same reason a vector-only request is: no version records a tier, and the only value on offer would be the one the row holds *today*, so answering from it would say "in the tier it is in today" without saying so. `AsOfRow.Retention` is therefore always empty — deliberately, which is the difference between a field that says nothing and one that lies. Tags, pin and scope are read from the live row for the same reason a version has none of them, but they existed before `memory_history` did, so that read is a documented borrow; the tier did not exist when the change log did, and nothing consumes the value anyway (the as_of decay passes `project` explicitly, because a version's score is the one it would have had). `TestMemoriesAsOfOnAStoreBehindTheTierColumnStillReads` drops both v19 columns and reads on, because that statement runs on a handle `ghost context --as-of` opened read-only.

`ghost prune` is the only thing that removes a `session` row, and it is never run for you: no lifecycle phase, no hook, no scheduler calls it. It is dry-run by default, and the preview is a read built from the same predicate, scope and order every apply batch re-derives — so a dry run cannot describe rows the apply would not have selected, and the preview writes nothing at all. That parity is about the PREVIEW, not about what an apply reports: each batch re-derives the predicate under the write lock, so a row a save that landed in between pinned, re-tiered or re-saved stops matching and is spared while still standing in the candidate list. An applied report is therefore counted and listed from `RemovedIDs`, and the rows it did not take are named as spared rather than dropped from the account — headlining the candidate count claimed removals nobody performed (3050 against 3049 removed, with the spared row still live in the store). The spared line names the change rather than the outcome, because a write that removed the row outright leaves nothing behind to be spared. An apply deletes in bounded batches of 500 rows per `BEGIN IMMEDIATE` transaction, each batch with its own tombstones and its own rollback, so the store's write lock is never held open for a whole backlog; a batch that fails rolls back whole and the report carries what actually committed. Each removal appends a `delete` tombstone to `memory_history` before the DELETE, in that batch's transaction. Its activity term is `COALESCE(last_accessed, max(updated_at, created_at, expires_at))`, compared through SQLite's `datetime()` because the columns it coalesces do not all hold one shape: `Store.Touch` writes `last_accessed` as RFC 3339 while `datetime('now')` and `sessionExpiry` wrote the others. It is `max` over three columns rather than a fourth `COALESCE` term because `created_at` is `NOT NULL`, so a trailing term is unreachable, and the question is the NEWEST stamp rather than the first one present — which is what lets a fold, the one write that extends a session row's life without touching `updated_at`, count as activity (#772). Two consequences, both in the safe direction: an untouched session memory saved at T is removed at T + `SessionTTL` + grace rather than T + grace, and a row whose expiry a fold refreshed is not eligible until that fresh TTL is up. A text comparison between them agrees across dates and disagrees within one — 'T' sorts above the space — so a row read earlier on the cutoff's own day would be kept and the prune would run a day late. Nothing in production records `last_accessed` today (`Store.Touch` has no caller), which is why that would have been invisible; a guarantee that holds only while a column has no writer is not a guarantee. In practice the grace is measured from the row's last **write**, and the default is a week for that reason.

## Credential guard

**No write path stores a credential value.** Stored content is not private to the machine that wrote it: it is embedded, replayed into the context of every later session in the project, mirrored into the Obsidian vault, and quoted into the prompt of the next `reflect`, `resolve` or `supersede` call — which is a CLI harness talking to a third-party model. A credential that reaches the database has already been copied to all of those, and the database is a file the user syncs and backs up.

`internal/secret` decides whether a value looks like a credential, and the store layer enforces it. `memory.rejectSecret` is the single seam, called before any statement on every path that writes caller-supplied text: `UpsertWithOptions` (and therefore `Upsert`, `UpsertWithProvenance`, `ghost_memory_save`, `ghost_save_global`, and the first-contact import), `UpdateMemory`, `RecordDecision` (including each entry of `alternatives`), the three task writers, `UpdateLearnedContext`, and the four portable importers `ImportMemory`/`ImportTask`/`ImportDecision`/`ImportProject` (a project's name and path are replayed into every later session's digest too; `repo_remote` needs no guard because `NormalizeRepoRemote` strips the userinfo). The importers are the ones that matter most for where the text came from: they write with raw `INSERT`s rather than through `Upsert`, and their input is a JSONL artifact that arrived from somewhere.

The importers place the guard in a narrow window — after the `SELECT 1 FROM <table> WHERE id = ?` presence check and before the `apply=false` early return — and both edges are load-bearing. Above the presence check, a record already in the store would be *refused*, turning the portable format's "never overwrites an id that already exists, so re-running is always safe" into a hard failure over a row that is not being written; re-importing an artifact a pre-guard build exported would fail on records that were previously no-ops. Below the `apply=false` return, a dry run would classify a record differently from the apply run it previews, and that parity is the only reason a dry run is worth running. A refusal is a `*SecretContentError` unwrapping to `memory.ErrSecretContent`, naming the field and the credential format and never the value — the message itself reaches the log, the saving agent, and a model.

The detector works on the *shape* of a value, never on the words around it. That is the whole design, and it is what separates it from the keyword heuristic reflection already uses for a different question (`looksLikeSecret`, which decides whether text is safe to *widen* to every project and therefore must not reject a save). A memory store's ordinary vocabulary — "rotate the password quarterly", "the access token lives in the runner env", "the tokenizer keeps a 30k vocabulary" — must keep saving, so every rule needs a value: a fixed provider prefix at a realistic length, a complete PEM block, a labelled Cardano key field, an unbroken hex run long enough that it cannot be a digest and mixed enough that it cannot be padding, a whole 24-word BIP-39 mnemonic, or a credential-shaped name assigned a value no one documenting the setting would have written. `internal/secret`'s tests are half positive and half negative for that reason; the negative half is this repo's own database. Two of the rules need the matched text inspected rather than a boolean, so both walk *every* candidate in the save: a first-match implementation would let a `${DB_PASSWORD}` placeholder earlier in the text shadow a real credential later in it, which is not an adversarial ordering but what an incident note reads like.

Reflection output is filtered at the write boundary, and both halves of that position are load-bearing. `cmd/ghost`'s `applyReflection` is the single seam every proposal passes through on its way to `ApplyReflection`, and it runs *after* the drop guard's audit. `dropCredentialProposals` returns fresh slices rather than filtering in place, and `applyReflection` returns those **post-drop** slices to its caller: the `Applied: N memories consolidated` line counts them, so returning only a count left it counting the caller's pre-drop lists, and a partial drop reported 2 written when 1 was. It is a drop rather than a refusal because `ApplyReflection` replaces a project's corpus in one transaction, so a refusal would roll back an entire consolidation over one hallucinated value. It is after the audit because the audit asks which **inputs** the output failed to account for, and `executeOps` emits every unclaimed input verbatim — so a memory the tier carried forward is byte-identical to the output that carried it. Filtering before the audit makes that input look unaccounted for, and both outcomes are wrong: `RetainGuardedDrops` re-adds it verbatim and the credential is written back, making the drop a no-op *after* the audit has printed its content to stderr; or some other output happens to cover 45% of its tokens, the audit stays quiet, and `ReplaceNonManual` deletes the stored row with no `--allow-drops` — the one deletion path the drop guard exists to close. At the boundary the memory is still accounted for, so `RetainGuardedDrops` does not re-add it and the drop is not a no-op. It is, however, **deleted from the store**: `ReplaceNonManual` removes every replaceable row the emitted set does not account for, and this one no longer is. That is the correct outcome — the stored row IS the value, and leaving it is the leak — but it is a deletion, so the report describes the mechanism rather than counting anything — `dropped` counts *proposals*, and the rows the replace removed is a different number in both directions, since a fresh merge carried nothing and a `manual`/`builtin`/pinned/resolved row is not in the replace's candidate set — and it fires only when a project replace actually ran. A round the drop empties writes nothing, is reported as `Applied: nothing` with the surviving row named, and **records no skip fingerprint**: the corpus is byte-identical, and a fingerprint over it is what makes `--skip-unchanged` skip the project forever, so a stored credential would never be revisited. `applyReflection` returns an `applied` flag for exactly this, and it is false whenever nothing was written; a removal claim printed before anyone knows whether a replace runs closes the incident in the operator's head while the value sits in the database. `--allow-drops` does not gate it, because that flag is the operator authorising what the **model** chose to drop. Clearing a credential out of a database is therefore not only a report-first scan: an unattended `ghost reflect --apply` removes the rows it finds, and says so.

The per-drop report carries format, category, scope and length, never content — and that guarantee is enforced at **every `cmd/ghost` print site that renders stored memory text in a report** (the lifecycle listings and `ghost history`), not only at the write boundary. `displayProposal` substitutes `<withheld: format, category, bytes>` for the content wherever `ghost reflect` echoes a proposal or a guarded drop, because the proposal listing and the drop-guard warning both printed 120 truncated characters and that stdout is the append-only `lifecycle.log` in the autonomous path. The other renderers are that same substitution over a different cut: `displayClaim` for text a result records without a category (a rewrite's replacement, a disposal claim's witness, a fold's discarded wording), `displayStored` for a stored memory in a one-line listing — it keeps `firstLine` at 70 characters and withholds the row whole instead — and `displayProposal`/`displayClaim` with no limit for `ghost history`, which prints the text in full because a history whose text is elided cannot answer the question it exists for. The marker is built in one place (`withheld`), so a new site cannot invent a second spelling, and it names the format, the category and the byte count because a report that says only "redacted" is indistinguishable from a report that lost the row.

**The class is a report, and the path outside it is deliberate.** `ghost context` and the SessionStart hook's digest print the same stored rows — `internal/mcpinit`'s `quoteData` wraps a memory, task, decision and learned context in `«»` and nothing else — and they are outside the substitution on purpose. They are a data feed rather than a report a person reads: a memory withheld from the injected context is silently out of every later session, which is a worse failure than printing one, and the same text is already reachable through `ghost_memory_search`, whose contract is to return what is stored. The control on that path is the write boundary plus a store scan, which is the same answer the search tool has.

**Four listings were the sites left out**, and the argument for them was that the write-boundary guard already refuses their input. That argument is about the databases this build writes, and it says nothing about a database written before the guard existed: `rejectSecret` is not retroactive, a pre-guard row can still hold a value, and 70 characters is more than a GitHub PAT needs. They are `ghost resolve`'s confirmed listing, `ghost resolve --reassess`'s kept listing (one shared renderer, `memoryLines`), `resolve --mark`'s per-row listing and `supersede --withdraw`'s target text — the last two exist so an operator can confirm from the output that they acted on the note they meant, which is exactly the reader who is holding a stale, pre-guard store. `memoryLines` is a named function because both resolve listings were inline loops inside `runResolve`, which cannot be called from a test: it opens a store, builds a harness provider and calls `os.Exit`.

**`ghost history` carries both layers, and the print site is the second rather than the only one.** History content is redacted at **write** time by the `ghost_history_content` filter in `internal/memory`, and that is the right layer for it: it is the only one that also covers the reads Ghost performs itself — an `as_of` search answers from a recorded version and feeds session injection — and every future reader of that table, none of which would have to remember a print site. It is also the layer that erases rather than hides, which is why `ghost history purge` remains the redaction path for what is on disk. But a filter installed today cannot reach the rows already there, and it does not cover `merged_content` at all: that column is a plain `?` beside the filtered one, holding the wording a `FoldOnly` fold dropped, so on a pre-guard store it is the one text field `ghost history` prints that the write-time filter never saw. `printHistoryEntry` and `printHistoryJSON` therefore substitute as well — the `--json` form too, because a piped stream lands in a file as surely as a terminal scrolls away, and two forms of one command disagreeing about the same row is wrong either way, while only the *values* change: a clean entry still decodes to exactly what the store returned. A row written before the filter existed renders as `<withheld: …>`, and the write-time redaction's own notice still renders as itself, because the two are not the same statement and only the notice says the text was removed on the way in and that purging erases the rest. What none of this reaches is a read Ghost performs over stored text as data: `ghost_memory_search` returns a pre-guard row's text like any other, and `ghost context` injects it into a session's instructions the same way. Those are the report-first job a store scan and `ghost history purge` belong to, and the write-boundary guard has never claimed to do them.


**The Cardano rules are discriminated on the CBOR header**, because the negative corpus for them is not prose — it is the key file formats themselves, and they collide by construction. A signing key and a *verification* key are the same 32 bytes, so both carry `5820`; an *extended* key is a 32-byte key plus a 32-byte chain code, so its private half and its **published** half both carry `5840`; the KES signing key carries a two-byte length (`5902 60`) that a Plutus script also carries. Only three things separate them: the CBOR length byte, the `"type"` envelope, and the surrounding line.

| CBOR length | shape | verdict |
|---|---|---|
| 32 (`5820`) | cold / payment / stake signing key, **and every verification key** | refused only on a line that also names a signing key |
| 64 (`5840`) | **extended** signing key, and the published extended verification key | refused unless the enclosing JSON object names a verification key and labels the value |
| 128 (`5880`) | no published counterpart | refused on the tag |
| 608 (`5902 60`) | the KES signing key — the one two-byte length that is a key | refused on the tag |
| 417+ (`5901 a1`…) | a Plutus script wrapper | stored |

A 64-byte value is refused **unless its enclosing JSON object publishes it** — it names a `VerificationKey` *and* labels something as key material — because a 64-byte value is an extended key and the verification half of that pair is published; refusing the tag unconditionally refuses a memory about a wallet. Both conditions, because either alone is satisfied by a document that merely mentions the other. The run floor is **68** hex characters, not 64: a 32-byte transaction hash is 64 and a key's `cborHex` adds a 4-byte CBOR header, and "signed with payment.skey, submitted tx <64 hex>" is one of the most common Cardano memories there is. The walk is a post-table pass, not a rule, because the decision needs the length *and* the envelope *and* the line.

**That exemption has had three scopes, and the sequence is the lesson.** It started as the whole text, which made it a property of the memory rather than of the value: one `ghost_memory_save` recording a pool's published `cc-hot-StakeExtendedVerificationKey` — a phrase a block producer writes constantly — also stored an extended **signing** key pasted anywhere else in the same content. Scoping it to the **line** fixed that and broke pretty-printed envelopes, where `"type"` and `"cborHex"` are on separate lines, which is what any indented document is. The scope that holds both is the **enclosing object**, bounded by its own braces: a JSON document's fields *are* the envelope, so a vkey name on the line above its own value describes that value, and a name in a different object does not. The backward scan is the part that carries it, and it stops at a closing brace as well as an opening one — a `}` above the value means the nearest object has already closed — which is what separates two objects in one memory from one object in one memory. A closing brace is required *after* the value too, so a truncated document is refused rather than exempted, and the backward read is fenced at 4 KB. The 32-byte branch still reads the **line**, and should: a 32-byte value is a signing key or a verification key with nothing in the envelope to tell them apart, so the line is the widest scope that can carry a decision. A `"type"` field alone is not a key either — `{"type": "StakePoolSigningKey_ed25519"}` in a runbook is documentation, and a pool operator's runbook names that type on its own, so the type rule requires a value beside the name.

**A URL userinfo password** is refused unless it is a template (`${VAR}`, `<db_password>`), not all capitals, not a placeholder word, not a product's default account name, and not equal to its own user — `postgres://postgres:postgres@localhost` is the most common DSN in a README and is not a secret by any reading. A field named after a **digest** of its value (`token_sha`, `fingerprint`, `checksum`) holds a digest, not the value, and storing a digest is the correct thing to do. A value that is a **dotted identifier path** names a place rather than holding a thing: every credential encoding Ghost knows excludes `.`, so `opts.OAuthClientSecretFromVault` and `secrets.GITHUB_TOKEN` are the same judgement arriving through different surfaces.

**The per-match cost is bounded by the next assignment, and the report is scanned in full.** Two post-table passes need the text after a value — the shell-variable test and the quoted-argument scan — and both used to recompute the rest of the *text* for every match, so a save holding *n* assignments cost *n* scans of everything after the first. There are now three bounds, each tightening the last: line ends are indexed once per `Detect` and found by binary search; a command's argument list ends at the next `;` or `|`, because a pipe is where a new command begins; and it also ends at the **next candidate**, which the walk already knows. So each lookup reads one command's worth of text and the lookups do not overlap. 256 KB measures in the low hundreds of milliseconds, pinned by `TestDetectIsLinearInLineLength`. The mnemonic pass also built a `[]string` of every word in the content before looking at the first, which was 67% of all allocations (~10 KB per 4 KB save; now one allocation per call).

Two things are recorded rather than fixed. A keyword prefilter in front of each rule is the obvious remaining win — the profile puts essentially all of the ~2 ms per 4 KB save in regexp's own machine, over about twenty backtracking scans — but Go's `Regexp.LiteralPrefix` returns empty for a pattern whose first instruction is a word boundary, which every rule here has, so a *derived* prefilter is useless and a hand-written one per rule is a silent false negative waiting to happen. **Both cost properties are mutation-verified, and finding out took a second fix.** The first attempt was not enough and the check that proved it was a fixture I had got wrong: the linearity test held many `$relay_addr` tokens but no `=`, so `assignmentRe` matched nothing, the loop never ran, and the quadratic code was never on the path. A performance test that does not execute what it bounds cannot fail. The fixture now plants complete `$aN = <22 characters>` assignments with keys that are deliberately not flagged, so the walk runs to the end of the line where the cost is paid — and it immediately failed, at 66x the time for 8x the input.

The line index had removed the newline scan and the statement cut had removed the scan to the next `;`, but a line with **no separators** — which is exactly what a wall of `$a = value` is — still left the flag regex reading to the end of the line for every candidate on it. The bound that holds is the **next candidate**: a command's argument list ends where the next assignment begins, which the walk already knows, so each lookup reads one command's worth of text and the lookups do not overlap. 2,000 candidates on a 52 KB line went from **1.83 s to 27 ms**, and 8x the candidates now costs 10x the time. The linearity assertion is the **median of per-round ratios over interleaved rounds**, and two earlier estimators failed in opposite directions, both of them the estimator rather than the property. A single shot per size passed locally and read 7.3x on CI (944 ms then 6.9 s) — a loaded runner, not a quadratic scan — which made a 6x bar a red check. Taking the minimum of five runs per size was **worse**: the small measurement benefits more from noise removal than the large one does (514 ms against 2.28 s), so min-of-N raises the ratio. Timing both sizes inside one round is the robust form, because a GC pause or a stolen timeslice inflates both and cancels. The bar is **8x**, which is not a round number but the geometric midpoint between the linear prediction (4x) and the quadratic one (16x) — the tightest bar that treats the two failure directions symmetrically. Measured: 3.4-4.5x here, and **16.04x with the per-match rescan restored**, so the separation is roughly 2x on each side.

**Nothing bounded `detectQuotedArgument` until this round, and the gap was real.** Both cost fixtures have no flag after the value, so `valueIsCommand` is false — and the quoted scan sits inside the same `if shellVar && valueIsCommand` — so neither reached it, while a comment claimed one of them did. `TestDetectBoundsTheQuotedArgumentScan` closes it, and its fixture has to satisfy **four** things at once: a flag after the value, no quoted argument that looks like a credential, a value `isCommandWord` accepts (so the loop `continue`s instead of falling through to a finding), and **a quote character on the line**, because the scan returns before its regex when the line has none. That last one is what made the first version of this fixture useless: the mutation that made the scan read the whole line instead of the window passed at 7.85x, identical to the honest reading, because the changed line was never executed. `ConvertTo-SecureString` is what makes the third reachable — it is a real cmdlet of 22 characters, which clears the 20-character value floor `assignmentRe` requires, where `Get-Credential` at 14 matches nothing at all. With a quoted `-Label "pool"` on the line, the same mutation reads **100x** against a 7.9x honest reading.

Its bar is 20x, and the arithmetic is recorded because I got it wrong first: with 8x the input the **linear** prediction is 8x, so a bar of 8x sits exactly on an honest measurement — this one read 7.85x and would have failed on a quiet machine. Quadratic here is 64x, so the geometric midpoint is sqrt(8·64) = 22.6.

That fixture also had to change, and it is the second round of a lesson this work has now taught three times. It carried a `-asPlainText` flag after each value, added so that "each candidate reaches the flag test" — and it did, and the test went on **passing with the quadratic code in place at 4.02x**, because a flag makes the flag test return true, the walk returns on the *first* candidate, and the per-match cost is never paid. Reaching an expensive line is not what exercises it; scanning the rest of the line to decide is, and that happens whether the answer is yes or no. Without the flag the same mutation reads 16.04x and fails. A fixture that returns early cannot bound anything.

The mnemonic splitter is checked by **allocated bytes scaled against content length** rather than by allocation count, because `strings.FieldsFunc` returns one slice however many words it finds — a count cannot see the regression at all. Nor can an absolute bound: race instrumentation alone moves the same code by two and a half orders of magnitude, so a bound tight enough to be meaningful locally fails on a loaded runner. The bar is a **ratio** — 8× the content must not cost more than 1.5× the bytes per call — set from the four numbers the two forms actually measure, in both instrumentations, with all four written down beside it: the walking form 1.0× plain and 0.3× under `-race`, the collecting form 10.6× and 2.7×.

Getting an instrument that could carry that took three attempts, and the two that failed are worth recording because each looked like a flake and neither was. `testing.Benchmark`'s `AllocsPerOp` is an average over a GC-paced loop, so it measures the whole process's allocation state: run it inside a package where other tests are also allocating and it measures them too, and the byte ratio failed intermittently at 0.8× against a 2.0× bar while passing every time it ran alone. Minimising over five runs narrowed that and did not remove it. `MemStats.TotalAlloc` with GC off for the window is **exact** rather than sampled — 129 B/op at 4 KB and 129 B/op at 32 KB, 1.0×, identical across eight consecutive runs. The residual noise was then the process's own floor: eight calls of a 500-byte fixture is a 4 KB signal that one background allocation doubles, so the 4 KB figure read 569, 8975, 3582, 569, 8975 across six runs. And the figure stayed bimodal at 421 or 4,203 B/op until the cause was `runtime.GC` clearing every `sync.Pool` — Go's regexp keeps its match machine in one, so the first call after a collection rebuilds it — which is a **one-time** cost measured as a per-call one. The window now warms up after the collection. Both assertions are ratios rather than timings, and both were confirmed RED against the code they name under `-race` as well as plain.

A refusal never prints the value it refused, and that reaches the *report* paths, not only the error. `ghost import` echoes a record's own words back in its per-record line and in its rejected list, so `portable.safeDetail` substitutes the record's id whenever the **whole** of the field holds a credential — not the 60-rune prefix the label is cut from, since a token is longer than the cut and `ghp_Ab12Cd34Ef5…` scans as clean and prints half a live credential on a record being printed for an unrelated reason. The prefix is still what gets printed, so a record whose first line is clean keeps its preview, and `labelOrID` and `printRecordLine` both refuse the detail outright for a `*SecretContentError`. The substitution is where the detail is built rather than where it is printed, because a record can be rejected for an unrelated reason — a missing task status, an unknown project — and still carry a credential, and because `RecordResult.Detail` is consumed from another package.

The rule is measured against a real corpus as well as against hand-written cases, because a memory about credentials is full of things that look like one. Running the detector over longmemeval-s (500 records, ~25,000 conversation turns) found eleven distinct lines it flagged; **three are genuine leaked credentials** — a `dckr_pat_` Docker Hub token, a `gho_` GitHub OAuth token, and a 40-character hex SparkPost key — and the other eight were pasted code and pasted documentation, each of which fixed a gate: a value that is an expression (`PasswordResetTokenGenerator().make_token(user)`), a key that is a shell variable (`$domainAdminPassword = ConvertTo-SecureString …`), a value that contains its own key's name (`accessToken: req.body.accessToken`), and a PEM rule that matched a Snowflake help page naming both markers in one sentence. A backslash is deliberately *not* an expression character, because a markdown-escaped `dckr\_pat\_…` is a real token.

Three boundaries remain deliberate. `CreateFromCorpus` is outside it, and exists because of that measurement: the corpus cannot be ingested if the detector is right about those three credentials, and a retrieval benchmark that refuses its dataset measures nothing. It is a named function rather than a flag, it is not on `provider.MemoryStore`, it is not in the MCP surface, and its only callers are the three `bench/` dataset seeders. The claim justifying it is deliberately the narrow one: the row lands in a scratch database that dies with the run, and is never injected into a session's context, never mirrored to the vault, and never quoted into a harness prompt. It is **not** "never embedded" — the two seeders that run a non-fts condition embed every ingested turn and send it to Ollama — and the narrow claim is the one quoted because the wide one is what a future author would cite to widen the carve-out to a bench run against a real store, where the vault and reflect exposures are real. `RestoreSnapshot` and `SeedGlobalMemories` are outside the guard — they write byte-exact data into a restored store, and they are outside the `MaxContentLen` contract for the same reason; a restore that silently dropped rows would be worse than the leak, because a restore is a user's own database coming back. `ReplaceNonManual` is outside it because filtering there would be actively destructive: it deletes every replaceable row the snapshot does not account for, so dropping a credential-shaped memory from the emitted set deletes the stored row, and an unattended nightly reflect would silently remove a credential from a pre-guard database with no report and no way to recover it. That is the same reason the reflection drop guard requires an explicit `--allow-drops` before deleting anything. `applyReflection` drops a credential-shaped proposal before the store sees it — after the audit, for the reason above — and clearing a credential out of an existing database remains a separate report-first scan rather than a side effect of consolidation. Both exclusions are exported on `provider.MemoryStore`, and the honest form of the claim is that each has exactly one production caller and each of those filters upstream.

## Memory axes

A memory is described along four independent axes. The axes are orthogonal: a row can be live, in-date, contradicted, and low-confidence at the same time, and each of those facts is stored and judged separately. This section is the normative definition of the axes; the gaps listed against each one are tracked as issues and are the plan in [`ROADMAP.md`](ROADMAP.md#part-9--architecture-direction-memory-axes-and-context-assembly-p0p3).

| Axis | Question it answers | Storage today | Status |
|---|---|---|---|
| **Lifecycle** | Is this memory still current, what replaced it, and how long is it wanted? | `memories.resolved_at`, `memories.pinned`, `memories.retention` + `expires_at` (schema v19), the relation CHECK in `internal/memory/schema.go` (`duplicate`, `contradicts`, `supersedes`, `elaborates`, `causes`), `memory_snapshots`, `memory_history`, `audit_log` | Partial — retention/ownership tiers landed in v19 ([#587](https://github.com/wcatz/ghost/issues/587)): three tiers, a `persistent` tier every automated pass spares, a bounded decay for `session` rows, and a `ghost prune` that is dry-run by default and never runs on a timer (see [Retention tiers](#retention-tiers)). `memory_history` records every state change and who made it, and an `as_of` read consumes it for liveness and content (see [Historical retrieval](#historical-retrieval-as_of)), but a current retrieval still does not. No transition model across the axes is documented beyond the per-writer rules in this file and [Retention tiers](#retention-tiers) |
| **Validity** | Is this memory true *now*, and when was it last checked? | `memories.valid_from`, `valid_until`, `verified_at` | Shipped — the columns are read into `memory.Memory`, stage 2 of the assembler evaluates them against the request clock so a row with a closed window is withheld rather than ranked ([#581](https://github.com/wcatz/ghost/issues/581)), and the three writer tools accept them so a caller can state a claim's period ([#575](https://github.com/wcatz/ghost/issues/575)). Partial in one respect: `ghost_memories_list` and `ghost_search_all` still show a closed window, marked `expired` rather than withheld, because they browse rather than filter — every surface that ASSEMBLES a context withholds it, which is now `ghost_memory_search`, the session-start block, `ghost_project_context` and the `ghost://memories/global` resource (see the assembler section below for the list). A store whose rows all predate the writer contract still reads every row as unset, and the evaluation is corpus-neutral for it; `Store.RestoreSnapshot` and `Store.ImportMemory` carry the triple too, so an imported window is honoured the same way. A bare date is a whole day, and a `valid_until` is the END of it (see the assembler section). A `verified_at` also leaves a record in `memory_provenance` in the same transaction, so `EvidenceCounts.Verified` counts checks rather than reading a column that only ever holds the latest, and it counts over records that CARRY a stamp rather than over `verified` rows. Which mechanism is in play differs per writer — a live save appends a `verified` row, an import stamps its own `imported` row, a restore reinstates a re-created row's records and deliberately leaves an in-place row's alone, and the corpus route records no stamp at all — so [Evidence provenance](#evidence-provenance) enumerates the set in its writer table rather than this cell, which is a summary, and a summary of four per-writer mechanisms is a fourth claim site. (Named by link rather than by direction: this cell also points at the assembler section below, so a reader told to look below for both finds one and not the other.) Where a record IS written its stamp is the store's clock and never the caller's — a verifier that could date its own check could date it before the thing it checked, and the import holds to that too rather than copying a date in from a file — so the column and the record are two different facts and both are kept. Gated on the caller stating one in that call: a partial edit keeps an earlier `verified_at` by `COALESCE`, and re-recording that would report a check nobody repeated. An `as_of` read is filtered too, and against the instant it asked for rather than the wall clock: `assemble.Run` moves `Now` to the `as_of` value before the stages run, so stage 2 judges the window at T and a row that was true at T but has since expired is kept for that question ([#683](https://github.com/wcatz/ghost/pull/683)) |
| **Relationships** | What does this memory connect to, contradict, or replace? | `memory_links` (directed), near-duplicate links created by `Upsert`, scope-conflict exemption ([#563](https://github.com/wcatz/ghost/pull/563)) | Every writer refuses a scope-conflicting pair and every reader ignores one already stored ([#563](https://github.com/wcatz/ghost/pull/563), [#574](https://github.com/wcatz/ghost/issues/574)). Writers: `Upsert`'s two `duplicate` dedup probes (at save time), the linker's `related` edges, and `ghost supersede`'s `supersedes`/`causes` candidates. Readers: `DemotionPenalties` and `SupersedePenalties` (ranking), `ghost resolve`'s supersedes piggyback and the repair pass's matching floor (which would otherwise stamp `resolved_at` on the older endpoint), and the two fold-target liveness checks that decide whether a row may be folded into (which would otherwise turn every re-save of that row into a duplicate), and the assembler's conflict stage (which would otherwise mark both rows `conflicts_with` and note a pair that is two true claims) |
| **Confidence** | How much should a caller trust this, and why is it here? | `memories.confidence` plus write-time provenance columns `agent`, `session_id`, `source_ref`; `memory_history` records who performed each write; `memory_provenance` records every observation ([#673](https://github.com/wcatz/ghost/issues/673)) | Inert for ranking, visible to the reader. The three writer tools accept `confidence` and `source_ref`, `agent` comes from the existing provenance path and `session_id` from the host's session when it reports one ([#575](https://github.com/wcatz/ghost/issues/575)), and the shared item line renders all four. Nothing scores on any of them: stage 4's multiplier stays pinned at `1.0`, and the evidence counts are recorded in the assembler's trace and weigh nothing. The history has three readers: `ghost history <memory-id>`, `Store.MemoryHistory`, and the `as_of` read, which consults it for content and liveness but not for confidence. The evidence table has four: `Store.MemoryProvenance`, `Store.MemoryEvidenceCounts`, the portable artifact, and `Signals.Evidence` in the trace |

Axis interaction rules:
- **Supersede wins over time.** A memory that has a live replacement is demoted regardless of a later `verified_at` or higher confidence on the old row.
- **Resolved leaves injection, not the database.** `resolved_at` removes a row from ranked session injection ([#559](https://github.com/wcatz/ghost/issues/559)) but keeps it searchable and auditable.
- **Contradiction is symmetric, duplicate is directional.** A `contradicts` pair must never appear together in one assembled block; a `duplicate` edge points at the row that survives, and folding must not cross a scope conflict ([#574](https://github.com/wcatz/ghost/issues/574)). Not yet enforced: stage 5 of the assembler *records* a co-occurring `contradicts` pair and leaves both rows in place, because the existing contract requires a contradicted row to survive while a duplicate restatement sinks. Separating the pair needs its own contract change, which is the remaining work under [#581](https://github.com/wcatz/ghost/issues/581). Until then the pair is MARKED rather than separated: when both rows of a live `contradicts` edge are in the rendered answer, each line carries `conflicts_with=` followed by the other row's rendered id (`assemble.Token`), on the same physical line. Nothing is removed or reordered. **Marking does not satisfy "must never appear together"**: that stays open under [#581](https://github.com/wcatz/ghost/issues/581). A pair with one side withheld, cut by a budget, or whose edge was withdrawn says nothing, and neither does a pair whose scopes conflict (`memory.ScopesConflict`, the rule every other reader of a link applies: `environment=production` and `environment=development` are two true claims, not a contradiction), which is also not noted. A pair split across an edge-read chunk boundary (`edgeChunkIDs`) is never read, so it is not marked either; the `edges_partial` note already says so.
- **A scope conflict blocks the relation, and is not repaired by deleting it.** Two memories naming `environment=production` and `environment=development` are two claims about two places, so no relation may be proposed or created between them — not `duplicate` (at save time), not `related` (at link time), not `supersedes` (at supersede time), because each of those writers is the only one that can see both scopes at the moment it decides. An edge that already exists stays in the graph and is exempt at read time instead: every reader ignores it, and none deletes a row to reach that verdict. The exemption is stated where the decision is made, and every writer that chooses between candidates states it *inside* the query, because the same `LIMIT` chooses them: a conflict decided after the cut spends the budget on rows the caller may not use and misses a compatible candidate ranked just below ([#665](https://github.com/wcatz/ghost/issues/665)). The two cosine writers narrow before their window (`SearchVectorScoped`, in Go over the scan rather than inside a statement, so its top-k is the limit and there is no statement to fold the rule into), so a neighbour budget counts only rows a memory may relate to. `Upsert`'s two dedup probes and `foldTargetStillLive` each carry a second, SQL statement of the rule (`scopesConflictSQL`, held to the Go one by a test that runs both over the same table), for two different reasons: the probes because their own `LIMIT 15` chooses the candidates — so a save whose fifteen best FTS matches all name another environment still finds the compatible duplicate at rank 16 — and `foldTargetStillLive` because it has no window to protect, names one row by id, and carries the rule to keep a scope-conflicting `supersedes` edge from being read as a verdict on it.
- **Scope and project membership are not axes.** They are access predicates applied before scoring ([#577](https://github.com/wcatz/ghost/issues/577)); a memory that fails them is out of scope regardless of its other axes. The rule is decided where the rows are read, in whichever form that read allows. Search applies it in Go over the widened pool `Store.Candidates` returns — both legs have already run by then, and fusion narrows the fused pool, so a SQL form there would narrow nothing it still had to decide about — and the assembler keeps it in Go over the same set. A reader whose candidate set *is* a statement's own `LIMIT` has no rows left to decide over once the cut is taken, and no corpus scan to decide them with, so it carries a SQL statement instead: `memory.ScopeMatchesSQL`, bound by the store's passive fetch. A test runs that form and the Go one over the same rows, and a second test pins the single divergence between them — a stored scope value that is not a string. The cosine writers in the bullet above are that difference made concrete: a brute-force pass over the corpus can decide before its own top-k, in Go, and backfill to fill it.
- **Who wrote it is not how much to trust it.** `agent`/`session_id`/`source_ref` describe who wrote a row, and `memory_history` ([#578](https://github.com/wcatz/ghost/issues/578)) records who performed each write since — but none of them is a ranking input, and a confidence value is not a verdict anything computes. `memory_provenance` ([#673](https://github.com/wcatz/ghost/issues/673)) now keeps every observation rather than the last, and the assembler reports the count; it still does not rank on it. (Write-time authorship, the change log and the evidence records are three different things; see [Memory history](#memory-history) and [Evidence provenance](#evidence-provenance).)

## Context assembly (target design)

> **Partly built.** The seam exists (`internal/assemble`, `assemble.Run`, and
> `Store.Candidates` behind it), the formatted `ghost_memory_search` path runs on
> it with both filters applied before the window closes, a derived abstention
> outcome and a response-fit byte cap, and the session-start
> injector now runs on it too — a PASSIVE request through `loadSessionPassive`
> (`internal/mcpinit/session_passive.go`), so the selection, the caps, the order
> and the near-duplicate pass are the assembler's rather than a second
> implementation of them, while the per-item PREVIEW budget stayed with the
> renderer that has to say a line was cut. The header's totals are the
> assembler's too rather than a second census of the store: the shown, ranked-out
> and withheld counts come from the same retrieval's trace, and "of M total" is
> the trace plus ONE count (`Store.PassiveEligibleCount`) over the window's own
> predicates (`passiveWhere`, shared with the fetch), so M is every eligible row
> the store holds and not the over-fetched window's size. A row a stage withheld
> is inside the window and so is not counted twice; eligible rows the over-fetch
> never read are ranked out, because the window is ordered by the ranking and cut
> at its limit (`BucketTally.CountedAgainst` takes the over-fetch limit for exactly
> that split). A row the retriever fetched and then removed as a near-duplicate
> loser never enters `CandidateSet.Rows`, but the retriever reports it in
> `CandidateSet.DroppedLosers` with the ids it lost to, and stage 6 files it as a
> `dedup` / `near_duplicate` decision (`Decision.Against` names the winner). That
> decision is the one source: `CountsFor` counts it as `Deduped` (counted in the
> total, reported with the withheld rows, never as the ranking's cut),
> `BucketTally.CountedAgainst` does not infer losers from the store's count, the
> retrieval record carries it as a dropped verdict, and explain reports it as not
> included with `near_duplicate_of` set (and `near_duplicate_penalty` 0, because it was
> never ranked with a penalty). Today that explain row is EMPTY IN PRACTICE: explain
> requires a query, query mode carries no passive policies, and so `DroppedLosers` is
> never filled on a path explain can reach; the projection is pinned by a hand-built
> test for a future passive explain surface. The fetch's own SQL validity predicate removes closed-window rows before the LIMIT, so `PassiveEligibleCount` returns them too (one statement, the shared `passivePopulationSQL`) and they are withheld, counted once, never beyond the window. The sentence after the counts names which half of the difference
> is the ranking's and which a stage withheld; when every row was withheld, the
> block prints `assemble.EmptyNote`, the same sentence `ghost_project_context`
> prints for that state. It therefore renders and applies
> `memories.scope` from the shared label and the shared rule, and renders every
> line through `assemble.Item.Line` (`internal/mcpinit/hook.go`), so the
> contradiction marker reaches it too. `explain: true` is a projection of the same
> `assemble.Run` as the formatted answer. What does not exist yet: stage 5 does not
> SEPARATE a `contradicts` pair — it records the pair and the renderer marks both
> lines, which is not the same as keeping them apart — and stage 7 (diversity) is a
> pass-through. The plan to converge the surfaces, and the remaining work above, is
> [#581](https://github.com/wcatz/ghost/issues/581), staged in
> [`2026-09-25-context-assembler-design.md`](superpowers/specs/2026-09-25-context-assembler-design.md).

What exists now:

- **`internal/assemble`** owns selection, validity, predicates, provenance, the
  stage list, budget closure, the outcome and the shared item renderer. It
  depends on one method, `Candidates(context.Context, memory.CandidateRequest)`,
  never on `*sql.DB`. `internal/memory` never imports it; the retriever DTOs
  live in `internal/memory` because a method on `*memory.Store` cannot name a
  type from a package that imports it.
- **`Store.Candidates`** returns a *widened, untrimmed* set: the window selected
  exactly as production search selects it (fusion, status demotion, the keyword
  reservation, hydration with its deleted-row backfill, decay over the window,
  then the supersede and near-duplicate demotions), followed by the rows that
  window cut, in the same decay order. With no predicate, closing the set to the
  window reproduces the production search row for row; with a predicate, rows
  beyond the window are reachable. Re-deriving the window's decisions from the
  wider pool would drop the reservation and widen the demotions, so it is not
  done.
- **One snapshot per retrieval.** Legs, hydration, the edge load and the penalty
  lookups run in one read transaction on an injected read-only handle
  (`memory.OpenReadDB` + `memory.NewStoreWithRead`), whose DSN has no
  `_txlock`, so the transaction is a plain deferred read and does not take the
  write lock the primary handle's `BEGIN IMMEDIATE` would. A store with no read
  handle falls back to the primary connection and logs that cost once.
  `OpenReadDB` refuses `:memory:`; in-memory and bench stores run the snapshot on
  the handle they already hold, which has no concurrent writer.
- **Stage 3 is where category and scope verdicts live**, and both run over the
  widened set. That closes the
  mechanism behind the old scope post-filter: a post-filter over a closed window can only remove from the
  answer, so a matching row the window cut was invisible and the tool reported
  absence while the memory existed. Project membership stays in SQL and is
  recorded as a per-row verdict, never applied as a second drop.
- **The three validity columns are readable and writable.** `Memory` carries
  `valid_from`, `valid_until` and `verified_at` as `*string`, bound on every
  retrieval scan, and stage 2 interprets them against the request clock
  (`valid`, `future`, `expired`, `unverified`, `unset`; an unreadable value is
  reported as `validity_unparseable` rather than read as valid). The three writer
  tools accept them, so a claim's period is expressible at last. `Create`,
  `ImportMemory` and `RestoreSnapshot` carry the triple too, and every row
  written before any of those reads nil, which is what stage 2 calls unset —
  so a store nobody has written a window into is corpus-neutral for the stage.

  **The surfaces that run the pipeline filter on it**, and there are four:
  `ghost_memory_search`, the session-start block,
  `ghost_project_context` (with the `ghost://project/{id}/context` resource and
  the `recall_project` prompt, which share its read), and the
  `ghost://memories/global` resource. `ghost_memories_list` and
  `ghost_search_all` still return a closed window, and the shared renderer marks
  it `expired` rather than printing a retired claim unmarked — honest, but not a
  filter. Those two are the whole remainder: every other agent-facing read
  assembles, so the split is now between surfaces that browse and surfaces that
  build context, rather than between one that filters and a set that does not.

  A surface that filters has to be able to say what it filtered, or it reports an
  empty project as an empty one. `ghost_project_context` is the case that forced
  the rule: its empty branch was a **census** — "nothing has been saved for it" —
  which was true of a loader that only ever lost rows to its own cap, and became
  a lie the moment stage 2 could empty the section. The census is now gated on
  the verdict, and only `no_memories` — the over-fetched window came back empty,
  which is the sole absence a `LIMIT n*2` read can honestly claim — keeps it.
  Rows found and withheld render the assembler's own abstention sentence and name
  `ghost_memories_list`, where the retired rows are still visible with their
  marker.

  And the check is on the PROJECT's own rows rather than on the block, which the
  same case forces: `projectContextBudget` sets `IncludeGlobal`, so the block is
  populated by `_global` rows whenever the store holds any — and
  `cmd/ghost/bootstrap.go` seeds the global memories on every real store. Gating on
  emptiness therefore reported the one case that was never at risk and missed the
  one that was, answering a project whose every memory has retired with the
  cross-project preferences under a `## Memories` heading. `projectContextOwnRowsNote`
  is the census moved off that gate: no admitted row is the requested project's, so
  it says how many the project holds, that none is above, and where to browse them.

  A project-keyed section outside the assembler is the third shape, and it is the
  one that keeps the block non-empty when the memory read admits nothing at all:
  `ghost reflect` writes learned context into `ghost_state` and writes no memory row,
  so a mature project can hold a full block and an empty read. (`## Recent Decisions`
  is the same shape only after its COMPANION memory is gone: `RecordDecision` inserts a
  `decision_log` row in the same transaction and the tool reports it, so a decision
  normally arrives with a live row of the project's own and the section is not what
  emptied the read.) A verdict-gated note is then the only sentence that says the
  memories behind the summary have been withheld, and without it a caller is handed a
  conclusion derived from retired rows and told nothing about their retirement. So the
  note's own gate is the ADMITTED ROWS rather than the rendered text, and the two
  empty-block callers ask the function that owns the choice rather than spelling the
  gate themselves.

  The deferral is NOT on the verdict alone, because the verdict describes the WINDOW
  and the sentence is read as a claim about the project — the same union that created
  the mixed-bucket problem. A project holding no memory row at all, on a store whose
  cross-project rows have all aged out, gets `all_invalid` from stage 2 and no
  admitted item, so a verdict-only deferral would tell that project Ghost found and
  retired rows of its own and point at a `ghost_memories_list` that returns nothing
  for it.

  Which makes the project-scoped COUNT the input to every sentence about a project
  rather than a precondition of one branch, and the three gates that render one — the
  two empty-block branches and the appended note — all go through the single function
  that owns the choice. The union bug was on the two empty-block branches first, and
  this change fixed them in the wrong order twice: first only the third gate, then a
  shared predicate used as a *permission* — read, satisfied, and then discarded, so a
  project whose rows were all withdrawn by `ghost resolve` still got the never-saved
  census. A gate a caller can pass without obeying is the failure mode the single
  function exists to prevent, so the count is read where the sentence is chosen.

  That last shape is worth stating because the two readers disagree about what a
  project holds, and the disagreement is a fact about the SQL rather than about this
  surface: the passive window binds `resolved_at IS NULL` and `CountMemories` does
  not. So a project whose every row `ghost resolve` has withdrawn has an empty window
  and a count of one, no stage withheld anything, and the abstention is false.

  Which is why there are TWO counts and not one, because the two sentences are claims
  about different populations. The COUNT sentence — "Ghost holds N memories for this
  project and none of them is in the block above" — is about what the project holds,
  names no cause, and `CountMemories` answers it. The ABSTENTION names a cause, so it
  is a claim about which rows that cause explains, and the population it can be about
  is the WINDOW's: the project's own rows `passiveFetchSQL` could have admitted.
  `projectWindowRowCount` asks exactly that — `CountActiveMemories`, a
  `windowCountCapableStore` assertion, whose predicate is the window's own — and
  where it is zero, or the store cannot answer, the cause belongs to somebody else and
  the count sentence is the honest one.

  Both directions of that error are real and only one was found at a time. With no
  window count, a project holding nothing was told its rows had been RETIRED on the
  strength of aged-out `_global` rows; with a count but no window count, a project
  whose rows were all WITHDRAWN was told the same thing, and the abstention named a
  cause for rows that were not in the window at all. The sentence is only true of the
  population it can name, so the note is gated on a count of that population.

  A count that ERRORS is not a sentence at all: an unreadable count is evidence the
  project holds rows neither way, and the cheap direction is a missing note rather than
  a claim about memories that do not exist.
  The count is `CountMemories`, which covers rows left out for ANY reason — validity,
  the cap, deduplication, resolution — so the sentence names no cause and stays true
  in all of them. The population split has to live at the caller, because with one
  bucket holding two populations nothing above it can tell them apart: the live
  global is an admitted item, so the outcome is `answerable` and the reason is empty
  even when every row of the project was withheld.

  The sentence that renders must also not promise a rendering the assembler does not
  control. `Result.Abstention` is bytes a **caller** renders, and the callers do not
  agree on what follows it: the search surface writes `assemblerNotes(res.Notes)`
  after it, so "the note below breaks the removals down per stage" is there true,
  while `ghost_project_context`, the project-context resource and the `recall_project`
  prompt return the abstention as the **entire** answer and render no notes. The
  promise was dropped from the passive half for that reason — not from the search
  half, where the renderer keeps it — and what carries the reason without the notes
  is the clause inside the sentence: *withheld as out of date, their validity windows
  having closed or not yet opened*.

  The same rule reaches one step earlier, because a surface that filters can also
  be handed **no project to filter over**. `ghost_project_context` resolves the
  caller's name first, and an unknown name resolves to `""`; the old loader was
  handed that and read `project_id = '' OR project_id = '_global'`, so it rendered
  the global rows under a `## Memories` heading for a project that does not exist,
  and reached its "not registered" sentence only when the store happened to hold no
  globals. The assembler refuses a project context with no project, so the three
  project-context surfaces skip the project-keyed reads, render the
  `## Global (applies to all projects)` section — which does not depend on a
  project, and which the base ref did deliver, under the wrong heading — and
  **append** the not-registered sentence. Appending rather than returning it in
  place is the load-bearing half: a sentence alone drops the cross-project
  preferences on a project's *first* session, which is exactly when they matter and
  exactly what the server's own SessionStart instructions tell the agent to look
  for. The `as_of` branch is the one that **refuses** rather than appends, and the
  asymmetry is the contract: a caller who asked for an instant must not be handed
  the present, and there is no set to show — a past reading of a project Ghost has
  never seen is not a reading of anything. The answer names the instant, so the
  reader can see the read never happened.
- **A stamp can be replaced but not removed.** The write path stores NULL for an
  absent value and treats an empty string as the same request — "no claim" — so
  a claim recorded by mistake is corrected by writing a different one rather than
  by retracting it. The alternative, an explicit clear, would give the column a
  third state ("stated, then withdrawn") that retrieval would have to interpret,
  and nothing consumes one today.
- **A bare date is a whole day, and which end of it depends on the field.** A
  `valid_from` is the start of the named day, at midnight; a `valid_until` is the
  end of it, at 23:59:59. The asymmetry is the point rather than an artefact:
  `assemble.ExpiredAt` is a strict "before now", so a `valid_until` stored at
  midnight would retire the claim from the first instant of the day the caller said
  it was true through, and `assemble.stampText` prints a date back for the
  boundary a date stands for — midnight on a start and a verification, 23:59:59 on
  an end — so the rendered label and the filter cannot disagree about which day a
  claim covers. Two equal dates are therefore a one-day window; two equal instants
  are refused. A stamp the writer did not produce — one an artifact or a restored
  snapshot left — is printed at the instant it is stored.

- **The session-start surface shows and applies scope.** Its rows come from one
  `assemble.Run` — a PASSIVE request, a no-query retrieval with its own bucket
  policies, never `weak` ([#758](https://github.com/wcatz/ghost/pull/758)). The
  verdict of that run (`answerable`/`not_applicable`, or `empty` with a reason such
  as `no_memories`, a claim about the over-fetched window and not about the store)
  is NOT rendered on this surface: it reaches the trace and the retrieval record,
  and the block prints only the header's counts and, when every project row was
  withheld, `assemble.EmptyNote`. The block prints each row's scope with `assemble.ScopeLabel`, the same label
  a search line carries, so the two cannot spell one scope differently. It applies
  `injection.session_scope` when the key is set, and the filter is
  `memory.ScopeMatchesSQL`, the SQL statement of the rule stage 3 applies in Go
  through `assemble.ScopeContradicts`, held to the Go form by a test that runs
  both over the same rows. It has to be decided in SQL for the reason
  `scopesConflictSQL` gives: a statement that chooses its own candidate set with a
  `LIMIT` cannot check scope after that cut, and a passive bucket's `OverFetch` IS
  its candidate set — 45 rows for the project bucket and 16 for `_global` — so a
  check applied afterwards would spend the budget on rows the session excluded and
  never reach an eligible one ranked below the cut. With the key unset the clause
  is absent, so the query and the ranking are the ones that shipped — the label is
  not part of that, and is deliberately new: a row that carries scope is labelled
  on its line whether or not a session scope is configured, so the label does not depend on the filter. The SELECTION is the
  one that shipped for every branch the golden fixture exercises, and not for
  `_global`'s supersede demotion, which the old global loader never ran and the
  assembler's passive demotion runs for every bucket: a `supersedes` edge between
  two `_global` rows now pushes the replaced row out of a bucket capped at 8. It
  is kept rather than reverted, on the bucket's own stated ground that a superseded
  preference is not worth one of eight cross-project slots; the alternative — a
  `SlicePolicy` field saying globals ignore a relationship the rest of the system
  honours — is a second answer to one question.
  The column is substituted rather than named on a store below the version that
  added it (`migrateV12`): the read runs on a handle that migrates nothing, so
  naming `memories.scope` on such a store would fail the query with "no such
  column", which a read-only caller reads as no rows — a digest with its header, its
  tasks and its decisions and no memories, and nothing saying why. A store below
  the floor selects a NULL literal instead, which is what every row in it carries by
  definition, so the block is the one that store produced before scope was read.
  What moved, and what did not, is the reason the two bucket policies are now ONE
  statement (`sessionPassiveBudget`): the block is a caller of `Run`, so the
  selection, the caps, the order and the near-duplicate pass are the assembler's
  rather than a private copy of them, while the per-item PREVIEW budget and the
  ellipsis that says a line was cut stayed with the renderer — `Slice.ClampBytes`
  cuts to a budget and stops, and a caller cannot tell a clamped row from one that
  happened to be exactly its budget, which is the one distinction the ellipsis
  needs. That seam SERVES two requests by refusing them
  rather than approximating them, and both refusals are in `validatePassiveBudget`.
  A slice's **bucket must be the requested project or `_global`**: the bucket is the
  project predicate (the store binds it as the `WHERE` clause and never consults
  `Mode`), so a mismatched bucket would read and inject a project the request never
  named, and stage 3 only records that as `Signals[id].ProjectMatch=false`, which
  nothing refuses. And a passive request cannot carry a **`Category` or `Retention`
  filter**: the passive fetch binds neither in SQL, and its window is the policies'
  own over-fetches rather than a widened one, so the filter would be applied after
  selection rather than in it — the caller would get `all_out_of_category` while the
  store held the rows it asked for just below the cut. Refusing is the honest
  answer to a filter the fetch cannot honour, and a caller that needs one can send
  a query, which is the shape that already widens for it.
- **The trace is recorded unconditionally**, with per-stage counts, dropped ids,
  per-row decisions and the exact floors that were evaluated. `explain: true`
  projects it, together with the per-candidate ranking facts the retriever records
  when asked (see "Explain is a record of the ranking").
- **Abstention is derived, and it is an outcome rather than an empty list.**
  Every block carries
  `answerable`, `weak` or `empty` with a reason from a closed vocabulary, and
  `ghost_memory_search` renders the verdict as a machine line
  (`[ghost:outcome=… reason=… floor_fts_rank=… abstain_cosine=… candidates=…
  admitted=… legs=… tokens_est=…]`, with a trailing `retrieval_partial`
  token whenever a leg ran and failed) plus a human sentence for the two
  non-answerable cases. A **passive** block — no query, so no leg, no FTS rank
  and no cosine to compare — is never `weak`: it is `answerable`/`not_applicable`,
  an empty over-fetched window is `no_memories` (a statement about the window, not
  a census of the store) unless the validity predicate emptied it, in which case it
  is `all_invalid` and carries the exclusion wording: passive validity runs in
  `passiveFetchSQL` before the `LIMIT` (so an expired row cannot spend the window),
  the store counts what it removed into `CandidateSet.ValidityExcluded`, and
  assemble stage 2 stays the authority and must agree. The passive demotions
  (supersede, then near-duplicate, which drops its losers on `_global`) run after
  validity as well: `selectPassive` sets aside any row outside its window before the
  selection and the demotions, so an expired row can neither demote nor remove a
  live one, and it trails the eligible rows for stage 2 to drop. And
  the machine line a passive result carries (`Result.Machine`, which no passive
  surface prints) reports `abstain_cosine=not_applied` because the surface has no
  vector arm for a reader to configure. `weak` withholds no row — only the response-fit pass may
  remove one — so a caller can see the weak candidates and judge them; what it
  gets is the instruction not to. Arm A (a keyword rank of 0-3) is on; **arm B
  (a vector cosine) ships OFF**, because the bench no-answer report shows the
  answerable and no-answer cosine distributions overlap, so no constant
  separates them, and `context.abstain_cosine` is a decision a user makes rather
  than one Ghost infers. The cosine is range-checked on BOTH sides of the seam —
  `config` on its environment and file paths, and `validateRequest` on the request
  itself — because a guard that lives only in the layer above the seam is one the
  next caller does not inherit, and all four unusable values fail silently rather
  than loudly: a negative and a NaN read as OFF, and an infinite or above-one one
  arms a threshold no cosine can clear. An unavailable embedder is never a reason to call a match
  weak: a result whose rows carry keyword ranks is judged by arm A whether or not the vector leg
  ran, and the leg's condition is reported on the line instead of in the reason. A leg that
  *failed* suppresses the floor outright (`retrieval_partial`), because a verdict needs every
  applicable leg's input. And a result with **no arm able to judge it** is `no_floor_arm`: no arm
  held a VALUE, which is a different question from which legs ran. A leg that answered can have
  retrieved nothing — a semantic query sharing no words with the corpus leaves every row at the -1
  "never retrieved" sentinel, as does an `as_of` read for the vector leg, since an embedding records
  current content only — and a threshold applied to a sentinel is a comparison nobody made. Reporting
  `below_floor` there would be a claim against a threshold nobody applied, which is the same error as
  blaming an embedder outage for a weak keyword hit, in the other direction. The line and the trace both keep "a threshold" and "a
threshold that ran" apart: it renders `off`, `not_applied` (configured, but the
  vector leg never ran or ran and failed) or the number, and
`Floors` carries `VectorArmOn` (what the request configured) beside
`VectorApplied` (the arm's own state: it was on AND the vector leg executed, so
a cosine could be compared at all), so a configured floor on a machine with no
embedder is visible as unused rather than looking like a floor that cleared
something. Neither field records a comparison against a particular row — an empty
result reads no cosine, and a result whose first row cleared the keyword arm
never reaches one — and what did execute is `Trace.Legs`, the map the line's
`legs=` field renders. (`VectorAvailable` is not it: that means the caller
supplied a query vector, which reads true for a machine whose embedder answered
and whose vector leg then failed.)
- **The empty result says which of three things happened.** An empty block whose
  rows were found and then withheld — out of date, out of scope, out of
  category, or cut by a budget — says "no sufficiently trustworthy memory found"
  and names the cause. A search that never finished says so. Only
  `no_candidates` may say nothing matched, and only over coverage every
  applicable leg vouched for: available, error-free, untruncated, and
  `LegStatus.CoverageComplete`. The vector leg reports that last one as **false**
  until its expected/indexed/unembedded counts are reconciled, so a hybrid search
  cannot claim absence today, and `Run` does not work around it — a row with no
  embedding is invisible to the leg's own scan, so nothing the leg read can speak
  for the rows it never saw. Where absence is not earned, the answer carries the
  window note instead, which opens "no match within the searched window" and
  closes "this is not evidence that nothing exists"; that note is the spec's
  mechanism for suppressing an absence claim. The reconciliation is still
  **owed**, and it lands with a change that owns the counts in `internal/memory`:
  the two `COUNT(*)` scans it costs run on every hybrid search, on the live tool
  path, so the abstention change reads `CoverageComplete` and obeys it rather than
  filling it.
- **`response_fit` is a `Run` post-pass, and it measures the whole response.**
  While the rendered envelope exceeds `Budget.MaxBytes` it drops the
  lowest-ranked row, recomputes the outcome, re-derives and re-bounds the notes,
  and re-renders; notes give way before the verdict line, and if the verdict
  itself cannot fit, `Run` returns `ErrResponseBudgetExceeded` rather than an
  outcome. A trim also suppresses the window caveat, which would otherwise blame
  the window for a shortfall the byte cap caused and advise raising a limit that
  only makes the response larger. The window note an incomplete-coverage empty
  result carries names each knob the request actually set and no other: the item
  limit when one was set, the scope filter when one was set, and the category
  filter when one was set. An unfiltered hybrid search is the shipped default,
  and it does set a limit, so the note every such caller gets advises widening
  that limit and says nothing about a scope filter it never passed. `Budget.MaxBytes` is the RESPONSE's bytes and `Slice.MaxBytes` is item
  content — one field cannot bound both units. `ghost_memory_search` caps a
  response at twice `memory.MaxContentLen` (16000 bytes), the same order as the
  session-start injector's CONTENT (15 project memories at 200 bytes plus 8 globals
  at 300 is about 5.4 KB of content; the block that wraps it in headings, per-row
  labels and a tasks and decisions section is larger, which is why the cap is a
  multiple rather than a match). The content cap is the binding half, not the
  "same order" argument: at or below 8000 a maximum-length memory plus the
  truncation marker, the item line's framing and the verdict line is already past
  the cap, so the pass would drop the only row and the answer would come back
  `empty`/`all_over_budget` naming a server-side cap no search argument reaches.
  Tokens are reported as an ESTIMATE (bytes/4, rounded up, `tokens_est=`) for
  callers that budget in tokens; bytes remain the unit and there is no tokenizer.

What the remaining stages will add, in pipeline order: conflict separation
(stage 5, where a `contradicts` pair is recorded and its two rendered lines are
marked, but neither row is removed or moved) and diversity (7, off by default and
a pass-through). Both are tracked under [#581](https://github.com/wcatz/ghost/issues/581). Stage 6 already records the near-duplicate losers the retriever
removed, with the row each lost to; it does not decide them.

Both consumers should call one assembler with an explicit budget, so every surface applies the same predicates in the same order and every stage is testable in isolation:

```text
query
  1. retrieve       hybrid FTS + vector candidates, widened window (0.3 FTS / 0.7 vector RRF)
  2. validity       drop or bound rows outside valid_from/valid_until, flag unverified
  3. scope          machine-readable memories.scope match, project membership
  4. provenance     bounded penalty for unattributed or low-confidence rows
  5. conflicts      records a contradicts pair (both rows stay; the renderer marks both lines);
                    separating the pair is not built. Supersede demotion is the retriever's.
  6. dedup          collapse duplicate/near-duplicate links to one representative
                    (the retriever's removed losers are recorded here, with their winner)
  7. diversity      cap per-source share so one project cannot crowd out the rest
                    (designed, not built: today a recorded pass-through)
  8. budget         final ordering, then the per-slice hard trim
  9. render         one renderer shared by search output and injected context
       → outcome    answerable | weak | empty, with a reason from a closed set
       → response_fit  drop the lowest-ranked row until the whole response fits
       → Trace      per-stage row counts and per-row exclusion reasons
```

Rules the pipeline must hold:

- **Filters precede window closure.** Stages 2-4 run over the widened candidate set from stage 1, never over an already-truncated list.
- **One renderer, one field set.** Scope, validity, confidence, the writing agent
  and the source reference are rendered by one implementation per field — scope
  already, through `assemble.ScopeLabel`; the rest through
  `assemble.ValidityLabel`, `ConfidenceLabel`, `AgentLabel` and `SourceRefLabel`.
  `ghost_memory_search`, the session-start block, `ghost_project_context` and the
  `ghost://memories/global` resource all reach them through `assemble.Item.Line`.
  The contradiction marker is the same kind of field: `Item.ConflictsWith`, set in ONE place (`pipeline.markConflicts`, run on every pass of the `response_fit` post-pass so a survivor never names a dropped partner) and rendered by `assemble.ConflictsLabel`, so every surface above inherits it.
  What still renders `memory.Memory` directly calls the same four:
  `ghost_memories_list` and `ghost_search_all`, plus the PRESENT-TENSE read's
  sibling — `ghost_project_context --as_of`, which `validateRequest` refuses to
  put through the assembler, so its rows reach `formatMemories` unchanged
  (`splitMemoriesByProject` is the predicate written against `memory.AsOfRow`
  because of it). The
  project-context block and the global listing were both already converged on the
  **labels** before they moved onto the assembler (`formatMemories` called the
  same four in the same order), so what those moves changed was the selection and
  the stages rather than the rendering — which is why their goldens are
  byte-identical while their one behavioural difference is not in them at all.
- **The trace is the explain payload.** `explain:true` is a projection of the same `Run` the formatted answer comes from, so explain and the answer cannot disagree about what a row was or why it was withheld.
- **Abstention is an outcome.** If no row clears the relevance floor, the assembler returns `weak` or `empty` with a reason rather than passing stale candidates through. An unmeasured threshold is never the default, and a leg that could not run is never evidence that a match is weak.
- **The budget is a hard boundary, in the unit it names.** Stage 8's slice caps are item content; the response-fit post-pass is the complete response. Both trim deterministically and both are tested at, just under, and just over the limit; injection and search use different budgets but the same code.
- **One renderer owns the response.** The assembler renders the search answer whole — listing, verdict sentence, filter caveat, diagnostics and the machine line — because a byte cap enforced against a second rendering is a cap on text the caller never receives.
- **The pipeline is measurable.** `ghost bench --context` measures context precision, contamination rate, budget adherence, diversity, and token cost, and `ghost bench --passive` measures the passive surfaces (see [Benchmarks](benchmarks.md)); contamination classification reuses the production exclusion reasons so the two cannot drift.

## Concurrency contract

**Multiple Ghost processes may open the same database concurrently, and SQLite is the synchronization layer.** This is a supported mode, not an accident: a CLI command, a live MCP server, a hook-spawned lifecycle subprocess, and a maintenance run routinely overlap.

Ghost does not run a single owning daemon that other commands route through. Each process opens its own handle and relies on the database for isolation.

The guarantees rest on these settings:

| Setting | Where | Why |
|---|---|---|
| `journal_mode(WAL)` | `memory.OpenDB` | Readers never block on a writer for their snapshot, so a hook read cannot be stalled by a reflection write. WAL is persisted in the database file, so it applies to every connection to that file. |
| `busy_timeout(5000)` | `memory.OpenDB`, `mcpinit.rwDSN` | A write arriving mid-contention retries for up to 5 seconds instead of failing on the first collision. Without it, concurrent writes return `SQLITE_BUSY` and the memory is silently lost. This is a bound, not a guarantee: a transaction held longer than 5 seconds still fails the writer with `SQLITE_BUSY`, and callers that treat extended contention as recoverable (`bumpSessionCount`, for one) must handle that error rather than assume the write landed. A save and a reflection apply get one further attempt through `Store.beginWrite`, which is a fresh budget rather than a larger one — see [Boundaries](#boundaries). |
| `busy_timeout(1000)` | `mcpinit.roDSN`, CLI read paths | Read-only connections are not exposed to write-lock contention under WAL, so a short timeout is enough to catch real problems without hanging a hook. |
| `busy_timeout` scoped to the caller's deadline | `memory.Store.beginScopedWrite` (retrieval record only) | The one write that runs on every retrieval path — `ghost_memory_search` and, since #850, the session-start block and the project-context surfaces — lowers `busy_timeout` on a PINNED connection for the duration of its own transaction, to a 150 ms CEILING, lowered further to `remaining − 40ms` when that is positive and under 150 ms — and restores the value it found before returning. Note what the floor means: when the caller's remaining deadline is at or under the 40 ms margin — or there is no deadline at all, which is the multi-process child's case — the 150 ms is kept, so the write can wait past a deadline it cannot see. That is deliberate and it is the direction that loses a row rather than a caller's time, but it does mean the timeout is a CEILING on this path rather than a bound the caller's context can always tighten. This exists because a context cannot bound SQLite's busy handler: that wait is a sleep loop inside the driver's C call, so the assembler's 250 ms `recordWriteBudget` — which `emit` applies to the record write alone, separately from the caller's own context, and which no retrieval surface sets itself — had no effect on it: 5 s inside `BEGIN IMMEDIATE` and then, through `beginWrite`'s one bounded retry, 10 s, against a search that had already been answered. Two properties make the scoping safe rather than a store-wide change: the pragma is per-connection, so it is set on the pinned connection the transaction actually uses rather than on whichever the pool hands out; and the store-wide five seconds is untouched for every other write, including this one outside the transaction. The value is READ BACK before it is changed and PUT BACK after, because a connection left at 150 ms would silently shorten every later write's budget for the life of the process. This path deliberately does not retry: two attempts at 150 ms is past the caller's budget, and a record is the one write whose loss is recoverable. |
| `SetMaxOpenConns(1)` | `memory.OpenDB` | Pins each handle to one connection so `PRAGMA data_version` polls compare against a stable baseline. `obsidian sync` uses that counter to detect commits from other processes; an unpinned pool would compare connection-local counters instead of points in database history. |
| `foreign_keys(ON)` | `memory.OpenDB` | Enforces the `memories.project_id` and `memory_links` foreign keys, so a write cannot insert a memory for a project that does not exist or leave a link pointing at a row that is gone. A connection without it accepts both, and the row-level damage is invisible until a later read cascades or a project listing disagrees with the memories attributed to it. |
| no `_txlock` | `memory.OpenReadDB` | The read-only handle exists so a snapshot read can be a plain deferred `BEGIN`. On the primary handle every `BeginTx` is `BEGIN IMMEDIATE`, so a read transaction there would hold the write lock for its whole lifetime — long enough to block every concurrent writer in the machine. |
| `_txlock=immediate` | `memory.OpenDB` | `BeginTx` issues `BEGIN IMMEDIATE`, taking the write lock at transaction start. A deferred transaction that reads first and writes later holds a WAL read snapshot, and the read-to-write upgrade fails with `SQLITE_BUSY_SNAPSHOT` if another process committed in between — an error `busy_timeout` does not retry. Without this, a read-then-write transaction such as `UpdateMemory` fails outright under concurrent handles instead of waiting. |

A read-only connection deliberately sets no `journal_mode`: setting it writes the database header, which a read-only connection cannot do.

### Boundaries

- **Writers are serialized by SQLite, not by Ghost.** There is no application-level writer lock for ordinary memory operations. The per-project lifecycle PID file (`AcquireLifecycleLock`) prevents two *maintenance runs* from overlapping; it does not govern memory reads or writes.
- **A read that decides a write belongs inside the write transaction.** `Upsert` is the worked example: it probes for a duplicate, then strengthens the row it found or inserts a new one. Probed outside the transaction, the gap between probe and write is a window any other handle can use — a save that strengthened the same row in that window had its increment overwritten by arithmetic on a value read before the fact, and a row committed in that window was invisible to the probe, so two saves of one fact became two unlinked rows. Both are cross-process races that a per-`Store` mutex cannot close. The transaction is therefore opened before the probe and `_txlock=immediate` holds the write lock across it, and the strengthen is `SET importance = MIN(1.0, importance + ?)` so the value it adds to is the one the row holds when the statement runs. A read-modify-write outside a transaction is the same bug with a different statement.
- **A wait is not a hold, and only one of them is worth lengthening.** Holding the write lock longer is a decision about the transaction; failing to get it is a decision about the machine. A writer that ran out of `busy_timeout` did not meet a long holder — SQLite's busy handler is unfair, keeps no queue, and re-polls on a schedule that grows to 100 ms, so a waiter asleep at the top of that schedule loses every hand-off to a writer that is awake. Measured on a saturated one-core runner (#671): a save's transaction held the write lock for under 10 ms at the median and 200 ms at worst against a five-second budget, while writers that lost the schedule spent 3.6-5.4 s waiting with other writers committing in the gaps. So the answer to "a save was lost to SQLITE_BUSY" is a second attempt, not a longer first one: `Store.beginWrite` retries `BEGIN` exactly once, which restarts the busy handler at its shortest polls (1, 2, 5 ms) and buys a second budget besides. It is bounded to one attempt and to lock refusal alone — a second attempt at a closed handle or a spent context fails identically and only delays the report — and a save that cannot get in after two budgets is on a machine that is not running Ghost.
- **The line is drawn at a memory nothing else would recreate.** The retry reaches a save, an edit, a decision record (whose companion row is an ordinary memory, and `ghost_decision_record` is a live tool) and a reflection apply — the four writes whose loss loses knowledge. The other `BeginTx` sites are excluded on a stated ground rather than a tidy one: they write projects, links, the history tables and portable imports, and the resolve/supersede marks and deletes they also cover all REPORT their own failure to the caller, so a lock refusal there is an operation that did not happen and is visible as one. A lost memory is the opposite — nothing reports it, and the agent that wrote it carries on as if it were stored. Seeding and snapshot restore are excluded for the ordinary reason: both are idempotent and run again.
- **The probes stay inside the transaction, and the batch stays one transaction.** Both are "shorten the hold" ideas, and the measurement says neither is where the time goes. The dedup probes cost single-digit milliseconds on a 1200-row corpus on both sides of the #669 change, against a five-second budget; splitting the reflection batch would buy a shorter hold by giving up the property the samplers in `TestMultiProcessSharedDatabase` assert — that a reader sees the whole batch or none of it — and the batch is measured at 15-2000 ms, so it is not the writer that ran out of budget in the first place. What actually cost that save was queueing behind other writers' short transactions, which is the previous bullet.
- **The write lock is measured where a process can see it.** Contention is between processes, so `internal/memory/writelock.go`'s observer is a seam rather than a `_test.go` file: `SetWriteLockObserver` is nil in production, and the multi-process test installs one in each child so the fleet can report, per write path, the distribution of the wait (`BeginTx` in) and of the hold (`BeginTx` to `Commit`). A transaction that never took the lock is reported as a *lost* write with its whole wait and no hold, because a distribution over committed transactions alone describes only the contention that was survivable. The save path's retries are counted, not asserted on: a fleet leaning on the second attempt is a number to read, not a failure.
- **A handle is one connection.** Process-level concurrency is the number of open handles, not the number of goroutines. Goroutines within one process contend with each other for that single connection.
- **Holding a pinned connection blocks the pool.** `Store.beginScopedWrite` is the first production caller of `db.Conn(ctx)` on a *`Store`* — `migrate` has pinned one since before this rule existed, and holds it across `PRAGMA foreign_keys=OFF`, every step's `conn.BeginTx` and the `foreign_key_check`, so the rule already had a production case. What `beginScopedWrite` adds is a pin held inside a *live* store's retrieval path, and it obeys the rule the same way: it holds the pin for the whole transaction and issues only `conn.*` calls, because a `s.db.*` call made while the pin is held would wait for the one connection the pinning code can release. Code that pins `db.Conn(ctx)` must not then issue a `db.*` call on the same handle: with `MaxOpenConns(1)` that call waits for a connection only the pinning code can release, and blocks forever.
- **This contract is solo mode.** It bounds one machine and one database file. A networked multi-writer backend is a separate deployment mode, not a relaxation of these settings; see `ROADMAP.md`.

### Tests

`TestConcurrentProcessesMixedReadWrite` opens several handles against one file and runs two phases against each other: concurrent inserts with FTS readers, then concurrent content rewrites with FTS readers. It asserts no `SQLITE_BUSY` failures, no dropped writes, and no row returned by a search whose stored content does not contain the searched terms. The rewrite phase exists because `memories_au`, the trigger keeping the index in step with content, fires only `WHEN old.content != new.content` — inserts alone never exercise it. `TestOpenDBPinsPoolToSingleConnection` pins the pool setting directly. Both are contract guards: they pass while the contract holds and fail if a setting that provides it is removed.

Every handle in that test comes from the same binary and the same DSN builder, so it cannot see a setting that only one open path carries. `TestMultiProcessSharedDatabase` closes that gap with real processes: it builds a helper program (`internal/memory/testdata/multiproc`) once and runs the entry points the contract names against one database file — two MCP servers driving the real `ghost_memory_save` and `ghost_memory_search` over the real stdio transport, two CLI children doing `Upsert` and `UpdateMemory`, two read-only handles searching the rows those children are rewriting, and one lifecycle writer applying a single `ApplyReflection` batch plus a `maintenance_runs` row. Ten processes, ten handles, one file.

What it asserts, beyond "nothing crashed":

- every process exits successfully and no `SQLITE_BUSY` or `SQLITE_BUSY_SNAPSHOT` reaches any of them. The test itself retries nothing, and the store's save path absorbs exactly one `BEGIN` refusal by design (see [Boundaries](#boundaries)), so a pass is evidence that the documented budget plus that one attempt carried the contention — not that a backoff hid it. The run reports how many transactions needed the second attempt, so a fleet that is living on it is visible in the log rather than silently tolerated, and anything that survives it still fails here through its own process, in `Errs` and again in `Busy`. What keeps that from becoming a measurement of how long a transaction may be held is the trigger described below, not a cap on the writers: the batch comes as soon as every writer has made its share of writes, so the corpus is still small when it arrives, and a run in which something *else* decided the timing would be the one that grew it.
- the write-lock distributions, per write path, reported rather than gated. Every child measures its own store's write transactions and the parent prints p50/p99/max of the hold and of the wait, each hold beside the budget it would be read against (a fraction of the busy timeout the contract's writers actually got, read back from a live connection). Hold and wait are the two halves of #671 and they mean opposite things: the hold is how long the run made every other writer wait, the wait is how long the run was made to wait, and the failure was a large wait beside a small hold. These are REPORTED and not asserted on, because a hold is wall-clock time for a transaction that does real work and scales with how loaded the machine is — the same fleet measured a save's median hold at 1.3 ms idle and 82 ms with three copies of itself on one core, and the 60-row batch at 21-600 ms against 0.5-2.0 s. A gate over those numbers would make this test the one place whose verdict depends on runner speed, which is the dependence #671 asked to remove from it. The ceiling that actually guards a long write transaction is `TestWriteLockHoldLeavesRoomInsideTheBusyTimeout` in `internal/memory`, on a quiet machine, where 250 ms sits above the worst hold this fleet produced under deliberate three-way oversubscription (~200 ms) and 180× above a measured idle hold — so the failure names a real regression rather than a runner, without being loose enough to catch only a catastrophe. A write path the budget table does not name still fails the run, so a new one cannot be measured and have nothing said about it.
- the load was still writing when the batch committed. Two barriers say so. The batch waits for every writer to report that it has completed its share of writes, so it is triggered by writer progress rather than by a clock; then it announces itself and waits for every writer to report a write issued *after* that announcement, which it cannot do until each has come back around its loop. The batch therefore cannot commit until every writer has written across the announcement. The measurement has to be a file: the batch holds SQLite's write lock for its whole transaction, so a writer cannot commit *during* it, a row count taken either side would measure the two gaps around the lock, and a writer testing a flag on both sides of its own write cannot tell a write that straddled the instant from one that ran entirely after it. Without this the run could pass with the batch landing on an idle file, which is the vacuous case the guard exists to prevent.
- every row id a process reported creating is present afterwards, read back through a fresh handle. A writer that gave up under contention is a lost memory.
- a read transaction opened before the batch commits still describes the pre-batch state afterwards, and a new snapshot then sees the whole batch. The transaction is opened the way `Candidates` opens its read snapshot — `BeginTx` with `ReadOnly`, which issues a plain `BEGIN` even under `_txlock=immediate` and so pins a WAL read snapshot without taking the write lock.
- a second reader, taking a fresh snapshot on every sample straight across the commit, observes the pre-batch state and the post-batch state and nothing between them. A pinned snapshot cannot show this — it shows the pre-batch state whatever the writer did — which is why it takes two readers. It announces its first sample as a barrier, so the batch cannot commit before a pre-commit reading exists, and its tail is counted from that signal rather than from its first sample, because a read that begins while the writer is still finishing its commit can legitimately describe the pre-batch snapshot.
- no reader sees a row whose content does not contain a term its FTS match claimed, in the CLI and read-only readers, which hold `[]memory.Memory` and check the terms. The MCP readers are not part of this: `ghost_memory_search` returns formatted text, so a row rewritten between its FTS match and the read that hydrates the result is indistinguishable from a stale index entry, and a search is two queries. Their reads establish that the tools answer under contention.
- `journal_mode`, `foreign_keys`, `busy_timeout`, `SetMaxOpenConns(1)` and a read-only handle that refuses writes are read back from a live connection rather than from the DSN that produced it. The fleet exercises two of the tree's DSN builders: `memory.OpenDB` (the six read-write roles) and `memory.readOnlyDSN` (the four read-only ones), with `busy_timeout` 5000 and 1000 respectively. `foreign_keys` is asserted on the read-write shape only, because that is the shape whose DSN sets it; the contract table above now says so too. It opens neither `mcpinit.rwDSN` nor `mcpinit.roDSN`, nor `cmd/ghost`'s own read-only spelling, so a regression in one of those would still pass this test. `_txlock=immediate` cannot be read back at all — it is a driver parameter — so it is asserted behaviourally, by showing that a write from a second connection is refused while a read-then-write transaction is open on the first, which is what a deferred `BEGIN` would not do.
- `user_version` is unchanged and `PRAGMA integrity_check` is `ok`. All six read-write processes ran `OpenDB`'s open path concurrently, which is `initSQL`'s `CREATE ... IF NOT EXISTS` pass plus its `CREATE UNIQUE INDEX IF NOT EXISTS`; `migrate()` itself is not entered, because the parent creates and stamps the database at the current schema and `migrate` only runs when the file is behind. The four read-only processes opened through `OpenDBReadOnly`, which runs no DDL at all.

Ordering between the processes is carried by barrier files, not sleeps: each announces the state it has reached and waits for the state it depends on — a writer announces the writes it has completed, a sampler announces its first reading, the maintenance process announces its commit — so a slow machine makes the run slower rather than wrong, and the loop's jittered pause between a writer's iterations gives a sleeping waiter a turn instead of letting an unpaced writer take every gap in the lock. The test skips under `-short` (it builds a binary and starts ten processes) and makes no LLM call, so it needs no live-test gate. What it does not cover: a schema-changing migration committing against live readers. `migrate()` is not entered at all here — see the `user_version` bullet — so a real ALTER TABLE racing the load and the readers is untested. What the runner does change is the shape of the reported distributions, not which assertion fires: no assertion here compares a wall-clock duration, so a slow machine makes this test slower and its log longer rather than its verdict different.

## Backup and portable transfer

Two independent mechanisms move the store out of a machine, and they answer different questions.

**A backup is a restorable copy of the database.** `memory.Store.Backup` (`internal/memory/backup.go`) writes one with SQLite's `VACUUM INTO`, which reads a single snapshot of the source and builds the destination from it. That is what makes it correct against a live WAL database, where copying files cannot be: a writer committing during the copy is reflected in the result whole or not at all, where a `cp` of `ghost.db` alone loses whatever the `-wal` held and a `cp` of the pair can capture a torn page. It is also why no extra transaction wraps it — the write lock is held for the length of the vacuum and nothing longer, so a running MCP server keeps serving.

`memory.vacuumInto` is the single implementation of "put a restorable copy of this database over there", and both callers use it: the pre-migration copy `OpenDB` takes before any migration step, and `ghost backup`. One implementation means the refusal to replace an existing file, the mode the copy is created at, and the `TightenPermissions` pass cannot drift apart between the two — the pre-migration copy is the same width as the database it came from, and so is a `--out` destination outside the data directory, which has no 0700 parent to shield it. `Store.Backup` splits the two halves — `vacuumAndCount` under the store's write lock, the manifest write after it — because hashing the snapshot re-reads every byte of the database, and a second full pass inside that EXCLUSIVE lock would stall every reader a live server has for a read of a file that belongs to nobody but this call.

The copy is created at 0600 *before* the vacuum rather than chmod'ed after it, and the path is claimed with `O_EXCL`. `VACUUM INTO` names no mode for the file it creates, so a copy it creates lands at SQLite's default minus the umask (measured 0644 under umask 000) and a create-then-chmod leaves a window in which a full copy of the memory database is group- and world-readable. `O_EXCL` is also the atomic claim, so there is no gap between the `Lstat` that classifies the destination and the create that takes it, and a dangling symlink at that path is refused rather than written through. SQLite accepts an existing *empty* file as a `VACUUM INTO` destination and keeps its mode, and refuses a non-empty one — so the emptiness `reserveBackupPath` guarantees is the emptiness SQLite checks. The `TightenPermissions` call after the vacuum is a second line rather than the only one: the open mode is advisory, and setgid directories, ACLs and some network mounts can leave a file wider than the mode asked for.

`BackupResult.Counts` is counted by opening the *written* file read-only, never the live one. The counts a restore is checked against have to describe the file that was written: a writer that committed after the vacuum would otherwise make the printed numbers and the snapshot's contents disagree, and the reader has no way to tell which is right.

**A manifest is what makes a backup checkable.** `writeBackupManifest` (`internal/memory/manifest.go`) records beside the snapshot what a restore is at risk of: the schema version the copy was written at, the same five `BackupCounts` the report printed, the file's size, and its SHA-256. It is called from `Store.Backup` rather than from the CLI so that there is one place a backup is described, in the same way there is one place one is taken. Its own `manifest_version` is the shape of the *document* and moves only when a field changes meaning — it is not `schemaVersion` and does not move with it, for the same reason `portable.SchemaVersion` is not: a manifest outlives the binary that wrote it, so its meaning is its fields, not the tables behind them. A manifest stamped with a version this build does not read is refused rather than guessed at.

The hash is the part nothing else can substitute. `integrity_check` reads the pages SQLite uses and the counts read the rows, so a byte flipped in the header's file change counter — the field a write interrupted before the header was finalised leaves wrong — leaves both exactly as they were and reads as a healthy backup. The size is checked before the digest so a truncated or appended-to copy is named as the size change it plainly is, rather than as a 64-character hash a reader has to diff by hand. A truncated copy is a different case from a flipped byte: it fails `integrity_check` and cannot be opened for a schema at all, which is why the size and digest are checked first and need nothing but the path.

The manifest is created at 0600 like the snapshot, for the same reason: it is a full description of the memory database, and a description is not something to leave at the width a create-then-chmod passes through. It is *replaced* rather than refused-on-exists, which is the opposite of the snapshot's rule for the opposite reason — it is derived from the snapshot beside it, so one found at that path belongs to an earlier backup of a file the user has since deleted. Nothing is lost: the snapshot it described is gone, and it is the snapshot, not its manifest, that a backup is. A manifest that fails to parse is refused by every reader rather than ignored, so a write interrupted part-way leaves a file that fails loudly instead of one silently treated as absent — which would turn a damaged sidecar into an unverified backup that still looked fine. Replacing also means the open has no `O_EXCL`, and that is where the difference from the snapshot bites. `reserveBackupPath` gets symlink safety for free: O_EXCL is the atomic claim, so its `Lstat` and its create are one step with no window between them. A manifest cannot have that, so it has TWO defences and the `Lstat` is only the classification — the enforcement is `O_NOFOLLOW` on the open (`manifestOpenFlags`, split per platform because Windows has no such flag and its `Lstat` is the whole defence there). With only the `Lstat`, a link planted in the window between the two would be written *through* and its target truncated, overwritten with a manifest and narrowed to 0600: a file the user never named, in a `--out` directory they chose.

**Verification reads the copy and nothing else.** `memory.VerifyBackup` runs four checks, in this order: the size and the SHA-256 against the manifest's, SQLite's `integrity_check`, the file's own `user_version` against this build's *and* against the manifest's, and the row counts against the manifest's. Each is reported as `ok`, `failed` or `skipped`. The digest is first because it is the only check that needs nothing but the path — a truncated copy has no schema to read, so the last three cannot run at all — and the structural checks then qualify a file the digest has already identified. Every check that *can* run does: a copy with a flipped byte is still worth reporting as structurally sound, and a report that stopped at the first failure would answer "this is not a backup" to a file that is mostly one.

`skipped` is a state of its own and is not a pass, and the CLI keeps the strongest word for the strongest result: `verified` is printed only when every check ran, a file with skipped checks is `checked`, and `refusing` exits non-zero. A **missing** manifest is not a failure: the pre-migration copy an upgrade takes is written without one, and it is exactly the copy a user wants to check after a bad upgrade, so refusing it would make the command useless when it is wanted. `ghost backup verify` opens **no** store at all — not through `bootstrap()`, which would migrate and seed the database in the data directory. The moment a user reaches for this command is the moment they are least sure what state their own store is in, and a check that changed it would be the worst possible answer. SQLite may still build a `-wal` and a `-shm` beside the file being checked, which is why the documented claim is the narrow one ("does not open the database in your data directory") rather than "reads nothing else".

The counts are the *corpus* (`projects`, `memories`, `memory_links`, `tasks`, `decisions`), and they are counted from the written file for the reason above — so a store that changed between two backups gets two manifests that each describe their own file, and `ghost prune` moving `memories` is nothing special: the backup taken after it records the smaller number and verifies. The change log is deliberately not among the counts, because a `delete` tombstone is the record of a row that is **not** in the corpus, and a restore should not be told to expect a memory the manifest says is gone.

The schema check is **asymmetric on purpose**. A file from a newer Ghost is refused, because `OpenDB` refuses it outright and no next step with this build can open it. A file from an older Ghost passes, because that is what the documented restore path produces: the pre-migration copy `OpenDB` writes is by construction at a lower `user_version` than the build that wrote it, and the next ordinary open migrates it. A symmetric check would reject the one file the command exists to protect.

`PRAGMA integrity_check` needed more care than a `QueryRow`. Its unit is a *finding*, not a row: several findings arrive newline-separated inside one row, and a driver may hand over the whole report as a single row, so the rows are split before anything is judged — reading one row would report a file with twelve findings as having one. The findings are rejoined with `; ` because the CLI prints one line per check and a newline in a detail would shift every column under it. On a badly damaged file the query itself can fail partway with rows already delivered, and those are reported along with the error, because "it got this far and then could not continue" is more use than either half. The list is capped and the number left out is stated, so a cap is never silent.

`VerifyReport.CountsRead` and `VerifyReport.BytesRead` exist because the zero value of a row count is indistinguishable from a genuinely empty database, and the zero value of a size is indistinguishable from a 0-byte file. A run that could not finish leaves them false, and the report says `not checked` in place of either — "no rows" and "0 bytes" are claims about a file, and they are the two claims this report must never make about a file it did not measure. A damaged sidecar is the reachable case for both: `ReadBackupManifest` fails before the `os.Stat`, so a perfectly good multi-megabyte snapshot would otherwise be reported as 0 bytes.

`ManifestPresent` is separate from `HasManifest` for the same reason, in the other direction. The two failures are opposites: a *missing* sidecar is expected — a pre-migration copy has none — and merely limits what was checked, while one that is *present and unreadable* is damage. Collapsing them would tell the reader to go looking for a file that is sitting right beside the snapshot, which is precisely the "treating a damaged sidecar as absent" the readers of a manifest are built not to do.

**An export is an inspectable artifact.** `internal/portable` writes the store as JSON Lines — one self-describing object per line behind a schema-version header — and reads one back. It is a wire format with its own version (currently 2, which added the evidence records nested under each memory; a v1 file still imports, because the readable range is an explicit list of the versions whose rules still hold), independent of `schemaVersion`: an artifact is a file that outlives the ghost that wrote it, so its meaning is the shape of the records, not the tables behind them. Ordering is fixed (projects, then memories, tasks and decisions, each by id) and the header carries no timestamp, so two exports of an unchanged store are byte-identical and a diff of two artifacts shows only what changed in the store. The line-oriented shape is also why a damaged file is survivable: a line that is not JSON is rejected on its own, by line number, and the records around it still import — which is what the reader gets from a truncated or hand-mangled artifact. Whole-file refusals are reserved for the problems that are not one line's: a missing or unreadable schema version, a second header, an unknown record type. Those mean the file is not this format, or not one this build can interpret, and applying part of it would be importing data whose meaning is a guess.

The reads and writes are separate store methods rather than reuse, and each for a stated reason. `PortableProjects` exists because `ListProjects` has no `repo_remote` column — the field that makes two checkouts of one repository one project, and that another machine needs to resolve a project from its own directory. `PortableMemories` exists because the ordinary list readers drop the validity triple, and restoring a memory with a fresh `created_at` would age it out of injection immediately. The `Import*` methods exist because no existing writer can preserve an imported id, an imported `created_at` or an imported pin: `Create` generates its own id and `Upsert` is deliberately a dedup probe, which is the wrong operation for a restore. Each takes an `apply` flag, so a dry run runs the same validation as the run it previews rather than a second code path that can disagree with it.

Four things are deliberately outside the artifact, and the first three are there for one shared reason — they are derived rather than stored knowledge. `memory_embeddings` and `memory_links` are rebuilt by the embedding and linking workers, and `memories.resolve_kept_hash` is recomputed by the resolve pass; shipping them would mean shipping a model's output as if it were the memory, importing edges ahead of their endpoints, and marking a memory the classifier has never seen as reviewed.

The fourth is Ghost's own `_global` `builtin` seeds, and they are excluded because no import could ever reconcile them. `SeedGlobalMemories` writes each seed under the schema's default `hex(randomblob(16))` id, which is per-install, so an artifact's copy never equals the destination's own — and every importer dedups by id alone. Importing one machine's artifact into a store Ghost had already bootstrapped therefore inserted a second row with byte-identical content: a second pinned `builtin` copy of a shipped rule, or, after the default provenance downgrade, an `onboarding` copy that has silently lost the "this is Ghost's own" label. Nothing repairs it, because re-seeding skips by content and no path deletes memories.

The exclusion is at the export read rather than in the import: it keeps the import rule simple, and the destination already re-creates exactly those rows by content on every open, so a restored store ends up with one copy — written by Ghost, not two. It is on the seed (`project_id` **and** `source`) rather than on the project, because a user's own memory filed under `_global` is the only copy of itself and has no other way in.

Provenance is a per-run policy, not a per-record one, and it lives in `memory.ImportOptions` because the store is what enforces it. A dry run opens the database read-only, so the preview cannot migrate a store whose schema is behind or seed the builtin rows while reporting "nothing written" — the import's own writes all return before their INSERT when `apply` is false, so the read-only connection loses no capability the preview needs.

Read-only therefore carries two obligations the read-write path does not, and both are in `openReadOnlyTransferStore` because it is the only read-only open the transfer commands use. It must not create the database, and it must not read a schema it cannot query: `PRAGMA user_version` is read off the open connection through `memory.DBUserVersion` and compared with `memory.SchemaVersion()`, and a store behind or ahead is refused before the first query. Without that check the failure is SQLite's `no such column: repo_remote`, which names a column rather than the version mismatch and lands on exactly the machine the docs tell a user to prepare. The check reads and never writes, so a version check cannot itself become the migration it is reporting the absence of.

Strict on both sides — equal and only equal passes — which is stricter than the failure it replaces needs to be. The only post-v10 columns these readers select are `projects.repo_remote` (v11) and `memories.scope` (v12), so a v12-v16 store is perfectly queryable and a floor would have kept `ghost export` working on one. A floor is not chosen because it hardcodes an assumption about which columns exist at which version, nothing enforces that assumption, and the day a reader selects a column added in a later migration the floor admits a store that fails with `no such column` again — with nothing in the suite asserting about the old versions to notice. Equality is self-maintaining: the store is refused unless it provably carries every column this build selects. The cost is one read-write open for a v12-v16 user before an export works. It can be `ghost mcp init`, any session, or `ghost backup` — and `ghost backup` is reached through `bootstrap()` itself, so it migrates and seeds the store it copies, and `OpenDB` refuses a store from a newer Ghost exactly as it refuses one to any read-write opener. It runs no version check of its own, but that makes it a way to *pay* this cost rather than a way around it: there is no read-only way in this build to get a copy, and describing `ghost backup` as one would tell a user that a command which writes has read their store. Read-write is kept for it deliberately, because a store behind the current schema is exactly the store a user most wants a restorable copy of, and a read-only open would refuse it. `openReadOnlyTransferStoreUnchecked` exists only so a test can prove the strictness is about the version: it opens a behind store, shows it queries fine, and shows the checked opener refusing it anyway.

Import never overwrites a record whose id already exists, and that is the property the rest of its behaviour rests on. The artifact is the older of the two copies by construction, so an overwrite would restore stale data over live data — and making re-running a no-op is what lets a rejected record be repaired by fixing the file and running the import again. A record that cannot be imported is rejected individually and the run continues, because one hand-edited line must not abandon ten thousand good ones; the rejections are counted and the command exits non-zero, so a partial import is never reported as a complete one.

Three decisions belong to the caller rather than to the store, and they are the same shape: the store owns what a row may be, the importer owns what a run means.

The first is provenance. An artifact is a file that arrived from somewhere, and on its own authority it would be able to plant rows that read as the user's own words (`manual`) or as Ghost's shipped rules (`builtin`) — both excluded from consolidation by name — or a pinned row, which is excluded whatever its source. So `ImportOptions.TrustProvenance` is off by default and every imported memory is stamped `onboarding` (the source `internal/claudeimport` already uses for memories brought in from outside Ghost) and unpinned; the flag restores the artifact's own values for a user restoring their own database. That asymmetry is the deliberate part: the cost of the default being wrong is planted provenance, and the cost of the flag being wrong is a user passing it once.

The second is project identity. Project ids are per-install, so an artifact names a project the destination has never seen while the destination usually has its own for the same checkout or repository — and `projects.path` and `repo_remote` are both UNIQUE, so inserting the artifact's project would collide, and one collision would take every child record with it because they all name the artifact's id. `resolveProjectMapping` therefore maps each artifact project onto the project that already holds the checkout before any record runs, and the store's own `projectCollision` check catches whatever survives that as a sentence naming the project it collides with, in the dry run as well as the apply. The third is which fields fall back to a column default. A record that states no `created_at` must not be stored with the empty string, because `julianday('')` is NULL and NULL sorts the row out of every ranked read; a record that states no `importance` must not be stored as 0, because `Store.Create` binds a `Memory` saved without one as 0 and an artifact carrying `"importance":0` would be silently promoted to the column's 0.5 on every re-import. Both fallbacks therefore live in the SQL, where the column default can still apply — a value bound for a column never lets its default — and the importance field is a pointer so that an absent key and a stated 0 stay distinguishable.

## Untrusted parse surfaces

Three packages read text somebody else wrote, and none of that text can be trusted to be well-formed, to be the size it claims, or to be a name. `internal/claudeimport` reads Claude Code's auto-memory files, which a session derived from a checkout wrote; `internal/hostevent` reads a host hook's stdin, including values the host itself copied out of a transcript; `internal/obsidian` reads the store, which a portable artifact can seed with ids, names, tags and bodies Ghost never generated. The invariant is one sentence and it is stated once, in `internal/adversarial`: **a value parsed from an untrusted surface is data** — stored exactly as it was parsed, naming at most one path component inside the directory its surface owns, and never deciding the parse. `internal/adversarial` is test-only support; the fixtures in all three packages drive the same corpus and the same assertions over it, so relaxing the invariant fails all three at once instead of drifting in whichever file was read last. The MCP-side suite (`internal/mcpserver/adversarial_test.go`, #538) proves the same invariant one layer out, over the wire.

The interesting failures were all silent ones, which is why each of these is a fixture rather than a rule in prose:

- **A ceiling that discards instead of refusing.** `parseFrontmatter` scanned front-matter lines with `bufio.Scanner` at its 64 KiB default, and a line past the default stops the scan with an error the loop never read — so a memory file with a long description lost its *type*, and with it its category, while the unparsed fences were stored as the body. A line now gets its own bound (`maxFrontmatterLine`), and a block that could not be read whole is not front matter at all: the file imports as body text, which is the same fallback an unterminated block already got. A file past `maxMemoryFileBytes` is skipped, because a memory this import keeps is capped at 8 KB and a file orders of magnitude past that is not one.
- **A name that decides a path.** A note's filename is `slug(content) + "-" + id fragment + ".md"`, and the id fragment used to be a bare prefix of the record's id. Ghost mints hex ids, so that held for everything the store wrote itself — but `Store.ImportMemory` writes an artifact's ids verbatim, and an id can hold a separator, a NUL or a backslash. A separator put the note outside the `Memories/` subtree prune manages, and the NUL failed the write with `EINVAL`, which failed the *whole* export on every run and every retry, taking every other project's notes with it. `idToken` keeps the prefix when it names a single path component — a dash, a dot or a space is left exactly as it was, so no real note is renamed — and hashes the whole id otherwise, because hashing keeps two different hostile ids apart where replacing the offending bytes would collapse them onto one filename. `folderNames` bounds a project folder for the same reason, since a project name is whatever a caller sent.
- **A reader that does not agree with its writer.** `fm` writes `ghost_id` through `yamlScalar`, which quotes any value a YAML reader would not take as a plain scalar; `hasGhostID` read the line back raw, so it returned the *quoted* text for every id that needed quoting. Prune then looked that quoted text up in a keep-set keyed by the real id, did not find it, and deleted the note it had just written — on every export, for good. Only the double-quoted form `yamlScalar` emits is now unquoted: this function reports which files are Ghost's to prune, so a single-quoted value in a hand-written note must keep reading as its own text.
- **A key that has to be recoverable, chosen so it does not have to be.** The same failure has a second and quieter form, and it is why the keep-set is a set of canonical note PATHS relative to the vault root (`keepSet`, keyed by `keepKey`) rather than a map from `ghost_id` to path, and why it is not keyed on the bare basename either — the same slug occurs in two project folders, and a memory and a task with the same title can share an id fragment, so a basename keyed on the live note in one place would keep a stale copy alive in another. This is only the retention half of prune's decision and the other half is unchanged: the closed front-matter block is what makes a file Ghost's to touch at all, so a note without one is never removed, whatever it is called. What moved is the second question — *which* note this is — from the id parsed out of the note to the path the system wrote it at. `yamlScalar` flattens a tab, a newline and a carriage return to a space, which is what keeps every key on one line, and it is lossy: `tab<TAB>id`, `tab<CR><LF>id` and `tab id` are three records and one `ghost_id` line. Keyed on that line the three notes collide on one key, the other two match nothing, and prune deletes them: the export writes three notes and leaves one, silently, every run. Keyed on the path the system wrote, there is nothing to collide, because that path names one location and one record. The general rule this follows: a renderer and its reader have to agree on an encoding, and a key that decides a *delete* should be one the system holds rather than one it has to parse back out of a file it wrote for someone else to read.

Two properties are the opposite of a filter, and the fixtures say so explicitly. A planted payload must still be **retrievable**: dropping it would hide the tampering from the user, which is worse than returning it as data, and quotation into the data block happens on the way out (`quoteData`, #538) rather than on the way in. And `hostevent` **fails open** on all of it — oversized, deeply nested, invalid UTF-8, a NUL, an unknown event name — because "allow the stop" is the only response the hook contract emits. A ceiling (`maxPayloadBytes`) was the one thing missing there, because nothing on the path bounded the read: the hook read stdin with an unbounded `io.ReadAll` and `Parse` retains the payload twice. The hook now reads through `hostevent.ReadPayload`, which stops one byte past the ceiling, so the ceiling bounds the allocation as well as the parse.

## Configuration and filesystem layout

`internal/config` loads compiled defaults, `/etc/ghost/config.yaml`, the user config file, and `GHOST_*` environment variables. Commands apply supported flag overrides after loading. The data directory is resolved from `XDG_DATA_HOME` or the user's home directory and contains `ghost.db`.

`memory.TightenPermissions` (`internal/memory/perms_unix.go`, a no-op on Windows) strips the group and other bits from the configured data directory and from `ghost.db`, `ghost.db-wal` and `ghost.db-shm`. The pass is subtractive by construction and cannot widen a mode, it `Lstat`s each path and chmods only regular files so a symlink is skipped rather than followed, and it is scoped to the configured data directory so a database opened in a scratch or eval tree does not have its parent chmod'ed. A chmod that fails logs a warning and does not fail the caller.

There are exactly **two** read-write *open functions* in the tree, and both call it. `memory.OpenDB` creates the database or migrates it and calls the pass once, after its first successful query — the point at which the database and the `-wal` and `-shm` files beside it all exist. `mcpinit.bumpSessionCount` is the session hook's single deliberate write, over its own short-lived `rwDSN` connection, and calls it twice: after its existence check, and again after the write. The second call is not redundant. A clean close deletes the `-wal` and `-shm`, so when no other process holds the database, the hook's own INSERT is what recreates them, at whatever mode SQLite gives a new file, and the counter it just wrote is in them until the deferred `Close`. The first call cannot see files that do not exist yet, and the second cannot run before they do.

Every other open is read-only and deliberately stays that way: a diagnostic must be able to report on a database it cannot modify, and a read-only connection cannot create one. That is `memory.OpenReadDB` — the read-only constructor this tree prefers, which refuses a missing file and an in-memory path — at `cmd/ghost/project.go`'s `openDiagnosticStore`, `cmd/ghost/transfer_store.go`'s `openReadOnlyTransferStore` (and `internal/memory/backup.go`'s `countBackupContents`, which counts a written backup), the MCP server's injected read handle in `bootstrap()`, and the session hook's store and global-memory read in `internal/mcpinit/hook.go`. Two package-local `roDSN` families predate it and are not `OpenReadDB` calls: the one in `internal/mcpinit` (`stophook.go`, `lifecyclelock.go`, `lifecyclemarker.go`) and its separate namesake in `cmd/ghost/obsidian.go`, which opens through `sql.Open` directly. `eval/cycle` is mixed: its `state.go` and `inject.go` open read-write through `OpenDB` and write, while `inject.go` also has one `mode=ro` read — so an eval run tightens the three files in its scratch tree and not the directory holding them, since `isDataDir` does not match a scratch path.

"Read-only" is a property of the open, not of the command, and a read-write open tightens wherever it is called. `cmd/ghost`'s shared `bootstrap()` is the usual route, and it already ran migrations and stamped `user_version` before any command-specific work, so most commands tighten simply by starting up. Three paths open read-write without it, and each was already writing or migrating for its own reasons: `mcpinit.checkStoreHealth` (behind `ghost mcp status`, which must not bootstrap — a status check that created the database would turn the next run's "no Ghost database" line into a healthy one), `runMaintenanceStatus`, and three paths under `ghost hook` / `ghost context`:
- `mcpinit.importMemories`, reached by `finalizePlugin` before any of the session-start gates when a Claude Code plugin install is finalizing for the first time. It opens read-write to import memory files, so that one fire tightens regardless of source or whether it is a subagent.
- `runSessionStart`, which returns early for a subagent, a `resume` and a `compact`, and gates the bump on `projectID != ""` and on the source being empty or `startup` — so a `clear` does not bump either.
- `RenderSessionContext`, which opencode's plugin reaches by spawning `ghost context` at start, because opencode has no context-injection surface of its own — it returns before `runSessionStart` on the `InjectContext` gate, and its plugin spawns the context render instead. It has no source or subagent gate at all, and is gated only on `projectID != ""`. Its `as_of` half (`RenderSessionContextAt` with an instant) is not on that list at all: a historical read opens the same read-only handle, runs no Obsidian sync and no counter bump, so it tightens nothing — which is the point, since a question about the past must not touch the present.

So a genuine new session in a directory that resolves to a project tightens; on the `ghost hook` path a resumed, cleared or compacted one does not, while on the opencode path any start does. That is the same gate that keeps the session counter honest. Goose's session start tightens nothing at all, for the same `InjectContext` reason. A session stop can reach a read-write open indirectly when the stop hook spawns `ghost lifecycle` for a reflection pass.

That is why the pass is a named exported function rather than a line inside `OpenDB`: a read-only path is not a place to add a side effect to, but a read-write one is, and there are two of them.

The contract guards are `internal/memory/perms_test.go` and `internal/mcpinit/hookperms_test.go` (the latter `!windows`, since the assertions are about POSIX mode bits); see [`configuration.md`](configuration.md#data-directory-permissions) for the user-facing description.

### The development-build data-directory guard

`config.CheckDevDataDir` (`internal/config/devforbid.go`) refuses a data directory `GHOST_DEV_FORBID_DATA_DIR` names when this build is not a release, and it is called from ONE place: `config.DataDirPath`. Every path into the data directory in the tree goes through that function or through `config.DataDir`, which calls it, so "no path can reach a forbidden data directory" is a property of the tree rather than a rule each caller has to remember.

That placement is the second attempt, and the first one is why it is worth stating. Guarding the store-opening entry points (`cmd/ghost`'s `bootstrap()` and its per-command resolvers, plus a set of wrappers in `internal/mcpinit`) left four paths that resolve the data directory WITHOUT opening a store, and three of them wrote: `scratch.Root` (so `ghost lifecycle`'s `scratch.Reap`, its first statement, created `<dataDir>/scratch`), `WriteLifecycleFailure` and `ClearLifecycleFailure` (whose `resolved == ""` fallback then wrote or deleted a marker named after a project the code had been told it cannot resolve), and `readLifecycleFailure` (which runs on every session start, ahead of anything else, and whose age-based self-clean calls `ClearLifecycleFailure`). A guard placed at the callers is a guard with a hole in it, and the hole is found by a path that resolves a directory for a purpose other than opening a store.

So the callers' obligation is the ordinary one: handle the error. The fail-open paths — every hook loader, the marker bookkeeping, `TightenPermissions`' `isDataDir`, the scratch budget's run record — already returned, skipped or reported on a data-dir error of any kind, which is why the refusal needed no new handling in any of them. `ghost mcp status` reports it as a failed check rather than staying silent, because its job is to say what is wrong, and `cmd/ghost`'s commands report it and exit `1`. The one exception is that report's own pre-existing early return: under a plugin-managed install `mcpinit.Status` reports the plugin and skips the whole store section, so no store check — the refusal included — runs on that path.

"Release" is `selfupdate.IsRelease`, so it is the release parser's answer and not a second spelling of "looks like a version": `dev`, a `git describe` stamp and a prerelease all parse and are not releases, and a tag that is not a semantic version is not one either. The version reaches `config` as a value `cmd/ghost`'s dispatch hands over once (`config.SetBuildVersion`, beside `memory.SetDetectRemote`, from the same `version` var the release ldflags stamp and `ghost version` prints), and `config`'s own default is `dev`: a build that never wired the version is a development build and is refused, which is the direction a default has to fail in.

The ordering is the other property. The check runs BEFORE `config.DataDir` creates anything, so a refusal leaves no phantom data directory, no migration and no pre-migration copy — and it is a refusal, not a skip: a path that resolved nothing does not fall back to a raw name and write there anyway. Both sides of the comparison are canonicalized (absolute, symlinks resolved, a missing tail re-attached to the deepest existing ancestor), because a guard that compared strings would protect the store only for the spelling its author happened to test.

See [`configuration.md`](configuration.md) for the layered configuration contract and [`internal/config/config.example.yaml`](../internal/config/config.example.yaml) for the annotated YAML template. The development-build guard above is an environment variable rather than a config key, so it appears in `configuration.md`'s variable list and its own contract is [`cli.md`'s section on it](cli.md#development-builds-refusing-a-real-store).

## Build and release

Ghost is built as a static binary with CGO disabled:

```bash
CGO_ENABLED=0 go build -o ghost ./cmd/ghost
```

GoReleaser produces Linux, macOS, and Windows binaries for amd64 and arm64, with checksums. The Docker build uses a Go Alpine builder and an Alpine runtime, also with `CGO_ENABLED=0`. CI runs tests, race tests, vetting, linting, vulnerability scanning, and workflow validation.

Every workflow job names an explicit runner image rather than a floating `-latest` label, and the exception is the one non-required job that exists to test a candidate — a job whose purpose is to try the next image is the one place a non-production label belongs.

The Linux pin is `ubuntu-24.04`, decided 2026-09-30 (#812): GitHub moves `ubuntu-latest` to Ubuntu 26 on 2026-10-19, and every release attestation published to date was minted on 24.04, so a floating label would move the image under the provenance clients verify. The non-required `ubuntu-26-canary` job in `ci.yml` runs the `build-and-test` steps on `ubuntu-26.04` so the eventual bump is made against a run that has already been green — the pin moves when the canary passes, not when the date arrives.

The same rule covers the Windows legs, decided 2026-09-30 (#830): `windows-plugin`'s matrix names `windows-2025-vs2026` rather than `windows-latest`, because that floating label moves between Windows Server images, which changes pwsh, the preinstalled toolchain and the path and permission behaviour that `install.ps1`'s decision tests and the `internal/mcpinit` Windows tests exercise. The label names the Visual Studio toolset as well as the Windows release, so neither can move under the matrix; `windows-11-arm` was already explicit. No canary covers this label, because `ubuntu-26-canary` runs no Windows step — a deliberate Windows bump is decided by pinning the candidate and reading the `Image:` line the run reports, which is where this pin came from (`windows-latest` resolved to `windows-2025-vs2026` in run 36772364289). The label must be one actionlint knows, or be declared in `.github/actionlint.yaml`; 1.7.12 knows this one already.

## Testing

Three layers, and the third is about the artifact rather than the packages.

**In-process tests** call into `internal/...` directly. That is where logic is tested, and it is the overwhelming majority of the tree: the store, the assembler, the consolidation tiers, the migration steps, the hook parsers. `go test ./...` runs them, and they run in CI on every leg.

**Contract guards** pin the properties a change could quietly undo. `TestConcurrentProcessesMixedReadWrite` and `TestMultiProcessSharedDatabase` (see [the concurrency contract](#concurrency-contract)) are the largest; there are others for permission tightening, the credential guard's reach, and the prompt contracts. They are named for the property, not the function, and each one fails if the setting or rule it depends on is removed.

**End-to-end tests** exercise the BUILT binary, and they live in `e2e/` behind the `e2e` build tag:

```bash
make test-e2e          # or: go test -tags e2e ./e2e/ -count=1
```

The tag is the whole mechanism for keeping `go test ./...` and CI unchanged: with it absent every file in `e2e/` is excluded, and the `./...` pattern skips a directory whose files are all build-constrained out — silently, and verified rather than assumed. There is no second `go test` line in CI, so the layer runs on demand and locally, not on every push.

What it covers, and why it cannot be in-process:

- **The MCP surface**, read from the server's own `tools/list` and checked in both directions against a table of subtests: a tool added to the product with no subtest fails the suite. Every resource, resource template and prompt too, driven over the real stdio transport with the go-sdk client.
- **Every CLI subcommand** in `cmd/ghost/help.go`'s `usageByCommand` table, **parsed out of that file at run time** and compared against the suite's own table in both directions — a subcommand registered with no table row fails, and so does a row for a subcommand the product does not register. Each row accounts for itself in exactly one of three declared ways (a `run` closure that invokes it, a `coveredBy` naming the dedicated test that does, or a `helpOnly` reason), so "never run" cannot hide in a comment. One command is help-only: `ghost upgrade` reaches a hardcoded `api.github.com` URL that no environment variable redirects, and this suite makes no network call by design. Its help and its refusal of an unknown flag are both exercised.
- **The lifecycle hooks** for all four hosts and all three events, with per-host transcript fixtures in each host's own native format, the fail-open contract for eleven malformed payloads, and the Stop hook's spawn behind its pid lock and `min_interval` cooldown.
- **The upgrade path**: a store downgraded to the previous schema version is migrated by an ordinary read-write open, and the pre-migration backup, the version stamp, the surviving rows and the first post-migration history baseline are all asserted. A store stamped NEWER is refused, and the assertion is that the refusal wrote nothing.
- **Concurrency across processes**: two `ghost mcp` servers and a CLI writer against one store for a few seconds, asserting no `SQLITE_BUSY` reaches a caller and every reported id is in the store exactly once afterwards.

Three things make it safe to run, and each is enforced rather than hoped for:

- **One build, the real program.** `TestMain` runs `go build` once into a temp dir; every test executes that file. Nothing re-execs `os.Args[0]`, because under `go test` the test binary IS the suite.
- **A sandbox per test.** Temp `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME` and `XDG_CACHE_HOME`, and a child environment BUILT from an allowlist rather than filtered from `os.Environ()` — a filter cannot answer "is there a variable here nobody thought of". Nothing reaches the developer's own `~/.local/share/ghost`, `~/.claude`, `~/.codex`, `~/.config/opencode` or `~/.config/goose`. The one layer a sandbox cannot redirect is the system-wide `/etc/ghost/config.yaml`.
- **No real model call, ever.** The four CLI harnesses are shell fakes first on `PATH`, answering from the prompt they were handed rather than from a table of test names: the reflect answer is a `keep` per id in the prompt, the resolve and supersede answers are the files a test writes. The embedding endpoint is an in-process HTTP server producing a deterministic hashed bag-of-words vector, so hybrid search and the supersede candidate scan have a real vector space and no model is loaded.

Two properties of the fakes are load-bearing and easy to get wrong, so they are stated here. A backend's answer shape is not uniform: `opencode` requires JSONL on stdout (`{"type":"text","part":{"type":"text","text":…}}`) and any non-JSON line is a hard error there, while `claude`, `codex` and `goose` take bare text. And the operation is decided by scanning BOTH stdin and argv, because the `claude` backend is invoked with the system prompt as a real `--system-prompt` argument and only the user content on stdin.

## Historical design records

The `docs/superpowers/` tree contains archived specifications, plans, and reports. It explains how the architecture reached its current shape but is not the canonical source for current behavior. Start with this page, the source, and the current user documentation.
