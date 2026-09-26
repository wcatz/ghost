package embedding

import (
	"strconv"
	"strings"
)

// Task prefixes. Some embedding models are trained on asymmetric input: the
// same encoder is asked for a passage and for a question, and the model only
// produces comparable vectors when it is told which role it is playing.
// nomic-embed-text expects "search_document: " on text to be stored and
// "search_query: " on a search query. Sending both unprefixed embeds them into
// one undifferentiated space, which is measurable but always worse.
//
// They are named constants keyed by model family rather than scattered
// literals at the call sites, because the two halves have to travel together:
// the identity below folds them into the string the store compares, so turning
// a prefix on is what tells the store the old vectors are no longer comparable.

// NomicTaskDocument prefixes text that will be stored and searched over.
const NomicTaskDocument = "search_document: "

// NomicTaskQuery prefixes a search query.
const NomicTaskQuery = "search_query: "

// identityPrefixed is appended to a model identity whose vectors were produced
// with a task prefix. Prefixing changes every vector the model emits, so it is
// part of the space's identity rather than a detail of how it was written.
const identityPrefixed = "+prefix"

// TaskPrefixes is the pair of task prefixes one model family requires. Both
// fields are empty for a family that requires none.
type TaskPrefixes struct {
	Document string
	Query    string
}

// taskPrefixesByFamily keys the required prefixes by model family — the name
// with its tag stripped, so one entry covers every tag of the same weights
// ("nomic-embed-text", "nomic-embed-text:v1.5", "nomic-embed-text:latest").
var taskPrefixesByFamily = map[string]TaskPrefixes{
	"nomic-embed-text": {Document: NomicTaskDocument, Query: NomicTaskQuery},
}

// ModelFamily returns the family of an Ollama model name: everything before the
// tag separator, so "nomic-embed-text:v1.5" and "nomic-embed-text" are one
// family. A name that is only a tag has no family.
func ModelFamily(model string) string {
	if i := strings.IndexByte(model, ':'); i >= 0 {
		return model[:i]
	}
	return model
}

// TaskPrefixesFor returns the task prefixes the model requires, and the empty
// pair for any family not listed above.
func TaskPrefixesFor(model string) TaskPrefixes {
	return taskPrefixesByFamily[ModelFamily(model)]
}

// VectorIdentity returns the string stamped into memory_embeddings.model for
// every vector produced by model at the given dimensions, and the value the
// store compares each stored vector against before letting it into a search
// (see memory.Store.SetEmbeddingIdentity). It reads
// "<model>[:<dimensions>][+prefix]":
//
//   - the model name, because vectors from two models are not comparable even
//     when they happen to share a width;
//   - the dimensions, because a model that can be served at more than one
//     width (nomic-embed-text is Matryoshka-trained) produces a different
//     space per width and the blob length alone would not say which one a row
//     was written at;
//   - the task prefix, because switching it on re-embeds everything and must
//     therefore retire the old vectors instead of mixing the two spaces.
//
// The value is opaque: it is compared for equality and never parsed, which is
// why the delimiters can be unambiguous without a grammar.
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

// prefixed returns the model-required prefix plus text, or text unchanged when
// the model requires no prefix. Written so the empty case allocates nothing
// beyond the concatenation the caller was going to make anyway.
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
