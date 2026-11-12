package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
	"github.com/wcatz/ghost/internal/secret"
)

type historyOptions struct {
	MemoryID string
	Limit    int
	JSON     bool
	Purge    bool
}

func parseHistoryArgs(args []string) (historyOptions, error) {
	var opts historyOptions
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--limit":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			n, err := parseHistoryLimit(args[i])
			if err != nil {
				return opts, err
			}
			opts.Limit = n
		case strings.HasPrefix(arg, "--limit="):
			n, err := parseHistoryLimit(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				return opts, err
			}
			opts.Limit = n
		case arg == "--json":
			opts.JSON = true
		case strings.HasPrefix(arg, "-"):
			return opts, fmt.Errorf("unknown flag %q", arg)
		case arg == "purge" && opts.MemoryID == "":
			opts.Purge = true
		default:
			if opts.MemoryID != "" {
				return opts, fmt.Errorf("history takes one memory id, got %s as well", memory.RefusalArg("id", arg))
			}
			opts.MemoryID = arg
		}
	}
	if opts.MemoryID == "" {
		return opts, fmt.Errorf("a memory id is required")
	}
	if opts.Purge && opts.Limit > 0 {
		return opts, fmt.Errorf("--limit has nothing to limit when purging: nothing is printed")
	}
	if opts.Purge && opts.JSON {
		return opts, fmt.Errorf("--json has nothing to print when purging: nothing survives")
	}
	return opts, nil
}

func historyCompactRequested(args []string) bool {
	return len(args) > 0 && args[0] == "compact"
}

func parseHistoryLimit(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("--limit needs a positive whole number, got %q", value)
	}
	return n, nil
}

