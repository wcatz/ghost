package tui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/wcatz/ghost/internal/orchestrator"
	"github.com/wcatz/ghost/internal/provider"
)

// Colors for terminal output.
const (
	colorReset  = "\033[0m"
	colorGreen  = "\033[32m"
	colorYellow = "\033[33m"
	colorCyan   = "\033[36m"
	colorGray   = "\033[90m"
	colorBold   = "\033[1m"
	colorRed    = "\033[31m"
)

// REPL runs the interactive read-eval-print loop.
type REPL struct {
	orch    *orchestrator.Orchestrator
	active  *orchestrator.Session
	ctx     context.Context
	cancel  context.CancelFunc
	showCost bool
}

// NewREPL creates a new interactive REPL.
func NewREPL(orch *orchestrator.Orchestrator, showCost bool) *REPL {
	ctx, cancel := context.WithCancel(context.Background())
	return &REPL{
		orch:     orch,
		ctx:      ctx,
		cancel:   cancel,
		showCost: showCost,
	}
}

// Run starts the REPL loop.
func (r *REPL) Run(initialSession *orchestrator.Session) error {
	r.active = initialSession

	fmt.Printf("%s%sGhost%s v0.1.0 | %s%s%s (%s, %s)\n",
		colorBold, colorCyan, colorReset,
		colorGreen, r.active.ProjectName, colorReset,
		r.active.ProjectPath, r.active.Mode.Name,
	)
	fmt.Printf("Type %s/help%s for commands, %s/quit%s to exit.\n\n", colorYellow, colorReset, colorYellow, colorReset)

	scanner := bufio.NewScanner(os.Stdin)
	for {
		prompt := fmt.Sprintf("%s%s%s/%s%s> ",
			colorGreen, r.active.ProjectName, colorReset,
			colorCyan, r.active.Mode.Name,
		)
		fmt.Print(prompt + colorReset)

		if !scanner.Scan() {
			break
		}

		input := strings.TrimSpace(scanner.Text())
		if input == "" {
			continue
		}

		// Handle REPL commands.
		if strings.HasPrefix(input, "/") {
			if quit := r.handleCommand(input); quit {
				return nil
			}
			continue
		}

		// Send to the agent.
		r.sendMessage(input)
	}

	return scanner.Err()
}

// RunOneShot sends a single message and exits.
func (r *REPL) RunOneShot(session *orchestrator.Session, message string) error {
	r.active = session
	r.sendMessage(message)
	return nil
}

