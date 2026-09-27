package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// backupUsage, exportUsage and importUsage are the per-command help texts. Each
// goes to stdout for -h/--help — a user who asks is not making an error — and
// to stderr alongside an argument error, so a mistyped flag is answered with the
// syntax that would have worked.
const (
	backupUsage = `Usage: ghost backup [--out <path>]

Writes a consistent snapshot of the memory database while Ghost is running.

  --out <path>   Where to write it. Default: a timestamped file beside the
                 database, in the Ghost data directory. The path must not
                 already exist — an existing backup is never replaced.

The snapshot is taken with SQLite's VACUUM INTO, which reads one consistent
snapshot of a live WAL database rather than copying its files: a copy made by
hand can capture a torn state, and copying only ghost.db without its -wal loses
whatever the write-ahead log held. The write lock is held for the length of the
vacuum and no longer.

The command prints the path, the file size, and the row count of each table, so
a restore can be checked against what the file actually contains. To restore,
stop Ghost, move the file into the data directory as ghost.db (removing the
old ghost.db, ghost.db-wal and ghost.db-shm first), and start Ghost again.
`

	exportUsage = `Usage: ghost export [--project <name>] [--out <file.jsonl>]

Writes memories, tasks, decisions and projects as JSON Lines — one JSON object
per line, readable, diffable and editable.

  --project <name>   Export one project, matched by name or id exactly.
  --out <path>       Where to write it. Default: a timestamped .jsonl beside
                     the database. Use "-" for standard output.

Embeddings are not exported: they are derived from content by a local model and
the embedding worker rebuilds them. Memory links are not exported either — they
only mean something between two memories that are both present, and the linking
worker recomputes related edges after an import.

Two exports of an unchanged database are byte-identical, so an artifact can be
diffed against the previous one. Use ` + "`ghost import`" + ` to load one back.
`

	importUsage = `Usage: ghost import <file.jsonl> [--apply] [--trust-provenance]

Loads a JSONL artifact written by ` + "`ghost export`" + `.

  --apply              Actually write. Without it the command is a dry run that
                       reports what it would create, skip and reject, and writes
                       nothing — over a read-only connection, so the preview
                       cannot migrate or seed the store either.
  --trust-provenance   Keep each memory's own source and pin state. Without it
                       every imported memory is stamped "onboarding" and
                       unpinned, so a file from somewhere else cannot plant rows
                       that read as your own words or as Ghost's shipped rules.
                       Pass it when the artifact is your own export.

A record whose id is already in the database is skipped, never overwritten: the
artifact is the older of the two copies, so overwriting would restore stale data
over live data. This also makes re-running an import always safe, which is how
you repair a run that rejected a record.

Imported memory content is held to the same length cap and the same validation
as a normal save, so an artifact from another machine cannot write a row Ghost
would refuse to produce. A file whose schema version this build does not read is
refused outright.
`
)

// backupOptions, exportOptions and importOptions are the parsed arguments of
// the three commands.
type (
	backupOptions struct {
		Out string
	}
	exportOptions struct {
		Project string
		Out     string
	}
	importOptions struct {
		File  string
		Apply bool
		// TrustProvenance keeps each memory's own source and pin state instead
		// of downgrading them. See memory.ImportOptions.TrustProvenance; the
		// short version is that an artifact is a file that arrived from
		// somewhere, and on its own authority it does not get to plant rows that
		// read as your own words or as Ghost's shipped rules.
		TrustProvenance bool
	}
)

// parseBackupArgs parses `ghost backup`. An unknown flag is an error rather
// than being ignored, because the one flag that matters here is --out: a
// misspelled --ou would otherwise write the default file and leave the user
// believing it went where they asked.
func parseBackupArgs(args []string) (backupOptions, error) {
	var opts backupOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag --out needs a value")
			}
			i++
			opts.Out = args[i]
		case strings.HasPrefix(arg, "--out="):
			opts.Out = strings.TrimPrefix(arg, "--out=")
		default:
			return opts, fmt.Errorf("unknown argument %q", arg)
		}
	}
	return opts, nil
}

