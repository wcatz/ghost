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

	"github.com/wcatz/ghost/internal/memory"
)

// historyOptions is `ghost history`'s parsed arguments.
type historyOptions struct {
	// MemoryID is the memory whose history to read, or to purge with `purge`.
	// Required: with no id there is nothing to print, and guessing one (say, the
	// most recent write) would answer a different question than the one asked.
	MemoryID string
	// Limit caps how many entries are printed, newest kept. Zero means the
	// caller passed no --limit, and the store's own default applies: every
	// version the growth policy retains.
	Limit int
	// JSON prints one machine-readable object per entry instead of the
	// human-readable block.
	JSON bool
	// Purge erases the memory and every recorded version of it, instead of
	// printing its history. See runHistoryPurge.
	Purge bool
}

// parseHistoryArgs parses `ghost history <memory-id> [--limit N] [--json]` and
// `ghost history purge <memory-id>`. Only --limit and --json are recognized;
// anything else is an error rather than being ignored, because a silently
// misparsed flag here would print a confident and wrong history.
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
				return opts, fmt.Errorf("history takes one memory id, got %q as well", arg)
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

// parseHistoryLimit accepts a positive whole number. Zero is refused rather
// than read as "unlimited": a reader who types --limit 0 asking for no entries
// would be handed the store's full default, which is the opposite of what they
// asked for and not something the output would reveal.
func parseHistoryLimit(value string) (int, error) {
	n, err := strconv.Atoi(value)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("--limit needs a positive whole number, got %q", value)
	}
	return n, nil
}

// historyUsage is the help for `ghost history`: stdout for -h/--help (see
// handleHelp), and the same text on stderr after a usage error. One text for
// both, so the two cannot drift.
const historyUsage = `Usage: ghost history <memory-id> [--limit N] [--json]
       ghost history purge <memory-id>

Prints one memory's append-only history: every insert, edit, reflection
rewrite, duplicate fold, resolve, supersession, restore, import and deletion,
oldest first, each with the content, category, importance, resolved_at and
source the memory held once that write landed.

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
              and a delete cannot reach it.

The history outlives the memory: a deleted memory's last state is still
readable here unless it was purged.

This command writes no memory, history or project row. It does open the store
read-write, the same open 'ghost maintenance status' and 'ghost backup' use, so
a database predating the history table is migrated by the open — and that
migration first writes the pre-migration backup copy it always takes.
`

// historyView is everything `ghost history` prints. A value the printer takes,
// so the output contract is testable without a database.
type historyView struct {
	MemoryID   string
	Entries    []memory.HistoryEntry
	Live       *memory.Memory
	NoDatabase bool
}

// printMemoryHistory renders the view: a header naming the memory and whether it
// is still live, then one block per event, oldest first. Empty states — no
// database, an id that names neither a memory nor a history — print an explicit
// sentence rather than nothing, so an empty result is never mistaken for a
// memory with no past.
func printMemoryHistory(w io.Writer, v historyView) error {
	if v.NoDatabase {
		_, err := fmt.Fprintln(w, "no Ghost database yet (run ghost first) — no history to read")
		return err
	}
	switch {
	case v.Live != nil:
		if _, err := fmt.Fprintf(w, "memory %s (%s/%s, live)\n", v.MemoryID, v.Live.Category, v.Live.Source); err != nil {
			return err
		}
	case len(v.Entries) > 0:
		// Not an error: the history outliving the row is the feature, and this
		// line is what tells a reader that the entries below are tombstones
		// rather than a live memory's recent edits.
		if _, err := fmt.Fprintf(w, "memory %s (no longer live — the entries below are all it left behind)\n", v.MemoryID); err != nil {
			return err
		}
	default:
		// No row and no history: an id that was never written, or one whose
		// history has already been pruned. Saying "deleted" here would claim a
		// tombstone that is not there.
		_, err := fmt.Fprintf(w, "no memory and no history recorded for %s — it was never written, or its history has been pruned\n", v.MemoryID)
		return err
	}
	if len(v.Entries) == 0 {
		_, err := fmt.Fprintf(w, "no history recorded for %s — nothing has written it since this store reached schema v17\n", v.MemoryID)
		return err
	}
	for _, e := range v.Entries {
		if err := printHistoryEntry(w, e); err != nil {
			return err
		}
	}
	return nil
}

