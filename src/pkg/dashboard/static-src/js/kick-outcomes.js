    // The API, not the browser's role hint, decides whether evidence is visible.
    function kickOutcomesHtml(data) {
      const labels = { opened_pr: 'Opened PR', pushed_commit: 'Pushed commit', commented: 'Commented', stood_down: 'Stood down', refused: 'Refused', still_running: 'Still running', ended_no_visible_action: 'Ended — no visible action' };
      const detailed = data.detail === true;
      const kicks = detailed ? (Array.isArray(data.kicks) ? data.kicks : []) : (data.latest ? [data.latest] : []);
      if (!kicks.length) return '<p class="text-muted">No recorded kicks yet.</p>';
      const time = value => {
        const date = new Date(value);
        return value && !isNaN(date.getTime()) ? esc(formatDashboardDateTime(date)) : 'Unknown time';
      };
      return '<ol class="kick-outcome-list">' + kicks.map(k => {
        let evidence = '';
        if (detailed) {
          const trigger = [k.trigger_kind, k.trigger_actor].filter(Boolean).join(' · ');
          evidence += '<div class="text-muted">Trigger: ' + esc(trigger || 'unknown') + '</div>';
          evidence += '<div class="text-muted">Started: ' + time(k.started_at) + (k.ended_at ? ' · Ended: ' + time(k.ended_at) : '') + '</div>';
          // Quotes and every other API string are untrusted. Restrict link protocols too.
          let prURL = '';
          try { const url = new URL(k.pr_url); if (url.protocol === 'https:' || url.protocol === 'http:') prURL = url.href; } catch (_) { /* no valid PR link */ }
          if (prURL) evidence += '<a href="' + esc(prURL) + '" target="_blank" rel="noopener noreferrer">PR ' + esc(k.pr_number ? '#' + k.pr_number : k.pr_url) + '</a>';
          if (k.branch) evidence += '<div>Branch: <code>' + esc(k.branch) + '</code></div>';
          if (k.sha) evidence += '<div>Commit: <code>' + esc(k.sha) + '</code></div>';
          if (k.refusal_code) evidence += '<div>Refusal: <code>' + esc(k.refusal_code) + '</code></div>';
          if (k.quote) evidence += '<blockquote>' + esc(k.quote) + '</blockquote>';
        }
        return '<li><strong>' + esc(labels[k.kind] || 'Unknown outcome') + '</strong> <span class="text-muted">' + time(k.at) + '</span><div>' + esc(k.summary || '') + '</div>' + evidence + '</li>';
      }).join('') + '</ol>';
    }

    async function refreshKickOutcomes(agent) {
      const panel = document.querySelector('#oc-agent-detail .kick-outcomes');
      if (!panel || panel.dataset.agent !== agent) return;
      if (panel.dataset.pending === 'true' || Date.now() - Number(panel.dataset.fetchedAt || 0) < 10000) return;
      panel.dataset.pending = 'true';
      try {
        const response = await fetch('/api/kick-outcomes?agent=' + encodeURIComponent(agent), { headers: _inceptionAuthHeaders() });
        if (!response.ok) throw new Error('unavailable');
        const data = await response.json();
        if (panel.isConnected && panel.dataset.agent === agent) panel.querySelector('.kick-outcomes-body').innerHTML = kickOutcomesHtml(data);
      } catch (_) {
        if (panel.isConnected) panel.querySelector('.kick-outcomes-body').innerHTML = '<p class="text-muted">Kick outcomes unavailable. Retrying on the next refresh.</p>';
      } finally {
        panel.dataset.pending = 'false';
        panel.dataset.fetchedAt = String(Date.now());
      }
    }
