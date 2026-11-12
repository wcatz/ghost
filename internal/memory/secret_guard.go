package memory

import (
	"errors"
	"fmt"

	"github.com/wcatz/ghost/internal/secret"
)

// ErrSecretContent is the sentinel every credential refusal unwraps to, so a
var ErrSecretContent = errors.New("refusing to store credential-shaped content")

// SecretContentError is the refusal a write path returns for a value
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
func RejectSecret(field, text string) error {
	return rejectSecret(field, text)
}

// ProjectIdentifierLabel is what a store-level refusal names the value as when
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
