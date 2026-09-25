package reflection

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestNormalizeReflectMemoriesScopeRules pins the rules that decide what an
// LLM emission may claim about itself.
//
// The scope rule is the one with teeth. The SQLite tier refuses to promote
// secret-looking content and justified the LLM tier's gap by pointing at its
// extraction prompt — but a prompt is a request, not a guarantee, so the check
// has to be on the value the model actually returned. A global memory is
// replayed into every future session in every project: promoting a credential
// does not contain a leak, it takes one confined to a single project and widens
// it to all of them (issue #545).
//
// These are unit-level on purpose. Driving them through a harness call would
// make a billable spawn part of ordinary `go test ./...` — the problem issue
// #548 exists to remove — and would test the model's cooperation rather than
// Ghost's own rule.
func TestNormalizeReflectMemoriesScopeRules(t *testing.T) {
	cases := []struct {
		name      string
		memories  []ReflectMemory
		wantScope []string
	}{
		{
			name: "secret-looking content is never promoted",
			memories: []ReflectMemory{
				{Category: "fact", Content: "The api key for this service is sk-live-123.", Scope: "global"},
				{Category: "fact", Content: "Rotate the deployment password every quarter.", Scope: "global"},
				{Category: "fact", Content: "The CI access_token lives in the runner env.", Scope: "global"},
				{Category: "fact", Content: "GITHUB_TOKEN=ghp_example", Scope: "global"},
				{Category: "fact", Content: "CLIENT_SECRET: rotate this value", Scope: "global"},
				{Category: "fact", Content: "PRIVATE_KEY=-----BEGIN PRIVATE KEY-----", Scope: "global"},
				{Category: "fact", Content: "api-key: example-secret", Scope: "global"},
			},
			wantScope: []string{"project", "project", "project", "project", "project", "project", "project"},
		},
		{
			name: "ordinary cross-repo knowledge is still promoted",
			memories: []ReflectMemory{
				{Category: "convention", Content: "Conventional Commits are used across all repos.", Scope: "global"},
			},
			wantScope: []string{"global"},
		},
		{
			name: "an unknown scope collapses to project",
			memories: []ReflectMemory{
				{Category: "fact", Content: "Something neutral.", Scope: "everywhere"},
				{Category: "fact", Content: "Something neutral."},
			},
			wantScope: []string{"project", "project"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := ReflectionResult{Memories: tc.memories}
			normalizeReflectMemories(&result)
			if len(result.Memories) != len(tc.wantScope) {
				t.Fatalf("got %d memories, want %d", len(result.Memories), len(tc.wantScope))
			}
			for i, want := range tc.wantScope {
				if result.Memories[i].Scope != want {
					t.Errorf("memory %d (%q) scope = %q, want %q",
						i, result.Memories[i].Content, result.Memories[i].Scope, want)
				}
			}
		})
	}
}

// TestParseReflectionResponseErrorSnippetIsRuneSafe keeps malformed model
// output from exposing a split UTF-8 rune in a diagnostic preview.
func TestParseReflectionResponseErrorSnippetIsRuneSafe(t *testing.T) {
	bad := "x" + strings.Repeat("🙂", 40) + "not-json"
	_, err := parseReflectionResponse(bad)
	if err == nil {
		t.Fatal("parseReflectionResponse unexpectedly accepted malformed JSON")
	}
	if !utf8.ValidString(err.Error()) {
		t.Errorf("error contains a split UTF-8 rune: %q", err.Error())
	}
	if strings.Contains(err.Error(), `\x`) || strings.Contains(err.Error(), `\u`) {
		t.Errorf("error exposes a split rune as an escape: %q", err.Error())
	}
}

// TestNormalizeReflectMemoriesFieldRules covers the rest of the normalization:
// these guard the apply transaction (an out-of-range importance or unknown
// category fails a schema CHECK mid-transaction and sinks the whole round).
func TestNormalizeReflectMemoriesFieldRules(t *testing.T) {
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "not-a-real-category", Content: "a", Importance: 1.7},
		{Category: "fact", Content: "b", Importance: -3},
		{Category: "fact", Content: "c", Tags: nil},
	}}
	normalizeReflectMemories(&result)

	if got := result.Memories[0].Importance; got != 1 {
		t.Errorf("importance 1.7 clamped to %v, want 1", got)
	}
	if got := result.Memories[1].Importance; got != 0 {
		t.Errorf("importance -3 clamped to %v, want 0", got)
	}
	if got := result.Memories[0].Category; got != "fact" {
		t.Errorf("invalid category %q fell back to %q, want fact", "not-a-real-category", got)
	}
	if result.Memories[2].Tags == nil {
		t.Error("nil tags left nil — the schema expects a list, not a NULL")
	}
}

// TestLooksLikeSecretCatchesLabelFreeCredentials: the value-level guard matters
// because every label pattern in looksLikeSecret needs a word followed by a
// space, so a model returning the credential itself — which is the more likely
// failure when a repository contains a live key — matched nothing and was
// promoted to _global, widening a live secret into every project's injected
// context.
func TestLooksLikeSecretCatchesLabelFreeCredentials(t *testing.T) {
	// The bodies are deliberately obvious placeholders. An earlier version of
	// this table used realistic-looking values and GitHub's push protection
	// rejected the commit as a leaked Slack token, which is the guard working
	// exactly as intended: what this test must prove is that the FORMAT is
	// recognised, not that a particular real credential is.
	const (
		stripeBody  = "EXAMPLEKEY00000000000000"
		githubBody  = "EXAMPLE0000000000000000000000000000"
		slackBody   = "0000000000-0000000000-EXAMPLEPLACEHOLDER"
		awsBody     = "IOSFODNN7EXAMPLE"
		gitlabBody  = "EXAMPLEplaceholder000"
		jwtBody     = "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.EXAMPLEpayload.EXAMPLEsig"
		opaqueToken = "aB3dEfGh1jKlMn0pQrStUvWxYz456789"
	)
	secrets := []string{
		"the deploy key is sk-live-" + stripeBody,
		"ghp_" + githubBody,
		"xoxb-" + slackBody,
		"AKIA" + awsBody,
		"glpat-" + gitlabBody,
		"eyJ" + jwtBody,
		opaqueToken,
	}
	for _, s := range secrets {
		if !looksLikeSecret(s) {
			t.Errorf("looksLikeSecret(%q) = false, want true — a label-free credential reached _global", s)
		}
	}

	// Prose must not be caught: the guard exists to stop one narrow shape, and
	// a false positive would demote an ordinary cross-project fact to
	// project-scoped for no reason.
	prose := []string{
		"prefer squash merges and wait for the windows job to finish",
		"the release checklist lives in docs and the changelog is generated",
		"ghost projects are identified by longest canonical path prefix first",
		"a long standing preference about how the team validates cardano node upgrades",
	}
	for _, p := range prose {
		if looksLikeSecret(p) {
			t.Errorf("looksLikeSecret(%q) = true, want false — ordinary prose is not a credential", p)
		}
	}

	// The label forms that already worked still work.
	labelled := []string{"the api key is in 1password", "password: hunter2", "bearer abc123"}
	for _, l := range labelled {
		if !looksLikeSecret(l) {
			t.Errorf("looksLikeSecret(%q) = false, want true", l)
		}
	}
}
