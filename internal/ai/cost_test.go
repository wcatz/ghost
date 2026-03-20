package ai

import (
	"math"
	"testing"
)

const floatTol = 1e-9

func almostEqual(a, b, tol float64) bool {
	return math.Abs(a-b) < tol
}

// ---------- 1. TestModelPricing ----------

func TestModelPricing(t *testing.T) {
	tests := []struct {
		name                                   string
		model                                  string
		wantInput, wantOutput, wantCW, wantCR float64
	}{
		{
			name:       "haiku model",
			model:      "claude-3-haiku-20240307",
			wantInput:  HaikuInputPerM,
			wantOutput: HaikuOutputPerM,
			wantCW:     HaikuCacheWritePerM,
			wantCR:     HaikuCacheReadPerM,
		},
		{
			name:       "haiku substring match",
			model:      "some-haiku-variant",
			wantInput:  HaikuInputPerM,
			wantOutput: HaikuOutputPerM,
			wantCW:     HaikuCacheWritePerM,
			wantCR:     HaikuCacheReadPerM,
		},
		{
			name:       "opus model",
			model:      "claude-opus-4-20250514",
			wantInput:  OpusInputPerM,
			wantOutput: OpusOutputPerM,
			wantCW:     OpusCacheWritePerM,
			wantCR:     OpusCacheReadPerM,
		},
		{
			name:       "opus substring match",
			model:      "my-opus-thing",
			wantInput:  OpusInputPerM,
			wantOutput: OpusOutputPerM,
			wantCW:     OpusCacheWritePerM,
			wantCR:     OpusCacheReadPerM,
		},
		{
			name:       "sonnet model",
			model:      "claude-3-5-sonnet-20241022",
			wantInput:  SonnetInputPerM,
			wantOutput: SonnetOutputPerM,
			wantCW:     SonnetCacheWritePerM,
			wantCR:     SonnetCacheReadPerM,
		},
		{
			name:       "unknown defaults to sonnet",
			model:      "totally-unknown-model",
			wantInput:  SonnetInputPerM,
			wantOutput: SonnetOutputPerM,
			wantCW:     SonnetCacheWritePerM,
			wantCR:     SonnetCacheReadPerM,
		},
		{
			name:       "empty string defaults to sonnet",
			model:      "",
			wantInput:  SonnetInputPerM,
			wantOutput: SonnetOutputPerM,
			wantCW:     SonnetCacheWritePerM,
			wantCR:     SonnetCacheReadPerM,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inP, outP, cwP, crP := modelPricing(tt.model)
			if inP != tt.wantInput {
				t.Errorf("input: got %f, want %f", inP, tt.wantInput)
			}
			if outP != tt.wantOutput {
				t.Errorf("output: got %f, want %f", outP, tt.wantOutput)
			}
			if cwP != tt.wantCW {
				t.Errorf("cacheWrite: got %f, want %f", cwP, tt.wantCW)
			}
			if crP != tt.wantCR {
				t.Errorf("cacheRead: got %f, want %f", crP, tt.wantCR)
			}
		})
	}
}

// ---------- 2. TestCostTracker_AddNil ----------

func TestCostTracker_AddNil(t *testing.T) {
	ct := &CostTracker{}

	// Add nil via both methods — should not panic.
	ct.Add(nil)
	ct.AddWithModel(nil, "claude-3-5-sonnet-20241022")

	in, out, cw, cr := ct.Totals()
	if in != 0 || out != 0 || cw != 0 || cr != 0 {
		t.Errorf("expected all zeros after nil adds, got in=%d out=%d cw=%d cr=%d", in, out, cw, cr)
	}
	if ct.Cost() != 0 {
		t.Errorf("expected zero cost after nil adds, got %f", ct.Cost())
	}
}

// ---------- 3. TestCostTracker_AddWithModel ----------

