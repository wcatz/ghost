// Webview entry point — compiled by esbuild into out/webview/chat.js
// This runs inside the webview iframe, NOT in the extension host.

import { renderMarkdown } from "./markdown";
import type { ExtToWebviewMessage, WebviewToExtMessage } from "../protocol";

// Acquire the VS Code API handle (available in webview context)
declare function acquireVsCodeApi(): {
  postMessage(msg: WebviewToExtMessage): void;
  getState(): unknown;
  setState(state: unknown): void;
};

const vscode = acquireVsCodeApi();

// DOM elements
const messagesEl = document.getElementById("messages")!;
const inputEl = document.getElementById("input") as HTMLTextAreaElement;
const sendBtn = document.getElementById("send-btn")!;
const abortBtn = document.getElementById("abort-btn")!;
const attachBtn = document.getElementById("attach-btn")!;
const footerCost = document.getElementById("footer-cost")!;
const approvalOverlay = document.getElementById("approval-overlay")!;
const approvalToolName = document.getElementById("approval-tool-name")!;
const approvalSummary = document.getElementById("approval-summary")!;
const autoApproveBtn = document.getElementById("auto-approve-btn")!;
const sessionCostEl = document.getElementById("session-cost")!;
const connectionDot = document.getElementById("connection-dot")!;
const sessionInfoEl = document.getElementById("session-info")!;
const modeBadge = document.getElementById("mode-badge")!;
const imagePreview = document.getElementById("image-preview")!;
const previewImg = document.getElementById("preview-img") as HTMLImageElement;
const removeImageBtn = document.getElementById("remove-image")!;

// State
let streaming = false;
let autoApprove = false;
let currentAssistantBubble: HTMLElement | null = null;
let accumulatedText = "";
let renderTimer: number | null = null;
let pendingImage: { media_type: string; data: string } | null = null;
const messageQueue: string[] = [];

// --- Send ---

function send(): void {
  const text = inputEl.value.trim();
  if (!text && !pendingImage) return;

  if (streaming) {
    messageQueue.push(text);
    inputEl.value = "";
    return;
  }

  addUserBubble(text);
  vscode.postMessage({ type: "send", text, image: pendingImage ?? undefined });
  inputEl.value = "";
  clearImage();
  setStreaming(true);
}

sendBtn.addEventListener("click", send);
inputEl.addEventListener("keydown", (e) => {
  if (e.key === "Enter" && !e.shiftKey) {
    e.preventDefault();
    send();
  }
});

abortBtn.addEventListener("click", () => {
  vscode.postMessage({ type: "abort" });
});

attachBtn.addEventListener("click", () => {
  vscode.postMessage({ type: "attach_image" });
});

// --- Auto-approve ---

autoApproveBtn.addEventListener("click", () => {
  autoApprove = !autoApprove;
  autoApproveBtn.setAttribute("aria-pressed", String(autoApprove));
  autoApproveBtn.title = autoApprove ? "Auto-approve ON" : "Auto-approve OFF";
  autoApproveBtn.classList.toggle("active", autoApprove);
  vscode.postMessage({ type: "set_auto_approve", enabled: autoApprove });
});

// --- Image ---

removeImageBtn.addEventListener("click", clearImage);

function clearImage(): void {
  pendingImage = null;
  imagePreview.classList.add("hidden");
}

// Paste handler
document.addEventListener("paste", (e) => {
  const items = e.clipboardData?.items;
  if (!items) return;
  for (const item of items) {
    if (item.type.startsWith("image/")) {
      e.preventDefault();
      const blob = item.getAsFile();
      if (!blob) return;
      const reader = new FileReader();
      reader.onload = () => {
        const base64 = (reader.result as string).split(",")[1];
        pendingImage = { media_type: item.type, data: base64 };
        previewImg.src = reader.result as string;
        imagePreview.classList.remove("hidden");
      };
      reader.readAsDataURL(blob);
      return;
    }
  }
});

// --- Streaming state ---

function setStreaming(active: boolean): void {
  streaming = active;
  sendBtn.classList.toggle("hidden", active);
  abortBtn.classList.toggle("hidden", !active);
  messagesEl.setAttribute("aria-busy", String(active));

  if (!active) {
    // Drain message queue
    if (messageQueue.length > 0) {
      const next = messageQueue.shift()!;
      inputEl.value = next;
      send();
    }
  }
}

// --- DOM rendering ---

function addUserBubble(text: string): void {
  const bubble = document.createElement("div");
  bubble.className = "message user-message";
  bubble.setAttribute("role", "article");
  bubble.setAttribute("aria-label", "You said");
  bubble.textContent = text;
  messagesEl.appendChild(bubble);
  scrollToBottom();
}

function ensureAssistantBubble(): HTMLElement {
  if (!currentAssistantBubble) {
    currentAssistantBubble = document.createElement("div");
    currentAssistantBubble.className = "message assistant-message";
    currentAssistantBubble.setAttribute("role", "article");
    currentAssistantBubble.setAttribute("aria-label", "Ghost said");
    messagesEl.appendChild(currentAssistantBubble);
  }
  return currentAssistantBubble;
}

function appendText(delta: string): void {
  accumulatedText += delta;
  scheduleRender();
}

function scheduleRender(): void {
  if (renderTimer !== null) return;
  renderTimer = window.setTimeout(() => {
    renderTimer = null;
    const bubble = ensureAssistantBubble();
    bubble.innerHTML = renderMarkdown(accumulatedText);
    scrollToBottom();
  }, 50);
}

