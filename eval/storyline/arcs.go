package main

// The three arcs that let the runner measure usefulness rather than delivery
// (issue #974). Each one has a block property (Expect) and an answer property
// (Carries, Avoids), and each is written so the answer cannot be read out of the
// script of the stage that is graded on it: Validate refuses a Carries spelling
// that stage's own script contains, and refuses one no earlier record holds.

// CorrectionReplay is the arc for the question "does a correction stick": the
// user corrects the agent in session 1, and sessions 2 and 3 get the same task
// with no correction in their text. The task has a natural wrong answer (the
// conventional Idempotency-Key header) and a right one no model could guess (an
// invented header name), so repeating the mistake is what an agent with no
// memory does, and the right answer can only come from what session 1 left
// behind.
//
// The answer cannot leak from the script because the correction exists only in
// session 1's script. Sessions 2 and 3 ask the same question and name neither
// header.
func CorrectionReplay() Storyline {
	const task = "Answer in at most three sentences, using only what you know about this project:\n" +
		"- which HTTP header carries the idempotency key on a charge request."
	return Storyline{
		Key:     "correction-replay",
		Title:   "A correction made in session 1 must not be repeated",
		Project: "acme-billing",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "acme-billing is the card-charging service behind the Acme storefront; the gateway client lives in internal/gateway.",
				Tags:     []string{"acme-billing"},
				Mark:     "card-charging service",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on acme-billing. You are writing the charge request for the payment gateway.\n\n" +
					"Your first answer named the Idempotency-Key header. The user corrected you: this gateway ignores that header and " +
					"deduplicates charges on X-Acme-Dedupe-Token instead, and sending the standard header silently double-charges on a retry.\n\n" +
					"Restate the correct header in one sentence.",
				Records: []Record{
					{
						Key:      "dedupe-header",
						Category: "gotcha",
						Content:  "The acme-billing gateway ignores the Idempotency-Key header; charges are deduplicated on the X-Acme-Dedupe-Token header, and the standard header double-charges on a retry.",
						Tags:     []string{"acme-billing", "gateway"},
						Mark:     "X-Acme-Dedupe-Token",
					},
				},
			},
			{
				Script:  "Second session on acme-billing. You are writing the retry wrapper around the charge call.\n\n" + task,
				Expect:  []string{"dedupe-header"},
				Carries: []AnswerCheck{{Name: "corrected-header", Any: []string{"X-Acme-Dedupe-Token"}}},
				Avoids:  []AnswerCheck{{Name: "repeated-mistake", Any: []string{"Idempotency-Key"}}},
			},
			{
				Script:  "Third session on acme-billing. You are writing the test for the charge call's retry behaviour.\n\n" + task,
				Expect:  []string{"dedupe-header"},
				Carries: []AnswerCheck{{Name: "corrected-header", Any: []string{"X-Acme-Dedupe-Token"}}},
				Avoids:  []AnswerCheck{{Name: "repeated-mistake", Any: []string{"Idempotency-Key"}}},
			},
		},
		Judge: "Did the session use the corrected approach (%s: %s) rather than repeat the mistake the correction was about?",
	}
}

