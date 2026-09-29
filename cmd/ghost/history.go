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

	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/memref"
)

// historyOptions is `ghost history`'s parsed arguments.
type historyOptions struct {
	// MemoryID is the memory whose history to read, or to purge with `purge`. It
	// is a REF: a full id, or a prefix of 8 or more characters that names exactly
	// one memory, resolved through internal/memref once the store is open (see
	// resolveHistoryRef) — except under `purge`, which takes a whole id only.
	// Required: with no ref there is nothing to print, and guessing one (say, the
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
	// printing its history. See purgeHistoryMemory, which also owns the rule that
	// this one takes a whole id rather than a ref.
	Purge bool
}

// parseHistoryArgs parses `ghost history <memory-ref> [--limit N] [--json]` and
// `ghost history purge <memory-id>`. Only --limit and --json are recognized;
// anything else is an error rather than being ignored, because a silently
// misparsed flag here would print a confident and wrong history. The ref is NOT
// resolved here: that needs the store, and this stays a pure function of argv.
//
// `compact` is NOT a mode here. It is routed away before this runs
// (historyCompactRequested), because it is a different request with its own
// flags and its own parser, and mixing the two would mean this function
// accepting --apply for a mode that has no meaning in it — or refusing it, which
// would be a parser reporting a flag that works.
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

