// Unified webview provider for Ghost chat.
// Used by both the sidebar panel and the editor panel.

import * as vscode from "vscode";
import { spawn, type ChildProcess } from "child_process";
import { GhostClient } from "./ghost-client";
import { GhostStatusBar } from "./status-bar";
import { getNonce } from "./util";
import type {
  ExtToWebviewMessage,
  WebviewToExtMessage,
  SessionInfo,
  ImageAttachment,
} from "./protocol";
import WebSocket from "ws";

export class ChatWebview implements vscode.Disposable {
  private webview: vscode.Webview;
  private extensionUri: vscode.Uri;
  private client: GhostClient;
  private statusBar: GhostStatusBar;
  private session?: SessionInfo;
  private abortFn?: () => void;
  private disposables: vscode.Disposable[] = [];
  private onModeChanged?: (mode: string) => void;

  // Voice capture state (runs in extension host, not webview).
  private voiceProc?: ChildProcess;
  private voiceWs?: WebSocket;
  private triggerActive = false;
  private triggerTimer?: ReturnType<typeof setTimeout>;
  private static readonly TRIGGER_WORD = "ghost";
  private static readonly TRIGGER_TIMEOUT_MS = 30_000;
  private triggerRegex = new RegExp(ChatWebview.TRIGGER_WORD, "i");

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

  setOnModeChanged(cb: (mode: string) => void): void {
    this.onModeChanged = cb;
  }

  postMessage(msg: ExtToWebviewMessage): void {
    this.webview.postMessage(msg);
  }

