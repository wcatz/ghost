package main

// CompactionContinuity is the mid-task compaction arc for #2164. The first
// session leaves a task in flight and an open question. A compaction happens
// before the second session opens; the next session must still see the task and
// the question through the handoff, even though the ranked block was condensed.
// The graded property is continuity of the in-flight state, not the presence of
// any one memory row.
func CompactionContinuity() Storyline {
	const taskMark = "RELAY-PORT-DECISION-2164"
	return Storyline{
		Key:     "compaction-continuity",
		Title:   "A mid-task compaction must hand the in-flight state to the next session",
		Project: "atlas-relay",
		Opening: []Record{
			{
				Key:      "relay-owner",
				Category: "fact",
				Content:  "atlas-relay forwards settlement events between the ledger and the warehouse.",
				Tags:     []string{"atlas-relay"},
				Mark:     "forwards settlement events",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on atlas-relay. Record the decision the team is in the middle of making, then stop.",
				Records: []Record{
					{
						Key:      "relay-task",
						Category: "decision",
						Content:  "Open decision " + taskMark + ": pick the relay listener port before tomorrow's warehouse cutover.",
						Tags:     []string{"atlas-relay", "in-flight"},
						Mark:     taskMark,
					},
				},
			},
			{
				Script:        "The host compacted this session's history mid-task. Answer in at most two sentences, using only what the post-compaction injection carries: when is the warehouse cutover?",
				Compact:       true,
				CompactScript: "Still working on " + taskMark + ": the listener port is not chosen yet and the warehouse cutover is tomorrow.",
				// The cutover is in the transcript, not in a saved memory: the
				// measurement reads it from the post-compaction injection, where
				// it can only have arrived through the handoff. The answer grade
				// asks for the same in-flight fact the handoff carries.
				Carries: []AnswerCheck{{Name: "in-flight-cutover", Any: []string{"tomorrow"}}},
			},
		},
	}
}
