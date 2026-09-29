package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// config is the whole of a storyline run's inputs. Every one of them has a
// default that needs nothing but the checkout, so the local command in
// docs/benchmarks.md is the one a reader can paste.
type config struct {
	storyline  string
	repoDir    string
	keep       bool
	judge      bool
	model      string
	ollamaURL  string
	drainTO    time.Duration
	authFile   string
	resultsDir string
	timeout    time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.storyline, "storyline", "reversed-decision", "storyline key to run; see storylines() in eval/storyline/storyline.go for the registry")
	flag.StringVar(&cfg.repoDir, "repo", ".", "ghost repo root containing cmd/ghost")
	flag.BoolVar(&cfg.keep, "keep", false, "keep the scratch dir (the store, the built binary and the logs) after the run")
	flag.BoolVar(&cfg.judge, "judge", false, "add the opt-in LLM judge over the final session's answer")
	flag.StringVar(&cfg.model, "model", "", "opencode model for the sessions (empty keeps Ghost's own default, opencode/big-pickle)")
	flag.StringVar(&cfg.ollamaURL, "ollama", "http://localhost:11434", "Ollama base URL for the reachability check")
	flag.DurationVar(&cfg.drainTO, "drain-timeout", 3*time.Minute, "max wait for the embedding drain the arc stages need")
	flag.StringVar(&cfg.authFile, "opencode-auth-file", "", "optional path to an opencode auth.json copied into the scratch data dir so sandboxed sessions authenticate")
	flag.StringVar(&cfg.resultsDir, "results-dir", "eval/storyline/results", "directory for the run report")
	flag.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "max time for one session or one arc stage")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	story, err := StorylineByKey(cfg.storyline)
	if err != nil {
		return err
	}
	// Validated before anything is built, spawned or spent: a storyline whose
	// grade could not mean what it says should fail at the flag boundary, not
	// after three model sessions.
	if err := story.Validate(); err != nil {
		return err
	}

	// SIGINT/SIGTERM cancels the run, so an interrupted run stops at the next
	// call boundary rather than leaving a ghost subprocess behind in the scratch
	// tree.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repoDir, err := filepath.Abs(cfg.repoDir)
	if err != nil {
		return fmt.Errorf("resolve -repo: %w", err)
	}
	// The scratch tree lives inside the checkout's own gitignored .sandbox/ so a
	// run cannot write into a developer's real data dir even by accident, and so
	// -keep hands back a tree that is obviously a run artifact rather than one
	// that has to be guessed at.
	sandboxRoot := filepath.Join(repoDir, ".sandbox")
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sandboxRoot, err)
	}
	scratch, err := os.MkdirTemp(sandboxRoot, "storyline-"+story.Key+"-*")
	if err != nil {
		return fmt.Errorf("create scratch dir: %w", err)
	}
	if !cfg.keep {
		defer func() { _ = os.RemoveAll(scratch) }()
	}
	dirs, err := makeScratchLayout(scratch)
	if err != nil {
		return err
	}
	if err := seedOpencodeAuth(scratch, cfg.authFile); err != nil {
		return err
	}
	env := scratchEnv(scratch, cfg.model)

	// The session-start block resolves a project from a DIRECTORY, and the
	// directory it resolves is the one the storyline's project is bound to. The
	// checkout therefore lives inside the scratch tree, named for the project, so
	// nothing a run resolves can reach the developer's own store or the repo it
	// was launched from.
	workDir := filepath.Join(dirs["work"], story.Project)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return fmt.Errorf("create work dir: %w", err)
	}

	fmt.Printf("storyline: %s (%s) — %s\n", story.Key, story.Project, story.Title)
	fmt.Printf("scratch: %s\n", scratch)

	fmt.Println("building ghost binary from the checkout under test...")
	bin := filepath.Join(scratch, "ghost")
	if err := buildGhost(ctx, repoDir, bin); err != nil {
		return err
	}

	ghost := newBinaryGhost(bin, env, story.Project, workDir,
		filepath.Join(dirs["data"], "ghost", "ghost.db"), cfg.ollamaURL, cfg.drainTO, os.Stderr)
	if err := ghost.start(ctx); err != nil {
		return err
	}
	// The embedding worker lives INSIDE the ghost mcp process, so the session has
	// to survive until the drain completes or nothing will ever embed. Settle
	// closes it; a run that dies before then is a run with an orphaned child, so
	// the close is armed here too.
	defer ghost.abort()

	var judge Agent
	if cfg.judge {
		judge = newAgent(cfg.model)
	}
	r := &Run{
		Story:   story,
		WorkDir: workDir,
		Ghost:   ghost,
		Agent:   newAgent(cfg.model),
		Judge:   judge,
		Timeout: cfg.timeout,
		Out:     os.Stdout,
	}
	res, err := r.Execute(ctx)
	if err != nil {
		return err
	}
	for _, c := range res.Checks {
		mark := "FAIL"
		if c.Passed {
			mark = "PASS"
		}
		fmt.Printf("%s %s — %s\n", mark, c.Name, oneLine(c.Detail))
	}
	path, err := writeReport(cfg.resultsDir, res, modelLabel(cfg.model))
	if err != nil {
		return err
	}
	fmt.Printf("report written: %s\n", path)
	if cfg.keep {
		fmt.Printf("scratch kept: %s\n", scratch)
	}
	if !res.Passed() {
		return fmt.Errorf("%d of %d checks failed: %s",
			len(res.FailedNames()), len(res.Checks), strings.Join(res.FailedNames(), ", "))
	}
	return nil
}

// modelLabel is what the report attributes the answers to. An empty -model means
// Ghost's own default was used, so the report says which default instead of
// leaving the model field empty — a blank attribution in a report about a model's
// behaviour is the one thing a reader cannot recover from the artifact.
func modelLabel(model string) string {
	if strings.TrimSpace(model) == "" {
		return "(ghost default) opencode/big-pickle"
	}
	return model
}
