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
func parseObsidianFlags(args []string) (out, project, interval string, err error) {
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
				project = args[i]
			case "--interval":
				interval = args[i]
			}
		case strings.HasPrefix(arg, "--out="):
			out = strings.TrimPrefix(arg, "--out=")
		case strings.HasPrefix(arg, "--project="):
			project = strings.TrimPrefix(arg, "--project=")
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

// runObsidian implements `ghost obsidian export|sync` — a one-way mirror of
// the store into an Obsidian-readable Markdown vault.
func runObsidian() {
	if len(os.Args) < 3 || (os.Args[2] != "export" && os.Args[2] != "sync") {
		fmt.Fprintln(os.Stderr, `Usage: ghost obsidian <export|sync> [flags]

Flags:
  --out string       Vault directory (default ~/Documents/GhostVault or obsidian.vault_dir)
  --project string   Mirror a single project (plus Global)
  --interval string  sync only: poll cadence (default 30s or obsidian.interval)`)
		os.Exit(1)
	}
	mode := os.Args[2]
	out, project, interval, err := parseObsidianFlags(os.Args[3:])
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
