package memory

// #879's review of the reader: two bounded reads, ONE snapshot.
//
// The reader reads the project's negative verdicts and then the content of the
// memories those verdicts name. Read with nothing between them — the shape this
// had before the review — the window is open: a verdict another process DELETED
// between the two reads was still counted for one pass, which reports a figure
// naming a row the table no longer holds. That is the one direction every filter
// in this reader is pointed away from, so the shape is asserted rather than
// described: the transaction is counted as well as the statements, because two
// statements with no BEGIN around them are exactly the shape that window is.
//
// The count is on TWO rather than one because of what one statement would have
// to project to carry both answers: a join puts `memories.content` — the largest
// field in the table — beside every VERDICT row, capped at 50000 audit rows and
// unbounded in size, where the second read here returns one row per memory. So
// this test pins both halves of the trade: two statements and one BEGIN, and
// the second statement's id list is one entry per MEMORY, not one per verdict.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
)

// usefulnessContentIDs matches the second read's id list, so the test can count
// how many memories one pass asked the content read for.
var usefulnessContentIDs = regexp.MustCompile(`WHERE project_id = \? AND id IN \(([^)]*)\)`)

// usefulnessQueryCounter records every statement and every transaction a store
// issues against the database. Counting the driver rather than adding a seam to
// the store keeps the assertion on real SQL — the same choice
// hydration_count_test.go makes for the id-list reads — because what is being
// measured is how many snapshots the reader took, not how many helpers it
// called.
type usefulnessQueryCounter struct {
	inner driver.Connector

	mu      sync.Mutex
	queries []string
	begins  int
}

func (c *usefulnessQueryCounter) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &usefulnessQueryConn{Conn: conn, owner: c}
	if q, ok := conn.(driver.QueryerContext); ok {
		wrapped.QueryerContext = q
	}
	if p, ok := conn.(driver.ConnPrepareContext); ok {
		wrapped.ConnPrepareContext = p
	}
	if b, ok := conn.(driver.ConnBeginTx); ok {
		wrapped.ConnBeginTx = b
	}
	if e, ok := conn.(driver.ExecerContext); ok {
		wrapped.ExecerContext = e
	}
	return wrapped, nil
}

func (c *usefulnessQueryCounter) Driver() driver.Driver { return c.inner.Driver() }

func (c *usefulnessQueryCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = nil
	c.begins = 0
}

// beginCount is the number of transactions begun since the last reset.
func (c *usefulnessQueryCounter) beginCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.begins
}

// recorded returns the statements issued since the last reset, in order.
func (c *usefulnessQueryCounter) recorded() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.queries...)
}

func (c *usefulnessQueryCounter) record(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, query)
}

func (c *usefulnessQueryCounter) recordBegin() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.begins++
}

type usefulnessQueryConn struct {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	owner *usefulnessQueryCounter
}

func (c *usefulnessQueryConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.owner.record(query)
	return c.QueryerContext.QueryContext(ctx, query, args)
}

// BeginTx is recorded because the snapshot IS the transaction: two statements
// with no BEGIN around them are the window this reader exists to close.
func (c *usefulnessQueryConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	if c.ConnBeginTx == nil {
		return nil, fmt.Errorf("usefulness test conn: no transaction support to record")
	}
	c.owner.recordBegin()
	return c.ConnBeginTx.BeginTx(ctx, opts)
}

var (
	_ driver.Conn               = (*usefulnessQueryConn)(nil)
	_ driver.QueryerContext     = (*usefulnessQueryConn)(nil)
	_ driver.ConnBeginTx        = (*usefulnessQueryConn)(nil)
	_ driver.ConnPrepareContext = (*usefulnessQueryConn)(nil)
)

// TestTheUsefulnessReaderIsTwoReadsInOneSnapshot: a pass costs exactly TWO
// statements — the verdicts, then one row per memory for the content — inside
// exactly ONE transaction.
//
// Both halves are asserted because each one alone is the wrong shape:
//
//   - one statement is the joined read this review moved away from: it projects
//     a full copy of memories.content per VERDICT row (retrieval_audit is capped
//     at 50000) to keep one per memory;
//   - two statements with no transaction is the window the review closed: a
//     verdict deleted between them was counted for one pass.
//
// The id list of the second read is checked too, so "one row per memory" is a
// measurement rather than a claim: four verdicts over two memories must ask for
// two ids, not four.
//
// The fixture must actually take, or a zero would agree for the wrong reason:
// the evidence is checked before the counts are.
func TestTheUsefulnessReaderIsTwoReadsInOneSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ghost.db")
	seed, err := OpenDB(path)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed db: %v", err)
	}

	dsn := "file:" + path + "?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)"
	inner, err := sqlite.NewConnector(dsn)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	counter := &usefulnessQueryCounter{inner: inner}
	db := sql.OpenDB(counter)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureProject(ctx, "p1", "/tmp/usefulness-one-read", "p1"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	plant := auditPlant(t, path)
	usefulnessMemory(t, plant, "p1", "M1", "2026-01-01 00:00:00")
	usefulnessMemory(t, plant, "p1", "M2", "2026-01-01 00:00:00")
	// Four verdicts over TWO memories, so an id list keyed on verdicts rather
	// than on memories is a different number than the one asserted below.
	usefulnessVerdict(t, plant, "p1", "M1", VerdictOutcomeContradicted, "ses_1", "2026-09-24 10:00:00")
	usefulnessVerdict(t, plant, "p1", "M1", VerdictOutcomeContradicted, "ses_2", "2026-09-24 10:00:01")
	usefulnessVerdict(t, plant, "p1", "M1", VerdictOutcomeContradicted, "ses_3", "2026-09-24 10:00:02")
	usefulnessVerdict(t, plant, "p1", "M2", VerdictOutcomeContradicted, "ses_4", "2026-09-24 11:00:00")

	counter.reset()
	got, err := s.UsefulnessByMemory(ctx, "p1")
	if err != nil {
		t.Fatalf("UsefulnessByMemory: %v", err)
	}
	if g := got["M1"]; g.Contradicted != 3 {
		t.Fatalf("evidence for M1 = %+v, want three contradictions; the fixture did not take, so the counts "+
			"below would be measuring an empty read", g)
	}
	if g := got["M2"]; g.Contradicted != 1 {
		t.Fatalf("evidence for M2 = %+v, want one contradiction; the fixture did not take, so the counts "+
			"below would be measuring an empty read", g)
	}

	if n := counter.beginCount(); n != 1 {
		t.Errorf("UsefulnessByMemory began %d transactions, want 1 — without a transaction the verdicts and "+
			"the content they are judged against are two reads with a window between them, in which a verdict "+
			"another process deleted is still counted, which is a figure naming a row the table no longer "+
			"holds (#879)", n)
	}
	statements := counter.recorded()
	if len(statements) != 2 {
		t.Errorf("UsefulnessByMemory issued %d statements, want 2 — one for the verdicts and one, one row per "+
			"memory, for the content: a single joined statement copies memories.content once per VERDICT row "+
			"(retrieval_audit is capped at %d) to keep one copy per memory", len(statements), retrievalAuditRowsCap)
	}
	if len(statements) == 2 {
		m := usefulnessContentIDs.FindStringSubmatch(statements[1])
		if m == nil {
			t.Errorf("second statement is not the one-row-per-memory content read:\n%s", statements[1])
		} else if n := strings.Count(m[1], "?"); n != 2 {
			t.Errorf("content read bound %d memory ids, want 2 — four verdicts name two memories, and an id "+
				"list keyed on verdict rows rather than on memories asks for the same text four times", n)
		}
	}
}
