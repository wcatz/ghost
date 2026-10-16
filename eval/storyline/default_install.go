package main

// The default-install arcs. Each one changes what is true about a project in a
// shape the truth-change arcs in truth.go do not cover, and all but the last
// run no lifecycle between sessions: automatic lifecycle phases are off by
// default, so the session after each change is graded on what the save path and
// the read side alone left behind. The arc's own lifecycle still runs after the
// last session, so the final block and the end-state searches show what a
// supersede pass would have changed; the stage searches are the default-install
// reading.
//
// The shapes:
//
//   - retired-dependency: a claim retired with no replacement. The retirement
//     note names only what is gone, so it holds no value the original lacks.
//   - reverted-change: A, then B, then back to A. The revert note restates A's
//     values, so it can look like a second wording of A.
//   - prose-only-change: a toolchain swap told in plain lowercase words, the
//     save path's documented known limit, graded for its user-visible cost.
//   - recategorised-replacement: the replacement is saved as a decision where
//     the original was a fact.
//   - repeated-tuning: one setting changed three times after it was first set;
//     only the newest of four notes is current.
//   - superseded-convention: a replaced convention, graded on the default
//     install's block and again after a lifecycle pass, which can link the
//     pair but never retire a convention.

// RetiredDependency is the retirement arc: the mail provider is removed in
// session 2 and nothing replaces it. Session 3 must not be told the provider
// is current, and a search in the old wording must lead to the removal.
func RetiredDependency() Storyline {
	return Storyline{
		Key:     "retired-dependency",
		Title:   "A dependency removed with no replacement must stop being presented as current",
		Project: "parcel-notify",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "parcel-notify sends customers their order and delivery notices for the Parcel storefront.",
				Tags:     []string{"parcel-notify"},
				Mark:     "order and delivery notices",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on parcel-notify. You are wiring up how order notices reach customers.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what parcel-notify does.",
				Records: []Record{
					{
						Key:          "mail-sendgrid",
						Category:     "dependency",
						Content:      "Order emails go out through SendGrid: internal/mail wraps the sendgrid-go v3 client and reads SENDGRID_API_KEY at boot.",
						Tags:         []string{"parcel-notify", "mail"},
						Mark:         "wraps the sendgrid-go v3 client",
						SupersededBy: "mail-removed",
					},
				},
			},
			{
				Script: "Second session on parcel-notify. Email was cut from the product this sprint and you have just deleted the mail code.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- how order notices reached customers before this change.",
				Expect:  []string{"mail-sendgrid"},
				Carries: []AnswerCheck{{Name: "old-provider", Any: []string{"SendGrid"}}},
				Records: []Record{
					{
						// The retirement names only what is gone: no new value.
						Key:      "mail-removed",
						Category: "dependency",
						Content:  "SendGrid was removed from parcel-notify: internal/mail is deleted, nothing reads SENDGRID_API_KEY, and order emails are no longer sent at all.",
						Tags:     []string{"parcel-notify", "mail"},
						Mark:     "internal/mail is deleted",
					},
				},
			},
			{
				Script: "Third session on parcel-notify. A customer says the order emails stopped, and support asks what to tell them.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- whether parcel-notify still sends order emails, and through what.",
				Expect:  []string{"mail-removed"},
				Carries: []AnswerCheck{{Name: "email-retired", Any: []string{"no longer sent", "deleted"}}},
				Avoids:  []AnswerCheck{{Name: "retired-client", Any: []string{"sendgrid-go"}}},
				Probes: []Probe{
					{
						// The old wording is what a reader with an old stack trace or
						// an old ticket searches for.
						Name:  "mail-provider",
						Query: "order emails SendGrid client",
						Want:  "mail-removed",
						Over:  []string{"mail-sendgrid"},
					},
				},
			},
		},
		Probes: []Probe{
			{
				Name:  "mail-provider",
				Query: "order emails SendGrid client",
				Want:  "mail-removed",
				Over:  []string{"mail-sendgrid"},
			},
		},
		Judge: "Did the session say the dependency is gone (%s: %s) rather than present it as current?",
	}
}

