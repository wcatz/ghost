package mcpserver

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Some MCP clients serialize numeric and array tool arguments as JSON strings
// when the advertised input schema uses a type union (e.g. ["null","number"],
// which go-sdk infers for optional pointer/slice fields). The stringified
// value then fails schema validation ("has type string, want one of
// null, number") before it ever reaches a handler, making importance,
// priority, and tags-style arguments unusable from those clients.
//
// The fields below are therefore declared as `any` in their arg structs:
// jsonschema-go infers an unconstrained schema (property `true`), so
// validation cannot reject them, and each handler normalizes the value with
// these helpers before use. Native numbers/arrays pass through unchanged;
// stringified forms are parsed; anything else is a clear per-field error
// rather than a silent default.

// optFloat32 normalizes an optional numeric argument to a *float32.
// nil (absent or JSON null) returns nil, preserving "omit to keep current".
func optFloat32(v any, name string) (*float32, error) {
	f, err := toFloat64(v, name)
	if err != nil || f == nil {
		return nil, err
	}
	n := float32(*f)
	return &n, nil
}

// defaultImportanceArg normalizes an importance argument and applies the
// tool's documented default when it is absent.
func defaultImportanceArg(v any, fallback float32) (float32, error) {
	p, err := optFloat32(v, "importance")
	if err != nil {
		return 0, err
	}
	return defaultImportance(p, fallback), nil
}

// optInt normalizes an optional numeric argument to a *int. The value must be
// integer-valued (2, "2", 2.0); a fractional number is an error.
func optInt(v any, name string) (*int, error) {
	f, err := toFloat64(v, name)
	if err != nil || f == nil {
		return nil, err
	}
	if *f != float64(int64(*f)) {
		return nil, fmt.Errorf("%s must be an integer, got %v", name, v)
	}
	n := int(int64(*f))
	return &n, nil
}

// optStringSlice normalizes an optional array argument to a []string. It
// accepts []string, []any of strings, and a JSON-encoded array string
// (e.g. "[\"a\",\"b\"]" — how stringifying clients serialize arrays). A
// nil/absent value returns nil so callers can distinguish "omit" from
// "clear" (empty slice).
func optStringSlice(v any, name string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of strings, got element %v (%T)", name, e, e)
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return nil, fmt.Errorf("%s: expected an array of strings, got empty string", name)
		}
		var out []string
		if err := json.Unmarshal([]byte(trimmed), &out); err == nil {
			return out, nil
		}
		// A bare []any that failed the element check above would have
		// errored already; here only a malformed or non-array string lands.
		return nil, fmt.Errorf("%s must be an array of strings, got %q", name, t)
	default:
		return nil, fmt.Errorf("%s must be an array of strings, got %v (%T)", name, v, v)
	}
}

func toFloat64(v any, name string) (*float64, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case float64:
		return &t, nil
	case float32:
		f := float64(t)
		return &f, nil
	case int:
		f := float64(t)
		return &f, nil
	case int64:
		f := float64(t)
		return &f, nil
	case json.Number:
		f, err := t.Float64()
		if err != nil {
			return nil, fmt.Errorf("%s must be a number, got %q", name, t.String())
		}
		return &f, nil
	case string:
		trimmed := strings.TrimSpace(t)
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a number, got %q", name, t)
		}
		return &f, nil
	case *float32:
		if t == nil {
			return nil, nil
		}
		f := float64(*t)
		return &f, nil
	case *float64:
		if t == nil {
			return nil, nil
		}
		return t, nil
	case *int:
		if t == nil {
			return nil, nil
		}
		f := float64(*t)
		return &f, nil
	default:
		return nil, fmt.Errorf("%s must be a number, got %v (%T)", name, v, v)
	}
}
