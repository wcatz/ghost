package tui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/orchestrator"
	"golang.org/x/term"
)

// IsTerminal returns true if stdin is a terminal (not piped).
func IsTerminal() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// RunOneShot sends a single message and prints the streamed response.
// Used for pipe mode and one-shot CLI mode. No bubbletea involved.
func RunOneShot(session *orchestrator.Session, message string, showCost bool) {
	approvalFn := func(toolName string, toolInput json.RawMessage) bool {
		// In one-shot mode, auto-approve reads, prompt for writes.
		fmt.Fprintf(os.Stderr, "\n%s⚡ [%s]%s ", colorYellow, toolName, colorReset)

		var summary map[string]interface{}
		if err := json.Unmarshal(toolInput, &summary); err == nil {
			if cmd, ok := summary["command"].(string); ok {
				fmt.Fprintf(os.Stderr, "%s\n", cmd)
			} else if path, ok := summary["path"].(string); ok {
				fmt.Fprintf(os.Stderr, "%s\n", path)
			}
		}

		// If not a terminal, auto-approve.
		if !IsTerminal() {
			return true
		}

		fmt.Fprintf(os.Stderr, "Allow? [y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		return response == "y" || response == "yes"
	}

	events := session.Send(nil, message, approvalFn)

	for evt := range events {
		switch evt.Type {
		case "text":
			fmt.Print(evt.Text)
		case "tool_use_start":
			if evt.ToolUse != nil {
				fmt.Fprintf(os.Stderr, "\n%s⚙ [%s]%s ", colorGray, evt.ToolUse.Name, colorReset)
			}
		case "done":
			fmt.Println()
			if showCost && evt.Usage != nil {
				fmt.Fprintf(os.Stderr, "%s[tokens: in=%d out=%d cache_create=%d cache_read=%d]%s\n",
					colorGray, evt.Usage.InputTokens, evt.Usage.OutputTokens,
					evt.Usage.CacheCreationInputTokens, evt.Usage.CacheReadInputTokens, colorReset,
				)
			}
		case "error":
			fmt.Fprintf(os.Stderr, "\n%serror: %v%s\n", colorRed, evt.Error, colorReset)
		}
	}
}

// RunPipe reads all of stdin and sends it as a single one-shot message.
func RunPipe(session *orchestrator.Session, input string, showCost bool) {
	RunOneShot(session, strings.TrimSpace(input), showCost)
}

// sendStreamToStdout is a helper that drains a StreamEvent channel to stdout.
// Used by the legacy REPL's sendMessage.
func sendStreamToStdout(events <-chan ai.StreamEvent, showCost bool) {
	for evt := range events {
		switch evt.Type {
		case "text":
			fmt.Print(evt.Text)
		case "tool_use_start":
			if evt.ToolUse != nil {
				fmt.Printf("\n%s⚙ [%s]%s ", colorGray, evt.ToolUse.Name, colorReset)
			}
		case "done":
			fmt.Println()
			if showCost && evt.Usage != nil {
				fmt.Printf("%s[tokens: in=%d out=%d cache_create=%d cache_read=%d]%s\n",
					colorGray, evt.Usage.InputTokens, evt.Usage.OutputTokens,
					evt.Usage.CacheCreationInputTokens, evt.Usage.CacheReadInputTokens, colorReset,
				)
			}
		case "error":
			fmt.Printf("\n%serror: %v%s\n", colorRed, evt.Error, colorReset)
		}
	}
}