// printHistoryEntry renders one event: when it happened, which write path it
// was, who performed it when the path knew, and the state the memory held
// afterwards. The content is printed in full — a history whose text is elided
// cannot answer the question it exists for — except that it goes through the same
// credential substitution every other cmd/ghost site rendering stored memory text
// uses, because the write-time filter which also guards this table cannot reach a
// row written before it existed, and `ghost history` is the command that store's
// owner is asked to run. See displayProposal for the two layers and why this is
// the second rather than the only one.
//
// The substitution is computed once, in displayedHistoryEntry, and the --json
// printer encodes the same values: two forms of one command cannot then disagree
// about what a row holds.
func printHistoryEntry(w io.Writer, e memory.HistoryEntry) error {
	shown := displayedHistoryEntry(e)
	who := "agent unknown"
	if e.Agent != "" {
		who = "agent " + e.Agent
		if e.SessionID != "" {
			who += " session " + e.SessionID
		}
	}
	if _, err := fmt.Fprintf(w, "\n%s  %s  (%s)\n", e.RecordedAt, e.Phase, who); err != nil {
		return err
	}
	resolved := "unresolved"
	if e.ResolvedAt != nil {
		resolved = "resolved at " + *e.ResolvedAt
	}
	if _, err := fmt.Fprintf(w, "  %s/%s importance %.2f, %s, project %s\n",
		e.Category, e.Source, e.Importance, resolved, e.ProjectID); err != nil {
		return err
	}
	// The other end of the event, when it has one. A reader asking "what
	// replaced this" or "what is claiming it" needs it on the same screen as the
	// text, not in the JSON.
	if e.RelatedID != "" {
		if _, err := fmt.Fprintf(w, "  related memory: %s\n", e.RelatedID); err != nil {
			return err
		}
	}
	// The folded-in text is a memory's own wording, from a row a FoldOnly fold
	// deliberately dropped, and it is the one text field here the write-time
	// filter never saw: `ghost_history_content` wraps the content column, and
	// merged_content is a plain column beside it. So this is the field most likely
	// to still hold a value on a store where the content column does not.
	if shown.MergedContent != "" {
		if _, err := fmt.Fprintf(w, "  folded-in text: %s\n", shown.MergedContent); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "  %s\n", shown.Content)
	return err
}

// singleLine collapses a folded-in text onto one line so the surrounding
// indentation stays readable. The content column above it is printed verbatim:
// that is the record, and this is a summary of something the record already
// holds elsewhere.
func singleLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// printHistoryJSON writes one JSON object per entry. The store's own
// HistoryEntry is the schema, so the machine-readable form cannot drift from
// what the read API returns.
//
// The two text fields are substituted rather than encoded verbatim, and the schema
// is not what changes — the keys, their types and their order are the entry's own,
// so a consumer sees a string it can branch on. What it must not see is the value:
// --json is the form people pipe into jq and into files, which outlives the
// terminal the way lifecycle.log outlives the run, and the human form already
// withholds it. Two forms of one command disagreeing about the same row is the
// wrong answer either way, and a consumer that wants the redacted notice rather
// than the value gets the same one the human form prints.
func printHistoryJSON(w io.Writer, entries []memory.HistoryEntry) error {
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(displayedHistoryEntry(e)); err != nil {
			return err
		}
	}
	return nil
}

// displayedHistoryEntry is the entry with its two text fields put through the
// substitution, for both printers. A copy, because the caller's slice is the
// store's read and nothing here may rewrite it.
//
// The two fields are not the same column and are not treated as one: content is
// the write-time filter's, and merged_content is not, so a store can hold a value
// in the second and not the first. The limit is zero — no cut — because the history
// answers "what did this hold" and a cut is not an answer; what is withheld is
// withheld whole, with the byte count of the text that was not printed.
func displayedHistoryEntry(e memory.HistoryEntry) memory.HistoryEntry {
	e.Content = displayProposal(e.Content, e.Category, 0)
	if e.MergedContent != "" {
		e.MergedContent = displayClaim(singleLine(e.MergedContent), 0)
	}
	return e
}

