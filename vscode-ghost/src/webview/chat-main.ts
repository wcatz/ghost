// Webview entry point -- compiled by esbuild into out/webview/chat.js
// This runs inside the webview iframe, NOT in the extension host.

import { renderMarkdown, renderToolOutput, escapeHtml } from "./markdown";
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
const denyInstructionsInput = document.getElementById("deny-instructions") as HTMLInputElement;
const denyWithBtn = document.getElementById("deny-with-btn")!;

const micBtn = document.getElementById("mic-btn")!;

// State
let streaming = false;
let autoApprove = false;
let currentAssistantBubble: HTMLElement | null = null;
let accumulatedText = "";
let accumulatedThinking = "";
let currentThinkingBlock: HTMLElement | null = null;
let renderTimer: number | null = null;
let pendingImage: { media_type: string; data: string } | null = null;
const messageQueue: string[] = [];
// Tool timing: track start time for duration display
const toolStartTimes = new Map<string, number>();

// --- Send ---

function send(): void {
  const text = inputEl.value.trim();
  if (!text && !pendingImage) return;

  // Check for slash commands first
  if (text.startsWith("/") && !pendingImage) {
    const handled = executeSlashCommand(text);
    if (handled) {
      inputEl.value = "";
      slashMenu.classList.add("hidden");
      autoResizeInput();
      return;
    }
  }

  if (streaming) {
    messageQueue.push(text);
    inputEl.value = "";
    autoResizeInput();
    return;
  }

  addUserBubble(text);
  vscode.postMessage({ type: "send", text, image: pendingImage ?? undefined });
  inputEl.value = "";
  clearImage();
  autoResizeInput();
  setStreaming(true);
}

sendBtn.addEventListener("click", send);
inputEl.addEventListener("keydown", (e) => {
  // Slash menu navigation takes priority
  if (!slashMenu.classList.contains("hidden")) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      slashSelectedIndex = Math.min(slashSelectedIndex + 1, slashFiltered.length - 1);
      updateSlashMenu();
      return;
    }
    if (e.key === "ArrowUp") {
      e.preventDefault();
      slashSelectedIndex = Math.max(slashSelectedIndex - 1, 0);
      updateSlashMenu();
      return;
    }
    if (e.key === "Enter") {
      e.preventDefault();
      if (slashFiltered[slashSelectedIndex]) {
        inputEl.value = slashFiltered[slashSelectedIndex].cmd + " ";
        slashMenu.classList.add("hidden");
        // If the command has no args, execute it directly
        const cmd = slashFiltered[slashSelectedIndex].cmd;
        if (!slashFiltered[slashSelectedIndex].hasArgs) {
          inputEl.value = cmd;
          send();
        }
      }
      return;
    }
    if (e.key === "Escape") {
      e.preventDefault();
      slashMenu.classList.add("hidden");
      inputEl.value = "";
      autoResizeInput();
      return;
    }
  }

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

// --- Auto-resize textarea ---
function autoResizeInput(): void {
  inputEl.style.height = "auto";
  inputEl.style.height = Math.min(inputEl.scrollHeight, 150) + "px";
}
inputEl.addEventListener("input", autoResizeInput);

// --- Auto-approve ---

