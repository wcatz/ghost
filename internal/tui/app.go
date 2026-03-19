package tui

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/orchestrator"
	"github.com/wcatz/ghost/internal/provider"
	"github.com/wcatz/ghost/internal/voice"
)

// view modes for the TUI.
const (
	viewMain    = "main"
	viewApprove = "approve"
	viewPalette = "palette"
	viewHelp    = "help"
)

// version is set at build time.
var version = "dev"

// thinkingTickMsg fires periodically while thinking is active.
type thinkingTickMsg time.Time

// App is the root bubbletea model.
type App struct {
	// Core references.
	orch          *orchestrator.Orchestrator
	session       *orchestrator.Session
	cfg           *config.Config
	ctx           context.Context
	cancel        context.CancelFunc
	daemonWarning string // non-empty when ghost serve is unreachable at startup

	// Components.
	chatView chatViewport
	input    inputArea
	toolbar  toolbar
	status   statusBar
	approval approvalDialog
	palette  commandPalette

	// State.
	currentView  string
	width        int
	height       int
	isProcessing bool
	imgProtocol  imageProtocol
	git          gitInfo

	// Inline block accumulators.
	thinkingAccum string                        // accumulated thinking text for current turn
	completedTools map[string]completedToolInfo  // tool_use_end info keyed by tool ID, awaiting tool_result

	// Thinking timer state.
	thinkingStart  time.Time // when thinking phase began
	thinkingTokens int       // estimated token count for thinking phase
	thinkingActive bool      // true while thinking events are streaming

	// Voice: set via SetVoice(). Nil if voice disabled.
	voiceFn     func(ctx context.Context) (transcript, response string, err error)
	voiceActive bool

	// Channels for async communication.
	activeStream <-chan ai.StreamEvent // current event channel from Session.SendAsync
	approvalCh   chan provider.ApprovalRequest
}

// NewApp creates a new bubbletea application model.
func NewApp(
	orch *orchestrator.Orchestrator,
	session *orchestrator.Session,
	cfg *config.Config,
	daemonWarning string,
) App {
	ctx, cancel := context.WithCancel(context.Background())

	imgProto := parseImageProtocol(cfg.Display.ImageProtocol)

	return App{
		orch:          orch,
		session:       session,
		cfg:           cfg,
		ctx:           ctx,
		cancel:        cancel,
		daemonWarning: daemonWarning,
		chatView:      newChatViewport(80, 20),
		input:         newInputArea(),
		toolbar:       newToolbar(),
		status:        newStatusBar(&session.Cost, session.EstimateTokens, func() int { return ai.ContextForModel(session.Model()) }),
		approval:      newApprovalDialog(),
		palette:       newCommandPalette(),
		currentView:   viewMain,
		imgProtocol:   imgProto,
		approvalCh:    make(chan provider.ApprovalRequest, 4),
	}
}

// Init returns initial commands.
func (a App) Init() tea.Cmd {
	cmds := []tea.Cmd{
		a.input.textarea.Focus(),
		a.listenForApprovals(),
		fetchGitInfo(a.session.ProjectPath),
	}
	if a.daemonWarning != "" {
		cmds = append(cmds, func() tea.Msg {
			return daemonWarningMsg(a.daemonWarning)
		})
	}
	return tea.Batch(cmds...)
}

// daemonWarningMsg is a startup message injected when ghost serve is unreachable.
type daemonWarningMsg string

