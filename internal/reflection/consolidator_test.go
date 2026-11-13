package reflection

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestTieredConsolidator_UsesFirstAvailable(t *testing.T) {
	unavailable := &stubConsolidator{name: "unavail", available: false}
	available := &stubConsolidator{
		name:      "avail",
		available: true,
		result:    ReflectionResult{LearnedContext: "from-avail"},
	}

	tc := NewTieredConsolidator([]Consolidator{unavailable, available}, slog.Default())
	result, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.LearnedContext != "from-avail" {
		t.Errorf("expected result from avail tier, got %q", result.LearnedContext)
	}
	if tc.ActiveTier() != "tiered:avail" {
		t.Errorf("expected active tier 'tiered:avail', got %q", tc.ActiveTier())
	}
}

func TestTieredConsolidator_FallsBackOnError(t *testing.T) {
	failing := &stubConsolidator{name: "fail", available: true, err: fmt.Errorf("boom")}
	fallback := &stubConsolidator{
		name:      "fallback",
		available: true,
		result:    ReflectionResult{LearnedContext: "from-fallback"},
	}

	tc := NewTieredConsolidator([]Consolidator{failing, fallback}, slog.Default())
	result, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.LearnedContext != "from-fallback" {
		t.Errorf("expected fallback result, got %q", result.LearnedContext)
	}
}

func TestTieredConsolidator_AllFail(t *testing.T) {
	failing := &stubConsolidator{name: "fail", available: true, err: fmt.Errorf("boom")}

	tc := NewTieredConsolidator([]Consolidator{failing}, slog.Default())
	_, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err == nil {
		t.Fatal("expected error when all tiers fail")
	}
}

// TestTieredConsolidatorDoesNotFallThroughACancelledCaller pins that a
// cancelled caller is fatal rather than a tier failure. The LLM tier reports
// context.Canceled when the command is asked to stop, and reading that as a
// tier failure would run the mechanical tier and, with --apply, rewrite the
// corpus for a run the operator stopped. The error is the cancellation, not the
// fallback's success, which is what proves the fallback never ran.
func TestTieredConsolidatorDoesNotFallThroughACancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	llm := &stubConsolidator{name: "llm", available: true, err: context.Canceled}
	fallback := &stubConsolidator{name: "sqlite", available: true, mechanical: true, result: ReflectionResult{LearnedContext: "mechanical"}}

	tc := NewTieredConsolidator([]Consolidator{llm, fallback}, slog.Default())
	_, err := tc.Consolidate(ctx, ReflectionInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Consolidate returned %v, want the cancellation rather than a fall-through", err)
	}
}

// TestTieredConsolidatorRefusesAMechanicalResultFromACancelledCaller is the
// other half: the mechanical tier never consults the context, so a result it
// returns after the caller gave up must still be refused. Without the check
// after EVERY tier, a bare mechanical selection would hand back a result — and
// the apply path would rewrite the corpus.
func TestTieredConsolidatorRefusesAMechanicalResultFromACancelledCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sqlite := &stubConsolidator{name: "sqlite", available: true, mechanical: true, result: ReflectionResult{LearnedContext: "mechanical"}}

	tc := NewTieredConsolidator([]Consolidator{sqlite}, slog.Default())
	_, err := tc.Consolidate(ctx, ReflectionInput{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Consolidate returned %v, want the cancellation rather than a result", err)
	}
}

func TestTieredConsolidator_NoneAvailable(t *testing.T) {
	tc := NewTieredConsolidator([]Consolidator{
		&stubConsolidator{name: "off", available: false},
	}, slog.Default())

	if tc.Available(context.Background()) {
		t.Error("should not be available when no tiers are")
	}
	_, err := tc.Consolidate(context.Background(), ReflectionInput{})
	if err == nil {
		t.Fatal("expected error when no tiers available")
	}
}

func TestSQLiteConsolidator_MergesDuplicates(t *testing.T) {
	sc := NewSQLiteConsolidator()

	input := ReflectionInput{
		CurrentContext: "existing context",
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "Ghost uses SQLite for storage with FTS5", Importance: 0.7, Tags: []string{"db"}},
			{Category: "fact", Content: "Ghost uses SQLite for persistent storage with FTS5 search", Importance: 0.8, Tags: []string{"storage"}},
			{Category: "pattern", Content: "completely different memory about patterns", Importance: 0.6, Tags: []string{}},
		},
	}

	result, err := sc.Consolidate(context.Background(), input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The two similar "fact" memories should be merged into one.
	if len(result.Memories) != 2 {
		t.Fatalf("expected 2 memories after dedup, got %d", len(result.Memories))
	}
	if result.LearnedContext != "existing context" {
		t.Error("should preserve existing learned context")
	}

	// The merged memory should have the higher importance.
	for _, m := range result.Memories {
		if m.Category == "fact" && m.Importance != 0.8 {
			t.Errorf("merged fact should have importance 0.8, got %v", m.Importance)
		}
	}
}

