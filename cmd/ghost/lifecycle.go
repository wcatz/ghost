package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/resolve"
	"github.com/wcatz/ghost/internal/scratch"
	"github.com/wcatz/ghost/internal/supersede"
)

// runLifecycle runs the enabled auto-consolidation phases for one project, in
// order, inside a single process: reflect (which rewrites memories), then
// resolve (which stamps resolved_at), then supersede (which links memories).
//
// Each phase is a separate `ghost` child process, so a failure in one phase
// cannot corrupt the next, and a failing phase does not abort the remaining
// ones — the stop hook spawns this detached and never reads its exit status,
// so this log (stderr below) plus the lifecycle-last-failure marker recorded
// at the end of the run — which the next session-start surfaces as an alert —
// are the record. Running them sequentially here is the whole point: spawned
// as three independent processes they raced, and reflect's rewrite could
// replace rows while supersede was classifying them (foreign-key aborts and
// lost resolved_at stamps).
//
// Internal subcommand: not listed in help.
func runLifecycle() {
	projectName, source, err := parseLifecycleArgs(os.Args[2:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	// Hold the per-project lifecycle claim for the whole chain. The claim is
	// keyed on the RESOLVED project id, so a manual run by name and a
	// hook-spawned run (which passes the id) contend for the same file. A lock
	// that cannot be evaluated at all is a warning, not a stop: the phases are
	// convergent, and a skipped maintenance run is worse than a rare overlap.
	release, ok, lockErr := mcpinit.AcquireLifecycleLock(projectName)
	switch {
	case lockErr != nil:
		fmt.Fprintf(os.Stderr, "lifecycle: warning: could not take the per-project lock (%v); continuing unlocked\n", lockErr)
	case !ok:
		fmt.Fprintf(os.Stderr, "lifecycle: another lifecycle is already running for project %s; exiting\n", projectName)
		return
	}
	defer release()

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: locate ghost binary: %v\n", err)
		os.Exit(1)
	}

	// Consolidation is only worth an unattended write when a real LLM tier is
	// reachable: without one, reflect's --tier auto would fall through to the
	// Jaccard-only sqlite tier and rewrite every non-manual memory for no
	// quality gain. resolve/supersede have no local fallback, so a missing
	// harness simply makes their phase fail fast and be logged.
	llmOK := ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary).Available() ||
		ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary).Available()

	reflectSkipped := cfg.Reflection.AutoReflect && !llmOK
	if reflectSkipped {
		fmt.Fprintln(os.Stderr, "lifecycle: skipping reflect — no CLI binary available")
	}

	// Stale per-invocation scratch dirs from crashed harness children would
	// otherwise accumulate between lifecycles; reap before this run's own
	// children start creating new ones. A reap failure is a warning, not a
	// stop: the phases are the point of this process and they converge.
	if removed, reapErr := scratch.Reap(24 * time.Hour); reapErr != nil {
		fmt.Fprintf(os.Stderr, "lifecycle: warning: scratch reap failed: %v\n", reapErr)
	} else {
		fmt.Fprintf(os.Stderr, "lifecycle: scratch reap removed=%d\n", removed)
	}

	// Outcome tracking for the end-of-run marker: a phase that could not run
	// (skipped for want of an LLM backend — the original silent incident) or
	// exited non-zero counts as failed; success counts only phases that
	// actually ran.
	phasesRan := 0
	var failedPhases []string
	var firstPhaseErr string
	recordFailure := func(phase, msg string) {
		failedPhases = append(failedPhases, phase)
		if firstPhaseErr == "" {
			firstPhaseErr = msg
		}
	}
	if reflectSkipped {
		recordFailure("reflect", mcpinit.NoLLMBackendError)
	}

	for _, ph := range lifecyclePhases(cfg, projectName, llmOK) {
		phaseArgs := append([]string{}, ph.args...)
		if source != "" {
			phaseArgs = append(phaseArgs, "--source", source)
		}
		// One context per branch, so nothing is created and then abandoned.
		var ctx context.Context
		var cancel context.CancelFunc
		if ph.timeout > 0 {
			ctx, cancel = context.WithTimeout(context.Background(), ph.timeout)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
		cmd := exec.CommandContext(ctx, exe, phaseArgs...)
		// A deadline must not SIGKILL a phase mid-write or orphan the CLI
		// harness it spawned: the phase runs in its own process group, gets a
		// SIGTERM first, and only the whole group is force-killed if it misses
		// the grace period.
		setPhaseProcessGroup(cmd)
		cmd.Cancel = func() error { return terminatePhaseProcess(cmd) }
		// WaitDelay is a backstop for Wait itself; the group-wide escalation is
		// the watchdog below, because WaitDelay would kill only the direct child
		// and leave a harness grandchild that ignored SIGTERM orphaned.
		cmd.WaitDelay = phaseGracePeriod + 5*time.Second
		// Tee into a bounded tail. The child's output still reaches the
		// terminal (and lifecycle.log for a detached run) unchanged — but
		// runErr.Error() alone is "exit status 1", and that is all the marker
		// ever recorded. When a phase dies without saying why, the tail is the
		// only record of what it printed last.
		tail := newPhaseTail(phaseFailureTailMax)
		cmd.Stdout = io.MultiWriter(os.Stdout, tail)
		cmd.Stderr = io.MultiWriter(os.Stderr, tail)

		phaseDone := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				time.Sleep(phaseGracePeriod)
				killPhaseProcess(cmd)
			case <-phaseDone:
			}
		}()

		start := time.Now()
		runErr := cmd.Run()
		close(phaseDone)
		cancel()
		if runErr != nil {
			// The cause, not the exit status: with no output this becomes
			// "exit 1; argv: ghost reflect --project x --apply; ghost dev"
			// instead of the bare "exit status 1" that made 357 log entries
			// indistinguishable.
			cause := phaseFailureCause(phaseArgs, runErr, tail.String())
			fmt.Fprintf(os.Stderr, "lifecycle: %s failed after %s (continuing): %v\n", ph.name, time.Since(start).Round(time.Second), runErr)
			recordFailure(ph.name, cause)
			continue
		}
		phasesRan++
		fmt.Fprintf(os.Stderr, "lifecycle: %s completed in %s\n", ph.name, time.Since(start).Round(time.Second))
	}

	// Record the outcome durably: any failed phase leaves an atomic
	// lifecycle-last-failure.json marker that the next session-start turns
	// into a visible alert (detached runs otherwise speak only to
	// lifecycle.log); a run where every phase that ran succeeded clears an
	// earlier marker. The per-phase stderr lines above are unchanged — for a
	// foreground run they are already the terminal's visible record.
	if err := mcpinit.FinishLifecycleRun(projectName, phasesRan, failedPhases, firstPhaseErr); err != nil {
		fmt.Fprintf(os.Stderr, "lifecycle: warning: could not record run outcome: %v\n", err)
	}
}

