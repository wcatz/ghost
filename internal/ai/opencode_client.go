package ai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

type OpenCodeClient struct {
	binary string

	model string
}

const DefaultOpenCodeModel = "opencode/big-pickle"

// NewOpenCodeClient creates an OpenCodeClient that invokes `opencode` on PATH.
func NewOpenCodeClient() *OpenCodeClient {
	return NewOpenCodeClientWithBinary("opencode")
}

// NewOpenCodeClientWithBinary creates an OpenCodeClient that invokes the given
// binary path (absolute, or a name resolved from PATH).
func NewOpenCodeClientWithBinary(binary string) *OpenCodeClient {
	return &OpenCodeClient{binary: binary}
}

func NewOpenCodeClientWithBinaryAndModel(binary, model string) *OpenCodeClient {
	return &OpenCodeClient{binary: binary, model: model}
}

func (c *OpenCodeClient) Reflect(ctx context.Context, prompt string) (string, TokenUsage, error) {
	text, err := c.run(ctx, prompt)
	return text, TokenUsage{}, err
}

func (c *OpenCodeClient) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	return c.run(ctx, systemPrompt+"\n\n"+userContent)
}

func (c *OpenCodeClient) run(ctx context.Context, prompt string) (_ string, err error) {
	ctx, cancel := boundHarnessCall(ctx)
	defer cancel()
	defer func() { err = explainDefaultBound(ctx, err) }()

	major := c.majorVersion(ctx)
	model := c.model
	if model == "" {
		model = os.Getenv("GHOST_OPENCODE_MODEL")
	}
	if model == "" {
		model = DefaultOpenCodeModel
	}
	cmd, cleanup, err := c.subprocessEnv(ctx, openCodeInvocationArgs(major, model), openCodePolicyFor(major))
	if err != nil {
		return "", err
	}
	defer cleanup()

	cmd.Stdin = strings.NewReader(openCodeNoToolsPreamble + prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := runHarnessCommand(cmd); err != nil {

		if cause := ctx.Err(); cause != nil {
			return "", fmt.Errorf("opencode run: %w: %s: %w", err, harnessFailureOutput(stdout.String(), stderr.String()), cause)
		}

		return "", fmt.Errorf("opencode run: %w: %s", err, harnessFailureOutput(stdout.String(), stderr.String()))
	}
	return parseOpenCodeOutput(stdout.String())
}

var opencodeMajors sync.Map

type opencodeBinaryID struct {
	path    string
	size    int64
	modTime time.Time
}

func (c *OpenCodeClient) majorVersion(ctx context.Context) int {
	path, err := exec.LookPath(c.binary)
	if err != nil {
		return 0
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	id := opencodeBinaryID{path: path, size: info.Size(), modTime: info.ModTime()}
	if v, ok := opencodeMajors.Load(id); ok {
		return v.(int)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	probe, release, err := harnessCommand(pctx, path, []string{"--version"}, os.Environ(), harnessOpencode)
	if err != nil {
		return 0
	}
	defer release()
	out, err := runHarnessCommandOutput(probe)
	if err != nil {
		return 0
	}
	major := OpencodeMajorVersion(string(out))
	if major <= 0 {
		return 0
	}
	opencodeMajors.Store(id, major)
	return major
}

func OpencodeMajorVersion(out string) int {
	for _, field := range strings.Fields(out) {
		field = strings.TrimPrefix(field, "v")
		major, _, ok := strings.Cut(field, ".")
		if !ok {
			continue
		}
		if n, err := strconv.Atoi(major); err == nil {
			return n
		}
	}
	return 0
}

func openCodeInvocationArgs(major int, model string) []string {
	if major >= 2 {
		return []string{"run", "--format", "json", "--standalone", "--title", ghostSessionTitle, "-m", model}
	}
	return []string{"run", "--format", "json", "--pure", "--title", ghostSessionTitle, "-m", model}
}

func openCodePolicyFor(major int) string {
	if major >= 2 {
		return openCodeAskConfig
	}
	return openCodeDenyConfig
}

const openCodeAskConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "*": "ask"
  },
  "mcp": {},
  "plugin": []
}`

const openCodeDenyConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "permission": {
    "*": "deny",
    "mcp_*": "deny"
  },
  "tools": {
    "bash": false,
    "edit": false,
    "write": false,
    "read": false,
    "grep": false,
    "glob": false,
    "list": false,
    "patch": false,
    "webfetch": false,
    "task": false,
    "todowrite": false,
    "todoread": false
  },
  "mcp": {},
  "plugin": []
}`

func (c *OpenCodeClient) subprocessEnv(ctx context.Context, args []string, policy string) (*exec.Cmd, func(), error) {
	cmd, release, err := harnessCommand(ctx, c.binary, args, os.Environ(), harnessOpencode)
	if err != nil {
		return nil, nil, err
	}
	if err := configureOpenCodeIsolation(cmd, policy); err != nil {
		release()
		return nil, nil, err
	}
	return cmd, release, nil
}

func configureOpenCodeIsolation(cmd *exec.Cmd, policy string) error {
	if cmd.Dir == "" {
		return fmt.Errorf("opencode scratch directory is empty")
	}
	home := filepath.Join(cmd.Dir, "home")
	configDir := filepath.Join(cmd.Dir, "opencode-config")
	dataDir := filepath.Join(cmd.Dir, "opencode-data")
	cacheDir := filepath.Join(cmd.Dir, "opencode-cache")
	stateDir := filepath.Join(cmd.Dir, "opencode-state")
	runtimeDir := filepath.Join(cmd.Dir, "opencode-runtime")
	for _, dir := range []string{home, configDir, dataDir, cacheDir, stateDir, runtimeDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("opencode isolated dir %s: %w", dir, err)
		}
	}
	configPath := filepath.Join(configDir, "opencode.json")
	if err := os.WriteFile(configPath, []byte(policy), 0o600); err != nil {
		return fmt.Errorf("opencode isolated config: %w", err)
	}
	if err := copyOpenCodeAuth(cmd.Env, dataDir); err != nil {
		return err
	}

	env := cmd.Env
	for key, value := range map[string]string{
		"HOME":                            home,
		"USERPROFILE":                     home,
		"XDG_CONFIG_HOME":                 configDir,
		"XDG_DATA_HOME":                   dataDir,
		"XDG_CACHE_HOME":                  cacheDir,
		"XDG_STATE_HOME":                  stateDir,
		"XDG_RUNTIME_DIR":                 runtimeDir,
		"OPENCODE_CONFIG_DIR":             configDir,
		"OPENCODE_CONFIG":                 configPath,
		"OPENCODE_CONFIG_CONTENT":         policy,
		"OPENCODE_CLI_CONFIG_CONTENT":     policy,
		"OPENCODE_DISABLE_PROJECT_CONFIG": "1",
	} {
		env = setHarnessEnvValue(env, key, value)
	}
	cmd.Env = env
	return nil
}

