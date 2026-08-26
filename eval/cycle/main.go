// Command evalcycle grades Ghost's staleness pipeline end-to-end. It injects
// an annotated memory corpus into a scratch-isolated ghost instance through
// the real MCP save path, runs `ghost supersede`, `ghost resolve`, and
// `ghost reflect --tier opencode` against it, grades each stage's behavior
// against the corpus annotations, and writes a Markdown scorecard.
//
// Usage:
//
//	go run ./eval/cycle [--corpus eval/cycle/corpus/corpus.jsonl]
//	    [--project acme-migration] [--repo .] [--keep] [--skip-reflect]
//	    [--drain-timeout 3m] [--ollama http://localhost:11434]
//	    [--results-dir eval/cycle/results]
//	    [--floors "supersede_precision=0.60,..."]
//	    [--opencode-auth-file path/to/auth.json]
//
// Every ghost process runs with XDG_DATA_HOME/XDG_CONFIG_HOME inside a
// throwaway temp dir and with ANTHROPIC_API_KEY stripped, so nothing touches
// the production DB and no Anthropic credits are spent: classify/consolidate
// ride the claude-or-opencode CLI path. Set GHOST_OPENCODE_MODEL to pin the
// opencode subprocess model.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wcatz/ghost/eval/cycle/corpus"
)

type config struct {
	corpusPath  string
	project     string
	repoDir     string
	keep        bool
	skipReflect bool
	drainTO     time.Duration
	ollamaURL   string
	resultsDir  string
	gradeOnly   string // existing scratch dir: re-grade only (no ghost processes)
	floors      string // comma-separated metric=min floors over stage P/R
	authFile    string // opencode auth.json to seed into the scratch data dir
	floorMap    map[string]float64
}

