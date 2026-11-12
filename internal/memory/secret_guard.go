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
	Field  string
	Format string
}

func (e *SecretContentError) Error() string {
	return fmt.Sprintf("%s in %s: %s. Ghost never stores credential values — record where "+
		"the value lives and how to read it, never the value itself",
		ErrSecretContent.Error(), e.Field, e.Format)
}

func (e *SecretContentError) Unwrap() error { return ErrSecretContent }

func rejectSecret(field, text string) error {
	finding, ok := secret.Detect(text)
	if !ok {
		return nil
	}
	return &SecretContentError{Field: field, Format: finding.Label}
}

// RejectSecret is `rejectSecret` under the name a boundary can call: a boundary
// that would NAME a caller-supplied value asks it first, so `ensureProjectFor`
// hands a refused `project_id` back as the guard's own sentence — naming the
// argument while the credential behind it is never repeated. Not every caller
// is a refusal, and `checkoutToBind` is the one that is not: it asks the same
// question and then DECLINES silently (`ok=false`), producing no sentence and
// naming nothing, and the value it guards is the server's own `repoTopLevel`
// checkout rather than a caller's argument. A renderer that has to print such a
// value asks it to choose between naming and withholding (mcpinit's
// `renderMarkerProject`, which names an ordinary project and withholds a
// credential-shaped one).
func RejectSecret(field, text string) error {
	return rejectSecret(field, text)
}

// ProjectIdentifierLabel is what a store-level refusal names the value as when
// the store cannot see the argument's own name.
const ProjectIdentifierLabel = "project identifier"

// ArgEchoMaxBytes is the longest argument a refusal repeats back; see RefusalArg.
const ArgEchoMaxBytes = 256

// ProjectArg renders a caller's argument — the value a `project_id` or `project`
func ProjectArg(field, value string) string {
	refusal := rejectSecret(field, value)
	if refusal == nil {
		return fmt.Sprintf("%q", value)
	}
	var detected *SecretContentError
	if errors.As(refusal, &detected) {
		return fmt.Sprintf("%q", "<"+field+" withheld: it holds a "+detected.Format+">")
	}
	return fmt.Sprintf("%q", "<"+field+" withheld: it holds a credential>")
}

// RefusalArg is ProjectArg for a sentence that REFUSES an argument, and it adds
// one thing: a value longer than ArgEchoMaxBytes is withheld with its length, so
// a refusal never hands the caller back a 200 KB argument. The token guard runs
// on the whole value first, so a token inside an oversized one is still named by
// its format.
func RefusalArg(field, value string) string {
	if len(value) > ArgEchoMaxBytes {
		if refusal := rejectSecret(field, value); refusal != nil {
			return ProjectArg(field, value)
		}
		return fmt.Sprintf("%q", fmt.Sprintf("<%s withheld: it is %d bytes, longer than the %d a refusal repeats>",
			field, len(value), ArgEchoMaxBytes))
	}
	return ProjectArg(field, value)
}

// TokenGuardPlaceholder returns the placeholder a refusal would print for a
// value the token guard withholds, and whether the guard withheld it at all.
func TokenGuardPlaceholder(field, value string) (string, bool) {
	if rejectSecret(field, value) == nil {
		return "", false
	}
	return RefusalArg(field, value), true
}

type secretField struct{ name, text string }

func rejectSecretFields(fields ...secretField) error {
	for _, f := range fields {
		if err := rejectSecret(f.name, f.text); err != nil {
			return err
		}
	}
	return nil
}

func rejectSecretList(field string, values []string) error {
	for i, v := range values {
		if err := rejectSecret(fmt.Sprintf("%s[%d]", field, i), v); err != nil {
			return err
		}
	}
	return nil
}
