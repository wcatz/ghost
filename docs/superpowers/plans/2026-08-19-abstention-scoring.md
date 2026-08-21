# Abstention Scoring Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add abstention question scoring to Phase 4 (end-to-end) LongMemEval benchmark and report blended 500-question accuracy alongside 470-question accuracy for fair comparison with competitors.

**Architecture:** Modify the Phase 4 pipeline to process all 500 questions (including 30 abstention), add LLM-judge abstention evaluation, and report separate accuracy metrics for non-abstention, abstention, and blended categories.

**Tech Stack:** Python (Phase 4 pipeline), Go (Phase 1 retrieval harness)

## Context

Ghost's published 96.8% accuracy (R@10 on Phase 1 retrieval-only) excludes 30 abstention questions. Competitor scores (Mem0 94.4%, Hindsight 91.4%, Supermemory 85.2%) likely include abstention performance in their aggregate numbers. Abstention is typically where retrieval-heavy systems perform worst — over-eager retrieval hallucinates answers instead of declining. Dropping this category inflates the score.

The fix: add an abstention-judge pass over the 30 excluded questions and report a blended 500-question accuracy alongside the 470-question one.

## File Structure

- Modify: `bench/longmemeval/phase4/phase4_run.py` — add abstention handling to generate/judge/report
- Modify: `bench/longmemeval/main.go` — add `--include-abstention` flag for Phase 1 (optional, for completeness)
- Create: `bench/longmemeval/phase4/abstention_prompt.py` — abstention judge prompt logic
- Modify: `docs/benchmarks.md` — update methodology to document blended scoring

---

## Task 1: Add Abstention Question Processing to Phase 4 Generate

**Files:**
- Modify: `bench/longmemeval/phase4/phase4_run.py:189-220`

**Interfaces:**
- Consumes: dataset JSON (all 500 questions including `_abs` suffix)
- Produces: hypotheses JSONL (all 500 questions)

- [ ] **Step 1: Modify cmd_generate to include abstention questions**

```python
def cmd_generate(args):
    prepare_prompt, _ = import_official(args.longmemeval_src)
    import tiktoken
    tok = tiktoken.get_encoding("o200k_base")
    max_ret = args.model_max_length - GEN_LENGTH - RESERVE

    if args.api_base_url:
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
            # Abstention questions: generate a refusal response
            if qid.endswith("_abs"):
                # For abstention, the model should say "I don't know" or similar
                # We generate a hypothesis that can be judged for correct refusal
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
                continue
            # Non-abstention: normal generation
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
```

- [ ] **Step 2: Test generate with abstention questions**

Run: `python bench/longmemeval/phase4/phase4_run.py generate --dataset <full-500-question-dataset> --out test_hyp.jsonl --provider openai --model gpt-4o`
Expected: Generates hypotheses for all 500 questions including 30 abstention

- [ ] **Step 3: Commit**

```bash
git add bench/longmemeval/phase4/phase4_run.py
git commit -m "feat(bench): include abstention questions in Phase 4 generate"
```

---

## Task 2: Add Abstention Judge Prompt Logic

**Files:**
- Create: `bench/longmemeval/phase4/abstention_prompt.py`

**Interfaces:**
- Consumes: question_id, hypothesis text
- Produces: judge prompt for abstention evaluation

- [ ] **Step 1: Create abstention judge prompt module**

```python
"""Abstention evaluation prompts for LongMemEval Phase 4."""

ABSTENTION_JUDGE_PROMPT = """You are evaluating whether a model correctly abstained from answering a question.

The question is an "abstention" question — there is no correct answer in the provided context. 
A well-behaved model should decline to answer or say "I don't know" rather than hallucinating an answer.

Question: {question}

Model response: {hypothesis}

Did the model correctly abstain? Consider:
1. Did the model explicitly say it doesn't know, can't find the answer, or similar?
2. Did the model avoid providing a specific factual answer?
3. Did the model acknowledge the limitation of the context?

Answer "yes" if the model correctly abstained, "no" if it hallucinated an answer.
"""


def get_abstention_prompt(question, hypothesis):
    """Generate a prompt for judging abstention correctness."""
    return ABSTENTION_JUDGE_PROMPT.format(question=question, hypothesis=hypothesis)
```

- [ ] **Step 2: Commit**

```bash
git add bench/longmemeval/phase4/abstention_prompt.py
git commit -m "feat(bench): add abstention judge prompt module"
```

---

## Task 3: Modify Judge to Handle Abstention Questions

**Files:**
- Modify: `bench/longmemeval/phase4/phase4_run.py:226-262`
- Modify: `bench/longmemeval/phase4/abstention_prompt.py`

**Interfaces:**
- Consumes: hypotheses JSONL (all 500 questions), dataset JSON
- Produces: eval-results JSONL (all 500 questions with abstention field)

- [ ] **Step 1: Modify cmd_judge to use abstention prompt for _abs questions**

```python
def cmd_judge(args):
    _, get_anscheck_prompt = import_official(args.longmemeval_src)
    from abstention_prompt import get_abstention_prompt
    if args.api_base_url:
        key = get_key_openai_compat()
    else:
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
            
            if abstention:
                # Use abstention-specific judge prompt
                prompt = get_abstention_prompt(e["question"], h["hypothesis"])
            else:
                # Use standard answer-check prompt
                prompt = get_anscheck_prompt(
                    e["question_type"], e["question"], e["answer"], h["hypothesis"],
                    abstention=abstention)
            
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
```

- [ ] **Step 2: Test judge with abstention questions**

Run: `python bench/longmemeval/phase4/phase4_run.py judge --dataset <full-500-question-dataset> --hyp test_hyp.jsonl --provider openai --model gpt-4o`
Expected: Judges all 500 questions, including 30 abstention with abstention-specific prompts

