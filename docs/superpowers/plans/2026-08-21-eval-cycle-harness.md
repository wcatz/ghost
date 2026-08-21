# Eval Cycle Harness Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Repeatable harness that injects an annotated corpus into a scratch-isolated ghost instance, runs supersede → resolve → reflect, grades each stage against annotations, and writes a Markdown scorecard — all LLM calls billed through opencode/claude CLI, zero Anthropic API spend.

**Architecture:** In-repo main package `eval/cycle` (mirrors `bench/longmemeval`). Runner builds the working-tree ghost binary, points `XDG_DATA_HOME`/`XDG_CONFIG_HOME` into a throwaway temp dir, injects via a real `ghost mcp` stdio session (go-sdk client), gates on embedding drain, then runs each stage once with `--apply`, parsing the same output it grades. Spec: `docs/superpowers/specs/2026-08-21-eval-cycle-harness-design.md`.

**Tech Stack:** Go 1.26, modernc.org/sqlite (driver name `sqlite`), github.com/modelcontextprotocol/go-sdk v1.7.0 (`mcp.CommandTransport` + `Client.Connect`), stdlib regexp/text for parsers and report.

## Global Constraints

- Every child process gets `XDG_DATA_HOME=<scratch>/data`, `XDG_CONFIG_HOME=<scratch>/config`, and **no** `ANTHROPIC_API_KEY`.
- Valid memory categories: architecture, decision, pattern, convention, gotcha, dependency, preference, fact.
- CLI prints 8-char lowercase hex id prefixes; corpus keys map back via ids captured at injection.
- `go vet ./...` clean and `go test ./...` green before every commit.
- Module path: `github.com/wcatz/ghost`.

---

### Task 1: opencode fallback for resolve/supersede classifiers

**Files:**
- Modify: `cmd/ghost/main.go` (`buildClassifyProvider`, ~line 575)
- Test: `cmd/ghost/main_test.go`

**Interfaces:**
- Consumes: `ai.NewCLIProviderWithBinaries(claudeBinary, opencodeBinary string) *ai.CLIProvider` (has `.Available() bool`, satisfies `ai.Provider`), `ai.NewFallbackProvider(primary, secondary Provider, secondaryIsDryRunOnly bool)`.
- Produces: unchanged signature `buildClassifyProvider(cfg *config.Config, logger *slog.Logger) (*ai.FallbackProvider, error)`; new behavior: no key + no claude but opencode resolvable ⇒ provider works via opencode.

- [ ] **Step 1: Write failing tests**

Append to `cmd/ghost/main_test.go`:

```go
func stubBinary(t *testing.T, payload string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fakeopencode")
	script := "#!/bin/sh\nprintf '%s\\n' '" + payload + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	return path
}

func TestBuildClassifyProvider_NoKeyNoBackendErrors(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // empty dir: no claude/opencode anywhere
	cfg := &config.Config{}
	_, err := buildClassifyProvider(cfg, slogNewDiscard())
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("want backend error, got %v", err)
	}
}

func TestBuildClassifyProvider_NoKeyFallsToOpencodeStub(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	stub := stubBinary(t, `{"type":"text","part":{"type":"text","text":"STUB_CLASSIFY_OK"}}`)
	cfg := &config.Config{}
	cfg.CLI.OpenCodeBinary = stub
	p, err := buildClassifyProvider(cfg, slogNewDiscard())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	out, err := p.Classify(context.Background(), "sys prompt", "user content")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if out != "STUB_CLASSIFY_OK" {
		t.Fatalf("got %q", out)
	}
}

func TestBuildClassifyProvider_KeySetBuildsRegardless(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg := &config.Config{}
	cfg.API.Key = "sk-test"
	p, err := buildClassifyProvider(cfg, slogNewDiscard())
	if err != nil || p == nil {
		t.Fatalf("build: p=%v err=%v", p, err)
	}
}
```

plus helper `func slogNewDiscard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }` (skip if an equivalent already exists in the file).

Note: PATH pointing at an empty temp dir defeats bare-name LookPath while absolute-path stubs still resolve. If `config.Config` field names differ (`cfg.CLI.OpenCodeBinary` / `cfg.API.Key`), match the struct.

- [ ] **Step 2: Run tests, verify fail**

Run: `go test ./cmd/ghost/ -run TestBuildClassifyProvider -v`
Expected: FAIL (opencode-stub case errors with "requires ANTHROPIC_API_KEY").

- [ ] **Step 3: Implement**

Replace body of `buildClassifyProvider`:

```go
func buildClassifyProvider(cfg *config.Config, logger *slog.Logger) (*ai.FallbackProvider, error) {
	cli := ai.NewCLIProviderWithBinaries(cfg.CLI.ClaudeBinary, cfg.CLI.OpenCodeBinary)
	if cfg.API.Key == "" {
		if !cli.Available() {
			return nil, errors.New("requires ANTHROPIC_API_KEY or a `claude`/`opencode` binary on PATH")
		}
		return ai.NewFallbackProvider(cli, nil, false), nil
	}
	primary := ai.NewAnthropicProvider(ai.NewClient(cfg.API.Key, logger))
	if !cli.Available() {
		return ai.NewFallbackProvider(primary, nil, false), nil
	}
	return ai.NewFallbackProvider(primary, cli, true), nil
}
```

Behavior preserved when key set (CLIProvider prefers claude exactly as before). Update the stale doc comment above the function to mention opencode.

- [ ] **Step 4: Tests pass + full suite**

Run: `go test ./cmd/ghost/ -run TestBuildClassifyProvider -v && go vet ./... && go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/ghost/main.go cmd/ghost/main_test.go
git commit -m "feat(classify): fall back to claude-or-opencode CLI when no API key set"
```