// parseLifecycleArgs parses the internal lifecycle subcommand's arguments. The
// project comes from --project when given, so a project whose name begins with
// a dash — or is literally "--source" — cannot be misread as a flag: without
// that, the hook's argv realigned and the write phases ran against a different
// project than the one whose pid file was claimed. A lone positional is still
// accepted for manual use, but a bare dash-leading positional is an unknown
// flag (it is indistinguishable from one; use --project for such names). The
// phase subcommands take the same verbatim --project form, so a dash-prefixed
// name round-trips through the whole chain. Anything unexpected is an error
// rather than being ignored, because a silently misparsed project is a
// wrong-project write.
func parseLifecycleArgs(args []string) (project, source string, err error) {
	projectSet, sourceSet := false, false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--project":
			if projectSet {
				return "", "", fmt.Errorf("--project given more than once")
			}
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--project requires a value")
			}
			project, projectSet = args[i+1], true
			i++
		case "--source":
			if sourceSet {
				return "", "", fmt.Errorf("--source given more than once")
			}
			if i+1 >= len(args) {
				return "", "", fmt.Errorf("--source requires a value")
			}
			source, sourceSet = args[i+1], true
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", fmt.Errorf("unknown flag %q", args[i])
			}
			if projectSet {
				return "", "", fmt.Errorf("unexpected extra argument %q", args[i])
			}
			project, projectSet = args[i], true
		}
	}
	if !projectSet || project == "" {
		return "", "", fmt.Errorf("--project is required (usage: ghost lifecycle --project <name> [--source <src>])")
	}
	return project, source, nil
}

// consolidationContext bounds a single consolidation call by
// reflection.consolidation_timeout_minutes. It exists because the bound used to
// be a hardcoded 3 minutes, which a large project's prompt hits (the opencode
// backend takes about that long for ~190 memories), and a kill there is fatal
// on the autonomous --require-llm path, which has no fallback tier.
//
// Zero or negative means NO bound: context.WithTimeout(parent, 0) would cancel
// the call immediately, which is the opposite of what "unset" should mean.
func consolidationContext(parent context.Context, minutes int) (context.Context, context.CancelFunc) {
	if minutes <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, time.Duration(minutes)*time.Minute)
}

// phaseGracePeriod is how long a phase has to exit after its deadline's
// SIGTERM before its whole process group is force-killed.
const phaseGracePeriod = 30 * time.Second

// lifecyclePhase is one step of the auto-consolidation chain: a ghost
// subcommand, its arguments, and the outer bound on how long it may run. A
// zero timeout means no bound.
type lifecyclePhase struct {
	name    string
	args    []string
	timeout time.Duration
}

// lifecyclePhases returns the enabled phases in execution order: reflect, then
// resolve, then supersede. The order is the contract — consolidation rewrites
// memories, so it has to finish before resolve stamps resolved_at or supersede
// links rows. Reflect is dropped when llmOK is false (no real LLM tier means a
// Jaccard-only rewrite for no quality gain). Extracted from runLifecycle so the
// ordering is unit-testable without spawning processes.
//
// Every phase shares reflection.lifecycle_timeout_minutes, which defaults to a
// generous 60 minutes: unbounded is unsafe because the stop hook keeps one
// per-project PID file keyed to this parent's liveness, so a single hung phase
// would silently disable auto-consolidation for that project until the process
// was killed by hand. The bound is long enough that a legitimately slow pass
// (resolve classifies up to 8 candidates per CLI-harness call, several seconds
// each) completes; set the value to 0 to remove the bound entirely.
//
// The project is passed to every phase with the explicit --project form (not
// positionally) so a dash-prefixed project name is parsed as a name by the
// phase subcommands rather than as a flag — one uniform emission for all
// projects, dash-leading or not.
func lifecyclePhases(cfg *config.Config, projectName string, llmOK bool) []lifecyclePhase {
	timeout := time.Duration(cfg.Reflection.LifecycleTimeoutMinutes) * time.Minute
	var phases []lifecyclePhase
	if cfg.Reflection.AutoReflect && llmOK {
		phases = append(phases, lifecyclePhase{"reflect", []string{"reflect", "--project", projectName, "--apply", "--require-llm", "--skip-unchanged"}, timeout})
	}
	if cfg.Reflection.AutoResolve {
		phases = append(phases, lifecyclePhase{"resolve", []string{"resolve", "--project", projectName, "--apply"}, timeout})
	}
	if cfg.Reflection.AutoSupersede {
		phases = append(phases, lifecyclePhase{"supersede", []string{"supersede", "--project", projectName, "--apply"}, timeout})
	}
	return phases
}

// clampReflectMemories applies the shared content cap (memory.MaxContentLen)
// to consolidation output before it reaches the store, so reflection
// proposals obey the same single-cap contract as MCP saves: content over the
// cap is cut at a rune boundary with the explicit truncation marker appended
// instead of being stored silently. Returns how many contents were cut.
func clampReflectMemories(mems []reflection.ReflectMemory) int {
	cut := 0
	for i := range mems {
		if clamped, wasCut := memory.ClampContent(mems[i].Content); wasCut {
			mems[i].Content = clamped
			cut++
		}
	}
	return cut
}

