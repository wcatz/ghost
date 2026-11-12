package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// ManifestVersion identifies the backup manifest format version.
const ManifestVersion = 1

const manifestSuffix = ".manifest.json"

// BackupManifest describes a backup snapshot.
type BackupManifest struct {
	ManifestVersion int          `json:"manifest_version"`
	SchemaVersion   int          `json:"schema_version"`
	CreatedAt       string       `json:"created_at"`
	Database        string       `json:"database"`
	Bytes           int64        `json:"bytes"`
	SHA256          string       `json:"sha256"`
	Counts          BackupCounts `json:"counts"`
}

// ManifestPath returns the manifest path for a backup snapshot.
func ManifestPath(dbPath string) string { return dbPath + manifestSuffix }

// FileSHA256 returns the SHA-256 digest of a file.
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
	f, err := openManifestFile(path)
	if err != nil {
		return "", err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("narrow manifest %s: %w", path, err)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return "", fmt.Errorf("write manifest %s: %w", path, err)
	}
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

// lines inline so that the O_NOFOLLOW refusal and the message it maps to are
func openManifestFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to write the manifest to %s, which is not a regular file", path)
	} else if err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("check the manifest path %s: %w", path, err)
	}

	f, err := os.OpenFile(path, manifestOpenFlags, 0o600)
	if err != nil {
		return nil, manifestOpenError(path, err)
	}
	return f, nil
}

func manifestOpenError(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("refusing to write the manifest to %s, which became a symlink and I will not write through it", path)
	}
	return fmt.Errorf("create manifest %s: %w", path, err)
}

// ReadBackupManifest reads and validates a backup manifest.
func ReadBackupManifest(path string) (BackupManifest, error) {
	var m BackupManifest
	info, err := os.Stat(path)
	if err != nil {
		return BackupManifest{}, err
	}
	if !info.Mode().IsRegular() {
		return BackupManifest{}, fmt.Errorf("the manifest at %s is not a regular file (%s)", path, info.Mode().Type())
	}
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
	if !isSHA256Hex(m.SHA256) {
		return BackupManifest{}, fmt.Errorf("the manifest at %s is not readable as a manifest: its sha256 is not 64 lowercase hex digits: %s",
			path, RefusalArg("sha256", m.SHA256))
	}
	return m, nil
}

func isSHA256Hex(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// VerifyState reports the result of a backup verification check.
type VerifyState string

const (
	VerifyOK      VerifyState = "ok"
	VerifyFailed  VerifyState = "failed"
	VerifySkipped VerifyState = "skipped"
)

// VerifyCheck reports one backup verification check.
type VerifyCheck struct {
	Name   string
	State  VerifyState
	Detail string
}

// VerifyReport reports backup verification results.
type VerifyReport struct {
	Path            string
	Bytes           int64
	BytesRead       bool
	HasManifest     bool
	ManifestPresent bool
	ManifestErr     error
	ManifestPath    string
	SchemaVersion   int
	Counts          BackupCounts
	CountsRead      bool
	Checks          []VerifyCheck
	Problems        []string
}

// Failed returns the names of failed checks.
func (r VerifyReport) Failed() []string {
	var names []string
	for _, c := range r.Checks {
		if c.State == VerifyFailed {
			names = append(names, c.Name)
		}
	}
	return names
}

// VerifyBackup verifies a backup snapshot and its manifest.
func VerifyBackup(ctx context.Context, path string) (VerifyReport, error) {
	rep := VerifyReport{Path: path, ManifestPath: ManifestPath(path)}

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
	case os.IsNotExist(err) && !rep.ManifestPresent:
	default:
		rep.ManifestErr = err
	}
	fail := func(err error) (VerifyReport, error) {
		if rep.ManifestErr != nil {
			return rep, rep.ManifestErr
		}
		return rep, err
	}

	info, err := os.Stat(path)
	if err != nil {
		return fail(err)
	}
	if !info.Mode().IsRegular() {
		return fail(fmt.Errorf("the backup at %s is not a regular file (%s)", path, info.Mode().Type()))
	}
	rep.Bytes, rep.BytesRead = info.Size(), true

	if rep.ManifestErr != nil {
		rep.add(checkManifestUnreadable("sha256"))
	} else {
		rep.add(checkHash(path, manifest, rep.HasManifest, info.Size()))
	}

	db, err := OpenReadDB(path)
	if err != nil {
		return fail(fmt.Errorf("%s cannot be checked as a backup: %w", path, err))
	}
	defer db.Close() //nolint:errcheck
	rep.add(checkIntegrity(ctx, db, path))

	// lines would read like a build that only has two checks, and an operator
	var readErr error
	version, err := DBUserVersion(db)
	if err != nil {
		rep.add(checkSchemaVersionUnreadable(err))
		readErr = fmt.Errorf("read the schema version of %s: %w", path, err)
	} else {
		rep.SchemaVersion = version
		rep.add(checkSchemaVersion(version, manifest, rep.HasManifest))
	}
	if rep.ManifestErr != nil {
		rep.add(checkManifestUnreadable("row counts"))
		return fail(readErr)
	}
	counts, err := CountRows(ctx, db)
	if err != nil {
		rep.add(checkCountsUnreadable(err))
		if readErr == nil {
			readErr = err
		}
	} else {
		rep.Counts, rep.CountsRead = counts, true
		rep.add(checkCounts(counts, manifest, rep.HasManifest))
	}
	return rep, readErr
}

func (r *VerifyReport) add(c VerifyCheck) {
	r.Checks = append(r.Checks, c)
	if c.State == VerifyFailed {
		r.Problems = append(r.Problems, c.Detail)
	}
}

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

func capFindings(findings []string, n int) (shown []string, extra int) {
	if len(findings) <= n {
		return findings, 0
	}
	return findings[:n], len(findings) - n
}

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

func checkSchemaVersionUnreadable(err error) VerifyCheck {
	return VerifyCheck{
		Name:   "schema version",
		State:  VerifySkipped,
		Detail: fmt.Sprintf("could not read the file's schema version: %v", err),
	}
}

func checkManifestUnreadable(name string) VerifyCheck {
	return VerifyCheck{
		Name:   name,
		State:  VerifySkipped,
		Detail: "the manifest beside it could not be read, so there is nothing to compare against",
	}
}

func checkCountsUnreadable(err error) VerifyCheck {
	return VerifyCheck{
		Name:   "row counts",
		State:  VerifySkipped,
		Detail: fmt.Sprintf("could not count the file's rows: %v", err),
	}
}

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

func checkHash(path string, m BackupManifest, hasManifest bool, size int64) VerifyCheck {
	c := VerifyCheck{Name: "sha256"}
	if !hasManifest {
		c.State = VerifySkipped
		c.Detail = "no manifest to compare against"
		return c
	}
	if size != m.Bytes {
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
	if !isSHA256Hex(m.SHA256) {
		c.State = VerifyFailed
		c.Detail = "the manifest's sha256 is not 64 lowercase hex digits: " + RefusalArg("sha256", m.SHA256)
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

// String formats backup counts for display.
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

func shortHash(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}
