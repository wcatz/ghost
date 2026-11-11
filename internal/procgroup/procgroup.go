// Package procgroup holds the process-group mechanics shared by the two
// places Ghost confines a child that may spawn helpers of its own: the
// lifecycle phase spawn (cmd/ghost) and the LLM-harness spawn (internal/ai).
package procgroup
