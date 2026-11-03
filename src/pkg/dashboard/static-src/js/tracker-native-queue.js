    // Forge-owned queues are observations, not seats in Hive's preparation
    // train and not pipeline rows. This module never moves or mutates items.
    const NATIVE_QUEUE_SECOND_MS = 1000;
    const NATIVE_QUEUE_MINUTE_S = 60;
    function stageTrackerNativeAge(value, nowMs) {
      const at = Date.parse(value || '');
      if (!Number.isFinite(at) || at > nowMs) return 'age unknown';
      return Math.floor((nowMs - at) / (NATIVE_QUEUE_SECOND_MS * NATIVE_QUEUE_MINUTE_S)) + 'm';
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

    function stageTrackerNativeMember(member, observation, all, page) {
      const same = item => String(item.repo || '').toLowerCase() === String(observation.repo || '').toLowerCase()
        && Number(item.number) === Number(member.pr) && item.head === member.head && item.base_ref === observation.base;
      const where = page.some(same) ? 'on this page' : all.some(same) ? 'outside current page/filters' : 'outside tracker snapshot or head changed';
      const repo = String(observation.repo || '');
      const url = /^[-\w.]+\/[-\w.]+$/.test(repo) ? `https://github.com/${repo}/pull/${member.pr}` : '';
      return `${stageTrackerNativeLink(url, '#' + member.pr)} <code title="${esc(member.head || '')}">${esc(String(member.head || '').slice(0, 12) || 'head unknown')}</code> <span class="nq-muted">${esc(where)}</span>`;
    }

    function stageTrackerNativeQueueHtml(observations, items, pageItems, nowMs, repoFilter) {
      const all = stageTrackerNativeIdentities(items), page = stageTrackerNativeIdentities(pageItems);
      const queues = (observations || []).filter(q => !repoFilter || !q.repo || q.repo.toLowerCase() === repoFilter.toLowerCase());
      if (!queues.length) return '';
      return '<h3>Forge-owned merge queues</h3><p class="nq-muted">Hive admits and repairs; the forge validates and merges. Separate from preparation/readiness and the actual pipeline rows. No ETA is inferred.</p>'
        + queues.map(q => {
          const observed = Date.parse(q.observed_at || '');
          const stale = q.stale || !Number.isFinite(observed) || !q.fresh_for_s || nowMs - observed > q.fresh_for_s * NATIVE_QUEUE_SECOND_MS;
          const entries = Array.isArray(q.entries) ? q.entries : [];
          const candidates = Array.isArray(q.candidates) ? q.candidates : [];
          const outcomes = Array.isArray(q.outcomes) ? q.outcomes : [];
          const label = q.provider === 'github-native' ? 'GitHub merge queue' : String(q.provider || 'Unknown provider');
          return `<section class="nq-scope"><h4>${esc(label)} · ${esc(q.repo || 'scope unknown')} ${esc(q.base || '')}</h4>
            <p class="nq-muted">${esc(q.state || 'unknown')} · ${stale ? '<strong>STALE / unavailable as current evidence</strong>' : 'observed ' + esc(stageTrackerNativeAge(q.observed_at, nowMs)) + ' ago'} · ${q.complete ? 'bounded snapshot complete' : 'coverage incomplete / unknown'}.
            ${esc(q.reason || '')} Queue references remain visible across row filters; repo filter applies here.</p>
            <ol class="nq-entries">${entries.map(entry => `<li value="${Number(entry.position) || 1}">${stageTrackerNativeMember(entry, q, all, page)} · ${esc(entry.state || 'state unknown')} · queued ${esc(stageTrackerNativeAge(entry.enqueued_at, nowMs))}</li>`).join('')}</ol>
            ${!entries.length ? '<p class="nq-muted">' + (q.complete && !stale ? 'No entries observed in this snapshot.' : 'Entry order unavailable; this does not prove an empty queue.') + '</p>' : ''}
            <div class="nq-candidates">${candidates.map(candidate => {
              const proven = candidate.membership === 'proven' && Array.isArray(candidate.members) && candidate.members.length > 0;
              return `<article class="nq-candidate"><h5>Validation candidate <code title="${esc(candidate.sha || '')}">${esc(String(candidate.sha || '').slice(0, 12) || 'SHA unknown')}</code></h5>
                <p class="nq-muted">${esc(candidate.ref || 'ref unknown')}<br>Base <code>${esc(candidate.base_sha || 'unknown')}</code> · tree <code>${esc(candidate.tree || 'unknown')}</code></p>
                <p>${proven ? 'Exact member heads proven at snapshot time by event base, live chain and ancestry' : '<strong>Member mapping unknown — never inferred from FIFO</strong>'}</p>
                ${proven ? '<ul>' + candidate.members.map(member => '<li>' + stageTrackerNativeMember(member, q, all, page) + '</li>').join('') + '</ul>' : ''}
                <p class="nq-muted">${esc(candidate.reason || '')}</p>
                <details><summary>Candidate required CI: ${esc(candidate.ci || 'unknown')}${candidate.checks_known ? '' : ' (required policy unavailable)'}</summary>
                  <p class="nq-muted">Checks belong to synthetic SHA <code>${esc(candidate.sha || 'unknown')}</code>, never copied from PR heads. Passing checks alone is not a merged receipt.</p>
                  <ul>${(candidate.checks || []).map(check => `<li>${stageTrackerNativeLink(check.url, check.name)} · ${esc(check.status || 'unknown')} ${esc(check.conclusion || '')}${check.app_id ? ' · App ' + Number(check.app_id) : ''}</li>`).join('')}</ul>
                  ${!(candidate.checks || []).length ? '<p class="nq-muted">No required check evidence available.</p>' : ''}
                </details></article>`;
            }).join('')}</div>
            ${!candidates.length ? '<p class="nq-muted">No validation candidate observed. Enqueue is not testing or merge completion.</p>' : ''}
            ${outcomes.length ? '<details><summary>Observed lifecycle receipts</summary><ul>' + outcomes.map(outcome => `<li>${esc(outcome.kind)} ${outcome.pr ? '#' + Number(outcome.pr) : ''} <code>${esc(outcome.candidate_sha || outcome.merge_sha || outcome.head || '')}</code> · ${esc(stageTrackerNativeAge(outcome.observed_at, nowMs))} ago · ${esc(outcome.source)}</li>`).join('') + '</ul><p class="nq-muted">Removal/destroyed is not proof of failure, rebuild reason or merge. Only an observed merged receipt means merged. History is bounded and may have gaps after restart.</p></details>' : ''}
          </section>`;
        }).join('');
    }

    function stageTrackerNativeQueueSync(data, pageRows, nowMs, repoFilter) {
      const root = document.getElementById('activity-monitor-native-queue');
      if (!root) return;
      const rows = (pageRows || []).map(row => row.item || row).filter(Boolean);
      const html = stageTrackerNativeQueueHtml((data || {}).integration_queues || [], (data || {}).items || [], rows, nowMs, repoFilter);
      root.hidden = !html;
      setIfChanged(root, html);
    }
