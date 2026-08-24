# Resolve's sampling path after the 2026-07-28 Sampling deprecation

**Status:** Draft (2026-08-24) — closes #349.
**Author:** Wayne (wcatz)
**Builds on:** #312 (CLI fallback for `ghost_resolve`); `2026-07-26-classifier-fallback.md`; go-sdk 1.7.0 compat fix (SEP-2322 negotiation).

## 1. Problem

`ghost_resolve`'s live-session path classifies through MCP sampling
(`ai.SamplingProvider` wrapping a synchronous `ServerSession.CreateMessage`,
internal/ai/sampling_provider.go:34) because that spends zero credits and uses
the calling session's own model. MCP specification revision **2026-07-28**
(now the published current revision) deprecates Sampling outright (SEP-2577),
and go-sdk 1.7.0 already forbids the synchronous call shape once any client
negotiates the new protocol. Today this is latent — no real-world client
negotiates 2026-07-28 by default — but the mechanism is now formally on a
removal track, and the spec names the replacement we already ship elsewhere:
call LLM provider APIs directly.

The question is not "how do we keep sampling working" but "which provider
should the live-session path standardize on."

## 2. Grounding: read against the actual specs and SDK (researched 2026-08-24)

Primary sources consulted: the MCP 2026-07-28 changelog, the feature-lifecycle
policy, the SEP index, the go-sdk v1.7.0 release notes and godoc, the Go SDK
client guide, and (for corroboration) the Python SDK's deprecation table.

