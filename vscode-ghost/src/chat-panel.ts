import * as vscode from "vscode";
import {
  GhostClient,
  Session,
  ApprovalRequest,
} from "./ghost-client";
import { getChatHtml } from "./webview-html";

export class ChatPanelProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = "ghost.chat";

  private view?: vscode.WebviewView;
  private client: GhostClient;
  private session?: Session;
  private abortFn?: () => void;

  constructor(
    private readonly extensionUri: vscode.Uri,
    client: GhostClient
  ) {
    this.client = client;
  }

  public setClient(client: GhostClient): void {
    this.client = client;
  }

  public resolveWebviewView(
    webviewView: vscode.WebviewView,
    _context: vscode.WebviewViewResolveContext,
    _token: vscode.CancellationToken
  ): void {
    this.view = webviewView;

    webviewView.webview.options = {
      enableScripts: true,
      localResourceRoots: [
        vscode.Uri.joinPath(this.extensionUri, "media"),
      ],
    };

    webviewView.webview.html = getChatHtml(webviewView.webview, this.extensionUri);

    webviewView.webview.onDidReceiveMessage(async (msg) => {
      switch (msg.type) {
        case "send":
          await this.handleSend(msg.text, msg.image);
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
        case "set_auto_approve":
          await this.handleAutoApprove(msg.enabled);
          break;
        case "attach_image":
          await this.handleAttachImage();
          break;
        case "ready":
          await this.handleReady();
          break;
      }
    });
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
      this.postMessage({ type: "error", text: `Failed to create session: ${err}` });
    }
  }

  private async handleSend(text: string, image?: { media_type: string; data: string }): Promise<void> {
    if (!text.trim() && !image) {
      return;
    }
    await this.ensureSession();
    if (!this.session) {
      return;
    }

    this.postMessage({ type: "user_message", text });
    this.postMessage({ type: "streaming", active: true });

    const { events, abort } = this.client.sendMessage(this.session.id, text, image);
    this.abortFn = abort;

    events.on("text", (t: string) => this.postMessage({ type: "text_delta", text: t }));
    events.on("thinking", (t: string) => this.postMessage({ type: "thinking_delta", text: t }));
    events.on("tool_start", (data: { id: string; name: string }) =>
      this.postMessage({ type: "tool_start", ...data }));
    events.on("tool_delta", (data: { id: string; delta: string }) =>
      this.postMessage({ type: "tool_delta", ...data }));
    events.on("tool_end", (data: { id: string; name: string }) =>
      this.postMessage({ type: "tool_end", ...data }));
    events.on("approval", (data: ApprovalRequest) =>
      this.postMessage({ type: "approval_required", tool_name: data.tool_name, input: data.input }));
    events.on("approval_resolved", () =>
      this.postMessage({ type: "approval_resolved" }));
    events.on("done", (data: Record<string, unknown>) => {
      this.postMessage({ type: "done", usage: data.usage, stop_reason: data.stop_reason });
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
    if (!this.session) return;
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
    if (!this.session) return;
    try {
      const result = await this.client.setMode(this.session.id, mode);
      this.postMessage({ type: "mode_changed", mode: result.mode });
    } catch (err) {
      this.postMessage({ type: "error", text: `Set mode failed: ${err}` });
    }
  }

  private async handleAutoApprove(enabled: boolean): Promise<void> {
    if (!this.session) return;
    try {
      await this.client.setAutoApprove(this.session.id, enabled);
    } catch (err) {
      this.postMessage({ type: "error", text: `Auto-approve failed: ${err}` });
    }
  }

  private async handleAttachImage(): Promise<void> {
    const uris = await vscode.window.showOpenDialog({
      canSelectMany: false,
      filters: { Images: ["png", "jpg", "jpeg", "gif", "webp"] },
    });
    if (!uris || uris.length === 0) return;
    const data = await vscode.workspace.fs.readFile(uris[0]);
    const base64 = Buffer.from(data).toString("base64");
    const ext = uris[0].fsPath.split(".").pop()?.toLowerCase() || "png";
    const mimeMap: Record<string, string> = {
      png: "image/png", jpg: "image/jpeg", jpeg: "image/jpeg",
      gif: "image/gif", webp: "image/webp",
    };
    this.postMessage({
      type: "image_data",
      image: { media_type: mimeMap[ext] || "image/png", data: base64 },
    });
  }

  public postMessage(msg: Record<string, unknown>): void {
    this.view?.webview.postMessage(msg);
  }
}
