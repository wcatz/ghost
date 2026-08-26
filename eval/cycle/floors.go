package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// floorMetricKeys are the only metric names -floors accepts, matching the
// graded LLM stages. Reflect is count-graded, not P/R-graded, so it has no
// floor keys.
var floorMetricKeys = map[string]bool{
	"supersede_precision": true,
	"supersede_recall":    true,
	"resolve_precision":   true,
	"resolve_recall":      true,
}

// parseFloors parses a comma-separated "metric=min" spec over the stage
// precision/recall metrics. Empty spec -> no floors.
func parseFloors(spec string) (map[string]float64, error) {
	floors := make(map[string]float64)
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return floors, nil
	}
	for _, part := range strings.Split(spec, ",") {
		key, val, found := strings.Cut(strings.TrimSpace(part), "=")
		if !found || key == "" || val == "" {
			return nil, fmt.Errorf("invalid floor %q: want metric=min", part)
		}
		if !floorMetricKeys[key] {
			return nil, fmt.Errorf("unknown floor metric %q (valid: %s)",
				key, joinSortedKeys(floorMetricKeys))
		}
		f, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return nil, fmt.Errorf("invalid floor value for %q: %w", key, err)
		}
		floors[key] = f
	}
	return floors, nil
}

// checkFloors compares stage metrics against floors and returns one
// description per violation, in key-sorted order. Exact equality is not a
// violation. Floors over metrics absent from the map always violate — a
// missing metric means its stage never produced a grade.
func checkFloors(metrics, floors map[string]float64) []string {
	keys := make([]string, 0, len(floors))
	for k := range floors {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var violations []string
	for _, k := range keys {
		got, ok := metrics[k]
		floor := floors[k]
		switch {
		case !ok:
			violations = append(violations, fmt.Sprintf("floor %s=%g: metric missing", k, floor))
		case got < floor:
			violations = append(violations, fmt.Sprintf("floor %s=%g: got %g", k, floor, got))
		}
	}
	return violations
}

func joinSortedKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

// seedOpencodeAuth copies an opencode auth.json into the scratch data dir so
// the sandboxed opencode subprocess finds credentials under its overridden
// XDG_DATA_HOME. Empty path is a no-op so local runs (which need none) stay
// unchanged. CI passes the auth file materialized from an Actions secret.
func seedOpencodeAuth(scratch, authFile string) error {
	if authFile == "" {
		return nil
	}
	b, err := os.ReadFile(authFile)
	if err != nil {
		return fmt.Errorf("read opencode auth file: %w", err)
	}
	dstDir := filepath.Join(scratch, "data", "opencode")
	if err := os.MkdirAll(dstDir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(dstDir, "auth.json")
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		return fmt.Errorf("seed opencode auth: %w", err)
	}
	return nil
}