func main() {
	var cfg config
	flag.StringVar(&cfg.corpusPath, "corpus", "eval/cycle/corpus/corpus.jsonl", "path to the annotated corpus JSONL")
	flag.StringVar(&cfg.project, "project", "acme-migration", "ghost project name for the run")
	flag.StringVar(&cfg.repoDir, "repo", ".", "ghost repo root containing cmd/ghost")
	flag.BoolVar(&cfg.keep, "keep", false, "keep the scratch dir after the run")
	flag.BoolVar(&cfg.skipReflect, "skip-reflect", false, "run supersede+resolve only (skips consolidation)")
	flag.DurationVar(&cfg.drainTO, "drain-timeout", 3*time.Minute, "max wait for embedding drain")
	flag.StringVar(&cfg.ollamaURL, "ollama", "http://localhost:11434", "Ollama base URL for the reachability check")
	flag.StringVar(&cfg.resultsDir, "results-dir", "eval/cycle/results", "directory for dated reports")
	flag.StringVar(&cfg.gradeOnly, "grade-only", "", "existing kept scratch dir: re-grade final state + saved raw outputs, run nothing")
	flag.StringVar(&cfg.floors, "floors", "", "comma-separated metric=min floors over stage P/R, e.g. \"supersede_precision=0.60,resolve_recall=0.30\" (keys: supersede_precision, supersede_recall, resolve_precision, resolve_recall); a violation fails the run after the report is written")
	flag.StringVar(&cfg.authFile, "opencode-auth-file", "", "path to an opencode auth.json copied into the scratch data dir so sandboxed LLM stages authenticate (empty: local runs need none)")
	flag.Parse()
	floors, err := parseFloors(cfg.floors)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	cfg.floorMap = floors
	if err := run(cfg); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	if cfg.gradeOnly != "" {
		return gradeOnlyRun(cfg)
	}
	entries, err := corpus.Load(cfg.corpusPath)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	if err := corpus.Validate(entries); err != nil {
		return fmt.Errorf("validate corpus: %w", err)
	}
	fmt.Printf("corpus: %d entries from %s\n", len(entries), cfg.corpusPath)

	scratch, err := os.MkdirTemp("", "ghost-evalcycle-*")
	if err != nil {
		return err
	}
	if !cfg.keep {
		defer func() { _ = os.RemoveAll(scratch) }()
	}
	for _, sub := range []string{"data", "config"} {
		if err := os.MkdirAll(filepath.Join(scratch, sub), 0o700); err != nil {
			return err
		}
	}
	if err := seedOpencodeAuth(scratch, cfg.authFile); err != nil {
		return err
	}
	env := scratchEnv(scratch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("building ghost binary...")
	ghostBin := filepath.Join(scratch, "ghost")
	build := exec.CommandContext(ctx, "go", "build", "-o", ghostBin, "./cmd/ghost")
	build.Dir = cfg.repoDir
	var buildErr bytes.Buffer
	build.Stderr = &buildErr
	if err := build.Run(); err != nil {
		return fmt.Errorf("go build: %w: %s", err, strings.TrimSpace(buildErr.String()))
	}

	dbPath := filepath.Join(scratch, "data", "ghost", "ghost.db")

	fmt.Println("injecting corpus via ghost mcp...")
	mcpSess, err := startMCP(ctx, ghostBin, env)
	if err != nil {
		return err
	}
	ids, err := saveAll(ctx, mcpSess, cfg.project, entries)
	if err != nil {
		return fmt.Errorf("inject: %w", err)
	}
	if err := restampChronology(dbPath, cfg.project, entries, ids); err != nil {
		return fmt.Errorf("restamp chronology: %w", err)
	}
	if err := writeIDsFile(scratch, ids); err != nil {
		return fmt.Errorf("persist ids: %w", err)
	}
	meta := fmt.Sprintf("skip_reflect: %v\n", cfg.skipReflect)
	_ = os.WriteFile(filepath.Join(scratch, "meta.yaml"), []byte(meta), 0o644)

	// The embedding worker lives INSIDE the ghost mcp process — the session
	// must stay open until embeddings drain or nothing will ever embed.
	fmt.Println("waiting for embeddings to drain...")
	if err := waitForEmbeddings(ctx, cfg.ollamaURL, dbPath, cfg.project, cfg.drainTO); err != nil {
		return err
	}
	mcpSess.close()

	rep := ReportData{
		Date:        time.Now().Format("2006-01-02 15:04"),
		CorpusPath:  cfg.corpusPath,
		Project:     cfg.project,
		Total:       len(entries),
		ScratchDir:  scratch,
		SkipReflect: cfg.skipReflect,
	}

	fmt.Println("stage: supersede")
	// Explicit deadlines: the CLI LLM clients only apply their own short
	// default timeout when the caller's context has none — a deadline here
	// both bounds the stage and lets classify calls run past that default.
	supCtx, supCancel := context.WithTimeout(ctx, 8*time.Minute)
	supOut, supErrOut, err := runGhost(supCtx, env, ghostBin, "supersede", cfg.project, "--apply")
	supCancel()
	if err != nil {
		return fmt.Errorf("supersede: %w", err)
	}
	writeRaw(cfg.resultsDir, "supersede", supOut+supErrOut)
	_ = os.WriteFile(filepath.Join(scratch, "supersede.out.txt"), []byte(supOut+supErrOut), 0o644)
	rep.Supersede = gradeSupersede(ids, entries, parseSupersedeLines(supOut))
	reportStage("supersede", rep.Supersede)

	fmt.Println("stage: resolve")
	resCtx, resCancel := context.WithTimeout(ctx, 8*time.Minute)
	resOut, resErrOut, err := runGhost(resCtx, env, ghostBin, "resolve", cfg.project, "--apply")
	resCancel()
	if err != nil {
		return fmt.Errorf("resolve: %w", err)
	}
	writeRaw(cfg.resultsDir, "resolve", resOut+resErrOut)
	_ = os.WriteFile(filepath.Join(scratch, "resolve.out.txt"), []byte(resOut+resErrOut), 0o644)
	rep.Resolve = gradeResolve(entries, parseResolveIDs(resOut), ids)
	reportStage("resolve", rep.Resolve)

	if !cfg.skipReflect {
		fmt.Println("stage: reflect (--tier opencode)")
		rowsBefore, ferr := fetchMemRows(dbPath, cfg.project)
		if ferr != nil {
			return fmt.Errorf("pre-reflect state: %w", ferr)
		}
		refCtx, refCancel := context.WithTimeout(ctx, 15*time.Minute)
		// --allow-drops: in the scratch DB, guarded-category deletions are
		// data the reflect grader measures, not production harm.
		refOut, refErrOut, rerr := runGhost(refCtx, env, ghostBin, "reflect", cfg.project, "--tier", "opencode", "--apply", "--allow-drops")
		refCancel()
		if rerr != nil && strings.Contains(rerr.Error(), "SQLITE_BUSY") {
			time.Sleep(5 * time.Second)
			refCtx2, refCancel2 := context.WithTimeout(ctx, 15*time.Minute)
			refOut, refErrOut, rerr = runGhost(refCtx2, env, ghostBin, "reflect", cfg.project, "--tier", "opencode", "--apply", "--allow-drops")
			refCancel2()
		}
		if rerr != nil {
			return fmt.Errorf("reflect: %w\noutput:\n%s\nstderr:\n%s", rerr, refOut, refErrOut)
		}
		writeRaw(cfg.resultsDir, "reflect", refOut+refErrOut)
		_ = os.WriteFile(filepath.Join(scratch, "reflect.out.txt"), []byte(refOut+refErrOut), 0o644)
		rowsAfter, ferr := fetchMemRows(dbPath, cfg.project)
		if ferr != nil {
			return fmt.Errorf("post-reflect state: %w", ferr)
		}
		rep.Reflect = gradeReflect(entries, rowsBefore, rowsAfter)
		reportReflect(rep.Reflect)
	} else {
		fmt.Println("stage: reflect skipped (--skip-reflect)")
	}

	path, err := writeReport(cfg.resultsDir, rep)
	if err != nil {
		return err
	}
	fmt.Printf("report written: %s\n", path)
	if cfg.keep {
		fmt.Printf("scratch kept: %s\n", scratch)
	}
	return failOnFloors(cfg.floorMap, rep)
}

// stageMetrics flattens the graded stages into the key map -floors checks.
func stageMetrics(rep ReportData) map[string]float64 {
	return map[string]float64{
		"supersede_precision": rep.Supersede.Precision(),
		"supersede_recall":    rep.Supersede.Recall(),
		"resolve_precision":   rep.Resolve.Precision(),
		"resolve_recall":      rep.Resolve.Recall(),
	}
}

// failOnFloors returns an error describing every floor violation so CI fails
// AFTER the report is written — artifacts must exist for triage even when the
// grade is bad. Floors are never exact-match assertions: LLM stages are
// nondeterministic (observed resolve R wobble ±0.25 across identical inputs).
func failOnFloors(floors map[string]float64, rep ReportData) error {
	if len(floors) == 0 {
		return nil
	}
	violations := checkFloors(stageMetrics(rep), floors)
	for _, v := range violations {
		fmt.Printf("FLOOR VIOLATION: %s\n", v)
	}
	if len(violations) > 0 {
		return fmt.Errorf("%d floor violation(s):\n%s", len(violations), strings.Join(violations, "\n"))
	}
	return nil
}

// gradeOnlyRun re-grades a previously completed (kept) run: fetches the final
// memory state from that scratch DB and re-parses the saved raw stage outputs,
// then writes a fresh report. No ghost processes are started.
func gradeOnlyRun(cfg config) error {
	entries, err := corpus.Load(cfg.corpusPath)
	if err != nil {
		return fmt.Errorf("load corpus: %w", err)
	}
	if err := corpus.Validate(entries); err != nil {
		return fmt.Errorf("validate corpus: %w", err)
	}
	dbPath := filepath.Join(cfg.gradeOnly, "data", "ghost", "ghost.db")
	ids, err := readIDsFile(cfg.gradeOnly)
	if err != nil {
		return fmt.Errorf("load ids (pre-ids.json scratch?): %w", err)
	}
	rep := ReportData{
		Date:        time.Now().Format("2006-01-02 15:04") + " (grade-only)",
		CorpusPath:  cfg.corpusPath,
		Project:     cfg.project,
		Total:       len(entries),
		ScratchDir:  cfg.gradeOnly,
		SkipReflect: cfg.skipReflect,
	}
	// Raw outputs and run metadata are bound to the scratch dir — a regrade
	// can never mix outputs from a different run or silently skip a stage.
	supOut, err := os.ReadFile(filepath.Join(cfg.gradeOnly, "supersede.out.txt"))
	if err != nil {
		return fmt.Errorf("supersede output missing from scratch (did the stage run?): %w", err)
	}
	rep.Supersede = gradeSupersede(ids, entries, parseSupersedeLines(string(supOut)))
	reportStage("supersede", rep.Supersede)
	resOut, err := os.ReadFile(filepath.Join(cfg.gradeOnly, "resolve.out.txt"))
	if err != nil {
		return fmt.Errorf("resolve output missing from scratch (did the stage run?): %w", err)
	}
	rep.Resolve = gradeResolve(entries, parseResolveIDs(string(resOut)), ids)
	reportStage("resolve", rep.Resolve)

	meta, merr := os.ReadFile(filepath.Join(cfg.gradeOnly, "meta.yaml"))
	skipReflect := merr == nil && strings.Contains(string(meta), "true")
	if !skipReflect {
		rowsAfter, ferr := fetchMemRows(dbPath, cfg.project)
		if ferr != nil {
			return fmt.Errorf("final state: %w", ferr)
		}
		rep.Reflect = gradeReflect(entries, nil, rowsAfter)
		reportReflect(rep.Reflect)
	}
	rep.SkipReflect = skipReflect
	path, err := writeReport(cfg.resultsDir, rep)
	if err != nil {
		return err
	}
	fmt.Printf("report written: %s\n", path)
	return failOnFloors(cfg.floorMap, rep)
}

// writeRaw persists one stage's raw CLI output next to the report so misses
// can be judged against exactly what the stage printed.
func writeRaw(dir, stage, out string) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	name := filepath.Join(dir, time.Now().Format("2006-01-02")+"-"+stage+".out.txt")
	_ = os.WriteFile(name, []byte(out), 0o644) //nolint:errcheck // diagnostics only
}

