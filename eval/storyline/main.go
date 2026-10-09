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
	without    bool
	runs       int
	model      string
	ollamaURL  string
	drainTO    time.Duration
	authFile   string
	resultsDir string
	timeout    time.Duration
}

func main() {
	var cfg config
	flag.StringVar(&cfg.storyline, "storyline", "reversed-decision", "storyline key to run, a comma-separated list, or all; see storylines() in eval/storyline/storyline.go for the registry")
	flag.StringVar(&cfg.repoDir, "repo", ".", "ghost repo root containing cmd/ghost")
	flag.BoolVar(&cfg.keep, "keep", false, "keep the scratch dir (the store, the built binary and the logs) after the run")
	flag.BoolVar(&cfg.judge, "judge", false, "add the opt-in LLM judge over the final session's answer")
	flag.BoolVar(&cfg.without, "without-ghost", false, "also run the without-Ghost arm: the same scripts, model and saves, with an empty session block, reported side by side with the Ghost arm")
	flag.IntVar(&cfg.runs, "runs", 1, "runs per arm per storyline; the issue's target numbers are quoted for 10")
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

// storyline keys: "all" is every shipped storyline, otherwise a comma-separated
// list. A key the registry does not have is named rather than defaulted.
func selectStorylines(spec string) ([]Storyline, error) {
	if strings.TrimSpace(spec) == "all" {
		return storylines(), nil
	}
	var out []Storyline
	for _, key := range strings.Split(spec, ",") {
		s, err := StorylineByKey(strings.TrimSpace(key))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// cell is one run of one arm of one storyline: its result, or the error that
// ended it, and the report written for it.
type cell struct {
	story  Storyline
	arm    string
	run    int
	res    *Result
	err    error // the run's own error; nil means res holds a graded result
	report string
	// reportErr is a failure to write the run's report, kept apart from err so a
	// completed, graded run is never counted as one that did not run.
	reportErr error
}

func run(cfg config) error {
	stories, err := selectStorylines(cfg.storyline)
	if err != nil {
		return err
	}
	// Validated before anything is built, spawned or spent: a storyline whose
	// grade could not mean what it says should fail at the flag boundary, not
	// after three model sessions.
	for _, story := range stories {
		if err := story.Validate(); err != nil {
			return err
		}
	}
	if cfg.runs < 1 {
		return fmt.Errorf("-runs must be at least 1, got %d", cfg.runs)
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
	// The scratch trees live inside the checkout's own gitignored .sandbox/ so a
	// run cannot write into a developer's real data dir even by accident, and so
	// -keep hands back trees that are obviously run artifacts rather than ones
	// that have to be guessed at.
	sandboxRoot := filepath.Join(repoDir, ".sandbox")
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sandboxRoot, err)
	}
	// Resolved once, here, before anything is built or spawned: the sessions, the
	// arc's phases and the report all have to name ONE model, and leaving the
	// choice to a fallback would let an inherited GHOST_OPENCODE_MODEL decide the
	// sessions alone (see resolveModel).
	model := resolveModel(cfg.model)

	// The binary is built once for the whole invocation, from the checkout under
	// test: every run and both arms drive the same build.
	buildDir, err := os.MkdirTemp(sandboxRoot, "storyline-build-*")
	if err != nil {
		return fmt.Errorf("create build dir: %w", err)
	}
	if !cfg.keep {
		defer func() { _ = os.RemoveAll(buildDir) }()
	}
	fmt.Println("building ghost binary from the checkout under test...")
	bin := filepath.Join(buildDir, "ghost")
	if err := buildGhost(ctx, repoDir, bin); err != nil {
		return err
	}

	arms := []string{armWithGhost}
	if cfg.without {
		arms = append(arms, armWithoutGhost)
	}
	var cells []cell
	for _, story := range stories {
		// Runs are the outer loop and the arms the inner one, so a run's two arms
		// are taken back to back on the same model and the same build.
		for n := 1; n <= cfg.runs; n++ {
			for _, arm := range arms {
				c := cell{story: story, arm: arm, run: n}
				c.res, c.err = runCell(ctx, cfg, sandboxRoot, bin, model, story, arm, n)
				if c.err != nil {
					fmt.Printf("%s %s run %d: ERROR: %v\n", story.Key, arm, n, c.err)
				} else {
					// A report that could not be written is its own failure: the run
					// finished and was graded, so its result stays in every tally and
					// in the exit code, and err stays the RUN's error alone.
					c.report, c.reportErr = writeCellReport(cfg.resultsDir, story, arm, n, c.res, model)
					printCell(c)
				}
				cells = append(cells, c)
				if ctx.Err() != nil {
					return ctx.Err()
				}
			}
		}
	}

	summary := formatSummary(cells, cfg.runs)
	fmt.Print("\n" + summary)
	path, err := writeSummary(cfg.resultsDir, stories, model, summary)
	if err != nil {
		return err
	}
	fmt.Printf("summary written: %s\n", path)
	if cfg.keep {
		fmt.Printf("scratch kept under: %s\n", sandboxRoot)
	}

	// The exit code is the Ghost arm's: its gating checks failing is a finding.
	// The without-Ghost arm is the control, so a miss there is the measurement
	// and does not decide the exit code; an error in it still does, because a
	// control that did not run measures nothing.
	var failed []string
	for _, c := range cells {
		if c.reportErr != nil {
			failed = append(failed, fmt.Sprintf("%s %s run %d: report not written: %v", c.story.Key, c.arm, c.run, c.reportErr))
		}
		switch {
		case c.err != nil:
			failed = append(failed, fmt.Sprintf("%s %s run %d errored", c.story.Key, c.arm, c.run))
		case c.arm == armWithGhost && !c.res.Passed():
			failed = append(failed, fmt.Sprintf("%s run %d: %s", c.story.Key, c.run, strings.Join(c.res.FailedNames(), ", ")))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d run(s) failed: %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}

// runCell is one run of one arm in its own scratch tree: its own data dir, its
// own work dir, its own ghost mcp process. Both arms get the same sandbox, the
// same environment and the same model; they differ in Run.WithoutGhost alone.
func runCell(ctx context.Context, cfg config, sandboxRoot, bin, model string, story Storyline, arm string, n int) (*Result, error) {
	scratch, err := os.MkdirTemp(sandboxRoot, fmt.Sprintf("storyline-%s-%s-%d-*", story.Key, arm, n))
	if err != nil {
		return nil, fmt.Errorf("create scratch dir: %w", err)
	}
	if !cfg.keep {
		defer func() { _ = os.RemoveAll(scratch) }()
	}
	dirs, err := makeScratchLayout(scratch)
	if err != nil {
		return nil, err
	}
	if err := seedOpencodeAuth(scratch, cfg.authFile); err != nil {
		return nil, err
	}
	env := scratchEnv(scratch, model)

	// The session-start block resolves a project from a DIRECTORY, and the
	// directory it resolves is the one the storyline's project is bound to. The
	// checkout therefore lives inside the scratch tree, named for the project, so
	// nothing a run resolves can reach the developer's own store or the repo it
	// was launched from.
	workDir := filepath.Join(dirs["work"], story.Project)
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return nil, fmt.Errorf("create work dir: %w", err)
	}
	fmt.Printf("storyline: %s (%s) — %s [%s, run %d/%d]\n", story.Key, story.Project, story.Title, arm, n, cfg.runs)
	fmt.Printf("scratch: %s\n", scratch)

	ghost := newBinaryGhost(bin, env, story.Project, workDir,
		filepath.Join(dirs["data"], "ghost", "ghost.db"), cfg.ollamaURL, cfg.drainTO, os.Stderr)
	if err := ghost.start(ctx); err != nil {
		return nil, err
	}
	// The embedding worker lives INSIDE the ghost mcp process, so the session has
	// to survive until the drain completes or nothing will ever embed. Settle
	// closes it; a run that dies before then, and the without-Ghost arm that never
	// settles, are runs with an orphaned child, so the close is armed here too.
	defer ghost.abort()

	var judge Agent
	if cfg.judge {
		judge = newAgent(model)
	}
	r := &Run{
		Story:        story,
		WorkDir:      workDir,
		Ghost:        ghost,
		Agent:        newAgent(model),
		Judge:        judge,
		WithoutGhost: arm == armWithoutGhost,
		Timeout:      cfg.timeout,
		Out:          os.Stdout,
	}
	return r.Execute(ctx)
}

// printCell prints one run's checks, so a single run reads the way it always did.
func printCell(c cell) {
	for _, ch := range c.res.Checks {
		fmt.Printf("%s %s — %s\n", ch.verdict(), ch.Name, oneLine(ch.Detail))
	}
	if c.reportErr != nil {
		fmt.Printf("report NOT written: %v\n", c.reportErr)
		return
	}
	fmt.Printf("report written: %s\n", c.report)
}

// writeCellReport writes one run's report, named for its arm and number so no
// run overwrites another.
func writeCellReport(dir string, story Storyline, arm string, n int, res *Result, model string) (string, error) {
	return writeReportAs(dir, fmt.Sprintf("%s-%s-run%d.md", story.Key, arm, n), res, model)
}
