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

// countingConnector wraps the sqlite driver and tallies the statements that
// match a substring, so a test can assert how many times a query shape ran
// without touching the store's own code. Counting the driver rather than a
// seam in SearchFTS/SearchVector keeps the assertion on real SQL: the concern
// is how much work happens while the single pooled connection is reserved.
type countingConnector struct {
	inner   driver.Connector
	mu      sync.Mutex
	queries []string
}

func (c *countingConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &countingConn{Conn: conn, owner: c}
	if preparer, ok := conn.(driver.ConnPrepareContext); ok {
		wrapped.ConnPrepareContext = preparer
	}
	// Promoted so the read-only transaction explain opens still works: without
	// ConnBeginTx, database/sql falls back to Begin and the driver reports that
	// it cannot do read-only transactions.
	if beginner, ok := conn.(driver.ConnBeginTx); ok {
		wrapped.ConnBeginTx = beginner
	}
	if execer, ok := conn.(driver.ExecerContext); ok {
		wrapped.ExecerContext = execer
	}
	if queryer, ok := conn.(driver.QueryerContext); ok {
		wrapped.QueryerContext = queryer
	}
	return wrapped, nil
}

func (c *countingConnector) Driver() driver.Driver { return c.inner.Driver() }

func (c *countingConnector) record(query string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = append(c.queries, query)
}

// countMatching reports how many recorded statements contain substr.
func (c *countingConnector) countMatching(substr string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, q := range c.queries {
		if strings.Contains(q, substr) {
			n++
		}
	}
	return n
}

func (c *countingConnector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.queries = nil
}

// countingConn embeds the driver interfaces the sqlite conn implements, so the
// optional context-aware ones are promoted and intercepted. Embedding the
// interfaces (not the concrete conn) is what lets QueryContext/ExecContext be
// wrapped while Prepare, Begin and Close pass through untouched.
type countingConn struct {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	owner *countingConnector
}

func (c *countingConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.owner.record(query)
	return c.QueryerContext.QueryContext(ctx, query, args)
}

func (c *countingConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.owner.record(query)
	return c.ExecerContext.ExecContext(ctx, query, args)
}

var (
	_ driver.Conn               = (*countingConn)(nil)
	_ driver.QueryerContext     = (*countingConn)(nil)
	_ driver.ExecerContext      = (*countingConn)(nil)
	_ driver.ConnPrepareContext = (*countingConn)(nil)
)

// TestExplainSearchScopedRunsEachLegOnce pins the cost of the explain snapshot.
//
// OpenDB caps the pool at one connection, so every statement issued between
// BeginTx and Rollback is time a concurrent save, touch or background write
// cannot have the connection at all. Explain used to re-run both retrieval legs
// inside that transaction — a second FTS scan and a second full embedding scan —
// purely to rebuild rows the search it had just run had already fetched. This
// asserts each leg shape runs exactly once per explain, so the regression cannot
// come back unnoticed.
func TestExplainSearchScopedRunsEachLegOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "explain-leg-count.sqlite")

	// Open through OpenDB first so the schema and migrations exist, then reopen
	// on the counting connector: migrations run once, and the counted handle is
	// the one the store under test uses.
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
	counter := &countingConnector{inner: inner}
	db := sql.OpenDB(counter)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureProject(ctx, testProject, "/tmp/explain-legs", testProject); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	dev := makeExplainScopedMemory(t, s, "development database configuration", "development")
	prod := makeExplainScopedMemory(t, s, "production database configuration", "production")
	if err := s.StoreEmbedding(ctx, dev, []float32{0.1, 0.99}, "test"); err != nil {
		t.Fatalf("StoreEmbedding(dev): %v", err)
	}
	if err := s.StoreEmbedding(ctx, prod, []float32{1, 0}, "test"); err != nil {
		t.Fatalf("StoreEmbedding(prod): %v", err)
	}

	// Count only the explain call itself.
	counter.reset()
	if _, err := s.ExplainSearchScoped(ctx, testProject, "database configuration", []float32{1, 0}, 5, map[string]string{"environment": "production"}); err != nil {
		t.Fatalf("ExplainSearchScoped: %v", err)
	}

	// The FTS leg is the only statement selecting from memories_fts; the vector
	// leg is the only one scanning memory_embeddings for similarity.
	const ftsLeg = "memories_fts MATCH"
	const vectorLeg = "FROM memory_embeddings e"
	if got := counter.countMatching(ftsLeg); got != 1 {
		t.Errorf("FTS leg ran %d times inside the explain snapshot, want 1: the diagnostics must reuse "+
			"the rows the search already fetched, because each one is time a concurrent writer cannot "+
			"have the store's single connection", got)
	}
	if got := counter.countMatching(vectorLeg); got != 1 {
		t.Errorf("vector leg ran %d times inside the explain snapshot, want 1, for the same reason", got)
	}
}
