package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"io"
	"log/slog"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
)

// idReadCounter records the width of every "WHERE id IN (?, ?, ...)" lookup
// GetByIDs issues. Counting the driver rather than adding a seam to the store
// keeps the assertion on real SQL: what matters is which rows the search asked
// the database for, not which internal helper it asked.
type idReadCounter struct {
	inner driver.Connector

	mu    sync.Mutex
	sizes []int
}

var inClause = regexp.MustCompile(`WHERE id IN \(([^)]*)\)`)

func (c *idReadCounter) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &idReadConn{Conn: conn, owner: c}
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

func (c *idReadCounter) Driver() driver.Driver { return c.inner.Driver() }

func (c *idReadCounter) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sizes = nil
}

// idListSizes returns the number of ids in each id-list read, in order.
func (c *idReadCounter) idListSizes() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]int(nil), c.sizes...)
}

func (c *idReadCounter) record(query string) {
	if m := inClause.FindStringSubmatch(query); m != nil {
		c.mu.Lock()
		c.sizes = append(c.sizes, len(strings.Split(m[1], ",")))
		c.mu.Unlock()
	}
}

type idReadConn struct {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	owner *idReadCounter
}

func (c *idReadConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.owner.record(query)
	return c.QueryerContext.QueryContext(ctx, query, args)
}

var (
	_ driver.Conn               = (*idReadConn)(nil)
	_ driver.QueryerContext     = (*idReadConn)(nil)
	_ driver.ConnBeginTx        = (*idReadConn)(nil)
	_ driver.ConnPrepareContext = (*idReadConn)(nil)
)

// hydrationCountingStore returns a store whose id-list reads are counted. It
// opens the file once for schema and migrations, then reopens on the counting
// connector so the store under test owns the counted handle.
func hydrationCountingStore(t *testing.T) (*Store, context.Context, *idReadCounter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hydration-reads.sqlite")

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
	counter := &idReadCounter{inner: inner}
	db := sql.OpenDB(counter)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	store := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := store.EnsureProject(ctx, "test-proj", "/tmp/hydration", "test-proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	return store, ctx, counter
}
