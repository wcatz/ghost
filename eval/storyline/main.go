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

type config struct {
	storyline  string
	repoDir    string
	keep       bool
	judge      bool
	without    bool
	notes      bool
	runs       int
	model      string
	ollamaURL  string
	drainTO    time.Duration
	authFile   string
	resultsDir string
	timeout    time.Duration
	fake       bool
}

func main() {
	var cfg config
	flag.StringVar(&cfg.storyline, "storyline", "reversed-decision", "storyline key to run, a comma-separated list, or all; see storylines() in eval/storyline/storyline.go for the registry")
	flag.StringVar(&cfg.repoDir, "repo", ".", "ghost repo root containing cmd/ghost")
	flag.BoolVar(&cfg.keep, "keep", false, "keep the scratch dir (the store, the built binary and the logs) after the run")
	flag.BoolVar(&cfg.judge, "judge", false, "add the opt-in LLM judge over the final session's answer")
	flag.BoolVar(&cfg.without, "without-ghost", false, "also run the without-Ghost arm: the same scripts, model and saves, with an empty session block, reported side by side with the Ghost arm")
	flag.BoolVar(&cfg.notes, "flat-notes", false, "also run the flat-notes arm: the same scripts, model and saves, with every record saved so far handed over as a plain notes file (a host's built-in file memory), reported beside the other arms")
	flag.IntVar(&cfg.runs, "runs", 1, "runs per arm per storyline; the issue's target numbers are quoted for 10")
	flag.StringVar(&cfg.model, "model", "", "opencode model for the sessions (empty keeps Ghost's own default, opencode/big-pickle)")
	flag.StringVar(&cfg.ollamaURL, "ollama", "http://localhost:11434", "Ollama base URL for the reachability check")
	flag.DurationVar(&cfg.drainTO, "drain-timeout", 3*time.Minute, "max wait for the embedding drain the arc stages need")
	flag.StringVar(&cfg.authFile, "opencode-auth-file", "", "optional path to an opencode auth.json copied into the scratch data dir so sandboxed sessions authenticate")
	flag.StringVar(&cfg.resultsDir, "results-dir", "eval/storyline/results", "directory for the run report")
	flag.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "max time for one session or one arc stage")
	flag.BoolVar(&cfg.fake, "fake-harness", false, "run with no model at all: sessions get a fixed answer, and supersede/resolve reach a deterministic fake opencode whose classifier is an oracle built from the storyline's own annotations; only the block and store lines are graded")
	flag.Parse()

	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

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

type cell struct {
	story     Storyline
	arm       string
	run       int
	res       *Result
	err       error // the run's own error; nil means res holds a graded result
	report    string
	reportErr error
}

func run(cfg config) error {
	stories, err := selectStorylines(cfg.storyline)
	if err != nil {
		return err
	}
	for _, story := range stories {
		if err := story.Validate(); err != nil {
			return err
		}
	}
	if cfg.fake && cfg.judge {
		return fmt.Errorf("-judge asks a model, and -fake-harness runs without one")
	}
	if cfg.fake && cfg.notes {
		return fmt.Errorf("-flat-notes grades answers only, and -fake-harness answers with a fixed string")
	}
	if cfg.runs < 1 {
		return fmt.Errorf("-runs must be at least 1, got %d", cfg.runs)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	repoDir, err := filepath.Abs(cfg.repoDir)
	if err != nil {
		return fmt.Errorf("resolve -repo: %w", err)
	}
	sandboxRoot := filepath.Join(repoDir, ".sandbox")
	if err := os.MkdirAll(sandboxRoot, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", sandboxRoot, err)
	}
	model := resolveModel(cfg.model)

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
	var fakeBin string
	if cfg.fake {
		fakeBin = filepath.Join(buildDir, "fakeharness")
		if err := buildFakeHarness(ctx, repoDir, fakeBin); err != nil {
			return err
		}
	}

	arms := []string{armWithGhost}
	if cfg.without {
		arms = append(arms, armWithoutGhost)
	}
	if cfg.notes {
		arms = append(arms, armFlatNotes)
	}
	var cells []cell
	for _, story := range stories {
		for n := 1; n <= cfg.runs; n++ {
			for _, arm := range arms {
				c := cell{story: story, arm: arm, run: n}
				c.res, c.err = runCell(ctx, cfg, sandboxRoot, bin, fakeBin, model, story, arm, n)
				if c.err != nil {
					fmt.Printf("%s %s run %d: ERROR: %v\n", story.Key, arm, n, c.err)
				} else {
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

	failed := exitFailures(cells)
	if len(failed) > 0 {
		return fmt.Errorf("%d run(s) failed: %s", len(failed), strings.Join(failed, "; "))
	}
	return nil
}

func runCell(ctx context.Context, cfg config, sandboxRoot, bin, fakeBin, model string, story Storyline, arm string, n int) (*Result, error) {
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
	if cfg.fake {
		dir, err := installFakeHarness(scratch, fakeBin, story)
		if err != nil {
			return nil, err
		}
		env = prependPath(env, dir)
		fmt.Printf("fake harness: %s (oracle: %d pair(s))\n", dir, len(oraclePairs(story)))
	}

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
	defer ghost.abort()

	var judge Agent
	if cfg.judge {
		judge = newAgent(model)
	}
	agent := newAgent(model)
	if cfg.fake {
		agent = fixedAgent{}
	}
	r := &Run{
		Story:        story,
		WorkDir:      workDir,
		Ghost:        ghost,
		Agent:        agent,
		Judge:        judge,
		WithoutGhost: arm == armWithoutGhost,
		FlatNotes:    arm == armFlatNotes,
		FakeHarness:  cfg.fake,
		Timeout:      cfg.timeout,
		StageGap:     stageGap,
		Out:          os.Stdout,
	}
	return r.Execute(ctx)
}

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

func writeCellReport(dir string, story Storyline, arm string, n int, res *Result, model string) (string, error) {
	return writeReportAs(dir, fmt.Sprintf("%s-%s-run%d.md", story.Key, arm, n), res, model)
}

func exitFailures(cells []cell) []string {
	var failed []string
	for _, c := range cells {
		if c.reportErr != nil {
			failed = append(failed, fmt.Sprintf("%s %s run %d: report not written: %v", c.story.Key, c.arm, c.run, c.reportErr))
		}
		switch {
		case c.err != nil:
			failed = append(failed, fmt.Sprintf("%s %s run %d errored", c.story.Key, c.arm, c.run))
		case c.arm == armWithGhost && c.res != nil && !c.res.Passed():
			failed = append(failed, fmt.Sprintf("%s run %d: %s", c.story.Key, c.run, strings.Join(c.res.FailedNames(), ", ")))
		}
	}
	return failed
}