// RevertedChange is the revert arc: the job backend moves from river to asynq
// in session 2 and back to river in session 3. Session 4 must be told river is
// current, and must not be told asynq is. The revert note restates the
// original's values, which is the natural way to write "back to A".
func RevertedChange() Storyline {
	return Storyline{
		Key:     "reverted-change",
		Title:   "A change that was reverted must not be presented as current",
		Project: "heron-jobs",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "heron-jobs is the background worker for the Heron booking site; it sends reminders and runs invoice exports.",
				Tags:     []string{"heron-jobs"},
				Mark:     "Heron booking site",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on heron-jobs. You are setting up the job queue for the worker.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what heron-jobs runs.",
				Records: []Record{
					{
						Key:          "jobs-river",
						Category:     "decision",
						Content:      "Background jobs run on river: cmd/worker starts with JOB_BACKEND=river and job rows live in the river_job table in the main Postgres.",
						Tags:         []string{"heron-jobs", "queue"},
						Mark:         "job rows live in the river_job table",
						SupersededBy: "jobs-asynq",
					},
				},
			},
			{
				Script: "Second session on heron-jobs. The team moved the worker's queue this week to take load off the main database.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- which queue the worker used before the move.",
				Expect:  []string{"jobs-river"},
				Carries: []AnswerCheck{{Name: "first-backend", Any: []string{"JOB_BACKEND=river"}}},
				Records: []Record{
					{
						Key:          "jobs-asynq",
						Category:     "decision",
						Content:      "Background jobs moved to asynq: cmd/worker starts with JOB_BACKEND=asynq and the queues live on jobs-redis-01.corp.example.",
						Tags:         []string{"heron-jobs", "queue"},
						Mark:         "JOB_BACKEND=asynq",
						SupersededBy: "jobs-reverted",
					},
				},
			},
			{
				Script: "Third session on heron-jobs. The queue move went badly in production and was rolled back this morning.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- which queue the worker moved to.",
				Expect:  []string{"jobs-asynq"},
				Carries: []AnswerCheck{{Name: "second-backend", Any: []string{"JOB_BACKEND=asynq"}}},
				Records: []Record{
					{
						// "Back to A": the original's values, restated.
						Key:      "jobs-reverted",
						Category: "decision",
						Content:  "Background jobs are back on river: the asynq move was reverted after it lost retries, and cmd/worker starts with JOB_BACKEND=river again.",
						Tags:     []string{"heron-jobs", "queue"},
						Mark:     "the asynq move was reverted",
					},
				},
			},
			{
				Script: "Fourth session on heron-jobs. You are adding a new job type and need the worker's queue settings.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the JOB_BACKEND value the worker starts with.",
				Expect:  []string{"jobs-reverted"},
				Carries: []AnswerCheck{{Name: "current-backend", Any: []string{"JOB_BACKEND=river"}}},
				Avoids:  []AnswerCheck{{Name: "reverted-backend", Any: []string{"JOB_BACKEND=asynq"}}},
				Probes: []Probe{
					{
						Name:  "job-backend",
						Query: "JOB_BACKEND the worker starts with",
						Want:  "jobs-reverted",
						Over:  []string{"jobs-asynq"},
					},
					{
						// What a reader working from the asynq rollout searches for.
						Name:  "asynq-queues",
						Query: "asynq queues jobs-redis-01",
						Want:  "jobs-reverted",
						Over:  []string{"jobs-asynq"},
					},
				},
			},
		},
		Probes: []Probe{
			{
				Name:  "job-backend",
				Query: "JOB_BACKEND the worker starts with",
				Want:  "jobs-reverted",
				Over:  []string{"jobs-asynq"},
			},
			{
				Name:  "asynq-queues",
				Query: "asynq queues jobs-redis-01",
				Want:  "jobs-reverted",
				Over:  []string{"jobs-asynq"},
			},
		},
		KnownFailing: map[string]string{
			"search-finds:session-4:job-backend":            "near-duplicate removal drops the revert note in favour of the original it restates (folded as its duplicate at save), although the revert note is the reverted claim's replacement",
			"search-ranks:session-4:job-backend:jobs-asynq": "with the revert note removed, the reverted claim ranks first and unmarked, and the original is marked as replaced by the reverted claim",
		},
		Judge: "Did the session use the backend the revert restored (%s: %s) rather than the reverted one?",
	}
}

