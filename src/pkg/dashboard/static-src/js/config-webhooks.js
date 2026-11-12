    /* Settings → Forge App → GitHub webhooks (hivecommons/hive#18017).
       Owner-facing setup for /api/webhook/github: the exact payload URL,
       the events to tick, a per-hive secret the owner generates here and
       sees once, optional App-side hook creation, and the live receiver
       status. Hosted spokes get deliveries through the hub relay with no
       owner action, so for them the section is a status readout. */

    let _webhookSetup = null;
    let _webhookSecretOnce = '';
    let _webhookConfirmRegenerate = false;

    function renderGovWebhooksSection() {
      return `
        <div style="font-weight:600;font-size:0.85rem;margin:18px 0 6px">GitHub webhooks</div>
        <div style="font-size:0.78rem;color:var(--muted);margin-bottom:8px">
          Webhooks let GitHub tell this hive about pushes, PRs, reviews and checks as they
          happen: faster kicks, fewer API calls, a slower safety-net cadence.
          <a href="${webhookDocsHref()}" target="_blank" rel="noopener noreferrer">Owner guide ↗</a>
        </div>
        <div id="webhook-setup-host"><div style="color:var(--muted);font-size:0.8rem">Loading webhook status…</div></div>`;
    }

    function webhookDocsHref() {
      return (_webhookSetup && _webhookSetup.docs_url) || 'https://github.com/hivecommons/hive/blob/v5/src/docs/webhooks.md#setting-up-webhooks-owner-guide';
    }

    async function loadWebhookSetup() {
      const host = document.getElementById('webhook-setup-host');
      if (!host) return;
      try {
        const res = await fetch('/api/config/github/webhooks');
        if (res.status === 403) {
          host.innerHTML = '<div style="color:var(--muted);font-size:0.78rem">Owner access required to view webhook setup.</div>';
          return;
        }
        if (!res.ok) throw new Error('HTTP ' + res.status);
        _webhookSetup = await res.json();
        renderWebhookSetup();
      } catch (e) {
        host.innerHTML = `<div style="color:var(--red);font-size:0.8rem">Failed to load webhook status: ${esc(e.message)}</div>`;
      }
    }

    function webhookStatusChip(d) {
      const h = d.health || {};
      if (d.verification === 'none') return chip('not configured', 'var(--muted)');
      if (h.events_1h > 0 || h.healthy) return chip(`receiving · ${h.events_1h || 0}/1h`, 'var(--green)');
      if (h.last_event_at) return chip('quiet · last ' + webhookAgo(h.last_event_at), 'var(--yellow)');
      return chip('no events yet', 'var(--yellow)');
      function chip(text, color) {
        return `<span style="font-size:var(--fs-xs);font-weight:600;padding:var(--sp-1) var(--sp-4);border-radius:10px;border:1px solid ${color};color:${color}">${esc(text)}</span>`;
      }
    }

    function webhookAgo(iso) {
      const t = Date.parse(iso);
      if (!t) return '—';
      const s = Math.max(0, Math.round((Date.now() - t) / 1000));
      if (s < 60) return s + 's ago';
      if (s < 3600) return Math.round(s / 60) + 'm ago';
      if (s < 86400) return Math.round(s / 3600) + 'h ago';
      return Math.round(s / 86400) + 'd ago';
    }

    function webhookCopyBtn(text, label) {
      return `<button type="button" class="hv-btn btn-secondary btn-sm" data-action="webhookCopy" data-arg-types="t" data-copy="${esc(text)}" style="margin-left:6px">${esc(label || 'Copy')}</button>`;
    }

    async function webhookCopy(btn) {
      const text = btn && btn.dataset.copy || '';
      const ok = await writeClipboardText(text);
      showToast(ok ? 'Copied' : 'Could not copy — select the text and copy it by hand', ok ? 'info' : 'error');
    }

    function renderWebhookSetup() {
      const host = document.getElementById('webhook-setup-host');
      const d = _webhookSetup;
      if (!host || !d) return;
      const h = d.health || {};
      const row = (label, valueHtml) => forgeAppRow(label, valueHtml);
      const code = (t) => `<code style="font-size:0.74rem;user-select:all">${esc(t)}</code>`;
      const relayHeld = !!d.hosted;
      const direct = d.secret_source !== '';

      let html = `<div style="border:1px solid var(--border);border-radius:8px;padding:12px 14px;margin-bottom:var(--sp-5);background:var(--surface)">
        <div style="display:flex;align-items:center;gap:10px;margin-bottom:var(--sp-4);flex-wrap:wrap">
          <span style="font-weight:600;font-size:0.9rem">Receiver status</span>
          ${webhookStatusChip(d)}
        </div>
        ${row('Verification', code(d.verification || 'none') + (relayHeld && !direct ? ' <span class="text-muted">— hub relay; the GitHub App installation delivers through the hub, nothing to set up</span>' : ''))}
        ${row('Last event', h.last_event_at ? esc(new Date(h.last_event_at).toLocaleString()) + ' <span class="text-muted">(' + esc(webhookAgo(h.last_event_at)) + ')</span>' : '<span class="text-muted">none yet</span>')}
        ${row('Deliveries (1h)', esc(String(h.events_1h || 0)) + ' <span class="text-muted">· ' + esc(String(h.invalidations_1h || 0)) + ' cache patches · ' + esc(String(h.ci_events_1h || 0)) + ' CI events</span>')}
        ${row('Cadence', code(h.mode || 'polling') + (h.mode_reason ? ' <span style="color:var(--yellow)">' + esc(h.mode_reason) + '</span>' : ''))}
        ${(!h.last_event_at && d.verification !== 'none') ? '<div style="font-size:0.76rem;color:var(--yellow);margin-top:6px">No events yet — on GitHub open the webhook (Settings → Webhooks → your hook → Recent deliveries) and check the response code: 401 means the secret differs, 404 means the path is wrong, 503 means this hive has no secret.</div>' : ''}
      </div>`;

      html += `<div style="border:1px solid var(--border);border-radius:8px;padding:12px 14px;margin-bottom:var(--sp-5);background:var(--surface)">
        <div style="font-weight:600;font-size:0.9rem;margin-bottom:var(--sp-4)">${relayHeld ? 'Direct webhook (optional)' : 'Set up a webhook on GitHub'}</div>
        ${relayHeld ? '<div style="font-size:0.76rem;color:var(--muted);margin-bottom:8px">This hive already receives deliveries the hub relays from the GitHub App installation. A direct webhook with your own secret is only needed for repositories the App is not installed on.</div>' : ''}
        <div style="font-size:0.78rem;font-weight:600;margin:6px 0 2px">1. Secret</div>
        ${renderWebhookSecretBlock(d)}
        <div style="font-size:0.78rem;font-weight:600;margin:12px 0 2px">2. Add the webhook in GitHub</div>
        <div style="font-size:0.76rem;color:var(--muted)">Organization (one hook for every repo) → <b>Settings → Webhooks → Add webhook</b>, or per repository under the repo's Settings.</div>
        ${row('Payload URL', d.payload_url ? code(d.payload_url) + webhookCopyBtn(d.payload_url) + ' <span class="text-muted">(' + esc(d.payload_url_source) + ')</span>' : '<span style="color:var(--yellow)">unknown — set <code>dashboard.public_url</code> to this hive\u2019s public origin; the path is <code>' + esc(d.path) + '</code></span>')}
        ${row('Content type', code(d.content_type))}
        ${row('Secret', '<span class="text-muted">the value from step 1</span>')}
        ${row('SSL verification', '<span class="text-muted">keep enabled</span>')}
        ${row('Events', '<span class="text-muted">"Let me select individual events", then tick:</span><br>' + (d.events || []).map(e => code(e)).join(' ') + webhookCopyBtn((d.events || []).join(', '), 'Copy list'))}
        ${row('Optional', Object.entries(d.optional_events || {}).map(([k, v]) => code(k) + ' <span class="text-muted">— ' + esc(v) + '</span>').join('<br>'))}
        ${renderWebhookCreateBlock(d)}
      </div>`;
      host.innerHTML = html;
    }

    function renderWebhookSecretBlock(d) {
      const code = (t) => `<code style="font-size:0.74rem;user-select:all">${esc(t)}</code>`;
      let html = '';
      if (_webhookSecretOnce) {
        html += `<div style="border:1px solid var(--green);border-radius:var(--r);padding:8px 10px;margin:6px 0;font-size:0.78rem">
          <div style="font-weight:600;color:var(--green)">New secret — shown once</div>
          <div style="margin:4px 0;word-break:break-all">${code(_webhookSecretOnce)}${webhookCopyBtn(_webhookSecretOnce)}</div>
          <div class="text-muted">Paste it into the GitHub webhook\u2019s <b>Secret</b> field. The hive keeps only the file; closing this dialog forgets the value.</div>
        </div>`;
      }
      if (d.secret_source === 'env') {
        html += `<div style="font-size:0.76rem">Configured from <code>GITHUB_WEBHOOK_SECRET</code> (a Secret mount). Use that same value in GitHub; rotate it in the deployment, not here.</div>`;
        return html;
      }
      const state = d.secret_source === 'file'
        ? `Configured · stored at <code>${esc(d.secret_path || '')}</code>${_webhookSecretOnce ? '' : ' (value not shown again — regenerate if lost)'}`
        : '<span style="color:var(--yellow)">No secret yet — generate one. Without it every delivery is rejected with 503.</span>';
      const label = _webhookConfirmRegenerate ? 'Confirm: replace secret' : (d.secret_source === 'file' ? 'Regenerate secret' : 'Generate secret');
      const cls = _webhookConfirmRegenerate ? 'btn-danger' : 'btn-primary';
      html += `<div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;font-size:0.76rem;margin-top:4px">
        <span>${state}</span>
        <button type="button" class="hv-btn ${cls} btn-sm" data-action="webhookGenerateSecret" data-arg-types="t">${esc(label)}</button>
        ${_webhookConfirmRegenerate ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="webhookCancelRegenerate">Cancel</button><span class="text-muted">GitHub hooks using the old secret will start failing with 401 until you update them.</span>' : ''}
      </div>`;
      return html;
    }

    function renderWebhookCreateBlock(d) {
      if (d.app_probe_error) {
        return `<div style="font-size:0.74rem;color:var(--muted);margin-top:10px">Could not read the App installation\u2019s permissions (${esc(d.app_probe_error)}); create the hook by hand as above.</div>`;
      }
      if (!d.app_org_hooks && !d.app_repo_hooks) return '';
      const scopes = [];
      if (d.app_org_hooks) scopes.push(`<option value="organization">organization ${esc(d.org)}</option>`);
      if (d.app_repo_hooks) (d.repos || []).forEach(r => scopes.push(`<option value="repository:${esc(r)}">repository ${esc(d.org)}/${esc(r)}</option>`));
      const disabled = !d.create_available;
      const why = !d.secret_configured ? 'generate a secret first' : (!d.payload_url ? 'set dashboard.public_url first' : '');
      return `<div style="border-top:1px solid var(--border);margin-top:10px;padding-top:10px">
        <div style="font-size:0.78rem;font-weight:600;margin-bottom:4px">Or let the App create it</div>
        <div style="display:flex;align-items:center;gap:10px;flex-wrap:wrap;font-size:0.76rem">
          <select id="webhook-create-scope" style="font-size:0.76rem">${scopes.join('')}</select>
          <button type="button" class="hv-btn btn-primary btn-sm" data-action="webhookCreateHook" data-arg-types="t" ${disabled ? 'disabled' : ''}>Create webhook for me</button>
          ${why ? '<span class="text-muted">— ' + esc(why) + '</span>' : '<span class="text-muted">uses the App installation\u2019s hooks permission; subscribes the events above with this hive\u2019s secret</span>'}
        </div>
        <div id="webhook-create-result" style="font-size:0.76rem;margin-top:6px"></div>
      </div>`;
    }

    function webhookCancelRegenerate() {
      _webhookConfirmRegenerate = false;
      renderWebhookSetup();
    }

    async function webhookGenerateSecret(btn) {
      if (_webhookSetup && _webhookSetup.secret_source === 'file' && !_webhookConfirmRegenerate) {
        _webhookConfirmRegenerate = true;
        renderWebhookSetup();
        return;
      }
      _webhookConfirmRegenerate = false;
      if (btn) btn.disabled = true;
      try {
        const res = await fetch('/api/config/github/webhooks/secret', { method: 'POST' });
        const body = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(body.error || ('HTTP ' + res.status));
        _webhookSecretOnce = body.secret || '';
        showToast('Webhook secret generated — copy it now, it is shown once', 'info');
        await loadWebhookSetup();
      } catch (e) {
        showToast('Could not generate secret: ' + e.message, 'error');
        if (btn) btn.disabled = false;
      }
    }

    async function webhookCreateHook(btn) {
      const sel = document.getElementById('webhook-create-scope');
      const out = document.getElementById('webhook-create-result');
      const v = (sel && sel.value) || 'organization';
      const [scope, repo] = v.split(':');
      if (btn) btn.disabled = true;
      if (out) out.textContent = 'Creating…';
      try {
        const res = await fetch('/api/config/github/webhooks/create', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ scope, repo: repo || '' })
        });
        const body = await res.json().catch(() => ({}));
        if (!res.ok) throw new Error(body.error || ('HTTP ' + res.status));
        if (out) out.innerHTML = `<span style="color:var(--green)">Created ${esc(body.scope)} hook #${esc(String(body.hook_id))} on ${esc(body.target)}.</span> <a href="${esc(body.url)}" target="_blank" rel="noopener noreferrer">View on GitHub ↗</a> — push a commit or comment on a PR and watch “Deliveries (1h)” move.`;
        showToast('Webhook created', 'info');
      } catch (e) {
        if (out) out.innerHTML = `<span style="color:var(--red)">${esc(e.message)}</span>`;
      } finally {
        if (btn) btn.disabled = false;
      }
    }

    /* Forget the one-time secret whenever the Settings dialog closes so it
       never lingers in a re-rendered tab. */
    function webhookSetupReset() {
      _webhookSecretOnce = '';
      _webhookConfirmRegenerate = false;
    }
