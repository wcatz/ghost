import * as vscode from "vscode";
import {
  GhostClient,
  Session,
  ApprovalRequest,
} from "./ghost-client";

export class ChatEditorPanel {
  public static readonly viewType = "ghost.chatEditor";
  public static currentPanel?: ChatEditorPanel;

  private readonly panel: vscode.WebviewPanel;
  private client: GhostClient;
  private session?: Session;
  private abortFn?: () => void;
  private disposables: vscode.Disposable[] = [];

  public static createOrShow(
    extensionUri: vscode.Uri,
    client: GhostClient
  ): ChatEditorPanel {
    const column = vscode.window.activeTextEditor
      ? vscode.window.activeTextEditor.viewColumn
      : undefined;

    if (ChatEditorPanel.currentPanel) {
      ChatEditorPanel.currentPanel.panel.reveal(column);
      return ChatEditorPanel.currentPanel;
    }

    const panel = vscode.window.createWebviewPanel(
      ChatEditorPanel.viewType,
      "Ghost Chat",
      column || vscode.ViewColumn.One,
      {
        enableScripts: true,
        retainContextWhenHidden: true,
        localResourceRoots: [vscode.Uri.joinPath(extensionUri, "media")],
      }
    );

    ChatEditorPanel.currentPanel = new ChatEditorPanel(
      panel,
      extensionUri,
      client
    );
    return ChatEditorPanel.currentPanel;
  }

  private constructor(
    panel: vscode.WebviewPanel,
    private readonly extensionUri: vscode.Uri,
    client: GhostClient
  ) {
    this.panel = panel;
    this.client = client;

    this.panel.iconPath = vscode.Uri.joinPath(
      extensionUri,
      "media",
      "ghost-icon.svg"
    );

    this.panel.webview.html = this.getHtml(this.panel.webview);

    this.panel.onDidDispose(() => this.dispose(), null, this.disposables);

    this.panel.webview.onDidReceiveMessage(
      async (msg) => {
        switch (msg.type) {
          case "send":
            await this.handleSend(msg.text);
            break;
          case "approve":
            await this.handleApprove(msg.approved);
            break;
          case "abort":
            this.handleAbort();
            break;
          case "setMode":
            await this.handleSetMode(msg.mode);
            break;
          case "ready":
            await this.handleReady();
            break;
        }
      },
      null,
      this.disposables
    );
  }

  public setClient(client: GhostClient): void {
    this.client = client;
  }

  public dispose(): void {
    ChatEditorPanel.currentPanel = undefined;
    this.panel.dispose();
    while (this.disposables.length) {
      const d = this.disposables.pop();
      if (d) {
        d.dispose();
      }
    }
  }

  private async handleReady(): Promise<void> {
    const available = await this.client.isAvailable();
    this.postMessage({ type: "status", connected: available });
    if (available) {
      await this.ensureSession();
    }
  }

  private async ensureSession(): Promise<void> {
    if (this.session) {
      return;
    }
    const folders = vscode.workspace.workspaceFolders;
    if (!folders || folders.length === 0) {
      this.postMessage({
        type: "error",
        text: "No workspace folder open. Open a project first.",
      });
      return;
    }
    try {
      this.session = await this.client.createSession(folders[0].uri.fsPath);
      this.postMessage({ type: "session", session: this.session });
    } catch (err) {
      this.postMessage({
        type: "error",
        text: `Failed to create session: ${err}`,
      });
    }
  }

