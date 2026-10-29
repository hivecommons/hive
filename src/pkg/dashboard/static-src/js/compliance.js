    // ── Settings → Compliance (#11080) ──
    // Panel 1 picks the framework profiles (compliance.frameworks, saved by
    // the footer Save through PUT /api/config/governor/compliance). Panel 2
    // lists every control of the selected profiles from
    // GET /api/compliance/status. Inline toggles and "Apply recommended" only
    // STAGE values in the owning section's dirty bag, so the footer Save
    // writes each setting through that section's own owner-only endpoint.
    const COMPLIANCE_DISCLAIMER = 'Hive is not certified; this maps your configuration to control requirements.';
    const COMPLIANCE_STATUS_PILLS = {
      meets: ['ok', 'Meets'],
      deviates: ['warn', 'Deviates'],
      off: ['error', 'Off'],
      not_covered: ['neutral', 'Not covered'],
    };
    let _complianceState = { loading: false, loaded: false, forbidden: false, error: '', report: null };
    let _complianceLoadSeq = 0;

    function complianceSetData(path, value) {
      let node = _configState.data = _configState.data || {};
      for (let i = 0; i < path.length - 1; i++) {
        if (!node[path[i]] || typeof node[path[i]] !== 'object') node[path[i]] = {};
        node = node[path[i]];
      }
      node[path[path.length - 1]] = value;
    }

    function complianceStageTrustedAuthors(key, value) {
      const staged = ((_configState.dirty || {})['auto-merge'] || {}).trusted_authors || {};
      markDirty('auto-merge', 'trusted_authors', Object.assign({}, staged, { [key]: value }));
      complianceSetData(['auto_merge', 'trusted_authors', key], value);
    }

    function complianceStageContributorLane(key, value) {
      const staged = ((_configState.dirty || {})['auto-merge'] || {}).contributor_lane || {};
      markDirty('auto-merge', 'contributor_lane', Object.assign({}, staged, { [key]: value }));
      complianceSetData(['auto_merge', 'contributor_lane', key], value);
    }

    // Where each mapped setting lives in Settings. `stage` exists only for
    // booleans the owning section's endpoint can write; every other mapping
    // gets an "Open setting" link to its tab, or a hint when it is
    // hive.yaml-only.
    const COMPLIANCE_SETTING_OWNERS = {
      'review.require_approval': { tab: 'Features', stage: v => { markDirty('review', 'require_approval', v); complianceSetData(['review', 'require_approval'], v); } },
      'auto_merge.self_authored': { tab: 'Features', stage: v => { markDirty('auto-merge', 'self_authored', v); complianceSetData(['auto_merge', 'self_authored'], v); } },
      'auto_merge.trusted_authors.enabled': { tab: 'Features', stage: v => complianceStageTrustedAuthors('enabled', v) },
      'auto_merge.trusted_authors.require_github_permission': { tab: 'Features', stage: v => complianceStageTrustedAuthors('require_github_permission', v) },
      'auto_merge.contributor_lane.enabled': { tab: 'Features', stage: v => complianceStageContributorLane('enabled', v) },
      'auto_merge.contributor_lane.require_reviewer_verdict': { tab: 'Features', stage: v => complianceStageContributorLane('require_reviewer_verdict', v) },
      'auto_merge.contributor_lane.registry_tiers_sufficient': { tab: 'Features', stage: v => complianceStageContributorLane('registry_tiers_sufficient', v) },
      'auto_merge.contributor_lane.approve_held_runs_for_registry_trusted': { tab: 'Features', stage: v => complianceStageContributorLane('approve_held_runs_for_registry_trusted', v) },
      'auto_merge.contributor_lane.owner_approval_releases_workflow_fence': { tab: 'Features', stage: v => complianceStageContributorLane('owner_approval_releases_workflow_fence', v) },
      'auto_merge.required_checks': { tab: 'Features' },
      'sentinel.enabled': { tab: 'Security', stage: v => markSentinelDirty('enabled', v) },
      'sentinel.disabled_behaviors': { tab: 'Security' },
      'agent_sandbox.enabled': { tab: 'Security', stage: v => { markDirty('security', 'agentSandboxEnabled', v); complianceSetData(['security', 'agentSandboxEnabled'], v); } },
      'dashboard.authorized_users': { tab: 'Security' },
      'escalation.disabled': { tab: 'Health', stage: v => { markDirty('escalation', 'disabled', v); if (_configState.data && _configState.data.escalation) _configState.data.escalation.disabled = v; } },
    };

    async function loadComplianceStatus() {
      const seq = ++_complianceLoadSeq;
      _complianceState = Object.assign({}, _complianceState, { loading: true, error: '' });
      complianceRepaint();
      try {
        const res = await fetch('/api/compliance/status');
        if (seq !== _complianceLoadSeq) return;
        if (res.status === 403) {
          _complianceState = { loading: false, loaded: true, forbidden: true, error: '', report: null };
        } else if (!res.ok) {
          throw new Error(await saveErrorMessage(res));
        } else {
          _complianceState = { loading: false, loaded: true, forbidden: false, error: '', report: await res.json() };
        }
      } catch (err) {
        if (seq !== _complianceLoadSeq) return;
        _complianceState = { loading: false, loaded: true, forbidden: false, error: err && err.message ? err.message : String(err), report: null };
      }
      complianceRepaint();
      if (viewerIsOwner() && !_complianceState.forbidden && !_complianceState.error) loadComplianceEvidence();
    }

    function complianceRepaint() {
      const root = document.getElementById('compliance-config-root');
      if (root) root.innerHTML = renderGovComplianceBody();
    }

    function renderGovCompliance() {
      return '<div id="compliance-config-root">' + renderGovComplianceBody() + '</div>';
    }

    function complianceSelectedFrameworks() {
      if (_configState.complianceFrameworks) return _configState.complianceFrameworks.slice();
      return ((_complianceState.report || {}).frameworks || []).slice();
    }

    function complianceStaged() {
      if (!_configState.complianceStaged) _configState.complianceStaged = {};
      return _configState.complianceStaged;
    }

    function renderGovComplianceBody() {
      const st = _complianceState;
      const banner = '<div class="cmp-banner" role="note"><strong>' + esc(COMPLIANCE_DISCLAIMER) + '</strong>' +
        (st.report && st.report.disclaimer ? '<div class="cmp-banner-detail">' + esc(String(st.report.disclaimer)) + '</div>' : '') + '</div>';
      if (!st.loaded) return banner + '<p class="cmp-note">Loading compliance status…</p>';
      if (st.forbidden) return banner + '<p class="cmp-note">Compliance status is visible to mergers and owners only.</p>';
      if (st.error) {
        return banner + '<p class="cmp-error">Could not load compliance status: ' + esc(st.error) + '</p>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="loadComplianceStatus">Retry</button>';
      }
      const owner = viewerIsOwner();
      const readOnly = owner ? '' : '<p class="cmp-note">Read-only — only owners can change these settings.</p>';
      return banner + readOnly + renderComplianceFrameworks(st.report || {}, owner) + renderComplianceControls(st.report || {}, owner) +
        renderCompliancePosture(owner) + renderComplianceEvidence(st.report || {}, owner);
    }

    function renderComplianceFrameworks(report, owner) {
      const selected = complianceSelectedFrameworks();
      const available = report.available || [];
      const rows = available.map(f => {
        const id = String(f.id || '');
        const on = selected.includes(id);
        return '<label class="cmp-framework">' +
          '<input type="checkbox" ' + (on ? 'checked ' : '') + (owner ? '' : 'disabled ') +
            'data-change-action="complianceToggleFramework" data-arg0="' + esc(id) + '" data-arg-types="s,c">' +
          '<span><span class="cmp-framework-name">' + esc(String(f.name || id)) + '</span> <span class="cmp-framework-desc">' + esc(String(f.version || '')) + ' · ' + Number(f.controls || 0) + ' controls</span>' +
          (f.description ? '<div class="cmp-framework-desc">' + esc(String(f.description)) + '</div>' : '') + '</span>' +
        '</label>';
      }).join('');
      const pending = _configState.complianceFrameworks ? '<p class="cmp-note cmp-staged">Framework selection changes apply on Save; the controls below still show the saved selection.</p>' : '';
      const loadErr = report.profile_load_error ? '<p class="cmp-error">Profile load error: ' + esc(String(report.profile_load_error)) + '</p>' : '';
      const unknown = (report.unknown_frameworks || []).length ? '<p class="cmp-error">Unknown frameworks in hive.yaml: ' + esc((report.unknown_frameworks || []).join(', ')) + '</p>' : '';
      return '<div class="config-section">' +
        '<h3>Framework profile</h3>' +
        '<p class="cmp-note">Select the frameworks whose controls this hive maps its settings against (<code>compliance.frameworks</code>). None selected turns compliance evaluation off.</p>' +
        (rows || '<p class="cmp-note">No framework profiles are shipped in this build.</p>') + pending + loadErr + unknown +
      '</div>';
    }

    function complianceToggleFramework(id, checked) {
      if (!viewerIsOwner()) return;
      const next = complianceSelectedFrameworks().filter(f => f !== id);
      if (checked) next.push(id);
      _configState.complianceFrameworks = next;
      markDirty('compliance', 'frameworks', next);
      complianceRepaint();
    }

    function complianceOwnerFor(setting) {
      if (!setting || setting.builtin || String(setting.setting_path || '').startsWith('env.')) return null;
      return COMPLIANCE_SETTING_OWNERS[setting.setting_path] || null;
    }

    // A mapping is toggleable when it is a boolean whose owning section can
    // write it and whose recommendation is a plain true/false.
    function complianceToggleable(setting) {
      const owner = complianceOwnerFor(setting);
      return !!(owner && owner.stage && setting.kind === 'bool' && (setting.recommended === 'true' || setting.recommended === 'false'));
    }

    function complianceCurrentBool(setting) {
      const staged = complianceStaged();
      if (Object.prototype.hasOwnProperty.call(staged, setting.setting_path)) return staged[setting.setting_path];
      return setting.current === 'true';
    }

    function renderComplianceSettingControl(setting, owner) {
      const path = String(setting.setting_path || '');
      if (setting.builtin) return '<span class="cmp-note">fixed in code</span>';
      if (path.startsWith('env.')) return '<span class="cmp-note">deployment environment</span>';
      if (complianceToggleable(setting) && owner) {
        const on = complianceCurrentBool(setting);
        return '<div class="config-toggle-switch' + (on ? ' on' : '') + '" role="switch" aria-checked="' + on + '" aria-label="' + esc(path) + '" data-action="complianceToggleSetting" data-arg0="' + esc(path) + '" data-arg-types="s,t"></div>';
      }
      const where = complianceOwnerFor(setting);
      if (where && where.tab) return '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="switchConfigTab" data-tab="' + esc(where.tab) + '">Open setting</button>';
      return '<span class="cmp-note">hive.yaml only</span>';
    }

    function complianceToggleSetting(path, el) {
      if (!viewerIsOwner() || !el) return;
      const owner = COMPLIANCE_SETTING_OWNERS[path];
      if (!owner || !owner.stage) return;
      const next = !el.classList.contains('on');
      owner.stage(next);
      complianceStaged()[path] = next;
      complianceRepaint();
    }

    function renderComplianceSettingValue(setting) {
      const staged = complianceStaged();
      const isStaged = Object.prototype.hasOwnProperty.call(staged, setting.setting_path);
      const current = isStaged ? String(staged[setting.setting_path]) : String(setting.current == null ? '' : setting.current);
      return '<div class="cmp-setting"><span>' + esc(current || '(unset)') + ' → ' + esc(String(setting.recommended || '')) + '</span>' +
        (isStaged ? '<span class="cmp-staged">staged</span>' : '') + '</div>';
    }

    function renderComplianceControls(report, owner) {
      const controls = report.controls || [];
      if (!(report.frameworks || []).length) {
        return '<div class="config-section"><h3>Controls</h3><p class="cmp-note">Select a framework and save to see its controls.</p></div>';
      }
      const multi = (report.frameworks || []).length > 1;
      const domains = [];
      const byDomain = {};
      controls.forEach(c => {
        const d = String(c.domain || 'Other');
        if (!byDomain[d]) { byDomain[d] = []; domains.push(d); }
        byDomain[d].push(c);
      });
      const s = report.summary || {};
      const summary = '<p class="cmp-note">' + Number(s.meets || 0) + ' meet · ' + Number(s.deviates || 0) + ' deviate · ' + Number(s.off || 0) + ' off · ' + Number(s.not_covered || 0) + ' not covered by Hive</p>';
      const sections = domains.map(d => {
        const rows = byDomain[d].map(c => {
          const pill = COMPLIANCE_STATUS_PILLS[c.status] || ['neutral', String(c.status || '')];
          const tip = [c.text, c.citation ? 'Citation: ' + c.citation : ''].filter(Boolean).join(' — ');
          const settings = c.settings || [];
          const settingCell = c.not_covered
            ? '<span class="cmp-note">' + esc(String(c.rationale || 'Not covered by any Hive setting.')) + '</span>'
            : settings.map(st => '<div class="cmp-setting"><code>' + esc(String(st.setting_path || '')) + '</code>' + renderComplianceSettingControl(st, owner) + '</div>').join('');
          const valueCell = c.not_covered ? '' : settings.map(renderComplianceSettingValue).join('');
          return '<tr>' +
            '<td><code>' + esc((multi ? c.framework + ' ' : '') + String(c.control_id || '')) + '</code> ' + esc(String(c.title || '')) +
              (tip ? ' ' + settingHelpMark('compliance', 'the-settings--compliance-tab', String(c.control_id || ''), esc(tip)) : '') + '</td>' +
            '<td>' + settingCell + '</td>' +
            '<td>' + valueCell + '</td>' +
            '<td><span class="badge-status" data-status="' + pill[0] + '">' + esc(pill[1]) + '</span></td>' +
          '</tr>';
        }).join('');
        const applyBtn = owner && complianceDomainChanges(d).length
          ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="complianceApplyDomain" data-arg0="' + esc(d) + '">Apply recommended</button>'
          : '';
        return '<div class="cmp-domain-head"><span class="cmp-domain-title">' + esc(d) + '</span>' + applyBtn + '</div>' +
          '<table class="cmp-table"><thead><tr><th>Control</th><th>Implementing setting(s)</th><th>Current → recommended</th><th>Status</th></tr></thead><tbody>' + rows + '</tbody></table>';
      }).join('');
      return '<div class="config-section"><h3>Controls</h3>' + summary + sections +
        '<p class="cmp-note">Toggles and "Apply recommended" stage changes in the owning section; nothing is written until Save. Statuses refresh after the save.</p></div>';
    }

    // complianceDomainChanges lists the boolean mappings in one domain whose
    // effective value (staged or saved) differs from the recommendation and
    // that the owning section can write, one entry per setting path.
    function complianceDomainChanges(domain) {
      const seen = {};
      const out = [];
      (((_complianceState.report || {}).controls) || []).forEach(c => {
        if (String(c.domain || 'Other') !== domain) return;
        (c.settings || []).forEach(st => {
          if (seen[st.setting_path] || !complianceToggleable(st)) return;
          const want = st.recommended === 'true';
          if (complianceCurrentBool(st) === want) return;
          seen[st.setting_path] = true;
          out.push({ path: st.setting_path, from: complianceCurrentBool(st), to: want });
        });
      });
      return out;
    }

    function complianceDomainManual(domain) {
      const seen = {};
      const out = [];
      (((_complianceState.report || {}).controls) || []).forEach(c => {
        if (String(c.domain || 'Other') !== domain) return;
        (c.settings || []).forEach(st => {
          if (st.status === 'meets' || complianceToggleable(st) || seen[st.setting_path]) return;
          seen[st.setting_path] = true;
          out.push(st.setting_path + ' (' + (st.current || 'unset') + ' → ' + st.recommended + ')');
        });
      });
      return out;
    }

    async function complianceApplyDomain(domain) {
      if (!viewerIsOwner()) return;
      const changes = complianceDomainChanges(domain);
      if (!changes.length) { showToast('Every writable setting in ' + domain + ' already matches the recommendation', 'info'); return; }
      const manual = complianceDomainManual(domain);
      const lines = changes.map(ch => '• ' + ch.path + ': ' + ch.from + ' → ' + ch.to);
      let message = 'These settings will be staged for Save:\n\n' + lines.join('\n');
      if (manual.length) message += '\n\nNot changed here (edit in their own section):\n' + manual.map(m => '• ' + m).join('\n');
      const ok = await hiveConfirm({ title: 'Apply recommended — ' + domain, message, confirmLabel: 'Stage ' + changes.length + ' change' + (changes.length === 1 ? '' : 's'), danger: false });
      if (!ok) return;
      const staged = complianceStaged();
      changes.forEach(ch => {
        COMPLIANCE_SETTING_OWNERS[ch.path].stage(ch.to);
        staged[ch.path] = ch.to;
      });
      showToast('Staged ' + changes.length + ' change' + (changes.length === 1 ? '' : 's') + ' — Save & close to apply', 'success');
      complianceRepaint();
    }

    // ── Compliance posture history, evidence exports and attestations (#11081) ──
    // Owner only. Panel 3 draws GET /api/compliance/posture/history as one
    // sparkline per check; panel 4 downloads GET /api/compliance/export and
    // lists / records owner attestations (GET/POST /api/compliance/attestations).
    const COMPLIANCE_POSTURE_PILLS = {
      pass: ['ok', 'Pass'],
      fail: ['error', 'Fail'],
      skip: ['neutral', 'Skip'],
      error: ['warn', 'Error'],
    };
    const COMPLIANCE_EXPORTS = [
      { kind: 'controls', format: 'md', label: 'Control mapping (Markdown)' },
      { kind: 'controls', format: 'json', label: 'Control mapping (JSON)' },
      { kind: 'posture', format: 'csv', label: 'Posture history (CSV)' },
      { kind: 'posture', format: 'json', label: 'Posture history (JSON)' },
      { kind: 'audit', format: 'json', label: 'Audit log slice (JSON)' },
      { kind: 'config', format: 'json', label: 'Config snapshot + sha256 (JSON)' },
      { kind: 'attestations', format: 'csv', label: 'Attestations (CSV)' },
      { kind: 'bundle', format: 'json', label: 'Evidence bundle (JSON)' },
    ];
    const COMPLIANCE_HISTORY_RANGES = [7, 30, 90, 365];
    const COMPLIANCE_SPARK_MAX = 60;
    let _complianceEvidence = { loaded: false, error: '', history: null, attestations: [], days: 30, since: '', until: '', running: false, busy: false, draft: { framework: '', reviewed_on: '', note: '' } };
    let _complianceEvidenceSeq = 0;

    function complianceTime(at) {
      const d = new Date(at);
      return isNaN(d.getTime()) ? String(at || '') : formatDashboardDateTime(d);
    }

    function complianceTodayISO() {
      const d = new Date();
      return d.getFullYear() + '-' + String(d.getMonth() + 1).padStart(2, '0') + '-' + String(d.getDate()).padStart(2, '0');
    }

    async function loadComplianceEvidence() {
      if (!viewerIsOwner()) return;
      const seq = ++_complianceEvidenceSeq;
      try {
        const [hist, att] = await Promise.all([
          fetch('/api/compliance/posture/history?since=' + encodeURIComponent(_complianceEvidence.days * 24 + 'h')),
          fetch('/api/compliance/attestations'),
        ]);
        if (seq !== _complianceEvidenceSeq) return;
        if (!hist.ok) throw new Error(await saveErrorMessage(hist));
        if (!att.ok) throw new Error(await saveErrorMessage(att));
        const history = await hist.json();
        const attBody = await att.json();
        if (seq !== _complianceEvidenceSeq) return;
        _complianceEvidence = Object.assign({}, _complianceEvidence, { loaded: true, error: '', history, attestations: attBody.attestations || [] });
      } catch (err) {
        if (seq !== _complianceEvidenceSeq) return;
        _complianceEvidence = Object.assign({}, _complianceEvidence, { loaded: true, error: err && err.message ? err.message : String(err) });
      }
      complianceRepaint();
    }

    function complianceSetHistoryDays(value) {
      const n = Number(value);
      if (!n) return;
      _complianceEvidence.days = n;
      loadComplianceEvidence();
    }

    function complianceSparkline(points) {
      const pts = (points || []).slice(-COMPLIANCE_SPARK_MAX);
      if (!pts.length) return '<span class="cmp-note">No runs in range</span>';
      const bars = pts.map(p => {
        const status = String(p.status || '');
        return '<span class="cmp-spark-bar cmp-spark-' + esc(status) + '" title="' + esc(complianceTime(p.at) + ' — ' + status) + '"></span>';
      }).join('');
      return '<span class="cmp-spark" role="img" aria-label="' + esc('Last ' + pts.length + ' results') + '">' + bars + '</span>';
    }

    function complianceEvidenceLinks(refs) {
      if (!(refs || []).length) return '';
      return '<div class="cmp-evidence">' + refs.map(ref => {
        const r = String(ref || '');
        return /^https?:\/\//i.test(r)
          ? '<a href="' + esc(r) + '" target="_blank" rel="noopener noreferrer">' + esc(r.replace(/^https?:\/\/(www\.)?github\.com\//i, '')) + '</a>'
          : '<code>' + esc(r) + '</code>';
      }).join(' ') + '</div>';
    }

    function renderCompliancePosture(owner) {
      const head = '<div class="config-section"><h3>Posture checks</h3>';
      if (!owner) return head + '<p class="cmp-note">Posture history is visible to owners only.</p></div>';
      const ev = _complianceEvidence;
      if (!ev.loaded) return head + '<p class="cmp-note">Loading posture history…</p></div>';
      if (ev.error) {
        return head + '<p class="cmp-error">Could not load posture history and attestations: ' + esc(ev.error) + '</p>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="loadComplianceEvidence">Retry</button></div>';
      }
      const h = ev.history || {};
      const options = COMPLIANCE_HISTORY_RANGES.map(d => '<option value="' + d + '"' + (d === ev.days ? ' selected' : '') + '>Last ' + d + ' days</option>').join('');
      const toolbar = '<div class="cmp-toolbar">' +
        '<select data-change-action="complianceSetHistoryDays" data-arg-types="v" aria-label="Posture history range">' + options + '</select>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="complianceRunPosture"' + (ev.running ? ' disabled' : '') + '>' + (ev.running ? 'Running…' : 'Run checks now') + '</button>' +
        '<span class="cmp-note">' + Number(h.runs || 0) + ' run' + (Number(h.runs) === 1 ? '' : 's') + ' in range' + (h.truncated ? ' (most recent 1000 shown)' : '') + '</span>' +
      '</div>';
      const rows = (h.checks || []).map(c => {
        const last = c.last || null;
        const pill = last ? (COMPLIANCE_POSTURE_PILLS[last.status] || ['neutral', String(last.status || '')]) : ['neutral', 'No runs'];
        const s = c.summary || {};
        return '<tr>' +
          '<td>' + esc(String(c.title || c.check_id || '')) + '<div class="cmp-note"><code>' + esc(String(c.check_id || '')) + '</code> · ' + esc((c.control_ids || []).join(', ')) + '</div></td>' +
          '<td>' + complianceSparkline(c.points) + '<div class="cmp-note">' + Number(s.pass || 0) + ' pass · ' + Number(s.fail || 0) + ' fail · ' + Number(s.skip || 0) + ' skip · ' + Number(s.error || 0) + ' error</div></td>' +
          '<td><span class="badge-status" data-status="' + pill[0] + '">' + esc(pill[1]) + '</span>' + (last ? '<div class="cmp-note">' + esc(complianceTime(last.at)) + '</div>' : '') + '</td>' +
          '<td>' + (last && last.detail ? esc(String(last.detail)) : '') +
            (c.last_fail_at ? '<div class="cmp-note">Last failure: ' + esc(complianceTime(c.last_fail_at)) + '</div>' : '') +
            complianceEvidenceLinks(last ? last.evidence_refs : []) + '</td>' +
        '</tr>';
      }).join('');
      return head +
        '<p class="cmp-note">Continuous checks the hive runs against itself every <code>compliance.posture_checks.interval</code> while a framework is selected. A pass is evidence the hive behaved as configured, not a certification.</p>' +
        toolbar +
        '<table class="cmp-table"><thead><tr><th>Check</th><th>History</th><th>Last result</th><th>Detail &amp; evidence</th></tr></thead><tbody>' + rows + '</tbody></table>' +
      '</div>';
    }

    async function complianceRunPosture() {
      if (!viewerIsOwner() || _complianceEvidence.running) return;
      _complianceEvidence.running = true;
      complianceRepaint();
      try {
        const res = await fetch('/api/compliance/posture/run', { method: 'POST', headers: { 'Content-Type': 'application/json' } });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const run = await res.json();
        const s = run.summary || {};
        showToast('Posture checks: ' + Number(s.pass || 0) + ' pass, ' + Number(s.fail || 0) + ' fail, ' + Number(s.skip || 0) + ' skip, ' + Number(s.error || 0) + ' error', Number(s.fail || 0) ? 'error' : 'success');
      } catch (err) {
        showToast('Could not run posture checks: ' + (err && err.message ? err.message : String(err)), 'error');
      }
      _complianceEvidence.running = false;
      loadComplianceEvidence();
    }

    function complianceSetExportRange(which, value) {
      if (which !== 'since' && which !== 'until') return;
      _complianceEvidence[which] = String(value || '').trim();
    }

    async function complianceDownload(kind, format) {
      if (!viewerIsOwner()) return;
      const qs = new URLSearchParams({ kind: String(kind || ''), format: String(format || '') });
      if (_complianceEvidence.since) qs.set('since', _complianceEvidence.since);
      if (_complianceEvidence.until) qs.set('until', _complianceEvidence.until);
      try {
        const res = await fetch('/api/compliance/export?' + qs.toString(), { cache: 'no-store' });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        const name = hiveFilenameFromDisposition(res.headers.get('Content-Disposition') || '', 'hive-compliance-' + kind + '.' + format);
        const text = await res.text();
        hiveExportViewer({title:name,filename:name,text:text,mime:res.headers.get('Content-Type') || '',sourceURL:'/api/compliance/export?' + qs.toString()});
        showToast('Opened ' + name + ' in the dashboard', 'success');
      } catch (err) {
        showToast('Export failed: ' + (err && err.message ? err.message : String(err)), 'error');
      }
    }

    function complianceSetAttestDraft(field, value) {
      if (!Object.prototype.hasOwnProperty.call(_complianceEvidence.draft, field)) return;
      _complianceEvidence.draft[field] = String(value || '');
    }

    function complianceAttestFrameworks(report) {
      const selected = report.frameworks || [];
      const available = report.available || [];
      const ids = selected.length ? selected : available.map(f => String(f.id || ''));
      return ids.filter(Boolean).map(id => {
        const info = available.find(f => f.id === id);
        return { id, name: info && info.name ? info.name : id };
      });
    }

    async function complianceRecordAttestation() {
      if (!viewerIsOwner() || _complianceEvidence.busy) return;
      const draft = _complianceEvidence.draft;
      const fwEl = document.getElementById('cmp-attest-framework');
      const dayEl = document.getElementById('cmp-attest-date');
      const noteEl = document.getElementById('cmp-attest-note');
      const body = {
        framework: fwEl ? fwEl.value : draft.framework,
        reviewed_on: dayEl ? dayEl.value : (draft.reviewed_on || complianceTodayISO()),
        note: (noteEl ? noteEl.value : draft.note).trim(),
      };
      if (!body.framework || !body.reviewed_on) { showToast('Choose a framework and review date', 'error'); return; }
      const ok = await hiveConfirm({
        title: 'Record attestation',
        message: 'Record that you reviewed ' + body.framework + ' on ' + body.reviewed_on + '?\n\nThe attestation is written to the audit log under your login and cannot be edited. It records that a person looked; it does not change any control status.',
        confirmLabel: 'Record attestation',
        danger: false,
      });
      if (!ok) return;
      _complianceEvidence.busy = true;
      complianceRepaint();
      try {
        const res = await fetch('/api/compliance/attestations', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        if (!res.ok) throw new Error(await saveErrorMessage(res));
        _complianceEvidence.draft = { framework: body.framework, reviewed_on: '', note: '' };
        showToast('Attestation recorded', 'success');
      } catch (err) {
        showToast('Could not record attestation: ' + (err && err.message ? err.message : String(err)), 'error');
      }
      _complianceEvidence.busy = false;
      loadComplianceEvidence();
    }

    function renderComplianceEvidence(report, owner) {
      const head = '<div class="config-section"><h3>Evidence &amp; attestations</h3>';
      if (!owner) return head + '<p class="cmp-note">Exports and attestations are available to owners only.</p></div>';
      const ev = _complianceEvidence;
      const range = '<div class="cmp-toolbar">' +
        '<label class="cmp-note">From <input type="date" value="' + esc(ev.since) + '" data-change-action="complianceSetExportRange" data-arg0="since" data-arg-types="s,v"></label>' +
        '<label class="cmp-note">To <input type="date" value="' + esc(ev.until) + '" data-change-action="complianceSetExportRange" data-arg0="until" data-arg-types="s,v"></label>' +
        '<span class="cmp-note">Empty means the last 30 days.</span>' +
      '</div>';
      const buttons = '<div class="cmp-toolbar">' + COMPLIANCE_EXPORTS.map(x =>
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="complianceDownload" data-arg0="' + esc(x.kind) + '" data-arg1="' + esc(x.format) + '">' + esc(x.label) + '</button>'
      ).join('') + '</div>';
      const exportsHtml = '<h4>Exports</h4>' +
        '<p class="cmp-note">Downloads for the date range below; each export is audited as <code>compliance_export</code>. The audit slice reports the oldest entry it found because the audit log rotates by size. The config snapshot is the redacted effective config with the sha256 of its exact bytes. Bulk review-evidence bundles arrive with #11058.</p>' +
        range + buttons;

      const frameworks = complianceAttestFrameworks(report);
      const draft = ev.draft;
      const chosen = draft.framework || (frameworks[0] ? frameworks[0].id : '');
      const fwOptions = frameworks.map(f => '<option value="' + esc(f.id) + '"' + (f.id === chosen ? ' selected' : '') + '>' + esc(f.name) + '</option>').join('');
      const form = frameworks.length
        ? '<div class="cmp-attest-form">' +
            '<label class="cmp-note">Framework <select id="cmp-attest-framework" data-change-action="complianceSetAttestDraft" data-arg0="framework" data-arg-types="s,v">' + fwOptions + '</select></label>' +
            '<label class="cmp-note">Reviewed on <input id="cmp-attest-date" type="date" max="' + esc(complianceTodayISO()) + '" value="' + esc(draft.reviewed_on || complianceTodayISO()) + '" data-change-action="complianceSetAttestDraft" data-arg0="reviewed_on" data-arg-types="s,v"></label>' +
            '<label class="cmp-note cmp-attest-note">Note <textarea id="cmp-attest-note" rows="2" maxlength="2000" placeholder="Scope, evidence reviewed, follow-ups" data-change-action="complianceSetAttestDraft" data-arg0="note" data-arg-types="s,v">' + esc(draft.note) + '</textarea></label>' +
            '<button type="button" class="hv-btn btn-primary btn-sm" data-action="complianceRecordAttestation"' + (ev.busy ? ' disabled' : '') + '>' + (ev.busy ? 'Recording…' : 'Record attestation') + '</button>' +
          '</div>'
        : '<p class="cmp-note">No framework profiles are available to attest.</p>';
      const names = {};
      frameworks.forEach(f => { names[f.id] = f.name; });
      const items = ev.attestations || [];
      const rows = items.map(a => '<tr>' +
        '<td>' + esc(names[a.framework] || String(a.framework || '')) + '</td>' +
        '<td>' + esc(String(a.reviewed_on || '')) + '</td>' +
        '<td>' + esc(String(a.by || '')) + '</td>' +
        '<td>' + esc(complianceTime(a.at)) + '</td>' +
        '<td>' + esc(String(a.note || '')) + '</td>' +
      '</tr>').join('');
      const table = !ev.loaded
        ? '<p class="cmp-note">Loading attestations…</p>'
        : (items.length
          ? '<table class="cmp-table"><thead><tr><th>Framework</th><th>Reviewed on</th><th>By</th><th>Recorded</th><th>Note</th></tr></thead><tbody>' + rows + '</tbody></table>'
          : '<p class="cmp-note">No attestations recorded yet.</p>');
      const attestHtml = '<h4>Attestations</h4>' +
        '<p class="cmp-note">Record that you reviewed a framework\'s controls. Attestations are kept in the audit log (<code>compliance_attestation</code>) with your login; they record that a person looked and do not change any status.</p>' +
        form + table;
      return head + exportsHtml + attestHtml + '</div>';
    }

    function addSensingItem(key, inputId) {
      const input = document.getElementById(inputId);
      const val = input.value.trim();
      if (!val) return;
      if (!_configState.data.sensing) _configState.data.sensing = {};
      if (!_configState.data.sensing[key]) _configState.data.sensing[key] = [];
      _configState.data.sensing[key].push(val);
      markDirty('sensing', key, _configState.data.sensing[key]);
      input.value = '';
      renderConfigTab(_configState.tab);
    }

    function removeSensingItem(key, index) {
      if (!_configState.data.sensing || !_configState.data.sensing[key]) return;
      _configState.data.sensing[key].splice(index, 1);
      markDirty('sensing', key, _configState.data.sensing[key]);
      renderConfigTab(_configState.tab);
    }