// OpsFact is the cross-project arc: a host and port learned in one repository
// are needed in another. The runner holds one project, so the fact is saved
// through ghost_save_global, the cross-project bucket every project's session
// start reads, in the stage that learns it; the later sessions are the other
// repository's. The sessions run with no tools, so ghost_search_all is not
// reachable from them and the global bucket in the injection is the only
// cross-project route this arc can measure.
//
// The answer cannot leak from the script because only session 1's script states
// the address, and the address is an invented one a model could not guess;
// sessions 2 and 3 ask for it without naming either half.
func OpsFact() Storyline {
	const task = "Answer in at most two sentences, using only what you know about this project:\n" +
		"- the host and the port the queue client connects to."
	checks := []AnswerCheck{
		{Name: "queue-host", Any: []string{"pg-queue-03.corp.example"}},
		{Name: "queue-port", Any: []string{"6432"}},
	}
	return Storyline{
		Key:     "ops-fact",
		Title:   "An ops fact saved globally in one repository is needed in another",
		Project: "invoice-worker",
		Opening: []Record{
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "invoice-worker renders customer invoices from the billing queue; it has no database of its own.",
				Tags:     []string{"invoice-worker"},
				Mark:     "renders customer invoices",
			},
		},
		Stages: []Stage{
			{
				Script: "First session, in the infra-runbooks repository. The platform lead tells you the job-queue broker for every service " +
					"is pg-queue-03.corp.example on port 6432, TLS only.\n\nRestate the address in one sentence.",
				Records: []Record{
					{
						Key:      "queue-broker",
						Category: "fact",
						Content:  "Every service reaches the job-queue broker at pg-queue-03.corp.example:6432, TLS only.",
						Tags:     []string{"ops", "queue"},
						Mark:     "pg-queue-03.corp.example:6432",
						Global:   true,
					},
				},
			},
			{
				Script: "Second session, now in invoice-worker, a different repository from the one where the broker address was given. " +
					"You are configuring the worker's queue client.\n\n" + task,
				Expect:  []string{"queue-broker"},
				Carries: checks,
			},
			{
				Script:  "Third session on invoice-worker. You are writing the readiness probe for the queue connection.\n\n" + task,
				Expect:  []string{"queue-broker"},
				Carries: checks,
			},
		},
		Judge: "Did the session use the stored address (%s: %s) rather than a guessed or placeholder one?",
	}
}

// StaleFact is the expiry arc: a fact past its valid_until must not be used. The
// opening holds the old endpoint with a window that closed in January 2025, so
// it is expired on every date a run can happen on, and session 1 is told the
// current endpoint and records it. The wording of the old record does not say it
// is old: the validity filter alone has to withhold it from the block.
//
// The answer cannot leak from the script because only session 1's script names
// the current endpoint, and the old one appears in no script at all; sessions 2
// and 3 ask which URL to call.
func StaleFact() Storyline {
	const task = "Answer in at most two sentences, using only what you know about this project:\n" +
		"- which base URL the client calls."
	current := []AnswerCheck{{Name: "current-endpoint", Any: []string{"ledger-v2.corp.example"}}}
	expired := []AnswerCheck{{Name: "expired-endpoint", Any: []string{"ledger-v1.corp.example"}}}
	return Storyline{
		Key:     "stale-fact",
		Title:   "A fact past its valid_until must not be used",
		Project: "ledger-client",
		Opening: []Record{
			{
				Key:        "old-endpoint",
				Category:   "fact",
				Content:    "The ledger API base URL is https://ledger-v1.corp.example/api.",
				Tags:       []string{"ledger-client", "endpoint"},
				Mark:       "ledger-v1.corp.example",
				ValidUntil: "2025-01-31",
			},
			{
				Key:      "service-owner",
				Category: "fact",
				Content:  "ledger-client is the Go client library the finance services use to post entries.",
				Tags:     []string{"ledger-client"},
				Mark:     "Go client library",
			},
		},
		Stages: []Stage{
			{
				Script: "First session on ledger-client. The ledger team tells you the API moved: the base URL is now " +
					"https://ledger-v2.corp.example/api, and the old host is gone.\n\nRestate the new base URL in one sentence.",
				Records: []Record{
					{
						Key:      "new-endpoint",
						Category: "fact",
						Content:  "The ledger API base URL is https://ledger-v2.corp.example/api.",
						Tags:     []string{"ledger-client", "endpoint"},
						Mark:     "ledger-v2.corp.example",
					},
				},
			},
			{
				Script:  "Second session on ledger-client. You are writing the client's default configuration.\n\n" + task,
				Expect:  []string{"new-endpoint"},
				Carries: current,
				Avoids:  expired,
			},
			{
				Script:  "Third session on ledger-client. You are writing the integration test's fixture.\n\n" + task,
				Expect:  []string{"new-endpoint"},
				Carries: current,
				Avoids:  expired,
			},
		},
		Judge: "Did the session use the current endpoint (%s: %s) and not the expired one?",
	}
}