// Update processes messages.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case gitInfoMsg:
		a.git = gitInfo(msg)
		return a, nil

	case daemonWarningMsg:
		a.chatView.addMessage(chatMessage{kind: msgWarning, raw: string(msg)})
		return a, nil

	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.resize()
		return a, nil

	case streamEventMsg:
		return a.handleStreamEvent(msg)

	case streamDoneMsg:
		a.flushThinking()
		a.isProcessing = false
		a.status.isProcessing = false
		a.toolbar.clear()
		a.completedTools = nil
		a.thinkingActive = false
		return a, nil

	case thinkingTickMsg:
		// Only continue ticking if still actively thinking.
		if a.thinkingActive {
			cmds = append(cmds, thinkingTickCmd())
		}
		return a, tea.Batch(cmds...)

	case approvalRequestMsg:
		a.currentView = viewApprove
		a.approval.show(provider.ApprovalRequest(msg))
		a.input.blur()
		// Continue listening for more approvals.
		cmds = append(cmds, a.listenForApprovals())
		return a, tea.Batch(cmds...)

	case imagePasteMsg:
		// Image paste: show preview and send to Claude.
		a.chatView.addMessage(chatMessage{kind: msgUser, raw: "[pasted image]"})
		a.isProcessing = true
		a.status.startProcessing()
		a.chatView.startNewAssistantMessage()
		a.activeStream = a.session.SendImageAsync(
			a.ctx, "Describe and analyze this image.", msg.mediaType, msg.data, a.approvalCh,
		)
		return a, waitForStreamEvent(a.activeStream)

	case voiceResultMsg:
		a.voiceActive = false
		if msg.err != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: fmt.Sprintf("Voice error: %v", msg.err)})
		} else if msg.transcript != "" {
			a.chatView.addMessage(chatMessage{kind: msgUser, raw: fmt.Sprintf("[voice] %s", msg.transcript)})
			if msg.response != "" {
				a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: msg.response})
			}
		}
		return a, nil

	case commandMsg:
		return a.handleCommand(msg)

	case tea.KeyPressMsg:
		return a.handleKey(msg)

	case tea.MouseWheelMsg:
		// Route mouse wheel to chat viewport for scrolling.
		if a.currentView == viewMain {
			var cmd tea.Cmd
			a.chatView, cmd = a.chatView.update(msg)
			cmds = append(cmds, cmd)
			return a, tea.Batch(cmds...)
		}

	case tea.MouseClickMsg:
		// Route clicks to viewport for expand/collapse on tool/thinking blocks.
		if a.currentView == viewMain {
			// The Y coordinate is relative to the terminal. Subtract header (2 lines)
			// to get viewport-relative line.
			viewportLine := msg.Y - 2
			if viewportLine >= 0 && viewportLine < a.chatView.height {
				a.chatView.toggleBlockAtLine(viewportLine)
			}
			return a, nil
		}
	}

	// Pass to sub-components.
	switch a.currentView {
	case viewApprove:
		var cmd tea.Cmd
		a.approval, cmd = a.approval.update(msg)
		if !a.approval.active {
			a.currentView = viewMain
			a.input.focus()
		}
		cmds = append(cmds, cmd)
	case viewPalette:
		var cmd tea.Cmd
		a.palette, cmd = a.palette.update(msg)
		if !a.palette.active {
			a.currentView = viewMain
			a.input.focus()
		}
		cmds = append(cmds, cmd)
	default:
		// Update toolbar spinner.
		if a.toolbar.hasActive() {
			var cmd tea.Cmd
			a.toolbar, cmd = a.toolbar.update(msg)
			cmds = append(cmds, cmd)
		}
	}

	return a, tea.Batch(cmds...)
}

// View renders the entire TUI.
// renderHeader builds the top status line:
// mode · model · git:(branch*) ↑N ↓M · 👻
func (a App) renderHeader() string {
	divider := headerDividerStyle.Render(" · ")

	mode := headerModeStyle.Render(a.session.Mode.Name)
	model := headerModelStyle.Render(shortModelName(a.session.Model()))

	var parts []string
	parts = append(parts, mode)
	parts = append(parts, model)

	if a.git.err == nil && a.git.branch != "" {
		branch := a.git.branch
		if a.git.dirty {
			branch += "*"
		}
		gitStr := headerGitStyle.Render("git:(") +
			headerGitBranchStyle.Render(branch) +
			headerGitStyle.Render(")")
		if a.git.ahead > 0 || a.git.behind > 0 {
			sync := ""
			if a.git.ahead > 0 {
				sync += fmt.Sprintf("↑%d", a.git.ahead)
			}
			if a.git.behind > 0 {
				if sync != "" {
					sync += " "
				}
				sync += fmt.Sprintf("↓%d", a.git.behind)
			}
			gitStr += " " + headerGitStyle.Render(sync)
		}
		parts = append(parts, gitStr)
	}

	if a.session.AutoApprove() {
		parts = append(parts, headerGhostYoloStyle.Render("YOLO"))
	}

	result := ""
	for i, p := range parts {
		if i > 0 {
			result += divider
		}
		result += p
	}
	
	// Add a subtle separator line below the header
	headerLine := headerStyle.Render(" ") + result
	separator := lipgloss.NewStyle().
		Foreground(colorSubtle).
		Render(strings.Repeat("─", a.width))
	
	return headerLine + "\n" + separator
}

