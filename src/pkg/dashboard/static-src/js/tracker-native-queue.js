    // Forge-owned queues are observations, not seats in Hive's preparation
    // train and not pipeline rows. This module never moves or mutates items.
    //
    // Presentation (hivecommons/hive#18702): overview first, details on
    // demand. One head line for the whole panel, one status line per branch
    // that has something to say, calm branches folded into a count, and every
    // caveat stated once in the head's help mark. Age ("checked 9m ago") is
    // the normal way to show old evidence; warning styling is kept for a
    // failed refresh, missing authentication, or evidence far older than the
    // rotation explains. Nothing here claims more than the observation knows.
    const NATIVE_QUEUE_SECOND_MS = 1000;
    const NATIVE_QUEUE_MINUTE_S = 60;
    // The background reader refreshes this many branches per pass; a full
    // rotation over N branches therefore takes ceil(N / this) passes.
    const NATIVE_QUEUE_SCOPES_PER_PASS = 2;
    // Evidence is "overdue" (a real problem, not a rotation delay) once it is
    // older than the fresh window plus this many full rotations.
    const NATIVE_QUEUE_OVERDUE_ROTATIONS = 2;
    // fresh_for_s is twice the reader's interval (see stage_tracker_native_queue.go).
    const NATIVE_QUEUE_FRESH_INTERVALS = 2;
    const NATIVE_QUEUE_SHA_SHORT = 12;
    // One line on purpose: the render test lifts it with jsConstLine.
    const NATIVE_QUEUE_HELP = 'What this panel knows. GitHub owns these queues; Hive only reads them, at most two branches every three minutes, so a branch is normally re-checked every few rotations and "checked 9m ago" is expected, not a fault. Queue membership comes from GitHub’s entry list only, never from a PR number in a ref name or from queue position. A test group’s checks are the checks on its own synthetic commit, never the PR’s; missing, pending, skipped or unreadable checks are never shown as passing, and passing checks are not a merge receipt. An empty list in an old snapshot does not prove the queue is empty now. Finished groups are counted, not described: a vanished group ref does not say whether it merged, failed or was rebuilt; only a merged receipt means merged. The repo filter applies here; PR references say whether that exact PR and head is on this page, on another page, or outside the snapshot.';

    function stageTrackerNativeAgeMs(value, nowMs) {
      const at = Date.parse(value || '');
      if (!Number.isFinite(at) || at > nowMs) return NaN;
      return nowMs - at;
    }
    function stageTrackerNativeAge(value, nowMs) {
      const ms = stageTrackerNativeAgeMs(value, nowMs);
      if (!Number.isFinite(ms)) return 'age unknown';
      const minutes = Math.floor(ms / (NATIVE_QUEUE_SECOND_MS * NATIVE_QUEUE_MINUTE_S));
      return minutes < 1 ? 'less than 1m' : minutes + 'm';
    }
    function stageTrackerNativeChecked(value, nowMs) {
      const ms = stageTrackerNativeAgeMs(value, nowMs);
      if (!Number.isFinite(ms)) return 'not checked yet';
      return ms < NATIVE_QUEUE_SECOND_MS * NATIVE_QUEUE_MINUTE_S ? 'checked just now' : 'checked ' + stageTrackerNativeAge(value, nowMs) + ' ago';
    }

    function stageTrackerNativeLink(url, label) {
      try {
        const parsed = new URL(url);
        if (parsed.protocol === 'https:') return `<a href="${esc(parsed.href)}" target="_blank" rel="noopener noreferrer">${esc(label)}</a>`;
      } catch (_) {}
      return esc(label);
    }

    function stageTrackerNativeIdentities(items) {
      const result = [];
      (items || []).forEach(item => {
        if (!item) return;
        if (item.kind === 'pr') result.push(item);
        (item.linked_prs || []).forEach(pr => result.push(Object.assign({ repo: item.repo }, pr)));
      });
      return result;
    }

    // Where this exact repo/PR/head/base is relative to the page: the honesty
    // word the design doc requires, now one quiet span rather than a sentence.
    function stageTrackerNativeWhere(member, observation, all, page) {
      const same = item => String(item.repo || '').toLowerCase() === String(observation.repo || '').toLowerCase()
        && Number(item.number) === Number(member.pr) && item.head === member.head && item.base_ref === observation.base;
      return page.some(same) ? 'on this page' : all.some(same) ? 'on another page' : 'outside snapshot or head changed';
    }
    function stageTrackerNativePrLink(observation, pr) {
      const repo = String(observation.repo || '');
      const url = /^[-\w.]+\/[-\w.]+$/.test(repo) ? `https://github.com/${repo}/pull/${Number(pr)}` : '';
      return stageTrackerNativeLink(url, '#' + Number(pr));
    }
    function stageTrackerNativeMember(member, observation, all, page) {
      const head = String(member.head || '');
      return `${stageTrackerNativePrLink(observation, member.pr)} <code title="${esc(head)}">${esc(head.slice(0, NATIVE_QUEUE_SHA_SHORT) || 'head unknown')}</code> <span class="ham-merged-meta">${esc(stageTrackerNativeWhere(member, observation, all, page))}</span>`;
    }

    // GitHub's MergeQueueEntryState slugs in plain words; an unknown slug is
    // shown lower-cased rather than invented.
    function stageTrackerNativeEntryWord(state) {
      const slug = String(state || '').toUpperCase();
      switch (slug) {
        case 'AWAITING_CHECKS': return 'awaiting checks';
        case 'QUEUED': return 'queued';
        case 'MERGEABLE': return 'ready to merge';
        case 'LOCKED': return 'locked';
        case 'UNMERGEABLE': return 'cannot merge';
        case '': return 'state unknown';
        default: return slug.toLowerCase().replace(/_/g, ' ');
      }
    }
    // Candidate CI verdicts in plain words. "unknown" and "unavailable" stay
    // visibly unknown; nothing here reads as passing unless it is.
    function stageTrackerNativeCiWord(candidate) {
      switch (String(candidate.ci || '')) {
        case 'passed': return 'checks passed';
        case 'pending': return 'checks running';
        case 'failed': return 'checks failed';
        case 'blocked': return 'checks blocked';
        case 'destroyed': return 'group destroyed';
        case 'unavailable': return 'checks unreadable';
        default: return candidate.checks_known ? 'checks unknown' : 'required checks unknown (policy unreadable)';
      }
    }
    // A finished group (synthetic ref gone or moved) is history, not a live
    // candidate. Older servers sent no flag, only the reason sentence.
    function stageTrackerNativeCandidateLive(candidate) {
      if (!candidate) return false;
      if (candidate.expired) return false;
      return !/^Synthetic ref unavailable/.test(String(candidate.reason || ''));
    }

    // Pure: the facts one branch's status line and badges are built from.
    function stageTrackerNativeScopeFacts(q, nowMs, totalScopes) {
      const entries = Array.isArray(q.entries) ? q.entries : [];
      const candidates = Array.isArray(q.candidates) ? q.candidates : [];
      const live = candidates.filter(stageTrackerNativeCandidateLive);
      const finished = candidates.length - live.length;
      const outcomes = Array.isArray(q.outcomes) ? q.outcomes : [];
      const ageMs = stageTrackerNativeAgeMs(q.observed_at, nowMs);
      const freshMs = (Number(q.fresh_for_s) || 0) * NATIVE_QUEUE_SECOND_MS;
      const stale = !!q.stale || !Number.isFinite(ageMs) || !freshMs || ageMs > freshMs;
      const rotationMs = Math.ceil(Math.max(1, totalScopes) / NATIVE_QUEUE_SCOPES_PER_PASS) * (freshMs / NATIVE_QUEUE_FRESH_INTERVALS);
      const overdue = Number.isFinite(ageMs) && freshMs > 0 && ageMs > freshMs + NATIVE_QUEUE_OVERDUE_ROTATIONS * rotationMs;
      const state = String(q.state || 'unknown');
      const reason = String(q.reason || '');
      // Real problems only: a failed refresh or missing authentication keeps
      // the state at "unavailable"; a scope not yet visited is "unknown".
      let problem = '';
      if (state === 'unavailable') problem = /authentication/i.test(reason) ? 'GitHub App auth unavailable' : 'refresh failed';
      else if (overdue) problem = 'not checked for ' + stageTrackerNativeAge(q.observed_at, nowMs);
      const failing = live.some(c => c.ci === 'failed');
      const calm = !problem && !entries.length && !live.length && (state === 'observed' || state === 'not-enabled' || state === 'unknown');
      return { entries, live, finished, outcomes, ageMs, stale, overdue, state, reason, problem, failing, calm,
        complete: !!q.complete, entriesComplete: !!q.entries_complete };
    }

    // Pure: one status line per branch. It answers "is anything queued, what
    // is at the front" and never claims an empty queue from old evidence.
    function stageTrackerNativeScopeLine(q, facts, nowMs) {
      if (facts.state === 'not-enabled') return 'no merge queue on this branch';
      if (facts.state === 'unknown' && !facts.entries.length) return facts.ageMs >= 0 ? 'queue state unknown' : 'waiting for its first check';
      if (facts.state === 'unavailable' && !facts.entries.length) return 'queue unknown; last refresh failed';
      const parts = [];
      if (facts.entries.length) {
        const front = facts.entries.slice().sort((a, b) => (Number(a.position) || 0) - (Number(b.position) || 0))[0];
        parts.push(facts.entries.length + (facts.entries.length === 1 ? ' PR queued' : ' PRs queued') + (facts.entriesComplete ? '' : ' (order partial)'));
        parts.push('front #' + Number(front.pr) + ' ' + stageTrackerNativeEntryWord(front.state)
          + (front.enqueued_at ? ', in queue ' + stageTrackerNativeAge(front.enqueued_at, nowMs) : ''));
      } else {
        parts.push(facts.entriesComplete && !facts.stale ? 'nothing queued' : 'nothing queued at last check');
      }
      if (facts.live.length) {
        const words = facts.live.map(stageTrackerNativeCiWord);
        parts.push(facts.live.length === 1 ? 'testing: ' + words[0] : facts.live.length + ' groups testing: ' + words.join(', '));
      }
      return parts.join(' · ');
    }

    function stageTrackerNativeHelpMark(id, text) {
      return `<span class="config-info ham-native-help" tabindex="0" data-stop="1" aria-label="${esc(text)}" aria-describedby="${esc(id)}"><span class="config-tooltip" id="${esc(id)}" role="tooltip">${esc(text)}</span></span>`;
    }
    function stageTrackerNativeScopeName(q) {
      return q.repo ? esc(q.repo) + ' → ' + esc(q.base || 'base unknown') : 'scope not configured';
    }
    function stageTrackerNativeBadges(facts) {
      const out = [];
      if (facts.problem) out.push(`<span class="ham-badge ham-native-badge-problem">${esc(facts.problem)}</span>`);
      if (facts.failing) out.push('<span class="ham-badge ham-native-badge-problem">checks failed</span>');
      if (!facts.problem && facts.stale && facts.entries.length) out.push('<span class="ham-badge ham-native-badge-notice">awaiting re-check</span>');
      return out.join('');
    }

    // Details on demand: the full entry order, live test groups with their
    // checks, finished groups as a count, and lifecycle receipts.
    function stageTrackerNativeScopeDetails(q, facts, all, page, nowMs) {
      const blocks = [];
      if (facts.entries.length) {
        blocks.push(`<ol class="ham-merged-list ham-queue-list ham-native-entries">${facts.entries.map(entry => `<li value="${Number(entry.position) || 1}"><span class="ham-merged-num">${esc(String(Number(entry.position) || ''))}.</span><span class="ham-native-entry">${stageTrackerNativeMember(entry, q, all, page)}</span><span class="ham-merged-meta">${esc(stageTrackerNativeEntryWord(entry.state))}${entry.enqueued_at ? ' · ' + esc(stageTrackerNativeAge(entry.enqueued_at, nowMs)) : ''}</span></li>`).join('')}</ol>`);
      }
      facts.live.forEach(candidate => {
        const proven = candidate.membership === 'proven' && Array.isArray(candidate.members) && candidate.members.length > 0;
        const sha = String(candidate.sha || '');
        const checks = Array.isArray(candidate.checks) ? candidate.checks : [];
        blocks.push(`<div class="ham-native-group"><div class="ham-native-group-head">Test group <code title="${esc(sha)}">${esc(sha.slice(0, NATIVE_QUEUE_SHA_SHORT) || 'SHA unknown')}</code> · ${esc(stageTrackerNativeCiWord(candidate))}</div>`
          + (proven ? `<ul class="ham-merged-list ham-native-members">${candidate.members.map(member => `<li><span class="ham-native-entry">${stageTrackerNativeMember(member, q, all, page)}</span></li>`).join('')}</ul>` : '<div class="ham-merged-meta">members not proven</div>')
          + (checks.length ? `<ul class="ham-merged-list ham-native-checks">${checks.map(check => `<li><span class="ham-native-entry">${stageTrackerNativeLink(check.url, check.name)}</span><span class="ham-merged-meta">${esc(check.status || 'unknown')}${check.conclusion ? ' · ' + esc(check.conclusion) : ''}</span></li>`).join('')}</ul>` : '')
          + '</div>');
      });
      if (facts.finished) blocks.push(`<div class="ham-merged-meta">${esc(String(facts.finished))} finished ${facts.finished === 1 ? 'group' : 'groups'} (outcome not observed)</div>`);
      if (facts.outcomes.length) {
        blocks.push(`<ul class="ham-merged-list ham-native-receipts">${facts.outcomes.map(outcome => `<li><span class="ham-native-entry">${esc(outcome.kind)}${outcome.pr ? ' ' + stageTrackerNativePrLink(q, outcome.pr) : ''} <code>${esc(String(outcome.candidate_sha || outcome.merge_sha || outcome.head || '').slice(0, NATIVE_QUEUE_SHA_SHORT))}</code></span><span class="ham-merged-meta">${esc(stageTrackerNativeAge(outcome.observed_at, nowMs))} ago · ${esc(outcome.source)}</span></li>`).join('')}</ul>`);
      }
      if (!facts.complete && facts.reason) blocks.push(`<div class="ham-merged-meta">${esc(facts.reason)}</div>`);
      if (!blocks.length) return '';
      const summary = [facts.entries.length ? facts.entries.length + (facts.entries.length === 1 ? ' entry' : ' entries') : '',
        facts.live.length ? facts.live.length + (facts.live.length === 1 ? ' test group' : ' test groups') : '',
        facts.finished ? facts.finished + ' finished' : '',
        facts.outcomes.length ? facts.outcomes.length + (facts.outcomes.length === 1 ? ' receipt' : ' receipts') : ''].filter(Boolean).join(', ');
      return `<details class="ham-native-details"><summary>${esc(summary || 'details')}</summary>${blocks.join('')}</details>`;
    }

    function stageTrackerNativeScopeRow(q, facts, all, page, nowMs) {
      return `<li class="ham-native-row${facts.problem || facts.failing ? ' ham-native-row-problem' : ''}"><span class="ham-native-scope">${stageTrackerNativeScopeName(q)}</span>`
        + `<span class="ham-native-line">${esc(stageTrackerNativeScopeLine(q, facts, nowMs))}${stageTrackerNativeBadges(facts)}</span>`
        + `<span class="ham-merged-meta">${esc(stageTrackerNativeChecked(q.observed_at, nowMs))}</span>`
        + stageTrackerNativeScopeDetails(q, facts, all, page, nowMs) + '</li>';
    }

    function stageTrackerNativeQueueHtml(observations, items, pageItems, nowMs, repoFilter) {
      const all = stageTrackerNativeIdentities(items), page = stageTrackerNativeIdentities(pageItems);
      const total = (observations || []).length;
      const queues = (observations || []).filter(q => !repoFilter || !q.repo || q.repo.toLowerCase() === repoFilter.toLowerCase());
      if (!queues.length) return '';
      const scopes = queues.map(q => ({ q, facts: stageTrackerNativeScopeFacts(q, nowMs, total) }));
      const queued = scopes.reduce((n, s) => n + s.facts.entries.length, 0);
      const testing = scopes.reduce((n, s) => n + s.facts.live.length, 0);
      const problems = scopes.filter(s => s.facts.problem || s.facts.failing).length;
      const newest = scopes.map(s => s.facts.ageMs).filter(Number.isFinite).sort((a, b) => a - b)[0];
      const branches = queues.length === 1 ? '1 branch' : queues.length + ' branches';
      // "nothing queued" is only said about branches that were actually read:
      // not one still waiting for its first visit, nor one whose refresh failed.
      const read = scopes.filter(s => s.facts.state === 'observed' || s.facts.state === 'not-enabled').length;
      const headline = queued ? queued + (queued === 1 ? ' PR queued' : ' PRs queued') + ' on ' + branches
        : read === queues.length ? 'nothing queued on ' + branches
        : read ? 'nothing queued on ' + read + ' of ' + queues.length + ' branches' : branches;
      const summary = [headline,
        testing ? testing + (testing === 1 ? ' group testing' : ' groups testing') : '',
        problems ? problems + (problems === 1 ? ' branch needs attention' : ' branches need attention') : '',
        Number.isFinite(newest) ? 'last ' + stageTrackerNativeChecked(new Date(nowMs - newest).toISOString(), nowMs) : 'not checked yet'].filter(Boolean).join(' · ');
      const head = `<div class="ham-train-head ham-native-head"><span class="ham-train-chip">⛓ GitHub merge queues</span><span class="ham-native-sum">${esc(summary)}</span>${stageTrackerNativeHelpMark('ham-native-help-tip', NATIVE_QUEUE_HELP)}</div>`;
      const active = scopes.filter(s => !s.facts.calm);
      const calm = scopes.filter(s => s.facts.calm);
      const rows = active.length ? `<ol class="ham-merged-list ham-queue-list ham-native-list">${active.map(s => stageTrackerNativeScopeRow(s.q, s.facts, all, page, nowMs)).join('')}</ol>` : '';
      let folded = '';
      if (calm.length) {
        // A branch never visited yet is folded too, but counted honestly: it
        // is "not checked yet", not "nothing queued".
        const unchecked = calm.filter(s => s.facts.state === 'unknown').length;
        const empty = calm.length - unchecked;
        const label = calm.length === 1 && !active.length ? '' : [empty ? (empty === 1 ? '1 branch' : empty + ' branches') + ' with nothing queued' : '',
          unchecked ? (unchecked === 1 ? '1 branch' : unchecked + ' branches') + ' not checked yet' : ''].filter(Boolean).join(', ');
        const list = `<ul class="ham-merged-list ham-native-list ham-native-calm">${calm.map(s => `<li><span class="ham-native-scope">${stageTrackerNativeScopeName(s.q)}</span><span class="ham-native-line">${esc(stageTrackerNativeScopeLine(s.q, s.facts, nowMs))}</span><span class="ham-merged-meta">${esc(stageTrackerNativeChecked(s.q.observed_at, nowMs))}</span>${stageTrackerNativeScopeDetails(s.q, s.facts, all, page, nowMs)}</li>`).join('')}</ul>`;
        folded = label ? `<details class="ham-native-details ham-native-folded"><summary>${esc(label)}</summary>${list}</details>` : list;
      }
      return head + rows + folded;
    }

    function stageTrackerNativeQueueSync(data, pageRows, nowMs, repoFilter) {
      const root = document.getElementById('activity-monitor-native-queue');
      if (!root) return;
      const rows = (pageRows || []).map(row => row.item || row).filter(Boolean);
      const html = stageTrackerNativeQueueHtml((data || {}).integration_queues || [], (data || {}).items || [], rows, nowMs, repoFilter);
      root.hidden = !html;
      setIfChanged(root, html);
    }