func copyOpenCodeAuth(env []string, dataDir string) error {
	if harnessEnvValue(env, "OPENCODE_API_KEY") != "" {
		return nil
	}
	source := openCodeAuthSource(env)
	if source == "" {
		return nil
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return fmt.Errorf("read opencode auth file: %w", err)
	}
	authDir := filepath.Join(dataDir, "opencode")
	if err := os.MkdirAll(authDir, 0o700); err != nil {
		return fmt.Errorf("create opencode auth directory: %w", err)
	}
	if err := os.WriteFile(filepath.Join(authDir, "auth.json"), contents, 0o600); err != nil {
		return fmt.Errorf("copy opencode auth file: %w", err)
	}
	return nil
}

func openCodeAuthSource(env []string) string {
	var candidates []string
	seen := make(map[string]bool)
	add := func(parts ...string) {
		if len(parts) == 0 || parts[0] == "" {
			return
		}
		path := filepath.Join(parts...)
		if !seen[path] {
			seen[path] = true
			candidates = append(candidates, path)
		}
	}

	if dataHome := harnessEnvValue(env, "XDG_DATA_HOME"); dataHome != "" {
		add(dataHome, "opencode", "auth.json")
	}
	for _, homeKey := range []string{"HOME", "USERPROFILE"} {
		if home := harnessEnvValue(env, homeKey); home != "" {
			add(home, ".local", "share", "opencode", "auth.json")
			add(home, ".config", "opencode", "auth.json")
			add(home, "Library", "Application Support", "opencode", "auth.json")
		}
	}
	for _, dataKey := range []string{"LOCALAPPDATA", "APPDATA"} {
		if dataHome := harnessEnvValue(env, dataKey); dataHome != "" {
			add(dataHome, "opencode", "auth.json")
		}
	}
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() {
			return path
		}
	}
	return ""
}

func harnessEnvValue(env []string, key string) string {
	for _, kv := range env {
		name, value, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(name, key) {
			return value
		}
	}
	return ""
}

func setHarnessEnvValue(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		name, _, ok := strings.Cut(kv, "=")
		if ok && strings.EqualFold(name, key) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, key+"="+value)
}

var tempDirKeys = []string{"TMPDIR", "TMP", "TEMP"}

func isTempDirKey(kv string) bool {
	key, _, found := strings.Cut(kv, "=")
	if !found {
		return false
	}
	for _, candidate := range tempDirKeys {
		if strings.EqualFold(key, candidate) {
			return true
		}
	}
	return false
}

type opencodeEvent struct {
	Type string `json:"type"`
	Part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"part"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

const openCodeNoToolsPreamble = "You have no tools in this session. Do not call any tool; answer directly in plain text. "

var maxOpencodeOutputLine = 64 << 20

func parseOpenCodeOutput(raw string) (string, error) {
	var sb strings.Builder
	sc := bufio.NewScanner(strings.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), maxOpencodeOutputLine)
	for sc.Scan() {
		if len(strings.TrimSpace(string(sc.Bytes()))) == 0 {
			continue
		}
		var ev opencodeEvent
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			return "", fmt.Errorf("opencode run: unparseable output line: %w", err)
		}
		if ev.Type == "error" {
			errType := ev.Error.Type
			if errType == "" {
				errType = "unknown"
			}
			if ev.Error.Message != "" {
				return "", fmt.Errorf("opencode run: error event %q: %s", errType, memory.RefusalArg("message", ev.Error.Message))
			}
			return "", fmt.Errorf("opencode run: error event %q", errType)
		}
		if ev.Type == "text" && ev.Part.Type == "text" {
			sb.WriteString(ev.Part.Text)
		}
	}
	if err := sc.Err(); err != nil {
		return "", fmt.Errorf("opencode run: read output: %w", err)
	}
	return sb.String(), nil
}
