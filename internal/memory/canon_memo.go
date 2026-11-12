package memory

import (
	"sync"
	"time"
)

const (
	canonMemoTTL   = 30 * time.Second
	canonMemoRetry = 2 * time.Second
)

const canonMemoMax = 4096

type canonEntry struct {
	canon string
	ok    bool
	at    time.Time
}

type canonMemo struct {
	mu      sync.Mutex
	entries map[string]canonEntry
	now     func() time.Time
}

func (m *canonMemo) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

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

func (m *canonMemo) invalidate() {
	m.mu.Lock()
	m.entries = nil
	m.mu.Unlock()
}

var opsThatLeaveProjectsAlone = map[string]bool{
	"create": true, "upsert": true, "update": true, "delete": true, "replace": true,
	"promote": true, "touch": true, "toggle-pin": true, "increment-interaction": true,
	"store-embedding": true, "delete-embedding": true, "flag-memory": true,
	"clear-resolved": true, "set-resolved": true, "mark-resolve-kept": true,
	"mark-supersede-neither": true, "create-link": true, "create-link-autocommit": true,
	"invalidate-link": true, "mark-link-scanned": true,
	"create-task": true, "complete-task": true, "update-task": true,
	"decision-record": true, "supersede-decision": true,
	"update-learned-context": true, "set-reflect-signature": true,
	"history-compact": true, "history-compact-stamp": true, "prune batch": true,
	"purge-history": true, "fold-rows": true,
	"ensure-project": true, "repo-project": true,
}

func (m *canonMemo) noteWrite(op string) {
	if !opsThatLeaveProjectsAlone[op] {
		m.invalidate()
	}
}