---

### Task 2: GHOST_OPENCODE_MODEL pinning

**Files:**
- Modify: `internal/ai/opencode_client.go`
- Test: `internal/ai/opencode_client_test.go`

**Interfaces:**
- Produces: `OpenCodeClient` honors env `GHOST_OPENCODE_MODEL`; when set (non-empty), `run` appends `-m <model>` between `--pure` and the prompt.

- [ ] **Step 1: Failing test**

Add to `internal/ai/opencode_client_test.go` (reuse existing fake-opencode script helpers there if present; else create):

```go
func TestOpenCodeClient_ModelFlagFromEnv(t *testing.T) {
	dir := t.TempDir()
	echoArgs := filepath.Join(dir, "fakeoc")
	script := "#!/bin/sh\nprintf '{\"type\":\"text\",\"part\":{\"type\":\"text\",\"text\":\"%s\"}}\\n' \"$*\"\n"
	if err := os.WriteFile(echoArgs, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHOST_OPENCODE_MODEL", "deepseek/deepseek-v4")
	c := NewOpenCodeClientWithBinary(echoArgs)
	out, err := c.Classify(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if !strings.Contains(out, "-m deepseek/deepseek-v4") || !strings.Contains(out, "--pure") {
		t.Fatalf("model flag not passed: %q", out)
	}
}

func TestOpenCodeClient_NoEnvNoFlag(t *testing.T) {
	t.Setenv("GHOST_OPENCODE_MODEL", "")
	// same echo stub; assert output does NOT contain "-m"
}
```

- [ ] **Step 2: Verify fail** — Run: `go test ./internal/ai/ -run TestOpenCodeClient_Model -v`. Expected: FAIL (no `-m` in args).

- [ ] **Step 3: Implement**

In `OpenCodeClient.run`, replace the args construction:

```go
	args := []string{"run", "--format", "json", "--pure"}
	if model := os.Getenv("GHOST_OPENCODE_MODEL"); model != "" {
		args = append(args, "-m", model)
	}
	args = append(args, prompt)
```

Document the env var in the type doc comment.

- [ ] **Step 4: Pass + suite.** Run: `go test ./internal/ai/ -v && go vet ./... && go test ./...`

- [ ] **Step 5: Commit** — `git commit -m "feat(ai): honor GHOST_OPENCODE_MODEL to pin the opencode subprocess model"`

---

### Task 3: corpus loader package

**Files:**
- Create: `eval/cycle/corpus/load.go`
- Test: `eval/cycle/corpus/load_test.go`

**Interfaces:**
- Produces: `Entry{Key, Category, Content string; Importance float32; Tags []string; ExpectedSupersededBy string; ExpectedResolved bool; MergeGroup string}` (JSON tags: `key, category, content, importance, tags, expected_superseded_by, expected_resolved, merge_group`), `(e Entry) TagsOrEmpty() []string`, `Load(path string) ([]Entry, error)`, `Validate(entries []Entry) error`.

