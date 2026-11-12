package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/mcpserver"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/portable"
)

const (
	backupUsage = `Usage: ghost backup [--out <path>]
       ghost backup verify <file>

Writes a consistent snapshot of the memory database while Ghost is running.

  --out <path>   Where to write it. Default: a timestamped file beside the
                 database, in the Ghost data directory. The path must not
                 already exist — an existing backup is never replaced. An EMPTY
                 value is refused, not read as the default: --out= is a script
                 with an unset variable, and the default is the flag left off.

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

  --project <name>   Export one project, matched by name or id exactly. A name
                     that matches nothing is an error, and so is a name that
                     matches MORE than one project (the ids are named). An exact
                     id is matched first and selects exactly one project. Default:
                     every project. An EMPTY value is refused rather than read as
                     the default — --project= is a script with an unset variable,
                     and the way to export everything is to leave the flag off.
                     Naming the flag twice is refused too, whatever the values are.
  --out <path>       Where to write it. Default: a timestamped .jsonl beside
                     the database. Use "-" for standard output. An EMPTY value
                     is refused rather than read as the default — --out= is a
                     script with an unset variable, and the way to the default
                     is to leave the flag off.

Embeddings are not exported: they are derived from content by a local model and
the embedding worker rebuilds them. The ` + "`related`" + ` edges of the memory graph are not exported
either — they are derived the same way, and the linking worker recomputes them
after an import. The ` + "`supersedes`" + ` and ` + "`causes`" + ` edges ARE exported: each one is a
classifier's judgement about a pair of memories, and nothing recomputes it, so
an artifact that dropped them would restore a store in which a memory a
` + "`supersedes`" + ` edge had demoted was no longer demoted. An edge travels only when both
its endpoints are memories the artifact carries; one whose endpoint is missing
is left out, and the export says how many.

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
  --trust-provenance   Keep each memory's own source, pin state and retention
                       tier. Without it every imported memory is stamped
                       "onboarding", unpinned and set to the durable "project"
                       tier, so a file from somewhere else cannot plant rows
                       that read as your own words or as Ghost's shipped rules,
                       and cannot plant a keep-forever tier no lifecycle pass
                       may take back. Pass it when the artifact is your own
                       export.

A record whose id is already in the database is skipped, never overwritten: the
artifact is the older of the two copies, so overwriting would restore stale data
over live data. This also makes re-running an import always safe, which is how
you repair a run that rejected a record.

The ` + "`supersedes`" + ` and ` + "`causes`" + ` edges of the memory graph are imported with the
memories they join, and an edge naming a memory the artifact does not carry is
rejected and named. An edge the store already holds is left alone, and an edge
the store holds WITHDRAWN stays withdrawn — an import is not a claim about a
withdrawal.

Imported memory content is held to the same length cap and the same validation
as a normal save, so an artifact from another machine cannot write a row Ghost
would refuse to produce. A file whose schema version this build does not read is
refused outright.
`
)

type (
	backupOptions struct {
		Out string
	}
	exportOptions struct {
		Project string
		Out     string
	}
	importOptions struct {
		File            string
		Apply           bool
		TrustProvenance bool
	}
)

func parseBackupArgs(args []string) (backupOptions, error) {
	var opts backupOptions
	outSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag --out needs a value")
			}
			i++
			if outSeen {
				return opts, fmt.Errorf("--out was given twice (%s and %s)",
					memory.RefusalArg("out", opts.Out), memory.RefusalArg("out", args[i]))
			}
			if args[i] == "" {
				return opts, errors.New("--out requires a value")
			}
			opts.Out = args[i]
			outSeen = true
		case strings.HasPrefix(arg, "--out="):
			v := strings.TrimPrefix(arg, "--out=")
			if outSeen {
				return opts, fmt.Errorf("--out was given twice (%s and %s)",
					memory.RefusalArg("out", opts.Out), memory.RefusalArg("out", v))
			}
			if v == "" {
				return opts, errors.New("--out requires a value")
			}
			opts.Out = v
			outSeen = true
		default:
			return opts, fmt.Errorf("unknown argument %s", memory.RefusalArg("argument", arg))
		}
	}
	return opts, nil
}