  dispose(): void {
    this.abortFn?.();
    this.voiceStop();
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
      case "voice_start":
        await this.handleVoiceStart();
        break;
      case "voice_stop":
        this.voiceStop();
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
      emitter.on("tool_result", (data: Record<string, unknown>) => {
        this.postMessage({
          type: "tool_result",
          id: (data.id as string) ?? "",
          name: (data.name as string) ?? "",
          output: (data.output as string) ?? "",
          is_error: data.is_error === true || data.is_error === "true",
        });
      });
      emitter.on("tool_diff", (data: Record<string, string>) => {
        // Render diffs as tool results so they appear in the chat
        const diffText = data.diff || data.patch || "";
        if (diffText) {
          this.postMessage({
            type: "tool_result",
            id: data.id ?? "",
            name: data.name ?? "file_edit",
            output: diffText,
            is_error: false,
          });
        }
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
        // Only mark streaming complete on final done, not mid-turn tool_use.
        if (stopReason !== "tool_use") {
          this.postMessage({ type: "streaming", active: false });
        }
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
      this.onModeChanged?.(result.mode);
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
        if (this.session) {
          await this.handleSend("Summarize our conversation so far in a concise paragraph, then we can continue from that context.");
        }
        break;
      case "tokens": {
        const cost = this.statusBar.getCost();
        this.postMessage({
          type: "system_message",
          text: cost ? `Session usage: ${cost}` : "No token data yet — send a message first.",
        });
        break;
      }
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
        await this.handleExport();
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

  private async handleVoiceStart(): Promise<void> {
    // Voice capture runs entirely in the extension host (Node.js).
    // VSCode webviews cannot access navigator.mediaDevices.getUserMedia.
    //
    // Always-on listening with wake word ("ghost"):
    //   - Mic stays open, continuously streaming to AssemblyAI
    //   - Utterances containing the wake word activate a 30s session
    //   - During the session, all utterances are sent as chat messages
    //   - The wake word is stripped from the transcript before sending
    //   - Session auto-deactivates after 30s of silence
    try {
      const result = await this.client.getTranscribeToken();

      const proc = spawn("arecord", [
        "-f", "S16_LE", "-r", "16000", "-c", "1", "-t", "raw", "-q", "-",
      ]);

      proc.on("error", (err) => {
        this.postMessage({ type: "voice_error", text: `Mic capture failed: ${err.message}. Install alsa-utils.` });
        this.voiceStop();
      });

      this.voiceProc = proc;

      const wsUrl = `${result.ws_url}?token=${result.token}&sample_rate=16000&encoding=pcm_s16le`;
      const ws = new WebSocket(wsUrl);
      this.voiceWs = ws;

      ws.on("open", () => {
        this.postMessage({ type: "voice_started" });
        proc.stdout.on("data", (chunk: Buffer) => {
          if (ws.readyState === WebSocket.OPEN) {
            ws.send(chunk);
          }
        });
      });

      ws.on("message", (data: Buffer | string) => {
        try {
          const msg = JSON.parse(data.toString());
          if (msg.type === "Turn") {
            const text: string = msg.transcript || "";

            if (msg.end_of_turn && text.trim()) {
              this.handleVoiceUtterance(text);
            } else if (text) {
              // Show partial — indicate trigger state.
              if (this.triggerActive) {
                this.postMessage({ type: "voice_partial", text });
              } else if (this.triggerRegex.test(text)) {
                this.postMessage({ type: "voice_partial", text: `[wake] ${text}` });
              }
            }
          } else if (msg.type === "Termination") {
            this.voiceStop();
          } else if (msg.type === "Error") {
            this.postMessage({ type: "voice_error", text: `AssemblyAI: ${JSON.stringify(msg)}` });
            this.voiceStop();
          }
        } catch {
          // Ignore unparseable messages.
        }
      });

      ws.on("error", (err) => {
        this.postMessage({ type: "voice_error", text: `WebSocket error: ${err.message}` });
        this.voiceStop();
      });

      ws.on("close", () => {
        this.voiceStop();
      });

      proc.on("close", () => {
        if (this.voiceWs && this.voiceWs.readyState === WebSocket.OPEN) {
          this.voiceWs.send(JSON.stringify({ type: "Terminate" }));
        }
      });

    } catch (err) {
      this.postMessage({ type: "voice_error", text: `Voice unavailable: ${err}` });
      this.voiceStop();
    }
  }

  private handleVoiceUtterance(text: string): void {
    const hasTrigger = this.triggerRegex.test(text);

    if (hasTrigger) {
      this.activateTrigger();
    }

    if (!this.triggerActive) {
      // Not activated — ignore utterance.
      return;
    }

    // Strip the wake word and clean up leading punctuation/whitespace.
    const cleanText = text.replace(this.triggerRegex, "").replace(/^[\s,.:!?]+/, "").trim();
    if (!cleanText) {
      // Wake word only — silently activate, don't send empty message.
      return;
    }

    // Reset the trigger timer on each utterance (keeps session alive).
    this.activateTrigger();

    this.postMessage({ type: "voice_final", text: cleanText });
    this.handleSend(cleanText);
  }

  private activateTrigger(): void {
    this.triggerActive = true;
    clearTimeout(this.triggerTimer);
    this.triggerTimer = setTimeout(() => {
      this.triggerActive = false;
      this.postMessage({ type: "system_message", text: "Voice session timed out — say \"ghost\" to reactivate" });
    }, ChatWebview.TRIGGER_TIMEOUT_MS);
    this.postMessage({ type: "voice_triggered" });
  }

  private voiceStop(): void {
    if (this.voiceProc) {
      this.voiceProc.kill();
      this.voiceProc = undefined;
    }
    if (this.voiceWs) {
      if (this.voiceWs.readyState === WebSocket.OPEN) {
        this.voiceWs.send(JSON.stringify({ type: "Terminate" }));
        this.voiceWs.close();
      }
      this.voiceWs = undefined;
    }
    this.triggerActive = false;
    clearTimeout(this.triggerTimer);
    this.postMessage({ type: "voice_stopped" });
  }

  private async handleExport(): Promise<void> {
    if (!this.session) {
      this.postMessage({ type: "system_message", text: "No active session" });
      return;
    }
    try {
      const history = await this.client.getHistory(this.session.id);
      if (history.length === 0) {
        this.postMessage({ type: "system_message", text: "No messages to export" });
        return;
      }
      const markdown = history
        .map((m) => `## ${m.role === "user" ? "You" : "Ghost"}\n\n${m.content}`)
        .join("\n\n---\n\n");
      await vscode.env.clipboard.writeText(markdown);
      this.postMessage({ type: "system_message", text: `Exported ${history.length} messages to clipboard` });
    } catch (err) {
      this.postMessage({ type: "error", text: `Export failed: ${err}` });
    }
  }

  private async ensureSession(): Promise<void> {
    try {
      const workspacePath = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath;
      if (!workspacePath) {
        this.postMessage({ type: "error", text: "No workspace folder open — Ghost needs a project to work with." });
        return;
      }
      const sessions = await this.client.listSessions();
      // Find a session that matches the current workspace.
      const match = sessions.find((s) => s.project_path === workspacePath);
      if (match) {
        this.session = match;
      } else {
        this.session = await this.client.createSession(workspacePath);
      }
      if (this.session) {
        this.postMessage({ type: "session", session: this.session });
        this.statusBar.setMode(this.session.mode);
        this.onModeChanged?.(this.session.mode);
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
    const ghostIconUri = this.webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "media", "ghost-icon.svg"),
    );
    const cspSource = this.webview.cspSource;
    const pcmProcessorUri = this.webview.asWebviewUri(
      vscode.Uri.joinPath(this.extensionUri, "media", "pcm-processor.js"),
    );

    return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy"
    content="default-src 'none'; style-src ${cspSource} 'unsafe-inline'; script-src 'nonce-${nonce}' blob:; img-src data: ${cspSource}; connect-src wss://streaming.assemblyai.com; worker-src blob: ${cspSource};">
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
      <button id="auto-approve-btn" class="header-btn" aria-label="Auto-approve off" aria-pressed="false" title="Auto-approve off"><img src="${ghostIconUri}" class="ghost-btn-icon" alt="ghost" /></button>
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
      <button id="mic-btn" class="icon-btn" aria-label="Voice input" data-pcm-processor="${pcmProcessorUri}">&#x1F3A4;</button>
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
    this.chatWebview.setOnModeChanged((mode) => {
      const label = mode.charAt(0).toUpperCase() + mode.slice(1);
      panel.title = `Ghost ${label}`;
    });
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
    panel.iconPath = vscode.Uri.joinPath(extensionUri, "media", "ghost-tab.png");
    ChatEditorPanel.currentPanel = new ChatEditorPanel(panel, extensionUri, client, statusBar);
  }

  setClient(client: GhostClient): void {
    this.chatWebview.setClient(client);
  }

  postMessage(msg: ExtToWebviewMessage): void {
    this.chatWebview.postMessage(msg);
  }
}
