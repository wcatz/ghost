package main

// PinsOverBucket is the store-efficiency arc for #1831: across sessions the owner
// pins more rules than the project's session-start bucket holds (more than
// sessionMemoriesCap, plus one long pin). The final session must be told about
// the overflow in the pin result or the health output. The graded clause is that
// the owner was told, not that every pin appears.
func PinsOverBucket() Storyline {
	// 16 distinct pinned rules plus one long pinned rule = 17 pinned memories,
	// the bucket holds 15. The final session's block should tell the owner that
	// pinned rows were cut.
	pins := make([]Record, 0, 17)
	for i := 1; i <= 16; i++ {
		key := pinsKey(i)
		mark := pinsMark(i)
		content := "Rule " + mark + ": this is pinned rule number " + mark + " for atlas-ledger; keep this invariant under all edits."
		pins = append(pins, Record{
			Key:      key,
			Category: "convention",
			Content:  content,
			Tags:     []string{"atlas-ledger", "pin"},
			Mark:     mark,
			Pin:      true,
		})
	}
	// Long pin: content well over the per-item preview budget (200 bytes for
	// project memories) so it also stresses the byte-fit path.
	longMark := "LONG-PIN-RULE-EXTRA-LARGE-FOR-ATLAS"
	longContent := "Rule " + longMark + ": " + longString(480) + " keep this long invariant pinned for atlas-ledger."
	pins = append(pins, Record{
		Key:      "pin-long",
		Category: "convention",
		Content:  longContent,
		Tags:     []string{"atlas-ledger", "pin"},
		Mark:     longMark,
		Pin:      true,
	})

	// Split pins across three pinning sessions, then a final reading session.
	return Storyline{
		Key:     "pins-over-bucket",
		Title:   "Pinning more rules than the session-start bucket holds must be reported",
		Project: "atlas-ledger",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "atlas-ledger is the double-entry ledger for Atlas; it posts every settlement.",
				Tags:     []string{"atlas-ledger"},
				Mark:     "double-entry ledger for Atlas",
			},
		},
		Stages: []Stage{
			{
				Script:  "First session on atlas-ledger. You are recording the invariants the ledger must keep.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what atlas-ledger does.",
				Records: pins[0:6],
			},
			{
				Script:  "Second session on atlas-ledger. You are adding more ledger invariants.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what atlas-ledger does.",
				Records: pins[6:12],
			},
			{
				Script:  "Third session on atlas-ledger. You are adding the remaining invariants, including a very long rule.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what atlas-ledger does.",
				Records: pins[12:17],
			},
			{
				Script: "Fourth session on atlas-ledger. You are writing the runbook for a new engineer.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- how many pinned rules the ledger has and which ones are shown.",
				// No Expect: the graded clause is that the owner was told about the
				// overflow, not that every pin appears. The final block should
				// contain the pinned-cut notice.
			},
		},
		KnownFailing: map[string]string{
			"pins-overflow-noticed:pin-result": "issue #1831: pinning more than sessionMemoriesCap (plus one long pin) is not reported in the pin result or the health output",
		},
	}
}

func pinsKey(i int) string {
	return "pin-" + twoDigits(i)
}

func pinsMark(i int) string {
	return "PIN-RULE-" + twoDigits(i) + "-ATLAS"
}

func twoDigits(i int) string {
	if i < 10 {
		return "0" + itoa(i)
	}
	return itoa(i)
}

func itoa(i int) string {
	// small helper without importing strconv in this file's init
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func longString(n int) string {
	// deterministic long filler, no secrets, no randomness
	s := ""
	for len(s) < n {
		s += "ABCD-1234-"
	}
	return s[:n]
}

// DuplicateNeverMerged is the store-efficiency arc for #1834: two sessions save
// the same rule in near-identical words. After the lifecycle phases run (fake
// harness), the store must hold one live row for the rule, and the session block
// shows it once.
func DuplicateNeverMerged() Storyline {
	return Storyline{
		Key:     "duplicate-never-merged",
		Title:   "Two near-identical saves of one rule must leave one live row and one block line",
		Project: "fern-api",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "fern-api is the edge API for the Fern shipping portal.",
				Tags:     []string{"fern-api"},
				Mark:     "Fern shipping portal",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on fern-api. You are documenting the platform guard.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what fern-api is.",
				Records: []Record{
					{
						Key:      "deploy-guard",
						Category: "convention",
						Content:  "Production schema changes require explicit approval from the platform team before rollout.",
						Tags:     []string{"fern-api", "deploy"},
						Mark:     "explicit approval from the platform team before rollout",
					},
				},
			},
			{
				Script: "Second session on fern-api. You are documenting the same guard in slightly different words.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what fern-api is.",
				Records: []Record{
					{
						Key:      "deploy-guard-dup",
						Category: "convention",
						Content:  "Explicit approval from the platform team is required before rolling out production schema changes.",
						Tags:     []string{"fern-api", "deploy"},
						Mark:     "Explicit approval from the platform team is required before rolling out",
					},
				},
				Lifecycle: true,
			},
			{
				Script: "Third session on fern-api. You are writing the pre-deploy checklist.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- the guard for production schema changes.",
			},
		},
		KnownFailing: map[string]string{
			"duplicate-merged:block-shows-once": "issue #1834: near-identical duplicate is not merged, so the block shows the rule twice",
			"duplicate-merged:store-holds-one":  "issue #1834: near-identical duplicate leaves two live rows instead of one",
		},
	}
}

// DatedGlobalStatus is the store-efficiency arc for #1832: a session saves a
// dated rollout status line with ghost_save_global. A later session in a
// different project must not carry that line in its session block once its
// valid_until passes. The save result carries the advisory when no valid_until
// was given.
func DatedGlobalStatus() Storyline {
	return Storyline{
		Key:     "dated-global-status",
		Title:   "A dated global rollout status must leave other projects' blocks after it expires",
		Project: "relay-ops",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "relay-ops is the rollout dashboard for the Relay fleet.",
				Tags:     []string{"relay-ops"},
				Mark:     "rollout dashboard for the Relay fleet",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on relay-ops. You are recording the rollout status for the fleet.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what relay-ops is.",
				Records: []Record{
					{
						Key:        "rollout-status",
						Category:   "fact",
						Content:    "Rollout status 2026-10-26: Relay v2.3.1 is live in prod, rollback window closes at 2026-10-27T00:00:00Z.",
						Tags:       []string{"relay-ops", "rollout"},
						Mark:       "Relay v2.3.1 is live in prod",
						Global:     true,
						ValidUntil: "2026-10-27T00:00:00Z",
					},
				},
			},
			{
				Script: "Second session on relay-ops. You are recording a second status without a date.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what relay-ops is.",
				Records: []Record{
					{
						Key:      "rollout-status-undated",
						Category: "fact",
						Content:  "Rollout status: Relay v2.3.2 is live in prod, no rollback window was recorded.",
						Tags:     []string{"relay-ops", "rollout"},
						Mark:     "Relay v2.3.2 is live in prod",
						Global:   true,
						// No ValidUntil: the save should advise that a dated
						// status belongs with a valid_until.
					},
				},
			},
			{
				Script: "Third session on relay-ops, now checking from a different checkout that should not see the expired global.\n\nAnswer in at most two sentences, using only what this session's context already holds:\n- what relay-ops is.",
				// The final block is the one a different project's session would
				// be handed: the expired global must not be there.
			},
		},
		KnownFailing: map[string]string{
			"dated-global-advisory:rollout-status-undated": "issue #1832: saving a dated rollout status without valid_until does not advise to add one",
		},
	}
}
