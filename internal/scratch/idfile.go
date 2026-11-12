// The follow-up id file: the list `ghost supersede --reassess --apply` and
package scratch

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const idFileStaleAge = 14 * 24 * time.Hour

var reassessIDFile = regexp.MustCompile(
	`^supersede-reassess-[A-Za-z0-9._-]{1,40}-[0-9]{8}T[0-9]{6}Z-([0-9]+)-[0-9a-f]{16}\.ids$`)

// ReassessTargetsName builds the follow-up id file's name: the repair, the
func ReassessTargetsName(projectName string) (string, error) {
	suffix, err := UniqueSuffix()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("supersede-reassess-%s-%s-%s.ids",
		sanitizeNamePart(projectName), time.Now().UTC().Format("20060102T150405Z"), suffix), nil
}

func isReassessIDFile(name string) bool {
	match := reassessIDFile.FindStringSubmatch(name)
	if match == nil {
		return false
	}
	return isPidSegment(match[1])
}

func sanitizeNamePart(s string) string {
	const maxLen = 40
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= maxLen {
			break
		}
	}
	if b.Len() == 0 {
		return "project"
	}
	return b.String()
}
