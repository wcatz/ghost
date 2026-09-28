package ai

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

type CodexClient struct {
	binary string
}

func NewCodexClient() *CodexClient {
	return NewCodexClientWithBinary("codex")
}

func NewCodexClientWithBinary(binary string) *CodexClient {
	return &CodexClient{binary: binary}
}

func (c *CodexClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

func (c *CodexClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *CodexClient) run(ctx context.Context, prompt string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultTimeout)
		defer cancel()
	}
	support := codexFeaturesFor(ctx, c.binary)
	args := codexInvocationArgs(support.declared)
	// The warning is skipped for a caller that stopped waiting. It learned
	// nothing about this codex, so the placeholder it was handed is not a probe
	// result and must not be reported as one: `probed == false` is indistinguishable
	// from a real "did not answer" here, and reporting it would fire the
	// unverified WARN and CONSUME its latch. The latch exists so a later genuine
	// unverified verdict is not silenced, so one cancelled turn in a long-lived
	// server would swallow the real diagnostic for the rest of the process — and
	// the turn that swallows it is the one that lost the most information.
	//
	// Nothing is lost by skipping: the turn fails on its own context below, and
	// any real verdict is still reported by the next call, because the verdict is
	// cached per identity and only a real one may spend the latch.
	if ctx.Err() == nil {
		warnOnWeakerCodexPolicy(support)
	}
	cmd, release, _ := harnessCommand(ctx, c.binary, args, os.Environ(), harnessCodex)
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("codex exec: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// codexInvocationArgs is the whole argv of a `codex exec` turn, kept as a named
// value so the no-tool policy can be pinned as a golden rather than grepped for.
//
// The no-tools keys are codexNoToolFeatureKeys, FILTERED against what the
// installed codex declares. The probe is codexFeaturesFor; the two reasons it
// exists are load-bearing and point opposite ways.
//
// codex SILENTLY IGNORES a `-c` key it does not know — the fail-OPEN direction.
// `-c` overrides are collected as raw strings and applied onto the config tree
// (utils/cli/src/config_override.rs: apply_toml_override just inserts the
// segment), and `FeaturesToml` deserialises without `deny_unknown_fields`, so a
// key renamed or removed upstream is dropped without complaint: the tool comes
// back on, the call still succeeds, and nothing in a lifecycle log says so. The
// strict reading is opt-in (`--strict-config`, default off) and Ghost does not
// pass it, so TestLiveCodexDeclaresTheNoToolFeatureKeys provokes an unknown key
// and reports which way the installed codex actually goes. Passing a key to a
// codex that does not have the FEATURE is harmless, so the filter is about not
// claiming a policy the binary cannot honour — not about avoiding a silent
// no-op that costs nothing.
//
// The filter warns and NEVER refuses. A codex missing a key is codex without the
// surface that key governs, and codex is selected with no version floor anywhere
// — CLIProvider picks it by exec.LookPath with no lower bound, and a session-
// routed client names it as its backend with nothing checked at all — so
// refusing would fail that session's reflect, resolve and supersede calls on a
// working install over a surface that may not exist. The failure being prevented
// is a tool that is already absent; the failure a refusal creates is a lifecycle
// that does not run at all. The weaker policy is reported once per process (a
// lifecycle run makes hundreds of calls on the same binary, and a warning per
// call would bury the phase output it exists to interrupt) and recorded nowhere
// else, which is the honest limit of what a diagnostic can do.
//
// The probe runs ONCE per binary identity, cached in codexFeatureCache and keyed
// by path/size/mtime exactly as claudeCapabilitiesFor keys its own probe: a
// long-lived Ghost process (the MCP server) must re-probe a codex upgraded in
// place rather than reuse a stale answer, and a per-call probe would double the
// process count of a lifecycle that spawns one harness per consolidation, per
// resolve candidate and per supersede pair.
//
// A probe that gets NO ANSWER is cached too, for codexFeatureRetry rather than
// forever. The negative verdict is the common case on exactly the old codex this
// design supports, so leaving it uncached would cost that install an extra
// process on every call; caching it for the life of the process is the opposite
// failure, because a codex mid-upgrade would be remembered as "declares nothing"
// until the parent exited. The interval is far longer than a call and far shorter
// than a process.
//
// codex has no "no tools" flag — `--sandbox read-only` bounds what a tool may
// DO, not which tools EXIST — so the policy is the list. The three that matter
// most:
//
//   - Both exec tools. codex registers its shell surface in one place gated on
//     features.shell_tool, and inside that it picks the unified PTY-backed exec
//     tool (features.unified_exec) or the one-shot exec, so disabling only
//     features.shell_tool still leaves the other available.
//   - view_image, a plain local file read, which the sandbox's write policy
//     does not cover at all.
//   - web_search, which is the top-level `web_search` setting rather than a
//     feature key, and "disabled" is its documented off value.
//
// The plugin and connector surfaces go as a group because each contributes a
// tool of its own: plugins (local plugin skills and tools), tool_suggest (offers
// to install one mid-turn) and apps (connectors), with
// skill_mcp_dependency_install closing the last path in — installing an MCP
// server on demand runs a command. agents.enabled and features.multi_agent are
// both spelled out though one subsumes the other in current codex: they are
// separate config keys, and only spelling both means a codex that drops one does
// not silently re-enable sub-agent spawning.
//
// RESIDUAL, named rather than papered over: codex has NO flag or config key that
// disables apply_patch. Its registry entry is gated on the model catalog
// advertising an apply_patch tool type, not on an invocation-settable key, and
// the apply_patch_freeform feature is marked Removed. The read-only sandbox is
// therefore the boundary for it, not a missing flag: a model that tries to write
// is refused by the sandbox rather than never offered the tool. The prompt is
// memory text, so the exposure is a refused write, not a written file. For the
// same reason no config key reaches the skills extension's own read/list tools;
// they are off unless a skill exists, and skills are discovered from the
// invocation's neutral scratch working directory, which holds none.
//
// `-` is codex's documented sentinel for "the prompt is on stdin" (its default
// when the prompt argument is omitted, stated explicitly here so the intent
// survives a flag default change). The prompt is never an argv element (issue
// #560): the kernel caps one argument at 32 pages and a reflect prompt is built
// from up to 2000 memories of 8000 bytes, so a large project produced a prompt
// that failed the spawn with E2BIG.
func codexInvocationArgs(declared map[string]bool) []string {
	// The flags below are unconditional, and two of them are not feature keys at
	// all: agents.enabled is an `agents` table key and web_search is the
	// top-level setting, so `codex features list` says nothing about either and
	// filtering them by that list would drop a restriction on every codex.
	args := []string{
		"exec",
		"--sandbox", "read-only",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"--ephemeral",
	}
	for _, key := range codexNoToolFeatureKeys {
		// A nil map means the probe did not answer (an unresolvable binary, a
		// codex without `features list`, a timeout). Passing every key is then
		// the same behaviour as before the probe existed, and codex ignores the
		// ones it does not know — so the fallback is today's policy rather than
		// a weaker one.
		if declared != nil && !declared[key] {
			continue
		}
		args = append(args, "-c", "features."+key+"=false")
	}
	return append(args, "-c", "agents.enabled=false", "-c", `web_search="disabled"`, "-")
}

// codexNoToolFeatureKeys is the no-tools policy, as data rather than as literal
// argv, because it now has to be FILTERED against what the installed codex
// declares. Order is the order they appear in the argv, so a reviewer can diff
// this list against a golden without reformatting anything.
var codexNoToolFeatureKeys = []string{
	"shell_tool",
	"unified_exec",
	"view_image",
	"apps",
	"plugins",
	"tool_suggest",
	"skill_mcp_dependency_install",
	"remote_plugin",
	"hooks",
	"multi_agent",
}

// codexFeatureSupport is what the probe learned about one codex binary: the keys
// it declares, and whether the probe actually answered.
//
// `probed` is separate from an empty `declared` because the two cases are
// different. An empty set means codex ran `features list` and declares none of
// the keys — a policy that is genuinely much weaker, and worth warning about. A
// nil set means the probe never got an answer, which is not evidence about the
// codex at all, so nothing is filtered and nothing is claimed.
type codexFeatureSupport struct {
	declared map[string]bool
	probed   bool
	// at is when the probe ran, for the ONE thing the cache is not keyed on: a
	// negative verdict is re-tried after codexFeatureRetry so a codex that stops
	// answering (an in-place upgrade, a PATH that briefly went missing) recovers
	// without a restart.
	at time.Time
}

// codexFeatureRetry is how long an unanswerable probe is trusted before the next
// call re-asks.
//
// The negative verdict has to be cached at all: a codex whose `features list`
// does not answer is precisely the OLD codex this design exists to support, so
// not caching it would give that install an extra process on every one of the
// hundreds of calls a lifecycle makes — the doubling the cache exists to avoid,
// for the whole run rather than for a window. But caching it for the life of the
// process is the other failure: a codex upgraded in place, where the parent
// Ghost process is long-lived, would keep passing keys the new binary does not
// know and never notice. A short re-ask interval is the compromise, and it is
// deliberately much longer than a call (a lifecycle's calls are seconds apart)
// and much shorter than a process (the MCP server outlives many upgrades).
const codexFeatureRetry = 5 * time.Minute

// codexBinaryID identifies a codex install by identity rather than by name, so
// an in-place upgrade is re-probed instead of reusing a stale answer, and a
// long-lived Ghost process (the MCP server) notices.
type codexBinaryID struct {
	path    string
	size    int64
	modTime time.Time
}

// codexFeatureCache holds one probe result per codex binary identity. A
// lifecycle run spawns hundreds of harness processes, and Ghost spawns one
// process per consolidation, resolve candidate and supersede pair, so probing
// per call would double the process count. Keyed by identity for the same
// reason claudeCapabilitiesFor is: a cached answer about a binary that has since
// been replaced is a wrong answer.
// probeKey is this identity as the string codexProbeGroup keys on, and it is
// built from exactly the three fields the cache key compares — no more, so two
// identities can never share a flight, and no fewer, so the same binary always
// lands on the same one. NUL joins them because a path cannot contain one, so
// no combination of values can spell another identity's key.
func (id codexBinaryID) probeKey() string {
	return id.path + "\x00" + strconv.FormatInt(id.size, 10) + "\x00" + strconv.FormatInt(id.modTime.UnixNano(), 10)
}

var codexFeatureCache sync.Map // codexBinaryID -> codexFeatureSupport

// codexProbeGroup collapses a cold burst into ONE `features list` child, keyed
// on the binary identity, so N concurrent first-callers spawn 1 process and all
// share its answer. Without it the cache is check-then-probe and every caller
// that arrives before the first one stores runs its own (#741).
//
// The group is here for the SHAPE claudeCapabilitiesFor needs it for, and this
// probe would survive without it: a per-key mutex map would do, because the
// flight caches a verdict the probe could NOT reach, so the leader leaves a
// FRESH negative and each follower finds it in codexCachedSupport and returns
// without spawning. (A verdict reached under a DEAD context is the one the
// flight refuses to cache, which is a cancellation rather than an answer and
// does not change this.) The difference that makes singleflight necessary for
// claude — a failure not being cached — is the opposite of this probe's design,
// which caches precisely so an old codex costs one process for the whole run.
// It is the same mechanism in both, chosen once, rather than a claim that this
// probe needs a guarantee it does not.
//
// What it does NOT do is decide what is RETAINED or when that changes. A flight
// is forgotten the moment it lands, so codexCachedSupport below still owns both
// halves of the policy this package documents: a positive is kept for the life
// of the process, and a negative is kept only until codexFeatureRetry.
//
// What it does cost is the caller's own context, which singleflight never
// consults. Here the cost is not a refusal — this probe never errors — but a
// leader whose context died would hand every follower the NEGATIVE that its
// killed probe produced, and that negative is a statement about one cancelled
// turn rather than about the codex. Both halves are handled at the call site
// below: the dead leader's flight stores nothing, and a live caller goes round
// again under its own context. The shared half is isSharedProbeCancellation in
// probe.go.
var codexProbeGroup singleflight.Group

// codexCachedSupport returns the verdict cached for this identity when it is
// still one Ghost may hand out, and reports false when it is not — either
// nothing is cached, or what is has EXPIRED. The expiry is the second case and
// must stay inside this function, because it is read from two places now (the
// caller below and the flight) and a check-then-probe pair with the freshness
// test in only one of them is how a long-lived parent stops noticing a codex
// upgraded in place.
func codexCachedSupport(id codexBinaryID) (codexFeatureSupport, bool) {
	cached, ok := codexFeatureCache.Load(id)
	if !ok {
		return codexFeatureSupport{}, false
	}
	support := cached.(codexFeatureSupport)
	if support.probed || time.Since(support.at) < codexFeatureRetry {
		return support, true
	}
	return codexFeatureSupport{}, false
}

// codexFeaturesFor is the whole of the no-tools policy's runtime half, and it
// has three properties that are each load-bearing.
//
// NEVER ERRORS. A diagnostic that can fail a harness call is worse than the
// weaker policy it measures, so every failure path yields the zero value — a nil
// key set, meaning "pass everything" — and is reported by
// warnOnWeakerCodexPolicy. There is no code path from here to a refused
// invocation, on purpose: see warnOnWeakerCodexPolicy.
//
// CACHED PER BINARY IDENTITY, never per call. A lifecycle run spawns hundreds of
// harness processes on one codex — one per consolidation, per resolve candidate
// and per supersede pair — so a per-call probe would double the process count of
// the whole run. Keyed by path/size/mtime exactly as claudeCapabilitiesFor keys
// its own probe, so a long-lived Ghost process (the MCP server) re-probes a codex
// upgraded in place rather than reusing a stale answer.
//
// THE NEGATIVE VERDICT IS CACHED TOO, for codexFeatureRetry, and that is the
// subtlety. A codex whose `features list` does not answer is precisely the OLD
// codex this design supports, so leaving it uncached would cost that install an
// extra process on every one of the hundreds of calls a lifecycle makes — the
// doubling the cache exists to avoid, for the whole run. Caching it for the life
// of the process is the opposite failure: a codex upgraded in place would keep
// getting keys the new binary does not know, and the parent MCP server would
// never notice. So a negative is stored like any other answer and re-asked after
// the interval, which is far longer than a call and far shorter than a process.
//
// codexFeaturesFor probes the installed codex once per binary identity for the
// feature keys it declares, using the same `codex features list` a person would
// run, and caches the answer.
//
// It never returns an error. A probe that cannot answer yields a nil key set,
// which codexInvocationArgs treats as "pass everything" — the behaviour before
// this probe existed — and warnOnWeakerCodexPolicy reports once. A harness call
// failing because a diagnostic could not run would be a worse outcome than the
// weaker policy the diagnostic is measuring.
func codexFeaturesFor(ctx context.Context, binary string) codexFeatureSupport {
	path, err := exec.LookPath(binary)
	if err != nil {
		return codexFeatureSupport{}
	}
	info, err := os.Stat(path)
	if err != nil {
		return codexFeatureSupport{}
	}
	id := codexBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}
	if support, ok := codexCachedSupport(id); ok {
		return support
	}
	// Cold, or a negative verdict older than codexFeatureRetry. Either way ONE
	// probe answers for the whole burst, and the re-read inside it catches the
	// flight that landed between the lookup above and this call — including a
	// positive one, which lands for the life of the process and is therefore
	// worth the second check rather than a wasted spawn.
	//
	// The loop covers the one case a single round cannot: the leader's context
	// died mid-probe, so the flight's verdict is that leader's accident rather
	// than this caller's answer, and this caller goes round again under its own
	// context. Re-entering the GROUP rather than probing directly is the point —
	// a direct re-probe would put this burst back to one child per caller, the
	// bug #741 removes, on the rare path out of a common one. Nothing was cached
	// by the round that died (the flight stores only while its caller's context
	// is alive), so the next round's re-read misses and really does re-probe
	// rather than serve the dead leader's negative.
	//
	// Each round consumes one more cancellation, so the rounds are bounded by
	// the callers sharing this key, and a caller whose OWN context is dead
	// leaves without looping: isSharedProbeCancellation requires a live ctx, and
	// both arms of the select below check it. It is never worse than the
	// behaviour before #741, which was one probe per caller, however many rounds
	// that took.
	for {
		flight := codexProbeGroup.DoChan(id.probeKey(), func() (any, error) {
			if support, ok := codexCachedSupport(id); ok {
				return support, nil
			}
			support := probeCodexFeatures(ctx, path, id)
			if ctx.Err() != nil {
				// This is OUR context dying, and the verdict above says nothing
				// about the codex — see probeContextError. Marking it is what
				// stops a dead leader from answering on behalf of the live
				// callers sharing its flight, who are entitled to a probe of
				// their own rather than to one turn of the pre-probe policy.
				// Marked even if the probe somehow answered, because a verdict
				// from a context that was already cancelled is not one to hand a
				// caller that is still running.
				//
				// The store lives HERE rather than in probeCodexFeatures, and this
				// single check is why: it is the one place that knows both facts,
				// so "was the caller alive when this probe was killed" and "is
				// this verdict worth keeping" cannot be answered a moment apart by
				// two pieces of code. A probe killed by this context ALWAYS returns
				// after the cancellation was recorded — that is what killed it — so
				// the check below cannot miss it, and a probe that failed on its
				// own terms or hit its 10s cap leaves this context alive and is
				// cached, which is the policy codexFeatureRetry exists for.
				return support, probeContextError{ctx.Err()}
			}
			codexFeatureCache.Store(id, support)
			return support, nil
		})
		// A caller's own context governs its own call, and singleflight hands
		// back the leader's outcome without consulting it. Neither failure is a
		// refusal here: this probe cannot return one, so the worst outcome
		// available is the weaker policy — which is what this caller would have
		// got had it probed for itself, and never something worse.
		select {
		case <-ctx.Done():
			// This caller's context is dead, so it gets the zero verdict — "pass
			// everything", the behaviour before this probe existed — rather than a
			// wait for a verdict it cannot act on. Nothing is stored for it: it
			// stores nothing itself, and a flight it led stored nothing either,
			// because the store above is behind the same check as the marker.
			return codexFeatureSupport{}
		case res := <-flight:
			// BOTH arms have to agree, because this one cannot be assumed: when a
			// caller's context is already done AND the flight channel already
			// holds a result, both cases are ready and Go picks at random, so a
			// dead caller leaves by this arm about half the time. Checking the
			// caller's own context first is what makes the choice between the two
			// arms immaterial, instead of leaving the answer to a coin.
			if ctx.Err() != nil {
				return codexFeatureSupport{}
			}
			// The marker's error is the one error a live caller can answer for
			// itself, so it is tested BEFORE the blanket handling below.
			if isSharedProbeCancellation(res.Err, ctx) {
				// The leader's context died; this caller's did not. Round again
				// rather than accepting a verdict this caller cannot distinguish
				// from one its own probe would have produced.
				continue
			}
			// The flight's error is nil except for the marker handled above, and
			// this function has no error to return — the zero value is what a
			// probe that could not answer means here, so an error is handled
			// rather than asserted past into a nil-interface panic.
			if res.Err != nil {
				return codexFeatureSupport{}
			}
			return res.Val.(codexFeatureSupport)
		}
	}
}

