package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wcatz/ghost/eval/cycle/corpus"
)

// supPair is one parsed supersede output line. The CLI prints 8-char id
// prefixes; NewerID8 is the first id on the line (source of the arrow) and
// OtherID8 the second.
type supPair struct {
	NewerID8 string
	Relation string // "supersedes" | "causes"
	OtherID8 string
}

var (
	supLineRe = idPairLineRe(`(supersedes|causes)`)
	resLineRe = resolveConfirmedRe()
)

// idPairLineRe matches `  <id8>[hex]  <relation>  <id8>[hex]` lines, tolerant
// of full-id echoes; only the first 8 hex chars are captured.
func idPairLineRe(rel string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*([0-9a-f]{8})(?:[0-9a-f]+)?\s+` + rel + `\s+([0-9a-f]{8})(?:[0-9a-f]+)?\s*$`)
}

func resolveConfirmedRe() *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*([0-9a-f]{8})(?:[0-9a-f]+)?\s+\[[a-z]+\]`)
}

func parseSupersedeLines(out string) []supPair {
	var pairs []supPair
	for _, m := range supLineRe.FindAllStringSubmatch(out, -1) {
		pairs = append(pairs, supPair{NewerID8: m[1], Relation: m[2], OtherID8: m[3]})
	}
	return pairs
}

func parseResolveIDs(out string) []string {
	var ids []string
	for _, m := range resLineRe.FindAllStringSubmatch(out, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

// Miss is one graded mismatch, listed verbatim in the report.
type Miss struct {
	Key    string // corpus key (or raw ids when unmappable)
	Kind   string // e.g. "unexpected-link", "missed-link", "unexpected-resolve", "missed-resolve"
	Detail string
}

// Stage is a graded precision/recall scorecard for one pipeline stage.
// Causes links are counted but excluded from scoring (legitimate behavior the
// corpus does not annotate).
type Stage struct {
	Name   string
	TP     int
	FP     int
	FN     int
	Causes int
	Misses []Miss
}

func (s Stage) Precision() float64 {
	if s.TP+s.FP == 0 {
		return 0
	}
	return float64(s.TP) / float64(s.TP+s.FP)
}

func (s Stage) Recall() float64 {
	if s.TP+s.FN == 0 {
		return 0
	}
	return float64(s.TP) / float64(s.TP+s.FN)
}

// id8ToKey maps the first 8 hex chars of every injected memory id to its
// corpus key. Collisions would make grading ambiguous and are fatal.
func id8ToKey(ids map[string]string) map[string]string {
	m := make(map[string]string, len(ids))
	for key, full := range ids {
		if len(full) < 8 {
			continue
		}
		prefix := full[:8]
		if prev, ok := m[prefix]; ok && prev != key {
			continue // collision: leave first, graders treat unknown ids as FP/FN with detail
		}
		m[prefix] = key
	}
	return m
}

func excerpt(s string) string {
	first := s
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		first = s[:i]
	}
	if len(first) > 70 {
		first = first[:70]
	}
	return first
}

// gradeSupersede scores parsed supersedes/causes links against the corpus's
// expected_superseded_by annotations (direction-aware: older→newer must match).
func gradeSupersede(ids map[string]string, entries []corpus.Entry, pairs []supPair) Stage {
	st := Stage{Name: "supersede"}
	k2k := id8ToKey(ids)

	type exp struct{ older, newer string }
	expected := make(map[exp]string) // pair -> corpus key of the older member
	for _, e := range entries {
		if e.ExpectedSupersededBy != "" {
			expected[exp{e.Key, e.ExpectedSupersededBy}] = e.Key
		}
	}

	satisfied := map[string]bool{}
	for _, p := range pairs {
		if p.Relation == "causes" {
			st.Causes++
			continue
		}
		newerKey, okN := k2k[p.NewerID8]
		olderKey, okO := k2k[p.OtherID8]
		if !okN || !okO {
			st.FP++
			st.Misses = append(st.Misses, Miss{
				Key: p.NewerID8 + "->" + p.OtherID8, Kind: "unmapped-link",
				Detail: "linked pair not resolvable to corpus keys",
			})
			continue
		}
		if wantKey, ok := expected[exp{olderKey, newerKey}]; ok {
			st.TP++
			satisfied[wantKey] = true
		} else {
			st.FP++
			st.Misses = append(st.Misses, Miss{
				Key: olderKey, Kind: "unexpected-link",
				Detail: fmt.Sprintf("superseded by %q (%q | %q)", newerKey, excerpt(contentOf(entries, olderKey)), excerpt(contentOf(entries, newerKey))),
			})
		}
	}
	for _, e := range entries {
		if e.ExpectedSupersededBy == "" || satisfied[e.Key] {
			continue
		}
		st.FN++
		st.Misses = append(st.Misses, Miss{
			Key: e.Key, Kind: "missed-link",
			Detail: fmt.Sprintf("expected supersede by %q not proposed (%q)", e.ExpectedSupersededBy, excerpt(e.Content)),
		})
	}
	sortMisses(st.Misses)
	return st
}

// gradeResolve scores confirmed resolved-evidence memories against
// expected_resolved annotations.
func gradeResolve(entries []corpus.Entry, confirmedID8s []string, ids map[string]string) Stage {
	st := Stage{Name: "resolve"}
	k2k := id8ToKey(ids)

	confirmedKeys := map[string]bool{}
	for _, id8 := range confirmedID8s {
		key, ok := k2k[id8]
		if !ok {
			st.FP++
			st.Misses = append(st.Misses, Miss{
				Key: id8, Kind: "unmapped-resolve",
				Detail: "confirmed id not resolvable to a corpus key",
			})
			continue
		}
		e := entryByKey(entries, key)
		if e != nil && e.ExpectedResolved {
			st.TP++
			confirmedKeys[key] = true
		} else {
			st.FP++
			detail := ""
			if e != nil {
				detail = excerpt(e.Content)
			}
			st.Misses = append(st.Misses, Miss{
				Key: key, Kind: "unexpected-resolve",
				Detail: fmt.Sprintf("resolved but not annotated as evidence (%q)", detail),
			})
		}
	}
	for _, e := range entries {
		if !e.ExpectedResolved || confirmedKeys[e.Key] {
			continue
		}
		st.FN++
		st.Misses = append(st.Misses, Miss{
			Key: e.Key, Kind: "missed-resolve",
			Detail: fmt.Sprintf("expected resolved-evidence not confirmed (%q)", excerpt(e.Content)),
		})
	}
	sortMisses(st.Misses)
	return st
}

func contentOf(entries []corpus.Entry, key string) string {
	if e := entryByKey(entries, key); e != nil {
		return e.Content
	}
	return ""
}

func entryByKey(entries []corpus.Entry, key string) *corpus.Entry {
	for i := range entries {
		if entries[i].Key == key {
			return &entries[i]
		}
	}
	return nil
}

func sortMisses(misses []Miss) {
	sort.Slice(misses, func(i, j int) bool {
		if misses[i].Kind != misses[j].Kind {
			return misses[i].Kind < misses[j].Kind
		}
		return misses[i].Key < misses[j].Key
	})
}
