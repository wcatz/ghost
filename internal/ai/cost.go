package ai

import (
	"fmt"
	"sync"
)

// Pricing per million tokens (USD) — Claude Sonnet 4.5 / Haiku 4.5.
const (
	SonnetInputPerM       = 3.00
	SonnetOutputPerM      = 15.00
	SonnetCacheWritePerM  = 3.75
	SonnetCacheReadPerM   = 0.30
	HaikuInputPerM        = 0.80
	HaikuOutputPerM       = 4.00
	HaikuCacheWritePerM   = 1.00
	HaikuCacheReadPerM    = 0.08
)

// CostTracker accumulates token usage and computes costs.
// Safe for concurrent use.
type CostTracker struct {
	mu                       sync.Mutex
	InputTokens              int
	OutputTokens             int
	CacheCreationInputTokens int
	CacheReadInputTokens     int
}

// Add accumulates token usage from a response.
func (ct *CostTracker) Add(u *TokenUsage) {
	if u == nil {
		return
	}
	ct.mu.Lock()
	ct.InputTokens += u.InputTokens
	ct.OutputTokens += u.OutputTokens
	ct.CacheCreationInputTokens += u.CacheCreationInputTokens
	ct.CacheReadInputTokens += u.CacheReadInputTokens
	ct.mu.Unlock()
}

// Cost returns the total cost in USD for Sonnet pricing.
func (ct *CostTracker) Cost() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	input := float64(ct.InputTokens) / 1e6 * SonnetInputPerM
	output := float64(ct.OutputTokens) / 1e6 * SonnetOutputPerM
	cacheWrite := float64(ct.CacheCreationInputTokens) / 1e6 * SonnetCacheWritePerM
	cacheRead := float64(ct.CacheReadInputTokens) / 1e6 * SonnetCacheReadPerM
	return input + output + cacheWrite + cacheRead
}

// CostWithoutCache returns what the cost would have been without caching.
func (ct *CostTracker) CostWithoutCache() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	allInput := ct.InputTokens + ct.CacheCreationInputTokens + ct.CacheReadInputTokens
	input := float64(allInput) / 1e6 * SonnetInputPerM
	output := float64(ct.OutputTokens) / 1e6 * SonnetOutputPerM
	return input + output
}

// Savings returns the dollar amount saved by caching.
func (ct *CostTracker) Savings() float64 {
	return ct.CostWithoutCache() - ct.Cost()
}

// CacheHitRate returns the percentage of input tokens served from cache.
func (ct *CostTracker) CacheHitRate() float64 {
	ct.mu.Lock()
	defer ct.mu.Unlock()
	total := ct.InputTokens + ct.CacheCreationInputTokens + ct.CacheReadInputTokens
	if total == 0 {
		return 0
	}
	return float64(ct.CacheReadInputTokens) / float64(total) * 100
}

// Summary returns a formatted cost summary string.
func (ct *CostTracker) Summary() string {
	cost := ct.Cost()
	savings := ct.Savings()
	rate := ct.CacheHitRate()
	return fmt.Sprintf("$%.2f (saved $%.2f, %.0f%% cache)", cost, savings, rate)
}
