//go:build e2e

// Package e2e exercises the BUILT ghost binary the way a user runs it.
//
// Every other test in this repository calls into a package in this process:
// the store, the assembler, the CLI's run* functions. That is the right way to
// test logic, and it cannot see anything that lives between the two — argv
// parsing, the config file, the exit code, the JSON-RPC framing, the harness
// child process, the schema migration an old database actually takes. This
// package is the layer above all of that: one `go build` of ./cmd/ghost, then
// nothing but that binary on a sandboxed filesystem.
//
// Three rules make the result worth having.
//
//   - Build once, run the real program. TestMain compiles the binary a single
//     time and every test executes that file. Nothing re-execs os.Args[0]:
//     under `go test` the test binary IS the suite, so a self-spawn re-runs
//     every test in the package once per spawn.
//   - Every run is sandboxed. A temp HOME, XDG_CONFIG_HOME, XDG_DATA_HOME and
//     XDG_CACHE_HOME, and a child environment built from an allowlist rather
//     than inherited, so no GHOST_* setting, host marker or real
//     ~/.local/share/ghost is reachable. The only layer a sandbox cannot
//     redirect is the system-wide /etc/ghost/config.yaml.
//   - Nothing real is called. The LLM harnesses are shell fakes first on PATH,
//     and the embedding endpoint is a deterministic in-process HTTP server.
//     No subscription-billed CLI and no local model is ever reached.
//
// The build tag is why `go test ./...` and CI are unchanged: with the tag
// absent every file here is excluded, and a directory whose files are all
// build-constrained out is silently skipped by the ./... pattern (verified
// empirically, not assumed). Run it with `make test-e2e`, or:
//
//	go test -tags e2e ./e2e/ -count=1
package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	_ "modernc.org/sqlite"
)

// ghostBin is the single binary every test here runs. TestMain builds it once;
// GHOST_E2E_BIN overrides that for a developer pointing the suite at an
// already-built binary.
var ghostBin string

// commandTimeout bounds one `ghost ...` invocation. Generous, because a
// reflection phase legitimately spawns a harness and a migration legitimately
// takes its pre-migration backup — but finite, so a hang fails the test instead
// of the package.
const commandTimeout = 3 * time.Minute

// TestMain builds the binary once and removes it afterwards. The build is the
// reason this package exists, so it is not cached across runs: a stale binary
// would quietly test yesterday's code.
func TestMain(m *testing.M) {
	dir, cleanup, err := resolveBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: %v\n", err)
		os.Exit(1)
	}
	ghostBin = dir
	code := m.Run()
	cleanup()
	os.Exit(code)
}

// resolveBinary returns the binary to test and the function that releases
// whatever acquiring it allocated.
func resolveBinary() (string, func(), error) {
	if p := os.Getenv("GHOST_E2E_BIN"); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", nil, err
		}
		if _, err := os.Stat(abs); err != nil {
			return "", nil, fmt.Errorf("GHOST_E2E_BIN=%s: %w", p, err)
		}
		return abs, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "ghost-e2e-bin-")
	if err != nil {
		return "", nil, fmt.Errorf("temp dir for the build: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	out := filepath.Join(dir, "ghost")
	if runtime.GOOS == "windows" {
		out += ".exe"
	}
	root, err := moduleRoot()
	if err != nil {
		cleanup()
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "build", "-o", out, "./cmd/ghost")
	cmd.Dir = root
	// CGO_ENABLED=0 is the shipped build (Makefile, goreleaser, Dockerfile);
	// a cgo build would exercise a configuration no user ever runs.
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("go build ./cmd/ghost: %w\n%s", err, stderr.String())
	}
	return out, cleanup, nil
}

// moduleRoot walks up from the working directory to the directory holding
// go.mod, so the build is anchored on the module rather than on the caller's
// cwd.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// fakeModel is the model name the sandbox configuration asks for. It is the
// shipped default, so the task-prefix table in internal/embedding/prefix.go
// takes its real branch rather than a bypass.
const fakeModel = "nomic-embed-text:v1.5"

// fakeDimensions is the width the fake embedding endpoint produces. Smaller
// than the shipped 768 purely to keep the HTTP payloads small; it is set in
// the sandbox config, and both the client and the store read it from there.
const fakeDimensions = 256

// harnessBinaries are the four CLI harnesses the fakes impersonate. All four
// are installed because the MCP server's ghost_resolve routes by the CALLING
// CLIENT's name, and that name is this suite's to choose.
var harnessBinaries = []string{"opencode", "claude", "codex", "goose"}

// sandbox is one isolated filesystem plus the environment that reaches it.
// Every command a test runs goes through it, so a test cannot accidentally
// inherit the developer's own Ghost state.
type sandbox struct {
	t *testing.T

	root    string // everything below is inside here
	home    string
	config  string // XDG_CONFIG_HOME
	data    string // XDG_DATA_HOME
	cache   string // XDG_CACHE_HOME
	tmp     string // TMPDIR
	work    string // the directory sessions run in
	bin     string // fake harness directory, first on PATH
	scratch string // GHOST_SCRATCH_DIR

	env     []string
	ollama  *ollamaStub
	started time.Time
}

