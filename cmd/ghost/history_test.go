package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

func TestParseHistoryArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    historyOptions
		wantErr string
	}{
		{
			name: "id only",
			args: []string{"abc123"},
			want: historyOptions{MemoryID: "abc123"},
		},
		{
			name: "id with limit and json",
			args: []string{"abc123", "--limit", "5", "--json"},
			want: historyOptions{MemoryID: "abc123", Limit: 5, JSON: true},
		},
		{
			name: "attached limit value",
			args: []string{"--limit=7", "abc123"},
			want: historyOptions{MemoryID: "abc123", Limit: 7},
		},
		{
			name:    "no id",
			args:    []string{"--json"},
			wantErr: "a memory id is required",
		},
		{
			name:    "two ids",
			args:    []string{"abc123", "def456"},
			wantErr: "history takes one memory id",
		},
		{
			name:    "unknown flag",
			args:    []string{"abc123", "--verbose"},
			wantErr: `unknown flag "--verbose"`,
		},
		{
			name:    "limit without a value",
			args:    []string{"abc123", "--limit"},
			wantErr: "flag --limit needs a value",
		},
		{
			name:    "limit zero is not unlimited",
			args:    []string{"abc123", "--limit", "0"},
			wantErr: "--limit needs a positive whole number",
		},
		{
			name:    "limit not a number",
			args:    []string{"abc123", "--limit", "many"},
			wantErr: "--limit needs a positive whole number",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHistoryArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("parseHistoryArgs(%v) = %+v, want error containing %q", tt.args, got, tt.wantErr)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHistoryArgs(%v): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parseHistoryArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// historyEntry builds a recorded event for the printer tests, so the output
// contract is asserted without a database or a store.
func historyEntry(phase, agent, content, recordedAt string) memory.HistoryEntry {
	return memory.HistoryEntry{
		ID:         "entry-" + phase,
		MemoryID:   "m1",
		ProjectID:  "p1",
		RecordedAt: recordedAt,
		Phase:      phase,
		Agent:      agent,
		Content:    content,
		Category:   "gotcha",
		Importance: 0.7,
		Source:     "mcp",
	}
}

func TestPrintMemoryHistory_EmptyStates(t *testing.T) {
	var noDB bytes.Buffer
	if err := printMemoryHistory(&noDB, historyView{MemoryID: "m1", NoDatabase: true}); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	if !strings.Contains(noDB.String(), "no Ghost database") {
		t.Errorf("no-database state printed nothing explicit:\n%s", noDB.String())
	}

	var unknown bytes.Buffer
	if err := printMemoryHistory(&unknown, historyView{MemoryID: "m1"}); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	if !strings.Contains(unknown.String(), "no memory and no history recorded for m1") {
		t.Errorf("an id with neither a memory nor a history printed nothing explicit:\n%s", unknown.String())
	}
	if strings.Contains(unknown.String(), "no longer live") {
		t.Errorf("an id that was never written is reported as a deleted memory:\n%s", unknown.String())
	}
}

// TestReadHistoryViewReportsAFailedLivenessRead: the header distinguishes "still
// live" from "no longer live" and nothing else, so a liveness read that FAILS
// must not be reported as a memory that is gone. runHistory ends in os.Exit, so
// the read is a function and this is its test: a closed database is a real read
// failure, and it must surface as an error rather than as an empty result.
func TestReadHistoryViewReportsAFailedLivenessRead(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	s := memory.NewStore(db, nil)
	if err := s.EnsureProject(context.Background(), "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	entries := []memory.HistoryEntry{{MemoryID: "m1", Phase: "save", Content: "text"}}
	view, err := withLiveness(context.Background(), s, "m1", entries)
	if err == nil {
		t.Fatalf("a failed liveness read returned a view instead of an error: %+v", view)
	}
	if view.Live != nil {
		t.Error("a failed liveness read produced a claim; the printer would have said the memory is gone")
	}
}

// TestReadHistoryViewCarriesTheLiveRow: the ordinary path — a live memory's
// history is reported as live, and a memory that is not there is not an error.
func TestReadHistoryViewCarriesTheLiveRow(t *testing.T) {
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer func() { _ = db.Close() }()
	s := memory.NewStore(db, nil)
	ctx := context.Background()
	if err := s.EnsureProject(ctx, "p", "/tmp/p", "p"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	id, err := s.Create(ctx, "p", memory.Memory{Category: "fact", Content: "a live fact", Source: "mcp", Importance: 0.5})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	view, err := readHistoryView(ctx, s, id, 0)
	if err != nil {
		t.Fatalf("readHistoryView: %v", err)
	}
	if view.Live == nil {
		t.Error("a live memory was reported as not live")
	}
	if view.MemoryID != id || len(view.Entries) == 0 {
		t.Errorf("view = %+v, want the id and its history", view)
	}

	gone, err := readHistoryView(ctx, s, "no-such-memory", 0)
	if err != nil {
		t.Fatalf("a memory that was never written must not be an error: %v", err)
	}
	if gone.Live != nil || len(gone.Entries) != 0 {
		t.Errorf("view for an unknown id = %+v, want empty and not live", gone)
	}
}

func TestParseHistoryArgsPurge(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    historyOptions
		wantErr string
	}{
		{
			name: "purge with an id",
			args: []string{"purge", "abc123"},
			want: historyOptions{MemoryID: "abc123", Purge: true},
		},
		{
			name:    "purge with no id",
			args:    []string{"purge"},
			wantErr: "a memory id is required",
		},
		{
			name:    "purge and limit contradict each other",
			args:    []string{"purge", "abc123", "--limit", "5"},
			wantErr: "--limit has nothing to limit when purging",
		},
		{
			name:    "purge and json contradict each other",
			args:    []string{"purge", "abc123", "--json"},
			wantErr: "--json has nothing to print when purging",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseHistoryArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("parseHistoryArgs(%v) error = %v, want it to contain %q", tt.args, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseHistoryArgs(%v): %v", tt.args, err)
			}
			if got != tt.want {
				t.Errorf("parseHistoryArgs(%v) = %+v, want %+v", tt.args, got, tt.want)
			}
		})
	}
}

// TestRunHistoryPurgeTakesAFullIDOnly: a purge erases a memory's text for good,
// and the argument that says which memory is the only thing standing between an
// operator and a mistake nothing can undo. The read path takes the eight
// characters a report prints (#720); this one does not, and the refusal says so
// instead of reporting the memory as absent — which is what a prefix-shaped
// argument used to be answered with, and it is false whenever the memory is
// there.
func TestRunHistoryPurgeTakesAFullIDOnly(t *testing.T) {
	s, live, gone := refTestStore(t)
	ctx := context.Background()
	_ = live

	// A prefix of a live memory, and of one already deleted: both are ids this
	// store holds and neither may be purged by a prefix.
	for _, tc := range []struct{ what, ref string }{
		{"a live memory", memref.Short(refTestLiveID)},
		{"an already deleted memory", memref.Short(gone)},
	} {
		t.Run("a prefix of "+tc.what, func(t *testing.T) {
			err := purgeHistoryMemory(ctx, s, tc.ref)
			if err == nil {
				t.Fatal("a purge resolved a prefix")
			}
			if !strings.Contains(err.Error(), "whole memory id") {
				t.Errorf("the refusal does not say a whole id is needed: %v", err)
			}
		})
	}

	// And nothing was erased by either refusal: the read that decides is a read,
	// but the guarantee has to be that no text went with it.
	if entries, err := s.MemoryHistory(ctx, gone, 0); err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	} else if len(entries) == 0 {
		t.Error("a refused prefix purge erased the deleted memory's recorded text")
	}
	if rows, err := s.GetByIDs(ctx, []string{refTestLiveID}); err != nil {
		t.Fatalf("GetByIDs: %v", err)
	} else if len(rows) == 0 {
		t.Error("a refused prefix purge deleted the live memory")
	}
}

// historyTokenID is an AWS-access-key-shaped id, the shape a memory id can hold:
// `ghost import` writes an artifact's ids verbatim and CheckImportedID bounds only
// length and control characters, so a stored id — and a ref a caller types for one
// — can be token-shaped. The refusal sentences on this command render every such
// value through memory.RefusalArg, the same renderer internal/memref's miss
// refusal uses, so none of them hands the value back.
const historyTokenID = "AKIAIOSFODNN7EXAMPLE"

// assertHistoryRefusalWithheld asserts an error names the field and the format of
// a token-shaped value and holds no part of the value. The placeholder is the one
// memory.RefusalArg returns for a value under its cap.
func assertHistoryRefusalWithheld(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a refusal")
	}
	msg := err.Error()
	if strings.Contains(msg, historyTokenID) {
		t.Errorf("the refusal echoes the token-shaped value:\n%s", msg)
	}
	if want := "<" + field + " withheld: it holds a AWS access key ID>"; !strings.Contains(msg, want) {
		t.Errorf("the refusal does not carry %q:\n%s", want, msg)
	}
}