func (a App) View() tea.View {
	var content string

	if a.width == 0 {
		content = "Loading..."
	} else {
		// Header.
		header := a.renderHeader()

		// Main viewport.
		viewport := a.chatView.view()

		// Active tool indicator (1 line when running, 0 when idle).
		toolView := a.toolbar.view()

		// Input area.
		inputView := a.input.view()

		// Status bar.
		statusView := a.status.view()

		// Assemble main layout.
		var sections []string
		sections = append(sections, header)
		sections = append(sections, viewport)
		if toolView != "" {
			sections = append(sections, toolView)
		}
		sections = append(sections, inputView)
		sections = append(sections, statusView)

		content = lipgloss.JoinVertical(lipgloss.Left, sections...)

		// Overlays.
		switch a.currentView {
		case viewApprove:
			overlay := a.approval.view()
			if overlay != "" {
				content = lipgloss.Place(a.width, a.height,
					lipgloss.Center, lipgloss.Center,
					overlay,
				)
			}
		case viewPalette:
			paletteView := a.palette.view()
			if paletteView != "" {
				content = lipgloss.Place(a.width, a.height,
					lipgloss.Center, lipgloss.Top,
					paletteView,
				)
			}
		case viewHelp:
			helpView := renderHelp(a.width)
			content = lipgloss.Place(a.width, a.height,
				lipgloss.Center, lipgloss.Center,
				helpView,
			)
		}
	}

	v := tea.NewView(content)
	v.AltScreen = true
	v.MouseMode = tea.MouseModeCellMotion
	return v
}

// flushThinking writes accumulated thinking text as an inline block in the viewport.
func (a *App) flushThinking() {
	if a.thinkingAccum != "" {
		a.chatView.addThinkingBlock(a.thinkingAccum)
		a.thinkingAccum = ""
	}
	a.toolbar.setThinking(false)
	a.thinkingActive = false
}

// thinkingTickCmd returns a tea.Cmd that fires after 100ms.
func thinkingTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(t time.Time) tea.Msg {
		return thinkingTickMsg(t)
	})
}

// handleStreamEvent processes a streaming event from the session.
func (a App) handleStreamEvent(msg streamEventMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg.Type {
	case "thinking":
		if !a.thinkingActive {
			a.thinkingActive = true
			a.thinkingStart = time.Now()
			a.thinkingTokens = 0
			cmds = append(cmds, thinkingTickCmd())
		}
		a.toolbar.setThinking(true)
		a.thinkingAccum += msg.Text
		// Approximate token count: ~4 chars per token.
		delta := len(msg.Text) / 4
		a.thinkingTokens += delta
		a.toolbar.addThinkingTokens(delta)

	case "text":
		// Thinking phase ended — flush it inline before appending text.
		a.flushThinking()
		a.chatView.appendToLastAssistant(msg.Text)

	case "tool_use_start":
		// Thinking phase ended — flush it inline before tool starts.
		a.flushThinking()
		if msg.ToolUse != nil {
			a.toolbar.startTool(msg.ToolUse.ID, msg.ToolUse.Name)
		}

	case "tool_input_delta":
		if msg.ToolUse != nil {
			a.toolbar.updateInput(msg.ToolUse.ID, msg.ToolUse.InputDelta)
		}

	case "tool_use_end":
		if msg.ToolUse != nil {
			name, dur, ok := a.toolbar.completeTool(msg.ToolUse.ID)
			if ok {
				// Stash completed tool info; it will be rendered inline when tool_result arrives.
				if a.completedTools == nil {
					a.completedTools = make(map[string]completedToolInfo)
				}
				a.completedTools[msg.ToolUse.ID] = completedToolInfo{name: name, duration: dur}
			}
		}

	case "tool_result":
		if msg.ToolUse != nil {
			isError := msg.Metadata != nil && msg.Metadata["is_error"] == "true"
			info, ok := a.completedTools[msg.ToolUse.ID]
			if ok {
				// Render the completed tool inline in the chat viewport.
				a.chatView.addToolBlock(msg.ToolUse.ID, info.name, info.duration, msg.Text, isError, false)
				delete(a.completedTools, msg.ToolUse.ID)
			}
		}

	case "done":
		a.flushThinking()
		a.isProcessing = false
		a.status.stopProcessing()
		// Clear any stale completed tool info.
		a.completedTools = nil

		// Check for truncation (max_tokens hit).
		if msg.StopReason == "max_tokens" {
			a.chatView.addMessage(chatMessage{
				kind: msgWarning,
				raw:  "Response truncated -- hit token limit. Use /continue to get more.",
			})
		}

	case "error":
		a.flushThinking()
		if msg.Error != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: msg.Error.Error()})
		}
		a.isProcessing = false
		a.status.stopProcessing()
		a.completedTools = nil
	}

	// Keep listening for more events (the channel is still open).
	if a.isProcessing && msg.Type != "done" {
		cmds = append(cmds, waitForStreamEvent(a.activeStream))
	}

	return a, tea.Batch(cmds...)
}