// newSandbox creates a sandbox with a config file that points every external
// dependency at a fake: the embedding endpoint at the in-process stub, the
// harnesses at the shell scripts in bin, and the scratch root inside the
// sandbox so no harness child can be tempted to write to a real data dir.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake harnesses are POSIX shell scripts; the CLI surface is covered on POSIX")
	}
	root := t.TempDir()
	s := &sandbox{
		t:       t,
		root:    root,
		home:    filepath.Join(root, "home"),
		config:  filepath.Join(root, "config"),
		data:    filepath.Join(root, "data"),
		cache:   filepath.Join(root, "cache"),
		tmp:     filepath.Join(root, "tmp"),
		work:    filepath.Join(root, "work"),
		bin:     filepath.Join(root, "bin"),
		scratch: filepath.Join(root, "scratch"),
		started: time.Now(),
	}
	for _, d := range []string{s.home, s.config, s.data, s.cache, s.tmp, s.work, s.bin, s.scratch} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	s.ollama = newOllamaStub(t, fakeModel, fakeDimensions)
	s.writeHarnessFakes()
	s.writeConfig(configOpts{})
	s.env = s.buildEnv()
	return s
}

// buildEnv assembles the child environment from an allowlist. It is built, not
// filtered: os.Environ() minus a list of prefixes cannot answer "is there a
// variable here nobody thought of", and a sandbox assembled that way inherits
// whatever the developer had exported.
func (s *sandbox) buildEnv() []string {
	binDir := filepath.Dir(ghostBin)
	return []string{
		"PATH=" + s.bin + string(os.PathListSeparator) + binDir +
			string(os.PathListSeparator) + "/usr/local/bin" +
			string(os.PathListSeparator) + "/usr/bin" +
			string(os.PathListSeparator) + "/bin",
		"HOME=" + s.home,
		"USERPROFILE=" + s.home,
		"USER=e2e",
		"LOGNAME=e2e",
		"SHELL=/bin/sh",
		"TMPDIR=" + s.tmp,
		"TMP=" + s.tmp,
		"TEMP=" + s.tmp,
		"XDG_CONFIG_HOME=" + s.config,
		"XDG_DATA_HOME=" + s.data,
		"XDG_CACHE_HOME=" + s.cache,
		"LANG=C",
		"TERM=dumb",
		// The embedding endpoint the config also names, so a child that
		// re-derives it from the environment agrees with the file.
		"GHOST_OLLAMA_URL=" + s.ollama.URL,
		"GHOST_SCRATCH_DIR=" + s.scratch,
	}
}

// configOpts is the set of decisions a sandbox's config.yaml encodes. It is a
// struct rather than free text because a test that appends its own section can
// otherwise silently produce a duplicate YAML key, and which of the two values
// wins then depends on the parser — a test whose setting does not take effect
// fails for a reason that has nothing to do with what it is testing.
type configOpts struct {
	// sessionScope is written as injection.session_scope. Empty omits the
	// whole section.
	sessionScope map[string]string
	// autoReflect, autoResolve and autoSupersede are the three
	// auto-consolidation phases. All default off, so a Stop hook never spawns
	// a chain unless a test asks for one.
	autoReflect   bool
	autoResolve   bool
	autoSupersede bool
	// minInterval is lifecycle.min_interval. Empty uses the shipped default.
	minInterval string
	// extra is appended verbatim, for the handful of keys with no field here.
	extra string
}

// writeConfig writes the sandbox config.yaml. Every value is a test decision
// rather than a default:
//
//   - embedding points at the stub, so hybrid search and the supersede
//     candidate scan have vectors to work with and no model is ever called;
//   - linking is off, because the linking worker's near-duplicate demotion
//     would reorder results underneath assertions about a different surface.
func (s *sandbox) writeConfig(opts configOpts) {
	var sb strings.Builder
	sb.WriteString("# Written by the e2e suite. Every value here is a test decision.\n")
	fmt.Fprintf(&sb, "embedding:\n  enabled: true\n  ollama_url: %s\n  model: %s\n  dimensions: %d\n",
		s.ollama.URL, fakeModel, fakeDimensions)
	sb.WriteString("linking:\n  enabled: false\n")
	sb.WriteString("obsidian:\n  auto_sync: false\n  interval: 30s\n")
	fmt.Fprintf(&sb, "reflection:\n  auto_reflect: %t\n  auto_resolve: %t\n  auto_supersede: %t\n",
		opts.autoReflect, opts.autoResolve, opts.autoSupersede)
	// Short bounds: a harness spawn here is a shell script, so the shipped
	// 10- and 60-minute timeouts are never the thing under test, and a shorter
	// one turns a hang into a failure instead of a package timeout.
	sb.WriteString("  consolidation_timeout_minutes: 2\n  lifecycle_timeout_minutes: 3\n")
	if opts.minInterval != "" {
		fmt.Fprintf(&sb, "lifecycle:\n  min_interval: %s\n", opts.minInterval)
	}
	if len(opts.sessionScope) > 0 {
		sb.WriteString("injection:\n  session_scope:\n")
		// Sorted so the file is byte-stable across runs, which matters because
		// two runs of the same test must produce the same config.
		for _, k := range sortedKeys(opts.sessionScope) {
			fmt.Fprintf(&sb, "    %s: %s\n", k, opts.sessionScope[k])
		}
	}
	sb.WriteString("scratch:\n  max_bytes: 67108864\n")
	if opts.extra != "" {
		sb.WriteString(opts.extra)
		if !strings.HasSuffix(opts.extra, "\n") {
			sb.WriteString("\n")
		}
	}
	dir := filepath.Join(s.config, "ghost")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatalf("mkdir config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(sb.String()), 0o600); err != nil {
		s.t.Fatalf("write config.yaml: %v", err)
	}
}

