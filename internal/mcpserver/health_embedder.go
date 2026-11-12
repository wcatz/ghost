package mcpserver

import (
	"context"
	"fmt"
)

func (s *Server) embedderDiagnosticLines(ctx context.Context) []string {
	d, ok := s.embedder.(embedderDiagnostics)
	if !ok {
		return nil
	}
	if !d.Alive(ctx) {
		return []string{"  ⚠ Ollama unreachable\n"}
	}
	present, err := d.HasModel(ctx)
	switch {
	case err != nil:
		return []string{fmt.Sprintf("  ⚠ could not check Ollama model %q: %v\n", d.Model(), err)}
	case !present:
		return []string{fmt.Sprintf("  ⚠ model %q not installed in Ollama — run: ollama pull %s\n", d.Model(), d.Model())}
	}
	return nil
}
