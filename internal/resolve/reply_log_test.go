package resolve

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// The classifier's reply and the ids on the Debug lines reach the phase's
// stderr, which a detached lifecycle run appends to lifecycle.log. These tests
// pin that what reaches that log is bounded and token-guarded: the reply goes
// through memory.RefusalArg, like the ids beside it.

// replyLogToken is a GitHub-token-shaped value. Assembled rather than written
// out, because GitHub push protection matches the format anywhere in a diff.
// replyLogFragment is part of its body, so a partial leak fails too.
var (
	replyLogToken    = "ghp_" + strings.Repeat("a1B2c3D4e5F6", 3) + "AbCd"
	replyLogFragment = "a1B2c3D4e5F6"
)

// replyLogBound is the most any one logged value may hold: the rendered
// argument (at most ArgEchoMaxBytes, quoted) plus room for its placeholder.
const replyLogBound = 2*memory.ArgEchoMaxBytes + 64

type replyRecord struct {
	level slog.Level
	msg   string
	attrs map[string]string
}

// replyLog is a slog.Handler that keeps every record, at every level, so a
// test reads each attribute exactly as a text handler would print it.
type replyLog struct {
	mu      sync.Mutex
	records []replyRecord
}

func (c *replyLog) Enabled(context.Context, slog.Level) bool { return true }
func (c *replyLog) Handle(_ context.Context, r slog.Record) error {
	rec := replyRecord{level: r.Level, msg: r.Message, attrs: map[string]string{}}
	r.Attrs(func(a slog.Attr) bool {
		rec.attrs[a.Key] = a.Value.String()
		return true
	})
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, rec)
	return nil
}
func (c *replyLog) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *replyLog) WithGroup(string) slog.Handler      { return c }

// only returns the one record whose message holds substr.
func (c *replyLog) only(t *testing.T, substr string) replyRecord {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	var got []replyRecord
	for _, r := range c.records {
		if strings.Contains(r.msg, substr) {
			got = append(got, r)
		}
	}
	if len(got) != 1 {
		t.Fatalf("records for %q = %d, want 1; log: %+v", substr, len(got), c.records)
	}
	return got[0]
}

// assertNothingLeaks checks every value of every record: no fragment of the
// token, and nothing over the bound.
func (c *replyLog) assertNothingLeaks(t *testing.T, fragments ...string) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		for k, v := range r.attrs {
			for _, f := range fragments {
				if strings.Contains(v, f) || strings.Contains(r.msg, f) {
					t.Errorf("%q logged %s=%s, which holds the token", r.msg, k, v)
				}
			}
			if len(v) > replyLogBound {
				t.Errorf("%q logged %s with %d bytes, want at most %d", r.msg, k, len(v), replyLogBound)
			}
		}
	}
}

func TestClassifyChunkLogsTheReplyBoundedAndGuarded(t *testing.T) {
	big := strings.Repeat("x", 100_000)
	for _, tc := range []struct {
		name string
		// resps is what the fake harness answers, call by call: the batch reply,
		// then the per-note fallback answers when the batch is unparseable.
		resps     []string
		msg       string
		wantInLog []string
	}{
		{
			name:      "an unparseable reply holding a token",
			resps:     []string{"1: maybe, the note quotes " + replyLogToken + "\n2: maybe", "KEEP", "KEEP"},
			msg:       "batch reply unparseable",
			wantInLog: []string{"withheld", "GitHub"},
		},
		{
			name:      "a reply missing a verdict, holding a token",
			resps:     []string{"1: KEEP\n2: hmm " + replyLogToken},
			msg:       "batch reply missing verdicts",
			wantInLog: []string{"withheld", "GitHub"},
		},
		{
			name:      "an unparseable 100 KB reply",
			resps:     []string{big, "KEEP", "KEEP"},
			msg:       "batch reply unparseable",
			wantInLog: []string{"withheld", strconv.Itoa(len(big)) + " bytes"},
		},
		{
			name:      "a 100 KB reply missing a verdict",
			resps:     []string{"1: KEEP\n2: " + big},
			msg:       "batch reply missing verdicts",
			wantInLog: []string{"withheld", strconv.Itoa(len("1: KEEP\n2: "+big)) + " bytes"},
		},
		{
			name:      "an ordinary short reply stays readable",
			resps:     []string{"1: KEEP\n2: hmm, not sure"},
			msg:       "batch reply missing verdicts",
			wantInLog: []string{"1: KEEP", "2: hmm, not sure"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &replyLog{}
			cls := NewResolutionClassifier(&fakeProvider{resps: tc.resps})
			cls.SetLogger(slog.New(log))
			if _, err := cls.IsResolvedBatch(context.Background(), []string{"note a", "note b"}); err != nil {
				t.Fatalf("IsResolvedBatch: %v", err)
			}
			rec := log.only(t, tc.msg)
			reply, ok := rec.attrs["reply"]
			if !ok {
				t.Fatalf("%q carries no reply attribute: %v", rec.msg, rec.attrs)
			}
			for _, want := range tc.wantInLog {
				if !strings.Contains(reply, want) {
					t.Errorf("reply=%s, want it to hold %q", reply, want)
				}
			}
			log.assertNothingLeaks(t, replyLogFragment, strings.Repeat("x", replyLogBound))
		})
	}
}

