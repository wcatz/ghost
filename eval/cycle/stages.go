package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/wcatz/ghost/eval/cycle/corpus"
)

type supPair struct {
	NewerID8 string
	Relation string
	OtherID8 string
}

var (
	supLineRe = regexp.MustCompile(`(?mi)^\s*([0-9a-f]{8})(?:[0-9a-f]+)?\s+(supersedes|causes)\s+([0-9a-f]{8})(?:[0-9a-f]+)?\s*$`)
	resLineRe = regexp.MustCompile(`(?mi)^\s*([0-9a-f]{8})(?:[0-9a-f]+)?\s+\[[a-z]+\]`)
)

func parseSupersedeLines(out string) []supPair {
	var pairs []supPair
	for _, m := range supLineRe.FindAllStringSubmatch(out, -1) {
		pairs = append(pairs, supPair{
			NewerID8: strings.ToLower(m[1]),
			Relation: m[2],
			OtherID8: strings.ToLower(m[3]),
		})
	}
	return pairs
}

func parseResolveIDs(out string) []string {
	var ids []string
	for _, m := range resLineRe.FindAllStringSubmatch(out, -1) {
		ids = append(ids, strings.ToLower(m[1]))
	}
	return ids
}

type Miss struct {
	Key    string
	Kind   string
	Detail string
}

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

func id8ToKey(ids map[string]string) map[string]string {
	m := make(map[string]string, len(ids))
	for key, full := range ids {
		if len(full) < 8 {
			continue
		}
		prefix := strings.ToLower(full[:8])
		if prev, ok := m[prefix]; ok && prev != key {
			continue
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

func gradeSupersede(ids map[string]string, entries []corpus.Entry, pairs []supPair) Stage {
	st := Stage{Name: "supersede"}
	k2k := id8ToKey(ids)

	seenPairs := map[supPair]bool{}
	deduped := make([]supPair, 0, len(pairs))
	for _, p := range pairs {
		if !seenPairs[p] {
			seenPairs[p] = true
			deduped = append(deduped, p)
		}
	}

	type exp struct{ older, newer string }
	expected := make(map[exp]string)
	for _, e := range entries {
		if e.ExpectedSupersededBy != "" {
			expected[exp{e.Key, e.ExpectedSupersededBy}] = e.Key
		}
	}

	satisfied := map[string]bool{}
	for _, p := range deduped {
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

func gradeResolve(entries []corpus.Entry, confirmedID8s []string, ids map[string]string) Stage {
	st := Stage{Name: "resolve"}
	k2k := id8ToKey(ids)

	seenIDs := map[string]bool{}
	confirmedKeys := map[string]bool{}
	for _, id8 := range confirmedID8s {
		if seenIDs[id8] {
			continue
		}
		seenIDs[id8] = true
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
