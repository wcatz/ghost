package memory

import (
	"time"

	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/wcatz/ghost/internal/config"
)

const queryKeyFileName = "retrieval-record.key"
const queryKeyBytes = 32

var (
	queryKeyMu         sync.Mutex
	queryKeyLoaded     bool
	queryKeyBytes_     []byte
	queryKeyErr        error
	queryKeyRetryAfter time.Time
)

const queryKeyRetryDelay = 30 * time.Second

func queryKey() ([]byte, error) {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	return queryKeyLocked()
}

func queryKeyLocked(force ...bool) ([]byte, error) {
	if queryKeyLoaded {
		return queryKeyBytes_, nil
	}
	now := time.Now()
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

func (s *Store) WarmQueryKey() error {
	queryKeyMu.Lock()
	defer queryKeyMu.Unlock()
	_, err := queryKeyLocked(true)
	return err
}

var errKeyNotRegular = errors.New("the retrieval key path is not a regular file")

func readKeyFile(path string) ([]byte, error) {
	f, err := os.OpenFile(path, keyFileOpenFlags, 0) // #nosec G304 -- a file inside the Ghost data directory
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%w (%s)", errKeyNotRegular, info.Mode().Type())
	}
	return io.ReadAll(f)
}

func loadOrCreateQueryKey() ([]byte, error) {
	dir, err := config.DataDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate the data directory for the retrieval key: %w", err)
	}
	path := filepath.Join(dir, queryKeyFileName)

	raw, err := readKeyFile(path)
	if err == nil {
		if key, cErr := classifyQueryKey(raw, path); cErr != nil {
			return nil, cErr
		} else {
			return key, nil
		}
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read the retrieval key at %s: %w", path, err)
	}

	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return nil, fmt.Errorf("create %s for the retrieval key: %w", dir, mkErr)
	}
	key := make([]byte, queryKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate a retrieval key: %w", err)
	}
	return publishQueryKey(dir, path, key)
}

func publishQueryKey(dir, path string, key []byte) ([]byte, error) {
	tmp, err := os.CreateTemp(dir, queryKeyFileName+".*.tmp")
	if err != nil {
		return nil, fmt.Errorf("stage the retrieval key in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("tighten the staged retrieval key: %w", err)
	}
	if _, err := tmp.WriteString(hex.EncodeToString(key)); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("write the staged retrieval key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, fmt.Errorf("flush the staged retrieval key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("write the staged retrieval key: %w", err)
	}

	if err := os.Link(tmpName, path); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fallbackPublishQueryKey(path, key, err)
		}
		return readPublishedQueryKey(path)
	}
	return key, nil
}

func readPublishedQueryKey(path string) ([]byte, error) {
	return readPublishedKey(
		func() ([]byte, error) { return readPublishedQueryKeyOnce(path) },
		readPublishedAttempts, readPublishedRetry, path,
	)
}

func readPublishedKey(read func() ([]byte, error), attempts int, wait time.Duration, path string) ([]byte, error) {
	var last error
	for attempt := range attempts {
		key, err := read()
		if err == nil {
			return key, nil
		}
		last = err
		if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errQueryKeyShort) {
			return nil, err
		}
		if attempt == attempts-1 {
			break
		}
		if wait > 0 {
			time.Sleep(wait)
		}
	}
	if errors.Is(last, errQueryKeyShort) {
		return nil, last
	}
	return nil, fmt.Errorf("the retrieval key another process created at %s did not become readable: %w", path, last)
}

const (
	readPublishedAttempts = 5
	readPublishedRetry    = 10 * time.Millisecond
)

func readPublishedQueryKeyOnce(path string) ([]byte, error) {
	raw, err := readKeyFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		return nil, fmt.Errorf("read the retrieval key at %s: %w", path, err)
	}
	return classifyQueryKey(raw, path)
}

func classifyQueryKey(raw []byte, path string) ([]byte, error) {
	if len(raw) < queryKeyBytes*2 {
		return nil, fmt.Errorf("%w: %d bytes at %s, %d needed", errQueryKeyShort, len(raw), path, queryKeyBytes*2)
	}
	key, decErr := hex.DecodeString(string(raw))
	if decErr != nil {
		return nil, fmt.Errorf("the retrieval key at %s is not hex (%d bytes, %v) — it is corrupt or has "+
			"been edited; delete it to start a new key, knowing that records already written under it stop "+
			"grouping", path, len(raw), decErr)
	}
	if len(key) < queryKeyBytes {
		return nil, fmt.Errorf("%w: %d bytes decoded at %s, %d needed", errQueryKeyShort, len(raw), path, queryKeyBytes)
	}
	return key, nil
}

var errQueryKeyShort = errors.New("the retrieval key file is too short to be a key")

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

func digestWith(key []byte, query string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(query))
	return hex.EncodeToString(mac.Sum(nil))
}

// ReadRetrievalKey returns the per-install retrieval key without creating it.
func ReadRetrievalKey() ([]byte, error) {
	dir, err := config.DataDirPath()
	if err != nil {
		return nil, fmt.Errorf("locate the data directory for the retrieval key: %w", err)
	}
	path := filepath.Join(dir, queryKeyFileName)
	raw, err := readKeyFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("the retrieval key at %s does not exist yet (a store that has "+
				"never recorded a retrieval has no key, and this reader will not create one)", path)
		}
		return nil, fmt.Errorf("read the retrieval key at %s: %w", path, err)
	}
	return classifyQueryKey(raw, path)
}

// QueryDigest returns the keyed digest for query, or "" when query is empty.
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