// TestResolveVetoLogWithholdsStoredIDs pins the Debug "resolve veto kept memory"
// line: its id is a stored id, and the line reaches the phase's stderr and, on
// the detached path, lifecycle.log, so it must go through memory.RefusalArg like
// the reply lines above.
func TestResolveVetoLogWithholdsStoredIDs(t *testing.T) {
	log := &replyLog{}
	rule := memory.Memory{ID: replyLogToken, Category: "gotcha",
		Content: "NEVER run `dingo database restore` with source and target on the same spindle. Fixed in the 0.31 release notes."}
	store := &fakeStore{candidates: []memory.Memory{rule}}
	// The classifier would resolve the rule; the veto must stop it before the
	// call and log the id it settled on.
	cls := &fakeClassifier{drop: map[string]bool{rule.Content: true}}

	if _, _, err := Run(context.Background(), store, cls, "proj", true, slog.New(log)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	rec := log.only(t, "resolve veto kept memory")
	if got := rec.attrs["id"]; strings.Contains(got, replyLogFragment) || !strings.Contains(got, "withheld") {
		t.Errorf("the veto line logged id=%s, want the guard's placeholder", got)
	}
	log.assertNothingLeaks(t, replyLogFragment)
}

// TestReassessVetoLogsWithholdStoredIDs pins the repair pass's two veto lines:
// the deferred-to-classifier line names the vetoed id AND the newer id that
// shadowed it, and the kept line names the vetoed id. All are stored ids, and
// both lines reach the detached lifecycle.log, so each must go through
// memory.RefusalArg.
func TestReassessVetoLogsWithholdStoredIDs(t *testing.T) {
	t.Run("veto kept", func(t *testing.T) {
		log := &replyLog{}
		rule := memory.Memory{ID: replyLogToken, Category: "gotcha",
			Content: "NEVER run `dingo database restore` with source and target on the same spindle"}
		store := &fakeStore{alreadyResolved: []memory.Memory{rule}}
		cls := &fakeClassifier{}

		if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, slog.New(log)); err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		rec := log.only(t, "reassess veto kept memory")
		if got := rec.attrs["id"]; strings.Contains(got, replyLogFragment) || !strings.Contains(got, "withheld") {
			t.Errorf("the veto line logged id=%s, want the guard's placeholder", got)
		}
		log.assertNothingLeaks(t, replyLogFragment)
	})

	t.Run("veto deferred", func(t *testing.T) {
		log := &replyLog{}
		changelog := memory.Memory{ID: replyLogToken, Category: "changelog", UpdatedAt: "2026-09-01 00:00:00",
			Content: "changelog 0.36.0: always check `internal/resolve/reassess.go` before a repair; #698 landed"}
		newer := memory.Memory{ID: replyLogToken, Category: "changelog", UpdatedAt: "2026-09-20 00:00:00",
			Content: "#698 is closed: internal/resolve/reassess.go now takes --only, so the 0.36.0 note is history"}
		store := &fakeStore{alreadyResolved: []memory.Memory{changelog}, candidates: []memory.Memory{newer}}
		cls := &fakeClassifier{}

		if _, _, err := Reassess(context.Background(), store, cls, "proj", true, Scope{}, slog.New(log)); err != nil {
			t.Fatalf("Reassess: %v", err)
		}
		rec := log.only(t, "reassess veto deferred to the classifier")
		for _, key := range []string{"id", "newer"} {
			got := rec.attrs[key]
			if strings.Contains(got, replyLogFragment) || !strings.Contains(got, "withheld") {
				t.Errorf("the deferred line logged %s=%s, want the guard's placeholder", key, got)
			}
		}
		log.assertNothingLeaks(t, replyLogFragment)
	})
}
