// Unified webview provider for Ghost chat.
// Used by both the sidebar panel and the editor panel.

import * as vscode from "vscode";
import { GhostClient } from "./ghost-client";
import { GhostStatusBar } from "./status-bar";
import { getNonce } from "./util";
import type {
  ExtToWebviewMessage,
  WebviewToExtMessage,
  SessionInfo,
  ImageAttachment,
} from "./protocol";

export class ChatWebview implements vscode.Disposable {
  private webview: vscode.Webview;
  private extensionUri: vscode.Uri;
  private client: GhostClient;
  private statusBar: GhostStatusBar;
  private session?: SessionInfo;
  private abortFn?: () => void;
  private disposables: vscode.Disposable[] = [];

  constructor(
    webview: vscode.Webview,
    extensionUri: vscode.Uri,
    client: GhostClient,
    statusBar: GhostStatusBar,
  ) {
    this.webview = webview;
    this.extensionUri = extensionUri;
    this.client = client;
    this.statusBar = statusBar;

    webview.options = {
      enableScripts: true,
      localResourceRoots: [vscode.Uri.joinPath(extensionUri, "media"), vscode.Uri.joinPath(extensionUri, "out")],
    };

    webview.html = this.getHtml();

    this.disposables.push(
      webview.onDidReceiveMessage((msg: WebviewToExtMessage) => this.onMessage(msg)),
    );
  }

  setClient(client: GhostClient): void {
    this.client = client;
  }

  postMessage(msg: ExtToWebviewMessage): void {
    this.webview.postMessage(msg);
  }

  dispose(): void {
    this.abortFn?.();
    this.disposables.forEach((d) => d.dispose());
  }

  private async onMessage(msg: WebviewToExtMessage): Promise<void> {
    switch (msg.type) {
      case "ready":
        await this.ensureSession();
        await this.loadHistory();
        break;
      case "send":
        await this.handleSend(msg.text, msg.image);
        break;
      case "approve":
        await this.handleApprove(msg.approved, msg.instructions);
        break;
      case "abort":
        this.abortFn?.();
        break;
      case "set_auto_approve":
        if (this.session) {
          await this.client.setAutoApprove(this.session.id, msg.enabled);
        }
        break;
      case "setMode":
        await this.handleSetMode(msg.mode);
        break;
      case "attach_image":
        await this.handleAttachImage();
        break;
      case "slash_command":
        await this.handleSlashCommand(msg.command, msg.args);
        break;
    }
  }

  private async handleSend(text: string, image?: ImageAttachment): Promise<void> {
    if (!this.session) {
      await this.ensureSession();
    }
    if (!this.session) {
      this.postMessage({ type: "error", text: "No session available" });
      return;
    }

    this.postMessage({ type: "streaming", active: true });

    try {
      const { events: emitter, abort } = this.client.sendMessage(this.session.id, text, image);
      this.abortFn = abort;

      emitter.on("text", (text: string) => {
        this.postMessage({ type: "text_delta", text });
      });
      emitter.on("thinking", (text: string) => {
        this.postMessage({ type: "thinking_delta", text });
      });
      emitter.on("tool_start", (data: Record<string, string>) => {
        this.postMessage({ type: "tool_start", id: data.id, name: data.name });
      });
      emitter.on("tool_delta", (data: Record<string, string>) => {
        this.postMessage({ type: "tool_delta", id: data.id, delta: data.delta });
      });
      emitter.on("tool_end", (data: Record<string, string>) => {
        this.postMessage({ type: "tool_end", id: data.id, name: data.name });
      });
      emitter.on("approval", (data: Record<string, unknown>) => {
        this.postMessage({
          type: "approval_required",
          tool_name: data.tool_name as string,
          input: data.input,
        });
      });
      emitter.on("approval_resolved", () => {
        this.postMessage({ type: "approval_resolved" });
      });
      emitter.on("done", (data: Record<string, unknown>) => {
        const sessionCost = (data.session_cost as string) ?? null;
        const stopReason = (data.stop_reason as string) ?? "end_turn";
        this.postMessage({
          type: "done",
          session_cost: sessionCost,
          usage: (data.usage as ExtToWebviewMessage & { type: "done" })["usage"] ?? null,
          stop_reason: stopReason,
        });
        this.postMessage({ type: "streaming", active: false });
        // Update status bar cost
        if (sessionCost) {
          this.statusBar.setCost(sessionCost);
        }
      });
      emitter.on("error", (err: Error) => {
        this.postMessage({ type: "error", text: err.message ?? "Unknown error" });
        this.postMessage({ type: "streaming", active: false });
      });
      emitter.on("close", () => {
        this.postMessage({ type: "streaming", active: false });
      });
    } catch (err) {
      this.postMessage({ type: "error", text: String(err) });
      this.postMessage({ type: "streaming", active: false });
    }
  }

