package memory

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	sqlite "modernc.org/sqlite"
)

// vetoReadFailer refuses the one query that reads the `contradicts` relation,
// so the passive drop path can be driven through ITS error branch: pairs read
// fine, the veto read then fails, and the rows have to survive with nothing
// removed. Counting the refusals keeps the assertion honest — a test that never
// reached the veto read would otherwise pass for the wrong reason.
//
// It wraps the driver rather than adding a seam to the store, the same way
// hydration_count_test.go counts id-list reads: what is under test is the
// production SQL and the branch it feeds, not a hook bolted beside it.
type vetoReadFailer struct {
	inner driver.Connector

	mu       sync.Mutex
	refusals int
}

const contradictsQueryMark = "relation = 'contradicts'"

func (c *vetoReadFailer) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.inner.Connect(ctx)
	if err != nil {
		return nil, err
	}
	wrapped := &vetoFailConn{Conn: conn, owner: c}
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

func (c *vetoReadFailer) Driver() driver.Driver { return c.inner.Driver() }

func (c *vetoReadFailer) refusalCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.refusals
}

type vetoFailConn struct {
	driver.Conn
	driver.ConnPrepareContext
	driver.ConnBeginTx
	driver.ExecerContext
	driver.QueryerContext
	owner *vetoReadFailer
}

func (c *vetoFailConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if strings.Contains(query, contradictsQueryMark) {
		c.owner.mu.Lock()
		c.owner.refusals++
		c.owner.mu.Unlock()
		return nil, errors.New("contradicting pairs: injected veto read failure")
	}
	return c.QueryerContext.QueryContext(ctx, query, args)
}

var (
	_ driver.Conn               = (*vetoFailConn)(nil)
	_ driver.QueryerContext     = (*vetoFailConn)(nil)
	_ driver.ConnBeginTx        = (*vetoFailConn)(nil)
	_ driver.ConnPrepareContext = (*vetoFailConn)(nil)
)

// vetoFailureFixture is passiveFixture's `_global` shape on a store whose
// `contradicts` reads fail: the same rows, the same created_at clock, so the
// pair's loser is decided by the same ordering the passing tests use.
func vetoFailureFixture(t *testing.T) (*Store, *vetoReadFailer) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "veto-read-failure.sqlite")
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
	failer := &vetoReadFailer{inner: inner}
	db := sql.OpenDB(failer)
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	ctx := context.Background()
	st := NewStore(db, nil)
	if err := st.EnsureProject(ctx, "proj", "proj", "proj"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	if err := st.EnsureProject(ctx, "_global", "_global", "global"); err != nil {
		t.Fatalf("EnsureProject _global: %v", err)
	}
	// The `_global` rows the drop-path test needs, in the pinned/importance/
	// updated order the bucket reads them in: gp_low pinned first, then gp_high
	// above gp_mid, so gp_mid is the pair's loser.
	rows := []struct {
		id      string
		imp     float32
		pinned  bool
		created string
	}{
		{"gp_low", 0.10, true, "2026-01-07 00:00:00"},
		{"gp_mid", 0.50, false, "2026-01-08 00:00:00"},
		{"gp_high", 0.90, false, "2026-01-09 00:00:00"},
		{"gp_res", 0.99, false, "2026-01-10 00:00:00"},
	}
	for _, r := range rows {
		resolved := ""
		if r.id == "gp_res" {
			resolved = "2026-01-11 00:00:00"
		}
		if _, err := db.Exec(
			`INSERT INTO memories (id, project_id, category, content, source, importance, pinned, resolved_at, created_at, updated_at)
			 VALUES (?, '_global', 'preference', ?, 'manual', ?, ?, ?, ?, ?)`,
			r.id, "content of "+r.id, r.imp, r.pinned, nullIfEmpty(resolved), r.created, r.created,
		); err != nil {
			t.Fatalf("insert %s: %v", r.id, err)
		}
	}
	return st, failer
}

// TestPassiveVetoReadFailureKeepsTheRowsAndDropsNothing: the contradicts veto
// is read only on the drop path, and "I could not check for a hidden
// contradiction" is not evidence that there is none — so when that read fails
// the pass must keep every row and remove nothing, exactly as it does when the
// penalty read itself fails. Without it, a failed veto read would fall through
// to the drop with an empty contradicted map and silently delete the loser of a
// pair whose conflict the store could not be asked about.
func TestPassiveVetoReadFailureKeepsTheRowsAndDropsNothing(t *testing.T) {
	st, failer := vetoFailureFixture(t)
	ctx := context.Background()
	if err := st.CreateLink(ctx, "gp_mid", "gp_high", "duplicate", 1, "manual"); err != nil {
		t.Fatalf("link duplicate: %v", err)
	}

	set, err := st.Candidates(ctx, passiveRequest("proj", globalPassivePolicy()))
	if err != nil {
		t.Fatalf("global passive: %v", err)
	}

	// The veto read was actually reached and refused: without this, an empty
	// result would let the test pass while exercising no failure at all.
	if n := failer.refusalCount(); n != 1 {
		t.Fatalf("the contradicts read was refused %d time(s), want exactly 1 — the veto read either never ran or ran twice", n)
	}
	// The rows the read covered are all still there, and nothing was filed as a
	// dropped loser: an unreadable veto must not turn into a drop.
	ids := passiveIDs(set)
	for _, id := range []string{"gp_low", "gp_mid", "gp_high"} {
		if !containsStr(ids, id) {
			t.Errorf("with the veto read failing, %s must be kept; items = %v", id, ids)
		}
	}
	if len(set.DroppedLosers) != 0 {
		t.Errorf("DroppedLosers = %+v, want none: a veto read that failed decided nothing", set.DroppedLosers)
	}
	// The counterpart is TestCandidatesPassiveGlobalDropsTheNearDuplicateLoser:
	// the same four `_global` rows, the same duplicate edge and the same
	// ordering, on a store whose veto read works — where gp_mid IS removed. The
	// two together pin that this branch differs from the working one by the
	// removal and by nothing else.
}
