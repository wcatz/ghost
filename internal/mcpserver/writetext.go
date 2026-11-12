package mcpserver

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/wcatz/ghost/internal/memory"
)

const maxProjectIDLen = 4096

func blankWriteText(value string) bool {
	return strings.TrimFunc(value, func(r rune) bool {
		return unicode.IsSpace(r) || memory.IsFormatRune(r)
	}) == ""
}

func validateRequiredText(field, value string) error {
	if blankWriteText(value) {
		return fmt.Errorf("%s is required and must not be empty or blank", field)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return fmt.Errorf("%s must not contain a NUL byte", field)
	}
	return nil
}

func validateProjectIDArgument(projectID string) error {
	if err := validateRequiredText("project_id", projectID); err != nil {
		return err
	}
	if len(projectID) > maxProjectIDLen {
		return fmt.Errorf("project_id must be at most %d bytes, got %d — it is a project name or a checkout path, not a document",
			maxProjectIDLen, len(projectID))
	}
	return nil
}
