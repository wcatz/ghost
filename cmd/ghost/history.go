package main

import (
	"context"
	"encoding/json"
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
	// MemoryID is the memory whose history to read. Required: with no id there
	// is nothing to print, and guessing one (say, the most recent write) would
	// answer a different question than the one asked.
	MemoryID string
	// Limit caps how many entries are printed, newest kept. Zero means the
	// caller passed no --limit, and the store's own default applies: every
	// version the growth policy retains.
	Limit int
	// JSON prints one machine-readable object per entry instead of the
	// human-readable block.
	JSON bool
}

// parseHistoryArgs parses `ghost history <memory-id> [--limit N] [--json]`. Only
// --limit and --json are recognized; anything else is an error rather than
// being ignored, because a silently misparsed flag here would print a confident
// and wrong history.
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

Prints one memory's append-only history (issue #578): every insert, edit,
reflection rewrite, duplicate fold, resolve, supersession, restore, import and
deletion, oldest first, each with the content, category, importance, resolved_at
and source the memory held once that write landed.

  --limit N   Print only the newest N entries (default: all the store keeps)
  --json      One JSON object per entry, for scripting

The history survives the memory it describes: a deleted memory's last state is
still readable here.

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
		// history the growth policy has already dropped. Saying "deleted"
		// here would claim a tombstone that is not there.
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
// cannot answer the question it exists for.
func printHistoryEntry(w io.Writer, e memory.HistoryEntry) error {
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
	_, err := fmt.Fprintf(w, "  %s\n", e.Content)
	return err
}

// printHistoryJSON writes one JSON object per entry. The store's own
// HistoryEntry is the schema, so the machine-readable form cannot drift from
// what the read API returns.
func printHistoryJSON(w io.Writer, entries []memory.HistoryEntry) error {
	enc := json.NewEncoder(w)
	for _, e := range entries {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}

// runHistory implements `ghost history <memory-id>`.
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
		view := historyView{MemoryID: opts.MemoryID, NoDatabase: true}
		if opts.JSON {
			// Not an empty stream: a script that piped a no-database run into a
			// loop would otherwise see zero lines and conclude the memory has no
			// history, which is a different claim. An error object plus a
			// non-zero exit is the shape a script can actually branch on.
			printHistoryJSONError(os.Stdout, "no Ghost database yet (run ghost first) — no history to read")
			os.Exit(1)
		}
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
	view, err := readHistoryView(ctx, s, opts.MemoryID, opts.Limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	if err := printHistory(os.Stdout, view, opts.JSON); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
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
func printHistory(w io.Writer, v historyView, asJSON bool) error {
	if asJSON {
		return printHistoryJSON(w, v.Entries)
	}
	return printMemoryHistory(w, v)
}