func parseBackupVerifyArgs(args []string) (string, error) {
	var positional []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return "", fmt.Errorf("unknown flag %s", memory.RefusalArg("flag", arg))
		}
		positional = append(positional, arg)
	}
	if len(positional) != 1 {
		return "", fmt.Errorf("expected exactly one backup file, got %d", len(positional))
	}
	return positional[0], nil
}

func parseExportArgs(args []string) (exportOptions, error) {
	var opts exportOptions
	projectSeen := false
	outSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag --out needs a value")
			}
			i++
			if outSeen {
				return opts, fmt.Errorf("--out was given twice (%s and %s)",
					memory.RefusalArg("out", opts.Out), memory.RefusalArg("out", args[i]))
			}
			if args[i] == "" {
				return opts, errors.New("--out requires a value")
			}
			opts.Out = args[i]
			outSeen = true
		case strings.HasPrefix(arg, "--out="):
			v := strings.TrimPrefix(arg, "--out=")
			if outSeen {
				return opts, fmt.Errorf("--out was given twice (%s and %s)",
					memory.RefusalArg("out", opts.Out), memory.RefusalArg("out", v))
			}
			if v == "" {
				return opts, errors.New("--out requires a value")
			}
			opts.Out = v
			outSeen = true
		case arg == "--project":
			if i+1 >= len(args) {
				return opts, errors.New("--project requires a value")
			}
			if projectSeen {
				return opts, errors.New("expected exactly one project")
			}
			if args[i+1] == "" {
				return opts, errors.New("--project requires a value")
			}
			opts.Project = args[i+1]
			projectSeen = true
			i++
		case strings.HasPrefix(arg, "--project="):
			if projectSeen {
				return opts, errors.New("expected exactly one project")
			}
			v := strings.TrimPrefix(arg, "--project=")
			if v == "" {
				return opts, errors.New("--project requires a value")
			}
			opts.Project = v
			projectSeen = true
		default:
			return opts, fmt.Errorf("unknown argument %s", memory.RefusalArg("argument", arg))
		}
	}
	return opts, nil
}

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
			return opts, fmt.Errorf("unknown flag %s", memory.RefusalArg("flag", arg))
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

const transferTimeLayout = "20060102T150405Z"

func backupDefaultPath(dbPath, out string, at time.Time) (string, error) {
	if out != "" {
		return out, nil
	}
	if dbPath == "" {
		return "", fmt.Errorf("cannot locate the database to derive a default backup path from")
	}
	return memory.BackupFileName(dbPath, at.UTC()), nil
}

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

func runBackupCore(ctx context.Context, store *memory.Store, out io.Writer, dest string) error {
	res, err := store.Backup(ctx, dest)
	if err != nil {
		return err
	}
	return printBackupReport(out, res)
}

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
	if rep.BytesRead {
		headline += fmt.Sprintf(": %d bytes", rep.Bytes)
	}
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
	switch {
	case rep.ManifestPresent && !rep.HasManifest:
		if _, err := fmt.Fprintf(out, "  the manifest at %s is present but could not be read: %v\n",
			rep.ManifestPath, rep.ManifestErr); err != nil {
			return err
		}
	case !rep.ManifestPresent:
		if _, err := fmt.Fprintf(out, "  (no manifest at %s, so the hash and the recorded counts were not checked)\n",
			rep.ManifestPath); err != nil {
			return err
		}
	}
	return nil
}

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

	store, err := openReadOnlyTransferStore(dataDir, exportOperation)
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

var exportPublish = func(tmp, dest string) error {
	if err := exportLink(tmp, dest); err == nil {
		_ = os.Remove(tmp)
		return nil
	} else if os.IsExist(err) {
		return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
	}
	return exportPublishByCopy(tmp, dest)
}

var exportLink = os.Link