// TestPurgePrefixRefusalWithholdsATokenShapedRef: an argument that names nothing
// is the miss sentence, and a token-shaped one is withheld there.
func TestPurgePrefixRefusalWithholdsATokenShapedRef(t *testing.T) {
	assertHistoryRefusalWithheld(t, purgePrefixRefusal(nil, historyTokenID), "id")
}

// TestPurgePrefixRefusalWithholdsTheRefItWraps: memref's own refusal is wrapped
// in front of this command's sentence, so the clause this command owns renders
// the ref through the guard too rather than adding a second, raw copy of it. This
// asserts the placeholder the clause adds and not the absence of the value,
// because the copies inside memref's own sentence are memref's to render.
func TestPurgePrefixRefusalWithholdsTheRefItWraps(t *testing.T) {
	ids := []string{historyTokenID + "AAAA", historyTokenID + "BBBB"}
	err := purgePrefixRefusal(ids, historyTokenID)
	if err == nil {
		t.Fatal("purgePrefixRefusal accepted an ambiguous ref")
	}
	if want := "<id withheld: it holds a AWS access key ID>"; !strings.Contains(err.Error(), want) {
		t.Errorf("the refusal does not render the wrapped ref through the guard:\n%s", err)
	}
}

// TestRunHistoryPurgeErasesTheFullID: the full-id path is unchanged — the row and
// its recorded text go in one transaction, and the report says how much text was
// about to go. This is the regression guard for the asymmetry above: refusing a
// prefix must not cost a full id the thing the command exists to do.
func TestRunHistoryPurgeErasesTheFullID(t *testing.T) {
	s, live, _ := refTestStore(t)
	ctx := context.Background()

	if err := purgeHistoryMemory(ctx, s, live); err != nil {
		t.Fatalf("purgeHistoryMemory: %v", err)
	}
	if rows, err := s.GetByIDs(ctx, []string{live}); err != nil {
		t.Fatalf("GetByIDs: %v", err)
	} else if len(rows) != 0 {
		t.Error("the purge left the live row")
	}
	if entries, err := s.MemoryHistory(ctx, live, 0); err != nil {
		t.Fatalf("MemoryHistory: %v", err)
	} else if len(entries) != 0 {
		t.Errorf("the purge left %d recorded version(s)", len(entries))
	}
}

