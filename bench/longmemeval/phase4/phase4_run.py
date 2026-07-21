#!/usr/bin/env python3
"""Phase 4 end-to-end LongMemEval-S driver: retrieve -> generate -> judge.

This is the generation + judging half of the Phase 4 pipeline. The retrieval
half is Ghost itself:

    go run ./bench/longmemeval -condition hybrid ... -retrieval-out ranked.jsonl
    python merge_retrieval.py --dataset longmemeval_s_cleaned.json \
        --retrieval ranked.jsonl --out merged.json
    python phase4_run.py generate --dataset merged.json --out hyp.jsonl ...
    python phase4_run.py judge    --dataset merged.json --hyp hyp.jsonl ...
    python phase4_run.py report   --dataset merged.json --hyp hyp.jsonl

Fidelity: prompt assembly (generation) and the yes/no grading templates
(judging) are the ORIGINAL LongMemEval functions, imported verbatim from a
LongMemEval checkout (`--longmemeval-src` or $LONGMEMEVAL_SRC pointing at its
`src/` dir). Only the API client is swapped, so results are reproducible
against the published harness. See bench/longmemeval/phase4/README.md.

Providers: `openai` (leaderboard-comparable when gen+judge are gpt-4o) or
`anthropic` (Claude gen/judge — NOT leaderboard-comparable; documented as an
internal "Ghost retrieval + Claude, Claude-judged" check).

No secret is ever logged. Keys come from the environment
(OPENAI_API_KEY / ANTHROPIC_API_KEY); for anthropic, ~/.config/ghost/config.yaml
(api.key) is a fallback so Ghost's own key can be reused.
"""
import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from collections import defaultdict

# Official generation defaults (run_generation.py, non-CoT).
GEN_LENGTH = 500          # max_tokens for generation
RESERVE = 1000            # headroom subtracted from model context for the prompt
DEFAULT_MODEL_MAX = 128000  # gpt-4o context; Claude (200k) is safely larger

# Backoff-eligible transient statuses (429 rate limit, 5xx, 529 overloaded).
RETRY_STATUS = {429, 500, 502, 503, 529}


# --------------------------------------------------------------------------
# key sourcing (never logged)
# --------------------------------------------------------------------------
def get_key(provider):
    if provider == "openai":
        k = os.environ.get("OPENAI_API_KEY")
        if not k:
            sys.exit("error: OPENAI_API_KEY not set")
        return k
    # anthropic: env first, then Ghost config fallback
    k = os.environ.get("ANTHROPIC_API_KEY")
    if k:
        return k
    cfg = os.path.expanduser("~/.config/ghost/config.yaml")
    if os.path.exists(cfg):
        in_api = False
        for line in open(cfg):
            if re.match(r"^\S", line):                 # top-level key
                in_api = line.strip().startswith("api:")
                continue
            if in_api:
                m = re.match(r"\s+key:\s*(\S+)", line)
                if m:
                    return m.group(1).strip().strip('"').strip("'")
    sys.exit("error: ANTHROPIC_API_KEY not set and no api.key in "
             "~/.config/ghost/config.yaml")


# --------------------------------------------------------------------------
# HTTP with retry (stdlib only; no openai/anthropic SDK dependency)
# --------------------------------------------------------------------------
def _post(url, headers, body, max_retries=6):
    data = json.dumps(body).encode()
    for attempt in range(max_retries):
        req = urllib.request.Request(url, data=data, headers=headers, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=300) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            status = e.code
            if status in RETRY_STATUS and attempt < max_retries - 1:
                wait = min(2 ** attempt, 30)
                sys.stderr.write(f"  http {status}, retry in {wait}s "
                                 f"({attempt + 1}/{max_retries})\n")
                time.sleep(wait)
                continue
            # non-retryable: surface body WITHOUT any header (no key leak)
            try:
                detail = e.read().decode()[:300]
            except Exception:
                detail = ""
            raise RuntimeError(f"HTTP {status}: {detail}") from None
        except (urllib.error.URLError, TimeoutError) as e:
            if attempt < max_retries - 1:
                wait = min(2 ** attempt, 30)
                sys.stderr.write(f"  net error, retry in {wait}s "
                                 f"({attempt + 1}/{max_retries})\n")
                time.sleep(wait)
                continue
            raise RuntimeError(f"network error: {e}") from None
    raise RuntimeError("exhausted retries")


