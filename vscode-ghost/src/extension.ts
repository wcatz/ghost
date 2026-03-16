// Ghost VSCode Extension — activation and command registration.

import * as vscode from "vscode";
import { GhostClient } from "./ghost-client";
import { ChatSidebarProvider, ChatEditorPanel } from "./chat-webview";
import { MemoryPanelProvider } from "./memory-panel";
import { GhostStatusBar } from "./status-bar";

let currentClient: GhostClient;
let healthInterval: ReturnType<typeof setInterval> | undefined;

function createClient(): GhostClient {
  const config = vscode.workspace.getConfiguration("ghost");
  const serverUrl = config.get<string>("serverUrl", "http://127.0.0.1:2187");
  const authToken = config.get<string>("authToken", "");
  return new GhostClient(serverUrl, authToken);
}

export function activate(context: vscode.ExtensionContext): void {
  currentClient = createClient();

  const chatProvider = new ChatSidebarProvider(context.extensionUri, currentClient);
  const memoryProvider = new MemoryPanelProvider(context.extensionUri, currentClient);
  const statusBar = new GhostStatusBar();

  // Register view providers
  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider(ChatSidebarProvider.viewType, chatProvider),
    vscode.window.registerWebviewViewProvider(MemoryPanelProvider.viewType, memoryProvider),
  );

  // Register commands
  context.subscriptions.push(
    vscode.commands.registerCommand("ghost.sendMessage", async () => {
      const text = await vscode.window.showInputBox({ prompt: "Message Ghost" });
      if (text) {
        chatProvider.postMessage({ type: "send_from_command", text });
      }
    }),
    vscode.commands.registerCommand("ghost.newSession", async () => {
      try {
        const workspacePath = vscode.workspace.workspaceFolders?.[0]?.uri.fsPath ?? process.cwd();
        await currentClient.createSession(workspacePath);
        vscode.window.showInformationMessage("Ghost: New session created");
      } catch (err) {
        vscode.window.showErrorMessage(`Ghost: ${err}`);
      }
    }),
    vscode.commands.registerCommand("ghost.setMode", async () => {
      const mode = await vscode.window.showQuickPick(["chat"], { placeHolder: "Select mode" });
      if (mode) {
        statusBar.setMode(mode);
      }
    }),
    vscode.commands.registerCommand("ghost.searchMemories", async () => {
      vscode.commands.executeCommand("ghost.memories.focus");
    }),
    vscode.commands.registerCommand("ghost.showChat", () => {
      vscode.commands.executeCommand("ghost.chat.focus");
    }),
    vscode.commands.registerCommand("ghost.openEditor", () => {
      ChatEditorPanel.createOrShow(context.extensionUri, currentClient);
    }),
  );

  // Health check — uses mutable currentClient reference
  async function checkHealth(): Promise<void> {
    const available = await currentClient.isAvailable();
    statusBar.setConnected(available);
    chatProvider.postMessage({ type: "status", connected: available });
  }

  healthInterval = setInterval(checkHealth, 15000);
  checkHealth();

  // Config change handler — updates client everywhere
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration("ghost.serverUrl") || e.affectsConfiguration("ghost.authToken")) {
        currentClient = createClient();
        chatProvider.setClient(currentClient);
        memoryProvider.setClient(currentClient);
        checkHealth();
      }
    }),
  );

  // Cleanup
  context.subscriptions.push({
    dispose: () => {
      if (healthInterval) clearInterval(healthInterval);
      statusBar.dispose();
    },
  });
}

export function deactivate(): void {
  if (healthInterval) {
    clearInterval(healthInterval);
    healthInterval = undefined;
  }
}
