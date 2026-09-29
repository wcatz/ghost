package memory

// #646: the per-install key behind the retrieval record's query digest, and the
// properties that make it a privacy control rather than a hash.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// useQueryKeyDir points the key at a temp tree and clears the per-process cache,
// because the key is resolved once and a test cannot otherwise see the second
// resolution it is asking about.
//
// It takes the ghost DATA directory (what fakeDataDir returns) and points
// XDG_DATA_HOME at its parent, because config.DataDirPath joins "ghost" onto
// XDG_DATA_HOME itself. Both are cleared on cleanup: the env var, or one test
// would point the next one's key at the wrong tree.
func useQueryKeyDir(t *testing.T, dataDir string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", filepath.Dir(dataDir))
	resetQueryKeyCache()
	t.Cleanup(resetQueryKeyCache)
}

// TestTheAssemblerCannotCreateAKeyOnTheMachineItRunsOn: the property that made
// the key a SEAM method rather than a package call, pinned from this side.
//
// A per-install key is resolved through the filesystem, so any code that reaches
// for it can write to the machine it happens to be running on. That is exactly
// what happened: the assembler resolved the key itself, and running this
// repository's own assemble suite CREATED A KEY IN THE DEVELOPER'S REAL DATA
// DIRECTORY — silently, and then reused the one a previous run had left, so the
// suite's behaviour depended on the state of the machine.
//
// So the key is reachable only through a Store's DigestQuery method, and this
// asserts the reachability rather than the hygiene: the digest exists, the sink
// can produce it, and nothing in the assembling path needs a filesystem. The
// hygiene half is the mcpserver TestMain that sandboxes the XDG roots, which is
// what a run of that suite depends on.
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
	// The key is the STORE's, so the store's own reader is the only thing that has
	// to agree with it about what a digest looks like.
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

// TestQueryDigestIsKeyedNotAPlainHashOfTheQuery: the reason the digest is an
// HMAC, asserted against the thing it replaces.
//
// A bare sha256 of a query is a fingerprint, not a privacy property. The question
// is short ("who owns the k8s cluster?"), the store's own memories are a
// ready-made dictionary of the things a user would ask, and hashing stops
// neither — a party who can guess the question can CONFIRM it, which is all a
// fingerprint has to do. A key is what makes a guess insufficient, so the
// assertion is the direct one: the digest must not be the unsalted hash.
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

// TestQueryDigestGroupsWithinAStoreAndNotAcross: what the audit can and cannot
// group, which is the one thing the key is allowed to change.
//
// Grouping by "the same question was asked" is the column's whole purpose, and
// an HMAC keeps it inside one store: the same query under the same key is the
// same digest. Across two keys it is not, and that is the deliberate cost — a
// `ghost backup` restored on another machine re-keys every row it carries, so the
// restored records do not group with the original's, and nothing groups them by
// content either. That is the point of the key rather than a defect in it.
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

// TestQueryDigestForAnEmptyQueryIsEmpty: a call with no question carries no
// fingerprint, and a session-start injection is exactly that.
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

// TestTheQueryKeyLivesOutsideTheDatabaseAndIsNotInABackup: where the key is, and
// the reason it is not in the database file.
//
// `ghost backup` is a VACUUM INTO of ghost.db, so anything inside ghost.db is in
// every backup the user keeps — and a backup is the single most ordinary thing to
// hand a colleague. A key stored in the database would ship with the very records
// it protects. So the key is a sibling FILE in the data directory, and this
// asserts both halves: it exists there with no group or other access, and its
// bytes are absent from the database file itself.
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
	// The one POSIX-only claim in this file, and skipped rather than costing the
	// whole file a //go:build !windows tag: Windows has no group or other bits, so
	// the mode there says nothing about the key's protection. Everything else here
	// — the key's location, its atomic publish, its refusal on a corrupt file — is
	// platform-independent and worth running there.
	if runtime.GOOS != "windows" {
		info, err := os.Stat(keyPath)
		if err != nil {
			t.Fatalf("stat the key: %v", err)
		}
		if perm := info.Mode().Perm(); perm&0o077 != 0 {
			t.Errorf("the key is mode %v, want no group or other access", perm)
		}
	}

	// A real store beside it, and the key must not be inside.
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
	// Fold the WAL back, so the bytes we read are the whole database.
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

