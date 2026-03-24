package reflection

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// OllamaConsolidator uses a local Ollama model for consolidation.
// Mid-tier: free, runs locally, quality depends on model.
type OllamaConsolidator struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllamaConsolidator creates a consolidator that calls Ollama's /api/chat.
func NewOllamaConsolidator(baseURL, model string) *OllamaConsolidator {
	return &OllamaConsolidator{
		baseURL: baseURL,
		model:   model,
		client:  &http.Client{Timeout: 90 * time.Second},
	}
}

func (o *OllamaConsolidator) Name() string { return "ollama:" + o.model }

func (o *OllamaConsolidator) Available(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, o.baseURL+"/", nil)
	if err != nil {
		return false
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

type ollamaChatRequest struct {
	Model    string          `json:"model"`
	Messages []ollamaMessage `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format"`
	Options  map[string]any  `json:"options,omitempty"`
}

type ollamaMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ollamaChatResponse struct {
	Message ollamaMessage `json:"message"`
}

// reflectionSchema enforces structured JSON output via Ollama's GBNF grammar.
var reflectionSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"learned_context": {"type": "string"},
		"memories": {
			"type": "array",
			"items": {
				"type": "object",
				"properties": {
					"category": {"type": "string", "enum": ["architecture","decision","pattern","convention","gotcha","dependency","preference","fact"]},
					"content": {"type": "string"},
					"importance": {"type": "number"},
					"tags": {"type": "array", "items": {"type": "string"}}
				},
				"required": ["category", "content", "importance", "tags"]
			}
		}
	},
	"required": ["learned_context", "memories"]
}`)

func (o *OllamaConsolidator) Consolidate(ctx context.Context, input ReflectionInput) (ReflectionResult, error) {
	prompt := BuildReflectionPrompt(input)

	reqBody := ollamaChatRequest{
		Model: o.model,
		Messages: []ollamaMessage{
			{Role: "system", Content: "You analyze a software developer's coding patterns and produce structured memory output. Return ONLY valid JSON matching the required schema. Be specific and actionable."},
			{Role: "user", Content: prompt},
		},
		Stream:  false,
		Format:  reflectionSchema,
		Options: map[string]any{"temperature": 0},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return ReflectionResult{}, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, o.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return ReflectionResult{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := o.client.Do(req)
	if err != nil {
		return ReflectionResult{}, fmt.Errorf("ollama chat: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return ReflectionResult{}, fmt.Errorf("ollama returned %d", resp.StatusCode)
	}

	var chatResp ollamaChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&chatResp); err != nil {
		return ReflectionResult{}, fmt.Errorf("decode response: %w", err)
	}

	return parseReflectionResponse(chatResp.Message.Content), nil
}
