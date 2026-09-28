package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The sidecar manifest is what turns "there is a file" into "there is a copy I
// can check". A backup on its own can only be asked whether it opens; a manifest
// beside it carries the three things a restore is actually at risk of — which
// schema wrote it, how many rows it holds, and what it hashes to — so
// `ghost backup verify` can answer before the file is trusted rather than
// after.

// ManifestVersion is the version of the manifest format itself, stamped into
// every manifest this build writes and checked before one is read.
//
// It is not memory.SchemaVersion and it does not move with it: a manifest
// describes a file, and the shape of that description is a wire format of its
// own, exactly as the portable artifact's SchemaVersion is. It increments only
// when a field changes meaning or is removed — an added optional field does not
// take it, so a manifest written by a slightly older Ghost still verifies. A
// manifest stamped with a version this build does not read is refused rather
// than guessed at, because every field in it would be read on the writer's
// assumption rather than the reader's.
const ManifestVersion = 1

// manifestSuffix is what a manifest's name adds to the snapshot's, so the two
// are found as a pair and neither is mistaken for the other. A JSON extension on
// the sidecar is deliberate: the manifest is a document, and opening a backup
// directory in an editor should show the manifests as text rather than offering
// to open them as databases.
const manifestSuffix = ".manifest.json"

// BackupManifest is what one backup wrote beside its snapshot: enough to
// recognise that snapshot later and say what it should contain.
//
// The counts are the same BackupCounts the printed report carries, from the same
// count of the same file, so a manifest and a report can never disagree about
// what a backup holds. The SHA256 is of the file as `ghost backup` wrote it, and
// it is the only part that detects a change the file's own contents would not —
// a byte flipped in a page SQLite never reads is invisible to integrity_check
// and to every row count, and is the difference between a copy that is
// restorable and one that is a plausible-looking file.
type BackupManifest struct {
	ManifestVersion int          `json:"manifest_version"`
	SchemaVersion   int          `json:"schema_version"`
	CreatedAt       string       `json:"created_at"`
	Database        string       `json:"database"`
	Bytes           int64        `json:"bytes"`
	SHA256          string       `json:"sha256"`
	Counts          BackupCounts `json:"counts"`
}

// ManifestPath is where the manifest describing the snapshot at dbPath lives.
// One convention, derived from the snapshot's own name: a backup and its
// manifest are moved together, and `--out` needs no second path to be told
// apart.
func ManifestPath(dbPath string) string { return dbPath + manifestSuffix }