// consolidatable returns the memories reflection may rewrite: non-resolved,
// unpinned, non-manual/non-builtin rows. ReplaceNonManual preserves exactly
// the excluded set, so this is the input the consolidator sees — and
// therefore the set the skip-unchanged fingerprint must cover.
func consolidatable(mems []memory.Memory) []memory.Memory {
	out := make([]memory.Memory, 0, len(mems))
	for _, m := range mems {
		if m.ResolvedAt != nil || m.Pinned || m.Source == "manual" || m.Source == "builtin" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// reflectSkipDecision reports whether a --skip-unchanged run may skip: the
// flag is set on an apply run and the stored fingerprint matches the current
// input. Pure so the decision is testable without a store.
func reflectSkipDecision(skipUnchanged, apply bool, stored, current string) bool {
	return skipUnchanged && apply && stored != "" && stored == current
}

// reflectMaySkip additionally honors the two flags that change what an apply
// DOES rather than what it reads. The input signature describes the project
// corpus, not what was done to it, so neither may inherit a prior default
// apply's unchanged verdict: --promote-globals moves cross-project candidates
// into _global, and --allow-drops deletes the inputs no output referenced
// instead of re-adding them verbatim.
func reflectMaySkip(skipUnchanged, apply, promoteGlobals, allowDrops bool, stored, current string) bool {
	return !promoteGlobals && !allowDrops && reflectSkipDecision(skipUnchanged, apply, stored, current)
}

// reflectArgs is one parsed `ghost reflect` invocation.
type reflectArgs struct {
	project        string
	tier           string
	source         string
	apply          bool
	restore        bool
	requireLLM     bool
	allowDrops     bool
	skipUnchanged  bool
	promoteGlobals bool
}

// parseReflectArgs parses `ghost reflect`'s arguments (everything after the
// subcommand word). Hand-rolled, matching the historical loop exactly: value
// flags accept both "--flag value" and "--flag=value", positionals set the
// project (last one wins), and unknown flags are silently ignored — the caller
// prints the usage block when the project comes out empty. The project may
// also come from --project, which takes the NEXT argument verbatim — a
// dash-leading name such as -x or --odd is a name, not a flag — so the
// lifecycle coordinator can emit one uniform form for every project; the
// positional form is unchanged for manual use. A valueless --project (no
// argument, or an empty --project=) is an error rather than a silent fall
// back to another interpretation. Extracted from runReflect so the argv
// contract is unit-testable without spawning.
func parseReflectArgs(args []string) (reflectArgs, error) {
	p := reflectArgs{tier: "auto"}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--project":
			if i+1 >= len(args) {
				return p, errors.New("--project requires a value")
			}
			p.project = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--project="):
			p.project = strings.TrimPrefix(args[i], "--project=")
			if p.project == "" {
				return p, errors.New("--project requires a value")
			}
		case args[i] == "--tier" && i+1 < len(args):
			p.tier = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--tier="):
			p.tier = strings.TrimPrefix(args[i], "--tier=")
		case args[i] == "--apply":
			p.apply = true
		case args[i] == "--restore":
			p.restore = true
		case args[i] == "--require-llm":
			p.requireLLM = true
		case args[i] == "--allow-drops":
			p.allowDrops = true
		case args[i] == "--skip-unchanged":
			p.skipUnchanged = true
		case args[i] == "--promote-globals":
			p.promoteGlobals = true
		case args[i] == "--source" && i+1 < len(args):
			p.source = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--source="):
			p.source = strings.TrimPrefix(args[i], "--source=")
		case !strings.HasPrefix(args[i], "-"):
			p.project = args[i]
		}
	}
	return p, nil
}

// runReflect manually triggers memory consolidation for a project.
// Defaults to dry-run (preview only). Use --apply to save results.
// Use --restore to undo the last consolidation from snapshot.
func runReflect() {
	parsed, parseErr := parseReflectArgs(os.Args[2:])
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", parseErr)
		os.Exit(1)
	}
	projectName := parsed.project
	tierValue := parsed.tier
	source := parsed.source
	apply, restore, requireLLM, allowDrops, skipUnchanged := parsed.apply, parsed.restore, parsed.requireLLM, parsed.allowDrops, parsed.skipUnchanged
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost reflect <project> [flags]

