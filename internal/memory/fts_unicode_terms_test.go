package memory

import (
	"context"
	"strings"
	"testing"
)

// A keyword query written in a script the ASCII edge trim used to strip reached
// the index as an EMPTY term list, so a memory written in that script could not
// be found by a query in the same script: the FTS leg handed FTS5 a MATCH with
// no term in it, and the historical leg ran no keyword pass at all. The trim
// keeps a letter, a digit and a mark of ANY script now, and BOTH paths read the
// one extraction, so a Cyrillic, Arabic, CJK or accented query reaches the rows
// holding it exactly as an ASCII query reaches an ASCII word.
//
// The CJK case is the one with a limit rather than a fix: unicode61 reads an
// unspaced CJK run as ONE token, so a query for 日本語 matches a whole run (or a
// prefix of one, with a trailing *) and never a word inside it.

// scriptCase is one script's fixture: the word a query names and the content
// that holds it. The contents share no word with each other, so a query for one
// of them can only reach the row that holds it.
type scriptCase struct {
	name    string
	word    string
	content string
}

var scriptCases = []scriptCase{
	{
		name:    "cyrillic",
		word:    "память",
		content: "память про запуск деплоя",
	},
	{
		name:    "arabic",
		word:    "مرحبا",
		content: "مرحبا بكم في صفحة النشر",
	},
	{
		name:    "cjk",
		word:    "日本語",
		content: "日本語 deployment note",
	},
	{
		name:    "accented",
		word:    "café",
		content: "café release rehearsal",
	},
}

// scriptPrefixes is the prefix of each case's word: the first two runes of it.
// A prefix of the run the index holds is the only thing a query for part of a
// run can match, which is the CJK limit stated rather than worked around.
var scriptPrefixes = map[string]string{
	"cyrillic": "пам",
	"arabic":   "مر",
	"cjk":      "日本",
	"accented": "ca",
}

// saveScriptMemories writes one memory per case and returns the id of each.
func saveScriptMemories(t *testing.T, s *Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	ids := make(map[string]string, len(scriptCases))
	for _, tc := range scriptCases {
		id, _, _, err := s.UpsertWithProvenance(ctx, testProject, "fact", tc.content, "mcp", 0.5, nil, Provenance{})
		if err != nil {
			t.Fatalf("save the %s memory: %v", tc.name, err)
		}
		ids[tc.name] = id
	}
	return ids
}

// TestCandidatesMatchesANonLatinQuery: the current path. A query in a script
// the trim used to strip found nothing, so a memory written in that script was
// unreachable by a keyword search in it — the answer was empty rather than
// wrong, but it ruled keyword retrieval out for every user who writes in a
// non-Latin script.
func TestCandidatesMatchesANonLatinQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := saveScriptMemories(t, s)

	for _, tc := range scriptCases {
		t.Run(tc.name, func(t *testing.T) {
			set, err := s.Candidates(ctx, CandidateRequest{
				ProjectID: testProject,
				Mode:      ProjectScoped,
				Query:     tc.word,
				Condition: CondFTSOnly,
				Params:    DefaultSearchParams(),
				Now:       asOfAt(t, asOfStampFarFuture),
				Fetch:     Fetch{FTSTopK: 20, VectorTopK: 20, Limit: 10},
			})
			if err != nil {
				t.Fatalf("Candidates(%q): %v", tc.word, err)
			}
			if !containsID(set, ids[tc.name]) {
				t.Errorf("the search for %q returns %v, want the memory holding that word (%s): a query in a non-Latin script must reach the rows that hold it",
					tc.word, candidateIDs(t, set), ids[tc.name])
			}
			leg := set.Legs["fts"]
			if !leg.Applicable || !leg.Attempted || !leg.Available {
				t.Errorf("the keyword leg reports %+v for %q, want it applicable, attempted and available", leg, tc.word)
			}
			if leg.Eligible == 0 {
				t.Errorf("the keyword leg reports no eligible row for %q, want the row that holds the word", tc.word)
			}
		})
	}
}

// TestCandidatesMatchesANonLatinPrefixQuery: the trailing `*` means a prefix
// match, and it has to reach the same rows the whole word reaches. A CJK run is
// ONE token to the index, so the prefix of a run is the only thing a query for
// part of a run can match — which is the limit the docs state, and the reason
// the CJK case is here rather than a claim of full segmentation.
func TestCandidatesMatchesANonLatinPrefixQuery(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := saveScriptMemories(t, s)

	for _, tc := range scriptCases {
		t.Run(tc.name, func(t *testing.T) {
			prefix := scriptPrefixes[tc.name]
			set, err := s.Candidates(ctx, CandidateRequest{
				ProjectID: testProject,
				Mode:      ProjectScoped,
				Query:     prefix + "*",
				Condition: CondFTSOnly,
				Params:    DefaultSearchParams(),
				Now:       asOfAt(t, asOfStampFarFuture),
				Fetch:     Fetch{FTSTopK: 20, VectorTopK: 20, Limit: 10},
			})
			if err != nil {
				t.Fatalf("Candidates(%q*): %v", prefix, err)
			}
			if !containsID(set, ids[tc.name]) {
				t.Errorf("the search for %q* returns %v, want the memory holding the whole word (%s): a prefix query must reach it",
					prefix, candidateIDs(t, set), ids[tc.name])
			}
		})
	}
}