// runHistory implements `ghost history <memory-id>`, and `ghost history purge
// <memory-id>`.
//
// The store is opened with memory.OpenDB, like every other report-style command
// (`ghost maintenance status`, `ghost backup`). That is a read-write open and is
// not described as anything else: a store predating the history table is
// migrated, and migration writes a full pre-migration backup beside the database
// first. The alternative — the strictly read-only transfer opener — would refuse
// exactly the store a user is most likely to ask this question about (one they
// just upgraded and have not run a session against yet), so the command migrates
// instead of failing, and says so in its help rather than promising to write
// nothing.
func runHistory() {
	opts, err := parseHistoryArgs(os.Args[2:])
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
			// There is nothing to redact: no store holds the text. Saying so is
			// better than reporting a successful purge of nothing.
			fmt.Fprintf(os.Stderr, "no Ghost database at %s — nothing to purge\n", dbPath)
			os.Exit(1)
		}
		if opts.JSON {
			// Not an empty stream: a script that piped a no-database run into a
			// loop would otherwise see zero lines and conclude the memory has no
			// history, which is a different claim. An error object plus a
			// non-zero exit is the shape a script can actually branch on.
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
		runHistoryPurge(ctx, s, opts.MemoryID)
		return
	}
	view, err := readHistoryView(ctx, s, opts.MemoryID, opts.Limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	// The refusal is already on stdout in --json form (it is the machine's copy
	// of the message), so the exit code carries it and stderr stays quiet.
	if err := printHistory(os.Stdout, view, opts.JSON); err != nil {
		if !errors.Is(err, errNothingToReport) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

// runHistoryPurge erases a memory's recorded text, and the memory itself when it
// is still there.
//
// Two cases, because a purge asked for at DELETE time cannot cover a memory that
// is already gone: a live memory is deleted with its history in one transaction,
// and a memory that exists only as a tombstone has its history erased on its own.
// Without the second case, "erase that secret" asked after the memory was deleted
// would report the memory as not found and leave the text exactly where it was —
// the failure the command exists to prevent.
//
// The count is read BEFORE the purge, so the operator sees how much text is about
// to go rather than being told "done" and left guessing whether the argument was
// even a live memory. A purge is irreversible by construction — that is what
// makes it a redaction path — so the count is the only warning there is.
func runHistoryPurge(ctx context.Context, s *memory.Store, memoryID string) {
	entries, err := s.MemoryHistory(ctx, memoryID, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	live, err := s.GetByIDs(ctx, []string{memoryID})
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(entries) == 0 && len(live) == 0 {
		fmt.Fprintf(os.Stderr, "no memory and no history for %s — nothing to purge\n", memoryID)
		os.Exit(1)
	}
	switch {
	case len(live) > 0:
		fmt.Printf("Purging memory %s and %d recorded version(s) of its text.\n", memoryID, len(entries))
		if err := s.DeleteWithOptions(ctx, memoryID, memory.DeleteOptions{PurgeHistory: true}); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	default:
		// Already deleted. The row is not coming back and nothing asked for it to.
		fmt.Printf("Memory %s is already deleted; purging its %d recorded version(s) of text.\n", memoryID, len(entries))
		if _, err := s.PurgeMemoryHistory(ctx, memoryID); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
	}
	// Accurate about what the transaction covers: the row, its history, and the
	// snapshots that could restore it all go. What no purge can reach is a copy
	// outside this database — an earlier backup, another machine's store — or a
	// text quoted into something that was never Ghost's memory.
	fmt.Println("Purged. This memory's row, its recorded history and any reflection snapshot holding it are gone.")
}

// readHistoryView gathers what the printer needs from an open store: the memory
// id it is reporting on, its recorded history, and whether the row is still
// live.
//
// The two reads are kept apart on purpose. A MISS is not an error — a deleted
// memory's history is exactly what a reader is here for — but a failed read is,
// because "no live row" is the only other thing Live distinguishes, and a report
// that cannot tell them apart would state the wrong one: a read failure would
// print "no longer live" about a memory that is still there. So the liveness
// read's error is returned rather than folded into an empty result.
//
// It is a function rather than inline code in runHistory so that error is
// testable at all: runHistory ends in os.Exit.
func readHistoryView(ctx context.Context, s *memory.Store, memoryID string, limit int) (historyView, error) {
	entries, err := s.MemoryHistory(ctx, memoryID, limit)
	if err != nil {
		return historyView{}, err
	}
	return withLiveness(ctx, s, memoryID, entries)
}

// withLiveness is the second read on its own, so the rule it has to keep — a
// MISS is fine, a FAILURE is not — is one function a test can drive on its own.
// Folding it back into readHistoryView would put it behind a read that fails
// first under the same conditions, which is exactly how a swallowed error hides:
// the test would still see an error, from the wrong read.
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

// printHistoryJSONError writes the one shape of --json output that is not a
// history entry: a refusal, carrying the reason. Every line of a --json stream
// is otherwise an entry, so a consumer can tell them apart by a field it never
// has to guess about.
func printHistoryJSONError(w io.Writer, message string) {
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}

// printHistory chooses the rendering. --json prints only the entries, so the
// output is pipeable into jq with no header line to strip.
//
// A JSON run with nothing to report is a refusal, not an empty stream. Zero
// lines with exit 0 is the one answer a script cannot act on: it cannot tell
// "this memory was never written" from "it predates the history table" from
// "the growth policy pruned it" from "the query found nothing because the tool
// is broken", and all four would be the same. So the miss takes the same shape as
// the no-database case — an error object and a non-zero exit — and the caller
// turns that into os.Exit(1).
//
// Liveness is the one distinction the JSON form does not repeat per line, and it
// does not need to: the last entry's phase says it. A memory that is gone ends
// in a `delete` row.
func printHistory(w io.Writer, v historyView, asJSON bool) error {
	if asJSON {
		// Zero entries is a refusal whatever the liveness says, and the two
		// misses want different sentences. A LIVE memory with no history is the
		// pre-v17 case: the row predates the table and nothing this build has
		// done has written one, so "no memory and no history" would be false and
		// an empty stream would let a script conclude the memory has no past
		// rather than that Ghost was not watching when it was written.
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

// errNothingToReport says a report found nothing to print, which is a non-zero
// exit for a machine reader and a sentence for a person.
var errNothingToReport = errors.New("nothing to report")
