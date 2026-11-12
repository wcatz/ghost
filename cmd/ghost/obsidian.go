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

// parseObsidianFlags parses flags for `ghost obsidian <mode>`.
// It rejects unknown flags, missing values, and duplicate or empty --out, --project, or --interval.
func parseObsidianFlags(args []string) (out, project, interval string, err error) {
	projectSeen := false
	outSeen, intervalSeen := false, false
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
				if outSeen {
					return "", "", "", fmt.Errorf("--out was given twice (%s and %s)",
						memory.RefusalArg("out", out), memory.RefusalArg("out", args[i]))
				}
				if args[i] == "" {
					return "", "", "", errors.New("--out requires a value")
				}
				out = args[i]
				outSeen = true
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
				if intervalSeen {
					return "", "", "", fmt.Errorf("--interval was given twice (%s and %s)",
						memory.RefusalArg("interval", interval), memory.RefusalArg("interval", args[i]))
				}
				if args[i] == "" {
					return "", "", "", errors.New("--interval requires a value")
				}
				interval = args[i]
				intervalSeen = true
			}
		case strings.HasPrefix(arg, "--out="):
			v := strings.TrimPrefix(arg, "--out=")
			if outSeen {
				return "", "", "", fmt.Errorf("--out was given twice (%s and %s)",
					memory.RefusalArg("out", out), memory.RefusalArg("out", v))
			}
			if v == "" {
				return "", "", "", errors.New("--out requires a value")
			}
			out = v
			outSeen = true
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
			v := strings.TrimPrefix(arg, "--interval=")
			if intervalSeen {
				return "", "", "", fmt.Errorf("--interval was given twice (%s and %s)",
					memory.RefusalArg("interval", interval), memory.RefusalArg("interval", v))
			}
			if v == "" {
				return "", "", "", errors.New("--interval requires a value")
			}
			interval = v
			intervalSeen = true
		default:
			return "", "", "", fmt.Errorf("unknown or malformed flag %q", arg)
		}
	}
	return out, project, interval, nil
}

// roDSN builds a read-only SQLite DSN for dbPath.
func roDSN(dbPath string) string {
	u := url.URL{
		Scheme:   "file",
		Opaque:   (&url.URL{Path: dbPath}).EscapedPath(),
		RawQuery: "mode=ro&_pragma=busy_timeout(1000)",
	}
	return u.String()
}

// obsidianUsage is the usage text for `ghost obsidian`.
const obsidianUsage = `Usage: ghost obsidian <export|sync> [flags]

Flags:
  --out string       Vault directory (default ~/Documents/GhostVault or obsidian.vault_dir)
  --project string   Mirror a single project (plus Global)
  --interval string  sync only: poll cadence (default 30s or obsidian.interval)
`

// runObsidian runs `ghost obsidian export|sync`.
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
	db, err := sql.Open("sqlite", roDSN(dbPath))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: open %s: %v\n", dbPath, err)
		os.Exit(1)
	}
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