autoApproveBtn.addEventListener("click", () => {
  autoApprove = !autoApprove;
  autoApproveBtn.setAttribute("aria-pressed", String(autoApprove));
  autoApproveBtn.title = autoApprove ? "YOLO — auto-approving all tools" : "Auto-approve off";
  autoApproveBtn.classList.toggle("active", autoApprove);
  const icon = autoApproveBtn.querySelector(".ghost-btn-icon") as HTMLImageElement | null;
  if (icon) {
    icon.style.filter = autoApprove
      ? "invert(27%) sepia(100%) saturate(7000%) hue-rotate(0deg) brightness(100%) contrast(100%)"
      : "invert(73%) sepia(89%) saturate(400%) hue-rotate(140deg) brightness(100%) contrast(100%)";
  }
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

// Drag and drop handler
document.addEventListener("dragover", (e) => {
  e.preventDefault();
  document.body.classList.add("drag-over");
});

document.addEventListener("dragleave", (e) => {
  e.preventDefault();
  document.body.classList.remove("drag-over");
});

document.addEventListener("drop", (e) => {
  e.preventDefault();
  document.body.classList.remove("drag-over");
  const files = e.dataTransfer?.files;
  if (!files || files.length === 0) return;
  const file = files[0];
  if (!file.type.startsWith("image/")) return;
  const reader = new FileReader();
  reader.onload = () => {
    const base64 = (reader.result as string).split(",")[1];
    pendingImage = { media_type: file.type, data: base64 };
    previewImg.src = reader.result as string;
    imagePreview.classList.remove("hidden");
  };
  reader.readAsDataURL(file);
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
  bubble.className = "message user";
  bubble.setAttribute("role", "article");
  bubble.setAttribute("aria-label", "You said");
  bubble.textContent = text;
  messagesEl.appendChild(bubble);
  scrollToBottom();
}

function ensureAssistantBubble(): HTMLElement {
  if (!currentAssistantBubble) {
    currentAssistantBubble = document.createElement("div");
    currentAssistantBubble.className = "message assistant";
    currentAssistantBubble.setAttribute("role", "article");
    currentAssistantBubble.setAttribute("aria-label", "Ghost said");
    messagesEl.appendChild(currentAssistantBubble);
  }
  return currentAssistantBubble;
}

// Get or create the text content div inside the assistant bubble.
// Tool indicators and results are siblings of this div, so innerHTML
// replacement doesn't destroy them.
function ensureTextDiv(): HTMLElement {
  const bubble = ensureAssistantBubble();
  let textDiv = bubble.querySelector<HTMLElement>(".assistant-text");
  if (!textDiv) {
    textDiv = document.createElement("div");
    textDiv.className = "assistant-text";
    bubble.appendChild(textDiv);
  }
  return textDiv;
}

function appendText(delta: string): void {
  accumulatedText += delta;
  scheduleRender();
}

function scheduleRender(): void {
  if (renderTimer !== null) return;
  renderTimer = window.setTimeout(() => {
    renderTimer = null;
    const textDiv = ensureTextDiv();
    textDiv.innerHTML = renderMarkdown(accumulatedText);
    scrollToBottom();
  }, 50);
}

function finalizeAssistant(): void {
  if (renderTimer !== null) {
    clearTimeout(renderTimer);
    renderTimer = null;
  }
  // Finalize thinking block
  if (currentThinkingBlock && accumulatedThinking) {
    const contentEl = currentThinkingBlock.querySelector(".thinking-content");
    if (contentEl) {
      contentEl.textContent = accumulatedThinking;
    }
  }
  currentThinkingBlock = null;
  accumulatedThinking = "";

  // Finalize assistant text
  if (accumulatedText) {
    const textDiv = ensureTextDiv();
    textDiv.innerHTML = renderMarkdown(accumulatedText);
  }
  currentAssistantBubble = null;
  accumulatedText = "";
  scrollToBottom();
}

function addThinkingBlock(delta: string): void {
  accumulatedThinking += delta;
  if (!currentThinkingBlock) {
    currentThinkingBlock = document.createElement("details");
    currentThinkingBlock.className = "thinking-block";
    const summary = document.createElement("summary");
    summary.textContent = "Thinking...";
    summary.setAttribute("aria-label", "Toggle thinking details");
    currentThinkingBlock.appendChild(summary);
    const content = document.createElement("div");
    content.className = "thinking-content";
    currentThinkingBlock.appendChild(content);
    ensureAssistantBubble().appendChild(currentThinkingBlock);
  }
  const contentEl = currentThinkingBlock.querySelector(".thinking-content");
  if (contentEl) {
    contentEl.textContent = accumulatedThinking;
  }
  scrollToBottom();
}

function addToolIndicator(name: string, id: string): void {
  toolStartTimes.set(id, Date.now());
  const el = document.createElement("div");
  el.className = "tool-indicator";
  el.id = `tool-${id}`;
  el.setAttribute("role", "status");
  el.setAttribute("aria-label", `Running tool: ${name}`);
  el.innerHTML = `<span class="spinner" aria-hidden="true"></span> <span class="tool-name">${escapeHtml(name)}</span>`;
  ensureAssistantBubble().appendChild(el);
  scrollToBottom();
}

function completeToolIndicator(id: string, name: string): void {
  const el = document.getElementById(`tool-${id}`);
  if (el) {
    const startTime = toolStartTimes.get(id);
    const duration = startTime ? ((Date.now() - startTime) / 1000).toFixed(1) : "?";
    toolStartTimes.delete(id);
    el.innerHTML = `<span class="check" aria-hidden="true">\u2713</span> <span class="tool-name">${escapeHtml(name)}</span> <span class="tool-time">${duration}s</span>`;
    el.setAttribute("aria-label", `Tool complete: ${name} (${duration}s)`);
  }
}

function addToolResult(name: string, output: string, isError: boolean): void {
  if (!output || output.trim().length === 0) return;
  // Only show tool results if they have meaningful content
  const el = document.createElement("details");
  el.className = "tool-result-block" + (isError ? " tool-error" : "");
  const summary = document.createElement("summary");
  summary.setAttribute("aria-label", `Tool output: ${name}`);
  summary.textContent = isError ? `${name} (error)` : `${name} output`;
  el.appendChild(summary);
  const content = document.createElement("div");
  content.className = "tool-result-content";
  content.innerHTML = renderToolOutput(output);
  el.appendChild(content);
  ensureAssistantBubble().appendChild(el);
  scrollToBottom();
}

function addErrorMessage(text: string): void {
  const el = document.createElement("div");
  el.className = "message error";
  el.setAttribute("role", "alert");
  el.setAttribute("aria-label", "Error");
  el.textContent = text;
  messagesEl.appendChild(el);
  scrollToBottom();
}

function addSystemMessage(text: string): void {
  const el = document.createElement("div");
  el.className = "message system";
  el.setAttribute("role", "status");
  el.textContent = text;
  messagesEl.appendChild(el);
  scrollToBottom();
}

function addTruncationWarning(): void {
  const el = document.createElement("div");
  el.className = "message warning";
  el.setAttribute("role", "alert");
  el.textContent = "Response was truncated (max tokens reached). Use /continue to keep going.";
  messagesEl.appendChild(el);
  scrollToBottom();
}

function scrollToBottom(): void {
  messagesEl.scrollTo({ top: messagesEl.scrollHeight, behavior: "auto" });
}

// --- Approval ---

function showApproval(toolName: string, input: unknown): void {
  if (autoApprove) {
    vscode.postMessage({ type: "approve", approved: true });
    return;
  }
  approvalToolName.textContent = toolName;
  approvalSummary.textContent = typeof input === "object" && input !== null
    ? JSON.stringify(input, null, 2).slice(0, 500)
    : String(input);
  denyInstructionsInput.value = "";
  approvalOverlay.classList.remove("hidden");
  approvalOverlay.focus();
}

function hideApproval(): void {
  approvalOverlay.classList.add("hidden");
  denyInstructionsInput.value = "";
}

// Approval keyboard
approvalOverlay.addEventListener("keydown", (e) => {
  // Don't capture keys when typing in the instructions input
  if (e.target === denyInstructionsInput) {
    if (e.key === "Enter") {
      e.preventDefault();
      const instructions = denyInstructionsInput.value.trim();
      if (instructions) {
        vscode.postMessage({ type: "approve", approved: false, instructions });
        hideApproval();
      }
    }
    return;
  }
  if (e.key === "y") {
    e.preventDefault();
    vscode.postMessage({ type: "approve", approved: true });
    hideApproval();
  } else if (e.key === "n") {
    e.preventDefault();
    vscode.postMessage({ type: "approve", approved: false });
    hideApproval();
  } else if (e.key === "Escape") {
    e.preventDefault();
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
  vscode.postMessage({ type: "approve", approved: false });
  hideApproval();
});
denyWithBtn.addEventListener("click", () => {
  const instructions = denyInstructionsInput.value.trim();
  vscode.postMessage({ type: "approve", approved: false, instructions: instructions || undefined });
  hideApproval();
});

// --- Message handler ---

window.addEventListener("message", (event) => {
  const msg = event.data as ExtToWebviewMessage;
  switch (msg.type) {
    case "text_delta":
      appendText(msg.text);
      break;
    case "thinking_delta":
      addThinkingBlock(msg.text);
      break;
    case "tool_start":
      addToolIndicator(msg.name, msg.id);
      break;
    case "tool_delta":
      // Accumulate tool input (optional display)
      break;
    case "tool_end":
      completeToolIndicator(msg.id, msg.name);
      break;
    case "tool_result":
      addToolResult(msg.name, msg.output, msg.is_error);
      break;
    case "approval_required":
      showApproval(msg.tool_name, msg.input);
      break;
    case "approval_resolved":
      hideApproval();
      break;
    case "done":
      // Mid-turn "done" (stop_reason: "tool_use") means the agentic loop
      // is about to execute tools and continue. Don't finalize the bubble —
      // keep accumulating into the same assistant message.
      if (msg.stop_reason === "tool_use") {
        // Flush any pending text render, but keep the bubble alive.
        if (renderTimer !== null) {
          clearTimeout(renderTimer);
          renderTimer = null;
          if (accumulatedText) {
            const textDiv = ensureTextDiv();
            textDiv.innerHTML = renderMarkdown(accumulatedText);
          }
        }
      } else {
        finalizeAssistant();
        setStreaming(false);
      }
      if (msg.session_cost) {
        footerCost.textContent = msg.session_cost;
      }
      if (msg.stop_reason === "max_tokens") {
        addTruncationWarning();
      }
      break;
    case "error":
      addErrorMessage(msg.text);
      setStreaming(false);
      break;
    case "streaming":
      if (msg.active) {
        hideApproval(); // safety net: dismiss stale overlays on new stream
      }
      setStreaming(msg.active);
      break;
    case "aborted":
      addSystemMessage("Stream aborted: " + msg.reason);
      hideApproval();
      setStreaming(false);
      break;
    case "status":
      connectionDot.className = msg.connected ? "dot connected" : "dot disconnected";
      connectionDot.setAttribute("aria-label", msg.connected ? "Connected" : "Disconnected");
      break;
    case "session":
      sessionInfoEl.textContent = msg.session.project_name;
      modeBadge.textContent = msg.session.mode;
      modeBadge.className = "mode-badge mode-" + msg.session.mode;
      break;
    case "monthly_cost":
      sessionCostEl.textContent = msg.text;
      break;
    case "mode_changed":
      modeBadge.textContent = msg.mode;
      modeBadge.className = "mode-badge mode-" + msg.mode;
      addSystemMessage(`Mode changed to: ${msg.mode}`);
      break;
    case "user_message":
      addUserBubble(msg.text);
      break;
    case "system_message":
      addSystemMessage(msg.text);
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
          bubble.className = "message assistant";
          bubble.setAttribute("role", "article");
          bubble.setAttribute("aria-label", "Ghost said");
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
    case "voice_error":
      addErrorMessage(msg.text);
      voiceActive = false;
      micBtn.classList.remove("voice-active");
      micBtn.setAttribute("aria-label", "Voice input");
      break;
    case "voice_started":
      voiceActive = true;
      voiceUpdatePartial('Listening — say "ghost" to activate...');
      break;
    case "voice_stopped":
      voiceActive = false;
      voiceRemovePartial();
      micBtn.classList.remove("voice-active", "voice-triggered");
      micBtn.setAttribute("aria-label", "Voice input");
      break;
    case "voice_triggered":
      micBtn.classList.add("voice-triggered");
      voiceUpdatePartial("Activated — listening for your message...");
      break;
    case "voice_partial":
      voiceUpdatePartial(msg.text);
      break;
    case "voice_final":
      voiceRemovePartial();
      break;
  }
});

// --- Slash commands ---

interface SlashCommand {
  cmd: string;
  desc: string;
  hasArgs: boolean;
}

const slashCommands: SlashCommand[] = [
  { cmd: "/mode", desc: "Change mode (chat, code, debug, review, plan, refactor)", hasArgs: true },
  { cmd: "/continue", desc: "Continue truncated response", hasArgs: false },
  { cmd: "/compact", desc: "Compact conversation context", hasArgs: false },
  { cmd: "/tokens", desc: "Show token usage", hasArgs: false },
  { cmd: "/cost", desc: "Show session cost", hasArgs: false },
  { cmd: "/clear", desc: "Clear conversation display", hasArgs: false },
  { cmd: "/auto-approve", desc: "Toggle auto-approve mode", hasArgs: false },
  { cmd: "/export", desc: "Export conversation", hasArgs: false },
  { cmd: "/health", desc: "Check daemon connection", hasArgs: false },
  { cmd: "/theme", desc: "Theme information", hasArgs: false },
];

const slashMenu = document.getElementById("slash-menu")!;
let slashSelectedIndex = 0;
let slashFiltered: SlashCommand[] = [];

function updateSlashMenu(): void {
  const value = inputEl.value;
  if (!value.startsWith("/") || streaming) {
    slashMenu.classList.add("hidden");
    return;
  }

  const query = value.toLowerCase();
  slashFiltered = slashCommands.filter(
    (c) => c.cmd.toLowerCase().startsWith(query) || c.desc.toLowerCase().includes(query.slice(1))
  );

  if (slashFiltered.length === 0) {
    slashMenu.classList.add("hidden");
    return;
  }

  slashSelectedIndex = Math.min(slashSelectedIndex, slashFiltered.length - 1);
  slashMenu.innerHTML = slashFiltered
    .map((c, i) =>
      `<div class="slash-item${i === slashSelectedIndex ? " selected" : ""}" role="option" aria-selected="${i === slashSelectedIndex}"><span class="slash-cmd">${escapeHtml(c.cmd)}</span> <span class="slash-desc">${escapeHtml(c.desc)}</span></div>`
    )
    .join("");
  slashMenu.classList.remove("hidden");
}

function executeSlashCommand(fullText: string): boolean {
  const parts = fullText.trim().split(/\s+/);
  const cmd = parts[0].toLowerCase();
  const args = parts.slice(1).join(" ");

  switch (cmd) {
    case "/clear":
      messagesEl.innerHTML = "";
      return true;
    case "/cost": {
      const costText = sessionCostEl.textContent || footerCost.textContent || "No cost data yet";
      addSystemMessage(`Session cost: ${costText}`);
      return true;
    }
    case "/auto-approve":
      autoApproveBtn.click();
      return true;
    case "/mode":
      if (args) {
        vscode.postMessage({ type: "slash_command", command: "mode", args });
      } else {
        addSystemMessage("Usage: /mode <chat|code|debug|review|plan|refactor>");
      }
      return true;
    case "/continue":
      vscode.postMessage({ type: "slash_command", command: "continue" });
      return true;
    case "/compact":
      vscode.postMessage({ type: "slash_command", command: "compact" });
      return true;
    case "/tokens":
      vscode.postMessage({ type: "slash_command", command: "tokens" });
      return true;
    case "/export":
      vscode.postMessage({ type: "slash_command", command: "export" });
      return true;
    case "/health":
      vscode.postMessage({ type: "slash_command", command: "health" });
      return true;
    case "/theme":
      vscode.postMessage({ type: "slash_command", command: "theme" });
      return true;
    default:
      return false;
  }
}

inputEl.addEventListener("input", () => {
  updateSlashMenu();
  autoResizeInput();
});

slashMenu.addEventListener("click", (e) => {
  const target = (e.target as HTMLElement).closest(".slash-item");
  if (target) {
    const index = Array.from(slashMenu.children).indexOf(target);
    if (slashFiltered[index]) {
      const cmd = slashFiltered[index].cmd;
      if (slashFiltered[index].hasArgs) {
        inputEl.value = cmd + " ";
        slashMenu.classList.add("hidden");
        inputEl.focus();
      } else {
        inputEl.value = cmd;
        slashMenu.classList.add("hidden");
        send();
      }
    }
  }
});

// --- Voice Input ---
// Mic capture and WebSocket streaming run in the extension host (Node.js).
// The webview only sends start/stop signals and displays results.

let voiceActive = false;
let voicePartialEl: HTMLElement | null = null;

// Update (or create) a single in-place partial transcript element.
function voiceUpdatePartial(text: string): void {
  if (!voicePartialEl) {
    voicePartialEl = document.createElement("div");
    voicePartialEl.className = "message system voice-partial";
    voicePartialEl.setAttribute("role", "status");
    messagesEl.appendChild(voicePartialEl);
  }
  voicePartialEl.textContent = text;
  scrollToBottom();
}

function voiceRemovePartial(): void {
  voicePartialEl?.remove();
  voicePartialEl = null;
}

micBtn.addEventListener("click", () => {
  if (voiceActive) {
    vscode.postMessage({ type: "voice_stop" });
    voiceActive = false;
    micBtn.classList.remove("voice-active");
    micBtn.setAttribute("aria-label", "Voice input");
  } else {
    addSystemMessage("Starting voice capture...");
    micBtn.classList.add("voice-active");
    micBtn.setAttribute("aria-label", "Stop voice input");
    vscode.postMessage({ type: "voice_start" });
  }
});

// Tell extension we're ready
vscode.postMessage({ type: "ready" });