Flags:
  --tier string   Consolidation tier: auto, cli, opencode, sqlite (default "auto")
  --apply         Save results (default is dry-run/preview only)
  --restore       Undo the last consolidation from snapshot
  --require-llm   Fail instead of falling back to the Jaccard-only sqlite tier
  --allow-drops   Apply even when memories would be deleted without a merge
  --promote-globals Write cross-project candidates to _global (default: keep them project-scoped)
  --skip-unchanged Skip when the consolidatable set is unchanged since the last
                   applied consolidation (used by the auto lifecycle)
  --source string CLI harness for the auto tier: claude-code, opencode, codex,
                   or goose. Defaults to the calling harness (detected from
                   the environment and process ancestry); an undetectable
                   caller is an error. Ignored by explicit --tier cli/opencode/sqlite.
  --project string Project name; an alternative to the positional form that
                   takes the next argument verbatim, so dash-prefixed names work.`)
		os.Exit(1)
	}

	cfg, logger, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck

	ctx := context.Background()

	projectID := resolveProjectOrExit(ctx, store, projectName)

	if restore {
		n, err := store.RestoreSnapshot(ctx, projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Restored %d memories from snapshot for %s\n", n, projectName)
		return
	}

	// An explicit --source always wins; otherwise the auto tier needs the
	// calling harness so consolidation runs through the session's own
	// configured billing path. This runs after --restore, which is a pure DB
	// undo and must work without any harness. Explicit tiers (cli/opencode/sqlite)
	// select their backend directly and keep working without a detectable
	// caller — in particular the offline sqlite floor stays available from
	// any shell.
	if tierValue == "auto" {
		detected, err := detectPhaseSource(source)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		source = detected
	}

	var consolidator reflection.Consolidator

	// Source-aware auto tier: source is always resolved (explicit --source or
	// detectPhaseSource), so the auto tier runs the same CLI harness that
	// served the session. This lets the stop-hook or cron reflect run through
	// the configured harness, whose authentication and billing Ghost leaves alone.
	if tierValue == "auto" && source != "" {
		sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
		if sp.Available() {
			var tiers []reflection.Consolidator
			tiers = append(tiers, reflection.NewNamedConsolidator(sp, sp.Name()))
			if !requireLLM {
				tiers = append(tiers, reflection.NewSQLiteConsolidator())
			}
			consolidator = reflection.NewTieredConsolidator(tiers, logger)
		} else {
			// Falling through silently here hides a misconfiguration: the
			// caller asked for the source-matched backend and would otherwise
			// see no indication that the offline tier was used instead.
			fmt.Fprintf(os.Stderr, "warning: no CLI binary matches source %q; falling through to the auto tier's SQLite floor (or failing under --require-llm)\n", source)
		}
	}

	// If source-aware path above didn't build a consolidator, fall through
	// to the standard tier switch.
	if consolidator == nil {
		switch tierValue {
		case "haiku":
			// Removed: Ghost's memory management no longer calls the Anthropic
			// API. Use --tier auto (default), --tier cli, or --tier opencode.
			fmt.Fprintln(os.Stderr, "error: haiku tier removed — Ghost's memory management no longer calls the Anthropic API; use --tier auto (default) or --tier cli")
			os.Exit(1)
		case "cli":
			binary := "claude"
			if cfg.CLI.ClaudeBinary != "" {
				binary = cfg.CLI.ClaudeBinary
			}
			if _, err := exec.LookPath(binary); err != nil {
				hint := "set cli.claude_binary"
				if cfg.CLI.ClaudeBinary != "" {
					hint = "check that cli.claude_binary (" + binary + ") is a valid, executable path"
				}
				fmt.Fprintf(os.Stderr, "error: cli tier requires the `%s` binary on PATH (or %s)\n", binary, hint)
				os.Exit(1)
			}
			consolidator = reflection.NewNamedConsolidator(ai.NewCLIClientWithBinary(binary), "cli")
		case "opencode":
			binary := "opencode"
			if cfg.CLI.OpenCodeBinary != "" {
				binary = cfg.CLI.OpenCodeBinary
			}
			if _, err := exec.LookPath(binary); err != nil {
				hint := "set cli.opencode_binary"
				if cfg.CLI.OpenCodeBinary != "" {
					hint = "check that cli.opencode_binary (" + binary + ") is a valid, executable path"
				}
				fmt.Fprintf(os.Stderr, "error: opencode tier requires the `%s` binary on PATH (or %s)\n", binary, hint)
				os.Exit(1)
			}
			consolidator = reflection.NewNamedConsolidator(ai.NewOpenCodeClientWithBinary(binary), "opencode")
		case "sqlite":
			if requireLLM {
				fmt.Fprintln(os.Stderr, "error: --require-llm conflicts with --tier sqlite")
				os.Exit(1)
			}
			consolidator = reflection.NewSQLiteConsolidator()
		default: // "auto"
			// Route through the calling harness resolved by detectPhaseSource
			// (--source or caller detection) — never the old claude-first
			// cascade. The offline SQLite tier is the only fallback;
			// --require-llm is the autonomous-reflect guard and forbids it, so
			// an unavailable harness exits non-zero without touching the DB.
			var tiers []reflection.Consolidator
			if sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary); sp.Available() {
				tiers = append(tiers, reflection.NewNamedConsolidator(sp, sp.Name()))
			}
			if !requireLLM {
				tiers = append(tiers, reflection.NewSQLiteConsolidator())
			}
			consolidator = reflection.NewTieredConsolidator(tiers, logger)
		}
	}

	// Pin the harness model now that the tier/source resolves the harness: the
	// env is read per opencode invocation, so setting it after the consolidator
	// is built but before it runs is safe, and only here can we tell whether
	// the pin actually applies (a pin for a non-opencode harness is inert and
	// warned about above the pin itself).
	reflectHarness := tierValue
	if tierValue == "auto" {
		reflectHarness = source
	}
	applyPhaseModel(cfg.CLI.ModelReflect, reflectHarness)

	if !consolidator.Available(ctx) {
		fmt.Fprintf(os.Stderr, "error: consolidator %q is not available\n", consolidator.Name())
		os.Exit(1)
	}

	if !apply {
		fmt.Println("DRY RUN (use --apply to save results)")
		fmt.Println()
	}
	fmt.Printf("Project:      %s (%s)\n", projectName, projectID)
	fmt.Printf("Consolidator: %s\n", consolidator.Name())

	// Captured BEFORE fetching the consolidation input, then handed to
	// ReplaceNonManual: ghost reflect runs as a separate process from the live
	// MCP server, so any non-manual memory saved from this instant on wasn't
	// seen by the consolidator and must survive the replace. Capturing after
	// GetAll would leave a gap where a concurrent save is neither in the
	// snapshot nor recent enough to be preserved.
	consolidatedSince, err := store.CurrentTimestamp(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: get timestamp: %v\n", err)
		os.Exit(1)
	}
	// Load every memory, not a capped page: ReplaceNonManual replaces the whole
	// non-manual/unpinned/unresolved set for the project, so the input must
	// cover exactly that set. A LIMIT silently dropped the overflow (memories
	// beyond the cap were deleted by the replace but never seen by the
	// consolidator, surviving only in the snapshot), reachable as soon as a
	// project exceeds the cap. GetAll applies no source/pinned filter, so
	// consolidatable below excludes manual, pinned, and resolved rows — ReplaceNonManual
	// preserves all three and inserts the consolidator output alongside, so
	// feeding them in would duplicate each as a fresh reflection row on every
	// apply.
	existingMemories, err := store.GetAll(ctx, projectID, -1)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: get memories: %v\n", err)
		os.Exit(1)
	}
	// Resolved-evidence memories are excluded from consolidation input: ghost
	// resolve de-weighted them and they must survive reflect untouched, not be
	// re-emitted as fresh unresolved duplicates by the consolidator (see issue
	// #318). ReplaceNonManual independently excludes them from its delete, so
	// they are never touched either way.
	live := consolidatable(existingMemories)
	resolvedCount := 0
	for _, m := range existingMemories {
		if m.ResolvedAt != nil {
			resolvedCount++
		}
	}
	if skipUnchanged && apply {
		currentSig := reflection.InputSignature(live)
		storedSig, err := store.GetReflectInputSignature(ctx, projectID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: read reflect signature: %v\n", err)
		} else if reflectMaySkip(skipUnchanged, apply, parsed.promoteGlobals, parsed.allowDrops, storedSig, currentSig) {
			fmt.Printf("reflect: consolidatable set unchanged since the last applied consolidation — skipping (%d memories, no LLM call)\n", len(live))
			return
		}
	}
	// Bound the prompt. The consolidator rewrites its entire input in one call,
	// so an enormous project would either blow the model's context or, without
	// --require-llm, fall through to the O(n^2) SQLite tier and stall. Refuse
	// loudly rather than emit a partial or truncated result.
	const maxConsolidationInput = 2000
	if len(live) > maxConsolidationInput {
		fmt.Fprintf(os.Stderr, "error: project %s has %d consolidatable memories, above the %d limit for a single consolidation — nothing written\n", projectName, len(live), maxConsolidationInput)
		os.Exit(1)
	}
	currentContext, _ := store.GetLearnedContext(ctx, projectID)

	if resolvedCount > 0 {
		fmt.Printf("Memories:     %d existing (%d resolved, excluded from consolidation)\n", len(existingMemories), resolvedCount)
	} else {
		fmt.Printf("Memories:     %d existing\n", len(existingMemories))
	}
	fmt.Println("Running consolidation...")

	// Recent git activity and a language hint ground the prompt and give the
	// fabrication guard a set of SHAs that legitimately exist in the repo.
	// Both are best-effort: a project with no recorded path, no repository, or
	// no git binary simply contributes nothing.
	projectPath, _ := store.GetProjectPath(ctx, projectID)
	lastCommits, projectLanguage := reflection.CollectGitContext(projectPath)

	// Known other-project names feed the cross-project contamination guard:
	// consolidation only sees this project's data, so an emitted memory that
	// names another project never mentioned in the input is dropped.
	otherNames, nameErr := store.ListProjectNames(ctx)
	if nameErr != nil {
		fmt.Fprintf(os.Stderr, "warning: list projects for contamination guard: %v\n", nameErr)
	}
	filteredNames := make([]string, 0, len(otherNames))
	for _, n := range otherNames {
		if n != projectName && n != "_global" {
			filteredNames = append(filteredNames, n)
		}
	}

	input := reflection.ReflectionInput{
		ExistingMemories:  live,
		CurrentContext:    currentContext,
		LastCommits:       lastCommits,
		ProjectLanguage:   projectLanguage,
		ProjectName:       projectName,
		OtherProjectNames: filteredNames,
		// The prompt tells the model what omitting an input costs, and that is
		// the other side of this run's --allow-drops: without it an unreferenced
		// memory is re-added verbatim, with it the memory is deleted.
		AllowDrops: allowDrops,
	}

	consolidateCtx, cancel := consolidationContext(ctx, cfg.Reflection.ConsolidationTimeoutMinutes)
	defer cancel()

	result, err := consolidator.Consolidate(consolidateCtx, input)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: consolidation failed: %v\n", err)
		os.Exit(1)
	}

	var validMemories []reflection.ReflectMemory
	for _, m := range result.Memories {
		if strings.TrimSpace(m.Content) != "" {
			validMemories = append(validMemories, m)
		}
	}
	result.Memories = validMemories

	fmt.Printf("Result:       %d memories (%s)\n", len(result.Memories), reflectCategoryParts(result.Memories))
	fmt.Println()

	var projectMems, globalMems []reflection.ReflectMemory
	for _, m := range result.Memories {
		if m.Scope == "global" {
			globalMems = append(globalMems, m)
		} else {
			projectMems = append(projectMems, m)
		}
	}

	if len(projectMems) > 0 {
		fmt.Printf("  Project-scoped (%d):\n", len(projectMems))
		for _, m := range projectMems {
			truncated := truncateForDisplay(m.Content, 120)
			fmt.Printf("    [%s] (%.1f) %s\n", m.Category, m.Importance, truncated)
		}
	}
	if len(globalMems) > 0 {
		if parsed.promoteGlobals {
			fmt.Printf("  Global-scoped (%d):\n", len(globalMems))
		} else {
			fmt.Printf("  Cross-project (%d) — kept project-scoped unless --promote-globals:\n", len(globalMems))
		}
		for _, m := range globalMems {
			truncated := truncateForDisplay(m.Content, 120)
			fmt.Printf("    [%s] (%.1f) %s\n", m.Category, m.Importance, truncated)
		}
	}
	fmt.Println()

	if len(result.Memories) == 0 {
		fmt.Println("No memories returned from consolidation.")
		return
	}

	// Drop audit (#337, covering every category since #549): no input memory
	// may be deleted without a merge target. The autonomous phase spawned by
	// lifecyclePhases (`reflect --apply --require-llm`) runs unattended, which
	// is exactly where an unreferenced architecture or decision memory
	// disappears, and the `manual` source excluded below covers none of it:
	// seeds are 'builtin' and agent saves are 'mcp'. Refusing the whole
	// consolidation preserved fidelity but meant a rich project was never
	// consolidated at all (measured: 1 success in 13 attempts on a 220-memory
	// project), so the dropped memories are now retained VERBATIM instead: the
	// zero-loss invariant still holds and the rest of the consolidation
	// applies. Because the retained content is byte-identical to the stored
	// memory, ReplaceNonManual reuses its row and its embedding/links survive
	// (#452). --allow-drops keeps its meaning: accept the deletions.
	guardedDrops := reflection.AuditGuardedDrops(input, result)
	if len(guardedDrops) > 0 {
		fmt.Fprintf(os.Stderr, "WARNING: %d memory(ies) had no surviving merge target:\n", len(guardedDrops))
		for _, d := range guardedDrops {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", d.Category, truncateForDisplay(d.Content, 100))
		}
		if allowDrops {
			fmt.Fprintf(os.Stderr, "  --allow-drops set: these %d memories will be DELETED\n", len(guardedDrops))
		} else {
			retained := reflection.RetainGuardedDrops(guardedDrops)
			result.Memories = append(result.Memories, retained...)
			projectMems = append(projectMems, retained...)
			fmt.Fprintf(os.Stderr, "  retained %d verbatim — nothing dropped\n", len(retained))
		}
	}

	// One cap for every writer: consolidation output obeys the same
	// memory.MaxContentLen as MCP saves, cut with the same explicit marker,
	// so a reflection proposal can never store content the save path would
	// have refused to store silently.
	if cuts := clampReflectMemories(projectMems) + clampReflectMemories(globalMems); cuts > 0 {
		fmt.Fprintf(os.Stderr, "warning: %d consolidation memory content(s) exceeded the %d-byte cap and were truncated with an explicit marker\n", cuts, memory.MaxContentLen)
	}

	var existingNonManual int
	for _, m := range live {
		if m.Source != "manual" {
			existingNonManual++
		}
	}
	if existingNonManual >= 6 && len(projectMems) < existingNonManual/2 {
		fmt.Fprintf(os.Stderr, "WARNING: consolidation returned %d project memories vs %d existing non-manual (>50%% reduction)\n",
			len(projectMems), existingNonManual)
		if len(globalMems) > 0 {
			fmt.Fprintf(os.Stderr, "  (%d memories classified as global — check scope accuracy)\n", len(globalMems))
		}
	}

	if !apply {
		fmt.Println("Dry run complete. Re-run with --apply to save these results.")
		return
	}

	// Cross-project candidates are NOT promoted to _global unless asked.
	//
	// _global is injected into every future session in every project. Even the
	// cautious session-start wording does not make reflection output
	// user-confirmed, so letting a reflection pass write there unattended
	// moved content — potentially summarised from an untrusted repository —
	// straight into every project's shared context with no human in the loop
	// (issue #545). Keeping them project-scoped is
	// still useful and fully reversible, so promotion is now an explicit
	// decision: `ghost reflect --apply --promote-globals`.
	projectForSummary := projectMems
	if !parsed.promoteGlobals && len(globalMems) > 0 {
		projectForSummary = append(append([]reflection.ReflectMemory(nil), projectMems...), globalMems...)
	}

	// One call, one transaction. applyReflection is the store's apply
	// boundary: the project replace, every _global write, and the recovery of
	// any candidate that could not be promoted either commit together or roll
	// back together. Doing the parts here instead — as an earlier revision of
	// this PR did — put two failure windows between them. A crash or a signal
	// after the replace and before promotion left a candidate in neither place,
	// and with promotion OFF every cross-project candidate was deleted from the
	// project by the replace and then written nowhere at all, because promotion
	// returns early and the recovery list was empty. applyReflection folds
	// those candidates back into the project itself when promotion is off.
	preserved, promoted, keptMems, err := applyReflection(
		ctx, store, projectID, projectMems, globalMems, consolidatedSince, parsed.promoteGlobals)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: save memories: %v\n", err)
		os.Exit(1)
	}
	if promoted > 0 {
		fmt.Printf("Promoted %d/%d global memories\n", promoted, len(globalMems))
	}
	if len(globalMems) > 0 && parsed.promoteGlobals {
		// A partial promotion is reported, never counted as a clean success:
		// the candidate is back in the project, so the count of "kept" is the
		// part that still needs a decision from the operator.
		if len(keptMems) > 0 {
			fmt.Fprintln(os.Stderr, recoveryWarning(len(keptMems), len(globalMems)))
		}
	}

	// A round that wrote something counts as applied, and that includes a
	// candidate that could not be promoted and was written back into the
	// project: it is a row the project now holds, so it belongs in the count
	// and in the category breakdown, or the summary would report fewer
	// memories than the project actually has.
	//
	// Keying this on the project-memory count alone dropped both the summary
	// and the learned context for a promotion-only round — exactly the round
	// the flag was added for.
	if promoted > 0 || len(keptMems) > 0 || len(projectMems) > 0 || len(globalMems) > 0 {
		// Everything the project ends up holding, which is projectMems plus the
		// candidates that landed back in it: all of them when promotion is off,
		// and the kept subset when it is on. appliedSummary counts rows, so
		// leaving these out understates what was written.
		appliedProjectMems := append([]reflection.ReflectMemory(nil), projectMems...)
		if !parsed.promoteGlobals {
			appliedProjectMems = append(appliedProjectMems, globalMems...)
		} else {
			// Only the candidates that actually failed promotion. Adding all of
			// globalMems here would count the promoted rows as project memories
			// too, which is the over-count this replaced.
			keptText := make(map[string]bool, len(keptMems))
			for _, m := range keptMems {
				keptText[m.Content] = true
			}
			for _, m := range globalMems {
				if keptText[m.Content] {
					appliedProjectMems = append(appliedProjectMems, m)
				}
			}
		}
		// appliedSummary reports the REAL promoted count rather than the number
		// of candidates, so a partial promotion does not print "3 promoted to
		// global" one line after "Promoted 1/3".
		summary := appliedSummary(appliedProjectMems, globalMems, promoted, parsed.promoteGlobals)
		fmt.Printf("Applied: %s\n", summary)
		if len(globalMems) > 0 && !parsed.promoteGlobals {
			fmt.Println("(re-run with --promote-globals to inject them into every project)")
		}
		if len(projectForSummary) > 0 {
			fmt.Println(restoreHint(promoted))
		} else if promoted > 0 {
			fmt.Println("(promoted globals must be removed from _global by hand; no project snapshot was created)")
		}

		// One cap for every writer, learned-context summary included: the
		// consolidator's own output is clamped with the same marker as its
		// memories so the "any Ghost writer" claim on memory.MaxContentLen
		// holds for every field this command writes. This stays outside the
		// project-count guard so an only-global round still records context.
		if learned, learnedCut := memory.ClampContent(result.LearnedContext); learned != "" {
			if learnedCut {
				fmt.Fprintf(os.Stderr, "warning: learned context exceeded the %d-byte content cap and was truncated with an explicit marker\n", memory.MaxContentLen)
			}
			if err := store.UpdateLearnedContext(ctx, projectID, learned, summary); err != nil {
				fmt.Fprintf(os.Stderr, "warning: update learned context: %v\n", err)
			}
		}
	}

	// A save that raced the LLM round trip is preserved untouched by
	// ReplaceNonManual and is merged by the NEXT reflection. Recording a
	// fingerprint over the post-apply corpus would absorb that survivor and make
	// --skip-unchanged skip the merge, so record nothing when one happened; the
	// next run re-consolidates and merges it.
	if len(preserved) > 0 {
		fmt.Fprintf(os.Stderr, "note: %d memory(ies) were saved during consolidation and preserved; not recording the skip fingerprint so the next run merges them\n", len(preserved))
	} else if postMemories, err := store.GetAll(ctx, projectID, -1); err != nil {
		fmt.Fprintf(os.Stderr, "warning: record reflect signature: reload memories: %v\n", err)
	} else if err := store.SetReflectInputSignature(ctx, projectID, reflection.InputSignature(consolidatable(postMemories))); err != nil {
		fmt.Fprintf(os.Stderr, "warning: record reflect signature: %v\n", err)
	}
}

// applyPhaseModel pins the opencode harness model for this phase process when
// the config sets one, and warns when the chosen harness cannot honor the pin.
// ai.OpenCodeClient reads GHOST_OPENCODE_MODEL per invocation; each lifecycle
// phase runs as its own process, so setting it here cannot leak across phases.
// An empty config leaves any inherited value alone. harness is the resolved
// CLI-harness token ("opencode", "claude-code", "codex", "goose", or the
// explicit reflect tiers) — a non-empty model with a non-opencode harness is a
// silently-inert pin (claude/codex/goose have no model flag), surfaced once
// here rather than ignored.
func applyPhaseModel(model, harness string) {
	if model == "" {
		return
	}
	_ = os.Setenv("GHOST_OPENCODE_MODEL", model)
	if harness != "" && harness != "opencode" {
		fmt.Fprintf(os.Stderr, "warning: cli.model_* pin %q configured but the %s harness has no model flag; the pin will not apply (only the opencode harness honors model pins)\n", model, harness)
	}
}

// detectCallingSource is the process/env self-detection used when --source is
// absent. It is a package variable so tests can pin the undetected case: the
// test process's own ancestor chain can legitimately contain a harness (running
// `go test` from an opencode session), which would otherwise make that case
// environment-dependent.
var detectCallingSource = ai.DetectSource

// detectPhaseSource resolves the CLI-harness source for reflect/resolve/
// supersede: an explicit --source always wins; otherwise the calling harness is
// detected from the environment and process ancestry. There is no fallback
// cascade — an undetectable caller is an error, because silently classifying
// through a different harness than the session's would use the wrong
// configured billing path and betray the user's routing choice.
func detectPhaseSource(flagSource string) (string, error) {
	if flagSource != "" {
		return flagSource, nil
	}
	source := detectCallingSource()
	if source == "" {
		return "", ai.ErrUndetectableHarness
	}
	fmt.Fprintf(os.Stderr, "ghost: using calling harness %q (detected)\n", source)
	return source, nil
}

// buildClassifyProviderForSource builds the Provider resolve/supersede classify
// through: the CLI harness matching the resolved source. An empty source is an
// error, not a fallback — callers resolve it with detectPhaseSource (--source
// or caller detection). The Anthropic HTTP API no longer exists, so these
// commands only need the source's CLI binary on PATH (or configured via
// cli.*_binary).
func buildClassifyProviderForSource(cfg *config.Config, source string) (ai.Provider, error) {
	if source == "" {
		return nil, ai.ErrUndetectableHarness
	}
	sp := ai.NewSourceProviderForSource(source, cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary, cfg.CLI.CodexBinary, cfg.CLI.GooseBinary)
	if !sp.Available() {
		return nil, fmt.Errorf("source %q: no CLI binary available", source)
	}
	return sp, nil
}

// parseSupersedeArgs parses `ghost supersede`'s arguments (everything after
// the subcommand word). Hand-rolled, matching the historical loop exactly:
// value flags accept both "--flag value" and "--flag=value", positionals set
// the project (last one wins — --project assigns the same way), a
// non-numeric --threshold keeps the default, and any other flag is an
// unknown-flag error (which the caller prints and exits on). The project may
// come from --project, which takes the NEXT argument verbatim — a
// dash-leading name such as -x or --odd is a name, not a flag — so the
// lifecycle coordinator can emit one uniform form for every project; a
// valueless --project is an error. Extracted from runSupersede so the argv
// contract is unit-testable without os.Exit.
func parseSupersedeArgs(args []string) (project, source string, apply bool, threshold float32, err error) {
	threshold = 0.80 // supersession candidates are the SAME fact — tighter than the 0.70 'related' floor
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--apply":
			apply = true
		case args[i] == "--project":
			if i+1 >= len(args) {
				return "", "", false, 0, errors.New("--project requires a value")
			}
			project = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
			if project == "" {
				return "", "", false, 0, errors.New("--project requires a value")
			}
		case args[i] == "--threshold" && i+1 < len(args):
			if v, verr := strconv.ParseFloat(args[i+1], 32); verr == nil {
				threshold = float32(v)
			}
			i++
		case strings.HasPrefix(args[i], "--threshold="):
			if v, verr := strconv.ParseFloat(strings.TrimPrefix(args[i], "--threshold="), 32); verr == nil {
				threshold = float32(v)
			}
		case args[i] == "--source" && i+1 < len(args):
			source = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case !strings.HasPrefix(args[i], "-"):
			project = args[i]
		default:
			return "", "", false, 0, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return project, source, apply, threshold, nil
}

// runSupersede implements `ghost supersede <project> [--apply]` — the creation
// half of staleness-aware ranking. It proposes newer→older 'supersedes' links
// over the project's live memories (cosine-similar candidates, CLI-harness
// confirmed) and, with --apply, writes them. Dry-run by default. Re-runnable:
// it self-heals after `ghost reflect` cascade-deletes links. Consumed by
// search only when SupersedeDemote is set. See docs/benchmarks.md Phase 3.
func runSupersede() {
	projectName, source, apply, threshold, parseErr := parseSupersedeArgs(os.Args[2:])
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", parseErr)
		os.Exit(1)
	}
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost supersede <project> [flags]

Flags:
  --apply             Write the supersedes/causes links (default is dry-run/preview)
  --threshold float   Min cosine similarity for a candidate pair (default 0.80)
  --source string     CLI harness to classify through: claude-code, opencode,
                      codex, or goose. Defaults to the calling harness
                      (detected from the environment and process ancestry); an
                      undetectable caller is an error.
  --project string    Project name; an alternative to the positional form that
                      takes the next argument verbatim, so dash-prefixed names work.

Classifies each candidate as supersedes, causes, or neither. Runs through the
configured CLI harness of the calling session (--source overrides; otherwise
detected from the environment and process ancestry — an undetectable caller is
an error, never a fallback to a different harness). The harness owns its
authentication and billing.`)
		os.Exit(1)
	}

	source, err := detectPhaseSource(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg, logger, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck
	ctx := context.Background()

	projectID := resolveProjectOrExit(ctx, store, projectName)
	applyPhaseModel(cfg.CLI.ModelSupersede, source)
	provider, err := buildClassifyProviderForSource(cfg, source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: ghost supersede %v\n", err)
		os.Exit(1)
	}
	cls := supersede.NewRelationClassifier(provider)
	cls.SetLogger(logger)
	res, classified, err := supersede.Run(ctx, store, cls, projectID, threshold, apply, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	verb := "would link"
	if apply {
		verb = "linked"
	}
	short := func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	fmt.Printf("%s: %d candidate pairs in %d classify call(s), %d cached, %d supersedes, %d causes, %d reclassified, %s\n",
		projectName, res.Candidates, cls.Calls(), res.Skipped, res.Confirmed, res.CausesCreated, res.Reclassified, verb)
	if res.Unclassified > 0 {
		fmt.Printf("  %d pair(s) skipped: unclassifiable verdict (logged; the pass still completed)\n", res.Unclassified)
	}
	for _, c := range classified {
		switch c.Relation {
		case supersede.RelationSupersedes:
			fmt.Printf("  %s  supersedes  %s\n", short(c.NewerID), short(c.OlderID))
		case supersede.RelationCauses:
			fmt.Printf("  %s  causes  %s\n", short(c.OlderID), short(c.NewerID))
		}
	}
	if !apply && (res.Confirmed > 0 || res.CausesCreated > 0 || res.Reclassified > 0) {
		fmt.Println("\nRe-run with --apply to write these links.")
	}
}

// parseResolveArgs parses `ghost resolve`'s arguments (everything after the
// subcommand word). Hand-rolled, matching the historical loop exactly: exactly
// one project — positionally (unchanged back-compat) or from --project, which
// takes the NEXT argument verbatim so a dash-leading name such as -x or --odd
// is a name, not a flag (a second project in any mixture keeps the historical
// "expected exactly one project" error); value flags in both "--flag value"
// and "--flag=value" forms; any other flag an unknown-flag error — which the
// caller prints and exits on. A valueless --project is an error. Extracted
// from runResolve so the argv contract is unit-testable without os.Exit.
func parseResolveArgs(args []string) (project, source string, apply bool, err error) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--apply":
			apply = true
		case args[i] == "--project":
			if i+1 >= len(args) {
				return "", "", false, errors.New("--project requires a value")
			}
			if project != "" {
				return "", "", false, errors.New("expected exactly one project")
			}
			project = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--project="):
			v := strings.TrimPrefix(args[i], "--project=")
			if v == "" {
				return "", "", false, errors.New("--project requires a value")
			}
			if project != "" {
				return "", "", false, errors.New("expected exactly one project")
			}
			project = v
		case args[i] == "--source" && i+1 < len(args):
			source = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case !strings.HasPrefix(args[i], "-"):
			if project != "" {
				return "", "", false, errors.New("expected exactly one project")
			}
			project = args[i]
		default:
			return "", "", false, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	return project, source, apply, nil
}