def chat(provider, model, key, prompt, max_tokens):
    """Single-user-message chat completion, temperature 0. Returns text."""
    if provider == "openai":
        body = {"model": model, "temperature": 0, "max_tokens": max_tokens, "n": 1,
                "messages": [{"role": "user", "content": prompt}]}
        headers = {"Authorization": f"Bearer {key}", "Content-Type": "application/json"}
        out = _post("https://api.openai.com/v1/chat/completions", headers, body)
        return out["choices"][0]["message"]["content"]
    # anthropic
    body = {"model": model, "temperature": 0, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}]}
    headers = {"x-api-key": key, "anthropic-version": "2023-06-01",
               "Content-Type": "application/json"}
    out = _post("https://api.anthropic.com/v1/messages", headers, body)
    # concatenate all text blocks (usually one)
    return "".join(b.get("text", "") for b in out["content"] if b.get("type") == "text")


# --------------------------------------------------------------------------
# official prompt functions, imported from a LongMemEval checkout
# --------------------------------------------------------------------------
def import_official(longmemeval_src):
    src = longmemeval_src or os.environ.get("LONGMEMEVAL_SRC")
    if not src:
        sys.exit("error: pass --longmemeval-src or set $LONGMEMEVAL_SRC "
                 "(the LongMemEval repo's src/ dir)")
    sys.path.insert(0, os.path.join(src, "generation"))
    sys.path.insert(0, os.path.join(src, "evaluation"))
    from run_generation import prepare_prompt          # noqa: E402
    from evaluate_qa import get_anscheck_prompt         # noqa: E402
    return prepare_prompt, get_anscheck_prompt


def load_done(path):
    """question_ids already present in an append-only JSONL (resume support)."""
    done = set()
    if os.path.exists(path):
        for line in open(path):
            line = line.strip()
            if line:
                done.add(json.loads(line)["question_id"])
    return done


# --------------------------------------------------------------------------
# generate
# --------------------------------------------------------------------------
def cmd_generate(args):
    prepare_prompt, _ = import_official(args.longmemeval_src)
    import tiktoken
    tok = tiktoken.get_encoding("o200k_base")
    max_ret = args.model_max_length - GEN_LENGTH - RESERVE

    key = get_key(args.provider)
    data = json.load(open(args.dataset))
    done = load_done(args.out)
    if done:
        sys.stderr.write(f"resume: {len(done)} already generated, skipping them\n")

    n_done = len(done)
    with open(args.out, "a") as fout:
        for i, entry in enumerate(data):
            qid = entry["question_id"]
            if qid in done:
                continue
            prompt = prepare_prompt(
                entry, args.retriever_type, args.topk_context, args.useronly,
                args.history_format, args.cot, tok, "openai", max_ret, "none")
            answer = chat(args.provider, args.model, key, prompt, GEN_LENGTH).strip()
            fout.write(json.dumps({"question_id": qid, "hypothesis": answer}) + "\n")
            fout.flush()
            n_done += 1
            if n_done % 10 == 0 or i == len(data) - 1:
                sys.stderr.write(f"  generated {n_done}/{len(data)}\n")
    sys.stderr.write(f"done: {args.out}\n")


