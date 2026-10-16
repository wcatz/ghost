package main

// The truth-change arcs. Each one changes what is true about the project partway
// through, and grades whether the sessions after the change — and a reader's
// search — are told the new truth rather than the old one.
//
// Three of them run the lifecycle after the stage that records the change
// (Stage.Lifecycle), the order a host's Stop hook gives a project whose
// lifecycle is enabled, so a stale line in a later block is a finding about a
// store the lifecycle has already read, not about the runner's ordering.
// version-upgrade deliberately does not: automatic lifecycle phases are off by
// default, so it grades the session after the change on the store a default
// install hands it, where only the save path has run.
//
// Each arc is written so its absence checks cannot be failed by its own
// wording: a rename or an upgrade note restates the old name or pin, so the
// superseded record's Mark is a phrase only that record contains (Validate
// refuses one that is not).

// RenamedEndpoint is the rename arc: an endpoint and its handler are renamed in
// session 2, the new note naming the old path as gone. Session 3 must be told
// the new name and not the old one, and a search for the OLD name must lead to
// the new record rather than to the one it replaced.
func RenamedEndpoint() Storyline {
	return Storyline{
		Key:     "renamed-endpoint",
		Title:   "A renamed endpoint must stop being presented under its old name",
		Project: "harbor-gateway",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "harbor-gateway is the public edge API for the Harbor shipping portal; partners call it with signed requests.",
				Tags:     []string{"harbor-gateway"},
				Mark:     "Harbor shipping portal",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on harbor-gateway. You are documenting how partners submit shipment events.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- how a partner submits shipment events.",
				Records: []Record{
					{
						Key:          "ingest-endpoint",
						Category:     "architecture",
						Content:      "Partners submit shipment events with POST /v1/ingest, served by ingestHandler in api/ingest.go; the body is a JSON array of up to 500 events.",
						Tags:         []string{"harbor-gateway", "api"},
						Mark:         "ingestHandler in api/ingest.go",
						SupersededBy: "events-endpoint",
					},
				},
			},
			{
				Script: "Second session on harbor-gateway. The partner API was versioned this sprint and you have just been told what moved.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what partners called before the change.",
				Expect: []string{"ingest-endpoint"},
				Records: []Record{
					{
						Key:      "events-endpoint",
						Category: "architecture",
						Content:  "The shipment-event endpoint was renamed: partners now call POST /v2/events, served by eventsHandler in api/events.go, and the old /v1/ingest path returns 410 Gone.",
						Tags:     []string{"harbor-gateway", "api"},
						Mark:     "POST /v2/events",
					},
				},
				Lifecycle: true,
			},
			{
				Script: "Third session on harbor-gateway. A partner's integration is failing and you are writing them the call they should make.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the method and path a partner calls to submit shipment events, and\n" +
					"- which handler serves it.",
				Expect:  []string{"events-endpoint"},
				Carries: []AnswerCheck{{Name: "current-path", Any: []string{"/v2/events"}}},
				Avoids:  []AnswerCheck{{Name: "old-handler", Any: []string{"ingestHandler"}}},
			},
		},
		Probes: []Probe{
			{
				// The old name is what a reader with an old link, an old log line or
				// an old ticket searches for.
				Name:  "old-name",
				Query: "ingest endpoint ingestHandler",
				Want:  "events-endpoint",
				Over:  []string{"ingest-endpoint"},
			},
		},
		Judge: "Did the session give the current endpoint (%s: %s) rather than the one it replaced?",
	}
}

