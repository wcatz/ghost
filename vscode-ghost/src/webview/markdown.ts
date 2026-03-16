// Markdown rendering using the marked library.
// This runs in the webview (browser context).

import { marked } from "marked";

// Configure marked
marked.setOptions({
  breaks: true,
  gfm: true,
  async: false,
});

// Custom renderer for code blocks with copy button
const renderer = new marked.Renderer();
renderer.code = function ({ text, lang }: { text: string; lang?: string }) {
  const id = "cb-" + Math.random().toString(36).substring(2, 8);
  const langLabel = lang
    ? `<span class="code-lang">${escapeHtml(lang)}</span>`
    : "";
  return `<div class="code-block">
    <div class="code-header">${langLabel}<button class="copy-btn" data-target="${id}" aria-label="Copy code">Copy</button></div>
    <pre><code id="${id}">${escapeHtml(text)}</code></pre>
  </div>`;
};

marked.use({ renderer });

export function renderMarkdown(text: string): string {
  if (!text) return "";
  return marked.parse(text, { async: false }) as string;
}

export function escapeHtml(text: string): string {
  const div = document.createElement("div");
  div.textContent = text;
  return div.innerHTML;
}

// Set up copy button handler via event delegation
document.addEventListener("click", (e) => {
  const btn = (e.target as HTMLElement).closest(".copy-btn") as HTMLElement | null;
  if (!btn) return;
  const targetId = btn.dataset.target;
  if (!targetId) return;
  const codeEl = document.getElementById(targetId);
  if (!codeEl) return;
  navigator.clipboard.writeText(codeEl.textContent ?? "").then(() => {
    btn.textContent = "Copied!";
    setTimeout(() => { btn.textContent = "Copy"; }, 2000);
  });
});