// handleKey processes keyboard input.
func (a App) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C: interrupt if processing, quit if idle.
	if key.Matches(msg, keys.Quit) {
		if a.isProcessing {
			return a.interruptStream()
		}
		a.cancel()
		return a, tea.Quit
	}
	// Ctrl+D always force-quits.
	if key.Matches(msg, keys.ForceQuit) {
		a.cancel()
		return a, tea.Quit
	}

	// Overlay-specific.
	switch a.currentView {
	case viewApprove:
		var cmd tea.Cmd
		a.approval, cmd = a.approval.update(msg)
		if !a.approval.active {
			a.currentView = viewMain
			a.input.focus()
		}
		return a, cmd
	case viewPalette:
		var cmd tea.Cmd
		a.palette, cmd = a.palette.update(msg)
		if !a.palette.active {
			a.currentView = viewMain
			a.input.focus()
		}
		return a, cmd
	case viewHelp:
		// Any key closes help.
		a.currentView = viewMain
		a.input.focus()
		return a, nil
	}

	// Main view keys.
	switch {
	case key.Matches(msg, keys.Interrupt):
		if a.isProcessing {
			return a.interruptStream()
		}

	case key.Matches(msg, keys.Palette):
		a.currentView = viewPalette
		a.palette.open()
		a.input.blur()
		return a, nil

	case key.Matches(msg, keys.NewLine):
		// Insert a literal newline into the textarea.
		a.input.textarea.InsertString("\n")
		return a, nil

	case key.Matches(msg, keys.Send):
		if a.isProcessing {
			return a, nil
		}
		text := a.input.submit()
		if text == "" {
			return a, nil
		}
		// Check for slash commands.
		if strings.HasPrefix(text, "/") {
			parts := strings.Fields(text)
			return a.handleCommand(commandMsg{Command: parts[0], Args: parts[1:]})
		}
		return a.sendMessage(text)

	case key.Matches(msg, keys.HistoryUp):
		if a.input.value() == "" {
			a.input.historyUp()
			return a, nil
		}
	case key.Matches(msg, keys.HistoryDown):
		a.input.historyDown()
		return a, nil

	case key.Matches(msg, keys.PageUp), key.Matches(msg, keys.PageDown),
		key.Matches(msg, keys.Home), key.Matches(msg, keys.End):
		var cmd tea.Cmd
		a.chatView, cmd = a.chatView.update(msg)
		return a, cmd

	case key.Matches(msg, keys.ToolNext), key.Matches(msg, keys.ToolPrev):
		// Scroll viewport (tool blocks are inline now).
		var cmd tea.Cmd
		a.chatView, cmd = a.chatView.update(msg)
		return a, cmd
	case key.Matches(msg, keys.ToolToggle):
		if a.input.value() == "" {
			// Toggle the last tool/thinking block in the viewport.
			a.toggleLastBlock()
			return a, nil
		}

	case key.Matches(msg, keys.CopyBlock):
		if a.input.value() == "" {
			code := extractLastCodeBlock(a.chatView.messages)
			if code != "" {
				copyOSC52(code)
				a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "*Copied code block to clipboard*"})
			} else {
				a.chatView.addMessage(chatMessage{kind: msgWarning, raw: "No code block found to copy."})
			}
			return a, nil
		}

	case key.Matches(msg, keys.PushToTalk):
		if a.voiceFn != nil && !a.voiceActive && !a.isProcessing {
			a.voiceActive = true
			a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "[voice] Listening..."})
			return a, func() tea.Msg {
				transcript, response, err := a.voiceFn(a.ctx)
				return voiceResultMsg{transcript: transcript, response: response, err: err}
			}
		}
	}

	// ? opens help when input is empty.
	if msg.String() == "?" && a.input.value() == "" {
		a.currentView = viewHelp
		a.input.blur()
		return a, nil
	}

	// Default: pass to input area.
	var cmd tea.Cmd
	a.input, cmd = a.input.update(msg)
	return a, cmd
}

