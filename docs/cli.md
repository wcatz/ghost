# Ghost CLI reference

The `ghost` binary is both the MCP server and the maintenance CLI. Run `ghost help` for the built-in top-level summary, or `ghost help <command>` (including a two-word path such as `ghost help project bind`) for that one command's own usage on stdout — `ghost -h <command>` does the same. A name that matches no command is reported on stderr and the summary is shown instead — a help token in place of the name is a second help request rather than a typo, so it is answered with the summary and nothing else, and `ghost help -h <command>` names the command as `ghost -h <command>` does.

Every subcommand accepts `-h` or `--help`: it prints that command's usage on stdout and exits `0`, before anything with a side effect runs — no configuration load, no database open, no file written, no harness spawned. The top-level summary (`ghost help`, `ghost --help`) is unchanged by this and still prints to stderr.

Two spellings decide whether a flag is a request. The token after a flag that *this* command takes a value for is a value, never a help request — `ghost reflect --project -h` runs reflect for a project named `-h` — and a flag belonging to a different command is not a value at all, so `ghost upgrade --cwd -h` prints the upgrade usage instead of upgrading with the help flag swallowed as `--cwd`'s value. And a bare `--` ends the options for that scan: a token after it is an operand, never a help request, so `ghost reflect -- --help` runs reflect rather than printing usage. What the command then does with that operand is its own parser's business — none of them implements `--` (several report it as an unknown flag), so a project whose name looks like a flag is still addressed with the verbatim `--project <name>` form.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | The command ran, or the reader asked a question (`-h`/`--help`, `ghost help`). |
| `2` | **Usage error.** The CLI cannot act on the command line: a command that does not exist, a subcommand that is not one of its command's, a command group invoked with no subcommand, or no command at all. |
| `1` | The command was found and ran, and failed: a database that would not open, a refused operation, a harness that could not be reached, an argument its own parser rejects. |

A usage error prints one diagnostic line naming the word that matched nothing, then the usage of the level it was typed at — both on stderr — and nothing on stdout, so a caller reading a command's output sees an empty stream rather than a command list where results should have been:

```console
$ ghost project frobnicate
ghost project: unknown command: "frobnicate"

Usage: ghost project delete <name-or-id> [--apply]
       ghost project merge <old-name-or-id> <new-name-or-id>
       ghost project bind <project-id> <checkout-directory>
$ echo $?
2
```

The same shape applies at every level: `ghost mcp nope` and `ghost project` (no subcommand) exit `2` the same way, `ghost frobnicate` names the top level and shows the command list, and bare `ghost` reports that no command was given. Two command groups have a default action — bare `ghost mcp` starts the server and bare `ghost backup` takes the snapshot — because both bare forms are invocations a user is told to type.

A word after a command that takes an *operand* is not a subcommand and is never reported as an unknown one: `ghost history <memory-id>`, `ghost reflect <project>` and `ghost import <file>` all take the word as the thing they were asked about, and their own parsers report an operand they cannot use — a missing memory id (`ghost history`), a second project (`ghost resolve`), a file that is not there (`ghost import`), an unknown flag. `ghost reflect` is the exception among those parsers and keeps its historical behaviour: given two positionals it consolidates the last one, without a diagnostic. An unknown **flag** is not a routing error either, and never exits `2` — the command was found, so what answers the flag is that command's own parser: `ghost bench --wat` and `ghost upgrade --wat` reject the flag — an unknown flag and an unknown argument respectively — and exit `1`, while `ghost reflect --wat` ignores an unknown flag, as it always has.

Stored values are rendered, not echoed: an id goes through `assemble.Token`, a project name or path through `assemble.Label`, and stored free text inside `«…»` data delimiters — so a newline inside a stored id or memory cannot start a line the reader takes for Ghost's own output, and text between `«` and `»` is data.

## MCP server

### `ghost mcp`

Starts the MCP server over stdio:

```bash
ghost mcp
```

MCP clients normally spawn this command themselves. Do not run it as a long-lived daemon unless your client is configured to manage the process.

### `ghost mcp init`

Configures supported MCP clients:

```bash
ghost mcp init
ghost mcp init --client claude
ghost mcp init --client opencode
ghost mcp init --client codex
ghost mcp init --client goose
ghost mcp init --client all
```

| Flag | Meaning |
|---|---|
| `--client <name>` | Target `claude`, `opencode`, `codex`, `goose`, or `all`. Without it, detect clients on `PATH`. |
| `--dry-run` | Print the changes without writing them. |

The operation is idempotent. If multiple supported clients are detected, Ghost configures all of them unless a target is specified. Plugin-managed Claude Code integrations are detected and left under plugin control.

### `ghost mcp status`

Checks one client integration:

```bash
ghost mcp status --client claude
ghost mcp status --client opencode
ghost mcp status --client codex
ghost mcp status --client goose
```

Without `--client`, status targets Claude Code. The checks include client registration, lifecycle wiring, the database, Ollama reachability, and embedding/link coverage where applicable. A generic MCP client has no Ghost-specific status integration.

Registration means what each client reads: `claude mcp get ghost` for Claude Code, the `[mcp_servers.ghost]` table in `~/.codex/config.toml` for codex, the packaged `mcp.json` for goose, and the `mcp.ghost` entry in opencode's config file for opencode. For opencode, status judges your user-level layers — opencode's config directory (`$OPENCODE_CONFIG_DIR` when set, else `$XDG_CONFIG_HOME/opencode`) with **both** of its config spellings read as separate layers, `opencode.json` then `opencode.jsonc` (opencode loads both, and the later one wins a conflict), then `$OPENCODE_CONFIG` (a file path), then `$OPENCODE_CONFIG_CONTENT` (inline JSON), later sources overriding earlier ones key by key — and checks the entry is present, enabled, and resolving to the ghost binary on PATH; a per-checkout `opencode.json` and the `.opencode` directory are deliberately not judged, because either would make the verdict depend on the directory the command was typed in. The entry is the fallback: opencode's lifecycle plugin registers `mcp.ghost` at startup, so a broken entry is an informational warning while the plugin is current and an error only when the plugin is missing as well. A config layer status cannot read or parse is an error under either gate — opencode drops such a layer, so the entry in it is unknown rather than covered. When the entry itself is the error, the failing line names the file to edit and the edit that repairs it — the whole `mcp.ghost` block to paste when the entry is absent — because `ghost mcp init --client opencode` installs the lifecycle adapter and never writes opencode's config; when a layer could not be read or parsed, the line names that file instead.

Status also lists any project that records no usable checkout and no repository remote, with the `ghost project bind` command that repairs it. Those projects are not a health failure — every check above can pass while sessions in such a checkout silently get no injected context — and the section is omitted entirely when there is nothing to fix. The listing reads the database without opening it for writing, so a status run never creates the store it is reporting on. The section also names the follow-up step for the standalone, init-managed Claude integration: after binding, `ghost mcp init` writes the per-checkout memory redirect that a newly absolute path makes the redirect check expect — unless that checkout has a `MEMORY.md` of its own, which init only overwrites when the content looks like a stale Ghost redirect.

Status also reports how fast `memory_history` is filling, and how much of that is noise:

```text
  - history: 8 version rows in 24h, 6 restatements (75%), deepest memory 7/50 versions, store 8/20000 rows
  ! 75% of the 8 version rows written in the last 24h restate the version before them (warning threshold 20%) — they take up room without recording anything, and the retention caps are what evict them: `ghost history compact` reclaims pre-#727 restatements and deliberately leaves what this build wrote, plus the newest version of any memory
  ! memory 1F2E3D4C5B6A7988 is closest to the per-memory cap: it holds 7 of its 50 versions and wrote 7 in the last 24h, so the cap is 6.1 days away at that rate (warning threshold 14 days)
```

The restatement finding is careful about what it tells you to run, and the care is
the point. The share counts **every** version row that recorded exactly what the row
before it held, while the compaction removes only a subset: a memory's newest
version, a row naming another memory, any phase but `reflect`, anything recorded
after the repair's own `--before` bound, and every row of a memory that has since
been deleted. The report therefore measures the **pressure** a store is under rather
than what a command could reclaim, and the sentence says so — the caps are what
evict a restatement this build wrote, because the repair will not touch it.