// RegressedFix is the regression arc: a crash recorded in session 1 is fixed in
// session 2, holds through session 3, and comes back in session 4. While the fix
// holds the crash note is stale; once it regresses the fix note is, and session
// 5 — the first one after the regression — must be told the crash is back and
// must not be told the fix still holds.
func RegressedFix() Storyline {
	return Storyline{
		Key:     "regressed-fix",
		Title:   "A fix that regressed must not be reported as still holding",
		Project: "tally-exporter",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "tally-exporter is the nightly job that exports per-tenant usage to the billing warehouse.",
				Tags:     []string{"tally-exporter"},
				Mark:     "per-tenant usage",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on tally-exporter. Last night's export died and you have just read the panic.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what the nightly export job does.",
				Records: []Record{
					{
						Key:          "export-crash",
						Category:     "gotcha",
						Content:      "The nightly export panics with 'assignment to entry in nil map' when a tenant has no usage rows; the job exits 2 and the warehouse misses that night's load.",
						Tags:         []string{"tally-exporter", "export"},
						Mark:         "the warehouse misses that night's load",
						SupersededBy: "export-fixed",
					},
				},
			},
			{
				Script: "Second session on tally-exporter. The change for the nightly crash merged today.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what made the nightly export crash.",
				Expect: []string{"export-crash"},
				Records: []Record{
					{
						Key:          "export-fixed",
						Category:     "fact",
						Content:      "Fixed in commit 4f2a9c1: the exporter initialises the per-tenant totals map before the loop, so a tenant with no usage rows no longer crashes the nightly export; the job has been green since.",
						Tags:         []string{"tally-exporter", "export"},
						Mark:         "Fixed in commit 4f2a9c1",
						SupersededBy: "export-regressed",
					},
				},
				Lifecycle: true,
			},
			{
				Script: "Third session on tally-exporter. You are reviewing tonight's export run plan.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- whether tenants with no usage rows still crash the export, and which commit says so.",
				Expect:  []string{"export-fixed"},
				Carries: []AnswerCheck{{Name: "fix-commit", Any: []string{"4f2a9c1"}}},
			},
			{
				Script: "Fourth session on tally-exporter. Last night's export failed again, and you have just traced it.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what the last recorded change to the crash was.",
				Expect: []string{"export-fixed"},
				Records: []Record{
					{
						Key:      "export-regressed",
						Category: "gotcha",
						Content:  "Regression: the nightly export panics again with 'assignment to entry in nil map' when a tenant has no usage rows; the tenant-loader refactor in commit 9be71d0 dropped the totals-map initialisation that 4f2a9c1 added, so the 4f2a9c1 change no longer holds.",
						Tags:     []string{"tally-exporter", "export"},
						Mark:     "commit 9be71d0",
					},
				},
				Lifecycle: true,
			},
			{
				Script: "Fifth session on tally-exporter. You are deciding whether tonight's export can run unattended.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- whether tenants with no usage rows crash the export, and which commit is responsible.",
				Expect:  []string{"export-regressed"},
				Carries: []AnswerCheck{{Name: "regression-commit", Any: []string{"9be71d0"}}},
			},
		},
		Probes: []Probe{
			{
				Name:  "crash-status",
				Query: "nightly export crash tenants with no usage rows",
				Want:  "export-regressed",
				Over:  []string{"export-fixed"},
			},
		},
		Judge: "Did the session report the current state of the crash (%s: %s) rather than an earlier one?",
	}
}

// VersionUpgrade is the dependency arc: the database driver is upgraded a major
// version in session 2. The two notes are near-identical sentences about one
// pin, which is exactly the shape a near-duplicate check is built to fold, so
// the arc also grades that the upgrade was stored as a replacement rather than
// as a second wording of the old pin.
//
// It runs with the lifecycle OFF between sessions — the default — so session 3
// is graded on what the save path alone left behind: its block, and the search
// it would run before touching the driver code.
func VersionUpgrade() Storyline {
	return Storyline{
		Key:     "version-upgrade",
		Title:   "An upgraded dependency pin must stop appearing as current",
		Project: "quill-ledger",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "quill-ledger is the double-entry ledger service; it talks to Postgres 16.",
				Tags:     []string{"quill-ledger"},
				Mark:     "double-entry ledger service",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on quill-ledger. You are setting up the database layer.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- which database the ledger talks to.",
				Records: []Record{
					{
						Key:          "pgx-pin",
						Category:     "dependency",
						Content:      "go.mod pins github.com/jackc/pgx/v4 at v4.18.1; the pool is built with pgxpool.Connect and the code imports pgx/v4 everywhere.",
						Tags:         []string{"quill-ledger", "deps"},
						Mark:         "v4.18.1",
						SupersededBy: "pgx-upgrade",
					},
				},
			},
			{
				Script: "Second session on quill-ledger. The driver upgrade merged this morning.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- which driver version the ledger pinned before the upgrade.",
				Expect: []string{"pgx-pin"},
				Records: []Record{
					{
						Key:      "pgx-upgrade",
						Category: "dependency",
						Content:  "go.mod now pins github.com/jackc/pgx/v5 at v5.6.0; the pool is built with pgxpool.New and every import moved to pgx/v5.",
						Tags:     []string{"quill-ledger", "deps"},
						Mark:     "v5.6.0",
					},
				},
				// No Lifecycle: see the doc comment.
			},
			{
				Script: "Third session on quill-ledger. You are adding a new repository type and need the driver import and the pool constructor.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the driver version go.mod pins, and\n" +
					"- the pool constructor to call.",
				Expect:  []string{"pgx-upgrade"},
				Carries: []AnswerCheck{{Name: "current-pin", Any: []string{"v5.6.0"}}},
				Avoids:  []AnswerCheck{{Name: "old-pin", Any: []string{"v4.18.1"}}},
				Probes: []Probe{
					{
						Name:  "driver-pin",
						Query: "pgx version pinned in go.mod",
						Want:  "pgx-upgrade",
						Over:  []string{"pgx-pin"},
					},
					{
						// The old spelling is what a reader with an old build log or an
						// old import line searches for.
						Name:  "old-constructor",
						Query: "pgxpool.Connect pgx v4",
						Want:  "pgx-upgrade",
						Over:  []string{"pgx-pin"},
					},
				},
			},
		},
		Probes: []Probe{
			{
				Name:  "driver-pin",
				Query: "pgx version pinned in go.mod",
				Want:  "pgx-upgrade",
				Over:  []string{"pgx-pin"},
			},
			{
				Name:  "old-constructor",
				Query: "pgxpool.Connect pgx v4",
				Want:  "pgx-upgrade",
				Over:  []string{"pgx-pin"},
			},
		},
		Judge: "Did the session use the current driver pin (%s: %s) rather than the one it replaced?",
	}
}