  private async handleSend(text: string): Promise<void> {
    if (!text.trim()) {
      return;
    }
    await this.ensureSession();
    if (!this.session) {
      return;
    }

    this.postMessage({ type: "user_message", text });
    this.postMessage({ type: "streaming", active: true });

    const { events, abort } = this.client.sendMessage(this.session.id, text);
    this.abortFn = abort;

    events.on("text", (t: string) => {
      this.postMessage({ type: "text_delta", text: t });
    });
    events.on("thinking", (t: string) => {
      this.postMessage({ type: "thinking_delta", text: t });
    });
    events.on("tool_start", (data: { id: string; name: string }) => {
      this.postMessage({ type: "tool_start", ...data });
    });
    events.on("tool_delta", (data: { id: string; delta: string }) => {
      this.postMessage({ type: "tool_delta", ...data });
    });
    events.on("tool_end", (data: { id: string; name: string }) => {
      this.postMessage({ type: "tool_end", ...data });
    });
    events.on("approval", (data: ApprovalRequest) => {
      this.postMessage({
        type: "approval_required",
        tool_name: data.tool_name,
        input: data.input,
      });
    });
    events.on("done", (data: Record<string, unknown>) => {
      this.postMessage({
        type: "done",
        usage: data.usage,
        stop_reason: data.stop_reason,
      });
      this.postMessage({ type: "streaming", active: false });
      this.abortFn = undefined;
    });
    events.on("error", (err: Error) => {
      this.postMessage({ type: "error", text: err.message });
      this.postMessage({ type: "streaming", active: false });
      this.abortFn = undefined;
    });
    events.on("close", () => {
      this.postMessage({ type: "streaming", active: false });
      this.abortFn = undefined;
    });
  }

  private async handleApprove(approved: boolean): Promise<void> {
    if (!this.session) {
      return;
    }
    try {
      await this.client.approve(this.session.id, approved);
    } catch (err) {
      this.postMessage({ type: "error", text: `Approval failed: ${err}` });
    }
  }

  private handleAbort(): void {
    if (this.abortFn) {
      this.abortFn();
      this.abortFn = undefined;
      this.postMessage({ type: "streaming", active: false });
    }
  }

  private async handleSetMode(mode: string): Promise<void> {
    if (!this.session) {
      return;
    }
    try {
      const result = await this.client.setMode(this.session.id, mode);
      this.postMessage({ type: "mode_changed", mode: result.mode });
    } catch (err) {
      this.postMessage({ type: "error", text: `Set mode failed: ${err}` });
    }
  }

  public postMessage(msg: Record<string, unknown>): void {
    this.panel.webview.postMessage(msg);
  }

  private getHtml(webview: vscode.Webview): string {
    const nonce = getNonce();
    const styleUri = webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "media", "chat.css")
    );

    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy"
    content="default-src 'none'; style-src ${webview.cspSource} 'unsafe-inline'; script-src 'nonce-${nonce}';">
  <link rel="stylesheet" href="${styleUri}">
  <title>Ghost Chat</title>
