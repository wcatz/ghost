package main

import (
	"bytes"
	"encoding/json"
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
