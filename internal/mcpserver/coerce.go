package mcpserver

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/wcatz/ghost/internal/memory"
)

func optFloat32(v any, name string) (*float32, error) {
	f, err := toFloat64(v, name)
	if err != nil || f == nil {
		return nil, err
	}
	n := float32(*f)
	return &n, nil
}

func defaultImportanceArg(v any, fallback float32) (float32, error) {
	p, err := optFloat32(v, "importance")
	if err != nil {
		return 0, err
	}
	return defaultImportance(p, fallback), nil
}

func optInt(v any, name string) (*int, error) {
	f, err := toFloat64(v, name)
	if err != nil || f == nil {
		return nil, err
	}
	if *f != float64(int64(*f)) {
		return nil, fmt.Errorf("%s must be an integer, got a fractional number", name)
	}
	n := int(int64(*f))
	return &n, nil
}

func optStringSlice(v any, name string) ([]string, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case []string:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for i, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be an array of strings, got element %d of type %T", name, i+1, e)
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
		return nil, fmt.Errorf("%s must be an array of strings, got %s", name, memory.RefusalArg(name, t))
	default:
		return nil, fmt.Errorf("%s must be an array of strings, got %T", name, v)
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
			return nil, fmt.Errorf("%s must be a number, got %s", name, memory.RefusalArg(name, t.String()))
		}
		return finiteFloat64(f, name, t.String())
	case string:
		trimmed := strings.TrimSpace(t)
		f, err := strconv.ParseFloat(trimmed, 64)
		if err != nil {
			return nil, fmt.Errorf("%s must be a number, got %s", name, memory.RefusalArg(name, t))
		}
		return finiteFloat64(f, name, trimmed)
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
		return nil, fmt.Errorf("%s must be a number, got %T", name, v)
	}
}

func finiteFloat64(f float64, name, raw string) (*float64, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%s must be a finite number, got %q", name, raw)
	}
	return &f, nil
}

func optScope(v any, name string) (map[string]string, error) {
	if v == nil {
		return nil, nil
	}
	switch t := v.(type) {
	case map[string]string:
		if len(t) == 0 {
			return nil, nil
		}
		return t, nil
	case map[string]any:
		if len(t) == 0 {
			return nil, nil
		}
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]string, len(t))
		for i, k := range keys {
			s, ok := t[k].(string)
			if !ok {
				return nil, fmt.Errorf("%s entry %d (in key order) must be a string value, got %T", name, i+1, t[k])
			}
			out[k] = s
		}
		return out, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(t), &m); err != nil {
			return nil, fmt.Errorf("%s must be a JSON object of string values: %w", name, err)
		}
		if len(m) == 0 {
			return nil, nil
		}
		return m, nil
	default:
		return nil, fmt.Errorf("%s must be an object of string values, got %T", name, v)
	}
}