- [ ] **Step 1: Failing tests** — table covering: happy load of mixed annotated entries (# comments + blank lines skipped), duplicate key rejection, Validate errors for unknown `expected_superseded_by`, lone merge group (<2 members), invalid category, empty key/content.

```go
package corpus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validJSONL = `# comment line
{"key":"a","category":"fact","content":"alpha","importance":0.7,"expected_resolved":true}
{"key":"b","category":"gotcha","content":"beta","importance":0.8,"expected_superseded_by":"c"}

{"key":"c","category":"fact","content":"gamma newer","importance":0.7}
{"key":"d1","category":"convention","content":"merge one","importance":0.6,"merge_group":"g"}
{"key":"d2","category":"fact","content":"merge two","importance":0.6,"merge_group":"g"}
`

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "corpus.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadMixedAnnotations(t *testing.T) {
	entries, err := Load(writeTemp(t, validJSONL))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(entries) != 5 {
		t.Fatalf("want 5, got %d", len(entries))
	}
	if !entries[0].ExpectedResolved || entries[1].ExpectedSupersededBy != "c" {
		t.Fatalf("annotations lost: %+v", entries[:2])
	}
	if entries[0].TagsOrEmpty() == nil {
		t.Fatal("TagsOrEmpty must never be nil")
	}
}

func TestLoadDuplicateKeyRejected(t *testing.T) {
	dup := `{"key":"x","category":"fact","content":"one","importance":0.7}
{"key":"x","category":"fact","content":"two","importance":0.7}`
	if _, err := Load(writeTemp(t, dup)); err == nil || !strings.Contains(err.Error(), "duplicate key") {
		t.Fatalf("want duplicate key error, got %v", err)
	}
}

func TestValidateOK(t *testing.T) {
	entries, _ := Load(writeTemp(t, validJSONL))
	if err := Validate(entries); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

func TestValidateUnknownSupersedeRef(t *testing.T) {
	bad := `{"key":"a","category":"fact","content":"x","importance":0.7,"expected_superseded_by":"missing"}`
	entries, _ := Load(writeTemp(t, bad))
	err := Validate(entries)
	if err == nil || !strings.Contains(err.Error(), "not in corpus") {
		t.Fatalf("want ref error, got %v", err)
	}
}

func TestValidateLoneMergeGroupAndBadCategory(t *testing.T) {
	lone := `{"key":"a","category":"fact","content":"x","importance":0.7,"merge_group":"solo"}`
	entries, _ := Load(writeTemp(t, lone))
	if err := Validate(entries); err == nil || !strings.Contains(err.Error(), "need >=2") {
		t.Fatalf("want group-size error, got %v", err)
	}
	badcat := `{"key":"a","category":"nonsense","content":"x","importance":0.7}`
	entries2, _ := Load(writeTemp(t, badcat))
	if err := Validate(entries2); err == nil || !strings.Contains(err.Error(), "invalid category") {
		t.Fatalf("want category error, got %v", err)
	}
}
```

- [ ] **Step 2: Verify fail** — `go test ./eval/cycle/corpus/ -v` → build failure (package absent).

- [ ] **Step 3: Implement `load.go`**

```go
// Package corpus loads and validates the annotated eval-cycle memory corpus.
// Annotations ride alongside normal save fields but are harness-only.
package corpus

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Entry is one corpus memory plus optional grading annotations.
type Entry struct {
	Key                  string   `json:"key"`
	Category             string   `json:"category"`
	Content              string   `json:"content"`
	Importance           float32  `json:"importance"`
	Tags                 []string `json:"tags,omitempty"`
	ExpectedSupersededBy string   `json:"expected_superseded_by,omitempty"` // on the OLDER member; value is the newer key
	ExpectedResolved     bool     `json:"expected_resolved,omitempty"`
	MergeGroup           string   `json:"merge_group,omitempty"`
}

// TagsOrEmpty returns e.Tags, never nil (MCP save requires a JSON array).
func (e Entry) TagsOrEmpty() []string {
	if e.Tags == nil {
		return []string{}
	}
	return e.Tags
}

var validCategories = map[string]bool{
	"architecture": true, "decision": true, "pattern": true, "convention": true,
	"gotcha": true, "dependency": true, "preference": true, "fact": true,
}

// Load reads entries from JSONL, skipping blank and '#' comment lines.
func Load(path string) ([]Entry, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	var out []Entry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	line := 0
	seen := map[string]bool{}
	for sc.Scan() {
		line++
		b := strings.TrimSpace(sc.Text())
		if b == "" || strings.HasPrefix(b, "#") {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(b), &e); err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		if seen[e.Key] {
			return nil, fmt.Errorf("line %d: duplicate key %q", line, e.Key)
		}
		seen[e.Key] = true
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Validate checks cross-entry integrity so malformed corpora fail loudly
// before any LLM spend.
func Validate(entries []Entry) error {
	keys := make(map[string]bool, len(entries))
	for i, e := range entries {
		if e.Key == "" || e.Content == "" {
			return fmt.Errorf("entry %d: key and content are required", i)
		}
		if !validCategories[e.Category] {
			return fmt.Errorf("%s: invalid category %q", e.Key, e.Category)
		}
		keys[e.Key] = true
	}
	for _, e := range entries {
		if e.ExpectedSupersededBy != "" && !keys[e.ExpectedSupersededBy] {
			return fmt.Errorf("%s: expected_superseded_by %q not in corpus", e.Key, e.ExpectedSupersededBy)
		}
	}
	groups := map[string]int{}
	for _, e := range entries {
		if e.MergeGroup != "" {
			groups[e.MergeGroup]++
		}
	}
	for g, n := range groups {
		if n < 2 {
			return fmt.Errorf("merge group %q has %d members; need >=2", g, n)
		}
	}
	return nil
}
```

- [ ] **Step 4: Pass + suite.** `go test ./eval/cycle/corpus/ -v && go vet ./...`
- [ ] **Step 5: Commit** — `feat(eval): annotated corpus loader for eval-cycle harness`

---

### Task 4: corpus data — acme-migration, 55 annotated memories

**Files:**
- Create: `eval/cycle/corpus/corpus.jsonl`
- Test: extend `eval/cycle/corpus/load_test.go` with a golden test over the committed file.

**Interfaces:**
- Produces: dataset consumed by Task 5–8 via `corpus.Load`; composition contract asserted by `TestCommittedCorpusShape`: 55 entries, 8 `expected_superseded_by`, 12 `expected_resolved`, groups `staging_api_base|redis_maxmemory|bastion_ssh_port` × 3 members, 18 unannotated distractors.

- [ ] **Step 1: Golden shape test (fails until Task Step 2 writes data)**

```go
func TestCommittedCorpusShape(t *testing.T) {
	entries, err := Load(filepath.Join("..", "..", "eval", "cycle", "corpus", "corpus.jsonl"))
	if err != nil {
		t.Skipf("committed corpus not present yet: %v", err)
	}
	if err := Validate(entries); err != nil {
		t.Fatalf("validate committed corpus: %v", err)
	}
	sup, res, grouped := 0, 0, map[string]int{}
	for _, e := range entries {
		switch {
		case e.ExpectedSupersededBy != "":
			sup++
		case e.ExpectedResolved:
			res++
		}
		if e.MergeGroup != "" {
			grouped[e.MergeGroup]++
		}
	}
	if sup != 8 || res != 12 {
		t.Fatalf("composition drift: supersedes=%d resolved=%d", sup, res)
	}
	if len(grouped) != 3 {
		t.Fatalf("want 3 merge groups, got %d (%v)", len(grouped), grouped)
	}
	distractors := len(entries) - sup - res
	for _, n := range grouped {
		distractors -= n
	}
	if distractors != 18 {
		t.Fatalf("want 18 distractors, got %d", distractors)
	}
}
```

(Path note: test lives IN eval/cycle/corpus, so use `filepath.Join("corpus.jsonl")` directly — adjust to plain `"corpus.jsonl"`.)

- [ ] **Step 2: Write `corpus.jsonl`** — full 55-line dataset (verbatim block below; header comments explain annotation fields).

Composition: 8 supersede pairs — deploy_target, db_host, app_port, ci_runner, tls_terminator, backup_tool, queue_transport, primary_region. Resolved evidence ×12 — spikes/changelogs/postmortem/cost estimate/locators. Merge groups ×3×3 — staging_api_base, redis_maxmemory, bastion_ssh_port. Distractors ×18 — conventions(4), preferences(2), architecture(4), gotchas(4), dependencies(4).

```jsonl
# eval-cycle corpus v1 — fake project "acme-migration" (Fly.io → Hetzner migration storyline).
# Annotations (harness-only): expected_superseded_by (on OLDER member; value=newer key),
# expected_resolved (resolved-evidence), merge_group (near-duplicate cluster).
# Composition: 8 supersede pairs (16), resolved evidence (12), merge groups (3x3=9), distractors (18).
{"key":"deploy_target_old","category":"architecture","content":"Orchestrator deploys to Fly.io: two shared-cpu-1x machines in iad, fly.toml is the source of truth for the release process.","importance":0.8,"tags":["acme-migration","deploy"],"expected_superseded_by":"deploy_target_hetzner"}
{"key":"deploy_target_hetzner","category":"architecture","content":"Orchestrator now deploys to Hetzner CX32 VMs via Docker Compose; ops/deploy.sh on the bastion is the single entrypoint and fly.toml is retired.","importance":0.8,"tags":["acme-migration","deploy","hetzner"]}
{"key":"db_host_old","category":"fact","content":"Postgres runs as a Fly Postgres app reachable at orchestrator-db.internal, snapshots managed by the platform.","importance":0.7,"tags":["acme-migration","postgres"],"expected_superseded_by":"db_host_compose"}
{"key":"db_host_compose","category":"fact","content":"Postgres moved into the stack: postgres:16 container in compose, data under /srv/orchestrator/pgdata on the Hetzner volume.","importance":0.7,"tags":["acme-migration","postgres","compose"]}
{"key":"app_port_old","category":"fact","content":"The Orchestrator HTTP listener binds :8080 and Fly's edge proxy handles TLS termination in front of it.","importance":0.7,"tags":["acme-migration","network"],"expected_superseded_by":"app_port_caddy"}
{"key":"app_port_caddy","category":"fact","content":"Since Caddy landed, the app binds :8081 on the tailscale interface only; Caddy terminates TLS and fronts everything public.","importance":0.7,"tags":["acme-migration","network","caddy"]}
{"key":"ci_runner_old","category":"convention","content":"CI runs on GitHub-hosted ubuntu-latest runners; deploy jobs gate on an environment approval in the GH UI.","importance":0.6,"tags":["acme-migration","ci"],"expected_superseded_by":"ci_runner_selfhosted"}
{"key":"ci_runner_selfhosted","category":"convention","content":"Deploys run on a self-hosted GitHub runner on the Hetzner bastion; environment approvals were dropped since only main reaches prod.","importance":0.6,"tags":["acme-migration","ci"]}
{"key":"tls_terminator_old","category":"fact","content":"TLS certs for orchestrator.acme.dev come from Fly's built-in Let's Encrypt integration; renewals are automatic and config-free.","importance":0.6,"tags":["acme-migration","tls"],"expected_superseded_by":"tls_caddy_auto"}
{"key":"tls_caddy_auto","category":"fact","content":"Caddy issues and renews the orchestrator.acme.dev certificate automatically; cert config lives only in caddy/Caddyfile.","importance":0.6,"tags":["acme-migration","tls","caddy"]}
{"key":"backup_tool_old","category":"pattern","content":"Database backups rely on Fly volume snapshots taken nightly by the platform scheduler; restore is a support-ticket procedure.","importance":0.7,"tags":["acme-migration","backup"],"expected_superseded_by":"backup_restic_b2"}
{"key":"backup_restic_b2","category":"pattern","content":"Backups are nightly restic snapshots of pgdump output pushed to B2 bucket acme-orch-backups at 03:00 UTC; restore script is ops/restore.sh.","importance":0.7,"tags":["acme-migration","backup"]}
{"key":"queue_transport_old","category":"architecture","content":"The job queue is Redis lists (LPUSH/BRPOP) driven by queue/consumer.go; retries are manual requeues by whoever notices.","importance":0.7,"tags":["acme-migration","queue"],"expected_superseded_by":"queue_redis_streams"}
{"key":"queue_redis_streams","category":"architecture","content":"Queue migrated to Redis Streams consumer groups (XADD/XREADGROUP); pending entries auto-retry after 60s idle via XAUTOCLAIM.","importance":0.7,"tags":["acme-migration","queue"]}
{"key":"primary_region_old","category":"fact","content":"All production workloads live in region iad for proximity to the US user base; there is no DR story.","importance":0.6,"tags":["acme-migration","regions"],"expected_superseded_by":"region_fsn1"}
{"key":"region_fsn1","category":"fact","content":"Production now runs in Hetzner fsn1; EU latency improved ~40ms and US traffic is served behind Cloudflare from the same box.","importance":0.6,"tags":["acme-migration","regions"]}
{"key":"spike_gc_p99","category":"fact","content":"Investigation note: the 07-14 p99 spike traced to GC pressure from 256MB parse buffers; fixed by streaming parsing in PR #412 (merged). History only.","importance":0.5,"tags":["acme-migration","investigation"],"expected_resolved":true}
{"key":"fix_psql_conn_leak_changelog","category":"gotcha","content":"Changelog: pgx pool connection leak fixed in v0.9.3 (PR #398); root cause was missing Release on the error path. Concluded work.","importance":0.5,"tags":["acme-migration","changelog"],"expected_resolved":true}
{"key":"cost_estimate_fly","category":"fact","content":"Cost estimate from May: Fly.io projected $148/mo at current traffic. Kept for the migration decision record; actuals have since replaced it.","importance":0.4,"tags":["acme-migration","cost"],"expected_resolved":true}
{"key":"spike_nats_eval","category":"dependency","content":"Closed spike: evaluated NATS JetStream as queue replacement; rejected for single-node ops burden. Conclusion captured in the streams migration.","importance":0.4,"tags":["acme-migration","spike"],"expected_resolved":true}
{"key":"spike_tailscale_eval","category":"pattern","content":"Closed spike: compared raw WireGuard vs Tailscale for the admin mesh; picked Tailscale. Setup notes preserved for provenance.","importance":0.4,"tags":["acme-migration","spike"],"expected_resolved":true}
{"key":"postmortem_0709_deploy_fail","category":"gotcha","content":"Postmortem (concluded): the 07-09 deploy failure was a stale compose hash on the bastion; mitigated by a git-pull guard in deploy.sh. No open actions.","importance":0.5,"tags":["acme-migration","postmortem"],"expected_resolved":true}
{"key":"fix_oom_worker_changelog","category":"fact","content":"Changelog: worker OOM fixed in v0.9.1 by bounding the batch channel to 64. Issue closed, no follow-ups.","importance":0.4,"tags":["acme-migration","changelog"],"expected_resolved":true}
{"key":"estimate_downtime_window","category":"fact","content":"Migration plan estimated a 20-minute downtime window for final cutover; cutover completed 07-19 in 14 minutes actual.","importance":0.4,"tags":["acme-migration","migration"],"expected_resolved":true}
{"key":"pr_compose_split_locator","category":"fact","content":"PR locator: compose file split into base+prod overlays landed in PR #405 (merged). Reference only.","importance":0.4,"tags":["acme-migration"],"expected_resolved":true}
{"key":"investigation_slow_drain","category":"fact","content":"Investigation note: slow queue drain was a missing XACK after a handler panic; fixed in v0.9.4 with a deferred-ack wrapper. Concluded.","importance":0.5,"tags":["acme-migration","investigation"],"expected_resolved":true}
{"key":"benchmark_caddy_nginx","category":"fact","content":"Benchmark note (closed): Caddy vs nginx TLS renewal automation; Caddy won on zero-config renewals. Decision recorded elsewhere.","importance":0.4,"tags":["acme-migration","benchmark"],"expected_resolved":true}
{"key":"changelog_seed_importer","category":"fact","content":"Changelog: the seed-data importer shipped in v0.9.0; feature complete per epic ACME-120 closure notes.","importance":0.4,"tags":["acme-migration","changelog"],"expected_resolved":true}
{"key":"staging_api_base_a","category":"fact","content":"Staging API base URL is https://staging.acme.dev/api — set STAGING_API_BASE before running integration tests.","importance":0.6,"tags":["acme-migration","staging"],"merge_group":"staging_api_base"}
{"key":"staging_api_base_b","category":"fact","content":"Integration tests point at STAGING_API_BASE=https://staging.acme.dev/api.","importance":0.6,"tags":["acme-migration","staging"],"merge_group":"staging_api_base"}
{"key":"staging_api_base_c","category":"convention","content":"For integration runs, export STAGING_API_BASE=https://staging.acme.dev/api first.","importance":0.6,"tags":["acme-migration","staging"],"merge_group":"staging_api_base"}
{"key":"redis_maxmemory_a","category":"gotcha","content":"Redis maxmemory must stay at 512mb on prod or the OOM killer reaps it during batch windows.","importance":0.8,"tags":["acme-migration","redis"],"merge_group":"redis_maxmemory"}
{"key":"redis_maxmemory_b","category":"gotcha","content":"Keep redis maxmemory at 512mb on the prod box; raising it invites OOM kills under batch load.","importance":0.8,"tags":["acme-migration","redis"],"merge_group":"redis_maxmemory"}
{"key":"redis_maxmemory_c","category":"fact","content":"Prod Redis is capped at maxmemory 512mb.","importance":0.7,"tags":["acme-migration","redis"],"merge_group":"redis_maxmemory"}
{"key":"bastion_ssh_port_a","category":"fact","content":"SSH to the Hetzner bastion goes through port 2222, not 22.","importance":0.7,"tags":["acme-migration","bastion"],"merge_group":"bastion_ssh_port"}
{"key":"bastion_ssh_port_b","category":"fact","content":"Bastion sshd listens on 2222.","importance":0.7,"tags":["acme-migration","bastion"],"merge_group":"bastion_ssh_port"}
{"key":"bastion_ssh_port_c","category":"convention","content":"Use `ssh -p 2222 ops@bastion.acme.dev` for all bastion access.","importance":0.6,"tags":["acme-migration","bastion"],"merge_group":"bastion_ssh_port"}
{"key":"conv_commit_style","category":"convention","content":"Commits follow Conventional Commits (feat:/fix:/chore:); squash-merge only.","importance":0.6,"tags":["acme-migration","conventions"]}
{"key":"conv_branch_naming","category":"convention","content":"Branches are named <type>/<ticket>-<slug>, e.g. feat/ACME-142-compose-split.","importance":0.6,"tags":["acme-migration","conventions"]}
{"key":"conv_pr_labels","category":"convention","content":"Every PR carries exactly one of: backend, infra, docs, tooling — CI routes checks off the label.","importance":0.6,"tags":["acme-migration","conventions"]}
{"key":"conv_codeowners","category":"convention","content":"infra/** is owned by @acme/platform and service code by @acme/backend; CODEOWNERS is enforced.","importance":0.6,"tags":["acme-migration","conventions"]}
{"key":"pref_short_prs","category":"preference","content":"Wayne prefers PRs under ~400 changed lines; split bigger changes into stacks.","importance":0.5,"tags":["acme-migration","preferences"]}
{"key":"pref_rebase_no_merge","category":"preference","content":"Feature branches get rebased onto main before review — no merge commits.","importance":0.5,"tags":["acme-migration","preferences"]}
{"key":"arch_service_boundaries","category":"architecture","content":"Orchestrator splits into ingest, scheduler, and billing-api services; shared types live in internal/contract.","importance":0.8,"tags":["acme-migration","architecture"]}
{"key":"arch_event_topics","category":"architecture","content":"Event bus topics follow <domain>.<entity>.<verb>, e.g. billing.invoice.finalized.","importance":0.7,"tags":["acme-migration","architecture"]}
{"key":"arch_schema_owner","category":"architecture","content":"Schema migrations are owned by Orchestrator itself via goose, never applied out-of-band.","importance":0.7,"tags":["acme-migration","architecture"]}
{"key":"arch_tailscale_admin","category":"architecture","content":"Admin surfaces (metrics, pprof) are reachable over Tailscale only, never the public internet.","importance":0.8,"tags":["acme-migration","architecture"]}
{"key":"gotcha_systemd_restart","category":"gotcha","content":"systemd Restart=always masks crash loops caused by config errors — check journalctl before trusting unit health.","importance":0.8,"tags":["acme-migration","gotchas"]}
{"key":"gotcha_docker_tty","category":"gotcha","content":"docker compose exec without -T fails inside CI pipes ('the input device is not a TTY'); always pass -T non-interactively.","importance":0.8,"tags":["acme-migration","gotchas"]}
{"key":"gotcha_hetzner_ipv6","category":"gotcha","content":"Hetzner CX VMs are dual-stack but the private network is IPv4-only; service discovery must not advertise public v6 addresses.","importance":0.8,"tags":["acme-migration","gotchas"]}
{"key":"gotcha_pg_buffers","category":"gotcha","content":"postgres shared_buffers=256MB is tuned for the CX32's 4GB RAM; raising it without resizing causes swap thrash.","importance":0.8,"tags":["acme-migration","postgres"]}
{"key":"dep_postgres16","category":"dependency","content":"Postgres pinned to major 16 (compose tag postgres:16-alpine).","importance":0.6,"tags":["acme-migration","versions"]}
{"key":"dep_redis72","category":"dependency","content":"Redis pinned to 7.2.x (compose tag redis:7.2-alpine).","importance":0.6,"tags":["acme-migration","versions"]}
{"key":"dep_caddy28","category":"dependency","content":"Caddy pinned to 2.8.x; Caddyfile syntax is stable across the line.","importance":0.6,"tags":["acme-migration","versions"]}
{"key":"dep_restic017","category":"dependency","content":"restic 0.17.x on the bastion; repo configured as b2:acme-orch-backups.","importance":0.6,"tags":["acme-migration","versions"]}
```

- [ ] **Step 3: Golden test passes.** `go test ./eval/cycle/corpus/ -v`
- [ ] **Step 4: Commit** — `feat(eval): acme-migration annotated corpus (55 memories)`

---

### Task 5: runner core — isolation, injection, drain gate

**Files:**
- Create: `eval/cycle/main.go`, `eval/cycle/inject.go`
- Test: `eval/cycle/main_test.go` (scratchEnv only; MCP path exercised end-to-end in Task 9)

**Interfaces:**
- Consumes: `corpus.Load`, `corpus.Validate`, `corpus.Entry`.
- Produces:
  - `scratchEnv(scratch string) []string` — inherited env minus `XDG_DATA_HOME/XDG_CONFIG_HOME/ANTHROPIC_API_KEY`, plus scratch XDG pair.
  - `startMCP(ctx, ghostBin string, env []string) (*mcpSession, error)`; `(*mcpSession) callText(ctx, tool string, args map[string]any) (string, error)`; `saveAll(ctx, s, project string, entries []corpus.Entry) (map[string]string /*key→full id*/, error)`.
  - `waitForEmbeddings(ctx, ollamaURL, dbPath, projectName string, timeout time.Duration) error`.
  - `runGhost(ctx, env, bin string, args ...string) (string, error)`.

- [ ] **Step 1: Failing env test**

```go
func TestScratchEnvIsolates(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/leak/data")
	t.Setenv("XDG_CONFIG_HOME", "/leak/config")
	t.Setenv("ANTHROPIC_API_KEY", "sk-leak")
	env := scratchEnv("/scratch")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "/leak/") || strings.Contains(joined, "ANTHROPIC_API_KEY=") {
		t.Fatalf("leaked env: %s", joined)
	}
	if !strings.Contains(joined, "XDG_DATA_HOME=/scratch/data") ||
		!strings.Contains(joined, "XDG_CONFIG_HOME=/scratch/config") {
		t.Fatalf("missing scratch overrides: %s", joined)
	}
}
```

- [ ] **Step 2: Verify fail**, then **implement** `main.go` skeleton + `scratchEnv` + flags (`--corpus eval/cycle/corpus/corpus.jsonl --project acme-migration --repo . --keep --skip-reflect --drain-timeout 3m --ollama http://localhost:11434 --results-dir eval/cycle/results`) and a `run()` that currently: loads+validates corpus, creates scratch (`os.MkdirTemp("", "ghost-evalcycle-*")` with subdirs `data`,`config`), removes on exit unless `--keep`.

- [ ] **Step 3: implement `inject.go`** per Interfaces above:

```go
var savedIDRe = regexp.MustCompile(`Memory saved \(id:\s*([0-9a-fA-F]{16})\)`)

type mcpSession struct{ sess *mcp.ClientSession }

func startMCP(ctx context.Context, ghostBin string, env []string) (*mcpSession, error) {
	cmd := exec.Command(ghostBin, "mcp")
	cmd.Env = env
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "ghost-evalcycle", Version: "0.1.0"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("connect mcp: %w", err)
	}
	return &mcpSession{sess: sess}, nil
}

func (s *mcpSession) close() { _ = s.sess.Close() }

func textOf(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func (s *mcpSession) callText(ctx context.Context, tool string, args map[string]any) (string, error) {
	res, err := s.sess.CallTool(ctx, &mcp.CallToolRequest{Params: mcp.CallToolParams{Name: tool, Arguments: args}})
	if err != nil {
		return "", err
	}
	if res.IsError {
		return "", fmt.Errorf("%s failed: %s", tool, textOf(res))
	}
	return textOf(res), nil
}

func saveAll(ctx context.Context, s *mcpSession, project string, entries []corpus.Entry) (map[string]string, error) {
	ids := make(map[string]string, len(entries))
	for _, e := range entries {
		out, err := s.callText(ctx, "ghost_memory_save", map[string]any{
			"project_id": project,
			"content":    e.Content,
			"category":   e.Category,
			"importance": e.Importance,
			"tags":       e.TagsOrEmpty(),
		})
		if err != nil {
			return nil, fmt.Errorf("save %s: %w", e.Key, err)
		}
		m := savedIDRe.FindStringSubmatch(out)
		if m == nil {
			return nil, fmt.Errorf("save %s: unrecognized response %q", e.Key, out)
		}
		if prev, dupKey := ids[e.Key]; dupKey && prev != m[1] {
			return nil, fmt.Errorf("save %s: id collision", e.Key)
		}
		ids[e.Key] = m[1]
	}
	return ids, nil
}
```

Adjust the regex width if store ids aren't 32-hex (verify against a live save during bring-up; relax to `([0-9a-fA-F]+)`).

`waitForEmbeddings` (in inject.go too):

```go
func waitForEmbeddings(ctx context.Context, ollamaURL, dbPath, projectName string, timeout time.Duration) error {
	if err := ollamaReachable(ollamaURL); err != nil {
		return fmt.Errorf("embedding drain needs Ollama: %w", err)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck
	deadline := time.Now().Add(timeout)
	for {
		total, done := 0, 0
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memories m JOIN projects p ON m.project_id=p.id WHERE p.name=? OR p.id=?`,
			projectName, projectName).Scan(&total); err != nil {
			return fmt.Errorf("count memories: %w", err)
		}
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM memory_embeddings e JOIN memories m ON e.memory_id=m.id JOIN projects p ON m.project_id=p.id WHERE p.name=? OR p.id=?`,
			projectName, projectName).Scan(&done); err != nil {
			return fmt.Errorf("count embeddings: %w", err)
		}
		if total > 0 && done == total {
			fmt.Printf("embeddings drained: %d/%d\n", done, total)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("embedding drain timeout: %d/%d embedded after %s", done, total, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func ollamaReachable(url string) error {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url + "/api/tags")
	if err != nil {
		return fmt.Errorf("GET %s/api/tags: %w (is Ollama running?)", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama status %d", resp.StatusCode)
	}
	return nil
}
```

