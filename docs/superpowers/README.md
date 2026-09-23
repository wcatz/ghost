# Superpowers project records

This directory is an archive, not a second documentation site. The files here
preserve plans, design discussions, implementation reports, and evaluations as
they stood when they were written. Unchecked boxes, old status labels, removed
types, old API endpoints, and obsolete command examples are intentional parts
of the historical record; they must not be interpreted as current Ghost
behavior.

For current behavior, use the [documentation index](../README.md), the focused
pages linked from it, and the source code. In particular:

- `README.md`, `docs/installation.md`, `docs/configuration.md`, `docs/usage.md`,
  `docs/mcp.md`, and `docs/architecture.md` describe the shipped runtime.
- `docs/benchmarks.md` is the canonical benchmark index. Its sections labeled
  **Historical record** are retained for reproducibility and are not current
  routing documentation.
- [`eval/cycle/`](../../eval/cycle/) is the current graded staleness-pipeline harness. The older
  `docs/superpowers/eval/` workflow predates the CLI-harness migration and is
  retained only as an archival experiment.

Several current pages link to a historical design because it records the
reasoning behind a shipped change. A link to a historical document is not a
claim that its proposed implementation is still present. The most important
superseded seams are the direct Anthropic client, `FallbackProvider`, MCP
sampling, the Haiku tier, and OpenCode V1-only `--pure` invocation.
