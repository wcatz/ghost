# PR for #965: bench: fix 9 wrong labels, review 6 ambiguous queries, drop duplicate corpus rows

This is a **re-baseline** of the benchmark floors after correcting the label set and dropping duplicate corpus rows. The ranking is unchanged — only the labels and corpus were corrected.

## Changes

### 9 Clear Label Corrections (queries.jsonl)
| line | query | change |
|---|---|---|
| 13 | `q_consolidate` | add `restore_snapshot:3`, `empty_set_guard:1` |
| 15 | `q_git_policy` | add `pr_always:3` |
| 19 | `q_bp_exposure` | `relay_topology 1 → 3` |
| 64 | `q_error_budget_use` | `error_budget_freeze 1 → 3` |
| 89 | `q_k3s_join` | `k3s_native_tailscale 1 → 3` |
| 90 | `q_runners` | `arc_self_hosted 1 → 3` |
| 105 | `q_runbook_alert` | `alert_runbook_link 1 → 3` |
| 145 | `q_image_scan` | `vuln_scan_pr 1 → 3` |
| 212 | `q_manual_survive` | `source_reflection 1 → 3` |

### 6 Ambiguous Queries — proposed change applied
| line | query | change |
|---|---|---|
| 81 | `q_docs_location` | add `decision_record_tool:1` |
| 83 | `q_claim_done` | add `evidence_or_unknown:2` |
| 98 | `q_pin_behavior` | `pin_survives_reflect 1 → 3` |
| 128 | `q_model_promote` | `eval_harness 1 → 3`, `canary_model 1 → 2` |
| 140 | `q_flaky_policy` | `flaky_file_issue 1 → 2` |
| 172 | `q_degrade_order` | `load_shed 1 → 2` |

### Corpus Duplicates Dropped (memories.jsonl)
- `no_main_direct` (identical to `no_main_push`) — dropped
- `yaml_two_space` (identical to `yaml_indent`) — dropped
- `grafana_port` / `grafana_ingress_port` (Jaccard 0.92) — both labeled on `q_monitoring`
- `arc_runners` / `arc_self_hosted` (Jaccard 0.78) — both labeled on `q_runners`

The two dropped keys had their labels re-pointed to the surviving key with the max of the two gains. No test or --context/--passive corpus depends on the dropped keys.

### Re-floored TestBenchRegressionFloors
Floors moved with the measurements, keeping the same per-row margin each already carried.

| Condition | Old Floor (NDCG/Recall@10) | New Floor (NDCG/Recall@10) | Observed (new) | Margin kept |
|---|---|---|---|---|
| fts-only | 0.73 / 0.67 | 0.74 / 0.67 | 0.759 / 0.702 | 0.019 / 0.032 |
| vector-only | 0.78 / 0.75 | 0.79 / 0.75 | 0.814 / 0.766 | 0.024 / 0.016 |
| hybrid | 0.80 / 0.75 | 0.81 / 0.75 | 0.831 / 0.765 | 0.021 / 0.015 |

## Before / After `ghost bench`

### Before (v2: 551 memories)
```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.467   0.625   0.697    0.836    0.749
vector-only      0.503   0.694   0.764    0.882    0.800
hybrid           0.520   0.712   0.763    0.902    0.818

220 graded queries, 551 memories. Retrieval-only, no LLM judge.

Fused vs one leg at a time, paired per query (95% percentile bootstrap, 20000 resamples):
comparison                        mean        lo        hi  queries
fused - vector-only            +0.0179   +0.0028   +0.0335   220  ahead of the leg
fused - fts-only               +0.0686   +0.0466   +0.0919   220  ahead of the leg
```

### After (v3: 549 memories)
```
condition          R@1     R@5    R@10   MRR@10  NDCG@10
fts-only         0.466   0.630   0.702    0.838    0.759
vector-only      0.507   0.698   0.766    0.894    0.814
hybrid           0.522   0.718   0.765    0.911    0.831

220 graded queries, 549 memories. Retrieval-only, no LLM judge.

Fused vs one leg at a time, paired per query (95% percentile bootstrap, 20000 resamples):
comparison                        mean        lo        hi  queries
fused - vector-only            +0.0175   +0.0029   +0.0325   220  ahead of the leg
fused - fts-only               +0.0722   +0.0504   +0.0951   220  ahead of the leg
```

## Before / After `ghost bench --context`

### Before
```
context assembly (ghost_memory_search's own block: 10 items / 16000 bytes; report-only, no gate)
  project bench, 220 queries measured, 220 answered, 2200 admitted rows
  context precision          0.138 (304/2200)
  item cap trimmed          0.499 (2191/4391); 0.082 (27/331) of the graded ones among them
  estimated tokens         296.609 (65254/220)
  rendered bytes           2232.964 (491252/220)
  largest response                 2411 bytes
  largest block, tokens            336 (est.)
```

### After
```
context assembly (ghost_memory_search's own block: 10 items / 16000 bytes; report-only, no gate)
  project bench, 220 queries measured, 220 answered, 2200 admitted rows
  context precision          0.141 (310/2200)
  item cap trimmed          0.499 (2191/4391); 0.077 (26/336) of the graded ones among them
  estimated tokens         296.850 (65307/220)
  rendered bytes           2233.932 (491465/220)
  largest response                 2438 bytes
  largest block, tokens            346 (est.)
```

## Before / After `ghost bench --passive`

**Identical** — the passive corpus is synthetic and unchanged.

```
passive context: 217 rows, 4 projects + _global, clock 2026-06-01T12:00:00Z

session start / ghost context (4 blocks)
  withheld leakage       0.000 (0/55)   (must be 0)
  expected-row recall    1.000 (67/67)
  cross-project          0.000 (0/88)
  _global share          0.364 (32/88)
  budget use             0.957 (88/92)
  budget cut             0.642 (158/246)
  duplicate rate         0.045 (4/88)
  header honesty         PASS   (8 count lines agree)

session start, scope env=production (4 blocks)
  withheld leakage       0.000 (0/66)   (must be 0)
  expected-row recall    1.000 (67/67)
  cross-project          0.000 (0/87)
  _global share          0.368 (32/87)
  budget use             0.946 (87/92)
  budget cut             0.630 (148/235)
  duplicate rate         0.046 (4/87)
  header honesty         PASS   (8 count lines agree)

ghost_project_context (limit 20) (4 blocks)
  withheld leakage       0.000 (0/55)   (must be 0)
  expected-row recall    1.000 (64/64)
  cross-project          0.000 (0/80)
  _global share          0.463 (37/80)
  budget use             1.000 (80/80)
  budget cut             0.675 (166/246)
  duplicate rate         0.000 (0/80)

project resource / recall_project (4 blocks)
  withheld leakage       0.000 (0/55)   (must be 0)
  expected-row recall    1.000 (64/64)
  cross-project          0.000 (0/103)
  _global share          0.583 (60/103)
  budget use             0.736 (103/140)
  budget cut             0.581 (143/246)
  duplicate rate         0.000 (0/103)
```

## Verification
- `go vet ./internal/bench/... ./cmd/ghost/...` — clean
- `gofmt -l .` — clean
- `go test -race ./internal/bench -run '^Test[^L]' -count=1 -p 1` — PASS
- `go test ./cmd/ghost -count=1` — PASS
- `go test -tags e2e ./e2e/ -count=1` — PASS
- `TestBenchRegressionFloors` — PASS with re-floored values