// FileSHA256 is the lowercase hex SHA-256 of a file's bytes, streamed rather
// than read whole: a backup is the whole memory database, and a manifest that
// could not be written because the file did not fit in memory would be a
// manifest nobody gets.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeBackupManifest records what the snapshot at dest holds, and returns the
// path it wrote. Called by Store.Backup rather than by the CLI so that there is
// one place a backup is described, in the same way there is one place a backup
// is taken (vacuumInto).
//
// The manifest is at 0600 before anything is written to it, for the reason the
// snapshot is (see vacuumInto): a full description of the memory database —
// every count, and a hash that says whether the copy beside it is intact — is
// not something to leave at the width a create-then-chmod would pass through.
//
// It is written with O_TRUNC rather than refused-on-exists, which is the
// opposite of the snapshot's own rule and for the opposite reason. The snapshot
// is never replaced because the file there is a copy someone may be relying on;
// a manifest is derived from that snapshot, so one found at this path belongs to
// an earlier backup of a file the user has since deleted, and replacing it is
// the only way it can describe the snapshot that is actually there. Nothing is
// lost either way: the snapshot it described is gone, and it is the snapshot,
// not the manifest, that is a backup.
//
// A manifest that fails to parse is refused by every reader rather than ignored
// (see VerifyBackup), so a write interrupted part-way leaves a file that fails
// loudly instead of one that is silently treated as absent — which would turn a
// damaged sidecar into an unverified backup that still looked fine.
//
// The path is classified with an Lstat before the create, which the snapshot
// beside it also does and for the same reason: this open has no O_EXCL, because
// the manifest is replaced rather than refused, and without O_NOFOLLOW a symlink
// at this path would be written THROUGH. That is not a hypothetical for a --out
// destination: a backup directory is a plausible place to put a link, and the
// target would be truncated, overwritten with a manifest and narrowed to 0600.
func writeBackupManifest(dest string, counts BackupCounts, at time.Time) (string, error) {
	path := ManifestPath(dest)
	sum, err := FileSHA256(dest)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dest)
	if err != nil {
		return "", fmt.Errorf("stat backup: %w", err)
	}
	m := BackupManifest{
		ManifestVersion: ManifestVersion,
		SchemaVersion:   SchemaVersion(),
		CreatedAt:       at.UTC().Format(time.RFC3339),
		Database:        filepath.Base(dest),
		Bytes:           info.Size(),
		SHA256:          sum,
		Counts:          counts,
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode manifest: %w", err)
	}
	// Created, not created-and-truncated. O_TRUNC would be simpler and is
	// wrong: the open mode applies only to a file this call CREATES, so
	// truncating a manifest that is already there leaves it at whatever width
	// something else gave it — and then a full description of the memory
	// database is rewritten at 0644. The mode is asserted with an explicit
	// Chmod before a byte is written, so the window vacuumInto closes for the
	// snapshot is closed here too, on the replacing path as well as the
	// creating one.
	// Classified before the create, for the reason reserveBackupPath classifies
	// the snapshot: this open has no O_EXCL (the manifest is replaced, not
	// refused) and no O_NOFOLLOW, so a symlink already at this path would be
	// followed and its TARGET truncated, rewritten and chmod'ed to 0600 — Ghost
	// clobbering and narrowing a file the user never named, through a --out
	// directory they chose. A dangling link is the worst case of all: a
	// stat-based check reads it as absent.
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("refusing to write the manifest to %s, which is not a regular file", path)
	} else if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("check the manifest path %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", fmt.Errorf("create manifest %s: %w", path, err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("narrow manifest %s: %w", path, err)
	}
	// Truncated through the open handle rather than by O_TRUNC, so the file is
	// at 0600 before its first byte of new content and not merely after.
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write manifest %s: %w", path, err)
	}
	// Close before reporting a failure, so the bytes are either on disk or the
	// error names a file that was never finished.
	if _, err := f.Write(append(body, '\n')); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write manifest %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("write manifest %s: %w", path, err)
	}
	TightenPermissions(path)
	return path, nil
}

// ReadBackupManifest reads a manifest written by writeBackupManifest. A missing
// file is reported as fs.ErrNotExist so a caller can tell "there is no manifest"
// from "the manifest is unreadable" — the first is an expected state a pre-
// migration copy is always in, and the second is damage.
func ReadBackupManifest(path string) (BackupManifest, error) {
	var m BackupManifest
	body, err := os.ReadFile(path)
	if err != nil {
		return BackupManifest{}, err
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return BackupManifest{}, fmt.Errorf("the manifest at %s is not readable as a manifest: %w", path, err)
	}
	if m.ManifestVersion != ManifestVersion {
		return BackupManifest{}, fmt.Errorf("the manifest at %s is format v%d, and this Ghost reads v%d", path, m.ManifestVersion, ManifestVersion)
	}
	return m, nil
}

// VerifyState is one check's verdict.
type VerifyState string

const (
	// VerifyOK: the check ran and the file passed it.
	VerifyOK VerifyState = "ok"
	// VerifyFailed: the check ran and the file did not pass it. The Detail says
	// what it found.
	VerifyFailed VerifyState = "failed"
	// VerifySkipped: the check could not run, and the Detail says why. It is
	// deliberately not a pass: a check that did not run says nothing about the
	// file, and a report that read it as a pass would be the whole failure this
	// command exists to prevent.
	VerifySkipped VerifyState = "skipped"
)

// VerifyCheck is one named check and its verdict. The name is a fixed string
// rather than a field the caller supplies, so a report is comparable between
// runs and a script can key on the names.
type VerifyCheck struct {
	Name   string
	State  VerifyState
	Detail string
}

