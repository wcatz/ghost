// Ghost VSCode Extension -- activation and command registration.

import * as vscode from "vscode";
import { ChildProcess, spawn } from "child_process";
import { GhostClient } from "./ghost-client";
import { ChatSidebarProvider, ChatEditorPanel } from "./chat-webview";
import { MemoryPanelProvider } from "./memory-panel";
import { GhostStatusBar } from "./status-bar";

let currentClient: GhostClient;
let healthInterval: ReturnType<typeof setInterval> | undefined;
let ghostProcess: ChildProcess | undefined;

function createClient(): GhostClient {
  const config = vscode.workspace.getConfiguration("ghost");
  const serverUrl = config.get<string>("serverUrl", "http://127.0.0.1:2187");
  const authToken = config.get<string>("authToken", "");
  return new GhostClient(serverUrl, authToken);
}

export function activate(context: vscode.ExtensionContext): void {
  currentClient = createClient();

  const statusBar = new GhostStatusBar();
  const chatProvider = new ChatSidebarProvider(context.extensionUri, currentClient, statusBar);
  const memoryProvider = new MemoryPanelProvider(context.extensionUri, currentClient);

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
      const modes = ["chat", "code", "debug", "review", "plan", "refactor"];
      const mode = await vscode.window.showQuickPick(modes, { placeHolder: "Select mode" });
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
      ChatEditorPanel.createOrShow(context.extensionUri, currentClient, statusBar);
    }),
  );

  // Health check -- uses mutable currentClient reference via closure
  async function checkHealth(): Promise<void> {
    const available = await currentClient.isAvailable();
    statusBar.setConnected(available);
    chatProvider.postMessage({ type: "status", connected: available });
  }

  healthInterval = setInterval(checkHealth, 15000);
  checkHealth();

  // Auto-start ghost serve if configured and not already running
  const autoStart = vscode.workspace.getConfiguration("ghost").get<boolean>("autoStart", false);
  if (autoStart) {
    currentClient.isAvailable().then((available) => {
      if (!available) {
        startGhostServe(statusBar, chatProvider);
      }
    });
  }

  // Config change handler -- updates client everywhere
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (e.affectsConfiguration("ghost.serverUrl") || e.affectsConfiguration("ghost.authToken")) {
        currentClient = createClient();
        chatProvider.setClient(currentClient);
        memoryProvider.setClient(currentClient);
        if (ChatEditorPanel.currentPanel) {
          ChatEditorPanel.currentPanel.setClient(currentClient);
        }
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
  if (ghostProcess) {
    ghostProcess.kill();
    ghostProcess = undefined;
  }
}

function startGhostServe(statusBar: GhostStatusBar, chatProvider: ChatSidebarProvider): void {
  const outputChannel = vscode.window.createOutputChannel("Ghost");
  outputChannel.appendLine("Starting ghost serve...");

  ghostProcess = spawn("ghost", ["serve"], {
    stdio: ["ignore", "pipe", "pipe"],
    detached: false,
  });

  ghostProcess.stdout?.on("data", (data: Buffer) => {
    outputChannel.appendLine(data.toString().trimEnd());
  });

  ghostProcess.stderr?.on("data", (data: Buffer) => {
    outputChannel.appendLine(data.toString().trimEnd());
  });

  ghostProcess.on("error", (err) => {
    outputChannel.appendLine(`Failed to start ghost: ${err.message}`);
    vscode.window.showWarningMessage(
      "Ghost: could not start 'ghost serve'. Is ghost installed and in PATH?"
    );
    ghostProcess = undefined;
  });

  ghostProcess.on("exit", (code) => {
    outputChannel.appendLine(`ghost serve exited with code ${code}`);
    ghostProcess = undefined;
    statusBar.setConnected(false);
    chatProvider.postMessage({ type: "status", connected: false });
  });

  // Give it a moment to start, then check health
  setTimeout(async () => {
    const available = await currentClient.isAvailable();
    statusBar.setConnected(available);
    chatProvider.postMessage({ type: "status", connected: available });
    if (available) {
      outputChannel.appendLine("ghost serve is ready");
    }
  }, 2000);
}
