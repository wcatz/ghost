package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func useQueryKeyDir(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Dir(dataDir))
	resetQueryKeyCache()
	t.Cleanup(resetQueryKeyCache)
}

func TestTheAssemblerCannotCreateAKeyOnTheMachineItRunsOn(t *testing.T) {
	dir := fakeDataDir(t)
	useQueryKeyDir(t, dir)

	s := testStore(t)
	digest, err := s.DigestQuery("who owns the k8s cluster")
	if err != nil {
		t.Fatalf("(*Store).DigestQuery: %v", err)
	}
	if len(digest) != 64 {
		t.Fatalf("the store's digest is %d characters, want 64: %q", len(digest), digest)
	}
	if err := s.RecordRetrieval(context.Background(), RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest,
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: "MEM-1", Kept: true}},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	recs, err := s.RetrievalRecords(context.Background(), 1)
	if err != nil {
		t.Fatalf("RetrievalRecords: %v", err)
	}
	if len(recs) != 1 || recs[0].QueryHash != digest {
		t.Errorf("the stored digest round-tripped as %+v, want %q", recs, digest)
	}
}

func TestQueryDigestIsKeyedNotAPlainHashOfTheQuery(t *testing.T) {
	useQueryKeyDir(t, fakeDataDir(t))

	const q = "who owns the k8s cluster"
	got, err := QueryDigest(q)
	if err != nil {
		t.Fatalf("QueryDigest: %v", err)
	}
	bare := sha256.Sum256([]byte(q))
	if got == hex.EncodeToString(bare[:]) {
		t.Error("QueryDigest returned the unsalted sha256 of the query — a guessable question is then " +
			"confirmable by anyone holding the store")
	}
	if len(got) != 64 {
		t.Fatalf("digest is %d characters, want a 64-character HMAC: %q", len(got), got)
	}
	if strings.Trim(got, "0123456789abcdef") != "" {
		t.Fatalf("digest %q is not hex — the column's CHECK would refuse it", got)
	}
}

func TestQueryDigestGroupsWithinAStoreAndNotAcross(t *testing.T) {
	useQueryKeyDir(t, fakeDataDir(t))

	const q = "database configuration pooling"
	first, err := QueryDigest(q)
	if err != nil {
		t.Fatalf("QueryDigest: %v", err)
	}
	again, err := QueryDigest(q)
	if err != nil {
		t.Fatalf("QueryDigest (again): %v", err)
	}
	if first != again {
		t.Errorf("the same query digested twice under one key: %q then %q — the audit could not group "+
			"repeat questions", first, again)
	}
	if other := digestWith([]byte("a different per-install key"), q); other == first {
		t.Error("two keys produced the same digest for one query — the key is not being applied")
	}
	other, err := QueryDigest("database configuration retry")
	if err != nil {
		t.Fatalf("QueryDigest (other): %v", err)
	}
	if other == first {
		t.Error("two different questions produced the same digest under one key")
	}
}

func TestQueryDigestForAnEmptyQueryIsEmpty(t *testing.T) {
	useQueryKeyDir(t, fakeDataDir(t))
	got, err := QueryDigest("")
	if err != nil {
		t.Fatalf("QueryDigest(\"\"): %v", err)
	}
	if got != "" {
		t.Errorf("QueryDigest(\"\") = %q, want empty: a call with no query carries no fingerprint", got)
	}
}

func TestTheQueryKeyLivesOutsideTheDatabaseAndIsNotInABackup(t *testing.T) {
	dir := fakeDataDir(t)
	useQueryKeyDir(t, dir)

	if _, err := QueryDigest("anything"); err != nil {
		t.Fatalf("QueryDigest: %v", err)
	}
	keyPath := filepath.Join(dir, queryKeyFileName)
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the key: %v", err)
	}
	if len(raw) < 32 {
		t.Errorf("the key file holds %d bytes, want at least 32", len(raw))
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat the key: %v", err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("the key is mode %v, want no group or other access", perm)
		}
	}

	dbPath := filepath.Join(dir, "ghost.db")
	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	defer db.Close() //nolint:errcheck
	s := NewStore(db, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := s.EnsureProject(context.Background(), testProject, "/tmp/test", "test"); err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}
	digest, err := QueryDigest("who owns the k8s cluster")
	if err != nil {
		t.Fatalf("QueryDigest: %v", err)
	}
	if err := s.RecordRetrieval(context.Background(), RetrievalRecord{
		ProjectID: testProject, Source: "search", QueryHash: digest,
		Outcome: "answerable", Reason: "floor_met",
		Verdicts: []RowVerdict{{ID: "MEM-1", Kept: true}},
	}); err != nil {
		t.Fatalf("RecordRetrieval: %v", err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	dbBytes, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read the database: %v", err)
	}
	if bytes.Contains(dbBytes, raw) {
		t.Error("the query key is inside the database file, so every `ghost backup` carries it — the key " +
			"must be a sibling file, not a row and not a pragma")
	}
}

