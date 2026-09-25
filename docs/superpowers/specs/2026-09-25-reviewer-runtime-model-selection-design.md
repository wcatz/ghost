# Runtime Free-Model Selection for the CI Reviewer

**Issue:** #597
**Status:** Implemented design for the reviewer workflow

## Problem

The reviewer prefers the anonymous OpenCode free tier, but that tier changes frequently.
A model can be removed from the catalog, reject the read-only custom agent with
`FreeTierError`, or hit a rate limit. Replaying a fixed list wastes review time and can
fail before a currently usable model is tried.

## Design

The `review` job keeps one ordered `MODELS` environment variable as the maintainer-facing
free-model preference and fallback. A selection phase runs before the real review and
invokes `opencode models` from `$RUNNER_TEMP`; OpenCode therefore never discovers project
config or plugins from the pull-request checkout during selection. The catalog command
intentionally uses the normal model service (not `--standalone`): the private standalone
service has no provider catalog on a keyless runner, while the normal command can query one
when the runner exposes it. The working directory still isolates project configuration.
Probes use `--standalone` so they cannot attach to a project-local service.

The catalog is reduced to anonymous free-tier candidates:

- provider-qualified `opencode/*` ids whose model part ends in `-free`; and
- the exact `opencode/big-pickle` id.

`opencode-go/*` is never eligible for the free path, even when its model name ends in
`-free`, because it is a paid endpoint. Other providers and non-free ids are not selected.
When the catalog is available, the static `MODELS` ids that are present retain their listed
order, followed by other catalog free ids in catalog order. Safe static entries missing from
the catalog are retained as last-resort free fallbacks after the discovered candidates; if
catalog discovery fails, the filtered static list is used directly.

Each free candidate is probed with a tiny prompt using the same `--agent ghost-reviewer`
read-only agent and an explicit `-m`. Probes use `--standalone` and execute from
`$RUNNER_TEMP`. A clean non-empty JSON event stream is a pass. A `FreeTierError`, rate limit,
or other probe error skips the candidate. `Agent not found` is fatal and is never converted
into a fallback to OpenCode's default, write-capable agent.

The probe phase records every free candidate that accepts the agent, capped at eight so
selection and retry budgets remain finite. The free review step tries those models in
order, using the first one initially and continuing to the next only when a free review
fails for a non-agent reason. Every candidate reply is passed through the trusted base
`extract_findings_from_reply` and `validate` functions before it is accepted; malformed,
tool-only, or off-contract replies are treated as candidate failures. If every free
candidate fails at either stage, a separate paid step invokes the single job-level
`PAID_MODEL`. The paid route is deliberately `opencode-go/muse-spark-1.3-contributor`: it is
the cheapest suitable current Go coding model and is not Kimi. The `OPENCODE_GO_API_KEY`
secret is mapped to `OPENCODE_API_KEY` only in the paid step's environment, never in the
free step or a free child. A percent-escaped warning names the paid model and the free
failure reason. A bounded selection timeout is handed to the paid step; an explicit
`Agent not found` marker remains fatal.

The workflow logs the selected free or paid model and logs every considered free candidate
that was skipped. Provider/model text is normalized and percent-escaped before it is placed
in a GitHub Actions workflow command. The post-review command and trusted script boundary
are unchanged.

## Safety invariants

- The free catalog, probe, and review paths are credential-free.
- The paid secret is supplied only through the separate fallback step environment, never
  materialized in a run script, a free-model process, or a later GitHub step.
- A reply is accepted only after trusted extraction and schema validation; invalid free
  replies continue to the next free candidate and invalid paid replies fail the review.
- The free candidate list and retry deadlines are capped so the paid step retains time.
- The agent file remains in `$HOME/.config/opencode/agent` and keeps explicit read-only
  tool permissions.
- Every free catalog/probe command runs from `$RUNNER_TEMP`, not the PR workspace.
- A missing agent is a hard error and never triggers the paid fallback.
- Cancellation and a changed PR head stop paid/model/post steps; a stale head check fails
  the run, and the post step rechecks the head immediately before creating a review, so
  superseded runs cannot spend or post.
- Trusted post-review scripts continue to come from the default branch.

## Verification

The workflow is checked with `actionlint` and the review job on the implementation PR is
the end-to-end proof. The PR body records the selection and skip log lines from that run.
Focused behavior harnesses exercise catalog ordering, free exhaustion, invalid-reply
validation, probe skips, fatal agent errors, workflow-command escaping, and the paid
step-only secret boundary. Their mutants are checked by temporarily reverting the
corresponding behavior.
