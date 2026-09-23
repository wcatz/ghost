# GhostMem minimal launch rebrand — design

**Status:** Approved for planning (2026-09-23)
**Scope:** Human-facing branding only; no runtime compatibility changes.

## Problem

`Ghost` is a strong product name, but it is already strongly associated with the
Ghost blogging platform. That makes the MCP memory server harder to discover and
easier to confuse in search results, marketplace listings, and casual technical
discussion. The project needs a clearer product label without breaking the
installations, integrations, and user data that already use the `ghost` name.

## Goals

- Present the product as **GhostMem** in the primary launch surfaces.
- Reduce confusion with the Ghost blogging platform.
- Preserve every existing binary, MCP, package, URL, configuration, and data
  compatibility contract.
- Keep the change small enough to review as a branding/documentation PR.
- Leave historical records and technical identifiers factually intact.

## Non-goals

- Renaming the `ghost` binary or adding a `ghostmem` binary.
- Renaming `ghost_*` MCP tools or `ghost://` resource URIs.
- Renaming the Go module/import path or the `wcatz/ghost` repository URL.
- Renaming container images, release archives, config/data directories,
  environment variables, or plugin package IDs.
- Rewriting historical Superpowers records.
- Adding migration tooling, aliases, or a deprecation mechanism.

## Brand boundary

### Human-facing surfaces to rename

1. `README.md`: title, headings, comparison labels, and explanatory prose.
2. `docs/README.md`: title and introductory copy.
3. `overview.html`: visible title, navigation brand, headings, and explanatory
   copy.
4. GitHub repository About/description text: lead with “GhostMem” followed by
   the local-first MCP memory-server description.
5. Human-readable plugin/marketplace text:
   - `.claude-plugin/marketplace.json` description and plugin `displayName` /
     description fields.
   - `plugin/.claude-plugin/plugin.json` `displayName` and description.
   - `plugin-windows/.claude-plugin/plugin.json` `displayName` and description.

The existing `assets/ghost.png` file and image remain unchanged; only the README
alt text may use “GhostMem.”

### Compatibility surfaces to preserve

- Executable: `ghost`, `ghost mcp`, `ghost mcp init`, and all subcommands.
- Protocol: `ghost_*` tool names, `ghost://` resources, MCP server metadata,
  and embedded MCP instructions.
- Distribution: `github.com/wcatz/ghost`, Go module/import path, OCI image name,
  release/archive names, and GitHub release URLs.
- Local state: config paths, data directory, database filename, environment
  variables, cache paths, and hook commands.
- Plugin identifiers: `ghost`, `ghost-windows-amd64`, and
  `ghost-windows-arm64` package names.
- Technical documentation: `CLAUDE.md`, focused CLI/config/architecture pages,
  benchmark records, code comments, and historical Superpowers documents.

A reader may therefore see the product called GhostMem while a command example
still says `ghost`; that is intentional and avoids a breaking rename.

## Copy direction

The new public label is always written exactly as **GhostMem**. The primary
description should make the category explicit, for example:

> GhostMem — a local-first MCP memory server for Claude Code, opencode, Cursor,
> and other MCP clients.

Keep the existing explanation of local SQLite storage, cross-client memory, and
optional embeddings. Do not claim that GhostMem is a new runtime or a renamed
binary.

## User experience and compatibility

Existing installations continue to work without migration:

1. `ghost mcp init` reads the existing config and store.
2. Existing MCP clients continue to call the existing `ghost_*` tools.
3. Existing plugin package IDs and release artifacts remain installable.
4. Existing databases, snapshots, Obsidian vaults, and lifecycle hooks remain
   untouched.
5. `ghost upgrade` and release checks continue to use the existing repository
  and artifact identities.

There is no user-facing “formerly Ghost” banner, alias, or migration warning.
The old word remains only when it is part of a retained technical identifier or
an archival record.

## Validation

- Search the approved human-facing files for `Ghost` brand prose and replace it
  deliberately; do not perform a repository-wide replacement.
- Assert that the binary, MCP names, module path, repository URL, package/image
  identifiers, config paths, and environment variables are unchanged.
- Validate all modified JSON manifests and plugin/marketplace metadata.
- Run the existing Go test suite, Go vet, Markdown link/anchor checks, and
  repository manifest checks.
- Review the rendered README, `overview.html`, and plugin display metadata to
  confirm the new label is visible while technical examples remain valid.
- Review the diff to ensure no historical records or runtime source files were
  changed.

## Risks and accepted trade-offs

- The repository slug, package identifiers, and CLI remain `ghost`, so some
  technical search results and protocol metadata will still use the old word.
  This is accepted to avoid breaking integrations.
- Plugin users may need to refresh marketplace metadata to see the new display
  name, while the package ID remains unchanged.
- The existing ghost icon remains associated with the old brand until a separate
  visual-identity decision is made.

## Acceptance criteria

- Primary public launch surfaces consistently say **GhostMem**.
- No runtime, MCP, package, URL, config, or data identifier is renamed.
- Existing installation and upgrade paths remain valid.
- Historical documentation remains archival and is not rewritten.
- Automated validation passes, and the final diff contains only the approved
  human-facing branding changes.