func TestQueryDigestSaysSoWhenTheKeyCannotBeRead(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatalf("seed the blocking file: %v", err)
	}
	t.Setenv("XDG_DATA_HOME", blocker)
	resetQueryKeyCache()
	t.Cleanup(resetQueryKeyCache)

	got, err := QueryDigest("who owns the k8s cluster")
	if err == nil {
		t.Fatalf("QueryDigest succeeded with no writable key directory, digest %q", got)
	}
	if got != "" {
		t.Errorf("the failed digest is %q, want empty: a row must never carry an unsalted hash", got)
	}
}

func TestAFailedKeyIsRetriedOnABackoffRatherThanEverySearch(t *testing.T) {
	dataDir := fakeDataDir(t)
	useQueryKeyDir(t, dataDir)
	keyPath := filepath.Join(dataDir, queryKeyFileName)

	if err := os.WriteFile(keyPath, []byte("not hex at all"), 0o600); err != nil {
		t.Fatalf("seed a corrupt key: %v", err)
	}
	store := testStore(t)
	_, first := store.DigestQuery("who owns the k8s cluster")
	if first == nil {
		t.Fatal("QueryDigest accepted a key file that is not hex")
	}
	_, second := store.DigestQuery("who owns the k8s cluster")
	if second == nil {
		t.Fatal("QueryDigest succeeded on the second call inside the backoff")
	}
	if second.Error() != first.Error() {
		t.Errorf("the cached failure changed between calls:\n first:  %v\n second: %v", first, second)
	}
	if queryKeyRetryDelay <= 0 || queryKeyRetryDelay > 5*time.Minute {
		t.Errorf("the retry delay is %v, want a positive value short enough that a recovered key is "+
			"picked up while a server runs", queryKeyRetryDelay)
	}

	good := strings.Repeat("ab", queryKeyBytes)
	if err := os.WriteFile(keyPath, []byte(good), 0o600); err != nil {
		t.Fatalf("repair the key: %v", err)
	}
	if _, err := store.DigestQuery("who owns the k8s cluster"); err == nil {
		t.Error("a call inside the backoff picked up a repaired key — the backoff is not being honoured")
	}
	if err := store.WarmQueryKey(); err != nil {
		t.Fatalf("WarmQueryKey after a repair: %v — startup must not inherit a cached failure", err)
	}
	digest, err := store.DigestQuery("who owns the k8s cluster")
	if err != nil {
		t.Fatalf("DigestQuery after warming: %v", err)
	}
	if want := digestWith(mustDecodeHex(t, good), "who owns the k8s cluster"); digest != want {
		t.Errorf("digest %q does not match the repaired key's HMAC %q", digest, want)
	}
}

func mustDecodeHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode %q: %v", s, err)
	}
	return b
}

func TestWarmingTheQueryKeyTakesTheFilesystemOffTheSearchPath(t *testing.T) {
	dataDir := fakeDataDir(t)
	useQueryKeyDir(t, dataDir)

	s := testStore(t)
	if err := s.WarmQueryKey(); err != nil {
		t.Fatalf("WarmQueryKey: %v", err)
	}
	warmed, err := s.DigestQuery("who owns the k8s cluster")
	if err != nil {
		t.Fatalf("DigestQuery: %v", err)
	}

	if err := os.RemoveAll(filepath.Join(dataDir, queryKeyFileName)); err != nil {
		t.Fatalf("remove the key file: %v", err)
	}
	after, err := s.DigestQuery("who owns the k8s cluster")
	if err != nil {
		t.Fatalf("DigestQuery after the key file was removed: %v — the key is being re-read on the "+
			"search path, so every first search pays for a filesystem round trip", err)
	}
	if after != warmed {
		t.Errorf("the warmed digest changed after the key file was removed: %q then %q", warmed, after)
	}
}

