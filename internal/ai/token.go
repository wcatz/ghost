package ai

// TokenUsage reports the token counts of a CLI-harness call (claude -p
// --output-format json, opencode run --json, codex/goose equivalents). The
// json tags keep the wire shape stable — cli_client.go and opencode_client.go
// parse `usage` from the subprocess's JSON output into this shape.
type TokenUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
}