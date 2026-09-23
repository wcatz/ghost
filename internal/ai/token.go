package ai

// TokenUsage is the provider-neutral wire shape reserved for future token
// accounting. Current CLI subprocess adapters return a zero value because they
// do not parse provider usage metadata; the JSON tags preserve the shape for
// callers and future adapters.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}