`runGhost`:

```go
func runGhost(ctx context.Context, env []string, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("ghost %s: %w: %s",
			strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
```

Wire `run()` through step order: build binary (`go build -o <scratch>/ghost ./cmd/ghost`, Dir=`--repo`) → startMCP → saveAll → waitForEmbeddings (session stays open — the embedding worker lives inside `ghost mcp`) → close MCP session (DB at `<scratch>/data/ghost/ghost.db`). Print progress per phase.

- [ ] **Step 4: Pass + suite.** `go test ./eval/cycle/... -v && go vet ./... && go test ./...`
- [ ] **Step 5: Commit** — `feat(eval): evalcycle runner core (scratch isolation, MCP inject, drain gate)`

---

### Task 6: stage output parsers + link/resolution grading

**Files:**
- Create: `eval/cycle/stages.go`
- Test: `eval/cycle/stages_test.go`

**Interfaces:**
- Produces:
  - `parseSupersedeLines(out string) []supPair` where `supPair{NewerID8, Relation, OtherID8 string}` (Relation `supersedes|causes`).
  - `parseResolveIDs(out string) []string` (id8 list).
  - `Stage{Name string; TP, FP, FN, Causes int; Misses []Miss}`, `Miss{Key, Kind, Detail string}`, `(Stage) Precision() float64`, `(Stage) Recall() float64` (0.0 on zero denom).
  - `gradeSupersede(ids map[string]string, entries []corpus.Entry, pairs []supPair) Stage` — direction-aware vs `ExpectedSupersededBy`; causes links counted in `Causes` only (reported, not scored).
  - `gradeResolve(entries []corpus.Entry, confirmedID8s []string, ids map[string]string) Stage`.