func TestCostTracker_AddWithModel(t *testing.T) {
	ct := &CostTracker{}

	u1 := &TokenUsage{
		InputTokens:              100,
		OutputTokens:             200,
		CacheCreationInputTokens: 50,
		CacheReadInputTokens:     25,
	}
	u2 := &TokenUsage{
		InputTokens:              300,
		OutputTokens:             400,
		CacheCreationInputTokens: 150,
		CacheReadInputTokens:     75,
	}

	ct.AddWithModel(u1, "claude-3-5-sonnet-20241022")
	ct.AddWithModel(u2, "claude-3-haiku-20240307")

	in, out, cw, cr := ct.Totals()
	if in != 400 {
		t.Errorf("input: got %d, want 400", in)
	}
	if out != 600 {
		t.Errorf("output: got %d, want 600", out)
	}
	if cw != 200 {
		t.Errorf("cacheWrite: got %d, want 200", cw)
	}
	if cr != 100 {
		t.Errorf("cacheRead: got %d, want 100", cr)
	}
}

// ---------- 4. TestCostTracker_Cost ----------

func TestCostTracker_Cost(t *testing.T) {
	tests := []struct {
		name     string
		model    string
		usage    TokenUsage
		wantCost float64
	}{
		{
			name:  "sonnet 1M input only",
			model: "claude-3-5-sonnet-20241022",
			usage: TokenUsage{InputTokens: 1_000_000},
			// 1M * 3.00 / 1M = $3.00
			wantCost: 3.00,
		},
		{
			name:  "haiku 1M output only",
			model: "claude-3-haiku-20240307",
			usage: TokenUsage{OutputTokens: 1_000_000},
			// 1M * 5.00 / 1M = $5.00
			wantCost: 5.00,
		},
		{
			name:  "opus mixed tokens",
			model: "claude-opus-4-20250514",
			usage: TokenUsage{
				InputTokens:              500_000,
				OutputTokens:             200_000,
				CacheCreationInputTokens: 100_000,
				CacheReadInputTokens:     300_000,
			},
			// input:      500k / 1M * 5.00  = 2.50
			// output:     200k / 1M * 25.00 = 5.00
			// cacheWrite: 100k / 1M * 6.25  = 0.625
			// cacheRead:  300k / 1M * 0.50  = 0.15
			wantCost: 2.50 + 5.00 + 0.625 + 0.15,
		},
		{
			name:  "sonnet all token types",
			model: "claude-3-5-sonnet-20241022",
			usage: TokenUsage{
				InputTokens:              1_000_000,
				OutputTokens:             500_000,
				CacheCreationInputTokens: 200_000,
				CacheReadInputTokens:     800_000,
			},
			// input:      1M   / 1M * 3.00  = 3.00
			// output:     500k / 1M * 15.00 = 7.50
			// cacheWrite: 200k / 1M * 3.75  = 0.75
			// cacheRead:  800k / 1M * 0.30  = 0.24
			wantCost: 3.00 + 7.50 + 0.75 + 0.24,
		},
		{
			name:     "zero tokens",
			model:    "claude-3-5-sonnet-20241022",
			usage:    TokenUsage{},
			wantCost: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := &CostTracker{}
			ct.AddWithModel(&tt.usage, tt.model)
			got := ct.Cost()
			if !almostEqual(got, tt.wantCost, floatTol) {
				t.Errorf("Cost() = %f, want %f", got, tt.wantCost)
			}
		})
	}
}

// ---------- 5. TestCostTracker_CostWithoutCache ----------

func TestCostTracker_CostWithoutCache(t *testing.T) {
	ct := &CostTracker{}
	u := &TokenUsage{
		InputTokens:              100_000,
		OutputTokens:             50_000,
		CacheCreationInputTokens: 200_000,
		CacheReadInputTokens:     300_000,
	}
	ct.AddWithModel(u, "claude-3-5-sonnet-20241022")

	// All input tokens treated at base input rate:
	// allInput = 100k + 200k + 300k = 600k
	// input cost: 600k / 1M * 3.00 = 1.80
	// output cost: 50k / 1M * 15.00 = 0.75
	want := 1.80 + 0.75
	got := ct.CostWithoutCache()
	if !almostEqual(got, want, floatTol) {
		t.Errorf("CostWithoutCache() = %f, want %f", got, want)
	}
}

