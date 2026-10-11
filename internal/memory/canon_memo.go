package memory

import (
	"sync"
	"time"
)

// canonMemoTTL bounds how long a stored path's resolved form is trusted, and
// canonMemoRetry how long an UNRESOLVABLE one is.
//
// The memo is keyed by the stored path STRING, so a database write can never
// make an entry name the wrong project: the same text resolves to the same
// place until the filesystem changes. What can go stale is therefore the
// filesystem side — a symlink retargeted, a directory removed or created, a
// short name remapped — and a write from another process, neither of which this
// Store sees. Three things bound that. resolveByPath re-resolves every
// SURVIVING path fresh before it routes a caller, so a stale entry can never
// send a caller into a project (a stale refusal is all that is left, and it is
// bounded by the TTL). Project writes that move, bind, merge, import, restore or
// delete a project clear the whole memo (noteWrite). And entries expire.
//
// An unresolvable entry (the recorded directory does not exist, or cannot be
// read) is RETRIED after the short canonMemoRetry rather than remembered: a
// project is routinely recorded before its checkout exists, or on a volume
// that is mounted later, and a long "missing" would hide it.
const (
	canonMemoTTL   = 30 * time.Second
	canonMemoRetry = 2 * time.Second
)

// canonMemoMax caps the entries held. Past it the whole map is dropped rather
// than evicted entry by entry: the working set is the project table, so hitting
// the cap means the table was rewritten under a long-lived process, and a
// refill is one pass.
const canonMemoMax = 4096

type canonEntry struct {
	canon string
	ok    bool
	at    time.Time
}

// canonMemo remembers canonicalPath(stored) for each recorded project path. It
// has its own mutex because ResolveProject holds only the Store's read lock, so
// resolves run concurrently. The zero value is ready to use.
type canonMemo struct {
	mu      sync.Mutex
	entries map[string]canonEntry
	// now is the clock; nil means time.Now. A test sets it to move time.
	now func() time.Time
}

func (m *canonMemo) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// lookup returns the canonical form of stored, resolving it on a miss or an
// expired entry. ok is false when the path cannot be resolved.
func (m *canonMemo) lookup(stored string) (string, bool) {
	now := m.clock()
	m.mu.Lock()
	e, found := m.entries[stored]
	m.mu.Unlock()
	ttl := canonMemoTTL
	if found && !e.ok {
		ttl = canonMemoRetry
	}
	if found && now.Sub(e.at) < ttl {
		return e.canon, e.ok
	}
	canon, err := canonicalPath(stored)
	e = canonEntry{canon: canon, ok: err == nil, at: now}
	m.mu.Lock()
	if m.entries == nil || len(m.entries) >= canonMemoMax {
		m.entries = make(map[string]canonEntry)
	}
	m.entries[stored] = e
	m.mu.Unlock()
	return e.canon, e.ok
}

// invalidate drops every entry. It is called by the write seam for every
// transaction that may add, move, rename, bind, merge, import or delete a
// project, so the next resolve reads the filesystem again.
func (m *canonMemo) invalidate() {
	m.mu.Lock()
	m.entries = nil
	m.mu.Unlock()
}

// opsThatLeaveProjectsAlone names the write-seam operations that cannot leave a
// memo entry wrong: they do not touch a project's path, id or existence, or (the
// last two) only add one. The default is the other way round: an
// operation NOT listed here clears the memo, so a new project writer that is
// added to the seam without thinking about the memo is covered, and a new
// memory-only writer merely costs one refill. Memory saves, search
// bookkeeping and link/task/decision writes are the hot ones, and clearing on
// each of them would defeat the memo for a session that saves as it goes.
var opsThatLeaveProjectsAlone = map[string]bool{
	"create": true, "upsert": true, "update": true, "delete": true, "replace": true,
	"promote": true, "touch": true, "toggle-pin": true, "increment-interaction": true,
	"store-embedding": true, "delete-embedding": true, "flag-memory": true,
	"clear-resolved": true, "set-resolved": true, "mark-resolve-kept": true,
	"mark-supersede-neither": true, "create-link": true, "create-link-autocommit": true,
	"invalidate-link": true, "mark-link-scanned": true, "retire-link-scan": true,
	"create-task": true, "complete-task": true, "update-task": true,
	"decision-record": true, "supersede-decision": true, "record-retrieval-audits": true,
	"update-learned-context": true, "set-reflect-signature": true,
	"history-compact": true, "history-compact-stamp": true, "prune batch": true,
	"purge-history": true, "fold-rows": true,
	// ensure-project and repo-project run on every save, so clearing on them
	// would make nearly every resolve cold. They only ADD a project (or fold a
	// duplicate into one, which deletes a row and leaves an unreferenced key):
	// a new row has a new key and no entry, and an entry is a function of its
	// key, so neither can leave an entry that names the wrong project.
	"ensure-project": true, "repo-project": true,
}

// noteWrite is called by the write seam when a transaction opens.
func (m *canonMemo) noteWrite(op string) {
	if !opsThatLeaveProjectsAlone[op] {
		m.invalidate()
	}
}