// TestCandidatesAsOfMatchesANonLatinQuery: the historical path, which reads the
// SAME extraction and had the same hole. The as_of leg has no vector leg at
// all, so a query in a non-Latin script matched nothing there in every case.
func TestCandidatesAsOfMatchesANonLatinQuery(t *testing.T) {
	s := testStore(t)
	ids := saveScriptMemories(t, s)
	for _, tc := range scriptCases {
		stampHistory(t, s, ids[tc.name], asOfStampSave)
	}

	for _, tc := range scriptCases {
		t.Run(tc.name, func(t *testing.T) {
			set := asOfSearch(t, s, testProject, tc.word, asOfStampSave)
			if !containsID(set, ids[tc.name]) {
				t.Errorf("the historical search for %q returns %v, want the memory holding that word (%s): the as_of leg reads the same extraction and must match the same way",
					tc.word, candidateIDs(t, set), ids[tc.name])
			}
		})
	}
}

// TestCandidatesAsOfMatchesANonLatinPrefixQuery: the trailing `*` means a
// prefix match, and it has to mean the same thing on the historical path as on
// the current one. The historical matcher reads the prefix flag off the same
// term, so a query for a PREFIX of a word reaches the row holding the whole
// word.
func TestCandidatesAsOfMatchesANonLatinPrefixQuery(t *testing.T) {
	s := testStore(t)
	ids := saveScriptMemories(t, s)
	for _, tc := range scriptCases {
		stampHistory(t, s, ids[tc.name], asOfStampSave)
	}

	for _, tc := range scriptCases {
		t.Run(tc.name, func(t *testing.T) {
			prefix := scriptPrefixes[tc.name]
			set := asOfSearch(t, s, testProject, prefix+"*", asOfStampSave)
			if !containsID(set, ids[tc.name]) {
				t.Errorf("the historical search for %q* returns %v, want the memory holding the whole word (%s): a prefix query must reach it the way it does over the present",
					prefix+"*", candidateIDs(t, set), ids[tc.name])
			}
		})
	}
}

// TestSanitizeFTSKeepsNonLatinWords: the MATCH string the FTS leg hands FTS5.
// A term in another script is quoted and escaped exactly as an ASCII term is —
// the widening changes WHICH terms survive the trim, never how a term is
// written — so a widened term cannot re-enter FTS5's query grammar.
func TestSanitizeFTSKeepsNonLatinWords(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "a cyrillic word survives the trim",
			input: "память",
			want:  `"память"`,
		},
		{
			name:  "an arabic word survives the trim",
			input: "مرحبا",
			want:  `"مرحبا"`,
		},
		{
			name:  "a cjk run survives the trim",
			input: "日本語",
			want:  `"日本語"`,
		},
		{
			name:  "an accented word survives the trim",
			input: "café",
			want:  `"café"`,
		},
		{
			name:  "a widened term is quoted and escaped like any other",
			input: `é"o`,
			want:  `"é""o"`,
		},
		{
			name:  "a prefix star stays outside the quotes",
			input: "日本*",
			want:  `"日本"*`,
		},
		{
			name:  "a widened word and an ascii word are both emitted",
			input: "память deployment",
			want:  `"память" OR "deployment"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeFTS(tc.input); got != tc.want {
				t.Errorf("sanitizeFTS(%q)\n  got:  %s\n  want: %s", tc.input, got, tc.want)
			}
		})
	}
}

// TestSanitizeFTSKeepsTheTermBudgetForNonLatinWords: the budget is the budget.
// A widened term is an ordinary term, so a query of twelve non-Latin words still
// emits ten of them — the cap is what keeps the OR query narrow, and it must
// not grow because a script widened.
func TestSanitizeFTSKeepsTheTermBudgetForNonLatinWords(t *testing.T) {
	words := []string{
		"память", "запуск", "деплоя", "конфигурация", "хранилище", "ключ",
		"مرحبا", "النشر", "التكوين", "المخزن", "المفتاح", "الاحتفاظ",
	}
	got := sanitizeFTS(strings.Join(words, " "))
	terms := splitFTSTERMs(t, got)
	if len(terms) != ftsSearchWordLimit {
		t.Errorf("sanitizeFTS emitted %d terms, want exactly %d: the budget is unchanged by the widening", len(terms), ftsSearchWordLimit)
	}
	for _, term := range terms {
		if term == "" {
			t.Errorf("sanitizeFTS emitted an empty term: %s", got)
		}
	}
}