func TestAShortKeyFileIsTreatedAsStillBeingWrittenRatherThanCorrupt(t *testing.T) {
	dataDir := fakeDataDir(t)
	useQueryKeyDir(t, dataDir)
	keyPath := filepath.Join(dataDir, queryKeyFileName)

	store := testStore(t)
	corrupt := strings.Repeat("z", queryKeyBytes*2)
	if err := os.WriteFile(keyPath, []byte(corrupt), 0o600); err != nil {
		t.Fatalf("seed a corrupt key: %v", err)
	}
	_, err := store.DigestQuery("who owns the k8s cluster")
	if err == nil {
		t.Fatal("QueryDigest accepted a full-length non-hex key file")
	}
	if !strings.Contains(err.Error(), "not hex") {
		t.Errorf("a full-length non-hex file is reported as %q, want the corruption verdict", err)
	}
	if errors.Is(err, errQueryKeyShort) {
		t.Error("a full-length non-hex file was reported as short — the retry loop would spin on it")
	}

	if err := os.WriteFile(keyPath, []byte("zz"), 0o600); err != nil {
		t.Fatalf("seed a short key: %v", err)
	}
	if wErr := store.WarmQueryKey(); wErr == nil {
		t.Fatal("WarmQueryKey accepted a 2-byte key file")
	}
	_, err = store.DigestQuery("who owns the k8s cluster")
	if err == nil {
		t.Fatal("QueryDigest accepted a 2-byte key file")
	}
	if !errors.Is(err, errQueryKeyShort) {
		t.Errorf("a 2-byte file is reported as %v, want the errQueryKeyShort verdict so the loop retries it", err)
	}
	if !strings.Contains(err.Error(), "too short") {
		t.Errorf("the short-file message does not say what is wrong: %v", err)
	}
}

type scriptedKeyReader struct {
	outcomes []keyOutcome
	calls    int
}

type keyOutcome struct {
	key []byte
	err error
}

func (s *scriptedKeyReader) read() ([]byte, error) {
	i := s.calls
	s.calls++
	if i >= len(s.outcomes) {
		i = len(s.outcomes) - 1
	}
	return s.outcomes[i].key, s.outcomes[i].err
}

func shortKeyError() error {
	return fmt.Errorf("%w: 2 bytes at /somewhere, %d needed", errQueryKeyShort, queryKeyBytes*2)
}

func corruptKeyError() error {
	return errors.New("the retrieval key at /somewhere is not hex (64 bytes) — it is corrupt or has been " +
		"edited; delete it to start a new key")
}

func TestTheLosersRetryKeepsLookingWhileAKeyIsStillBeingWritten(t *testing.T) {
	want := []byte("a finished key")
	reader := &scriptedKeyReader{outcomes: []keyOutcome{
		{err: shortKeyError()},
		{err: shortKeyError()},
		{key: want},
	}}

	key, err := readPublishedKey(reader.read, readPublishedAttempts, 0, "/somewhere")
	if err != nil {
		t.Fatalf("readPublishedKey gave up on a key that was still being written: %v — the loop "+
			"refused the short-file verdict instead of waiting for it", err)
	}
	if !bytes.Equal(key, want) {
		t.Errorf("the key read as %q, want the finished one", key)
	}
	if reader.calls != 3 {
		t.Errorf("the read was attempted %d times, want 3 — two shorts and then the key", reader.calls)
	}
}

