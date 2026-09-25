package memory

const builtinSeedContent = "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

// CanonicalOriginSource applies the small compatibility correction needed by
// read-only context paths that can render a database before the v15 migration
// has run. A legacy builtin row is identifiable by its frozen shipped content;
// all other manual rows retain their source.
func CanonicalOriginSource(source, content string) string {
	if source == "manual" && content == builtinSeedContent {
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
// is treated as non-user material and rendered verbatim rather than being
// silently granted trust if a newer writer slips past the schema check.
func OriginClass(source string) (own bool, label string) {
	if source == "manual" {
		return true, ""
	}
	return false, source
}
