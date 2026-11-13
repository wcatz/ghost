package assemble

// RetrievalWindow is the window Run will ask the retriever for on this request.
// Exported so a caller whose own path has to describe the same window asks here
// instead of repeating the rule: explain mode reports the ranking of a window,
// and a diagnosis of a window the tool does not use describes nothing.
func RetrievalWindow(req Request) int { return retrievalWindow(req) }
