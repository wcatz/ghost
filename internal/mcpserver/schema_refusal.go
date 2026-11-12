package mcpserver

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

const schemaRefusalPrefix = `validating "arguments": `

const schemaRefusalField = "argument"

const schemaRefusalMaxBytes = 1024

const schemaRefusalTruncationMarker = "…[refusal truncated]"

func withholdSchemaRefusal(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		if err != nil || method != "tools/call" {
			return res, err
		}
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || call.Params == nil {
			return res, err
		}
		result, ok := res.(*mcp.CallToolResult)
		if !ok || !result.IsError {
			return res, err
		}
		text, content, ok := schemaRefusalText(result)
		if !ok {
			return res, err
		}
		bounded := boundSchemaRefusal(text, call.Params.Arguments)
		if bounded != text {
			content.Text = bounded
		}
		return res, err
	}
}

func schemaRefusalText(result *mcp.CallToolResult) (string, *mcp.TextContent, bool) {
	for _, c := range result.Content {
		text, ok := c.(*mcp.TextContent)
		if !ok {
			continue
		}
		if strings.HasPrefix(text.Text, schemaRefusalPrefix) {
			return text.Text, text, true
		}
	}
	return "", nil, false
}

func boundSchemaRefusal(text string, args json.RawMessage) string {
	for _, form := range withheldArgumentForms(args) {
		text = strings.ReplaceAll(text, form.from, form.to)
	}
	if len(text) > schemaRefusalMaxBytes {
		text = clampSchemaRefusal(text, schemaRefusalMaxBytes) + schemaRefusalTruncationMarker
	}
	return text
}

type argumentForm struct {
	from string
	to   string
}

func withheldArgumentForms(args json.RawMessage) []argumentForm {
	if len(args) == 0 {
		return nil
	}
	var decoded any
	if err := json.Unmarshal(args, &decoded); err != nil {
		return nil
	}
	var forms []argumentForm
	collectWithheldForms(decoded, &forms)
	sort.SliceStable(forms, func(i, j int) bool { return len(forms[i].from) > len(forms[j].from) })
	return forms
}

func collectWithheldForms(value any, forms *[]argumentForm) {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			collectWithheldForms(key, forms)
			collectWithheldForms(item, forms)
		}
	case []any:
		for _, item := range v {
			collectWithheldForms(item, forms)
		}
	case string:
		shown := memory.RefusalArg(schemaRefusalField, v)
		if shown == strconv.Quote(v) {
			return
		}
		*forms = append(*forms,
			argumentForm{from: strconv.Quote(v), to: shown},
			argumentForm{from: v, to: shown})
	}
}

func clampSchemaRefusal(text string, n int) string {
	if n <= 0 || len(text) <= n {
		return text
	}
	cut := n
	for cut > 0 && text[cut]&0xC0 == 0x80 {
		cut--
	}
	return text[:cut]
}
