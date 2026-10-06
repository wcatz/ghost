package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/obsidian"
)

// parseObsidianFlags parses the flags following `ghost obsidian <mode>`. It
// errors on an unknown flag or a value flag missing its argument rather than
// silently falling back to defaults (a misspelled --intervl would otherwise
// leave the user believing a cadence that isn't in effect).
//
// --out and --interval are destinations and cadences, so they stay
// last-one-wins: a second one is an operator overriding their own earlier flag.
// --project is a SCOPE and refuses a second value and an empty one, with the
// same two sentences every other scope-taking parser uses, because the harm it
// prevents is wider than export's: an empty project is not a narrower mirror,
// it is every project's memories written into the vault. A script that ran
// `ghost obsidian export --project "$PROJECT"` with PROJECT unset answered
// "mirror everything" while the command line read as if it were scoped.
func parseObsidianFlags(args []string) (out, project, interval string, err error) {
	// projectSeen counts OCCURRENCES of a project rather than testing the value
	// for emptiness: a `project != ""` test cannot see `--project= --project
	// ghost`, which would read the empty first value as no project given and let
	// the second one name the scope of a command line that named it twice — and
	// on the empty-first spelling that is the scope that mirrors EVERYTHING. Both
	// spellings ask through this one bool, so a repeat is refused whichever form
	// it is typed in, and the duplicate is asked FIRST as it is everywhere else:
	// a named value followed by any second value is named as the duplicate it is,
	// and two empty values stop at the first one, since with no first value there
	// is no scope in the command line to be a duplicate of.
	projectSeen := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--out", arg == "--project", arg == "--interval":
			if i+1 >= len(args) {
				return "", "", "", fmt.Errorf("flag %s needs a value", arg)
			}
			i++
			switch arg {
			case "--out":
				out = args[i]
			case "--project":
				if projectSeen {
					return "", "", "", errors.New("expected exactly one project")
				}
				if args[i] == "" {
					return "", "", "", errors.New("--project requires a value")
				}
				project = args[i]
				projectSeen = true
			case "--interval":
				interval = args[i]
			}
		case strings.HasPrefix(arg, "--out="):
			out = strings.TrimPrefix(arg, "--out=")
		case strings.HasPrefix(arg, "--project="):
			if projectSeen {
				return "", "", "", errors.New("expected exactly one project")
			}
			project = strings.TrimPrefix(arg, "--project=")
			if project == "" {
				return "", "", "", errors.New("--project requires a value")
			}
			projectSeen = true
		case strings.HasPrefix(arg, "--interval="):
			interval = strings.TrimPrefix(arg, "--interval=")
		default:
			return "", "", "", fmt.Errorf("unknown or malformed flag %q", arg)
		}
	}
	return out, project, interval, nil
}

// roDSN builds a read-only DSN for the given database path. The file: URI
// form is required: modernc.org/sqlite honors mode=ro only on URI DSNs — a
// bare path opens silently read-write (verified against v1.53.0). The path is
// URI-escaped so a '?' or '#' in $XDG_DATA_HOME/$HOME can't be parsed as the
// query separator or a fragment (which would drop mode=ro or open the wrong
// file). No journal_mode pragma: setting it writes the DB header, which a
// read-only connection cannot do — it would fail against a non-WAL database
// and is a pure no-op against a WAL one.
func roDSN(dbPath string) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: dbPath}).EscapedPath(),
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)",
	}
	return u.String()
}

// obsidianUsage is the help for `ghost obsidian`: stderr when the mode is
// missing or unknown (a usage error, exit 2), stdout for -h/--help (see
// handleHelp). One text for both, so the two can never drift.
const obsidianUsage = `Usage: ghost obsidian <export|sync> [flags]

Flags:
  --out string       Vault directory (default ~/Documents/GhostVault or obsidian.vault_dir)
  --project string   Mirror a single project (plus Global)
  --interval string  sync only: poll cadence (default 30s or obsidian.interval)
`

// runObsidian implements `ghost obsidian export|sync` — a one-way mirror of
// the store into an Obsidian-readable Markdown vault. mode is the subcommand
// the dispatch already matched (it is the only caller, and it is what answers a
// word that is neither mode with obsidianUsage and exit 2), and args are the
// flags that follow it — both from the same argv, so a caller that drove the
// dispatch with a synthetic one cannot have the mode and the flags disagree.
func runObsidian(mode string, args []string) {
	out, project, interval, err := parseObsidianFlags(args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if out == "" {
		out = cfg.Obsidian.VaultDir
	}
	if out == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: cannot resolve home dir: %v\n", err)
			os.Exit(1)
		}
		out = filepath.Join(home, "Documents", "GhostVault")
	}
	if interval == "" {
		interval = cfg.Obsidian.Interval
	}

	// A GHOST_DEV_FORBID_DATA_DIR refusal arrives from here, before the
	// directory is created and before the store below is opened: the mirror reads
	// every row of it, and so does a dev build's refusal have to (#721).
	dataDir, err := config.DataDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	dbPath := filepath.Join(dataDir, "ghost.db")
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "error: no database at %s — run ghost mcp init or start a session first\n", dbPath)
		os.Exit(1)
	}
	// Read-only: safe alongside a live MCP server.
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open %s: %v\n", dbPath, err)
		os.Exit(1)
	}
	// PRAGMA data_version (sync mode) is per-connection; pin the pool to one
	// connection so polls compare against a stable baseline. memory.OpenDB does
	// this for read-write opens — raw sql.Open here needs it explicitly.
	db.SetMaxOpenConns(1)
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	store := memory.NewStore(db, logger)
	defer store.Close() //nolint:errcheck

	ex := &obsidian.Exporter{Store: store, Logger: logger}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if mode == "export" {
		if err := ex.Export(ctx, out, project); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				fmt.Println("Stopped.")
				return
			}
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Mirrored to %s\n", out)
		return
	}
	d, err := time.ParseDuration(interval)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: bad --interval %q: %v\n", interval, err)
		os.Exit(1)
	}
	if d <= 0 {
		fmt.Fprintf(os.Stderr, "error: --interval must be positive, got %s\n", d)
		os.Exit(1)
	}
	fmt.Printf("Syncing to %s every %s (Ctrl-C to stop)\n", out, d)
	if err := obsidian.Sync(ctx, ex, db, out, project, d); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			fmt.Println("Stopped.")
			return
		}
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}