// ProseOnlyChange is the plain-word arc: the lint toolchain is swapped in
// session 2, and both notes say so in plain lowercase words with no version,
// path, identifier or backticked span. The save path's value guard and the
// read side's replacement guess both read value tokens only, so neither can see
// this change: it is the documented known limit (a plain lowercase word outside
// backticks), graded here for what it costs the session after the change.
func ProseOnlyChange() Storyline {
	return Storyline{
		Key:     "prose-only-change",
		Title:   "A change told only in plain words must stop presenting the old way as current",
		Project: "lumen-web",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "lumen-web is the marketing site and docs portal; it is a static build deployed on every merge.",
				Tags:     []string{"lumen-web"},
				Mark:     "marketing site and docs portal",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on lumen-web. You are setting up the pre-push checks.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what lumen-web is.",
				Records: []Record{
					{
						Key:          "lint-eslint",
						Category:     "convention",
						Content:      "Lint and format with eslint and prettier: run npm run lint before every push, and the editor formats on save with prettier.",
						Tags:         []string{"lumen-web", "lint"},
						Mark:         "run npm run lint before every push",
						SupersededBy: "lint-biome",
					},
				},
			},
			{
				Script: "Second session on lumen-web. The team swapped the lint toolchain this sprint.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- which linter the site used before the swap.",
				Expect:  []string{"lint-eslint"},
				Carries: []AnswerCheck{{Name: "old-linter", Any: []string{"eslint"}}},
				Records: []Record{
					{
						Key:      "lint-biome",
						Category: "convention",
						Content:  "Lint and format with biome now, not eslint and prettier: run npm run check before every push, and the editor formats on save with biome.",
						Tags:     []string{"lumen-web", "lint"},
						Mark:     "run npm run check before every push",
					},
				},
			},
			{
				Script: "Third session on lumen-web. You are writing the contributing guide's pre-push step.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the command to run before every push, and the formatter the editor uses.",
				Expect:  []string{"lint-biome"},
				Carries: []AnswerCheck{{Name: "current-check", Any: []string{"npm run check"}}},
				Avoids:  []AnswerCheck{{Name: "old-check", Any: []string{"npm run lint"}}},
				Probes: []Probe{
					{
						Name:  "pre-push",
						Query: "lint command to run before every push",
						Want:  "lint-biome",
						Over:  []string{"lint-eslint"},
					},
				},
			},
		},
		Probes: []Probe{
			{
				Name:  "pre-push",
				Query: "lint command to run before every push",
				Want:  "lint-biome",
				Over:  []string{"lint-eslint"},
			},
		},
		// The session-3 lines are the documented known limit (a plain lowercase
		// word outside backticks is never a value token). The final block, after the
		// arc's lifecycle, is graded on the supersedes edge's own marker and passes.
		KnownFailing: map[string]string{
			"distinct-save:lint-biome":                    "known limit: a change told in plain lowercase words has no value token, so the save path folds the replacement into the claim it replaces and strengthens it",
			"stale-original:session-3":                    "known limit: the read side's replacement guess reads value tokens only, so the strengthened old convention is the first line of the block, unmarked",
			"search-finds:session-3:pre-push":             "known limit: near-duplicate removal drops the replacement in favour of the strengthened claim it replaces",
			"search-ranks:session-3:pre-push:lint-eslint": "known limit: the old convention is the only matching row returned, unmarked",
		},
		Judge: "Did the session use the current toolchain (%s: %s) rather than the one it replaced?",
	}
}

