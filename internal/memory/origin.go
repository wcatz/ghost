package memory

const builtinSeedContent = "NEVER add Co-Authored-By or any AI attribution to commit messages. All commits belong to the user."

// CanonicalOriginSourceForProject rewrites a legacy global builtin seed row to source "builtin".
func CanonicalOriginSourceForProject(projectID, source, content string) string {
	if source == "manual" && projectID == GlobalProjectID && content == builtinSeedContent {
		return "builtin"
	}
	return source
}

// OriginClass reports whether source is direct user material and, if not, its label.
func OriginClass(source string) (own bool, label string) {
	if source == "manual" {
		return true, ""
	}
	return false, source
}
