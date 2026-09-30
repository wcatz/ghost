package memory

// #646: the per-install key behind the retrieval record's query digest.
//
// The record stores a digest of the query so the audit can group repeat
// questions. A bare sha256 does not make that private: the question is short, the
// store's own memories are a ready-made dictionary of what a user would ask, and
// a hash confirms a guess as efficiently as it hides the original — which is all a
// fingerprint has to do. So the digest is an HMAC under a key that lives OUTSIDE
// the database, and the two properties that follow from where the key lives are
// the ones worth stating:
//
//   - A `ghost backup` is a VACUUM INTO of ghost.db, so anything inside ghost.db
//     is in every backup the user keeps — and a backup is the most ordinary thing
//     to hand a colleague. A key in the database would ship with the records it
//     protects. The key is a sibling file, so it is not in the backup, not in a
//     portable export, and not in a store another machine receives.
//   - Restoring a backup ONTO ANOTHER MACHINE therefore re-keys every row it
//     carries: the restored store's digests do not group with the original's, and
//     nothing groups them by content either. Grouping works within one install,
//     which is where the report needs it, and a report spanning two machines
//     loses the cross-machine "same question asked" grouping. That is the cost of
//     the key, stated rather than discovered.
//
// A key that cannot be loaded is a REFUSAL, not a fallback: see QueryDigest.

import (
	"time"

	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/wcatz/ghost/internal/config"
)

// queryKeyFileName is the key's file inside the ghost data directory. Named for
// what it is, and a sibling of ghost.db rather than part of it.
const queryKeyFileName = "retrieval-record.key"

// queryKeyBytes is the key length. 32 bytes is the HMAC-SHA256 block size and far
// more than a digest can be brute-forced against; it is not a tunable.
const queryKeyBytes = 32

var (
	// queryKeyMu guards the one-per-process cache. The key is read once and held:
	// it is a file read on a path that runs at every search, and a key that moved
	// between two records would silently break the grouping the column exists for.
	queryKeyMu     sync.Mutex
	queryKeyLoaded bool
	queryKeyBytes_ []byte
	// queryKeyErr is cached WITH A BACKOFF rather than not cached at all. Neither
	// extreme is right: caching it forever makes one unlucky moment permanent (a
	// server that started a moment before its key existed), and never caching it
	// re-runs the whole resolution — data-directory lookup, read, mkdir, rand, a
	// temp file, an fsync and a link — on EVERY search, while holding this mutex,
	// for a store that simply cannot keep a key. The backoff turns that from
	// per-search cost into per-interval cost, and WarmQueryKey bypasses it so
	// startup gets a real attempt rather than an inherited failure.
	queryKeyErr        error
	queryKeyRetryAfter time.Time
)

// queryKeyRetryDelay is how long a FAILED resolution is remembered before it is
// retried. Long enough that a store without a usable key costs one resolution per
// interval instead of one per search, short enough that a key that becomes
// available is picked up while a server is still running.
const queryKeyRetryDelay = 30 * time.Second

// resetQueryKeyCache clears the cache so a test can resolve the key against a
// different directory. Production never calls it.
func resetQueryKeyCache() {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	queryKeyLoaded, queryKeyBytes_ = false, nil
	queryKeyErr, queryKeyRetryAfter = nil, time.Time{}
}

// queryKey returns the per-install key, loading it or creating it on first use.
//
// Two processes racing to create the file cannot end up with two keys and a store
// whose records are split across them: publication is atomic, so the loser sees
// the winner's COMPLETE key and uses that. A failure is not cached -- see below.
func queryKey() ([]byte, error) {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	return queryKeyLocked()
}

