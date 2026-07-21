#!/usr/bin/env python3
"""Merge Ghost's ranked_items into the LongMemEval dataset as retrieval_results.

Produces an augmented dataset (list JSON) that the official run_generation.py
reads via --in_file: each entry gains
    entry['retrieval_results'] = {'ranked_items': [{corpus_id, text}, ...]}
Optionally restricts to a type-stratified subset (for a smoke run).

Verifies corpus_id resolution: every ranked corpus_id must be a
haystack_session_id (after run_generation's noans_->answer_ normalization), or
the generator would silently see empty context. Aborts on any mismatch.

Input `--retrieval` is the JSONL produced by:
    go run ./bench/longmemeval -condition hybrid ... -retrieval-out ranked.jsonl
"""
import argparse
import json
import sys
from collections import defaultdict


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", required=True,
                    help="LongMemEval-S dataset JSON (longmemeval_s_cleaned.json)")
    ap.add_argument("--retrieval", required=True, help="Ghost -retrieval-out JSONL")
    ap.add_argument("--out", required=True)
    ap.add_argument("--per-type", type=int, default=0,
                    help="keep first N answerable questions of each type (0=all)")
    args = ap.parse_args()

    dataset = json.load(open(args.dataset))
    ranked = {}
    for line in open(args.retrieval):
        line = line.strip()
        if not line:
            continue
        obj = json.loads(line)
        ranked[obj["question_id"]] = obj["ranked_items"]

    missing_ret = 0
    bad_corpus = 0
    kept = []
    counts = defaultdict(int)
    for entry in dataset:
        qid = entry["question_id"]
        if qid.endswith("_abs"):
            continue  # abstention: excluded from retrieval scoring, skip
        if qid not in ranked:
            missing_ret += 1
            continue
        # verify every corpus_id resolves to a haystack session (post-normalize)
        haystack = set(entry["haystack_session_ids"])
        for it in ranked[qid]:
            cid = it["corpus_id"].replace("noans_", "answer_")
            if cid not in haystack:
                bad_corpus += 1
                print(f"BAD corpus_id {it['corpus_id']!r} not in haystack for {qid}",
                      file=sys.stderr)
        if args.per_type > 0:
            if counts[entry["question_type"]] >= args.per_type:
                continue
            counts[entry["question_type"]] += 1
        entry["retrieval_results"] = {"ranked_items": ranked[qid]}
        kept.append(entry)

    if bad_corpus:
        print(f"ABORT: {bad_corpus} corpus_id(s) did not resolve", file=sys.stderr)
        sys.exit(1)
    if missing_ret:
        print(f"WARN: {missing_ret} answerable questions had no retrieval line",
              file=sys.stderr)

    json.dump(kept, open(args.out, "w"))
    print(f"Wrote {len(kept)} entries to {args.out}")
    if args.per_type:
        print("per-type:", dict(counts))


if __name__ == "__main__":
    main()
