// Frozen pre-cutover browser oracle for Go classifier and CSV parity.
    function normalizeIssueBandConfig(raw) {
      const cfg = raw || {};
      const uniq = (arr, fallback) => {
        const out = [];
        (Array.isArray(arr) && arr.length ? arr : fallback).forEach(v => {
          const s = String(v || '').trim();
          if (s && !out.some(x => x.toLowerCase() === s.toLowerCase())) out.push(s);
        });
        return out;
      };
      const staleDays = Number(cfg.stale_days || cfg.staleDays || REPO_ISSUE_BAND_DEFAULTS.staleDays);
      return {
        waitingLabels: uniq(cfg.waiting_labels || cfg.waitingLabels, REPO_ISSUE_BAND_DEFAULTS.waitingLabels),
        doneLabels: uniq(cfg.done_labels || cfg.doneLabels, REPO_ISSUE_BAND_DEFAULTS.doneLabels),
        staleDays: Number.isFinite(staleDays) && staleDays > 0 ? staleDays : REPO_ISSUE_BAND_DEFAULTS.staleDays
      };
    }

    function repoIssueBandConfig() {
      return normalizeIssueBandConfig(window._repoIssueBandConfig || {});
    }

    function canonicalHiveHoldLabel() {
      const hiveId = String(((window._lastStatus || {}).hiveId) || '').trim();
      return hiveId ? ('hive-pause/' + hiveId) : 'hold';
    }

    function holdLabels(labels) {
      const canonical = canonicalHiveHoldLabel().toLowerCase();
      return (labels || []).map(l => String(l)).filter(l => {
        const lower = l.toLowerCase();
        return lower === canonical || HOLD_LABEL_SPELLINGS.some(s => lower.includes(s));
      });
    }

    function heldReason(item) {
      const labels = (item && item.labels ? item.labels : []).map(l => String(l));
      const held = holdLabels(labels);
      const needsHuman = labels.includes('needs-human');
      const parts = [];
      parts.push('On hold' + (held.length ? ' — label ' + held.map(l => '`' + l + '`').join(', ') : '') + ': agents will not act on this until the hold label is removed');
      if (item && item.hive_attributed) parts.push('opened by a hive agent — a generic `hold` here is usually the ACMM level gate; the dashboard hive-pause hold is only removed by an operator');
      if (needsHuman) parts.push('needs-human: automated fix attempts exhausted, a human must review this');
      return parts.join('; ');
    }

    function issueLabelSet(issue) {
      return new Set(((issue && issue.labels) || []).map(l => String(l).toLowerCase()));
    }

    function issueHasAnyLabel(issue, labels) {
      const set = issueLabelSet(issue);
      return (labels || []).some(l => set.has(String(l).toLowerCase()));
    }

    function issueAgentRole(issue) {
      for (const label of ((issue && issue.labels) || [])) {
        const raw = String(label || '');
        if (raw.toLowerCase().startsWith('agent/')) return raw.slice(raw.indexOf('/') + 1).trim();
      }
      return '';
    }

    function issueClaimed(issue) {
      const labels = ((issue && issue.labels) || []).map(l => String(l).toLowerCase());
      return labels.includes('claimed') || labels.some(l => l.startsWith('hive/claimed-by-'));
    }

    function issueAcknowledged(issue) {
      return !!(issue && issue.human_acknowledged) || issueLabelSet(issue).has('approved-direction');
    }

    function issueLinkedPRState(issue) {
      return null;
    }

    function issueUpdatedAt(issue) {
      const value = (issue && (issue.updated_at || issue.created_at)) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY;
    }

    function issueIsStale(issue) {
      const t = issueUpdatedAt(issue);
      if (!Number.isFinite(t)) return false;
      return (Date.now() - t) > repoIssueBandConfig().staleDays * MS_PER_DAY;
    }

    function issueBandInfo(issue) {
      const cfg = repoIssueBandConfig();
      const labels = issueLabelSet(issue);
      const linked = issueLinkedPRState(issue) || {};
      const role = issueAgentRole(issue);
      const acknowledged = issueAcknowledged(issue);
      const matches = {
        done: issueHasAnyLabel(issue, cfg.doneLabels) || !!linked.merged,
        waiting: issueHasAnyLabel(issue, cfg.waitingLabels),
        inProgress: ((issue && issue.assignees) || []).length > 0 || issueClaimed(issue) || !!linked.open,
        // Agent-filed is an operator queue only until a human acknowledges
        // the proposal (#5117); acknowledged issues fall through to the
        // claimed/unclaimed bands with the role badge still on the pill.
        agentFiled: !!role && !acknowledged
      };
      let band = 'ready';
      if (matches.done) band = 'done';
      else if (matches.waiting) band = 'waiting';
      else if (matches.inProgress) band = 'in-progress';
      else if (matches.agentFiled) band = 'agent-filed';
      const signals = [];
      if (labels.has('blocked')) signals.push({ glyph: '⛔', label: 'blocked' });
      if (labels.has('needs-decision') || labels.has('2-discussing')) signals.push({ glyph: '❓', label: 'needs decision' });
      if (labels.has('needs-human')) signals.push({ glyph: '⚠', label: 'needs human review' });
      if (labels.has('epic')) signals.push({ glyph: '◆', label: 'epic' });
      if (matches.inProgress) signals.push({ glyph: '👤', label: 'assigned or claimed' });
      if (linked.open) signals.push({ glyph: '🔗', label: 'open PR references this issue' });
      if (matches.done) signals.push({ glyph: '✓', label: linked.merged ? 'merged PR references this issue' : 'already done' });
      if (role) signals.push({ role: role, label: 'agent-filed by ' + role + (acknowledged ? ', acknowledged by a human' : ', not yet acknowledged') });
      if (issueIsStale(issue)) signals.push({ glyph: '🕒', label: 'stale: no activity > ' + cfg.staleDays + 'd' });
      return { band, role, matches, signals };
    }

    function issueBandSpec(band) {
      const cfg = repoIssueBandConfig();
      switch (band) {
        case 'in-progress': return { label: 'Claimed', short: 'claimed', rule: 'assigned, claimed by an agent, or an open PR references it — nothing needed unless it stalls' };
        case 'agent-filed': return { label: 'Needs triage', short: 'triage', rule: 'filed by an agent (agent/<role> label) with no approved-direction label, no human assignee, and self-authorization hold is on for the repo (ACMM < 6 or github.self_authorization_hold=true) — add the label, assign a human, or close it. A human comment also acknowledges for #5117 but is not in the snapshot, so a commented-on proposal still shows here' };
        case 'waiting': return { label: 'Needs human', short: 'needs human', rule: 'labelled ' + (cfg.waitingLabels || []).join(', ') + ' — a human must unblock or decide before agents continue' };
        case 'done': return { label: 'Confirm & close', short: 'close?', rule: 'an agent applied ' + (cfg.doneLabels || []).join(', ') + ' or a merged PR references it — verify the work landed and close the issue' };
        default: return { label: 'Unclaimed', short: 'unclaimed', rule: 'no other band matched — nobody is assigned, nothing claimed it, and no human gate applies; this does not by itself mean agents will pick it up' };
      }
    }

    function issueBandLabel(band) {
      return issueBandSpec(band).label;
    }

    function issueBandRule(band) {
      return issueBandSpec(band).rule;
    }

    function issueBandTip(band) {
      const spec = issueBandSpec(band);
      return spec.label + ': ' + spec.rule;
    }

    function issueBandRank(band) {
      return { ready: 0, 'in-progress': 1, 'agent-filed': 2, waiting: 3, done: 4 }[band] ?? 9;
    }

    function groupedRepoIssues(issues) {
      const groups = new Map();
      (issues || []).forEach(issue => {
        const info = issueBandInfo(issue);
        const key = info.band;
        if (!groups.has(key)) groups.set(key, { key, band: info.band, label: issueBandLabel(info.band), tip: issueBandTip(info.band), issues: [] });
        groups.get(key).issues.push({ issue, info });
      });
      const out = Array.from(groups.values());
      out.forEach(g => g.issues.sort((a, b) => issueUpdatedAt(a.issue) - issueUpdatedAt(b.issue) || Number(a.issue.number || 0) - Number(b.issue.number || 0)));
      out.sort((a, b) => issueBandRank(a.band) - issueBandRank(b.band));
      return out;
    }

    function overviewRepoName(repo) {
      return (repo && (repo.full || repo.name)) || '';
    }

    function overviewIssueBandSlices(repos) {
      const counts = new Map(OVERVIEW_ISSUE_BAND_ORDER.map(band => [band, 0]));
      const items = new Map(OVERVIEW_ISSUE_BAND_ORDER.map(band => [band, []]));
      (repos || []).forEach(r => {
        const repo = overviewRepoName(r);
        const actionable = (r.actionableIssues || []).map(issue => ({ issue, held: false }));
        const held = (r.heldIssues || []).map(issue => ({ issue, held: true }));
        const meta = new Map(actionable.concat(held).map(entry => [entry.issue, entry]));
        groupedRepoIssues(actionable.concat(held).map(entry => entry.issue)).forEach(g => {
          counts.set(g.band, (counts.get(g.band) || 0) + g.issues.length);
          const next = g.issues.map(entry => {
            const source = meta.get(entry.issue) || {};
            return Object.assign({}, entry.issue || {}, {
              item: entry.issue,
              issue: entry.issue,
              repo,
              held: !!source.held,
              info: entry.info,
              updated_at: (entry.issue && entry.issue.updated_at) || '',
              created_at: (entry.issue && entry.issue.created_at) || '',
              number: entry.issue && entry.issue.number
            });
          });
          items.set(g.band, (items.get(g.band) || []).concat(next));
        });
      });
      OVERVIEW_ISSUE_BAND_ORDER.forEach(band => {
        items.set(band, (items.get(band) || []).sort((a, b) => issueUpdatedAt(a.item || a.issue || a) - issueUpdatedAt(b.item || b.issue || b) || Number((a.item || a.issue || a).number || 0) - Number((b.item || b.issue || b).number || 0)));
      });
      return OVERVIEW_ISSUE_BAND_ORDER.map(band => ({
        key: band,
        label: issueBandLabel(band),
        rule: issueBandRule(band),
        count: counts.get(band) || 0,
        className: 'overview-issue-' + band,
        items: items.get(band) || []
      }));
    }

    function prLabelSet(pr) {
      return new Set(((pr && pr.labels) || []).map(l => String(l).toLowerCase()));
    }

    function prHasAnyLabel(pr, labels) {
      const set = prLabelSet(pr);
      return (labels || []).some(l => set.has(String(l).toLowerCase()));
    }

    function prQueued(pr) {
      const want = String(window._hiveAutoMergeLabel || 'lgtm').toLowerCase();
      return prLabelSet(pr).has(want);
    }

    function prAgentRole(pr) {
      if (pr && pr.hive_agent) return String(pr.hive_agent).trim();
      for (const label of ((pr && pr.labels) || [])) {
        const raw = String(label || '');
        if (raw.toLowerCase().startsWith('agent/')) return raw.slice(raw.indexOf('/') + 1).trim();
      }
      return pr && pr.app_authored ? 'app' : '';
    }

    function prUpdatedAt(pr) {
      const value = (pr && (pr.updated_at || pr.created_at)) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY;
    }

    function prCreatedAt(pr) {
      const value = (pr && pr.created_at) || '';
      const t = Date.parse(value);
      return Number.isFinite(t) ? t : Number.POSITIVE_INFINITY;
    }

    function prReviewClassRank(pr) {
      switch (String((pr && pr.review_class) || '')) {
        case 'fix': return 0;
        case 'tests': return 2;
        default: return 1;
      }
    }

    function prIsStale(pr) {
      const t = prUpdatedAt(pr);
      if (!Number.isFinite(t)) return false;
      return (Date.now() - t) > repoIssueBandConfig().staleDays * MS_PER_DAY;
    }

    function prCIFailing(pr) {
      const checks = (pr && pr.failing_checks) || [];
      return pr && String(pr.ci_status || '').toLowerCase() === 'failing' && Array.isArray(checks) && checks.length > 0;
    }

    function prGitHubReview(pr) {
      const p = (pr && pr.protection) || {};
      const changesBy = Array.isArray(p.changes_requested_by) ? p.changes_requested_by : [];
      const approvals = Number(p.approvals_given || 0);
      const who = changesBy.length ? ' by ' + changesBy.map(l => '@' + l).join(', ') : '';
      const given = approvals ? approvals + ' approval' + (approvals === 1 ? '' : 's') : '';
      switch (String(p.review_decision || '').toUpperCase()) {
        case 'APPROVED': return { decision: 'approved', glyph: '👍', label: 'approved on GitHub' + (given ? ' (' + given + ')' : '') };
        case 'CHANGES_REQUESTED': return { decision: 'changes-requested', glyph: '👎', label: 'changes requested' + who };
        case 'REVIEW_REQUIRED': return { decision: 'review-required', glyph: '👀', label: 'approving review required by branch protection' + (given ? ' (' + given + ' given)' : '') };
        default: break;
      }
      if (changesBy.length) return { decision: '', glyph: '👎', label: 'changes requested' + who + ' — no review decision from GitHub' };
      if (approvals) return { decision: '', glyph: '👍', label: given + ' on GitHub — no review decision' };
      return null;
    }

    function prRequestedReviews(pr) {
      const logins = (Array.isArray(pr && pr.requested_reviewers) ? pr.requested_reviewers : []).map(l => '@' + l);
      const teams = (Array.isArray(pr && pr.requested_teams) ? pr.requested_teams : []).map(t => 'team ' + t);
      return logins.concat(teams);
    }

    function prConversation(pr) {
      const comments = Number((pr && pr.comment_count) || 0);
      const threads = Number((pr && pr.review_thread_count) || 0);
      if (!comments && !threads) return null;
      const parts = [];
      if (comments) parts.push(comments + ' comment' + (comments === 1 ? '' : 's'));
      if (threads) parts.push(threads + ' review thread' + (threads === 1 ? '' : 's'));
      return { total: comments + threads, label: parts.join(', ') };
    }

    function prBandInfo(pr, held) {
      const cfg = repoIssueBandConfig();
      const labels = prLabelSet(pr);
      const v = pr && pr.merge_verdict ? pr.merge_verdict : {};
      const role = prAgentRole(pr);
      const review = prGitHubReview(pr);
      const matches = {
        waiting: !!held || prHasAnyLabel(pr, PR_HUMAN_GATE_LABELS.concat(cfg.waitingLabels || [])) || holdLabels(Array.from(labels)).length > 0,
        eligible: (v && v.state === 'eligible') || prQueued(pr),
        blocked: (v && v.state === 'blocked') || (pr && pr.mergeable === 'no') || prCIFailing(pr),
        inReview: (v && v.state === 'outstanding') || !!(pr && pr.review_url) || !!review,
        draft: !!(pr && pr.draft)
      };
      let band = 'open';
      if (matches.waiting) band = 'waiting';
      else if (matches.eligible) band = 'eligible';
      else if (matches.blocked) band = 'blocked';
      else if (matches.inReview) band = 'in-review';
      else if (matches.draft) band = 'draft';
      const signals = [];
      if (role) signals.push({ role: role, label: 'agent-authored by ' + role });
      if (labels.has('needs-human')) signals.push({ glyph: '⚠', label: 'needs human review' });
      if (labels.has('needs-decision') || labels.has('2-discussing')) signals.push({ glyph: '❓', label: 'needs decision' });
      if (held || holdLabels(Array.from(labels)).length > 0) signals.push({ glyph: '⏸', label: 'held' });
      if (matches.eligible) signals.push({ glyph: '✓', label: prQueued(pr) ? 'queued or merge-eligible' : 'merge-eligible' });
      if (matches.inReview) signals.push({ glyph: '◐', label: 'in review or outstanding' });
      if (review) signals.push({ glyph: review.glyph, label: review.label });
      const requested = prRequestedReviews(pr);
      if (requested.length) signals.push({ glyph: '👥', label: 'review requested from ' + requested.join(', ') });
      const convo = prConversation(pr);
      if (convo) signals.push({ glyph: '🗨 ' + convo.total, label: convo.label });
      if (prCIFailing(pr)) signals.push({ glyph: '✗ CI', label: 'failing CI: ' + ((pr.failing_checks || []).join(', ') || 'check failure') });
      if (pr && pr.mergeable === 'no') signals.push({ glyph: '⑂', label: 'not mergeable' + (pr.mergeable_state ? ': ' + pr.mergeable_state : '') });
      if (prQueued(pr)) signals.push({ glyph: '🔀', label: 'queued for auto-merge' });
      if (prIsStale(pr)) signals.push({ glyph: '🕒', label: 'stale: no activity > ' + cfg.staleDays + 'd' });
      return { band, role, held: !!held, matches, signals, review };
    }

    function prBandSpec(band) {
      const cfg = repoIssueBandConfig();
      switch (band) {
        case 'waiting': return { label: 'Needs human', rule: 'held, or labelled ' + Array.from(new Set(PR_HUMAN_GATE_LABELS.concat(cfg.waitingLabels || []).map(l => String(l).toLowerCase()))).join(', ') + ' — a human must review, decide, or release the hold before automation continues' };
        case 'eligible': return { label: 'Merge-eligible', rule: 'sweep verdict eligible, or queued for Hive auto-merge — nothing needed; it merges on its own' };
        case 'blocked': return { label: 'Blocked', rule: 'blocked sweep verdict, merge conflicts, or failing CI — read the verdict, rebase, or fix the checks' };
        case 'in-review': return { label: 'In review', rule: 'outstanding sweep verdict, a Hive review posted, or a GitHub review decision — nothing needed until the review resolves' };
        case 'draft': return { label: 'Draft', rule: 'draft on GitHub — nothing needed until it is marked ready for review' };
        default: return { label: 'Open', rule: 'no verdict, review, hold, or draft flag yet — an ordinary open PR' };
      }
    }

    function prBandLabel(band) {
      return prBandSpec(band).label;
    }

    function prBandRule(band) {
      return prBandSpec(band).rule;
    }

    function prBandTip(band) {
      const spec = prBandSpec(band);
      return spec.label + ': ' + spec.rule;
    }

    function prBandRank(band) {
      const idx = PR_BAND_ORDER.indexOf(band);
      return idx >= 0 ? idx : PR_BAND_ORDER.length;
    }

    function groupedRepoPRs(openPrs, heldPrs) {
      const groups = new Map();
      const entries = (openPrs || []).map(pr => ({ pr, held: false })).concat((heldPrs || []).map(pr => ({ pr, held: true })));
      entries.forEach(entry => {
        const info = prBandInfo(entry.pr, entry.held);
        const key = info.band;
        if (!groups.has(key)) groups.set(key, { key, band: info.band, label: prBandLabel(info.band), tip: prBandTip(info.band), prs: [] });
        groups.get(key).prs.push({ pr: entry.pr, held: entry.held, info });
      });
      const out = Array.from(groups.values());
      out.forEach(g => g.prs.sort((a, b) => prUpdatedAt(a.pr) - prUpdatedAt(b.pr) || prReviewClassRank(a.pr) - prReviewClassRank(b.pr) || prCreatedAt(a.pr) - prCreatedAt(b.pr) || Number(a.pr.number || 0) - Number(b.pr.number || 0)));
      out.sort((a, b) => prBandRank(a.band) - prBandRank(b.band));
      return out;
    }

    function overviewPRBandSlices(repos) {
      const counts = new Map(PR_BAND_ORDER.map(band => [band, 0]));
      const items = new Map(PR_BAND_ORDER.map(band => [band, []]));
      (repos || []).forEach(r => {
        const repo = overviewRepoName(r);
        groupedRepoPRs(r.openPrs || [], r.heldPrs || []).forEach(g => {
          counts.set(g.band, (counts.get(g.band) || 0) + g.prs.length);
          const next = g.prs.map(entry => Object.assign({}, entry.pr || {}, {
            item: entry.pr,
            pr: entry.pr,
            repo,
            held: !!entry.held,
            info: entry.info,
            updated_at: (entry.pr && entry.pr.updated_at) || '',
            created_at: (entry.pr && entry.pr.created_at) || '',
            number: entry.pr && entry.pr.number
          }));
          items.set(g.band, (items.get(g.band) || []).concat(next));
        });
      });
      PR_BAND_ORDER.forEach(band => {
        items.set(band, (items.get(band) || []).sort((a, b) => prUpdatedAt(a.item || a.pr || a) - prUpdatedAt(b.item || b.pr || b) || prReviewClassRank(a.item || a.pr || a) - prReviewClassRank(b.item || b.pr || b) || prCreatedAt(a.item || a.pr || a) - prCreatedAt(b.item || b.pr || b) || Number((a.item || a.pr || a).number || 0) - Number((b.item || b.pr || b).number || 0)));
      });
      return PR_BAND_ORDER.map(band => ({
        key: band,
        label: prBandLabel(band),
        rule: prBandRule(band),
        count: counts.get(band) || 0,
        className: 'overview-pr-' + band,
        items: items.get(band) || []
      }));
    }

    function overviewCsvCell(v) {
      if (Array.isArray(v)) v = v.map(x => x == null ? '' : String(x)).join(';');
      if (v == null) v = '';
      if (typeof v === 'boolean') v = v ? 'true' : 'false';
      let text = String(v);
      // Spreadsheet formula injection (CWE-1236): a leading =, +, -, @, tab or
      // CR makes Excel/Sheets evaluate the cell as a formula, and titles/labels
      // in these rows are written by anyone who can open an issue or PR on a
      // watched repo. Neutralize with a leading apostrophe; plain numbers
      // (e.g. -5) stay untouched so numeric columns still sort.
      if (/^[=+\-@\t\r]/.test(text) && !/^-?\d+(\.\d+)?$/.test(text)) text = "'" + text;
      return /[",\r\n]/.test(text) ? '"' + text.replace(/"/g, '""') + '"' : text;
    }

    function overviewCsv(rows, columns) {
      const lineBreak = String.fromCharCode(13, 10);
      const header = (columns || []).map(overviewCsvCell).join(',');
      const body = (rows || []).map(row => (columns || []).map(col => overviewCsvCell(row ? row[col] : '')).join(','));
      return String.fromCharCode(0xfeff) + [header].concat(body).join(lineBreak) + lineBreak;
    }

    function overviewCsvColumns(kind) {
      if (kind === 'pr' || kind === 'prs') return ['band', 'band_rule', 'repo', 'number', 'title', 'url', 'author', 'draft', 'merge_verdict', 'mergeable', 'failing_checks', 'review_decision', 'review_class', 'held', 'hold_reason', 'labels', 'updated_at', 'stale', 'signals'];
      return ['band', 'band_rule', 'repo', 'number', 'title', 'url', 'state_signals', 'labels', 'assignees', 'agent_role', 'acknowledged', 'held', 'hold_reason', 'linked_prs', 'updated_at', 'stale'];
    }

    function overviewItemUrl(item, kind) {
      const raw = (item && (item.item || item.issue || item.pr)) || item || {};
      if (raw.url) return raw.url;
      const repo = (item && item.repo) || raw.repo || '';
      if (!repo || !raw.number) return '';
      const base = String(window._githubBaseUrl || 'https://github.com').replace(/\/$/, '');
      return base + '/' + repo + '/' + (kind === 'pr' ? 'pull' : 'issues') + '/' + raw.number;
    }

    function overviewSignalLabels(info) {
      return ((info && info.signals) || []).map(s => s && s.label ? s.label : '').filter(Boolean);
    }

    function overviewLinkedPRs(issue) {
      return (Array.isArray(issue && issue.linked_prs) ? issue.linked_prs : []).map(pr => {
        const num = pr && pr.number ? '#' + pr.number : '';
        const state = pr && (pr.merged ? 'merged' : pr.state) ? ' ' + (pr.merged ? 'merged' : pr.state) : '';
        const repo = pr && pr.repo ? pr.repo : '';
        return (repo ? repo : '') + num + state;
      }).filter(Boolean);
    }

    function overviewMergeVerdictText(pr) {
      const v = pr && pr.merge_verdict;
      if (!v) return '';
      if (typeof v === 'string') return v;
      if (v.state && v.reason) return String(v.state) + ': ' + String(v.reason);
      return v.state || v.reason || '';
    }

    function overviewReviewDecision(pr) {
      return (pr && pr.protection && pr.protection.review_decision) || pr.review_decision || '';
    }

    function overviewIssueCsvRow(item, band) {
      const issue = (item && (item.item || item.issue)) || item || {};
      const info = (item && item.info) || issueBandInfo(issue);
      const key = band || info.band;
      const held = !!(item && item.held);
      return {
        band: issueBandLabel(key),
        band_rule: issueBandRule(key),
        repo: (item && item.repo) || issue.repo || '',
        number: issue.number || '',
        title: issue.title || '',
        url: overviewItemUrl(item || issue, 'issue'),
        state_signals: overviewSignalLabels(info),
        labels: (issue.labels || []).map(l => String(l)),
        assignees: (issue.assignees || []).map(a => String(a)),
        agent_role: issueAgentRole(issue),
        acknowledged: issueAcknowledged(issue),
        held,
        hold_reason: held ? heldReason(issue) : '',
        linked_prs: overviewLinkedPRs(issue),
        updated_at: issue.updated_at || '',
        stale: issueIsStale(issue)
      };
    }

    function overviewPRCsvRow(item, band) {
      const pr = (item && (item.item || item.pr)) || item || {};
      const held = !!(item && item.held);
      const info = (item && item.info) || prBandInfo(pr, held);
      const key = band || info.band;
      return {
        band: prBandLabel(key),
        band_rule: prBandRule(key),
        repo: (item && item.repo) || pr.repo || '',
        number: pr.number || '',
        title: pr.title || '',
        url: overviewItemUrl(item || pr, 'pr'),
        author: pr.author || '',
        draft: !!pr.draft,
        merge_verdict: overviewMergeVerdictText(pr),
        mergeable: pr.mergeable || '',
        failing_checks: (pr.failing_checks || []).map(c => String(c)),
        review_decision: overviewReviewDecision(pr),
        review_class: pr.review_class || '',
        held,
        hold_reason: held ? heldReason(pr) : '',
        labels: (pr.labels || []).map(l => String(l)),
        updated_at: pr.updated_at || '',
        stale: prIsStale(pr),
        signals: overviewSignalLabels(info)
      };
    }

    function overviewCsvRows(kind, band) {
      const isPR = kind === 'pr' || kind === 'prs';
      const slices = isPR ? overviewPRBandSlices(_overviewLastRepos || []) : overviewIssueBandSlices(_overviewLastRepos || []);
      return slices.filter(s => !band || s.key === band).flatMap(s => (s.items || []).map(item => isPR ? overviewPRCsvRow(item, s.key) : overviewIssueCsvRow(item, s.key)));
    }