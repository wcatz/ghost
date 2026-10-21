package mcpserver

// The embedder's own diagnostics in ghost_health (#1738).
//
// The lines this file renders are the report's answer about the EMBEDDING
// BACKEND: whether it is reachable and whether the model it needs is
// installed. Both questions are answered by the network — the liveness probe
// hits the endpoint's root, the model check its tags — so they are the only
// part of the report that is not a store read.
//
// That makes them the part of the report that must NOT run while the
// diagnostic read is open. The read handle has ONE connection, and it is
// shared with the retrieval snapshot (Candidates) and the usefulness read, so
// a report that held it across a probe would make a concurrent search wait
// for the probe — up to seven seconds of it — for a connection the probe does
// not need. The probes therefore run BEFORE the read is opened, and the lines
// they render are put back in the same place, so the report is the same
// report.

import (
	"context"
	"fmt"
)

// embedderDiagnosticLines renders the embedder's own diagnostics: whether the
// backend is reachable, and whether the model it needs is installed.
//
// The lines are returned rather than written, because the caller renders them
// where the report wants them and not where they were read.
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
