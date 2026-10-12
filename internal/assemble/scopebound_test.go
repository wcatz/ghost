package assemble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// maxScopeLabelLen is the most a ScopeLabel can print. The cut comes before the
// quoting, so the bound is a constant: a cut part quoted by Token grows by at most
// four bytes per byte (a NUL prints as \x00), plus two quotes and the marker.
func maxScopeLabelLen() int {
	part := func(n int) int { return 4*n + 2 + len(scopeTruncationMarker) }
	perKey := part(memory.MaxScopeKeyLen) + 1 + part(memory.MaxScopeValueLen) + 1
	return len(" scope{}") + memory.MaxScopeKeys*perKey + len(" …[1000000 more scope keys]")
}

func TestScopeLabelIsBoundedForAHugeStoredScope(t *testing.T) {
	huge := map[string]string{"environment": strings.Repeat("v", 200000)}
	for i := 0; i < 500; i++ {
		huge[fmt.Sprintf("key%03d%s", i, strings.Repeat("k", 5000))] = strings.Repeat("é", 5000)
	}
	got := ScopeLabel(huge)
	if len(got) > maxScopeLabelLen() {
		t.Fatalf("ScopeLabel printed %d bytes for a huge scope, bound is %d", len(got), maxScopeLabelLen())
	}
	if !strings.Contains(got, scopeTruncationMarker) {
		t.Errorf("a cut value carries no marker: %.200s", got)
	}
	if !strings.Contains(got, "more scope keys]") {
		t.Errorf("a cut key list carries no count: ...%s", got[len(got)-80:])
	}
	if !strings.Contains(got, fmt.Sprintf("…[%d more scope keys]", len(huge)-memory.MaxScopeKeys)) {
		t.Errorf("the dropped-key count is wrong: ...%s", got[len(got)-80:])
	}
	if !strings.HasSuffix(got, "}") {
		t.Errorf("the label is not closed: ...%s", got[len(got)-40:])
	}
}

// TestScopeLabelNeverShortensAScopeAWriterAccepts holds the direction of the
// agreement the same way the tag bound does: the print bound is the writer's own
// constants, so a scope exactly at every limit prints whole.
func TestScopeLabelNeverShortensAScopeAWriterAccepts(t *testing.T) {
	scope := map[string]string{}
	for i := 0; i < memory.MaxScopeKeys; i++ {
		k := fmt.Sprintf("%02d", i) + strings.Repeat("k", memory.MaxScopeKeyLen-2)
		scope[k] = strings.Repeat("v", memory.MaxScopeValueLen)
	}
	got := ScopeLabel(scope)
	if strings.Contains(got, scopeTruncationMarker) || strings.Contains(got, "more scope keys") {
		t.Errorf("a scope at the writer's limits was shortened: %.200s", got)
	}
	for k, v := range scope {
		if !strings.Contains(got, k+"="+v) {
			t.Fatalf("key %.10s... missing from the label", k)
		}
	}
}

func TestScopeLabelOrdinaryScopeIsUnchanged(t *testing.T) {
	got := ScopeLabel(map[string]string{"environment": "production", "component": "api"})
	if want := " scope{component=api environment=production}"; got != want {
		t.Errorf("ScopeLabel = %q, want %q", got, want)
	}
}