// ---------- 6. TestCostTracker_Savings ----------

func TestCostTracker_Savings(t *testing.T) {
	ct := &CostTracker{}
	u := &TokenUsage{
		InputTokens:              100_000,
		OutputTokens:             50_000,
		CacheCreationInputTokens: 200_000,
		CacheReadInputTokens:     300_000,
	}
	ct.AddWithModel(u, "claude-3-5-sonnet-20241022")

	cost := ct.Cost()
	costWithout := ct.CostWithoutCache()
	savings := ct.Savings()

	wantSavings := costWithout - cost
	if !almostEqual(savings, wantSavings, floatTol) {
		t.Errorf("Savings() = %f, want %f (costWithout=%f, cost=%f)", savings, wantSavings, costWithout, cost)
	}

	// Savings should be positive when cache read tokens exist.
	if savings <= 0 {
		t.Errorf("expected positive savings, got %f", savings)
	}

	// Verify the actual values.
	// Cost:
	// input:      100k / 1M * 3.00 = 0.30
	// output:     50k  / 1M * 15.00 = 0.75
	// cacheWrite: 200k / 1M * 3.75 = 0.75
	// cacheRead:  300k / 1M * 0.30 = 0.09
	// total = 1.89
	//
	// CostWithoutCache:
	// allInput = 600k / 1M * 3.00 = 1.80
	// output = 50k / 1M * 15.00 = 0.75
	// total = 2.55
	//
	// savings = 2.55 - 1.89 = 0.66
	if !almostEqual(savings, 0.66, floatTol) {
		t.Errorf("Savings() = %f, want 0.66", savings)
	}
}

// ---------- 7. TestCostTracker_CacheHitRate ----------

func TestCostTracker_CacheHitRate(t *testing.T) {
	tests := []struct {
		name     string
		usage    *TokenUsage
		wantRate float64
	}{
		{
			name:     "zero total returns 0",
			usage:    nil,
			wantRate: 0,
		},
		{
			name: "all cache read",
			usage: &TokenUsage{
				CacheReadInputTokens: 1000,
			},
			// total = 0 + 0 + 1000 = 1000, rate = 1000/1000 * 100 = 100
			wantRate: 100,
		},
		{
			name: "no cache at all",
			usage: &TokenUsage{
				InputTokens: 1000,
			},
			// total = 1000 + 0 + 0 = 1000, rate = 0/1000 * 100 = 0
			wantRate: 0,
		},
		{
			name: "50% cache hit",
			usage: &TokenUsage{
				InputTokens:         500,
				CacheReadInputTokens: 500,
			},
			// total = 500 + 0 + 500 = 1000, rate = 500/1000 * 100 = 50
			wantRate: 50,
		},
		{
			name: "mixed input types",
			usage: &TokenUsage{
				InputTokens:              200,
				CacheCreationInputTokens: 300,
				CacheReadInputTokens:     500,
			},
			// total = 200 + 300 + 500 = 1000, rate = 500/1000 * 100 = 50
			wantRate: 50,
		},
		{
			name: "25% cache hit",
			usage: &TokenUsage{
				InputTokens:              600,
				CacheCreationInputTokens: 150,
				CacheReadInputTokens:     250,
			},
			// total = 600 + 150 + 250 = 1000, rate = 250/1000 * 100 = 25
			wantRate: 25,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := &CostTracker{}
			if tt.usage != nil {
				ct.AddWithModel(tt.usage, "claude-3-5-sonnet-20241022")
			}
			got := ct.CacheHitRate()
			if !almostEqual(got, tt.wantRate, floatTol) {
				t.Errorf("CacheHitRate() = %f, want %f", got, tt.wantRate)
			}
		})
	}
}

// ---------- 8. TestCostTracker_Summary ----------