// historyCompactRequested reports whether `ghost history` was asked for the
// store-wide compaction (#730) rather than for one memory's history.
//
// It is a named function because runHistory ends in os.Exit, so an inline word
// comparison would be untestable — and the routing is the one thing that decides
// whether a word the reader typed is a REQUEST or a memory id. A memory id is 32
// hex characters, so nothing a reader can type is ambiguous with `compact`; the
// decision is made on the first word alone, exactly as `purge` is, and every
// other word is still read as the id it looks like.
func historyCompactRequested(args []string) bool {
	return len(args) > 0 && args[0] == "compact"
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
                                 this repair will NOT remove AND whose WRITER moved
                                 updated_at in the same statement that filed it: a
                                 save, an update, or a reflection, and nothing else.
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
              locks, and it works in bounded transactions so it does not hold the
              write lock over a whole store's history.

The history outlives the memory: a deleted memory's last state is still
readable here unless it was purged.

Reading a history writes no memory, history or project row. It does open the
store read-write, the same open 'ghost maintenance status' and 'ghost backup'
use, so a database predating the history table is migrated by the open — and that
migration first writes the pre-migration backup copy it always takes. 'compact'
under --apply writes memory_history and memories, deliberately and only then.
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
	args := os.Args[2:]
	// `compact` is routed away before parseHistoryArgs sees it: it is a different
	// request with its own flags and its own parser, and this command's first
	// operand stays a memory id.
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
		// The purge path deliberately does NOT take a prefix (#720). It erases a
		// memory's text for good, and the argument naming which memory is the only
		// thing standing between an operator and a mistake nothing undoes — so it
		// takes a WHOLE id, and says so when given a prefix one. The read path
		// prints the full id a prefix resolves to, which is the way through.
		if err := purgeHistoryMemory(ctx, s, opts.MemoryID); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// Resolution happens after the store is open, so the id set it decides over
	// is the one this store holds, and before the read, so a refused ref is never
	// reported as a memory with no history.
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
	// The refusal is already on stdout in --json form (it is the machine's copy
	// of the message), so the exit code carries it and stderr stays quiet.
	if err := printHistory(os.Stdout, view, opts.JSON); err != nil {
		if !errors.Is(err, errNothingToReport) {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
		os.Exit(1)
	}
}

// purgeHistoryMemory erases a memory's recorded text, and the memory itself when
// it is still there, or explains why it will not.
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
//
// It returns an error rather than exiting, because the whole decision it makes —
// which of the two cases applies, and whether the argument is a whole id at all —
// is the part worth driving without a process, and runHistory's own os.Exit is one
// line away from the call.
//
// A PREFIX is refused, and this is the one place in Ghost that answers "a memory
// by ref" without taking one. `ghost history` resolves the eight characters a
// report prints (#720) and `ghost resolve --mark` and `ghost supersede --withdraw`
// change rows by ref, so a prefix is the form a caller has on screen — and this is
// the form that erases recorded text for good, where a wrong target is not a
// mistyped flag but an unprintable memory. An echoed full id would not make that
// safe: the announcement and the transaction are one breath apart, and a
// single-character slip in a pasted prefix is still a unique match, so there is no
// moment in which to notice it was the wrong one. So the argument must BE the whole
// id, and the repair is the read path: `ghost history <prefix>` prints the full id
// it resolved, and that goes in here.
//
// A prefix is therefore refused with the full id it names, where there is exactly
// one. Naming it is not a confirmation — nothing is erased either way — it is the
// next command, and refusing without it would leave the operator to find the id in
// a report again.
func purgeHistoryMemory(ctx context.Context, s *memory.Store, memoryID string) error {
	// ONE read of the id set, and both decisions made from it: whether the argument
	// is a whole id, and — if it is not — what to say about it. Reading it twice
	// would mean two reads that could disagree, on a path whose whole job is to
	// erase the right text.
	ids, err := s.AnyMemoryIDsByIDPrefix(ctx, memoryID)
	if err != nil {
		// Propagated, not folded into "not a whole id": a store that cannot be
		// read has told us nothing about the argument, and a prefix sentence here
		// would send the operator after a full id they may already have.
		return fmt.Errorf("check the memory id against this store: %w", err)
	}
	stored, ok := wholeMemoryID(ids, memoryID)
	if !ok {
		return purgePrefixRefusal(ids, memoryID)
	}
	// The STORED spelling, never the caller's. Ids are matched case-insensitively
	// when a ref is resolved — a report can print a hex id uppercased whatever the
	// column holds — and every read below compares case-SENSITIVELY, because neither
	// `memories.id` nor `memory_history.memory_id` carries COLLATE NOCASE. Purging
	// the caller's spelling of an id stored in another one would find no row and no
	// history and report "nothing to purge" on the redaction path, while the text
	// sat in the database.
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
		return fmt.Errorf("no memory and no history for %s — nothing to purge", memoryID)
	}
	switch {
	case len(live) > 0:
		fmt.Printf("Purging memory %s and %d recorded version(s) of its text.\n", memoryID, len(entries))
		if err := s.DeleteWithOptions(ctx, memoryID, memory.DeleteOptions{PurgeHistory: true}); err != nil {
			return err
		}
	default:
		// Already deleted. The row is not coming back and nothing asked for it to.
		fmt.Printf("Memory %s is already deleted; purging its %d recorded version(s) of text.\n", memoryID, len(entries))
		if _, err := s.PurgeMemoryHistory(ctx, memoryID); err != nil {
			return err
		}
	}
	// Accurate about what the transaction covers: the row, its history, and the
	// snapshots that could restore it all go. What no purge can reach is a copy
	// outside this database — an earlier backup, another machine's store — or a
	// text quoted into something that was never Ghost's memory.
	fmt.Println("Purged. This memory's row, its recorded history and any reflection snapshot holding it are gone.")
	return nil
}

