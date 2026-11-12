package supersede

import (
	"regexp"

	"github.com/wcatz/ghost/internal/resolve"
)

var retireMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bno longer\b`),
	regexp.MustCompile(`(?i)\bretir(?:e|es|ed|ing|al|ement)\b`),
	regexp.MustCompile(`(?i)\bremov(?:e|es|ed|ing|al)\b`),
	regexp.MustCompile(`(?i)\bdeprecat(?:e|es|ed|ing|ion)\b`),
	regexp.MustCompile(`(?i)\bobsolete\b`),
	regexp.MustCompile(`(?i)\breplac(?:e|es|ed|ing|ement)\b`),
	regexp.MustCompile(`(?i)\b(?:supersed(?:e|es|ed|ing)|supersessions?)\b`),
	regexp.MustCompile(`(?i)\bdrop(?:s|ped|ping)?\b`),
	regexp.MustCompile(`(?i)\brelax(?:es|ed|ing)?\b`),
	regexp.MustCompile(`(?i)\bloosen(?:s|ed|ing)?\b`),
	regexp.MustCompile(`(?i)\blift(?:s|ed|ing)?\b`),
	regexp.MustCompile(`(?i)\bwaiv(?:e|es|ed|ing)\b`),
	regexp.MustCompile(`(?i)\bmust now\b`),
	regexp.MustCompile(`(?i)\bnot (?:be )?required\b`),
	regexp.MustCompile(`(?i)\bexception to\b`),
}

// VetoSupersede reports whether a candidate pair should not form a supersedes edge.
func VetoSupersede(c Candidate) (reason string, vetoed bool) {
	imperative, _ := resolve.VetoKeepImperative(c.OlderContent)
	if imperative == "" {
		return "", false
	}
	if namesRetirement(c.NewerContent) {
		return "", false
	}
	return "older note states a rule (" + imperative + ") the newer note does not retire", true
}

func namesRetirement(content string) bool {
	for _, re := range retireMarkers {
		if re.MatchString(content) {
			return true
		}
	}
	return false
}