// parseExportArgs parses `ghost export`. Both --flag value and --flag=value are
// accepted, matching the obsidian command's spelling.
func parseExportArgs(args []string) (exportOptions, error) {
	var opts exportOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out" || arg == "--project":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			value := args[i]
			if arg == "--out" {
				opts.Out = value
			} else {
				opts.Project = value
			}
		case strings.HasPrefix(arg, "--out="):
			opts.Out = strings.TrimPrefix(arg, "--out=")
		case strings.HasPrefix(arg, "--project="):
			opts.Project = strings.TrimPrefix(arg, "--project=")
		default:
			return opts, fmt.Errorf("unknown argument %q", arg)
		}
	}
	return opts, nil
}

// parseImportArgs parses `ghost import <file> [--apply]`. Exactly one file is
// accepted: importing two artifacts in one run would interleave their records
// and produce a store that is the union of both, which is not something either
// file asked for.
func parseImportArgs(args []string) (importOptions, error) {
	var opts importOptions
	var positional []string
	for _, arg := range args {
		switch {
		case arg == "--apply":
			opts.Apply = true
		case arg == "--trust-provenance":
			opts.TrustProvenance = true
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) != 1 {
		return opts, fmt.Errorf("expected exactly one artifact file, got %d", len(positional))
	}
	opts.File = positional[0]
	return opts, nil
}

// transferTimeLayout is the UTC timestamp a default backup or export name
// carries. The same layout as the memory package's backup name, so one
// convention covers every timestamped file in the data directory.
const transferTimeLayout = "20060102T150405Z"

// backupDefaultPath resolves where a backup goes. An explicit --out is used
// verbatim; otherwise the file lands beside the live database with a UTC
// timestamp, so a series of backups sorts by age and none can be mistaken for
// the database itself.
func backupDefaultPath(dbPath, out string, at time.Time) (string, error) {
	if out != "" {
		return out, nil
	}
	if dbPath == "" {
		return "", fmt.Errorf("cannot locate the database to derive a default backup path from")
	}
	return memory.BackupFileName(dbPath, at.UTC()), nil
}

// exportDefaultPath resolves where an artifact goes. "-" means standard output,
// which is left for the caller to open.
func exportDefaultPath(dataDir, out string, at time.Time) (string, error) {
	switch {
	case out == "-":
		return "-", nil
	case out != "":
		return out, nil
	case dataDir == "":
		return "", fmt.Errorf("cannot locate the data directory to derive a default export path from")
	default:
		return filepath.Join(dataDir, fmt.Sprintf("ghost-export-%s.jsonl", at.UTC().Format(transferTimeLayout))), nil
	}
}

// runBackup implements `ghost backup [--out <path>]`.
//
// The store is reached through bootstrap(), so this is a read-WRITE open: it
// runs migrations and seeds the builtin rows, and it is refused outright for a
// store from a newer Ghost. An earlier version of this comment said "nothing on
// the source is written", which was false and load-bearing — it made `ghost
// backup` sound like the read-only way to get a copy, and a user acting on that
// would have been migrated by a command that read as a read. (VACUUM INTO itself
// writes only the destination, so the vacuum is not the reason; the open is.)
//
// Read-write is kept deliberately. A store behind the current schema is exactly
// the store a user most wants a restorable copy of, and a read-only open would
// refuse it — the same version gate export has. So a migration is what this
// command does on such a store, and it is a migration they need before export
// works at all. It is safe to run against a live MCP server, which is the point
// of using the online path rather than copying the files.
func runBackup() {
	opts, err := parseBackupArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, backupUsage)
		os.Exit(1)
	}

	dataDir, err := dataDirPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	// Stat first: a backup that created the database it was asked to back up
	// would report a snapshot of an empty store, which reads as "you have no
	// memories" rather than "there is no database".
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: no database at %s — start a session, or run ghost mcp init, first\n", dbPath)
		os.Exit(1)
	} else if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}

	dest, err := backupDefaultPath(dbPath, opts.Out, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	_, _, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck

	if err := runBackupCore(context.Background(), store, os.Stdout, dest); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runBackupCore takes the snapshot and reports it. Split out of runBackup so the
// whole command is testable against a temp-dir store, without the argument
// parsing, the config load or the process exit that surround it — and without
// re-executing the test binary, which is the one thing a cmd/ghost test must
// never do.
func runBackupCore(ctx context.Context, store *memory.Store, out io.Writer, dest string) error {
	res, err := store.Backup(ctx, dest)
	if err != nil {
		return err
	}
	return printBackupReport(out, res)
}

// printBackupReport names the file and every count a restore would be checked
// against. The counts are per table rather than a single total, because "the
// file is there" is not the question — "does it hold what my store held" is.
func printBackupReport(out io.Writer, res memory.BackupResult) error {
	if _, err := fmt.Fprintf(out, "backed up %s (%d bytes)\n", res.Path, res.Bytes); err != nil {
		return err
	}
	rows := []struct {
		name  string
		count int
	}{
		{"projects", res.Counts.Projects},
		{"memories", res.Counts.Memories},
		{"memory_links", res.Counts.MemoryLinks},
		{"tasks", res.Counts.Tasks},
		{"decisions", res.Counts.Decisions},
	}
	for _, r := range rows {
		if _, err := fmt.Fprintf(out, "  %-13s %d\n", r.name+":", r.count); err != nil {
			return err
		}
	}
	return nil
}

// runExport implements `ghost export [--project <name>] [--out <file.jsonl>]`.
//
// The store is opened read-only: an export is a read, and running it against a
// live MCP server must not be able to change anything. The summary goes to
// stdout for a file destination and to stderr when the artifact itself is
// stdout, so `ghost export --out - | jq` gets clean JSON with the summary out of
// the pipe's way.
func runExport() {
	opts, err := parseExportArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, exportUsage)
		os.Exit(1)
	}

	dataDir, err := dataDirPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dest, err := exportDefaultPath(dataDir, opts.Out, time.Now())
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	store, err := openReadOnlyTransferStore(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer store.Close() //nolint:errcheck

	summary := os.Stdout
	if dest == "-" {
		summary = os.Stderr
	}
	if err := runExportCore(context.Background(), store, summary, dest, opts.Project); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runExportCore writes the artifact and reports what it wrote. On a file
// destination the file is created with the same 0600 width the database has,
// before any record is written, so a failure part-way leaves a file only the
// user can read.
func runExportCore(ctx context.Context, store *memory.Store, summary io.Writer, dest, projectFilter string) error {
	if dest == "-" {
		stats, err := portable.Export(ctx, store, os.Stdout, projectFilter)
		if err != nil {
			return err
		}
		return printExportSummary(summary, "-", stats)
	}
	f, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		// O_EXCL refuses an existing artifact for the same reason a backup
		// does: overwriting one would destroy the copy someone may be holding.
		// The message says so, because "file exists" on its own reads as an
		// obstacle rather than as the decision this is.
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
		}
		return fmt.Errorf("create %s: %w", dest, err)
	}
	stats, exportErr := portable.Export(ctx, store, f, projectFilter)
	// Close before reporting, so a write error the close surfaces is not lost
	// and the file is flushed either way.
	if err := f.Close(); err != nil && exportErr == nil {
		exportErr = fmt.Errorf("close %s: %w", dest, err)
	}
	if exportErr != nil {
		// A partial artifact is worse than none: it is a file the user can see,
		// and importing it half-succeeds. Removing it leaves the failure as the
		// only record of the run.
		if rmErr := os.Remove(dest); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("%w (and the partial file %s could not be removed: %v)", exportErr, dest, rmErr)
		}
		return exportErr
	}
	return printExportSummary(summary, dest, stats)
}

