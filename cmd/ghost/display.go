package main

import (
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/memory"
	"github.com/wcatz/ghost/internal/secret"
)

// truncateForDisplay shortens s to at most n bytes without splitting a
// multi-byte character. The marker is deliberately display-only; the stored
// memory keeps the full content unless the writer's content cap applies.
func truncateForDisplay(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return memory.TruncateUTF8(s, n) + "..."
}

// displayStored renders a stored memory for a one-line listing in a report: its
// first line at the listing's width, and withheld whole when the content holds a
// credential.
//
// It is displayProposal's substitution over firstLine's cut, and both halves are
// kept because they answer different questions. The first-line cut is the
// listing's compactness: these lines sit under a summary and identify a row, and a
// memory with a runbook body does not get to print its body. The substitution is
// the safety half — 70 characters is more than a GitHub PAT needs, the input is the
// stored corpus rather than a proposal this run is holding, and the write-boundary
// guard that refuses such a value is not retroactive, so a pre-guard row can still
// carry one. See displayProposal for which sites that covers — and for why
// `ghost context`, which prints the same rows, is not one of them.
//
// An empty category is a real case and not a mistake: `ghost supersede --withdraw`
// prints an edge's target text and the edge records no category of its own, so the
// marker is the shorter one displayClaim produces.
func displayStored(content, category string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, category, content)
	}
	return assemble.PreviewLine(content, limit)
}
