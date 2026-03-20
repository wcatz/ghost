package ai

import (
	"fmt"
	"strings"
	"sync"
)

// Pricing per million tokens (USD).
const (
	SonnetInputPerM      = 3.00
	SonnetOutputPerM     = 15.00
	SonnetCacheWritePerM = 3.75
	SonnetCacheReadPerM  = 0.30

	HaikuInputPerM      = 1.00
	HaikuOutputPerM     = 5.00
	HaikuCacheWritePerM = 1.25
	HaikuCacheReadPerM  = 0.10

	OpusInputPerM      = 5.00
	OpusOutputPerM     = 25.00
	OpusCacheWritePerM = 6.25
	OpusCacheReadPerM  = 0.50
)

// modelPricing returns (inputPerM, outputPerM, cacheWritePerM, cacheReadPerM) for a model.
func modelPricing(model string) (float64, float64, float64, float64) {
	switch {
	case strings.Contains(model, "haiku"):
		return HaikuInputPerM, HaikuOutputPerM, HaikuCacheWritePerM, HaikuCacheReadPerM
	case strings.Contains(model, "opus"):
		return OpusInputPerM, OpusOutputPerM, OpusCacheWritePerM, OpusCacheReadPerM
	default: // sonnet or unknown
		return SonnetInputPerM, SonnetOutputPerM, SonnetCacheWritePerM, SonnetCacheReadPerM
	}
}

// CostTracker accumulates token usage and computes costs per model.
// Safe for concurrent use.
type CostTracker struct {
	mu      sync.Mutex
	entries []usageEntry
}

type usageEntry struct {
	model string
	usage TokenUsage
}

// Add accumulates token usage from a response with model context.
func (ct *CostTracker) Add(u *TokenUsage) {
	ct.AddWithModel(u, "")
}

// AddWithModel accumulates token usage tagged with the model that produced it.
func (ct *CostTracker) AddWithModel(u *TokenUsage, model string) {
	if u == nil {
		return
	}
	ct.mu.Lock()
	ct.entries = append(ct.entries, usageEntry{model: model, usage: *u})
	ct.mu.Unlock()
}

// totals returns aggregate token counts across all entries.
func (ct *CostTracker) totals() (input, output, cacheWrite, cacheRead int) {
	for _, e := range ct.entries {
		input += e.usage.InputTokens
		output += e.usage.OutputTokens
		cacheWrite += e.usage.CacheCreationInputTokens
		cacheRead += e.usage.CacheReadInputTokens
	}
	return
}

// Totals returns aggregate token counts across all entries (exported).
func (ct *CostTracker) Totals() (input, output, cacheWrite, cacheRead int) {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	return ct.totals()
}

// Cost returns the total cost in USD using per-model pricing.
func (ct *CostTracker) Cost() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	var total float64
	for _, e := range ct.entries {
		inP, outP, cwP, crP := modelPricing(e.model)
		total += float64(e.usage.InputTokens) / 1e6 * inP
		total += float64(e.usage.OutputTokens) / 1e6 * outP
		total += float64(e.usage.CacheCreationInputTokens) / 1e6 * cwP
		total += float64(e.usage.CacheReadInputTokens) / 1e6 * crP
	}
	return total
}

// CostWithoutCache returns what the cost would have been without caching.
func (ct *CostTracker) CostWithoutCache() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	var total float64
	for _, e := range ct.entries {
		inP, outP, _, _ := modelPricing(e.model)
		allInput := e.usage.InputTokens + e.usage.CacheCreationInputTokens + e.usage.CacheReadInputTokens
		total += float64(allInput) / 1e6 * inP
		total += float64(e.usage.OutputTokens) / 1e6 * outP
	}
	return total
}

// Savings returns the dollar amount saved by caching.
func (ct *CostTracker) Savings() float64 {
	return ct.CostWithoutCache() - ct.Cost()
}

// CacheHitRate returns the percentage of input tokens served from cache.
func (ct *CostTracker) CacheHitRate() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	input, _, cacheWrite, cacheRead := ct.totals()
	total := input + cacheWrite + cacheRead
	if total == 0 {
		return 0
	}
	return float64(cacheRead) / float64(total) * 100
}

// Summary returns a formatted cost summary string.
func (ct *CostTracker) Summary() string {
	cost := ct.Cost()
	savings := ct.Savings()
	rate := ct.CacheHitRate()
	return fmt.Sprintf("$%.2f (saved $%.2f, %.0f%% cache)", cost, savings, rate)
}

// CostForUsage computes the USD cost for a single usage entry with model-specific pricing.
func CostForUsage(u *TokenUsage, model string) float64 {
	if u == nil {
		return 0
	}
	inP, outP, cwP, crP := modelPricing(model)
	var total float64
	total += float64(u.InputTokens) / 1e6 * inP
	total += float64(u.OutputTokens) / 1e6 * outP
	total += float64(u.CacheCreationInputTokens) / 1e6 * cwP
	total += float64(u.CacheReadInputTokens) / 1e6 * crP
	return total
}

// CostWithoutCacheForUsage computes what the cost would have been without caching
// for the given raw token counts and model. Used for monthly savings calculations.
func CostWithoutCacheForUsage(input, output, cacheWrite, cacheRead int, model string) float64 {
	inP, outP, _, _ := modelPricing(model)
	allInput := input + cacheWrite + cacheRead
	return float64(allInput)/1e6*inP + float64(output)/1e6*outP
}
