package memory

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func sampleSearchRecord() SearchRecord {
	return SearchRecord{
		ProjectID: testProject,
		Source:    "search",
		Query:     "k3s ingress certificate rotation",
		Scope:     map[string]string{"environment": "production"},
		Category:  "gotcha",
		Retention: "project",
		Results:   []string{"MEM-3", "MEM-1", "MEM-2"},
	}
}

func TestRecordSearchStoresTheQueryAndItsRankedResults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if err := s.RecordSearch(ctx, sampleSearchRecord()); err != nil {
		t.Fatalf("RecordSearch: %v", err)
	}
	rows, err := s.SearchRecords(ctx, 10)
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("SearchRecords returned %d rows, want 1", len(rows))
	}
	got := rows[0]
	want := sampleSearchRecord()
	if got.Query != want.Query || got.ProjectID != want.ProjectID || got.Source != want.Source {
		t.Errorf("got query/project/source = %q/%q/%q, want %q/%q/%q",
			got.Query, got.ProjectID, got.Source, want.Query, want.ProjectID, want.Source)
	}
	if got.Category != want.Category || got.Retention != want.Retention {
		t.Errorf("got category/retention = %q/%q, want %q/%q", got.Category, got.Retention, want.Category, want.Retention)
	}
	if got.Scope["environment"] != "production" {
		t.Errorf("scope = %v, want environment=production", got.Scope)
	}
	if strings.Join(got.Results, ",") != strings.Join(want.Results, ",") {
		t.Errorf("results = %v, want %v (rank order is load-bearing)", got.Results, want.Results)
	}
}

func TestRecordSearchWritesExactlyOneAuditRowPerCall(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := s.RecordSearch(ctx, sampleSearchRecord()); err != nil {
			t.Fatalf("RecordSearch %d: %v", i, err)
		}
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'search'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 3 {
		t.Errorf("audit search rows = %d, want 3", n)
	}
}

func TestRecordSearchRefusesATokenShapedQueryAndStoresNothing(t *testing.T) {
	s := testStore(t)
	rec := sampleSearchRecord()
	rec.Query = "the key is ghp_" + strings.Repeat("a", 36)
	err := s.RecordSearch(context.Background(), rec)
	if !errors.Is(err, ErrSecretContent) {
		t.Fatalf("RecordSearch error = %v, want ErrSecretContent", err)
	}
	if strings.Contains(err.Error(), rec.Query) {
		t.Errorf("refusal echoed the caller's query: %v", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("audit_log holds %d rows after a refused query, want 0", n)
	}
}

func TestRecordSearchRefusesATokenShapedScopeValueAndStoresNothing(t *testing.T) {
	s := testStore(t)
	rec := sampleSearchRecord()
	rec.Scope = map[string]string{"token": "ghp_" + strings.Repeat("a", 36)}
	if err := s.RecordSearch(context.Background(), rec); !errors.Is(err, ErrSecretContent) {
		t.Fatalf("RecordSearch error = %v, want ErrSecretContent", err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_log`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("audit_log holds %d rows after a refused scope, want 0", n)
	}
}

func TestRecordSearchEvictsOldestSearchRowsOnly(t *testing.T) {
	s := testStore(t)
	oldCap := searchRecordRowsCap
	searchRecordRowsCap = 3
	t.Cleanup(func() { searchRecordRowsCap = oldCap })
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		rec := sampleSearchRecord()
		rec.Query = "query-" + string(rune('a'+i))
		if err := s.RecordSearch(ctx, rec); err != nil {
			t.Fatalf("RecordSearch %d: %v", i, err)
		}
	}
	// An unrelated audit action is never touched by the search cap.
	if _, err := s.db.Exec(`INSERT INTO audit_log (action, details) VALUES ('reflect', '{}')`); err != nil {
		t.Fatalf("seed non-search row: %v", err)
	}
	rows, err := s.SearchRecords(ctx, 100)
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("SearchRecords returned %d rows, want 3", len(rows))
	}
	for _, row := range rows {
		if row.Query == "query-a" || row.Query == "query-b" {
			t.Errorf("evicted query %q is still present", row.Query)
		}
	}
	var all int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'search'`).Scan(&all); err != nil {
		t.Fatalf("count: %v", err)
	}
	if all != 3 {
		t.Errorf("search rows = %d, want 3", all)
	}
	var reflect int
	if err := s.db.QueryRow(`SELECT count(*) FROM audit_log WHERE action = 'reflect'`).Scan(&reflect); err != nil {
		t.Fatalf("count: %v", err)
	}
	if reflect != 1 {
		t.Errorf("non-search audit rows = %d, want 1", reflect)
	}
}

func TestSearchRecordsReturnsNewestFirstAndHonorsTheLimit(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, q := range []string{"first", "second", "third"} {
		rec := sampleSearchRecord()
		rec.Query = q
		if err := s.RecordSearch(ctx, rec); err != nil {
			t.Fatalf("RecordSearch %q: %v", q, err)
		}
	}
	rows, err := s.SearchRecords(ctx, 2)
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("SearchRecords(2) returned %d rows, want 2", len(rows))
	}
	if rows[0].Query != "third" || rows[1].Query != "second" {
		t.Errorf("order = %q, %q; want newest first: third, second", rows[0].Query, rows[1].Query)
	}
}

func TestSearchRecordsToleratesAnUnreadableDetailsDocument(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO audit_log (action, details) VALUES ('search', 'not json')`); err != nil {
		t.Fatalf("seed bad row: %v", err)
	}
	rows, err := s.SearchRecords(ctx, 10)
	if err != nil {
		t.Fatalf("SearchRecords: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("SearchRecords returned %d rows, want 1", len(rows))
	}
	if rows[0].Query != "" || len(rows[0].Results) != 0 {
		t.Errorf("unreadable row decoded to %+v, want an empty record", rows[0])
	}
}
