package memory

// OriginClass interprets a memories.source value for surfaces that inject or
// list stored material. The first result reports whether the source is direct
// user material; the second is the label to show when it is not.
//
// Keeping this classification beside the schema vocabulary prevents the
// SessionStart banner and MCP listings from growing different trust rules for
// the same row. An unknown value is treated as non-user material and rendered
// verbatim rather than being silently granted trust if a newer writer slips
// past the schema check.
func OriginClass(source string) (own bool, label string) {
	if source == "manual" {
		return true, ""
	}
	return false, source
}
