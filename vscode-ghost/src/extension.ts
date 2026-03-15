import * as vscode from "vscode";
import { GhostClient } from "./ghost-client";
import { ChatPanelProvider } from "./chat-panel";
import { ChatEditorPanel } from "./chat-editor";
import { MemoryPanelProvider } from "./memory-panel";
import { GhostStatusBar } from "./status-bar";

let statusBar: GhostStatusBar;
let healthInterval: ReturnType<typeof setInterval>;

export function activate(context: vscode.ExtensionContext): void {
  const client = createClient();

  // --- Webview Providers ---
  const chatProvider = new ChatPanelProvider(context.extensionUri, client);
  const memoryProvider = new MemoryPanelProvider(
    context.extensionUri,
    client
  );

  context.subscriptions.push(
    vscode.window.registerWebviewViewProvider(
      ChatPanelProvider.viewType,
      chatProvider
    ),
    vscode.window.registerWebviewViewProvider(
      MemoryPanelProvider.viewType,
      memoryProvider
    )
  );

  // --- Status Bar ---
  statusBar = new GhostStatusBar(client);
  context.subscriptions.push({ dispose: () => statusBar.dispose() });

  // --- Commands ---
  context.subscriptions.push(
    vscode.commands.registerCommand("ghost.sendMessage", async () => {
      const text = await vscode.window.showInputBox({
        prompt: "Message Ghost",
        placeHolder: "Ask Ghost something...",
      });
      if (text) {
        chatProvider.postMessage({ type: "send_from_command", text });
      }
    }),

    vscode.commands.registerCommand("ghost.newSession", async () => {
      const folders = vscode.workspace.workspaceFolders;
      if (!folders || folders.length === 0) {
        vscode.window.showErrorMessage("No workspace folder open.");
        return;
      }
      try {
        const session = await client.createSession(
          folders[0].uri.fsPath
        );
        vscode.window.showInformationMessage(
          `Ghost session started: ${session.project_name}`
        );
        statusBar.setMode(session.mode);
      } catch (err) {
        vscode.window.showErrorMessage(`Failed to start session: ${err}`);
      }
    }),

    vscode.commands.registerCommand("ghost.setMode", async () => {
      const modes = [
        "code",
        "chat",
        "debug",
        "review",
        "plan",
        "refactor",
      ];
      const mode = await vscode.window.showQuickPick(modes, {
        placeHolder: "Select Ghost mode",
      });
      if (mode) {
        // Find active session and set mode.
        try {
          const sessions = await client.listSessions();
          if (sessions.length === 0) {
            vscode.window.showWarningMessage("No active Ghost session.");
            return;
          }
          const result = await client.setMode(sessions[0].id, mode);
          statusBar.setMode(result.mode);
          chatProvider.postMessage({
            type: "mode_changed",
            mode: result.mode,
          });
        } catch (err) {
          vscode.window.showErrorMessage(`Set mode failed: ${err}`);
        }
      }
    }),

    vscode.commands.registerCommand("ghost.searchMemories", async () => {
      const query = await vscode.window.showInputBox({
        prompt: "Search Ghost memories",
        placeHolder: "architecture, patterns, decisions...",
      });
      if (query) {
        try {
          const projects = await client.listProjects();
          if (projects.length === 0) {
            vscode.window.showWarningMessage("No Ghost projects found.");
            return;
          }
          const memories = await client.searchMemories(
            projects[0].id,
            query
          );
          if (memories.length === 0) {
            vscode.window.showInformationMessage("No memories found.");
            return;
          }
          const items = memories.map((m) => ({
            label: `[${m.category}] ${m.content.slice(0, 80)}`,
            detail: `importance: ${(m.importance * 100).toFixed(0)}% | ${m.source} | ${m.tags.join(", ")}`,
            memory: m,
          }));
          await vscode.window.showQuickPick(items, {
            placeHolder: `${memories.length} memories found`,
          });
        } catch (err) {
          vscode.window.showErrorMessage(`Search failed: ${err}`);
        }
      }
    }),

    vscode.commands.registerCommand("ghost.showChat", () => {
      vscode.commands.executeCommand("ghost.chat.focus");
    }),

    vscode.commands.registerCommand("ghost.openEditor", () => {
      ChatEditorPanel.createOrShow(context.extensionUri, client);
    })
  );

  // --- Configuration Change ---
  context.subscriptions.push(
    vscode.workspace.onDidChangeConfiguration((e) => {
      if (
        e.affectsConfiguration("ghost.serverUrl") ||
        e.affectsConfiguration("ghost.authToken")
      ) {
        const newClient = createClient();
        chatProvider.setClient(newClient);
        memoryProvider.setClient(newClient);
        statusBar.setClient(newClient);
      }
    })
  );

  // --- Health Check ---
  checkHealth(client);
  healthInterval = setInterval(() => checkHealth(client), 15000);
  context.subscriptions.push({
    dispose: () => clearInterval(healthInterval),
  });
}

export function deactivate(): void {
  if (healthInterval) {
    clearInterval(healthInterval);
  }
}

function createClient(): GhostClient {
  const config = vscode.workspace.getConfiguration("ghost");
  const serverUrl = config.get<string>(
    "serverUrl",
    "http://127.0.0.1:2187"
  );
  const authToken = config.get<string>("authToken", "");
  return new GhostClient(serverUrl, authToken);
}

async function checkHealth(client: GhostClient): Promise<void> {
  const available = await client.isAvailable();
  statusBar?.setConnected(available);
}
