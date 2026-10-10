package memory

import (
	"strings"
	"testing"
)

func TestDistinctiveTerms(t *testing.T) {
	got := DistinctiveTerms("Please fix the sqlitebackup race in stophook.go and ghost_windows_arm64, the sqlitebackup one", 10)
	var texts []string
	ident := map[string]bool{}
	for _, q := range got {
		texts = append(texts, q.Text)
		ident[q.Text] = q.Identifier
	}
	if strings.Join(texts, ",") != "stophook.go,ghost_windows_arm64,fix,sqlitebackup,race,one" {
		t.Errorf("terms = %v: want identifiers first, then content words, stopwords and the repeat dropped", texts)
	}
	if !ident["stophook.go"] || !ident["ghost_windows_arm64"] || ident["sqlitebackup"] {
		t.Errorf("identifier flags = %v", ident)
	}
	if n := len(DistinctiveTerms("alpha beta gamma delta", 2)); n != 2 {
		t.Errorf("cap not applied: %d", n)
	}
	if DistinctiveTerms("the and of", 5) != nil || DistinctiveTerms("x", 0) != nil {
		t.Error("stopwords-only or max 0 must yield nothing")
	}
	// Bounded: a huge input costs a bounded scan and prints nothing on stderr.
	if n := len(DistinctiveTerms(strings.Repeat("word ", 100000), 5)); n > 2 {
		t.Errorf("repeat collapse failed (the 8 KiB cut may leave one partial word): %d", n)
	}
}