// probeCodexFeatures is the body of the single flight: the one child.
//
// It runs under the FIRST caller's context, so a burst takes the first caller's
// deadline for its shared probe rather than a deadline of its own. That is the
// accepted cost of one probe rather than N, and it is bounded twice over: the
// probe is a diagnostic capped at 10s, and the flight stores its verdict only
// while that caller's context is alive, so it never hands a cancellation nobody
// else had to anyone who comes after it.
//
// It stores nothing. That belongs to the flight, which is the only place that
// knows whether the answer is worth keeping — see the store there.
func probeCodexFeatures(ctx context.Context, path string, id codexBinaryID) codexFeatureSupport {
	// Bounded like the claude capability probe: an unanswering probe must not
	// spend the caller's budget, which the caller needs for the model call.
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, _ := harnessCommand(probeCtx, path, []string{"features", "list"}, os.Environ(), harnessCodex)
	defer release()
	// `probed` means THE PROBE ANSWERED, not merely that it ran. That distinction
	// is the whole reason the field exists, and the two "no" cases must not
	// collapse into one:
	//
	//   - probe failed, or printed something that is not a feature table: we
	//     know NOTHING about this codex. `declared` stays nil, nothing is
	//     filtered, and the warning says "unverified" rather than accusing the
	//     codex of lacking features it was never asked about.
	//   - a table with keys we recognise but none of ours: codex told us it has
	//     none of them, which IS an answer. `declared` is an empty set, the
	//     policy is stripped to nothing, and the warning says so plainly.
	support := codexFeatureSupport{at: time.Now()}
	if out, err := probe.Output(); err == nil {
		// Output that is not a feature table must NOT be read as an
		// authoritative answer, because a positive verdict is cached for the life
		// of the process: a header row, a footer count, a progress line, or a
		// single stray token would strip all ten overrides and the only signal
		// would be a WARN. isCodexFeatureTable is the floor, and it is deliberately
		// a test of SHAPE rather than an overlap with the policy's own key list —
		// a real codex older than this policy answers with a full table naming
		// features Ghost has never heard of and none of the ten it disables, and
		// that is a genuine weaker policy rather than an unreadable answer.
		text := string(out)
		if !isCodexFeatureTable(text) {
			support.probed = false
		} else {
			support.probed = true
			support.declared = parseCodexFeaturesList(text)
		}
	}
	// Cached by the flight, not here: probeCodexFeatures is the PROBE, and the
	// decision to keep its answer belongs with the decision to mark it, in one
	// place that knows both. Storing unconditionally would hand every caller
	// "this codex declares nothing" for codexFeatureRetry over one client that
	// disconnected; a follower cannot repair that afterwards, because a flight is
	// over by the time anyone hears about it.
	return support
}