func exportPublishByCopy(tmp, dest string) error {
	src, err := os.Open(tmp)
	if err != nil {
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	defer src.Close() //nolint:errcheck
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
		}
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	_, err = io.Copy(out, src)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("publish %s: %w", dest, err)
	}
	_ = os.Remove(tmp)
	return nil
}

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
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("refusing to overwrite an existing file: %s", dest)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("check %s: %w", dest, err)
	}
	f, err := os.CreateTemp(filepath.Dir(dest), ".ghost-export-*.tmp")
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	tmp := f.Name()
	stats, exportErr := portable.Export(ctx, store, f, projectFilter)
	if exportErr == nil {
		if err := f.Sync(); err != nil {
			exportErr = fmt.Errorf("sync %s: %w", dest, err)
		}
	}
	if err := f.Close(); err != nil && exportErr == nil {
		exportErr = fmt.Errorf("close %s: %w", dest, err)
	}
	if exportErr == nil {
		exportErr = exportPublish(tmp, dest)
	}
	if exportErr != nil {
		if rmErr := os.Remove(tmp); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("%w (and the temporary file %s could not be removed: %v)", exportErr, tmp, rmErr)
		}
		return exportErr
	}
	if err := printExportSummary(summary, dest, stats); err != nil {
		return err
	}
	return reportSkippedRecords(warn, stats.Skipped)
}

var repairableKinds = map[string]bool{
	portable.TypeProject: true,
	portable.TypeMemory:  true,
}

const validityAdvice = "  A memory left out for its valid_from, valid_until, verified_at or confidence keeps its id and can be repaired in place: replace the value with ghost_memory_update (an RFC 3339 instant or a date, a window that ends after it starts, a confidence from 0 to 1), then re-export. The tool can replace a stamp but not remove one, so to clear a stamp, delete the row with ghost_memory_delete and re-save it without it. A repaired row may be named again if its evidence records hold a stale copy of the same claim — the evidence row is not updated by ghost_memory_update, so the supported route is the same delete-and-save-again the evidence advice names, with its stated cost."

const evidenceAdvice = "  A memory left out because one of its evidence records carries a stamp no accepted form reads or a rating outside 0 to 1 keeps its id, but no tool edits an evidence record: they are appended by a save, an import and a reflect. The repair is to delete the row with ghost_memory_delete and re-save it, which loses its links, history and evidence, and then re-export."

const linkAdvice = "  An edge left out is a claim this store still holds: the artifact does not carry it, so the claim does not travel with this export, and the demotion it causes stays here. The field that stopped it is named above, and no tool edits an edge — `ghost supersede <project> --withdraw <source> <target> --apply` (or the ghost_link_withdraw tool) withdraws a claim you do not want, and correcting the field itself is an edit of the database."