// RecategorisedReplacement is the category arc: the cache is a fact in session
// 1 and its replacement is recorded as a decision in session 2, which is how an
// agent records a change the team chose. The pair is the same claim with a
// different value, in two categories.
func RecategorisedReplacement() Storyline {
	return Storyline{
		Key:     "recategorised-replacement",
		Title:   "A replacement saved in another category must still retire the original",
		Project: "kestrel-api",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "kestrel-api is the read API for the Kestrel flight tracker; most of its traffic is cacheable lookups.",
				Tags:     []string{"kestrel-api"},
				Mark:     "Kestrel flight tracker",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on kestrel-api. You are reading how lookups are served.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what kestrel-api serves.",
				Records: []Record{
					{
						Key:          "cache-memcached",
						Category:     "fact",
						Content:      "Lookup responses are cached in Memcached: internal/cache dials memcached-01.corp.example:11211 and every entry lives 300s.",
						Tags:         []string{"kestrel-api", "cache"},
						Mark:         "memcached-01.corp.example:11211",
						SupersededBy: "cache-ristretto",
					},
				},
			},
			{
				Script: "Second session on kestrel-api. The team decided this week to drop the shared cache server.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- where lookup responses were cached before the decision.",
				Expect:  []string{"cache-memcached"},
				Carries: []AnswerCheck{{Name: "old-cache", Any: []string{"Memcached"}}},
				Records: []Record{
					{
						Key:      "cache-ristretto",
						Category: "decision",
						Content:  "Decision: lookup responses are cached in-process with ristretto instead of Memcached; internal/cache keeps a 64 MB budget per pod, entries still live 300s, and memcached-01 is being shut down.",
						Tags:     []string{"kestrel-api", "cache"},
						Mark:     "in-process with ristretto",
					},
				},
			},
			{
				Script: "Third session on kestrel-api. You are writing the capacity plan for the lookup cache.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- where lookup responses are cached, and how much memory the cache may use.",
				Expect:  []string{"cache-ristretto"},
				Carries: []AnswerCheck{{Name: "current-cache", Any: []string{"ristretto"}}},
				Avoids:  []AnswerCheck{{Name: "old-cache-host", Any: []string{"memcached-01.corp.example"}}},
				Probes: []Probe{
					{
						Name:  "lookup-cache",
						Query: "where lookup responses are cached",
						Want:  "cache-ristretto",
						Over:  []string{"cache-memcached"},
					},
				},
			},
		},
		Probes: []Probe{
			{
				Name:  "lookup-cache",
				Query: "where lookup responses are cached",
				Want:  "cache-ristretto",
				Over:  []string{"cache-memcached"},
			},
		},
		KnownFailing: map[string]string{
			"stale-original:session-3":                            "the read side's replacement guess holds a pair across categories to the save path's cross-category fold bar (token Jaccard >= 0.7), which a replacement that rewords does not clear, so the old fact is unmarked; the same texts in one category are marked",
			"search-ranks:session-3:lookup-cache:cache-memcached": "with no replacement pair across categories, search neither moves the decision ahead of the fact it replaced nor marks the fact",
		},
		Judge: "Did the session use the current cache (%s: %s) rather than the one it replaced?",
	}
}