// wholeMemoryID reports the id in ids that ref names WHOLE, and whether it names
// one — the test a purge is gated on. It returns the STORED spelling, which is the
// half that matters: the reads a purge then makes compare case-sensitively, so a
// boolean would let a case-folded spelling through the gate and then find nothing.
//
// It asks `memref.ResolveIn` rather than scanning for a case-insensitive match, and
// that is the whole design. Whether a ref IS a stored id is a ref rule — the 8-
// character floor, byte-exact precedence, the ambiguity and third-casing refusals —
// and a gate that re-derived any of it would be the second implementation this
// package exists to prevent. Re-deriving it wrongly is not hypothetical: a store can
// hold two ids differing only in letter case (`ghost import` writes an artifact's
// ids verbatim and its presence probe is case-sensitive, so it can land both), and a
// first-match scan over that set picks whichever SQLite's BINARY collation sorts
// first. `ghost history purge abc…` would then erase `ABC…` — the wrong memory's
// text, irreversibly, while the one named kept it. `ResolveIn` refuses the set
// instead, because no single spelling reaches both.
//
// So the whole id is whatever `ResolveIn` resolved, checked against the ref with
// EqualFold: a resolved PREFIX is not a whole id, and a refusal is not one either —
// including a miss, and including a third casing, which memref refuses because no
// spelling of the caller's reaches the stored ones.
//
// It is a membership test, not a length test: a length test would have to hardcode
// what a full id looks like, and nothing says an id is 32 hex characters — a store
// full of imported notes holds ids that are whatever the artifacts held, and an
// eight-character id there is a whole one.
func wholeMemoryID(ids []string, ref string) (string, bool) {
	id, err := memref.ResolveIn(ids, "id", ref)
	if err != nil {
		// Every refusal, without distinguishing them. The gate's question is
		// "is this a whole id", and no refusal means yes; what the refusal SAID
		// is the refusal's job, one function along.
		return "", false
	}
	if !strings.EqualFold(id, ref) {
		// Resolved to a longer id this ref begins with: a prefix, which is the
		// case this command refuses for its own reason.
		return "", false
	}
	return id, true
}

// purgePrefixRefusal explains that a prefix is not enough here, and names the full
// id when the prefix picks out exactly one. The ambiguity case is memref's own
// refusal — reached through the shared rules rather than restated, so the answer to
// "which memory did you mean" cannot differ between this and the read path.
//
// An argument that names NOTHING is not a prefix, and saying it was would be a
// false claim about a string that is too: after a successful purge the id is in
// neither table, so a re-run of the same command would be told its own argument was
// a prefix. The miss is reported as the miss it is — nothing to purge — and that
// sentence is the answer whether the id was purged already or never existed.
//
// The length is what separates the two only in the refusal's wording. It is read
// here for the same reason memref measures the floor in characters: a byte count
// would call a four-byte ref a "4-character prefix" of an id whose own characters
// are not bytes.
func purgePrefixRefusal(ids []string, ref string) error {
	if len(ids) == 0 {
		// The "nothing to purge" answer, not the prefix one. Which is right
		// depends on nothing this call can see: the id may be a whole id whose
		// memory and text are both already gone, and there is nothing to say to
		// an operator who has just watched a purge succeed.
		return fmt.Errorf("no memory and no history for %s — nothing to purge", ref)
	}
	id, err := memref.ResolveIn(ids, "id", ref)
	if err != nil {
		// Ambiguous, or below memref's floor. Both are memref's sentences rather
		// than this command's, wrapped so the reason a prefix was refused at all
		// is on the same line.
		return fmt.Errorf("purge needs a whole memory id and %q is not one: %w", ref, err)
	}
	return fmt.Errorf("purge needs a whole memory id, not the %d-character prefix %q, because it erases recorded text for good — the memory it names is %s: run 'ghost history purge %s' to purge it",
		utf8.RuneCountInString(ref), ref, id, id)
}