// queryKeyLocked is queryKey without the lock, so WarmQueryKey can force a real
// attempt past a cached failure without unlocking in between.
func queryKeyLocked(force ...bool) ([]byte, error) {
	if queryKeyLoaded {
		return queryKeyBytes_, nil
	}
	now := time.Now()
	// A forced attempt SKIPS the cached failure rather than returning it: that is
	// the whole point of forcing, and returning the cached error here would mean
	// startup reports a stale reason for a condition that may already be gone.
	if queryKeyErr != nil && len(force) == 0 && now.Before(queryKeyRetryAfter) {
		return nil, queryKeyErr
	}
	key, err := loadOrCreateQueryKey()
	if err != nil {
		queryKeyErr, queryKeyRetryAfter = err, now.Add(queryKeyRetryDelay)
		return nil, err
	}
	queryKeyBytes_, queryKeyLoaded, queryKeyErr, queryKeyRetryAfter = key, true, nil, time.Time{}
	return queryKeyBytes_, nil
}

// WarmQueryKey resolves the per-install key NOW, so the search path never pays for
// it.
//
// A cold key costs a data-directory resolution, a read, and on a first install a
// mkdir and a create -- unbounded filesystem work on a path that runs inside every
// search, on the first call of every process, which is exactly what the record
// write's own budget exists to prevent. Calling this where the store is built
// moves that cost to startup, where a slow filesystem costs a slow start and
// nothing else.
//
// It returns the error rather than logging it, because the caller knows whether a
// missing key is worth telling the operator about: a server that starts and serves
// is not broken by a store whose records cannot be grouped by question, and the
// per-call path reports the same failure with the same reason anyway.
func (s *Store) WarmQueryKey() error {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	// force: startup is exactly when a cached failure should be retried, because
	// the thing that failed may be true now (the directory exists, the other
	// process has published its key) and this is the one call whose whole purpose
	// is to try.
	_, err := queryKeyLocked(true)
	return err
}

func loadOrCreateQueryKey() ([]byte, error) {
	// DataDirPath, not DataDir: resolving the location must not be what creates
	// the directory, and a refusal here has to name the path rather than a
	// half-made tree.
	dir, err := config.DataDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate the data directory for the retrieval key: %w", err)
	}
	path := filepath.Join(dir, queryKeyFileName)

	raw, err := os.ReadFile(path)
	if err == nil {
		key, decErr := hex.DecodeString(string(raw))
		if decErr == nil && len(key) >= queryKeyBytes {
			return key, nil
		}
		// A key file we cannot read is not silently replaced: overwriting it would
		// re-key every record already written under it, which is a silent loss of
		// the grouping the column exists to provide. A user who deleted the file
		// gets the same state honestly, from a stated error.
		return nil, fmt.Errorf("the retrieval key at %s is unreadable (%d bytes, hex error %v); "+
			"records already written under it can no longer be grouped with new ones. Delete it to start a "+
			"new key, knowing that the old records keep their digests and stop grouping", path, len(raw), decErr)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read the retrieval key at %s: %w", path, err)
	}

	// MkdirAll because a fresh install has the data directory's parent but perhaps
	// not the directory itself, and 0700 because the key is a secret.
	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return nil, fmt.Errorf("create %s for the retrieval key: %w", dir, mkErr)
	}
	key := make([]byte, queryKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate a retrieval key: %w", err)
	}
	return publishQueryKey(dir, path, key)
}

// publishQueryKey creates the key file ATOMICALLY, with its content already in it,
// which rules out the window the obvious version leaves open.
//
// Creating with O_EXCL and writing afterwards puts a 0-byte file at the real path
// between the two calls. A second process in that window reads the empty file, is
// refused, and -- because the old code cached the failure -- records an empty
// query_hash for the rest of its life. A crash in that window leaves the same
// 0-byte file on disk permanently, and every later start refuses the key for the
// same reason.
//
// So the content goes to a private temp file, is flushed, and is then HARD LINKED
// into place: link is atomic and fails with EEXIST if the name is taken, so the
// destination is either absent or complete. There is no state in which the real
// path exists and is unreadable, which is what lets the EEXIST branch below keep
// its promise of using the winner's key rather than a half-written one.
func publishQueryKey(dir, path string, key []byte) ([]byte, error) {
	tmp, err := os.CreateTemp(dir, queryKeyFileName+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("stage the retrieval key in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// The staged copy never outlives this call, on any path out of it.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("tighten the staged retrieval key: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key)); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write the staged retrieval key: %w", err)
	}
	// Flushed before it is published, so a crash cannot leave a named-but-empty
	// file after all.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("flush the staged retrieval key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write the staged retrieval key: %w", err)
	}

	if err := os.Link(tmpName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			// Hard links are unavailable on some filesystems, which is a reason to
			// fall back rather than to refuse a working install.
			return fallbackPublishQueryKey(path, key, err)
		}
		// Another process published first. Theirs is the key: two keys in one store
		// would split its records' grouping, which is worse than losing a race.
		return readPublishedQueryKey(path)
	}
	return key, nil
}