// reconfigure rewrites the sandbox config.yaml, for the tests that need a
// different setting.
func (s *sandbox) reconfigure(opts configOpts) {
	s.t.Helper()
	s.writeConfig(opts)
}

// harnessScript is the fake CLI harness. It is one template rendered per
// binary name; the differences between the backends are exactly the two
// things that matter to a caller — how the capability probe is answered, and
// whether stdout is JSONL or the bare answer.
//
// The script decides what to answer by reading the prompt it was handed, which
// is the honest way to fake a model: the three prompts Ghost sends are
// distinguishable by their own contract text, and a fake keyed on a
// hand-maintained table of test names would prove nothing about whether Ghost
// sends a parseable answer.
const harnessScript = `#!/bin/sh
# Fake @NAME@ harness. It never contacts a model, a network or a real opencode.
dir=@DIR@
name=@NAME@

printf 'ARGV %s\n' "$*" >> "$dir/$name.argv"

case "$1:$2" in
  session:list)
    cat "$dir/$name.sessions" 2>/dev/null || printf '[]'
    exit 0
    ;;
  session:delete)
    printf '%s\n' "$3" >> "$dir/$name.deleted"
    exit 0
    ;;
  mcp:ls)
    printf 'ghost\n'
    exit 0
    ;;
  mcp:get)
    if [ -f "$dir/claude.mcp.ghost" ]; then
      printf 'ghost:\n  Scope: User\n'
      cat "$dir/claude.mcp.ghost"
      exit 0
    fi
    printf 'No MCP server named "ghost" found.\n' >&2
    exit 1
    ;;
  mcp:add-json)
    shift 5
    printf '%s\n' "$*" > "$dir/claude.mcp.ghost"
    exit 0
    ;;
  mcp:remove)
    rm -f "$dir/claude.mcp.ghost"
    exit 0
    ;;
esac

case "$1" in
  --version)
    # opencode reads the major version off stdout to choose its argv, and
    # mcpinit uses the same probe to decide how to verify registration.
    printf '%s\n' '2.0.15'
    exit 0
    ;;
  --help)
    # The claude adapter refuses to run without these no-tools flags, and
    # detects the optional ones by substring. Lower-case in the probe output
    # is fine: the parser lower-cases the whole help text.
    printf '%s\n' '--safe-mode' '--restricted' '--strict-mcp-config' \
      '--disable-slash-commands' '--tools' '--disallowedtools' '--setting-sources'
    exit 0
    ;;
esac

in="$dir/.stdin.$$"
cat > "$in"
printf -- '--- prompt (%s) ---\n' "$(date +%s 2>/dev/null || echo 0)" >> "$dir/$name.prompts"
cat "$in" >> "$dir/$name.prompts"
printf '\n' >> "$dir/$name.prompts"

# --- which operation is this? -------------------------------------------------
# The three prompts carry their own contract text, and the markers below are
# that contract rather than an implementation detail: "learned_context" is the
# field the reflect answer must have, "closed-by:" is the field a RESOLVED
# verdict must have, and OLDER/NEWER is the pair the supersede prompt hands
# over. A marker that stopped matching would make every assertion downstream
# fail loudly rather than quietly answer the wrong question.
#
# The rubric is searched in argv as well as stdin, and it has to be: the claude
# backend is invoked with the system prompt as a real --system-prompt argument
# and only the user content on stdin, so a classifier reading stdin alone would
# not see that this is a resolve call at all. codex and goose join the two
# before stdin; opencode prefixes stdin. The union is what every backend needs.
av="$dir/.argv.$$"
printf '%s\n' "$*" > "$av"
has() { grep -q -e "$1" "$in" "$av"; }
if has 'learned_context'; then
  kind=reflect
elif has 'closed-by:'; then
  kind=resolve
elif has 'NEWER'; then
  kind=supersede
else
  kind=unknown
fi
printf '%s\n' "$kind" >> "$dir/$name.kinds"

# A test can ask for the next N calls of one kind to FAIL, the way a real harness
# occasionally exits 1. The counter is a file because every classify call is its
# own process, and only the first N fail: the calls after them answer normally,
# which is exactly what a transient failure looks like and what the retry is for.
n_calls=0
if [ -f "$dir/$kind.calls" ]; then
  n_calls=$(cat "$dir/$kind.calls" 2>/dev/null || printf 0)
fi
printf '%s\n' "$((n_calls + 1))" > "$dir/$kind.calls"
if [ -f "$dir/$kind.fails" ]; then
  n_fails=$(cat "$dir/$kind.fails" 2>/dev/null || printf 0)
  if [ "$n_calls" -lt "$n_fails" ]; then
    # On stderr and with no stdout: that is what a real failure looks like, and
    # the product reads the complaint from there.
    printf 'fake %s: simulated harness failure on %s call %s\n' "$name" "$kind" "$((n_calls + 1))" >&2
    exit 1
  fi
fi

# --- build the answer ---------------------------------------------------------
# The batched prompts number their items ("1." on a line of its own); the
# single-item prompts do not. Count the numbers rather than the items, so a
# note whose text happens to contain a number cannot inflate the count. The
# numbered markers are read from stdin only — they are per-item content, and
# counting them out of argv would count the rubric's own worked examples.
n=0
for num in $(sed -n 's/^\([0-9][0-9]*\)\.$/\1/p' "$in"); do
  n=$((n + 1))
done

case "$kind" in
  reflect)
    # One "keep <id>" per input id, taken from the prompt's own id list. A keep
    # adds no text, so it cannot trip the identifier rule, and the output size
    # equals the input size, so the quality gate is satisfied honestly rather
    # than by a corpus that vanished.
    ops=''
    for id in $(sed -n 's/^- id:\([^ ]*\).*/\1/p' "$in"); do
      if [ -n "$ops" ]; then ops="$ops,"; fi
      ops="$ops\"keep $id\""
    done
    if [ -z "$ops" ]; then
      printf 'fake reflect: the prompt carried no memory ids\n' >&2
      exit 4
    fi
    answer="{\"learned_context\":\"fake consolidation\",\"ops\":[$ops]}"
    ;;
  resolve|supersede)
    if [ "$kind" = resolve ]; then
      ans=$(cat "$dir/resolve.answer" 2>/dev/null || printf 'KEEP')
    else
      ans=$(cat "$dir/supersede.answer" 2>/dev/null || printf 'NEITHER')
    fi
    if [ "$n" -gt 1 ]; then
      answer=''
      i=1
      while [ "$i" -le "$n" ]; do
        if [ -n "$answer" ]; then answer="$answer
"; fi
        answer="$answer$i: $ans"
        i=$((i + 1))
      done
    else
      answer="$ans"
    fi
    ;;
  *)
    printf 'fake %s: unrecognised prompt (first 200 bytes: %s)\n' "$name" "$(head -c 200 "$in" | tr '\n' ' ')" >&2
    exit 5
    ;;
esac

rm -f "$in" "$av"

# claude, codex and goose take the answer as bare stdout: their adapters either
# return the stream verbatim or TrimSpace it, and then the operation's own parser
# reads it. That is why every diagnostic above went to stderr — a banner here
# would be parsed as part of the verdict.
printf '%s\n' "$answer"
`

