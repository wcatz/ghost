package memory

import (
	"errors"
	"fmt"

	"github.com/wcatz/ghost/internal/secret"
)

// ErrSecretContent is the sentinel every credential refusal unwraps to, so a
// caller can tell "Ghost will not store this" from "the database was
// unreachable" without matching on the message.
var ErrSecretContent = errors.New("refusing to store credential-shaped content")

// SecretContentError is the refusal a write path returns for a value
// internal/secret recognised. It names the field and the format and nothing
// else: the message reaches the log file, the saving agent's context, and — for
// reflection — a prompt sent to a third-party model, so quoting the value would
// relocate the secret rather than contain it.
type SecretContentError struct {
	// Field is the caller's name for the argument that was refused, e.g.
	// "content" or "rationale". It is what makes the refusal actionable: an
	// agent that put a credential in the rationale of a decision needs to be
	// told it was the rationale.
	Field string
	// Format is secret.Finding's human-readable label, e.g. "AWS access key
	// ID". It tells the saving agent which value to remove without showing it
	// the value.
	Format string
}

func (e *SecretContentError) Error() string {
	return fmt.Sprintf("%s in %s: %s. Ghost never stores credential values — record where "+
		"the value lives and how to read it, never the value itself",
		ErrSecretContent.Error(), e.Field, e.Format)
}

func (e *SecretContentError) Unwrap() error { return ErrSecretContent }

// rejectSecret returns a *SecretContentError when text holds a credential. It
// is the store's single seam for the guard, called before any statement on
// every path that writes caller-supplied text: refusing first means a caller
// that retries cannot race a partial write, and it means the refusal is the
// same error whatever the store was doing when the value arrived.
func rejectSecret(field, text string) error {
	finding, ok := secret.Detect(text)
	if !ok {
		return nil
	}
	return &SecretContentError{Field: field, Format: finding.Label}
}

// secretField is one caller-supplied text to check, named the way the caller
// named the argument so the refusal says which one to fix.
type secretField struct{ name, text string }

// rejectSecretFields checks the named fields in order and returns the first
// refusal. It is the form every multi-field writer uses, so the field in the
// error is one the caller recognises.
func rejectSecretFields(fields ...secretField) error {
	for _, f := range fields {
		if err := rejectSecret(f.name, f.text); err != nil {
			return err
		}
	}
	return nil
}

// rejectSecretList checks every entry of a list-valued field and names the
// offending one by index. decisions.alternatives is the reason this exists: it
// is rendered back to the agent by ghost_decisions_list, so an entry is stored
// and replayed exactly like the rationale beside it — the only difference is
// that it is a list, and "alternatives" alone would not say which entry to fix.
func rejectSecretList(field string, values []string) error {
	for i, v := range values {
		if err := rejectSecret(fmt.Sprintf("%s[%d]", field, i), v); err != nil {
			return err
		}
	}
	return nil
}

// The guard's reach, and its deliberate limits.
//
// Reached: every write of text a caller supplied, whether it arrives through a
// tool, an import, or a model — UpsertWithOptions (and so Upsert,
// UpsertWithProvenance, the MCP save tools, the Claude first-contact import,
// and reflection's candidate writes), UpdateMemory, RecordDecision, the three
// task writers, UpdateLearnedContext, and the three portable importers. The
// importers matter more than their position in this list suggests: they write
// with raw INSERTs rather than through Upsert, and their input is a JSONL
// artifact that arrived from somewhere.
//
// Not reached, on purpose:
//
//   - Store.Create, RestoreSnapshot and SeedGlobalMemories. These write
//     byte-exact data into a throwaway or restored store: bench/eval seeders
//     and dataset fixtures, a snapshot restore, and Ghost's own builtin seeds.
//     A detector cannot be allowed to make a benchmark's third-party corpus
//     unloadable, and a restore that silently dropped rows would be worse than
//     the leak it prevents. The same three are outside the MaxContentLen
//     contract in content.go, for the same reason and the same reason only.
//   - Content already in the database. This guard reads what a caller is
//     trying to write; it does not sweep rows a previous version stored. Doing
//     that is a separate, report-first job — a detection pass over existing
//     rows has to be able to tell an operator what it found without changing
//     their memory store under them. Note that the portable importers are NOT
//     in this second category: an artifact is untrusted input arriving now,
//     not a row that was already here.
//
// The consequence of the first limit is stated plainly: a caller that reaches
// Create directly bypasses the guard. Create is a raw "insert exactly this"
// primitive on the provider.MemoryStore interface and has no production caller
// — the bench harness is its only user — so the exposure is a future caller
// reading the interface rather than a hole in a shipped path. If that changes,
// the guard moves to Create with it.
