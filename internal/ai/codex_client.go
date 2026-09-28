package ai

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
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
	warnOnWeakerCodexPolicy(support)
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
// surface that key governs, and Ghost picks codex by exec.LookPath with no
// version floor anywhere, so refusing would fail every reflect, resolve and
// supersede call on a working install over a surface that may not exist — a
// worse failure than the one this policy prevents, and one with a much larger
// blast radius. The weaker policy is reported once per process (a lifecycle run
// makes hundreds of calls on the same binary, and a warning per call would bury
// the phase output it exists to interrupt) and recorded nowhere else, which is
// the honest limit of what a diagnostic can do.
//
// The probe runs ONCE per binary identity, cached in codexFeatureCache and keyed
// by path/size/mtime exactly as claudeCapabilitiesFor keys its own probe: a
// long-lived Ghost process (the MCP server) must re-probe a codex upgraded in
// place rather than reuse a stale answer, and a per-call probe would double the
// process count of a lifecycle that spawns one harness per consolidation, per
// resolve candidate and per supersede pair. A probe that gets no answer is NOT
// cached, so a codex mid-upgrade is re-asked rather than remembered as
// declaring nothing.
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
var codexFeatureCache sync.Map // codexBinaryID -> codexFeatureSupport

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
	if cached, ok := codexFeatureCache.Load(id); ok {
		if support := cached.(codexFeatureSupport); support.probed || time.Since(support.at) < codexFeatureRetry {
			return support
		}
		// A negative verdict older than the interval. Re-probe rather than keep
		// it: this is where a codex upgraded in place, or one whose PATH entry
		// went missing and came back, is noticed by a long-lived parent.
	}

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
		declared := parseCodexFeaturesList(string(out))
		if len(declared) == 0 {
			// Output that parses to no key at all is not a feature table, so it is
			// treated as no answer: an empty set would strip the whole policy on a
			// codex that merely printed something unexpected.
			support.probed = false
		} else {
			// A key set is an answer, even when it holds none of ours — that is
			// the case where the policy really is unavailable and the warning
			// says so.
			support.probed = true
			support.declared = declared
		}
	}
	// Cached whether it answered or not: an unanswering codex is the old-codex
	// case this whole design supports, and re-probing it on every call would
	// double a lifecycle's process count for the entire run. Only the
	// unanswered case expires (see codexFeatureRetry).
	codexFeatureCache.Store(id, support)
	return support
}

// codexWeakPolicyWarned makes the weaker-policy warning fire ONCE per process.
// It has to be once: a lifecycle run makes hundreds of harness calls on the same
// binary, and a warning per call would bury the phase output it is meant to
// interrupt. The once is per process rather than per key so that a codex
// missing two keys and a codex missing one are reported the same number of
// times.
var codexWeakPolicyWarned sync.Once

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
		codexWeakPolicyWarned.Do(func() {
			slog.Warn("codex feature probe did not answer; passing the whole no-tools key list, which is the behaviour before the probe existed. This is not evidence that any tool is on: the codex may be older than this policy, or `codex features list` may be unavailable.",
				"policy", "unverified")
		})
		return
	}
	missing := missingCodexFeatureKeys(support.declared)
	if len(missing) == 0 {
		return
	}
	codexWeakPolicyWarned.Do(func() {
		slog.Warn("installed codex does not declare every no-tools feature key, so the no-tools policy is WEAKER than on a current codex. Running anyway: a missing key means this codex does not have the surface it governs, and refusing would fail every reflect, resolve and supersede call over a surface that may not exist.",
			"missing", strings.Join(missing, ","))
	})
}

// parseCodexFeaturesList reads `codex features list` output. codex prints one
// row per feature as "<key>  <stage>  <enabled>" in a fixed-width layout, so
// the first field of each line is the key and the rest is prose about it.
func parseCodexFeaturesList(out string) map[string]bool {
	keys := make(map[string]bool)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		keys[fields[0]] = true
	}
	return keys
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
