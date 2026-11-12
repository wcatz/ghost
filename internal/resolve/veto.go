// Deterministic KEEP vetoes: the cheap, free half of resolve's KEEP bias.
package resolve

import "regexp"

type vetoPattern struct {
	pattern string
	re      *regexp.Regexp
}

var (
	keepVetoImperatives = []vetoPattern{
		{"never", regexp.MustCompile(`(?i)\bnever\b`)},
		{"do not", regexp.MustCompile(`(?i)\bdo not\b`)},
		{"don't", regexp.MustCompile(`(?i)\bdo(?:n[’'])t\b`)},
		{"must", regexp.MustCompile(`(?i)\bmust\b`)},
		{"always", regexp.MustCompile(`(?i)\balways\b`)},
		{"required", regexp.MustCompile(`(?i)\brequired\b`)},
	}

	keepVetoOpenMarkers = []vetoPattern{
		{"not yet", regexp.MustCompile(`(?i)\bnot yet\b`)},
		{"outstanding", regexp.MustCompile(`(?i)\boutstanding\b`)},
		{"still pending", regexp.MustCompile(`(?i)\bstill pending\b`)},
		{"still open", regexp.MustCompile(`(?i)\bstill open\b`)},
		{"still stale", regexp.MustCompile(`(?i)\bstill stale\b`)},
		{"unresolved", regexp.MustCompile(`(?i)\bunresolved\b`)},
		{"todo", regexp.MustCompile(`(?i)\btodo\b`)},
	}

	keepVetoFixLanded = []vetoPattern{
		{"fixed in commit", regexp.MustCompile(`(?i)\bfixed in commit\b`)},
	}
)

// VetoKeep reports whether content carries a deterministic KEEP signal, and
func VetoKeep(content string) (reason string, vetoed bool) {
	if reason, vetoed := VetoKeepImperative(content); vetoed {
		return reason, true
	}
	for _, p := range keepVetoOpenMarkers {
		if p.re.MatchString(content) {
			return p.pattern, true
		}
	}
	for _, p := range keepVetoFixLanded {
		if p.re.MatchString(content) {
			return p.pattern, true
		}
	}
	return "", false
}

// VetoKeepImperative reports whether content states a standing rule or
func VetoKeepImperative(content string) (reason string, vetoed bool) {
	for _, p := range keepVetoImperatives {
		if p.re.MatchString(content) {
			return p.pattern, true
		}
	}
	return "", false
}