// VerifyReport is what VerifyBackup found in one file: the checks it ran, in
// the order it ran them, and a flat list of the ones that failed.
//
// Problems is not derived state a caller has to recompute from Checks, because
// "did anything fail" is the question the whole command answers and it is asked
// of the report, not of a scan over it. It holds the same Detail strings, so the
// two cannot disagree.
type VerifyReport struct {
	// Path is the file that was checked.
	Path string
	// Bytes is its size, read from the file. checkHash compares it against the
	// manifest's, so a mismatch is the first thing reported about a copy that
	// was truncated or appended to.
	//
	// Meaningful only when BytesRead is set. A run that failed before the stat —
	// a damaged sidecar, a path that is not there — leaves it false, and a caller
	// that printed the zero value would be reporting a 0-byte file rather than an
	// unmeasured one. The flag is the size's counterpart to CountsRead.
	Bytes int64
	// BytesRead reports whether Bytes was actually read. Not inferable from the
	// value: a genuinely empty file is a 0-byte file, and "this file is empty"
	// must never be printed for a file nobody stat'ed.
	BytesRead bool
	// HasManifest reports whether a manifest was found beside the file AND read.
	// It is false for a pre-migration copy, which the migration path writes
	// without one.
	HasManifest bool
	// ManifestPresent reports whether a sidecar was there at all, read or not.
	// It is separate from HasManifest because the two failures are opposites and
	// a reader must be told which one they have: a missing sidecar is expected
	// and merely limits what was checked, while a sidecar that is present and
	// unreadable is DAMAGE, and calling that "no manifest" would send the reader
	// looking for a file that is sitting right there.
	ManifestPresent bool
	// ManifestErr is why a present sidecar could not be read, or nil. It rides
	// on the report so the message can be specific — an unreadable format
	// version, a truncated JSON document — rather than a bare refusal.
	ManifestErr error
	// ManifestPath is the sidecar that was read, or would have been.
	ManifestPath string
	// SchemaVersion is what the file itself carries, read from its own
	// PRAGMA user_version rather than from the manifest.
	SchemaVersion int
	// Counts is what the file holds, counted from the file. It is reported even
	// when there is no manifest to compare it against, because it is the answer
	// to "what would a restore of this file hold".
	//
	// Meaningful only when CountsRead is set. A run that could not finish
	// counting leaves the zero value here, and a caller that printed it would be
	// reporting an empty file rather than an unmeasured one.
	Counts BackupCounts
	// CountsRead reports whether Counts was actually read. It is a flag rather
	// than a method because the zero value is indistinguishable from a genuinely
	// empty database, and "this file holds nothing" must never be printed for a
	// file nobody counted.
	CountsRead bool
	// Checks is every check run, in order.
	Checks []VerifyCheck
	// Problems is the Detail of each failed check, in the same order. Empty
	// means the file can be trusted.
	Problems []string
}

// Failed is the names of the checks that failed, for a message that has to name
// them. It walks the same Checks that add folded the Details into Problems, so
// the two cannot list different sets.
func (r VerifyReport) Failed() []string {
	var names []string
	for _, c := range r.Checks {
		if c.State == VerifyFailed {
			names = append(names, c.Name)
		}
	}
	return names
}