// ScopedSetting is the scope arc: one setting has a different value in staging
// and in production, and the two notes say so only through their scope — their
// prose names no environment, which is how an agent writing from inside one
// environment naturally phrases it. A production session must not be handed the
// staging value, a staging session must not be handed the production one, and
// the two values are both true, so nothing may let one supersede the other (the
// pair is a Lure: the fake classifier calls it a supersession, and only Ghost's
// own scope guard can refuse it).
func ScopedSetting() Storyline {
	staging := map[string]string{"environment": "staging"}
	production := map[string]string{"environment": "production"}
	return Storyline{
		Key:     "scoped-setting",
		Title:   "A production session must not be shown a staging value as applying",
		Project: "orbit-search",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "orbit-search is the product search API; it runs in a staging and a production environment from one image.",
				Tags:     []string{"orbit-search"},
				Mark:     "product search API",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on orbit-search, working in staging. You tuned the database pool for the staging load test.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what orbit-search is.",
				SessionScope: staging,
				Records: []Record{
					{
						Key:      "pool-staging",
						Category: "fact",
						Content:  "The database pool is capped at max_conns=24; the load test showed extra connections only queue at the small database.",
						Tags:     []string{"orbit-search", "database"},
						Mark:     "max_conns=24",
						Scope:    staging,
					},
				},
			},
			{
				Script: "Second session on orbit-search, working in production. The on-call lead set the production pool after last week's saturation incident.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what orbit-search is.",
				SessionScope: production,
				Withheld:     []string{"pool-staging"},
				Records: []Record{
					{
						Key:      "pool-production",
						Category: "fact",
						Content:  "The database pool is capped at max_conns=180; below that the request queue saturates during the evening peak.",
						Tags:     []string{"orbit-search", "database"},
						Mark:     "max_conns=180",
						Scope:    production,
					},
				},
				Lifecycle: true,
			},
			{
				Script: "Third session on orbit-search, working in production. You are sizing the connection pool for a new replica.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the pool cap the replica should use.",
				SessionScope: production,
				Expect:       []string{"pool-production"},
				Withheld:     []string{"pool-staging"},
				Carries:      []AnswerCheck{{Name: "production-cap", Any: []string{"max_conns=180"}}},
				Avoids:       []AnswerCheck{{Name: "staging-cap", Any: []string{"max_conns=24"}}},
			},
			{
				Script: "Fourth session on orbit-search, working in staging again. You are re-running the load test.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the pool cap the load test runs with.",
				SessionScope: staging,
				Expect:       []string{"pool-staging"},
				Withheld:     []string{"pool-production"},
				Carries:      []AnswerCheck{{Name: "staging-cap", Any: []string{"max_conns=24"}}},
			},
		},
		FinalScope: production,
		Lures:      []Lure{{Older: "pool-staging", Newer: "pool-production"}},
		Probes: []Probe{
			{
				Name:   "production-pool",
				Query:  "database pool max_conns",
				Scope:  production,
				Want:   "pool-production",
				Absent: []string{"pool-staging"},
			},
			{
				Name:   "staging-pool",
				Query:  "database pool max_conns",
				Scope:  staging,
				Want:   "pool-staging",
				Absent: []string{"pool-production"},
			},
		},
		Judge: "Did the session use the staging value (%s: %s) rather than the production one?",
	}
}