// TestQueryDigestSaysSoWhenTheKeyCannotBeRead: the failure path, and it refuses
// to record a fingerprint rather than falling back to an unsalted one.
//
// A key that cannot be created — a read-only data directory, a permissions
// problem — must NOT degrade to a bare sha256, because that is exactly the
// fingerprint the key exists to remove, and it would degrade silently. The digest
// comes back EMPTY with the reason; the record still carries its verdicts, and
// the caller logs that the grouping is unavailable.
func TestQueryDigestSaysSoWhenTheKeyCannotBeRead(t *testing.T) {
	// A regular FILE where the data directory's parent should be, so creating the
	// key's directory fails with ENOTDIR. A read-only directory would have been the
	// obvious fixture and it is the wrong one: the suite can run as root, where a
	// mode of 0500 stops nothing, and a test that passes because the OS ignored it
	// is worse than no test.
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

// TestAKeyFailureIsNotRememberedForTheLifeOfTheProcess: a transient failure must
// not become a permanent one.
//
// The key is cached once per process, which is what keeps a file read off the
// search path. Caching the FAILURE as well as the success is the trap: a store
// that loses the startup race, or starts a moment before the key's directory
// exists, would record an empty query_hash on every call for the rest of its life
// — and report the FIRST failure's reason for all of them, which is a diagnostic
// that points at the wrong cause.
//
// So: fail, repair the environment, call again, and the call must succeed. The
// 0-byte key file is the standing example — it is exactly what a lost race or a
// crash mid-write leaves behind, and it is the one state that used to be both
// reachable and permanent.
func TestAKeyFailureIsNotRememberedForTheLifeOfTheProcess(t *testing.T) {
	dataDir := fakeDataDir(t)
	useQueryKeyDir(t, dataDir)
	keyPath := filepath.Join(dataDir, queryKeyFileName)

	// A key file that exists and holds nothing: the state a lost race leaves.
	if err := os.WriteFile(keyPath, nil, 0o600); err != nil {
		t.Fatalf("seed an empty key file: %v", err)
	}
	if _, err := QueryDigest("who owns the k8s cluster"); err == nil {
		t.Fatal("QueryDigest accepted a 0-byte key file")
	}

	// Repair it, the way an operator would, and the NEXT call must succeed.
	good := strings.Repeat("ab", queryKeyBytes)
	if err := os.WriteFile(keyPath, []byte(good), 0o600); err != nil {
		t.Fatalf("repair the key file: %v", err)
	}
	digest, err := QueryDigest("who owns the k8s cluster")
	if err != nil {
		t.Fatalf("QueryDigest after repair: %v — the first failure was cached for the process", err)
	}
	want := digestWith(mustDecodeHex(t, good), "who owns the k8s cluster")
	if digest != want {
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

// TestWarmingTheQueryKeyTakesTheFilesystemOffTheSearchPath: the placement fix, and
// it is asserted by removing the filesystem rather than by timing anything.
//
// Resolving the key costs a data-directory resolution, a read, and on a first
// install a mkdir and a create. That work happened inside every search, on the
// first call of every process — unbounded filesystem I/O between the answer being
// computed and Run returning, which is precisely the wait the record write's own
// 250ms budget was added to prevent, and it landed on the first search a user
// ever ran. WarmQueryKey moves it to startup.
//
// The test deletes the key file AND its directory after warming, so a later digest
// can only come from memory: a warm key that still reads the file fails here.
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

	// Remove the key entirely. A warmed key must not need it again.
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
