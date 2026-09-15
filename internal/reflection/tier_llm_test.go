package reflection

import (
	"bytes"
	"log/slog"
	"testing"
)

// TestTieredConsolidatorInjectsLogger pins the wiring that lets an LLM tier's
// diagnostics reach the configured sink: NewTieredConsolidator hands its
// logger to tiers that accept one (LlmConsolidator.SetLogger).
func TestTieredConsolidatorInjectsLogger(t *testing.T) {
	var buf bytes.Buffer
	lg := slog.New(slog.NewTextHandler(&buf, nil))
	llm := NewLlmConsolidator(nil)

	_ = NewTieredConsolidator([]Consolidator{llm}, lg)

	if llm.log() != lg {
		t.Fatal("tiered consolidator did not inject its logger into the LLM tier")
	}
}