// codexWeakPolicyWarned makes the weaker-policy warning fire ONCE PER PROCESS
// PER VERDICT. It has to be once: a lifecycle run makes hundreds of harness calls
// on the same binary, and a warning per call would bury the phase output it is
// meant to interrupt.
//
// There are TWO latches, not one, and sharing a single sync.Once would be a bug
// rather than a shortcut. The two verdicts are not interchangeable, and the
// suppressed one is the more informative. The reachable order is a long-lived
// MCP server whose first codex call probes while the binary is mid-upgrade and
// gets no answer (the unverified warning consumes a shared latch), after which
// the codexFeatureRetry re-ask returns a real answer naming the missing keys —
// and the operator is never told which surfaces are on. So each verdict is latched
// separately and both can be reported, in either order.
var (
	codexUnverifiedWarned sync.Once
	codexWeakerWarned     sync.Once
)

// warnOnWeakerCodexPolicy reports, once per process, that this codex does not
// support every key the no-tools policy asks for — and that Ghost will run
// anyway.
//
// The two cases are worded differently because they mean different things. A
// missing key is codex not having the feature at all, so the surface it governs
// is not there to restrict. A failed probe is us not knowing, and passing every
// key is the pre-probe behaviour. Neither refuses: refusing would fail every
// reflect, resolve and supersede call on a working install over a surface that
// may not exist, which is a worse failure than the one this policy prevents.
func warnOnWeakerCodexPolicy(support codexFeatureSupport) {
	if !support.probed {
		codexUnverifiedWarned.Do(func() {
			slog.Warn("codex feature probe did not answer; passing the whole no-tools key list, which is the behaviour before the probe existed. This is not evidence that any tool is on: the codex may be older than this policy, or `codex features list` may be unavailable.",
				"policy", "unverified")
		})
		return
	}
	missing := missingCodexFeatureKeys(support.declared)
	if len(missing) == 0 {
		return
	}
	codexWeakerWarned.Do(func() {
		slog.Warn("installed codex does not declare every no-tools feature key, so the no-tools policy is WEAKER than on a current codex. Running anyway: a missing key means this codex does not have the surface it governs, and refusing would fail every reflect, resolve and supersede call over a surface that may not exist.",
			"missing", strings.Join(missing, ","))
	})
}