func TestTheLosersRetryKeepsLookingWhileAKeyIsNotThereYet(t *testing.T) {
	want := []byte("a key that appeared")
	reader := &scriptedKeyReader{outcomes: []keyOutcome{
		{err: os.ErrNotExist},
		{key: want},
	}}

	key, err := readPublishedKey(reader.read, readPublishedAttempts, 0, "/somewhere")
	if err != nil {
		t.Fatalf("readPublishedKey gave up on a key that had not appeared yet: %v", err)
	}
	if !bytes.Equal(key, want) {
		t.Errorf("the key read as %q, want the one that appeared", want)
	}
	if reader.calls != 2 {
		t.Errorf("the read was attempted %d times, want 2", reader.calls)
	}
}

func TestTheLosersRetryRefusesACorruptKeyWithoutLookingAgain(t *testing.T) {
	reader := &scriptedKeyReader{outcomes: []keyOutcome{
		{err: corruptKeyError()},
		{key: []byte("a key that would be wrong to accept")},
	}}

	_, err := readPublishedKey(reader.read, readPublishedAttempts, 0, "/somewhere")
	if err == nil {
		t.Fatal("readPublishedKey accepted a corrupt key")
	}
	if reader.calls != 1 {
		t.Errorf("the read was attempted %d times, want exactly 1 — a key that will not become a key "+
			"by being read again is refused, not retried", reader.calls)
	}
	if errors.Is(err, errQueryKeyShort) {
		t.Error("a corrupt key was reported as too short, so the loop would spin on corruption")
	}
	if !strings.Contains(err.Error(), "not hex") {
		t.Errorf("the refusal is %v, want the corruption verdict — this error is what an operator reads", err)
	}
}

func TestTheLosersRetryTellsItsTwoExhaustedStatesApart(t *testing.T) {
	short := &scriptedKeyReader{outcomes: []keyOutcome{{err: shortKeyError()}}}
	_, shortErr := readPublishedKey(short.read, readPublishedAttempts, 0, "/somewhere")
	if !errors.Is(shortErr, errQueryKeyShort) {
		t.Errorf("a key that stayed short is reported as %v, want the too-short verdict", shortErr)
	}
	if !strings.Contains(shortErr.Error(), "too short") {
		t.Errorf("the too-short message does not say what is wrong: %v", shortErr)
	}
	if strings.Contains(shortErr.Error(), "did not become readable") {
		t.Errorf("a too-short key is reported as merely unreadable: %v \u2014 the two exhausted states "+
			"are then indistinguishable in the message, and the size and remedy are buried", shortErr)
	}
	if short.calls != readPublishedAttempts {
		t.Errorf("a key that stayed short was read %d times, want all %d attempts", short.calls, readPublishedAttempts)
	}

	absent := &scriptedKeyReader{outcomes: []keyOutcome{{err: os.ErrNotExist}}}
	_, absentErr := readPublishedKey(absent.read, readPublishedAttempts, 0, "/somewhere")
	if errors.Is(absentErr, errQueryKeyShort) {
		t.Error("a key that never appeared was reported as too short, which is a different problem")
	}
	if !strings.Contains(absentErr.Error(), "did not become readable") {
		t.Errorf("the never-appeared message is %v, want the readability verdict", absentErr)
	}
}

func TestReadPublishedQueryKeyReachesThePolicyThroughTheFile(t *testing.T) {
	dataDir := fakeDataDir(t)
	useQueryKeyDir(t, dataDir)
	path := filepath.Join(dataDir, queryKeyFileName)

	good := strings.Repeat("ab", queryKeyBytes)
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatalf("seed a good key: %v", err)
	}
	start := time.Now()
	key, err := readPublishedQueryKey(path)
	if err != nil {
		t.Fatalf("readPublishedQueryKey on a good key: %v", err)
	}
	if !bytes.Equal(key, mustDecodeHex(t, good)) {
		t.Error("the good key did not read back as itself")
	}
	if elapsed := time.Since(start); elapsed >= readPublishedRetry {
		t.Errorf("a good key took %v to read, so the loop slept before succeeding", elapsed)
	}

	if err := os.WriteFile(path, []byte(strings.Repeat("z", queryKeyBytes*2)), 0o600); err != nil {
		t.Fatalf("seed a corrupt key: %v", err)
	}
	if _, err := readPublishedQueryKey(path); err == nil || !strings.Contains(err.Error(), "not hex") {
		t.Errorf("a corrupt key through the entry point is %v, want the corruption verdict refused at once", err)
	}
}
