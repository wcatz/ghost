package supersede

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The classifier's reply reaches the phase's stderr, which a detached lifecycle
// run appends to lifecycle.log — the same reason ClassifyBatch renders the ids
// on these lines through memory.RefusalArg. These tests pin that the reply on
// the same lines, and in errUnparseableVerdict, is bounded and token-guarded.

// replyLogToken is a GitHub-token-shaped value. Assembled rather than written
// out, because GitHub push protection matches the format anywhere in a diff.
// replyLogFragment is part of its body, so a partial leak fails too.
var (
	replyLogToken    = "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	replyLogFragment = "a1B2c3D4e5F6"
)

// replyLogBound is the most any one logged value may hold: the rendered reply
// (at most ArgEchoMaxBytes, quoted) plus room for its placeholder and the
// error's own prefix.
const replyLogBound = 2*memory.ArgEchoMaxBytes + 64

// assertReplyLogClean checks every value of every record: no fragment of the
// token, and nothing over the bound.
func assertReplyLogClean(t *testing.T, log *capturedLog) {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, r := range log.records {
		for k, v := range r.attrs {
			if strings.Contains(v, replyLogFragment) || strings.Contains(r.msg, replyLogFragment) {
				t.Errorf("%q logged %s=%s, which holds the token", r.msg, k, v)
			}
			if len(v) > replyLogBound {
				t.Errorf("%q logged %s with %d bytes, want at most %d", r.msg, k, len(v), replyLogBound)
			}
		}
	}
}

func replyLogPairs(n int) []Candidate {
	out := make([]Candidate, n)
	for i := range out {
		out[i] = Candidate{
			NewerID: "N" + strconv.Itoa(i+1), NewerContent: "newer note " + strconv.Itoa(i+1),
			OlderID: "O" + strconv.Itoa(i+1), OlderContent: "older note " + strconv.Itoa(i+1),
		}
	}
	return out
}

func TestClassifierLogsTheReplyBoundedAndGuarded(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	type line struct {
		msg, key string
		count    int
	}
	for _, tc := range []struct {
		name  string
		resp  string // every call answers this
		pairs int
		lines []line
		// wantInLog must appear in every checked line's value.
		wantInLog []string
	}{
		{
			name:  "an unparseable batch reply holding a token, and its fallback",
			resp:  "1: perhaps " + replyLogToken + "\n2: perhaps",
			pairs: 2,
			lines: []line{
				{"batch reply unparseable", "reply", 1},
				{"unparseable verdict in batch fallback", "error", 2},
			},
			wantInLog: []string{"withheld", "GitHub"},
		},
		{
			name:      "a batch reply missing a verdict, holding a token",
			resp:      "1: NEITHER\n2: hmm " + replyLogToken,
			pairs:     2,
			lines:     []line{{"batch reply missing verdicts", "reply", 1}},
			wantInLog: []string{"withheld", "GitHub"},
		},
		{
			name:      "a lone pair's unparseable reply holding a token",
			resp:      "perhaps " + replyLogToken,
			pairs:     1,
			lines:     []line{{"supersede: unparseable verdict", "error", 1}},
			wantInLog: []string{"withheld", "GitHub"},
		},
		{
			name:  "an unparseable 100 KB reply, and its fallback",
			resp:  big,
			pairs: 2,
			lines: []line{
				{"batch reply unparseable", "reply", 1},
				{"unparseable verdict in batch fallback", "error", 2},
			},
			wantInLog: []string{"withheld", strconv.Itoa(len(big)) + " bytes"},
		},
		{
			name:      "a 100 KB reply missing a verdict",
			resp:      "1: NEITHER\n2: " + big,
			pairs:     2,
			lines:     []line{{"batch reply missing verdicts", "reply", 1}},
			wantInLog: []string{"withheld", strconv.Itoa(len("1: NEITHER\n2: "+big)) + " bytes"},
		},
		{
			name:      "an ordinary short reply stays readable",
			resp:      "1: NEITHER\n2: hmm, not sure",
			pairs:     2,
			lines:     []line{{"batch reply missing verdicts", "reply", 1}},
			wantInLog: []string{"1: NEITHER", "2: hmm, not sure"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &capturedLog{}
			cls := NewRelationClassifier(&fakeProvider{resp: tc.resp})
			cls.SetRetryDelay(0)
			cls.SetLogger(log.logger())
			if _, err := cls.ClassifyBatch(context.Background(), replyLogPairs(tc.pairs)); err != nil {
				t.Fatalf("ClassifyBatch: %v", err)
			}
			for _, l := range tc.lines {
				recs := log.find(l.msg)
				if len(recs) != l.count {
					t.Fatalf("records for %q = %d, want %d; log:\n%s", l.msg, len(recs), l.count, dumpRecords(log))
				}
				for _, r := range recs {
					v, ok := r.attrs[l.key]
					if !ok {
						t.Fatalf("%q carries no %s attribute: %v", r.msg, l.key, r.attrs)
					}
					for _, want := range tc.wantInLog {
						if !strings.Contains(v, want) {
							t.Errorf("%q logged %s=%s, want it to hold %q", r.msg, l.key, v, want)
						}
					}
				}
			}
			assertReplyLogClean(t, log)
		})
	}
}

// TestUnparseableVerdictErrorCarriesARenderedReply: the error is what every
// caller logs, so the reply is rendered where the error is built rather than at
// each print. It still matches the sentinel.
func TestUnparseableVerdictErrorCarriesARenderedReply(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	for _, tc := range []struct {
		name, resp string
		want       []string
	}{
		{"a token", "perhaps " + replyLogToken, []string{"withheld", "GitHub"}},
		{"a 100 KB reply", big, []string{"withheld", strconv.Itoa(len(big)) + " bytes"}},
		{"a short reply", "perhaps, not sure", []string{"perhaps, not sure"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cls := NewRelationClassifier(&fakeProvider{resp: tc.resp})
			_, err := cls.Classify(context.Background(), replyLogPairs(1)[0])
			if !errors.Is(err, errUnparseableVerdict) {
				t.Fatalf("Classify: err = %v, want errUnparseableVerdict", err)
			}
			msg := err.Error()
			if strings.Contains(msg, replyLogFragment) {
				t.Errorf("error holds the token: %s", msg)
			}
			if len(msg) > replyLogBound {
				t.Errorf("error is %d bytes, want at most %d", len(msg), replyLogBound)
			}
			for _, w := range tc.want {
				if !strings.Contains(msg, w) {
					t.Errorf("error = %s, want it to hold %q", msg, w)
				}
			}
		})
	}
}
