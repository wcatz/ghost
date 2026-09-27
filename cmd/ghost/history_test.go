package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
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

func TestPrintMemoryHistory_PrintsEveryEventOldestFirst(t *testing.T) {
	var buf bytes.Buffer
	view := historyView{
		MemoryID: "m1",
		Live:     &memory.Memory{ID: "m1", Category: "gotcha", Source: "mcp"},
		Entries: []memory.HistoryEntry{
			historyEntry("save", "claude-code", "the first thing ghost was told", "2026-09-26 10:00:00"),
			historyEntry("update", "", "the second thing ghost was told", "2026-09-26 11:00:00"),
		},
	}
	if err := printMemoryHistory(&buf, view); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "live") {
		t.Errorf("output does not say the memory is live:\n%s", out)
	}
	saveAt := strings.Index(out, "2026-09-26 10:00:00  save")
	updateAt := strings.Index(out, "2026-09-26 11:00:00  update")
	if saveAt < 0 || updateAt < 0 {
		t.Fatalf("output is missing an event header:\n%s", out)
	}
	if saveAt > updateAt {
		t.Errorf("events printed newest first:\n%s", out)
	}
	// The agent is named when the write path knew it, and the content is
	// printed in full — a history whose text is elided cannot answer the
	// question it exists for.
	if !strings.Contains(out, "agent claude-code") {
		t.Errorf("output does not name the performing agent:\n%s", out)
	}
	if !strings.Contains(out, "agent unknown") {
		t.Errorf("output does not admit the update had no known agent:\n%s", out)
	}
	for _, want := range []string{"the first thing ghost was told", "the second thing ghost was told", "gotcha/mcp importance 0.70", "unresolved", "project p1"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestPrintMemoryHistory_SaysWhenTheMemoryIsGone(t *testing.T) {
	var buf bytes.Buffer
	deleted := "the old deploy script assumes a single region"
	if err := printMemoryHistory(&buf, historyView{
		MemoryID: "m1",
		Entries:  []memory.HistoryEntry{historyEntry("delete", "", deleted, "2026-09-26 12:00:00")},
	}); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "no longer live") {
		t.Errorf("output does not say the memory is gone, so a tombstone reads as a live record:\n%s", out)
	}
	if !strings.Contains(out, deleted) {
		t.Errorf("the tombstone lost the text the memory held:\n%s", out)
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

// TestPrintHistoryJSONIsOneObjectPerEntry: the machine-readable form must be
// pipeable — no header line, no prose — and must carry the store's own fields,
// so it cannot drift from what MemoryHistory returns.
func TestPrintHistoryJSONIsOneObjectPerEntry(t *testing.T) {
	var buf bytes.Buffer
	err := printHistory(&buf, historyView{
		MemoryID: "m1",
		Entries: []memory.HistoryEntry{
			historyEntry("save", "claude-code", "first", "2026-09-26 10:00:00"),
			historyEntry("merge", "opencode", "second", "2026-09-26 10:00:01"),
		},
	}, true)
	if err != nil {
		t.Fatalf("printHistory: %v", err)
	}
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2:\n%s", len(lines), buf.String())
	}
	for i, phase := range []string{"save", "merge"} {
		var got memory.HistoryEntry
		if err := json.Unmarshal([]byte(lines[i]), &got); err != nil {
			t.Fatalf("line %d is not JSON: %v\n%s", i, err, lines[i])
		}
		if got.Phase != phase {
			t.Errorf("line %d phase = %q, want %q", i, got.Phase, phase)
		}
		if got.Agent == "" {
			t.Errorf("line %d lost the performing agent", i)
		}
	}
}

// TestPrintHistoryJSONErrorIsDistinguishable: every other line of a --json
// stream is a history entry, so the refusal carries its own field. A consumer
// must not have to infer "nothing happened" from a stream that came back empty.
func TestPrintHistoryJSONErrorIsDistinguishable(t *testing.T) {
	var buf bytes.Buffer
	printHistoryJSONError(&buf, "no Ghost database yet (run ghost first) — no history to read")
	var got struct {
		Error      string `json:"error"`
		MemoryID   string `json:"memory_id"`
		RecordedAt string `json:"recorded_at"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("the refusal is not JSON: %v\n%s", err, buf.String())
	}
	if got.Error == "" {
		t.Error("the refusal carries no reason")
	}
	if got.MemoryID != "" || got.RecordedAt != "" {
		t.Errorf("the refusal also carries entry fields (%q, %q) — a consumer cannot tell the two shapes apart", got.MemoryID, got.RecordedAt)
	}
}

// TestMemoryHistoryLiveFlagDecidesTheHeader: the header's live / no-longer-live
// distinction comes from Live alone, so a reader who misreads it is told the
// wrong thing about a memory that is still there. This pins which combinations
// produce which sentence, because the printer cannot tell "the read failed" from
// "the row is gone" — runHistory refuses on the former instead.
func TestMemoryHistoryLiveFlagDecidesTheHeader(t *testing.T) {
	entry := historyEntry("save", "claude-code", "a fact worth keeping", "2026-09-26 10:00:00")

	var live bytes.Buffer
	if err := printMemoryHistory(&live, historyView{
		MemoryID: "m1",
		Live:     &memory.Memory{ID: "m1", Category: "gotcha", Source: "mcp"},
		Entries:  []memory.HistoryEntry{entry},
	}); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	if !strings.Contains(live.String(), "live)") || strings.Contains(live.String(), "no longer live") {
		t.Errorf("a live memory is not reported as live:\n%s", live.String())
	}

	var gone bytes.Buffer
	if err := printMemoryHistory(&gone, historyView{
		MemoryID: "m1",
		Entries:  []memory.HistoryEntry{entry},
	}); err != nil {
		t.Fatalf("printMemoryHistory: %v", err)
	}
	if !strings.Contains(gone.String(), "no longer live") {
		t.Errorf("a memory with no live row is not reported as gone:\n%s", gone.String())
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

	// The liveness read is driven on its own, with a history that did succeed:
	// a failure of BOTH reads is easy to produce, and it would pass even if the
	// liveness error were swallowed again.
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

// TestPrintHistoryEntryNamesTheOtherEnd: the two fields that make a history
// followable — which memory replaced this one, and what text a fold brought in.
// A reader asking "what replaced this" needs the answer on the same screen as the
// text, not only in the JSON.
func TestPrintHistoryEntryNamesTheOtherEnd(t *testing.T) {
	var buf bytes.Buffer
	e := historyEntry("delete", "", "the fact this memory used to hold", "2026-09-27 09:00:00")
	e.RelatedID = "SUCCESSOR01"
	e.MergedContent = "the near-duplicate a fold brought in"
	if err := printHistoryEntry(&buf, e); err != nil {
		t.Fatalf("printHistoryEntry: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "related memory: SUCCESSOR01") {
		t.Errorf("output does not name the successor:\n%s", out)
	}
	if !strings.Contains(out, "folded-in text: the near-duplicate a fold brought in") {
		t.Errorf("output does not carry the folded-in text:\n%s", out)
	}
	// A row with neither says nothing about either, rather than printing empty
	// labels on every event.
	var plain bytes.Buffer
	if err := printHistoryEntry(&plain, historyEntry("save", "claude-code", "text", "2026-09-27 09:00:00")); err != nil {
		t.Fatalf("printHistoryEntry: %v", err)
	}
	if strings.Contains(plain.String(), "related memory") || strings.Contains(plain.String(), "folded-in text") {
		t.Errorf("a row with no other end printed an empty label:\n%s", plain.String())
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

// TestPrintHistoryJSONStaysQuietWhenThereIsHistory: the refusal is for the miss
// only. A memory that has rows — live or tombstoned — prints them and returns no
// error, because the entries themselves carry the answer (a deleted memory ends
// in a delete row).
func TestPrintHistoryJSONStaysQuietWhenThereIsHistory(t *testing.T) {
	for _, v := range []historyView{
		{MemoryID: "live", Live: &memory.Memory{ID: "live"}, Entries: []memory.HistoryEntry{historyEntry("save", "", "text", "t")}},
		{MemoryID: "gone", Entries: []memory.HistoryEntry{historyEntry("delete", "", "text", "t")}},
	} {
		var buf bytes.Buffer
		if err := printHistory(&buf, v, true); err != nil {
			t.Errorf("printHistory(%s) = %v, want no error", v.MemoryID, err)
		}
		if !strings.Contains(buf.String(), "\"phase\"") {
			t.Errorf("printHistory(%s) printed no entries:\n%s", v.MemoryID, buf.String())
		}
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
	// It must NOT claim the memory does not exist — it does, and the sentence is
	// the difference between "Ghost has nothing on it" and "Ghost was not
	// watching yet".
	if strings.Contains(got.Error, "no memory and no history") {
		t.Errorf("the refusal says there is no memory, but the row is live: %q", got.Error)
	}
	if !strings.Contains(got.Error, "schema v17") {
		t.Errorf("the refusal does not name the reason: %q", got.Error)
	}
}
