import * as vscode from "vscode";

/**
 * Generates the chat webview HTML used by both sidebar and editor panels.
 * Contains markdown rendering, cost tracking, slash commands, thinking display,
 * auto-approve toggle, and image attachment support.
 */
export function getChatHtml(
  webview: vscode.Webview,
  extensionUri: vscode.Uri
): string {
  const nonce = getNonce();
  const styleUri = webview.asWebviewUri(
    vscode.Uri.joinPath(extensionUri, "media", "chat.css")
  );

  return `<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <meta http-equiv="Content-Security-Policy"
    content="default-src 'none'; style-src ${webview.cspSource} 'unsafe-inline'; script-src 'nonce-${nonce}'; img-src data: ${webview.cspSource};">
  <link rel="stylesheet" href="${styleUri}">
  <title>Ghost Chat</title>
</head>
<body>
  <div id="header">
    <div id="header-left">
      <span id="connection-dot"></span>
      <span id="session-info"></span>
      <span id="mode-badge"></span>
    </div>
    <div id="header-right">
      <button id="auto-approve-btn" class="header-btn" title="Auto-approve OFF"><svg class="ghost-toggle" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 2C7 2 3 6 3 11v7c0 1 1 2 2 2h1c1 0 1-1 1-2v-1c0-1 1-1 1 0v1c0 1 0 2 1 2h1c1 0 1-1 1-2v-1c0-1 1-1 1 0v1c0 1 0 2 1 2h1c1 0 1-1 1-2v-1c0-1 1-1 1 0v1c0 1 0 2 1 2h1c1 0 2-1 2-2v-7c0-5-4-9-9-9z"/><circle cx="9" cy="10" r="1.5" fill="currentColor"/><circle cx="15" cy="10" r="1.5" fill="currentColor"/></svg></button>
      <span id="session-cost"></span>
    </div>
  </div>
  <div id="messages"></div>
  <div id="slash-menu" class="hidden"></div>
  <div id="approval-overlay" class="hidden" tabindex="0">
    <div id="approval-modal">
      <div class="modal-header">Tool Approval Required</div>
      <div id="approval-tool" class="modal-tool"></div>
      <div id="approval-input-preview" class="modal-preview"></div>
      <div class="modal-actions">
        <button id="approve-btn" class="modal-btn approve">Allow <kbd>y</kbd></button>
        <button id="deny-btn" class="modal-btn deny">Deny <kbd>n</kbd></button>
      </div>
      <div class="modal-instructions">
        <input id="deny-instructions" type="text" placeholder="Deny with instructions... (optional)" />
        <button id="deny-with-btn" class="modal-btn deny-with">Deny with feedback</button>
      </div>
    </div>
  </div>
  <div id="image-preview" class="hidden">
    <img id="preview-img" />
    <button id="remove-image">&times;</button>
  </div>
  <div id="input-area">
    <textarea id="input" placeholder="Message Ghost... (/ for commands)" rows="1"></textarea>
    <div id="input-actions">
      <button id="attach-btn" class="icon-btn" title="Attach image">&#x1f4ce;</button>
      <button id="send-btn" class="icon-btn" title="Send (Enter)">&#x27A4;</button>
      <button id="abort-btn" class="icon-btn hidden" title="Stop">&#x25A0;</button>
    </div>
  </div>
  <div id="footer">
    <span id="footer-cost"></span>
    <span id="footer-savings"></span>
  </div>
  <script nonce="${nonce}">
    const vscode = acquireVsCodeApi();
    const messagesEl = document.getElementById('messages');
    const inputEl = document.getElementById('input');
    const sendBtn = document.getElementById('send-btn');
    const abortBtn = document.getElementById('abort-btn');
    const attachBtn = document.getElementById('attach-btn');
    const approvalOverlay = document.getElementById('approval-overlay');
    const approvalTool = document.getElementById('approval-tool');
    const approvalPreview = document.getElementById('approval-input-preview');
    const approveBtn = document.getElementById('approve-btn');
    const denyBtn = document.getElementById('deny-btn');
    const denyInstructions = document.getElementById('deny-instructions');
    const denyWithBtn = document.getElementById('deny-with-btn');
    const connectionDot = document.getElementById('connection-dot');
    const sessionInfo = document.getElementById('session-info');
    const modeBadge = document.getElementById('mode-badge');
    const autoApproveBtn = document.getElementById('auto-approve-btn');
    const slashMenu = document.getElementById('slash-menu');
    const imagePreview = document.getElementById('image-preview');
    const previewImg = document.getElementById('preview-img');
    const removeImageBtn = document.getElementById('remove-image');
    const footerCost = document.getElementById('footer-cost');
    const footerSavings = document.getElementById('footer-savings');

    let currentAssistantEl = null;
    let currentAssistantText = '';
    let currentThinkingEl = null;
    let currentThinkingText = '';
    let streaming = false;
    let autoApprove = false;
    let pendingImage = null;
    let toolTimers = {};

    // --- Cost tracking ---
    let sessionTotals = { input: 0, output: 0, cacheCreate: 0, cacheRead: 0 };

    function estimateCost(usage) {
      const i = (usage.input_tokens || 0) * 3.0 / 1e6;
      const o = (usage.output_tokens || 0) * 15.0 / 1e6;
      const cw = (usage.cache_creation_input_tokens || 0) * 3.75 / 1e6;
      const cr = (usage.cache_read_input_tokens || 0) * 0.30 / 1e6;
      return i + o + cw + cr;
    }

    function cacheSavings(usage) {
      return (usage.cache_read_input_tokens || 0) * (3.0 - 0.30) / 1e6;
    }

    function formatUSD(n) {
      if (n < 0.01) return '$' + n.toFixed(4);
      return '$' + n.toFixed(2);
    }

    function formatTokens(n) {
      if (n < 1000) return '' + n;
      if (n < 10000) return (n / 1000).toFixed(1) + 'k';
      return Math.round(n / 1000) + 'k';
    }

    function updateFooter() {
      const total = estimateCost({
        input_tokens: sessionTotals.input,
        output_tokens: sessionTotals.output,
        cache_creation_input_tokens: sessionTotals.cacheCreate,
        cache_read_input_tokens: sessionTotals.cacheRead
      });
      const saved = sessionTotals.cacheRead * (3.0 - 0.30) / 1e6;
      footerCost.textContent = total > 0 ? formatUSD(total) : '';
      if (saved > 0) {
        const pct = Math.round(sessionTotals.cacheRead / (sessionTotals.input + sessionTotals.cacheRead + 1) * 100);
        footerSavings.textContent = 'saved ' + formatUSD(saved) + ' (' + pct + '% cache)';
      } else {
        footerSavings.textContent = '';
      }
    }

    // --- Markdown rendering ---
    function renderMarkdown(text) {
      let html = escapeHtml(text);
      // Code blocks
      html = html.replace(/\`\`\`(\\w*)?\\n([\\s\\S]*?)\`\`\`/g, (_, lang, code) => {
        const id = 'cb-' + Math.random().toString(36).substr(2, 6);
        return '<div class="code-block"><div class="code-header"><span class="code-lang">' +
          (lang || '') + '</span><button class="copy-btn" data-target="' + id +
          '">Copy</button></div><pre><code id="' + id + '">' + code.trim() + '</code></pre></div>';
      });
      // Inline code
      html = html.replace(/\`([^\`]+)\`/g, '<code class="inline-code">$1</code>');
      // Bold
      html = html.replace(/\\*\\*(.+?)\\*\\*/g, '<strong>$1</strong>');
      // Italic
      html = html.replace(/(?<![*])\\*(?![*])(.+?)(?<![*])\\*(?![*])/g, '<em>$1</em>');
      // Links
      html = html.replace(/\\[([^\\]]+)\\]\\(([^)]+)\\)/g, '<a href="$2">$1</a>');
      // Line breaks
      html = html.replace(/\\n/g, '<br>');
      return html;
    }

    function escapeHtml(text) {
      const d = document.createElement('div');
      d.textContent = text;
      return d.innerHTML;
    }

    let userScrolledUp = false;

    messagesEl.addEventListener('scroll', () => {
      const atBottom = messagesEl.scrollHeight - messagesEl.scrollTop - messagesEl.clientHeight < 80;
      userScrolledUp = !atBottom;
    });

    function scrollToBottom(force) {
      if (!force && userScrolledUp) return;
      messagesEl.scrollTo({ top: messagesEl.scrollHeight, behavior: 'smooth' });
    }

    // --- Message rendering ---
    function addMessage(role, text) {
      const div = document.createElement('div');
      div.className = 'message ' + role;
      if (role === 'user') {
        div.textContent = text;
      } else {
        div.innerHTML = renderMarkdown(text);
      }
      messagesEl.appendChild(div);
      scrollToBottom();
      return div;
    }

    function ensureAssistantBubble() {
      if (!currentAssistantEl) {
        currentAssistantEl = document.createElement('div');
        currentAssistantEl.className = 'message assistant';
        messagesEl.appendChild(currentAssistantEl);
      }
      return currentAssistantEl;
    }

    function addToolIndicator(id, name, status) {
      const el = document.createElement('details');
      el.className = 'tool-indicator ' + status;
      el.dataset.toolName = name;
      el.dataset.toolId = id;
      const icon = status === 'running' ? '<span class="spinner"></span>' : '<span class="check">&#10003;</span>';
      el.innerHTML = '<summary>' + icon + ' <span class="tool-name">' + escapeHtml(name) + '</span><span class="tool-time"></span></summary><pre class="tool-output"></pre>';
      messagesEl.appendChild(el);
      if (status === 'running') {
        toolTimers[id] = Date.now();
      }
      scrollToBottom();
      return el;
    }

    // --- Slash commands ---
    const slashCommands = [
      { cmd: '/mode code', desc: 'Code writing mode' },
      { cmd: '/mode chat', desc: 'Conversational mode' },
      { cmd: '/mode debug', desc: 'Debugging mode' },
      { cmd: '/mode review', desc: 'Code review mode' },
      { cmd: '/mode plan', desc: 'Architecture planning' },
      { cmd: '/mode refactor', desc: 'Refactoring mode' },
      { cmd: '/clear', desc: 'Clear conversation' },
      { cmd: '/cost', desc: 'Show session cost' },
      { cmd: '/auto-approve', desc: 'Toggle auto-approve' },
    ];
    let slashSelected = 0;

    function showSlashMenu(query) {
      const q = query.toLowerCase();
      const filtered = slashCommands.filter(c =>
        c.cmd.toLowerCase().includes(q) || c.desc.toLowerCase().includes(q)
      );
      if (filtered.length === 0) {
        slashMenu.classList.add('hidden');
        return;
      }
      slashSelected = Math.min(slashSelected, filtered.length - 1);
      slashMenu.innerHTML = filtered.map((c, i) =>
        '<div class="slash-item' + (i === slashSelected ? ' selected' : '') + '" data-cmd="' +
        escapeHtml(c.cmd) + '"><span class="slash-cmd">' + escapeHtml(c.cmd) +
        '</span><span class="slash-desc">' + escapeHtml(c.desc) + '</span></div>'
      ).join('');
      slashMenu.classList.remove('hidden');

      slashMenu.querySelectorAll('.slash-item').forEach(el => {
        el.addEventListener('click', () => {
          executeSlashCommand(el.dataset.cmd);
          slashMenu.classList.add('hidden');
          inputEl.value = '';
          inputEl.focus();
        });
      });
    }

    function executeSlashCommand(cmd) {
      if (cmd.startsWith('/mode ')) {
        const mode = cmd.split(' ')[1];
        vscode.postMessage({ type: 'setMode', mode });
      } else if (cmd === '/clear') {
        messagesEl.innerHTML = '';
        currentAssistantEl = null;
        currentThinkingEl = null;
        currentAssistantText = '';
        currentThinkingText = '';
        sessionTotals = { input: 0, output: 0, cacheCreate: 0, cacheRead: 0 };
        updateFooter();
      } else if (cmd === '/cost') {
        const total = estimateCost({
          input_tokens: sessionTotals.input, output_tokens: sessionTotals.output,
          cache_creation_input_tokens: sessionTotals.cacheCreate,
          cache_read_input_tokens: sessionTotals.cacheRead
        });
        const saved = sessionTotals.cacheRead * 2.70 / 1e6;
        addMessage('system', 'Session cost: ' + formatUSD(total) +
          ' | in:' + formatTokens(sessionTotals.input) +
          ' out:' + formatTokens(sessionTotals.output) +
          (sessionTotals.cacheRead > 0 ? ' | Cache saved: ' + formatUSD(saved) : ''));
      } else if (cmd === '/auto-approve') {
        autoApprove = !autoApprove;
        autoApproveBtn.innerHTML = autoApprove ? '&#x1f513;' : '&#x1f512;';
        autoApproveBtn.title = autoApprove ? 'Auto-approve ON' : 'Auto-approve OFF';
        vscode.postMessage({ type: 'set_auto_approve', enabled: autoApprove });
        addMessage('system', 'Auto-approve ' + (autoApprove ? 'enabled' : 'disabled'));
      }
    }

    // --- Send ---
    let messageQueue = [];

    function send() {
      const text = inputEl.value.trim();
      if (!text) return;

      // Handle slash commands locally (even during streaming).
      if (text.startsWith('/')) {
        const match = slashCommands.find(c => c.cmd === text || text.startsWith(c.cmd));
        if (match) {
          executeSlashCommand(match.cmd);
          inputEl.value = '';
          inputEl.style.height = 'auto';
          slashMenu.classList.add('hidden');
          return;
        }
      }

      const msg = { type: 'send', text };
      if (pendingImage) {
        msg.image = pendingImage;
        pendingImage = null;
        imagePreview.classList.add('hidden');
      }

      if (streaming) {
        // Queue for after current response completes.
        messageQueue.push(msg);
        addMessage('queued', '⏳ Queued: ' + text);
      } else {
        vscode.postMessage(msg);
      }
      inputEl.value = '';
      inputEl.style.height = 'auto';
      slashMenu.classList.add('hidden');
    }

    function drainQueue() {
      if (messageQueue.length > 0 && !streaming) {
        const next = messageQueue.shift();
        vscode.postMessage(next);
      }
    }

    sendBtn.addEventListener('click', send);
    inputEl.addEventListener('keydown', (e) => {
      if (slashMenu.classList.contains('hidden') === false) {
        const items = slashMenu.querySelectorAll('.slash-item');
        if (e.key === 'ArrowDown') {
          e.preventDefault();
          slashSelected = Math.min(slashSelected + 1, items.length - 1);
          showSlashMenu(inputEl.value);
          return;
        }
        if (e.key === 'ArrowUp') {
          e.preventDefault();
          slashSelected = Math.max(slashSelected - 1, 0);
          showSlashMenu(inputEl.value);
          return;
        }
        if (e.key === 'Enter') {
          e.preventDefault();
          if (items[slashSelected]) {
            executeSlashCommand(items[slashSelected].dataset.cmd);
            inputEl.value = '';
            slashMenu.classList.add('hidden');
          }
          return;
        }
        if (e.key === 'Escape') {
          slashMenu.classList.add('hidden');
          return;
        }
      }
      if (e.key === 'Escape' && streaming) {
        e.preventDefault();
        vscode.postMessage({ type: 'abort' });
        return;
      }
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        send();
      }
    });

    inputEl.addEventListener('input', () => {
      inputEl.style.height = 'auto';
      inputEl.style.height = Math.min(inputEl.scrollHeight, 150) + 'px';
      const val = inputEl.value;
      if (val.startsWith('/')) {
        slashSelected = 0;
        showSlashMenu(val);
      } else {
        slashMenu.classList.add('hidden');
      }
    });

    // --- Abort ---
    abortBtn.addEventListener('click', () => vscode.postMessage({ type: 'abort' }));

    // --- Auto-approve toggle ---
    autoApproveBtn.addEventListener('click', () => {
      autoApprove = !autoApprove;
      autoApproveBtn.classList.toggle('yolo', autoApprove);
      autoApproveBtn.title = autoApprove ? 'Auto-approve ON (YOLO)' : 'Auto-approve OFF';
      vscode.postMessage({ type: 'set_auto_approve', enabled: autoApprove });
    });

    // --- Approval ---
    function closeApproval() {
      approvalOverlay.classList.add('hidden');
      denyInstructions.value = '';
    }
    approveBtn.addEventListener('click', () => {
      vscode.postMessage({ type: 'approve', approved: true });
      closeApproval();
    });
    denyBtn.addEventListener('click', () => {
      vscode.postMessage({ type: 'approve', approved: false });
      closeApproval();
    });
    denyWithBtn.addEventListener('click', () => {
      const instructions = denyInstructions.value.trim();
      vscode.postMessage({ type: 'approve', approved: false, instructions: instructions || undefined });
      closeApproval();
    });
    denyInstructions.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        denyWithBtn.click();
      }
    });
    // Keyboard shortcuts on overlay
    approvalOverlay.addEventListener('keydown', (e) => {
      if (e.target === denyInstructions) return;
      if (e.key === 'y') { approveBtn.click(); e.preventDefault(); }
      if (e.key === 'n') { denyBtn.click(); e.preventDefault(); }
    });

    // --- Image attach ---
    attachBtn.addEventListener('click', () => vscode.postMessage({ type: 'attach_image' }));
    removeImageBtn.addEventListener('click', () => {
      pendingImage = null;
      imagePreview.classList.add('hidden');
    });

    // Paste image from clipboard
    document.addEventListener('paste', (e) => {
      const items = e.clipboardData?.items;
      if (!items) return;
      for (const item of items) {
        if (item.type.startsWith('image/')) {
          e.preventDefault();
          const file = item.getAsFile();
          if (!file) continue;
          const reader = new FileReader();
          reader.onload = () => {
            const base64 = reader.result.split(',')[1];
            pendingImage = { media_type: file.type, data: base64 };
            previewImg.src = reader.result;
            imagePreview.classList.remove('hidden');
          };
          reader.readAsDataURL(file);
          break;
        }
      }
    });

    // Drag-drop
    document.body.addEventListener('dragover', (e) => { e.preventDefault(); document.body.classList.add('drag-over'); });
    document.body.addEventListener('dragleave', () => document.body.classList.remove('drag-over'));
    document.body.addEventListener('drop', (e) => {
      e.preventDefault();
      document.body.classList.remove('drag-over');
      const file = e.dataTransfer.files[0];
      if (file && file.type.startsWith('image/')) {
        const reader = new FileReader();
        reader.onload = () => {
          const base64 = reader.result.split(',')[1];
          pendingImage = { media_type: file.type, data: base64 };
          previewImg.src = reader.result;
          imagePreview.classList.remove('hidden');
        };
        reader.readAsDataURL(file);
      }
    });

    // Copy buttons (delegated)
    document.addEventListener('click', (e) => {
      if (e.target.classList.contains('copy-btn')) {
        const target = document.getElementById(e.target.dataset.target);
        if (target) {
          navigator.clipboard.writeText(target.textContent);
          e.target.textContent = 'Copied!';
          setTimeout(() => { e.target.textContent = 'Copy'; }, 1500);
        }
      }
    });

    // --- Messages from extension ---
    window.addEventListener('message', (event) => {
      const msg = event.data;
      switch (msg.type) {
        case 'status':
          connectionDot.className = msg.connected ? 'dot connected' : 'dot disconnected';
          break;

        case 'session':
          sessionInfo.textContent = msg.session.project_name;
          modeBadge.textContent = msg.session.mode;
          modeBadge.className = 'mode-badge mode-' + msg.session.mode;
          break;

        case 'user_message':
          addMessage('user', msg.text);
          currentAssistantEl = null;
          currentAssistantText = '';
          currentThinkingEl = null;
          currentThinkingText = '';
          break;

        case 'text_delta':
          currentAssistantText += msg.text;
          const el = ensureAssistantBubble();
          el.innerHTML = renderMarkdown(currentAssistantText);
          scrollToBottom();
          break;

        case 'thinking_delta':
          currentThinkingText += msg.text;
          if (!currentThinkingEl) {
            const details = document.createElement('details');
            details.className = 'thinking-block';
            details.innerHTML = '<summary>Thinking...</summary><div class="thinking-content"></div>';
            messagesEl.appendChild(details);
            currentThinkingEl = details.querySelector('.thinking-content');
          }
          currentThinkingEl.textContent = currentThinkingText;
          scrollToBottom();
          break;

        case 'tool_start':
          addToolIndicator(msg.id, msg.name, 'running');
          break;

        case 'tool_delta': {
          const tools = messagesEl.querySelectorAll('.tool-indicator');
          for (let i = tools.length - 1; i >= 0; i--) {
            if (tools[i].dataset.toolId === msg.id) {
              const out = tools[i].querySelector('.tool-output');
              if (out) out.textContent += msg.delta;
              break;
            }
          }
          break;
        }

        case 'tool_end': {
          const indicators = messagesEl.querySelectorAll('.tool-indicator.running');
          indicators.forEach(ind => {
            if (ind.dataset.toolId === msg.id) {
              ind.className = 'tool-indicator done';
              const s = ind.querySelector('summary .spinner, summary .check');
              if (s) s.outerHTML = '<span class="check">&#10003;</span>';
              const elapsed = toolTimers[msg.id] ? ((Date.now() - toolTimers[msg.id]) / 1000).toFixed(1) + 's' : '';
              const timeEl = ind.querySelector('summary .tool-time');
              if (timeEl && elapsed) timeEl.textContent = ' ' + elapsed;
              delete toolTimers[msg.id];
            }
          });
          break;
        }

        case 'tool_diff': {
          if (msg.diff) {
            const diffEl = document.createElement('div');
            diffEl.className = 'diff-block';
            const header = document.createElement('div');
            header.className = 'diff-header';
            header.textContent = msg.path || msg.name || 'file edit';
            diffEl.appendChild(header);
            const pre = document.createElement('pre');
            pre.className = 'diff-content';
            const lines = msg.diff.split('\\n');
            for (const line of lines) {
              const span = document.createElement('span');
              if (line.startsWith('+ ')) {
                span.className = 'diff-add';
              } else if (line.startsWith('- ')) {
                span.className = 'diff-del';
              } else if (line.startsWith('---') || line.startsWith('+++')) {
                span.className = 'diff-meta';
              }
              span.textContent = line;
              pre.appendChild(span);
              pre.appendChild(document.createTextNode('\\n'));
            }
            diffEl.appendChild(pre);
            messagesEl.appendChild(diffEl);
            scrollToBottom();
          }
          break;
        }

        case 'approval_required':
          if (autoApprove) {
            vscode.postMessage({ type: 'approve', approved: true });
          } else {
            approvalTool.textContent = msg.tool_name;
            // Show input preview
            let preview = '';
            try {
              const inp = typeof msg.input === 'string' ? JSON.parse(msg.input) : msg.input;
              if (inp.command) preview = inp.command;
              else if (inp.path) preview = inp.path;
              else if (inp.content) preview = inp.content.substring(0, 200);
              else preview = JSON.stringify(inp, null, 2).substring(0, 300);
            } catch(e) { preview = ''; }
            approvalPreview.textContent = preview;
            approvalOverlay.classList.remove('hidden');
            approvalOverlay.focus();
            scrollToBottom(true);
          }
          break;

        case 'approval_resolved':
          // External approval (e.g. via Telegram) — dismiss the overlay.
          approvalOverlay.classList.add('hidden');
          break;

        case 'done': {
          currentAssistantEl = null;
          currentAssistantText = '';
          currentThinkingEl = null;
          currentThinkingText = '';
          if (msg.usage) {
            const u = msg.usage;
            sessionTotals.input += (u.input_tokens || 0);
            sessionTotals.output += (u.output_tokens || 0);
            sessionTotals.cacheCreate += (u.cache_creation_input_tokens || 0);
            sessionTotals.cacheRead += (u.cache_read_input_tokens || 0);

            const cost = estimateCost(u);
            const saved = cacheSavings(u);
            const info = document.createElement('div');
            info.className = 'usage-info';
            let text = formatUSD(cost) + ' | in:' + formatTokens(u.input_tokens || 0) +
              ' out:' + formatTokens(u.output_tokens || 0);
            if (u.cache_read_input_tokens > 0) {
              text += ' | cached:' + formatTokens(u.cache_read_input_tokens) +
                ' (saved ' + formatUSD(saved) + ')';
            }
            info.textContent = text;
            messagesEl.appendChild(info);
            scrollToBottom();
            updateFooter();
          }
          break;
        }

        case 'streaming':
          streaming = msg.active;
          sendBtn.classList.toggle('hidden', msg.active);
          abortBtn.classList.toggle('hidden', !msg.active);
          if (msg.active) {
            attachBtn.classList.add('hidden');
          } else {
            attachBtn.classList.remove('hidden');
            drainQueue();
          }
          break;

        case 'mode_changed':
          modeBadge.textContent = msg.mode;
          modeBadge.className = 'mode-badge mode-' + msg.mode;
          break;

        case 'image_data':
          pendingImage = msg.image;
          previewImg.src = 'data:' + msg.image.media_type + ';base64,' + msg.image.data;
          imagePreview.classList.remove('hidden');
          break;

        case 'error':
          addMessage('error', msg.text);
          break;

        case 'send_from_command':
          inputEl.value = msg.text;
          send();
          break;
      }
    });

    vscode.postMessage({ type: 'ready' });
  </script>
</body>
</html>`;
}

export function getNonce(): string {
  let text = "";
  const chars =
    "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789";
  for (let i = 0; i < 32; i++) {
    text += chars.charAt(Math.floor(Math.random() * chars.length));
  }
  return text;
}