- [ ] **Step 1: Failing tests** with realistic fixture outputs:

```go
const supOut = `acme-migration: 3 candidate pairs, 2 supersedes, 1 causes, 0 reclassified, linked
  aabbccdd11223344  supersedes  556677889900aabb
  deadbeefdeadbeef  supersedes  0011223344556677
  feedfacefeedface  causes  1111222233334444
Re-run with --apply would not appear here.`

func TestParseSupersedeLines(t *testing.T) {
	pairs := parseSupersedeLines(supOut)
	if len(pairs) != 3 {
		t.Fatalf("want 3, got %d: %+v", len(pairs), pairs)
	}
	if pairs[0].NewerID8 != "aabbccdd" || pairs[0].OtherID8 != "55667788" || pairs[0].Relation != "supersedes" {
		t.Fatalf("pair0 wrong: %+v", pairs[0])
	}
	if pairs[2].Relation != "causes" {
		t.Fatalf("pair2 wrong: %+v", pairs[2])
	}
}

const resOut = `acme-migration: 30 loaded, 12 after prefilter, 2 confirmed evidence, would resolve 2
  aabbccdd11223344  [fact]  Some content line here
  9988776655443322  [gotcha]  Another content line
Re-run with --apply to mark these resolved.`

func TestParseResolveIDs(t *testing.T) {
	got := parseResolveIDs(resOut)
	want := []string{"aabbccdd", "99887766"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}
```