// RepeatedTuning is the many-updates arc: one rate limit is set in session 1
// and changed in sessions 2, 3 and 4, each note naming the setting and the file
// it lives in. Session 5 must be told the newest value as current, and each
// older value must be absent or marked behind the note that replaced it.
func RepeatedTuning() Storyline {
	return Storyline{
		Key:     "repeated-tuning",
		Title:   "Only the newest of several values of one setting may be presented as current",
		Project: "sparrow-ingest",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "sparrow-ingest accepts telemetry batches from customer devices and writes them to the event store.",
				Tags:     []string{"sparrow-ingest"},
				Mark:     "telemetry batches",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on sparrow-ingest. You are setting the per-tenant ingest limit for launch.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what sparrow-ingest accepts.",
				Records: []Record{
					{
						Key:          "limit-200",
						Category:     "fact",
						Content:      "The per-tenant ingest limit is INGEST_RATE_LIMIT=200 requests per second, set in deploy/values.yaml.",
						Tags:         []string{"sparrow-ingest", "limits"},
						Mark:         "INGEST_RATE_LIMIT=200",
						SupersededBy: "limit-350",
					},
				},
			},
			{
				Script: "Second session on sparrow-ingest. The nightly device backfill was throttled, so the limit was raised.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the limit before the raise.",
				Expect:  []string{"limit-200"},
				Carries: []AnswerCheck{{Name: "launch-limit", Any: []string{"INGEST_RATE_LIMIT=200"}}},
				Records: []Record{
					{
						Key:          "limit-350",
						Category:     "fact",
						Content:      "The per-tenant ingest limit is now INGEST_RATE_LIMIT=350 requests per second in deploy/values.yaml; the launch value throttled the nightly device backfill.",
						Tags:         []string{"sparrow-ingest", "limits"},
						Mark:         "INGEST_RATE_LIMIT=350",
						SupersededBy: "limit-500",
					},
				},
			},
			{
				Script: "Third session on sparrow-ingest. The holiday firmware push is coming and the limit was raised again.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the limit before this raise.",
				Expect:  []string{"limit-350"},
				Carries: []AnswerCheck{{Name: "backfill-limit", Any: []string{"INGEST_RATE_LIMIT=350"}}},
				Records: []Record{
					{
						Key:          "limit-500",
						Category:     "fact",
						Content:      "The per-tenant ingest limit was raised to INGEST_RATE_LIMIT=500 requests per second in deploy/values.yaml for the holiday firmware push.",
						Tags:         []string{"sparrow-ingest", "limits"},
						Mark:         "INGEST_RATE_LIMIT=500",
						SupersededBy: "limit-450",
					},
				},
			},
			{
				Script: "Fourth session on sparrow-ingest. One tenant starved the event store writer during the push, so the limit was lowered.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the limit before it was lowered.",
				Expect:  []string{"limit-500"},
				Carries: []AnswerCheck{{Name: "holiday-limit", Any: []string{"INGEST_RATE_LIMIT=500"}}},
				Records: []Record{
					{
						Key:      "limit-450",
						Category: "fact",
						Content:  "The per-tenant ingest limit is back down to INGEST_RATE_LIMIT=450 requests per second in deploy/values.yaml; the holiday value let one tenant starve the event store writer.",
						Tags:     []string{"sparrow-ingest", "limits"},
						Mark:     "INGEST_RATE_LIMIT=450",
					},
				},
			},
			{
				Script: "Fifth session on sparrow-ingest. You are writing the runbook entry for a tenant that hits the ingest limit.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the per-tenant limit in force, and the file it is set in.",
				Expect:  []string{"limit-450"},
				Carries: []AnswerCheck{{Name: "current-limit", Any: []string{"INGEST_RATE_LIMIT=450"}}},
				Avoids:  []AnswerCheck{{Name: "older-limit", Any: []string{"INGEST_RATE_LIMIT=200", "INGEST_RATE_LIMIT=350", "INGEST_RATE_LIMIT=500"}}},
				Probes: []Probe{
					{
						Name:  "ingest-limit",
						Query: "per-tenant ingest limit INGEST_RATE_LIMIT",
						Want:  "limit-450",
						Over:  []string{"limit-200", "limit-350", "limit-500"},
					},
				},
			},
		},
		Probes: []Probe{
			{
				Name:  "ingest-limit",
				Query: "per-tenant ingest limit INGEST_RATE_LIMIT",
				Want:  "limit-450",
				Over:  []string{"limit-200", "limit-350", "limit-500"},
			},
		},
		Judge: "Did the session use the newest limit (%s: %s) rather than an earlier one?",
	}
}