// VerifyBackup checks a file someone is considering restoring, and reports what
// it found. It reads the file and the manifest beside it and nothing else: it
// does not open the live store, so running it cannot migrate, seed or otherwise
// disturb the database in the data directory — which matters because the moment
// a user wants to check a backup is the moment they are least sure what state
// their own store is in.
//
// The checks, in the order they run:
//
//  1. the size and the SHA-256, against the manifest's. First because it is the
//     only check that needs nothing but the path: a truncated copy, a file
//     appended to, and a file that is not a database at all are all cases where
//     the digest is the most informative thing that can be said, and all three
//     would go unmentioned if the file had to open first.
//  2. the file opens as a database on a read-only handle, and SQLite's own
//     integrity_check passes. This comes before the two checks below because it
//     is the question they depend on: a file that is not a database, or is a
//     damaged one, has no rows to count and no schema to read.
//  3. the schema version the file carries, against the one this build reads and
//     against the one the manifest recorded.
//  4. the row counts, against the manifest's.
//
// Every check that can run does run, and one that cannot reports as skipped
// rather than being dropped. A run that stopped at the first failure would
// answer "this is not a backup" to a file whose only problem is a manifest it
// cannot read — a different claim, and the wrong one to hand someone deciding
// whether to keep looking. A run that could not finish at all (the file would
// not open, or a table was missing) still returns the checks that did run, and
// leaves CountsRead false so nothing prints a row count nobody read.
//
// A missing manifest is not a failure. The copy a user restores from after a bad
// upgrade is written by the migration path, which writes no manifest, so
// refusing one would make the command useless exactly when it is wanted. The
// two manifest-derived checks report as skipped, which says they did not run
// rather than that they passed, and the structural ones still stand. A manifest
// that is present but unreadable IS a failure — that is damage, not an absent
// optional file, and treating it as absent would downgrade a damaged sidecar
// into an unverified backup that still looked fine.
//
// What it reads is the file it was given and the manifest beside it. It does
// NOT open the database in the data directory, and that is the property that
// matters: running this cannot migrate or seed the live store. The narrower
// wording is deliberate — SQLite builds the WAL index of a database that is
// already in WAL mode, so a `-wal` and a `-shm` can appear beside the file
// being checked, and OpenReadDB is the shared read-only constructor whose DSN
// cannot be narrowed to stop that without breaking every other reader.
func VerifyBackup(ctx context.Context, path string) (VerifyReport, error) {
	rep := VerifyReport{Path: path, ManifestPath: ManifestPath(path)}

	// The manifest is read first so that a hard error about it — an unreadable
	// one, or one written by a format this build does not read — is the
	// diagnosis, rather than a check-by-check account of a file nobody can
	// compare anything against.
	// Lstat before the read, so a sidecar that is there and unreadable is a
	// different report from one that is not there. ReadFile cannot tell them
	// apart on its own: both surface as "no manifest" to a caller that only looks
	// at the error.
	if _, err := os.Lstat(rep.ManifestPath); err == nil {
		rep.ManifestPresent = true
	} else if !os.IsNotExist(err) {
		return rep, fmt.Errorf("check the manifest at %s: %w", rep.ManifestPath, err)
	}
	var manifest BackupManifest
	switch m, err := ReadBackupManifest(rep.ManifestPath); {
	case err == nil:
		rep.HasManifest = true
		manifest = m
	case os.IsNotExist(err):
		// Expected for a pre-migration copy. Recorded as the absence it is.
	default:
		rep.ManifestErr = err
		return rep, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return rep, err
	}
	rep.Bytes, rep.BytesRead = info.Size(), true

	// Before the open, deliberately; see the ordering note on VerifyBackup. The size
	// and the digest are what a truncated or appended-to copy is diagnosed by,
	// and neither of them needs the file to be a database.
	rep.add(checkHash(path, manifest, rep.HasManifest, info.Size()))

	db, err := OpenReadDB(path)
	if err != nil {
		return rep, fmt.Errorf("%s cannot be checked as a backup: %w", path, err)
	}
	defer db.Close() //nolint:errcheck

	rep.add(checkIntegrity(ctx, db, path))
	version, err := DBUserVersion(db)
	if err != nil {
		return rep, fmt.Errorf("read the schema version of %s: %w", path, err)
	}
	rep.SchemaVersion = version
	rep.add(checkSchemaVersion(version, manifest, rep.HasManifest))
	counts, err := CountRows(ctx, db)
	if err != nil {
		// CountsRead stays false, and Counts stays at its zero value: nothing was
		// read, and a caller that printed them would be reporting an empty file
		// rather than an unmeasured one.
		return rep, err
	}
	rep.Counts, rep.CountsRead = counts, true
	rep.add(checkCounts(counts, manifest, rep.HasManifest))
	return rep, nil
}

// add appends a check and folds a failure into Problems, so a caller cannot
// build one without the other.
func (r *VerifyReport) add(c VerifyCheck) {
	r.Checks = append(r.Checks, c)
	if c.State == VerifyFailed {
		r.Problems = append(r.Problems, c.Detail)
	}
}

