package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

// backupUsage, exportUsage and importUsage are the per-command help texts. Each
// goes to stdout for -h/--help — a user who asks is not making an error — and
// to stderr alongside an argument error, so a mistyped flag is answered with the
// syntax that would have worked.
const (
	backupUsage = `Usage: ghost backup [--out <path>]
       ghost backup verify <file>

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
a restore can be checked against what the file actually contains. It also writes
a sidecar manifest beside the snapshot — <snapshot>.manifest.json — recording the
schema version, the row count of each table, the size and a SHA-256 of the
snapshot. That is what "ghost backup verify" checks against; see below and
"ghost backup verify --help".

To restore, stop Ghost, move the file into the data directory as ghost.db
(removing the old ghost.db, ghost.db-wal and ghost.db-shm first), and start Ghost
again. Run "ghost backup verify <file>" on the copy first.
`

	backupVerifyUsage = `Usage: ghost backup verify <file>

Checks a backup before you trust it, and exits non-zero if the file cannot be
vouched for. It reads the file and the manifest beside it, and it does not open
the database in your data directory, so running it can neither migrate nor seed
your live store — which matters, because the moment a user reaches for this
command is the moment they are least sure what state their own store is in.

Four checks, all reported, in the order they run:

  sha256           The file is the size its manifest records and hashes to what
                   the manifest records. This is the only check that needs
                   nothing but the path, which is why it runs first: a truncated
                   or appended-to copy is diagnosed here, and a file that is not
                   a database at all still gets an answer.
  integrity check  SQLite's own PRAGMA integrity_check. When it fails, its
                   answer is printed as it stands: it names the pages and the
                   reasons, which is the most anyone will be told about the
                   damage.
  schema version   The version the file carries, against the one this build
                   reads and the one its manifest recorded. A file from a newer
                   Ghost is a downgrade to restore and is refused; an older one
                   is what the documented restore path produces, and is reported
                   as restorable with the migration named.
  row counts       The rows the file holds, against the manifest's.

Every check that can run does. One that cannot reports as skipped, which says it
did not run rather than that it passed.

A file with no manifest beside it is not refused, and does not get the word
"verified" either: a pre-migration copy — taken before a migration — is written
without one, and it is exactly the copy a user wants to check after a bad
upgrade. Such a file is reported as checked, with the two manifest-derived checks
marked skipped and the other two still standing.

A manifest that is present but unreadable IS a refusal. That is damage, not an
absent optional file, and treating it as absent would quietly downgrade a damaged
sidecar into an unverified backup that still looked fine.
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

A record ` + "`ghost import`" + ` would refuse is LEFT OUT and named on stderr, and
the command exits non-zero. The exporter applies the importer's own refusal
predicates, so what is left out is exactly what a restore would reject. A
credential-shaped field is refused by design and the report names the field, never
the value; see the export section of "ghost help" for the whole rule.

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

// parseBackupVerifyArgs parses `ghost backup verify <file>`. Exactly one file:
// two paths in one run would produce one verdict for a question that has two
// answers, and a flag is a flag this command has no meaning for — it reads one
// file, and the only way to change which file is to name a different one.
func parseBackupVerifyArgs(args []string) (string, error) {
	var positional []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return "", fmt.Errorf("unknown flag %q", arg)
		}
		positional = append(positional, arg)
	}
	if len(positional) != 1 {
		return "", fmt.Errorf("expected exactly one backup file, got %d", len(positional))
	}
	return positional[0], nil
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
//
// The manifest is named too. It is a second file the user did not ask for, and
// this report is the only place they learn it exists: a backup whose description
// is in a file nobody was told about is a backup nobody will check.
func printBackupReport(out io.Writer, res memory.BackupResult) error {
	if _, err := fmt.Fprintf(out, "backed up %s (%d bytes)\n", res.Path, res.Bytes); err != nil {
		return err
	}
	if res.ManifestPath != "" {
		if _, err := fmt.Fprintf(out, "  %-13s %s\n", "manifest:", res.ManifestPath); err != nil {
			return err
		}
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

// runBackupVerify implements `ghost backup verify <file>`.
//
// Unlike every other command here it opens no store: bootstrap() migrates the
// database in the data directory and seeds its builtin rows, and the moment a
// user reaches for this command is the moment they are least sure what state
// their own store is in. Nothing here reads anything but the file it was given
// and the manifest beside it, so the check is equally safe against a copy on
// another machine and against a copy of a store this binary has never seen.
func runBackupVerify(args []string) {
	path, err := parseBackupVerifyArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, backupVerifyUsage)
		os.Exit(1)
	}
	if err := runBackupVerifyCore(context.Background(), os.Stdout, path); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runBackupVerifyCore checks one file and prints every check. Split out of
// runBackupVerify for the same reason runBackupCore is split out of runBackup:
// the whole command, minus the argument parsing and the process exit.
//
// The report is printed even when the run fails, hard errors included — the
// checks that did run are the diagnosis, and a file that is not a database at
// all still produces a report worth reading. A failed check is an error return
// as well as a line of output, because the exit code is what a script branches
// on: a verify that printed "failed" and exited 0 would be read as a pass by
// every caller that is not parsing the text.
func runBackupVerifyCore(ctx context.Context, out io.Writer, path string) error {
	rep, err := memory.VerifyBackup(ctx, path)
	if perr := printVerifyReport(out, rep, err); perr != nil && err == nil {
		err = perr
	}
	if err != nil {
		return err
	}
	if failed := rep.Failed(); len(failed) > 0 {
		return fmt.Errorf("%s cannot be trusted as a backup: %s failed", path, strings.Join(failed, ", "))
	}
	return nil
}

// printVerifyReport states the file, what it holds, and every check with its
// verdict. The error is a parameter rather than something printVerifyReport
// infers, because a run that failed has established nothing: printing it as
// though it had would put a verdict word on a report with no checks in it.
//
// The verdict is a column of its own rather than folded into the detail,
// because the difference between ok, failed and skipped is the whole report. A
// check that did not run says nothing about the file, and a reader who cannot
// see that difference has been handed a reassurance they did not earn — which is
// the exact failure this command exists to prevent, one level up.
//
// Three verdicts, and the middle one is the point of having three:
//
//	verified  every check ran and passed
//	checked   nothing failed, but something did not run — which is what a
//	          pre-migration copy, taken with no manifest, is
//	refusing  a check failed, or the file could not be checked at all
//
// "verified" is reserved for the first. It is the strongest word the command
// can print, and a file whose hash was never checked has not earned it: the side
// car is the part a user forgets to copy, so a manifest-less file is the single
// most likely thing to arrive here, and it must not be answered as though the
// same thing had been established as for a file with a manifest.
//
// Nothing here is printed unless it was measured. The size is gated on
// BytesRead and the row counts on CountsRead, because the zero value of a size is
// a 0-byte file and the zero value of a row count is an empty database — neither
// can stand for "not measured", and a run that reached neither says so.
func printVerifyReport(out io.Writer, rep memory.VerifyReport, runErr error) error {
	skipped := 0
	for _, c := range rep.Checks {
		if c.State == memory.VerifySkipped {
			skipped++
		}
	}
	var verdict string
	switch {
	case runErr != nil || len(rep.Failed()) > 0:
		verdict = "refusing"
	case skipped > 0:
		verdict = "checked"
	default:
		verdict = "verified"
	}

	headline := rep.Path
	// Each clause is gated on the flag for what it states, never on the value:
	// the zero value of a size is a 0-byte file and the zero value of a row count
	// is an empty database, so neither can stand for "not measured".
	if rep.BytesRead {
		headline += fmt.Sprintf(": %d bytes", rep.Bytes)
	}
	// One arm, not three: the counts are printed only where they were read, and
	// the condition is written once so there is no second path through this
	// switch that could print a count the report does not have.
	switch {
	case runErr != nil || !rep.CountsRead:
		headline += ", not checked"
	default:
		headline += ", " + rep.Counts.String()
	}
	if _, err := fmt.Fprintf(out, "%s %s\n", verdict, headline); err != nil {
		return err
	}
	for _, c := range rep.Checks {
		if _, err := fmt.Fprintf(out, "  %-16s %-8s %s\n", c.Name, c.State, c.Detail); err != nil {
			return err
		}
	}
	// The footer distinguishes the two ways a sidecar can be unusable, because
	// they are opposites and "no manifest" is wrong for one of them: a missing
	// sidecar is expected and merely limits what was checked, while one that is
	// there and unreadable is damage, and telling that reader to go look for a
	// file sitting right beside the snapshot is the "treating it as absent" this
	// command is built not to do.
	switch {
	case rep.ManifestPresent && !rep.HasManifest:
		if _, err := fmt.Fprintf(out, "  the manifest at %s is present but could not be read: %v\n",
			rep.ManifestPath, rep.ManifestErr); err != nil {
			return err
		}
	case !rep.ManifestPresent:
		// Once, at the bottom: the table above already says it in two rows, and
		// saying it a third time in prose would bury the line being looked for.
		if _, err := fmt.Fprintf(out, "  (no manifest at %s, so the hash and the recorded counts were not checked)\n",
			rep.ManifestPath); err != nil {
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
	if err := runExportCore(context.Background(), store, summary, os.Stderr, dest, opts.Project); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// runExportCore writes the artifact and reports what it wrote. On a file
// destination the file is created with the same 0600 width the database has,
// before any record is written, so a failure part-way leaves a file only the
// user can read.
//
// summary is where the run states what it did, and warn is where it names the
// records it could not write. They are separate writers because the artifact may
// BE stdout (`ghost export -`), and a warning that lands inside a JSONL stream
// corrupts it for the reader that is piping it straight into an import.
func runExportCore(ctx context.Context, store *memory.Store, summary, warn io.Writer, dest, projectFilter string) error {
	if dest == "-" {
		stats, err := portable.Export(ctx, store, os.Stdout, projectFilter)
		if err != nil {
			return err
		}
		if err := printExportSummary(summary, "-", stats); err != nil {
			return err
		}
		return reportSkippedRecords(warn, stats.Skipped)
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
	// From here the artifact is COMPLETE and importable, so the file is KEPT even
	// when records were left out of it. That is the whole difference from the
	// branch above: this artifact imports cleanly, it is just not the whole
	// store, and deleting it would destroy a working backup over a warning about
	// the rows it does not contain.
	if err := printExportSummary(summary, dest, stats); err != nil {
		return err
	}
	return reportSkippedRecords(warn, stats.Skipped)
}

// repairableKinds are the record kinds Ghost can delete SELECTIVELY, which is not
// every kind it exports. A project goes through `ghost project delete`; a memory
// through the `ghost_memory_delete` tool. A task and a decision have no such
// surface — no CLI subcommand, no MCP tool, and no DELETE against either table in
// internal/memory.
//
// "Selectively" is the whole distinction, and an earlier version of this comment
// got it wrong in both directions. `tasks.project_id` and `decisions.project_id`
// are both `REFERENCES projects(id) ON DELETE CASCADE` (schema.go), and
// Store.DeleteProject counts both, so `ghost project delete` DOES remove a task or
// a decision row — just never alone. The report therefore says "by itself" and
// names the blunt repair, because "cannot be removed through Ghost at all" would
// stop an operator looking for a repair that is one command away.
//
// And there is no top-level `ghost delete`: `case "delete"` sits inside
// `case "project":` in dispatchCommand, so the only spelling is
// `ghost project delete`. Naming a bare `delete` would send someone to a
// top-level usageError and exit 2.
var repairableKinds = map[string]bool{
	portable.TypeProject: true,
	portable.TypeMemory:  true,
}

// reportSkippedRecords names every record an export left out and returns an error
// when there was one, so the command exits non-zero and a backup script notices.
//
// The error mirrors the importer's: import counts its rejections and returns an
// error AFTER printing the per-record report, and this does the same for the rows
// it could not write. A count of what was written is not a count of what exists,
// and a partial export reported as a success is the failure mode worth spending
// an exit code on.
func reportSkippedRecords(out io.Writer, skipped []portable.SkippedRecord) error {
	if len(skipped) == 0 {
		return nil
	}
	var hasRepairable, hasUnrepairable, hasSecret bool
	for _, sk := range skipped {
		// The id through assemble.Token, the same renderer the import report uses
		// for the id it names: an id carrying a newline would forge a line on the
		// very report that exists to name it (#791). Such an id is exactly what
		// gets here, so this is not a precaution.
		//
		// The reason beside it is the importer's own message, and for a credential
		// refusal it names the FIELD and nothing else — so this line is safe to
		// print for the same reason the import report's is. The value itself is in
		// no SkippedRecord field and never reaches this writer.
		if _, err := fmt.Fprintf(out, "  ! left out: %s %s — %s\n", sk.Type, assemble.Token(sk.ID), sk.Reason); err != nil {
			return err
		}
		// A batch can hold both kinds, so the advice is collected rather than
		// decided from the first row: naming a project command for a task is the
		// mistake this exists to stop, and deciding from row one would do exactly
		// that to every task after a project.
		if repairableKinds[sk.Type] {
			hasRepairable = true
		} else {
			hasUnrepairable = true
		}
		if sk.Secret {
			hasSecret = true
		}
	}
	// The credential advice is printed FIRST and separately, because it is not an
	// alternative to the re-key advice below but a different thing entirely, and
	// an operator who read the re-key sentence first would conclude that a
	// credential-shaped row has to be DELETED to get rid of the value. It has not:
	// the row is fine apart from the one field, the field is the value, and
	// replacing the value with a pointer to where it lives is both the documented
	// policy and the fix that keeps the memory.
	//
	// The command is named per KIND, and the field lists inside it are DERIVED from
	// the tools' own argument structs (mcpserver.EditableFieldList) rather than
	// written here. The first version of this sentence transcribed them, and it was
	// wrong in four places: it promised `ghost_memory_update` could edit a memory's
	// agent and session_id (neither is an argument — the tool writes them from the
	// EDITING SESSION's provenance, and deliberately does not let a caller name
	// their own author) and that `ghost_task_update` could edit a task's title and
	// notes (it takes status, priority and description; a task's title is written at
	// insert and its notes only by ghost_task_complete). A user who followed it got
	// a rejected call, which is worse than no advice at all — the harm the delete
	// sentence below is careful to avoid by naming only commands that exist.
	//
	// A transcribe-and-hope list is the same mistake as a second refusal predicate,
	// and it drifted for the same reason. Deriving it means a field added to a tool
	// is a field this sentence names, and one removed stops being named.
	if hasSecret {
		// The paragraph, in three moves and no more: the refusal is by design, the
		// value is not printed, the fix is to point at where the value lives, and
		// the re-export is the LAST step rather than an afterthought — a corrected
		// row is still refused by the next export until the artifact is written
		// again, so an operator who edits the field and stops has changed nothing a
		// restore will read.
		if _, err := fmt.Fprintf(out, "  A credential-shaped field is refused on import BY DESIGN and the value is never stored — this report names the field, never the value. Replace the value with WHERE it lives and how to read it, never the value itself. %s Re-export afterwards: a corrected row is still refused until the artifact is written again.\n", secretRepairAdvice()); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(out, "  Ghost cannot re-key a row: memory_links, the recorded history and every `ghost history` read are attached to the id this store holds, so the row was left as it is and left out of the artifact.\n"); err != nil {
		return err
	}
	// Both advice sentences below are per-BATCH and each names only the kinds it
	// applies to, so a mixed batch gets both: an operator with a skipped project
	// still needs the command even when the same artifact also skipped a task.
	//
	// The repair is KIND-AWARE, because a command that cannot delete the row is
	// worse than no command at all: the operator is told what to type, types it,
	// and gets "project not found" or a no-op. Only a project and a memory can be
	// deleted SELECTIVELY — `ghost project delete <project>` (there is no
	// top-level `ghost delete`) and the `ghost_memory_delete` tool. A task and a
	// decision cannot, and for those two the sentence says so while naming the
	// blunt repair that does work.
	if hasRepairable {
		// "an id this build accepts" was the whole sentence when a shape refusal was
		// the only possible reason, and it is now too narrow: a project refused for
		// an empty name, or a memory refused for credential-shaped content, is not
		// fixed by re-saving it under a different id. So the sentence names the
		// reason the report already gave and points at the other one, which is the
		// edit the field needs.
		phrase := "delete the row and re-save it under an id this build accepts"
		if hasSecret {
			phrase = "delete the row and re-save it (for a credential-shaped field, editing the field is usually what you want instead — see above)"
		}
		if _, err := fmt.Fprintf(out, "  To include it, %s: `ghost project delete <project>` drops that project and every row under it, and a memory goes through the ghost_memory_delete tool.\n", phrase); err != nil {
			return err
		}
	}
	if hasUnrepairable {
		unrepairable := []string{}
		for _, kind := range []string{portable.TypeTask, portable.TypeDecision} {
			for _, sk := range skipped {
				if sk.Type == kind {
					unrepairable = append(unrepairable, kind)
					break
				}
			}
		}
		if _, err := fmt.Fprintf(out, "  Ghost has NO delete surface for a %s BY ITSELF: no `ghost %s delete`, no tool for it, and no DELETE against the table, so removing that row on its own means editing the database directly. `ghost project delete <project>` does remove it, along with every other row in that project — the blunt repair is available, it just is not selective. Until then the row stays in the store and out of every artifact.\n",
			strings.Join(unrepairable, " or a "), unrepairable[len(unrepairable)-1]); err != nil {
			return err
		}
	}
	// "because this build cannot import them" was accurate while a shape refusal was
	// the only possible reason and is too narrow now: the exporter applies the
	// importer's own predicates (#813), so a record is left out for exactly the
	// reasons `ghost import` would give — which is the whole point, and is what
	// the sentence has to say rather than implying a Ghost quirk.
	why := "because `ghost import` would refuse them"
	if hasSecret {
		why = "because `ghost import` refuses them — the credential guard by design"
	}
	return fmt.Errorf("%s left out of this artifact, %s — they are named above, and the artifact is complete for every other record", countLabel(len(skipped)), why)
}

// secretFieldFixers is the field-to-tool mapping the credential advice is built
// from, and it is keyed by the FIELD rather than by the record kind — because a
// field is what the `!` line above named, so it is what the operator is holding.
//
// The keys are the column names `memory.CheckImported*` reports, which is what
// `portable.SkippedRecord.Reason` carries, so the advice and the refusal name the
// same thing. Every VALUE is checked against the tool's real arguments by
// TestEveryCredentialFieldIsEditableByTheToolTheAdviceNames — the test that exists
// because the first version of this map was a transcription and was wrong in four
// places.
//
// It is deliberately short: only the fields that are BOTH credential-guarded and
// tool-editable. A field in neither list is a bug, which
// TestTheCredentialAdviceCoversEveryGuardedField is what catches.
var secretFieldFixers = map[string]string{
	"content":     "ghost_memory_update",
	"tags":        "ghost_memory_update",
	"source_ref":  "ghost_memory_update",
	"description": "ghost_task_update",
	// notes is writable, but only by ghost_task_complete — which also marks the
	// task done, so secretRepairAdvice names it with that consequence rather than
	// as a neutral edit.
	"notes": "ghost_task_complete",
}

// secretUnfixableFields are the credential-guarded fields NO tool can write, named
// explicitly rather than left out. Silence would be the wrong default: the operator
// is holding a row the report just refused, is told to edit the field, and would
// find the advice covers every field they can see and not theirs.
//
// Each is unwritable for a stated reason, and the reasons differ:
//
//   - a memory's `agent` and `session_id`. The update tool DOES write both columns,
//     but from the EDITING SESSION's provenance (provenanceFor), because a caller
//     must not be able to name their own author. So a credential in a pre-#656
//     row's agent cannot be edited away through any tool — the tool would replace
//     it with the editing session's own token, and the honest statement is that
//     the column is not the caller's to set.
//   - a task's `title`. Written at insert and by no update: ghost_task_update
//     takes status, priority and description, and ghost_task_complete writes notes
//     and completed_at.
//   - a decision's `title`, `decision`, `rationale` and `alternatives`. There is no
//     decision update tool of any kind.
//   - a project's `name` and `path`. There is no project update tool either — a
//     project is created, bound, merged or deleted, never edited in place.
//
// These are exactly the fields the transcription named a tool for, which is why
// this list is spelled out rather than derived from the absence of an entry.
var secretUnfixableFields = []string{
	"a memory's agent and session_id",
	"a task's title",
	"a decision's title, decision, rationale and alternatives",
	"a project's name and path",
}

// editableBy reports whether a tool really takes a field, read from the tool's own
// argument struct rather than from the mapping beside it. It is the check that makes
// secretFieldFixers safe to keep, and it is why the sentence is built from the
// derived answer instead of from the map's own key.
func editableBy(tool, field string) bool {
	for _, f := range mcpserver.EditableFields(tool) {
		if f == field {
			return true
		}
	}
	return false
}

// secretRepairAdvice is the whole credential paragraph's tail: which tool edits
// which field, and — stated first, because it is the common case for a field with
// no tool — what to do when nothing can.
//
// The order is deliberate. A reader looking for their own field should find it in
// the first sentence, and the fields no tool can write are the minority and the
// slower path. Every field list in the output comes from
// mcpserver.EditableFieldList, which reflects over the tool's own argument struct,
// so a sentence here cannot promise a field a tool does not take.
func secretRepairAdvice() string {
	// Grouped BY TOOL rather than listed field by field. The first version said
	// "`content` through `ghost_memory_update` edits category, confidence,
	// content, …" once per field, which is nine repetitions of the same clause and
	// a reader cannot tell which repetition applies to the field they are holding.
	// A tool takes several of the fields, so one clause per tool says it once.
	byTool := map[string][]string{}
	for field, tool := range secretFieldFixers {
		if !editableBy(tool, field) {
			// The mapping has drifted from the tool it names, which is the failure
			// the review found. Better to move the field to the unwritable
			// sentence than to name a tool that will reject the call, and
			// TestEveryCredentialFieldIsEditableByTheToolTheAdviceNames fails on
			// this too — but the report must still be true if a test is ever not
			// run.
			byTool[""] = append(byTool[""], field)
			continue
		}
		byTool[tool] = append(byTool[tool], field)
	}

	tools := make([]string, 0, len(byTool))
	for tool := range byTool {
		if tool != "" {
			tools = append(tools, tool)
		}
	}
	sort.Strings(tools)

	var b strings.Builder
	if len(tools) > 0 {
		b.WriteString("Edit it in place: ")
		for i, tool := range tools {
			if i > 0 {
				b.WriteString("; ")
			}
			// The tool's own field list, in its own order, narrowed to the fields
			// this report is about — a tool takes more arguments than there are
			// credential-guarded fields, and naming the unrelated ones is noise.
			fmt.Fprintf(&b, "%s edits %s", mcpserver.EditableFieldList(tool), fieldListInToolOrder(tool, byTool[tool]))
			// ghost_task_complete is not a neutral edit: it marks the task done.
			// Saying "notes" without that would send someone to close a task to
			// clear a token out of it.
			if tool == "ghost_task_complete" {
				b.WriteString(" — which also marks the task done")
			}
		}
		b.WriteString(".")
	}
	if b.Len() > 0 {
		b.WriteString(" ")
	}
	// The unwritable half, which is the half the review was about. Both sources are
	// named: the fields a tool was expected to edit and cannot, and the ones that
	// never had a tool. The route for both is the same and it is the only one.
	unfixable := append([]string{}, secretUnfixableFields...)
	if drifted := byTool[""]; len(drifted) > 0 {
		sort.Strings(drifted)
		unfixable = append(unfixable, drifted...)
	}
	fmt.Fprintf(&b, "No tool can edit %s — those columns are written only by a save, a create or a restore, so clearing one means editing the database directly, or deleting the row as below.",
		strings.Join(unfixable, ", "))
	return b.String()
}

// fieldListInToolOrder renders the subset of a tool's fields that this report is
// about, in the tool's own order so the reader meets them as the tool's schema
// states them.
func fieldListInToolOrder(tool string, fields []string) string {
	want := make(map[string]bool, len(fields))
	for _, f := range fields {
		want[f] = true
	}
	var kept []string
	for _, f := range mcpserver.EditableFields(tool) {
		if want[f] {
			kept = append(kept, f)
		}
	}
	return mcpserver.HumanFieldList(kept)
}

// printExportSummary states where the artifact is and what it holds, and says in
// the SAME line when it does not hold everything — because a headline that counts
// only what was written reads as a count of the store, which is the claim a
// backup script is about to act on.
func printExportSummary(out io.Writer, path string, stats portable.Stats) error {
	headline := fmt.Sprintf("exported %s to %s",
		pluralRecords(stats.Projects, stats.Memories, stats.Tasks, stats.Decisions), path)
	if len(stats.Skipped) > 0 {
		headline += fmt.Sprintf(" — %s left out, see below", countLabel(len(stats.Skipped)))
	}
	_, err := fmt.Fprintln(out, headline)
	return err
}

// printImportReport states what an import did, or — for a dry run — what it
// would do. The headline leads with the count because that is what a reader
// printRecordLine reports one record's outcome, and is what makes a dry run
// reviewable: the reader sees each memory's content prefix, not just a count.
func printRecordLine(out io.Writer, r portable.RecordResult) error {
	detail := r.Detail
	// The id through assemble.Token, once, for every line it reaches. A record's
	// id is a key this line prints with no quoting of its own, so an id carrying a
	// newline forges a report line here — the same class as the rendered memory
	// line, on a surface a store written before the import shape check existed
	// can still reach (#791). A well-formed id is written bare, so the report is
	// byte-identical for every ordinary artifact.
	//
	// It is the SAME id printed by portable's labelOrID, and both render it
	// through the one function rather than each choosing a spelling, because a
	// report whose headline and whose per-record lines disagree about how an id
	// looks is worse than either choice.
	id := assemble.Token(r.ID)
	// A credential refusal prints the id and nothing else. portable's
	// safeDetail already reduces the detail to the id for any record whose text
	// holds one — see the note there — and this is the second line for a
	// RecordResult that reached this function with a detail set by something
	// other than that helper. The format still reaches the reader through
	// report.Errors, which is the part needed to fix the artifact.
	var refused *memory.SecretContentError
	if errors.As(r.Error, &refused) {
		detail = id
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
		detail = id
	default:
		// Quoted on BOTH arms, not only when there is an id. The old switch
		// reached `%q` only through `case r.ID != ""`, so a RecordResult with no
		// id and a non-empty detail fell through both arms and reached Fprintf
		// with no quoting at all — and `detail` holds a memory's content, which
		// may hold a carriage return, which a terminal reads as "return to
		// column 0 and overwrite". A created memory always has an id, so this
		// was not reachable from `ghost import`; the switch had no reason to
		// depend on that, though, and a record type added later need not (#791).
		//
		// `%q` is also what neutralises a CR reaching here from a store written
		// before the preview cut at one — defence in depth, not the reason for
		// the arm.
		if r.ID != "" {
			detail = fmt.Sprintf("%q (%s)", detail, id)
		} else {
			detail = strconv.Quote(detail)
		}
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
