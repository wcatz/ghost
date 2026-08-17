// internal/ai/fallback_provider.go
package ai

import "context"

// FallbackProvider tries primary first; only on a credit-exhaustion error
// does it fall through to secondary. Any other primary error (invalid key,
// permission denied, network failure) is returned as-is — those aren't
// credit outages and a fallback answer wouldn't fix them next time either.
// secondary may be nil (no fallback exists for this path): a credit
// exhaustion error is then returned unchanged, so the caller fails fast
// instead of degrading.
//
// secondaryIsDryRunOnly documents whether a secondary answer must be treated
// as advisory-only by the caller (no writes). No path currently wires a
// non-nil secondary with secondaryIsDryRunOnly=false — every real secondary
// added to this seam so far (see the sampling path) either doesn't exist
// headless or is dry-run-restricted; the flag exists so a future trusted
// secondary can opt out explicitly rather than silently inheriting a
// permissive default.
type FallbackProvider struct {
	primary               Provider
	secondary             Provider
	secondaryIsDryRunOnly bool
	alwaysFallback        bool
}

// NewFallbackProvider builds a FallbackProvider. secondary may be nil — a
// primary-only provider simply returns the primary's error unfallen-through.
func NewFallbackProvider(primary, secondary Provider, secondaryIsDryRunOnly bool) *FallbackProvider {
	return &FallbackProvider{primary: primary, secondary: secondary, secondaryIsDryRunOnly: secondaryIsDryRunOnly}
}

// NewAlwaysFallbackProvider is like NewFallbackProvider, except it falls
// through to secondary on ANY primary error, not just credit exhaustion.
// Use this where primary has no notion of "credit" at all — e.g. MCP
// sampling, where "Method not found" (client doesn't implement sampling),
// a version-gated protocol rejection, or a user declining the request are
// all equally "this mechanism is unavailable right now," unlike the
// Anthropic API where an invalid key or network failure genuinely wouldn't
// be fixed by retrying against a different provider.
func NewAlwaysFallbackProvider(primary, secondary Provider, secondaryIsDryRunOnly bool) *FallbackProvider {
	return &FallbackProvider{primary: primary, secondary: secondary, secondaryIsDryRunOnly: secondaryIsDryRunOnly, alwaysFallback: true}
}

// DryRunOnlyOnFallback reports whether a ClassifyResult.FromFallback=true
// result must be treated as advisory-only (no writes) by the caller.
func (f *FallbackProvider) DryRunOnlyOnFallback() bool {
	return f.secondaryIsDryRunOnly
}

func (f *FallbackProvider) Classify(ctx context.Context, systemPrompt, userContent string) (ClassifyResult, error) {
	out, err := f.primary.Classify(ctx, systemPrompt, userContent)
	if err == nil {
		return ClassifyResult{Text: out, FromFallback: false}, nil
	}
	if f.secondary == nil || (!f.alwaysFallback && !isCreditExhausted(err)) {
		return ClassifyResult{}, err
	}
	out, err = f.secondary.Classify(ctx, systemPrompt, userContent)
	if err != nil {
		return ClassifyResult{}, err
	}
	return ClassifyResult{Text: out, FromFallback: true}, nil
}
