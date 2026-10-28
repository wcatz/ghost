package main

import (
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

func TestConsolidationInputBoundMirrorsMemory(t *testing.T) {
	if maxConsolidationInput != memory.MaxConsolidationInput {
		t.Fatalf("cmd/ghost maxConsolidationInput (%d) != memory.MaxConsolidationInput (%d): the scan bound has drifted",
			maxConsolidationInput, memory.MaxConsolidationInput)
	}
}
