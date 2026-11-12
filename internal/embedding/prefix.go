package embedding

import (
	"strconv"
	"strings"
)

// NomicTaskDocument prefixes stored document text for nomic models.
const NomicTaskDocument = "search_document: "

// NomicTaskQuery prefixes search queries for nomic models.
const NomicTaskQuery = "search_query: "

const identityPrefixed = "+prefix"

// TaskPrefixes holds the document and query prefixes for a model family.
type TaskPrefixes struct {
	Document string
	Query    string
}

var taskPrefixesByFamily = map[string]TaskPrefixes{
	"nomic-embed-text": {Document: NomicTaskDocument, Query: NomicTaskQuery},
}

// ModelFamily returns the normalized family name for an Ollama model.
func ModelFamily(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		model = model[i+1:]
	}
	if i := strings.IndexByte(model, ':'); i >= 0 {
		return model[:i]
	}
	return model
}

// TaskPrefixesFor returns the task prefixes required by model.
func TaskPrefixesFor(model string) TaskPrefixes {
	return taskPrefixesByFamily[ModelFamily(model)]
}

// VectorIdentity returns the identity string for vectors from model and dimensions.
func VectorIdentity(model string, dimensions int) string {
	identity := model
	if dimensions > 0 {
		identity = model + ":" + strconv.Itoa(dimensions)
	}
	if TaskPrefixesFor(model).Document != "" {
		identity += identityPrefixed
	}
	return identity
}

func (p TaskPrefixes) prefixedDocument(text string) string {
	if p.Document == "" {
		return text
	}
	return p.Document + text
}

func (p TaskPrefixes) prefixedQuery(text string) string {
	if p.Query == "" {
		return text
	}
	return p.Query + text
}

// PrefixedDocument returns text prefixed for document embedding.
func PrefixedDocument(model, text string) string {
	return TaskPrefixesFor(model).prefixedDocument(text)
}

// PrefixedQuery returns text prefixed for query embedding.
func PrefixedQuery(model, text string) string {
	return TaskPrefixesFor(model).prefixedQuery(text)
}
