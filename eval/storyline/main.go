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
	model      string
	ollamaURL  string
	drainTO    time.Duration
	authFile   string
	resultsDir string
	timeout    time.Duration
	runs       int
}

// armResult holds the result of one arm run.
type armResult struct {
	name    string
	res     *Result
	err     error
	scratch string
}

func main() {
	var cfg config
	flag.StringVar(&cfg.storyline, "storyline", "reversed-decision", "storyline key to run; see storylines() in eval/storyline/storyline.go for the registry")
	flag.StringVar(&cfg.repoDir, "repo", ".", "ghost repo root containing cmd/ghost")
	flag.BoolVar(&cfg.keep, "keep", false, "keep the scratch dir (the store, the built binary and the logs) after the run")
	flag.BoolVar(&cfg.judge, "judge", false, "add the opt-in LLM judge over the final session's answer")
	flag.BoolVar(&cfg.without, "without-ghost", false, "run the without-Ghost arm: same scripts, same model, empty block (not a missing hook)")
	flag.StringVar(&cfg.model, "model", "", "opencode model for the sessions (empty keeps Ghost's own default, opencode/big-pickle)")
	flag.StringVar(&cfg.ollamaURL, "ollama", "http://localhost:11434", "Ollama base URL for the reachability check")
	flag.DurationVar(&cfg.drainTO, "drain-timeout", 3*time.Minute, "max wait for the embedding drain the arc stages need")
	flag.StringVar(&cfg.authFile, "opencode-auth-file", "", "optional path to an opencode auth.json copied into the scratch data dir so sandboxed sessions authenticate")
	flag.StringVar(&cfg.resultsDir, "results-dir", "eval/storyline/results", "directory for the run report")
	flag.DurationVar(&cfg.timeout, "timeout", defaultTimeout, "max time for one session or one arc stage")
	flag.IntVar(&cfg.runs, "runs", 1, "number of runs per arm (default 1; coordinator uses 10)")
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
	// Resolved once, here, before anything is built or spawned: the sessions, the
	// arc's phases and the report all have to name ONE model, and leaving the
	// choice to a fallback would let an inherited GHOST_OPENCODE_MODEL decide the
	// sessions alone (see resolveModel).
	model := resolveModel(cfg.model)

	// The without-Ghost arm uses an empty block (not a missing hook). Both arms
	// run on the same model and report side by side per arc with each session's
	// block size.
	arms := []struct {
		name      string
		withGhost bool
	}{
		{"with-ghost", true},
		{"without-ghost", false},
	}
	if cfg.without {
		arms = []struct {
			name      string
			withGhost bool
		}{{"without-ghost", false}}
	}

	var results []armResult

	for _, arm := range arms {
		for runIdx := 1; runIdx <= cfg.runs; runIdx++ {
			scratch, err := os.MkdirTemp(sandboxRoot, fmt.Sprintf("storyline-%s-%s-run%d-*", story.Key, arm.name, runIdx))
			if err != nil {
				return fmt.Errorf("create scratch dir for %s run %d: %w", arm.name, runIdx, err)
			}
			if !cfg.keep {
				defer func(s string) { _ = os.RemoveAll(s) }(scratch)
			}
			dirs, err := makeScratchLayout(scratch)
			if err != nil {
				return err
			}
			if err := seedOpencodeAuth(scratch, cfg.authFile); err != nil {
				return err
			}
			env := scratchEnv(scratch, model)

			workDir := filepath.Join(dirs["work"], story.Project)
			if err := os.MkdirAll(workDir, 0o700); err != nil {
				return fmt.Errorf("create work dir: %w", err)
			}

			fmt.Printf("storyline: %s (%s) — %s [arm: %s, run: %d/%d]\n", story.Key, story.Project, story.Title, arm.name, runIdx, cfg.runs)
			fmt.Printf("scratch: %s\n", scratch)

			var ghost Ghost
			var bin string
			if arm.withGhost {
				fmt.Println("building ghost binary from the checkout under test...")
				bin = filepath.Join(scratch, "ghost")
				if err := buildGhost(ctx, repoDir, bin); err != nil {
					results = append(results, armResult{name: arm.name, err: err, scratch: scratch})
					continue
				}
				ghost = newBinaryGhost(bin, env, story.Project, workDir,
					filepath.Join(dirs["data"], "ghost", "ghost.db"), cfg.ollamaURL, cfg.drainTO, os.Stderr)
				if err := ghost.Start(ctx); err != nil {
					results = append(results, armResult{name: arm.name, err: err, scratch: scratch})
					continue
				}
				defer ghost.Abort()
			} else {
				// Without-Ghost arm: no ghost binary, no MCP, empty block
				ghost = newEmptyGhost()
			}

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
				Timeout:      cfg.timeout,
				Out:          os.Stdout,
				WithoutGhost: !arm.withGhost,
			}
			res, err := r.Execute(ctx)
			if err != nil {
				results = append(results, armResult{name: arm.name, err: err, scratch: scratch})
				continue
			}
			results = append(results, armResult{name: arm.name, res: res, scratch: scratch})
		}
	}

	// Print side-by-side summary per arc
	fmt.Println("\n=== SIDE-BY-SIDE SUMMARY ===")
	for _, arm := range arms {
		armResults := filterResults(results, arm.name)
		if len(armResults) == 0 {
			continue
		}
		fmt.Printf("\n--- %s (%d runs) ---\n", arm.name, len(armResults))
		for i, ar := range armResults {
			if ar.err != nil {
				fmt.Printf("  run %d: ERROR: %v\n", i+1, ar.err)
				continue
			}
			// Print block sizes per session
			var blockSizes []string
			for _, sess := range ar.res.Sessions {
				blockSizes = append(blockSizes, fmt.Sprintf("%dB", len(sess.Block)))
			}
			fmt.Printf("  run %d: blocks=%s checks=%d/%d\n", i+1, strings.Join(blockSizes, ","), countPassed(ar.res.Checks), len(ar.res.Checks))
		}
	}

	// Write combined report
	path, err := writeCombinedReport(cfg.resultsDir, story, model, results, cfg.runs)
	if err != nil {
		return err
	}
	fmt.Printf("\nreport written: %s\n", path)
	if cfg.keep {
		fmt.Printf("scratch roots kept under: %s\n", sandboxRoot)
	}

	// Check if any arm had failures
	for _, ar := range results {
		if ar.err != nil {
			return fmt.Errorf("arm %s had errors", ar.name)
		}
		if ar.res != nil && !ar.res.Passed() {
			return fmt.Errorf("arm %s: %d of %d checks failed: %s",
				ar.name, len(ar.res.FailedNames()), len(ar.res.Checks), strings.Join(ar.res.FailedNames(), ", "))
		}
	}
	return nil
}