// printExportSummary states where the artifact is and what it holds.
func printExportSummary(out io.Writer, path string, stats portable.Stats) error {
	_, err := fmt.Fprintf(out, "exported %s to %s\n",
		pluralRecords(stats.Projects, stats.Memories, stats.Tasks, stats.Decisions), path)
	return err
}

// printImportReport states what an import did, or — for a dry run — what it
// would do. The headline leads with the count because that is what a reader
// printRecordLine reports one record's outcome, and is what makes a dry run
// reviewable: the reader sees each memory's content prefix, not just a count.
func printRecordLine(out io.Writer, r portable.RecordResult) error {
	detail := r.Detail
	// A credential refusal prints the id and nothing else. portable's
	// safeDetail already reduces the detail to the id for any record whose text
	// holds one — see the note there — and this is the second line for a
	// RecordResult that reached this function with a detail set by something
	// other than that helper. The format still reaches the reader through
	// report.Errors, which is the part needed to fix the artifact.
	var refused *memory.SecretContentError
	if errors.As(r.Error, &refused) {
		detail = r.ID
	}
	// Only a memory has provenance to speak of. A project or a task line that
	// said "provenance kept as exported" would be a sentence about a field the
	// record does not have.
	isCreate := r.Action == portable.ActionCreate && r.Type == portable.TypeMemory
	switch {
	case isCreate && r.Downgraded:
		// The provenance rewrite is the one thing about the record that is not
		// visible afterwards: the stored source is the downgraded one, so the
		// artifact's own value is gone from the store entirely. It is said here
		// or not at all.
		detail = strings.TrimSpace(detail) + " (provenance downgraded to " + memory.DowngradedSource + ", unpinned)"
	case isCreate && r.Clamped:
		// A clamp is the other thing a reader cannot see in the file afterwards,
		// because the stored text is the cut version.
		detail = strings.TrimSpace(detail) + " …(truncated at the content cap)"
	case isCreate:
		detail = strings.TrimSpace(detail) + " (provenance kept as exported)"
	}
	switch {
	case detail == "":
		detail = r.ID
	case r.ID != "":
		detail = fmt.Sprintf("%q (%s)", detail, r.ID)
	}
	_, err := fmt.Fprintf(out, "  %-7s %-9s line %d  %s\n", r.Action, r.Type+":", r.Line, detail)
	return err
}

