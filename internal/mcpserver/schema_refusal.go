package mcpserver

// The SDK validates a tool call's arguments against the tool's advertised input
// schema BEFORE any handler runs, and its refusal repeats the caller's value
// and any unexpected property name verbatim: `validating "arguments": validating
// root: validating /properties/pin: type: ghp_… has type "string", want
// "boolean"` carries the whole value, and a property name of any length comes
// back whole inside `unexpected additional properties`. None of it reaches
// `memory.RefusalArg`, so the token guard and the echo bound every other refusal
// on this surface obeys are bypassed on the one refusal a client reaches with a
// single wrong-typed argument or an extra property — and the SDK produces it
// before Ghost sees the call at all.
//
// The refusal is the SDK's and the SDK owns the wording, so this file does not
// re-validate the arguments and does not re-word the refusal. It withholds the
// CALLER TEXT the wording carries, which is the only part of it that is not the
// schema's, and it bounds what is left. Every rendering of a value the SDK
// prints comes from the arguments — the value itself, and a property name — so
// the renderings to withhold are the caller's own, found in the arguments the
// request carries.
//
// The withholding is `memory.RefusalArg`'s, the renderer every other refusal on
// this surface uses: a token-shaped value comes back as a placeholder naming the
// format the guard found, and one over 256 bytes comes back as a placeholder
// naming its length. An ordinary value is left exactly as the SDK printed it,
// because a refusal that names an ordinary value is the diagnostic an agent
// needs and the guard has no verdict on it.
//
// The label in the placeholder is a constant, and it cannot be the caller's own
// name for the argument: on the path this file exists for, the caller's name
// for the argument IS the text being withheld.

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wcatz/ghost/internal/memory"
)

// schemaRefusalPrefix is the SDK's marker on a refusal it produces before any
// handler runs: the arguments did not match the tool's advertised input
// schema. It is the only prefix on this path — every other refusal a tool call
// can produce is Ghost's own sentence, and those name their argument through
// `memory.RefusalArg` already.
const schemaRefusalPrefix = `validating "arguments": `

// schemaRefusalField is what a withheld caller text is named as in the
// placeholder that replaces it: the SDK's own word for what it validated.
const schemaRefusalField = "argument"

// schemaRefusalMaxBytes bounds the refusal this file renders. A refusal is a
// diagnostic and not an answer, and the one shape that can grow without limit
// is a list of unexpected property names — a caller can send as many as it
// likes, and the SDK prints every one. Past the bound the refusal is cut and
// marked, the way `assemble` cuts a rendered value, so the reader can see that
// the sentence stops there rather than reading a whole list as the whole
// refusal.
const schemaRefusalMaxBytes = 1024

// schemaRefusalTruncationMarker marks a refusal cut at the bound above.
const schemaRefusalTruncationMarker = "…[refusal truncated]"

// withholdSchemaRefusal is the receiving middleware that keeps the SDK's
// schema-validation refusal from repeating the caller's value.
//
// It is the same shape as withholdResourceReadURI, and for the same reason: a
// refusal the SDK produces on its own never reaches a Ghost handler, so it
// bypasses every `memory.RefusalArg` on this path, and a client reaches it with
// one wrong-typed argument or one extra property. The URI there was a project
// segment; here it is the caller's own argument, which is the one value an
// agent pastes a token into.
//
// A refusal this file does not recognise is left exactly as it is: it is either
// Ghost's own sentence (which the guard already reached) or a shape this file
// has no rendering for, and the guard on the value has already run inside
// `memory.RefusalArg` for the first.
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

// schemaRefusalText returns the text of the SDK's schema refusal and the
// content block it lives in, so the block can be rewritten in place. A result
// that carries no such refusal is not one this file has anything to say about.
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

// boundSchemaRefusal renders the SDK's schema refusal so that it carries no
// caller text the guard would withhold, and no more of it than a refusal may
// repeat.
func boundSchemaRefusal(text string, args json.RawMessage) string {
	for _, form := range withheldArgumentForms(args) {
		text = strings.ReplaceAll(text, form.from, form.to)
	}
	if len(text) > schemaRefusalMaxBytes {
		text = clampSchemaRefusal(text, schemaRefusalMaxBytes) + schemaRefusalTruncationMarker
	}
	return text
}

// argumentForm is one rendering of caller text a refusal may carry, and the
// bounded rendering that replaces it.
type argumentForm struct {
	from string
	to   string
}

// withheldArgumentForms returns the renderings of the caller's arguments that
// a refusal must not carry.
//
// BOTH renderings of a withheld string are returned, because the SDK prints a
// value raw (`type: ghp_… has type "string"`) and a property name quoted
// (`unexpected additional properties ["ghp_…"]`). The quoted form is the
// longer one, so the longest-first order below replaces it before the raw one
// — and the raw one cannot then land inside a placeholder that quotes it
// again.
//
// The longest rendering comes first for the other reason too: a value that
// contains another withheld value is printed whole, so replacing the shorter
// one first would leave the rest of the longer one in the sentence.
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

// collectWithheldForms walks the decoded arguments and collects every string
// the guard would withhold — a value and a property name alike, because the SDK
// prints an unknown property's name back to the caller.
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

// clampSchemaRefusal cuts text to at most n bytes on a rune boundary, so a
// multi-byte character is never cut in half and the result is still valid
// UTF-8.
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
