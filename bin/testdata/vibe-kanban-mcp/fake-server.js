#!/usr/bin/env node
'use strict';

// Fake vibe-kanban MCP stdio server for bin/vibe-kanban-mirror.test.js.
//
// Speaks newline-delimited JSON-RPC like `npx -y vibe-kanban@latest --mcp` and
// implements only the tools the mirror calls, with the same argument and
// response shapes as upstream (crates/mcp/src/task_server/tools). Board state
// lives in the JSON file named by FAKE_VK_STATE so it survives across runs,
// which is what lets the test prove a rerun is idempotent. Every tools/call is
// appended to state.calls.

const fs = require('fs');
const readline = require('readline');

const statePath = process.env.FAKE_VK_STATE;
if (!statePath) {
  process.stderr.write('FAKE_VK_STATE is required\n');
  process.exit(2);
}

const STATUSES = ['Backlog', 'To do', 'In progress', 'In review', 'Done', 'Cancelled'];

function load() {
  if (!fs.existsSync(statePath)) return { projectId: '', issues: [], tags: [], issueTags: [], calls: [], seq: 0 };
  return JSON.parse(fs.readFileSync(statePath, 'utf8'));
}

function save(state) {
  fs.writeFileSync(statePath, JSON.stringify(state, null, 2));
}

function send(msg) {
  process.stdout.write(`${JSON.stringify(msg)}\n`);
}

function ok(data) {
  return { content: [{ type: 'text', text: JSON.stringify(data, null, 2) }] };
}

function fail(message, details) {
  const body = { success: false, error: message };
  if (details) body.details = details;
  return { content: [{ type: 'text', text: JSON.stringify(body, null, 2) }], isError: true };
}

function summary(issue) {
  return {
    id: issue.id,
    title: issue.title,
    simple_id: issue.simple_id,
    status: issue.status,
    pull_request_count: issue.latest_pr_url ? 1 : 0,
    latest_pr_url: issue.latest_pr_url || null,
    latest_pr_status: issue.latest_pr_status || null,
  };
}

function callTool(state, name, args) {
  const a = args || {};
  const project = a.project_id;
  switch (name) {
    case 'list_tags':
      if (project !== state.projectId) return fail('Project not found');
      return ok({ project_id: project, tags: state.tags });
    case 'list_issues': {
      if (project !== state.projectId) return fail('Project not found');
      const needle = String(a.search || '').toLowerCase();
      const issues = state.issues
        .filter((i) => !needle || `${i.title}\n${i.description || ''}`.toLowerCase().includes(needle))
        .slice(0, a.limit || 50)
        .map(summary);
      return ok({ issues, total_count: issues.length, returned_count: issues.length, limit: a.limit || 50, offset: 0, project_id: project });
    }
    case 'create_issue': {
      if (project !== state.projectId) return fail('Project not found');
      state.seq += 1;
      const issue = {
        id: `00000000-0000-4000-8000-${String(state.seq).padStart(12, '0')}`,
        simple_id: `VK-${state.seq}`,
        title: a.title,
        description: a.description || null,
        status: 'Backlog',
      };
      state.issues.push(issue);
      return ok({ issue_id: issue.id });
    }
    case 'update_issue': {
      const issue = state.issues.find((i) => i.id === a.issue_id);
      if (!issue) return fail('Issue not found');
      if (a.status !== undefined) {
        const match = STATUSES.find((s) => s.toLowerCase() === String(a.status).toLowerCase());
        if (!match) return fail(`Unknown status '${a.status}'. Available statuses: ${JSON.stringify(STATUSES)}`);
        issue.status = match;
      }
      if (a.title !== undefined) issue.title = a.title;
      if (a.description !== undefined) issue.description = a.description;
      return ok({ issue: { ...issue, tags: [] } });
    }
    case 'add_issue_tag': {
      if (!state.issues.some((i) => i.id === a.issue_id)) return fail('Issue not found');
      if (!state.tags.some((t) => t.id === a.tag_id)) return fail('Tag not found');
      state.issueTags.push({ issue_id: a.issue_id, tag_id: a.tag_id });
      return ok({ issue_tag_id: `it-${state.issueTags.length}` });
    }
    default:
      return null;
  }
}

const rl = readline.createInterface({ input: process.stdin });
rl.on('line', (line) => {
  if (!line.trim()) return;
  const msg = JSON.parse(line);
  if (msg.id === undefined || msg.id === null) return;
  if (msg.method === 'initialize') {
    send({
      jsonrpc: '2.0',
      id: msg.id,
      result: {
        protocolVersion: msg.params.protocolVersion,
        capabilities: { tools: {} },
        serverInfo: { name: 'fake-vibe-kanban', version: '0.0.0' },
      },
    });
    return;
  }
  if (msg.method === 'tools/call') {
    const state = load();
    state.calls.push({ name: msg.params.name, arguments: msg.params.arguments });
    const result = callTool(state, msg.params.name, msg.params.arguments);
    save(state);
    if (!result) {
      send({ jsonrpc: '2.0', id: msg.id, error: { code: -32602, message: `unknown tool ${msg.params.name}` } });
      return;
    }
    send({ jsonrpc: '2.0', id: msg.id, result });
    return;
  }
  send({ jsonrpc: '2.0', id: msg.id, error: { code: -32601, message: `method not found: ${msg.method}` } });
});
rl.on('close', () => process.exit(0));
