package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wcatz/ghost/internal/ai"
	"github.com/wcatz/ghost/internal/config"
	"github.com/wcatz/ghost/internal/orchestrator"
	"github.com/wcatz/ghost/internal/provider"
)

// view modes for the TUI.
const (
	viewMain    = "main"
	viewApprove = "approve"
	viewPalette = "palette"
)

// version is set at build time.
var version = "dev"

// App is the root bubbletea model.
type App struct {
	// Core references.
	orch    *orchestrator.Orchestrator
	session *orchestrator.Session
	cfg     *config.Config
	ctx     context.Context
	cancel  context.CancelFunc

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

	// Channels for async communication.
	activeStream <-chan ai.StreamEvent // current event channel from Session.SendAsync
	approvalCh   chan provider.ApprovalRequest
}

// NewApp creates a new bubbletea application model.
func NewApp(
	orch *orchestrator.Orchestrator,
	session *orchestrator.Session,
	cfg *config.Config,
) App {
	ctx, cancel := context.WithCancel(context.Background())

	imgProto := parseImageProtocol(cfg.Display.ImageProtocol)

	return App{
		orch:        orch,
		session:     session,
		cfg:         cfg,
		ctx:         ctx,
		cancel:      cancel,
		chatView:    newChatViewport(80, 20),
		input:       newInputArea(),
		toolbar:     newToolbar(),
		status:      newStatusBar(session.ProjectName, session.Mode.Name),
		approval:    newApprovalDialog(),
		palette:     newCommandPalette(),
		currentView: viewMain,
		imgProtocol: imgProto,
		approvalCh:  make(chan provider.ApprovalRequest, 4),
	}
}

// Init returns initial commands.
func (a App) Init() tea.Cmd {
	return tea.Batch(
		a.input.textarea.Focus(),
		a.listenForApprovals(),
	)
}

// Update processes messages.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.resize()
		return a, nil

	case streamEventMsg:
		return a.handleStreamEvent(msg)

	case streamDoneMsg:
		a.isProcessing = false
		a.status.isProcessing = false
		a.toolbar.clear()
		return a, nil

	case approvalRequestMsg:
		a.currentView = viewApprove
		a.approval.show(provider.ApprovalRequest(msg))
		a.input.blur()
		// Continue listening for more approvals.
		cmds = append(cmds, a.listenForApprovals())
		return a, tea.Batch(cmds...)

	case commandMsg:
		return a.handleCommand(msg)

	case tea.KeyMsg:
		return a.handleKey(msg)
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
func (a App) View() string {
	if a.width == 0 {
		return "Loading..."
	}

	// Header.
	header := headerStyle.Render(
		fmt.Sprintf("ghost %s | %s (%s)",
			version, a.session.ProjectName, a.session.Mode.Name),
	)

	// Main viewport.
	viewport := a.chatView.view()

	// Tool panel (only if tools active or recently completed).
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

	main := lipgloss.JoinVertical(lipgloss.Left, sections...)

	// Overlays.
	switch a.currentView {
	case viewApprove:
		overlay := a.approval.view()
		if overlay != "" {
			// Place overlay in the center of the screen.
			return lipgloss.Place(a.width, a.height,
				lipgloss.Center, lipgloss.Center,
				overlay,
				lipgloss.WithWhitespaceBackground(lipgloss.NoColor{}),
			)
		}
	case viewPalette:
		paletteView := a.palette.view()
		if paletteView != "" {
			return lipgloss.Place(a.width, a.height,
				lipgloss.Center, lipgloss.Top,
				paletteView,
				lipgloss.WithWhitespaceBackground(lipgloss.NoColor{}),
			)
		}
	}

	return main
}