  private async handleApprove(approved: boolean, instructions?: string): Promise<void> {
    if (!this.session) return;
    try {
      await this.client.approve(this.session.id, approved, instructions);
    } catch (err) {
      this.postMessage({ type: "error", text: `Approval failed: ${err}` });
    }
  }

  private async handleSetMode(mode: string): Promise<void> {
    if (!this.session) return;
    try {
      const result = await this.client.setMode(this.session.id, mode);
      this.statusBar.setMode(result.mode);
      this.postMessage({ type: "mode_changed", mode: result.mode });
    } catch (err) {
      this.postMessage({ type: "error", text: `Set mode failed: ${err}` });
    }
  }

  private async handleAttachImage(): Promise<void> {
    const uris = await vscode.window.showOpenDialog({
      canSelectFiles: true,
      canSelectMany: false,
      filters: { Images: ["png", "jpg", "jpeg", "gif", "webp"] },
    });
    if (!uris || uris.length === 0) return;
    const data = await vscode.workspace.fs.readFile(uris[0]);
    const ext = uris[0].fsPath.split(".").pop()?.toLowerCase() ?? "png";
    const mimeMap: Record<string, string> = {
      png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg", gif: "image/gif", webp: "image/webp",
    };
    const base64 = Buffer.from(data).toString("base64");
    this.postMessage({
      type: "image_data",
      image: { media_type: mimeMap[ext] ?? "image/png", data: base64 },
    });
  }

  private async handleSlashCommand(command: string, args?: string): Promise<void> {
    switch (command) {
      case "model":
        this.postMessage({ type: "system_message", text: "Model selection not yet implemented" });
        break;
      case "continue":
        if (this.session) {
          await this.handleSend("Continue from where you left off.");
        }
        break;
      case "compact":
        this.postMessage({ type: "system_message", text: "Compact not yet implemented" });
        break;
      case "tokens":
        this.postMessage({ type: "system_message", text: "Token info displayed in footer" });
        break;
      case "cost":
        // Handled in webview directly
        break;
      case "clear":
        // Handled in webview directly
        break;
      case "auto-approve":
        // Handled in webview directly
        break;
      case "export":
        this.postMessage({ type: "system_message", text: "Export not yet implemented" });
        break;
      case "health": {
        const available = await this.client.isAvailable();
        this.postMessage({
          type: "system_message",
          text: available ? "Ghost daemon: connected" : "Ghost daemon: disconnected",
        });
        break;
      }
      case "theme":
        this.postMessage({ type: "system_message", text: "Theme follows VS Code settings" });
        break;
      case "mode":
        if (args) {
          await this.handleSetMode(args);
        }
        break;
    }
  }

  private async ensureSession(): Promise<void> {
    try {
      const sessions = await this.client.listSessions();
      if (sessions.length > 0) {
        this.session = sessions[0];
      } else {
        const workspacePath = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? process.cwd();
        this.session = await this.client.createSession(workspacePath);
      }
      if (this.session) {
        this.postMessage({ type: "session", session: this.session });
        this.statusBar.setMode(this.session.mode);
      }
    } catch {
      // Server not available -- will retry on next send
    }
  }

  private async loadHistory(): Promise<void> {
    if (!this.session) return;
    try {
      const history = await this.client.getHistory(this.session.id);
      if (history.length > 0) {
        this.postMessage({ type: "history", messages: history });
      }
    } catch {
      // No history available
    }
  }

  private getHtml(): string {
    const nonce = getNonce();
    const styleUri = this.webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "media", "chat.css"),
    );
    const scriptUri = this.webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "out", "webview", "chat.js"),
    );
    const cspSource = this.webview.cspSource;

    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy"
    content="default-src 'none'; style-src ${cspSource} 'unsafe-inline'; script-src 'nonce-${nonce}'; img-src data: ${cspSource};">
  <link rel="stylesheet" href="${styleUri}">
  <title>Ghost Chat</title>