function finalizeAssistant(): void {
  if (renderTimer !== null) {
    clearTimeout(renderTimer);
    renderTimer = null;
  }
  if (accumulatedText) {
    const bubble = ensureAssistantBubble();
    bubble.innerHTML = renderMarkdown(accumulatedText);
  }
  currentAssistantBubble = null;
  accumulatedText = "";
  scrollToBottom();
}

function addThinkingIndicator(): void {
  const el = document.createElement("div");
  el.className = "thinking-indicator";
  el.id = "current-thinking";
  el.setAttribute("role", "status");
  el.setAttribute("aria-label", "Ghost is thinking");
  el.innerHTML = '<span class="thinking-dot"></span> thinking...';
  messagesEl.appendChild(el);
  scrollToBottom();
}

function removeThinkingIndicator(): void {
  document.getElementById("current-thinking")?.remove();
}

function addToolIndicator(name: string, id: string): void {
  removeThinkingIndicator();
  const el = document.createElement("div");
  el.className = "tool-indicator";
  el.id = `tool-${id}`;
  el.setAttribute("role", "status");
  el.setAttribute("aria-label", `Running tool: ${name}`);
  el.innerHTML = `<span class="tool-spinner"></span> ${escapeHtml(name)}`;
  messagesEl.appendChild(el);
  scrollToBottom();
}

function completeToolIndicator(id: string, name: string): void {
  const el = document.getElementById(`tool-${id}`);
  if (el) {
    el.innerHTML = `<span class="tool-done">&#x2713;</span> ${escapeHtml(name)}`;
    el.setAttribute("aria-label", `Tool complete: ${name}`);
  }
}

function addErrorMessage(text: string): void {
  const el = document.createElement("div");
  el.className = "message error-message";
  el.setAttribute("role", "alert");
  el.textContent = text;
  messagesEl.appendChild(el);
  scrollToBottom();
}

function scrollToBottom(): void {
  messagesEl.scrollTo({ top: messagesEl.scrollHeight, behavior: "auto" });
}

function escapeHtml(text: string): string {
  const div = document.createElement("div");
  div.textContent = text;
  return div.innerHTML;
}

// --- Approval ---

function showApproval(toolName: string, input: unknown): void {
  if (autoApprove) {
    vscode.postMessage({ type: "approve", approved: true });
    return;
  }
  approvalToolName.textContent = toolName;
  approvalSummary.textContent = typeof input === "object" && input !== null
    ? JSON.stringify(input, null, 2).slice(0, 200)
    : String(input);
  approvalOverlay.classList.remove("hidden");
  approvalOverlay.focus();
}

function hideApproval(): void {
  approvalOverlay.classList.add("hidden");
}

// Approval keyboard
approvalOverlay.addEventListener("keydown", (e) => {
  if (e.key === "y") {
    vscode.postMessage({ type: "approve", approved: true });
    hideApproval();
  } else if (e.key === "n") {
    const reason = prompt("Deny reason (optional):");
    vscode.postMessage({ type: "approve", approved: false, instructions: reason ?? undefined });
    hideApproval();
  } else if (e.key === "Escape") {
    vscode.postMessage({ type: "approve", approved: false });
    hideApproval();
  }
});

// Approval buttons
document.getElementById("approve-btn")?.addEventListener("click", () => {
  vscode.postMessage({ type: "approve", approved: true });
  hideApproval();
});
document.getElementById("deny-btn")?.addEventListener("click", () => {
  const reason = prompt("Deny reason (optional):");
  vscode.postMessage({ type: "approve", approved: false, instructions: reason ?? undefined });
  hideApproval();
});

// --- Message handler ---

window.addEventListener("message", (event) => {
  const msg = event.data as ExtToWebviewMessage;
  switch (msg.type) {
    case "text_delta":
      removeThinkingIndicator();
      appendText(msg.text);
      break;
    case "thinking_delta":
      if (!document.getElementById("current-thinking")) {
        addThinkingIndicator();
      }
      break;
    case "tool_start":
      addToolIndicator(msg.name, msg.id);
      break;
    case "tool_end":
      completeToolIndicator(msg.id, msg.name);
      break;
    case "approval_required":
      showApproval(msg.tool_name, msg.input);
      break;
    case "approval_resolved":
      hideApproval();
      break;
    case "done":
      finalizeAssistant();
      setStreaming(false);
      if (msg.session_cost) {
        footerCost.textContent = msg.session_cost;
        sessionCostEl.textContent = msg.session_cost;
      }
      break;
    case "error":
      addErrorMessage(msg.text);
      setStreaming(false);
      break;
    case "streaming":
      setStreaming(msg.active);
      break;
    case "status":
      connectionDot.className = msg.connected ? "dot connected" : "dot disconnected";
      connectionDot.setAttribute("aria-label", msg.connected ? "Connected" : "Disconnected");
      break;
    case "session":
      sessionInfoEl.textContent = msg.session.project_name;
      modeBadge.textContent = msg.session.mode;
      break;
    case "user_message":
      addUserBubble(msg.text);
      break;
    case "image_data":
      pendingImage = msg.image;
      previewImg.src = `data:${msg.image.media_type};base64,${msg.image.data}`;
      imagePreview.classList.remove("hidden");
      break;
    case "history":
      messagesEl.innerHTML = "";
      for (const m of msg.messages) {
        if (m.role === "user") {
          addUserBubble(m.content);
        } else {
          const bubble = document.createElement("div");
          bubble.className = "message assistant-message";
          bubble.setAttribute("role", "article");
          bubble.innerHTML = renderMarkdown(m.content);
          messagesEl.appendChild(bubble);
        }
      }
      scrollToBottom();
      break;
    case "send_from_command":
      inputEl.value = msg.text;
      send();
      break;
  }
});

// Tell extension we're ready
vscode.postMessage({ type: "ready" });