// TestPrintHistoryEntryNamesBothEndsOfAMove: a move row without its source and
// destination says nothing a reader can follow.
func TestPrintHistoryEntryNamesBothEndsOfAMove(t *testing.T) {
	var buf bytes.Buffer
	e := historyEntry("move", "claude-code", "text", "2026-09-27 09:00:00")
	e.FromProjectID = "proj-a"
	e.ToProjectID = "proj-b"
	if err := printHistoryEntry(&buf, e); err != nil {
		t.Fatalf("printHistoryEntry: %v", err)
	}
	if !strings.Contains(buf.String(), "moved: proj-a -> proj-b") {
		t.Errorf("output does not name both ends of the move:\n%s", buf.String())
	}
	var plain bytes.Buffer
	if err := printHistoryEntry(&plain, historyEntry("save", "claude-code", "text", "2026-09-27 09:00:00")); err != nil {
		t.Fatalf("printHistoryEntry: %v", err)
	}
	if strings.Contains(plain.String(), "moved:") {
		t.Errorf("a non-move row printed a moved line:\n%s", plain.String())
	}
}

// TestPrintHistoryJSONRefusesAnUnknownID: zero lines with exit 0 is the one
// answer a script cannot act on — "never written", "history pruned by the growth
// policy" and "the query found nothing" would all look the same. So the JSON form
// takes the same shape as the no-database refusal: an error object, and an error
// the caller turns into a non-zero exit.
func TestPrintHistoryJSONRefusesAnUnknownID(t *testing.T) {
	var buf bytes.Buffer
	err := printHistory(&buf, historyView{MemoryID: "nope"}, true)
	if !errors.Is(err, errNothingToReport) {
		t.Fatalf("printHistory error = %v, want errNothingToReport", err)
	}
	var got struct {
		Error string `json:"error"`
	}
	if jsonErr := json.Unmarshal(buf.Bytes(), &got); jsonErr != nil {
		t.Fatalf("the refusal is not JSON: %v\n%s", jsonErr, buf.String())
	}
	if got.Error == "" || !strings.Contains(got.Error, "nope") {
		t.Errorf("the refusal does not name the id: %q", got.Error)
	}
}

// TestPrintHistoryJSONRefusesALiveMemoryWithNoHistory: the pre-v17 case. A live
// memory whose row predates the history table has zero entries, and an empty
// stream with exit 0 would tell a script the memory has no past — rather than
// that Ghost was not watching when it was written. The refusal fires on the
// entry count, not on liveness, and says which of the two misses it is.
func TestPrintHistoryJSONRefusesALiveMemoryWithNoHistory(t *testing.T) {
	var buf bytes.Buffer
	err := printHistory(&buf, historyView{
		MemoryID: "predates",
		Live:     &memory.Memory{ID: "predates", Category: "fact"},
	}, true)
	if !errors.Is(err, errNothingToReport) {
		t.Fatalf("printHistory error = %v, want errNothingToReport", err)
	}
	var got struct {
		Error string `json:"error"`
	}
	if jsonErr := json.Unmarshal(buf.Bytes(), &got); jsonErr != nil {
		t.Fatalf("the refusal is not JSON: %v\n%s", jsonErr, buf.String())
	}
	if !strings.Contains(got.Error, "predates") {
		t.Errorf("the refusal does not name the id: %q", got.Error)
	}
	if strings.Contains(got.Error, "no memory and no history") {
		t.Errorf("the refusal says there is no memory, but the row is live: %q", got.Error)
	}
	if !strings.Contains(got.Error, "schema v17") {
		t.Errorf("the refusal does not name the reason: %q", got.Error)
	}
}