const historyUsage = `Usage: ghost history <memory-ref> [--limit N] [--json]
       ghost history purge <memory-id>
       ghost history compact [--project <p>] [--before <t>] [--fix-updated-at] [--apply]

Prints one memory's append-only history: every insert, edit, reflection
rewrite, duplicate fold, resolve, supersession, restore, import and deletion,
oldest first, each with the content, category, importance, resolved_at and
source the memory held once that write landed.

  <memory-ref>  A full memory id, or 8 or more characters of one — the same
              eight characters every Ghost report prints, so an id copied out
              of one can be pasted here. A prefix naming more than one memory is
              refused and says which, rather than picking one. Resolution
              reaches DELETED memories too, since their history is what is left
              of them.

  --limit N   Print only the newest N entries (default: all the store keeps)
  --json      One JSON object per entry, for scripting

  purge       Erase the memory AND every recorded version of it, including any
              reflection snapshot that could restore the row. This is the
              redaction path: the history deliberately keeps the text a memory
              used to hold, so deleting a memory that contained a credential
              leaves that credential in the database FILE unless it is purged.
              Printing is not redaction and this command does not pretend to be
              one: an entry whose text holds a credential-shaped value prints as
              '<withheld: format, category, bytes>' — or, for the folded-in text
              a history row records no category for, '<withheld: format, bytes>'
              — in the human and the --json form alike. The row, its history and its
              snapshots go in one transaction, so none can survive the others. A
              memory that is ALREADY deleted is handled here too: its recorded
              text is erased on its own, because the tombstone is the feature
              and a delete cannot reach it. Purge takes the WHOLE id and refuses
              a prefix: it erases recorded text for good, where a mistyped
              argument is not a message but an unprintable memory. Run
              'ghost history <prefix>' to see the full id a prefix names.

  compact     Repair what the pre-#727 reflect behaviour left in this store
              (#730). Until that fix landed, every applied reflection appended a
              byte-identical 'reflect' version for each memory it kept and set
              that memory's updated_at to the run's own time. New ones stopped
              being written; this removes the ones already here.

              A version row is removed ONLY when it records the same state as the
              row before it of the same memory, compared over every column a
              version stores: content, category, importance, resolved_at and
              source, AND it was recorded before the bound below. Six things
              always stay: a memory's FIRST version (the only statement of what it
              said), its NEWEST version (the statement of what it says now, and
              the same row the retention cap declines to trim), every event that
              records a claim the state does not (a tombstone, a supersede or its
              withdrawal, a resolve or its clearing, a merge, an import, a
              restore), any row naming another memory, every 'reflect' version
              except those — a retag filed as an update is a change somebody made
              on purpose and this table has no column for tags, so the recorded
              state cannot tell it from a no-op — and EVERY version of a memory
              that has since been deleted. A deleted memory has no live row, so a
              read of the past takes the age it measures from the version that
              answers it; removing one would change what that read computes, for a
              memory nobody can edit and nobody can restore. Its history is frozen
              the moment it is deleted, so this costs nothing.

                --project <p>   Compact one project only (id, name or path)
                --before <t>    Only consider versions recorded before <t>, a
                                2006-01-02 date or an RFC 3339 instant. The
                                default is 2026-09-28T17:14:07Z, the moment #727
                                reached main, because a current build writes a
                                byte-identical 'reflect' version of its own — a
                                consolidation merge carries the union of its
                                sources' tags, and the tags are not a column here —
                                and no state column can tell that from the damage.
                                Widen it only for a store whose clock is behind (a
                                restored backup, a copied database) and re-read the
                                dry run first. A bound PAST this one is warned about
                                on stderr, in a dry run as well as an apply, and the
                                default never warns. Whichever bound was used is
                                named in the report, because every count is a count
                                AT a bound.
                --fix-updated-at  Also move each live memory's updated_at back to
                                 the recorded_at of its ANCHOR — the newest version
                                 that does not itself REPEAT the version before it
                                 AND whose WRITER moved updated_at in the same
                                 statement that filed it: a save, an update, or a
                                 reflection, and nothing else.
                                 Applied only where a removable version sits ABOVE
                                 that anchor, since that version is the evidence a
                                 reflection run moved the stamp. A writer that
                                 changed state WITHOUT moving the stamp is not an
                                 anchor: 'ghost resolve' writes resolved_at and
                                 deliberately leaves updated_at alone, and folding a
                                 duplicate save writes importance and moves nothing,
                                 so trusting either as an anchor would set a stamp
                                 to a time the store had never held. Only ever
                                 BACKWARD, and a stamp no version explains is left
                                 alone. A memory with no such version at all has NO
                                 anchor — a memory from before this table existed
                                 whose next writer was an unresolve — so its stamp
                                 is left exactly as it is and the report counts it
                                 under "no recorded stamp write" rather than
                                 reporting nothing: its no-op flood is still
                                 removed, and a count of 0 fixed beside a store full
                                 of removed versions otherwise reads as a finished
                                 repair. Without the flag the redundant versions go
                                 and no stamp moves.

              Pass --apply and --fix-updated-at in the SAME run: the stamp repair
              needs the versions the deletion removes as its evidence, so a second
              run after an --apply has nothing left to act on.

              A dry run is the default and writes nothing; --apply writes. Either
              way the counts are per project, and a dry run reports exactly what
              the apply would do. Every row the run could not repair is named in
              the report rather than left to a count of zero to explain: a stamp no
              layout reads, and a stamp with no recorded write to restore it from.
              It refuses to run while a lifecycle run holds any of the projects'
              locks, it refuses a store a newer Ghost owns rather than repairing
              one, and it works in bounded transactions so it does not hold the
              write lock over a whole store's history.

              What a repair changes, and it is worth knowing before running one on
              a store an 'as_of' read matters for. Removing a redundant version
              changes nothing a read of the past is FOR: the state, the supersede
              edges and the creation time all answer as before, and only WHICH
              version is named can differ, so a memory that was resolved and then
              unresolved may read as merely saved for an instant whose reflect
              flood has been compacted away. --fix-updated-at moves because the
              stamp repair IS a write to the live row's UpdatedAt, and it moves it
              BACKWARD to the recorded time of its ANCHOR: the newest version that
              does not itself repeat the version before it whose writer moved the
              stamp in the same statement that filed it. That 'repeats' test is the
              DAMAGE rule, not the whole removal rule: a memory's NEWEST version is
              spared from removal, and a deleted memory's history is left alone,
              but neither guard is part of the anchor's definition, and being spared
              by one is not what makes a version an anchor. A pre-#727 no-op repeat
              is damage at any position, so a newest no-op reflect is never the
              anchor. The anchor is not necessarily earlier than the last write
              that moved the stamp: a memory later touched by a deliberate update,
              or by any post-#727 write, anchors on that write.
              That is what the flag is for, and it is the field that visibly
              changes. So three fields of an as_of answer can move, for those two
              reasons. A DELETED memory is left out of both repairs entirely, so
              for one of those nothing changes.

The history outlives the memory: a deleted memory's last state is still
readable here unless it was purged.

Reading a history writes no memory, history or project row. It does open the
store read-write, the same open 'ghost maintenance status' and 'ghost backup'
use, so a database predating the history table is migrated by the open — and that
migration first writes the pre-migration backup copy it always takes. 'compact'
under --apply writes memory_history and memories, deliberately and only then.
`