// parseCodexFeaturesList reads `codex features list` output. codex prints one
// row per feature as "<key>  <stage>  <enabled>" in a fixed-width layout, so the
// first field of each line is the key and the rest is prose about it.
//
// It is deliberately permissive — a line with a single token still yields a key —
// because the VALIDITY question is answered separately, by isCodexFeatureTable.
// Splitting it that way keeps "what did codex say" apart from "did codex say
// anything usable", and only the second one is allowed to change the policy.
func parseCodexFeaturesList(out string) map[string]bool {
	keys := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		// The first field of EVERY non-blank line, not just of well-formed rows.
		// That permissiveness is the point: the validity question belongs to
		// isCodexFeatureTable, so that "what did codex say" and "did codex say
		// anything usable" stay separate and only the second can change the
		// policy. TestCodexParseTakesEveryNonBlankLine pins it, because an
		// independent review restricted this to well-formed rows and the whole
		// suite stayed green — and the split it broke is what keeps a
		// mis-parsed table from silently disabling the policy.
		keys[fields[0]] = true
	}
	return keys
}

// codexFeatureStages is codex's own `Stage` vocabulary — the second column of
// `codex features list`, printed by `stage_str` as a fixed set of these words.
// It is the discriminator, and it is a real one rather than a proxy: a row of the
// table is "<key>  <stage>  <enabled>", so a line whose second field is not one
// of these words is not a row. A header, a footer, a progress line and a stray
// message all fail it, and so does the single-token output an unrelated fake
// emits.
var codexFeatureStages = map[string]bool{
	"stable":              true,
	"experimental":        true,
	"under development":   true,
	"deprecated":          true,
	"removed":             true,
	"disabled by default": true,
}

