// Markdown rendering using the marked library.
// This runs in the webview (browser context).

import { marked, type Tokens } from "marked";

// Configure marked for GFM with line breaks
marked.setOptions({
  breaks: true,
  gfm: true,
  async: false,
});

// Custom renderer for code blocks with copy button and syntax hints
const renderer = new marked.Renderer();

renderer.code = function (token: Tokens.Code): string {
  const lang = token.lang || "";
  const text = token.text;
  const id = "cb-" + Math.random().toString(36).substring(2, 8);
  const langLabel = lang
    ? `<span class="code-lang">${escapeHtml(lang)}</span>`
    : "";

  // Apply git diff coloring if the language is diff or content looks like a diff
  const isDiff = lang === "diff" || (lang === "" && looksLikeDiff(text));
  const codeHtml = isDiff ? renderDiffLines(text) : escapeHtml(text);

  return `<div class="code-block">
    <div class="code-header">${langLabel}<button class="copy-btn" data-target="${id}" aria-label="Copy code">Copy</button></div>
    <pre><code id="${id}" class="${lang ? "language-" + escapeHtml(lang) : ""}">${codeHtml}</code></pre>
  </div>`;
};

renderer.codespan = function (token: Tokens.Codespan): string {
  return `<code class="inline-code">${escapeHtml(token.text)}</code>`;
};

marked.use({ renderer });

/**
 * Render markdown text to HTML.
 */
export function renderMarkdown(text: string): string {
  if (!text) return "";
  return marked.parse(text, { async: false }) as string;
}

/**
 * Render tool output with diff detection.
 * If the output looks like a git diff, apply coloring.
 */
export function renderToolOutput(output: string): string {
  if (!output) return "";
  if (looksLikeDiff(output)) {
    return `<pre class="tool-output">${renderDiffLines(output)}</pre>`;
  }
  return `<pre class="tool-output">${escapeHtml(output)}</pre>`;
}

function looksLikeDiff(text: string): boolean {
  const lines = text.split("\n").slice(0, 10);
  let diffMarkers = 0;
  for (const line of lines) {
    if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("@@") ||
        line.startsWith("+") || line.startsWith("-")) {
      diffMarkers++;
    }
  }
  return diffMarkers >= 3;
}

function renderDiffLines(text: string): string {
  return text.split("\n").map((line) => {
    const escaped = escapeHtml(line);
    if (line.startsWith("+++") || line.startsWith("---") || line.startsWith("@@")) {
      return `<span class="diff-meta">${escaped}</span>`;
    }
    if (line.startsWith("+")) {
      return `<span class="diff-add">${escaped}</span>`;
    }
    if (line.startsWith("-")) {
      return `<span class="diff-del">${escaped}</span>`;
    }
    return escaped;
  }).join("\n");
}

export function escapeHtml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
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