type historyView struct {
	MemoryID   string
	Entries    []memory.HistoryEntry
	Live       *memory.Memory
	NoDatabase bool
}

func printMemoryHistory(w io.Writer, v historyView) error {
	if v.NoDatabase {
		_, err := fmt.Fprintln(w, "no Ghost database yet (run ghost first) — no history to read")
		return err
	}
	switch {
	case v.Live != nil:
		if _, err := fmt.Fprintf(w, "memory %s (%s/%s, live)\n", assemble.Token(v.MemoryID), v.Live.Category, v.Live.Source); err != nil {
			return err
		}
	case len(v.Entries) > 0:
		if _, err := fmt.Fprintf(w, "memory %s (no longer live — the entries below are all it left behind)\n", assemble.Token(v.MemoryID)); err != nil {
			return err
		}
	default:
		_, err := fmt.Fprintf(w, "no memory and no history recorded for %s — it was never written, or its history has been pruned\n", assemble.Token(v.MemoryID))
		return err
	}
	if len(v.Entries) == 0 {
		_, err := fmt.Fprintf(w, "no history recorded for %s — nothing has written it since this store reached schema v17\n", assemble.Token(v.MemoryID))
		return err
	}
	for _, e := range v.Entries {
		if err := printHistoryEntry(w, e); err != nil {
			return err
		}
	}
	return nil
}

func printHistoryEntry(w io.Writer, e memory.HistoryEntry) error {
	shown := displayedHistoryEntry(e)
	who := "agent unknown"
	if e.Agent != "" {
		who = "agent " + assemble.Data(e.Agent)
		if e.SessionID != "" {
			who += " session " + assemble.Data(e.SessionID)
		}
	}
	if _, err := fmt.Fprintf(w, "\n%s  %s  (%s)\n", assemble.Label(e.RecordedAt), e.Phase, who); err != nil {
		return err
	}
	resolved := "unresolved"
	if e.ResolvedAt != nil {
		resolved = "resolved at " + assemble.Label(*e.ResolvedAt)
	}
	if _, err := fmt.Fprintf(w, "  %s/%s importance %.2f, %s, project %s\n",
		e.Category, e.Source, e.Importance, resolved, assemble.Token(e.ProjectID)); err != nil {
		return err
	}
	if e.RelatedID != "" {
		if _, err := fmt.Fprintf(w, "  related memory: %s\n", assemble.Token(e.RelatedID)); err != nil {
			return err
		}
	}
	if e.FromProjectID != "" || e.ToProjectID != "" {
		if _, err := fmt.Fprintf(w, "  moved: %s -> %s\n", assemble.Token(e.FromProjectID), assemble.Token(e.ToProjectID)); err != nil {
			return err
		}
	}
	if shown.MergedContent != "" {
		if _, err := fmt.Fprintf(w, "  folded-in text: %s\n", assemble.Data(shown.MergedContent)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "  %s\n", assemble.Data(shown.Content))
	return err
}

func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func printHistoryJSON(w io.Writer, entries []memory.HistoryEntry) error {
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(displayedHistoryEntry(e)); err != nil {
			return err
		}
	}
	return nil
}

func displayedHistoryEntry(e memory.HistoryEntry) memory.HistoryEntry {
	e.Content = substituteHistoryText(e.Content, e.Category)
	if e.MergedContent != "" {
		e.MergedContent = substituteHistoryText(singleLine(e.MergedContent), "")
	}
	return e
}