# --------------------------------------------------------------------------
# judge
# --------------------------------------------------------------------------
def cmd_judge(args):
    _, get_anscheck_prompt = import_official(args.longmemeval_src)
    key = get_key(args.provider)

    meta = {e["question_id"]: e for e in json.load(open(args.dataset))}
    out_path = args.judged or (args.hyp + f".eval-results-{args.model}")
    done = load_done(out_path)
    if done:
        sys.stderr.write(f"resume: {len(done)} already judged, skipping them\n")

    hyps = [json.loads(l) for l in open(args.hyp) if l.strip()]
    n_done = len(done)
    with open(out_path, "a") as fout:
        for h in hyps:
            qid = h["question_id"]
            if qid in done:
                continue
            e = meta[qid]
            abstention = qid.endswith("_abs")
            prompt = get_anscheck_prompt(
                e["question_type"], e["question"], e["answer"], h["hypothesis"],
                abstention=abstention)
            resp = chat(args.provider, args.model, key, prompt, 10)
            label = "yes" in resp.lower()
            fout.write(json.dumps({
                "question_id": qid, "question_type": e["question_type"],
                "abstention": abstention, "autoeval_label": label,
                "judge_raw": resp.strip()}) + "\n")
            fout.flush()
            n_done += 1
            if n_done % 20 == 0 or n_done == len(hyps):
                sys.stderr.write(f"  judged {n_done}/{len(hyps)}\n")
    sys.stderr.write(f"done: {out_path}\n")
    _report(out_path)


# --------------------------------------------------------------------------
# report (aggregate exactly like evaluate_qa.py)
# --------------------------------------------------------------------------
def _report(judged_path):
    rows = [json.loads(l) for l in open(judged_path) if l.strip()]
    if not rows:
        sys.exit(f"error: no rows in {judged_path}")
    labels = [1 if r["autoeval_label"] else 0 for r in rows]
    by_type = defaultdict(list)
    for r in rows:
        by_type[r["question_type"]].append(1 if r["autoeval_label"] else 0)

    overall = sum(labels) / len(labels)
    print(f"\nQuestions judged : {len(labels)}")
    print(f"Accuracy (overall): {overall:.4f}")
    print("\nPer question_type:")
    for t in sorted(by_type):
        v = by_type[t]
        print(f"  {t:28s} {sum(v)/len(v):.4f}  (n={len(v)})")


def cmd_report(args):
    path = args.judged or (args.hyp + f".eval-results-{args.model}")
    _report(path)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    def add_common(p):
        p.add_argument("--provider", choices=["openai", "anthropic"], required=True)
        p.add_argument("--model", required=True,
                       help="e.g. gpt-4o-2024-08-06 | claude-sonnet-5 | claude-opus-4-8")
        p.add_argument("--longmemeval-src",
                       help="LongMemEval repo src/ dir (or set $LONGMEMEVAL_SRC)")

    g = sub.add_parser("generate", help="produce hypotheses JSONL")
    add_common(g)
    g.add_argument("--dataset", required=True, help="merged dataset (merge_retrieval.py --out)")
    g.add_argument("--out", required=True, help="hypotheses JSONL (append/resume-safe)")
    g.add_argument("--retriever-type", default="flat-session")
    g.add_argument("--topk-context", type=int, default=5)
    g.add_argument("--history-format", default="json")
    g.add_argument("--useronly", action="store_true")
    g.add_argument("--cot", action="store_true")
    g.add_argument("--model-max-length", type=int, default=DEFAULT_MODEL_MAX)
    g.set_defaults(func=cmd_generate)

    j = sub.add_parser("judge", help="grade hypotheses yes/no and report")
    add_common(j)
    j.add_argument("--dataset", required=True, help="merged dataset (for gold answers/types)")
    j.add_argument("--hyp", required=True, help="hypotheses JSONL from generate")
    j.add_argument("--judged", help="output path (default: <hyp>.eval-results-<model>)")
    j.set_defaults(func=cmd_judge)

    r = sub.add_parser("report", help="re-aggregate an existing judged file")
    r.add_argument("--hyp", required=True)
    r.add_argument("--model", required=True)
    r.add_argument("--judged")
    r.set_defaults(func=cmd_report)

    args = ap.parse_args()
    args.func(args)


if __name__ == "__main__":
    main()