1. **Sampling is deprecated, not removed — with a clock.**
   [2026-07-28 changelog](https://modelcontextprotocol.io/specification/2026-07-28/changelog):
   SEP-2577 puts Roots, Sampling, and Logging in the Deprecated state; they
   "remain fully functional during the deprecation window but new
   implementations should not add support for them." Under the
   [feature lifecycle policy](https://modelcontextprotocol.io/community/feature-lifecycle)
   a feature stays Deprecated ≥ 12 months from its deprecating revision — so
   removal is eligible no earlier than **2027-07-28**. The changelog's own
   suggested migration for Sampling: *"integrate directly with LLM provider
   APIs instead of Sampling."*
2. **go-sdk 1.7.0 marks our exact call site Deprecated and gates it by protocol
   version.** [`ServerSession.CreateMessage`](https://pkg.go.dev/github.com/modelcontextprotocol/go-sdk/mcp)
   carries *"Deprecated … Migrate to calling LLM provider APIs directly from
   your server."* On protocol ≥ 2026-07-28, server-to-client requests are no
   longer standalone JSON-RPC calls — a tool must return an
   `InputRequiredResult` (MRTR, SEP-2322) — and the SDK rejects the
   synchronous call (`assertServerInitiatedRequestAllowed`; already documented
   in internal/mcpserver/mcpserver_test.go:1376). On legacy protocol versions
   the SDK's `serverMultiRoundTripMiddleware` bridges `InputRequiredResult`
   back down to `ServerSession.CreateMessage` internally, so the MRTR *shape*
   works on both generations — see
   [the client guide](https://go.sdk.modelcontextprotocol.io/client).
3. **The deprecation follows the capability, not just the transport shape.**
   The Python SDK's table makes this concrete: even MRTR-delivered
   `create_message` fires `MCPDeprecationWarning`, and on a modern connection
   the send fails outright (*"no back-channel for server-initiated requests"*)
   — https://py.sdk.modelcontextprotocol.io/deprecated/. Rewriting
   `SamplingProvider` onto SEP-2322 would therefore buy compatibility past the
   client upgrade while still parking us on a feature slated for removal.
4. **All four Tier-1 SDKs speak 2026-07-28 as of the spec date**
   ([announcement](https://blog.modelcontextprotocol.io/posts/2026-07-28));
   [v1.7.0](https://github.com/modelcontextprotocol/go-sdk/releases/tag/v1.7.0)
   shipped it 2026-07-28 with MRTR replacing server-initiated calls. Client
   adoption lags the SDKs, which is why the breakage is latent today.
5. **SEP-2577 is Final** — https://modelcontextprotocol.io/seps — not
   in-review; there is no version of it where Sampling survives.

## 3. How resolve composes providers today

| Path | Composition | Writes |
| --- | --- | --- |
| Headless (`ghost resolve`, stop-hook detached spawn) | `buildClassifyProvider` (cmd/ghost/main.go:616): Anthropic primary when `ANTHROPIC_API_KEY` set, CLI (`claude`→`opencode`) secondary, dry-run-only on fallback; **CLI as sole full-write primary when no key** | allowed unless any answer came from the fallback (`internal/resolve/resolve.go:128`) |
| Live session (`ghost_resolve` MCP tool) | `ai.NewAlwaysFallbackProvider(ai.NewSamplingProvider(req.Session), ai.NewCLIClient(), true)` (internal/mcpserver/mcpserver.go:1008) — sampling primary, `claude -p` fallback, dry-run-only on fallback | allowed **only** when sampling answered |

Two incidental facts fall out of reading this code:

- The live path falls back to `ai.NewCLIClient()` — **claude only**, not
  `ai.CLIProvider` — so an opencode-only machine silently has no live-session
  fallback today, unlike every headless path.
- The reflection precedent (cmd/ghost/main.go:380) already treats the CLI as a
  trusted, write-capable tier, and headless resolve trusts CLI answers fully
  when no API key exists. "CLI answers are dry-run-only" is a live-session-only
  convention, inherited from sampling being the intended primary.

## 4. Options

**A) Keep sampling until forced removal.** Zero work now. When a connected
client negotiates ≥ 2026-07-28, every classification errors at the SDK gate,
falls to `claude -p`, gets marked `FromFallback`, and `apply:true` silently
downgrades to advisory output ("resolved 0") forever after — the client never
downgrades its protocol. We also carry a provider the spec tells new code not
to adopt, plus the eventual removal-day cleanup regardless.

**B) CLIProvider primary, sampling retired from the live path.** Compose the
live tool exactly like headless's no-key branch:
`ai.NewFallbackProvider(ai.NewCLIProvider(), nil, false)`. Matches the spec's
recommended migration verbatim, matches the reflection/headless precedent,
keeps the zero-API-credits property (subscription-billed `claude`/`opencode`),
and incidentally fixes the claude-only fallback gap. Cost: machines with
neither CLI binary lose live-session resolve entirely (headless unaffected) —
acceptable for a single maintainer-user who always has both installed.

**C) Direct Anthropic API always.** Breaks the documented no-credits property
of `ghost_resolve` (tool description, CLAUDE.md, mcp_instructions all promise
it). Rejected without hesitation; it is the one option that makes the tool
strictly worse than headless.

**D) Hybrid precedence sampling → CLI → logged error.** This is today's
composition plus better messaging. It inherits all of A's forced-migration
problems and keeps a deprecated fast path alive for a latency difference
measured in milliseconds on a background-classification tool. Complexity
without a payoff.

**E) Rewrite `SamplingProvider` onto SEP-2322 MRTR.** The previously-assumed
fix, examined and rejected: finding 3 — the deprecation follows the
capability, so MRTR-shaped sampling still dies at removal time; the rewrite is
the most SDK-internals-sensitive option (the test file already documents how
fragile the negotiate-fallback dance is); and it keeps writes gated on clients
implementing a feature the ecosystem is being told to drop. Maximum effort,
shortest remaining lifespan.

## 5. Decision: B

Standardize `ghost_resolve`'s live-session path on `ai.CLIProvider` as sole,
full-write primary — byte-for-byte the same composition headless uses when no
API key is configured — and delete `ai.SamplingProvider`. Rationale, honestly:

- Single maintainer-user, both CLI binaries always present: the sampling
  fast path buys nothing real today and is the only reason the live write path
  can silently degrade to advisory-only.
- The failure mode we're removing is the worst kind for this repo: silent
  capability downgrade discovered weeks after a host updates its SDK.
- Every other classify surface in ghost either already prefers the CLI or
  accepts it as full-write; this change removes the last special case rather
  than adding a new tiering rule.

## 6. Migration plan

One small PR, no schema or ranking changes:

1. `internal/mcpserver/mcpserver.go:1008` — replace the
   `NewAlwaysFallbackProvider(NewSamplingProvider(...), NewCLIClient(), true)`
   composition with a pre-checked `ai.NewCLIProvider()`: return a clean tool
   error ("requires a `claude` or `opencode` binary") before running when
   `!Available()`, else `NewFallbackProvider(cli, nil, false)`.
2. Same file, line 981 — rewrite the tool description (drop the sampling
   promise; state the CLI-provider/no-credits contract).
3. Delete `internal/ai/sampling_provider.go` + its test; rework
   `TestGhostResolve_FallsBackToCLIWhenSamplingUnavailable` into
   "writes succeed on a client with no sampling handler" (the guard flip means
   `apply:true` now writes there); retire the sampling happy-path test and
   `discoverlessTransport` scaffolding if nothing else consumes it.
4. Docs: CLAUDE.md classifier-fallback bullet, the mcp_instructions
   `ghost_resolve` blurb, close-comment on #349.

Effort: **half a day** including `go vet ./...` / `go test ./...` and the doc
edits. Nothing here touches `internal/resolve` semantics — the `anyFallback`
guard simply stops firing (nil secondary).

## 7. What breaks if we do nothing

Nothing today. Then, whenever any major host ships a 2026-07-28-negotiating
client (SDKs are ready; adoption is the only variable): live-session
`ghost_resolve --apply` stops writing — permanently, for that client, with no
error surfaced to the model beyond the appended "apply skipped" stderr note —
and ghost carries a deprecated-codepath provider until someone does this same
migration under time pressure. Headless paths are unaffected throughout, so
the blast radius is "one tool quietly loses its write half," not data loss.

## 8. Proposed decision record

- **Title:** Live-session `ghost_resolve` classifies via CLI provider; MCP sampling retired
- **Decision:** `ghost_resolve`'s MCP tool composes `ai.NewCLIProvider()` (claude→opencode, subscription-billed) as sole full-write primary, identical to the headless no-key branch of `buildClassifyProvider`; `ai.SamplingProvider` is deleted. Machines without a CLI binary get a clear tool error; no API credits spent on any path.
- **Rationale:** Spec 2026-07-28 deprecates Sampling (SEP-2577, ≥12-month window, removal eligible 2027-07-28) and names direct provider integration as the migration; go-sdk 1.7.0 already rejects the synchronous CreateMessage shape on new protocol versions, so keeping sampling guarantees a silent live-write downgrade on client upgrade. CLI-as-full-write matches existing headless/reflection precedent and preserves the tool's zero-credits property.
- **Alternatives rejected:** Keep sampling until forced removal (silent permanent degradation + deferred same migration); direct Anthropic API (breaks no-credits property); sampling→CLI hybrid precedence (complexity, same forced migration); SEP-2322/MRTR rewrite of SamplingProvider (most effort, deprecation follows the capability not the transport, shortest remaining lifespan).