func reportSkippedRecords(out io.Writer, skipped []portable.SkippedRecord) error {
	if len(skipped) == 0 {
		return nil
	}
	var hasRepairable, hasUnrepairable, hasSecret, hasScope, hasScopeChar, hasOrphan, hasValidity, hasEvidence, hasLink bool
	for _, sk := range skipped {
		label := assemble.Token(sk.ID)
		if sk.Type == portable.TypeLink && sk.Detail != "" {
			label = sk.Detail + " (" + assemble.Token(sk.ID) + ")"
		}
		if _, err := fmt.Fprintf(out, "  ! left out: %s %s — %s\n", sk.Type, label, sk.Reason); err != nil {
			return err
		}
		if sk.Orphan {
			hasOrphan = true
			continue
		}
		if sk.Validity {
			hasValidity = true
			continue
		}
		if sk.Evidence {
			hasEvidence = true
			continue
		}
		if sk.Type == portable.TypeLink {
			hasLink = true
			continue
		}
		if repairableKinds[sk.Type] {
			hasRepairable = true
		} else {
			hasUnrepairable = true
		}
		if sk.Secret {
			hasSecret = true
		}
		if sk.Scope {
			hasScope = true
		}
		if sk.ScopeChar {
			hasScopeChar = true
		}
	}
	if hasSecret {
		if _, err := fmt.Fprintln(out, "  A credential-shaped field is refused on import BY DESIGN and the value is never stored — this report names the field, never the value. Replace the value with WHERE it lives and how to read it, never the value itself, then re-export: a corrected row is still refused until the artifact is written again."); err != nil {
			return err
		}
		if err := printSecretFixes(out); err != nil {
			return err
		}
	}
	if hasRepairable || hasUnrepairable {
		if _, err := fmt.Fprintf(out, "  Ghost cannot re-key a row: memory_links, the recorded history and every `ghost history` read are attached to the id this store holds, so the row was left as it is and left out of the artifact.\n"); err != nil {
			return err
		}
	}
	if hasValidity {
		if _, err := fmt.Fprintln(out, validityAdvice); err != nil {
			return err
		}
	}
	if hasEvidence {
		if _, err := fmt.Fprintln(out, evidenceAdvice); err != nil {
			return err
		}
	}
	if hasOrphan {
		if _, err := fmt.Fprintln(out, "  A record left out because its project was left out is not refused for anything of its own, and deleting it would drop the only copy: fix the project and re-export, and the record is written with it."); err != nil {
			return err
		}
	}
	if hasLink {
		if _, err := fmt.Fprintln(out, linkAdvice); err != nil {
			return err
		}
	}
	if hasRepairable {
		phrase := "delete the row and re-save it under an id this build accepts"
		if hasSecret {
			phrase = "delete the row and re-save it (for a credential-shaped field, editing the field is usually what you want instead — see above)"
		}
		if hasScope && !hasSecret {
			phrase = fmt.Sprintf("delete the row and re-save it with a shorter scope (at most %d keys, each key at most %d bytes and each value at most %d bytes)", memory.MaxScopeKeys, memory.MaxScopeKeyLen, memory.MaxScopeValueLen)
		}
		if hasScope && hasScopeChar && !hasSecret {
			phrase = fmt.Sprintf("delete the row and re-save it with a shorter scope (at most %d keys, each key at most %d bytes and each value at most %d bytes) containing no blank, NUL, control, FORMAT or line-separator characters", memory.MaxScopeKeys, memory.MaxScopeKeyLen, memory.MaxScopeValueLen)
		} else if hasScopeChar && !hasSecret {
			phrase = "delete the row and re-save it with a scope containing no blank, NUL, control, FORMAT or line-separator characters"
		}
		if _, err := fmt.Fprintf(out, "  To include it, %s: `ghost project delete <project>` drops that project and every row under it, and a memory goes through the ghost_memory_delete tool.\n", phrase); err != nil {
			return err
		}
	}
	if hasScope && hasSecret {
		if _, err := fmt.Fprintf(out, "  A scope over its bound is a separate fix: delete the row and re-save it with a shorter scope (at most %d keys, each key at most %d bytes and each value at most %d bytes).\n", memory.MaxScopeKeys, memory.MaxScopeKeyLen, memory.MaxScopeValueLen); err != nil {
			return err
		}
	}
	if hasScopeChar && hasSecret {
		if _, err := fmt.Fprintf(out, "  A scope with invalid characters is a separate fix: delete the row and re-save it with a scope containing no blank, NUL, control, FORMAT or line-separator characters.\n"); err != nil {
			return err
		}
	}
	if hasUnrepairable {
		unrepairable := []string{}
		for _, kind := range []string{portable.TypeTask, portable.TypeDecision} {
			for _, sk := range skipped {
				if sk.Type == kind && !sk.Orphan {
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
	why := "because `ghost import` would refuse them"
	if hasSecret {
		why = "because `ghost import` refuses them — the credential guard by design"
	}
	return fmt.Errorf("%s left out of this artifact, %s — they are named above, and the artifact is complete for every other record", countLabel(len(skipped)), why)
}

var secretFieldFixers = map[string]string{
	"content":     "ghost_memory_update",
	"tags":        "ghost_memory_update",
	"source_ref":  "ghost_memory_update",
	"description": "ghost_task_update",
	"notes":       "ghost_task_complete",
}

var secretUnfixableRoutes = []string{
	"a memory's agent and session_id — the memory update overwrites both with the EDITING SESSION's identity, because a caller must not be able to name its own author, so it cannot clear a value a pre-#656 row already holds; edit the row's agent and session_id in the database directly",

	"a memory's evidence agent, session_id and source_ref — no tool writes an evidence row; they are appended by a save, an import and a reflect, and the only surface that reaches one is `ghost history purge <memory-id>`, which erases the memory's whole recorded history rather than editing a field",

	"a memory's scope keys and values (scope_key and scope) — written when the memory is saved; `ghost_memory_update` takes no scope, so the only routes are to delete the memory with ghost_memory_delete and save it again with a clean scope, or to edit the row's scope in the database directly",

	"a task's title — written when the task is created; `ghost_task_update` takes status, priority and description only, so the only route is the database directly",

	"a decision's title, decision, rationale, tags and alternatives — there is no decision update tool of any kind, and `ghost_memory_update` edits a MEMORY's tags rather than a decision's, so the only route is the database directly",

	"a project's path — `ghost project bind <project-id> <checkout-directory>` rewrites it, and note that `ghost project delete` is NOT the fix here: it cascades away every memory in the project",
	"a project's name — nothing rewrites it; a project is created, bound, merged or deleted, never renamed, so the only route is the database directly",
}

func editableBy(tool, field string) bool {
	for _, f := range mcpserver.EditableFields(tool) {
		if f == field {
			return true
		}
	}
	return false
}

func printSecretFixes(out io.Writer) error {
	byTool := map[string][]string{}
	var drifted []string
	for field, tool := range secretFieldFixers {
		if !editableBy(tool, field) {
			drifted = append(drifted, field)
			continue
		}
		byTool[tool] = append(byTool[tool], field)
	}

	tools := make([]string, 0, len(byTool))
	for tool := range byTool {
		tools = append(tools, tool)
	}
	sort.Strings(tools)
	for _, tool := range tools {
		line := fmt.Sprintf("  · %s edits %s", mcpserver.EditableFieldList(tool), fieldListInToolOrder(tool, byTool[tool]))
		if tool == "ghost_task_complete" {
			line += " — which also marks the task done"
		}
		if tool == "ghost_memory_update" && slices.Contains(byTool[tool], "tags") {
			line += " — on a memory row; a decision's tags have no tool (see below)"
		}
		if _, err := fmt.Fprintln(out, line); err != nil {
			return err
		}
	}
	if len(drifted) > 0 {
		sort.Strings(drifted)
		if _, err := fmt.Fprintf(out, "  · %s — the advice expected a tool for %s and it no longer takes %s, so treat %s as unwritable\n",
			strings.Join(drifted, " and "), mapPlural(len(drifted)), mapPlural(len(drifted)), mapPlural(len(drifted))); err != nil {
			return err
		}
	}
	for _, route := range secretUnfixableRoutes {
		if _, err := fmt.Fprintf(out, "  · No tool edits %s\n", route); err != nil {
			return err
		}
	}
	return nil
}

func mapPlural(n int) string {
	if n == 1 {
		return "1 field"
	}
	return fmt.Sprintf("%d fields", n)
}

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

func printExportSummary(out io.Writer, path string, stats portable.Stats) error {
	headline := fmt.Sprintf("exported %s to %s",
		pluralRecords(stats.Projects, stats.Memories, stats.Links, stats.Tasks, stats.Decisions), path)
	if len(stats.Skipped) > 0 {
		headline += fmt.Sprintf(" — %s left out, see below", countLabel(len(stats.Skipped)))
	}
	if _, err := fmt.Fprintln(out, headline); err != nil {
		return err
	}
	if stats.LinksLeftOut > 0 {
		if _, err := fmt.Fprintf(out, "  %s left out: an endpoint of the edge is not a memory this artifact carries, so the edge did not travel\n",
			pluralCount(stats.LinksLeftOut, "link", "links")); err != nil {
			return err
		}
	}
	return nil
}

func printRecordLine(out io.Writer, r portable.RecordResult) error {
	detail := r.Detail
	id := assemble.Token(r.ID)
	var refused *memory.SecretContentError
	if errors.As(r.Error, &refused) {
		detail = id
	}
	isCreate := r.Action == portable.ActionCreate
	isMemory := r.Type == portable.TypeMemory
	if isCreate && isMemory && r.Downgraded {
		detail = strings.TrimSpace(detail) + " (provenance downgraded to " + memory.DowngradedSource + ", unpinned, retention " + memory.RetentionProject + ")"
	}
	if isCreate && len(r.Clamped) > 0 {
		detail = strings.TrimSpace(detail) + " …(truncated at the cap: " + strings.Join(r.Clamped, ", ") + ")"
	}
	if isCreate && isMemory && !r.Downgraded && len(r.Clamped) == 0 {
		detail = strings.TrimSpace(detail) + " (provenance kept as exported)"
	}
	switch {
	case detail == "":
		detail = id
	default:
		if r.ID != "" {
			detail = fmt.Sprintf("%q (%s)", detail, id)
		} else {
			detail = strconv.Quote(detail)
		}
	}
	if r.DroppedRef != "" {
		field := "blocked_by"
		if r.Type == portable.TypeDecision {
			field = "superseded_by"
		}
		why := "that " + r.Type + " was rejected"
		switch r.DropReason {
		case portable.DropAbsent:
			why = "not in this artifact or this store"
		case portable.DropCycle:
			why = "part of a cycle"
		}
		detail += fmt.Sprintf(" (%s %s dropped: %s)", field, assemble.Token(r.DroppedRef), why)
	}
	_, err := fmt.Fprintf(out, "  %-7s %-9s line %d  %s\n", r.Action, r.Type+":", r.Line, detail)
	return err
}

func printImportReport(out io.Writer, path string, report portable.ImportReport, opts portable.ImportOptions) error {
	created := report.Created["project"] + report.Created["memory"] +
		report.Created["link"] + report.Created["task"] + report.Created["decision"]
	skipped := report.Skipped["project"] + report.Skipped["memory"] +
		report.Skipped["link"] + report.Skipped["task"] + report.Skipped["decision"]
	summary := pluralRecords(report.Created["project"], report.Created["memory"],
		report.Created["link"], report.Created["task"], report.Created["decision"])

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
	if report.Created["memory"] > 0 {
		note := "provenance and retention downgraded to " + memory.DowngradedSource + ", unpinned and " + memory.RetentionProject + " — pass --trust-provenance to keep the artifact's own"
		if opts.TrustProvenance {
			note = "provenance and retention kept as the artifact states them (--trust-provenance)"
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

func runImport() {
	opts, err := parseImportArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, importUsage)
		os.Exit(1)
	}

	artifact, store, err := openImportRun(opts.File, opts.Apply)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	defer store.Close() //nolint:errcheck

	if err := runImportReader(context.Background(), store, opts.File, artifact, importOptionsFrom(opts), os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func openImportRun(path string, apply bool) (*portable.Artifact, *memory.Store, error) {
	a, err := openImportArtifact(path)
	if err != nil {
		return nil, nil, err
	}
	store, err := openImportStore(apply)
	if err != nil {
		return nil, nil, err
	}
	return a, store, nil
}

func openImportArtifact(path string) (*portable.Artifact, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("no artifact at %s — write one with ghost export", path)
		}
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck
	return portable.Parse(f)
}

func importOptionsFrom(opts importOptions) portable.ImportOptions {
	return portable.ImportOptions{Apply: opts.Apply, TrustProvenance: opts.TrustProvenance}
}

func runImportCore(ctx context.Context, store *memory.Store, path string, opts portable.ImportOptions, out io.Writer) error {
	a, err := openImportArtifact(path)
	if err != nil {
		return err
	}
	return runImportReader(ctx, store, path, a, opts, out)
}

func runImportReader(ctx context.Context, store *memory.Store, path string, a *portable.Artifact, opts portable.ImportOptions, out io.Writer) error {
	report, err := a.Import(ctx, store, opts, func(r portable.RecordResult) {
		_ = printRecordLine(out, r)
	})
	if err != nil {
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

func pluralRecords(projects, memories, links, tasks, decisions int) string {
	parts := []string{
		pluralCount(projects, "project", "projects"),
		pluralCount(memories, "memory", "memories"),
		pluralCount(links, "link", "links"),
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

func countLabel(n int) string {
	if n == 1 {
		return "1 record"
	}
	return fmt.Sprintf("%d records", n)
}

func pluralCount(n int, singular, plural string) string {
	if n == 0 {
		return ""
	}
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}