func filterResults(results []armResult, name string) []armResult {
	var out []armResult
	for _, r := range results {
		if r.name == name {
			out = append(out, r)
		}
	}
	return out
}

func countPassed(checks []Check) int {
	n := 0
	for _, c := range checks {
		if c.Passed {
			n++
		}
	}
	return n
}

func writeCombinedReport(dir string, story Storyline, model string, results []armResult, runs int) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create results dir: %w", err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Storyline Evaluation Report: %s\n\n", story.Key)
	fmt.Fprintf(&b, "- storyline: %s\n", story.Key)
	fmt.Fprintf(&b, "- title: %s\n", story.Title)
	fmt.Fprintf(&b, "- project: %s\n", story.Project)
	fmt.Fprintf(&b, "- model: %s\n", model)
	fmt.Fprintf(&b, "- runs per arm: %d\n", runs)
	fmt.Fprintf(&b, "- arms: with-ghost, without-ghost\n\n")

	// Group results by arm
	arms := map[string][]armResult{}
	for _, r := range results {
		arms[r.name] = append(arms[r.name], r)
	}

	// Side-by-side summary table
	b.WriteString("## Side-by-Side Summary\n\n")
	b.WriteString("| Arm | Run | Block Sizes (per session) | Checks Passed | Total Checks |\n")
	b.WriteString("|-----|-----|---------------------------|---------------|--------------|\n")
	for _, armName := range []string{"with-ghost", "without-ghost"} {
		armResults := arms[armName]
		for i, ar := range armResults {
			if ar.err != nil {
				fmt.Fprintf(&b, "| %s | %d | ERROR: %v | - | - |\n", armName, i+1, ar.err)
				continue
			}
			var blockSizes []string
			for _, sess := range ar.res.Sessions {
				blockSizes = append(blockSizes, fmt.Sprintf("%dB", len(sess.Block)))
			}
			passed := countPassed(ar.res.Checks)
			total := len(ar.res.Checks)
			fmt.Fprintf(&b, "| %s | %d | %s | %d | %d |\n", armName, i+1, strings.Join(blockSizes, ", "), passed, total)
		}
	}

	b.WriteString("\n## Per-Arm Details\n\n")
	for _, armName := range []string{"with-ghost", "without-ghost"} {
		armResults := arms[armName]
		if len(armResults) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n", armName)
		for runIdx, ar := range armResults {
			if ar.err != nil {
				fmt.Fprintf(&b, "#### Run %d: ERROR\n\n```\n%v\n```\n\n", runIdx+1, ar.err)
				continue
			}
			res := ar.res
			verdict := "FAIL"
			if res.Passed() {
				verdict = "PASS"
			}
			fmt.Fprintf(&b, "#### Run %d: %s\n\n", runIdx+1, verdict)
			fmt.Fprintf(&b, "- work dir: %s\n", res.WorkDir)
			fmt.Fprintf(&b, "- judged: %v\n", res.Judged)
			if failed := res.FailedNames(); len(failed) > 0 {
				fmt.Fprintf(&b, "- failed checks: %s\n", strings.Join(failed, ", "))
			}

			b.WriteString("\n##### Checks\n\n")
			if len(res.Checks) == 0 {
				b.WriteString("no checks were graded\n")
			} else {
				for _, c := range res.Checks {
					mark := "FAIL"
					if c.Passed {
						mark = "PASS"
					}
					fmt.Fprintf(&b, "- %s %s — %s\n", mark, c.Name, oneLine(c.Detail))
				}
			}

			b.WriteString("\n##### Sessions\n\n")
			for _, sess := range res.Sessions {
				fmt.Fprintf(&b, "\n###### Session %d\n\n", sess.Index+1)
				fmt.Fprintf(&b, "injection (%d bytes):\n\n```\n%s```\n\n", len(sess.Block), sess.Block)
				fmt.Fprintf(&b, "answer:\n\n```\n%s\n```\n\n", strings.TrimSpace(sess.Answer))
			}

			if !res.WithoutGhost {
				fmt.Fprintf(&b, "\n##### Stage: ghost supersede --apply\n\n```\n%s```\n", res.Supersede)
				fmt.Fprintf(&b, "\n##### Stage: ghost resolve --apply\n\n```\n%s```\n", res.Resolve)
			}

			fmt.Fprintf(&b, "\n##### Final Injection (%d bytes)\n\n```\n%s```\n", len(res.FinalBlock), res.FinalBlock)
			if res.Judged {
				fmt.Fprintf(&b, "\n##### Judge\n\n```\n%s\n```\n", strings.TrimSpace(res.Verdict))
			}
			b.WriteString("\n---\n\n")
		}
	}

	path := filepath.Join(dir, story.Key+"-combined.md")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("write report: %w", err)
	}
	return path, nil
}