Grading tests build synthetic `ids` maps (16-hex values whose first 8 chars match fixtures) and small entry slices asserting TP/FP/FN/Misses/Causes exactly, including a wrong-direction supersedes (annotated older→newer reversed) counted FP+FN.

- [ ] **Step 2: Verify fail** (functions undefined).
- [ ] **Step 3: Implement** — regexes anchored like `(?m)^\s*([0-9a-f]{8})(?:[0-9a-f]+)?\s+(supersedes|causes)\s+([0-9a-f]{8})(?:[0-9a-f]+)?\s*$` (tolerate full-id echoes); grade functions per Interfaces doc.
- [ ] **Step 4: Pass + suite.**
- [ ] **Step 5: Commit** — `feat(eval): supersede/resolve output parsing and graded scoring`

---

### Task 7: reflect final-state grading

**Files:**
- Create: `eval/cycle/state.go`
- Test: `eval/cycle/state_test.go`

**Interfaces:**
- Produces:
  - `fetchMemRows(dbPath, projectName string) ([]memRow, error)` where `memRow{ID, Content string}` (read-only SQL).
  - `normContent(s string) string` (lowercase, whitespace collapsed).
  - `jaccard(a, b string) float64` over token sets.
  - `MergeResult{Group string, Status string /*collapsed|partial|lost*/, Survivors, Detail string}`.
  - `gradeReflect(entries []corpus.Entry, rowsBefore, rowsAfter []memRow) ReflectReport` where `ReflectReport{Before, After int; Groups []MergeResult; DroppedDistractors []string; DroppedImportant int}`.
  - Descendant rule: row counts toward an entry iff `jaccard(norm(row), norm(entry.Content)) >= 0.55`; distractor survives iff any row ≥ 0.65 (reflect rewrites merged content, so exact matching is impossible by design).

