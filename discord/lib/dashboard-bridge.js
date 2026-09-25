const http = require('http');
const { agentMessage } = require('./formatter');

const { AGENTS } = require('./agent-identities');

const SSE_RECONNECT_BASE_MS = 5000;
const SSE_RECONNECT_MAX_MS = 60000;
const DASHBOARD_STALE_WARN_MS = 30000;
const TOPIC_DEBOUNCE_MS = 5000;
const TOPIC_AGENT_ORDER_DEFAULT = ['scanner', 'ci-maintainer', 'architect', 'outreach', 'supervisor'];
const TOPIC_STATE_ICONS = { working: '🟢', idle: '⚪', paused: '🔴', off: '⚫' };
const PANE_CHROME_HINTS = [
  'open sidebar', '/ commands', '? help', 'tab next tab', 'esc to interrupt',
  'esc interrupt', 'esc cancel', 'ctrl+c', 'ctrl-c', 'ctrl + c', 'enter to send',
  'shift+tab', 'shift + tab', '↑/↓ to navigate',
];

function isPaneChromeLine(line) {
  const t = String(line || '').trim();
  if (!t) return true;
  const lower = t.toLowerCase();
  if (t.includes('```')) return true;
  if (/^[─━│┃┌┐└┘┏┓┗┛╭╮╰╯├┤┬┴┼╞╡╪═║╔╗╚╝╟╢╤╧╫╠╣╦╩╬╾╼╿╽╸╺╹╻╴╶╵╷\s]+$/.test(t)) return true;
  if (['❯', '›', '>'].includes(t)) return true;
  if (t === 'Type your message or @path/to/file' || t === 'Enter your prompt, / for commands') return true;
  if (lower.includes('esc cancel')) return true;
  if (t.startsWith('› ') && /(improve documentation|explain this codebase|ask anything|type a message|enter your prompt|message or @path)/i.test(t)) return true;
  const hintCount = PANE_CHROME_HINTS.filter(h => lower.includes(h)).length;
  if (hintCount >= 2 && t.length <= 180) return true;
  if (hintCount === 1 && t.length <= 180 && (t.startsWith('← ') || t.includes(' · ') || t.includes(' • '))) return true;
  const hasSpinner = /[◎◉●○◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/.test(t);
  if (/^[◎◉●○◐◑◒◓⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏]/.test(t) && /(working|thinking|processing|tokens|esc|ctrl\+c)/i.test(t) && t.length <= 160) return true;
  if (/(claude|copilot|gemini|openai codex|codex|bob-shell|goose)/i.test(t) &&
      (/(esc|ctrl\+c|\/ commands|\? help|tokens|context|%\/)/i.test(t) ||
       (hasSpinner && /(thinking|working|loading|processing)/i.test(t))) && t.length <= 180) return true;
  return false;
}

function sanitizePaneText(text, maxLines) {
  let lines = String(text || '').split('\n').filter(l => !isPaneChromeLine(l));
  if (maxLines > 0 && lines.length > maxLines) lines = lines.slice(-maxLines);
  return lines.join('\n').trim();
}

function completionOutcome(summary) {
  const lower = String(summary || '').toLowerCase();
  if (!lower) return '';
  if (/(nothing to do|no new work|no actionable|no changes)/.test(lower)) return 'no new work';
  if (/(\/pull\/| pr #|pull request|\/issues\/|issue #|commented)/.test(lower)) {
    return String(summary).split('\n').map(s => s.trim()).find(Boolean) || '';
  }
  return '';
}

function durationSuffix(lastKickAt) {
  if (!lastKickAt) return '';
  const started = Date.parse(lastKickAt);
  if (!Number.isFinite(started)) return '';
  const ms = Date.now() - started;
  if (ms < 0) return '';
  const min = Math.round(ms / 60000);
  if (min < 1) return ' (<1m)';
  if (min < 60) return ` (${min}m)`;
  const h = Math.floor(min / 60);
  const m = min % 60;
  return ` (${h}h${m ? ` ${m}m` : ''})`;
}

class DashboardBridge {
  constructor(config, sendMessage, sendEmbed, setTopic) {
    this.config = config;
    this.sendMessage = sendMessage;
    this.sendEmbed = sendEmbed;
    this.setTopic = setTopic || (() => {});
    this.lastState = null;
    this.lastTopic = '';
    this.topicTimer = null;
    this.reconnectDelay = SSE_RECONNECT_BASE_MS;
    this.staleTimer = null;
    this.firstEvent = true;
  }

  start() {
    this._connect();
  }

  stop() {
    if (this.req) {
      this.req.destroy();
      this.req = null;
    }
    clearTimeout(this.staleTimer);
  }

  _connect() {
    const url = new URL('/api/events', this.config.dashboardUrl);
    const req = http.get(url, (res) => {
      if (res.statusCode !== 200) {
        res.resume();
        this._scheduleReconnect();
        return;
      }

      this.reconnectDelay = SSE_RECONNECT_BASE_MS;
      this._resetStaleTimer();
      let buffer = '';

      res.on('data', (chunk) => {
        buffer += chunk.toString();
        const lines = buffer.split('\n\n');
        buffer = lines.pop();
        for (const block of lines) {
          const dataLine = block.split('\n').find(l => l.startsWith('data:'));
          if (dataLine) {
            try {
              const data = JSON.parse(dataLine.slice(5).trim());
              this._onEvent(data);
              this._resetStaleTimer();
            } catch (_) { /* ignore parse errors */ }
          }
        }
      });

      res.on('end', () => this._scheduleReconnect());
      res.on('error', () => this._scheduleReconnect());
    });

    req.on('error', () => this._scheduleReconnect());
    this.req = req;
  }

  _scheduleReconnect() {
    setTimeout(() => this._connect(), this.reconnectDelay);
    this.reconnectDelay = Math.min(this.reconnectDelay * 2, SSE_RECONNECT_MAX_MS);
  }

  _resetStaleTimer() {
    clearTimeout(this.staleTimer);
    this.staleTimer = setTimeout(() => {
      this.sendMessage(agentMessage('pipeline', '⚠️ Dashboard SSE connection lost — commands may not route'), 'alerts');
    }, DASHBOARD_STALE_WARN_MS);
  }

  _onEvent(data) {
    if (this.firstEvent) {
      this.lastState = data;
      this.firstEvent = false;
      return;
    }

    if (!this.lastState) {
      this.lastState = data;
      return;
    }

    this._diffAgents(data);
    this._diffGovernor(data);
    this._updateTopic(data);
    this.lastState = data;
  }

  _diffAgents(data) {
    if (!this.config.postAgentTransitions) return;
    const agents = Array.isArray(data.agents) ? data.agents : [];
    const prevAgents = Array.isArray(this.lastState.agents) ? this.lastState.agents : [];
    const prevMap = {};
    for (const a of prevAgents) { if (a.name) prevMap[a.name] = a; }

    for (const agent of agents) {
      const name = agent.name;
      if (!name) continue;
      const old = prevMap[name];
      if (!old) continue;

      if (old.busy !== agent.busy) {
        const doing = agent.doing ? ` — ${agent.doing.slice(0, 100)}` : '';
        if (agent.busy === 'idle' && old.busy === 'working') {
          this.sendMessage(agentMessage(name, this._completionText(agent, old)));
        } else if (agent.busy === 'working' && old.busy === 'idle') {
          this.sendMessage(agentMessage(name, `Working${doing}${this._detailsSuffix(name)}`));
        } else if (agent.cadence === 'paused' && old.cadence !== 'paused') {
          this.sendMessage(agentMessage(name, 'Paused'));
        } else if (agent.cadence === 'off' && old.cadence !== 'off') {
          this.sendMessage(agentMessage(name, 'Off (cadence rule)'));
        }
      }

      const oldSummary = sanitizePaneText(old.liveSummary || '', 4).trim();
      const newSummary = sanitizePaneText(agent.liveSummary || '', 4).trim();
      if (newSummary && newSummary !== oldSummary) {
        const lines = newSummary.split('\n').slice(0, 4).join('\n').slice(0, 400);
        this.sendMessage(agentMessage(name, `\n\`\`\`\n${lines}\n\`\`\``));
      }
    }
  }

  _completionText(agent, old) {
    const summary = sanitizePaneText(agent.liveSummary || '', 3);
    const duration = durationSuffix(agent.lastKickAt);
    const details = this._detailsSuffix(agent.name);
    const work = String(agent.doing || old.doing || '').trim().replace(/^done\s+/i, '').slice(0, 100);
    const outcome = completionOutcome(summary);
    if (outcome) {
      if (work && outcome !== 'no new work') return `Completed — ${work}: ${outcome}${duration}${details}`;
      return `Completed — ${outcome}${duration}${details}`;
    }
    if (work) return `Completed — finished a pass on ${work}${duration}${details}`;
    return `Completed — finished a pass — no new work${duration}${details}`;
  }

  _detailsSuffix(name) {
    if (!this.config.dashboardUrl || !name) return '';
    return ` — details: ${String(this.config.dashboardUrl).replace(/\/+$/, '')}/#${encodeURIComponent(name)}`;
  }

  _updateTopic(data) {
    const agents = Array.isArray(data.agents) ? data.agents : [];
    const agentMap = {};
    for (const a of agents) { if (a.name) agentMap[a.name] = a; }

    const agentOrder = agents.length > 0
      ? agents.map(a => a.name).filter(Boolean)
      : TOPIC_AGENT_ORDER_DEFAULT;
    const parts = agentOrder.map(name => {
      const a = agentMap[name];
      if (!a) return null;
      const emoji = (AGENTS[name] || {}).emoji || '?';
      const state = a.cadence === 'paused' ? 'paused' : a.cadence === 'off' ? 'off' : (a.busy || 'idle');
      const icon = TOPIC_STATE_ICONS[state] || TOPIC_STATE_ICONS.idle;
      return `${emoji}${icon}`;
    }).filter(Boolean);

    const gov = data.governor || {};
    const topic = `${parts.join(' ')} · ${gov.mode || '?'} · ${gov.issues || 0}i ${gov.prs || 0}pr`;

    if (topic === this.lastTopic) return;
    this.lastTopic = topic;
    clearTimeout(this.topicTimer);
    this.topicTimer = setTimeout(() => this.setTopic(topic), TOPIC_DEBOUNCE_MS);
  }

  _diffGovernor(data) {
    if (!this.config.postGovernorModeChanges) return;
    const gov = data.governor || {};
    const prevGov = this.lastState.governor || {};
    const govMode = gov.mode || '';
    const prevMode = prevGov.mode || '';

    if (govMode && prevMode && govMode !== prevMode) {
      const { governorEmbed } = require('./formatter');
      const queueDepth = (gov.issues || 0) + (gov.prs || 0);
      const agentStates = {};
      for (const agent of (Array.isArray(data.agents) ? data.agents : [])) {
        if (agent.name) agentStates[agent.name] = agent.busy || agent.state || 'unknown';
      }
      this.sendEmbed(governorEmbed(`${prevMode} → ${govMode}`, queueDepth, agentStates), 'alerts');
    }
  }
}

module.exports = { DashboardBridge };