// isCodexFeatureTable reports whether `codex features list` output can be
// trusted as codex's own answer about which features it has.
//
// The shape test is the ROW test, not an overlap test with the policy's own key
// list, and the difference matters. A real codex older than this policy answers
// with a full table naming features Ghost has never heard of and none of the ten
// it wants to disable — that is a genuine, reportable "this codex does not have
// those surfaces", and it must NOT be filed under "we could not read the output",
// because those two need different warnings and only one of them is a weaker
// policy. An overlap test would collapse them and lose the more informative
// message on exactly the install class this design exists to support.
//
// So: one line must parse as "<key> <known stage> <bool>", and that is the whole
// requirement. A single well-formed row is enough to establish the table is real
// — requiring a majority would reject a short table from a codex that declares
// few features, and requiring overlap would reject a genuinely older one.
func isCodexFeatureTable(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if codexFeatureRow(line) {
			return true
		}
	}
	return false
}

// codexFeatureRow reports whether one line is a well-formed feature row. The
// stage is checked by VALUE because codex prints it from a closed set, and the
// enabled column by SHAPE because it is whatever Rust's Debug prints for a bool,
// which is one of two words either way.
func codexFeatureRow(line string) bool {
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return false
	}
	if !codexFeatureStages[fields[1]] {
		return false
	}
	return fields[2] == "true" || fields[2] == "false"
}

// missingCodexFeatureKeys names the policy keys this codex does not declare, in
// policy order so the message is stable between runs.
func missingCodexFeatureKeys(declared map[string]bool) []string {
	var missing []string
	for _, key := range codexNoToolFeatureKeys {
		if !declared[key] {
			missing = append(missing, key)
		}
	}
	return missing
}