// opencodeWrap is the one tail the opencode backend needs and the others do
// not: its adapter parses stdout as JSONL and concatenates every text part, so
// the same answer text has to be delivered inside a text event. A banner or a
// stray non-JSON line on that stream is a hard error on the product side, which
// is why every diagnostic in the shared script went to stderr instead.
//
// The answer is escaped once, here, and interpolated into a single JSON string
// literal — the reflect answer is a JSON object of its own, so the escaping
// cannot be skipped without the harness being unreadable.
const opencodeWrap = `escaped=$(printf '%s' "$answer" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')
cat > "$dir/.answer.$$" <<EOF
{"type":"text","part":{"type":"text","text":"$escaped"}}
EOF
cat "$dir/.answer.$$"
rm -f "$dir/.answer.$$" "$in" "$av"
exit 0
`

// writeHarnessFakes installs one fake per backend, plus the two answer files a
// test edits to choose a verdict (resolve.answer, supersede.answer).
func (s *sandbox) writeHarnessFakes() {
	s.t.Helper()
	for _, name := range harnessBinaries {
		body := strings.NewReplacer(
			"@NAME@", name,
			"@DIR@", s.bin,
		).Replace(harnessScript)
		if name == "opencode" {
			// Splice the JSONL tail in place of the shared one: the opencode
			// adapter is the only backend that needs a JSON envelope around
			// the same answer text.
			cut := strings.Index(body, "rm -f \"$in\" \"$av\"\n\n# claude, codex")
			if cut < 0 {
				s.t.Fatalf("fake harness template lost its tail")
			}
			body = body[:cut] + opencodeWrap
		}
		path := filepath.Join(s.bin, name)
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			s.t.Fatalf("write fake %s: %v", name, err)
		}
	}
	for name, def := range map[string]string{"resolve.answer": "KEEP", "supersede.answer": "NEITHER"} {
		if err := os.WriteFile(filepath.Join(s.bin, name), []byte(def+"\n"), 0o644); err != nil {
			s.t.Fatalf("write %s: %v", name, err)
		}
	}
	// The opencode session list is read by `ghost opencode cleanup-sessions`.
	if err := os.WriteFile(filepath.Join(s.bin, "opencode.sessions"), []byte("[]\n"), 0o644); err != nil {
		s.t.Fatalf("write opencode.sessions: %v", err)
	}
}

// setHarnessAnswer chooses the verdict the fakes return for resolve or
// supersede. It is how a test says "this pass should resolve its candidates"
// without the fake having to understand the test.
func (s *sandbox) setHarnessAnswer(kind, answer string) {
	s.t.Helper()
	path := filepath.Join(s.bin, kind+".answer")
	if err := os.WriteFile(path, []byte(answer+"\n"), 0o644); err != nil {
		s.t.Fatalf("write %s: %v", path, err)
	}
}

// failHarnessCalls makes the next n calls of one kind (resolve, supersede) exit
// non-zero, the way a real harness occasionally does, and RESETS that kind's
// call counter. It is how a test says "the first call dies and the retry
// answers" (n=1) or "the call and its retry both die" (n=2) without the fake
// having to understand the test. The counter is a file because every classify
// call is its own process, and the failure is transient by design: calls after
// the n-th answer normally, which is the whole point of a retry.
func (s *sandbox) failHarnessCalls(kind string, n int) {
	s.t.Helper()
	if err := os.WriteFile(filepath.Join(s.bin, kind+".fails"), []byte(strconv.Itoa(n)+"\n"), 0o644); err != nil {
		s.t.Fatalf("write %s.fails: %v", kind, err)
	}
	if err := os.Remove(filepath.Join(s.bin, kind+".calls")); err != nil && !os.IsNotExist(err) {
		s.t.Fatalf("reset %s.calls: %v", kind, err)
	}
}