</head>
<body>
  <div id="header" role="banner">
    <div id="header-left">
      <span id="connection-dot" class="dot disconnected" role="status" aria-label="Disconnected"></span>
      <span id="session-info" aria-label="Project"></span>
      <span id="mode-badge" class="mode-badge" aria-label="Mode">chat</span>
    </div>
    <div id="header-right">
      <button id="auto-approve-btn" class="header-btn" aria-label="Auto-approve off" aria-pressed="false" title="Auto-approve off">&#x1F512;</button>
      <span id="session-cost" role="status" aria-label="Session cost"></span>
    </div>
  </div>

  <div id="messages" role="log" aria-label="Chat messages" aria-live="polite"></div>

  <div id="approval-overlay" class="hidden" role="dialog" aria-modal="true" aria-label="Tool approval required" tabindex="-1">
    <div id="approval-modal">
      <div class="modal-header">Tool Approval Required</div>
      <div id="approval-tool-name" class="modal-tool"></div>
      <pre id="approval-summary" class="modal-preview"></pre>
      <div class="modal-actions">
        <button id="approve-btn" class="modal-btn approve" aria-label="Allow tool execution">[y] Allow</button>
        <button id="deny-btn" class="modal-btn deny" aria-label="Deny tool execution">[n] Deny</button>
      </div>
      <div class="modal-instructions">
        <input id="deny-instructions" type="text" placeholder="Deny with instructions..." aria-label="Deny with instructions">
        <button id="deny-with-btn" class="modal-btn deny-with" aria-label="Deny with instructions">Deny + instruct</button>
      </div>
    </div>
  </div>

  <div id="image-preview" class="hidden" aria-label="Attached image preview">
    <img id="preview-img" alt="Attached image">
    <button id="remove-image" aria-label="Remove attached image">&times;</button>
  </div>

  <div id="slash-menu" class="hidden" role="listbox" aria-label="Slash commands"></div>

  <div id="input-area" role="form" aria-label="Message input">
    <textarea id="input" aria-label="Type a message" placeholder="Message Ghost... (/ for commands)" rows="1"></textarea>
    <div id="input-actions">
      <button id="attach-btn" class="icon-btn" aria-label="Attach image">&#x1F4CE;</button>
      <button id="send-btn" class="icon-btn" aria-label="Send message">&#x27A4;</button>
      <button id="abort-btn" class="icon-btn hidden" aria-label="Stop response">&#x25A0;</button>
    </div>
  </div>

  <div id="footer" role="status" aria-live="polite">
    <span id="footer-cost"></span>
  </div>

  <script nonce="${nonce}" src="${scriptUri}"></script>
</body>
</html>`;
  }
}

// --- Sidebar Provider (thin wrapper) ---

export class ChatSidebarProvider implements vscode.WebviewViewProvider {
  static readonly viewType = "ghost.chat";
  private chatWebview?: ChatWebview;

  constructor(
    private readonly extensionUri: vscode.Uri,
    private client: GhostClient,
    private readonly statusBar: GhostStatusBar,
  ) {}

  resolveWebviewView(view: vscode.WebviewView): void {
    this.chatWebview?.dispose();
    this.chatWebview = new ChatWebview(view.webview, this.extensionUri, this.client, this.statusBar);
  }

  setClient(client: GhostClient): void {
    this.client = client;
    this.chatWebview?.setClient(client);
  }

  postMessage(msg: ExtToWebviewMessage): void {
    this.chatWebview?.postMessage(msg);
  }
}

// --- Editor Panel (singleton, thin wrapper) ---

export class ChatEditorPanel {
  static currentPanel?: ChatEditorPanel;
  private chatWebview: ChatWebview;
  private panel: vscode.WebviewPanel;

  private constructor(panel: vscode.WebviewPanel, extensionUri: vscode.Uri, client: GhostClient, statusBar: GhostStatusBar) {
    this.panel = panel;
    this.chatWebview = new ChatWebview(panel.webview, extensionUri, client, statusBar);
    panel.onDidDispose(() => {
      this.chatWebview.dispose();
      ChatEditorPanel.currentPanel = undefined;
    });
  }

  static createOrShow(extensionUri: vscode.Uri, client: GhostClient, statusBar: GhostStatusBar): void {
    if (ChatEditorPanel.currentPanel) {
      ChatEditorPanel.currentPanel.panel.reveal();
      return;
    }
    const panel = vscode.window.createWebviewPanel(
      "ghost.editor",
      "Ghost Chat",
      vscode.ViewColumn.One,
      {
        enableScripts: true,
        retainContextWhenHidden: true,
        localResourceRoots: [vscode.Uri.joinPath(extensionUri, "media"), vscode.Uri.joinPath(extensionUri, "out")],
      },
    );
    ChatEditorPanel.currentPanel = new ChatEditorPanel(panel, extensionUri, client, statusBar);
  }

  setClient(client: GhostClient): void {
    this.chatWebview.setClient(client);
  }

  postMessage(msg: ExtToWebviewMessage): void {
    this.chatWebview.postMessage(msg);
  }
}
