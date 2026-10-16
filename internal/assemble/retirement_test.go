package assemble

import (
	"strings"
	"testing"
)

// The retired-dependency storyline's pair, word for word: the dependency, and the
// note saying it was removed with nothing replacing it. The retirement holds no
// value token the dependency lacks, so the value-swap guess never named it.
const (
	sendgridDep     = "Order emails go out through SendGrid: internal/mail wraps the sendgrid-go v3 client and reads SENDGRID_API_KEY at boot."
	sendgridRetired = "SendGrid was removed from parcel-notify: internal/mail is deleted, nothing reads SENDGRID_API_KEY, and order emails are no longer sent at all."
)

// TestRunPassiveOrdersARetirementFirstAndMarksTheRetiredRow is the session start
// after a dependency's removal on a default install: the retired claim ranked
// first (it is the more important row and nothing relates the two) and neither
// line said it was gone. The retirement must come first, and the retired row
// must stay, marked as maybe replaced by the retirement.
func TestRunPassiveOrdersARetirementFirstAndMarksTheRetiredRow(t *testing.T) {
	r := newReplacementStore(t)
	older := r.add(dep(sendgridDep, 0.8), 20)
	newer := r.add(dep(sendgridRetired, 0.7), 24)

	res := run(t, r.s, replacementPassiveRequest())
	oi, ni := indexOf(res.Items, older), indexOf(res.Items, newer)
	if oi < 0 || ni < 0 {
		t.Fatalf("answer %v must hold both rows (retired %s, retirement %s): the retired row is marked, not withheld", itemIDs(res.Items), older, newer)
	}
	if ni > oi {
		t.Errorf("answer %v puts the retired claim %s (index %d) ahead of its retirement %s (index %d)", itemIDs(res.Items), older, oi, newer, ni)
	}
	old := res.Items[oi]
	if len(old.MaybeReplacedBy) != 1 || old.MaybeReplacedBy[0] != newer {
		t.Errorf("retired row MaybeReplacedBy = %v, want [%s]", old.MaybeReplacedBy, newer)
	}
	prefix, _, _ := strings.Cut(old.Line(), "«")
	if !strings.Contains(prefix, MaybeReplacedMarker) || !strings.Contains(prefix, "maybe_replaced_by=`"+newer+"`") {
		t.Errorf("retired line does not carry the marker and the retirement's id in its prefix: %s", old.Line())
	}
	if n := res.Items[ni]; len(n.MaybeReplacedBy) > 0 || strings.Contains(n.Line(), MaybeReplacedMarker) {
		t.Errorf("the retirement itself is marked: %s", n.Line())
	}
}

// TestRunPassiveDoesNotPairAPlannedRetirement: a note that says a claim will no
// longer be needed once something ships is a plan, not a retirement, and the
// claim it names is still current.
func TestRunPassiveDoesNotPairAPlannedRetirement(t *testing.T) {
	r := newReplacementStore(t)
	r.add(dep("The LEGACY_AUTH flag gates the old session login path in internal/auth.", 0.8), 20)
	r.add(dep("The LEGACY_AUTH flag is no longer needed once SSO ships; internal/auth keeps the old session login path until then.", 0.7), 24)

	res := run(t, r.s, replacementPassiveRequest())
	if len(res.Items) != 2 {
		t.Fatalf("answer %v must hold both rows", itemIDs(res.Items))
	}
	assertUnmarked(t, res.Items)
}
