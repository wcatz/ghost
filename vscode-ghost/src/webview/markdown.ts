// Markdown rendering using the marked library + highlight.js syntax highlighting.
// This runs in the webview (browser context).

import { marked, type Tokens } from "marked";
import hljs from "highlight.js/lib/core";

// Register only the languages we need to keep the bundle small.
import typescript from "highlight.js/lib/languages/typescript";
import javascript from "highlight.js/lib/languages/javascript";
import go from "highlight.js/lib/languages/go";
import python from "highlight.js/lib/languages/python";
import bash from "highlight.js/lib/languages/bash";
import json from "highlight.js/lib/languages/json";
import yaml from "highlight.js/lib/languages/yaml";
import xml from "highlight.js/lib/languages/xml";
import css from "highlight.js/lib/languages/css";
import sql from "highlight.js/lib/languages/sql";
import rust from "highlight.js/lib/languages/rust";
import diff from "highlight.js/lib/languages/diff";
import markdown from "highlight.js/lib/languages/markdown";
import dockerfile from "highlight.js/lib/languages/dockerfile";
import c from "highlight.js/lib/languages/c";
import cpp from "highlight.js/lib/languages/cpp";
import java from "highlight.js/lib/languages/java";
import ruby from "highlight.js/lib/languages/ruby";
import php from "highlight.js/lib/languages/php";
import swift from "highlight.js/lib/languages/swift";
import kotlin from "highlight.js/lib/languages/kotlin";
import lua from "highlight.js/lib/languages/lua";
import shell from "highlight.js/lib/languages/shell";
import makefile from "highlight.js/lib/languages/makefile";
import ini from "highlight.js/lib/languages/ini";
import nginx from "highlight.js/lib/languages/nginx";

hljs.registerLanguage("typescript", typescript);
hljs.registerLanguage("javascript", javascript);
hljs.registerLanguage("go", go);
hljs.registerLanguage("python", python);
hljs.registerLanguage("bash", bash);
hljs.registerLanguage("json", json);
hljs.registerLanguage("yaml", yaml);
hljs.registerLanguage("xml", xml);
hljs.registerLanguage("html", xml);
hljs.registerLanguage("css", css);
hljs.registerLanguage("sql", sql);
hljs.registerLanguage("rust", rust);
hljs.registerLanguage("diff", diff);
hljs.registerLanguage("markdown", markdown);
hljs.registerLanguage("dockerfile", dockerfile);
hljs.registerLanguage("c", c);
hljs.registerLanguage("cpp", cpp);
hljs.registerLanguage("java", java);
hljs.registerLanguage("ruby", ruby);
hljs.registerLanguage("php", php);
hljs.registerLanguage("swift", swift);
hljs.registerLanguage("kotlin", kotlin);
hljs.registerLanguage("lua", lua);
hljs.registerLanguage("shell", shell);
hljs.registerLanguage("makefile", makefile);
hljs.registerLanguage("ini", ini);
hljs.registerLanguage("toml", ini);
hljs.registerLanguage("nginx", nginx);

// Aliases
hljs.registerLanguage("ts", typescript);
hljs.registerLanguage("js", javascript);
hljs.registerLanguage("tsx", typescript);
hljs.registerLanguage("jsx", javascript);
hljs.registerLanguage("py", python);
hljs.registerLanguage("sh", bash);
hljs.registerLanguage("zsh", bash);
hljs.registerLanguage("yml", yaml);
hljs.registerLanguage("rb", ruby);
hljs.registerLanguage("rs", rust);

// --- Marked configuration ---

marked.setOptions({
  breaks: true,
  gfm: true,
  async: false,
});

const renderer = new marked.Renderer();

renderer.code = function (token: Tokens.Code): string {
  const lang = token.lang || "";
  const text = token.text;
  const id = "cb-" + Math.random().toString(36).substring(2, 8);
  const langLabel = lang
    ? `<span class="code-lang">${escapeHtml(lang)}</span>`
    : "";

  // Diff blocks get structured rendering.
  const isDiff = lang === "diff" || (lang === "" && looksLikeDiff(text));
  if (isDiff) {
    return renderDiff(text);
  }

  // Syntax highlight with highlight.js.
  let codeHtml: string;
  if (lang && hljs.getLanguage(lang)) {
    codeHtml = hljs.highlight(text, { language: lang, ignoreIllegals: true }).value;
  } else if (!lang) {
    // Auto-detect for unlabeled blocks
    const result = hljs.highlightAuto(text);
    codeHtml = result.value;
  } else {
    codeHtml = escapeHtml(text);
  }

  // Add line numbers for blocks > 5 lines.
  const lines = codeHtml.split("\n");
  if (lines.length > 5) {
    codeHtml = lines
      .map((line, i) => `<span class="code-line"><span class="code-line-num">${i + 1}</span>${line}</span>`)
      .join("\n");
  }

  return `<div class="code-block">
    <div class="code-header">${langLabel}<button class="copy-btn" data-target="${id}" aria-label="Copy code">Copy</button></div>
    <pre><code id="${id}" class="hljs${lang ? " language-" + escapeHtml(lang) : ""}">${codeHtml}</code></pre>
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
 */
export function renderToolOutput(output: string): string {
  if (!output) return "";
  if (looksLikeDiff(output)) {
    return renderDiff(output);
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

// --- Enhanced diff renderer with line numbers ---

function renderDiff(text: string): string {
  const lines = text.split("\n");
  let html = '<div class="diff-container">';
  let oldLine = 0;
  let newLine = 0;

  for (const line of lines) {
    if (line.startsWith("--- ") || line.startsWith("+++ ")) {
      html += `<div class="diff-file-header">${escapeHtml(line)}</div>`;
    } else if (line.startsWith("@@")) {
      const match = line.match(/@@ -(\d+)(?:,\d+)? \+(\d+)/);
      if (match) {
        oldLine = parseInt(match[1]);
        newLine = parseInt(match[2]);
      }
      html += `<div class="diff-hunk-header">${escapeHtml(line)}</div>`;
    } else if (line.startsWith("+")) {
      html += `<div class="diff-line add"><span class="diff-line-num"></span><span class="diff-line-num">${newLine}</span><span class="diff-line-content">${escapeHtml(line.slice(1))}</span></div>`;
      newLine++;
    } else if (line.startsWith("-")) {
      html += `<div class="diff-line del"><span class="diff-line-num">${oldLine}</span><span class="diff-line-num"></span><span class="diff-line-content">${escapeHtml(line.slice(1))}</span></div>`;
      oldLine++;
    } else {
      const content = line.startsWith(" ") ? line.slice(1) : line;
      html += `<div class="diff-line context"><span class="diff-line-num">${oldLine || ""}</span><span class="diff-line-num">${newLine || ""}</span><span class="diff-line-content">${escapeHtml(content)}</span></div>`;
      if (oldLine) oldLine++;
      if (newLine) newLine++;
    }
  }
  html += "</div>";
  return html;
}

export function escapeHtml(text: string): string {
  return text
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

// Copy button handler via event delegation
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
