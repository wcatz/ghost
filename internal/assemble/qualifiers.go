package assemble

// The qualifiers: the statements that change what a block MEANS rather than what
// it contains. A historical read is the case this exists for (#647) — a block of
// rows that is silently a reading of the past is read as a reading of the
// present, and an agent that believes it is looking at now will contradict a
// memory that was true then and is not now.

import (
	"time"

	"github.com/wcatz/ghost/internal/memory"
)

// historicalQualifier is the sentence a surface shows above a block that was
// assembled at an instant rather than now.
//
// It names the four things a reader cannot see from the rows: where they came
// from (the recorded versions, deleted memories included), what did not run (the
// vector leg, conflict and near-duplicate handling, and the evidence counts),
// what the order is (matched query terms then decay, not bm25 — so it is not
// comparable with a current search's ranking), and what an absent field in the
// trace means. A reader who has only some of those will draw a conclusion the read
// does not support: that it is current, that the ranking is the same ranking,
// that an absence means what an absence usually means, or — on the counts — that
// a memory nobody can date rests on nothing.
func historicalQualifier(asOf time.Time) string {
	// The store owns the first sentence, because the store owns the fact and
	// another surface renders it too; this function adds only what is true of a
	// RETRIEVAL, which a project listing does not do.
	return memory.AsOfSourceNote(asOf) +
		" Retrieval was keyword-only over that historical text: an embedding records current content, so no vector " +
		"leg ran, and the link graph records no history, so conflict and near-duplicate handling did not run either. " +
		"The order is by matched query terms then decay, not bm25. The evidence counts were not read — an observation " +
		"is not versioned, so a count here would describe the present — which means this block's trace reports \"no " +
		"recorded evidence\" for every row because nothing was read, not because none exists."
}

// qualifiersFor builds the block's qualifiers from the request and what the
// retriever reported. It is a function of the request alone plus the retriever's
// counts, so the two disclosures cannot be derived from different places and
// disagree about what this retrieval was.
//
// The unknown count is worded by the store, not here: the assembler holds a
// count and the store holds the fact, and one sentence has to answer "what is
// missing from this set" for both a direct reader of the set and a reader of a
// search over it.
//
// AsOfValidityNote is the same sentence the two as_of listing surfaces print
// (#908), and a search needs it for the opposite reason from theirs: they judge
// the window at T and withhold what it excludes, while stage 2 here judges
// nothing and withholds nothing, so the row's bounds are the only thing left to
// explain — and they are today's. Its withheld count is ZERO by construction,
// not by omission: an as_of request withholds no row on that account, and the
// note's count clause is therefore absent rather than claiming a number.
func qualifiersFor(req Request, set *memory.CandidateSet) []string {
	var out []string
	if req.AsOf != nil {
		out = append(out, historicalQualifier(*req.AsOf))
		out = append(out, memory.AsOfValidityNote(*req.AsOf, 0))
	}
	if set != nil {
		if note := memory.AsOfUnknownNote(set.Unrecorded); note != "" {
			out = append(out, note)
		}
	}
	return out
}
