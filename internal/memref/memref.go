// Package memref resolves memory references typed by callers.
package memref

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
)

// MinRefLen is the shortest prefix a caller may pass.
const MinRefLen = 8

// FullIDLen is the character length of a Ghost-minted memory id.
const FullIDLen = 32

// MaxMatches caps how many ids an ambiguity refusal names.
const MaxMatches = 10

// Store provides the store operations needed for ref resolution.
type Store interface {
	MemoryIDsByIDPrefix(ctx context.Context, projectID, prefix string) ([]string, error)
}

// Resolve turns one ref into a memory id in the project.
func Resolve(ctx context.Context, store Store, projectID, which, ref string) (string, error) {
	ids, err := store.MemoryIDsByIDPrefix(ctx, projectID, ref)
	if err != nil {
		return "", err
	}
	return resolveIn(ids, projectID, which, ref)
}

// ResolveIn turns one ref into one of the provided ids.
func ResolveIn(ids []string, which, ref string) (string, error) {
	return resolveIn(ids, "", which, ref)
}

func resolveIn(ids []string, scope, which, ref string) (string, error) {
	matches := make([]string, 0, len(ids))
	for _, id := range ids {
		if hasPrefixFold(id, ref) {
			matches = append(matches, id)
		}
	}
	for _, id := range matches {
		if id == ref {
			return id, nil
		}
	}
	var folded []string
	for _, id := range matches {
		if strings.EqualFold(id, ref) {
			folded = append(folded, id)
		}
	}
	switch len(folded) {
	case 0:
		// No case-insensitive match either: the prefix rules below decide.
	case 1:
		return folded[0], nil
	default:
		return "", fmt.Errorf("the %s ref %q matches %d memories whose stored ids differ only in letter case (%s), and it is spelled as neither: ids are matched case-insensitively here, so this ref cannot address either of them",
			which, ref, len(folded), strings.Join(folded, ", "))
	}
	if utf8.RuneCountInString(ref) < MinRefLen {

		return "", fmt.Errorf("the %s ref %q is %d character(s), too short to be a prefix of an id — a prefix needs %d or more, or the full id",
			which, ref, utf8.RuneCountInString(ref), MinRefLen)
	}
	switch len(matches) {
	case 0:

		return "", fmt.Errorf("%w: no memory%s has an id starting with %s (%s)", ErrNoMatch, inProject(scope), memory.RefusalArg(which, ref), which)
	case 1:
		return matches[0], nil
	}

	shown, suffix := matches, ""
	if len(shown) > MaxMatches {
		suffix = fmt.Sprintf(", and %d more", len(matches)-MaxMatches)
		shown = shown[:MaxMatches]
	}
	return "", fmt.Errorf("the %s ref %q is ambiguous%s: %s%s — pass more characters of the id to choose one",
		which, ref, inProject(scope), strings.Join(shown, ", "), suffix)
}

// ErrNoMatch indicates that no memory matches the ref.
var ErrNoMatch = errors.New("no memory matches the ref")

func inProject(scope string) string {
	if scope == "" {
		return ""
	}
	return " in project " + scope
}

func hasPrefixFold(id, ref string) bool {
	ir, rr := []rune(id), []rune(ref)
	if len(ir) < len(rr) {
		return false
	}
	return strings.EqualFold(string(ir[:len(rr)]), ref)
}

// Short returns the report form of an id.
func Short(id string) string {
	if n := utf8.RuneCountInString(id); n > MinRefLen {
		return string([]rune(id)[:MinRefLen])
	}
	return id
}
