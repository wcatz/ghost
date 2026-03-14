package github

import "testing"

func TestScorePriority(t *testing.T) {
	tests := []struct {
		name        string
		reason      string
		subjectType string
		want        int
	}{
		// P0: security alerts
		{"security_alert reason", "security_alert", "Issue", P0},
		{"vulnerability in subjectType", "subscribed", "RepositoryVulnerabilityAlert", P0},
		{"vulnerability case-insensitive", "comment", "VULNERABILITY_ALERT", P0},
		{"security_alert overrides subjectType", "security_alert", "PullRequest", P0},

		// P1: review requests, CI activity
		{"review_requested", "review_requested", "PullRequest", P1},
		{"ci_activity", "ci_activity", "CheckSuite", P1},

		// P2: direct engagement
		{"mention", "mention", "Issue", P2},
		{"assign", "assign", "PullRequest", P2},
		{"comment", "comment", "Issue", P2},

		// P3: team-level
		{"subscribed", "subscribed", "PullRequest", P3},
		{"team_mention", "team_mention", "Issue", P3},

		// P4: everything else
		{"unknown reason", "state_change", "Issue", P4},
		{"empty reason", "", "PullRequest", P4},
		{"random reason", "some_future_reason", "Discussion", P4},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scorePriority(tt.reason, tt.subjectType)
			if got != tt.want {
				t.Errorf("scorePriority(%q, %q) = %d, want %d", tt.reason, tt.subjectType, got, tt.want)
			}
		})
	}
}

func TestBoolToInt(t *testing.T) {
	if got := boolToInt(true); got != 1 {
		t.Errorf("boolToInt(true) = %d, want 1", got)
	}
	if got := boolToInt(false); got != 0 {
		t.Errorf("boolToInt(false) = %d, want 0", got)
	}
}