// resolveHistoryRef turns the argument `ghost history` was given into the id its
// read uses (#720).
//
// It goes through internal/memref like every other surface that names a memory by
// ref, so a ref means here exactly what it means to `resolve --mark` and
// `supersede --withdraw` — the eight characters a report prints, an unambiguous
// longer prefix, or a full id of any shape. The rules are not restated here, which
// is the point: two implementations eventually disagree about which id one spelling
// addresses, and this command is the one place that would be most expensive to get
// wrong, because it prints a memory's whole recorded text.
//
// The id set is the one thing that differs, and it is why this is not `memref.Resolve`
// with a project. A history lookup is asked about DELETED memories more often than
// about live ones — the tombstone is the feature — and a deleted memory's id is in
// memory_history alone. It also reaches ids of LIVE memories that predate the
// history table, which have no history row at all, because migrateV17 deliberately
// does not backfill. The set is the whole store, and that is deliberate rather than
// a missing project predicate: this command has no project operand and a full id
// has always reached any row in the store, so scoping a PREFIX to a project would
// make the two forms of one ref disagree about where a memory may be looked for.
//
// A full id the store does not hold is NOT a refusal, and it passes through
// unchanged. The caller has said everything there is to say, and `readHistoryView`
// has always answered an unknown full id as "never written, or its history has been
// pruned" — a report about the id, not an error about the operator's argument.
// Routing it through the prefix rules would report it as a prefix of nothing, which
// is a claim about a string the caller never shortened.
//
// "Not a refusal" is narrower than "not an error", and the difference is
// `memref.ErrNoMatch`. Only the refusal that says the set holds NOTHING this ref
// can mean passes through, and only for a ref long enough not to be a truncation;
// an ambiguity and a third casing are refusals whatever the ref's length, and they
// reach the operator with the matches named. That distinction is memref's, and it
// is why the sentinel is.
func resolveHistoryRef(ctx context.Context, s *memory.Store, ref string) (string, error) {
	ids, err := s.AnyMemoryIDsByIDPrefix(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve the memory id: %w", err)
	}
	// memref's rules over the whole-store set, and its refusals name no project
	// because this set is not confined to one.
	id, err := memref.ResolveIn(ids, "id", ref)
	if err == nil {
		return id, nil
	}
	// A MISS on a ref at least as LONG as a whole id passes through to the read,
	// which reports it as it always has. The reason for the length is that such a ref
	// cannot be a TRUNCATION of an id — there is nothing left to shorten — so the
	// answer is about the id rather than about the operator's spelling of it.
	//
	// The reason for branching on the REFUSAL and not on the length alone is that a
	// length cannot tell the refusals apart. `ghost import` writes an artifact's ids
	// verbatim, so a store can hold two 40-character ids sharing 32 characters, or two
	// 32-character ids differing only in letter case; a gate on length would let
	// either through, and the read would then print "no memory and no history
	// recorded" about a memory the store plainly holds. That is a false claim of
	// exactly the kind #720 removes, and the reader cannot tell it from the truth.
	// `ErrNoMatch` is the distinction, and memref is where it belongs: it already
	// draws it, having separated this refusal from the ambiguity and third-casing
	// ones, and a caller that re-derived it from the message or the length would be
	// the second implementation this package exists to prevent.
	if errors.Is(err, memref.ErrNoMatch) && utf8.RuneCountInString(ref) >= memref.FullIDLen {
		return ref, nil
	}
	return "", err
}

// reportHistoryRefusal writes a refused ref in the form the caller asked for.
//
// A --json run's every line is otherwise an entry, so the refusal is the one
// `{"error": ...}` object a script can branch on — and it goes to STDOUT, not
// stderr, because stdout is the stream the script is reading, and a script that
// merged the two would read the message twice. The human form is a plain
// diagnostic on stderr, beside the report that never appeared.
//
// The write error is returned rather than dropped: the caller exits non-zero on it,
// and a refusal that failed to print must not read as a run that printed nothing.
func reportHistoryRefusal(out, errOut io.Writer, asJSON bool, message string) error {
	if asJSON {
		// printHistoryJSONError ignores its own error, so the refusal's own
		// encoding is a direct write rather than that helper's: the shape is the
		// same object, and here a failed write is an answer the caller needs.
		if _, err := fmt.Fprintln(out, historyJSONErrorLine(message)); err != nil {
			return err
		}
		return nil
	}
	_, err := fmt.Fprintf(errOut, "error: %s\n", message)
	return err
}

// historyJSONErrorLine is the one line a --json run prints instead of an entry.
func historyJSONErrorLine(message string) string {
	// json.Marshal cannot fail on a struct of one string field, so the error is
	// not a real path; the fallback keeps the line a valid object rather than
	// substituting something a script would misparse.
	b, err := json.Marshal(struct {
		Error string `json:"error"`
	}{Error: message})
	if err != nil {
		return `{"error":"the refusal could not be encoded"}`
	}
	return string(b)
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
	_, _ = io.WriteString(w, historyJSONErrorLine(message)+"\n")
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
