package mcpserver

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

type pinCtxRow struct {
	id, project, content string
	importance           float64
	pinned               bool
	validUntil           string
}

func seedPinContext(t *testing.T, db *sql.DB, high int, extra ...pinCtxRow) {
	t.Helper()
	rows := make([]pinCtxRow, 0, high+len(extra))
	for i := 0; i < high; i++ {
		rows = append(rows, pinCtxRow{id: fmt.Sprintf("hi-%05d", i), project: "vproj", content: fmt.Sprintf("bulk row number %03d", i), importance: 0.9})
	}
	rows = append(rows, extra...)
	for _, r := range rows {
		var until any
		if r.validUntil != "" {
			until = r.validUntil
		}
		if _, err := db.Exec(`
			INSERT INTO memories (id, project_id, category, content, source, importance, pinned, valid_until, created_at, updated_at)
			VALUES (?, ?, 'fact', ?, 'manual', ?, ?, ?, '2026-01-02 03:04:05', '2026-01-02 03:04:05')`,
			r.id, r.project, r.content, r.importance, r.pinned, until); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
}

func newPinStore(t *testing.T) (*memory.Store, *sql.DB) {
	t.Helper()
	db, err := memory.OpenDB(":memory:")
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st := memory.NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if _, err := db.Exec(`INSERT INTO projects (id, path, name) VALUES ('_global', '_global', 'global')`); err != nil {
		t.Fatalf("insert _global: %v", err)
	}
	if err := st.EnsureProject(context.Background(), "vproj", t.TempDir(), "vproj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return st, db
}

var pinCtxNow = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

func TestAPinnedRowIsOnTheProjectContextSurfaces(t *testing.T) {
	for _, bulk := range []int{30, 60} {
		t.Run(fmt.Sprintf("%d bulk rows", bulk), func(t *testing.T) {
			st, db := newPinStore(t)
			seedPinContext(t, db, bulk, pinCtxRow{id: "the-pin01", project: "vproj", content: "the pinned standing rule", importance: 0.1, pinned: true})
			tool, err := ProjectContextAt(context.Background(), st, "vproj", 20, pinCtxNow)
			if err != nil {
				t.Fatalf("tool: %v", err)
			}
			if !strings.Contains(tool, "the pinned standing rule") {
				t.Errorf("the pinned row is not in ghost_project_context. Got:\n%s", tool)
			}
			res, err := ProjectResourceAt(context.Background(), st, "vproj", pinCtxNow)
			if err != nil {
				t.Fatalf("resource: %v", err)
			}
			if !strings.Contains(res, "the pinned standing rule") {
				t.Errorf("the pinned row is not in the project resource. Got:\n%s", res)
			}
		})
	}
}

func TestAnExpiredPinnedRowIsWithheldFromTheProjectContext(t *testing.T) {
	st, db := newPinStore(t)
	seedPinContext(t, db, 30, pinCtxRow{id: "exp-pin01", project: "vproj", content: "an expired pinned rule", importance: 0.1, pinned: true, validUntil: "2026-01-15 00:00:00"})
	tool, err := ProjectContextAt(context.Background(), st, "vproj", 20, pinCtxNow)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	res, err := ProjectResourceAt(context.Background(), st, "vproj", pinCtxNow)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	for name, got := range map[string]string{"tool": tool, "resource": res} {
		if strings.Contains(got, "an expired pinned rule") {
			t.Errorf("%s offers an expired pinned row. Got:\n%s", name, got)
		}
	}
}

func TestProjectContextSaysHowManyPinnedRowsItCut(t *testing.T) {
	st, db := newPinStore(t)
	var pins []pinCtxRow
	for i := 0; i < 25; i++ {
		pins = append(pins, pinCtxRow{id: fmt.Sprintf("pin-%05d", i), project: "vproj", content: fmt.Sprintf("pinned rule %02d", i), importance: 0.2 + float64(i)*0.01, pinned: true})
	}
	seedPinContext(t, db, 10, pins...)
	tool, err := ProjectContextAt(context.Background(), st, "vproj", 20, pinCtxNow)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if !strings.Contains(tool, "5 pinned memories cut") {
		t.Errorf("the tool does not say 5 pinned rows were cut. Got:\n%s", tool)
	}
	if strings.Contains(tool, "bulk row number") || strings.Contains(tool, "pinned rule 04") || !strings.Contains(tool, "pinned rule 05") {
		t.Errorf("the 20 shown rows are not the 20 best-ranked pinned ones. Got:\n%s", tool)
	}
	res, err := ProjectResourceAt(context.Background(), st, "vproj", pinCtxNow)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if !strings.Contains(res, "5 pinned memories cut") {
		t.Errorf("the resource does not say 5 pinned rows were cut. Got:\n%s", res)
	}
}

func TestProjectContextSaysNothingWhenEveryPinnedRowFits(t *testing.T) {
	st, db := newPinStore(t)
	seedPinContext(t, db, 30, pinCtxRow{id: "the-pin01", project: "vproj", content: "the pinned standing rule", importance: 0.1, pinned: true})
	tool, _ := ProjectContextAt(context.Background(), st, "vproj", 20, pinCtxNow)
	if strings.Contains(tool, "pinned memories cut") {
		t.Errorf("a cut is reported where no pinned row was cut. Got:\n%s", tool)
	}
}

func seedGlobalPins(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	var rows []pinCtxRow
	for i := 0; i < n; i++ {
		rows = append(rows, pinCtxRow{id: fmt.Sprintf("gpin-%04d", i), project: "_global", content: fmt.Sprintf("global pinned rule %02d", i), importance: 0.2 + float64(i)*0.005, pinned: true})
	}
	seedPinContext(t, db, 0, rows...)
}

func TestAGlobalOnlyWindowPutsTheCutUnderTheGlobalHeading(t *testing.T) {
	st, db := newPinStore(t)
	seedGlobalPins(t, db, 25)
	got, err := ProjectContextAt(context.Background(), st, "_global", 20, pinCtxNow)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	if strings.Contains(got, "## Memories") {
		t.Errorf("a note created an empty Memories section. Got:\n%s", got)
	}
	if !strings.Contains(got, "5 pinned global memories cut") {
		t.Errorf("the global cut is not reported. Got:\n%s", got)
	}
}

func TestAGlobalCutIsNotAttributedToTheProject(t *testing.T) {
	st, db := newPinStore(t)
	seedGlobalPins(t, db, 25)
	seedPinContext(t, db, 0, pinCtxRow{id: "own-pin01", project: "vproj", content: "an own pinned rule", importance: 0.9, pinned: true})
	got, err := ProjectContextAt(context.Background(), st, "vproj", 20, pinCtxNow)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	memories, global, _ := strings.Cut(got, "## Global")
	if strings.Contains(memories, "pinned memories cut") || strings.Contains(memories, "pinned global memories cut") {
		t.Errorf("a global cut is reported under Memories. Got:\n%s", got)
	}
	if !strings.Contains(global, "6 pinned global memories cut") {
		t.Errorf("the global cut (26 pinned, 20 shown) is not reported under Global. Got:\n%s", got)
	}
}

func TestTheGlobalSectionOfTheResourceCountsWhatItDoesNotShow(t *testing.T) {
	st, db := newPinStore(t)
	seedGlobalPins(t, db, 40)
	got, err := ProjectResourceAt(context.Background(), st, "vproj", pinCtxNow)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if !strings.Contains(got, "20 pinned global memories cut") {
		t.Errorf("the resource does not report its global cut. Got:\n%s", got)
	}
}

func TestTheGlobalProjectResourceReportsItsPinnedCut(t *testing.T) {
	st, db := newPinStore(t)
	seedGlobalPins(t, db, 25)
	got, err := ProjectResourceAt(context.Background(), st, "_global", pinCtxNow)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	if !strings.Contains(got, "5 pinned global memories cut") {
		t.Errorf("the _global project resource drops the cut. Got:\n%s", got)
	}
}

func TestASupersededPinnedRowIsMarkedAndRanksAfterItsReplacement(t *testing.T) {
	st, db := newPinStore(t)
	seedPinContext(t, db, 5,
		pinCtxRow{id: "old-pin01", project: "vproj", content: "the old pinned rule", importance: 0.1, pinned: true},
		pinCtxRow{id: "new-row01", project: "vproj", content: "the replacement rule", importance: 0.05})
	if err := st.CreateLink(context.Background(), "new-row01", "old-pin01", "supersedes", 1, "manual"); err != nil {
		t.Fatalf("link: %v", err)
	}
	tool, err := ProjectContextAt(context.Background(), st, "vproj", 20, pinCtxNow)
	if err != nil {
		t.Fatalf("tool: %v", err)
	}
	res, err := ProjectResourceAt(context.Background(), st, "vproj", pinCtxNow)
	if err != nil {
		t.Fatalf("resource: %v", err)
	}
	for name, got := range map[string]string{"tool": tool, "resource": res} {
		newAt, oldAt := strings.Index(got, "the replacement rule"), strings.Index(got, "the old pinned rule")
		if newAt < 0 || oldAt < 0 || oldAt < newAt {
			t.Errorf("%s: want both rows with the replacement first. Got:\n%s", name, got)
		}
		if !strings.Contains(got, "superseded_by=`new-row01`") {
			t.Errorf("%s: the superseded pin is not marked. Got:\n%s", name, got)
		}
	}
}