// sendMessage sends user text to the session and starts consuming events.
func (a App) sendMessage(text string) (tea.Model, tea.Cmd) {
	a.isProcessing = true
	a.status.startProcessing()

	// Add user message to viewport.
	a.chatView.addMessage(chatMessage{kind: msgUser, raw: text})
	a.chatView.startNewAssistantMessage()

	// Send to session with async approval.
	a.activeStream = a.session.SendAsync(a.ctx, text, a.approvalCh)

	return a, waitForStreamEvent(a.activeStream)
}

// handleCommand executes a slash command.
func (a App) handleCommand(msg commandMsg) (tea.Model, tea.Cmd) {
	switch msg.Command {
	case "/quit", "/exit", "/q":
		a.cancel()
		return a, tea.Quit

	case "/mode":
		if len(msg.Args) > 0 {
			a.session.SetMode(msg.Args[0])
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  fmt.Sprintf("Mode switched to **%s**", a.session.Mode.Name),
			})
		}

	case "/model":
		if len(msg.Args) > 0 {
			name := strings.ToLower(msg.Args[0])
			var modelID string
			var isQuality bool
			switch {
			case strings.Contains(name, "haiku"):
				modelID = ai.ModelHaiku45
				isQuality = false
			case strings.Contains(name, "opus"):
				modelID = ai.ModelOpus46
				isQuality = true
			default:
				modelID = ai.ModelSonnet46
				isQuality = false
			}
			if isQuality {
				a.session.SetQualityModel(modelID)
			} else {
				a.session.SetFastModel(modelID)
			}
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  fmt.Sprintf("Model switched to **%s**", shortModelName(modelID)),
			})
		} else {
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  fmt.Sprintf("Current model: **%s** (mode: %s)\nUsage: `/model sonnet` | `/model haiku` | `/model opus`", shortModelName(a.session.Model()), a.session.Mode.Name),
			})
		}

	case "/continue":
		if a.isProcessing {
			return a, nil
		}
		return a.sendMessage("Please continue from where you left off.")

	case "/compact":
		if a.isProcessing {
			return a, nil
		}
		// Trigger compression by sending a message that forces a windowedMessages call.
		a.chatView.addMessage(chatMessage{
			kind: msgAssistant,
			raw:  "Compacting conversation history...",
		})
		// A regular send will trigger windowedMessages() which handles compression.
		return a.sendMessage("Summarize what we've been working on so far in 2-3 sentences, then continue.")

	case "/tokens":
		est := a.session.EstimateTokens()
		input, output, cacheWrite, cacheRead := a.session.Cost.Totals()
		info := fmt.Sprintf("**Token Estimates**\n"+
			"- Context: ~%s tokens\n"+
			"- Input: %s | Output: %s\n"+
			"- Cache write: %s | Cache read: %s\n"+
			"- Cache hit rate: %.0f%%",
			formatTokens(est),
			formatTokens(input), formatTokens(output),
			formatTokens(cacheWrite), formatTokens(cacheRead),
			a.session.Cost.CacheHitRate(),
		)
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/export":
		msgs := a.session.Messages()
		if len(msgs) == 0 {
			a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "No messages to export."})
			return a, nil
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("# Ghost Conversation Export\n\n**Project:** %s\n**Date:** %s\n\n---\n\n",
			a.session.ProjectName, time.Now().Format("2006-01-02 15:04")))
		for _, m := range msgs {
			role := strings.ToUpper(m.Role[:1]) + m.Role[1:]
			for _, b := range m.Content {
				if b.Type == "text" && b.Text != "" {
					sb.WriteString(fmt.Sprintf("### %s\n\n%s\n\n", role, b.Text))
				} else if b.Type == "tool_use" {
					sb.WriteString(fmt.Sprintf("**Tool:** %s\n\n", b.Name))
				}
			}
		}
		// Copy to clipboard via OSC 52.
		copyOSC52(sb.String())
		a.chatView.addMessage(chatMessage{
			kind: msgAssistant,
			raw:  fmt.Sprintf("Exported %d messages to clipboard (markdown).", len(msgs)),
		})

	case "/sessions":
		sessions := a.orch.ListSessions()
		if len(sessions) == 0 {
			a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "No active sessions."})
			return a, nil
		}
		var lines []string
		for _, s := range sessions {
			marker := "  "
			if s.ProjectID == a.session.ProjectID {
				marker = "* "
			}
			lines = append(lines, fmt.Sprintf("%s**%s** %s (%d messages)",
				marker, s.ProjectName, s.Mode.Name, s.MessageCount()))
		}
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: strings.Join(lines, "\n")})

	case "/new":
		a.session.ClearMessages()
		a.chatView.clear()
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "Fresh session started. Memories are preserved."})

	case "/resume":
		err := a.session.Resume(a.ctx)
		if err != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: fmt.Sprintf("Resume failed: %v", err)})
		} else {
			a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: fmt.Sprintf("Resumed previous session (%d messages loaded).", a.session.MessageCount())})
		}

	case "/clear":
		a.session.ClearMessages()
		a.chatView.clear()

	case "/memory":
		return a.handleMemoryCommand(msg.Args)

	case "/reflect":
		a.session.Refresh()
		a.chatView.addMessage(chatMessage{
			kind: msgAssistant,
			raw:  "Reflection triggered.",
		})
		return a, fetchGitInfo(a.session.ProjectPath)

	case "/briefing":
		if a.isProcessing {
			return a, nil
		}
		return a.sendMessage("Give me a brief status update. What project am I working on? What have we discussed? Any pending tasks or decisions?")

	case "/context":
		a.session.Refresh()
		info := fmt.Sprintf("**Project:** %s\n**Path:** %s\n**Messages:** %d",
			a.session.ProjectName, a.session.ProjectPath, a.session.MessageCount())
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/cost":
		cost := a.session.Cost.Cost()
		savings := a.session.Cost.Savings()
		cacheRate := a.session.Cost.CacheHitRate()
		info := fmt.Sprintf("**Session Cost**\n- Total: $%.4f\n- Savings: $%.2f (%.0f%% cache hit rate)\n- Messages: %d exchanges",
			cost, savings, cacheRate, a.session.MessageCount())
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/switch":
		if len(msg.Args) > 0 {
			sessions := a.orch.ListSessions()
			for _, s := range sessions {
				if strings.EqualFold(s.ProjectName, msg.Args[0]) || s.ProjectPath == msg.Args[0] {
					a.session = s
					a.chatView.addMessage(chatMessage{
						kind: msgAssistant,
						raw:  fmt.Sprintf("Switched to **%s**", s.ProjectName),
					})
					return a, fetchGitInfo(a.session.ProjectPath)
				}
			}
		}

	case "/projects":
		sessions := a.orch.ListSessions()
		var lines []string
		for _, s := range sessions {
			marker := "  "
			if s.ProjectID == a.session.ProjectID {
				marker = "* "
			}
			lines = append(lines, fmt.Sprintf("%s**%s** %s (%s)", marker, s.ProjectName, s.Mode.Name, s.ProjectPath))
		}
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: strings.Join(lines, "\n")})

	case "/image":
		if len(msg.Args) > 0 {
			path := strings.Join(msg.Args, " ")
			if !isImagePath(path) {
				a.chatView.addMessage(chatMessage{kind: msgError, raw: "unsupported image format"})
				return a, nil
			}
			// Load and base64-encode the image.
			data, mediaType, err := loadImageBase64(path)
			if err != nil {
				a.chatView.addMessage(chatMessage{kind: msgError, raw: err.Error()})
				return a, nil
			}
			// Show preview in chat.
			imgStr := renderImageFile(path, a.imgProtocol)
			a.chatView.addMessage(chatMessage{kind: msgUser, raw: fmt.Sprintf("[image: %s]", filepath.Base(path))})
			if imgStr != "" {
				a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: imgStr})
			}
			// Send to Claude with vision API.
			a.isProcessing = true
			a.status.startProcessing()
			a.chatView.startNewAssistantMessage()
			a.activeStream = a.session.SendImageAsync(
				a.ctx, "Describe and analyze this image.", mediaType, data, a.approvalCh,
			)
			return a, waitForStreamEvent(a.activeStream)
		}

	case "/voice":
		if a.voiceFn != nil {
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  "**Voice mode** is available. Press `ctrl+space` to start push-to-talk.",
			})
		} else {
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  "Voice mode is not configured. Set up whisper.cpp or a voice provider first.",
			})
		}

	case "/health":
		ctx := context.Background()
		memCount, err := a.session.Store().CountMemories(ctx, a.session.ProjectID)
		countStr := "error"
		if err == nil {
			countStr = fmt.Sprintf("%d", memCount)
		}
		unembedded, err := a.session.Store().UnembeddedMemoryIDs(ctx, a.session.ProjectID, 1000)
		embedStatus := "unknown"
		if err == nil {
			if len(unembedded) == 0 {
				embedStatus = "all embedded"
			} else {
				embedStatus = fmt.Sprintf("%d pending", len(unembedded))
			}
		}
		cost := a.session.Cost.Cost()
		info := fmt.Sprintf("**Health Check**\n"+
			"- Memories: %s\n"+
			"- Embeddings: %s\n"+
			"- Session cost: $%.4f\n"+
			"- Model: %s\n"+
			"- Messages: %d",
			countStr, embedStatus, cost,
			shortModelName(a.session.Model()),
			a.session.MessageCount(),
		)
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/history":
		msgs := a.session.Messages()
		userCount := 0
		assistantCount := 0
		toolCount := 0
		for _, m := range msgs {
			switch m.Role {
			case "user":
				for _, b := range m.Content {
					if b.Type == "tool_result" {
						toolCount++
					} else {
						userCount++
					}
				}
			case "assistant":
				assistantCount++
			}
		}
		est := a.session.EstimateTokens()
		info := fmt.Sprintf("**Conversation Stats**\n"+
			"- User messages: %d\n"+
			"- Assistant messages: %d\n"+
			"- Tool calls: %d\n"+
			"- Total messages: %d\n"+
			"- Estimated tokens: %s",
			userCount, assistantCount, toolCount, len(msgs), formatTokens(est))
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/theme":
		if len(msg.Args) > 0 {
			name := msg.Args[0]
			a.chatView.renderer.setTheme(name)
			a.chatView.rerenderAll()
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  fmt.Sprintf("Theme switched to **%s**", name),
			})
		} else {
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  "Usage: `/theme <name>`\nAvailable: `ghost-blue`, `dark`, `light`, `notty`, `auto`",
			})
		}

	case "/remind":
		if len(msg.Args) < 2 {
			a.chatView.addMessage(chatMessage{kind: msgWarning, raw: "Usage: /remind <time> <message>"})
		} else {
			a.chatView.addMessage(chatMessage{
				kind: msgWarning,
				raw:  "Reminders require a scheduler (not yet configured).",
			})
		}

	case "/reminders":
		a.chatView.addMessage(chatMessage{
			kind: msgAssistant,
			raw:  "No scheduler configured. Reminders are not available yet.",
		})

	case "auto-approve":
		a.session.SetAutoApprove(true)
		a.chatView.addMessage(chatMessage{
			kind: msgAssistant,
			raw:  "Auto-approve enabled for this session.",
		})
	}

	return a, nil
}

