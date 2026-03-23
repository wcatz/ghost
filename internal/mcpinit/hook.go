package mcpinit

import (
	"fmt"
	"io"
)

// HandleSessionStartHook is invoked by Claude Code at session start via:
//
//	ghost hook session-start
//
// Its stdout becomes visible in Claude's context, reinforcing Ghost usage.
// Zero DB access, zero network — runs in ~1ms.
func HandleSessionStartHook(stdin io.Reader, stdout io.Writer) {
	_, _ = io.Copy(io.Discard, stdin) // drain stdin
	fmt.Fprintln(stdout, "Ghost memory is active. Before starting work:")
	fmt.Fprintln(stdout, "1. Call ghost_list_projects to discover known projects")
	fmt.Fprintln(stdout, "2. Call ghost_project_context with the project name")
	fmt.Fprintln(stdout, "3. Save discoveries with ghost_memory_save during work")
}