- [ ] **Step 1: Failing tests** — synthetic rows: cluster with exactly one high-overlap descendant → `collapsed`; three originals still present → `partial`; none → `lost` (+DroppedImportant bump); distractor with no near row → listed in DroppedDistractors. normContent/jaccard table cases.
- [ ] **Step 2: Verify fail.**
- [ ] **Step 3: Implement** (SQL identical in shape to the drain counters, selecting `m.id, m.content` ordered by created_at then id for stable reports).
- [ ] **Step 4: Pass + suite.**
- [ ] **Step 5: Commit** — `feat(eval): reflect-stage set-level grading over final store state`

---

### Task 8: report writer + main wiring

**Files:**
- Create: `eval/cycle/report.go`
- Modify: `eval/cycle/main.go` (finish `run()`)

**Interfaces:**
- Produces: `writeReport(dir, dateStr string, d ReportData) (string, error)` where `ReportData{Date, CorpusPath, Project string; Total int; DrainDrained bool; Supersede, Resolve Stage; Reflect ReflectReport; Applied bool; Backend string /* cli.Name() */}`; Markdown layout: H1 date, environment block, per-stage scorecard tables (P/R/TP/FP/FN/causes), reflect table (group statuses, dropped lists), then full misclassification listing (one bullet per Miss with content excerpt ≤70 chars).

