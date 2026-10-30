    function markReleaseSentinelList(key, value) {
      markDirty('release-sentinel', key, splitConfigLines(value));
    }

    // renderGovReleaseSentinelSection renders the top-level release_sentinel
    // block (hivecommons/hive#9585, #11854): the bounded repair loop that
    // watches the current v<version> release tag's CI and dispatches repair
    // rounds when it fails. Values are marked dirty under the
    // 'release-sentinel' section, which the generic Save dispatcher PUTs to
    // /api/config/governor/release-sentinel.
    function renderGovReleaseSentinelSection(rs) {
      rs = rs || {};
      const envNote = (rs.effective_enabled !== undefined && rs.effective_enabled !== !!rs.enabled)
        ? `<div class="config-help">HIVE_RELEASE_SENTINEL_ENABLED overrides this: the sentinel is currently ${rs.effective_enabled ? 'on' : 'off'}.</div>` : '';
      const toggle = (key, on, label, yamlKey, tip) => `<div class="config-toggle">
            <div class="config-toggle-switch ${on ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="release-sentinel" data-key="${key}"></div>
            <span class="config-toggle-label">${label} ${yamlHint(yamlKey)} ${settingHelpMark('settings', 'features', label, tip)}</span>
          </div>`;
      const list = (key, label, yamlKey, placeholder, tip) => `<div class="config-field">
            <label>${label} ${yamlHint(yamlKey)} ${settingHelpMark('settings', 'features', label, tip)}</label>
            <textarea rows="2" placeholder="${esc(placeholder)}" data-change-action="markReleaseSentinelList" data-arg0="${key}" data-arg-types="s,v">${esc((rs[key] || []).join('\n'))}</textarea>
          </div>`;
      return `
        <div class="qa-section">
          <div class="qa-section-title">Release Sentinel</div>
          ${toggle('enabled', !!rs.enabled, 'Watch release CI and dispatch repair rounds', 'release_sentinel.enabled', 'Default off. When on, the governor watches the CI of the current v&lt;version&gt; release tag and, when it fails, dispatches bounded repair rounds to an agent. Off means nothing is watched, dispatched or persisted.')}
          ${envNote}
          <div class="config-field-row qa-hours">
            <div class="config-field"><label>Repo ${yamlHint('release_sentinel.repo')} ${settingHelpMark('settings', 'features', 'Release sentinel repo', `owner/name whose release tags are watched. Empty means the project's primary repo.`)}</label><input type="text" value="${esc(rs.repo || '')}" placeholder="${esc(rs.effective_repo || 'owner/name')}" data-change-action="markDirty" data-arg0="release-sentinel" data-arg1="repo" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Repair agent ${yamlHint('release_sentinel.agent')} ${settingHelpMark('settings', 'features', 'Release sentinel repair agent', `Agent lane repair rounds are dispatched to. Empty means ci-maintainer.`)}</label><input type="text" value="${esc(rs.agent || '')}" placeholder="ci-maintainer" data-change-action="markDirty" data-arg0="release-sentinel" data-arg1="agent" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field-row qa-hours">
            <div class="config-field"><label>Max rounds ${yamlHint('release_sentinel.max_rounds')} ${settingHelpMark('settings', 'features', 'Release sentinel max rounds', `Cap on repair rounds per release. 0 means the default (5); at most 50.`)}</label><input type="number" min="0" max="50" step="1" value="${rs.max_rounds || 0}" placeholder="5" data-change-action="markDirty" data-arg0="release-sentinel" data-arg1="max_rounds" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Round timeout ${yamlHint('release_sentinel.round_timeout')} ${settingHelpMark('settings', 'features', 'Release sentinel round timeout', `Go duration bounding one repair round, for example 90m or 2h. Empty means 2h.`)}</label><input type="text" value="${esc(rs.round_timeout || '')}" placeholder="2h" data-change-action="markDirty" data-arg0="release-sentinel" data-arg1="round_timeout" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Poll interval ${yamlHint('release_sentinel.poll_interval')} ${settingHelpMark('settings', 'features', 'Release sentinel poll interval', `Minimum Go duration between sentinel passes; rides the governor eval tick. Empty means 5m.`)}</label><input type="text" value="${esc(rs.poll_interval || '')}" placeholder="5m" data-change-action="markDirty" data-arg0="release-sentinel" data-arg1="poll_interval" data-arg-types="s,s,w"></div>
          </div>
          ${list('ignore_workflows', 'Ignored workflows', 'release_sentinel.ignore_workflows[]', 'One workflow name per line', 'Workflows whose runs never block a release (advisory bots and similar noise).')}
          ${list('release_workflows', 'Release workflows', 'release_sentinel.release_workflows[]', 'Tagged Release', 'Workflows that cut a release (name or file name, for example "Tagged Release" or tagged-release.yml). A policy failure in one of them before any tag exists is escalated to a human. Empty turns pre-tag detection off.')}
          <div class="config-field"><label>Release branch ${yamlHint('release_sentinel.release_branch')} ${settingHelpMark('settings', 'features', 'Release sentinel release branch', `Branch release tags are cut from and repair PRs merge into. Empty means the repository's default branch.`)}</label><input type="text" value="${esc(rs.release_branch || '')}" placeholder="default branch" data-change-action="markDirty" data-arg0="release-sentinel" data-arg1="release_branch" data-arg-types="s,s,w"></div>
          ${toggle('retag_enabled', !!rs.retag_enabled, 'Retag after merge', 'release_sentinel.retag_enabled', 'Default off. Once a repair PR for the release merges into the release branch, move the v&lt;version&gt; tag to the merge commit with one leased, atomic tag push. Needs a GitHub App installation; never applies while the sentinel itself is off. HIVE_RELEASE_SENTINEL_RETAG_ENABLED overrides it.')}
          ${toggle('retag_allow_intervening_commits', !!rs.retag_allow_intervening_commits, 'Allow intervening commits when retagging', 'release_sentinel.retag_allow_intervening_commits', 'Default off: the tag only ever moves to the old release plus the fix. On lets it move past other merges that landed on the release branch after the tag was cut.')}
        </div>`;
    }

    // renderGovFleetReportSection renders the self-report settings
    // (hivecommons/hive#11868): whether this hive files or joins issues on
    // the report repo when it notices unusual errors in itself. Values are
    // marked dirty under the 'fleet-report' section, which the generic Save
    // dispatcher PUTs to /api/config/governor/fleet-report. Keys:
    // governor.fleet_report.enabled, governor.fleet_report.repo,
    // governor.fleet_report.max_per_day, governor.fleet_report.include_repos.
    function renderGovFleetReportSection(fr) {
      fr = fr || {};
      const on = fr.enabled === true;
      const reposOn = fr.include_repos !== false;
      const ownersOn = fr.include_owners !== false;
      const ownerNote = ownersOn && Array.isArray(fr.owners) && fr.owners.length ? ` Currently: ${fr.owners.map(o => '@' + o).join(', ')}.` : '';
      const sourceNote = fr.explicit ? 'set in hive.yaml' : (fr.hub_managed ? 'default for hub-managed hives: on' : 'default for self-hosted hives: off (dry-run preview only)');
      const openReports = Array.isArray(fr.open_reports) ? fr.open_reports : [];
      const openHtml = openReports.length === 0
        ? '<span class="qa-note">No self-reports filed or joined yet.</span>'
        : `<span class="qa-note">Self-reports: ${openReports.length} open — ${openReports.slice(0, 6).map(e => e.url ? `<a href="${esc(e.url)}" target="_blank" rel="noopener">#${esc(String(e.number))}</a> ${esc(e.class || e.trigger || '')}${e.joined ? ' (joined)' : ''}` : `#${esc(String(e.number))}`).join(', ')}</span>`;
      return `
        <div class="qa-section">
          <div class="qa-section-title">Self-Reported Issues</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${on ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="fleet-report" data-key="enabled"></div>
            <span class="config-toggle-label">File issues upstream when this hive hits unusual errors ${settingHelpMark('settings', 'features', 'File issues upstream when this hive hits unusual errors', `When on, persistent problems Hive notices in itself (restart storms, repeated kick failures, recovered panics, degraded model discovery, inference auth failures, advisory post failures, ACMM shortfalls) are filed as issues on the report repo — or, when a matching issue is already open, this hive adds itself to it instead. Evidence is shape-only and scrubbed; never tokens or prompts. Rate-limited to the per-day cap below, one new filing per hour, and never re-filed within 7 days of being closed. ${esc(sourceNote)}.`)}</span>
          </div>
          <div class="config-field-row qa-hours">
            <div class="config-field"><label>Report repo ${settingHelpMark('settings', 'features', 'Report repo', `owner/repo that receives self-reports. Default hivecommons/hive. Hub-managed hives without write access relay through the hub, which files under the App identity.`)}</label><input type="text" value="${esc(fr.repo || '')}" placeholder="hivecommons/hive" data-change-action="markDirty" data-arg0="fleet-report" data-arg1="repo" data-arg-types="s,s,v"></div>
            <div class="config-field"><label>Max new issues per day ${settingHelpMark('settings', 'features', 'Max new issues per day', `Cap on NEW issues this hive may open per 24h. Joining an existing issue never counts. 0 means the default (5).`)}</label><input type="number" min="0" step="1" value="${fr.max_per_day || 0}" placeholder="5" data-change-action="markDirty" data-arg0="fleet-report" data-arg1="max_per_day" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${reposOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="fleet-report" data-key="include_repos"></div>
            <span class="config-toggle-label">Name the watched repositories in self-reports ${yamlHint('governor.fleet_report.include_repos')} ${settingHelpMark('settings', 'features', 'Name the watched repositories in self-reports', `When on (default), a self-report lists the owner/repo names this hive watches and the owners derived from them, and the fleet issue merges them into its Affected repos / Affected owners lines, so a maintainer can see which projects an error touches. The report repo is public: turn this off if the watched repos include private repositories — the fields then render as "—" and nothing about the repos leaves the hive.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${ownersOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="fleet-report" data-key="include_owners"></div>
            <span class="config-toggle-label">Name the hive owner(s) in self-reports ${yamlHint('governor.fleet_report.include_owners')} ${settingHelpMark('settings', 'features', 'Name the hive owner(s) in self-reports', `When on (default), a self-report names the GitHub login(s) of this hive's owner — the authorized users holding the owner role and the operator signed in via the device flow — on the hive's line of the fleet issue's Affected hives block, so a maintainer can see who is affected and ping them. Turn this off to render "(anonymous)" instead; no login then leaves the hive.${esc(ownerNote)}`)}</span>
          </div>
          <div class="qa-schedule">${fr.hive_slug ? `<span class="qa-note">Identified upstream as <code>${esc(fr.hive_slug)}</code>. </span>` : ''}${openHtml}</div>
        </div>`;
    }

    // renderGovReplanSection renders the stall-replan lane settings: the
    // periodic check that re-kicks the architect when an approved plan's
    // sub-tasks stop progressing. Values are marked dirty under the 'replan'
    // section, which the generic Save dispatcher PUTs to
    // /api/config/governor/replan; enabled arrives pre-resolved (nil = on).
    function renderGovReplanSection(rp) {
      rp = rp || {};
      const replanOn = rp.enabled !== false;
      return `
        <div class="settings-subsection">
          <div class="settings-subsection-title">Stall Replan</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${replanOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="replan" data-key="enabled"></div>
            <span class="config-toggle-label">Stall replan enabled ${yamlHint('governor.replan.enabled')} ${settingHelpMark('settings', 'features', 'Stall replan enabled', `Periodically scans approved plans whose sub-tasks have stopped progressing and re-kicks the architect to revise them, bounded by a per-plan cap. On by default; a no-op when there are no approved plans.`)}</span>
          </div>
          <div class="config-field-row" style="margin-top:10px">
            <div class="config-field"><label>Scan interval (s) ${yamlHint('governor.replan.interval_s')} ${settingHelpMark('settings', 'features', 'Scan interval (s)', `How often the lane scans for stalled plans, in seconds. Runs off the governor tick, so the effective floor is the governor eval interval. 0 means the default (1800 = 30m).`)}</label><input type="number" min="0" step="1" value="${rp.interval_s || 0}" placeholder="1800" data-change-action="markDirty" data-arg0="replan" data-arg1="interval_s" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Stall threshold (s) ${yamlHint('governor.replan.stall_threshold_s')} ${settingHelpMark('settings', 'features', 'Stall threshold (s)', `How long a plan may go without any child progressing before it is considered stalled, in seconds. 0 means the default (21600 = 6h).`)}</label><input type="number" min="0" step="1" value="${rp.stall_threshold_s || 0}" placeholder="21600" data-change-action="markDirty" data-arg0="replan" data-arg1="stall_threshold_s" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-field"><label>Max replans per plan ${yamlHint('governor.replan.max_replans')} ${settingHelpMark('settings', 'features', 'Max replans per plan', `Caps replans per plan before the lane stops and escalates to a human. 0 means the default (5).`)}</label><input type="number" min="0" step="1" value="${rp.max_replans || 0}" placeholder="5" data-change-action="markDirty" data-arg0="replan" data-arg1="max_replans" data-arg-types="s,s,N"></div>
        </div>`;
    }


    function markQuestionAutocloseLabels(value) {
      markDirty('question-autoclose', 'labels', splitConfigLines(value));
    }

    // renderGovQuestionAutocloseSection renders the question auto-close
    // toggle (hivecommons/hive#9584 dashboard follow-up): once the hive has
    // answered a `question`-labelled issue, it closes automatically after
    // an objection window unless the author reacts 👎. Values are marked
    // dirty under the 'question-autoclose' section, which the generic Save
    // dispatcher PUTs to /api/config/governor/question-autoclose. The live
    // schedule table is loaded separately (read-only, its own endpoint) via
    // loadQuestionAutocloseSchedule, scheduled with setTimeout the same way
    // renderGovFeatures kicks off loadRotationHeadroom.
    // Write-back containment (Settings → Features → Autonomy & ACMM). The
    // three unsolicited knobs, the per-repo mode map and the per-kind
    // level/label matrix are writeback.* in hive.yaml; the counter and
    // drill-down read the live gate through /api/writeback/suppressed.
    // Everything rendered here comes from GitHub (repo names, numbers,
    // previews) and goes through esc().
    function renderWritebackContainmentSection(f, platformDefaults) {
      f = f || {}; platformDefaults = platformDefaults || {};
      const defMin = platformDefaults.writebackMinLevel || 5;
      const optIn = (f.writebackOptInLabels || []).join(', ') || 'hive-review';
      const modes = f.writebackRepoModes || ['off', 'issues-only', 'labels-only', 'full'];
      const perRepo = f.writebackPerRepo || {};
      const perRepoText = Object.keys(perRepo).sort().map(r => r + '=' + perRepo[r]).join('\n');
      const matrix = f.writebackKindMatrix || [];
      const overrides = f.writebackKinds || {};
      setTimeout(loadWritebackSuppressed, 0);
      const rows = matrix.map(row => {
        const k = String(row.kind || ''); const p = row.policy || {}; const ov = overrides[k] || {};
        const scope = p.own_only ? 'hive-authored only' : (p.require_label ? 'opt-in label' : 'any item');
        const unl = p.unlabeled_min_level ? ` · without label L${esc(String(p.unlabeled_min_level))}` : '';
        return `<tr data-wb-kind="${esc(k)}">
          <td><code>${esc(k)}</code></td>
          <td>${esc(scope)}${unl}</td>
          <td><input type="number" min="0" max="6" step="1" class="wb-kind-level" data-setting-key="writeback.kinds.<key>.min_level" value="${esc(ov.min_level != null ? String(ov.min_level) : '')}" placeholder="${esc(String(p.min_level || 0))}" ${p.own_only ? 'disabled title="hive-authored items only; the ACMM floor is the hold-gate level"' : ''} data-change-action="markDirtyWritebackKinds" aria-label="${esc(k)} minimum level"></td>
          <td>${p.own_only ? '—' : `<select class="wb-kind-label" data-setting-key="writeback.kinds.<key>.require_label" data-change-action="markDirtyWritebackKinds" aria-label="${esc(k)} requires opt-in label"><option value="" ${ov.require_label == null ? 'selected' : ''}>default (${p.require_label ? 'yes' : 'no'})</option><option value="true" ${ov.require_label === true ? 'selected' : ''}>yes</option><option value="false" ${ov.require_label === false ? 'selected' : ''}>no</option></select>`}</td>
        </tr>`;
      }).join('');
      return `
        <div class="settings-subsection" id="writeback-containment">
          <div class="settings-subsection-title">Write-back containment ${settingHelpMark('settings', 'features', 'Write-back containment', `One gate every unsolicited GitHub comment, label and review passes through (<code>writeback.*</code>). It applies to items hive did not open and nobody opted in on: platform sweeps (sentinel, hold-gate notices, stage labels, supersession and duplicate notices, reporter-trust holds, …) stay silent on GitHub below the floor and surface in the dashboard instead. It never blocks hive's own PRs and issues, replies to <code>/hive</code> commands, or work started from this dashboard.`)}</div>
          <div id="writeback-banners"></div>
          <div class="config-field-row">
            <div class="config-field"><label>Minimum ACMM level ${yamlHint('writeback.unsolicited.min_level')} ${settingHelpMark('settings', 'features', 'Write-back minimum level', `Below this ACMM level hive writes nothing unsolicited on PRs or issues it did not open. Raises every per-kind floor in the matrix below; it never lowers one. 0 or empty uses the default (${esc(String(defMin))}).`)}</label><input type="number" min="0" max="6" step="1" value="${esc(f.writebackMinLevel != null ? String(f.writebackMinLevel) : '')}" placeholder="${esc(String(defMin))}" data-change-action="markDirty" data-arg0="features" data-arg1="writebackMinLevel" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-toggle" data-setting-key="writeback.unsolicited.require_labels"><div class="config-toggle-switch ${f.writebackRequireLabels !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="writebackRequireLabels"></div><span class="config-toggle-label">Only PRs carrying the review opt-in label ${settingHelpMark('settings', 'features', 'Write-back requires opt-in label', `When on (default), unsolicited write-backs on a foreign PR also need the review opt-in label (${esc(optIn)}, from review.require_labels). Turning it off lets each kind's level floor alone decide.`)}</span></div>
          <div class="config-toggle" data-setting-key="writeback.unsolicited.backfill"><div class="config-toggle-switch ${f.writebackBackfill ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="writebackBackfill"></div><span class="config-toggle-label">Backfill pre-existing items ${settingHelpMark('settings', 'features', 'Write-back backfill', `Off (default): when hive first sees a repo, or first gains write access to one, PRs and issues created before that moment are never written to unsolicited. On: process them like new items.`)}</span></div>
          <div class="config-field"><label>Per-repo mode ${yamlHint('writeback.per_repo')} ${settingHelpMark('settings', 'features', 'Write-back per-repo mode', `One <code>owner/name=mode</code> per line. Modes: ${esc(modes.join(', '))}. <code>off</code> writes nothing unsolicited there; <code>issues-only</code> allows only hive-filed issues (e.g. a repo listed so the deps agent can file regressions); <code>labels-only</code> allows labels but no comments; <code>full</code> skips the level and label gates for that repo. Empty lines clear.`)}</label><textarea rows="3" spellcheck="false" data-setting-key="writeback.per_repo.<key>.mode" placeholder="torch-spyre/torch-spyre=issues-only" data-change-action="markDirtyWritebackPerRepo" data-arg-types="v">${esc(perRepoText)}</textarea></div>
          <details class="config-field"><summary>Per-kind matrix ${yamlHint('writeback.kinds.<kind>.{min_level,require_label}')} ${settingHelpMark('settings', 'features', 'Write-back per-kind matrix', `What hive will say on GitHub at each ACMM level. Each kind has a minimum level and whether the opt-in label is required; leave a cell empty to keep the default shown as placeholder.`)}</summary>
            <table class="qa-table" id="wb-kind-table"><thead><tr><th>Kind</th><th>Scope</th><th>Min level</th><th>Opt-in label</th></tr></thead><tbody>${rows}</tbody></table>
          </details>
          <div class="qa-schedule">
            <div class="qa-schedule-label">Suppressed write-backs (last 24h): <span id="wb-suppressed-count">…</span></div>
            <div id="wb-suppressed-host"><div class="qa-note">Loading…</div></div>
            <div class="config-help">Why suppressed: policy reasons (<code>below-min-level</code>, <code>needs-opt-in-label</code>, <code>backfill-off</code>, <code>repo-mode-*</code>, <code>repo-paused</code>) come from the gates above; content reasons ${yamlHint('writeback.dedupe')} ${settingHelpMark('settings', 'features', 'Write-back duplicate suppression', `Comment kinds with a body also pass a content check. <code>duplicate</code>: the same comment (ignoring marker arguments, SHAs, timestamps and durations) already went to this item within <code>same_item_ttl</code> (24h). <code>burst</code>: the same notice just went to another item in this repo within <code>burst_window</code> (60s); held so a sweep does not fan one message out over many PRs. <code>rate-capped</code>: more than <code>hourly_cap</code> (60) comments of that kind on the repo in an hour; merge-path kinds are never capped. Nothing is queued: the next sweep asks again. <code>enabled: false</code> turns it off.`)} are <code>duplicate</code> (same comment on this item again), <code>burst</code> (same notice to several items within a minute) and <code>rate-capped</code> (hourly per-kind ceiling).</div>
          </div>
          <div class="config-help">Applies on the next sweep. Findings the gate suppressed stay visible in Security → Sentinel and <code>/api/sentinel</code>.</div>
        </div>`;
    }

    function markDirtyWritebackPerRepo(text) {
      const out = {};
      String(text || '').split('\n').forEach(line => {
        const t = line.trim(); if (!t) return;
        const i = t.indexOf('='); if (i <= 0) return;
        out[t.slice(0, i).trim()] = t.slice(i + 1).trim();
      });
      markDirty('features', 'writebackPerRepo', out);
    }

    function markDirtyWritebackKinds() {
      const out = {};
      document.querySelectorAll('#wb-kind-table tr[data-wb-kind]').forEach(tr => {
        const kind = tr.dataset.wbKind; const lvl = tr.querySelector('.wb-kind-level'); const lab = tr.querySelector('.wb-kind-label');
        const ov = {};
        if (lvl && !lvl.disabled && lvl.value !== '') ov.min_level = +lvl.value;
        if (lab && lab.value !== '') ov.require_label = lab.value === 'true';
        out[kind] = Object.keys(ov).length ? ov : null;
      });
      markDirty('features', 'writebackKinds', out);
    }

    function renderWritebackBanners(status) {
      const host = document.getElementById('writeback-banners');
      if (!host) return;
      const banners = (status && status.banners) || [];
      host.innerHTML = banners.map(b => `<div class="qa-note warn">${esc(b.message || '')}</div>`).join('');
    }

    async function loadWritebackSuppressed() {
      const host = document.getElementById('wb-suppressed-host');
      const count = document.getElementById('wb-suppressed-count');
      if (!host) return;
      try {
        const resp = await fetch('/api/writeback/suppressed?limit=100');
        const data = await resp.json();
        if (!resp.ok) throw new Error((data && data.error) || ('HTTP ' + resp.status));
        if (count) count.textContent = String(data.total_24h || 0);
        renderWritebackBanners(data.status);
        const items = data.items || [];
        if (!data.enabled) { host.innerHTML = '<div class="qa-note">Gate not active in this process.</div>'; return; }
        if (items.length === 0) { host.innerHTML = '<div class="qa-note">Nothing suppressed in the last 24 hours.</div>'; return; }
        const rows = items.map(it => `<tr>
            <td>${esc(it.at ? formatDashboardDateTime(it.at) : '')}</td>
            <td>${esc(it.repo || '')}${it.is_pr ? '#' : ' issue #'}${esc(String(it.number || ''))}</td>
            <td><code>${esc(it.kind || '')}</code></td>
            <td>${esc(it.reason || '')}</td>
            <td>${esc(it.preview || it.detail || '')}</td>
          </tr>`).join('');
        host.innerHTML = `<table class="qa-table"><thead><tr><th>When</th><th>Where</th><th>Kind</th><th>Why suppressed</th><th>Would have written</th></tr></thead><tbody>${rows}</tbody></table>`;
      } catch (e) {
        host.innerHTML = '<div class="qa-note warn">Suppressed list unavailable: ' + esc(e.message) + '</div>';
      }
    }

    async function loadSentinelFindings() {
      const host = document.getElementById('sentinel-findings-host');
      if (!host) return;
      try {
        const resp = await fetch('/api/sentinel');
        const data = await resp.json();
        if (!resp.ok) throw new Error((data && data.error) || ('HTTP ' + resp.status));
        const items = (data.findings || []).slice(0, 100);
        if (items.length === 0) { host.innerHTML = '<div class="qa-note">No sentinel findings since this process started.</div>'; return; }
        const rows = items.map(it => `<tr>
            <td>${esc(it.at ? formatDashboardDateTime(it.at) : '')}</td>
            <td>${esc(it.repo || '')}#${esc(String(it.number || ''))}</td>
            <td>${esc(it.author || '')}</td>
            <td>${esc((it.rules || []).join(', '))}</td>
            <td>${it.suppressed ? '<span class="ham-badge-sentinel">dashboard only</span> ' + esc(it.suppressed_reason || '') : (it.blocked ? 'commented + labelled' : esc(it.trusted_reason || 'trusted'))}</td>
          </tr>`).join('');
        host.innerHTML = `<div class="qa-note">${esc(String(data.total || 0))} finding(s), ${esc(String(data.suppressed || 0))} kept off GitHub by write-back containment.</div><table class="qa-table"><thead><tr><th>When</th><th>PR</th><th>Author</th><th>Rules</th><th>Outcome</th></tr></thead><tbody>${rows}</tbody></table>`;
      } catch (e) {
        host.innerHTML = '<div class="qa-note warn">Sentinel findings unavailable: ' + esc(e.message) + '</div>';
      }
    }

    function renderGovQuestionAutocloseSection(qa) {
      qa = qa || {};
      const qaOn = qa.enabled === true;
      setTimeout(() => { if (qaOn) loadQuestionAutocloseSchedule(); }, 0);
      return `
        <div class="qa-section">
          <div class="qa-section-title">Question Auto-Close</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${qaOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="question-autoclose" data-key="enabled"></div>
            <span class="config-toggle-label">Auto-close answered questions ${yamlHint('governor.question_autoclose.enabled')} ${settingHelpMark('settings', 'features', 'Auto-close answered questions', `Once the hive posts an answer on a question-labelled issue, it closes the issue after the window below unless the author reacts 👎 to the answer. Off by default.`)}</span>
          </div>
          <div class="config-field qa-hours"><label>Objection window (hours) ${yamlHint('governor.question_autoclose.hours')} ${settingHelpMark('settings', 'features', 'Objection window (hours)', `How long an answered question stays open for the author to object before it is closed. Minimum 1 hour; default 4.`)}</label><input type="number" min="1" step="1" value="${qa.hours || 4}" placeholder="4" data-change-action="markDirty" data-arg0="question-autoclose" data-arg1="hours" data-arg-types="s,s,N"></div>
          <div class="config-field-row qa-hours">
            <div class="config-field"><label>Question labels ${yamlHint('governor.question_autoclose.labels[]')} ${settingHelpMark('settings', 'features', 'Question labels', `Issue labels that mark an issue as an operator question eligible for the answer/objection close flow. One label per line; empty uses the shipped default.`)}</label><textarea rows="3" data-change-action="markQuestionAutocloseLabels" data-arg-types="v">${esc((qa.labels || []).join('\n'))}</textarea></div>
            <div class="config-field"><label>Human handoff label ${yamlHint('governor.question_autoclose.human_label')} ${settingHelpMark('settings', 'features', 'Human handoff label', `Label applied when auto-close should hand the issue back to a human instead of closing it. Empty uses the shipped default.`)}</label><input type="text" value="${esc(qa.human_label || '')}" placeholder="needs-human" data-change-action="markDirty" data-arg0="question-autoclose" data-arg1="human_label" data-arg-types="s,s,w"></div>
          </div>
          <div class="qa-schedule">
            <div class="qa-schedule-label">Live schedule</div>
            <div id="qa-schedule-host">${qaOn ? '<div class="qa-note">Loading…</div>' : '<div class="qa-note">Enable auto-close to see the live schedule.</div>'}</div>
          </div>
        </div>`;
    }

    // renderQuestionAutocloseScheduleTable renders the live schedule as a
    // plain text-only table — every field comes from GitHub (repo name,
    // issue number, timestamps) and is rendered through esc(), never as raw
    // innerHTML, so an issue in a repo the hive watches cannot inject markup
    // into the dashboard.
    function renderQuestionAutocloseScheduleTable(entries) {
      entries = entries || [];
      if (entries.length === 0) {
        return '<div class="qa-note">Nothing scheduled right now.</div>';
      }
      const rows = entries.map(e => `
        <tr>
          <td>${esc(e.repo || '')}#${esc(String(e.issue || ''))}</td>
          <td>${esc(e.answered_at || '')}</td>
          <td>${esc(e.closes_at || '')}</td>
          <td>${esc(e.state || '')}</td>
        </tr>`).join('');
      return `
        <table class="qa-table">
          <thead>
            <tr>
              <th>Issue</th>
              <th>Answered</th>
              <th>Closes</th>
              <th>State</th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>`;
    }

    // loadQuestionAutocloseSchedule fetches the live schedule and paints it
    // into the placeholder renderGovQuestionAutocloseSection left behind.
    // Errors render as plain text via esc(), matching loadRotationHeadroom's
    // failure path.
    async function loadQuestionAutocloseSchedule() {
      const host = document.getElementById('qa-schedule-host');
      if (!host) return;
      try {
        const resp = await fetch('/api/config/governor/question-autoclose/schedule');
        const data = await resp.json();
        if (!resp.ok) throw new Error((data && data.error) || ('HTTP ' + resp.status));
        host.innerHTML = renderQuestionAutocloseScheduleTable(data.entries);
      } catch (e) {
        host.innerHTML = '<div class="qa-note warn">Schedule unavailable: ' + esc(e.message) + '</div>';
      }
    }

    function projectObservabilityPlatformsChanged(el) {
      const family = el.dataset.family;
      const values = Array.from(el.selectedOptions).map(o => o.value);
      const state = _configState.data.project_observability || (_configState.data.project_observability = {});
      state[family] = values;
      markDirty('project-observability', family, values);
      renderConfigTab('Project Observability');
    }

    function projectObservabilityReferenceChanged(el) {
      const state = _configState.data.project_observability || (_configState.data.project_observability = {});
      const refs = Object.assign({}, state.references || {});
      const platform = el.dataset.platform;
      const key = el.dataset.refKey;
      refs[platform] = Object.assign({}, refs[platform] || {});
      refs[platform][key] = el.value.trim();
      state.references = refs;
      markDirty('project-observability', 'references', refs);
    }

    // renderGovProjectObservability configures the MANAGED PROJECT targets for
    // telemetry/operations. It never accepts raw endpoints or credentials:
    // only environment-variable names and secret-name/key references.
    // operabilityAgentStatusRow renders one read-only status line for an
    // operability agent. This tab deliberately cannot change any of it
    // (#7261): enablement is owned by the agent card and cadences by
    // Agents -> Cadences, and the enable toggles that used to live here were a
    // third switch that silently rewrote every mode's cadence on write and
    // reported an unrelated fact on read.
    function operabilityAgentStatusRow(name, status) {
      status = status || {};
      const cadences = status.cadences || {};
      const modes = Object.keys(cadences).sort();
      let detail;
      if (!status.configured) {
        detail = 'not configured on this hive';
      } else if (!status.enabled) {
        detail = 'disabled on its agent card';
      } else if (modes.length === 0) {
        detail = 'enabled &middot; no cadence in any mode, so it never runs on a schedule';
      } else {
        detail = 'enabled &middot; ' + modes.map(m => `${esc(m)} ${esc(cadences[m])}`).join(' / ');
      }
      return `<div class="config-field mb-4">
        <label>${esc(name)} agent</label>
        <div style="font-size:0.72rem;color:var(--muted)">${detail} &mdash; edit in <b>Agents &rarr; Cadences</b>.</div>
      </div>`;
    }

    function renderGovProjectObservability(p) {
      p = p || {};
      const familyKeys = ['open_source', 'kube_native', 'commercial'];
      const detected = p.detected || {};
      const hasConfiguredTargets = familyKeys.some(key => Array.isArray(p[key]) && p[key].length);
      const hasDetectedTargets = familyKeys.some(key => Array.isArray(detected[key]) && detected[key].length);
      // A first telemetry advisory run discovers likely backends. Preselect its
      // suggestions, but leave them dirty so only an operator's Save persists
      // them as declared project state.
      if (!hasConfiguredTargets && hasDetectedTargets && !p._detectedApplied) {
        p._detectedApplied = true;
        familyKeys.forEach(key => {
          p[key] = Array.isArray(detected[key]) ? detected[key].slice() : [];
          if (p[key].length) markDirty('project-observability', key, p[key]);
        });
      }
      const supported = p.supported || {
        open_source: ['prometheus','grafana','opentelemetry','loki','jaeger','tempo','mimir'],
        kube_native: ['servicemonitor','podmonitor','opentelemetry-operator','grafana-alloy','kube-state-metrics'],
        commercial: ['datadog','new-relic','dynatrace','honeycomb','splunk','grafana-cloud','google-analytics']
      };
      const refs = p.references || {};
      const family = (key, label, yamlKey) => {
        const selected = new Set(Array.isArray(p[key]) ? p[key] : []);
        return `<div class="config-field"><label>${label} ${yamlHint(yamlKey)}</label><select multiple size="${Math.min(7, (supported[key] || []).length)}" data-change-action="gh77" data-family="${key}">${(supported[key] || []).map(v => `<option value="${esc(v)}" ${selected.has(v) ? 'selected' : ''}>${esc(v)}</option>`).join('')}</select></div>`;
      };
      const selectedPlatforms = [...new Set([...(p.open_source || []), ...(p.kube_native || []), ...(p.commercial || [])])].sort();
      const refRows = selectedPlatforms.map(platform => {
        const ref = refs[platform] || {};
        return `<div style="border:1px solid var(--border);border-radius:7px;padding:9px;margin-top:var(--sp-4)"><div style="font-size:var(--fs-sm);font-weight:600;margin-bottom:var(--sp-3)">${esc(platform)}</div><div class="config-field-row"><div class="config-field"><label>Endpoint environment variable ${yamlHint('governor.project_observability.references.<key>.endpoint_env')}</label><input type="text" value="${esc(ref.endpoint_env || '')}" placeholder="OTEL_EXPORTER_OTLP_ENDPOINT" data-change-action="gh78" data-platform="${esc(platform)}" data-ref-key="endpoint_env"></div><div class="config-field"><label>Credential secret reference ${yamlHint('governor.project_observability.references.<key>.credential_secret')}</label><input type="text" value="${esc(ref.credential_secret || '')}" placeholder="observability-credentials/api-key" data-change-action="gh78" data-platform="${esc(platform)}" data-ref-key="credential_secret"></div></div></div>`;
      }).join('');
      return `
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">Targets for the managed project's telemetry and operations agents. Hive's own OTLP export remains in <b>Features</b>.</p>
        ${hasDetectedTargets ? '<div style="font-size:0.72rem;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:10px;margin-bottom:14px">Telemetry findings supplied detected platform suggestions. Review the selections below; Save explicitly confirms and persists them.</div>' : ''}
        <div style="font-size:0.72rem;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:10px;margin-bottom:14px">Select which observability backends the telemetry and operations agents should target in managed repositories. Whether those agents run at all is not set here: enablement lives on the agent card and per-mode schedules live in Agents → Cadences. Their current state is shown below, read-only.</div>
        ${operabilityAgentStatusRow('telemetry', p.telemetry_status)}
        ${operabilityAgentStatusRow('operations', p.operations_status)}
        <div style="margin:18px 0;padding-top:14px;border-top:1px solid var(--border)"><div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:var(--sp-4)">Target platforms</div><div class="config-field-row">${family('open_source','Open source','governor.project_observability.open_source[]')}${family('kube_native','Kube-native','governor.project_observability.kube_native[]')}</div>${family('commercial','Commercial','governor.project_observability.commercial[]')}</div>
        <div style="margin:18px 0;padding-top:14px;border-top:1px solid var(--border)"><div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:var(--sp-3)">Safe references</div><p style="font-size:0.72rem;color:var(--muted)">Names only. Literal URLs, tokens, and API keys are rejected server-side.</p>${refRows || '<p style="font-size:0.72rem;color:var(--muted)">Select a platform to configure its references.</p>'}</div>`;
    }


    function markAdvisoryQueueHealth(key, value) {
      const current = (((_configState.data || {}).advisory || {}).queue_health) || {};
      const prev = ((_configState.dirty.advisory || {}).queue_health) || {};
      const next = Object.assign({}, current, prev, { [key]: value });
      if (!_configState.data.advisory) _configState.data.advisory = {};
      _configState.data.advisory.queue_health = next;
      markDirty('advisory', 'queue_health', next);
    }

    // renderGovAdvisory renders the advisory digest settings: how many findings
    // the digest shows, and the lifecycle controls that stop findings from
    // accumulating forever once the condition behind them is gone.
    //
    // Values are marked dirty under the 'advisory' section, which the generic
    // Save dispatcher PUTs to /api/config/governor/advisory; they are prefilled
    // from the same GET /api/config/governor payload every other tab uses.
    function renderGovAdvisory(a) {
      a = a || {};
      const maxFindings = a.max_findings === undefined ? 10 : a.max_findings;
      const stalenessDays = a.staleness_days === undefined ? 7 : a.staleness_days;
      const updateIntervalS = a.update_interval_s === undefined ? 0 : a.update_interval_s;
      const postRetryBackoffS = a.post_retry_backoff_s === undefined ? 0 : a.post_retry_backoff_s;
      const qh = a.queue_health || {};
      return `
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">Controls the advisory digest posted to your repo: how much of it is shown, and when findings nobody re-reports are retired.</p>

        <div style="margin:18px 0">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:var(--sp-3)">What is shown</div>
          <div class="config-field">
            <label>Max findings shown ${yamlHint('governor.advisory.max_findings')} ${settingHelpMark('advisory', '', 'Max findings shown', `The digest renders this many findings, ranked by severity then recency across all agents. The remainder is counted in a note, never dropped silently. To render every finding, turn on "Show all findings" below.`)}</label>
            <input type="number" min="1" step="1" value="${maxFindings}" data-change-action="gh59">
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${a.show_all ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="advisory" data-key="show_all"></div>
            <span class="config-toggle-label">Show all findings ${yamlHint('governor.advisory.show_all')} <span style="color:var(--muted);font-weight:400">— override the max and show everything</span></span>
          </div>
        </div>

        <div style="margin:18px 0;padding-top:14px;border-top:1px solid var(--border)">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:var(--sp-3)">Where and when it posts</div>
          <div class="config-field-row">
            <div class="config-field">
              <label>Digest target ${yamlHint('governor.advisory.target')} ${settingHelpMark('advisory', '', 'Digest target', `Choose where the advisory digest is rewritten each cycle. GitHub uses the pinned advisory issue on the primary repo; Linear posts to the issue named below.`)}</label>
              <select data-change-action="markDirty" data-arg0="advisory" data-arg1="target" data-arg-types="s,s,v">
                <option value="github" ${(a.target || 'github') === 'github' ? 'selected' : ''}>GitHub advisory issue</option>
                <option value="linear" ${a.target === 'linear' ? 'selected' : ''}>Linear issue</option>
              </select>
            </div>
            <div class="config-field">
              <label>Linear issue ${yamlHint('governor.advisory.linear_issue')} ${settingHelpMark('advisory', '', 'Linear issue', `Issue key such as ONB-123. Required when the digest target is Linear.`)}</label>
              <input type="text" value="${esc(a.linear_issue || '')}" placeholder="ONB-123" data-change-action="markDirty" data-arg0="advisory" data-arg1="linear_issue" data-arg-types="s,s,w">
            </div>
          </div>
          <div class="config-field">
            <label>Update interval (s) ${yamlHint('governor.advisory.update_interval_s')} ${settingHelpMark('advisory', '', 'Update interval (s)', `How often the digest comment on the pinned advisory issue is checked and refreshed. 0 = default (60s, every governor cycle). Longer values (30&ndash;3600s) reduce GitHub API writes and notification churn on watched repos; changes apply live from the next cycle. Unchanged digests are already skipped, and the periodic forced refresh that heals hand-edited comments stretches with this interval.`)}</label>
            <input type="number" min="0" max="3600" step="1" value="${updateIntervalS}" data-change-action="gh76">
          </div>
        </div>

        <div style="margin:18px 0;padding-top:14px;border-top:1px solid var(--border)">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:var(--sp-3)">Keeping findings current</div>
          <div class="config-field-row">
            <div class="config-field">
              <label>Staleness window (days) ${yamlHint('governor.advisory.staleness_days')} ${settingHelpMark('advisory', '', 'Staleness window (days)', `Agents re-report a finding for as long as its condition holds. A finding no agent has re-reported within this many days is auto-closed and leaves the digest.`)}</label>
              <input type="number" min="1" step="1" value="${stalenessDays}" data-change-action="gh60">
            </div>
            <div class="config-field">
              <label>Post retry backoff (s) ${yamlHint('governor.advisory.post_retry_backoff_s')} ${settingHelpMark('advisory', '', 'Post retry backoff (s)', `Seconds to pause advisory digest writes after GitHub refuses the pinned comment with a permissions error. 0 uses the shipped default; a negative value disables the backoff.`)}</label>
              <input type="number" step="1" value="${postRetryBackoffS}" data-change-action="markDirty" data-arg0="advisory" data-arg1="post_retry_backoff_s" data-arg-types="s,s,N">
            </div>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${a.pr_autoclose === false ? '' : 'on'}" data-action="toggleConfigSwitch" data-section="advisory" data-key="pr_autoclose"></div>
            <span class="config-toggle-label">Close findings fixed by a merged PR ${yamlHint('governor.advisory.pr_autoclose')} <span style="color:var(--muted);font-weight:400">— match merged PR titles against open findings</span></span>
          </div>
        </div>

        <div class="config-field">
          <div class="config-subgroup">Queue health advice thresholds</div>
          <div class="config-help">Whole-number thresholds for weekly owner advice. 0 uses the shipped default.</div>
          <div class="config-field-row">
            <div class="config-field"><label>Blocked PR % ${yamlHint('governor.advisory.queue_health.blocked_pr_pct')}</label><input type="number" min="0" step="1" value="${qh.blocked_pr_pct || 0}" data-change-action="markAdvisoryQueueHealth" data-arg0="blocked_pr_pct" data-arg-types="s,N"></div>
            <div class="config-field"><label>Needs-human PR % ${yamlHint('governor.advisory.queue_health.needs_human_pr_pct')}</label><input type="number" min="0" step="1" value="${qh.needs_human_pr_pct || 0}" data-change-action="markAdvisoryQueueHealth" data-arg0="needs_human_pr_pct" data-arg-types="s,N"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Needs-human issue % ${yamlHint('governor.advisory.queue_health.needs_human_issue_pct')}</label><input type="number" min="0" step="1" value="${qh.needs_human_issue_pct || 0}" data-change-action="markAdvisoryQueueHealth" data-arg0="needs_human_issue_pct" data-arg-types="s,N"></div>
            <div class="config-field"><label>Stale blocked PRs ${yamlHint('governor.advisory.queue_health.stale_blocked_prs')}</label><input type="number" min="0" step="1" value="${qh.stale_blocked_prs || 0}" data-change-action="markAdvisoryQueueHealth" data-arg0="stale_blocked_prs" data-arg-types="s,N"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Lane share % ${yamlHint('governor.advisory.queue_health.lane_share_pct')}</label><input type="number" min="0" step="1" value="${qh.lane_share_pct || 0}" data-change-action="markAdvisoryQueueHealth" data-arg0="lane_share_pct" data-arg-types="s,N"></div>
            <div class="config-field"><label>Check share % ${yamlHint('governor.advisory.queue_health.check_share_pct')}</label><input type="number" min="0" step="1" value="${qh.check_share_pct || 0}" data-change-action="markAdvisoryQueueHealth" data-arg0="check_share_pct" data-arg-types="s,N"></div>
          </div>
        </div>`;
    }

    // markWorkSourceDirty records a work-source edit under the 'work-source'
    // dirty section (the generic Save dispatcher PUTs it to
    // /api/config/governor/work-source). Keys use dot notation for nested
    // sub-source fields, e.g. 'github_projects.org' — split and merged into a
    // nested object so the server receives the same shape as hive.yaml. The
    // live _configState.data.work_source copy is updated too, so re-renders
    // (the type dropdown swaps sub-forms) keep unsaved edits.
    function markWorkSourceDirty(path, value) {
      if (!_configState.dirty['work-source']) _configState.dirty['work-source'] = {};
      if (!_configState.data.work_source) _configState.data.work_source = {};
      const parts = path.split('.');
      let dirtyObj = _configState.dirty['work-source'];
      let dataObj = _configState.data.work_source;
      for (let i = 0; i < parts.length - 1; i++) {
        if (!dirtyObj[parts[i]]) dirtyObj[parts[i]] = {};
        if (!dataObj[parts[i]]) dataObj[parts[i]] = {};
        dirtyObj = dirtyObj[parts[i]];
        dataObj = dataObj[parts[i]];
      }
      dirtyObj[parts[parts.length - 1]] = value;
      dataObj[parts[parts.length - 1]] = value;
    }

    // wsLinearTeams returns the live (unsaved) Linear team list, creating the
    // nested objects on first use so edits before a save have somewhere to go.
    function wsLinearTeams() {
      if (!_configState.data.work_source) _configState.data.work_source = {};
      if (!_configState.data.work_source.linear) _configState.data.work_source.linear = {};
      const lin = _configState.data.work_source.linear;
      if (!Array.isArray(lin.teams)) lin.teams = [];
      return lin.teams;
    }
    // wsLinearTeamEdit updates one field of one team and marks the whole
    // team list dirty: the API replaces the list when 'linear.teams' is
    // present, so every edit sends the complete array.
    function wsLinearTeamEdit(idx, field, value) {
      const teams = wsLinearTeams();
      const t = teams[idx];
      if (!t) return;
      if (field === 'states') {
        t.states = value.split(',').map(s => s.trim()).filter(Boolean);
      } else if (field === 'cycles') {
        t.cycles = value ? 'current' : '';
      } else if (field === 'projects') {
        t.projects = value.split(',').map(s => s.trim()).filter(Boolean).map(s => {
          const eq = s.indexOf('=');
          return eq < 0 ? { name: s } : { name: s.slice(0, eq).trim(), repo: s.slice(eq + 1).trim() };
        });
      } else {
        t[field] = value;
      }
      markWorkSourceDirty('linear.teams', teams);
    }
    function wsLinearTeamAdd() {
      const teams = wsLinearTeams();
      teams.push({ key: '', repo: '', states: [], cycles: '', projects: [] });
      markWorkSourceDirty('linear.teams', teams);
      renderConfigTab('Work Source');
    }
    function wsLinearTeamRemove(idx) {
      const teams = wsLinearTeams();
      teams.splice(idx, 1);
      markWorkSourceDirty('linear.teams', teams);
      renderConfigTab('Work Source');
    }
    function wsLinearToggleAssignedOnly(el) {
      markWorkSourceDirty('linear.assigned_only', el.classList.toggle('on'));
    }
    function wsForgeReposFromText(value) {
      return String(value || '').split(/[\n,]+/).map(s => s.trim()).filter(Boolean).map(s => {
        const eq = s.indexOf('=');
        return eq < 0 ? { repo: s } : { repo: s.slice(0, eq).trim(), work_repo: s.slice(eq + 1).trim() };
      });
    }
    function wsForgeReposText(repos) {
      return (Array.isArray(repos) ? repos : []).map(r => r.work_repo ? `${r.repo}=${r.work_repo}` : r.repo).join('\n');
    }

    function wsWavefrontSource(wf) {
      wf = wf || {};
      if (wf.source === 'url' || wf.source === 'path') return wf.source;
      return wf.url ? 'url' : 'path';
    }

    function wsWavefrontErrors(wf) {
      wf = wf || {};
      if (!wf.enabled) return [];
      const path = String(wf.path || '').trim();
      const url = String(wf.url || '').trim();
      const repo = String(wf.repo || '').trim();
      const errors = [];
      if ((path ? 1 : 0) + (url ? 1 : 0) !== 1) errors.push('Choose exactly one graph source: file path or URL.');
      if (url && !/^https?:\/\//.test(url)) errors.push('URL must start with http:// or https://.');
      if (!/^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/.test(repo)) errors.push('Repo must use owner/name.');
      return errors;
    }

    function wsWavefrontSetEnabled(el) {
      markWorkSourceDirty('wavefront.enabled', el.classList.toggle('on'));
      renderConfigTab('Work Source');
    }

    function wsWavefrontSetSource(source) {
      markWorkSourceDirty('wavefront.source', source === 'url' ? 'url' : 'path');
      if (source === 'url') {
        markWorkSourceDirty('wavefront.path', '');
      } else {
        markWorkSourceDirty('wavefront.url', '');
      }
      renderConfigTab('Work Source');
    }

    function wsWavefrontSetLocation(source, value) {
      markWorkSourceDirty('wavefront.source', source === 'url' ? 'url' : 'path');
      if (source === 'url') {
        markWorkSourceDirty('wavefront.url', value);
        markWorkSourceDirty('wavefront.path', '');
      } else {
        markWorkSourceDirty('wavefront.path', value);
        markWorkSourceDirty('wavefront.url', '');
      }
    }

    // renderGovWorkSource renders the work_source settings: which external planning
    // tool hive reads work items from (Step 01 of the loop). Saved to
    // /api/config/governor/work-source.
    function renderGovWorkSource(ws) {
      ws = ws || {};
      const type = ws.type || '';
      const ghp = ws.github_projects || {};
      const lin = ws.linear || {};
      const jira = ws.jira || {};
      const gitea = ws.gitea || {};
      const gitlab = ws.gitlab || {};
      const ext = ws.external || {};
      const wf = ws.wavefront || {};
      const wfSource = wsWavefrontSource(wf);
      const wfErrors = wsWavefrontErrors(wf);
      const linTeams = Array.isArray(lin.teams) ? lin.teams : [];
      const linAgents = Array.isArray(_configState.data.agents) ? _configState.data.agents.slice().sort() : [];

      const typeOpts = [
        ['', 'GitHub Issues (default)'],
        ['github_projects', 'GitHub Projects v2'],
        ['linear', 'Linear'],
        ['jira', 'Jira'],
        ['gitea', 'Gitea / Forgejo'],
        ['gitlab', 'GitLab'],
        ['external', 'External provider (HTTP)'],
      ].map(([v, l]) => `<option value="${v}" ${type === v ? 'selected' : ''}>${l}</option>`).join('');

      return `
        <p style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:14px">
          Choose where hive reads work items from. Steps 03–07 (fix agent, change request, review, merge) are
          unaffected — agents still open pull requests or merge requests through the connected forge.
        </p>

        <div class="config-field mb-7">
          <label>Work source ${settingHelpMark('work-sources', '', 'Work source', `Issues is the default work-source concept; GitHub Issues is used when no alternate source is configured. GitHub Projects, Linear, Jira, Gitea/Forgejo, and GitLab are opt-in — add credentials below after selecting.`)}</label>
          <select data-change-action="gh61">${typeOpts}</select>
        </div>

        ${type === 'github_projects' ? `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:14px">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:10px">GitHub Projects v2</div>
          <div class="config-field">
            <label>Org</label>
            <input type="text" placeholder="my-org" value="${esc(ghp.org||'')}" data-change-action="markWorkSourceDirty" data-arg0="github_projects.org" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Project number</label>
            <input type="number" min="1" step="1" value="${ghp.project_number||''}" placeholder="42" data-change-action="gh62">
          </div>
          <div class="config-field">
            <label>Status filter ${settingHelpMark('work-sources', '', 'Status filter', `Comma-separated Status field values to include, e.g. "Todo, Backlog". Leave empty for all open items.`)}</label>
            <input type="text" placeholder="Todo, Backlog" value="${esc((ghp.states||[]).join(', '))}" data-change-action="gh63">
          </div>
          <div class="config-field">
            <label>Priority field ${settingHelpMark('work-sources', '', 'Priority field', `Name of the project's priority custom field, e.g. "Priority". Leave empty if not using priority.`)}</label>
            <input type="text" placeholder="Priority" value="${esc(ghp.priority_field||'')}" data-change-action="markWorkSourceDirty" data-arg0="github_projects.priority_field" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Iteration field ${settingHelpMark('work-sources', '', 'Iteration field', `Name of the project's sprint/iteration field. When set, only items in the current iteration are shown. Leave empty to include all.`)}</label>
            <input type="text" placeholder="Sprint" value="${esc(ghp.iteration_field||'')}" data-change-action="markWorkSourceDirty" data-arg0="github_projects.iteration_field" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Default repo ${settingHelpMark('work-sources', '', 'Default repo', `Fallback repo (owner/name) agents clone when an issue has no repository. Usually leave empty if your project only tracks one repo.`)}</label>
            <input type="text" placeholder="my-org/my-repo" value="${esc(ghp.default_repo||'')}" data-change-action="markWorkSourceDirty" data-arg0="github_projects.default_repo" data-arg-types="s,w">
          </div>
        </div>` : ''}

        ${type === 'gitea' ? `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:14px">
          <div style="color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:10px">Gitea / Forgejo</div>
          <div class="config-field">
            <label>Base URL</label>
            <input type="text" placeholder="https://codeberg.org" value="${esc(gitea.base_url||'')}" data-change-action="markWorkSourceDirty" data-arg0="gitea.base_url" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Token ${settingHelpMark('settings', 'features', 'Help', `Use \${GITEA_TOKEN} or leave empty to read token_env at runtime. The stored value is never shown after save.`)}</label>
            <input type="text" placeholder="${gitea.token_set ? '(set — leave blank to keep)' : '\${GITEA_TOKEN}'}" value="" data-change-action="markWorkSourceDirty" data-arg0="gitea.token" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Token env</label>
            <input type="text" placeholder="GITEA_TOKEN" value="${esc(gitea.token_env||'')}" data-change-action="markWorkSourceDirty" data-arg0="gitea.token_env" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Repositories ${settingHelpMark('settings', 'features', 'Help', `One per line: source repo, or source=work_repo when agents clone a different GitHub repo.`)}</label>
            <textarea rows="4" placeholder="acme/service&#10;acme/other=github-org/other" data-change-action="wsGiteaReposChanged">${esc(wsForgeReposText(gitea.repos))}</textarea>
          </div>
          <div class="config-field"><label>Labels</label><input type="text" placeholder="ready, bug" value="${esc((gitea.labels||[]).join(', '))}" data-change-action="wsGiteaLabelsChanged"></div>
          <div class="config-field"><label>States</label><input type="text" placeholder="open" value="${esc((gitea.states||[]).join(', '))}" data-change-action="wsGiteaStatesChanged"></div>
          <div class="config-field"><label>Assignee</label><input type="text" placeholder="hive-bot" value="${esc(gitea.assignee||'')}" data-change-action="markWorkSourceDirty" data-arg0="gitea.assignee" data-arg-types="s,w"></div>
          <div class="config-field"><label>Hold labels</label><input type="text" placeholder="hold, blocked" value="${esc((gitea.hold_labels||[]).join(', '))}" data-change-action="wsGiteaHoldLabelsChanged"></div>
        </div>` : ''}

        ${type === 'gitlab' ? `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:14px">
          <div style="color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:10px">GitLab</div>
          <div class="config-field">
            <label>Base URL</label>
            <input type="text" placeholder="https://gitlab.com" value="${esc(gitlab.base_url||'')}" data-change-action="markWorkSourceDirty" data-arg0="gitlab.base_url" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Token ${settingHelpMark('settings', 'features', 'Help', `Use \${GITLAB_TOKEN} or leave empty to read token_env at runtime. The stored value is never shown after save.`)}</label>
            <input type="text" placeholder="${gitlab.token_set ? '(set — leave blank to keep)' : '\${GITLAB_TOKEN}'}" value="" data-change-action="markWorkSourceDirty" data-arg0="gitlab.token" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Token env</label>
            <input type="text" placeholder="GITLAB_TOKEN" value="${esc(gitlab.token_env||'')}" data-change-action="markWorkSourceDirty" data-arg0="gitlab.token_env" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Projects ${settingHelpMark('settings', 'features', 'Help', `One per line: GitLab project path, or project=work_repo when agents clone a different GitHub repo.`)}</label>
            <textarea rows="4" placeholder="acme/subgroup/service&#10;acme/other=github-org/other" data-change-action="wsGitLabReposChanged">${esc(wsForgeReposText(gitlab.repos))}</textarea>
          </div>
          <div class="config-field"><label>Labels</label><input type="text" placeholder="ready, bug" value="${esc((gitlab.labels||[]).join(', '))}" data-change-action="wsGitLabLabelsChanged"></div>
          <div class="config-field"><label>States</label><input type="text" placeholder="opened" value="${esc((gitlab.states||[]).join(', '))}" data-change-action="wsGitLabStatesChanged"></div>
          <div class="config-field"><label>Assignee</label><input type="text" placeholder="hive-bot" value="${esc(gitlab.assignee||'')}" data-change-action="markWorkSourceDirty" data-arg0="gitlab.assignee" data-arg-types="s,w"></div>
          <div class="config-field"><label>Hold labels</label><input type="text" placeholder="hold, blocked" value="${esc((gitlab.hold_labels||[]).join(', '))}" data-change-action="wsGitLabHoldLabelsChanged"></div>
        </div>` : ''}

        ${type === 'external' ? `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:14px">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:10px">External provider (HTTP)</div>
          <p style="font-size:var(--fs-sm);color:var(--muted);margin:0 0 10px">
            A tracker connected via a small HTTPS shim speaking <code>hive.worksource/v1</code>. Hive stays the
            only authority over work identity, credentials, and admission — the provider only lists items.
          </p>
          <div class="config-field">
            <label>Name ${settingHelpMark('settings', 'features', 'Help', `Stable source type persisted in every work key. Lowercase letters, digits, underscore, starting with a letter (e.g. "acme"). Cannot be changed later without losing track of existing holds.`)}</label>
            <input type="text" placeholder="acme" value="${esc(ext.name||'')}" data-change-action="markWorkSourceDirty" data-arg0="external.name" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Display name ${settingHelpMark('settings', 'features', 'Help', `Dashboard label for this source (the source badge and this tab). Defaults to Name when empty.`)}</label>
            <input type="text" placeholder="Acme Tracker" value="${esc(ext.display_name||'')}" data-change-action="markWorkSourceDirty" data-arg0="external.display_name" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Base URL ${settingHelpMark('settings', 'features', 'Help', `Provider shim root, e.g. https://acme-shim.internal:8443. Must be https, except for a loopback host (a local sidecar shim).`)}</label>
            <input type="text" placeholder="https://acme-shim.internal:8443" value="${esc(ext.base_url||'')}" data-change-action="markWorkSourceDirty" data-arg0="external.base_url" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Auth token ${settingHelpMark('settings', 'features', 'Help', `Bearer token sent to the provider. Must be written as an environment reference, e.g. $ACME_WORKSOURCE_TOKEN — a literal value is rejected so the resolved secret never reaches the saved config.`)}</label>
            <input type="text" placeholder="$ACME_WORKSOURCE_TOKEN" value="${esc(ext.auth_token||'')}" data-change-action="markWorkSourceDirty" data-arg0="external.auth_token" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>CA bundle ${settingHelpMark('settings', 'features', 'Help', `Optional. PEM CA bundle for a private CA, also as an environment reference, e.g. $ACME_WORKSOURCE_CA. Leave empty to trust the system roots.`)}</label>
            <input type="text" placeholder="$ACME_WORKSOURCE_CA" value="${esc(ext.ca_bundle||'')}" data-change-action="markWorkSourceDirty" data-arg0="external.ca_bundle" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Source repositories ${settingHelpMark('settings', 'features', 'Help', `Required allow-list of owner/name repositories the provider may route work to. An item naming anything else is dropped.`)}</label>
            <input type="text" placeholder="your-org/app, your-org/platform" value="${esc((ext.repos||[]).join(', '))}" data-change-action="wsExternalReposChanged">
          </div>
          <div class="config-field">
            <label>Hold labels ${settingHelpMark('settings', 'features', 'Help', `Comma-separated label values Hive treats as a hold, in addition to whatever the provider filters.`)}</label>
            <input type="text" placeholder="hold, blocked" value="${esc((ext.hold_labels||[]).join(', '))}" data-change-action="wsExternalHoldLabelsChanged">
          </div>
          <div class="config-field">
            <label>Timeout (seconds) ${settingHelpMark('settings', 'features', 'Help', `Bounds the whole ListIssues call, pagination included. 0 uses the default (30s); the hub-enforced ceiling is 300s.`)}</label>
            <input type="number" min="0" max="300" step="1" placeholder="30" value="${ext.timeout_seconds||''}" data-change-action="wsExternalTimeoutChanged">
          </div>
          <div class="config-field m-0">
            <label>Dropped items this process ${settingHelpMark('settings', 'features', 'Help', `How many items this source has withheld since the hub last restarted, for failing a validation rule (duplicate key, bad external_id, disallowed repo, …). Counts only; details are in the hub log.`)}</label>
            <div><span class="status-badge ${(ext.dropped_items||0) > 0 ? 'disabled' : 'done'}">${(ext.dropped_items||0)}</span></div>
          </div>
        </div>` : ''}

        ${type === 'linear' ? `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:14px">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:10px">Linear</div>
          <div class="config-field">
            <label>API key ${settingHelpMark('work-sources', '', 'API key', `Linear personal API key (Settings → API → Personal API keys). Stored as a secret reference; use \${ENV_VAR} syntax to avoid putting the raw key in config. The stored value is never shown — leave blank to keep it.`)}</label>
            <input type="text" placeholder="${lin.api_key_set ? '(set — leave blank to keep)' : '\${LINEAR_API_KEY}'}" value="" data-change-action="markWorkSourceDirty" data-arg0="linear.api_key" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Hold labels ${settingHelpMark('work-sources', '', 'Hold labels', `Comma-separated Linear label names that gate an issue (like GitHub's "hold" label). Issues carrying any of these labels are excluded.`)}</label>
            <input type="text" placeholder="hold, blocked" value="${esc((lin.hold_labels||[]).join(', '))}" data-change-action="gh64">
          </div>
          <div class="config-field">
            <label>Session agent ${settingHelpMark('work-sources', '', 'Session agent', `Agent that takes Linear agent sessions (delegations and @-mentions). Required when the hive runs more than one agent; with exactly one agent it is implicit.`)}</label>
            ${linAgents.length ? `<select data-change-action="markWorkSourceDirty" data-arg0="linear.session_agent" data-arg-types="s,w">
              <option value="" ${!lin.session_agent ? 'selected' : ''}>${linAgents.length === 1 ? `(implicit: ${esc(linAgents[0])})` : '— choose an agent —'}</option>
              ${linAgents.map(a => `<option value="${esc(a)}" ${lin.session_agent === a ? 'selected' : ''}>${esc(a)}</option>`).join('')}
            </select>` : `<input type="text" placeholder="agent name" value="${esc(lin.session_agent||'')}" data-change-action="markWorkSourceDirty" data-arg0="linear.session_agent" data-arg-types="s,w">`}
          </div>
          <div class="config-toggle" style="margin:10px 0 14px">
            <div class="config-toggle-switch ${lin.assigned_only ? 'on' : ''}" data-action="wsLinearToggleAssignedOnly" data-arg-types="t"></div>
            <span class="config-toggle-label">Assigned only <span style="color:var(--muted);font-weight:400">— enumerate only issues assigned or delegated to the Hive app (requires a connected Linear agent)</span></span>
          </div>
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin:var(--sp-3) var(--sp-0) var(--sp-4)">Teams ${settingHelpMark('work-sources', '', 'Teams', `Each Linear team maps to the source repository (owner/name) agents clone for its issues. States narrows enumeration to those workflow states (e.g. "Todo, In Progress") — without it the whole backlog is counted and can push the governor into SURGE. Projects override the repo per Linear project.`)}</div>
          ${linTeams.length === 0 ? `<p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">No teams yet — add one so hive knows which source repository each team's issues belong to.</p>` : ''}
          ${linTeams.map((t, i) => `
          <div style="border:1px solid var(--border);border-radius:var(--r);padding:10px;margin-bottom:var(--sp-4)">
            <div style="display:grid;grid-template-columns:1fr 1fr auto;gap:var(--sp-4);align-items:end">
              <div class="config-field m-0">
                <label>Team key</label>
                <input type="text" placeholder="ENG" value="${esc(t.key||'')}" data-change-action="wsLinearTeamEdit" data-arg0="${i}" data-arg1="key" data-arg-types="j,s,w">
              </div>
              <div class="config-field m-0">
                <label>Source repository</label>
                <input type="text" placeholder="my-org/my-repo" value="${esc(t.repo||'')}" data-change-action="wsLinearTeamEdit" data-arg0="${i}" data-arg1="repo" data-arg-types="j,s,w">
              </div>
              <button type="button" class="hv-btn btn-danger btn-secondary btn-sm" data-action="wsLinearTeamRemove" data-arg0="${i}" data-arg-types="j">Remove</button>
            </div>
            <div style="display:grid;grid-template-columns:1fr auto;gap:var(--sp-4);align-items:end;margin-top:var(--sp-4)">
              <div class="config-field m-0">
                <label>States ${settingHelpMark('work-sources', '', 'States', `Comma-separated Linear workflow state names to include. Empty = every open state (whole backlog).`)}</label>
                <input type="text" placeholder="Todo, In Progress" value="${esc((t.states||[]).join(', '))}" data-change-action="wsLinearTeamEdit" data-arg0="${i}" data-arg1="states" data-arg-types="j,s,w">
              </div>
              <label style="font-size:var(--fs-sm);display:flex;align-items:center;gap:var(--sp-3);white-space:nowrap"><input type="checkbox" ${t.cycles === 'current' ? 'checked' : ''} data-change-action="wsLinearTeamEdit" data-arg0="${i}" data-arg1="cycles" data-arg-types="j,s,c"> Current cycle only</label>
            </div>
            <div class="config-field" style="margin:var(--sp-4) var(--sp-0) var(--sp-0)">
              <label>Projects ${settingHelpMark('work-sources', '', 'Projects', `Optional per-project source repository overrides, comma-separated as name=owner/repo (or just name to use the team default).`)}</label>
              <input type="text" placeholder="Billing=my-org/billing, Website" value="${esc((t.projects||[]).map(pr => pr.repo ? pr.name + '=' + pr.repo : pr.name).join(', '))}" data-change-action="wsLinearTeamEdit" data-arg0="${i}" data-arg1="projects" data-arg-types="j,s,w">
            </div>
          </div>`).join('')}
          <button type="button" class="hv-btn btn-secondary btn-secondary btn-primary btn-sm" data-action="wsLinearTeamAdd">+ Add team</button>
        </div>` : ''}

        ${type === 'jira' ? `
        <div style="border:1px solid var(--border);border-radius:8px;padding:14px;margin-bottom:14px">
          <div style="font-size:0.72rem;color:var(--muted);text-transform:uppercase;letter-spacing:.05em;margin-bottom:10px">Jira</div>
          <div class="config-field">
            <label>Deployment</label>
            <select data-change-action="gh77">
              <option value="" ${!jira.deployment || jira.deployment === 'cloud' ? 'selected' : ''}>Cloud (REST API v3)</option>
              <option value="datacenter" ${jira.deployment === 'datacenter' ? 'selected' : ''}>Data Center / Server (REST API v2)</option>
            </select>
          </div>
          <div class="config-field">
            <label>Base URL</label>
            <input type="text" placeholder="${jira.deployment === 'datacenter' ? 'https://jira.example.com/jira' : 'https://myorg.atlassian.net'}" value="${esc(jira.base_url||'')}" data-change-action="markWorkSourceDirty" data-arg0="jira.base_url" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Email ${settingHelpMark('work-sources', '', 'Email', `Required for Jira Cloud Basic auth. For Data Center basic auth, use Username + Password below instead.`)}</label>
            <input type="email" placeholder="bot@myorg.com" value="${esc(jira.email||'')}" data-change-action="markWorkSourceDirty" data-arg0="jira.email" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>API token / Data Center PAT ${settingHelpMark('work-sources', '', 'API token / Data Center PAT', `Cloud: Atlassian API token from id.atlassian.com. Data Center 8.14+: Personal Access Token sent as Authorization: Bearer. Use \${JIRA_API_TOKEN} to reference an environment variable.`)}</label>
            <input type="text" placeholder="${jira.api_token_set ? '(set — leave blank to keep)' : '\${JIRA_API_TOKEN}'}" value="" data-change-action="markWorkSourceDirty" data-arg0="jira.api_token" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Data Center username</label>
            <input type="text" placeholder="jira-bot" value="${esc(jira.username||'')}" data-change-action="markWorkSourceDirty" data-arg0="jira.username" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Data Center password ${settingHelpMark('work-sources', '', 'Data Center password', `Only used for Data Center Basic auth when no PAT is configured. Use \${JIRA_PASSWORD}; do not store a raw password.`)}</label>
            <input type="text" placeholder="${jira.password_set ? '(set — leave blank to keep)' : '\${JIRA_PASSWORD}'}" value="" data-change-action="markWorkSourceDirty" data-arg0="jira.password" data-arg-types="s,w">
          </div>
          ${jira.deployment === 'datacenter' ? `
          <div class="config-field">
            <strong>Data Center TLS</strong>
          </div>
          <div>
            <div class="config-field">
              <label>Custom CA bundle ${settingHelpMark('work-sources', '', 'Custom CA bundle', `PEM CA certificates appended to system trust roots. Use \${JIRA_DATACENTER_CA_BUNDLE} or paste PEM. Stored write-only; current value is never shown.`)}</label>
              <textarea rows="4" placeholder="${jira.ca_bundle_set ? '(set — leave blank to keep)' : '-----BEGIN CERTIFICATE-----\\n...'}" data-change-action="markWorkSourceDirty" data-arg0="jira.ca_bundle" data-arg-types="s,w"></textarea>
            </div>
            <div class="config-toggle">
              <div class="config-toggle-switch ${jira.insecure_skip_verify ? 'on' : ''}" data-action="gh78" data-arg-types="t"></div>
              <span class="config-toggle-label">Skip TLS verification — unsafe; testing only. Logs a warning whenever the Jira client is built.</span>
            </div>
            <div class="config-field">
              <label>mTLS client certificate ${settingHelpMark('work-sources', '', 'mTLS client certificate', `Optional PEM client certificate. Must be set with client key. Use an environment reference when possible.`)}</label>
              <textarea rows="3" placeholder="${jira.client_cert_set ? '(set — leave blank to keep)' : '\${JIRA_DATACENTER_CLIENT_CERT}'}" data-change-action="markWorkSourceDirty" data-arg0="jira.client_cert" data-arg-types="s,w"></textarea>
            </div>
            <div class="config-field">
              <label>mTLS client key ${settingHelpMark('work-sources', '', 'mTLS client key', `Optional PEM private key for the client certificate. Stored write-only; never shown after save.`)}</label>
              <textarea rows="3" placeholder="${jira.client_key_set ? '(set — leave blank to keep)' : '\${JIRA_DATACENTER_CLIENT_KEY}'}" data-change-action="markWorkSourceDirty" data-arg0="jira.client_key" data-arg-types="s,w"></textarea>
            </div>
          </div>` : ''}
          <div class="config-field">
            <label>Project keys ${settingHelpMark('work-sources', '', 'Project keys', `Comma-separated Jira project keys, e.g. "ENG, OPS". Used to build the default JQL query.`)}</label>
            <input type="text" placeholder="ENG, OPS" value="${esc((jira.project_keys||[]).join(', '))}" data-change-action="gh65">
          </div>
          <div class="config-field">
            <label>JQL override ${settingHelpMark('work-sources', '', 'JQL override', `Optional. Leave empty to use the default: "project in (&lt;keys&gt;) AND statusCategory != Done AND issuetype != Epic".`)}</label>
            <input type="text" placeholder="project = ENG AND status = Todo" value="${esc(jira.jql||'')}" data-change-action="markWorkSourceDirty" data-arg0="jira.jql" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Repo ${settingHelpMark('work-sources', '', 'Repo', `Source repository (owner/name) agents clone for these issues.`)}</label>
            <input type="text" placeholder="my-org/my-repo" value="${esc(jira.repo||'')}" data-change-action="markWorkSourceDirty" data-arg0="jira.repo" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Hold labels</label>
            <input type="text" placeholder="hold, blocked" value="${esc((jira.hold_labels||[]).join(', '))}" data-change-action="gh66">
          </div>
        </div>` : ''}

        ${type === '' ? `
        <div style="font-size:var(--fs-sm);color:var(--muted);background:color-mix(in srgb,var(--accent) 7%,transparent);border:1px solid color-mix(in srgb,var(--accent) 20%,transparent);border-radius:var(--r);padding:10px 12px">
          <b>Using default issues</b> — hive reads open issues from the projects listed in the Projects tab. No additional work-source configuration is needed.
        </div>` : ''}

        <div class="ws-wavefront-card">
          <div class="ws-wavefront-head">
            <div>
              <div class="ws-wavefront-kicker">Wavefront (Crustify) migration graph</div>
              <div class="ws-wavefront-help">
                Adds ready nodes from a Crustify/Wavefront migration graph as Hive run-stage work items; see
                <a class="text-accent" href="https://docs.hivecommons.dev/docs/hive/integrations" target="_blank" rel="noopener">Hive integrations</a>
                and <a class="text-accent" href="https://github.com/crustify-rs/crustify" target="_blank" rel="noopener">crustify-rs/crustify</a>.
                URL fetches plain JSON only today; there is no auth field.
              </div>
            </div>
            <div class="config-toggle ws-wavefront-toggle">
              <div class="config-toggle-switch ${wf.enabled ? 'on' : ''}" data-action="wsWavefrontSetEnabled" data-arg-types="t"></div>
              <span class="config-toggle-label">Enabled</span>
            </div>
          </div>
          ${wfErrors.length ? `<div class="ws-wavefront-errors">${wfErrors.map(esc).join('<br>')}</div>` : ''}
          <div class="config-field">
            <label>Source</label>
            <div class="ws-wavefront-source-options">
              <label class="ws-wavefront-radio"><input type="radio" name="wf-source" value="path" ${wfSource === 'path' ? 'checked' : ''} data-change-action="gh79"> File path</label>
              <label class="ws-wavefront-radio"><input type="radio" name="wf-source" value="url" ${wfSource === 'url' ? 'checked' : ''} data-change-action="gh79"> URL</label>
            </div>
          </div>
          <div class="config-field">
            <label>${wfSource === 'url' ? 'Graph URL' : 'Graph file path'}</label>
            <input type="text" placeholder="${wfSource === 'url' ? 'https://wavefront.example/graph.json' : '/data/wavefront/graph.json'}" value="${esc(wfSource === 'url' ? (wf.url||'') : (wf.path||''))}" data-change-action="gh80" data-source="${wfSource}">
          </div>
          <div class="config-field">
            <label>Repo ${settingHelpMark('work-sources', '', 'Repo', `owner/name repository the migration nodes are scoped to.`)}</label>
            <input type="text" placeholder="acme/crust" value="${esc(wf.repo||'')}" data-change-action="markWorkSourceDirty" data-arg0="wavefront.repo" data-arg-types="s,w">
          </div>
          <div class="config-field">
            <label>Receipts directory ${settingHelpMark('work-sources', '', 'Receipts directory', `Optional directory where Hive writes node-completion receipts. Empty keeps receipts in memory until restart.`)}</label>
            <input type="text" placeholder="/data/wavefront/receipts" value="${esc(wf.receipts_dir||'')}" data-change-action="markWorkSourceDirty" data-arg0="wavefront.receipts_dir" data-arg-types="s,w">
          </div>
        </div>`;
    }