</head>
<body>
  <div id="status-bar">
    <span id="connection-status">Connecting...</span>
    <span id="session-info"></span>
    <span id="mode-display"></span>
  </div>
  <div id="messages"></div>
  <div id="approval-bar" class="hidden">
    <span id="approval-tool"></span>
    <button id="approve-btn" class="approve">Allow</button>
    <button id="deny-btn" class="deny">Deny</button>
  </div>
  <div id="input-area">
    <textarea id="input" placeholder="Message Ghost..." rows="2"></textarea>
    <button id="send-btn" title="Send (Enter)">&#x27A4;</button>
    <button id="abort-btn" class="hidden" title="Stop">&#x25A0;</button>
  </div>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    const messagesEl = document.getElementById('messages');
    const inputEl = document.getElementById('input');
    const sendBtn = document.getElementById('send-btn');
    const abortBtn = document.getElementById('abort-btn');
    const approvalBar = document.getElementById('approval-bar');
    const approvalTool = document.getElementById('approval-tool');
    const approveBtn = document.getElementById('approve-btn');
    const denyBtn = document.getElementById('deny-btn');
    const connectionStatus = document.getElementById('connection-status');
    const sessionInfo = document.getElementById('session-info');
    const modeDisplay = document.getElementById('mode-display');

    let currentAssistantEl = null;
    let currentThinkingEl = null;
    let streaming = false;

    function scrollToBottom() {
      messagesEl.scrollTop = messagesEl.scrollHeight;
    }

    function addMessage(role, text) {
      const div = document.createElement('div');
      div.className = 'message ' + role;
      div.textContent = text;
      messagesEl.appendChild(div);
      scrollToBottom();
      return div;
    }

    function ensureAssistantBubble() {
      if (!currentAssistantEl) {
        currentAssistantEl = document.createElement('div');
        currentAssistantEl.className = 'message assistant';
        messagesEl.appendChild(currentAssistantEl);
      }
      return currentAssistantEl;
    }

    function addToolIndicator(name, status) {
      const div = document.createElement('div');
      div.className = 'tool-indicator ' + status;
      div.dataset.toolName = name;
      div.innerHTML = '<span class="tool-icon">' +
        (status === 'running' ? '&#9881;' : '&#10003;') +
        '</span> ' + name;
      messagesEl.appendChild(div);
      scrollToBottom();
      return div;
    }

    function send() {
      const text = inputEl.value.trim();
      if (!text || streaming) return;
      vscode.postMessage({ type: 'send', text });
      inputEl.value = '';
      inputEl.style.height = 'auto';
    }

    sendBtn.addEventListener('click', send);
    inputEl.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        send();
      }
    });

    inputEl.addEventListener('input', () => {
      inputEl.style.height = 'auto';
      inputEl.style.height = Math.min(inputEl.scrollHeight, 120) + 'px';
    });

    abortBtn.addEventListener('click', () => {
      vscode.postMessage({ type: 'abort' });
    });

    approveBtn.addEventListener('click', () => {
      vscode.postMessage({ type: 'approve', approved: true });
      approvalBar.classList.add('hidden');
    });
    denyBtn.addEventListener('click', () => {
      vscode.postMessage({ type: 'approve', approved: false });
      approvalBar.classList.add('hidden');
    });

    window.addEventListener('message', (event) => {
      const msg = event.data;
      switch (msg.type) {
        case 'status':
          connectionStatus.textContent = msg.connected ? 'Connected' : 'Disconnected';
          connectionStatus.className = msg.connected ? 'connected' : 'disconnected';
          break;
        case 'session':
          sessionInfo.textContent = msg.session.project_name;
          modeDisplay.textContent = msg.session.mode;
          break;
        case 'user_message':
          addMessage('user', msg.text);
          currentAssistantEl = null;
          currentThinkingEl = null;
          break;
        case 'text_delta':
          ensureAssistantBubble().textContent += msg.text;
          scrollToBottom();
          break;
        case 'thinking_delta':
          if (!currentThinkingEl) {
            currentThinkingEl = document.createElement('div');
            currentThinkingEl.className = 'message thinking';
            messagesEl.appendChild(currentThinkingEl);
          }
          currentThinkingEl.textContent += msg.text;
          scrollToBottom();
          break;
        case 'tool_start':
          addToolIndicator(msg.name, 'running');
          break;
        case 'tool_end': {
          const indicators = messagesEl.querySelectorAll('.tool-indicator.running');
          indicators.forEach(el => {
            if (el.dataset.toolName === msg.name) {
              el.className = 'tool-indicator done';
              el.querySelector('.tool-icon').innerHTML = '&#10003;';
            }
          });
          break;
        }
        case 'approval_required':
          approvalTool.textContent = msg.tool_name + ': approve?';
          approvalBar.classList.remove('hidden');
          scrollToBottom();
          break;
        case 'done':
          currentAssistantEl = null;
          currentThinkingEl = null;
          if (msg.usage) {
            const usage = msg.usage;
            const info = document.createElement('div');
            info.className = 'usage-info';
            info.textContent = 'in:' + (usage.input_tokens || 0) +
              ' out:' + (usage.output_tokens || 0);
            if (usage.cache_read_input_tokens) {
              info.textContent += ' cache:' + usage.cache_read_input_tokens;
            }
            messagesEl.appendChild(info);
            scrollToBottom();
          }
          break;
        case 'streaming':
          streaming = msg.active;
          sendBtn.classList.toggle('hidden', msg.active);
          abortBtn.classList.toggle('hidden', !msg.active);
          break;
        case 'mode_changed':
          modeDisplay.textContent = msg.mode;
          break;
        case 'error':
          addMessage('error', msg.text);
          break;
      }
    });

    vscode.postMessage({ type: 'ready' });
  </script>
</body>
</html>`;
  }
}

function getNonce(): string {
  let text = "";
  const chars =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  for (let i = 0; i < 32; i++) {
    text += chars.charAt(Math.floor(Math.random() * chars.length));
  }
  return text;
}
