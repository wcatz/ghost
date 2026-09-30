package supersede

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// capturedLog is a slog.Handler that keeps every record a pass emits, so a
// test can read the KEYS a line carries rather than re-deriving them from the
// call site.
//
// It exists because a slog level call takes alternating key/value arguments
// and the pairing happens inside slog, from the ARGUMENT LIST: a call that
// writes `"link", a, b, "scan", c, d` compiles, vets, and runs, and slog pairs
// it as link=a, b=scan, c=d — so `b` is read as a key while the author read it
// as a value. Nothing downstream of the handler can tell, because by then the
// record is well-formed.
//
// The one thing a handler CAN see is the pairing itself, which is why the
// assertions below are on keys.
type capturedLog struct {
	mu      sync.Mutex
	records []capturedRecord
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

func (c *capturedLog) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (c *capturedLog) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}

func (c *capturedLog) WithAttrs(_ []slog.Attr) slog.Handler { return c }
func (c *capturedLog) WithGroup(_ string) slog.Handler      { return c }

func (c *capturedLog) logger() *slog.Logger { return slog.New(c) }

// find returns every record whose message contains substr.
func (c *capturedLog) find(substr string) []capturedRecord {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []capturedRecord
	for _, r := range c.records {
		if strings.Contains(r.msg, substr) {
			out = append(out, r)
		}
	}
	return out
}

// Every key the pass logs is a lowercase label — "newer", "older", "reason",
// "error" — and that is the whole rule assertLogKeysAreLabels enforces. It is
// one rule because it covers both ways a key/value list can fail to line up:
//
//   - A memory id in the KEY slot is a value that landed there, which is what
//     #804 was: slog paired `"link", a, b, "scan", c, d` as link=a, b=scan, c=d.
//     go vet's slog check cannot see that, because the argument count is even
//     and every argument is a string — the pairing the author meant is not
//     recoverable from the types.
//   - slog's own !BADKEY marker is an argument list that does not pair at all —
//     an odd number of trailing args. vet does reject that one ("missing a final
//     value"), so a handler never sees it in this tree; the rule covers it so
//     the assertion does not depend on that staying true.
func assertLogKeysAreLabels(t *testing.T, log *capturedLog) {
	t.Helper()
	for _, r := range log.records {
		for key := range r.attrs {
			if !isLogKeyLabel(key) {
				t.Errorf("log line %q carries the key %q, which is not a lowercase label: a memory id or a value here means this call's arguments do not pair the way the author read them (slog renders an unpaired trailing argument as %q). attrs=%v",
					r.msg, key, badKeyMarker, r.attrs)
			}
		}
	}
}

// badKeyMarker is the key log/slog gives an argument it could not pair, per its
// documented handling of a key that is not a string. Named here so a failure
// message can say what the alternative to a label looks like.
const badKeyMarker = "!BADKEY"

// isLogKeyLabel reports whether s is a key a person wrote: a lowercase word,
// optionally with digits and underscores, and never a bare number.
func isLogKeyLabel(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
		case c >= '0' && c <= '9' && i > 0:
		case c == '_' && i > 0:
		default:
			return false
		}
	}
	return true
}

// TestRunNamesTheKeyOfEveryValueItLogs is #804.
//
// The line it pins is the one that reports a scan proposing the REVERSE of a
// live supersedes link: the only log line in the package whose subject is two
// disagreeing orientations of one pair, so the only one where naming the source
// of each id is the whole point. It passed `"link", l.SourceID, l.TargetID,
// "scan", cand.NewerID, cand.OlderID` — six values, four of them with a name and
// two without, and slog paired l.TargetID as a KEY, so a dry run printed
//
//	link=02EA044F… 3092A7BE…=scan 3092A7BE…=74CE9D10…
//
// which reads as three unrelated facts and names neither endpoint.
func TestRunNamesTheKeyOfEveryValueItLogs(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// The #641 damage already in the graph: a live link stale→fix, while the
	// scan reads the timestamps and proposes the pair the other way round. The
	// pass refuses that orientation and logs why, which is the record under
	// test.
	stale := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	fix := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, stale, fix, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, stale, fix)

	log := &capturedLog{}
	res, _, err := Run(ctx, store, &supersedesEverything{}, "p", 0.9, true, log.logger())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.OppositeLive != 1 {
		t.Fatalf("Result.OppositeLive = %d, want 1: the fixture must reach the reverse-of-a-live-link line, or this test asserts nothing", res.OppositeLive)
	}

	records := log.find("reverse of a live supersedes link")
	if len(records) != 1 {
		t.Fatalf("emitted %d record(s) about a reversed live link, want exactly 1: %v", len(records), log.records)
	}
	attrs := records[0].attrs
	// Each id under the key that says WHICH source asserted it. The link's own
	// direction and the scan's proposal disagree, so "newer"/"older" alone would
	// leave a reader unable to tell which id came from where.
	want := map[string]string{
		"link_source": stale,
		"link_target": fix,
		"scan_newer":  fix,
		"scan_older":  stale,
	}
	if len(attrs) != len(want) {
		t.Errorf("the line carries %d attribute(s) %v, want exactly %d %v: every value needs a key of its own, and no value may become one",
			len(attrs), attrs, len(want), want)
	}
	for key, wantValue := range want {
		if got, ok := attrs[key]; !ok {
			t.Errorf("no %q attribute; the line's attrs are %v", key, attrs)
		} else if got != wantValue {
			t.Errorf("%s = %q, want %q", key, got, wantValue)
		}
	}
	// The general form of the same rule, so the line is also covered by the
	// shape assertion its siblings get.
	assertLogKeysAreLabels(t, log)
}