func reportStage(name string, st Stage) {
	fmt.Printf("  %s: TP=%d FP=%d FN=%d causes=%d P=%.2f R=%.2f\n",
		name, st.TP, st.FP, st.FN, st.Causes, st.Precision(), st.Recall())
	for _, m := range st.Misses {
		fmt.Printf("    MISS [%s] %s: %s\n", m.Kind, m.Key, m.Detail)
	}
}

func reportReflect(rr ReflectReport) {
	fmt.Printf("  reflect: memories %d -> %d\n", rr.Before, rr.After)
	for _, g := range rr.Groups {
		fmt.Printf("    group %-18s %s (%s)\n", g.Group, g.Status, g.Survivors)
	}
	for _, k := range rr.DroppedDistractors {
		fmt.Printf("    DROPPED distractor: %s\n", k)
	}
}

// scratchEnv builds the child-process env for ghost: inherited vars minus any
// pre-existing XDG overrides and ANTHROPIC_API_KEY, plus the scratch pair. The
// strip is what forces classify down the CLI path — zero Anthropic spend.
func scratchEnv(scratch string) []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		switch {
		case strings.HasPrefix(kv, "XDG_DATA_HOME="),
			strings.HasPrefix(kv, "XDG_CONFIG_HOME="),
			strings.HasPrefix(kv, "ANTHROPIC_API_KEY="):
			continue
		}
		env = append(env, kv)
	}
	return append(env,
		"XDG_DATA_HOME="+filepath.Join(scratch, "data"),
		"XDG_CONFIG_HOME="+filepath.Join(scratch, "config"),
	)
}

// runGhost runs one ghost CLI command in the scratch environment, returning
// stdout and stderr separately — stderr carries warnings (e.g. the guarded-
// drop audit) worth persisting even when the command succeeds.
func runGhost(ctx context.Context, env []string, bin string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), errb.String(), fmt.Errorf("ghost %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), errb.String(), nil
}

// writeIDsFile persists the corpus-key → memory-id mapping inside the kept
// scratch dir so --grade-only can re-map CLI id prefixes to keys later.
func writeIDsFile(scratch string, ids map[string]string) error {
	b, err := json.MarshalIndent(ids, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(scratch, "ids.json"), b, 0o644)
}

func readIDsFile(scratch string) (map[string]string, error) {
	b, err := os.ReadFile(filepath.Join(scratch, "ids.json"))
	if err != nil {
		return nil, err
	}
	ids := map[string]string{}
	if err := json.Unmarshal(b, &ids); err != nil {
		return nil, err
	}
	return ids, nil
}
