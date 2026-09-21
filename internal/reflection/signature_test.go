package reflection

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestInputSignatureStableAndOrderIndependent(t *testing.T) {
	a := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.7, Source: "mcp", Tags: []string{"x", "y"}, Content: "one"}
	b := memory.Memory{ID: "b", UpdatedAt: "2026-09-02 00:00:00", Category: "gotcha", Importance: 0.9, Source: "reflection", Tags: []string{"z"}, Content: "two"}

	sigAB := InputSignature([]memory.Memory{a, b})
	sigBA := InputSignature([]memory.Memory{b, a})
	if sigAB != sigBA {
		t.Errorf("signature must be order-independent: %s != %s", sigAB, sigBA)
	}
	if sigAB == "" {
		t.Fatal("signature must not be empty")
	}
}

func TestInputSignatureInvalidatesOnPromptVisibleChange(t *testing.T) {
	base := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.7, Source: "mcp", Tags: []string{"x"}, Content: "one"}
	baseSig := InputSignature([]memory.Memory{base})

	mutate := map[string]func(m *memory.Memory){
		"content":    func(m *memory.Memory) { m.Content = "changed" },
		"category":   func(m *memory.Memory) { m.Category = "decision" },
		"importance": func(m *memory.Memory) { m.Importance = 0.8 },
		"source":     func(m *memory.Memory) { m.Source = "manual" },
		"tags":       func(m *memory.Memory) { m.Tags = []string{"y"} },
		"updated_at": func(m *memory.Memory) { m.UpdatedAt = "2026-09-02 00:00:00" },
		"id":         func(m *memory.Memory) { m.ID = "b" },
	}
	for name, fn := range mutate {
		m := base
		fn(&m)
		if got := InputSignature([]memory.Memory{m}); got == baseSig {
			t.Errorf("changing %s must change the signature", name)
		}
	}
}

func TestInputSignatureIgnoresAccessCount(t *testing.T) {
	a := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.7, Content: "one", AccessCount: 0}
	b := a
	b.AccessCount = 42
	if InputSignature([]memory.Memory{a}) != InputSignature([]memory.Memory{b}) {
		t.Error("access count must not affect the signature (ordinary reads would break the gate)")
	}
}
