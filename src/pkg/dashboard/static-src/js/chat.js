    // --- Chat rich formatting ---
    function sanitizeChatText(raw) {
      return String(raw || '')
        .replace(/^\s*← open sidebar · \/ commands · \? help · tab next tab\s*$/gmi, '')
        .replace(/(?:^|\n)(?:Status heartbeat\b[\s\S]*?)(?=\n\n|$)/gi, function(block) {
          return block.split('\n').slice(0, 4).join('\n') + (block.split('\n').length > 4 ? '\n… heartbeat details folded' : '');
        })
        .trim();
    }

    function formatChatHtml(raw) {
      const cleaned = sanitizeChatText(raw);
      const lines = cleaned.split('\n');
      const out = [];
      const URL_RE = /(https?:\/\/[^\s<>"')\]]+)/g;
      const BOLD_RE = /\*\*(.+?)\*\*/g;
      const CODE_RE = /`([^`]+)`/g;
      const MENTION_RE = /(^|[\s(])@([A-Za-z0-9][A-Za-z0-9_-]{0,38})\b/g;
      const OWNER_REPO_REF_RE = /\b([A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+)#(\d+)\b/g;
      const ISSUE_REF_RE = /(^|[\s(])#(\d+)\b/g;
      const BULLET_RE = /^[-•–]\s+(.*)$/;
      const TOOL_RE = /^[✕×X✗]\s+(.+?)\s*\((shell|Read|Edit|Write|Bash|WebSearch|WebFetch|Agent)\)\s*$/;
      let inFence = false;
      let fenceLang = '';
      let fence = [];

      function inlineFmt(text) {
        let s = escapeHtml(text || '');
        const links = [];
        s = s.replace(URL_RE, (url) => {
          const idx = links.length;
          links.push('<a href="' + url + '" target="_blank" rel="noopener noreferrer" style="color:var(--cyan)">' + url + '</a>');
          return '\u0000LINK' + idx + '\u0000';
        });
        s = s.replace(BOLD_RE, '<strong>$1</strong>');
        s = s.replace(CODE_RE, '<code>$1</code>');
        s = s.replace(OWNER_REPO_REF_RE, '<a class="chat-ref" href="https://github.com/$1/issues/$2" target="_blank" rel="noopener noreferrer" title="Open $1#$2">$1#$2</a>');
        s = s.replace(ISSUE_REF_RE, '$1<a class="chat-ref" href="https://github.com/hivecommons/hive/issues/$2" target="_blank" rel="noopener noreferrer" title="Preview issue/PR #$2">#$2</a>');
        s = s.replace(MENTION_RE, '$1<a class="chat-mention" href="https://github.com/$2" target="_blank" rel="noopener noreferrer" title="Mention @$2">@$2</a>');
        s = s.replace(/\u0000LINK(\d+)\u0000/g, (_, i) => links[Number(i)] || '');
        return s;
      }
      function flushFence() {
        const rawCode = sanitizeChatText(fence.join('\n'));
        if (!rawCode.trim()) {
          fence = [];
          fenceLang = '';
          return;
        }
        const code = escapeHtml(rawCode);
        const lang = fenceLang ? '<div class="hive-chat-kbd">' + escapeHtml(fenceLang) + '</div>' : '';
        out.push('<div class="chat-code-wrap"><button type="button" class="chat-copy-btn">copy</button><pre>' + lang + '<code>' + code + '</code></pre></div>');
        fence = [];
        fenceLang = '';
      }
      for (const line of lines) {
        const fenceMatch = line.match(/^```\s*([^`]*)$/);
        if (fenceMatch) {
          if (inFence) flushFence();
          inFence = !inFence;
          fenceLang = (fenceMatch[1] || '').trim();
          continue;
        }
        if (inFence) { fence.push(line); continue; }
        const trimmed = line.trim();
        if (!trimmed) { out.push('<div style="height:6px"></div>'); continue; }
        const toolMatch = trimmed.match(TOOL_RE);
        if (toolMatch) {
          out.push('<div class="sum-tool"><span class="sum-tool-type">' + escapeHtml(toolMatch[2]) + '</span>' + escapeHtml(toolMatch[1]) + '</div>');
          continue;
        }
        const bulletMatch = trimmed.match(BULLET_RE);
        if (bulletMatch) {
          out.push('<div style="padding-left:var(--sp-5);text-indent:-10px">• ' + inlineFmt(bulletMatch[1]) + '</div>');
          continue;
        }
        out.push('<div>' + inlineFmt(trimmed) + '</div>');
      }
      if (inFence) flushFence();
      return out.join('') || '<em>No output.</em>';
    }

    // --- Hive Chat ---
    const CHAT_MAX_DOM = 80;
    const CHAT_MAX_TRANSCRIPT = 60;
    const CHAT_MAX_COMMAND_HISTORY = 100;
    const CHAT_CONTEXT_LIMIT = 6;
    const CHAT_TRANSCRIPT_KEY = 'hive-chat-transcript-v2';
    const CHAT_LEGACY_STORAGE_KEY = 'hive-chat-history';
    const CHAT_COMMAND_HISTORY_KEY = 'hive-chat-command-history-v1';
    const CHAT_SETTINGS_KEY = 'hive-chat-settings-v2';
    const CHAT_DRAFT_KEY = 'hive-chat-draft-v1';
    const CHAT_CURSOR_KEY = 'hive-chat-cursor-v1';
    const CHAT_POLL_OPEN_MS = 2000;
    const CHAT_POLL_IDLE_MS = 10000;
    const CHAT_POLL_MAX_BACKOFF_MS = 60000;
    const CHAT_POLL_TIMEOUT_MS = 10000;
    const CHAT_WELCOME = 'Welcome to **Hive Chat**. Type `/help` for commands, press `Tab` to complete slash commands, `↑/↓` for command history, and `⌘/Ctrl+K` to focus this terminal-grade dialog.';

    const chatFab = document.getElementById('hiveChatFab');
    const chatPanel = document.getElementById('hiveChatPanel');
    const chatClose = document.getElementById('hiveChatClose');
    const chatMessages = document.getElementById('hiveChatMessages');
    const chatInput = document.getElementById('hiveChatInput');
    const chatSend = document.getElementById('hiveChatSend');
    const chatAutocomplete = document.getElementById('hiveChatAutocomplete');
    const chatCheatsheet = document.getElementById('hiveChatCheatsheet');
    const chatSuggestions = document.getElementById('hiveChatSuggestions');
    const chatShortcuts = document.getElementById('hiveChatShortcuts');
    const chatCheatToggle = document.getElementById('hiveChatCheatToggle');
    const chatMax = document.getElementById('hiveChatMax');
    const chatDock = document.getElementById('hiveChatDock');
    const chatPresence = document.getElementById('hiveChatPresence');
    let chatBusy = false;
    const CHAT_RESPONSE_TIMEOUT_MS = 30000;
    let chatContext = [];
    let chatLastSeq = 0;
    let chatEpoch = '';
    let chatPollTimer = null;
    let chatPollFailures = 0;
    let chatTranscript = [];
    let chatCommandHistory = [];
    let chatHistoryIndex = -1;
    let chatDraft = '';
    let chatCompletions = [];
    let chatCompletionIndex = 0;
    let chatSpekEnabled = false;
    let chatLastHeartbeatSig = '';
    let chatLastUserQuery = '';
    let chatPendingUserEchoes = new Map();

    async function chatFetchJson(url, options) {
      const resp = await fetch(url, options || {});
      return await resp.json();
    }

    const CHAT_COMMANDS = [
      { name:'help', aliases:['?'], usage:'/help [command]', description:'List commands or show command details.', example:'/help who', details:'`/help` renders the command registry. `/help <command>` shows one command with example and notes.', run: chatCommandHelp },
      { name:'clear', aliases:['cls'], usage:'/clear', description:'Clear the visible transcript.', example:'/clear', details:'Clears persisted chat transcript but keeps command history unless you choose clear history.', run: chatCommandClear },
      { name:'search', aliases:['find'], usage:'/search <text>', description:'Search the persisted transcript.', example:'/search relay', details:'Searches the locally persisted transcript and returns matching turns with timestamps.', run: chatCommandSearch },
      { name:'history', aliases:['hist'], usage:'/history [clear|text]', description:'Show, search, or clear command history.', example:'/history relay', details:'Use Up/Down to cycle history. `/history clear` deletes persisted command history; `/history text` searches it.', run: chatCommandHistoryCmd },
      { name:'retry', aliases:['again'], usage:'/retry', description:'Retry the last sent dialog.', example:'/retry', details:'Resends the last user prompt without you retyping it.', run: chatCommandRetry },
      { name:'edit', aliases:['revise'], usage:'/edit', description:'Put the last prompt back in the input for editing.', example:'/edit', details:'Restores the last sent prompt as a persistent draft.', run: chatCommandEdit },
      { name:'who', aliases:['presence'], usage:'/who', description:'Show who is online/idle in this hive.', example:'/who', details:'Uses `/api/presence`; unauthenticated local dashboards only show `local`.', run: chatCommandWho },
      { name:'agents', aliases:['agent'], usage:'/agents [stuck|paused|running|idle|failed] | /agents <name> [status|why|last|history]', description:'Agent roster, filters and per-agent detail cards.', example:'/agents scanner why', details:'Bare `/agents` lists every agent (BLOCKED ones with the reason inline). `/agents stuck|blocked|paused|running|idle|failed` filters the roster with the reason and since-when. `/agents <name> [status|why|last|history]` (or `/agents status <name>`) shows only that agent: state, block reason, current task, last completed turn, last kick, last PR/issue, restarts. Natural language works too: "which agent completed last?", "why is scanner blocked?", "which agents are idle?". Anything else is forwarded to Hive Chat search with the agent state attached.', run: chatForwardCommand('agents') },
      { name:'beads', aliases:['issues','issue'], usage:'/beads <query>', description:'Ask about beads/issues and queue state.', example:'/beads high priority', details:'Bare `/beads` gives bead counts per store. With arguments the question is answered from the actionable-issue snapshot (stage labels, priority, assignees, age).', run: chatForwardCommand('beads') },
      { name:'prs', aliases:['pr'], usage:'/prs [filters|question]', description:'List open pull requests, filtered or as a question.', example:'/prs waiting on review', details:'Bare `/prs` lists the open PRs per repo (author, stage, age, CI, review state, needs-decision/hold, mergeable), capped at 25 rows. Filters answered from the snapshot without a model: waiting on review, needs decision, hold, failing ci, mine, by <login>, repo <owner/repo>, stale, older than 7d. Any other question (`/prs which should merge first`) is answered from the open-PR snapshot by the grounded chat.', run: chatForwardCommand('prs') },
      { name:'governor', aliases:['gov'], usage:'/governor <query>', description:'Ask about governor cadence, kicks and policy.', example:'/governor next kick', details:'Forwards a focused governor question to Hive Chat search.', run: chatForwardCommand('governor') },
      { name:'knowledge', aliases:['kb'], usage:'/knowledge <query>', description:'Ask the knowledge base and imported docs.', example:'/knowledge relay setup', details:'Forwards a focused knowledge-base question to Hive Chat search.', run: chatForwardCommand('knowledge') },
      { name:'jam', aliases:['swarm'], usage:'/jam <query>', description:'Ask about jam sessions, contributors and live collaboration.', example:'/jam who is free?', details:'Presence questions (who is online / free / here / active) are answered locally from `/api/presence`; anything else is forwarded to Hive Chat search.', run: chatCommandJam },
      { name:'spek', aliases:['spectacular','spec'], usage:'/spek <query>', description:'Ask about Spektacular/inception campaigns and spec runs (v6/edge when unavailable).', example:'/spek active campaigns', details:'Uses v6/edge Spektacular concepts: inception campaigns, spec runs, relay and campaign state.', run: chatForwardCommand('spek') },
    ];
    const CHAT_COMMAND_BY_NAME = new Map();
    CHAT_COMMANDS.forEach(c => [c.name].concat(c.aliases || []).forEach(n => CHAT_COMMAND_BY_NAME.set(n, c)));

    const CHAT_CHEATS = [
      { group:'Agents', items:['/agents stuck', '/agents scanner status', '/agents scanner why', 'Which agent completed last?', 'Which agents are idle?'] },
      { group:'Beads / Issues / PRs', items:['/beads high priority', '/prs waiting on review', 'Summarize open release blockers'] },
      { group:'Governor', items:['/governor next kick', 'Why is the governor paused?', 'Show recent kick failures'] },
      { group:'Knowledge', items:['/knowledge work sources', 'Search docs for relay setup', 'What changed in v5?'] },
      { group:'Jam', items:['/who', '/jam who is online?', 'Show contributor activity'] },
      { group:'Spek / Spektacular', spek:true, items:['!runs spec owner/repo#N', '/spek active campaigns', '/spek spec runs', 'Explain inception relay state'] },
    ];

    function loadChatSettings() { return safeJsonParse(localStorage.getItem(CHAT_SETTINGS_KEY) || '{}', {}); }
    function saveChatSettings(patch) {
      const next = Object.assign(loadChatSettings(), patch || {});
      localStorage.setItem(CHAT_SETTINGS_KEY, JSON.stringify(next));
      return next;
    }
    function chatNow() { return formatDashboardTime(new Date(), { hour:'2-digit', minute:'2-digit', timeZoneName:'short' }); }
    function chatPersistTranscript() { localStorage.setItem(CHAT_TRANSCRIPT_KEY, JSON.stringify(chatTranscript.slice(-CHAT_MAX_TRANSCRIPT))); }
    function chatLoadCursor() {
      const saved = safeJsonParse(localStorage.getItem(CHAT_CURSOR_KEY) || '{}', {});
      chatEpoch = saved && typeof saved.epoch === 'string' ? saved.epoch : '';
      chatLastSeq = saved && Number.isFinite(Number(saved.seq)) ? Math.max(0, Math.floor(Number(saved.seq))) : 0;
    }
    function chatPersistCursor() { localStorage.setItem(CHAT_CURSOR_KEY, JSON.stringify({ epoch: chatEpoch, seq: chatLastSeq })); }
    function chatPersistCommandHistory() { localStorage.setItem(CHAT_COMMAND_HISTORY_KEY, JSON.stringify(chatCommandHistory.slice(-CHAT_MAX_COMMAND_HISTORY))); }
    function chatPersistDraft() { localStorage.setItem(CHAT_DRAFT_KEY, chatInput.value.slice(0, 4000)); }
    function chatSetDraft(text) {
      chatInput.value = String(text || '').slice(0, 4000);
      chatPersistDraft();
      chatInput.focus();
      chatInput.setSelectionRange(chatInput.value.length, chatInput.value.length);
      updateChatAutocomplete();
    }

    function enhanceChatMessage(el, text) {
      el.querySelectorAll('.chat-copy-btn').forEach(btn => {
        btn.addEventListener('click', () => {
          const code = btn.parentElement && btn.parentElement.querySelector('code');
          navigator.clipboard?.writeText(code ? code.textContent : '').then(() => { btn.textContent = 'copied'; setTimeout(() => { btn.textContent = 'copy'; }, 1200); }).catch(() => {});
        });
      });
      if ((text || '').length > 1800 || el.scrollHeight > 360) {
        el.classList.add('long', 'collapsed');
        const more = document.createElement('button');
        more.type = 'button';
        more.className = 'chat-more-btn';
        more.textContent = 'Show more';
        more.addEventListener('click', () => {
          const collapsed = el.classList.toggle('collapsed');
          more.textContent = collapsed ? 'Show more' : 'Show less';
        });
        el.insertAdjacentElement('afterend', more);
      }
    }

    function chatAddMsg(content, cls, opts) {
      opts = opts || {};
      let text = opts.rawText != null ? String(opts.rawText) : String(content || '');
      if (cls === 'system') text = sanitizeChatText(text);
      if (cls === 'system' && /^Status heartbeat\b/i.test(text)) {
        const sig = text.replace(/\d{1,2}:\d{2}:\d{2}|\d{4}-\d{2}-\d{2}T[^\s]+/g, '').slice(0, 400);
        if (sig && sig === chatLastHeartbeatSig) {
          const last = chatMessages.querySelector('.chat-msg.system:last-of-type');
          if (last) last.dataset.folded = String((parseInt(last.dataset.folded || '1', 10) || 1) + 1);
          saveChatHistoryCompat();
          return last;
        }
        chatLastHeartbeatSig = sig;
      }
      const el = document.createElement('div');
      el.className = 'chat-msg ' + cls;
      if (opts.gap) el.classList.add('chat-gap');
      const author = cls === 'peer' ? String(opts.author || '') : '';
      el.innerHTML = opts.html ? String(content || '') : (cls === 'user' ? escapeHtml(text) : cls === 'peer' ? '<span class="chat-author">' + escapeHtml(author) + '</span>' + escapeHtml(text) : formatChatHtml(text));
      if (cls !== 'thinking') el.innerHTML += '<span class="chat-time">' + chatNow() + '</span>';
      chatMessages.appendChild(el);
      enhanceChatMessage(el, text);
      while (chatMessages.children.length > CHAT_MAX_DOM) chatMessages.removeChild(chatMessages.firstChild);
      chatMessages.scrollTop = chatMessages.scrollHeight;
      if (cls !== 'thinking') {
        const entry = { cls: cls === 'user' ? 'user' : cls === 'peer' ? 'peer' : 'system', text: text, ts: Date.now() };
        if (author) entry.author = author;
        if (opts.gap) entry.gap = true;
        chatTranscript.push(entry);
        chatTranscript = chatTranscript.slice(-CHAT_MAX_TRANSCRIPT);
        chatPersistTranscript();
      }
      return el;
    }
    function saveChatHistoryCompat() { chatPersistTranscript(); }

    function restoreChatHistory() {
      const saved = safeJsonParse(localStorage.getItem(CHAT_TRANSCRIPT_KEY) || '[]', []);
      const legacy = safeJsonParse(localStorage.getItem(CHAT_LEGACY_STORAGE_KEY) || '[]', []);
      chatTranscript = Array.isArray(saved) && saved.length ? saved.slice(-CHAT_MAX_TRANSCRIPT) : (Array.isArray(legacy) ? legacy.map(m => ({ cls:m.cls, text:m.text || m.html || '', ts:Date.now() })).slice(-CHAT_MAX_TRANSCRIPT) : []);
      if (!chatTranscript.length) { chatAddMsg(CHAT_WELCOME, 'system'); return; }
      const keep = chatTranscript.slice();
      chatTranscript = [];
      keep.forEach(m => chatAddMsg(m.text || '', m.cls === 'user' ? 'user' : m.cls === 'peer' ? 'peer' : 'system', { author: m.author, gap: !!m.gap }));
    }

    function restoreCommandHistory() {
      const saved = safeJsonParse(localStorage.getItem(CHAT_COMMAND_HISTORY_KEY) || '[]', []);
      chatCommandHistory = Array.isArray(saved) ? saved.filter(x => typeof x === 'string').slice(-CHAT_MAX_COMMAND_HISTORY) : [];
    }

    function pushCommandHistory(q) {
      if (!q || !q.trim()) return;
      if (chatCommandHistory[chatCommandHistory.length - 1] !== q) chatCommandHistory.push(q);
      chatCommandHistory = chatCommandHistory.slice(-CHAT_MAX_COMMAND_HISTORY);
      chatPersistCommandHistory();
      chatHistoryIndex = -1;
      chatDraft = '';
    }

    function renderChatCheatsheet() {
      const groups = CHAT_CHEATS.map(g => {
        const edge = g.spek && !chatSpekEnabled ? ' <span class="hive-chat-kbd">v6/edge</span>' : '';
        const items = g.items.map(item => '<button type="button" class="hive-chat-example" data-chat-example="' + escapeHtml(item) + '">' + escapeHtml(item) + '</button>').join('');
        return '<div class="hive-chat-cheat-group"><h4>' + escapeHtml(g.group) + edge + '</h4>' + items + '</div>';
      }).join('');
      chatCheatsheet.innerHTML = '<div class="hive-chat-cheat-head"><span class="hive-chat-cheat-title">Command hints</span><button type="button" class="hive-chat-tool" data-chat-clear-history>clear history</button></div><input class="hive-chat-search" id="hiveChatTranscriptSearch" placeholder="Search transcript…">' + groups;
      chatCheatsheet.querySelectorAll('[data-chat-example]').forEach(btn => btn.addEventListener('click', () => insertChatExample(btn.getAttribute('data-chat-example') || '')));
      const transcriptSearch = chatCheatsheet.querySelector('#hiveChatTranscriptSearch');
      if (transcriptSearch) transcriptSearch.addEventListener('keydown', e => {
        if (e.key === 'Enter') { e.preventDefault(); chatAddMsg(chatSearchTranscript(transcriptSearch.value), 'system'); }
      });
      const clear = chatCheatsheet.querySelector('[data-chat-clear-history]');
      if (clear) clear.addEventListener('click', () => { chatCommandHistory = []; chatPersistCommandHistory(); chatAddMsg('Command history cleared.', 'system'); });
    }

    function renderChatSuggestions() {
      const suggestions = ['/help', '/who', '/agents stuck', '/beads high priority', '/prs waiting on review', chatSpekEnabled ? '/spek spec runs' : '/spek spec runs (v6/edge)'];
      chatSuggestions.innerHTML = suggestions.map(s => '<button type="button" class="hive-chat-suggestion" data-chat-suggestion="' + escapeHtml(s.replace(' (v6/edge)', '')) + '">' + escapeHtml(s) + '</button>').join('');
      chatSuggestions.querySelectorAll('[data-chat-suggestion]').forEach(btn => btn.addEventListener('click', () => insertChatExample(btn.getAttribute('data-chat-suggestion') || '')));
    }

    function applyChatSettings() {
      const st = loadChatSettings();
      if (st.width) chatPanel.style.width = st.width + 'px';
      if (st.height) chatPanel.style.height = st.height + 'px';
      chatCheatsheet.classList.toggle('collapsed', !!st.cheatsCollapsed);
      chatPanel.classList.toggle('maximized', st.mode === 'maximized');
      chatPanel.classList.toggle('docked', st.mode === 'docked');
    }

    function insertChatExample(text) {
      chatInput.value = text;
      chatInput.focus();
      chatInput.setSelectionRange(chatInput.value.length, chatInput.value.length);
      updateChatAutocomplete();
    }

    function chatCommandHelp(args) {
      const name = (args || '').trim().replace(/^\//, '').toLowerCase();
      if (name) {
        const cmd = CHAT_COMMAND_BY_NAME.get(name);
        if (!cmd) return 'No command named `' + name + '`. Try `/help`.';
        return '#### /' + cmd.name + '\n' + cmd.description + '\n\nUsage: `' + (cmd.usage || ('/' + cmd.name)) + '`\n\nExample: `' + cmd.example + '`\n\n' + (cmd.details || '');
      }
      return '#### Hive Chat commands\n' + CHAT_COMMANDS.map(c => '- `' + (c.usage || ('/' + c.name)) + '` — ' + c.description + ' Example: `' + c.example + '`').join('\n') + '\n\nA bare `/agents`, `/prs`, `/beads` or `/status` prints the summary. Add arguments (`/agents stuck`, `/prs waiting on review`) or ask in plain words (`which agent completed last?`) and Hive Chat answers from a read-only snapshot of the live hive state, citing the agents and items it used — or says it does not have that data. Grounded answers never take actions; they are gated by `chat.grounded_answers.enabled` (default on) and use the governor reviewer endpoint.';
    }
    function chatCommandClear() {
      chatTranscript = [];
      chatMessages.innerHTML = '';
      chatPersistTranscript();
      chatAddMsg('Transcript cleared. Command history is kept; use `/history clear` or the sidebar control to clear it.', 'system');
      return null;
    }
    function chatCommandHistoryCmd(args) {
      const term = (args || '').trim();
      if (term === 'clear') {
        chatCommandHistory = [];
        chatPersistCommandHistory();
        return 'Command history cleared.';
      }
      if (!chatCommandHistory.length) return 'No command history yet.';
      const rows = term ? chatCommandHistory.filter(h => h.toLowerCase().includes(term.toLowerCase())) : chatCommandHistory;
      if (!rows.length) return 'No command history entries match `' + term + '`.';
      return rows.slice(-20).map((h, i) => (i + 1) + '. `' + h + '`').join('\n');
    }
    function chatSearchTranscript(term) {
      const q = (term || '').trim().toLowerCase();
      if (!q) return 'Usage: `/search <text>`.';
      const rows = chatTranscript.filter(m => String(m.text || '').toLowerCase().includes(q)).slice(-12);
      if (!rows.length) return 'No transcript entries match `' + term + '`.';
      return rows.map(m => '- `' + formatDashboardTime(m.ts || Date.now(), { hour:'2-digit', minute:'2-digit', timeZoneName:'short' }) + '` **' + (m.cls === 'user' ? 'you' : 'hive') + '**: ' + String(m.text || '').replace(/\s+/g, ' ').slice(0, 220)).join('\n');
    }
    function chatCommandSearch(args) { return chatSearchTranscript(args); }
    async function chatCommandRetry() {
      if (!chatLastUserQuery) return 'No prompt to retry yet.';
      return await chatAskBackend(chatLastUserQuery, false);
    }
    function chatCommandEdit() {
      if (!chatLastUserQuery) return 'No prompt to edit yet.';
      chatSetDraft(chatLastUserQuery);
      return 'Last prompt restored to the input draft.';
    }
    async function chatCommandWho() {
      const data = await chatFetchJson('/api/presence', { headers: _inceptionAuthHeaders() });
      const users = (data && Array.isArray(data.users)) ? data.users : [];
      if (!users.length) return 'No authenticated users are visible. Local dashboards report only `local`.';
      return users.map(u => (u.active ? '🟢 ' : '⚪ ') + '**' + (u.display_name || u.username || 'local') + '**' + (u.you ? ' (you)' : '') + ' — ' + (u.active ? 'active' : 'idle') + (u.last_action ? ', last action ' + u.last_action : '')).join('\n');
    }
    const CHAT_JAM_PRESENCE_RE = /\b(online|who|free|here|around|present|presence|active|idle)\b/i;
    async function chatCommandJam(args) {
      const q = (args || '').trim();
      if (!q || CHAT_JAM_PRESENCE_RE.test(q)) return await chatCommandWho();
      return await chatForwardCommand('jam')(q);
    }
    function chatForwardCommand(scope) {
      return async function(args) {
        const q = (args || '').trim() || 'status';
        return await chatAskBackend(scope + ': ' + q, true);
      };
    }

    async function chatRunCommand(q) {
      const body = q.replace(/^\//, '').trim();
      const space = body.search(/\s/);
      const name = (space === -1 ? body : body.slice(0, space)).toLowerCase();
      const args = space === -1 ? '' : body.slice(space + 1).trim();
      const cmd = CHAT_COMMAND_BY_NAME.get(name);
      if (!cmd) return 'Unknown command `/' + name + '`. Try `/help`.';
      return await cmd.run(args);
    }

    async function chatAskBackend(q, suppressUserEcho) {
      if (!suppressUserEcho) chatAddMsg(q, 'user');
      chatTrackPendingUserEcho(q, 1);
      chatContext.push({ role: 'user', text: q });
      if (chatContext.length > CHAT_CONTEXT_LIMIT) chatContext = chatContext.slice(-CHAT_CONTEXT_LIMIT);
      const thinking = chatAddMsg('Thinking…', 'thinking');
      const controller = new AbortController();
      const timeout = setTimeout(() => controller.abort(), CHAT_RESPONSE_TIMEOUT_MS);
      try {
        const resp = await fetch('/api/chat', { method:'POST', headers:_inceptionAuthHeaders(), signal:controller.signal, body:JSON.stringify({ query:q, history:chatContext.slice(0, -1) }) });
        let data = {};
        try {
          data = await resp.json();
        } catch (_) {
          data = { error: resp.ok ? 'Chat returned an unreadable response.' : 'Chat request failed with HTTP ' + resp.status + '.' };
        }
        thinking.remove();
        if (!data.accepted && !data.seq) chatTrackPendingUserEcho(q, -1);
        if (!resp.ok || data.error) return 'Error: ' + (data.error || ('Chat request failed with HTTP ' + resp.status + '.'));
        if (!data.answer) {
          await chatPollMessages();
          return null;
        }
        const answer = data.answer || 'No results found.';
        chatContext.push({ role:'assistant', text: answer });
        if (chatContext.length > CHAT_CONTEXT_LIMIT) chatContext = chatContext.slice(-CHAT_CONTEXT_LIMIT);
        let html = formatChatHtml(answer);
        if (data.raw) html += '<details class="chat-raw-details"><summary>Search data</summary><pre>' + escapeHtml(data.raw) + '</pre></details>';
        if (data.sources && data.sources.length) html += '<div class="chat-sources">Sources: ' + data.sources.map(s => '<span>' + escapeHtml(String(s)) + '</span>').join('') + '</div>';
        chatAddMsg(html, 'system', { html:true, rawText: answer });
        return null;
      } catch (err) {
        thinking.remove();
        chatTrackPendingUserEcho(q, -1);
        if (err && err.name === 'AbortError') return 'Chat request timed out. Please try again.';
        return 'Network error: ' + ((err && err.message) || 'request failed');
      } finally {
        clearTimeout(timeout);
        if (thinking.isConnected) thinking.remove();
      }
    }

    async function chatSendQuery() {
      const q = chatInput.value.trim();
      if (!q || chatBusy) return;
      chatBusy = true;
      chatSend.disabled = true;
      chatInput.value = '';
      localStorage.removeItem(CHAT_DRAFT_KEY);
      closeChatAutocomplete();
      pushCommandHistory(q);
      if (q !== '/retry' && q !== '/edit') chatLastUserQuery = q;
      chatAddMsg(q, 'user');
      try {
        let response = null;
        if (q === 'help' || q.startsWith('/')) response = await chatRunCommand(q === 'help' ? '/help' : q);
        else response = await chatAskBackend(q, true);
        if (response) chatAddMsg(response, 'system');
      } finally {
        chatBusy = false;
        chatSend.disabled = false;
        chatInput.focus();
      }
    }

    function updateChatAutocomplete() {
      const val = chatInput.value;
      const hint = val.match(/^\/(\S+)\s/);
      if (hint) {
        const cmd = CHAT_COMMAND_BY_NAME.get(hint[1].toLowerCase());
        if (!cmd) { closeChatAutocomplete(); return; }
        chatCompletions = [];
        chatAutocomplete.innerHTML = '<div class="hive-chat-completion active"><span>' + escapeHtml(cmd.usage || ('/' + cmd.name)) + '</span><span class="hive-chat-kbd">' + escapeHtml(cmd.example || '') + '</span></div>';
        chatAutocomplete.classList.add('open');
        return;
      }
      const m = val.match(/^\/(\S*)$/);
      if (!m) { closeChatAutocomplete(); return; }
      const prefix = m[1].toLowerCase();
      chatCompletions = CHAT_COMMANDS.filter(c => c.name.startsWith(prefix) || (c.aliases || []).some(a => a.startsWith(prefix))).slice(0, 8);
      chatCompletionIndex = Math.min(chatCompletionIndex, Math.max(0, chatCompletions.length - 1));
      if (!chatCompletions.length) { closeChatAutocomplete(); return; }
      chatAutocomplete.innerHTML = chatCompletions.map((c, i) => '<div class="hive-chat-completion ' + (i === chatCompletionIndex ? 'active' : '') + '" data-command="' + escapeHtml(c.name) + '"><span>' + escapeHtml(c.usage || ('/' + c.name)) + '</span><span class="hive-chat-kbd">' + escapeHtml(c.description) + '</span></div>').join('');
      chatAutocomplete.classList.add('open');
      chatAutocomplete.querySelectorAll('[data-command]').forEach(el => el.addEventListener('mousedown', e => { e.preventDefault(); completeChatCommand(el.getAttribute('data-command')); }));
    }
    function closeChatAutocomplete() { chatAutocomplete.classList.remove('open'); chatAutocomplete.innerHTML = ''; chatCompletions = []; chatCompletionIndex = 0; }
    function completeChatCommand(name) { chatInput.value = '/' + name + ' '; chatInput.focus(); closeChatAutocomplete(); }

    function cycleChatHistory(dir) {
      if (!chatCommandHistory.length) return;
      if (chatHistoryIndex === -1) { chatDraft = chatInput.value; chatHistoryIndex = chatCommandHistory.length; }
      chatHistoryIndex = Math.max(0, Math.min(chatCommandHistory.length, chatHistoryIndex + dir));
      chatInput.value = chatHistoryIndex === chatCommandHistory.length ? chatDraft : chatCommandHistory[chatHistoryIndex];
      setTimeout(() => chatInput.setSelectionRange(chatInput.value.length, chatInput.value.length));
    }

    async function refreshChatPresence() {
      try {
        const data = await chatFetchJson('/api/presence', { headers: _inceptionAuthHeaders() });
        const users = (data && data.users) || [];
        const active = users.filter(u => u.active).length;
        const label = users.length ? (active + '/' + users.length + ' active') : 'local';
        chatPresence.innerHTML = '<span class="hive-presence-dot ' + (active ? 'active' : '') + '"></span><span>' + escapeHtml(label) + '</span>';
      } catch (_) {}
    }

    async function detectChatCapabilities() {
      try {
        const v = await chatFetchJson('/api/version');
        const branch = String((v && (v.branch || v.channel || v.imageRef || '')) || '').toLowerCase();
        chatSpekEnabled = branch.includes('v6') || branch.includes('edge');
      } catch (_) { chatSpekEnabled = false; }
      renderChatCheatsheet();
      renderChatSuggestions();
    }

    restoreCommandHistory();
    applyChatSettings();
    chatLoadCursor();
    restoreChatHistory();
    chatInput.value = localStorage.getItem(CHAT_DRAFT_KEY) || '';
    renderChatCheatsheet();
    renderChatSuggestions();
    detectChatCapabilities();
    refreshChatPresence();
    dashboardSetInterval('chat-presence', refreshChatPresence, 30000);

    chatFab.onclick = () => {
      const isOpen = chatPanel.classList.toggle('open');
      if (isOpen) hiveRememberModalOpen(chatPanel);
      chatFab.classList.toggle('open', isOpen);
      if (isOpen) {
        chatFab.classList.remove('has-unread');
        chatInput.focus();
        chatPollMessages();
        chatMessages.scrollTop = chatMessages.scrollHeight;
        refreshChatPresence();
      }
    };
    chatClose.onclick = () => { chatPanel.classList.remove('open'); chatFab.classList.remove('open'); };
    chatCheatToggle.onclick = () => { const collapsed = !chatCheatsheet.classList.contains('collapsed'); chatCheatsheet.classList.toggle('collapsed', collapsed); saveChatSettings({ cheatsCollapsed: collapsed }); };
    chatMax.onclick = () => { const on = !chatPanel.classList.contains('maximized'); chatPanel.classList.toggle('maximized', on); chatPanel.classList.remove('docked'); saveChatSettings({ mode: on ? 'maximized' : '' }); };
    chatDock.onclick = () => { const on = !chatPanel.classList.contains('docked'); chatPanel.classList.toggle('docked', on); chatPanel.classList.remove('maximized'); saveChatSettings({ mode: on ? 'docked' : '' }); };
    if (chatShortcuts) chatShortcuts.addEventListener('click', (e) => { if (e.target === chatShortcuts) chatShortcuts.classList.remove('open'); });

    (function initChatResize() {
      const handle = document.getElementById('hiveChatResize');
      const widthHandle = document.getElementById('hiveChatResizeWidth');
      let startY = 0, startH = 0, startX = 0, startW = 0;
      handle.addEventListener('pointerdown', (e) => { startY = e.clientY; startH = chatPanel.offsetHeight; handle.setPointerCapture(e.pointerId); e.preventDefault(); });
      handle.addEventListener('pointermove', (e) => { if (!startH) return; const newH = Math.max(280, Math.min(window.innerHeight * 0.92, startH + (startY - e.clientY))); chatPanel.style.height = newH + 'px'; });
      handle.addEventListener('pointerup', () => { if (startH) saveChatSettings({ height: chatPanel.offsetHeight }); startH = 0; });
      widthHandle.addEventListener('pointerdown', (e) => { startX = e.clientX; startW = chatPanel.offsetWidth; widthHandle.setPointerCapture(e.pointerId); e.preventDefault(); });
      widthHandle.addEventListener('pointermove', (e) => { if (!startW) return; const newW = Math.max(360, Math.min(window.innerWidth - 32, startW + (startX - e.clientX))); chatPanel.style.width = newW + 'px'; });
      widthHandle.addEventListener('pointerup', () => { if (startW) saveChatSettings({ width: chatPanel.offsetWidth }); startW = 0; });
    })();

    chatSend.onclick = chatSendQuery;
    chatInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); chatSendQuery(); return; }
      if (e.key === 'Tab' && chatCompletions.length) { e.preventDefault(); completeChatCommand(chatCompletions[chatCompletionIndex].name); return; }
      if (e.key === 'ArrowDown' && chatCompletions.length) { e.preventDefault(); chatCompletionIndex = (chatCompletionIndex + 1) % chatCompletions.length; updateChatAutocomplete(); return; }
      if (e.key === 'ArrowUp' && chatCompletions.length) { e.preventDefault(); chatCompletionIndex = (chatCompletionIndex + chatCompletions.length - 1) % chatCompletions.length; updateChatAutocomplete(); return; }
      if (e.key === 'ArrowUp' && !e.shiftKey && chatInput.selectionStart === 0) { e.preventDefault(); cycleChatHistory(-1); return; }
      if (e.key === 'ArrowDown' && !e.shiftKey) { e.preventDefault(); cycleChatHistory(1); return; }
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'r') { e.preventDefault(); chatAddMsg(chatCommandHistory.length ? chatCommandHistory.slice(-10).map(h => '`' + h + '`').join('\n') : 'No command history yet.', 'system'); }
    });
    chatInput.addEventListener('input', () => { chatInput.style.height = 'auto'; chatInput.style.height = Math.min(chatInput.scrollHeight, 80) + 'px'; updateChatAutocomplete(); chatHistoryIndex = -1; chatPersistDraft(); });
    document.addEventListener('keydown', (e) => {
      const inEditable = e.target && (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA' || e.target.isContentEditable);
      if (e.key === '?' && !inEditable) { e.preventDefault(); if (chatShortcuts) { chatShortcuts.classList.add('open'); hiveRememberModalOpen(chatShortcuts); } return; }
      if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); chatPanel.classList.add('open'); hiveRememberModalOpen(chatPanel); chatFab.classList.add('open'); chatInput.focus(); }
    });

    // Sidebar resize handle — drag right edge to resize sidebar width
    // Mobile drawer. Under 1024px (phones and portrait tablets) the sidebar is translated offscreen; .open
    // slides it back in. Nothing is persisted — a drawer left open across
    // reloads would be a bug, not a feature.
    (function initMobileDrawer() {
      const DRAWER_MAX_W = 1024; // must match the @media breakpoint above
      const sidebar = document.getElementById('oc-sidebar');
      const toggle = document.getElementById('oc-drawer-toggle');
      const backdrop = document.getElementById('oc-drawer-backdrop');
      if (!sidebar || !toggle || !backdrop) return;

      function isMobile() { return window.innerWidth <= DRAWER_MAX_W; }
      function isOpen() { return sidebar.classList.contains('open'); }

      function setOpen(open) {
        sidebar.classList.toggle('open', open);
        backdrop.classList.toggle('open', open);
        toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
        toggle.setAttribute('aria-label', open ? 'Close menu' : 'Open menu');
      }
      function closeDrawer() { if (isOpen()) setOpen(false); }

      toggle.addEventListener('click', (e) => {
        e.stopPropagation();
        setOpen(!isOpen());
      });
      backdrop.addEventListener('click', closeDrawer);
      // Rotating to landscape (or resizing past the breakpoint) must not strand
      // the drawer open — above 1024px the sidebar is a normal static column and
      // a leftover .open would leave the backdrop covering the page.
      window.addEventListener('resize', () => {
        if (!isMobile()) closeDrawer();
      });
      // Picking a destination should reveal it, not leave the drawer on top of
      // it. Capture phase so this runs even though the nav items are handled by
      // a delegated click listener further up the tree.
      sidebar.addEventListener('click', (e) => {
        if (!isMobile() || !isOpen()) return;
        const item = e.target.closest('[data-action="ocSelectAgent"], .oc-nav-item');
        if (!item) return;
        // Row-level action buttons (⚙ / ✕) open their own dialogs — keep the
        // drawer up so the user keeps their place in the list.
        if (e.target.closest('.oc-nav-actions, .oc-group-actions, .oc-tree-toggle, .oc-sidebar-group-header')) return;
        closeDrawer();
      });
      window.hiveCloseMobileDrawer = closeDrawer;
    })();

    (function initSidebarResize() {
      const SIDEBAR_WIDTH_KEY = 'hive-sidebar-width';
      const SIDEBAR_MIN_W = 280;
      const SIDEBAR_MAX_W = 400;
      const SIDEBAR_DEFAULT_W = 302;
      const saved = parseInt(localStorage.getItem(SIDEBAR_WIDTH_KEY), 10);
      if (saved >= SIDEBAR_MIN_W && saved <= SIDEBAR_MAX_W) {
        document.documentElement.style.setProperty('--sidebar-w', saved + 'px');
      }
      const handle = document.getElementById('ocSidebarResize');
      if (!handle) return;
      let startX = 0, startW = 0;
      handle.addEventListener('pointerdown', (e) => {
        startX = e.clientX;
        const cur = parseInt(getComputedStyle(document.documentElement).getPropertyValue('--sidebar-w'), 10) || SIDEBAR_DEFAULT_W;
        startW = cur;
        handle.setPointerCapture(e.pointerId);
        handle.classList.add('dragging');
        e.preventDefault();
      });
      handle.addEventListener('pointermove', (e) => {
        if (!startW) return;
        const newW = Math.max(SIDEBAR_MIN_W, Math.min(SIDEBAR_MAX_W, startW + (e.clientX - startX)));
        document.documentElement.style.setProperty('--sidebar-w', newW + 'px');
      });
      handle.addEventListener('pointerup', () => {
        if (startW) {
          const cur = parseInt(getComputedStyle(document.documentElement).getPropertyValue('--sidebar-w'), 10) || SIDEBAR_DEFAULT_W;
          localStorage.setItem(SIDEBAR_WIDTH_KEY, cur);
        }
        startW = 0;
        handle.classList.remove('dragging');
      });
    })();

    function chatTrackPendingUserEcho(text, delta) {
      const key = String(text || '');
      if (!key) return 0;
      const next = Math.max(0, (chatPendingUserEchoes.get(key) || 0) + delta);
      if (next) chatPendingUserEchoes.set(key, next);
      else chatPendingUserEchoes.delete(key);
      return next;
    }

    function chatRenderOutbound(msg, viewer) {
      if (!msg) return;
      const seq = Number(msg.seq || 0);
      if (seq && seq <= chatLastSeq) return;
      // Advance past every retained entry, even one with empty text, so the
      // cursor never sticks on an entry this panel will never render.
      if (seq) chatLastSeq = Math.max(chatLastSeq, seq);
      if (!msg.text) return;
      if (msg.role === 'user') {
        const author = String(msg.author_id || '');
        // The outbox is one shared channel: only a line whose author is this
        // viewer is "mine". Anyone else's — or an unattributed one — is shown
        // as a peer line and stays out of this viewer's conversational
        // context, which is posted back as `history` (hivecommons/hive#9135).
        if (author !== String(viewer || '')) {
          chatAddMsg(msg.text, 'peer', { author: author || 'unknown' });
          if (!chatPanel.classList.contains('open')) chatFab.classList.add('has-unread');
          return;
        }
        if ((chatPendingUserEchoes.get(msg.text) || 0) > 0) {
          chatTrackPendingUserEcho(msg.text, -1);
          return;
        }
        chatAddMsg(msg.text, 'user');
        chatContext.push({ role: 'user', text: msg.text });
      } else {
        chatAddMsg(msg.text, 'system');
        chatContext.push({ role: 'assistant', text: msg.text });
        if (!chatPanel.classList.contains('open')) chatFab.classList.add('has-unread');
      }
      if (chatContext.length > CHAT_CONTEXT_LIMIT) chatContext = chatContext.slice(-CHAT_CONTEXT_LIMIT);
    }

    // Applies one /api/chat/messages envelope. Seq is a per-process counter,
    // so the cursor is only valid inside the epoch that minted it: a new epoch
    // (hive restart) or a `next` below the cursor means this batch was filtered
    // against a stale cursor and must be discarded. Returns true when the
    // cursor was reset and the caller should poll again right away.
    function chatApplyPoll(data) {
      data = data || {};
      const epoch = String(data.epoch || '');
      const next = Number(data.next);
      const hadCursor = chatLastSeq > 0;
      if ((epoch && chatEpoch && epoch !== chatEpoch) || (Number.isFinite(next) && next < chatLastSeq)) {
        chatEpoch = epoch;
        chatLastSeq = 0;
        chatPersistCursor();
        if (hadCursor) chatAddMsg('Hive restarted — the chat channel was reset. Replies sent before the restart may be missing here.', 'system', { gap: true });
        return true;
      }
      if (epoch && !chatEpoch) chatEpoch = epoch;
      const viewer = String(data.viewer || '');
      (data.messages || []).forEach(m => chatRenderOutbound(m, viewer));
      // A gap always arrives with the full retained ring behind it, which is
      // more than CHAT_MAX_DOM rows: a marker placed before the batch would be
      // trimmed unseen, so it goes after the batch as the newest line.
      if (data.gap && hadCursor) chatAddMsg('Some earlier chat messages were dropped from the channel before this panel could fetch them.', 'system', { gap: true });
      chatPersistCursor();
      return false;
    }

    function chatPollDelay() {
      const base = chatPanel.classList.contains('open') ? CHAT_POLL_OPEN_MS : CHAT_POLL_IDLE_MS;
      return Math.min(CHAT_POLL_MAX_BACKOFF_MS, base * Math.pow(2, chatPollFailures));
    }
    function chatSchedulePoll() {
      clearTimeout(chatPollTimer);
      chatPollTimer = null;
      if (document.hidden) return;
      chatPollTimer = setTimeout(chatPollMessages, chatPollDelay());
    }
    function chatNotePollFailure() { chatPollFailures = Math.min(chatPollFailures + 1, 8); }

    // Polls the shared channel: every 2 s while the panel is open, every 10 s
    // while it is closed (so the unread badge still works), never while the
    // tab is hidden, and with exponential backoff up to 60 s after any
    // non-2xx or network failure (hivecommons/hive#9135).
    let chatPollInFlight = false;
    let chatPollQueued = false;
    async function chatPollMessages(retried) {
      // The timer, FAB open, visibilitychange and a /api/chat completion can
      // all call this; overlapping polls could resolve out of order across a
      // restart and flip the epoch back and forth. One poll at a time — but a
      // call that lands mid-flight queues one follow-up, because the request
      // in flight may predate the reply that caller is waiting for.
      if (chatPollInFlight) { chatPollQueued = true; return; }
      chatPollInFlight = true;
      chatPollQueued = false;
      clearTimeout(chatPollTimer);
      chatPollTimer = null;
      let again = false;
      // No timer is armed while a poll is in flight, so a stalled request
      // (proxy, browser connection pool) used to silence the channel until it
      // resolved — 99 s in hivecommons/hive#15137. Bound it.
      const controller = new AbortController();
      const abortTimer = setTimeout(() => controller.abort(), CHAT_POLL_TIMEOUT_MS);
      try {
        const resp = await fetch('/api/chat/messages?since=' + encodeURIComponent(chatLastSeq), {
          headers: _inceptionAuthHeaders(),
          signal: controller.signal,
        });
        if (!resp.ok) { chatNotePollFailure(); return; }
        const data = await resp.json();
        chatPollFailures = 0;
        again = chatApplyPoll(data) && retried !== true;
      } catch (_) {
        chatNotePollFailure();
      } finally {
        clearTimeout(abortTimer);
        chatPollInFlight = false;
        if (again) await chatPollMessages(true);
        else if (chatPollQueued) await chatPollMessages();
        else chatSchedulePoll();
      }
    }
    document.addEventListener('visibilitychange', () => {
      if (document.hidden) { clearTimeout(chatPollTimer); chatPollTimer = null; return; }
      chatPollFailures = 0;
      chatPollMessages();
    });
    chatPollMessages();