// readPublishedQueryKey is the loser's half of the race: read the winner's key
// rather than publish our own.
//
// It retries BRIEFLY, because the window it is closing is microseconds wide — the
// winner is between publishing the file and closing its descriptor, or a process
// that published between our read and our link has not finished returning yet.
// Waiting a few tens of milliseconds here is what stops a lost race from becoming
// the expensive case: without it this store falls through to the failure path, and
// the failure path now costs every search a cached refusal for the backoff window.
//
// A file that is PRESENT but unreadable is a different thing entirely and is never
// retried: that is corruption or a human's edit, and re-reading it on a loop would
// be work for the same answer.
func readPublishedQueryKey(path string) ([]byte, error) {
	for attempt := range readPublishedAttempts {
		key, err := readPublishedQueryKeyOnce(path)
		if err == nil {
			return key, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err // present but unusable: a refusal, not a race
		}
		if attempt == readPublishedAttempts-1 {
			return nil, fmt.Errorf("the retrieval key another process created at %s did not become readable: %w", path, err)
		}
		time.Sleep(readPublishedRetry)
	}
	return nil, fmt.Errorf("read the retrieval key another process created at %s: no attempt was made", path)
}

const (
	readPublishedAttempts = 5
	readPublishedRetry    = 10 * time.Millisecond
)

func readPublishedQueryKeyOnce(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the retrieval key another process created at %s: %w", path, err)
	}
	other, decErr := hex.DecodeString(string(raw))
	if decErr != nil || len(other) < queryKeyBytes {
		return nil, fmt.Errorf("the retrieval key another process created at %s is unreadable (%d bytes); "+
			"delete it to start a new key", path, len(raw))
	}
	return other, nil
}

// fallbackPublishQueryKey is the O_EXCL create-and-write path, for a filesystem
// with no hard links. It carries the empty-file window this package exists to
// close, so it is used only where the alternative is no key at all.
func fallbackPublishQueryKey(path string, key []byte, linkErr error) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return readPublishedQueryKey(path)
		}
		return nil, fmt.Errorf("create the retrieval key at %s (hard links unavailable: %v): %w", path, linkErr, err)
	}
	if _, err := f.WriteString(hex.EncodeToString(key)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("write the retrieval key to %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write the retrieval key to %s: %w", path, err)
	}
	return key, nil
}

// digestWith is the HMAC itself, over a key the caller supplies so a test can
// compare two keys without touching the filesystem.
func digestWith(key []byte, query string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// QueryDigest is the retrieval record's query_hash: an HMAC-SHA256 of the query
// under the per-install key, as 64 hex characters, or "" for a call that carried
// no query at all.
//
// It never falls back to an unsalted hash. A key that cannot be read returns
// ("", error): the caller logs it, the record is still written with its verdicts
// — which are the part that is not about the question — and the row carries no
// query_hash rather than a fingerprint it cannot protect. An empty hash is
// ambiguous between "no question" and "no key", which is stated in the schema
// comment, because a column that guesses is worse than one that says nothing.
//
// The key is where QueryDigest's comment says it is; see this file's header for
// what that costs a restore on another machine.
func QueryDigest(query string) (string, error) {
	if query == "" {
		return "", nil
	}
	key, err := queryKey()
	if err != nil {
		return "", err
	}
	return digestWith(key, query), nil
}
