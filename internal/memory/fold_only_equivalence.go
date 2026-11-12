package memory

import (
	"golang.org/x/text/unicode/norm"
	"strings"
)

func foldOnlyEquivalent(a, b string) bool {
	return normalizeForCompare(a) == normalizeForCompare(b)
}

func normalizeForCompare(s string) string {
	s = strings.ToLower(norm.NFKC.String(s))
	s = strings.NewReplacer("\u2019", "'", "\u02bc", "'").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return strings.TrimRight(s, ".,;:!? ")
}