// harnessLog returns everything the fakes recorded for one backend: the argv
// of every invocation, and every prompt it was sent.
func (s *sandbox) harnessLog(name string) (argv, prompts string) {
	a, err := os.ReadFile(filepath.Join(s.bin, name+".argv"))
	if err == nil {
		argv = string(a)
	}
	p, err := os.ReadFile(filepath.Join(s.bin, name+".prompts"))
	if err == nil {
		prompts = string(p)
	}
	return argv, prompts
}

// result is one finished `ghost ...` invocation.
type result struct {
	stdout string
	stderr string
	code   int
}

// String renders the invocation for a failure message, so a failing assertion
// says which command produced the output it is complaining about.
func (r result) String() string {
	return fmt.Sprintf("exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s", r.code, r.stdout, r.stderr)
}

// run executes the built binary in the sandbox with no stdin.
func (s *sandbox) run(args ...string) result {
	return s.runStdin("", args...)
}

// runStdin executes the built binary in the sandbox with the given stdin.
func (s *sandbox) runStdin(stdin string, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghostBin, args...)
	cmd.Env = s.env
	cmd.Dir = s.work
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			s.t.Fatalf("ghost %s: %v", strings.Join(args, " "), err)
		}
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// runIn runs the binary with an explicit working directory.
func (s *sandbox) runIn(dir string, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, ghostBin, args...)
	cmd.Env = s.env
	cmd.Dir = dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			s.t.Fatalf("ghost %s: %v", strings.Join(args, " "), err)
		}
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

// asExitError reports whether err is an *exec.ExitError and binds it.
func asExitError(err error, target **exec.ExitError) bool {
	ee, ok := err.(*exec.ExitError)
	if ok {
		*target = ee
	}
	return ok
}

// mustRun runs a command and fails the test unless it exits 0.
func (s *sandbox) mustRun(args ...string) result {
	s.t.Helper()
	r := s.run(args...)
	if r.code != 0 {
		s.t.Fatalf("ghost %s: %s", strings.Join(args, " "), r)
	}
	return r
}

// mustRunStdin runs a command with the given stdin and fails the test unless it
// exits 0. The hook events all read their payload from stdin, so this is their
// only entry point.
func (s *sandbox) mustRunStdin(stdin string, args ...string) result {
	s.t.Helper()
	r := s.runStdin(stdin, args...)
	if r.code != 0 {
		s.t.Fatalf("ghost %s: %s", strings.Join(args, " "), r)
	}
	return r
}

// mustFailStdin runs a command with the given stdin and fails the test unless
// it exits non-zero. It is mustFail for the commands that read stdin, so a
// command that blocks waiting for a confirmation cannot hang the suite.
func (s *sandbox) mustFailStdin(stdin string, args ...string) result {
	s.t.Helper()
	r := s.runStdin(stdin, args...)
	if r.code == 0 {
		s.t.Fatalf("ghost %s: expected a non-zero exit, got %s", strings.Join(args, " "), r)
	}
	return r
}

// mustFail runs a command and fails the test unless it exits non-zero. Used for
// the error paths, where a zero exit IS the bug.
func (s *sandbox) mustFail(args ...string) result {
	s.t.Helper()
	r := s.run(args...)
	if r.code == 0 {
		s.t.Fatalf("ghost %s: expected a non-zero exit, got %s", strings.Join(args, " "), r)
	}
	return r
}

// dbPath is the sandbox store.
func (s *sandbox) dbPath() string { return filepath.Join(s.data, "ghost", "ghost.db") }

// dataDir is the sandbox Ghost data directory.
func (s *sandbox) dataDir() string { return filepath.Join(s.data, "ghost") }

// openDB opens the sandbox store READ-ONLY. It is an observer, never a writer:
// the assertions need row-level facts (a resolved_at stamp, a link edge, a
// history entry) that no command prints, and a read-only open cannot perturb
// the thing being observed. mode=ro means a missing file is an error instead
// of a silently created empty database.
func (s *sandbox) openDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(s.dbPath()) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-only: %v", s.dbPath(), err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// queryInt runs a single-value count query against the sandbox store.
func (s *sandbox) queryInt(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.openDB(t).QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// queryStrings runs a query returning one text column per row.
func (s *sandbox) queryStrings(t *testing.T, query string, args ...any) []string {
	t.Helper()
	rows, err := s.openDB(t).Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return out
}

// row is a small pair of typed values read back from the store, for the
// assertions that need two columns and do not justify a struct.
type row struct {
	text string
	num  float64
}

// queryRow runs a query returning exactly one TEXT and one numeric column. It
// exists because the obvious query for "content and importance together" is
// two columns, and a test that concatenates them in SQL to make a one-column
// query would be asserting on a string the database built rather than on the
// two values themselves.
func (s *sandbox) queryRow(t *testing.T, query string, args ...any) row {
	t.Helper()
	var r row
	if err := s.openDB(t).QueryRow(query, args...).Scan(&r.text, &r.num); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return r
}

// mcpSession starts `ghost mcp` and returns a connected client session. The
// client name is "claude-code" so the server's harness routing has a real
// answer: ghost_resolve picks its backend from the CALLING CLIENT's reported
// name, and a name nothing recognises would fail the tool for a reason that
// has nothing to do with the product.
func (s *sandbox) mcpSession(t *testing.T) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "claude-code", Version: "e2e"}, nil)
	cs, err := client.Connect(ctx, commandTransport(t, s), nil)
	if err != nil {
		t.Fatalf("connect to `ghost mcp`: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// commandTransport starts `ghost mcp` in the sandbox over stdio. It is separate
// from mcpSession so a test can connect with a DIFFERENT client identity —
// which is the only way to reach the harness-routing branches that key on
// ClientInfo.Name.
func commandTransport(t *testing.T, s *sandbox) *mcp.CommandTransport {
	t.Helper()
	cmd := exec.Command(ghostBin, "mcp")
	cmd.Env = s.env
	cmd.Dir = s.work
	return &mcp.CommandTransport{Command: cmd}
}

// contextWithTimeout returns a context bounded by commandTimeout plus cleanup,
// for the direct SDK calls a test makes outside the call helper.
func contextWithTimeout(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), commandTimeout)
}

