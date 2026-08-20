#!/usr/bin/env python3
"""Convert MemoryAgentBench's Conflict_Resolution parquet into the JSONL
format bench/memoryagentbench's Go harness reads: one demo per line,
single-hop (fact_sh) rows only. See
docs/superpowers/specs/2026-08-20-memoryagentbench-supersede-benchmark-design.md.

Usage:
    pip install pyarrow
    curl -sL -o Conflict_Resolution-00000-of-00001.parquet \
        "https://huggingface.co/datasets/ai-hyz/MemoryAgentBench/resolve/main/data/Conflict_Resolution-00000-of-00001.parquet"
    python3 convert.py --parquet Conflict_Resolution-00000-of-00001.parquet --out demos.jsonl
"""
import argparse
import json
import sys

import pyarrow.parquet as pq


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--parquet", required=True, help="path to the downloaded Conflict_Resolution parquet")
    ap.add_argument("--out", required=True, help="output JSONL path")
    args = ap.parse_args()

    table = pq.read_table(args.parquet)
    rows = table.to_pylist()

    written = 0
    with open(args.out, "w") as f:
        for row in rows:
            meta = row["metadata"] or {}
            source = meta.get("source") or ""
            if not source.startswith("factconsolidation_sh_"):
                continue  # single-hop only — multi-hop needs a generation/decomposition step, out of scope
            questions = row["questions"] or []
            answers = row["answers"] or []
            qa_pair_ids = meta.get("qa_pair_ids") or []
            context = row["context"] or ""
            if not context:
                print(f"warning: {source} has empty context, skipping", file=sys.stderr)
                continue
            if len(questions) != len(answers) or len(questions) != len(qa_pair_ids):
                print(f"warning: {source} has mismatched questions/answers/qa_pair_ids lengths, skipping", file=sys.stderr)
                continue
            demo = {
                "source": source,
                "context": context,
                "questions": questions,
                "answers": answers,
                "qa_pair_ids": qa_pair_ids,
            }
            f.write(json.dumps(demo) + "\n")
            written += 1

    print(f"wrote {written} demo(s) to {args.out}", file=sys.stderr)
    if written == 0:
        sys.exit(1)


if __name__ == "__main__":
    main()
