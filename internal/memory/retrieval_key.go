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
	queryKeyErr    error
)

// resetQueryKeyCache clears the cache so a test can resolve the key against a
// different directory. Production never calls it.
func resetQueryKeyCache() {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	queryKeyLoaded, queryKeyBytes_, queryKeyErr = false, nil, nil
}

// queryKey returns the per-install key, loading it or creating it on first use.
//
// Creation is O_EXCL so two processes racing to make the file cannot end up with
// two keys and a store whose records are split across them: the loser reads the
// winner's file and uses that.
func queryKey() ([]byte, error) {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	if queryKeyLoaded {
		return queryKeyBytes_, queryKeyErr
	}
	queryKeyBytes_, queryKeyErr = loadOrCreateQueryKey()
	queryKeyLoaded = true
	return queryKeyBytes_, queryKeyErr
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
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			// Another process made it between our read and our write; theirs is
			// the key, and using a different one would split the store's records.
			raw, rErr := os.ReadFile(path)
			if rErr != nil {
				return nil, fmt.Errorf("read the retrieval key another process created at %s: %w", path, rErr)
			}
			other, decErr := hex.DecodeString(string(raw))
			if decErr != nil || len(other) < queryKeyBytes {
				return nil, fmt.Errorf("the retrieval key another process created at %s is unreadable", path)
			}
			return other, nil
		}
		return nil, fmt.Errorf("create the retrieval key at %s: %w", path, err)
	}
	// Write via the handle so the 0600 above is the mode on disk, then close
	// explicitly: a key left open is a key another process can still be writing.
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
