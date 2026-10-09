# PR.md — Before/after `ghost bench` output for #964

## Before (origin/main)

```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.467   0.625   0.697    0.836    0.749
vector-only      0.503   0.694   0.764    0.882    0.800
hybrid           0.520   0.712   0.763    0.902    0.818

220 graded queries, 551 memories. Retrieval-only, no LLM judge.

no-answer queries (n=24, nothing in the corpus answers these; report-only, no gate)
  A FALSE POSITIVE is a result returned for a query with no answer, and the false-positive rate is
  the share of queries with at least one returned row above that cosine floor. search.min_similarity
  ships 0, which refuses nothing, so at the shipped setting the rate is 1.000 in every condition;
  the floors below are the graded reading, and they are the band an abstention rule would live in.

  condition       results    mean top  max top  rate @0.30  rate @0.40  rate @0.50
  fts-only           10.0       0.548    0.697       1.000       1.000       0.625
  vector-only        10.0       0.584    0.697       1.000       1.000       0.875
  hybrid             10.0       0.584    0.697       1.000       1.000       0.875

Fused vs one leg at a time, paired per query (95% percentile bootstrap, 20000 resamples):
comparison                        mean        lo        hi  queries
fused - vector-only            +0.0179   +0.0028   +0.0335   220  ahead of the leg
fused - fts-only               +0.0686   +0.0466   +0.0919   220  ahead of the leg
A row whose interval contains 0.0 does not separate the two conditions; the sort above cannot either.

abstention baseline (hybrid path, the shipped ranking)
  results returned per query   10.0 (window 10, no similarity floor configured)
  mean top cosine             0.584  vs 0.741 for the 220 answerable queries
  floor refusing all of them   0.697 (the no-answer maximum) costs 51/220 answerable queries

  flavor          n    results/query     mean top
  near_miss      12             10.0        0.623
  off_domain     12             10.0        0.545

  floor     results/query     queries w/ hit     rate
  0.30              10.00         24/24       1.000
  0.40               9.79         24/24       1.000
  0.50               6.83         21/24       0.875
```

## After (this branch)

```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.467   0.625   0.697    0.836    0.749
vector-only      0.503   0.694   0.764    0.882    0.800
hybrid           0.520   0.712   0.763    0.902    0.818

top-row shares over the 220 answerable queries
  top row relevant           fts-only     0.786
  top row best-labelled      fts-only     0.709
  top row relevant           vector-only  0.841
  top row best-labelled      vector-only  0.768
  top row relevant           hybrid       0.868
  top row best-labelled      hybrid       0.800
  R@1 ceiling on these labels 0.599
R@1 divides by the labelled rows, not by 1, so a query grading four
scores 0.250 at R@1 however well it ranks. The ceiling is what a perfect
ranking posts on these labels; read every R@1 above against it.

220 graded queries, 551 memories. Retrieval-only, no LLM judge.

no-answer queries (n=24, nothing in the corpus answers these; report-only, no gate)
  A FALSE POSITIVE is a result returned for a query with no answer, and the false-positive rate is
  the share of queries with at least one returned row above that cosine floor. search.min_similarity
  ships 0, which refuses nothing, so at the shipped setting the rate is 1.000 in every condition;
  the floors below are the graded reading, and they are the band an abstention rule would live in.

  condition       results    mean top  max top  rate @0.30  rate @0.40  rate @0.50
  fts-only           10.0       0.548    0.697       1.000       1.000       0.625
  vector-only        10.0       0.584    0.697       1.000       1.000       0.875
  hybrid             10.0       0.584    0.697       1.000       1.000       0.875

Fused vs one leg at a time, paired per query (95% percentile bootstrap, 20000 resamples):
comparison                        mean        lo        hi  queries
fused - vector-only            +0.0179   +0.0028   +0.0335   220  ahead of the leg
fused - fts-only               +0.0686   +0.0466   +0.0919   220  ahead of the leg
A row whose interval contains 0.0 does not separate the two conditions; the sort above cannot either.

abstention baseline (hybrid path, the shipped ranking)
  results returned per query   10.0 (window 10, no similarity floor configured)
  mean top cosine             0.584  vs 0.741 for the 220 answerable queries
  floor refusing all of them   0.697 (the no-answer maximum) costs 51/220 answerable queries

  flavor          n    results/query     mean top
  near_miss      12             10.0        0.623
  off_domain     12             10.0        0.545

  floor     results/query     queries w/ hit     rate
  0.30              10.00         24/24       1.000
  0.40               9.79         24/24       1.000
  0.50               6.83         21/24       0.875
```

## Summary of changes

The table itself (the first three data lines with R@1, R@5, R@10, MRR@10, NDCG@10) is **unchanged** — other tools parse it, so no existing line was modified.

New lines added **after the table**:
- `top row relevant` — share of answerable queries whose first result is relevant (fts-only 0.786, vector-only 0.841, hybrid 0.868)
- `top row best-labelled` — share whose first result carries the highest labelled gain (fts-only 0.709, vector-only 0.768, hybrid 0.800)
- `R@1 ceiling on these labels` — mean of 1/(labelled rows per query) over the 220 answerable queries = 0.599
- Explanatory note: "R@1 divides by the labelled rows, not by 1..."

Per the issue: R@1 is recall@1 (fraction of labelled rows found at rank 1), not "right row first". On this corpus, 163/220 queries label 2–4 rows, so a perfect ranking scores 0.599 at R@1. The shipped hybrid R@1 of 0.520 is 87% of that ceiling, not 52%.