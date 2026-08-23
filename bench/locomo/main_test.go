package main

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestLoadConversation(t *testing.T) {
	raw := `{
	  "sample_id": "conv-test",
	  "qa": [],
	  "conversation": {
	    "session_1": [
	      {"speaker": "Alice", "dia_id": "D1:1", "text": "hi bob"},
	      {"speaker": "Bob", "dia_id": "D1:2", "text": "hey alice"}
	    ],
	    "session_1_date_time": "7 May 2023, Sunday 5:00pm",
	    "session_2_date_time": "14 May 2023",
	    "session_3": [
	      {"speaker": "Alice", "dia_id": "D3:1", "text": "the trip is booked"}
	    ]
	  }
	}`
	var c locoConversation
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	sessions := loadConversation(c)
	if len(sessions) != 2 {
		t.Fatalf("want 2 content-bearing sessions (date-only session_2 skipped), got %d", len(sessions))
	}
	if sessions[0].ID != "conv-test-s01" || sessions[0].Date == "" || len(sessions[0].Turns) != 2 {
		t.Fatalf("session 0 wrong: %+v", sessions[0])
	}
	if sessions[1].Num != 3 || sessions[1].Date != "" {
		t.Fatalf("session 3 wrong: %+v", sessions[1])
	}
}

// TestScoreTurns pins the turn-level metric semantics on a fixed ranking:
// gold at positions 3 and 7 (1-based).
func TestScoreTurns(t *testing.T) {
	gold := map[string]bool{"D3": true, "D7": true}
	ranked := []rankedTurn{
		{DiaID: "D1"}, {DiaID: "D2"},
		{DiaID: "D3"}, {DiaID: "D4"}, {DiaID: "D5"}, {DiaID: "D6"},
		{DiaID: "D7"}, {DiaID: "D8"}, {DiaID: "D9"}, {DiaID: "D10"}, {DiaID: "D11"},
	}
	r1, r5, r10, mrr, ndcg := scoreTurns(ranked, gold)
	if r1 != 0 || r5 != 1 || r10 != 1 {
		t.Fatalf("recall wrong: %f %f %f", r1, r5, r10)
	}
	if mrr <= 0.32 || mrr >= 0.34 { // 1/3
		t.Fatalf("mrr wrong: %f", mrr)
	}
	// Golds at ranks 3 and 7 (discount log2(rank+1)); ideal puts them at
	// ranks 1 and 2.
	dcg := 1/math.Log2(4) + 1/math.Log2(8)
	idcg := 1/math.Log2(2) + 1/math.Log2(3)
	if math.Abs(ndcg-dcg/idcg) > 1e-9 {
		t.Fatalf("ndcg wrong: got %f want %f", ndcg, dcg/idcg)
	}
}

func TestScoreSessions_HitWithinK(t *testing.T) {
	ranked := []rankedTurn{
		{SessionID: "s1"}, {SessionID: "s1"}, // dup collapses
		{SessionID: "s2"}, {SessionID: "s3"},
	}
	if scoreSessions(ranked, map[string]bool{"s3": true}, 2) != 0 {
		t.Fatal("s3 is rank 3; hit@2 must be 0")
	}
	if scoreSessions(ranked, map[string]bool{"s3": true}, 3) != 1 {
		t.Fatal("s3 is rank 3 after dedup; hit@3 must be 1")
	}
	if scoreSessions(ranked, map[string]bool{"s9": true}, 10) != 0 {
		t.Fatal("absent gold must never hit")
	}
}

func TestMemoryTextIncludesDateAndSpeaker(t *testing.T) {
	s := locoSession{Num: 2, Date: "14 May 2023", Turns: []locoTurn{{Speaker: "Bob"}}}
	got := memoryText(s, locoTurn{Speaker: "Bob", Text: "trip booked"})
	if !strings.Contains(got, "[Session 2 | 14 May 2023]") || !strings.Contains(got, "Bob: trip booked") {
		t.Fatalf("memory text wrong: %q", got)
	}
}
