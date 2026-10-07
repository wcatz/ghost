package memory

const builtinSeedContent = "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

// CanonicalOriginSourceForProject applies the small compatibility correction
// needed by read-only context paths that can render a database holding a row
// in the shape a pre-v15 build wrote, before the v15 migration has corrected
// it. A legacy builtin row is identifiable by two facts together: it sits in
// the reserved GlobalProjectID, and its content is the frozen shipped seed.
//
// The project check is not redundant with the content check. Content alone
// cannot tell the shipped rule from a memory a user wrote that happens to
// contain the same sentence — a project row doing that is direct user material,
// and relabelling it "builtin" both misattributes it and removes the
// absence-of-a-tag that marks a row as the user's own. The v15 migration has
// the same two-part condition for the same reason, so this stays a rewrite of
// the shipped global seed only. Every other row keeps its source.
//
// Callers pass the row's own project_id; a read path that has not loaded one
// cannot use this to decide the row is a shipped global rule.
func CanonicalOriginSourceForProject(projectID, source, content string) string {
	if source == "manual" && projectID == GlobalProjectID && content == builtinSeedContent {
		return "builtin"
	}
	return source
}

// OriginClass interprets a memories.source value for surfaces that inject or
// list stored material. The first result reports whether the source is direct
// user material; the second is the label to show when it is not.
//
// Keeping this classification beside the schema vocabulary prevents the
// SessionStart banner and MCP listings from growing different trust rules for
// the same row. `manual` is the direct-user marker; `builtin` is deliberately
// separate so Ghost-shipped rules cannot inherit that trust. An unknown value
// is treated as non-user material, and printed through assemble.SourceLabel
// (a quoted token when it is not a plain identifier) rather than being
// silently granted trust if a newer writer slips past the schema check.
func OriginClass(source string) (own bool, label string) {
	if source == "manual" {
		return true, ""
	}
	return false, source
}
