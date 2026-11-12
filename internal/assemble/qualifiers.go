package assemble

import (
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// historicalQualifier returns the sentence shown above a block assembled at an instant rather than now.
func historicalQualifier(asOf time.Time) string {
	return memory.AsOfSourceNote(asOf) +
		" Retrieval was keyword-only over that historical text: an embedding records current content, so no vector " +
		"leg ran, and the link graph records no history, so conflict and near-duplicate handling did not run either. " +
		"The order is by matched query terms then decay, not bm25. The evidence counts were not read — an observation " +
		"is not versioned, so a count here would describe the present — which means this block's trace reports \"no " +
		"recorded evidence\" for every row because nothing was read, not because none exists."
}

// qualifiersFor builds the block's qualifiers from the request and retriever counts.
func qualifiersFor(req Request, set *memory.CandidateSet) []string {
	var out []string
	if req.AsOf != nil {
		out = append(out, historicalQualifier(*req.AsOf))
		out = append(out, memory.AsOfBorrowedWindowNote(*req.AsOf))
	}
	if set != nil {
		if note := memory.AsOfUnknownNote(set.Unrecorded); note != "" {
			out = append(out, note)
		}
	}
	return out
}