func TestCostTracker_Summary(t *testing.T) {
	tests := []struct {
		name  string
		model string
		usage *TokenUsage
		want  string
	}{
		{
			name:  "zero usage",
			model: "claude-3-5-sonnet-20241022",
			usage: &TokenUsage{},
			want:  "$0.00 (saved $0.00, 0% cache)",
		},
		{
			name:  "known values",
			model: "claude-3-5-sonnet-20241022",
			usage: &TokenUsage{
				InputTokens:              100_000,
				OutputTokens:             50_000,
				CacheCreationInputTokens: 200_000,
				CacheReadInputTokens:     300_000,
			},
			// Cost = 0.30 + 0.75 + 0.75 + 0.09 = 1.89
			// CostWithoutCache = 1.80 + 0.75 = 2.55
			// Savings = 2.55 - 1.89 = 0.66
			// CacheHitRate = 300k / (100k+200k+300k) * 100 = 50
			want: "$1.89 (saved $0.66, 50% cache)",
		},
		{
			name:  "no cache tokens",
			model: "claude-3-haiku-20240307",
			usage: &TokenUsage{
				InputTokens:  1_000_000,
				OutputTokens: 500_000,
			},
			// Cost = 1.00 + 2.50 = 3.50
			// CostWithoutCache = 1.00 + 2.50 = 3.50
			// Savings = 0
			// CacheHitRate = 0
			want: "$3.50 (saved $0.00, 0% cache)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ct := &CostTracker{}
			ct.AddWithModel(tt.usage, tt.model)
			got := ct.Summary()
			if got != tt.want {
				t.Errorf("Summary() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------- 9. TestCostForUsage ----------

func TestCostForUsage(t *testing.T) {
	t.Run("nil returns 0", func(t *testing.T) {
		got := CostForUsage(nil, "claude-3-5-sonnet-20241022")
		if got != 0 {
			t.Errorf("CostForUsage(nil, ...) = %f, want 0", got)
		}
	})

	tests := []struct {
		name     string
		usage    TokenUsage
		model    string
		wantCost float64
	}{
		{
			name: "sonnet basic",
			usage: TokenUsage{
				InputTokens:  1_000_000,
				OutputTokens: 1_000_000,
			},
			model:    "claude-3-5-sonnet-20241022",
			wantCost: SonnetInputPerM + SonnetOutputPerM, // 3.00 + 15.00 = 18.00
		},
		{
			name: "haiku with cache",
			usage: TokenUsage{
				InputTokens:              500_000,
				OutputTokens:             250_000,
				CacheCreationInputTokens: 100_000,
				CacheReadInputTokens:     400_000,
			},
			model: "claude-3-haiku-20240307",
			// input:      500k / 1M * 1.00 = 0.50
			// output:     250k / 1M * 5.00 = 1.25
			// cacheWrite: 100k / 1M * 1.25 = 0.125
			// cacheRead:  400k / 1M * 0.10 = 0.04
			wantCost: 0.50 + 1.25 + 0.125 + 0.04,
		},
		{
			name: "opus all fields",
			usage: TokenUsage{
				InputTokens:              200_000,
				OutputTokens:             100_000,
				CacheCreationInputTokens: 300_000,
				CacheReadInputTokens:     500_000,
			},
			model: "claude-opus-4-20250514",
			// input:      200k / 1M * 5.00  = 1.00
			// output:     100k / 1M * 25.00 = 2.50
			// cacheWrite: 300k / 1M * 6.25  = 1.875
			// cacheRead:  500k / 1M * 0.50  = 0.25
			wantCost: 1.00 + 2.50 + 1.875 + 0.25,
		},
		{
			name:     "zero tokens",
			usage:    TokenUsage{},
			model:    "claude-3-5-sonnet-20241022",
			wantCost: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CostForUsage(&tt.usage, tt.model)
			if !almostEqual(got, tt.wantCost, floatTol) {
				t.Errorf("CostForUsage() = %f, want %f", got, tt.wantCost)
			}
		})
	}
}

// ---------- 10. TestCostForUsage_WithModel ----------

func TestCostForUsage_WithModel(t *testing.T) {
	// Same usage, different models should yield different costs.
	usage := &TokenUsage{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheCreationInputTokens: 1_000_000,
		CacheReadInputTokens:     1_000_000,
	}

	tests := []struct {
		name     string
		model    string
		wantCost float64
	}{
		{
			name:  "sonnet pricing",
			model: "claude-3-5-sonnet-20241022",
			// 3.00 + 15.00 + 3.75 + 0.30 = 22.05
			wantCost: SonnetInputPerM + SonnetOutputPerM + SonnetCacheWritePerM + SonnetCacheReadPerM,
		},
		{
			name:  "haiku pricing",
			model: "claude-3-haiku-20240307",
			// 1.00 + 5.00 + 1.25 + 0.10 = 7.35
			wantCost: HaikuInputPerM + HaikuOutputPerM + HaikuCacheWritePerM + HaikuCacheReadPerM,
		},
		{
			name:  "opus pricing",
			model: "claude-opus-4-20250514",
			// 5.00 + 25.00 + 6.25 + 0.50 = 36.75
			wantCost: OpusInputPerM + OpusOutputPerM + OpusCacheWritePerM + OpusCacheReadPerM,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CostForUsage(usage, tt.model)
			if !almostEqual(got, tt.wantCost, floatTol) {
				t.Errorf("CostForUsage() with %s = %f, want %f", tt.model, got, tt.wantCost)
			}
		})
	}

	// Verify that different models produce different costs.
	sonnetCost := CostForUsage(usage, "claude-3-5-sonnet-20241022")
	haikuCost := CostForUsage(usage, "claude-3-haiku-20240307")
	opusCost := CostForUsage(usage, "claude-opus-4-20250514")

	if almostEqual(sonnetCost, haikuCost, floatTol) {
		t.Error("sonnet and haiku costs should differ")
	}
	if almostEqual(sonnetCost, opusCost, floatTol) {
		t.Error("sonnet and opus costs should differ")
	}
	if almostEqual(haikuCost, opusCost, floatTol) {
		t.Error("haiku and opus costs should differ")
	}
}

// ---------- 11. TestCostWithoutCacheForUsage ----------

func TestCostWithoutCacheForUsage(t *testing.T) {
	tests := []struct {
		name       string
		input      int
		output     int
		cacheWrite int
		cacheRead  int
		model      string
		wantCost   float64
	}{
		{
			name:       "sonnet all at base rate",
			input:      100_000,
			output:     50_000,
			cacheWrite: 200_000,
			cacheRead:  300_000,
			model:      "claude-3-5-sonnet-20241022",
			// allInput = 100k + 200k + 300k = 600k
			// input cost: 600k / 1M * 3.00 = 1.80
			// output cost: 50k / 1M * 15.00 = 0.75
			wantCost: 1.80 + 0.75,
		},
		{
			name:       "haiku no cache tokens",
			input:      1_000_000,
			output:     500_000,
			cacheWrite: 0,
			cacheRead:  0,
			model:      "claude-3-haiku-20240307",
			// allInput = 1M, cost = 1.00
			// output = 500k, cost = 2.50
			wantCost: 1.00 + 2.50,
		},
		{
			name:       "opus heavy cache",
			input:      0,
			output:     100_000,
			cacheWrite: 500_000,
			cacheRead:  500_000,
			model:      "claude-opus-4-20250514",
			// allInput = 0 + 500k + 500k = 1M, cost = 5.00
			// output = 100k / 1M * 25.00 = 2.50
			wantCost: 5.00 + 2.50,
		},
		{
			name:       "all zeros",
			input:      0,
			output:     0,
			cacheWrite: 0,
			cacheRead:  0,
			model:      "claude-3-5-sonnet-20241022",
			wantCost:   0,
		},
		{
			name:       "unknown model defaults to sonnet",
			input:      1_000_000,
			output:     1_000_000,
			cacheWrite: 0,
			cacheRead:  0,
			model:      "unknown-model",
			// allInput = 1M, cost = 3.00
			// output = 1M, cost = 15.00
			wantCost: 3.00 + 15.00,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CostWithoutCacheForUsage(tt.input, tt.output, tt.cacheWrite, tt.cacheRead, tt.model)
			if !almostEqual(got, tt.wantCost, floatTol) {
				t.Errorf("CostWithoutCacheForUsage() = %f, want %f", got, tt.wantCost)
			}
		})
	}
}
