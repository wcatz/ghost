package resolve

// #648 slice 2: a flag is negative evidence, and nothing else.
//
// Three properties, in the order they can rot: the flag's REASON never leaves
// the store (it is free text an agent wrote and it would reach a third-party
// model the moment it reached a prompt), the flag joins the KEEP-cache
// fingerprint so a new one re-asks a cached KEEP exactly once (#885's rule), and
// a flag on its own still costs nothing beyond the read the evidence pass
// already pays for.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// flagReasonMarker is unmistakable text. Its presence anywhere in what the pass
// sends the harness is the failure, and asserting on a marker rather than on
// "the reason field" is what makes the assertion a measurement.
const flagReasonMarker = "ZZREASONNEVERLEAVESSTOREZZ"

// TestTheFlagReasonNeverReachesTheClassifier: the property, over a REAL store.
//
// A fake store's usefulness map would only prove the pass renders whatever it is
// handed, and the reason is not in that map at all — it is in a column of a
// table the reader has to be careful not to project. So the flag is written
// through the writer, read through the reader, and the assertion is on the bytes
// the harness is asked about.
func TestTheFlagReasonNeverReachesTheClassifier(t *testing.T) {
	s, ctx, rows := resolveRealStore(t)
	flagged := rows[0]
	const reason = flagReasonMarker + " the port was decommissioned in June"

	if err := s.FlagMemory(ctx, memory.FlagMemoryRequest{
		ProjectID: "usefulness", MemoryID: flagged.ID, Kind: memory.FlagKindWrong,
		Reason: reason, Agent: "codex", SessionID: "ses_flagged",
	}); err != nil {
		t.Fatalf("FlagMemory: %v", err)
	}

	cls := &fakeClassifier{}
	if _, _, err := Run(ctx, s, cls, "usefulness", false, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(cls.askedFor) == 0 {
		t.Fatal("the pass asked about nothing, so there is nothing to scan")
	}
	for _, asked := range cls.askedFor {
		if strings.Contains(asked, flagReasonMarker) {
			t.Errorf("a flag's reason text reached the harness:\n%s", asked)
		}
		if strings.Contains(asked, "codex") || strings.Contains(asked, "ses_flagged") {
			t.Errorf("a flag's attribution reached the harness, which is a claim about a verdict:\n%s", asked)
		}
	}
	// And the count DOES reach it, which is the whole effect the flag is allowed
	// to have. Without this the test above would pass for a reader that returned
	// no evidence at all.
	want := flagged.Content + "\n" + "audit: verdicts flagged=1"
	if !sentVerbatim(cls.askedFor, want) {
		t.Errorf("the flag's count did not reach the harness; pass sent %q, want the note plus %q",
			cls.askedFor, want)
	}
}

// TestKeepStampCarriesTheFlagOnlyWhenThereIsOne: #885's rule, stated on the
// fingerprint rather than on the pass.
//
// The base case is byte-identical to what it was before a flag existed, because
// every stored cache entry has to keep matching — an un-doubted corpus must not
// be re-asked for a field that is zero. The flagged case is a different string,
// which is what makes "one re-ask per new flag" a property of the stamp rather
// than of a counter somebody has to remember to compare.
func TestKeepStampCarriesTheFlagOnlyWhenThereIsOne(t *testing.T) {
	const content = "the retry budget is three attempts"
	base := memory.ContentHash(content)

	for _, tc := range []struct {
		name string
		ev   memory.UsefulnessEvidence
	}{
		// Deliberately NOT the zero-evidence case: that one produces the bare
		// content hash and is asserted separately below, since a table entry
		// expecting `hash:0:0:` and a check expecting `hash` cannot both hold.
		{"a contradiction with no flag", memory.UsefulnessEvidence{
			Contradicted: 2, LastAt: "2026-09-24 10:00:00",
		}},
		{"superseded with no flag", memory.UsefulnessEvidence{
			SupersededInSession: 1, LastAt: "2026-09-24 10:00:00",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := KeepStamp(content, tc.ev)
			// The pre-flag format, spelled out: hash:contradicted:superseded:LastAt.
			want := fmt.Sprintf("%s:%d:%d:%s", base, tc.ev.Contradicted, tc.ev.SupersededInSession, tc.ev.LastAt)
			if got != want {
				t.Errorf("KeepStamp = %q, want the format this build wrote before flags existed (%q) — a "+
					"stored cache entry has to keep matching", got, want)
			}
		})
	}
	// Zero evidence is the bare content hash, byte for byte: that is what makes
	// every pre-#880 entry a valid hit.
	if got := KeepStamp(content, memory.UsefulnessEvidence{}); got != base {
		t.Errorf("KeepStamp with no evidence = %q, want the bare content hash %q", got, base)
	}

	flagged := memory.UsefulnessEvidence{Flagged: 1, LastAt: "2026-09-24 10:00:00"}
	plain := memory.UsefulnessEvidence{Flagged: 0, LastAt: "2026-09-24 10:00:00"}
	flagStamp := KeepStamp(content, flagged)
	if flagStamp == KeepStamp(content, plain) {
		t.Errorf("a flag did not move the KEEP stamp (%q): a cached KEEP would stay a hit and the new flag "+
			"would never be re-asked", flagStamp)
	}
	if flagStamp == base {
		t.Errorf("the flagged stamp collapsed to the bare content hash %q", base)
	}
	// Deterministic over (content, evidence): the same pair always produces the
	// same key, which is what makes "re-asked once" a promise.
	if again := KeepStamp(content, flagged); again != flagStamp {
		t.Errorf("KeepStamp is not deterministic: %q then %q", flagStamp, again)
	}
	// Two flags are a different fingerprint from one, so a second flag re-asks too.
	two := memory.UsefulnessEvidence{Flagged: 2, LastAt: "2026-09-24 10:00:00"}
	if KeepStamp(content, two) == flagStamp {
		t.Errorf("one flag and two flags produce the same stamp %q, so the second flag would never be asked about",
			flagStamp)
	}
	// And the counts stay distinct fields: a flag at contradicted=0 must not be
	// confusable with a contradiction at flagged=0.
	if KeepStamp(content, memory.UsefulnessEvidence{Flagged: 1}) ==
		KeepStamp(content, memory.UsefulnessEvidence{Contradicted: 1}) {
		t.Errorf("flagged=1 and contradicted=1 produce the same stamp: the fingerprint cannot tell the two " +
			"evidence sources apart")
	}
}

// TestAFlagReasksACachedKeepOnceAndThenGoesQuiet: #885's rule end to end. A
// plain cache entry for content an agent has since flagged must reach the
// classifier again — with the count attached, as data inside the note — and the
// KEEP it comes back with must be re-stamped so it covers that flag. One re-ask,
// then quiet.
func TestAFlagReasksACachedKeepOnceAndThenGoesQuiet(t *testing.T) {
	const content = "cost estimate from May: $148 per month projected"
	evidence := map[string]memory.UsefulnessEvidence{
		"M1": {Flagged: 1, LastAt: "2026-10-06 09:00:00"},
	}
	store := &fakeStore{
		candidates: []memory.Memory{{ID: "M1", Content: content}},
		kept:       map[string]string{"M1": ContentHash(content)}, // a cache entry from before the flag
		usefulness: evidence,
	}

	cls := &fakeClassifier{}
	if _, _, err := Run(context.Background(), store, cls, "p1", false, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cls.calls != 1 {
		t.Fatalf("classifier calls = %d, want 1 — a cached KEEP an agent has flagged must be re-asked",
			cls.calls)
	}
	want := content + "\n" + evidence["M1"].Line()
	if !sentVerbatim(cls.askedFor, want) {
		t.Errorf("the pass sent %q, want the note plus its flag line %q", cls.askedFor, want)
	}

	// The apply pass keeps it and stamps the flag into the cache entry.
	cls2 := &fakeClassifier{}
	if _, _, err := Run(context.Background(), store, cls2, "p1", true, nil); err != nil {
		t.Fatalf("Run apply: %v", err)
	}
	if len(store.markedKept) != 1 {
		t.Fatalf("markedKept = %v, want one KEEP stamp", store.markedKept)
	}
	stamp := store.markedKept[0]["M1"]
	if stamp != KeepStamp(content, evidence["M1"]) {
		t.Errorf("re-stamped as %q, want the stamp covering that flag %q", stamp, KeepStamp(content, evidence["M1"]))
	}
	if stamp == ContentHash(content) {
		t.Errorf("the re-stamp is the bare content hash %q: a KEEP judged WITH a flag must not be cached as "+
			"one judged without it, or the flag is re-asked forever", stamp)
	}

	// Quiet.
	cls3 := &fakeClassifier{}
	res, _, err := Run(context.Background(), store, cls3, "p1", false, nil)
	if err != nil {
		t.Fatalf("Run after re-stamp: %v", err)
	}
	if cls3.calls != 0 || res.Skipped != 1 {
		t.Errorf("after the re-stamp the pass made %d classifier call(s) and skipped %d; want 0 and 1 — the "+
			"stamp already covers this flag", cls3.calls, res.Skipped)
	}
}