func TestSQLiteConsolidator_EmptyInput(t *testing.T) {
	sc := NewSQLiteConsolidator()
	result, err := sc.Consolidate(context.Background(), ReflectionInput{CurrentContext: "ctx"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(result.Memories) != 0 {
		t.Errorf("expected 0 memories for empty input, got %d", len(result.Memories))
	}
	if result.LearnedContext != "ctx" {
		t.Error("should preserve context on empty input")
	}
}

func TestInferGlobalScope(t *testing.T) {
	tests := []struct {
		name     string
		category string
		content  string
		want     string
	}{
		{"generic preference without cross-repo signal stays project", "preference", "use tabs not spaces", "project"},
		{"preference with explicit cross-repo signal is global", "preference", "use tabs not spaces across all repos", "global"},
		{"architecture is project", "architecture", "ghost uses 3-block prompt caching", "project"},
		{"cross-repo workflow", "fact", "deploy to infra cluster from any repo", "global"},
		{"single ssh hit stays project (was false positive, #319)", "fact", "SSH into node3 via bastion", "project"},
		{"issue #319 example: project-specific ssh note stays project", "fact", "SSH into relay 3 to restart the block producer", "project"},
		{"issue #319 example: project-specific relay preference stays project", "preference", "prefer using relay 3 for deploys", "project"},
		{"single always-use hit stays project (was false positive, #319)", "convention", "always use nerdctl not docker", "project"},
		{"two weak hits promote to global", "fact", "SSH into the shared cluster for maintenance", "global"},
		{"correlated weak hits in a project-scoped sentence stay project", "fact", "deploy to the project cluster for maintenance", "project"},
		{"project convention", "convention", "commit messages use feat/fix prefix", "project"},
		{"all repos", "pattern", "run go vet across all repos before release", "global"},
		{"dev machine", "fact", "dev machine runs Asahi Fedora on Apple Silicon", "global"},
		{"plain project fact", "fact", "FTS5 uses porter stemmer tokenizer", "project"},
		{"secret never global even as preference", "preference", "the api key for staging is abc123", "project"},
		{"credential fact stays project", "fact", "the deploy credential rotates every 90 days", "project"},
		{"bearer token stays project", "gotcha", "requests need a bearer token in the header", "project"},
		{"password in cross-repo phrasing stays project", "fact", "the grafana password works across all repos", "project"},
		// #966: a fact naming one host, node or cluster is an operations note
		// for the project that runs it, even with two weak hits or a strong one.
		{"numbered relay with two weak hits stays project", "fact", "ssh into relay-3 on the cluster to restart the block producer", "project"},
		{"numbered node with deploy phrasing stays project", "fact", "deploy to node-7 from CI", "project"},
		{"unhyphenated host number stays project", "fact", "restart node5 after the kernel update across all repos", "project"},
		{"compound staging host stays project", "fact", "staging-app-2 has a memory leak on dev machine", "project"},
		{"ipv4 address stays project", "fact", "ssh 10.0.0.12 and check the cluster logs", "project"},
		{"internal dns name stays project", "fact", "ssh bp1.internal then deploy to the cluster", "project"},
		// One named repository is project-scoped too; cross-repo language is not.
		{"hosted repository slug stays project", "fact", "releases are cut from github.com/wcatz/ghost across all repos", "project"},
		{"scp-style remote stays project", "fact", "push to git@github.com:wcatz/roller.git and use a dev machine", "project"},
		{"named repository stays project", "fact", "in the infra repo always use helmfile diff before apply across all repos", "project"},
		{"docs site link is not a repository", "fact", "workflow syntax is documented at docs.github.com/en/actions, use it across all repos", "global"},
		{"www host is the same repository host", "fact", "mirror www.github.com/wcatz/ghost across all repos", "project"},
		{"bare forge host is not a repository", "fact", "mirror everything on github.com/ across all repos", "global"},
		{"git@ without a remote path is not a repository", "fact", "ssh as git@ across all repos", "global"},
		{"a determiner is not a repository name", "fact", "run the migrations from the repo across all repos", "global"},
		{"our repo is not a repository name", "fact", "rebase in our repo before pushing across all projects", "global"},
		{"from any repo is not a named repository", "fact", "deploy to infra cluster from any repo", "global"},
		{"every repo is not a named repository", "fact", "run go vet in every repo on the dev machine", "global"},
		{"a file path is not a repository", "fact", "edit cmd/ghost/main.go across all repos", "global"},
		// Any node plus a number is read as a machine, node20 included: a wrong
		// project scope is recoverable, a host note replayed everywhere is not.
		{"node with a two-digit number is a host", "fact", "restart node-12 across all repos", "project"},
		{"node major version spelling is also read as a host", "fact", "use node20 across all repos", "project"},
		{"single digit node is a host", "fact", "restart node5 across all repos", "project"},
		// The marker is a concrete identifier: bare nouns and version-like tokens
		// do not make a fact host-specific.
		{"bare cluster noun still promotes with a second weak hit", "fact", "ssh into the shared cluster for maintenance", "global"},
		{"version-like token is not a host", "fact", "use sha-256 and utf-8 across all repos", "global"},
		{"strong pattern without a host stays global", "fact", "deploy to infra cluster from any repo", "global"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := inferGlobalScope(tt.category, tt.content)
			if got != tt.want {
				t.Errorf("inferGlobalScope(%q, %q) = %q, want %q", tt.category, tt.content, got, tt.want)
			}
		})
	}
}

// TestOpTierNormalizesScopeThroughTheTier moves the scope normalization check
// to where the value now arrives. Under the per-id contract (#639) the harness
// no longer states a scope for most memories, so what reaches
// normalizeReflectMemories is a stored row or a merge whose scope was inferred —
// and the one rule that still has teeth is that secret-looking text is never
// promoted, because a global memory is replayed into every project's injected
// context (issue #545).
func TestOpTierNormalizesScopeThroughTheTier(t *testing.T) {
	in := ReflectionInput{ProjectName: "ghost", ExistingMemories: []memory.Memory{
		{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1", Category: "convention", Content: "tabs, not spaces, in every repository we touch"},
		{ID: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2", Category: "fact", Content: "the deploy token is GITHUB_TOKEN=ghp_example in the runner env"},
	}}
	llm := NewLlmConsolidator(&fakeReflector{reply: `{"ops":["merge ` +
		`AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1,AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2` +
		` -> the deploy token is GITHUB_TOKEN=ghp_example in the runner env, and tabs are used in every repository we touch"]}`})

	result, err := llm.Consolidate(context.Background(), in)
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(result.Memories) != 1 {
		t.Fatalf("got %d memories, want the one merge: %+v", len(result.Memories), result.Memories)
	}
	if got := result.Memories[0].Scope; got != "project" {
		t.Errorf("scope = %q, want project — a merge naming a credential must never be promoted", got)
	}
}

// TestBuildReflectionPrompt_IncludesTags: existing tags must be serialized with
// their memory. Omitting them forced the LLM to invent fresh tags for every
// memory, cross-assigning keywords from unrelated memories in the corpus.
func TestBuildReflectionPrompt_IncludesTags(t *testing.T) {
	prompt := BuildReflectionPrompt(ReflectionInput{
		ProjectName:     "ghost",
		ProjectLanguage: "go",
		ExistingMemories: []memory.Memory{
			{Category: "fact", Content: "obsidian mirror is one-way", Importance: 0.7, Tags: []string{"obsidian", "vault"}},
			{Category: "gotcha", Content: "no tags on this one", Importance: 0.5},
		},
	})
	// `|` rather than `,` since the tag list is now escaped rather than joined: a
	// tag holding a comma or a pipe must not read as a different tag count, and a
	// comma cannot be the separator for that reason. An ordinary two-tag list is
	// byte-identical apart from the one character — see
	// TestTheOrdinaryTagListIsUnchanged, which is where the invisibility claim is
	// pinned.
	if !strings.Contains(prompt, "tags:[obsidian|vault]") {
		t.Errorf("prompt missing serialized tags:\n%s", prompt)
	}
	// Since #639 the tags are no longer the model's to state: a keep carries the
	// stored row's and a merge unions the ids it names, so the rule the prompt has
	// to carry is that it is done for the harness.
	if !strings.Contains(prompt, "tags are taken from the ids you name") {
		t.Errorf("prompt does not say how tags are derived")
	}
}

// TestGateMinOutput pins the scale-aware quality-gate minimum: strict 30% on
// small inputs, relaxing to a small absolute count on a large backlog (so a
// valid large consolidation is not rejected for keeping fewer than a
// percentage the prompt never asks for).
func TestGateMinOutput(t *testing.T) {
	cases := []struct {
		input int
		want  int
	}{
		{6, 2},
		{10, 3},
		{60, 18},
		{130, 11},
		{200, 5},
		{1000, 5},
	}
	for _, tc := range cases {
		if got := gateMinOutput(tc.input); got != tc.want {
			t.Errorf("gateMinOutput(%d) = %d, want %d", tc.input, got, tc.want)
		}
	}
}

// --- stubs ---

type stubConsolidator struct {
	name       string
	available  bool
	mechanical bool
	result     ReflectionResult
	err        error
}

func (s *stubConsolidator) Name() string                     { return s.name }
func (s *stubConsolidator) Available(_ context.Context) bool { return s.available }
func (s *stubConsolidator) Mechanical() bool                 { return s.mechanical }
func (s *stubConsolidator) Consolidate(_ context.Context, _ ReflectionInput) (ReflectionResult, error) {
	return s.result, s.err
}