// printImportReport states what an import did, or — for a dry run — what it
// would do. The headline leads with the count because that is what a reader
// scans for; the per-record lines came before it and the rejected ones follow.
func printImportReport(out io.Writer, path string, report portable.ImportReport, opts portable.ImportOptions) error {
	created := report.Created["project"] + report.Created["memory"] +
		report.Created["task"] + report.Created["decision"]
	skipped := report.Skipped["project"] + report.Skipped["memory"] +
		report.Skipped["task"] + report.Skipped["decision"]
	summary := pluralRecords(report.Created["project"], report.Created["memory"],
		report.Created["task"], report.Created["decision"])

	// A run that created nothing says so, rather than "imported no records" —
	// which reads as a failure and is in fact the correct answer to a re-run of
	// an artifact the store already holds in full.
	var headline string
	switch {
	case created == 0:
		headline = fmt.Sprintf("nothing to import from %s", path)
	case opts.Apply:
		headline = fmt.Sprintf("imported %s from %s", summary, path)
	default:
		headline = fmt.Sprintf("would import %s from %s", summary, path)
	}
	if _, err := fmt.Fprintln(out, headline); err != nil {
		return err
	}
	if skipped > 0 {
		if _, err := fmt.Fprintf(out, "skipped %s already present\n", countLabel(skipped)); err != nil {
			return err
		}
	}
	// The provenance policy is stated in the summary and not only on the record
	// lines: it decides what every memory in the file becomes, so a reader who
	// skips the per-record output still has to learn it.
	if report.Created["memory"] > 0 {
		note := "provenance downgraded to " + memory.DowngradedSource + " and unpinned — pass --trust-provenance to keep the artifact's own"
		if opts.TrustProvenance {
			note = "provenance kept as the artifact states it (--trust-provenance)"
		}
		if _, err := fmt.Fprintf(out, "  %s\n", note); err != nil {
			return err
		}
	}
	if report.Rejected > 0 {
		if _, err := fmt.Fprintf(out, "rejected %s\n", countLabel(report.Rejected)); err != nil {
			return err
		}
		for _, err := range report.Errors {
			if _, err := fmt.Fprintf(out, "  %v\n", err); err != nil {
				return err
			}
		}
	}
	if !opts.Apply {
		_, err := fmt.Fprintln(out, "\nnothing written — pass --apply to import")
		return err
	}
	return nil
}

