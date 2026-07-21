#!/usr/bin/env python3
"""Estimate Phase 4 generation+judge cost with NO API calls.

Uses the official prepare_prompt for byte-accurate prompt assembly and counts
tokens with tiktoken o200k_base (gpt-4o's tokenizer). For Anthropic models the
count is approximate — Claude tokenizes differently — but tiktoken is a
reasonable, conservative proxy for a dollar estimate.

    python cost_estimate.py --dataset merged.json --longmemeval-src <src> \
        [--gen-model gpt-4o] [--judge-model gpt-4o] [--topk 5 10 25 50]

Pricing table (USD per 1M tokens) is a static snapshot — verify current rates
before relying on the dollar figure.
"""
import argparse
import json
import os
import sys

# per-1M-token (input, output) — static snapshot, verify before trusting $.
PRICES = {
    "gpt-4o":          (2.50, 10.00),
    "claude-sonnet-5": (3.00, 15.00),
    "claude-opus-4-8": (15.00, 75.00),
    "claude-haiku-4-5": (1.00, 5.00),
}
GEN_LENGTH = 500
RESERVE = 1000
MODEL_MAX = 128000


def price(model):
    for k, v in PRICES.items():
        if model.startswith(k):
            return v
    sys.exit(f"error: no pricing for {model!r}; add it to PRICES")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", required=True)
    ap.add_argument("--longmemeval-src")
    ap.add_argument("--gen-model", default="gpt-4o")
    ap.add_argument("--judge-model", default="gpt-4o")
    ap.add_argument("--topk", type=int, nargs="+", default=[5, 10, 25, 50])
    args = ap.parse_args()

    src = args.longmemeval_src or os.environ.get("LONGMEMEVAL_SRC")
    if not src:
        sys.exit("error: pass --longmemeval-src or set $LONGMEMEVAL_SRC")
    sys.path.insert(0, os.path.join(src, "generation"))
    import tiktoken
    from run_generation import prepare_prompt

    data = json.load(open(args.dataset))
    tok = tiktoken.get_encoding("o200k_base")
    max_ret = MODEL_MAX - GEN_LENGTH - RESERVE
    gin, gout = price(args.gen_model)
    jin, jout = price(args.judge_model)

    print(f"gen={args.gen_model} (${gin}/${gout} per 1M)  "
          f"judge={args.judge_model} (${jin}/${jout} per 1M)")
    for topk in args.topk:
        tot_in = n = 0
        for e in data:
            p = prepare_prompt(e, "flat-session", topk, False, "json", False,
                               tok, "openai", max_ret, "none")
            tot_in += min(len(tok.encode(p, allowed_special={'<|endoftext|>'})), max_ret + 2000)
            n += 1
        gen_cost = tot_in / 1e6 * gin + n * GEN_LENGTH / 1e6 * gout
        judge_cost = n * 800 / 1e6 * jin + n * 10 / 1e6 * jout
        print(f"  topk={topk:2d}  avg_in={tot_in // n:>7} tok  "
              f"gen=${gen_cost:7.2f}  judge=${judge_cost:6.2f}  "
              f"TOTAL=${gen_cost + judge_cost:7.2f}  (n={n})")


if __name__ == "__main__":
    main()
