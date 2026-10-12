package mcpserver

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxProjectIDLen bounds the byte length of a caller-supplied `project_id` at the
// MCP write boundary (#1038).
//
// 4096 is the largest path a POSIX filesystem can address (`PATH_MAX`), and a
// project id is routinely a filesystem path — `ensureProjectFor` passes a caller's
// argument through as the id, the name and the path. A value wider than that is
// not a checkout anyone can open or a project anyone can name; it is a payload
// wearing a project's clothes, and `ghost_list_projects` renders the name of every
// project uncapped, so one accepted id turns into megabytes of listing.
//
// It deliberately does NOT live in `memory.CheckImportedProjectID`, which has no
// length bound ON PURPOSE (#844): `ghost export` writes a project id into the
// artifact and `ghost import` refuses a project the predicate refuses, and a
// refusal there CASCADES — a failed project step never records its id, so every
// memory, task and decision naming it is then rejected and a whole project is
// lost on restore. So the bound is a WRITE rule, like `validateTags`'s shape rule:
// it refuses a NEW project, and the importer keeps the bound-free predicate it
// needs to round-trip a legacy or artifact id. The gate at the call site is
// creation too — an existing project addressed by a long recorded path resolves
// before this is asked.
const maxProjectIDLen = 4096

// validateRequiredText refuses a required free-text argument a write tool must
// not store: a value that is empty or whitespace-only, one that is not valid
// UTF-8, or one carrying a NUL byte (#1038).
//
// It is the one rule behind every free-text argument the write tools require —
// memory content on the two save tools and the update, and a task's or a
// decision's title — because the old check at each site was `value == ""`, which
// accepts "   ", "\n\t" and "a\x00b" and stores them. A blank memory is a row
// that answers a later search with nothing, and a blank title is a task no
// listing can name.
//
// The sentence names the FIELD and never the value. That is not decoration: an
// invalid-UTF-8 or NUL-bearing argument is exactly the caller text this codebase
// keeps out of its answers (the renderer neutralises a stored value, but an error
// string is not rendered), so the refusal must be about the class, not the bytes.
// The field name is enough for a caller to act on: it is the one it passed.
func validateRequiredText(field, value string) error {
	// TrimSpace covers every Unicode space, so a value of one ideographic space
	// is blank too, not only an ASCII one.
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required and must not be empty or whitespace-only", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s must not contain a NUL byte", field)
	}
	return nil
}

// validateProjectIDArgument is validateRequiredText for the `project_id`
// argument, plus the length cap, so blank/malformed text and an oversized id are
// one call at the write boundary.
//
// The caller is the creation-gated block inside `ensureProjectFor`, so an id that
// resolves to a project the store ALREADY holds is never judged here — see the
// comment there, and maxProjectIDLen for why the length rule is a write rule.
func validateProjectIDArgument(projectID string) error {
	if err := validateRequiredText("project_id", projectID); err != nil {
		return err
	}
	if len(projectID) > maxProjectIDLen {
		return fmt.Errorf("project_id must be at most %d bytes, got %d — it is a project name or a checkout path, not a document",
			maxProjectIDLen, len(projectID))
	}
	return nil
}