// listToolNames returns the tools the server advertises. The list is read from
// the server rather than hardcoded, which is what makes the coverage check
// above it meaningful.
func listToolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	ctx, cancel := contextWithTimeout(t)
	defer cancel()
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

// resourceText flattens a resource read into one string.
func resourceText(res *mcp.ReadResourceResult) string {
	var sb strings.Builder
	for _, c := range res.Contents {
		if c.Text != "" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// templateAdvertised reports whether the server offers a resource template with
// the given URI template.
func templateAdvertised(t *testing.T, templates []*mcp.ResourceTemplate, uri string) bool {
	t.Helper()
	for _, tmpl := range templates {
		if tmpl.URITemplate == uri {
			return true
		}
	}
	return false
}

// promptText flattens a prompt's messages into one string.
func promptText(res *mcp.GetPromptResult) string {
	var sb strings.Builder
	for _, m := range res.Messages {
		if tc, ok := m.Content.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
			sb.WriteString("\n")
		}
	}
	return sb.String()
}

// containsString reports whether list holds want.
func containsString(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// call invokes one MCP tool and returns its text. A tool that reports an error
// is a failure: these tests assert that the product answers, not that it
// answers politely.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	text := resultText(res)
	if res.IsError {
		t.Fatalf("call %s reported an error: %s", name, text)
	}
	return text
}

// callExpectingError invokes one MCP tool that is expected to refuse, and
// returns its text.
func callExpectingError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		// A protocol-level error is a refusal too, and for some tools it is
		// the only shape the refusal can take.
		return err.Error()
	}
	if !res.IsError {
		t.Fatalf("call %s: expected a refusal, got %s", name, resultText(res))
	}
	return resultText(res)
}

// resultText flattens a tool result's content blocks into one string.
func resultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		} else {
			b, err := json.Marshal(c)
			if err == nil {
				sb.Write(b)
			}
		}
	}
	return sb.String()
}

// mustContain fails the test unless text holds want.
func mustContain(t *testing.T, what, text, want string) {
	t.Helper()
	if !strings.Contains(text, want) {
		t.Fatalf("%s: want %q in the output, got:\n%s", what, want, text)
	}
}

// mustNotContain fails the test if text holds unwanted.
func mustNotContain(t *testing.T, what, text, unwanted string) {
	t.Helper()
	if strings.Contains(text, unwanted) {
		t.Fatalf("%s: %q must not appear in the output, got:\n%s", what, unwanted, text)
	}
}

// mustMatch fails the test unless text matches pattern.
func mustMatch(t *testing.T, what, text, pattern string) {
	t.Helper()
	re, err := regexp.Compile(pattern)
	if err != nil {
		t.Fatalf("%s: bad pattern %q: %v", what, pattern, err)
	}
	if !re.MatchString(text) {
		t.Fatalf("%s: want a match for %q in the output, got:\n%s", what, pattern, text)
	}
}

// ollamaStub is the embedding endpoint. It answers the three requests Ghost
// makes — the reachability probe, the model list, and /api/embed — with
// deterministic vectors, so hybrid search, the health report and the supersede
// candidate scan all have a real vector space to work in without a model ever
// being loaded.
type ollamaStub struct {
	*httptest.Server
	mu      sync.Mutex
	inputs  []string
	failing bool
	// probeDelay and embedDelay hold one endpoint each, per request. They model
	// a machine that is BUSY rather than one whose embedding backend is broken:
	// a liveness probe and an embed both come back eventually, just not inside
	// the client's deadline. A sleep is the honest way to say that, because the
	// failure it reproduces is a request that took too long, not one that was
	// refused.
	probeDelay time.Duration
	embedDelay time.Duration
}

// slowEndpoint holds the liveness probe for probe and /api/embed for embed, per
// request, from now on. A delay the client has already walked away from returns
// as soon as it disconnects, so a slow stub never wedges the test's teardown.
func (s *ollamaStub) slowEndpoint(probe, embed time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probeDelay, s.embedDelay = probe, embed
}

// hold waits d, or returns early if the client gave up first.
func hold(r *http.Request, d time.Duration) {
	if d <= 0 {
		return
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-r.Context().Done():
	case <-t.C:
	}
}