// handleMemoryCommand handles /memory subcommands.
func (a App) handleMemoryCommand(args []string) (tea.Model, tea.Cmd) {
	ctx := context.Background()

	if len(args) == 0 {
		memories, err := a.session.Store().GetTopMemories(ctx, a.session.ProjectID, 30)
		if err != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: err.Error()})
			return a, nil
		}
		if len(memories) == 0 {
			a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "No memories yet."})
			return a, nil
		}
		var lines []string
		for _, m := range memories {
			id := m.ID
			if len(id) > 8 {
				id = id[:8]
			}
			lines = append(lines, fmt.Sprintf("- `%s` **[%s]** %.1f %s", id, m.Category, m.Importance, m.Content))
		}
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: strings.Join(lines, "\n")})
		return a, nil
	}

	switch args[0] {
	case "search":
		if len(args) < 2 {
			return a, nil
		}
		query := strings.Join(args[1:], " ")
		memories, err := a.session.Store().SearchFTS(ctx, a.session.ProjectID, query, 10)
		if err != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: err.Error()})
			return a, nil
		}
		if len(memories) == 0 {
			a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "No matching memories."})
			return a, nil
		}
		var lines []string
		for _, m := range memories {
			lines = append(lines, fmt.Sprintf("- **[%s]** %.1f %s", m.Category, m.Importance, m.Content))
		}
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: strings.Join(lines, "\n")})

	case "add":
		if len(args) < 2 {
			return a, nil
		}
		content := strings.Join(args[1:], " ")
		_, _, err := a.session.Store().Upsert(ctx, a.session.ProjectID, "fact", content, "manual", 0.8, []string{})
		if err != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: err.Error()})
			return a, nil
		}
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "Memory saved."})

	case "delete":
		if len(args) < 2 {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: "Usage: /memory delete <id>"})
			return a, nil
		}
		if err := a.session.Store().Delete(ctx, args[1]); err != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: err.Error()})
			return a, nil
		}
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: "Memory deleted."})
	}

	return a, nil
}