// checkIntegrity runs SQLite's own structural check and carries its answer
// through. When it fails it names the pages and the reasons, which is the most
// specific thing anyone will ever be told about the damage, so all of it is kept
// rather than summarised.
//
// Three things about the pragma's output shape the handling here, each measured
// against it rather than assumed:
//
//   - The unit is a FINDING, not a row. A row can carry several of them
//     newline-separated — "Tree 2 page 8 cell 6: Rowid 52 out of order" and
//     "Tree 2 page 8 cell 0: Offset 43881 out of range 433..4092" arrive
//     together, and a driver may hand over the whole report as one row. Splitting
//     first is what makes a cap on it mean anything, and it is what stops a
//     caller reading one row with a QueryRow from reporting a file with twelve
//     findings as having one.
//   - The findings carry newlines, and the report is a table with one line per
//     check, so they are joined with "; " — the finding is not lost, only
//     reflowed onto the line it is printed on.
//   - On a badly damaged file the query itself can fail partway, with the rows it
//     already produced still delivered. Those findings are reported along with
//     the error: "it got this far and then could not continue" is strictly more
//     use than either half alone, and more than a bare "could not check".
//
// Capped, because a badly damaged database yields a finding per page and a
// report nobody can read is not more informative than one that is. The number
// left out is stated rather than the cap being silent about it.
func checkIntegrity(ctx context.Context, db *sql.DB, path string) VerifyCheck {
	const maxReported = 5
	c := VerifyCheck{Name: "integrity check"}
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("SQLite could not check %s: %v", path, err)
		return c
	}
	defer rows.Close() //nolint:errcheck
	var findings []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			c.State = VerifyFailed
			c.Detail = fmt.Sprintf("SQLite could not read its own integrity report for %s: %v", path, err)
			return c
		}
		// Split before judging: a row is a container, and a container holding one
		// "ok" next to a complaint is not a pass.
		for _, line := range strings.Split(r, "\n") {
			if line = strings.TrimSpace(line); line != "" {
				findings = append(findings, line)
			}
		}
	}
	readErr := rows.Err()
	if len(findings) == 0 {
		c.State = VerifyFailed
		if readErr != nil {
			c.Detail = fmt.Sprintf("SQLite could not finish checking %s: %v", path, readErr)
		} else {
			c.Detail = fmt.Sprintf("SQLite reported nothing at all for %s, which is not a pass", path)
		}
		return c
	}
	if len(findings) > 0 && findings[0] == "ok" && readErr == nil {
		c.State = VerifyOK
		c.Detail = "SQLite reports the file is structurally sound"
		return c
	}
	c.State = VerifyFailed
	shown, extra := capFindings(findings, maxReported)
	if readErr != nil {
		c.Detail = fmt.Sprintf("SQLite reports the file is damaged and stopped reporting partway (%v): %s",
			readErr, strings.Join(shown, "; "))
	} else {
		c.Detail = "SQLite reports the file is damaged: " + strings.Join(shown, "; ")
	}
	if extra > 0 {
		c.Detail += fmt.Sprintf(" (and %d more)", extra)
	}
	return c
}

// capFindings returns at most n findings and how many were left out, so a caller
// can say so rather than silently truncating the answer to "is this sound".
func capFindings(findings []string, n int) (shown []string, extra int) {
	if len(findings) <= n {
		return findings, 0
	}
	return findings[:n], len(findings) - n
}

// checkSchemaVersion answers the question a restore actually turns on: will this
// build open this file. The file's own version decides it, and the manifest's
// has to agree — a manifest that misstates the file it describes is a
// description nobody can rely on for anything else either.
//
// The two directions are NOT symmetric, and that is the whole point of the
// check. A file from a NEWER Ghost is a downgrade to restore: OpenDB refuses it
// outright, so there is nothing this build can do with it, and the answer is to
// upgrade. A file from an OLDER Ghost is exactly what the documented restore
// path produces — every upgrade a user has ever done left one behind — and the
// next ordinary open migrates it, so it passes with the migration named.
//
// Refusing the older direction would be self-defeating: the pre-migration copy
// `OpenDB` writes before every migration step is by construction at a lower
// user_version than this build, so the file a user is told to keep in case an
// upgrade goes wrong would be the one file the command always rejected. A check
// whose answer is "no" for the copy it exists to protect is not a check.
func checkSchemaVersion(version int, m BackupManifest, hasManifest bool) VerifyCheck {
	c := VerifyCheck{Name: "schema version"}
	if version > SchemaVersion() {
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("the file is at schema v%d and this Ghost reads v%d — a newer Ghost wrote it, so upgrade before restoring it",
			version, SchemaVersion())
		if hasManifest && m.SchemaVersion != version {
			c.Detail += fmt.Sprintf(" (its manifest records v%d)", m.SchemaVersion)
		}
		return c
	}
	if hasManifest && m.SchemaVersion != version {
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("the manifest records schema v%d but the file is at v%d — it does not describe this file",
			m.SchemaVersion, version)
		return c
	}
	c.State = VerifyOK
	if version < SchemaVersion() {
		c.Detail = fmt.Sprintf("the file is at schema v%d and this Ghost reads v%d — it is restorable, and the next open migrates it",
			version, SchemaVersion())
		return c
	}
	c.Detail = fmt.Sprintf("the file is at schema v%d, this Ghost reads v%d", version, SchemaVersion())
	return c
}