func substituteHistoryText(content, category string) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, category, content)
	}
	return content
}

func runHistory() {
	args := os.Args[2:]
	if historyCompactRequested(args) {
		runHistoryCompact(args[1:])
		return
	}
	opts, err := parseHistoryArgs(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n\n%s", err, historyUsage)
		os.Exit(1)
	}

	dataDir, err := dataDirPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
			os.Exit(1)
		}
		if opts.Purge {
			fmt.Fprintf(os.Stderr, "no Ghost database at %s — nothing to purge\n", dbPath)
			os.Exit(1)
		}
		if opts.JSON {
			printHistoryJSONError(os.Stdout, "no Ghost database yet (run ghost first) — no history to read")
			os.Exit(1)
		}
		view := historyView{MemoryID: opts.MemoryID, NoDatabase: true}
		if err := printMemoryHistory(os.Stdout, view); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	db, err := memory.OpenDB(dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: database: %v\n", err)
		os.Exit(1)
	}
	defer db.Close() //nolint:errcheck

	ctx := context.Background()
	s := memory.NewStore(db, nil)
	if opts.Purge {
		if err := purgeHistoryMemory(ctx, s, opts.MemoryID); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	memoryID, err := resolveHistoryRef(ctx, s, opts.MemoryID)
	if err != nil {
		if werr := reportHistoryRefusal(os.Stdout, os.Stderr, opts.JSON, err.Error()); werr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", werr)
		}
		os.Exit(1)
	}
	view, err := readHistoryView(ctx, s, memoryID, opts.Limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if err := printHistory(os.Stdout, view, opts.JSON); err != nil {
		if !errors.Is(err, errNothingToReport) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

func purgeHistoryMemory(ctx context.Context, s historyPurgeStore, memoryID string) error {
	ids, err := s.AnyMemoryIDsByIDPrefix(ctx, memoryID)
	if err != nil {
		return fmt.Errorf("check the memory id against this store: %w", err)
	}
	stored, ok := wholeMemoryID(ids, memoryID)
	if !ok {
		return purgePrefixRefusal(ids, memoryID)
	}
	memoryID = stored
	entries, err := s.MemoryHistory(ctx, memoryID, 0)
	if err != nil {
		return err
	}
	live, err := s.GetByIDs(ctx, []string{memoryID})
	if err != nil {
		return err
	}
	if len(entries) == 0 && len(live) == 0 {
		return noMemoryToPurge(memoryID)
	}
	switch {
	case len(live) > 0:
		fmt.Printf("Purging memory %s and %d recorded version(s) of its text.\n", assemble.Token(memoryID), len(entries))
		outcome, err := s.DeletePurge(ctx, memoryID)
		if err != nil {
			return err
		}
		return printPurgeReport(os.Stdout, outcome)
	default:
		outcome, err := s.PurgeDeletedMemoryHistoryInProject(ctx, entries[len(entries)-1].ProjectID, memoryID)
		if err != nil {
			return err
		}
		fmt.Printf("Memory %s is already deleted; purged its %d recorded version(s) of text.\n", assemble.Token(memoryID), len(entries))
		return printPurgeReport(os.Stdout, outcome)
	}
}

type historyPurgeStore interface {
	AnyMemoryIDsByIDPrefix(ctx context.Context, prefix string) ([]string, error)
	MemoryHistory(ctx context.Context, memoryID string, limit int) ([]memory.HistoryEntry, error)
	GetByIDs(ctx context.Context, ids []string) ([]memory.Memory, error)
	DeletePurge(ctx context.Context, memoryID string) (memory.PurgeOutcome, error)
	PurgeDeletedMemoryHistoryInProject(ctx context.Context, projectID, memoryID string) (memory.PurgeOutcome, error)
}

func printPurgeReport(w io.Writer, outcome memory.PurgeOutcome) error {
	lines := []string{
		"Purged. This memory's row, its recorded history and any reflection snapshot holding it are gone.",
	}
	if outcome.CheckpointComplete {
		lines = append(lines, "The pages this purge freed were zeroed and the write-ahead log truncated, so the copy this memory held is gone from the database file and its -wal.")
	} else {
		lines = append(lines, "Other Ghost readers held the store, so the write-ahead log could not be truncated: this memory's text stays in it until they close and a later purge or checkpoint completes.")
	}
	lines = append(lines, "Text an older Ghost version, one running without secure delete, already freed into the file's free space, a copy in a backup taken before this, or another machine's store, is out of reach; `ghost backup` writes a clean copy without it.")
	for _, line := range lines {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

func wholeMemoryID(ids []string, ref string) (string, bool) {
	id, err := memref.ResolveIn(ids, "id", ref)
	if err != nil {
		return "", false
	}
	if !strings.EqualFold(id, ref) {
		return "", false
	}
	return id, true
}

func purgePrefixRefusal(ids []string, ref string) error {
	if len(ids) == 0 {
		return noMemoryToPurge(ref)
	}
	id, err := memref.ResolveIn(ids, "id", ref)
	if err != nil {
		return fmt.Errorf("purge needs a whole memory id and %s is not one: %w", memory.RefusalArg("id", ref), err)
	}
	return fmt.Errorf("purge needs a whole memory id, not the %d-character prefix %s, because it erases recorded text for good — the memory it names is %s: run 'ghost history purge %s' to purge it",
		utf8.RuneCountInString(ref), memory.RefusalArg("id", ref), memory.RefusalArg("id", id), memory.RefusalArg("id", id))
}

func noMemoryToPurge(id string) error {
	return fmt.Errorf("no memory and no history for %s — nothing to purge", memory.RefusalArg("id", id))
}

func resolveHistoryRef(ctx context.Context, s *memory.Store, ref string) (string, error) {
	ids, err := s.AnyMemoryIDsByIDPrefix(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve the memory id: %w", err)
	}
	id, err := memref.ResolveIn(ids, "id", ref)
	if err == nil {
		return id, nil
	}
	if errors.Is(err, memref.ErrNoMatch) && utf8.RuneCountInString(ref) >= memref.FullIDLen {
		return ref, nil
	}
	return "", err
}

func reportHistoryRefusal(out, errOut io.Writer, asJSON bool, message string) error {
	if asJSON {
		if _, err := fmt.Fprintln(out, historyJSONErrorLine(message)); err != nil {
			return err
		}
		return nil
	}
	_, err := fmt.Fprintf(errOut, "error: %s\n", message)
	return err
}

func historyJSONErrorLine(message string) string {
	b, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	if err != nil {
		return `{"error":"the refusal could not be encoded"}`
	}
	return string(b)
}

func readHistoryView(ctx context.Context, s *memory.Store, memoryID string, limit int) (historyView, error) {
	entries, err := s.MemoryHistory(ctx, memoryID, limit)
	if err != nil {
		return historyView{}, err
	}
	return withLiveness(ctx, s, memoryID, entries)
}

func withLiveness(ctx context.Context, s *memory.Store, memoryID string, entries []memory.HistoryEntry) (historyView, error) {
	live, err := s.GetByIDs(ctx, []string{memoryID})
	if err != nil {
		return historyView{}, err
	}
	view := historyView{MemoryID: memoryID, Entries: entries}
	if len(live) > 0 {
		view.Live = &live[0]
	}
	return view, nil
}

func printHistoryJSONError(w io.Writer, message string) {
	_, _ = io.WriteString(w, historyJSONErrorLine(message)+"\n")
}

func printHistory(w io.Writer, v historyView, asJSON bool) error {
	if asJSON {
		if len(v.Entries) == 0 {
			if v.Live != nil {
				printHistoryJSONError(w, fmt.Sprintf(
					"no history recorded for %s — nothing has written it since this store reached schema v17",
					v.MemoryID))
			} else {
				printHistoryJSONError(w, fmt.Sprintf(
					"no memory and no history recorded for %s — it was never written, or its history has been pruned",
					v.MemoryID))
			}
			return errNothingToReport
		}
		return printHistoryJSON(w, v.Entries)
	}
	return printMemoryHistory(w, v)
}

var errNothingToReport = errors.New("nothing to report")
