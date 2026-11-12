package main

import (
	"github.com/wcatz/ghost/internal/assemble"
	"github.com/wcatz/ghost/internal/secret"
)

// displayStored renders a stored memory for a one-line listing in a report: its
// first line at the listing's width, and withheld whole when the content holds a
// credential.
func displayStored(content, category string, limit int) string {
	if finding, ok := secret.Detect(content); ok {
		return withheld(finding, category, content)
	}
	return assemble.PreviewLine(content, limit)
}