That last guard is why this matters more now than it did. A `delete` tombstones a
memory and its history is **frozen**: no repair and no compaction will ever remove
those rows, and since [retention tiers](#ghost-memory-save) give `expires_at` a day
to arrive, a store now retires memories nobody asked it to. A store that churns
through session-tier memories fills its table with rows only the store cap can
reclaim, oldest-first — which is exactly what the store-cap finding below is for.

The line is store-wide, because both retention caps are (see
[`ghost history compact`](#ghost-history-compact)): how many version rows were
written in the **last 24 hours**, how many of them restated the version before them
of the same memory, the most versions any one memory holds against its cap of 50,
and the table's rows against the store cap of 20 000. A store with no history yet
prints `no version rows recorded yet` rather than a line of zeroes, and a store
with nothing written in the last 24 hours has no rate, no share and nothing to
project, so it prints the numbers and no finding.

`deepest memory` is the **store's widest history** — an aggregate for a line of
totals. It is usually *not* the memory a cap finding is about, and the finding
names its own. The per-memory cap is reached per memory, so its finding is about
one memory by id, quoting **that memory's** two counts: how many versions it holds
and how many it wrote in the window, which is the arithmetic the countdown closes
against. A memory sitting at its cap is trimmed by pruning while it is still being
written, so "at the cap" is the ordinary steady state of an active store rather than
an edge case, and it is a separate sentence from a countdown:

```text
  ! memory 1F2E3D4C5B6A7988 is at the per-memory cap: it holds 50 of its 50 versions and wrote 12 in the last 24h, so its oldest versions are what the trim takes now (warning threshold 14 days)
  ! the store holds 20000 of its 20000 history rows and wrote 310 in the last 24h, so it is at the store cap and the oldest rows in the table are what it trims — not the noisiest ones
```

The id is printed **whole** rather than abbreviated, because here it is an operand:
`ghost history 1F2E3D4C5B6A7988` is what you do with it.

Each finding gets its own `!` line, naming both the number that tripped it and the
threshold it tripped against:

| Finding | Fires when |
|---|---|
| restatement share | **more than 20%** of the last 24 hours' version rows restated their predecessor. 20% exactly is silent: a small amount of restatement is the ordinary shape of a working store, and a threshold that fires on it teaches an operator to ignore the line. Measures pressure, not what `ghost history compact` would reclaim — see above. |
| per-memory cap | some memory reaches its 50-version cap **within 14 days** at its own 24-hour rate — the soonest memory, measured against what *it* wrote rather than the store-wide rate. Reaching the cap starts trimming that memory's oldest versions, which may be the only record of what it said first. |
| store cap | the table reaches 20 000 rows **within 14 days** at the store's 24-hour rate. The store cap trims the **oldest rows in the table**, not the noisiest ones, so what disappears when the table fills is the oldest real change — and on a store that has been retiring memories, it is a deleted memory's frozen history, which no repair can reach. |

**A finding is a `!` line, not a failed check.** It never changes the exit code and
never brings back the `Run \`ghost mcp init\` to fix issues.` footer, because the
verdict above is about **wiring** — whether the memory features are reachable at
all — and `ghost mcp init` repairs wiring, not a history table. The repair a history
finding names is `ghost history compact`.

A store that opened cleanly and then could not be read is the one case that prints
an error instead of a finding — the report says so rather than printing nothing,
because a check that cannot run must not look like a check that passed:

```text
  ! history growth: count history versions in the window: no such table: memory_history
```

The read is read-only, takes no write lock, and costs one pass over the window plus
one covering-index pass over the table — measured at 1.7 ms typical and 183 ms worst
case on a full 20 000-row table. It adds no index, deliberately: a standalone
`recorded_at` index would cost a fifth of the cost of *writing* a history row, on
every save, to save those milliseconds on a status line. The same read, and the same
warning sentences, appear in the `ghost_health` MCP tool, so a terminal and an agent
looking at one store are told the same thing about it.

## Hooks

```bash
ghost hook <event> --source <host>
```

Events currently used by the host adapters include:

- `session-start`
- `stop`
- `session-end`

Supported source tokens include `claude-code`, `opencode`, `codex`, and `goose`. A missing or unknown source fails open with one diagnostic line and exit status `0`; it does not silently route through another host. Re-run `ghost mcp init` to repair legacy wiring.

`ghost hook` is normally called by an MCP client or lifecycle adapter, not by hand.

## Memory maintenance

All maintenance commands are dry-run by default. Preview the result before passing `--apply`.

### `ghost reflect <project>`

Consolidates memories for a project:

```bash
ghost reflect myproject
ghost reflect myproject --apply
```

| Flag | Meaning |
|---|---|
| `--tier auto\|cli\|opencode\|sqlite` | Select the consolidation backend. `auto` is the default and routes to the calling harness when available. |
| `--apply` | Save the consolidated result. |
| `--restore` | Restore the most recent consolidation snapshot. |
| `--require-llm` | Fail instead of falling back to the offline SQLite/Jaccard tier. |
| `--allow-drops` | Apply even when memories would be removed without a merge. Every category is under the drop guard, so without this flag an input memory no surviving memory explains is re-added verbatim instead of deleted. **Nothing is exempt**: an explicit `obsolete` drop, a rewrite, and a `superseded by <id>` drop are all audited the same way, and a row the model disposed of comes back unless a single output memory carries at least 45% of its tokens. A merge source is audited against the text of its OWN merge, not the whole result. A memory the harness simply never named is carried through unchanged, so a rewrite is not a free change of wording — its replacement has to carry the memory's substance, or the old row survives beside it until a later `ghost resolve` or `ghost supersede` demotes it. The cost of that is a possible duplicate; the alternative is a silent deletion with nobody watching. |
| `--promote-globals` | Promote cross-project candidates into `_global`; without this flag they remain project-scoped. |
| `--skip-unchanged` | Skip the LLM call when the consolidatable set is unchanged since the last applied pass. |
| `--full` | Print the full text of every memory the run reports, instead of the 120-byte preview. Display only — the result and the write are identical either way. |
| `--source <host>` | Explicit harness: `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

The `auto` tier uses the explicit source when provided, otherwise detects the calling harness. It does not silently switch to a different harness or billing path. When a source is known but its CLI binary is unavailable, auto can fall back to SQLite; the offline tier is also available for an explicit local run.

A harness-backed consolidation is asked for operations on the memory ids it is shown — `keep <id>`, `merge <id>,<id> -> <text>`, `rewrite <id> -> <text>`, `drop <id> reason: obsolete | superseded by <id>` — rather than for a rewritten list of memories. A memory leaves the corpus only through one of those operations: named as a merge source, named for a rewrite, or named in a drop with a reason — and only where a surviving memory accounts for it, or `--allow-drops` accepts the deletion. Naming an id is not by itself enough: a `rewrite` whose replacement does not carry the old row's substance leaves that row in the corpus verbatim, so a rewrite is not a free change of wording on an unattended run. Anything the response does not name is carried through unchanged, byte for byte, so it keeps its id, its embedding, its links and its age. A merge or rewrite that introduces a path, hash, version, hostname or number found in none of the memories it names is rejected and those memories are kept as they are, which is why a `rewrite` fixes a claim and never a specific. An operation Ghost cannot read, an id it did not supply, or a response carrying no operations fails that tier's result, and consolidation falls through to the next tier. One such rejection does not end the pass by itself: the same prompt is sent once more with the reader's complaint attached, the second answer is read under the same rules (no fuzzy ids, no partial application), and a run that needed that turn reports `repair: 1` on its `Result:` line. A second rejection fails the result as described above.

The report ends with an accounting of every input id, printed the same way for a dry run and for `--apply` and before the write, so a dry run previews it exactly:

```text
Inputs (7) accounted for; every count below is ids, so they add up to it:
Merges (3):
  new <- A1B2…02, A1B2…03, A1B2…04   (48 B from 141 B)
Refused by the grounding check (0):
Rewrites (1):
  A1B2…05 -> the ledger ingests through the bastion on port 2222, never 22
Dropped (1, each audited by the drop guard):
  A1B2…06 reason: obsolete — nothing in the result carries it; the drop guard re-added 1 row verbatim
Deleted (0):
Kept verbatim: 1    Passed through (not named): 1
```

Every input id appears in exactly one line or one count, and every count is a count of ids — a merge may name any number of sources, so `Merges (3)` above is three ids folded into one row, and the numbers add up to the input total. Every id is quoted in the spelling the database holds: the parser accepts any case (`memIDKey` compares case-insensitively) and normalises only a drop's successor target, so a response that spelled an id differently would otherwise put a key on the page that looks up nothing. That quoted-id discipline is what makes the count arithmetic checkable, and the reason an input a merge consumed can no longer disappear from the report. A merge line names no successor id because the merged row does not exist until `--apply` writes it; `ghost history <id>` shows the `related_id` that names it. `Deleted` is the set of rows an apply removes, one line each with the reason, and it is counted by the replace's own reuse pass rather than by asking whether the row's text is still in the result: reuse is content-keyed and claims **one** row per emission, so two inputs holding the same bytes both have their text in the result and only one of them is still there afterwards. A row nothing carries is a loss; a row whose identical twin was reused is a deduplication, and the knowledge is still in the project either way. That is the whole of the offline SQLite tier's absorptions, which name no ids and so appeared in no bucket of any report. A line the drop guard overrode says so, rather than leaving a drop to be read as a deletion that did not happen. A `superseded by` reason names the successor it replaces, so that id is quoted on the drop line and accounted for by its own count — the accounting is per input id, not per mention.

CLI-backed maintenance runs each harness with an allowlisted environment, isolated configuration, and tools/MCP disabled; see [Harness subprocess environment](configuration.md#harness-subprocess-environment).


### `ghost resolve <project>`

Marks resolved-evidence memories so they leave ranked session injection while remaining searchable:

```bash
ghost resolve myproject
ghost resolve myproject --apply
ghost resolve myproject --reassess
ghost resolve myproject --mark a1b2c3d4,e5f6a7b8 --apply
```

| Flag | Meaning |
|---|---|
| `--apply` | Stamp `resolved_at` on confirmed memories, or on the ones `--mark` names. |
| `--reassess` | Re-judge memories that are already resolved instead of unresolved ones. |
| `--mark <ids>` | Stamp `resolved_at` on these named memories, on your say-so. Comma-separated. Not combinable with `--reassess`. |
| `--mark-file <path>` | The same list, one id or prefix per line. |
| `--only <ids>` | With `--reassess`: judge only these memories. |
| `--only-file <path>` | With `--reassess`: the same, one id or prefix per line. |
| `--source <host>` | Classify through `claude-code`, `opencode`, `codex`, or `goose`. Not used by `--mark`: nothing is classified. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

The classifier is KEEP-biased in code, not only in prose. A local keyword prefilter proposes candidates, then a deterministic veto settles a candidate as KEEP with no harness call when its text carries a standing imperative (`never`, `do not`, `don't`, `must`, `always`, `required`) or an open marker (`not yet`, `outstanding`, `still pending`, `still open`, `still stale`, `unresolved`, `todo`) — case-insensitive and word-bounded. The prompt asks whether an agent starting a fresh session would make a mistake, repeat work, or break a rule without the note, and states that a date, PR number, commit hash, or "fixed in" does not resolve one. It also resolves a note that only restates what the repository already holds — the repository is authoritative, so an agent reads the file — while keeping any note that adds a rule, a constraint, or a reason the code does not state, whatever paths it cites. A `RESOLVED` verdict must carry a `closed-by:` fact naming what made the note obsolete; a verdict without one is read as KEEP, so a note the harness cannot explain away stays injectable.

Only explicit KEEP verdicts enter the content-hash cache (`memories.resolve_kept_hash`, prefix `v3` — `v1` and `v2` entries are re-asked, since they predate the veto and the `closed-by` rule); missing or garbled verdicts are counted as UNKNOWN and retried on a later pass. It fails when no source-specific harness can be selected; it never silently falls back to another harness.

#### `--reassess`

Re-runs the vetoes and the classifier over the memories that are **already** resolved, and repairs the ones that now come back KEEP:

```bash
ghost resolve myproject --reassess           # preview: prints the list
ghost resolve myproject --reassess --apply   # clear resolved_at on those rows
```

It is how a wrong resolution gets undone — the ordinary pass never looks at a row that already carries `resolved_at`, so a rule it buried was invisible to every later pass. The repair pass skips the keyword prefilter on purpose: the pool is already the small resolved subset, and a wrongly resolved note is usually hidden by the *absence* of a resolution keyword or by a narrative that reads like a fix. With `--apply` it clears `resolved_at` so the notes return to ranked session-start injection, and records the classifier's KEEP verdicts in the content-hash cache so the ordinary pass does not ask about them again. A classify failure is fatal and repairs nothing; a clear failure is fatal too and clears nothing either, because every batch runs in one transaction. An UNKNOWN verdict leaves `resolved_at` alone and is offered again. The stop hook's lifecycle phase never passes `--reassess`: it is an operator command.

Rows that the ordinary pass would re-stamp for free are left alone and reported as **still asserted by a link or correction**: the older endpoint of a live `supersedes` link, a row an unresolved correction still pairs with, and a row whose own correction is being repaired in the same run (clearing that correction would put it back in the pool and re-assert the pairing on the next pass). Clearing any of those would print a repair that the next ordinary pass immediately undoes.

Every one of those rows is **named**, one line each, with the thing that holds it — `held by supersedes <source-id>` or `held by correction <id>` — and every holder is listed when more than one applies, because the two reasons have different remedies and only one of them has a command attached to it:

```
myproject: 3 of 143 already resolved judged, 0 KEEP vetoed, 0 KEEP cached, 1 still RESOLVED, 1 still asserted by a link or correction, 0 UNKNOWN, would clear resolved_at for 1 (1 classify call(s))
  9f2c1d4e5a6b7c8d9e0f1a2b3c4d5e6f  [changelog]  held by supersedes 1122334455667788990011223344556677  Cost estimate from May: $148/mo projected; …
```

The id named for an edge is its **source** — the newer note `ghost supersede --withdraw` takes — and all of these ids print in full, not abbreviated to eight characters, because they are operands rather than references: a repair is already in your hand and should not need a second lookup.

That hold-back is re-checked to a **fixed point inside one run**, because whether a row is held depends on which other rows the run clears: a row the run clears joins the pool the next ordinary pass reads, and holding a row takes it back out. The re-check iterates until a round finds nothing new (it is bounded, and the bound is reported), so one `--reassess --apply` reaches the answer a second identical pass used to be needed for. A dry run iterates the same way and reports the same rounds and the same held rows — it is a preview of that run, not a cheaper approximation of it.

Only rows the ordinary pass would actually consider are held back: its pairing mechanism pairs the keyword-prefiltered subset, so a durable rule with no resolution keyword is repaired even when a correction being cleared in the same run shares its subject tokens. A hold is not a soft warning — it is reported as asserted on every later run, so the filter keeps the pass from parking a row nothing asserts.

#### `--mark`

Stamps `resolved_at` on the memories you *name*, rather than on the ones a pass proposes:

```bash
ghost resolve myproject --mark a1b2c3d4            # preview
ghost resolve myproject --mark a1b2c3d4 --apply
ghost resolve myproject --mark-file stale.ids --apply
```

`--mark` is for the case no pass reaches. The ordinary pass only ever proposes a memory whose text carries a resolution keyword, and even then it asks a KEEP-biased classifier about *that note* — not about whether another memory in the project supersedes it. So a note whose claim a newer memory says was fixed is frequently invisible to every pass: it holds no keyword, so nothing proposes it. When you have read both notes and know the older one is finished, this is the supported way to say so. The previous alternatives were leaving the memory in every session's ranked context or writing `resolved_at` with SQL — and the SQL route bypasses the `memory_history` row every writer appends, so the record of how the memory reached its current state would show it resolved with nothing saying who decided.

Nothing is classified, so no harness is called, nothing is billed, and the command works on a machine that cannot spawn one at all. The refs are the ones `ghost supersede --withdraw` takes and go through the same code, not a copy of its rules: a full id, or an unambiguous prefix of at least 8 **characters** (counted in characters, not bytes, because that is how the reports abbreviate). A full id is accepted whatever its shape — `ghost import` writes an artifact's ids verbatim — and an ambiguous prefix is refused with the matches listed rather than guessed at.

What it writes is the ordinary store path, so the write is the same one the pass makes: the same eligibility guard, the same `resolve` history row, the same transaction. Two things differ, and both matter:

- The `resolve` history row records **you** as the performer. It is the only resolve row in the database that can say a person decided rather than a classifier judged, which is the whole reason this is a command rather than SQL.
- The memory's cached KEEP verdict is **dropped**. That cache is keyed by content, and a row carrying one is skipped by the ordinary pass as `N KEEP cached`; leaving it would mean a memory buried on purpose came straight back the moment anything rewrote its text.

Only a memory in the project you named is marked. A ref that reaches another project, or a `_global` row (which refs reach, because a promotion moves a row while keeping the links pointing at it), is refused. Naming `_global` as the project is refused too: it holds the promoted memories *every* project injects, so one command cannot decide for all of them. A memory that is **already resolved** is reported as a no-op rather than as a change, and nothing is written on its account — including no history row, since a history row records a write. A **pinned** memory and one in a **standing category** (`convention`, `preference`) are likewise left alone and reported, because a pin is an instruction to keep a memory visible and resolve does not overrule it.

The write is one transaction over the whole request, so a failure rolls the whole thing back and the report says none of them moved — there is no partial mark to interpret. Several ids may be given at once, and the request is settled before anything is written: one bad ref out of five marks none of them.

The eligibility guard is re-checked *where the write happens*, not only where the refs were read, so a memory pinned, recategorized or moved by something else in between is declined rather than stamped. That decline is silent — it is another process's decision, not a failure — so it is reported as its own state (**declined**). Under `--apply` the per-row markers are chosen so the success is never the default: any row state the report does not recognise reads as *not marked*, because a report that claims a change it did not make is worse than one that admits a row it did not write.

Its inverse is `--reassess --only`, and every `--mark --apply` report prints the exact command for the memories it just stamped — scoped, never the project-wide re-judge. The MCP tool `ghost_resolve_mark` does the same thing over the tool surface.

### `ghost supersede <project>`

Proposes and classifies directed replacement relationships:

```bash
ghost supersede myproject
ghost supersede myproject --apply
ghost supersede myproject --reassess
ghost supersede myproject --withdraw a1b2c3d4 e5f6a7b8 --apply
```

| Flag | Meaning |
|---|---|
| `--apply` | Write `supersedes` and `causes` links, or withdraw the edges `--withdraw` / `--reassess` name. |
| `--reassess` | Re-judge the `supersedes` links already in the graph under the current rules and withdraw the ones they no longer support. Not combinable with `--withdraw`. |
| `--withdraw <source-id> <target-id>` | Withdraw one named `supersedes` link — the edge from `source-id` (the newer note) to `target-id` (the older, buried one). Repeatable. `--source` and `--threshold` are not used: nothing is classified. |
| `--threshold <float>` | Minimum cosine similarity for a candidate pair; default `0.80`. |
| `--consensus <N>` | Classify the candidate set **N times** and write only what *all N* passes proposed, in the same direction. `N` ≥ 2; default `1`, which is no gate. A pair the passes split on is reported as "not agreed" and nothing is written for it. Costs N times the classify calls. Refused with `--reassess` and `--withdraw`. |
| `--source <host>` | Classify through `claude-code`, `opencode`, `codex`, or `goose`. |
| `--project <name>` | Project name instead of the positional form. Takes the next argument verbatim, so dash-prefixed names work. |

Each candidate is classified as `supersedes`, `causes`, `reversed`, or `neither`, with each note's creation timestamp in the prompt. A `supersedes` link only ever points from the newer note to the older one, so a `reversed` verdict — the classifier says the older note holds the current value and the newer one restates an obsolete claim — is reported and refused instead of written; `--apply` also invalidates any `supersedes`/`causes` link the pair already carries. A refused verdict is never recorded in the NEITHER cache, so the pair is not skipped on later passes. The default source is the calling harness. Applying the pass enables targeted demotion during search for genuine replacement pairs.

**A `supersedes` has to retire *every* claim the older note makes, not one of them.** The link demotes the older note as a whole and marks it resolved, so a claim nobody retired drops out of an agent's context in the same instant as the one that was. A newer note that answers some of the older note's claims and leaves the rest standing is therefore `neither` — and specifically **not** `causes`, even when it reads as an elaboration that acts on the older one, because `causes` requires the older note's content to remain independently true and a partly-retired note by definition does not. A newer note that acts on an older one and retires *nothing* is `causes`; one that retires part of it and leaves the rest is not. The `replaced:` field may name several claims, separated by semicolons, when a whole note really is retired. Four shapes were the recurring wrong edges in [#779](https://github.com/wcatz/ghost/issues/779)'s re-measurement — three dry-run passes each on copies of three real stores — and none of them is a `supersedes`:

| Shape | Example | Why it is not a replacement |
|---|---|---|
| **Partial claim** | The older note lists four things about a nightly run; a newer note fixes one of them. | The other three claims are still true, and the edge would take them out of context with the fixed one. |
| **A log entry** | "Released 4.2: the worker moved to the new queue" / "Released 4.3: the worker gained a dead-letter topic". | Each entry records something that happened and stays true, so the later one does not make the earlier one untrue. Sharing a component, a host or a date is not a shared fact. **The exception, which overrides this rule only and not the coverage rule above: an entry that reports the open issue closed.** A status entry saying the fix shipped retires the earlier entry saying the build never got past it — so a blocker note and its fix are a supersession, while two releases are not. **Narrowing is not closing:** a report that leaves the blocker partly standing, or that is a second sighting of a problem still reproducing, does not retire the earlier report and is `neither` — which is why `#641`'s `status-report-divergence` pair is labeled `neither` and only its `status-report-fix` sibling is a supersession. |
| **A recurring defect** | The same failure seen on two different days. | One still-open problem, not a bug and its fix. Only a note that says it is fixed, and fixes it, supersedes the note that reported it. |
| **Parallel investigation** | Two notes on one stall, each about a different layer. | Neither retired the other; the timestamps say only which was written last. |

That pass measured 55% precision over its 108 distinct proposals, rising to 79% for the edges proposed in all three passes and falling to 33% for those proposed in one — so the wrong edges are an **unstable** classifier as much as a wrong one, which is what the [gate below](#gate-the---apply-on-agreement-between-passes) answers. [#779](https://github.com/wcatz/ghost/issues/779) carries those numbers; the table above is the rule the prompt carries, not a measurement of it.

**Upgrading: the tightened rubric applies to NEW pairs, and only `--reassess` reaches the edges already in the graph.** The NEITHER cache's key prefix moves with the rubric, so every cached verdict is re-asked — a fresh pair is judged under the new rules on the next ordinary pass. But a `supersedes` **edge** that is already live is held quiet by `skip-if-unchanged` until one of its endpoints changes, and a passing pass does not re-judge it. So an edge written under the old rubric stays until an edit touches it, or until you run:

```bash
ghost supersede <project> --reassess              # dry run: what the current rules would withdraw
ghost supersede <project> --reassess --apply      # withdraw those, and print the resolve repair
```

That is the whole upgrade step for the rubric, and it is deliberate rather than an oversight: a wrong edge under a tightened rubric is exactly the case no ordinary pass will look at again, which is the reason the repair path exists. The cache clearing is not the same thing and does not reach edges — a cached verdict is a decision about a pair the graph never linked.

One limit of the new rule is worth knowing, because the parser cannot check it: a `supersedes` answer must name a retired claim, and nothing verifies that it named *every* one. `replaced: the first claim; tbd` is accepted — the check is whether a claim is named, not whether all of them are. The coverage requirement is the model's to follow.
### Gate the `--apply` on agreement between passes

`ghost supersede <project> --apply --consensus 3` classifies the candidate set **three times** and writes only the edges all three passes proposed, in the same direction. A pair the passes split on is reported as **not agreed**, with its count and its ids, and nothing at all is written for it.

```
projy: 6 candidate pairs in 3 classify call(s), 0 cached, 1 supersedes, 0 causes, 0 reclassified, would link
  consensus 3: 18 pair(s) asked, and only what all 3 passes proposed was would link
  2 pair(s) not agreed: the classification passes split, so no edge was written and none was cached — re-run to ask again (fresh passes may agree), or drop --consensus to write the first pass's answer; raising it makes unanimity harder, not easier
  4b1c9e2a -> f0a3d5c7  [not agreed: 2 supersedes, 1 neither]
  91ee6b04 -> 2a7c1f88  [not agreed: 1 causes, 1 reversed, 1 unreadable]
```

**Unanimity, not a majority.** A 2-of-3 majority would write exactly the 0.56 row of that measurement. A pair the model read in one pass and answered differently in another is a pair whose verdict it has not settled, and the gate writes only settled ones.

A not-agreed tally reads in **descending vote count**, so the line leads with the majority — a split of one `supersedes` against two `neither` prints as `2 neither, 1 supersedes`, not the other way round — and a tie falls back to the fixed verdict order (`supersedes`, `causes`, `neither`, `reversed`, `unreadable`) so the same line comes out the same way on every run.

**Raising `--consensus` is not a remedy.** A split at N=3 has to satisfy one *more* pass at N=4, so a larger N makes unanimity strictly harder and can only suppress more pairs. The two things that help are a re-run — fresh passes may land on the same answer — and dropping the flag (or changing `--source`) to write what the first pass said.

Three things are worth knowing before you turn it on:

- **The gate is off unless you type it.** The default is one pass, so a hand-run pass is never silently tripled, and an existing script's bill does not change.
- **It does not multiply the pairs that were already going to be skipped.** A pair `skip-if-unchanged` or the NEITHER cache would have skipped is skipped **once**, and an unchanged live edge is not re-asked at all — so on a converged project the gate costs almost nothing, and the multiplier falls on the pairs that were going to be asked anyway. `consensus N: N × pairs asked` on the summary is the number to check the bill against.
- **A split is not a withdrawal, and not a cache row.** Nothing is written, nothing is withdrawn, and the pair is not cached — so it is asked again next pass rather than frozen on a verdict nobody reached. A live edge on such a pair is untouched and can still be withdrawn later, on a quorum.

`--consensus` is refused with `--reassess` and `--withdraw`: those judge edges the graph **already holds**, and a gate that refused to withdraw an edge the passes happened to split on would disable the repair that exists to do it.

`auto_supersede: true` runs the automatic phase through this gate by default, with the pass count from `reflection.supersede_consensus` (default 3, read only when `auto_supersede` is true). `auto_supersede` itself remains **off by default**.

**The automatic phase's wall time scales with N and its deadline does not.** `lifecycle_timeout_minutes` still bounds the whole supersede phase, unchanged, and it is a hang detector rather than a work budget — so enabling the gate at 3 multiplies the work against the same 60 minutes, and on a large project you raise the bound or set the count to 2. A deadline that expires under `--apply` can land `SIGTERM` inside the apply block, whose per-pair writes are separate transactions, so the phase ends with a **partial write**, **no report at all** and a `lifecycle-last-failure` marker the next session-start turns into an alert. Nothing is corrupted and a re-run converges; the point is that there is no partial report to read, which is the reason to raise the bound rather than to rely on the retry. `ghost supersede <project> --apply --consensus N` run by hand has no such bound and prints its report as it finishes.





A pair that already carried a live `supersedes` link is a **reclassification**, and a verdict other than `supersedes` on it is a withdrawal of that link — a real graph change, made through the same write that leaves the `unsupersede` history row. So it is reported per edge, in the same shape `--reassess` reports its withdrawals:

```
projy: 3 candidate pairs in 1 classify call(s), 0 cached, 1 supersedes, 1 causes, 2 reclassified, linked
  4b1c9e2a  supersedes  f0a3d5c7
  91ee6b04  causes  4b1c9e2a
  would withdraw  7d2e8f10 -> 1c5b9a36  [neither: both notes are still true]
```

The row names both ids, the verdict that withdrew the edge, and what the run did to it. Five markers, each a claim about the graph and only ever the one this run earned: `would withdraw` in a dry run, `withdrew` only where the write landed, `kept` where the edge the row is about survived and the run removed rows of the *other* relation instead, `already gone` where a concurrent pass had already removed it, and `not written` where the verdict was reached and the write refused. `kept` is a statement rather than a euphemism, and it exists because the row is reached whenever *any* row of the pair moved: a `supersedes` edge re-affirmed beside a `causes` cycle has its own edge still in the graph, and calling that `already gone` described an edge that never left.

A `causes` verdict says so on the same row, because it re-links the pair as well as dropping the edge — and so does a `supersedes` verdict on a pair that was linked as `causes`, which is the same change of relation read the other way round. The two clauses on a reclassified row are **independent**, not alternatives, because one verdict can do both: a `causes` cycle answered `causes` keeps the direction it was asked about *and* drops the edge asserting the other. The dropped clause is counted from what the run **moved** under `--apply` (`[+1 causes edge dropped]`) and from what it **read** in a dry run (`[+1 causes edge would be dropped]`), because a count only an applied run can reach is a count the preview does not preview — and the tense is the whole difference, since the dry run performs no deletion at all. The verb in the re-link clause follows the same reading: a changed relation is `re-linked`, an unchanged one is `re-affirmed`, and an unchanged relation *whose own edge moved* is `re-linked` too, because the edge the verdict wrote is a different edge from the one the row was about.

A dry run that would withdraw something says so and tells you to re-run with `--apply`; an `--apply` run that withdrew a `supersedes` edge prints **the same follow-up the two repair modes print**, scoped to the withdrawn targets' ids, because the withdrawal is only half the repair and the `resolved_at` the edge caused keeps the target out of ranked injection until that runs. A withdrawn `causes` edge prints **no** follow-up: resolve's supersedes piggyback acts on `supersedes`/`llm` edges alone, so no `causes` edge ever stamped the `resolved_at` that repair clears, and naming its target would send you to clear a memory nothing is holding down. A `neither` on a pair the graph never linked withdraws nothing and is not reported as a withdrawal: no edge ever justified a resolution for it.

The unit the pass judges is the **pair**, and it is that unit because a demotion lands on the target alone: two `supersedes` edges in opposite directions demote *both* memories of one pair and neither withdraws the other. A live `supersedes` edge therefore decides which way round its pair is judged, so one pass can never carry one pair both ways round and write the cycle — and neither can two passes, because the store re-checks the pair's other direction inside the transaction that writes the edge. That is the relation whose edge IS the demotion, so the direction to judge it in is the direction it was written in, and a `reversed` answer is how the model declines a backwards claim.

A live `causes` edge gets two of those three guarantees and deliberately not the third ([#823](https://github.com/wcatz/ghost/issues/823)). **skip-if-unchanged holds it**: an edge whose endpoints have not moved since it was judged is not re-asked. And a pair a `causes` edge names is **never cache-skipped**, because a cache skip is treated as a `neither` verdict and would assert the edge is not there. Together those are the whole of the re-bill fix: before this, a `causes` edge whose direction disagreed with the timestamps was re-proposed every pass, paid a classify call for the re-ask, and answered `causes` again, so the pass wrote the *other* direction and left the pair live in both.

**A `causes` edge's direction does not decide the question, and the difference is the one that matters.** `causes` is written older→newer, so a pair whose edge runs against the timestamps is asked in the direction the **timestamps** give, and a `causes` verdict writes that direction — dropping the edge it contradicts, so the pair converges on one edge in a single pass. The first cut of this did the opposite and it was a blocker: read through the edge's own direction, a live `causes` September→January edge asks "does the January note supersede the September one", the model answers yes, and the pass writes `supersedes January→September` — an edge that **demotes the memory that is actually current** and buries it, which is [#641](https://github.com/wcatz/ghost/issues/641)'s harm produced by the pass rather than found by it. A direction that buries a memory belongs to the relation that demotes. So the two relations are asymmetric on purpose, and the `proposed the reverse` count is `supersedes`-only: a `causes` edge can no longer be what a proposal opposed, because it is never proposed against. A live `causes` edge whose direction disagrees with the timestamps is still settled, and still converges — it is just settled by the verdict instead of by the prompt.

A pair whose two `causes` edges disagree with each other is a **cycle** and is **not** refused, which is the one place the two relations behave differently and the reason is the harm. A `supersedes` cycle is refused (`refused: a supersedes link is already live in BOTH directions`), because both its edges demote one of the pair's two memories and no orientation of it can be judged into a state worth keeping. Nothing demotes on a `causes` edge, and `ghost supersede --reassess` loads live `supersedes`/`llm` edges only — so a refused `causes` cycle would be a contradiction in the graph with a repair line pointing at a command that cannot see it. Such a pair is judged once in the direction the timestamps give it — the same rule every other `causes`-only pair follows, so a cycle is not a special case of anything — and the verdict converges it: an affirming verdict writes that direction and drops the other, a denying one drops both.

The report says so in five lines, one per reason a pair was not judged or not acted on, and every count on them is per pass:

| Report line | What it means |
|---|---|
| `N pair(s) vetoed` | The older note states a standing rule the newer one never names as retired, so no harness call was spent, no link written and nothing cached. Settled for free and re-decided for free on a later pass. |
| `N pair(s) not proposed` | Both notes carry the same `updated_at` **and** the same `created_at` — a bulk import stamps a whole batch at once — so there is no chronology to order them by. No call, no link, not cached. A live **`supersedes`** link on such a pair is still re-judged, because that edge carries its own direction. A pair whose only live link is a **`causes`** one is the shape that is not, and it cannot be otherwise: the question is asked by the timestamps, and two rows sharing both of them cannot answer it. The count is per **pair**, so a pair the scan already found as a near neighbour and then declined is not counted again when its live `causes` edge reaches the reconciliation — the two halves of one pass both refuse it, and the line reads pairs. |
| `N pair(s) proposed the reverse of a live supersedes link` | The scan's timestamps ordered a pair the graph already asserts the other way round. The reverse orientation was refused and the pair keeps the link's direction; it is re-judged only if an endpoint changed since the link was last confirmed. A proposal that *agrees* with a live link is not counted on this line and buys nothing: the link already describes that pair, so it faces the same endpoint-changed test. **`supersedes` only** — a `causes` edge no longer decides the question, so it is never proposed against. Its own mismatches are settled by the verdict instead, and show up as a reclassified row. |
| `N pair(s) refused: a supersedes link is already live in BOTH directions` | A cycle a pass before #778 could write. Each of its two edges demotes the endpoint the other promotes, so the pass judges nothing, writes nothing and withdraws nothing here, and prints the command that settles it — `ghost supersede <project> --reassess --apply`, with the real project name in it. |
| `N pair(s) not written` | Two `ghost supersede --apply` passes over one project each found a pair unclaimed, scanned it in **opposite** directions — an endpoint's `updated_at` moved between the two scans — and both reached a verdict. The store re-checks the pair's other direction inside the transaction that writes the edge, so the second write is refused and the graph holds one direction rather than a cycle. **Either relation**: for `supersedes` the cycle it prevents demotes both endpoints, and for `causes` it prevents a pair asserting that each note caused the other. This run wrote nothing for those pairs, and the next pass judges them in the direction the live edge asserts. On a `supersedes` pair, `ghost supersede <project> --reassess --apply` settles it if the two passes disagree about which note is current; it loads live `supersedes` edges only, so on a `causes` pair the next ordinary pass is the whole of the repair. |

Each line says what the pass **decided**, not what it judged. The first four counts are taken before the filters that spend a call; the fifth is taken at the write, and it is the only one that can happen to a pair the classifier already answered. A pair whose live edge is unchanged is held quiet by skip-if-unchanged whatever the line above it says — and the stamp that test reads moves when a verdict re-confirms the edge — to the freshness of the text that verdict was made against, not to the moment the edge was written, so an edit made while the classifier was running still buys the next call — and so an edge costs one classifier call per **endpoint edit** and nothing at all on a pass where no endpoint moved. That is the whole difference between a converged project costing nothing per pass and costing a harness call per batch of eight over every edge in it, and it is why the ordinary pass is not a second way to withdraw an edge: verdicts on one pair are not stable run to run, so a pass that re-asked an untouched edge would drop a correct one and re-create it on the next, spending two history rows on the target each time it flipped.

Candidates come from the stored vectors, and a memory with no vector is proposed as no new candidate by that scan. Those vectors are normally written by the embedding worker inside `ghost mcp` — another process, on its own schedule, running only if a server is up — so a pass started right after a save would otherwise propose nothing and report `0 candidate pairs` for a pair that was in front of the operator. The pass therefore embeds the project's own unembedded memories (up to 50, and only when embedding is enabled) before it scans, and reports how many it had to write. A pass that still could not read part of the project says so beside it, counting what it did not see rather than what the embed did not manage to write — so a bound reached, a budget that ran out, an endpoint that did not answer and a vector left over from another model all read as the one honest fact they are. A pass that stops on its own budget says so and says to re-run it, because the batch continues there; the other three point at the embedding worker instead, and a pass with `embedding.enabled: false` points at nothing, since no worker is running to be behind. That count is about the **new** candidates only: an edge already in the graph is re-judged without the scan having to find it again — from the link row alone when the scan cannot see the pair at all, or from the scan's own candidate when it proposes that same pair — so a `N reclassified` printed beside the line is not bounded by it. The bound is what keeps a single pass from embedding a whole corpus: this is a pass making the corpus it is about to read searchable, not a backfill, and the rest is the embedding worker's sweep. `--reassess` and `--withdraw` read no vectors and do none of it.

`--reassess` settles a cycle when it can, and hands it back when it cannot. A pair live in **both** directions is judged once, as one pair, in the direction the timestamps give it: a `supersedes` answer names one of the two live edges as current, so that edge stands and its reverse is withdrawn; a `reversed` answer names the other one, so the other stands; `neither` and `causes` deny the replacement in either direction and **both** edges are withdrawn. The report prints a `cycle:` block for each one, naming both edges and which of them stands, in the same tense as the rows above it — `would withdraw` in a dry run, `withdrew` only where the write landed, `already gone` where a concurrent pass took the edge first. A headline count of `withdrew 0` over a pair whose two edges are both still live is the one line a repair report must not leave standing on its own.

Two outcomes decide **nothing**, and they are reported differently because your next step differs. A **missing verdict** — the classify call failed, or its reply could not be read — means the pass asked and got nothing: no edge of the pair moved, and the answer is to **re-run the pass**. For a failed call the run says so itself, in its non-zero exit and its `unjudged` rows; for a garbled reply it does not — an unreadable pair is counted in the summary's `UNKNOWN` total, leaves the run exiting 0, and the cycle block is the only line that says the next pass re-asks it. A pair whose notes **share both timestamps** means there was no direction to ask about and the pass did not ask: that one a re-run cannot change, so the block prints the two `ghost supersede <project> --withdraw <source-id> <target-id> --apply` commands instead, because withdrawing one edge is the operator's own call — no model, nothing billed. An id beginning with a dash cannot be named from the CLI at all (the argument parser reads it as a flag), so for such an edge the block names the ids and points at `ghost_link_withdraw`, which parses no flags, with all three of that tool's parameters — `project_id`, `source_id` and `target_id` — since it refuses a call missing any of them.

The block's own line about the pair states the **decision**, never a write, because whether a write happened is per edge and is what the two markers above it say: `would withdraw` in a dry run, `withdrew` where the write landed, `already gone` where a concurrent pass took the edge first, and `not reached … STILL LIVE` where an earlier write failed and this one was never attempted.

A classify call that dies is **per call**, not per pass. A project's live edges are judged eight pairs to a harness call, so an 87-edge project is eleven calls, and a transport failure in the seventh used to throw the other ten away: every pair moved to `unjudged`, and the re-run paid for all eleven again. The chunks that answered are now applied like any other verdicts — those are complete, parsed, number-indexed answers, and nothing downstream needs the whole set at once, since a cycle is asked as one question and lives in one chunk — so the `unjudged` rows name only the pairs the failed call carried, and the non-zero exit still asks for the re-run. A cycle the failed call carried is reported as a **missing verdict** (the first outcome above), never as one that stood: no call answered about it, so both its edges are still live and the block says so. A reply whose verdict count does not match the pairs asked about is not a partial answer — the mapping from reply to pair is exactly what is in doubt — so nothing is applied there and every open pair is `unjudged`.

`--withdraw` is the repair for an edge the rules still accept. `--reassess` withdraws what the current rubric rejects, so a pair that is wrong for a reason no rubric can see — the newer note is not a replacement of the older one at all — keeps its edge and buries its target, because a classifier asked about it has not made a mistake by its own lights. `--withdraw` removes the edge you name, on your say-so, with no harness call and nothing billed. All three withdrawal paths are dry-run by default and all three write the `unsupersede` history row: the ordinary pass withdraws an edge it re-judges, and prints the same two things the two repair modes print — the per-edge row, and under `--apply` the follow-up. That follow-up is a **scoped** `ghost resolve <project> --reassess --only <the withdrawn edges' target ids> --apply`, with the same id list written as a `--only-file` beside it (an id holding a comma is not nameable by `--only`, which splits on commas, so it is carried by the file alone and the report says how many such ids there were; an id holding a newline is reachable through neither form, and the report says that memory stays resolved until the row is rewritten; a `#` on an `--only-file` line is a comment only when it follows whitespace, so an annotated `<id>   # note` still works while `<id>#note` is one id — it never prints the unscoped repair, which would re-judge the whole project) — because the withdrawal is only half the repair and the `resolved_at` the edge caused keeps the target out of ranked injection until that runs. Prefer the scoped form: an unscoped repair re-judges every resolved memory in the project.

A ref is a full memory id, or an unambiguous **8-or-more-character prefix** of one, because every Ghost report abbreviates to eight characters. A full id is accepted whatever its shape — `ghost import` writes an artifact's ids verbatim, and the id column only *defaults* to hex — while the 8-character floor applies to a prefix, which names a class of ids rather than one. A prefix naming two memories is refused with the matches listed rather than guessed at. A pair with no live `supersedes` link is an error, and the message names the target's live edges. Several pairs may be given at once, and the whole request is settled before anything is written — one bad pair out of five withdraws none of them.

The edge belongs to the named project if **either** endpoint does, and `_global` belongs to every project:

- **`_global` is the shared scope.** `ghost_memory_promote` and `ghost reflect --promote-globals` move a memory into `_global` and keep its links, so a live edge is left with one endpoint in the project and one in the shared scope. The project that owns the memory the edge **buries** can name that edge and withdraw it, and `_global` can too — otherwise a promotion would silently remove the operator-facing undo for a wrong edge, and the note would be demoted and `resolved_at`-stamped with no command able to name the edge that did either. Refs resolve to `_global` from every project, which is the same rule, and a `ghost supersede _global --withdraw` resolves its refs without a project predicate at all so both endpoints of such a pair are nameable.
- **A project is not.** An edge with both endpoints in another project is that project's edge, and neither withdrawable nor discoverable from here. A source in another project is reachable in exactly one way: the target is resolved first, so the live edges already known to hold it are in scope, and a source ref that names one of them resolves. That is the whole of the widening, and it is deliberately that narrow — the extra ids come from a read you can already see, so the command cannot be used to ask what else exists in a neighbour's corpus, and an ambiguous or too-short ref is still refused rather than answered out of it. Everything else stays project-scoped, which is what keeps a ref from resolving into a project you did not name.

`SupersedePenalties` carries **no** project predicate at all, so the ranking demotes a target for *any* live edge, and every project-scoped surface sees a subset of those. Before #786 the two answers did not even overlap on a promoted edge: the ranking demoted a target on a claim no command could name, and resolve's floor had already released the resolution the demotion was still justifying. What each surface now covers, stated exactly:

| Surface | Reads | Agrees with the ranking on |
|---|---|---|
| `ghost supersede <project> --reassess`, the ordinary pass, resolve's supersedes piggyback, its floor | edges with **both** endpoints in the project or `_global` | every edge with both endpoints there |
| `--withdraw`, `ghost_link_withdraw` | edges with **either** endpoint there | every edge with either endpoint there |

The passes need **both** because they judge a pair and then write links on it, so judging an edge acts on both of its endpoints — a pass that loaded an edge whose target belonged to a third project would be writing on a graph it was not run against. A targeted withdrawal needs only **either**, because the caller has already named the memory being un-buried and its project may withdraw whatever buries it.

The one shape neither row covers is an edge with an endpoint in another project and **neither** endpoint here: the ranking counts it for you, but it is that project's pair and no pass of yours judges it. An edge whose *target* is yours is covered by the second row even when its source is not, which is what the target-first resolution order buys.

The follow-up is scoped to the project that **owns the target**, not the project the command was named with. They are the same project for an edge of your own, and different exactly for `ghost supersede _global --withdraw` over a target in a project — where `ghost resolve _global --reassess` would resolve the selector and then find nothing in its pool, and every id would come back a miss under a block promising a clear.

The withdrawal is a soft invalidation: a later pass that still judges the pair a supersession re-creates it. The MCP tool `ghost_link_withdraw` does the same thing over the tool surface.
## Retention tiers

Every memory carries a retention tier: how long it is wanted, and what may be done to it. It is set on save, and it is the only thing in Ghost that can take a memory away on its own.

| Tier | Meaning | Expiry | Exempt from |
|---|---|---|---|
| `session` | True of the current conversation — an observation that is about now. | Derived on save, 24 hours out (`memories.expires_at`). | nothing |
| `project` | The default, and what every memory written before schema v19 reads as after migration. Persists until resolved or deleted. | none | nothing |
| `persistent` | User-declared keep-forever. | none | `ghost reflect`, `ghost resolve`, `ghost supersede`, `ghost prune`, and the ranking demotions those passes cause |

The exemption is one decision asked in every place it matters. A `persistent` row is excluded from the consolidation's snapshot and its replaceable set (so a consolidation never saw its text and cannot re-emit it, and a restore has no copy to put back), from the resolve and repair candidate queries and from `resolved_at` stamping, from supersede candidate pairs — before the classify call, so the verdict is never paid for — and from the two ranking demotions a `supersedes` or near-duplicate edge causes, which is what stops a protection from being lost one pass after it was declared. A persistent row is still searchable, still injected, and still deletable by `ghost_memory_delete` or `ghost project delete`: the tier keeps a memory away from the automated passes, not from you.

A near-duplicate save **raises** the surviving row's tier and never lowers it, so a `retention: persistent` save protects the row a later consolidation would absorb rather than only the copy it just stored — and a `session`-tier restatement of somebody else's durable memory cannot schedule that memory for deletion. The save result says which row carries the protection.

A `session` row is never removed automatically. It is a candidate for `ghost prune` and nothing else.

A tier is set by a save (`ghost_memory_save`, `ghost_save_global`) and there is no update path: restating a memory with `retention: persistent` folds into the existing row and raises it, which is how a memory that is already stored becomes keep-forever. A save that finds no near-duplicate stores its own new row, so restating a memory in substantially different words creates a second row rather than retiering the first.

### `ghost prune`

```bash
ghost prune                                     # report: dry run, writes nothing
ghost prune --apply                             # remove the rows it would have
ghost prune --project myproject --grace 168h --apply
```

| Flag | Meaning |
|---|---|
| `--project <name-or-id>` | Only this project. Default: every project, and the report says so. |
| `--grace <duration>` | How long past expiry an untouched row is left alone. Go duration (`168h`, `30m`); `7d` is not a unit, `0` is refused, and the default is `168h`. |
| `--apply` | Remove the rows instead of only reporting them. |

A row is a candidate only if **all** of these hold: its tier is `session`, it is not pinned, its derived expiry has passed, and nothing has touched it for the grace period. The activity a prune measures is `COALESCE(last_accessed, max(updated_at, created_at, expires_at))` — a recorded read is preferred, and nothing in Ghost records one today, so in practice the grace runs from the newest of the row's own stamps: its last write, or the expiry a fold refreshed when somebody restated the fact. An untouched session note is therefore wanted for a full `SessionTTL` after its last write and spared for another week after that, and a note somebody has just restated is not eligible at all until that fresh TTL is up. A `project` or `persistent` row is never a candidate however old it is, and neither is a pinned row — a pin is an explicit "keep this where it is", and the session promise was made for rows nobody overrode.

A store-wide prune (no `--project`) reaches `_global` too, and deliberately: `ghost_save_global` takes `retention`, so a global memory saved as `session` already carries an expiry, and sparing the project would leave that row in the store past an expiry nothing could honour. The report names every row it touches, and `--project <name-or-id>` is the way to keep the blast radius to one project.

The dry run is a read-only preview built from the same query every apply batch re-runs — same predicate, scope, and order — so it cannot describe rows the apply would not have selected, and it writes nothing at all. With `--apply` the removals go out in bounded batches of 500 rows per transaction, each batch with its own `delete` tombstones in `memory_history`, so a backlog never holds the store's write lock open for the whole cleanup; a batch that fails rolls back whole and the report names the rows that actually landed. The tombstone is what makes `ghost history <id>` still report what was lost, and with what text.

**Nothing in Ghost runs this for you.** No lifecycle pass, no hook, no scheduler calls it. A prune removes memories, so it happens when a person asks for it, and the default run asks first.

Two consequences worth knowing, both the same reason: a tier is a claim about a memory's owner rather than part of its text, and nothing that replays text carries one. `ghost export` / `ghost import` do not carry it, so a memory that made the round trip comes back as `project` — consolidatable, and never pruned. And a memory a consolidation WRITES is a `project` row: merging three facts into one does not make the result keep-forever, because a tier the model never chose is not a protection. `ghost reflect --restore` revives a deleted memory as `project` for the same reason — neither the snapshot nor the change log records a tier, so there is nothing to restore one from.

## Project operations

### `ghost project delete <name-or-id>`

Permanently removes a project and all of its child records. The command always previews the deletion first:

```bash
ghost project delete myproject
ghost project delete myproject --apply
```

The second form requires the project name to be retyped at the prompt. It is irreversible and refuses to delete `_global`.

### `ghost project merge <old> <new>`

Moves all records from the old project into the surviving project while preserving memory IDs, links, and pin state:

```bash
ghost project merge old-name new-name
```

Both arguments accept a project name, ID, path-prefix match, or basename match. A basename match is accepted only when exactly one candidate survives the recorded-path and repository-remote checks; an ambiguous match is rejected rather than guessed. The command refuses to merge a project into itself.

The source project's recorded checkout and repository do not survive it: the survivor keeps its own, and the source row is deleted. A merge is therefore how two records of one project become one, not how two checkouts are joined — `ghost project bind` is what records a checkout, and a project that already records a repository refuses to take on another. Joining a checkout to a project that was refusing it is a merge *and then* a bind, in that order, and the bind is refused until the merge has deleted the project that recorded the directory.

### `ghost project bind <project-id> <checkout-directory>`

Gives a project a recorded checkout, so a session in that directory resolves it:

```bash
ghost project bind infrastructure /home/wayne/git/infrastructure
```

The first argument is an existing project **id** — not a name or a path. A project that records no usable location cannot be resolved from a directory, so it gets no session-start context and no Stop-hook lifecycle work; this is the command that repairs that, and `ghost mcp status` lists every project that needs it. Pass `-h` or `--help` for this section.

It is also the repair for a checkout that has moved or been deleted, which `ghost mcp status` does *not* report: the status notice tests the recorded path's shape, not whether the directory still exists, so a moved checkout needs this command with its new path.

The directory is made absolute and cleaned, must exist and be a directory, and is stored as its **physical** path — symlinks resolved, because that is the directory a session reports. Ghost also records the checkout's Git remote when the project records none, so a second worktree of the same repository resolves to the same project.

The command refuses, writing nothing, when:

| Refusal | Reason |
|---|---|
| the project is `_global` | it holds every project's memories, not a checkout |
| the project id does not exist | bind never guesses which project was meant |
| the path is missing, is not a directory, or is the filesystem root | a path that cannot be compared against a session directory would never resolve |
| another project already records that directory | two projects on one checkout leave a session there resolving to whichever row ranked higher; the same directory reached through a symlink counts as already recorded |
| the path contains another project's checkout | a recorded path matches by prefix, so binding `~/git` while a project records `~/git/infra` would hand that project every unregistered clone beneath it |
| the path is inside another project's checkout, and that project has no remote | that project answers for every directory beneath it, clones of unrelated repositories included, so nesting a second project there entrenches an overlap only a repository identity resolves. Give the enclosing project a remote and the same bind succeeds: a session in a different repository then contradicts it, so a nested checkout — a submodule, a vendored repository — is legitimately its own project |
| path resolution could never match the path | resolution only considers recorded paths longer than ten characters, and refuses a tie for the longest match, so binding one would record a project no session can find |
| another project already records the detected remote | one repository is one project (git-ssh and https spellings normalize to the same remote) |
| the project already belongs to a different remote | merging is the repair; rebinding is not |

Binding the same project to the same directory again succeeds and changes nothing, so the command printed by `ghost mcp status` is safe to re-run. "The same directory" is compared as text, not as location: a row some earlier writer left as `/x/checkout/` is invisible to path resolution — the candidate query matches stored text, and that spelling is neither equal to a session's `/x/checkout` nor a prefix of it — so bind rewrites it to the spelling resolution can return rather than deciding the two are the same location. Such a project is not in the unbound notice either, so this is the only repair.

A successful bind that records a directory is followed by one more step **on the standalone, init-managed Claude Code integration**: run `ghost mcp init` to write that checkout's memory redirect. The installer only writes redirects for projects with an absolute path, and `ghost mcp status` counts every project that has one — looking under the path currently recorded — so until init runs again the redirect check reports the repair as incomplete. That includes **re-pointing** a project at a moved checkout: the redirect on disk is under the old directory, so status goes red exactly as it does after a first bind. A re-run that records the same directory again changes nothing and needs no step. This applies to neither of the other setups: the ghost Claude Code plugin manages its own wiring (its `mcp init` returns early writing nothing, and `mcp status` returns before the redirect check), and the opencode, codex and goose installers have no redirect at all.

One case needs action before init will write anything: `writeRedirects` decides by file content, not by who wrote the file. It skips a `MEMORY.md` that does not contain the `stored in Ghost` marker, and rewrites one that does when it still carries the stale `ghost_list_projects` tool-call marker — so a checkout whose own `MEMORY.md` still holds an old Ghost redirect can be replaced, while one that merely mentions Ghost is left alone. Merge or remove such a file first. `mcp status` counts a skipped project as not redirected and prints the same line as any other failure, so the bind output states the condition init uses rather than promising a redirect that will not appear. The bind output names the installation the step belongs to as well.

## Obsidian

### `ghost obsidian export`

Writes a one-way Markdown vault:

```bash
ghost obsidian export
ghost obsidian export --out ~/Documents/GhostVault
ghost obsidian export --project myproject --out ~/Documents/GhostVault
```

| Flag | Meaning |
|---|---|
| `--out <path>` | Vault directory. Defaults to `obsidian.vault_dir` or `~/Documents/GhostVault`. |
| `--project <name>` | Mirror one project plus global records. |

### `ghost obsidian sync`

Keeps the vault current by polling the database:

```bash
ghost obsidian sync --interval 30s
```

| Flag | Meaning |
|---|---|
| `--out <path>` | Vault directory. |
| `--project <name>` | Mirror one project plus global records. |
| `--interval <duration>` | Positive Go duration; defaults to `obsidian.interval` or `30s`. |

The sync opens the database read-only and is safe alongside a live MCP server. Press `Ctrl-C` to stop it.

## Backup, export and import

The memory database is a plain SQLite file, and these three commands are the supported way to move it around. `backup` and `export` answer different questions — a **backup** is a restorable copy of the database; an **export** is an inspectable artifact you can read, edit and diff — and neither is built on the other.

### `ghost backup`

Writes a consistent snapshot of the live database, safe to run while the MCP server is using it:

```bash
ghost backup
ghost backup --out ~/backups/ghost.db.snapshot
```

| Flag | Meaning |
|---|---|
| `--out <path>` | Where to write the snapshot. Defaults to a timestamped file beside the database in the data directory. The path must not already exist. |

The snapshot is taken with SQLite's `VACUUM INTO`, which reads one consistent snapshot of a live WAL database. Copying the file by hand cannot: copying `ghost.db` without its `-wal` loses whatever the write-ahead log held, and copying both while a write lands can capture a torn state. The write lock is held for the length of the vacuum and no longer.

The command prints the path, the file size, and the row count of each table it counted, so a restore can be checked against what the file actually contains:

```text
backed up ~/.local/share/ghost/ghost.db.backup-20260926T153207Z (204800 bytes)
  manifest:     ~/.local/share/ghost/ghost.db.backup-20260926T153207Z.manifest.json
  projects:     1
  memories:     4
  memory_links: 1
  tasks:        1
  decisions:    1
```

The file is created at `0600` **before** a byte of the snapshot is written to it, and the path is claimed with `O_EXCL`. Two things follow, both of which matter for an `--out` outside the `0700` data directory:

- **No window.** `VACUUM INTO` names no mode for the file it creates, so a copy it creates lands at SQLite's default minus the umask — `0644` under a permissive umask. Creating the file first means a full copy of the memory database is never group- or world-readable, not even briefly. SQLite accepts an existing *empty* file as a `VACUUM INTO` destination and keeps its mode; it refuses a non-empty one, which is what makes the emptiness the reservation guarantees into the thing SQLite checks.
- **No overwrite, and no symlink.** `O_EXCL` is the atomic claim, so there is no gap between "I looked and it was absent" and "I created it" for another writer or a symlink swap to slip into, and a dangling symlink at the destination is refused rather than written *through* to whatever it points at.

A vacuum that fails removes its own reservation, so a retry is not blocked by an empty leftover this run created.

To restore: run [`ghost backup verify`](#ghost-backup-verify) on the copy first, then stop Ghost, remove `ghost.db`, `ghost.db-wal` and `ghost.db-shm` from the data directory, move the snapshot in as `ghost.db`, and start Ghost again. Ghost migrates the restored file on the next open, taking a pre-migration copy first.

#### The sidecar manifest

Every backup also writes `<snapshot>.manifest.json` beside the snapshot, and the report names it:

```json
{
  "manifest_version": 1,
  "schema_version": 18,
  "created_at": "2026-09-26T15:32:07Z",
  "database": "ghost.db.backup-20260926T153207Z",
  "bytes": 204800,
  "sha256": "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
  "counts": {
    "projects": 1,
    "memories": 4,
    "memory_links": 1,
    "tasks": 1,
    "decisions": 1
  }
}
```

`manifest_version` is the shape of this document and moves only when a field changes meaning; `schema_version` is the database schema the snapshot was written at, and is what a restore is checked against. `counts` are the same five numbers the report printed, counted from the snapshot, so the two can never disagree about what a backup holds.

`bytes` and `sha256` are the point of the file. A copy that opens is not much of a promise — SQLite will open a file whose header was never finalised, and every row count will read correctly out of one. The hash is the only thing that notices a change to bytes the database does not read, which is why it is recorded at backup time rather than computed on demand.

The manifest is at `0600` before a byte of it is written, like the snapshot: it is a full description of the memory database, and a description is not something to leave at the width a create-then-chmod would pass through. The mode is set with an explicit `chmod` as well as through the open, because the open mode applies only to a file being *created* — and a manifest is replaced rather than refused, so the replacing path is the one where it would otherwise keep whatever width something else gave it. The path is classified with an `Lstat` before the create, which the snapshot beside it also does, and enforced with `O_NOFOLLOW` on the open. Both, because replacing means no `O_EXCL`: the snapshot gets atomicity for free — `O_EXCL` is the claim, so its `Lstat` and its create are one step — whereas a link planted in the window between this `Lstat` and this open would otherwise be written *through*, and its target truncated, overwritten and narrowed. On Windows there is no `O_NOFOLLOW` to enforce it and the `Lstat` is the whole of the defence, so that window is not closed there; creating a symlink on Windows needs a privilege it does not have on unix, which is a reason the exposure is smaller and not a fix.

Unlike the snapshot, the manifest *is* replaced rather than refused. It is derived from the snapshot beside it, so a manifest found at that path belongs to an earlier backup of a file that has since been deleted. Nothing is lost: the snapshot it described is gone, and it is the snapshot, not its manifest, that a backup is.

A **pre-migration** copy — the `<db>.pre-migrate-<unix>` file an upgrade takes before touching the schema — is written with **no** manifest. `ghost backup verify` still checks one, and says so; see below.

### `ghost backup verify`

Checks a backup before you restore it, and exits non-zero if the file cannot be vouched for:

```bash
ghost backup verify ~/.local/share/ghost/ghost.db.backup-20260926T153207Z
```

It reads the file it is given and the manifest beside it, and it does **not** open the database in your data directory — so running it can neither migrate that store nor seed its builtin rows. That is the property that matters, and it is the one that is true: the moment a user reaches for this command is the moment they are least sure what state their own store is in, and a check that quietly changed it would be the worst possible answer. It is equally safe against a copy on another machine and against a copy of a store this binary has never seen.

The narrower wording is deliberate. SQLite builds the WAL index of a database that is already in WAL mode, so a hand copy of a live `ghost.db` can acquire a `-wal` and a `-shm` beside it during a verify. They are empty — a read-only connection writes nothing to them — and Ghost's read-only constructor cannot be narrowed to stop it without breaking every other reader in the tree.

Four checks, all reported, in the order they run:

| Check | What it answers | What only it can see |
|---|---|---|
| `sha256` | Is this byte-for-byte the file the manifest describes? | Anything at all in bytes the database does not read: a flipped header field. |
| `integrity check` | Is the file a database SQLite considers structurally sound? | A torn or half-written page. SQLite's answer is printed as it stands — every finding it names, not just the first, and how many were left out of a capped report. |
| `schema version` | Will this build open this file? | Whether the file is from a newer Ghost (a downgrade to restore — refused, upgrade first) or an older one (restorable; the next open migrates it). |
| `row counts` | Does the file hold what the manifest says it holds? | A snapshot that lost rows, which a successful open and a matching hash between them would not. |

`sha256` runs first because it is the only check that needs nothing but the path. A truncated copy, a file appended to, and a file that is not a database at all are all cases where the digest is the most informative thing that can be said — and a truncated file has no schema to read, so the other three checks cannot run at all. The size is compared before the digest, so a copy that is short is named as the size change it plainly is rather than as a 64-character hash to diff by hand.

Every check that can run does run, and one that cannot reports as `skipped` rather than being dropped. A run that stopped at the first failure would answer "this is not a backup" to a file whose only problem is a manifest it cannot read — a different claim, and the wrong one to hand someone deciding whether to keep looking. A run that could not finish at all still prints the findings it did reach, and says `not checked` in place of a row count nobody read: "no rows" would be a claim about the file, and this report must never make one it did not check.

The first line is the verdict, and there are three of them:

| Verdict | Meaning |
|---|---|
| `verified` | Every check ran and every one passed. |
| `checked` | Nothing failed, but something did not run. |
| `refusing` | A check failed, or the file could not be checked at all. Exits non-zero. |

```text
verified ~/.local/share/ghost/ghost.db.backup-20260926T153207Z: 204800 bytes, 1 project, 4 memories, 1 link, 1 task and 1 decision
  sha256           ok       matches the manifest's sha256 9f86d081884c…
  integrity check  ok       SQLite reports the file is structurally sound
  schema version   ok       the file is at schema v18, this Ghost reads v18
  row counts       ok       the file holds the 1 project, 4 memories, 1 link, 1 task and 1 decision the manifest records
```

A file with **no manifest** beside it is not refused, and does not get the word `verified` either. A pre-migration copy — taken by an upgrade before it touched the schema — is written without one, and it is exactly the copy a user wants to check after a bad upgrade. It is reported as `checked`, with the two manifest-derived checks marked `skipped`:

```text
checked ~/.local/share/ghost/ghost.db.pre-migrate-1758800000: 204800 bytes, 1 project, 4 memories, 1 link, 1 task and 1 decision
  sha256           skipped  no manifest to compare against
  integrity check  ok       SQLite reports the file is structurally sound
  schema version   ok       the file is at schema v17 and this Ghost reads v18 — it is restorable, and the next open migrates it
  row counts       skipped  no manifest to compare against; the file holds 1 project, 4 memories, 1 link, 1 task and 1 decision
  (no manifest at ~/.local/share/ghost/ghost.db.pre-migrate-1758800000.manifest.json, so the hash and the recorded counts were not checked)
```

Both outputs above are what the command prints, verbatim, including the column widths.

Note the `schema version` line: a pre-migration copy is **always** at a lower version than the build that wrote it, because that is what "before the migration" means. So this case is not an accident of the example, and it is why an older file passes. `OpenDB` refuses a file from a newer Ghost outright, so that direction is a real failure and the message says to upgrade; an older file is what every restore of a pre-migration copy produces, and the next ordinary open migrates it.

A manifest that is *present but unreadable* **is** a refusal, and the report says so in those words rather than calling it absent — that is damage, not a missing optional file, and treating it as absent would send the reader looking for a file that is sitting right there. The report also never states a size or a row count it did not measure: a run that stopped early says `not checked` rather than `0 bytes` or `no rows`, because the zero value of a size is a 0-byte file and the zero value of a row count is an empty database.

Verify **before** restoring, not after: it is the difference between restoring a copy and finding out afterwards that it was never the copy you thought. The restore procedure itself is above, under [`ghost backup`](#ghost-backup).


### `ghost export`

Writes memories, tasks, decisions and projects as JSON Lines:

```bash
ghost export
ghost export --project myproject --out ~/backups/myproject.jsonl
ghost export --out - | head -3
```

| Flag | Meaning |
|---|---|
| `--project <name>` | Export one project, matched by name or id **exactly**. No path-prefix or basename fallback: a filter is a choice about what to copy, and one that resolved like project resolution could select a different project on another machine than the one named here. A filter that matches nothing is an error. |
| `--out <path>` | Where to write it. Defaults to a timestamped `.jsonl` beside the database. `-` writes to standard output, with the summary on stderr so the stream stays pipeable. |

The file is JSON Lines: one self-describing object per line, opened by a header line carrying the schema version.

```json
{"type":"header","schema_version":2}
{"type":"project","project":{"id":"p1","path":"/src/p1","name":"one","repo_remote":"github.com/wcatz/one","created_at":"…","updated_at":"…"}}
{"type":"memory","memory":{"id":"…","project_id":"p1","category":"gotcha","content":"…","importance":0.5,"source":"manual","pinned":false,"created_at":"…","scope":{"environment":"production"}}}
{"type":"task","task":{"id":"…","project_id":"p1","title":"…","status":"pending","priority":2}}
{"type":"decision","decision":{"id":"…","project_id":"p1","title":"…","decision":"…","rationale":"…","status":"active"}}
```

A memory record carries every column of the row that describes the memory: category, importance, access count, pin state, source, tags, created/updated timestamps, `resolved_at`, the validity triple (`valid_from`, `valid_until`, `verified_at`), the provenance fields (`agent`, `session_id`, `source_ref`, `confidence`) and `scope`. Since artifact schema v2 it also carries `evidence`, the list of `memory_provenance` records backing that memory — every agent that reported it and what each said. A restore that kept the row and dropped the evidence would report "supported by 0 observations" about a fact two agents had already reported, which is the one thing that table exists to prevent. A project record carries its id, path, name, repository remote and timestamps.

The header's `schema_version` is the *artifact* format version, not the database schema version: an artifact is a file that outlives the Ghost that wrote it, and its meaning is the shape of its records. A build reads a range rather than one version, and refuses a file outside it — a newer one is records whose fields it cannot interpret, and an older one would be a file written under rules that no longer hold.

Two exports of an unchanged database are **byte-identical**: projects come first, then memories, tasks and decisions, each in id order, and the header carries no timestamp. An artifact can therefore be diffed against the previous one, and a diff shows only what changed in the store.

These things are deliberately **not** exported:

| Excluded | Why |
|---|---|
| `memory_embeddings` | The vector is derived from the content by a local model, not stored knowledge. The embedding worker rebuilds it for every imported memory. |
| `memory_links` | A link only means something between two memories that are both present, so importing edges ahead of their endpoints would fail the foreign key or fabricate relationships. The linking worker recomputes related edges after an import. |
| `resolve_kept_hash` | A cache of the resolve classifier's verdicts keyed by content hash. Ghost recomputes it. |
| Ghost's own `_global` `builtin` seeds | The shipped rules this Ghost carries. Each is written under a per-install random id, so your copy and the destination's could never be recognised as the same row and the import would add a second one beside it — and nothing would ever remove it. The destination writes them itself, by content, on every open, so the restored store ends up with one copy, written by Ghost. A memory of your own filed under `_global` is **not** one of these: it is the only copy of itself, and it exports. |

That last row is why an export's memory count can be one lower than the row count `ghost backup` prints for the same store: the seed is in the database copy and deliberately not in the artifact.

**A record `ghost import` would refuse is left out and named.** The exporter applies the **importer's own** refusal predicates — the same four functions `ImportProject`, `ImportMemory`, `ImportTask` and `ImportDecision` call, and not a second copy of the rules — because a store can already hold a record this build refuses to import: written by a pre-`#791` import, reinstated by `ghost reflect --restore`, seeded by another tool, or edited by hand. Exporting one produced an artifact its own importer then rejected record by record, so the backup was not a backup and nothing said so until you needed it. Such a record is **left out** and reported on stderr, one `!` line each, with the id rendered so it cannot forge a line of its own:

```
exported 1 project, 1 memory to backups/one.jsonl — 1 record left out, see below
  ! left out: memory m-empty — ghost import refuses it: content is required
  Ghost cannot re-key a row: memory_links, the recorded history and every `ghost history` read are attached to the id this store holds, so the row was left as it is and left out of the artifact.
```

The reason on each `!` line is the **importer's own sentence**, verbatim — not a second description of the rule, because a second description is a second thing to keep in step with the first, and that is what made `ghost export` and `ghost import` disagree in the first place. The `!` line's own reason is also the only sentence about the record: the advice beneath it is per-batch and names commands, never records.

The file is **kept** and the command **exits non-zero**, the same convention the importer uses for a rejected record: what it wrote is a valid artifact, it is just not the whole store, and a partial export reported as a success is the failure mode worth spending an exit code on. So a `ghost export && …` backup script notices.

What is refused is everything the importer refuses, which is the whole of it:

| Refused | Why |
|---|---|
| An id carrying a control character, whitespace, a backtick or a `«»`, or over 128 bytes | A record id is a primary key and is printed **outside** the `«...»` data delimiters, so one of those forges a line of its own. A **project's** id, name and path are held to the same characters *except* whitespace **and except any length**, because a project id is routinely a filesystem path and a deep checkout is a long one. |
| An empty `name` or `path` on a project, an empty `project_id`, `content`, `title`, `decision` or `rationale` | A required field the importer will not write. A project with an empty name is the worst case, because a project step that fails takes its records with it. |
| An unknown `category` or `source` on a memory, an unknown `status` on a task or decision, a `priority` outside 0–4 | A value outside the schema's own set, which the importer refuses by name rather than letting a statement fail. |
| A credential-shaped value in any guarded field | **By design** — Ghost never stores a credential. See below. |
| An over-long `source_ref` or `agent`, an unknown evidence `kind` | Refused rather than clamped: a truncated path is a different path, and an id is a different row. |
| Any record whose **project** is left out of the artifact | The importer resolves each record's project against the artifact, so a memory under an absent project would be rejected as project-not-found. **A project's records go with it.** |
| A memory's or a decision's **`tags`** carrying a control character, a backtick or a `«»` | **Never.** A tag is a LABEL on an otherwise exportable row, so refusing one costs you the memory — or the decision, along with the companion memory `RecordDecision` wrote beside it — and nothing on the write path refused such a tag for the whole life of the feature, so any store written through it can hold one. A tag is carried byte for byte in both directions and printed neutralised and bounded. The characters are refused where a new tag can arrive: the four MCP write tools. |

A tag's characters are the one place this table says **never**, and the row above says why. The class is refused where a NEW tag can arrive instead — `ghost_memory_save`, `ghost_save_global`, `ghost_memory_update` and `ghost_decision_record` each refuse a tag holding a control character, a backtick or a `«»`, naming the position and the tag and writing nothing. `ghost_decision_record` is the only writer of a decision's tag list, so a decision's tags follow the same rule and the same round trip: a decision carrying `["«urgent»"]` exports whole and imports whole. A tag over 64 bytes is trimmed on a rune boundary rather than refused, because a shortened LABEL is a different label and not a different row. A credential-shaped tag is still refused **on a memory** — `CheckImportedMemory` guards that column, and a credential is not a rendering question. A **decision's** tags column is not guarded for credentials at all, because a refusal there would need its own answer about what an export then does with the record (#835).

The same predicates run on the way **in**, at the two places a project row can be created. `Store.EnsureProject`/`EnsureProjectWithRepo` and `Store.ResolveOrCreateRepoProject` each ask `memory.CheckImportedProject` — the exporter's and importer's own function, not a copy — so a project the table above would leave out cannot be opened by an ordinary `ghost_memory_save`, by a bench seeder, or by anything else that goes through the store. No CLI command creates a project row outside `ghost import`, which applies the same predicate ahead of its own INSERT. The check is on **creation**, and at both store routes that means it is asked on the arm about to `INSERT`, in the same transaction as the `INSERT` and after the lookups that decide whether a project exists at all. So a store that already holds a project of this shape — a pre-guard save, a restored snapshot, a hand edit, a checkout whose **directory name** carries a refused character, bound to a clean id by `ghost project bind`, which asks only whether a path is absolute — keeps working, including when the save names it by its **recorded path** rather than its id, which is the ordinary case: the reference resolves to the hostile id and the write still lands in the project you already have. The MCP boundary asks the same predicate, and gates it on the same question answered the same way, so an agent naming that project by its session directory is not refused either.

There is no re-keying: a different id is a different row, and the links, the recorded history and every `ghost history` read are attached to the one this store holds. What you can do about a left-out row depends on its kind, and only two kinds can be deleted **on their own**: a **project** through `ghost project delete <project>` and a **memory** through the `ghost_memory_delete` tool. A **task** and a **decision** have neither — there is no `ghost task delete`, no `ghost decision delete`, no MCP tool for either, and no `DELETE` against those two tables anywhere in Ghost — so removing one of those rows on its own means editing the database directly. They are not unremovable, though: both tables cascade from `projects`, so `ghost project delete <project>` **does** remove them along with every other row in that project. It is the blunt repair, and it is available. Until you run it the row stays in the store and out of every artifact, and the export report says which of the two cases you are in. (There is no top-level `ghost delete`: `delete` is a subcommand of `project`, so the full spelling is `ghost project delete`.)

### A credential-shaped field

A row refused by the credential guard is different from every other left-out row, and the report says so in its own paragraph rather than folding it into the delete advice:

```text
  ! left out: memory m-9f3c — its content is credential-shaped, and ghost import refuses to store one by design — Ghost never stores a credential value
  A credential-shaped field is refused on import BY DESIGN and the value is never stored — this report names the field, never the value. Replace the value with WHERE it lives and how to read it, never the value itself, then re-export: a corrected row is still refused until the artifact is written again.
  · `ghost_memory_update` edits content, source_ref or tags
  · `ghost_task_complete` edits notes — which also marks the task done
  · `ghost_task_update` edits description
  · No tool edits a memory's agent and session_id — the memory update overwrites both with the EDITING SESSION's identity, because a caller must not be able to name its own author, so it cannot clear a value a pre-#656 row already holds; edit the row's agent and session_id in the database directly
  · No tool edits a memory's evidence agent, session_id and source_ref — no tool writes an evidence row; they are appended by a save, an import and a reflect, and the only surface that reaches one is `ghost history purge <memory-id>`, which erases the memory's whole recorded history rather than editing a field
  · No tool edits a task's title — written when the task is created; `ghost_task_update` takes status, priority and description only, so the only route is the database directly
  · No tool edits a decision's title, decision, rationale and alternatives — there is no decision update tool of any kind, so the only route is the database directly
  · No tool edits a project's path — `ghost project bind <project-id> <checkout-directory>` rewrites it, and note that `ghost project delete` is NOT the fix here: it cascades away every memory in the project
  · No tool edits a project's name — nothing rewrites it; a project is created, bound, merged or deleted, never renamed, so the only route is the database directly
  Ghost cannot re-key a row: memory_links, the recorded history and every `ghost history` read are attached to the id this store holds, so the row was left as it is and left out of the artifact.
  To include it, delete the row and re-save it (for a credential-shaped field, editing the field is usually what you want instead — see above): `ghost project delete <project>` drops that project and every row under it, and a memory goes through the ghost_memory_delete tool.
```

The `!` line names the **field** and nothing else. The value is not in the report, not in the exit error, and not in any part of the export's own output — a refused credential must not be relocated into a terminal, a log or a paste. The same is true of the importer's report, and for the same reason.

To fix the row, **edit the field** rather than delete the memory: put a pointer to where the value lives and how to read it, never the value, then **re-export** — a corrected row is still refused until the artifact is written again.

Which tool edits which field is derived from the tools' own arguments, so the list cannot drift from them:

| Field | Fix |
|---|---|
| a memory's `content`, `tags` or `source_ref` | `ghost_memory_update` |
| a task's `description` | `ghost_task_update` |
| a task's `notes` | `ghost_task_complete` — which also marks the task **done** |
| a memory's `agent` or `session_id` | **no tool sets them from a caller argument.** The update *does* write both columns, but with the **editing session's** identity, because a caller must not be able to name its own author. It therefore cannot clear a value a pre-`#656` row already holds — so edit the row's `agent` and `session_id` in the database directly. |
| a memory's **evidence** `agent`, `session_id` or `source_ref` | **no tool.** Nothing writes an evidence row: they are appended by a save, an import and a reflect. The one surface that reaches one is `ghost history purge <memory-id>`, which erases the memory's whole recorded history rather than editing a field. |
| a task's `title` | **no tool.** Written at insert; `ghost_task_update` takes status, priority and description only. |
| a decision's `title`, `decision`, `rationale` or `alternatives` | **no tool.** There is no decision update tool of any kind. |
| a project's `path` | `ghost project bind <project-id> <checkout-directory>` — and note that `ghost project delete` is **not** the fix: it cascades away every memory in the project. |
| a project's `name` | **no tool.** Nothing rewrites a project's name; a project is created, bound, merged or deleted, never renamed. |

Where the route is "the database directly", the alternative is the delete command above. The field lists in the first three rows are derived from the tools' own arguments, so they cannot drift from them; the rest are stated per group because each has a different reason and a different route.

### `ghost import`

Loads an artifact written by `ghost export`:

```bash
ghost import ~/backups/one.jsonl
ghost import ~/backups/one.jsonl --apply
ghost import ~/backups/one.jsonl --apply --trust-provenance
```

| Flag | Meaning |
|---|---|
| `--apply` | Actually write. Without it the command is a dry run that reports what it would create, skip and reject, and writes nothing. |
| `--trust-provenance` | Keep each memory's own source and pin state instead of downgrading them. Off by default; pass it when the artifact is **your own** export. |

A dry run is not a separate code path: every record goes through the same validation an apply run would, so the preview describes the run that follows rather than a similar one. It also opens the database **read-only** — `ghost import` without `--apply` cannot migrate a database whose schema is behind, or seed the builtin rows, while reporting "nothing written". Two consequences worth knowing:

- A dry run needs a database to preview **into**, so on a machine with no Ghost store yet it reports that rather than creating one. Start a session (or run `ghost mcp init`) first, as `ghost export` also requires.
- Because it cannot migrate, a store from an **older** Ghost is refused with a message naming both versions rather than failing on a missing column:

  ```text
  error: the database at ~/.local/share/ghost/ghost.db is at schema v11 and this Ghost reads v17
         — start a session, or run ghost mcp init, to migrate it before exporting it or
         previewing an import into it
  ```

  A store from a **newer** Ghost gets a different message — upgrade Ghost — because migrating backwards is not the fix. Both are checked by reading `PRAGMA user_version` on the read-only connection, so the check never writes.

  The check is strict: the store must be at exactly this Ghost's schema version, not merely at or above some floor. The only columns `export` and a dry run select that postdate v10 are `projects.repo_remote` (v11) and `memories.scope` (v12), so a v12–v16 store would in fact query fine — but a floor would hardcode which columns exist at which version, and the day a reader selects a newer column it would quietly admit a store that fails with a missing-column error again. So a v12–v16 store is asked to run one read-write open first, which is `ghost mcp init` or any session. `ghost backup` runs no version check of its own, but it is itself a read-write open: it reaches the store through the same path as a session, so it migrates and seeds the store it copies, and a store from a newer Ghost is refused outright. That makes it one way to *pay* this cost — run it once, then the export works — rather than a way around it. It is safe to run against a live MCP server, which is why it uses `VACUUM INTO` at all.

### Imported provenance

By default an imported memory is stamped `source = "onboarding"` and unpinned, whatever the artifact says:

```text
  create  memory:   line 3  "carried across machines" (a1) (provenance downgraded to onboarding, unpinned)
imported 1 memory from ~/backups/one.jsonl
  provenance downgraded to onboarding and unpinned — pass --trust-provenance to keep the artifact's own
```

The reason is that an artifact is a file that arrived from somewhere, and on its own authority it would otherwise be able to plant rows that read as yours or as Ghost's:

| The artifact claims | What the store would then treat it as |
|---|---|
| `source: "manual"` | the user's own words — excluded from consolidation by name |
| `source: "builtin"` | a rule Ghost ships — excluded from consolidation by name, and presented as Ghost's own |
| `pinned: 1` | exempt from consolidation whatever its source |

`onboarding` is the source `internal/claudeimport` already uses for memories brought in from outside Ghost at first contact, and it is in the database's `CHECK` already, so downgrading needs no migration. A downgraded memory stays ordinary: consolidatable, and honest about where it came from.

**Importing your own export? Pass `--trust-provenance`.** The rows come back with their own source and pin, which is what a restore wants. The asymmetry is deliberate: the cost of the default being wrong is planted provenance, and the cost of the flag being wrong is passing it once.

```text
  skip    project:  line 2  "one" (p1)
  create  memory:   line 3  "the WAL holds a transaction…" (3A6B…)
would import 1 project, 1 memory from ~/backups/one.jsonl

nothing written — pass --apply to import
```

Rules the import follows:

- **A record whose id already exists is skipped, never overwritten.** The artifact is the older of the two copies by construction, so overwriting would restore stale data over live data. This also makes re-running an import always safe, which is how you repair a run that rejected a record.
- **A file whose schema version this build does not read is refused outright**, in either direction. Reading a newer one would insert records whose fields this build interprets by guesswork.
- **Imported memory content goes through the same length cap and the same validation as a normal save** — category, source and importance are checked against the schema's own value sets, and over-long content is cut at `MaxContentLen` with the same explicit marker. An artifact from another machine is another way to reach the memories table, and must not be a way around its rules.
- **Projects are created when missing**, and records are applied projects first, then memories, tasks and decisions, so a file whose records were reordered by an editor still imports. A `blocked_by` or `superseded_by` pointer is only honoured when the record it names is in the same artifact and is applied first; a pointer to a record the artifact does not contain, or one inside a cycle, is dropped.
- **A record that cannot be imported is rejected and the run continues**, so one hand-edited line does not abandon the rest of a large artifact. Rejections are counted and the command exits non-zero, so a partial import is never reported as a complete one.
- **A line that will not parse is rejected on its own.** A truncated, badly merged or hand-edited artifact still imports every record around the damage, and the bad line is reported with its line number. This is what the line-per-record shape buys: a file-level refusal is reserved for the problems that are not one line's — a missing or unreadable schema version, a second header, an unknown record type — where applying part of the file would mean importing data whose meaning is a guess.
- **A project this store already has is adopted, not duplicated.** Project ids are per-install, so an artifact from another machine names a project the destination has never seen while the destination very often has its own project for the same checkout or the same repository. Both `path` and `repo_remote` are unique, so inserting the artifact's project would collide — and because every memory, task and decision names the artifact's project id, one collision would take the whole file with it. Instead the records are attached to the project that is already there, and the report line says so:

  ```text
    skip    project:  line 2  "thing → ghost (this store already records that checkout or repository)"
    create  memory:   line 3  "the WAL holds a transaction…" (3A6B…)
  ```

  A collision that survives that — another writer claiming the same directory between the plan and the write — is refused by name, in the dry run as well as the apply:

  ```text
  error: project laptop-1 cannot be imported: this Ghost already records /src/thing as project ghost — import into that project, or merge it with `ghost project merge`
  ```

- **A record with no `created_at` or no `importance` takes the column's own default**, not a bound zero. A stored empty string makes `julianday('')` NULL, which makes the whole time-decay expression NULL and sorts the memory out of every ranked read — present in the store, invisible to recall. A *stated* `importance: 0` is kept as 0: a memory saved without an importance is stored as 0, exported as `"importance":0`, and re-importing it must not promote it to the 0.5 default.

Import does not run Upsert's near-duplicate probe. A restore is putting back what was there, not adding knowledge, and folding two rows of the artifact into one would silently drop a memory the user chose to keep.

## Context and benchmarks

### `ghost context`

Prints the passive session-start context block:

```bash
ghost context
ghost context --cwd /path/to/project
```

This is primarily used by the opencode adapter, which injects the returned block as instructions because opencode does not consume a stdout hook response.

### `ghost history <memory-ref>` / `ghost history purge <memory-id>` / `ghost history compact`

Prints one memory's append-only history: every insert, edit, reflection rewrite,
duplicate fold, resolve, supersession, restore, import and deletion, oldest
first.

```bash
ghost history <memory-ref>                   # human-readable changelog
ghost history <memory-ref> --limit 5         # the newest 5 entries
ghost history <memory-ref> --json | jq .phase # one JSON object per entry

ghost history purge <memory-id>              # erase the row AND its history
```

`<memory-ref>` is a full memory id **or** 8 or more characters of one — the same
eight characters every Ghost report prints, so an id copied out of one can be
pasted straight in. A prefix naming more than one memory is refused and says which,
rather than picking one, and a short argument that matches nothing is told it is a
prefix no id starts with rather than that a memory was never written. The id is
resolved across the whole store, so a ref reaches a memory in any project, and it
reaches **deleted** ones too: the tombstone is in `memory_history` and it is the
reason to read a history. It also reaches a live memory that predates the history
table, which has no history rows of its own. A full id the store does not hold
keeps its own answer — that it was never written, or its history has been pruned.

Each entry names when it happened, which write path made it (`save`, `update`,
`reflect`, `merge`, `resolve`, `unresolve`, `supersede`, `unsupersede`, `restore`,
`import`, `delete`, `baseline`), which agent and session performed it when the
write path knew — the lifecycle passes do not, and the entry says so rather than
inventing one — and the content, category, importance, `resolved_at` and source
the memory held once that write landed.

Two more fields appear when they apply: the memory on the other end of the event
(`related memory:` — what replaced a deleted row, or whose edge claims this one)
and the text a duplicate fold brought in and did not keep (`folded-in text:`).

The history outlives the memory: a deleted memory's last state is still readable
here, and a report that finds neither a row nor a history says so instead of
printing nothing. In `--json` form that refusal is a one-line `{"error": ...}`
object and a non-zero exit, never an empty stream a script would read as "no
history"; the live / no-longer-live distinction is not repeated per line, and does
not need to be — the last entry's phase says it, and a deleted memory ends in a
`delete` row.

`purge` is the exception, and it is the redaction path. Because the history keeps
the text a memory **used** to hold, deleting a memory that contained a credential
leaves that credential in the database file. Printing is not redaction, so the two
printers withhold it: an entry whose text holds a credential-shaped value prints as
`<withheld: format, category, bytes>` — in the human form and in `--json` alike,
where only the value changes and the schema is the store's own. The category is in
the marker only when the entry records one, so the folded-in text prints
`<withheld: format, bytes>` — a history row records no category for a fold's
discarded wording — and so does an edge's target text in `supersede --withdraw`,
which is the other caller with none. A row the write-time filter already redacted keeps
its own notice instead, which is the statement that the text was removed on the way
in rather than merely hidden here.
`purge` erases the text in both directions (the store primitive for the second
case is `Store.PurgeMemoryHistory`), and it also removes the reflection
snapshots naming the memory — otherwise `ghost reflect --restore` would bring the
row back, with its text and no history:

- a memory that is still there is deleted along with its history, in one
  transaction, so neither can survive the other;
- a memory that is **already deleted** has its history erased on its own. That
  second case is the one a delete-time purge cannot cover: the tombstone is the
  feature, so "erase that secret" asked an hour after the memory was deleted would
  otherwise report the memory as not found and leave the text where it is. The
  memory is not brought back — only the text goes.

`purge` takes the **whole id** and refuses a prefix, which is the one difference
from the read above. It erases recorded text for good, so a mistyped argument is
not a message but an unprintable memory, and echoing the resolved id would not make
it safe — the announcement and the transaction are one breath apart, and one wrong
character in a pasted prefix is still a unique match. The refusal prints the full id
the prefix names, so the way through is `ghost history <prefix>` and then
`ghost history purge <that id>`. A whole id the store does not hold is reported as a
miss ("nothing to purge"), not as a prefix — which is also what re-running a purge
that already succeeded looks like.

The MCP equivalent is `ghost_memory_delete` with `purge_history: true`, in both
directions — including for a memory that is already deleted, where it purges the
recorded text without restoring the row; use it whenever the intent is to erase
something rather than to retire a memory. Neither can reach a backup taken before
the purge, or another machine's copy of the store. The MCP equivalent is
`ghost_memory_delete` with `purge_history: true`; use it whenever the intent is to
erase something rather than to retire a memory.

Reading a history (`ghost history <ref>`, and `as_of`) writes no memory, history or
project row, and neither does `ghost history compact` **without** `--apply` — a dry
run is nothing but reads, and it is the command to reach for first. The two forms
that do write are `ghost history purge <ref>`, which deletes the recorded text, and
`ghost history compact --apply`, which deletes redundant versions and (with
`--fix-updated-at`) moves a stamp; see [the compact section](#ghost-history-compact)
for what that one touches. All of them open the store read-write — the same open
`ghost maintenance status` and `ghost backup` use — so a database predating the
history table is migrated by the open, and that migration first writes the full
pre-migration backup copy it always takes. The strictly read-only opener is not used
here because it refuses a store behind the current schema, which is exactly the store
someone is most likely to run this against after upgrading.

Entries are kept per the growth policy in [architecture.md](architecture.md#memory-history): the newest 50 versions of one memory, and the newest 20 000 rows in the store.

#### `ghost history compact`

Repairs what the pre-#727 reflect behaviour left in a store. Until #727 landed,
every applied reflection appended a **byte-identical** `reflect` version for each
memory it kept and set that memory's `updated_at` to the run's own time. On a store
that ran the unattended lifecycle, that measured at 80% of `memory_history`, and it
pushed real events toward the retention caps above while leaving `updated_at`
holding the time of a reflection instead of the time of the last real change — which
is what `ghost supersede` orients a candidate pair by and what `--skip-unchanged`'s
fingerprint carries as a change proxy. #727 stopped the new damage; this removes the
old.

```bash
ghost history compact                                  # dry run, every project
ghost history compact --project my-project             # dry run, one project
ghost history compact --apply                          # remove the redundant versions
ghost history compact --apply --fix-updated-at         # and restore updated_at too
ghost history compact --before 2026-10-01              # widen the bound (see below)
```

A version row is removed **only** when it records the same state as the row before
it of the same memory, in rowid order, compared over every column a version stores:
`content`, `category`, `importance`, `resolved_at` and `source`; and only when it is
a `reflect` version recorded before the bound. Six things always stay:

- a memory's **first** version — the only statement of what it said, with nothing
  to duplicate;
- a memory's **newest** version — the statement of what it says *now*, and the same
  row the per-memory retention cap already declines to trim;
- a `delete` tombstone, a `supersede` or its `unsupersede`, a `resolve` or its
  `unresolve`, a `merge`, an `import`, a `restore` — each records a claim the state
  does not, and a phase added to the schema later is not removable until it has
  been classified;
- any row carrying a `related_id` or `merged_content` — it is the thread a reader
  follows from one memory's past into its successor's, not a statement about this
  one;
- any version recorded at or after the bound;
- **every version of a memory that has since been deleted.** A deleted memory has
  no live row, so an `as_of` read of it takes the age it measures from the version
  that answers — and removing a version from it would change what a past read
  computes, for a memory nobody can edit and nobody can restore. Its history is
  frozen the moment it is deleted, so this costs nothing: the flood it would have
  cleaned up is never written to again.

`save`, `update` and `baseline` are never compacted even though they record a state
and nothing else, and the reason is a real one rather than caution: `ghost memory
update` appends an `update` version on **every** edit, and a **tags-only** edit is a
change to what the memory says about itself that this table cannot see, because it
has no column for tags. Both `update` versions and the stamps they moved survive.

`--before <t>` bounds the repair, and the default is `2026-09-28T17:14:07Z` — the
instant #727 reached main. A current build still files a byte-identical `reflect`
version on purpose: a consolidation merge whose survivor is one of its own sources
carries the union of that source's tags, and nothing in this table can see that,
because the tags are not a column. So the repair does not try to tell such a row
from the damage; it declines to touch any row a current build wrote. Widen the bound
only for a store whose clock is behind — a restored backup, a copied database —
and read the dry run at the wider bound before applying it. `<t>` is a `2006-01-02`
date or an RFC 3339 instant, and a bound the command cannot read is refused rather
than defaulted. **Every count is a count at a bound**, and the report names the one
it used:

```
history compact (dry run — nothing was written; pass --apply to write)
  removing only versions recorded before 2026-09-28 17:14:07
  my-project  19 redundant version(s), 1 updated_at restored
```

A bound that reaches **past** the default is warned about on **stderr**, in a dry
run as well as an apply, because a dry run is where you decide whether to pass
`--apply`, and a risk disclosed only by the write is disclosed after the decision.
It names the risk rather than restating the bound you typed:

```
warning: --before 2026-10-02 00:00:00 reaches past 2026-09-28 17:14:07, the instant
#727 shipped, so this run can remove 'reflect' versions a current build wrote — …
```

The default does **not** warn. A command whose zero configuration printed a warning
would train its reader to skip the one that matters.

`--fix-updated-at` is a second, separate repair, behind its own flag. Each live
memory's `updated_at` becomes the `recorded_at` of its **anchor**, and only where a
version that changed nothing sits **above** that anchor — that version is the
evidence a reflection run moved the stamp, and without it a stamp the history
cannot account for belongs to some other writer. The bound reaches this gate too,
for the same reason it reaches the delete: a version this repair would not remove
is not a version it may treat as proof that a reflection ran.

**The anchor is the newest version that does not itself repeat the version before it
whose WRITER moved `updated_at` in the same statement that filed it** — `save`,
`update` or `reflect`. The test for "repeats" is the DAMAGE rule on its own, not the
full removal rule: a version is damage when it records the state its predecessor
recorded, is a `reflect`, was recorded before the bound, and names no other memory.
The two retention guards — a memory's newest version is spared, and a deleted
memory's history is left alone — are **not** part of the anchor's definition — which
is why being spared by one is not what makes a version an anchor, and is not a reason to
look for one. A version can be the anchor while a guard spares it from removal, but only
if it repeats nothing; a repeating row is damage at any position in the history. A `ghost resolve` changes
`resolved_at` and says
in as many words that it leaves `updated_at` alone; a duplicate save that folds
changes `importance` and moves nothing. Answering with either would set a memory's
stamp to an instant the store never held on that column at all, and on a real store
this did so for 49 of 288 restored stamps. `ghost history compact --fix-updated-at`
therefore skips over all three and lands on the newest write that really moved the
stamp without itself being the damage. Note what that does *not* mean: the anchor is
not necessarily earlier than the last write that moved the stamp — a memory later
touched by a deliberate `update`, or by any post-#727 write, anchors on that write. It
does *not* mean a newest no-op `reflect` can be one: a pre-#727 no-op repeat is damage
whatever its position, so being spared by the newest-version guard does not make it an
anchor, and `TestCompactHistoryFixUpdatedAtRestoresTheLastStampWrite` is the case — its
newest version is a no-op reflect and the anchor is the `update` beneath the flood. "The
damage is earlier" is the reason a *flood* does not pin the stamp, not a guarantee about
any one memory. A version a
deliberate writer filed *and* moved the stamp is still an anchor: that is what
`save`/`update`/`reflect` membership means, and `TestCompactHistoryDoesNotRewindADeliberateBumpUnderARemovableRow`
pins the interleaving.

A memory whose history holds **no** version written by such a writer has no anchor
at all, and the answer is to leave its stamp exactly where it is and say so. A
pre-#727 memory is the case: it has no `save` version, so if its next writer was an
unresolve its only recorded version is one whose writer moved no stamp. Its no-op
reflect flood is still removed — the version removal never depended on there being a
stamp to move — and the report counts the row separately as
`no recorded stamp write`, so a report of `0 updated_at restored` beside a store full
of removed versions cannot be read as a finished repair. It is a different count from
`unreadable` on purpose: an unreadable stamp is a value that exists and no layout
reads, and this is a value whose *author* does not exist anywhere in the table.

It moves **only backward** — the damage moved a stamp forward, so the repair undoes
that — and a memory whose stamp is already at or before the target is left as it
is. Both stamps are read through the store's own layouts and the restored one is
written in the layout the store writes, so a whole-day value on either side is
readable and never written back. A memory whose stamp no layout reads is left alone
and counted separately (`stamps unreadable`), because a row this run could not
repair is a row whose supersede orientation is still wrong.

For a memory that is still **live**, neither repair changes what an `as_of` read
of it returns *for*. Its `content`, `category`, `importance`, `resolved_at`,
`source`, `project_id`, `created_at` and supersede edges are all read from the live
row or from a version carrying the same state, so a read of the same instant gives
the same answer after the repair as before it. Three fields do move, and they move
for two different reasons. `UpdatedAt` moves because the stamp repair *is* a write
to `memories.updated_at` — the column the as-of read takes from the live row — and
it moves **backward** to the recorded time of its anchor: the newest version that
does not itself repeat the version before it whose writer moved the stamp in the same
statement that filed it. That is the field a reader comparing output across the repair
would notice, and it is the one the flag exists to move. The anchor is not necessarily
earlier than the last write that moved the stamp: a memory later touched by a deliberate
`update` or any post-#727 write anchors on that write. A newest no-op `reflect` is damage
all the same and is never the anchor. The other two fields move only
because they *name* which version
answered: `VersionRecordedAt` and `VersionPhase` can now describe an earlier,
equivalent version — a memory that was resolved and unresolved, or folded, may read
as merely saved for an instant whose reflect flood has been compacted away. Nothing a
historical read is *for* changes; the attribution and the stamp do. A **deleted** memory is excluded from both repairs
instead, so for one of those none of the three fields move at all — which is why the
exclusion is a scope rule rather than a retention one.

Because the stamp repair needs that evidence, pass both flags in the **same** run:
`ghost history compact --apply --fix-updated-at`. An earlier run that already
removed the versions took the evidence with it, and the second run has nothing to
act on. That is not a quirk — a dry run reports the same numbers either way, and
the same is true of the apply.

`ghost mcp status` (and the `ghost_health` MCP tool) report how much of that damage
a store is still carrying before any of it is removed — the restatement share over
the last 24 hours, and how close the table and the memory nearest each cap are to
the two caps above. The share it names is the same comparison this command removes rows by,
over the same state columns, so the two cannot disagree; it counts every
restatement while the repair removes a **subset** of them (a memory's newest version
stays), so the share is how much noise the table carries rather than how much is
disposable. See [`ghost mcp status`](#ghost-mcp-status) for the thresholds.

Both repairs leave a memory that keeps only the versions a reader could want: its
first, its newest, and every event. Nothing here removes a `delete` tombstone, a
supersede or its withdrawal, a resolve or its clearing, a merge, an import, a
restore, or a row that names another memory.

A dry run is the default and writes nothing; `--apply` writes. Either way the
counts are per project, and the dry run's numbers are the apply's numbers rather
than an estimate: deleting a version that changed nothing cannot change whether the
row after it changed anything, so the set of removable rows is a fixed point of the
deletion. The work runs in bounded `BEGIN IMMEDIATE` batches, so it never holds the
write lock over a whole store's history, and running it twice is a no-op the second
time. It refuses to run while a lifecycle run holds any of the projects' locks, and
it checks **every** project before it touches any of them — a refusal that arrived
after the first project had been compacted would be a check that protects nothing.
The refusal names the projects, so an operator can either wait or re-run scoped with
`--project`.

This is the only `ghost history` mode that writes. It opens the store read-write
like the rest of the command and, under `--apply`, deletes `memory_history` rows and
updates `memories.updated_at`. Both are repairs rather than edits: a version row is
removed only when the row before it of the same memory says the same thing, so no
event and no state a reader could want is lost.

A run that fails partway through says what it had already done, per project,
including the project it stopped in — its committed batches are a store already
rewritten, and a report that dropped the project for having failed would send an
operator re-running a whole store to find out about one.

### `ghost bench`

Runs the built-in retrieval-quality benchmark without a network call or LLM judge:

```bash
ghost bench
ghost bench --sweep
ghost bench --context
```

Plain `ghost bench` prints the three-conditions table (keyword, vector, fused) over the embedded dataset, then three things beneath it: the **no-answer false-positive table** — what each condition returns for the 24 queries nothing in the corpus answers — the abstention baseline for the shipped fused path, and the **paired 95% interval between the fused condition and each single leg**, so the fusion margin quoted in the docs is a number the command prints rather than one only a test logs.

`--sweep` is a different report: it grid-searches the vector-leg weight (FTS weight is the complement) and prints each point's NDCG@10, R@1, R@10 and MRR@10 **plus a paired 95% interval against the shipped default** — because a sort by point estimate is not a ranking of points the dataset cannot separate. It returns there, so it prints neither table above. See [Benchmarks and methodology](benchmarks.md).

`--context` is a third report, and the only one that measures the **block** rather than the ranking: it assembles one context block per graded query through the same path `ghost_memory_search` takes, at that tool's own budget (10 items, 16000 response bytes), and reports how much of each block is graded-relevant, how much of it is contamination, whether it fit the budget, how the rows are spread across buckets and what the block costs in bytes and estimated tokens per answered query. It is report-only, and it prints only this section. The two flags cannot be combined — they are two reports over two questions — and the report is measured at a fixed instant rather than the wall clock so two runs of one binary print the same bytes. See [Benchmarks and methodology](benchmarks.md#context-assembly-ghost-bench---context).

## Scratch hygiene

### `ghost maintenance status`

Shows the live scratch root usage (bytes, file count) against the configured
`scratch.max_bytes` budget, followed by the most recent hygiene events — one
line per run with its timestamp, scratch bytes, and reaped counts:

```bash
ghost maintenance status
```

Budget checks that fired before a harness spawn (root was over budget, so
stale entries were reaped and possibly a loud warning emitted) are recorded to
the database; quiet under-budget spawns add no rows. `0` budget prints as
disabled.

### `ghost maintenance clean-scratch`

Reports pre-scratch-root debris — `~/.cache/ghost-tmp` and the shared system
temp dir's hidden `.<hex>-00000000.so` JIT-cache droppings — with counts,
bytes, and exact paths. Report-only by default:

```bash
ghost maintenance clean-scratch            # report: counts, bytes, paths
ghost maintenance clean-scratch --apply    # remove strict-signature matches
```

`--apply` removes only regular files matching the strict droppings signature
(never directories, even ones named like droppings), skips anything an
`lsof`/`fuser` probe reports as open or mmap'd, and — when neither probe tool
is available (Windows, minimal containers) — refuses to remove anything and
says so rather than risk deleting an open file.

## OpenCode sessions

### `ghost opencode cleanup-sessions`

One-shot cleanup of the lifecycle sessions OpenCode stored before the child's
data directory was isolated (#588): it targets sessions whose title is
**exactly** `[ghost]` and whose most recent activity is older than a grace
period. Sessions with any other title — including `[ghost] follow-up` or a
different casing — are never touched. Report-only by default:

```bash
ghost opencode cleanup-sessions                 # count what would go
ghost opencode cleanup-sessions --grace 24h     # older than a day instead of 1h
ghost opencode cleanup-sessions --apply         # delete them
```

| Flag | Meaning |
|---|---|
| `--grace <duration>` | Minimum age before a session is eligible, measured from its most recent activity (`created`/`updated`, whichever is later). Default `1h`; `0` accepts any age. |
| `--limit <n>` | How many sessions `opencode session list` is asked for. Default `20000` (`0` means that default, unlike `--grace 0`); a list that reaches the limit prints a truncation warning instead of pretending it is complete. |
| `--apply` | Perform the deletes. Without it nothing is deleted. |

Sessions are read through the real `opencode` binary (`session list --format
json`), so — like your own `opencode session list` — the run is scoped to the
current project: run it from the checkout whose sessions you want cleaned. The
child receives Ghost's harness environment allowlist, whose OpenCode-specific
entry is `OPENCODE_API_KEY`, so a store override such as `OPENCODE_DB` in your
shell does not reach it (opt in with `GHOST_PASSTHROUGH_ENV`, which re-exposes
whatever else you name). Timestamps are read as Unix milliseconds on the CLI's
word alone, so a timestamp that is missing or outside a plausible window
(before 2020-01-01, or more than five minutes ahead of the clock) is refused
instead of being read as "very old": a unit or session-JSON-shape change in
OpenCode's output fails closed on one session rather than making every session
look eligible, and the command prints a `warning:` line with the number it
skipped so the refusal never reads as an empty, clean run. To verify the unit
against your real CLI — `GHOST_LIVE_TESTS=1 go test ./internal/ai/ -v
-run TestOpenCodeSessionList_TimestampsAreMilliseconds`; it lists read-only,
resolves the binary the same way this command does (`cli.opencode_binary`,
else `PATH`), skips when your checkout has no sessions to inspect, and *fails*
when sessions are listed but none carries a timestamp — that is the shape
drift, not an empty checkout. Deletion uses `opencode session delete <id>` with
up to three attempts per session; a failed delete is reported and counted but
never stops the rest, and the command exits non-zero when any delete failed.
The timestamp and truncation warnings do not change that exit status: the run
did what it was asked, so read the report. The binary comes from `cli.opencode_binary`
(`GHOST_CLI_OPENCODE_BINARY`) when configured, otherwise from `PATH`.

New lifecycle sessions no longer need this command: since #568 the opencode
child runs with its data directory inside Ghost's invocation-owned scratch
tree, so it writes to its own store rather than the user's (see
[Harness subprocess environment](configuration.md#harness-subprocess-environment)).
This command exists for the backlog that predates that isolation.

## Maintenance and installation

### `ghost upgrade`

Checks GitHub Releases and replaces a standalone binary after verifying the release:

```bash
ghost upgrade
ghost upgrade --allow-downgrade    # install a release older than this binary
ghost upgrade --allow-prerelease   # install a prerelease (an rc, a beta)
ghost upgrade --allow-unattested   # install when the attestation cannot be checked
```

A plugin-managed binary refuses this path because the plugin manager owns it; use `/plugin update` in Claude Code instead.

**What is verified.** The downloaded archive has to agree with three things before a single byte reaches the installed binary:

1. the `digest` GitHub reports for that asset in the releases API (`sha256:<hex>`) — a digest GitHub computed for the bytes it holds, rather than one uploaded beside them. A missing digest, a digest for an algorithm this binary cannot compute, and a mismatch are all refusals, not warnings;
2. the release's `checksums.txt`, which every ghost release carries. The two checks are independent: `checksums.txt` is a second file in the same release, so whoever can replace the archive can replace the manifest that vouches for it;
3. from **v0.43.0 onwards**, the release's [build attestation](https://docs.github.com/actions/security-for-github-actions/using-artifact-attestations/using-artifact-attestations-to-establish-provenance-for-builds), fetched from GitHub by the digest of the bytes just downloaded and verified against the public Sigstore trust root. The first two are *integrity* checks; this third one is the *provenance* check, and it is the one that answers "which workflow built this". See [Attestations](#attestations) below.

The archive is checked before it is unpacked, so a substituted release is refused without its bytes ever being decompressed.

#### Attestations

The first two checks are both *integrity* checks. A digest from the releases API proves the download matches what GitHub holds; it does not say who published it, so anyone able to replace a release asset *and* the API's record of it defeats both. The attestation closes that gap. Every release asset from v0.43.0 carries one, minted by this repository's release workflow running on the release tag, and `ghost upgrade` checks it and refuses to install an archive nothing has vouched for.

The check is: the Sigstore bundle GitHub holds for the digest of the downloaded archive must carry a Fulcio certificate whose identity is `https://github.com/wcatz/ghost/.github/workflows/release.yml@refs/tags/v<version>`, issued by `https://token.actions.githubusercontent.com`, and must cover the archive's own digest. The trust root is fetched through TUF from the public Sigstore repository and cached under your Ghost data directory for a week, so a typical upgrade makes no network request to Sigstore at all and works offline against the cached root.

Three refusals, and they are not the same thing:

| | what it means | what permits it |
|---|---|---|
| **no attestation** | the service's index says the release publishes none for these bytes | `--allow-unattested` |
| **could not check** | the index, a bundle behind it, or the trust root could not be fetched | `--allow-unattested` |
| **attestation did not verify** | a bundle exists and is not this repository's release workflow, or does not cover these bytes | **nothing — always fatal** |

Only the *index* answering 404 means "no attestation". A 404 on the presigned `bundle_url` behind a listed attestation is a fault: the index said the release has one, so a user told otherwise would pass a flag that means something different, for a release that is in fact attested. And one attestation the service cannot serve does not discard the ones it can — a release legitimately has several, so refusing the whole lookup would let anyone who can publish one extra unreadable attestation make a release uninstallable.

`--allow-unattested` means exactly "this release has no attestation" and "nobody could be asked", never "the attestation did not check out". An attacker who can mint one bundle of their own would otherwise be handed the whole feature, so a rejected bundle is refused whatever flags you pass and its message does not name the flag. Either way the flag is loud: proceeding prints a warning on stderr *before* the install, not after it.

An outage is reported as an outage, never as "this release has no attestation" — a service that answered 500 established nothing, and telling you otherwise would turn a transient fault into a permanent belief about a release.

**Where the boundary is, and why it is a constant.** Releases before v0.43.0 are not checked, because a release published before the release workflow minted any cannot have an attestation; requiring one would make every upgrade to a pinned older release fail and would teach `--allow-unattested` as a routine flag. A version this binary cannot order (`dev`, a tag that is not a semver) counts as *requiring* an attestation, so "cannot tell" never reads as "old enough to skip". The boundary is a constant in [`internal/selfupdate/attestation.go`](../internal/selfupdate/attestation.go) rather than something computed, and the producer side is guarded by a test that fails the build if the release workflow stops attesting an asset — so the constant cannot quietly outrun the releases that have attestations.

**A second attestation from GitHub would change a fatal into a refusal you cannot override.** This repository attests with [`actions/attest-build-provenance`](.github/workflows/release.yml), whose certificate identity is `.github/workflows/release.yml@refs/tags/v*`. GitHub can *also* mint a release attestation of its own, with the identity `https://dotcom.releases.github.com`, when a repository has that feature enabled. If that ever happens here, a release that carries only GitHub's own attestation is no longer *absent* — the service lists a bundle for those bytes — and it is not this repository's release workflow, so it does not verify. The result is **unverifiable**: refused, and `--allow-unattested` does not reach it, because an attacker who can mint one bundle would otherwise be handed the whole feature.

So the operator's lever is not a flag. It is to keep the release workflow attesting every asset, so that any release that lists *any* attestation also lists one this client can verify. If a release ever reaches a user in the unverifiable state, the diagnosis is on the release, not on the machine: check that the release workflow's `attest` step ran, that the `Every attestation subject guard` step passed, and that the asset is named in the step's `subject-path`. The same applies to any release built from a commit where the workflow was edited to skip attestation.

**The `install.ps1` path** verifies the attestation too, using `gh attestation verify` when `gh` is present; see [installation](installation.md#windows) for exactly what happens when it is not.

**Ordering.** Three guards, and the release has to pass all of them. The release tag and the running version are compared as semantic versions, so a release older than the one already installed is refused rather than installed; `--allow-downgrade` turns that refusal into a deliberate install (with a warning on stderr) for a release that was withdrawn, or a build that has to be pinned while a newer one is investigated. And a release that is not final — a prerelease, an rc, a beta — is refused however new it is, so a candidate never ends up installed by a machine that asked for a stable build; `--allow-prerelease` is the deliberate opt-in, and it does not also permit an older release (a prerelease that is *also* backwards needs both flags). The prerelease check is reported as the prerelease it is, so the message names the flag that actually reaches it. Both refusals happen before anything is downloaded.

The attestation check runs *after* the download and *before* the archive is unpacked, and that order is forced rather than chosen: the lookup is keyed on the digest of the bytes that actually arrived, so it cannot happen before the transfer. Asking first, by the digest the release *reports* for the asset, would mean consulting the attacker's own metadata about the attacker's own bytes — the exact thing the digest checks exist to distrust. The cost is one wasted transfer on a release that turns out to be unattested; what it buys is that nothing nobody has vouched for is ever handed to a decompressor.

Neither flag re-installs the release you already have: being on the latest version is still "up to date". A version that cannot be ordered — a `dev` build, a tag that is not a semver — keeps upgrading unless it is identical to the release tag (ignoring a leading `v`), which reports up to date as before, and it is not read as a prerelease either; the digests still have to agree before anything is replaced.

**Bounds.** The whole run is bounded by a 12 minute budget — the release lookup, the manifest, the archive and the attestation lookup together — so an upgrade cannot sit through the sum of four per-request deadlines (twenty and a half minutes) on a link that is slow but not broken; the error names the budget, because "context deadline exceeded" on its own says which request gave up and not that the command ran out of time. The budget is longer than one transfer's own deadline, so a transfer that begins at once and would have finished is never cut short — but a slow manifest can leave the archive less than its own ten minutes, which is the budget working rather than a separate rule. Every request also carries its own context deadline (30s for the release lookup, 10 minutes for an asset transfer) that a caller can shorten or cancel, and every response is size-capped — 4 MiB of release metadata, 1 MiB of `checksums.txt`, 200 MiB of archive, and 128 MiB of what an archive inflates into. For a `.tar.gz` that last cap covers *every* entry, not just the binary: opening one inflates the entries ahead of it too, so a few KiB of highly compressible data in `README.md` would otherwise expand without limit. The attestation lookup has its own 30s deadline — applied to the attestations index, to a bundle fetched from it, and to the trust-root fetch, each separately, so a stalled Sigstore endpoint cannot hold the command open for the whole budget and then report the budget — plus a 4 MiB cap on the attestations response and 8 MiB cap on a fetched bundle, the latter applied *after* decompression, because a small compressed body could otherwise expand without limit and this is the one place a remote service chooses what a client decompresses. A stalled connection, a hung server or an oversized body fails the command instead of hanging or exhausting memory.

**Archive formats.** Windows releases ship a `.zip` and everything else a `.tar.gz`; both are unpacked, and the container decides which, not the file name. Only a regular, non-empty `ghost` or `ghost.exe` at the archive root is accepted — a directory or link entry carrying that name has no body, and installing one would leave a zero-byte executable behind a "Updated" line.

**Replacing a running binary on Windows.** Windows holds a running executable's image open, so a new binary cannot be renamed over it. The old one is renamed to `<binary>.old` first — which Windows does allow — and the new one takes the path it vacates. If the second step fails, the old binary is moved back, so a failed upgrade cannot leave an install with no binary. The `.old` file is this process's own image and cannot be deleted until the process exits, so it stays until the next upgrade reuses the name; delete it once no `ghost` is running. Unix renames over the target atomically and leaves nothing behind.

**When a write does not finish.** The replacement is staged in a temporary file beside the binary and renamed into place, so the install path only ever holds one whole binary. A write that is interrupted, a digest that does not agree, or a disk that fills all fail against the staging file: the installed binary comes out of them byte for byte unchanged and the partial file is removed, so a failed upgrade is a failed upgrade and never a half-installed ghost.

### `ghost version`

Prints the binary version:

```bash
ghost version
```

## Automatic lifecycle command

When automatic lifecycle work is enabled, the Stop hook may spawn:

```bash
ghost lifecycle <project>
```

This is an internal integration command rather than the normal way to start maintenance. It runs enabled phases in order—`reflect`, `resolve`, then `supersede`—and logs progress to the Ghost data directory. Each phase is bounded by the lifecycle timeout configured in `docs/configuration.md`.

## Development builds: refusing a real store

`GHOST_DEV_FORBID_DATA_DIR` names data directories a build that is **not a
release** refuses to open. It exists because a development build opened against a
real store migrates it to the branch's schema, and the installed release then
refuses that store as "newer than this build" until a release catches up. The
protection used to be an instruction to set the data paths on every command, and
an instruction is not a mechanism — the case that broke was a malformed
environment export that left `XDG_DATA_HOME` pointing at the real store.

```bash
# In a development shell:
export GHOST_DEV_FORBID_DATA_DIR="$HOME/.local/share/ghost"
```

- **The value is a list** of data directories, separated by the platform's list
  separator (`:` on Unix and macOS, `;` on Windows). An empty entry is a
  separator, not a directory.
- **Name the data directory itself** — `$XDG_DATA_HOME/ghost`, or
  `~/.local/share/ghost` — not its parent. The match is equality, so a store
  nobody listed is never refused.
- **Both sides are canonicalized** before they are compared: made absolute, with
  symlinks resolved and a path that does not exist yet handled. A store reached
  through a symlinked home, named relatively, or written with a trailing
  separator is the same store, and refusing one spelling only would be a guard
  that works for the spelling its author tested. (`git describe --long` is what
  `make build` stamps for the same reason: without it, a local build on a clean
  tree at a tag describes as the bare tag, which reads as a published release and
  would switch the guard off for the one binary that is definitely not one.)
- **A release build ignores the variable entirely.** "Release" is read from the
  build's own version — the same semantic-version parser `ghost upgrade` orders
  releases with — so `dev`, a `git describe` stamp, a prerelease, and anything
  that is not a semantic version are all development builds, and `0.39.0` (with
  or without a leading `v`) is not. That is what makes the variable safe to
  export into a development shell: the same environment reaches the release Ghost
  those sessions use through their MCP integration, and that one keeps working
  against the real store.

The refusal happens **before** anything touches the file, so nothing is created,
migrated or backed up — including the pre-migration copy a schema change would
otherwise write beside the database. The error names both the variable and the
directory:

```console
$ XDG_DATA_HOME=/home/ada/.local/share ghost backup --out /tmp/snap.db
error: resolve data directory: GHOST_DEV_FORBID_DATA_DIR refuses to open the data directory /home/ada/.local/share/ghost: this build is "dev", which is not a release, and a development build migrates a store it opens. Run a released ghost against this directory, or unset GHOST_DEV_FORBID_DATA_DIR
$ echo $?
1
```

**Every path into the data directory honors it**, because the check runs where
the directory is *resolved* rather than at each command that opens a store. That
covers the CLI subcommands, `ghost mcp`, `ghost mcp init`, the read-only
`ghost export` and dry-run `ghost import` — and also the paths that only write
bookkeeping into the directory: the scratch root a lifecycle run reaps, the
lifecycle-failure marker a run writes or clears, the per-project start stamp, the
Obsidian mirror's pid file, and the marker read on every session start. Nothing
is created, migrated, written or deleted in a refused directory, and a command
that resolved nothing does not fall back to a raw name and write there anyway.

Two paths behave differently, and both are deliberate:

- **The hook paths fail open.** A SessionStart/Stop hook, and the `ghost context`
  render opencode's plugin spawns, read the store and already answer every
  failure by rendering an empty block; a refused directory is one more such
  failure. The session is never blocked, no database is opened, and nothing is
  written beside it — so a development session simply has no context, while the
  release Ghost in the same session keeps serving the real store. A development
  build that is refused the store is not a broken session, it is the outcome the
  variable asked for.
- **`ghost mcp status` reports it.** The status report's job is to say what is
  wrong, so a refused directory appears as a failed line naming the variable,
  and the run exits non-zero — except under a plugin-managed install, where the
  report returns after naming the plugin and runs no store checks at all.

`ghost backup verify <file>` is the one command that reads nothing but the file
it was handed, so it resolves no data directory and has nothing to refuse.