// TestRunLogsOnlyLabelKeys is the package-wide half of #804: the same shape
// assertion, over a pass whose fixture reaches SEVERAL of the lines Run logs —
// so a future mispaired call anywhere in the pass fails here rather than in a
// log file nobody reads.
//
// The fixture is built to hit lines with DIFFERENT key vocabularies on purpose:
// a refusal naming a link's direction, a second refusal, a veto, an
// unclassifiable verdict, and the per-pair classified line. One line's keys
// being right says nothing about the next one's, which is the whole reason
// #804 survived in a tree where every other line was fine.
func TestRunLogsOnlyLabelKeys(t *testing.T) {
	store, db := seed(t)
	ctx := context.Background()

	// Pair 1: a live link whose direction the scan contradicts → the
	// reverse-of-a-live-link line.
	stale := add(t, store, db, "bug: the relay stalls on every consumer rebalance", []float32{1, 0, 0}, "2026-01-01 00:00:00")
	fix := add(t, store, db, "the relay rebalance stall is fixed: pin the consumer", []float32{0.98, 0.02, 0}, "2026-09-01 00:00:00")
	if err := store.CreateLink(ctx, stale, fix, "supersedes", 0.95, "llm"); err != nil {
		t.Fatal(err)
	}
	backdateLink(t, db, stale, fix)

	// Pair 2: both directions live → the bidirectional-refusal line.
	biNewer := add(t, store, db, "kubernetes now on 1.31", []float32{1, 0.02, 0}, "2026-09-02 00:00:00")
	biOlder := add(t, store, db, "kubernetes cluster runs 1.27", []float32{0.98, 0.03, 0}, "2026-01-02 00:00:00")
	for _, dir := range [][2]string{{biNewer, biOlder}, {biOlder, biNewer}} {
		if err := store.CreateLink(ctx, dir[0], dir[1], "supersedes", 0.95, "llm"); err != nil {
			t.Fatal(err)
		}
	}
	backdateLink(t, db, biNewer, biOlder)

	// Pair 3: an older note that states a rule the newer one never retires, so
	// the deterministic veto fires and the pair never reaches a classifier.
	vetoOlder := add(t, store, db, "never deploy on a Friday: the release train does not run", []float32{0.96, 0.06, 0}, "2026-02-01 00:00:00")
	vetoNewer := add(t, store, db, "the friday release train now ships from the automated pipeline", []float32{0.95, 0.07, 0}, "2026-09-03 00:00:00")

	// Pair 4: an ordinary fresh pair the classifier cannot parse a verdict for
	// → the unclassifiable line, and then the classified line for pair 1's
	// reclassification.
	blankNewer := add(t, store, db, "grafana listens on port 8080", []float32{0, 1, 0.01}, "2026-09-04 00:00:00")
	blankOlder := add(t, store, db, "grafana listens on port 80", []float32{0, 0.99, 0.02}, "2026-03-04 00:00:00")

	log := &capturedLog{}
	cls := &mockClassifier{verdict: func(newer, older string) Relation {
		if strings.Contains(newer, "grafana listens on port 8080") {
			return "" // the unparseable verdict a single bad phrasing produces
		}
		return RelationSupersedes
	}}
	if _, _, err := Run(ctx, store, cls, "p", 0.9, true, log.logger()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The shape assertion, over every line the fixture reached.
	assertLogKeysAreLabels(t, log)

	// And a floor on WHICH lines it reached: a fixture that quietly stopped
	// producing log lines would pass the shape assertion while testing nothing,
	// which is how a guard like this rots.
	for _, want := range []string{
		"reverse of a live supersedes link", // the #804 line
		"refusing a pair the graph claims in both directions",
		"vetoed pair whose older note states a rule",
		"skipping pair with an unclassifiable verdict",
		"supersede classified",
	} {
		if len(log.find(want)) == 0 {
			t.Errorf("no record matching %q; the fixture must still reach the lines it is meant to cover. Records seen:\n%s", want, dumpRecords(log))
		}
	}

	// A line that names a pair is only useful if it names the RIGHT pair, and
	// the shape assertion cannot tell a correctly-keyed id from a swapped one.
	// The unclassifiable pair and the vetoed pair are the two whose ids the
	// fixture knows, so read both back — each guarded by its own count, so a
	// line that stopped firing is reported by the loop above rather than here.
	if got := log.find("skipping pair with an unclassifiable verdict"); len(got) == 1 {
		attrs := got[0].attrs
		if attrs["newer"] != blankNewer || attrs["older"] != blankOlder {
			t.Errorf("the unclassifiable line names (%s, %s), want the grafana pair (%s, %s): the fixture exists so the line's keys carry the pair that produced them",
				attrs["newer"], attrs["older"], blankNewer, blankOlder)
		}
	}
	if got := log.find("vetoed pair whose older note states a rule"); len(got) == 1 {
		attrs := got[0].attrs
		if attrs["older"] != vetoOlder || attrs["newer"] != vetoNewer {
			t.Errorf("the veto line names (%s, %s), want the friday-train pair (%s, %s)",
				attrs["newer"], attrs["older"], vetoNewer, vetoOlder)
		}
		if attrs["reason"] == "" {
			t.Error("the veto line carries no reason; the whole point of the line is naming the imperative that fired")
		}
	}
}

// dumpRecords renders a captured log for a failure message.
func dumpRecords(log *capturedLog) string {
	log.mu.Lock()
	defer log.mu.Unlock()
	var b bytes.Buffer
	for _, r := range log.records {
		b.WriteString("  " + r.msg + " ")
		for k, v := range r.attrs {
			b.WriteString(k + "=" + v + " ")
		}
		b.WriteByte('\n')
	}
	return b.String()
}
