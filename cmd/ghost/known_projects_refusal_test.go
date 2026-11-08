package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/wcatz/ghost/internal/memory"
)

// TestKnownProjectRefusalsRenderNamesAsLabels holds the refusals that offer a
// reader the projects a store already knows: both operands of `ghost project
// merge` and the id `ghost project bind` could not find. They name stored text,
// so each name goes through assemble.Label — the renderer the neighbouring
// knownProjectsSentence already uses — rather than being interpolated raw.
//
// The fixture is a name holding U+202E, the right-to-left override: the import
// predicate accepts category Cf on purpose, so a store can hold this name, and
// printed raw it reorders the rest of the line a terminal or log shows. Label
// writes it as the literal escape, which is the only form that cannot reorder
// anything.
func TestKnownProjectRefusalsRenderNamesAsLabels(t *testing.T) {
	const (
		hostileName = "safe\u202Eevil"
		escapedName = `safe\u202eevil`
		plainName   = "ordinary project"
	)
	ctx := context.Background()

	newStore := func(t *testing.T) *memory.Store {
		t.Helper()
		store := bindStore(t)
		if err := store.EnsureProject(ctx, "proj", "/tmp/proj", hostileName); err != nil {
			t.Fatalf("EnsureProject hostile: %v", err)
		}
		if err := store.EnsureProject(ctx, "plain", "/tmp/plain", plainName); err != nil {
			t.Fatalf("EnsureProject plain: %v", err)
		}
		return store
	}

	for _, tc := range []struct {
		name string
		run  func(t *testing.T, store *memory.Store) error
	}{
		{
			name: "project merge refuses the old operand",
			run: func(t *testing.T, store *memory.Store) error {
				var out bytes.Buffer
				return runProjectMergeCore(ctx, store, &out, "no-such-project", "proj")
			},
		},
		{
			name: "project merge refuses the new operand",
			run: func(t *testing.T, store *memory.Store) error {
				var out bytes.Buffer
				return runProjectMergeCore(ctx, store, &out, "proj", "no-such-project")
			},
		},
		{
			name: "project bind refuses an unknown id",
			run: func(t *testing.T, store *memory.Store) error {
				var out bytes.Buffer
				return runProjectBindCore(ctx, store, &out, "no-such-id", t.TempDir(), noRemote)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run(t, newStore(t))
			if err == nil {
				t.Fatal("the refusal was accepted")
			}
			msg := err.Error()
			if !strings.Contains(strings.ToLower(msg), "known projects:") {
				t.Fatalf("the refusal does not list the known projects:\n%s", msg)
			}
			if strings.Contains(msg, hostileName) {
				t.Errorf("the refusal printed the stored name raw:\n%s", msg)
			}
			if !strings.Contains(msg, escapedName) {
				t.Errorf("the refusal does not print the stored name as a label:\n%s", msg)
			}
			// An ordinary name is byte-identical, which is what a label is for:
			// a renderer that withheld or quoted everything would keep the
			// hostile name out and stop offering the names to type.
			if !strings.Contains(msg, plainName) {
				t.Errorf("the refusal does not leave an ordinary name as written:\n%s", msg)
			}
		})
	}
}