// handleStreamEvent processes a streaming event from the session.
func (a App) handleStreamEvent(msg streamEventMsg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg.Type {
	case "text":
		a.chatView.appendToLastAssistant(msg.Text)
	case "tool_use_start":
		if msg.ToolUse != nil {
			a.toolbar.addTool(msg.ToolUse.ID, msg.ToolUse.Name)
		}
	case "tool_input_delta":
		if msg.ToolUse != nil {
			a.toolbar.updateInput(msg.ToolUse.ID, msg.ToolUse.InputDelta)
		}
	case "tool_use_end":
		if msg.ToolUse != nil {
			a.toolbar.completeTool(msg.ToolUse.ID)
		}
	case "done":
		a.isProcessing = false
		a.status.isProcessing = false
		a.status.updateUsage(msg.Usage)
	case "error":
		if msg.Error != nil {
			a.chatView.addMessage(chatMessage{kind: msgError, raw: msg.Error.Error()})
		}
		a.isProcessing = false
		a.status.isProcessing = false
	}

	// Keep listening for more events (the channel is still open).
	if a.isProcessing && msg.Type != "done" {
		cmds = append(cmds, waitForStreamEvent(a.activeStream))
	}

	return a, tea.Batch(cmds...)
}

// handleKey processes keyboard input.
func (a App) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Global keys.
	switch {
	case key.Matches(msg, keys.Quit):
		a.cancel()
		return a, tea.Quit
	case key.Matches(msg, keys.ForceQuit):
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
	}

	// Main view keys.
	switch {
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
	}

	// Default: pass to input area.
	var cmd tea.Cmd
	a.input, cmd = a.input.update(msg)
	return a, cmd
}

// sendMessage sends user text to the session and starts consuming events.
func (a App) sendMessage(text string) (tea.Model, tea.Cmd) {
	a.isProcessing = true
	a.status.isProcessing = true
	a.status.modeName = a.session.Mode.Name

	// Add user message to viewport.
	a.chatView.addMessage(chatMessage{kind: msgUser, raw: text})
	a.chatView.startNewAssistantMessage()

	// Send to session with async approval.
	events := a.session.SendAsync(a.ctx, text, a.approvalCh)

	return a, waitForStreamEvent(events)
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
			a.status.modeName = a.session.Mode.Name
			a.chatView.addMessage(chatMessage{
				kind: msgAssistant,
				raw:  fmt.Sprintf("Mode switched to **%s**", a.session.Mode.Name),
			})
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

	case "/context":
		a.session.Refresh()
		info := fmt.Sprintf("**Project:** %s\n**Path:** %s\n**Messages:** %d",
			a.session.ProjectName, a.session.ProjectPath, a.session.MessageCount())
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/cost":
		info := fmt.Sprintf("**Session Cost**\n- Input: %s tokens\n- Output: %s tokens\n- Cache reads: %s tokens\n- Estimated: $%.4f",
			formatTokens(a.status.totalInput),
			formatTokens(a.status.totalOutput),
			formatTokens(a.status.totalCacheRead),
			a.status.totalCostUSD,
		)
		a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: info})

	case "/switch":
		if len(msg.Args) > 0 {
			sessions := a.orch.ListSessions()
			for _, s := range sessions {
				if strings.EqualFold(s.ProjectName, msg.Args[0]) || s.ProjectPath == msg.Args[0] {
					a.session = s
					a.status.projectName = s.ProjectName
					a.status.modeName = s.Mode.Name
					a.chatView.addMessage(chatMessage{
						kind: msgAssistant,
						raw:  fmt.Sprintf("Switched to **%s**", s.ProjectName),
					})
					break
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
			if isImagePath(path) {
				imgStr := renderImageFile(path, a.imgProtocol)
				a.chatView.addMessage(chatMessage{kind: msgAssistant, raw: imgStr})
				// TODO: send image to Claude via SendMultimodal
			}
		}

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
			lines = append(lines, fmt.Sprintf("- **[%s]** %.1f %s", m.Category, m.Importance, m.Content))
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
	}

	return a, nil
}

// resize adjusts all component sizes.
func (a *App) resize() {
	headerH := 1
	statusH := 1
	inputH := 4
	toolH := 0
	if a.toolbar.hasActive() {
		toolH = 3
	}

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
	app := NewApp(orch, session, cfg)

	p := tea.NewProgram(
		app,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	_, err := p.Run()
	return err
}
