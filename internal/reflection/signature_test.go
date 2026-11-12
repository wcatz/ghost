package reflection

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

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

	a := memory.Memory{ID: "a", UpdatedAt: "2026-09-01 00:00:00", Category: "fact", Importance: 0.75, Content: "one"}
	b := a
	b.Importance = 0.749
	if InputSignature([]memory.Memory{a}) == InputSignature([]memory.Memory{b}) {
		t.Error("importance change across the prompt's one-decimal rounding boundary must change the signature")
	}
}