// runImport implements `ghost import <file.jsonl> [--apply] [--trust-provenance]`.
//
// The store is opened read-write only when the run will write. A dry run opens
// read-only, so it cannot migrate a database whose schema is behind or seed the
// builtin rows — see openImportStore for why that is not a detail. An import
// that writes goes through the same store methods a session uses, including the
// content cap and the category, source and importance validation: an artifact
// from another machine is another way to reach the memories table, and it must
// not be a way around its rules.
//
// A rejected record makes the command exit non-zero even though the rest of the
// file was applied. A partial import reported as a success is the failure mode
// worth spending an exit code on: the user has to learn that one record did not
// land.
func runImport() {
	opts, err := parseImportArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, importUsage)
		os.Exit(1)
	}

	store, err := openImportStore(opts.Apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer store.Close() //nolint:errcheck

	if err := runImportCore(context.Background(), store, opts.File, importOptionsFrom(opts), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// importOptionsFrom converts the parsed flags into the run's options. The two
// booleans are the whole of it, and the conversion is named rather than
// inlined so runImport and a test build the same thing.
func importOptionsFrom(opts importOptions) portable.ImportOptions {
	return portable.ImportOptions{Apply: opts.Apply, TrustProvenance: opts.TrustProvenance}
}

// runImportCore imports one artifact and prints the per-record outcome and the
// summary. Split out of runImport so the whole path is testable against a
// temp-dir store, without argument parsing, the config load or a process exit —
// and without re-executing the test binary.
func runImportCore(ctx context.Context, store *memory.Store, path string, opts portable.ImportOptions, out io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no artifact at %s — write one with ghost export", path)
		}
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck

	report, err := portable.Import(ctx, store, f, opts, func(r portable.RecordResult) {
		// A per-record write failure is reported inline and not propagated: the
		// summary and the exit code carry the outcome, and a report that stopped
		// at the first such failure would hide the records that did land.
		_ = printRecordLine(out, r)
	})
	if err != nil {
		// The file itself was refused — no header, an unreadable schema version.
		// Nothing was applied, so there is no run to summarise, and a summary here
		// would read as "an import happened and found nothing", which is a
		// different and wrong statement.
		return err
	}
	if err := printImportReport(out, path, report, opts); err != nil {
		return err
	}
	if report.Rejected > 0 {
		return fmt.Errorf("%s — the records above were rejected; fix them in the artifact and re-run, which is safe", countLabel(report.Rejected))
	}
	return nil
}

// pluralRecords renders "N projects, M memories, …" for the counts that are
// non-zero, in a fixed order, with a noun that agrees with its count.
func pluralRecords(projects, memories, tasks, decisions int) string {
	parts := []string{
		pluralCount(projects, "project", "projects"),
		pluralCount(memories, "memory", "memories"),
		pluralCount(tasks, "task", "tasks"),
		pluralCount(decisions, "decision", "decisions"),
	}
	kept := parts[:0]
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		return "no records"
	}
	return strings.Join(kept, ", ")
}

// countLabel is "N records" / "1 record".
func countLabel(n int) string {
	if n == 1 {
		return "1 record"
	}
	return fmt.Sprintf("%d records", n)
}

// pluralCount renders "N noun", with the plural form used for every count but
// one, and the empty string for a count of zero so a caller can drop the clause.
func pluralCount(n int, singular, plural string) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
