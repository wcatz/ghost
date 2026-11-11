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

func (c *CodexClient) run(ctx context.Context, prompt string) (_ string, err error) {
	ctx, cancel := boundHarnessCall(ctx)
	defer cancel()
	defer func() { err = explainDefaultBound(ctx, err) }()
	support := codexFeaturesFor(ctx, c.binary)
	args := codexInvocationArgs(support.declared)

	if ctx.Err() == nil {
		warnOnWeakerCodexPolicy(support)
	}
	cmd, release, err := harnessCommand(ctx, c.binary, args, os.Environ(), harnessCodex)
	if err != nil {
		return "", fmt.Errorf("codex exec: %w", err)
	}
	defer release()
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runHarnessCommand(cmd); err != nil {
		return "", fmt.Errorf("codex exec: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

func codexInvocationArgs(declared map[string]bool) []string {

	args := []string{
		"exec",
		"--sandbox", "read-only",
		"--ignore-user-config",
		"--ignore-rules",
		"--skip-git-repo-check",
		"--ephemeral",
	}
	for _, key := range codexNoToolFeatureKeys {

		if declared != nil && !declared[key] {
			continue
		}
		args = append(args, "-c", "features."+key+"=false")
	}
	return append(args, "-c", "agents.enabled=false", "-c", `web_search="disabled"`, "-")
}

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

type codexFeatureSupport struct {
	declared map[string]bool
	probed   bool
	// at is when the probe ran, for the ONE thing the cache is not keyed on: a
	// negative verdict is re-tried after codexFeatureRetry so a codex that stops
	// answering (an in-place upgrade, a PATH that briefly went missing) recovers
	// without a restart.
	at time.Time
}

const codexFeatureRetry = 5 * time.Minute

type codexBinaryID struct {
	path    string
	size    int64
	modTime time.Time
}

func (id codexBinaryID) probeKey() string {
	return id.path + "\x00" + strconv.FormatInt(id.size, 10) + "\x00" + strconv.FormatInt(id.modTime.UnixNano(), 10)
}

var codexFeatureCache sync.Map

var codexProbeGroup singleflight.Group

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

	for {
		flight := codexProbeGroup.DoChan(id.probeKey(), func() (any, error) {
			if support, ok := codexCachedSupport(id); ok {
				return support, nil
			}
			support := probeCodexFeatures(ctx, path, id)
			if ctx.Err() != nil {

				return support, probeContextError{ctx.Err()}
			}
			codexFeatureCache.Store(id, support)
			return support, nil
		})

		select {
		case <-ctx.Done():

			return codexFeatureSupport{}
		case res := <-flight:

			if ctx.Err() != nil {
				return codexFeatureSupport{}
			}

			if isSharedProbeCancellation(res.Err, ctx) {

				continue
			}

			if res.Err != nil {
				return codexFeatureSupport{}
			}
			return res.Val.(codexFeatureSupport)
		}
	}
}

func probeCodexFeatures(ctx context.Context, path string, id codexBinaryID) codexFeatureSupport {

	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, err := harnessCommand(probeCtx, path, []string{"features", "list"}, os.Environ(), harnessCodex)
	if err != nil {

		slog.Warn("codex feature probe could not be confined; its answer is unknown",
			"error", err)
		return codexFeatureSupport{at: time.Now()}
	}
	defer release()

	support := codexFeatureSupport{at: time.Now()}
	if out, err := runHarnessCommandOutput(probe); err == nil {

		text := string(out)
		if !isCodexFeatureTable(text) {
			support.probed = false
		} else {
			support.probed = true
			support.declared = parseCodexFeaturesList(text)
		}
	}

	return support
}

var (
	codexUnverifiedWarned sync.Once
	codexWeakerWarned     sync.Once
)

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

var codexFeatureStages = map[string]bool{
	"stable":              true,
	"experimental":        true,
	"under development":   true,
	"deprecated":          true,
	"removed":             true,
	"disabled by default": true,
}

func isCodexFeatureTable(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if codexFeatureRow(line) {
			return true
		}
	}
	return false
}

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

func missingCodexFeatureKeys(declared map[string]bool) []string {
	var missing []string
	for _, key := range codexNoToolFeatureKeys {
		if !declared[key] {
			missing = append(missing, key)
		}
	}
	return missing
}
