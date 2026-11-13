//go:build unix

package mcpinit

// The user-owned files ghost rewrites outside the Claude settings.json and the
// codex config.toml pair — codex hooks.json, the opencode plugin, the goose
// package and the MEMORY.md redirect — are the half of #551 that #621 left
// open. Every one of them is rewritten with the whole document ghost wants, so
// an interrupted write is not a cosmetic diff: whatever host reads the file
// next sees a truncated config, and a user hook that was in it is gone with no
// copy anywhere.
//
// Two properties per file pin that none of them goes through a bare
// os.WriteFile, which opens the target for writing and truncates it before the
// first byte of the new content lands.
//
//   - read-only target: the target is never opened O_WRONLY, so the repair
//     lands by rename and the target keeps its exact mode. The mode is the
//     observable; the property behind it is that a refused or failed write
//     cannot leave a half-written file. This is the same contract
//     TestWriteFileAtomicReplacesReadOnlyFile pins for codex config.toml.
//   - open reader keeps its snapshot: a reader that already holds the file open
//     still reads the complete previous document after the install. A
//     truncate-in-place write makes that handle read the new document, or a
//     prefix of it, instead. Rename is what keeps an in-flight reader
//     consistent, which is the case a codex or goose process starting at the
//     same moment as `ghost mcp init` is in.
//
// Unix only: on Windows os.Chmod(0400) sets FILE_ATTRIBUTE_READONLY, which
// blocks the rename that replaces the file, and Go opens files there without
// FILE_SHARE_DELETE, so a rename cannot replace a file a reader holds open.
// Neither property is observable on that platform.
