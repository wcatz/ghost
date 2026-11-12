package main

import (
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/secret"
)

// displayStored renders stored content for a one-line listing, withholding credential values.
func displayStored(content, category string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, category, content)
	}
	return assemble.PreviewLine(content, limit)
}
