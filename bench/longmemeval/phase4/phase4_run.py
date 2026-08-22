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
    if provider == "opencode":
        return ""  # subscription-billed CLI; no key
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


def get_key_openai_compat():
    """Get API key for OpenAI-compatible providers (OpenCode Go, etc.)."""
    for var in ("OPENCODE_API_KEY", "ZEN_API_KEY", "OPENAI_API_KEY"):
        k = os.environ.get(var)
        if k:
            return k
    sys.exit("error: no API key found; set OPENCODE_API_KEY, ZEN_API_KEY, "
             "or OPENAI_API_KEY")


# --------------------------------------------------------------------------
# HTTP with retry (stdlib only; no openai/anthropic SDK dependency)
# --------------------------------------------------------------------------
def _post(url, headers, body, max_retries=30):
    data = json.dumps(body).encode()
    for attempt in range(max_retries):
        hdrs = {**headers, "User-Agent": "ghost-phase4-bench/1.0"}
        req = urllib.request.Request(url, data=data, headers=hdrs, method="POST")
        try:
            with urllib.request.urlopen(req, timeout=300) as resp:
                return json.loads(resp.read())
        except urllib.error.HTTPError as e:
            status = e.code
            if status == 429 and attempt < max_retries - 1:
                try:
                    detail = e.read().decode()
                except Exception:
                    detail = ""
                # Parse GoUsageLimitError "Resets in Xmin"
                import re as _re
                m = _re.search(r"Resets in (\d+)min", detail)
                if m:
                    wait = int(m.group(1)) * 60
                    sys.stderr.write(f"  rate limit: resets in {m.group(1)}min, "
                                     f"sleeping {wait}s ({attempt + 1}/{max_retries})\n")
                else:
                    wait = min(2 ** attempt, 30)
                    sys.stderr.write(f"  http 429, retry in {wait}s "
                                     f"({attempt + 1}/{max_retries})\n")
                time.sleep(wait)
                continue
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


def chat(provider, model, key, prompt, max_tokens, api_base_url=None):
    """Single-user-message chat completion, temperature 0. Returns text."""
    if provider == "opencode":
        return chat_opencode(model, prompt)
    if provider == "openai":
        body = {"model": model, "temperature": 0, "max_tokens": max_tokens, "n": 1,
                "messages": [{"role": "user", "content": prompt}]}
        headers = {"Authorization": f"Bearer {key}", "Content-Type": "application/json"}
        base = (api_base_url or "https://api.openai.com").rstrip("/")
        out = _post(f"{base}/v1/chat/completions", headers, body)
        msg = out["choices"][0]["message"]
        # DeepSeek puts short answers in reasoning_content when max_tokens is tight
        return msg.get("content") or msg.get("reasoning_content", "")
    # anthropic
    body = {"model": model, "temperature": 0, "max_tokens": max_tokens,
            "messages": [{"role": "user", "content": prompt}]}
    headers = {"x-api-key": key, "anthropic-version": "2023-06-01",
               "Content-Type": "application/json"}
    out = _post("https://api.anthropic.com/v1/messages", headers, body)
    # concatenate all text blocks (usually one)
    return "".join(b.get("text", "") for b in out["content"] if b.get("type") == "text")


def chat_opencode(model, prompt):
    """Run one prompt through the `opencode` CLI (subscription-billed; no API
    key). Mirrors internal/ai.OpenCodeClient: --pure skips plugins, the child
    gets a scrubbed XDG_CONFIG_HOME and no ANTHROPIC_API_KEY so it cannot load
    the user's global opencode config or this repo's project config. Retries
    transient failures like _post."""
    import subprocess
    import tempfile
    last_err = None
    for attempt in range(3):
        try:
            env = {k: v for k, v in os.environ.items()
                   if k != "ANTHROPIC_API_KEY" and not k.startswith("XDG_CONFIG_HOME")}
            scratch = tempfile.mkdtemp(prefix="locomo-opencode-")
            env["XDG_CONFIG_HOME"] = scratch
            cmd = ["opencode", "run", "--format", "json", "--pure"]
            if model:
                cmd += ["-m", model]
            cmd.append(prompt)
            proc = subprocess.run(cmd, env=env, capture_output=True,
                                  text=True, timeout=600)
            if proc.returncode != 0:
                raise RuntimeError(f"opencode run: {proc.stderr[:300]}")
            parts = []
            for line in proc.stdout.splitlines():
                line = line.strip()
                if not line:
                    continue
                ev = json.loads(line)
                if ev.get("type") == "text" and ev.get("part", {}).get("type") == "text":
                    parts.append(ev["part"].get("text", ""))
            return "".join(parts)
        except (RuntimeError, subprocess.TimeoutExpired) as e:
            last_err = e
            wait = min(2 ** attempt, 30)
            sys.stderr.write(f"  opencode error ({e}), retry in {wait}s "
                             f"({attempt + 1}/3)\n")
            time.sleep(wait)
    raise RuntimeError(f"opencode exhausted retries: {last_err}")


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


