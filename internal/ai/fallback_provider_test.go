// internal/ai/fallback_provider_test.go
package ai

import (
	"context"
	"errors"
	"testing"
)

type fakeProvider struct {
	text            string
	err             error
	gotSystemPrompt string
	gotUserContent  string
}

func (f *fakeProvider) Classify(ctx context.Context, systemPrompt, userContent string) (string, error) {
	f.gotSystemPrompt = systemPrompt
	f.gotUserContent = userContent
	return f.text, f.err
}

func TestFallbackProvider_PrimarySucceeds(t *testing.T) {
	primary := &fakeProvider{text: "KEEP"}
	fp := NewFallbackProvider(primary, &fakeProvider{text: "RESOLVED"}, true)
	res, err := fp.Classify(context.Background(), "sys", "content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if primary.gotSystemPrompt != "sys" || primary.gotUserContent != "content" {
		t.Errorf("primary got (%q, %q), want (sys, content) forwarded unmodified", primary.gotSystemPrompt, primary.gotUserContent)
	}
	if res.Text != "KEEP" || res.FromFallback {
		t.Errorf("got %+v, want {KEEP false}", res)
	}
}

func TestFallbackProvider_CreditExhaustionFallsThrough(t *testing.T) {
	secondary := &fakeProvider{text: "RESOLVED"}
	fp := NewFallbackProvider(&fakeProvider{err: ErrCreditExhausted}, secondary, true)
	res, err := fp.Classify(context.Background(), "sys", "content")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Text != "RESOLVED" || !res.FromFallback {
		t.Errorf("got %+v, want {RESOLVED true}", res)
	}
	if secondary.gotSystemPrompt != "sys" || secondary.gotUserContent != "content" {
		t.Errorf("secondary got (%q, %q), want (sys, content) forwarded unmodified", secondary.gotSystemPrompt, secondary.gotUserContent)
	}
}

func TestFallbackProvider_OtherErrorFailsFast(t *testing.T) {
	primaryErr := errors.New("invalid API key — check ghost config")
	fp := NewFallbackProvider(&fakeProvider{err: primaryErr}, &fakeProvider{text: "RESOLVED"}, true)
	_, err := fp.Classify(context.Background(), "sys", "content")
	if !errors.Is(err, primaryErr) {
		t.Errorf("got %v, want %v (secondary must not be tried)", err, primaryErr)
	}
}

func TestFallbackProvider_NoSecondary_CreditExhaustionFailsFast(t *testing.T) {
	fp := NewFallbackProvider(&fakeProvider{err: ErrCreditExhausted}, nil, false)
	_, err := fp.Classify(context.Background(), "sys", "content")
	if !errors.Is(err, ErrCreditExhausted) {
		t.Errorf("got %v, want ErrCreditExhausted", err)
	}
}

func TestFallbackProvider_SecondaryAlsoFails(t *testing.T) {
	secondaryErr := errors.New("secondary provider: connection refused")
	fp := NewFallbackProvider(&fakeProvider{err: ErrCreditExhausted}, &fakeProvider{err: secondaryErr}, true)
	_, err := fp.Classify(context.Background(), "sys", "content")
	if !errors.Is(err, secondaryErr) {
		t.Errorf("got %v, want %v", err, secondaryErr)
	}
}

func TestFallbackProvider_DryRunOnlyOnFallback(t *testing.T) {
	fp := NewFallbackProvider(&fakeProvider{}, &fakeProvider{}, true)
	if !fp.DryRunOnlyOnFallback() {
		t.Error("expected DryRunOnlyOnFallback true")
	}
	fp2 := NewFallbackProvider(&fakeProvider{}, nil, false)
	if fp2.DryRunOnlyOnFallback() {
		t.Error("expected DryRunOnlyOnFallback false")
	}
}