// SetVoice configures the voice push-to-talk function.
// The function should execute one full PTT cycle (record -> transcribe -> respond -> speak)
// and return the transcript and response text.
func (a *App) SetVoice(fn func(ctx context.Context) (transcript, response string, err error)) {
	a.voiceFn = fn
}

// toggleLastBlock finds the last tool or thinking block in the viewport and toggles it.
func (a *App) toggleLastBlock() {
	for i := len(a.chatView.messages) - 1; i >= 0; i-- {
		msg := a.chatView.messages[i]
		if msg.kind == msgToolBlock || msg.kind == msgThinkingBlock {
			a.chatView.toggleBlock(i)
			return
		}
	}
}

// interruptStream cancels the current processing and resets state.
func (a App) interruptStream() (tea.Model, tea.Cmd) {
	a.cancel()
	// Create a new context for future requests.
	a.ctx, a.cancel = context.WithCancel(context.Background())
	a.flushThinking()
	a.isProcessing = false
	a.status.stopProcessing()
	a.toolbar.clear()
	a.completedTools = nil
	a.activeStream = nil
	a.chatView.addMessage(chatMessage{kind: msgWarning, raw: "Interrupted."})
	return a, nil
}

// resize adjusts all component sizes.
func (a *App) resize() {
	headerH := 2 // Header now has 2 lines (header + separator)
	statusH := 1
	inputH := 4
	toolH := a.toolbar.height() // 0 when idle, 1 when active

	viewportH := a.height - headerH - statusH - inputH - toolH - 2
	if viewportH < 5 {
		viewportH = 5
	}

	a.chatView.setSize(a.width, viewportH)
	a.input.setSize(a.width)
	a.status.setSize(a.width)
	a.approval.setWidth(a.width)
	a.palette.setWidth(a.width)
}

