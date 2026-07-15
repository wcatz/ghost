package supersede

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
)

// TestHaikuClassifierLive validates the actual prompt against a small labeled
// set. It needs a real API key, so it is skipped in CI; run it manually to get
// a precision signal on the classifier (the one piece of the creation path with
// no deterministic test). Genuine supersessions must be YES; parallel/unrelated
// facts must be NO — a false YES buries a still-valid memory, so NO errors are
// safer than YES errors, and the prompt is biased accordingly.
func TestHaikuClassifierLive(t *testing.T) {
	cfg, err := config.Load()
	if err != nil || cfg.API.Key == "" {
		t.Skip("no ANTHROPIC_API_KEY; skipping live Haiku classifier test")
	}
	cls := NewHaikuClassifier(ai.NewClient(cfg.API.Key, slog.New(slog.NewTextHandler(os.Stderr, nil))))
	ctx := context.Background()

	cases := []struct {
		newer, older string
		want         bool
	}{
		{"Production database migrated to Postgres 16; the 14 cluster is decommissioned.", "Production database runs Postgres 14.", true},
		{"The bastion SSH port moved from 22 to 2222 after the security review.", "The bastion host accepts SSH on port 22.", true},
		{"The repository default branch was renamed from master to main.", "The repository default branch is master.", true},
		{"cardano-node upgraded to 10.2.0 in production.", "Production cardano-node runs 10.1.4.", true},
		{"Staging database is Postgres 16.", "Production database is Postgres 16.", false},
		{"Grafana listens on port 80.", "Prometheus retention is 90 days.", false},
		{"Preview network magic is 2.", "Mainnet network magic is 764824073.", false},
		{"The relay node runs on k3s-mr-slave.", "The block producer runs on k3s-texas.", false},
	}

	correct := 0
	for _, c := range cases {
		got, err := cls.Supersedes(ctx, c.newer, c.older)
		if err != nil {
			t.Fatalf("classify: %v", err)
		}
		verdict := "ok"
		if got != c.want {
			verdict = "MISS"
		} else {
			correct++
		}
		t.Logf("[%s] want=%v got=%v  newer=%q", verdict, c.want, got, c.newer)
	}
	acc := float64(correct) / float64(len(cases))
	t.Logf("Haiku classifier accuracy on labeled set: %d/%d = %.2f", correct, len(cases), acc)
	// A loose floor — this is a smoke test of prompt quality, not a hard gate.
	if acc < 0.75 {
		t.Errorf("classifier accuracy %.2f below 0.75 — prompt may need work", acc)
	}
}
