package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/embedding"
	"github.com/wcatz/ghost/internal/followup"
	"github.com/wcatz/ghost/internal/mcpinit"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/reflection"
	"github.com/wcatz/ghost/internal/resolve"
	"github.com/wcatz/ghost/internal/scratch"
	"github.com/wcatz/ghost/internal/supersede"
)

// lifecycleUsage is the help for the internal `ghost lifecycle` subcommand:
// stderr for its usage error, stdout for -h/--help (see handleHelp).
const lifecycleUsage = `Usage: ghost lifecycle --project <name> [--source <src>]

Internal subcommand spawned by the Stop hook: runs the enabled
auto-consolidation phases — reflect, then resolve, then supersede — for one
project in order, in a single process. Not the normal way to start
maintenance; run ghost reflect <project> (or resolve/supersede) by hand.
A positional project is accepted, but --project takes the next argument
verbatim, so dash-prefixed names work.
`

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

	// Record the START for lifecycle.min_interval (#541): the stop hook reads
	// this stamp to decide whether the next turn's spawn is inside the cooldown.
	//
	// Placed here, after every precondition above that can exit non-zero — a
	// config file that does not parse, an unlocatable binary — because a run
	// that dies before doing any work must not burn a 30m window, and because
	// such a death precedes FinishLifecycleRun and so leaves no failure marker
	// either. This is the silent-death class the marker exists to catch, and
	// the stamp must not add to it.
	//
	// It is NOT conditional on holding the lock. A run turned away because
	// another one holds it has already returned above; this one proceeds, and
	// whether it held the lock or merely failed to take it, it is a real run
	// that the next turn's cooldown should account for. A foreground
	// `ghost lifecycle` writes it too, which is what a user retrying the alert's
	// command wants.
	//
	// Best-effort: a missing stamp only costs one extra spawn.
	if err := mcpinit.TouchLifecycleStart(projectName); err != nil {
		fmt.Fprintf(os.Stderr, "lifecycle: warning: could not record the run start (%v)\n", err)
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
//
// It wraps the whole consolidation, so the LLM tier's one repair turn shares this
// budget rather than getting a deadline of its own: a run that needs the repair
// has what is left of it. That is deliberate — the phase timeout above is the
// bound an operator can raise, and a per-call budget here would let a single
// pass outlive the phase it runs in.
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
// unpinned, non-manual/non-builtin rows — and, like every other pass reflection
// could reach with a rewrite, NOT a persistent row. ReplaceNonManual preserves
// exactly the excluded set, so this is the input the consolidator sees — and
// therefore the set the skip-unchanged fingerprint must cover.
func consolidatable(mems []memory.Memory) []memory.Memory {
	out := make([]memory.Memory, 0, len(mems))
	for _, m := range mems {
		if m.ResolvedAt != nil || m.Pinned || memory.RetentionExempt(m) || m.Source == "manual" || m.Source == "builtin" {
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

// promotionOutcome is what is knowable about --promote-globals at the moment the
// reduction report is printed.
//
// It is a type rather than a bare count because zero kept is AMBIGUOUS: after an
// apply it means every candidate became a _global row, and before one it means
// nothing has been written at all. Inferring the state from the count produced a
// report that told the operator a promotion "would" happen, printed after the
// write that had already promoted all nine — the same untruth as the note this
// replaced, pointing the other way. A count that is meaningful on its own cannot
// also be the answer to "has this happened yet".
type promotionOutcome struct {
	// applied is whether ApplyReflection has run. False for a dry run, and for the
	// pre-apply report on a run that will apply.
	applied bool
	// kept is how many candidates failed promotion and were written back into the
	// project. Only meaningful once applied.
	kept int
}

// keptInProject is how many candidates the project still holds. Before the apply
// that is unknowable, so it is 0 and the report labels the count optimistic
// rather than printing a number it cannot stand behind.
func (o promotionOutcome) keptInProject() int {
	if !o.applied {
		return 0
	}
	return o.kept
}

// reflectRetained is how many memories the project ends up holding from one
// applied result: its own project-scoped memories, plus the cross-project
// candidates that did not become _global rows and were written back into the
// project instead. It mirrors applyReflection's own rule rather than restating
// it, so the reduction warning below measures the set the project actually ends
// up with — the count that decides whether a memory survived. Reading
// len(projectMems) alone understates retention by the whole candidate set and
// fires the warning on rounds that lost nothing.
func reflectRetained(projectMems, globalMems []reflection.ReflectMemory, promoteGlobals bool, promotion promotionOutcome) int {
	if promoteGlobals {
		return len(projectMems) + promotion.keptInProject()
	}
	return len(projectMems) + len(globalMems)
}

// reductionWarnMinInput is the corpus size below which no ratio is reported.
// A 4-memory corpus compressing to 1 is a 75% reduction and says nothing, and
// a warning on nearly every such run is noise. The threshold is 6 for the same
// reason the quality gate's is: the gate's smallest input is gateMinInput = 6,
// so below it the gate judges nothing at all and this is the only signal there
// is — which is exactly when a percentage is worth least.
const reductionWarnMinInput = 6

// reportDisposedClaims prints what the response CLAIMED it disposed of. Nothing
// acts on these claims — an unattended reflect never deletes a memory on the
// model's say-so alone (#549) — so this is the only place a person deciding
// whether to pass --apply can see that the model tried to drop something.
//
// It deliberately does NOT say what became of a claim. The guarded-drop report
// already prints the outcome, and a second opinion about it would be a second
// implementation of the guard's decision waiting to disagree with the first. Read
// the two together: this says what was claimed, that says what was kept or
// deleted.
//
// Whether the claimed replacement survived into the result IS stated, because
// that is a fact about the result rather than an opinion about the guard, and
// because executeOps records Replacement.Text before the post-filters run:
// dropForeignProjectMemories deletes a memory naming a project the input corpus
// never mentioned. Printing a replacement this same run discarded would be the
// one thing a report meant to inform an --apply decision must not do.
//
// It is a function taking a writer rather than an inline loop because runReflect
// exits the process, so the loop is not reachable from a test. The limit is the
// caller's, so `ghost reflect --full` reaches the claim's text as well as the
// proposal list's, and the claim's text goes through displayClaim for the same
// reason the proposal list's does: it is derived from stored text and can carry
// a value a pre-guard row held.
func reportDisposedClaims(w io.Writer, limit int, result reflection.ReflectionResult) {
	if len(result.Replacements) == 0 {
		return
	}
	inResult := make(map[string]bool, len(result.Memories))
	for _, m := range result.Memories {
		inResult[m.Content] = true
	}
	// Discarded on purpose, as everywhere else on this path: a write that fails
	// cannot be reported through the same failed write, and the report lands on
	// the stdout of a dry run a person is reading.
	for _, r := range result.Replacements {
		switch {
		case r.Text == "":
			_, _ = fmt.Fprintln(w, "  Disposed of (model's claim): (no replacement text recorded)")
		case !inResult[r.Text]:
			_, _ = fmt.Fprintf(w, "  Disposed of (model's claim): its replacement is NOT in this result — %s\n",
				displayClaim(r.Text, limit))
		default:
			_, _ = fmt.Fprintf(w, "  Disposed of (model's claim): replaced by %s\n", displayClaim(r.Text, limit))
		}
	}
}

// reportReductionWarning prints the >50% reduction warning when a consolidation
// kept less than half its consolidatable input, and prints nothing otherwise.
//
// It is called exactly once per run, and WHERE depends on what is knowable. A dry
// run writes nothing so nothing can fail, so it reports before the (absent)
// write. A run that applies with promotion off folds the cross-project candidates
// back into the project either way, so the count cannot move and it also reports
// before the write. A run that applies WITH promotion on can only know the count
// afterwards, because a candidate may fail to become a _global row and land back
// in the project instead — so that one reports after the write. The pre-apply
// report is skipped rather than printed optimistically and corrected, because a
// candidate set large enough to cross the 50% line would otherwise produce a
// report of a reduction that did not happen, followed by silence about it.
//
// It takes the slices rather than two pre-counted numbers so the count it
// reports is derived where it is printed: on the unattended lifecycle path this
// line is the only report of a hard compression, so a wrong count here is a
// warning that fires on rounds which lost nothing, and one that stays silent on
// rounds that lost most of the corpus.
//
// The retained side is reflectRetained, not len(projectMems): with promotion off
// applyReflection folds the cross-project candidates back into the project, so
// they are survivors, and with promotion ON the ones that failed come back the
// same way. The input side is len(live) — manual, builtin, pinned and resolved
// rows are already excluded upstream, and every row left is one ReplaceNonManual
// would delete.
func reportReductionWarning(w io.Writer, live []memory.Memory, projectMems, globalMems []reflection.ReflectMemory, promoteGlobals bool, promotion promotionOutcome) {
	if len(live) < reductionWarnMinInput {
		return
	}
	retained := reflectRetained(projectMems, globalMems, promoteGlobals, promotion)
	// Doubled rather than len(live)/2: integer division rounds the half DOWN, so
	// 3 of 7 reads as "at least half" and prints nothing, when 3/7 is a 57%
	// reduction — more than the half the message names. The boundary is only
	// reachable on an odd corpus, which is why it was easy to miss.
	if retained*2 >= len(live) {
		return
	}
	// os.Stderr is best-effort here, as it is for every other report on this
	// path — a write that fails cannot be reported through the same failed write,
	// and on the unattended lifecycle path this stream is the stderr of a
	// detached process nobody reads. The guarded-drop report a few lines above
	// discards its results the same way.
	_, _ = fmt.Fprintf(w, "WARNING: consolidation left %d memories in the project vs %d consolidatable (>50%% reduction)\n",
		retained, len(live))
	if len(globalMems) == 0 {
		return
	}
	// The note states only what is true at this point in the run, and the three
	// states are distinguished by whether the apply has HAPPENED rather than by
	// the kept count. Before it, a candidate is classified global and nothing
	// more; calling them "promoted" there would claim a write that has not
	// occurred, and calling the whole set survivors would be wrong the other way,
	// since a candidate that promotes has left the project. After it, the split is
	// known and a candidate that promoted really did leave the project, so it is
	// not counted as retained and saying otherwise would overstate survival.
	switch {
	case !promoteGlobals:
		_, _ = fmt.Fprintf(w, "  (%d memories classified as global — kept project-scoped; check scope accuracy)\n", len(globalMems))
	case !promotion.applied:
		_, _ = fmt.Fprintf(w, "  (%d memories classified as global — would be promoted to _global on apply; a failed promotion keeps them in the project; check scope accuracy)\n",
			len(globalMems))
	case promotion.kept > 0:
		_, _ = fmt.Fprintf(w, "  (%d memories classified as global — %d promoted to _global, %d could not be and are back in the project; check scope accuracy)\n",
			len(globalMems), len(globalMems)-promotion.kept, promotion.kept)
	default:
		_, _ = fmt.Fprintf(w, "  (%d memories classified as global — all %d promoted to _global; check scope accuracy)\n",
			len(globalMems), len(globalMems))
	}
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
	// full asks for the whole text of every memory the run reports, instead of
	// the compact preview (#684). It is a display flag and nothing else: the
	// stored text, the content cap and the operations are identical either way,
	// and it is deliberately not on the unattended lifecycle path, whose stdout
	// is an append-only log a longer report would fill for nobody to read.
	full bool
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
		case args[i] == "--full":
			p.full = true
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

// reflectProposalLimit is how many bytes of a memory `ghost reflect` prints by
// default. A proposal list is a summary — 120 is a screenful of context per
// memory and a large project has hundreds — and the section that accounts for
// every input id is where a reader looks for detail when a line says something
// surprising. `ghost reflect --full` removes the cut everywhere at once, so the
// compact default and the whole text are one flag apart rather than two modes.
const reflectProposalLimit = 120

// reflectTextLimit resolves --full against one print site's compact default: no
// limit at all under --full, and the site's own default otherwise. It is a
// function rather than a bare `if full` at each site so the flag cannot reach
// some of them and not others, which is the failure mode a display flag spread
// across five call sites has.
func reflectTextLimit(full bool, compact int) int {
	if full {
		return 0
	}
	return compact
}

// reflectUsage is the help for `ghost reflect`: stderr when the project comes
// out empty (a usage error, exit 1), stdout for -h/--help (help is a
// question, not an error, exit 0 — see handleHelp). One text for both, so the
// two can never drift.
const reflectUsage = `Usage: ghost reflect <project> [flags]

Flags:
  --tier string   Consolidation tier: auto, cli, opencode, sqlite (default "auto")
  --apply         Save results (default is dry-run/preview only)
  --restore       Undo the last consolidation from snapshot
  --require-llm   Fail instead of falling back to the Jaccard-only sqlite tier
  --allow-drops   Apply even when memories would be deleted without a merge
  --promote-globals Write cross-project candidates to _global (default: keep them project-scoped)
  --skip-unchanged Skip when the consolidatable set is unchanged since the last
                   applied consolidation (used by the auto lifecycle)
  --full           Print every reported memory whole, instead of truncating it
                   to a compact preview. Display only: the result and the write
                   are identical either way.
  --source string CLI harness for the auto tier: claude-code, opencode, codex,
                   or goose. Defaults to the calling harness (detected from
                   the environment and process ancestry); an undetectable
                   caller is an error. Ignored by explicit --tier cli/opencode/sqlite.
  --project string Project name; an alternative to the positional form that
                   takes the next argument verbatim, so dash-prefixed names work.
`

// gatedLLMTier is the consolidator an explicitly-selected LLM backend runs
// through, and it is a named seam because the decision is not the obvious one:
// naming the backend outright used to also opt out of the quality gate, because
// the bare LlmConsolidator is not where the gate lives — that is inside
// reflection.TieredConsolidator, which the `auto` tier has always used. So the
// default and the unattended lifecycle path were bounded while
// `ghost reflect --tier cli --apply` applied a harness answer of any size, and
// eval/cycle, which measures with `--tier opencode --apply`, was grading an
// ungated consolidator while production ran a gated one (issue #549).
//
// The wrapper adds no fallback tier, so the selection still means "exactly this
// backend"; it only makes a too-small answer a failed run, which is the honest
// reading of that request when the answer came back truncated. It is a function
// rather than an inline call because runReflect exits the process, so the tier
// switch is not reachable from a test and the behaviour has to be pinned here.
func gatedLLMTier(c reflection.Consolidator, logger *slog.Logger) reflection.Consolidator {
	return reflection.NewGatedConsolidator(c, logger)
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
		fmt.Fprint(os.Stderr, reflectUsage)
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
			consolidator = gatedLLMTier(reflection.NewNamedConsolidator(ai.NewCLIClientWithBinary(binary), "cli"), logger)
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
			consolidator = gatedLLMTier(reflection.NewNamedConsolidator(ai.NewOpenCodeClientWithBinary(binary), "opencode"), logger)
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
	// non-manual/unpinned/unresolved/non-persistent set for the project, so the
	// input must cover exactly that set. A LIMIT silently dropped the overflow
	// (memories beyond the cap were deleted by the replace but never seen by the
	// consolidator, surviving only in the snapshot), reachable as soon as a
	// project exceeds the cap. GetAll applies no source/pinned/retention filter,
	// so consolidatable below excludes manual, pinned, resolved, and persistent
	// rows — ReplaceNonManual preserves all four and inserts the consolidator
	// output alongside, so feeding them in would duplicate each as a fresh
	// reflection row on every apply (measured for a persistent row: the sqlite
	// tier "passed through (not named)" a keep-forever memory and the replace
	// inserted a project-tier reflection copy beside it).
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

	fmt.Print(reflectResultLine(result))
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
			fmt.Printf("    [%s] (%.1f) %s\n", m.Category, m.Importance, displayProposal(m.Content, m.Category, reflectTextLimit(parsed.full, reflectProposalLimit)))
		}
	}
	if len(globalMems) > 0 {
		if parsed.promoteGlobals {
			fmt.Printf("  Global-scoped (%d):\n", len(globalMems))
		} else {
			fmt.Printf("  Cross-project (%d) — kept project-scoped unless --promote-globals:\n", len(globalMems))
		}
		for _, m := range globalMems {
			fmt.Printf("    [%s] (%.1f) %s\n", m.Category, m.Importance, displayProposal(m.Content, m.Category, reflectTextLimit(parsed.full, reflectProposalLimit)))
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
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", d.Category, displayProposal(d.Content, d.Category, reflectTextLimit(parsed.full, 100)))
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

	// What became of every input id (#684). Printed here, after the drop guard has
	// run and the cap has been applied and BEFORE the write, so a dry run and an
	// apply read the same and the dry run previews this section exactly. It is
	// the part of the report a person is actually auditing: the result list
	// above says what the corpus became, and this says what became of the rows
	// the run was given, which is the only place a merge is distinguishable from
	// a loss.
	reportInputAccounting(os.Stdout, reflectRun{
		input:      input,
		result:     result,
		guarded:    guardedDrops,
		allowDrops: allowDrops,
		full:       parsed.full,
	})

	// What the response claimed it disposed of, and whether the claimed
	// replacement survived into the result. See reportDisposedClaims.
	reportDisposedClaims(os.Stdout, reflectTextLimit(parsed.full, 80), result)

	// The >50% reduction warning. On the unattended lifecycle path this is the
	// only report of a hard compression, and it goes to the stderr of a process
	// nobody reads while the exit status stays 0 — so the count it prints has to
	// be the one that decides whether a memory survived.
	//
	// Reported here only when it cannot be wrong: a dry run writes nothing so
	// nothing can fail, and promotion-off folds the candidates back in either way
	// so the count cannot move. A run that applies WITH promotion on is reported
	// after the write instead, because a candidate that fails to become a _global
	// row lands back in the project, so a pre-apply count of projectMems alone
	// understates retention by the whole candidate set — and when the candidates
	// are enough to cross the 50% line, reporting it here would announce a
	// reduction that did not happen and then say nothing about the real one.
	if !apply || !parsed.promoteGlobals {
		reportReductionWarning(os.Stderr, live, projectMems, globalMems, parsed.promoteGlobals, promotionOutcome{})
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
	// The POST-drop sets, not the caller's pre-drop ones: the summary below counts
	// them, and a partial drop — one good proposal and one credential — has to
	// report only what was written.
	keptProjectMems, keptGlobalMems, preserved, promoted, keptMems, applied, err := applyReflection(
		ctx, store, projectID, projectMems, globalMems, consolidatedSince, parsed.promoteGlobals, replacedIDsByText(&result))
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

	// Nothing was written, and the reason has to be stated rather than left to
	// inference. A round whose only proposals held credentials is dropped
	// down to nothing, applyReflection returns without calling the store, and
	// the corpus is byte-identical to what it was — so a report that went on to
	// say "Applied" and record the skip fingerprint would be telling the
	// operator the round landed while the value is still in the store, still
	// injected into every later session, and now pinned by a signature that
	// makes --skip-unchanged skip the project forever. That is the worst
	// possible outcome for this control, and it is one branch below the one
	// that is careful.
	//
	// The reduction report is the else-if rather than a second if because the two
	// contradict each other in this case: nothing was applied, so kept is 0, and
	// a reduction report would announce "0 retained" immediately after the line
	// explaining that every proposal held a credential. #664's condition and its
	// output are untouched in every case that already existed; the only
	// behaviour this adds is in the !applied case, which did not exist before.
	if !applied {
		fmt.Println("Applied: nothing — every consolidation proposal this round held a credential value and was dropped")
		fmt.Println("  the project is unchanged, so a stored memory holding that value is still there: remove or correct it by hand")
	} else if apply && parsed.promoteGlobals {
		// The reduction warning, for the one case the pre-apply report had to skip:
		// this run applies with promotion on, so only now is it knowable how many
		// candidates stayed in the project. A candidate that failed promotion is a row
		// the project still holds, so it counts as retained — the count that decides
		// whether a memory survived, which is the whole point of the line. `applied` is
		// what distinguishes this from the pre-apply call: with no failures kept is 0,
		// and a bare 0 would read as "nothing has been written yet".
		//
		// Printed even when nothing failed, because the skipped pre-apply report means
		// this run would otherwise say nothing at all about how hard it compressed.
		// The POST-drop sets, like every other report after the apply. The
		// pre-apply report above passes projectMems/globalMems and is right to:
		// it runs before applyReflection, so the kept sets do not exist yet. This
		// one runs after, so a proposal that was dropped for holding a credential
		// is in neither list — and passing the caller's pre-drop lists counted it
		// as retained, which is the same miscount this function's own commit set
		// out to fix everywhere else. A partial drop — one good proposal and one
		// credential — has to report only what was written.
		reportReductionWarning(os.Stderr, live, keptProjectMems, keptGlobalMems, true,
			promotionOutcome{applied: true, kept: len(keptMems)})
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
	if applied && (promoted > 0 || len(keptMems) > 0 || len(keptProjectMems) > 0 || len(keptGlobalMems) > 0) {
		// Everything the project ends up holding, which is projectMems plus the
		// candidates that landed back in it: all of them when promotion is off,
		// and the kept subset when it is on. appliedSummary counts rows, so
		// leaving these out understates what was written.
		appliedProjectMems := append([]reflection.ReflectMemory(nil), keptProjectMems...)
		if !parsed.promoteGlobals {
			appliedProjectMems = append(appliedProjectMems, keptGlobalMems...)
		} else {
			// Only the candidates that actually failed promotion. Adding all of
			// globalMems here would count the promoted rows as project memories
			// too, which is the over-count this replaced.
			keptText := make(map[string]bool, len(keptMems))
			for _, m := range keptMems {
				keptText[m.Content] = true
			}
			for _, m := range keptGlobalMems {
				if keptText[m.Content] {
					appliedProjectMems = append(appliedProjectMems, m)
				}
			}
		}
		// appliedSummary reports the REAL promoted count rather than the number
		// of candidates, so a partial promotion does not print "3 promoted to
		// global" one line after "Promoted 1/3".
		summary := appliedSummary(appliedProjectMems, keptGlobalMems, promoted, parsed.promoteGlobals)
		fmt.Printf("Applied: %s\n", summary)
		if len(keptGlobalMems) > 0 && !parsed.promoteGlobals {
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
	if !applied {
		// Not "no preserved survivors" — nothing at all was replaced, so the
		// corpus still holds whatever it held. Recording a fingerprint over an
		// unchanged corpus is what would make --skip-unchanged skip this project
		// permanently, which is how a stored credential stops being revisited at
		// all. The round did not happen; it must not be recorded as having.
		fmt.Fprintln(os.Stderr, "note: nothing was written this round, so no skip fingerprint is recorded — the next run will reconsider this project")
	} else if len(preserved) > 0 {
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

// supersedePair is one `--withdraw <source-id> <target-id>` pair as it was typed,
// before the refs are resolved. Source is the superseding memory (a
// 'supersedes' edge is written newer→older) and Target the superseded one.
type supersedePair struct{ source, target string }

// parseSupersedeArgs parses `ghost supersede`'s arguments (everything after
// the subcommand word). Hand-rolled, matching the historical loop exactly:
// value flags accept both "--flag value" and "--flag=value", positionals set
// the project (last one wins — --project assigns the same way), a
// non-numeric --threshold keeps the default, --reassess selects the repair
// pass over the edges already in the graph (issue #686), --withdraw names the
// edges to remove outright, and any other flag is an unknown-flag error (which
// the caller prints and exits on). The project may come from --project, which
// takes the NEXT argument verbatim — a dash-leading name such as -x or --odd is
// a name, not a flag — so the lifecycle coordinator can emit one uniform form
// for every project; a valueless --project is an error. Extracted from
// runSupersede so the argv contract is unit-testable without os.Exit.
//
// --withdraw takes TWO operands and is refused alongside --reassess, for the two
// reasons that are load-bearing rather than stylistic. Its operands are
// consumed by the flag, so they can never be mistaken for the positional
// project — which is the trap a bare-word project makes of any operand this
// parser does not claim, and the reason the pair is read inside the flag's own
// clause instead of being collected as positionals. Neither operand may look
// like a flag, so `--withdraw <id> --apply` is a missing operand rather than a
// pair whose target is the string "--apply": the ids Ghost mints are
// hex(randomblob(16)), so neither a full one nor a prefix of one begins with a
// dash, and that is what makes the leading character usable as the test here.
//
// It is a CLI-ONLY limit, and the resolution half is deliberately wider: an
// imported id is whatever its artifact said (ghost import writes ids verbatim),
// so one beginning with a dash is nameable through `ghost_link_withdraw`, which
// parses no flags, and not from here. That asymmetry is the cost of reading a
// dash as a flag at all, it is loud — the pair is reported as a missing operand
// rather than resolved to the wrong memory — and it is the reason the flag form
// is documented as ids and prefixes of them rather than as "any id".
//
// And a command that re-judged every edge AND removed named ones would have two
// dry-run answers, so the reader is told which of the two repairs they asked for
// twice.
func parseSupersedeArgs(args []string) (project, source string, apply, reassess bool, threshold float32, withdraw []supersedePair, err error) {
	threshold = 0.80 // supersession candidates are the SAME fact — tighter than the 0.70 'related' floor
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--apply":
			apply = true
		case args[i] == "--reassess":
			reassess = true
		case args[i] == "--withdraw" && i+2 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.HasPrefix(args[i+2], "-"):
			withdraw = append(withdraw, supersedePair{source: args[i+1], target: args[i+2]})
			i += 2
		case args[i] == "--withdraw":
			return "", "", false, false, 0, nil, fmt.Errorf("--withdraw needs a source id and a target id: --withdraw <source-id> <target-id>")
		case args[i] == "--project":
			if i+1 >= len(args) {
				return "", "", false, false, 0, nil, errors.New("--project requires a value")
			}
			project = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
			if project == "" {
				return "", "", false, false, 0, nil, errors.New("--project requires a value")
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
			return "", "", false, false, 0, nil, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if len(withdraw) > 0 && reassess {
		return "", "", false, false, 0, nil, errors.New("--withdraw removes the edges you name and --reassess re-judges every edge in the graph; run them as two commands")
	}
	return project, source, apply, reassess, threshold, withdraw, nil
}

// supersedeUsage is the help for `ghost supersede`: stderr when the project
// comes out empty (a usage error, exit 1), stdout for -h/--help (see
// handleHelp). One text for both, so the two can never drift.
const supersedeUsage = `Usage: ghost supersede <project> [flags]

Flags:
  --apply             Write the supersedes/causes links (default is dry-run/preview)
  --reassess          Re-judge the supersedes links ALREADY in the graph and, with
                      --apply, withdraw the ones that no longer hold. This is how a
                      wrong supersession is repaired. --threshold is not used: there
                      are no candidates to select.
  --withdraw <source-id> <target-id>
                      Withdraw the ONE supersedes link from source-id to target-id.
                      Each id may be a full memory id or an unambiguous prefix of
                      one (8 or more characters). Repeatable. --apply writes the
                      unsupersede history row; without it nothing is written.
                      --source and --threshold are not used: nothing is classified.
                      Cannot be combined with --reassess (run them as two commands).
  --threshold float   Min cosine similarity for a candidate pair (default 0.80)
  --source string     CLI harness to classify through: claude-code, opencode,
                      codex, or goose. Defaults to the calling harness
                      (detected from the environment and process ancestry); an
                      undetectable caller is an error.
  --project string    Project name; an alternative to the positional form that
                      takes the next argument verbatim, so dash-prefixed names work.

Classifies each candidate as supersedes, causes, reversed, or neither. A
supersedes answer has to name the older note's claim that no longer holds, and
one that cannot is neither: a supersedes link demotes its target in ranking and
marks it resolved, so an edge between two notes that are both still true takes a
live memory out of every later session. A reversed verdict — the older note is
the current one and the newer note restates an obsolete claim — is refused
rather than written, because a supersedes link only ever points from the newer
note to the older one. Runs through the configured CLI harness of the calling
session (--source overrides; otherwise detected from the environment and process
ancestry — an undetectable caller is an error, never a fallback to a different
harness). The harness owns its authentication and billing.

--withdraw is the other repair, and the one for an edge the rules still accept:
--reassess withdraws what the current rubric rejects, so a pair that is wrong for
a reason no rubric can see (the newer note is not a replacement of the older one
at all) keeps its edge and buries its target. --withdraw removes the edge you
name, on your say-so, without asking a model. It settles the WHOLE request before
writing anything, so a pair that names no live edge withdraws none of them, and
an ambiguous prefix is a refusal listing the matches rather than a guess.

Withdrawing an edge (--withdraw --apply, or --reassess --apply) writes the
unsupersede history row and leaves the resolution it may have caused in place:
resolve treats a live edge as a floor, so that resolution becomes clearable only
now. BOTH runs therefore print their own follow-up — the exact

  ghost resolve <project> --reassess --only <those ids> --apply

— and writes the same id list under the data dir's scratch, ready for
--only-file. Prefer that scoped command over a bare "ghost resolve <project>
--reassess": an unscoped repair re-judges every resolved memory in the project,
not only the ones this withdrawal orphaned.
`

// supersedeWithdrawReport renders the --withdraw result: the count, then one line
// per named edge carrying the memory it was burying, and the step that un-hides
// it. The per-edge row is the point of the pass — an operator withdrawing an
// edge they believe is wrong has to be able to confirm from the output that it
// was the right edge, which is why the target's own first line is on it and not
// only its id.
//
// Four markers, because under --apply a row can be in none of the two states the
// other modes use. A concurrent pass withdrew the edge first, so this run moved
// nothing: "already gone", because claiming a withdrawal would claim a deletion
// that did not happen. Or an earlier row's write failed and this one was never
// reached, so the edge is STILL LIVE: "not reached", which is the marker a reader
// must not mistake for either of the others.
func supersedeWithdrawReport(projectName string, res supersede.WithdrawResult, apply bool) string {
	// A request that named no edge says nothing. It always came with an error —
	// a refused pair is the only way to get here — and a header reading "0
	// supersedes edge(s) named, withdrew 0" above that error is a report about a
	// graph nobody asked about, printed as though it were the answer.
	if res.Resolved == 0 {
		return ""
	}
	verb, count := "would withdraw", res.Resolved
	if apply {
		verb, count = "withdrew", res.Withdrawn
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d supersedes edge(s) named, %s %d\n", projectName, res.Resolved, verb, count)
	for _, l := range res.Links {
		marker := "would withdraw"
		if apply {
			switch {
			case l.Withdrawn:
				marker = "withdrew   "
			case l.NotAttempted:
				marker = "not reached"
			case l.WithdrawalFailed:
				marker = "FAILED    "
			default:
				marker = "already gone"
			}
		}
		// The edge's own source column, because it decides the follow-up: only a
		// 'supersedes'/'llm' edge is one resolve's piggyback ever stamps on
		// account of. The target's text goes through the stored-content
		// substitution with no category, because an edge records none: the marker
		// says the format and the length, which is all a reader of this line has.
		fmt.Fprintf(&b, "  %s  %s -> %s  [%s, strength %.2f]  %s\n",
			marker, shortID(l.SourceID), shortID(l.TargetID), l.LinkSource, l.Strength,
			displayStored(l.TargetText, "", 70))
	}
	if !apply {
		b.WriteString("\nRe-run with --apply to withdraw these edges.\n")
	}
	// The report ends here. The other half of the repair — the resolve pass that
	// clears the resolution this edge justified — is printed by the caller through
	// the SAME formatter --reassess uses, so both repairs emit one scoped command
	// for it. A second, hand-written hint here would be a second command string to
	// keep current, and #702 made the unscoped one wrong to suggest.
	return b.String()
}

// supersedeReassessReport renders the --reassess result: the per-outcome counts,
// then one line per edge the pass withdrew or would withdraw. The list is the
// point of the pass — a repair whose edges cannot be read is a repair nobody
// can decide about — and each line names the rule that withdrew the edge, which
// is what separates a wrong edge from a genuinely obsolete one.
//
// The count follows resolve's repair report: a dry run reports what the pass
// would do (the list it is about to print) and --apply reports what it did.
// Printing ReassessResult.Withdrawn in a dry run would put "would withdraw 0"
// above a list of three edges, because that field counts only invalidations that
// actually landed.
func supersedeReassessReport(projectName string, res supersede.ReassessResult, apply bool, withdrawn []supersede.WithdrawnEdge, calls, retries int) string {
	verb := "would withdraw"
	causesVerb := "would sweep"
	count := len(withdrawn)
	if apply {
		verb = "withdrew"
		causesVerb = "swept"
		count = res.Withdrawn
	}
	var b strings.Builder
	sweptNote := ""
	if res.CausesSweepFailed > 0 {
		sweptNote = fmt.Sprintf(", %d causes sweep(s) FAILED (unknown)", res.CausesSweepFailed)
	}
	// A prediction the pass could not read is its own sentence, beside the sweep
	// failures rather than inside them: no sweep ran, so "FAILED" would name a
	// write that never happened — and "would sweep 0" for a read that never
	// happened would tell an operator about to apply that there is nothing else
	// to delete.
	if res.CausesPredictionFailed > 0 {
		sweptNote += fmt.Sprintf(", %d causes prediction(s) unavailable (read failed, unknown)", res.CausesPredictionFailed)
	}
	// Pairs the classifier never answered (#699). The count and the list are
	// both here because the withdrawal lines below can look like the whole
	// story: a pass that withdrew six vetoed edges and judged nothing else is a
	// partial repair, and the edges it never reached have to be findable rather
	// than countable. The wording names BOTH ways a pair reaches this list — a
	// call that failed and a reply whose verdict count did not match the pairs
	// asked about — because on the second one the harness is fine and sending
	// the operator after the network would be a wrong lead printed by the
	// report whose whole job is the partial state.
	unjudgedNote := ""
	if n := len(res.Unjudged); n > 0 {
		unjudgedNote = fmt.Sprintf(", %d unjudged (no verdict: the classify call failed or answered with the wrong number of verdicts; their edges stand)", n)
	}
	fmt.Fprintf(&b, "%s: %d live supersedes edge(s), %d not judged, %d vetoed, %d still supersedes, %d neither, %d causes, %d reversed, %d UNKNOWN%s, %s %d, %s %d causes edge(s)%s (%d classify call(s)%s)\n",
		projectName, res.Loaded, res.Skipped, res.Vetoed, res.Confirmed, res.Neither, res.Causes, res.Reversed,
		res.Unclassified, unjudgedNote, verb, count, causesVerb, res.CausesWithdrawn, sweptNote, calls, retryNote(retries))
	for _, w := range withdrawn {
		// Three markers, because under --apply a row can be neither of the two
		// the other modes use: a concurrent pass withdrew the supersedes edge
		// first, and this run's own sweep may still have taken the 'causes' one.
		// Calling that "would withdraw" would claim a deletion that did not
		// happen and hide one that did.
		marker := "would withdraw"
		switch {
		case w.Written:
			marker = "withdrew   "
		case apply:
			marker = "already gone"
		}
		// A vetoed row was settled with no harness call, and a false veto here
		// deletes a correct edge the ordinary pass will not re-create, so the
		// line says which of the two decided it rather than leaving the reader
		// to infer it from the reason's wording.
		by := "classifier"
		if w.Vetoed {
			by = "veto, no harness call"
		}
		// The sweep removes a second graph row, so a row that has one says so:
		// an operator applying this is deciding about that deletion too, and on
		// the veto rows it is the only deletion no model adjudicated. A sweep
		// that ERRORED says "unknown" rather than a count, because the count is
		// not knowable after a failed write and a 0 would read as "nothing else
		// was deleted" — and so does a dry-run row whose PREDICTION could not be
		// read, which is the same mistake one step earlier.
		swept := ""
		switch {
		case w.SweepFailed:
			swept = "  [causes sweep FAILED — unknown]"
		case w.PredictionUnknown:
			swept = "  [causes edge — unknown: the prediction read failed]"
		case w.CausesSwept > 0:
			swept = fmt.Sprintf("  [+%d causes edge]", w.CausesSwept)
		}
		fmt.Fprintf(&b, "  %s  %s -> %s  [%s]%s  %s\n", marker, shortID(w.NewerID), shortID(w.OlderID), by, swept, w.Reason)
	}
	// One line per unjudged edge, in the same shape as the withdrawn rows so a
	// reader can tell at a glance which edges moved and which did not. The
	// reason is the same in every row — no verdict exists — so it is the state,
	// not a per-edge finding, and the pass exits non-zero for the rerun. It
	// names both causes for the reason the summary does.
	for _, u := range res.Unjudged {
		fmt.Fprintf(&b, "  unjudged    %s -> %s  [no verdict: the classify call failed or answered with the wrong number of verdicts, so the edge stands and the next pass re-asks it]\n",
			shortID(u.NewerID), shortID(u.OlderID))
	}
	if !apply && len(withdrawn) > 0 {
		b.WriteString("\nRe-run with --apply to withdraw these edges.")
	}
	return b.String()
}

// supersedeReport renders the pass's per-outcome report: the one-line summary
// followed by the deterministic veto's count. A pass that declined work it did
// not do and printed the same totals as a pass that found nothing to do reads
// as "nothing was skipped", so the veto is on the report (#686) — and it is
// printed by the one call below, so the report and the pass cannot drift.
//
// A retried call is on the report too, and only when there was one: a pass that
// had to re-ask a failed call is not the pass the summary describes, and a
// harness that is flapping shows up here before it shows up as a failure.
func supersedeReport(projectName string, res supersede.Result, verb string, calls, retries int) string {
	out := fmt.Sprintf("%s: %d candidate pairs in %d classify call(s)%s, %d cached, %d supersedes, %d causes, %d reclassified, %s\n",
		projectName, res.Candidates, calls, retryNote(retries), res.Skipped, res.Confirmed, res.CausesCreated, res.Reclassified, verb)
	if res.Vetoed == 0 {
		return out
	}
	return out + fmt.Sprintf("  %d pair(s) vetoed: the older note states a rule and the newer note does not name it retired — no classify call, no link, and not cached (re-decided free on a later pass)\n", res.Vetoed)
}

// retryNote is the ", N retried call(s)" clause the call counts share: empty
// when nothing was retried, so an ordinary pass's line is unchanged.
func retryNote(retries int) string {
	if retries <= 0 {
		return ""
	}
	return fmt.Sprintf(", %d retried after a failed call", retries)
}

// supersedeEmbedBound is how many memories one `ghost supersede` pass will embed
// for itself. It is the embedding worker's own project batch today, and stays a
// separate number on purpose: a daemon's backfill and an operator's wait are
// different problems, and a pass that had to embed a whole corpus would be doing
// the first one inside a command the second is blocked on. The rows past the
// bound are the sweep's — this is a pass making the corpus it is about to read
// searchable, not a backfill.
const supersedeEmbedBound = 50

// supersedeEmbedBudget is the whole pre-scan's time budget, and it exists
// because the bound alone is not a bound on TIME. The client allows 30s per
// request, so 50 memories against an endpoint that answers its liveness probe and
// then stalls every embed is half an hour of blocking before the candidate scan
// starts, with nothing printed for any of it.
//
// A minute, which is generous on purpose: a healthy local endpoint embeds a short
// memory in tens of milliseconds, so 50 rows here are seconds, and the budget is
// only reached by an endpoint that is slow per memory or slow on one of them. A
// ceiling this low that a healthy machine could hit would be a budget that
// silently truncated ordinary work, so the arithmetic is checked against the slow
// case instead: 50 rows at just over a second each, which is several times a real
// embed.
//
// Running out is not a failure, and it is not silent either — the pass says it
// stopped early and that a re-run continues the batch (see supersedeIndexNotes).
// That is the whole reason the ceiling can be this low, and it is also why the
// cause is reported rather than left to be inferred: a pass that cut itself off is
// not a daemon that is behind, and the two have different remedies.
const supersedeEmbedBudget = time.Minute

// supersedeIndexFacts is everything the report has to say about the vector index
// behind the pass: what this run wrote, how much of the project it could not
// read, and which cause applies to the part it could not read.
//
// One struct and one formatter rather than two notes taking a growing list of
// numbers, because the facts are not independent: a budget that expires makes
// BOTH lines different, and a boolean threaded through two signatures is how they
// would drift.
type supersedeIndexFacts struct {
	// embedded is what this pass wrote itself, before the scan.
	embedded int
	// unscored is what the scan could not read at all (Result.Unscored).
	unscored int
	// embeddingOn is the config's embedding.enabled.
	embeddingOn bool
	// budgetExpired is that this pass ran out of its own pre-scan budget.
	budgetExpired bool
}

// supersedeIndexNotes renders the two lines that make the candidate totals
// readable, and prints nothing at all for a pass that found every memory scorable
// and had nothing to write — an ordinary pass's answer to the operator's question
// is that there was nothing to do, which an unconditional "0" line would bury.
//
// Both lines are scoped to the NEW candidates the vector scan proposes, and say
// so in the same words, because the two are printed one after the other and the
// reclassified count above them is NOT bounded by the scan: the reclassify half
// works from link rows and reads no vector, so a memory with no vector can still
// appear in a pair this run considered.
func supersedeIndexNotes(f supersedeIndexFacts) string {
	var b strings.Builder
	if f.embedded > 0 || f.budgetExpired {
		b.WriteString(supersedeEmbeddedNote(f))
	}
	if f.unscored > 0 {
		b.WriteString(supersedeUnscoredNote(f))
	}
	return b.String()
}

// supersedeEmbeddedNote reports the vectors this pass had to produce before it
// could read the corpus, and what it means when it did not finish: a re-run
// continues the batch, which is the remedy, and naming the worker instead would
// send the operator after a daemon that was never behind.
func supersedeEmbeddedNote(f supersedeIndexFacts) string {
	if f.budgetExpired {
		return fmt.Sprintf("  %d %s embedded for this pass, then its %s pre-scan budget ran out — re-run `ghost supersede` to continue the batch; the rest are not waiting on anything\n",
			f.embedded, plural(f.embedded, "memory", "memories"), supersedeEmbedBudget)
	}
	return fmt.Sprintf("  %d %s embedded for this pass — they had no vector yet, and a note with no vector is proposed as no new candidate by the vector scan\n",
		f.embedded, plural(f.embedded, "memory", "memories"))
}

// supersedeUnscoredNote reports the part of the project the scan could not read,
// and points at the remedy that actually applies to it. Three of them, and
// choosing wrong is worse than silence: this pass cut itself off (re-run it), the
// daemon is behind (it fills the index, and `ghost mcp status` reports coverage),
// or embedding is off (nothing is filling the index at all, and there is no worker
// to go and look at).
func supersedeUnscoredNote(f supersedeIndexFacts) string {
	subject := fmt.Sprintf("%d %s had no vector when the pass scanned, so they proposed no new candidate pairs (edges already in the graph are re-judged from the link, not the vector)",
		f.unscored, plural(f.unscored, "memory", "memories"))
	switch {
	case f.budgetExpired:
		return "  " + subject + " — this pass stopped early; re-run `ghost supersede` to continue the batch\n"
	case !f.embeddingOn:
		return "  " + subject + " — embedding is disabled, so nothing is keeping the vector index up to date\n"
	default:
		return "  " + subject + " — the index is filled by the embedding worker in `ghost mcp`, and `ghost mcp status` reports its coverage\n"
	}
}

// plural picks the noun that agrees with n, so a report line reads as English
// at 1 and at 143.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// embedSupersedeCorpus embeds the memories of projectID that have no vector yet,
// and returns how many it wrote and whether its own budget ran out first.
//
// It runs before the candidate scan, and only when embedding is enabled: with
// embedding off there is no vector index to fill.
//
// WHY a pass does this at all: `supersede` is the one lifecycle phase whose
// NEW-candidate mechanism IS the vector index — the pair is proposed by cosine —
// so a memory it has no vector for is proposed as no new candidate by that scan
// (supersede.SelectCandidates skips it, though an edge already in the graph is
// re-proposed from its link row, with no vector involved). Those vectors are
// written by the embedding worker, which lives in `ghost mcp`: another process,
// on its own schedule, running only if a server is up. A user who saves two notes
// and runs the pass in the same breath therefore gets "0 candidate pairs" — a
// clean report for a pair that was in front of them, with nothing anywhere saying
// the index had not caught up. That window is short, and it is also exactly when
// a supersession is most likely to be wanted, so the pass closes it itself instead
// of leaving the answer to a daemon's timing (#716).
//
// It is the worker's own code (embedding.Worker.EmbedPending) rather than a
// second implementation, so the two cannot drift on what "unembedded" means, on
// the identity a vector is stamped with, or on what happens when the endpoint is
// down. The worker is built with no data dir: the Ollama-down marker is the
// daemon's outage bookkeeping, and a one-shot pass has no outage to keep time.
//
// budget bounds the whole call, and it is a parameter so a test can hand this a
// few milliseconds against a stalling endpoint: the ceiling is the property, and a
// test that has to sit out the production one is a test that does not run.
//
// budgetExpired is read from the DERIVED context, and only while the caller's own
// context is still alive: a parent cancelled underneath the pass is a shutdown,
// not a budget, and the report must not offer "re-run this command" for a process
// that is being told to stop.
//
// What the pass could NOT read is counted by the scan instead
// (Result.Unscored), because that is where the fact is true: a bound reached, a
// budget that ran out, an endpoint that did not answer, and a vector written under
// another model all end up as the same honest count, where a report derived from
// the embed call could only ever be a guess.
func embedSupersedeCorpus(ctx context.Context, cfg *config.Config, store *memory.Store, projectID string, budget time.Duration, logger *slog.Logger) (int, bool) {
	if !cfg.Embedding.Enabled {
		return 0, false
	}
	bounded, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	client := embedding.NewClient(cfg.Embedding.OllamaURL, cfg.Embedding.Model, cfg.Embedding.Dimensions)
	// The endpoint's last answer is not what this caller reports: the pass
	// already carries the honest count of what it could not read in
	// Result.Unscored, and the scan is where that fact is true (see the comment
	// above). Only the count is wanted here.
	embedded, _ := embedding.NewWorker(client, store, logger, 0, "").EmbedPending(bounded, projectID, supersedeEmbedBound)
	expired := bounded.Err() != nil && ctx.Err() == nil
	return embedded, expired
}

// runSupersede implements `ghost supersede <project> [--apply]` — the creation
// half of staleness-aware ranking. It proposes newer→older 'supersedes' links
// over the project's live memories (cosine-similar candidates, a deterministic
// imperative veto, then CLI-harness confirmation) and, with --apply, writes
// them. Dry-run by default. Re-runnable: it self-heals after `ghost reflect`
// cascade-deletes links. Consumed by search only when SupersedeDemote is set. A
// reversed verdict is reported and refused, never written (#641); a pair whose
// older note states a rule the newer note never retires is vetoed for free
// (#686). See docs/benchmarks.md Phase 3.
//
// --withdraw and --reassess are the two repair modes, and they are dispatched
// before anything else because --withdraw makes NO harness call: it is the
// operator's own judgement, so a machine with no detectable calling harness can
// still repair an edge, and nothing about it is billed. Both are dry-run by
// default; the --withdraw report also names the `ghost resolve --reassess --apply`
// step that clears a resolution a withdrawn edge caused, which is the half of
// this repair the graph cannot do on its own.
//
// Which leaves the creation path, and the one thing it does before any of that is
// embed the project's memories that have no vector yet (embedSupersedeCorpus).
// It has to: candidates are proposed by cosine, and those vectors are written by
// the embedding worker in another process, so a pass started seconds after a save
// would otherwise report an empty result for a corpus the operator can see (#716).
// The other two modes read no vectors, which is why the pre-scan sits after them.
func runSupersede() {
	projectName, source, apply, reassess, threshold, withdrawPairs, parseErr := parseSupersedeArgs(os.Args[2:])
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", parseErr)
		os.Exit(1)
	}
	if projectName == "" {
		fmt.Fprint(os.Stderr, supersedeUsage)
		os.Exit(1)
	}

	cfg, logger, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck
	ctx := context.Background()

	projectID := resolveProjectOrExit(ctx, store, projectName)

	if len(withdrawPairs) > 0 {
		runSupersedeWithdraw(ctx, store, logger, projectName, projectID, withdrawPairs, apply)
		return
	}

	source, err := detectPhaseSource(source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	applyPhaseModel(cfg.CLI.ModelSupersede, source)
	provider, err := buildClassifyProviderForSource(cfg, source)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: ghost supersede %v\n", err)
		os.Exit(1)
	}
	cls := supersede.NewRelationClassifier(provider)
	cls.SetLogger(logger)

	if reassess {
		res, withdrawn, err := supersede.Reassess(ctx, store, cls, projectID, apply, logger)
		// The report is printed before the error is raised, and the non-zero
		// exit stays: each invalidation is its own transaction, so a failure on
		// the Nth edge leaves N-1 already withdrawn and unreachable by a later
		// pass. A repair that partly happened has to be visible as such.
		fmt.Print(supersedeReassessReport(projectName, res, apply, withdrawn, cls.Calls(), cls.Retries()))
		// The follow-up too, for the same reason and before the exit: the edges
		// that did land orphaned resolutions that only a scoped resolve repair
		// can clear, and an operator who does not learn that from this run
		// learns it from a memory that stayed out of every session.
		if targets := withdrawnTargets(withdrawn); apply && len(targets) > 0 {
			path, werr := writeReassessTargets(projectName, targets, "ghost supersede --reassess --apply")
			if werr != nil {
				// The repair already landed and the command is printed either
				// way, so a scratch file that could not be written is a warning
				// and not a failed run.
				fmt.Fprintf(os.Stderr, "warning: write the follow-up id file: %v\n", werr)
			}
			fmt.Print(supersedeReassessFollowup(projectName, targets, path))
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	// The corpus the scan is about to read, made readable first: the vectors it
	// needs are normally written by the embedding worker inside `ghost mcp`, and
	// this pass cannot wait on another process to notice a save (see
	// embedSupersedeCorpus). It is the only phase here that has to do this, and
	// it happens after the two repair modes have returned, because neither of them
	// reads the vector index at all.
	embedded, budgetExpired := embedSupersedeCorpus(ctx, cfg, store, projectID, supersedeEmbedBudget, logger)

	res, classified, err := supersede.Run(ctx, store, cls, projectID, threshold, apply, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	verb := "would link"
	if apply {
		verb = "linked"
	}
	fmt.Print(supersedeReport(projectName, res, verb, cls.Calls(), cls.Retries()))
	fmt.Print(supersedeIndexNotes(supersedeIndexFacts{
		embedded: embedded, unscored: res.Unscored,
		embeddingOn: cfg.Embedding.Enabled, budgetExpired: budgetExpired,
	}))
	if res.Unclassified > 0 {
		fmt.Printf("  %d pair(s) skipped: unclassifiable verdict (logged; the pass still completed)\n", res.Unclassified)
	}
	if res.Reversed > 0 {
		// Only what holds for every counted pair: a supersedes link is never
		// written for one, in either mode, and the verdict never reaches the
		// NEITHER cache. Whether the pair comes back as a candidate, and
		// whether --apply dropped an existing link, are per-pair.
		// The drop is stated as what --apply does to the pair, not as what a run
		// will have done: the apply path skips it for a pair a concurrent pass
		// already invalidated, and the log carries the per-pair truth.
		fmt.Printf("  %d pair(s) refused: reversed verdict — the older note is the current one, so no supersedes link is written and the verdict is not cached (re-asked on a later pass); under --apply the pair's supersedes/causes links are dropped\n", res.Reversed)
	}
	for _, c := range classified {
		switch c.Relation {
		case supersede.RelationSupersedes:
			fmt.Printf("  %s  supersedes  %s\n", shortID(c.NewerID), shortID(c.OlderID))
		case supersede.RelationCauses:
			fmt.Printf("  %s  causes  %s\n", shortID(c.OlderID), shortID(c.NewerID))
		case supersede.RelationReversed:
			fmt.Printf("  %s  reversed, not written: %s supersedes it\n", shortID(c.NewerID), shortID(c.OlderID))
		}
	}
	if !apply && res.WouldWriteLinks() {
		fmt.Println("\nRe-run with --apply to write these links.")
	}
}

// resolveArgs is `ghost resolve`'s parsed command line, as one value rather
// than seven positional returns: --only and --only-file arrived with the repair
// scope (#698) and a call site that had to remember which string was which would
// be one transposition away from judging the wrong memories.
type resolveArgs struct {
	project  string
	source   string
	apply    bool
	reassess bool
	// only holds the --only selectors together with the --only-file lines, in
	// that order. Both flags may be given and the list is the union: the
	// supersede repair prints both forms for one set of ids, and an operator
	// who pastes both meant those ids, not a mistake worth refusing over.
	only []string
	// onlyFile is the path as typed, kept for the report line that names the
	// file the selectors came from.
	onlyFile string
	// mark and markFile name the memories to stamp resolved_at (#714), in the
	// same forms --only/--only-file take: a comma-separated list, or one id or
	// prefix per line in a file. They are separate fields rather than a mode
	// enum over one list because the two halves of a repair are both about
	// memories and an operator who gave both has asked for a command this
	// parser refuses outright — a state worth a field each and an explicit
	// check, not a mode whose value nobody can read.
	mark     []string
	markFile string
}

// hasMark reports whether the command line asked for a targeted mark. It is a
// method rather than a repeated len() test so the dispatch, the parser's
// refusal and the report cannot each spell the condition differently.
func (a resolveArgs) hasMark() bool { return len(a.mark) > 0 || a.markFile != "" }

// runSupersedeWithdraw implements `ghost supersede <project> --withdraw <source>
// <target> [--apply]`: the operator's own withdrawal of the named edges.
//
// It is a named function because runSupersede ends in os.Exit, and this path is
// worth driving on its own: the report is printed before the error is raised and
// the non-zero exit stays, so a request of several pairs where one write failed
// reports the edges that did land (each invalidation is its own transaction and
// a later pass will not see them again) rather than reporting nothing about a
// repair that partly happened. A refusal — an ambiguous ref, a pair with no live
// edge — writes nothing at all, so there is no partial withdrawal to report.
func runSupersedeWithdraw(ctx context.Context, store *memory.Store, logger *slog.Logger, projectName, projectID string, pairs []supersedePair, apply bool) {
	res, err := supersede.Withdraw(ctx, store, projectID, toWithdrawPairs(pairs), apply, logger)
	fmt.Print(supersedeWithdrawReport(projectName, res, apply))
	// The follow-up through the SAME helpers --reassess uses, and for the same
	// reason: the resolution this edge justified is cleared by a SCOPED resolve
	// repair, and an unscoped one re-judges every resolved memory in the project
	// (#698 measured that proposing to un-hide 143 rows, about 35% of them stale).
	// One formatter and one file writer means the two repairs cannot drift into
	// printing different commands for the same situation. It is printed before
	// the error below, for the reason that error exists at all: a partial
	// withdrawal still orphaned the targets it did reach.
	if targets := withdrawnLinkTargets(res.Links); apply && len(targets) > 0 {
		path, werr := writeReassessTargets(projectName, targets, "ghost supersede --withdraw --apply")
		if werr != nil {
			// The withdrawal already landed and the command is printed either way,
			// so a scratch file that could not be written is a warning and not a
			// failed run — the same trade --reassess makes.
			fmt.Fprintf(os.Stderr, "warning: write the follow-up id file: %v\n", werr)
		}
		fmt.Print(supersedeReassessFollowup(projectName, targets, path))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// withdrawnLinkTargets is the follow-up's id list for a --withdraw run: the
// targets of the edges it withdrew, deduplicated, in the order the rows were
// reported. #702's withdrawnTargets does this for the --reassess report's own row
// type; the two cannot share one function without a type parameter over two
// structs that differ in one field name, and a wrong answer here clears the
// wrong memories.
//
// Every row counts except the two whose edge is STILL LIVE. A row a concurrent
// pass took first is in the list, on withdrawnTargets' reasoning above: that pass
// left the same state behind — no live edge, a resolved_at nothing defends — so
// the target is just as repairable as one this process withdrew. A row never
// reached, or one whose write failed, is out: its edge still points at the
// target, so the repair would report it as still asserted and clear nothing.
func withdrawnLinkTargets(links []supersede.WithdrawnLink) []string {
	var out []string
	seen := make(map[string]bool, len(links))
	for _, l := range links {
		if l.TargetID == "" || seen[l.TargetID] || l.NotAttempted || l.WithdrawalFailed {
			continue
		}
		seen[l.TargetID] = true
		out = append(out, l.TargetID)
	}
	return out
}

// toWithdrawPairs maps the parsed command line onto the core's request, so the
// argv shape and the domain shape stay separate types.
func toWithdrawPairs(pairs []supersedePair) []supersede.WithdrawPair {
	out := make([]supersede.WithdrawPair, 0, len(pairs))
	for _, p := range pairs {
		out = append(out, supersede.WithdrawPair{Source: p.source, Target: p.target})
	}
	return out
}

// shortID is the report form of a memory id: its first eight CHARACTERS, which
// is the form every Ghost report prints and therefore the form an operator has
// on screen when they paste one into --withdraw. Ids are longer than 8 only
// because they are random, so the truncation is presentation and never a
// lookup key — the one path that resolves an id prefix does its own matching.
//
// By characters, not bytes, and that is what makes it pasteable at all. An id is
// not necessarily hex — `ghost import` writes an artifact's ids verbatim — so
// `id[:8]` on a CJK id is invalid UTF-8 on the report line and a selector the
// prefix query can never match. internal/supersede.short, internal/mcpserver's
// shortID and internal/followup's renderer all measure the same eight the same
// way, because the premise of the feature is that the printed id is the one you
// can hand back.
func shortID(id string) string {
	if utf8.RuneCountInString(id) > 8 {
		return string([]rune(id)[:8])
	}
	return id
}

// parseResolveArgs parses `ghost resolve`'s arguments (everything after the
// subcommand word). Hand-rolled, matching the historical loop exactly: exactly
// one project — positionally (unchanged back-compat) or from --project, which
// takes the NEXT argument verbatim so a dash-leading name such as -x or --odd
// is a name, not a flag (a second project in any mixture keeps the historical
// "expected exactly one project" error); value flags in both "--flag value"
// and "--flag=value" forms; any other flag an unknown-flag error — which the
// caller prints and exits on. A valueless --project is an error. --reassess
// selects the repair pass over already-resolved memories (issue #640); it
// combines with --apply exactly like it does on the ordinary pass.
//
// --only/--only-file scope the repair pass to the memories it is about (#698)
// and are refused without --reassess. The ordinary pass has no resolved pool to
// narrow, so accepting them there would be two flags that parse, say nothing in
// the report and change no decision. A scope flag that yields NO selector is also
// an error rather than an absent scope: `--only "$IDS"` with IDS unset is a
// routine shell mistake, and an unscoped `--apply` is the one repair outcome
// that must never happen by accident.
//
// --mark/--mark-file name the memories to stamp resolved_at, and are refused
// with --reassess for the reason --withdraw is (#714): the two are the two
// directions of the same stamp, and a command asking for both would have two
// dry-run answers. They take the same comma-separated list and the same
// one-per-line file as --only/--only-file, and the reader is told which of the
// two repairs they asked for twice. Extracted from runResolve so the argv
// contract is unit-testable without os.Exit.
func parseResolveArgs(args []string) (resolveArgs, error) {
	var out resolveArgs
	// addRefs records the comma-separated selectors one flag value carries, and
	// refuses a value that yields none — so the check is the same whether the
	// value arrived as "--only x" or "--only=x", and neither can leave the list
	// silently empty. It is one function for both flags because the only
	// difference between them is which field the refs land in, and a second copy
	// would be a second place for the empty-value refusal to be forgotten.
	addRefs := func(flag string, dst *[]string, value string) error {
		added := 0
		for _, part := range strings.Split(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				*dst = append(*dst, part)
				added++
			}
		}
		if added == 0 {
			return fmt.Errorf("%s requires at least one memory id or prefix", flag)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--apply":
			out.apply = true
		case args[i] == "--reassess":
			out.reassess = true
		case args[i] == "--mark" && i+1 < len(args):
			if err := addRefs("--mark", &out.mark, args[i+1]); err != nil {
				return resolveArgs{}, err
			}
			i++
		case strings.HasPrefix(args[i], "--mark="):
			if err := addRefs("--mark", &out.mark, strings.TrimPrefix(args[i], "--mark=")); err != nil {
				return resolveArgs{}, err
			}
		case args[i] == "--mark":
			return resolveArgs{}, errors.New("--mark requires at least one memory id or prefix")
		case args[i] == "--mark-file" && i+1 < len(args):
			// The raw value is stored, the trimmed one is what is checked: a
			// path may legitimately contain a space, and an empty or blank one
			// is the unset-variable mistake that must not become a request that
			// names every memory in the project.
			if strings.TrimSpace(args[i+1]) == "" {
				return resolveArgs{}, errors.New("--mark-file requires a path")
			}
			out.markFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--mark-file="):
			v := strings.TrimPrefix(args[i], "--mark-file=")
			if strings.TrimSpace(v) == "" {
				return resolveArgs{}, errors.New("--mark-file requires a path")
			}
			out.markFile = v
		case args[i] == "--mark-file":
			return resolveArgs{}, errors.New("--mark-file requires a path")
		case args[i] == "--project":
			if i+1 >= len(args) {
				return resolveArgs{}, errors.New("--project requires a value")
			}
			if out.project != "" {
				return resolveArgs{}, errors.New("expected exactly one project")
			}
			out.project = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--project="):
			v := strings.TrimPrefix(args[i], "--project=")
			if v == "" {
				return resolveArgs{}, errors.New("--project requires a value")
			}
			if out.project != "" {
				return resolveArgs{}, errors.New("expected exactly one project")
			}
			out.project = v
		case args[i] == "--source" && i+1 < len(args):
			out.source = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--source="):
			out.source = strings.TrimPrefix(args[i], "--source=")
		case args[i] == "--only" && i+1 < len(args):
			if err := addRefs("--only", &out.only, args[i+1]); err != nil {
				return resolveArgs{}, err
			}
			i++
		case strings.HasPrefix(args[i], "--only="):
			if err := addRefs("--only", &out.only, strings.TrimPrefix(args[i], "--only=")); err != nil {
				return resolveArgs{}, err
			}
		case args[i] == "--only":
			return resolveArgs{}, errors.New("--only requires at least one memory id or prefix")
		case args[i] == "--only-file" && i+1 < len(args):
			// The raw value is stored, the trimmed one is what is checked: a
			// path may legitimately contain a space, and an empty or blank one
			// is the unset-variable mistake that must not become an unscoped
			// repair.
			if strings.TrimSpace(args[i+1]) == "" {
				return resolveArgs{}, errors.New("--only-file requires a path")
			}
			out.onlyFile = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--only-file="):
			v := strings.TrimPrefix(args[i], "--only-file=")
			if strings.TrimSpace(v) == "" {
				return resolveArgs{}, errors.New("--only-file requires a path")
			}
			out.onlyFile = v
		case args[i] == "--only-file":
			return resolveArgs{}, errors.New("--only-file requires a path")
		case !strings.HasPrefix(args[i], "-"):
			if out.project != "" {
				return resolveArgs{}, errors.New("expected exactly one project")
			}
			out.project = args[i]
		default:
			return resolveArgs{}, fmt.Errorf("unknown flag %q", args[i])
		}
	}
	if len(out.mark) > 0 || out.markFile != "" {
		if out.reassess {
			return resolveArgs{}, errors.New("--mark stamps the memories you name and --reassess re-judges the ones ALREADY resolved; run them as two commands")
		}
		if len(out.only) > 0 || out.onlyFile != "" {
			// Unreachable in practice — --only is refused without --reassess, and
			// --mark is refused with it — but a parser that can produce a state
			// it cannot explain is a parser whose next flag will produce one it
			// can, and the message costs a line.
			return resolveArgs{}, errors.New("--mark stamps the memories you name and --only/--only-file scope the repair pass; run them as two commands")
		}
	}
	if len(out.only) > 0 || out.onlyFile != "" {
		if !out.reassess {
			return resolveArgs{}, errors.New("--only/--only-file scope the repair pass: use them with --reassess")
		}
	}
	return out, nil
}

// readRefSelectors reads a --only-file or a --mark-file: one memory id or 8+
// character prefix per line, '#' starting a comment, blank lines ignored. A
// comment is a WHOLE line starting with '#' (after optional leading space), which
// is what an annotated file looks like, and the id is taken verbatim from the rest
// of the line.
//
// flag is the spelling as typed, and it is the only thing that differs between the
// two callers: the format, the comment rule and every refusal are the same, and a
// second reader would be a second place for the two to disagree about which ids a
// file can name. It appears in the error messages because an operator who passed
// --mark-file and was told "read --only-file" would be sent to debug a flag they
// did not type.
//
// It used to strip from the first '#' anywhere, including mid-line, on the
// premise that a selector can never contain one — an id is hex, and so is a
// prefix of one. That stopped being true when `ghost import` made a stored id
// whatever its artifact said, and this file is the only surface that can carry
// some of those ids, so truncating one at a '#' turned it into a selector naming
// no row and the repair reported a miss for a memory it had just called
// repairable. A trailing comment therefore has to be a line of its own; the cost
// is one annotation style, and it buys an id that is still nameable.
//
// A file that yields no selectors is an error rather than an unscoped run. The
// operator pointed the pass at a file, and judging the whole project because
// the file turned out to be empty is the one outcome that must never happen by
// accident — and for --mark the same emptiness would otherwise be a mark of
// nothing, which reads as a completed no-op rather than as the refusal it is.
func readRefSelectors(flag, path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", flag, path, err)
	}
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		// A '#' starts a comment when it begins the line or follows whitespace, and
		// is part of the id otherwise. The rule used to be "the first '#' anywhere",
		// on the stated premise that a selector can never contain one — an id is
		// hex. `ghost import` made that false, and this file is the only surface
		// that can carry some of those ids.
		//
		// "Follows whitespace" is what keeps the old behaviour. The first attempt at
		// this rule was "a '#' only at the first non-space character", which is
		// narrower than it reads: `<id>   # the changelog note` became ONE selector,
		// prefixKey refused it as non-hex, and the repair aborted having judged
		// nothing — a file that worked now failing wholesale over its own
		// annotation. So a comment is a '#' that starts a word, and the one shape
		// that costs is an id containing " #", which no comment rule can have both
		// ways.
		if idx := strings.IndexByte(line, '#'); idx > 0 && (line[idx-1] == ' ' || line[idx-1] == '\t') {
			line = line[:idx]
		}
		if line = strings.TrimSpace(line); line != "" && line[0] != '#' {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s %s names no memory ids or prefixes", flag, path)
	}
	return out, nil
}

// resolveUsage is the help for `ghost resolve`: stderr when the project comes
// out empty (a usage error, exit 1), stdout for -h/--help (see handleHelp).
// One text for both, so the two can never drift.
const resolveUsage = `Usage: ghost resolve <project> [flags]

Flags:
  --apply         Stamp resolved_at on confirmed memories (default is dry-run/preview)
  --reassess      Re-judge memories that are ALREADY resolved and, with --apply,
                  clear resolved_at on the ones that now come back KEEP. This is
                  how a wrong resolution is repaired.
  --mark ids      Stamp resolved_at on THESE memories, on your say-so: a
                  comma-separated list of memory ids, or of 8+ character
                  prefixes. Nothing is classified, so no harness is called and
                  nothing is billed. A full id is taken as given whatever its
                  shape (an imported one can hold a space, a dash or a '#'); an
                  ambiguous prefix is refused with the matches listed, and a
                  memory already resolved is reported as a no-op. --source is
                  not used. Cannot be combined with --reassess (they are the two
                  directions of the same stamp; run them as two commands).
  --mark-file p   With --mark: the same list, read from a file, one id or prefix
                  per line. For a list too long to type on one line.
  --only ids      With --reassess: judge only these memories — a comma-separated
                  list of memory ids, or of 8+ character hex prefixes of them.
                  A full id is taken as given whatever its shape (an imported
                  one can hold a space, a dash or a '#'); the hex rule is about
                  a prefix. An id holding a comma cannot be named here, because
                  this list is comma-separated — --only-file can, and an id
                  holding a newline is reachable through neither.
  --only-file p   With --reassess: the same, read from a file: one id or prefix
                  per line. A '#' starts a comment when it begins the line or
                  follows whitespace, so "<id>   # note" annotates and
                  "<id>#note" is one id. For a list too long to type on one line.
  --source string CLI harness to classify through: claude-code, opencode,
                  codex, or goose. Defaults to the calling harness (detected
                  from the environment and process ancestry); an undetectable
                  caller is an error.
  --project string Project name; an alternative to the positional form that
                  takes the next argument verbatim, so dash-prefixed names work.

Marks resolved-evidence memories so they drop from session-start injection
(still searchable). Runs through the configured CLI harness of the calling
session (--source overrides; otherwise detected from the environment and process
ancestry — an undetectable caller is an error, never a fallback to a different
harness). The harness owns its authentication and billing.

Scope a repair with --only or --only-file rather than running a bare
--reassess: an unscoped repair re-judges EVERY resolved memory in the project,
and on a real store that proposed un-hiding memories which had been resolved
for good reasons. "ghost supersede <project> --reassess --apply" prints the
exact --only command that repairs what its withdrawal left resolved. Scoping
narrows which memories are judged and nothing else — a live 'supersedes' edge, a
correction pairing and the KEEP cache all still hold a judged row back.

--mark is the other repair, and the one for a memory no pass will propose: an
operator who has read a newer note in the same project saying the older one's fix
landed knows the older one is finished, and nothing in the pass can see that — the
note may hold no resolution keyword at all. --mark names the memories instead of
asking a model about them, which is also why it needs no harness and no --source.
It writes the same resolved_at the pass writes, through the same store path, so
it also writes the 'resolve' history row every writer appends — with YOU as the
performer, which is the only resolve row in the database that can say a person
decided it rather than a classifier. It also drops the row's KEEP cache entry, so
the pass does not report the row as cached-KEEP and bring it straight back after
any edit. The inverse is --reassess --only, and every --mark --apply report prints
the exact command for the memories it just stamped. A dry run prints none: it
stamped nothing, so a repair command would name memories the run never touched.
`

// runResolveMark implements `ghost resolve <project> --mark <ids> [--apply]`:
// the operator's own mark of the memories they name (#714).
//
// It is a named function because runResolve calls os.Exit on every error path,
// and this one is worth driving on its own: it is the only mode of `ghost
// resolve` that never asks a model anything, so the report's whole content is
// what the operator named and what became of it.
//
// The report is printed BEFORE the error is raised and the non-zero exit stays,
// and the two are not in tension here: the write is one transaction over the
// whole request, so an error is a rollback and the report is saying "none of
// these moved" rather than describing a partial mark. A refusal — an ambiguous
// ref, a memory another project owns — writes nothing at all, so there is no
// partial mark to report there either.
func runResolveMark(parsed resolveArgs, apply bool) {
	projectName := parsed.project
	// The list is read here rather than in the parser because it is file IO:
	// parseResolveArgs stays a pure function of argv, and a missing or unreadable
	// file is a usage error an operator can act on. The union is built into a
	// fresh slice so the file's lines cannot land in the parsed args' backing
	// array.
	refs := append([]string{}, parsed.mark...)
	if parsed.markFile != "" {
		fromFile, ferr := readRefSelectors("--mark-file", parsed.markFile)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", ferr)
			os.Exit(1)
		}
		refs = append(refs, fromFile...)
	}

	_, logger, store := bootstrap(os.Stderr, cliLogLevel(), failOnConfig)
	defer store.Close() //nolint:errcheck
	ctx := context.Background()
	projectID := resolveProjectOrExit(ctx, store, projectName)

	// The performer on the 'resolve' history row. Not a harness name: nothing was
	// classified here, and recording the calling harness would make the one row
	// that can say a person decided this look like a model's verdict — which is
	// the whole reason this write is a supported command rather than SQL.
	res, err := resolve.Mark(ctx, store, resolve.MarkRequest{
		ProjectID:  projectID,
		Refs:       refs,
		Provenance: memory.Provenance{Agent: markPerformer},
		Apply:      apply,
	}, logger)
	fmt.Print(resolveMarkReport(projectName, res, apply))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// markPerformer is the `agent` value on the history row an operator's mark
// writes. It names the KIND of performer, not a person: the column's vocabulary
// elsewhere is harness source tokens, and inventing a per-user identity here
// would put an unverified claim in a column whose whole purpose is being
// trustworthy. "operator" says what a reader of the history actually needs to
// know — this stamp was a person's decision, and no classifier was asked.
const markPerformer = "operator"

// resolveMarkReport renders the --mark result: the one-line summary, one row per
// named memory carrying its own text, and the command that undoes the whole
// request.
//
// The per-memory row is the point of the pass. An operator burying a memory has
// to be able to confirm from the output that it was the one they meant, which is
// the same reason the withdrawal report prints the memory its edge was burying —
// and a memory buried by mistake is invisible afterwards, so the check has to
// happen here or not at all.
//
// Every row carries a marker rather than being left to the absence of one, for
// the same reason the withdrawal report has four: a row this call did not stamp
// is one of four different things, and a report that grouped them as "not marked"
// would claim a state the reader cannot act on. "already resolved" is the issue's
// no-op, "pinned" and "standing category" are the store's guard, and "FAILED" is
// this run's own broken write.
func resolveMarkReport(projectName string, res resolve.MarkResult, apply bool) string {
	// A request that named nothing says nothing. It always came with an error — a
	// refused ref is the only way to get here, and a refusal writes nothing at
	// all — so a header reading "0 named, marked resolved 0" above that error is
	// a report about a corpus nobody asked about, printed as though it were the
	// answer. The same rule supersedeWithdrawReport follows, for the same reason.
	if res.Resolved == 0 {
		return ""
	}
	verb := "would mark resolved"
	count := 0
	for _, m := range res.Memories {
		if m.AlreadyResolved || m.Pinned || m.ExemptCategory {
			continue
		}
		count++
	}
	if apply {
		verb = "marked resolved"
		count = res.Marked
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %d named, %s %d, %d already resolved, %d pinned, %d in a standing category, %d declined\n",
		projectName, res.Resolved, verb, count, res.AlreadyResolved, res.Pinned, res.ExemptCategory, res.Declined)
	for _, m := range res.Memories {
		// The default is the CLAIM, not the absence of one, and that ordering is
		// deliberate: a state this switch does not know about must fall through
		// to a marker that says nothing was stamped, never to the success. The
		// store re-checks its eligibility guard at write time, so a row pinned or
		// recategorized between the read and the write is declined SILENTLY, and a
		// memory buried by mistake is invisible afterwards — an operator told
		// "marked" will not look for it again.
		marker := "would mark"
		if apply {
			marker = "not marked"
		}
		switch {
		case m.Marked:
			marker = "marked"
		case m.MarkFailed:
			marker = "FAILED"
		case m.AlreadyResolved:
			// The issue's no-op, and it is not a success: nothing was written on
			// this row's account, including no history row, because a history row
			// records a write and no write happened.
			marker = "already resolved"
		case m.Pinned:
			marker = "pinned"
		case m.ExemptCategory:
			marker = "standing category"
		case m.Declined:
			marker = "not marked (no longer eligible: pinned, recategorized, or moved since this call read it)"
		}
		fmt.Fprintf(&b, "  %s  %s  [%s]  %s\n", marker, shortID(m.ID), m.Category, displayStored(m.Content, m.Category, 70))
	}
	if !apply && count > 0 {
		b.WriteString("\nRe-run with --apply to stamp these resolved.\n")
	}
	// The inverse, and only under --apply: a dry run stamped nothing, so naming
	// a repair for it would be a command for a change that has not happened.
	//
	// It is rendered by internal/followup, the same renderer the supersede
	// repairs use, so the two halves of a repair cannot print different commands
	// for the same situation — and that renderer quotes a project name holding a
	// space or a metacharacter, which is what makes the command run at all.
	if apply {
		if stamped := markStampedIDs(res.Memories); len(stamped) > 0 {
			cmd, viaFileOnly, unnameable := followup.ResolveCommand(projectName, stamped)
			if cmd != "" {
				fmt.Fprintf(&b, "\nTo put any of these back into session injection, clear the stamp on exactly them:\n  %s\n", cmd)
			}
			if len(viaFileOnly) > 0 {
				// Named rather than dropped, for internal/followup's reason: `--only`
				// splits on commas, so an id holding one is not nameable by that flag
				// however it is quoted, and an operator told nothing would run the
				// command above, clear fewer memories than this run stamped, and
				// report a repair that did not happen.
				fmt.Fprintf(&b, "  (%d id(s) hold a comma, which --only cannot carry, so they are named here; put each on\n"+
					"   its own line in a file and use --only-file)\n", len(viaFileOnly))
				for _, id := range viaFileOnly {
					fmt.Fprintf(&b, "    %s\n", id)
				}
			}
			if len(unnameable) > 0 {
				fmt.Fprintf(&b, "  (%d id(s) hold a newline, which no --only or --only-file form can carry, so they stay\n"+
					"   resolved until the row is rewritten — delete and re-save the memory)\n", len(unnameable))
				for _, id := range unnameable {
					fmt.Fprintf(&b, "    %q\n", id)
				}
			}
		}
	}
	return b.String()
}

// markStampedIDs is the ids THIS call stamped, in the order they were reported.
// Only stamped rows: the follow-up is for memories this run buried, and a row
// that was already resolved before it ran is not something the command would
// change — naming it would send the operator to clear a stamp an earlier pass
// owns.
func markStampedIDs(rows []resolve.MarkedMemory) []string {
	out := make([]string, 0, len(rows))
	for _, m := range rows {
		if m.Marked {
			out = append(out, m.ID)
		}
	}
	return out
}

// resolveSummaryLine renders the one-line resolve result, including the
// deterministic KEEP vetoes and the UNKNOWN verdicts that remain eligible for a
// later pass.
func resolveSummaryLine(projectName string, res resolve.Result, apply bool, confirmed int, calls int) string {
	verb := "would resolve"
	count := confirmed
	if apply {
		verb = "resolved"
		count = res.Resolved
	}
	return fmt.Sprintf("%s: %d loaded, %d after prefilter, %d confirmed evidence, %d KEEP vetoed, %d KEEP cached, %d UNKNOWN, %s %d (%d classify call(s))\n",
		projectName, res.Loaded, res.Candidates, res.Confirmed+res.Superseded+res.Corrected,
		res.Vetoed, res.Skipped, res.Unknown, verb, count, calls)
}

// reassessSummaryLine renders the one-line --reassess result. The counts are
// per outcome, "still asserted" being the rows Run would re-stamp for free (a
// live supersedes edge or a correction pairing) and therefore refuses to clear.
// The verb reports what --apply would clear, or what it cleared — reKept counts
// the rows judged, Cleared the rows actually written — so a repair is never
// reported as larger than it was.
//
// A scoped run says how much of the project it looked at. "40 already resolved"
// would be a lie the moment --only narrowed the pool: the operator needs to see
// that 3 of 143 rows were judged, because the 140 they did not judge are the
// ones the flag exists to leave resolved (#698).
func reassessSummaryLine(projectName string, res resolve.ReassessResult, apply bool, reKept int, calls int) string {
	verb := "would clear resolved_at for"
	count := reKept
	if apply {
		verb = "cleared resolved_at for"
		count = res.Cleared
	}
	scoped := ""
	if res.Pool > res.Loaded {
		scoped = fmt.Sprintf("%d of %d already resolved judged", res.Loaded, res.Pool)
	} else {
		scoped = fmt.Sprintf("%d already resolved", res.Loaded)
	}
	return fmt.Sprintf("%s: %s, %d KEEP vetoed, %d KEEP cached, %d still RESOLVED, %d still asserted by a link or correction, %d UNKNOWN, %s %d (%d classify call(s))\n",
		projectName, scoped, res.Vetoed, res.Cached, res.StillResolved, res.Demoted, res.Unknown, verb, count, calls)
}

// reassessMissLines renders one line per selector that named no memory the
// repair could judge, so a mistyped id is visible in the run that ignored it
// rather than discovered later as a resolution that was never repaired. Empty
// for an unscoped pass, and for a scoped run in which every selector matched.
func reassessMissLines(res resolve.ReassessResult) string {
	if len(res.Misses) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range res.Misses {
		fmt.Fprintf(&b, "  %s  not judged: %s\n", m.Spec, m.Reason)
	}
	return b.String()
}

// reassessHeldLines renders one line per already-resolved row the repair leaves
// resolved, naming what holds it. The summary counts those rows; this is what an
// operator acts on, and the two reasons a row stays resolved need different
// remedies — a live 'supersedes' edge is undone by `ghost supersede --withdraw`,
// a correction pairing is undone by nothing and can only be READ, which is why
// the report has to name the correction rather than say "a correction".
//
// The ids print WHOLE, unlike every other id on a Ghost report, and unlike
// shortID beside them on this one. These are operands rather than references:
// the supersedes source is what --withdraw takes, the held row's own id is what a
// scoped repair names, and the correction's id is a note somebody has to go and
// read. An eight-character abbreviation of any of them turns a repair that was
// already in hand into a second lookup.
//
// The row's own text is rendered through displayStored, like every other stored
// line here, so a held row holding a credential has it withheld whole: the
// report is text an operator pastes into a shell, and the write-boundary guard
// is not retroactive.
func reassessHeldLines(held []resolve.HeldMemory) string {
	var b strings.Builder
	for _, h := range held {
		reason := h.Reason()
		if reason == "" {
			// Unreachable from Reassess, which only lists rows it has a hold
			// for. Defaulted rather than skipped, so a future caller that
			// reaches it prints the row and an operator can see it, instead of a
			// count with a silently shorter list under it.
			reason = "held by an unnamed assertion"
		}
		fmt.Fprintf(&b, "  %s  [%s]  %s  %s\n", h.Memory.ID, h.Memory.Category, reason,
			displayStored(h.Memory.Content, h.Memory.Category, 70))
	}
	return b.String()
}

// reassessRoundsLine reports the hold-back re-check's own cost, and only when it
// cost something: a run whose first re-check settled everything has no fixed
// point to announce, and a line an operator learns to skip is a line that stops
// being read when it matters. A run that iterated says so, because a repair
// settled over three rounds is not the answer a single re-check would have given
// and an operator reading the list is entitled to know that.
//
// A bound reached with a row still changing is a different sentence, and not a
// variant of the first: the repair is short of its fixed point, so a further pass
// may clear more, and saying only "N rounds" would read as a completed re-check.
func reassessRoundsLine(res resolve.ReassessResult) string {
	switch {
	case res.BoundHit:
		return fmt.Sprintf("  hold-back re-checked %d round(s) and stopped at its bound: a further pass may clear more\n", res.Rounds)
	case res.Rounds > 1:
		return fmt.Sprintf("  hold-back re-checked %d round(s) to reach its fixed point\n", res.Rounds)
	}
	return ""
}

// runResolve is the CLI entry for `ghost resolve`. It marks resolved-evidence
// memories (concluded work: findings, changelog notes, PR locators) so they
// drop out of session-start injection while staying searchable. Cheap local
// keyword prefilter proposes candidates; a deterministic KEEP veto settles the
// ones that state a standing rule or an open problem for free; the hosting CLI
// harness adjudicates the rest in batches with a fresh-session question biased
// to KEEP, and a RESOLVED must name what closed the note. Explicit KEEP
// verdicts are cached by content hash so a converged project makes no calls;
// UNKNOWN replies remain eligible for a later pass. Dry-run by default; --apply
// writes resolved_at and the cache.
// Re-runnable and reversible: any later Upsert/UpdateMemory of a memory clears
// its resolved_at, and `ghost resolve --reassess` re-judges the rows this pass
// already stamped. The stop hook spawns `ghost lifecycle` detached
// (internal/mcpinit/stophook.go); its resolve phase runs this with --apply.
func runResolve() {
	parsed, parseErr := parseResolveArgs(os.Args[2:])
	if parseErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", parseErr)
		os.Exit(1)
	}
	projectName, apply, reassess := parsed.project, parsed.apply, parsed.reassess
	if projectName == "" {
		fmt.Fprint(os.Stderr, resolveUsage)
		os.Exit(1)
	}

	// --mark is dispatched BEFORE the harness is detected or spawned, and that
	// ordering is the feature rather than an optimisation: it judges nothing, so
	// a machine that cannot spawn a CLI harness at all can still repair its
	// corpus, nothing is billed, and a --source the caller passed has nothing to
	// override. Every other mode reaches this point needing a classifier, so it
	// pays for one.
	if parsed.hasMark() {
		runResolveMark(parsed, apply)
		return
	}

	source, err := detectPhaseSource(parsed.source)
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

	if reassess {
		// The scope is read here rather than in the parser because it is file
		// IO: parseResolveArgs stays a pure function of argv, and a missing or
		// unreadable file is a usage error an operator can act on. The union is
		// built into a fresh slice so the file's lines cannot land in the parsed
		// args' backing array.
		only := append([]string{}, parsed.only...)
		if parsed.onlyFile != "" {
			fromFile, ferr := readRefSelectors("--only-file", parsed.onlyFile)
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", ferr)
				os.Exit(1)
			}
			only = append(only, fromFile...)
		}
		res, reKept, err := resolve.Reassess(ctx, store, cls, projectID, apply, resolve.Scope{Only: only}, logger)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(reassessSummaryLine(projectName, res, apply, len(reKept), cls.Calls()))
		fmt.Print(reassessRoundsLine(res))
		fmt.Print(reassessMissLines(res))
		fmt.Print(reassessHeldLines(res.Held))
		fmt.Print(memoryLines(reKept))
		if !apply && len(reKept) > 0 {
			fmt.Println("\nRe-run with --apply to return these to session injection.")
		}
		return
	}

	res, confirmed, err := resolve.Run(ctx, store, cls, projectID, apply, logger)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Print(resolveSummaryLine(projectName, res, apply, len(confirmed), cls.Calls()))
	if res.Superseded > 0 || res.Corrected > 0 {
		fmt.Printf("  (%d via supersedes links, %d via correction pairing, %d via LLM)\n",
			res.Superseded, res.Corrected, res.Confirmed)
	}
	fmt.Print(memoryLines(confirmed))
	if !apply && res.Confirmed+res.Superseded+res.Corrected > 0 {
		fmt.Println("\nRe-run with --apply to mark these resolved.")
	}
}

// memoryLines renders the per-memory listing the two resolve listings share: one
// line per memory, its short id, its category, and its own first line at the
// listing's 70 characters — withheld whole if the row holds a credential, since
// the input is the stored corpus and the write-boundary guard is not retroactive.
//
// It is a function because both call sites were inline loops in runResolve, and
// runResolve cannot be called from a test: it opens a store, builds a harness
// provider and calls os.Exit. The property these lines are responsible for — what
// a caller is shown about each memory it decided on — was therefore untestable, and
// the two commands print the same line for the same reason, so one renderer is the
// whole of both sites rather than a place for them to drift apart.
func memoryLines(memories []memory.Memory) string {
	var b strings.Builder
	for _, m := range memories {
		fmt.Fprintf(&b, "  %s  [%s]  %s\n", shortID(m.ID), m.Category, displayStored(m.Content, m.Category, 70))
	}
	return b.String()
}