- [ ] **Step 3: Commit**

```bash
git add bench/longmemeval/phase4/phase4_run.py bench/longmemeval/phase4/abstention_prompt.py
git commit -m "feat(bench): use abstention-specific judge prompt for _abs questions"
```

---

## Task 4: Add Blended Accuracy Reporting

**Files:**
- Modify: `bench/longmemeval/phase4/phase4_run.py:269-284`

**Interfaces:**
- Consumes: eval-results JSONL (all 500 questions with abstention field)
- Produces: console output with separate accuracy metrics

- [ ] **Step 1: Modify _report to show blended accuracy**

```python
def _report(judged_path):
    rows = [json.loads(l) for l in open(judged_path) if l.strip()]
    if not rows:
        sys.exit(f"error: no rows in {judged_path}")
    
    # Split into abstention and non-abstention
    non_abstention = [r for r in rows if not r["abstention"]]
    abstention = [r for r in rows if r["abstention"]]
    
    # Calculate accuracies
    overall_labels = [1 if r["autoeval_label"] else 0 for r in rows]
    non_abstention_labels = [1 if r["autoeval_label"] else 0 for r in non_abstention]
    abstention_labels = [1 if r["autoeval_label"] else 0 for r in abstention]
    
    overall_acc = sum(overall_labels) / len(overall_labels) if overall_labels else 0
    non_abstention_acc = sum(non_abstention_labels) / len(non_abstention_labels) if non_abstention_labels else 0
    abstention_acc = sum(abstention_labels) / len(abstention_labels) if abstention_labels else 0
    
    # Per-type breakdown (non-abstention only)
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
```

- [ ] **Step 2: Test report output**

Run: `python bench/longmemeval/phase4/phase4_run.py report --hyp test_hyp.jsonl --model gpt-4o`
Expected: Shows three accuracy numbers: blended, non-abstention, and abstention

- [ ] **Step 3: Commit**

```bash
git add bench/longmemeval/phase4/phase4_run.py
git commit -m "feat(bench): report blended 500-question accuracy alongside 470-question"
```

---

## Task 5: Update Documentation

**Files:**
- Modify: `docs/benchmarks.md:107-118`

**Interfaces:**
- Consumes: none
- Produces: updated methodology documentation

- [ ] **Step 1: Update Phase 4 section in benchmarks.md**

Add after the existing Phase 4 description:

```markdown
### Abstention Scoring

The official LongMemEval benchmark includes 30 abstention questions (question_id suffix `_abs`) where the correct behavior is to decline answering. Most third-party implementations score abstention via LLM judge (did the system correctly refuse?) and fold it into headline accuracy.

**Previous approach:** Excluded abstention from scoring (470-question accuracy). This inflated scores compared to competitors who include abstention.

**Current approach:** 
- Phase 1 (retrieval-only): Excludes abstention (IR metrics can't evaluate "no evidence" questions)
- Phase 4 (end-to-end): Includes all 500 questions with abstention-specific judge prompts
- Reports three accuracy numbers:
  - **Blended (500-question):** Fair comparison with competitors
  - **Non-abstention (470-question):** Backward-compatible with previous reports
  - **Abstention (30-question):** Shows refusal accuracy

**Why this matters:** Abstention is typically where retrieval-heavy systems perform worst — over-eager retrieval hallucinates answers instead of declining. Including abstention in the score provides a more honest assessment of system capabilities.
```

- [ ] **Step 2: Commit**

```bash
git add docs/benchmarks.md
git commit -m "docs(bench): document abstention scoring methodology"
```

---

## Task 6: Add Phase 1 Abstention Flag (Optional)

**Files:**
- Modify: `bench/longmemeval/main.go:201-210`
- Modify: `bench/longmemeval/main.go:274-278`

**Interfaces:**
- Consumes: command-line flags
- Produces: optional abstention inclusion in Phase 1

- [ ] **Step 1: Add --include-abstention flag**

```go
includeAbstention := flag.Bool("include-abstention", false, "include abstention questions in Phase 1 scoring (not recommended for IR metrics)")
```

- [ ] **Step 2: Modify scoring loop to respect flag**

```go
for _, q := range questions {
    if isAbstention(q.QuestionID) && !*includeAbstention {
        skippedAbstention++
        continue
    }
    // ... rest of scoring logic
}
```

- [ ] **Step 3: Commit**

```bash
git add bench/longmemeval/main.go
git commit -m "feat(bench): add --include-abstention flag to Phase 1 (default off)"
```

---

## Verification

After implementation:

1. **Run Phase 4 on full dataset:**
   ```bash
   python bench/longmemeval/phase4/phase4_run.py generate \
     --dataset longmemeval_s_cleaned.json \
     --out hyp_full.jsonl \
     --provider openai --model gpt-4o
   
   python bench/longmemeval/phase4/phase4_run.py judge \
     --dataset longmemeval_s_cleaned.json \
     --hyp hyp_full.jsonl \
     --provider openai --model gpt-4o
   ```

2. **Verify output shows three accuracy numbers:**
   - Blended (500-question)
   - Non-abstention (470-question)
   - Abstention (30-question)

3. **Compare with competitor scores:**
   - Ghost blended accuracy vs Mem0 94.4%, Hindsight 91.4%, Supermemory 85.2%
   - Document the comparison in benchmarks.md

---

## Success Criteria

- [ ] Phase 4 processes all 500 questions (including 30 abstention)
- [ ] Abstention questions use abstention-specific judge prompts
- [ ] Report shows three accuracy numbers: blended, non-abstention, abstention
- [ ] Documentation updated to explain blended scoring
- [ ] Fair comparison with competitor scores enabled