// resolveSummaryLine renders the one-line resolve result, including UNKNOWN
// verdicts that remain eligible for a later pass.
func resolveSummaryLine(projectName string, res resolve.Result, apply bool, confirmed int, calls int) string {
	verb := "would resolve"
	count := confirmed
	if apply {
		verb = "resolved"
		count = res.Resolved
	}
	return fmt.Sprintf("%s: %d loaded, %d after prefilter, %d confirmed evidence, %d KEEP cached, %d UNKNOWN, %s %d (%d classify call(s))\n",
		projectName, res.Loaded, res.Candidates, res.Confirmed+res.Superseded+res.Corrected,
		res.Skipped, res.Unknown, verb, count, calls)
}

// runResolve is the CLI entry for `ghost resolve`. It marks resolved-evidence
// memories (concluded work: findings, changelog notes, PR locators) so they
// drop out of session-start injection while staying searchable. Cheap local
// keyword prefilter proposes candidates; the hosting CLI harness adjudicates
// them in batches with a crisp conclusion-vs-evidence question biased to KEEP,
// and explicit KEEP verdicts are cached by content hash so a converged project
// makes no calls; UNKNOWN replies remain eligible for a later pass. Dry-run by
// default; --apply writes resolved_at and the cache.
// Re-runnable and reversible: any later Upsert/UpdateMemory of a memory clears
// its resolved_at. The stop hook spawns `ghost lifecycle` detached
// (internal/mcpinit/stophook.go); its resolve phase runs this with --apply.
func runResolve() {
	projectName, source, apply, parseErr := parseResolveArgs(os.Args[2:])
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", parseErr)
		os.Exit(1)
	}
	if projectName == "" {
		fmt.Fprintln(os.Stderr, `Usage: ghost resolve <project> [flags]

Flags:
  --apply         Stamp resolved_at on confirmed memories (default is dry-run/preview)
  --source string CLI harness to classify through: claude-code, opencode,
                  codex, or goose. Defaults to the calling harness (detected
                  from the environment and process ancestry); an undetectable
                  caller is an error.
  --project string Project name; an alternative to the positional form that
                  takes the next argument verbatim, so dash-prefixed names work.

Marks resolved-evidence memories so they drop from session-start injection
(still searchable). Runs through the configured CLI harness of the calling
session (--source overrides; otherwise detected from the environment and
process ancestry — an undetectable caller is an error, never a fallback to a
different harness). The harness owns its authentication and billing.`)
		os.Exit(1)
	}

	source, err := detectPhaseSource(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	cfg, logger, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck
	ctx := context.Background()

	projectID := resolveProjectOrExit(ctx, store, projectName)
	applyPhaseModel(cfg.CLI.ModelResolve, source)
	provider, err := buildClassifyProviderForSource(cfg, source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: ghost resolve %v\n", err)
		os.Exit(1)
	}
	cls := resolve.NewResolutionClassifier(provider)
	cls.SetLogger(logger)
	res, confirmed, err := resolve.Run(ctx, store, cls, projectID, apply, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	short := func(id string) string {
		if len(id) > 8 {
			return id[:8]
		}
		return id
	}
	fmt.Print(resolveSummaryLine(projectName, res, apply, len(confirmed), cls.Calls()))
	if res.Superseded > 0 || res.Corrected > 0 {
		fmt.Printf("  (%d via supersedes links, %d via correction pairing, %d via LLM)\n",
			res.Superseded, res.Corrected, res.Confirmed)
	}
	for _, m := range confirmed {
		fmt.Printf("  %s  [%s]  %s\n", short(m.ID), m.Category, firstLine(m.Content, 70))
	}
	if !apply && res.Confirmed+res.Superseded+res.Corrected > 0 {
		fmt.Println("\nRe-run with --apply to mark these resolved.")
	}
}