func (r *REPL) sendMessage(input string) {
	approvalFn := func(toolName string, toolInput json.RawMessage) provider.ApprovalResponse {
		fmt.Printf("\n%s⚡ [%s]%s ", colorYellow, toolName, colorReset)

		// Show a summary of what the tool wants to do.
		var summary map[string]interface{}
		if err := json.Unmarshal(toolInput, &summary); err == nil {
			if cmd, ok := summary["command"].(string); ok {
				fmt.Printf("%s\n", cmd)
			} else if path, ok := summary["path"].(string); ok {
				fmt.Printf("%s\n", path)
			}
		}

		fmt.Printf("Allow? [y/n]: ")
		reader := bufio.NewReader(os.Stdin)
		response, _ := reader.ReadString('\n')
		response = strings.TrimSpace(strings.ToLower(response))
		approved := response == "y" || response == "yes"
		return provider.ApprovalResponse{Approved: approved}
	}

	events := r.active.Send(r.ctx, input, approvalFn)

	for evt := range events {
		switch evt.Type {
		case "text":
			fmt.Print(evt.Text)
		case "tool_use_start":
			if evt.ToolUse != nil {
				fmt.Printf("\n%s⚙ [%s]%s ", colorGray, evt.ToolUse.Name, colorReset)
			}
		case "tool_use_end":
			// Tool completed — result will follow.
		case "done":
			fmt.Println()
			if r.showCost && evt.Usage != nil {
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

func (r *REPL) handleCommand(input string) bool {
	parts := strings.Fields(input)
	cmd := parts[0]
	args := parts[1:]

	switch cmd {
	case "/quit", "/exit", "/q":
		fmt.Println("Goodbye.")
		return true

	case "/help":
		r.printHelp()

	case "/mode":
		if len(args) == 0 {
			fmt.Printf("Current mode: %s%s%s\n", colorCyan, r.active.Mode.Name, colorReset)
			fmt.Println("Available: chat")
		} else {
			r.active.SetMode(args[0])
			fmt.Printf("Mode: %s%s%s\n", colorCyan, r.active.Mode.Name, colorReset)
		}

	case "/switch":
		if len(args) == 0 {
			sessions := r.orch.ListSessions()
			for _, s := range sessions {
				marker := " "
				if s.ProjectID == r.active.ProjectID {
					marker = "*"
				}
				fmt.Printf(" %s %s%s%s (%s)\n", marker, colorGreen, s.ProjectName, colorReset, s.ProjectPath)
			}
		} else {
			// Find session by name.
			sessions := r.orch.ListSessions()
			for _, s := range sessions {
				if strings.EqualFold(s.ProjectName, args[0]) || s.ProjectPath == args[0] {
					r.active = s
					fmt.Printf("Switched to: %s%s%s\n", colorGreen, s.ProjectName, colorReset)
					return false
				}
			}
			fmt.Printf("Project not found: %s\n", args[0])
		}

	case "/projects":
		sessions := r.orch.ListSessions()
		if len(sessions) == 0 {
			fmt.Println("No active sessions.")
		}
		for _, s := range sessions {
			marker := " "
			if s.ProjectID == r.active.ProjectID {
				marker = "*"
			}
			fmt.Printf(" %s %s%s%s %s (%s)\n", marker, colorGreen, s.ProjectName, colorReset, s.Mode.Name, s.ProjectPath)
		}

	case "/memory":
		r.handleMemoryCommand(args)

	case "/reflect":
		fmt.Println("Forcing reflection...")
		r.active.Refresh()
		fmt.Println("Reflection triggered.")

	case "/context":
		r.active.Refresh()
		fmt.Printf("Project: %s\nPath: %s\n", r.active.ProjectName, r.active.ProjectPath)
		fmt.Printf("Messages in conversation: %d\n", r.active.MessageCount())

	case "/clear":
		r.active.ClearMessages()
		fmt.Println("Conversation cleared (memories preserved).")

	case "/cost":
		summary := r.active.Cost.Summary()
		savings := r.active.Cost.Savings()
		rate := r.active.Cost.CacheHitRate()
		fmt.Printf("Session cost: %s\n", summary)
		fmt.Printf("Saved: $%.4f (%.0f%% cache hit rate)\n", savings, rate)

	default:
		fmt.Printf("Unknown command: %s (type /help)\n", cmd)
	}

	return false
}

func (r *REPL) handleMemoryCommand(args []string) {
	ctx := context.Background()

	if len(args) == 0 {
		// List all memories.
		memories, err := r.active.Store().GetTopMemories(ctx, r.active.ProjectID, 30)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if len(memories) == 0 {
			fmt.Println("No memories yet. Ghost will learn as you work.")
			return
		}
		fmt.Printf("%sMemories for %s (%d total):%s\n", colorBold, r.active.ProjectName, len(memories), colorReset)
		for _, m := range memories {
			pin := ""
			if m.Pinned {
				pin = " 📌"
			}
			fmt.Printf("  %s[%s]%s %.1f %s%s\n",
				colorCyan, m.Category, colorReset, m.Importance, m.Content, pin)
		}
		return
	}

	switch args[0] {
	case "search":
		if len(args) < 2 {
			fmt.Println("Usage: /memory search <query>")
			return
		}
		query := strings.Join(args[1:], " ")
		memories, err := r.active.Store().SearchFTS(ctx, r.active.ProjectID, query, 10)
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		if len(memories) == 0 {
			fmt.Println("No matching memories.")
			return
		}
		for _, m := range memories {
			fmt.Printf("  [%s] %.1f %s\n", m.Category, m.Importance, m.Content)
		}

	case "add":
		if len(args) < 2 {
			fmt.Println("Usage: /memory add <content>")
			return
		}
		content := strings.Join(args[1:], " ")
		_, _, err := r.active.Store().Upsert(ctx, r.active.ProjectID, "fact", content, "manual", 0.8, []string{})
		if err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		fmt.Println("Memory saved.")

	case "delete":
		if len(args) < 2 {
			fmt.Println("Usage: /memory delete <id>")
			return
		}
		if err := r.active.Store().Delete(ctx, args[1]); err != nil {
			fmt.Printf("Error: %v\n", err)
			return
		}
		fmt.Println("Memory deleted.")

	default:
		fmt.Println("Usage: /memory [search|add|delete]")
	}
}

func (r *REPL) printHelp() {
	fmt.Printf(`%sGhost Commands:%s
  /mode [name]      Switch mode (chat)
  /switch [name]    Switch active project
  /projects         List active project sessions
  /memory           List all memories
  /memory search    Search memories
  /memory add       Add a manual memory
  /memory delete    Delete a memory
  /reflect          Force memory consolidation
  /context          Show project context
  /cost             Show token usage and cost
  /clear            Clear conversation (keep memories)
  /help             Show this help
  /quit             Exit ghost
`, colorBold, colorReset)
}

// Note: IsTerminal moved to oneshot.go to avoid duplication.
