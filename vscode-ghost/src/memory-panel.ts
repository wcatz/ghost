import * as vscode from "vscode";
import { GhostClient, Memory } from "./ghost-client";

export class MemoryPanelProvider implements vscode.WebviewViewProvider {
  public static readonly viewType = "ghost.memories";

  private view?: vscode.WebviewView;
  private client: GhostClient;

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

    webviewView.webview.html = this.getHtml(webviewView.webview);

    webviewView.webview.onDidReceiveMessage(async (msg) => {
      switch (msg.type) {
        case "ready":
          await this.loadProjects();
          break;
        case "load_memories":
          await this.loadMemories(msg.projectId);
          break;
        case "search":
          await this.searchMemories(msg.projectId, msg.query);
          break;
        case "delete":
          await this.deleteMemory(msg.memoryId, msg.projectId);
          break;
      }
    });
  }

  private async loadProjects(): Promise<void> {
    try {
      const projects = await this.client.listProjects();
      this.postMessage({ type: "projects", projects });
    } catch (err) {
      this.postMessage({
        type: "error",
        text: `Failed to load projects: ${err}`,
      });
    }
  }

  private async loadMemories(projectId: string): Promise<void> {
    try {
      const memories = await this.client.listMemories(projectId);
      this.postMessage({ type: "memories", memories });
    } catch (err) {
      this.postMessage({
        type: "error",
        text: `Failed to load memories: ${err}`,
      });
    }
  }

  private async searchMemories(
    projectId: string,
    query: string
  ): Promise<void> {
    try {
      const memories = await this.client.searchMemories(
        projectId,
        query
      );
      this.postMessage({ type: "memories", memories });
    } catch (err) {
      this.postMessage({
        type: "error",
        text: `Search failed: ${err}`,
      });
    }
  }

  private async deleteMemory(
    memoryId: string,
    projectId: string
  ): Promise<void> {
    try {
      await this.client.deleteMemory(memoryId);
      // Reload to reflect deletion.
      await this.loadMemories(projectId);
    } catch (err) {
      this.postMessage({
        type: "error",
        text: `Delete failed: ${err}`,
      });
    }
  }

  private postMessage(msg: Record<string, unknown>): void {
    this.view?.webview.postMessage(msg);
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
  <title>Ghost Memories</title>
  <style>
    #controls { display: flex; gap: 4px; padding: 6px 8px; border-bottom: 1px solid var(--ghost-border); flex-shrink: 0; }
    #project-select { flex: 1; background: var(--ghost-input-bg); color: var(--ghost-input-fg); border: 1px solid var(--ghost-input-border); border-radius: 4px; padding: 4px; font-family: inherit; font-size: inherit; }
    #search-input { flex: 2; background: var(--ghost-input-bg); color: var(--ghost-input-fg); border: 1px solid var(--ghost-input-border); border-radius: 4px; padding: 4px 8px; font-family: inherit; font-size: inherit; outline: none; }
    #search-input:focus { border-color: var(--ghost-accent); }
    #memory-list { flex: 1; overflow-y: auto; padding: 8px; display: flex; flex-direction: column; gap: 6px; }
    .memory-card { background: var(--vscode-textBlockQuote-background, #2a2d2e); border: 1px solid var(--ghost-border); border-radius: 6px; padding: 8px 10px; }
    .memory-header { display: flex; justify-content: space-between; align-items: center; margin-bottom: 4px; }
    .memory-category { font-size: 0.8em; background: var(--ghost-input-bg); padding: 1px 6px; border-radius: 3px; color: var(--ghost-accent); }
    .memory-importance { font-size: 0.75em; color: var(--ghost-muted); }
    .memory-content { font-size: 0.9em; line-height: 1.4; white-space: pre-wrap; word-break: break-word; }
    .memory-footer { display: flex; justify-content: space-between; align-items: center; margin-top: 6px; font-size: 0.75em; color: var(--ghost-muted); }
    .memory-tags { display: flex; gap: 4px; flex-wrap: wrap; }
    .memory-tag { background: var(--ghost-input-bg); padding: 0 4px; border-radius: 2px; }
    .delete-btn { background: none; border: none; color: var(--ghost-error); cursor: pointer; font-size: 0.85em; opacity: 0.6; }
    .delete-btn:hover { opacity: 1; }
    .empty-state { text-align: center; color: var(--ghost-muted); padding: 20px; font-size: 0.9em; }
  </style>
</head>
<body>
  <div id="controls">
    <select id="project-select"><option value="">Select project...</option></select>
    <input id="search-input" type="text" placeholder="Search memories...">
  </div>
  <div id="memory-list">
    <div class="empty-state">Select a project to view memories</div>
  </div>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    const projectSelect = document.getElementById('project-select');
    const searchInput = document.getElementById('search-input');
    const memoryList = document.getElementById('memory-list');

    let currentProjectId = '';

    projectSelect.addEventListener('change', () => {
      currentProjectId = projectSelect.value;
      if (currentProjectId) {
        vscode.postMessage({ type: 'load_memories', projectId: currentProjectId });
      }
    });

    let searchTimeout;
    searchInput.addEventListener('input', () => {
      clearTimeout(searchTimeout);
      searchTimeout = setTimeout(() => {
        const query = searchInput.value.trim();
        if (!currentProjectId) return;
        if (query.length >= 2) {
          vscode.postMessage({ type: 'search', projectId: currentProjectId, query });
        } else if (query.length === 0) {
          vscode.postMessage({ type: 'load_memories', projectId: currentProjectId });
        }
      }, 300);
    });

    function renderMemories(memories) {
      if (!memories || memories.length === 0) {
        memoryList.innerHTML = '<div class="empty-state">No memories found</div>';
        return;
      }
      memoryList.innerHTML = memories.map(m => {
        const tags = (m.tags || []).map(t => '<span class="memory-tag">' + escapeHtml(t) + '</span>').join('');
        const date = new Date(m.created_at).toLocaleDateString();
        return '<div class="memory-card">' +
          '<div class="memory-header">' +
            '<span class="memory-category">' + escapeHtml(m.category) + '</span>' +
            '<span class="memory-importance">' + (m.importance * 100).toFixed(0) + '%</span>' +
          '</div>' +
          '<div class="memory-content">' + escapeHtml(m.content) + '</div>' +
          '<div class="memory-footer">' +
            '<div class="memory-tags">' + tags + '<span>' + escapeHtml(m.source) + ' · ' + date + '</span></div>' +
            '<button class="delete-btn" data-id="' + m.id + '">delete</button>' +
          '</div>' +
        '</div>';
      }).join('');

      memoryList.querySelectorAll('.delete-btn').forEach(btn => {
        btn.addEventListener('click', () => {
          vscode.postMessage({ type: 'delete', memoryId: btn.dataset.id, projectId: currentProjectId });
        });
      });
    }

    function escapeHtml(str) {
      const div = document.createElement('div');
      div.textContent = str || '';
      return div.innerHTML;
    }

    window.addEventListener('message', (event) => {
      const msg = event.data;
      switch (msg.type) {
        case 'projects':
          projectSelect.innerHTML = '<option value="">Select project...</option>' +
            msg.projects.map(p => '<option value="' + escapeHtml(p.id) + '">' + escapeHtml(p.name || p.id) + '</option>').join('');
          break;
        case 'memories':
          renderMemories(msg.memories);
          break;
        case 'error':
          memoryList.innerHTML = '<div class="empty-state" style="color:var(--ghost-error)">' + escapeHtml(msg.text) + '</div>';
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
