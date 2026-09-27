package reflection

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
)

const credentialFixture = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"

// logCapture collects a handler's output so a test can assert what a diagnostic
// does and does not carry.
type logCapture struct{ buf bytes.Buffer }

func (c *logCapture) Write(p []byte) (int, error) { return c.buf.Write(p) }

func (c *logCapture) logger() *slog.Logger {
	return slog.New(slog.NewTextHandler(&c.buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestDropSecretMemoriesRemovesCredentialOutput is the reflection half of issue
// #553. A consolidation prompt is a request, not a guarantee: a model asked to
// summarise ten memories will occasionally quote a value verbatim into its
// "consolidated" output, and that output becomes a new row — embedded, injected
// into every future session, and mirrored to the Obsidian vault. The store
// refuses such a save, but refusing it from inside the reflection tier would
// abort a whole apply transaction over one row; dropping the row is the same
// mitigation with the rest of the round intact.
func TestDropSecretMemoriesRemovesCredentialOutput(t *testing.T) {
	result := ReflectionResult{
		LearnedContext: "a summary of the project",
		Memories: []ReflectMemory{
			{Category: "fact", Content: "the relay listens on 2222"},
			{Category: "gotcha", Content: "the token is " + credentialFixture},
			{Category: "convention", Content: "commits are conventional commits"},
		},
	}
	capture := &logCapture{}
	dropSecretMemories(&result, capture.logger())

	if len(result.Memories) != 2 {
		t.Fatalf("got %d memories, want 2: %+v", len(result.Memories), result.Memories)
	}
	if result.Memories[0].Content != "the relay listens on 2222" ||
		result.Memories[1].Content != "commits are conventional commits" {
		t.Errorf("the surviving memories are not the two expected ones, in order: %+v", result.Memories)
	}
	if result.LearnedContext != "a summary of the project" {
		t.Errorf("dropSecretMemories changed LearnedContext to %q; it guards memories only", result.LearnedContext)
	}
}

// TestDropSecretMemoriesLogsTheFormatNotTheValue is what makes the drop safe to
// report. The diagnostic is the only place an operator learns a consolidation
// lost a memory, and it is also a place a credential would land in the log file
// forever — so it names the rule and the category and stops there.
func TestDropSecretMemoriesLogsTheFormatNotTheValue(t *testing.T) {
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "gotcha", Content: "the token is " + credentialFixture},
	}}
	capture := &logCapture{}
	dropSecretMemories(&result, capture.logger())

	out := capture.buf.String()
	if out == "" {
		t.Fatal("the drop was not logged — a silently discarded memory is indistinguishable from one the model never emitted")
	}
	if !strings.Contains(out, "GitHub personal access token") {
		t.Errorf("the log does not name the matched format: %s", out)
	}
	if !strings.Contains(out, "gotcha") {
		t.Errorf("the log does not name the category that lost the memory: %s", out)
	}
	if strings.Contains(out, credentialFixture) || strings.Contains(out, "0123456789abcdef") {
		t.Errorf("the log echoes the dropped credential: %s", out)
	}
}

// TestDropSecretMemoriesToleratesNilLogger matches the sibling guards
// (dropFabricatedMemories, dropForeignProjectMemories), which substitute
// slog.Default() for a nil logger rather than panicking or staying silent: the
// guard's job is to drop, and a drop that cannot report itself is the one
// failure mode its rationale names. The default logger is redirected for the
// duration so the substitution is exercised without the warning landing in the
// test's own output.
func TestDropSecretMemoriesToleratesNilLogger(t *testing.T) {
	var logged bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "fact", Content: "the relay listens on 2222"},
		{Category: "gotcha", Content: "the token is " + credentialFixture},
	}}
	dropSecretMemories(&result, nil)

	if len(result.Memories) != 1 {
		t.Fatalf("got %d memories, want 1: %+v", len(result.Memories), result.Memories)
	}
	if !strings.Contains(logged.String(), "GitHub personal access token") {
		t.Errorf("a nil logger produced no diagnostic, so the drop is unauditable: %q", logged.String())
	}
	if strings.Contains(logged.String(), credentialFixture) {
		t.Errorf("the substituted default logger echoed the credential: %q", logged.String())
	}
}

// TestDropSecretMemoriesKeepsProseAboutCredentials is the false-positive guard
// at the tier, where a false positive costs a memory: a consolidation that
// correctly observed "rotate the deployment password quarterly" must not lose
// that observation to a keyword match.
func TestDropSecretMemoriesKeepsProseAboutCredentials(t *testing.T) {
	result := ReflectionResult{Memories: []ReflectMemory{
		{Category: "convention", Content: "Rotate the deployment password every quarter."},
		{Category: "fact", Content: "The CI access_token lives in the runner env."},
		{Category: "gotcha", Content: "The tokenizer keeps a 30k vocabulary and drops unknown words."},
	}}
	capture := &logCapture{}
	dropSecretMemories(&result, capture.logger())

	if len(result.Memories) != 3 {
		t.Errorf("got %d memories, want all 3: %+v", len(result.Memories), result.Memories)
	}
}

// scriptedReflector is a harness stand-in: it returns a fixed reply, so the
// test exercises Ghost's own pipeline with no subprocess and no billable call
// (#548). The tier under test is handed model output that quotes a credential
// alongside two ordinary memories.
type scriptedReflector struct{ reply string }

func (s scriptedReflector) Reflect(context.Context, string) (string, ai.TokenUsage, error) {
	return s.reply, ai.TokenUsage{}, nil
}

// TestLlmConsolidatorDropsCredentialOutput pins the call site rather than the
// helper: a guard that nothing invokes is indistinguishable from a guard that
// does not exist, and this is the only place the LLM tier's output becomes
// store rows.
func TestLlmConsolidatorDropsCredentialOutput(t *testing.T) {
	reply, err := json.Marshal(ReflectionResult{
		Memories: []ReflectMemory{
			{Category: "fact", Content: "the relay listens on 2222"},
			{Category: "gotcha", Content: "the token is " + credentialFixture},
			{Category: "convention", Content: "commits are conventional commits"},
		},
	})
	if err != nil {
		t.Fatalf("build scripted reply: %v", err)
	}

	result, err := NewLlmConsolidator(scriptedReflector{reply: string(reply)}).
		Consolidate(context.Background(), ReflectionInput{})
	if err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	if len(result.Memories) != 2 {
		t.Fatalf("got %d memories, want 2: %+v", len(result.Memories), result.Memories)
	}
	for _, m := range result.Memories {
		if strings.Contains(m.Content, credentialFixture) {
			t.Errorf("a credential survived the tier: %q", m.Content)
		}
	}
}