// SupersededConvention is the standing-category arc, and the one arc here that
// runs a lifecycle between sessions: a team convention is replaced in session
// 2, session 3 is graded on the default install's store, and the lifecycle
// runs after session 3, as a host's Stop hook would on a project whose
// lifecycle is enabled. Session 4 is graded on the same claims after that
// pass. A convention or a preference is never resolved (resolve's standing-
// category guardrail), so the pass can link the pair but never retire the old
// note, and the arc grades what the block says about it before and after.
func SupersededConvention() Storyline {
	return Storyline{
		Key:     "superseded-convention",
		Title:   "A replaced convention must stay marked as replaced once the lifecycle has run",
		Project: "wren-cli",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "wren-cli is the command-line client for the Wren deploy service; it ships as a single static binary.",
				Tags:     []string{"wren-cli"},
				Mark:     "Wren deploy service",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on wren-cli. You are preparing the first public release.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- what wren-cli is.",
				Records: []Record{
					{
						Key:          "release-semver",
						Category:     "convention",
						Content:      "Release tags follow semver: tag `vMAJOR.MINOR.PATCH` from main and run `make release` to publish.",
						Tags:         []string{"wren-cli", "release"},
						Mark:         "Release tags follow semver",
						SupersededBy: "release-calver",
					},
				},
			},
			{
				Script: "Second session on wren-cli. The team changed how releases are numbered this sprint.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- how release tags were numbered before the change.",
				Expect:  []string{"release-semver"},
				Carries: []AnswerCheck{{Name: "old-scheme", Any: []string{"semver"}}},
				Records: []Record{
					{
						Key:      "release-calver",
						Category: "convention",
						Content:  "Release tags follow calendar versioning now: tag `YYYY.MM.N` from main and run `make release` to publish.",
						Tags:     []string{"wren-cli", "release"},
						Mark:     "follow calendar versioning",
					},
				},
			},
			{
				Script: "Third session on wren-cli. You are cutting this month's release.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the tag form and the make target for a release.",
				Expect:  []string{"release-calver"},
				Carries: []AnswerCheck{{Name: "current-tag", Any: []string{"YYYY.MM.N"}}},
				Avoids:  []AnswerCheck{{Name: "old-tag", Any: []string{"vMAJOR.MINOR.PATCH"}}},
				// Before the lifecycle, only the lexical guess relates the pair.
				Probes: []Probe{{
					Name:   "tag-form",
					Query:  "release tag form make release",
					Want:   "release-calver",
					Over:   []string{"release-semver"},
					Marked: []string{"release-semver"},
				}},
				// The project's lifecycle runs once this session ends.
				Lifecycle: true,
			},
			{
				Script: "Fourth session on wren-cli. You are writing the release section of the contributing guide.\n\n" +
					"Answer in at most two sentences, using only what this session's context already holds:\n" +
					"- the tag form and the make target for a release.",
				Expect:  []string{"release-calver"},
				Carries: []AnswerCheck{{Name: "current-tag", Any: []string{"YYYY.MM.N"}}},
				Avoids:  []AnswerCheck{{Name: "old-tag", Any: []string{"vMAJOR.MINOR.PATCH"}}},
				// After it, a live supersedes edge has switched the guess off for
				// the pair, and the search must still say the old tag is replaced.
				Probes: []Probe{{
					Name:   "tag-form-linked",
					Query:  "release tag form make release",
					Want:   "release-calver",
					Over:   []string{"release-semver"},
					Marked: []string{"release-semver"},
				}},
			},
		},
		Judge: "Did the session use the current release convention (%s: %s) rather than the one it replaced?",
	}
}