// newOllamaStub starts the stub and registers its shutdown with t.
func newOllamaStub(t *testing.T, model string, dims int) *ollamaStub {
	t.Helper()
	stub := &ollamaStub{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
			Input string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		stub.mu.Lock()
		stub.inputs = append(stub.inputs, req.Input)
		failing := stub.failing
		delay := stub.embedDelay
		stub.mu.Unlock()
		hold(r, delay)
		if failing {
			http.Error(w, "embedding backend unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float32{stubVector(req.Input, dims)}})
	})
	mux.HandleFunc("/api/tags", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": model}}})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		stub.mu.Lock()
		delay := stub.probeDelay
		stub.mu.Unlock()
		hold(r, delay)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "Ollama is running")
	})
	stub.Server = httptest.NewServer(mux)
	t.Cleanup(stub.Close)
	return stub
}

// embedCalls returns every input the stub was asked to embed, so a test can
// assert that the document and query task prefixes really were applied.
func (s *ollamaStub) embedCalls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.inputs...)
}

// stubVector is a deterministic bag-of-words embedding: each token contributes
// to one hashed dimension and the result is L2-normalised, so two near-identical
// memories land close together and two unrelated ones do not. That is the
// property the supersede candidate scan and the vector search leg actually
// need, and it is reproducible without a model.
//
// The task prefix is stripped first, and that is not a convenience: the prefix
// is a role marker ("search_document:" / "search_query:"), and a bag of words
// that counted it as content would give every document and every query one
// token in common — enough, on a short query, to make the vector leg return
// matches for a query that shares no word with anything in the store. A real
// asymmetric model does not have that artefact, and neither should the fake.
func stubVector(text string, dims int) []float32 {
	for _, prefix := range []string{"search_document:", "search_query:"} {
		if rest, ok := strings.CutPrefix(text, prefix); ok {
			text = rest
			break
		}
	}
	vec := make([]float32, dims)
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		h := fnv.New64a()
		_, _ = h.Write([]byte(tok))
		sum := h.Sum64()
		idx := int(sum % uint64(dims))
		sign := float32(1)
		if sum&(1<<63) != 0 {
			sign = -1
		}
		vec[idx] += sign
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		// An all-stopword input still has to produce a usable vector, and a
		// fixed one keeps it comparable rather than collapsing to zero.
		vec[0] = 1
		return vec
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec
}

// mustReadFile fails the test unless path exists, and returns its bytes.
func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// runUntilSignal starts a long-running command, lets it work for a moment, and
// then signals it the way a user stops it with Ctrl-C. It returns what the
// process printed.
//
// A command that must be signalled cannot be driven by run: it does not exit on
// its own, so a test that waited for it would wait for the package timeout. The
// settle delay is the caller's, because only the caller knows how long the
// command's first unit of work takes.
func (s *sandbox) runUntilSignal(t *testing.T, settle time.Duration, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, ghostBin, args...)
	cmd.Env = s.env
	cmd.Dir = s.work
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Start(); err != nil {
		t.Fatalf("ghost %s: %v", strings.Join(args, " "), err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// It finished on its own, which is a legitimate outcome for a command
		// that only sometimes polls; the caller asserts on the output either way.
		return result{stdout: out.String(), stderr: errb.String(), code: exitCode(err)}
	case <-time.After(settle):
	}

	// SIGINT first, because that is what the product installs a handler for
	// (signal.NotifyContext on SIGINT and SIGTERM) and a polite stop is the
	// behaviour under test. SIGKILL is the backstop for a process that ignored
	// it, so a bug cannot hang the suite.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal %s: %v", strings.Join(args, " "), err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("ghost %s ignored SIGINT for 10s", strings.Join(args, " "))
	}
	return result{stdout: out.String(), stderr: errb.String()}
}

// exitCode extracts a process exit code from a wait error, with 0 for success.
func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if asExitError(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

// countInFile runs a count query against a database file the product wrote. It
// opens read-only, so inspecting a backup cannot modify it.
func countInFile(t *testing.T, path, query string) int {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("query %q against %s: %v", query, path, err)
	}
	return n
}

// countInFileWhere is countInFile with bind arguments, for the assertions that
// scope a count to one project — a store holds the builtin global memories too,
// and counting them makes a "nothing was lost" check meaningless.
func countInFileWhere(t *testing.T, path, query string, args ...any) int {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	defer db.Close() //nolint:errcheck
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q against %s: %v", query, path, err)
	}
	return n
}

// stringsInFile runs a one-column text query against a database file.
func stringsInFile(t *testing.T, path, query string, args ...any) []string {
	t.Helper()
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open %s read-only: %v", path, err)
	}
	defer db.Close() //nolint:errcheck
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q against %s: %v", query, path, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	return out
}

// countNotes counts the Markdown notes under a vault, skipping the directories.
// It is how the obsidian checks compare a run against the run before it without
// depending on the file layout the exporter chooses internally.
func countNotes(t *testing.T, vault string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(vault, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", vault, err)
	}
	return n
}

// resolveSymlinks returns path with symlinks resolved, which is the form the
// product records for a bound checkout.
func resolveSymlinks(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	return resolved
}

// jsonUnmarshal is a thin alias so test files do not each import encoding/json
// for one call.
func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// jsonMarshalIndent is the matching alias, for the tests that edit a config
// file the product wrote.
func jsonMarshalIndent(v any) ([]byte, error) { return json.MarshalIndent(v, "", "  ") }