- [ ] **Step 1: Implement report.go** (fmt.Fprintf sections; filename `<date>-report.md`, suffix `-<HHMMSS>` if exists; MkdirAll results dir).
- [ ] **Step 2: Finish run()**: stage invocations — `runGhost(supersede --apply)` → parse+grade; `runGhost(resolve --apply)` → parse+grade; unless `--skip-reflect`: `rowsBefore := fetchMemRows(...)`, `runGhost(reflect --tier opencode --apply)` (retry once after 5s on SQLITE_BUSY text), `rowsAfter := fetchMemRows(...)`, `gradeReflect`. (The MCP session was already closed after the drain gate.) Write report, print one-line summary per stage to stdout. Exit 0 unless infrastructure error; scores live in the report.
- [ ] **Step 3: Compile + suite + vet.**
- [ ] **Step 4: Commit** — `feat(eval): evalcycle report writer and stage orchestration`

---

### Task 9: judged run + findings (execution-time)

**Files:**
- Create: `docs/superpowers/reports/2026-08-21-eval-cycle-findings.md` (from generated report + analysis)

- [ ] **Step 1: Smoke** — `go run ./eval/cycle --keep --skip-reflect` with Ollama up; inspect scratch DB contents manually; confirm ids/drain/report paths.
- [ ] **Step 2:** Full run `GHOST_OPENCODE_MODEL=<wayne's deepseek model> go run ./eval/cycle`; capture report.
- [ ] **Step 3:** Analyze every miss (threshold vs prompt vs classifier confusion vs annotation error); tune only if a bug is found (separate commits).
- [ ] **Step 4:** Write findings doc; commit `docs: eval cycle findings (first judged run)`.

## Out of scope

- CI workflow_dispatch wiring; deterministic PR slice; Obsidian/task surfaces; changing production ranking behavior.