def judge_type(qtype):
    """Map LoCoMo question types onto LongMemEval's answer-check templates
    (upstream get_anscheck_prompt raises NotImplementedError for anything
    else). Temporal keeps its off-by-one-lenient template; everything else
    uses the standard correctness check. Grouping/reporting still uses the
    original LoCoMo type."""
    return {
        "single-hop": "multi-session",
        "multi-hop": "multi-session",
        "open-domain": "multi-session",
        "temporal": "temporal-reasoning",
    }.get(qtype, qtype)


def safe_model(model):
    """Model ids like 'opencode-go/deepseek-v4-pro' contain '/', which would
    become a path separator in derived output filenames."""
    return model.replace("/", "_")


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

    if args.provider == "opencode":
        key = ""
    elif args.api_base_url:
        key = get_key_openai_compat()
    else:
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
            answer = chat(args.provider, args.model, key, prompt, GEN_LENGTH,
                          api_base_url=args.api_base_url).strip()
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
    from abstention_prompt import get_abstention_prompt
    if args.provider == "opencode":
        key = ""
    elif args.api_base_url:
        key = get_key_openai_compat()
    else:
        key = get_key(args.provider)

    meta = {e["question_id"]: e for e in json.load(open(args.dataset))}
    out_path = args.judged or (args.hyp + f".eval-results-{safe_model(args.model)}")
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

            if abstention:
                prompt = get_abstention_prompt(e["question"], h["hypothesis"])
            else:
                prompt = get_anscheck_prompt(
                    judge_type(e["question_type"]), e["question"], e["answer"],
                    h["hypothesis"], abstention=abstention)

            resp = chat(args.provider, args.model, key, prompt, 50,
                        api_base_url=args.api_base_url)
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

    # .get("abstention", False): legacy judged.jsonl rows written before the
    # abstention feature have no abstention key — they're all non-abstention.
    # cmd_judge can resume a partially-done legacy file (qid in done), so
    # treating the missing key as a hard error would crash on those rows.
    non_abstention = [r for r in rows if not r.get("abstention", False)]
    abstention = [r for r in rows if r.get("abstention", False)]

    overall_labels = [1 if r["autoeval_label"] else 0 for r in rows]
    non_abstention_labels = [1 if r["autoeval_label"] else 0 for r in non_abstention]
    abstention_labels = [1 if r["autoeval_label"] else 0 for r in abstention]

    overall_acc = sum(overall_labels) / len(overall_labels) if overall_labels else 0
    non_abstention_acc = (sum(non_abstention_labels) / len(non_abstention_labels)
                         if non_abstention_labels else 0)
    abstention_acc = (sum(abstention_labels) / len(abstention_labels)
                      if abstention_labels else 0)

    by_type = defaultdict(list)
    for r in non_abstention:
        by_type[r["question_type"]].append(1 if r["autoeval_label"] else 0)

    print(f"\nQuestions judged : {len(rows)}")
    print(f"  Non-abstention: {len(non_abstention)}")
    print(f"  Abstention:     {len(abstention)}")
    print(f"\nAccuracy (blended 500-question): {overall_acc:.4f}")
    print(f"Accuracy (non-abstention 470-question): {non_abstention_acc:.4f}")
    print(f"Accuracy (abstention 30-question): {abstention_acc:.4f}")

    print("\nPer question_type (non-abstention):")
    for t in sorted(by_type):
        v = by_type[t]
        print(f"  {t:28s} {sum(v)/len(v):.4f}  (n={len(v)})")


def cmd_report(args):
    path = args.judged or (args.hyp + f".eval-results-{safe_model(args.model)}")
    _report(path)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    sub = ap.add_subparsers(dest="cmd", required=True)

    def add_common(p):
        p.add_argument("--provider", choices=["openai", "anthropic", "opencode"], required=True)
        p.add_argument("--model", required=True,
                       help="e.g. gpt-4o-2024-08-06 | claude-sonnet-5 | claude-opus-4-8")
        p.add_argument("--longmemeval-src",
                       help="LongMemEval repo src/ dir (or set $LONGMEMEVAL_SRC)")
        p.add_argument("--api-base-url",
                       help="Override API base URL for OpenAI-compatible providers "
                            "(e.g. https://opencode.ai/zen/go)")

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
