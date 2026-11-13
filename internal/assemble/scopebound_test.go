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