// listenForApprovals returns a tea.Cmd that waits for approval requests.
func (a App) listenForApprovals() tea.Cmd {
	return waitForApproval(a.approvalCh)
}

// RunApp starts the bubbletea TUI.
func RunApp(orch *orchestrator.Orchestrator, cfg *config.Config, session *orchestrator.Session) error {
	app := NewApp(orch, session, cfg, "")

	// Wire voice pipeline if enabled.
	if cfg.Voice.Enabled {
		opts := voice.Options{
			STTBackend:        cfg.Voice.STTBackend,
			STTModel:          cfg.Voice.STTModel,
			TTSBackend:        cfg.Voice.TTSBackend,
			TTSModel:          cfg.Voice.TTSModel,
			TTSVoice:          cfg.Voice.TTSVoice,
			TTSRate:           cfg.Voice.TTSRate,
			SilenceMs:         cfg.Voice.SilenceMs,
			SampleRate:        cfg.Voice.SampleRate,
			InputDevice:       cfg.Voice.InputDevice,
			AssemblyAIAPIKey:  cfg.Voice.AssemblyAIAPIKey,
			ElevenLabsAPIKey:  cfg.Voice.ElevenLabsAPIKey,
			ElevenLabsVoiceID: cfg.Voice.ElevenLabsVoiceID,
			Logger:            slog.Default(),
		}
		respond := func(ctx context.Context, text string) (string, error) {
			ch := session.SendAsync(ctx, text, nil)
			var sb strings.Builder
			for ev := range ch {
				if ev.Type == "text" {
					sb.WriteString(ev.Text)
				}
			}
			return sb.String(), nil
		}
		pipeline, err := voice.New(opts, respond)
		if err != nil {
			slog.Default().Warn("voice pipeline unavailable", "error", err)
		} else {
			defer func() { _ = pipeline.Close() }()
			app.SetVoice(pipeline.HandlePushToTalk)
			slog.Default().Info("voice pipeline enabled", "stt", cfg.Voice.STTBackend)
		}
	}

	p := tea.NewProgram(app)

	_, err := p.Run()
	return err
}
