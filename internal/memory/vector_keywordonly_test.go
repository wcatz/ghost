package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
)

// readCountingConnector counts SELECT statements so a test can assert how many
// round trips a search path makes. Counting the driver rather than adding a seam
// to the store keeps the assertion on real SQL: the concern is what the store
// asks the database, not which internal helper it asked.
type readCountingConnector struct {
	inner driver.Connector

	mu    sync.Mutex
	reads []string
}

func (c *readCountingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &readCountrConn{Conn: conn, owner: c}
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

func (c *readCountingConnector) Driver() driver.Driver { return c.inner.Driver() }

// mark is the read count before the call under test, so only that call's
// statements are inspected.
func (c *readCountingConnector) mark() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.reads)
}

// byIDRehydrations counts the "WHERE id IN (...)" lookups GetByIDs issues. It is
// the statement this test is about, so it is matched by shape rather than by
// counting every SELECT: demotion legitimately reads links on this path.
func (c *readCountingConnector) byIDRehydrations(from int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, q := range c.reads[from:] {
		if strings.Contains(q, "WHERE id IN") {
			n++
		}
	}
	return n
}

type readCountrConn struct {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	owner *readCountingConnector
}

func (c *readCountrConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(query)), "SELECT") {
		c.owner.mu.Lock()
		c.owner.reads = append(c.owner.reads, query)
		c.owner.mu.Unlock()
	}
	return c.QueryerContext.QueryContext(ctx, query, args)
}

var (
	_ driver.Conn               = (*readCountrConn)(nil)
	_ driver.QueryerContext     = (*readCountrConn)(nil)
	_ driver.ConnBeginTx        = (*readCountrConn)(nil)
	_ driver.ConnPrepareContext = (*readCountrConn)(nil)
)

// TestSearchHybridKeywordOnlySkipsRehydration is the cost of putting the
// FTS-only fallback on the shared selection seam. SearchFTS already selects
// every column, so routing the fallback through fuseAndRank must not spend a
// second round trip re-reading rows the leg already returned — and the fallback
// is the degraded path, taken whenever there is no embedder or the vector floor
// empties the leg, so it is the one that can least afford it.
func TestSearchHybridKeywordOnlySkipsRehydration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyword-only-reads.sqlite")

	// Open once for schema and migrations, then reopen on the counting
	// connector so the store under test owns the counted handle.
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
	counter := &readCountingConnector{inner: inner}
	db := sql.OpenDB(counter)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureProject(ctx, "test-proj", "/tmp/keyword-only", "test-proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	for i := 0; i < 4; i++ {
		createTestMemory(t, s, ctx, "database configuration pooling timeout retry")
	}

	before := counter.mark()
	if _, err := s.SearchHybrid(ctx, "test-proj", "database configuration", nil, 5); err != nil {
		t.Fatalf("SearchHybrid: %v", err)
	}

	// The FTS rows are already hydrated, so the window must be materialised from
	// the slice in hand. A "WHERE id IN (...)" here means fuseAndRank re-read
	// them, which is the round trip this path must not pay.
	if n := counter.byIDRehydrations(before); n != 0 {
		t.Errorf("keyword-only search re-hydrated by id %d time(s), want 0: SearchFTS already "+
			"selects every column, and this is the degraded path taken whenever there is no "+
			"embedder or the vector floor empties the leg", n)
	}
}