// checkCounts compares what the file holds to what the manifest recorded. Both
// numbers come from the file — the manifest's is a claim, this is the reading —
// and they are read from the same query CountRows backs, so a count that moves
// between the backup and the verify is a real change rather than two functions
// that disagree about what to count.
func checkCounts(got BackupCounts, m BackupManifest, hasManifest bool) VerifyCheck {
	c := VerifyCheck{Name: "row counts"}
	if !hasManifest {
		c.State = VerifySkipped
		c.Detail = "no manifest to compare against; the file holds " + got.String()
		return c
	}
	if got != m.Counts {
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("the manifest records %s but the file holds %s",
			m.Counts.String(), got.String())
		return c
	}
	c.State = VerifyOK
	c.Detail = "the file holds the " + got.String() + " the manifest records"
	return c
}

// checkHash is the check the others cannot make. integrity_check reads the
// pages SQLite uses and the counts read the rows; a byte flipped anywhere else
// in the file — a free page, the unused tail of the last page — leaves both
// exactly as they were, and is caught by nothing but this.
func checkHash(path string, m BackupManifest, hasManifest bool, size int64) VerifyCheck {
	c := VerifyCheck{Name: "sha256"}
	if !hasManifest {
		c.State = VerifySkipped
		c.Detail = "no manifest to compare against"
		return c
	}
	if size != m.Bytes {
		// Checked before the hash so a truncated or appended-to copy is named
		// as the size change it plainly is, rather than as a 64-character
		// digest the reader has to diff by hand.
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("the manifest records %d bytes, the file is %d — it is not the file the manifest describes",
			m.Bytes, size)
		return c
	}
	sum, err := FileSHA256(path)
	if err != nil {
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("could not hash the file: %v", err)
		return c
	}
	if sum != m.SHA256 {
		c.State = VerifyFailed
		c.Detail = fmt.Sprintf("the manifest records sha256 %s… and the file hashes to %s… — it is not the file the manifest describes",
			shortHash(m.SHA256), shortHash(sum))
		return c
	}
	c.State = VerifyOK
	c.Detail = "matches the manifest's sha256 " + shortHash(sum) + "…"
	return c
}

// String renders a BackupCounts as prose in the order the tables are reported,
// dropping the ones that hold nothing: a message that is mostly "0 links, 0
// tasks, 0 decisions" makes the two that matter harder to find, and a store that
// holds nothing says so rather than printing five zeroes that read as data.
func (c BackupCounts) String() string {
	parts := make([]string, 0, 5)
	for _, p := range []struct {
		n    int
		one  string
		many string
	}{
		{c.Projects, "project", "projects"},
		{c.Memories, "memory", "memories"},
		{c.MemoryLinks, "link", "links"},
		{c.Tasks, "task", "tasks"},
		{c.Decisions, "decision", "decisions"},
	} {
		if p.n == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%d %s", p.n, plural(p.n, p.one, p.many)))
	}
	if len(parts) == 0 {
		return "no rows"
	}
	return joinWithAnd(parts)
}

// joinWithAnd is strings.Join with a final "and", so a message reads as a
// sentence rather than as a list a reader has to parse.
func joinWithAnd(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	case 2:
		return parts[0] + " and " + parts[1]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

// shortHash is the first 12 hex digits of a digest, for a message a person
// reads. A full 64 is enough to compare two reports by eye and short enough not
// to wrap the line it is on; the full value is in the manifest either way.
func shortHash(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}
