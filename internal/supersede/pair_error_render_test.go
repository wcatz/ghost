package supersede

// A supersede pass's failures reach stderr, and on the unattended path stderr is
// `lifecycle.log`: a line a maintenance run writes and every later session can be
// shown. The pair prefixes on those failures interpolated the two stored ids with
// a bare `%s`, and a stored id is not token-guarded — `ghost import` writes an
// artifact's id verbatim and `memory.CheckImportedID` never asks the guard — so a
// `ghp_`-shaped id rode out of the pass in the error it caused.
//
// These tests hold the relation classifier's two pair prefixes, which is the path
// the report's own reproduction names, and pin both halves of the renderer: a
// token-shaped id is withheld behind the format found, while an ordinary id is
// still named so the refusal keeps saying which pair failed.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// tokenShapedStoredID is obviously fake and shaped like the class an imported id
// can be: the GitHub personal access token rule is `ghp_` plus 36 characters, so
// it is exactly what `memory.RefusalArg` recognises and withholds.
const tokenShapedStoredID = "ghp_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// ordinaryStoredID is hex-shaped the way Ghost's own ids are, so it must print.
const ordinaryStoredID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// TestRelationClassifierWithholdsTokenShapedStoredIDsInPairErrors drives the
// single-pair path (the lone tail of a batch, `Classify` wrapped by
// `ClassifyBatch`). The token id must be absent and named only by format, and the
// ordinary id must still appear, or a renderer that withheld everything would
// keep the token out and leave the failure unable to name the pair.
func TestRelationClassifierWithholdsTokenShapedStoredIDsInPairErrors(t *testing.T) {
	cls := NewRelationClassifier(&fakeProvider{err: errors.New("harness down")})
	cls.SetRetryDelay(0)

	_, err := cls.ClassifyBatch(context.Background(), []Candidate{
		{NewerID: tokenShapedStoredID, OlderID: ordinaryStoredID, NewerContent: "n", OlderContent: "o"},
	})
	if err == nil {
		t.Fatal("want the classifier failure, got nil")
	}
	got := err.Error()
	if strings.Contains(got, tokenShapedStoredID) {
		t.Errorf("the failure echoed a token-shaped stored id: %s", got)
	}
	if !strings.Contains(got, `"<memory_id withheld: it holds a GitHub personal access token>"`) {
		t.Errorf("the failure did not withhold the token-shaped id by its format: %s", got)
	}
	if !strings.Contains(got, ordinaryStoredID) {
		t.Errorf("the failure withheld an ordinary stored id, so it can no longer name the pair: %s", got)
	}
}

// TestRelationClassifierNamesBothOrdinaryStoredIDsInPairErrors is the other half:
// a pair of ordinary ids is named, quoted, in the order the prefix carried them.
// Without it a renderer that returned a fixed placeholder for every id would pass
// the test above.
func TestRelationClassifierNamesBothOrdinaryStoredIDsInPairErrors(t *testing.T) {
	cls := NewRelationClassifier(&fakeProvider{err: errors.New("harness down")})
	cls.SetRetryDelay(0)

	_, err := cls.ClassifyBatch(context.Background(), []Candidate{
		{NewerID: ordinaryStoredID, OlderID: "cccccccccccccccccccccccccccccccc", NewerContent: "n", OlderContent: "o"},
	})
	if err == nil {
		t.Fatal("want the classifier failure, got nil")
	}
	if want := `"` + ordinaryStoredID + `"→"cccccccccccccccccccccccccccccccc": harness down`; err.Error() != want {
		t.Errorf("pair failure = %q, want %q", err.Error(), want)
	}
}

// TestRelationClassifierLogWithholdsTokenShapedStoredIDs is the log half of the
// same rule. The unparseable-verdict warning names the pair on its way to
// stderr, which on the detached lifecycle path is lifecycle.log, so it renders
// each id through the same guard as the error beside it. Without this a leak
// two lines above a guarded return would pass every test above.
func TestRelationClassifierLogWithholdsTokenShapedStoredIDs(t *testing.T) {
	log := &capturedLog{}
	cls := NewRelationClassifier(&fakeProvider{resp: "banana"})
	cls.SetLogger(log.logger())

	if _, err := cls.ClassifyBatch(context.Background(), []Candidate{
		{NewerID: tokenShapedStoredID, OlderID: ordinaryStoredID, NewerContent: "n", OlderContent: "o"},
	}); err != nil {
		t.Fatalf("an unparseable verdict is counted, not fatal: %v", err)
	}
	recs := log.find("supersede: unparseable verdict")
	if len(recs) != 1 {
		t.Fatalf("got %d unparseable-verdict line(s), want 1", len(recs))
	}
	if got := recs[0].attrs["newer"]; got != `"<memory_id withheld: it holds a GitHub personal access token>"` {
		t.Errorf("the log line did not withhold the token-shaped id: newer=%q", got)
	}
	if got, want := recs[0].attrs["older"], `"`+ordinaryStoredID+`"`; got != want {
		t.Errorf("the log line withheld an ordinary id: older=%q, want %q", got, want)
	}
	if strings.Contains(recs[0].attrs["newer"], tokenShapedStoredID) {
		t.Error("the log line echoed the token-shaped id")
	}
}