// writeTemp writes body to a file inside the sandbox's temp dir and returns the
// path. It exists for the one place a test needs a file on disk to feed an
// existing file-reading helper, rather than duplicating that helper.
func writeTemp(t *testing.T, s *sandbox, name, body string) string {
	t.Helper()
	path := filepath.Join(s.tmp, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

// bytesContain reports whether haystack holds needle.
func bytesContain(haystack []byte, needle string) bool {
	return strings.Contains(string(haystack), needle)
}

// countOccurrences counts non-overlapping occurrences of sub in s.
func countOccurrences(s, sub string) int {
	if sub == "" {
		return -1
	}
	return strings.Count(s, sub)
}

// hasAnyKey reports whether m carries at least one of the named keys.
func hasAnyKey(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// snapshotStore returns a single string describing the store's row counts, so a
// test can assert that a read-only command wrote nothing. The tables are the
// ones a status run could plausibly touch.
func snapshotStore(t *testing.T, s *sandbox) string {
	t.Helper()
	parts := make([]string, 0, 4)
	for _, table := range []string{"projects", "memories", "tasks", "decisions"} {
		parts = append(parts, fmt.Sprintf("%s=%d", table, s.queryInt(t, "SELECT COUNT(*) FROM "+table)))
	}
	return strings.Join(parts, " ")
}

// memoryWithContent returns the id of the single project memory whose content
// contains want. It is how a test names a memory by what it SAYS rather than by
// an id captured from a save call — which matters wherever the product orders
// rows by timestamp rather than by insertion, so that "the second one I saved"
// is not the same statement as "the newer one".
func memoryWithContent(t *testing.T, s *sandbox, want string) string {
	t.Helper()
	ids := s.queryStrings(t, `SELECT id FROM memories WHERE project_id = ? AND content LIKE ?`,
		e2eProject, "%"+want+"%")
	if len(ids) != 1 {
		t.Fatalf("%d memories in %s contain %q, want exactly 1", len(ids), e2eProject, want)
	}
	return ids[0]
}

// sessionCounterLine is the "**Session #N** with this project." line a context
// render emits, as a pattern.
var sessionCounterLine = regexp.MustCompile(`(?m)^\*\*Session #\d+\*\* with this project\.$`)

// stripSessionCounter removes the session-counter line from a rendered context
// block. A context render counts itself as a session, so two renders of the same
// state differ by exactly that number — which makes a byte comparison of two
// renders a test of the counter rather than of whatever the caller meant to
// compare.
func stripSessionCounter(block string) string {
	return sessionCounterLine.ReplaceAllString(block, "")
}

// mustNotExist fails the test if path exists.
func mustNotExist(t *testing.T, what, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s: %s exists", what, path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("%s: stat %s: %v", what, path, err)
	}
}

// sortedKeys is a small helper for the diffs several tests print.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// parseJSONLines decodes a JSONL artifact into its records, for the
// export/import round trip.
func parseJSONLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	b := mustReadFile(t, path)
	var out []map[string]any
	for i, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("%s line %d is not JSON: %v", path, i+1, err)
		}
		out = append(out, rec)
	}
	return out
}

// countRecords counts the records of one type in a JSONL artifact.
func countRecords(records []map[string]any, kind string) int {
	n := 0
	for _, r := range records {
		if r["type"] == kind {
			n++
		}
	}
	return n
}

// countRecordsFor counts the records of one type that belong to one project. An
// artifact holds the whole store, including the builtin global memories the
// store seeds itself, so a test asserting on its own data has to scope the
// count or it is counting Ghost's seeds.
func countRecordsFor(records []map[string]any, kind, project string) int {
	n := 0
	for _, r := range records {
		if r["type"] != kind {
			continue
		}
		payload, ok := r[kind].(map[string]any)
		if !ok {
			continue
		}
		if got, _ := payload["project_id"].(string); got == project {
			n++
		}
	}
	return n
}

// firstRecordOf returns the first record of one type carrying the given
// memory/task/decision id, decoded into out.
func firstRecordOf(t *testing.T, records []map[string]any, kind, idKey, id string, out any) bool {
	t.Helper()
	for _, r := range records {
		if r["type"] != kind {
			continue
		}
		payload, ok := r[kind].(map[string]any)
		if !ok {
			continue
		}
		if got, _ := payload[idKey].(string); got != id {
			continue
		}
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("marshal %s record: %v", kind, err)
		}
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("decode %s record: %v", kind, err)
		}
		return true
	}
	return false
}

// parseID pulls a memory id out of a tool's text answer. The save-family
// answers all lead with "Memory saved (id: <hex>)", so the id is read from
// that field rather than from the first word — which would be "Memory".
func parseID(t *testing.T, text string) string {
	t.Helper()
	const marker = "id: "
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("no %q field in the answer: %s", strings.TrimSpace(marker), text)
	}
	rest := text[i+len(marker):]
	end := strings.IndexAny(rest, " )\n\t")
	if end < 0 {
		end = len(rest)
	}
	candidate := rest[:end]
	if !looksLikeID(candidate) {
		t.Fatalf("id field %q is not an id: %s", candidate, text)
	}
	return candidate
}

// looksLikeID reports whether s has the shape of a Ghost id: hex, or hex with
// dashes (a UUID).
func looksLikeID(s string) bool {
	if s == "" {
		return false
	}
	body := strings.ReplaceAll(s, "-", "")
	if len(body) < 8 {
		return false
	}
	for _, r := range body {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}
