// The follow-up id file: the list `ghost supersede --reassess --apply` and
// `ghost supersede --withdraw … --apply` leave under the root so the resolve
// command that completes the repair can read it back through --only-file.
//
// It is a file rather than a line in the report because the list is too long
// to retype and the operator runs it in a second command, possibly days later,
// so it lives in the scratch root — the one place Ghost owns, creates 0700 and
// already reaps. It is therefore this package's to name, to recognise and to
// eventually collect: the writer in cmd/ghost and the reaper in Reap share
// one definition of the name instead of spelling it twice.
package scratch

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

// idFileStaleAge is how old a follow-up id file must be before Reap collects
// it.
//
// It is deliberately NOT Reap's maxAge argument. A per-invocation directory is
// abandoned scratch: it has no value to anyone the moment its owner is gone,
// so its age bound is whatever the caller asked for. The id file is the
// opposite — it is the operator's PENDING REPAIR, and the reason it is written
// under the root at all is that whoever runs the repair may come back days
// later, so a caller reaping with no age bound must not take it either.
// Two weeks is that window with room to spare, and it is still a bound: every
// run leaves one more file, each counts against scratch.max_bytes, and nothing
// else in Ghost ever removes them.
const idFileStaleAge = 14 * 24 * time.Hour

// reassessIDFile is the exact shape ReassessTargetsName writes, anchored at
// both ends: the fixed repair name, a project name sanitized to [A-Za-z0-9._-]
// (1–40 characters), the UTC second it was written in, UniqueSuffix's
// <pid>-<16 hex> run token, and the .ids extension.
//
// The pid segment is captured rather than spelled in the pattern, because
// isPidSegment decides it — the one rule for what a pid segment may look like,
// shared with the per-invocation directory name. A pid Ghost never has is
// evidence the file is somebody else's, and the file is removed on the
// strength of its name alone.
//
// It is strict for the reason isScratchDirName is: $GHOST_SCRATCH_DIR may
// name a directory somebody else writes to, so for an entry with no owner
// marker the NAME is the only ownership evidence there is. Near misses that
// live in the same directory do not qualify — a foreign list, a name one
// segment longer, uppercase hex, a timestamp missing its Z — and neither does
// anything that is not a regular file (Reap checks that before asking this).
var reassessIDFile = regexp.MustCompile(
	`^supersede-reassess-[A-Za-z0-9._-]{1,40}-[0-9]{8}T[0-9]{6}Z-([0-9]+)-[0-9a-f]{16}\.ids$`)

// ReassessTargetsName builds the follow-up id file's name: the repair, the
// project, the UTC second for a human reading a directory listing, and a
// per-run token so no two runs — or anything else in the root — can hold the
// same name.
//
// It is here rather than in cmd/ghost because two things must agree on this
// name: the writer that creates the file and Reap, which decides when it may
// be collected. One implementation, two consumers — the same argument
// UniqueSuffix makes for being in this package at all. The exported name is
// what cmd/ghost's test-pinnable variable points at.
//
// Every segment it emits is one reassessIDFile accepts; that correspondence is
// what TestReassessTargetsNameIsRecognisedByIDFileRule holds together, and
// breaking it would mean Reap silently never collects what the writer left.
func ReassessTargetsName(projectName string) (string, error) {
	suffix, err := UniqueSuffix()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("supersede-reassess-%s-%s-%s.ids",
		sanitizeNamePart(projectName), time.Now().UTC().Format("20060102T150405Z"), suffix), nil
}

// isReassessIDFile reports whether name has exactly the shape
// ReassessTargetsName writes, the pid segment included: a pid Ghost never has
// is a name Ghost never wrote, and a match here is what authorizes a deletion.
// It answers a question about a name alone, so a caller deciding whether to
// REMOVE something must pair it with a regular-file check: a directory or link
// wearing the name is not the file Ghost wrote.
func isReassessIDFile(name string) bool {
	match := reassessIDFile.FindStringSubmatch(name)
	if match == nil {
		return false
	}
	return isPidSegment(match[1])
}

// sanitizeNamePart renders a project name safe to embed in a file name. The
// name is free text — `ghost resolve --project` takes the next argument
// verbatim so a dash-leading project name works — and a name holding a
// separator would otherwise write the file outside the scratch root, or not at
// all. The result is 1–40 characters of [A-Za-z0-9._-], which is exactly the
// class reassessIDFile's project segment accepts: sanitizing narrower than the
// recogniser is safe, sanitizing wider would break the round trip.
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
