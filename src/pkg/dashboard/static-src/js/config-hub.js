    function renderGovHub(hub) {
      const dashUrl = hub.dashboard_url || window.location.origin || '';
      const snapUrl = hub.snapshot_url || (hub.auto_snapshot && dashUrl ? dashUrl.replace(/\/$/, '') + '/snapshot' : '');
      const disabledRepos = hub.disabled_repos || [];
      const repos = (_configState.data.repos || []).map(function(r) {
        const name = contribRepoShortName(r);
        const enabled = contribRepoMatchingDisabledEntries(r, disabledRepos).length === 0;
        return {name: name, full: r, enabled: enabled};
      });
      const rawDisabledRepos = hub.disabled_repos_raw || disabledRepos;
      const tiers = ['newcomer', 'contributor', 'trusted', 'merger', 'advisor'];
      const denyLabels = (hub.contribute_deny_labels || []);
      const denyTitles = (hub.contribute_deny_titles || []);
      const denyAuthors = (hub.contribute_deny_authors || []);
      const allowModels = (hub.contribute_allow_models || []);
      const frameAncestors = (hub.snapshot_frame_ancestors || []);
      const rejectUnknownModels = hub.contribute_reject_unknown_models || false;
      const minEffort = hub.contribute_min_reasoning_effort || '';
      const rejectUnknownEffort = hub.contribute_reject_unknown_effort || false;
      const effortLadder = hub.contribute_reasoning_effort_ladder || ['minimal', 'low', 'medium', 'high', 'xhigh', 'max'];
      const effortFloorOptions = contributeEffortFloorOptions(effortLadder, minEffort);
      const delegatableRoles = (hub.contribute_delegatable_roles || ['scanner','quality','outreach']).map(function(r) { return String(r || '').toLowerCase(); });
      const helpLinksText = (hub.contribute_help_links || []).map(function(l) { return (l.label || '') + ' | ' + (l.url || ''); }).join('\n');
      const privilegedDelegatableRoles = ['ci-maintainer', 'sec-check', 'architect'];
      // Filter modes: 'allow' = only matching pass; 'deny' (default) = matching skipped.
      const titlesMode = (hub.contribute_titles_mode === 'allow') ? 'allow' : 'deny';
      const authorsMode = (hub.contribute_authors_mode === 'allow') ? 'allow' : 'deny';
      const labelsMode = (hub.contribute_labels_mode === 'allow') ? 'allow' : 'deny';
      // Read-only, runtime-derived — never sent back on save (markDirty is
      // never wired to this field). See podNamespace() in
      // pkg/dashboard/pod_namespace.go for how the hub API resolved it
      // (POD_NAMESPACE downward-API env, else NAMESPACE, else the
      // service-account namespace file). Omitted entirely when unknown
      // (running outside a cluster, or an old spoke build with no
      // POD_NAMESPACE wired) rather than showing a blank/"undefined" line —
      // this is the spoke-side complement to the hive.kubestellar.io/*
      // namespace labels the hub stamps onto this same namespace at
      // provision/claim/assign time.
      const namespaceRow = hub.namespace ? `
        <div class="config-field">
          <label>Namespace ${settingHelpMark('hub-deployment', 'registering-spokes-to-your-hub', 'Namespace', `The Kubernetes namespace this hive's pod is running in. Unlike the hive's name/org, this never changes after the namespace is first created — see the hive.kubestellar.io/* labels on it for an operator-facing way to map it back to this hive.`)}</label>
          <input type="text" value="${esc(hub.namespace)}" readonly disabled style="opacity:0.7">
        </div>` : '';
      return `
        ${namespaceRow}
        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.enabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="enabled"></div>
          <span class="config-toggle-label">Hub Enabled ${settingHelpMark('hub-deployment', 'registering-spokes-to-your-hub', 'Hub Enabled', `Register this hive with the central hub at hive.hivecommons.dev so it appears in the public registry.`)}</span>
        </div>
        <div class="config-field">
          <label>Hub URL ${settingHelpMark('hub-deployment', 'registering-spokes-to-your-hub', 'Hub URL', `URL of the Hive Hub. Heartbeats and task status are sent here.`)}</label>
          <input type="text" value="${esc(hub.url || '')}" data-change-action="markDirty" data-arg0="hub" data-arg1="url" data-arg-types="s,s,v">
        </div>
        ${renderHubDashboardURLField(hub, dashUrl)}
        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.is_public ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="is_public"></div>
          <span class="config-toggle-label">Public ${settingHelpMark('hub-deployment', 'registering-spokes-to-your-hub', 'Public', `Show this hive on the public hub registry.`)}</span>
        </div>

        <hr style="border-color:var(--border);margin:var(--sp-6) var(--sp-0)">
        <h4 style="font-size:var(--fs-base);color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">Telemetry</h4>
        <div class="config-field" id="hub-telemetry-omit">
          <label>Withheld from hub heartbeat ${settingHelpMark('telemetry', 'withholding-heartbeat-identifiers', 'Withheld from hub heartbeat', `Set hub.heartbeat_omit in hive.yaml to withhold identifier classes from the heartbeat and task-status push. Upgrade and config delivery keep working; the hub simply shows less.`)}</label>
          ${renderHubTelemetryOmit(hub.heartbeat_omit)}
        </div>
        <hr style="border-color:var(--border);margin:var(--sp-6) var(--sp-0)">
        <h4 style="font-size:0.8rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">Auto-update</h4>
        <div class="config-toggle" id="hub-autoupdate-toggle-row">
          <div class="config-toggle-switch ${hub.auto_upgrade ? 'on' : ''}" id="hub-autoupdate-toggle" data-action="toggleConfigSwitch" data-section="hub" data-key="auto_upgrade"></div>
          <span class="config-toggle-label">Automatic Updates <span id="hub-autoupdate-managed-by"></span>${settingHelpMark('settings', 'hub', 'Automatic Updates', `When on, this hive is rolled forward to new versions automatically on the configured schedule. When off, upgrades are applied manually from the version chip at the bottom of the sidebar. On a hub-managed hive the hub owns this setting and the switch is read-only.`)}</span>
        </div>
        <div class="config-field">
          <label>Update schedule ${settingHelpMark('settings', 'hub', 'Update schedule', `The cadence applied to this hive's auto-updates, as reported by the hub on every heartbeat. "Unknown" means the hub has not yet told this spoke its schedule.`)}</label>
          <div style="font-size:0.78rem;color:var(--text)" id="hub-autoupdate-schedule">${esc(autoUpdatePeriodLabel(hub.auto_upgrade_mode))}</div>
        </div>
        <div id="hub-autoupdate-status" style="margin-top:var(--sp-3);font-size:0.78rem;color:var(--muted)">Loading auto-update status…</div>

        <hr style="border-color:var(--border);margin:var(--sp-6) var(--sp-0)">
        <h4 style="font-size:0.8rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">Snapshot</h4>
        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.auto_snapshot ? 'on' : ''}" data-action="toggleAutoSnapshot" data-arg-types="t" data-section="hub" data-key="auto_snapshot"></div>
          <span class="config-toggle-label">Auto-generate &amp; Publish Snapshot ${settingHelpMark('snapshots', '', 'Auto-generate & Publish Snapshot', `Generate a read-only snapshot at /snapshot every 15 min. When off, you can set a custom preview URL below.`)}</span>
        </div>
        <div class="config-field">
          <label>Snapshot / Preview URL ${settingHelpMark('snapshots', '', 'Snapshot / Preview URL', `${hub.auto_snapshot ? 'Auto-generated from Dashboard URL. Read-only while auto-publish is on.' : 'Enter a custom preview URL (e.g. your own static dashboard). Sent to hub as the Preview link.'}`)}</label>
          <input type="text" id="hub-snapshot-url" value="${esc(snapUrl)}" data-change-action="markDirty" data-arg0="hub" data-arg1="snapshot_url" data-arg-types="s,s,v" ${hub.auto_snapshot ? 'disabled style="opacity:0.5"' : ''} placeholder="${hub.auto_snapshot ? '' : 'https://your-site.com/dashboard-preview'}">
        </div>
        <div class="config-field">
          <label>Snapshot frame allowlist ${yamlHint('dashboard.snapshot_frame_ancestors[]')} ${settingHelpMark('snapshots', '', 'Snapshot frame allowlist', `Exact HTTPS origins allowed to embed the public read-only /snapshot page. Empty keeps frame-ancestors 'none' and X-Frame-Options: DENY. X-Frame-Options is omitted for /snapshot only when this allowlist is non-empty because XFO cannot express allowlists.`)}</label>
          <div style="display:flex;flex-wrap:wrap;gap:var(--sp-2);margin-bottom:var(--sp-3)">${frameAncestors.map(function(o) { return '<span style="display:inline-flex;align-items:center;gap:var(--sp-2);padding:var(--sp-1) var(--sp-4);border-radius:var(--r-pill);font-size:0.72rem;background:rgba(96,165,250,0.15);color:var(--blue);border:1px solid rgba(96,165,250,0.3)">' + esc(o) + ' <span data-action="removeHubFilter" data-arg0="snapshot_frame_ancestors" data-arg1="' + esc(o) + '" style="cursor:pointer;opacity:0.7">✕</span></span>';}).join('') || '<span style="font-size:0.72rem;color:var(--muted)">No origins allowed — /snapshot cannot be framed.</span>'}</div>
          <textarea rows="3" style="width:100%;font-family:var(--font-mono);font-size:0.72rem;resize:vertical" placeholder="https://docs.projectbluefin.io" data-change-action="setSnapshotFrameAncestorsFromText" data-arg-types="v">${esc(frameAncestors.join('\n'))}</textarea>
          <span style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:block">One exact origin per line: <code>https://host</code> or <code>https://host:port</code>. Paths, http://, credentials, and wildcards are rejected.</span>
        </div>

        <hr style="border-color:var(--border);margin:var(--sp-6) var(--sp-0)">
        <h4 style="font-size:0.8rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">Contribute Work Queue <span id="hub-queue-count" style="font-size:0.72rem;padding:var(--sp-1) var(--sp-4);border-radius:var(--r-pill);background:rgba(59,130,246,0.15);color:#60a5fa;border:1px solid rgba(59,130,246,0.3)">loading...</span></h4>

        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.contribute_suspended ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="contribute_suspended" style="${hub.contribute_suspended ? 'background:var(--red)' : ''}"></div>
          <span class="config-toggle-label" style="${hub.contribute_suspended ? 'color:var(--red);font-weight:600' : ''}">Suspend Contributions ${settingHelpMark('contributor-relay', '', 'Suspend Contributions', `Temporarily stop assigning tasks to contributors. Connected contributors stay online but idle.`)}</span>
        </div>

        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.contribute_wall_enabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="contribute_wall_enabled"></div>
          <span class="config-toggle-label">Contributor Wall ${settingHelpMark('contributor-relay', '', 'Contributor Wall', `Opt in to the public /contribute Operations wall. Off by default; posts are stored on the hub data volume and pruned by retention.`)}</span>
        </div>

        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.contribute_skip_assigned_to_others ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="contribute_skip_assigned_to_others"></div>
          <span class="config-toggle-label">Skip Issues Assigned to Others ${settingHelpMark('contributor-relay', '', 'Skip Issues Assigned to Others', `When on, the contribute queue never serves an issue that is already assigned to a different GitHub user, so contributors don't pick up work a maintainer has claimed. Mirrored on the /contribute Management &amp; Operations tab.`)}</span>
        </div>

        <div class="config-toggle">
          <div class="config-toggle-switch ${hub.contribute_cooldown_enabled ? 'on' : ''}" data-action="toggleCooldownEnabled" data-arg-types="t" data-section="hub" data-key="contribute_cooldown_enabled"></div>
          <span class="config-toggle-label">Task Cooldown ${settingHelpMark('contributor-relay', '', 'Task Cooldown', `After a task completes WITH a merged/verified PR, keep that issue out of the queue for the period below (default 168h = one week) so it isn't immediately re-offered. Turn off to disable cooldown gating entirely. Failure quarantine is separate and always on. Mirrored on the /contribute Management tab.`)}</span>
        </div>
        <div class="config-field" style="margin-left:44px" ${hub.contribute_cooldown_enabled ? '' : 'title="Enable Task Cooldown to edit the period"'}>
          <label>Cooldown Period (hours) ${settingHelpMark('contributor-relay', '', 'Cooldown Period (hours)', `The with-PR completion cooldown, in hours. 168 = one week (default). Range 1–8760.`)}</label>
          <input type="number" id="hub-cooldown-hours" min="1" max="8760" value="${hub.contribute_cooldown_hours || 168}" data-change-action="gh31" ${hub.contribute_cooldown_enabled ? '' : 'disabled style="opacity:0.5"'}>
        </div>

        <div class="config-field">
          <label for="hub-max-parallel-fixes">Fix budget: parallel fix sessions ${yamlHint('contribute.max_parallel_fixes')} ${settingHelpMark('contributor-relay', 'fix-pr-tasks', 'Fix budget', `Ceiling on how many red-PR fix-pr sub-sessions one contributor's relay may run beside its primary session (the relay advertises its own count; the hub clamps to this). Default 2; 1 forces preempt mode. Queued fixes beyond capacity are served before new work ("contributors should clean up their mess before taking on new stuff").`)}</label>
          <input type="number" id="hub-max-parallel-fixes" min="1" max="16" value="${hub.contribute_max_parallel_fixes || 2}" data-change-action="markDirty" data-arg0="hub" data-arg1="contribute_max_parallel_fixes" data-arg-types="s,s,N">
        </div>
        <div class="config-field">
          <label for="hub-min-focus-window">Fix budget: minimum focus window ${yamlHint('contribute.min_focus_window')}</label>
          <input type="text" id="hub-min-focus-window" value="${esc(hub.contribute_min_focus_window || '')}" placeholder="30m" data-change-action="markDirty" data-arg0="hub" data-arg1="contribute_min_focus_window" data-arg-types="s,s,w">
          <div class="config-help">In preempt mode, after a fix preempted the primary task, the hive will not preempt again until the resumed task has run uninterrupted for this long. Go duration; default 30m.</div>
        </div>

        <div class="config-field">
          <label>Contributor Agent Roles ${settingHelpMark('contributor-trust-and-roles', 'delegated-clanker-agent-roles', 'Contributor Agent Roles', `Default roles scanner, quality, and outreach are always delegatable. Check privileged roles to let owners assign them from a Contributor agent (ClankeR) card; supervisor is never delegatable.`)}</label>
          <div style="display:flex;flex-direction:column;gap:var(--sp-3)">
            ${['scanner','quality','outreach'].map(function(r) {
              return '<label style="display:flex;align-items:center;gap:var(--sp-4);font-size:0.78rem;color:var(--muted)" title="Default contributor role; always delegatable"><input type="checkbox" checked disabled> ' + r + ' <span style="font-size:0.65rem;color:var(--green)">always on</span></label>';
            }).join('')}
            ${privilegedDelegatableRoles.map(function(r) {
              const checked = delegatableRoles.includes(r);
              return '<label style="display:flex;align-items:center;gap:var(--sp-4);font-size:0.78rem"><input type="checkbox" ' + (checked ? 'checked ' : '') + 'data-change-action="toggleDelegatableAgentRole" data-arg0="' + r + '" data-arg-types="s,c"> ' + r + ' <span style="font-size:0.65rem;color:var(--muted)">Delegatable to contributors</span></label>';
            }).join('')}
          </div>
        </div>

        <div class="config-field">
          <label>Help &amp; community links ${settingHelpMark('contributor-relay', '', 'Help & community links', `Shown on /contribute Onboarding, Operations, and in relay terminals at connect. One "Label | URL" per line, max 5. Discord channel URLs only work for existing server members; prefer https://hivecommons.dev/discord for new contributors.`)}</label>
          <textarea rows="4" style="width:100%;font-family:var(--font-mono);font-size:0.72rem;resize:vertical" placeholder="Chat with other contributors | https://hivecommons.dev/discord" data-change-action="setContributeHelpLinksFromText" data-arg-types="v">${esc(helpLinksText)}</textarea>
          <span style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-2);display:block">Stored as <code>contribute.help_links[].label</code> and <code>contribute.help_links[].url</code>. Empty restores Hive's default contributor docs, issue tracker, and Twitch live stream links.</span>
        </div>

        ${renderFilterWithMode({
          label: 'Titles', noun: 'title', unit: 'issue',
          modeKey: 'contribute_titles_mode', listKey: 'contribute_deny_titles',
          mode: titlesMode, list: denyTitles, inputId: 'hub-deny-title-input',
          placeholder: 'e.g. *dashboard*, epic:*, /renovate/i'
        })}

        ${renderFilterWithMode({
          label: 'Authors', noun: 'author', unit: 'issue',
          modeKey: 'contribute_authors_mode', listKey: 'contribute_deny_authors',
          mode: authorsMode, list: denyAuthors, inputId: 'hub-deny-author-input',
          placeholder: 'e.g. renovate[bot], dependabot*'
        })}

        <hr style="border-color:var(--border);margin:var(--sp-6) var(--sp-0)">
        <h4 style="font-size:0.8rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">Model Filter ${allowModels.length === 0 ? '<span style="font-weight:400;color:var(--green);margin-left:var(--sp-4)">All models accepted</span>' : '<span style="font-weight:400;color:var(--blue);margin-left:var(--sp-4)">' + allowModels.length + ' pattern' + (allowModels.length > 1 ? 's' : '') + '</span>'}</h4>

        <div class="config-toggle">
          <div class="config-toggle-switch ${rejectUnknownModels ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="contribute_reject_unknown_models" style="${rejectUnknownModels ? 'background:var(--red)' : ''}"></div>
          <span class="config-toggle-label" style="${rejectUnknownModels ? 'color:var(--red);font-weight:600' : ''}">Reject Unknown Models ${settingHelpMark('contributor-relay', '', 'Reject Unknown Models', `When enabled and the allowlist is non-empty, contributors using models not in the list are rejected at connect time.`)}</span>
        </div>

        <div class="config-field">
          <label>Allowed Models ${settingHelpMark('contributor-relay', '', 'Allowed Models', `Models matching these patterns are accepted. Supports wildcards (*) and regex (/pattern/). Empty = allow all.`)}</label>
          <div id="hub-allow-models" style="display:flex;flex-wrap:wrap;gap:var(--sp-2);margin-bottom:var(--sp-2)">${allowModels.map(function(m) { return '<span style="display:inline-flex;align-items:center;gap:var(--sp-2);padding:var(--sp-1) var(--sp-4);border-radius:var(--r-pill);font-size:0.72rem;background:rgba(34,197,94,0.15);color:var(--green);border:1px solid rgba(34,197,94,0.3)">' + esc(m) + ' <span data-action="removeHubFilter" data-arg0="contribute_allow_models" data-arg1="' + esc(m) + '" style="cursor:pointer;opacity:0.7">✕</span></span>';}).join('')}</div>
          <div style="display:flex;flex-wrap:wrap;gap:var(--sp-2);margin-bottom:var(--sp-3)">
            <button class="hv-btn btn-danger btn-sm" data-action="addModelPreset" data-arg0="claude-opus*">+ opus</button>
            <button class="hv-btn btn-danger btn-sm" data-action="addModelPreset" data-arg0="claude-sonnet*">+ sonnet</button>
            <button class="hv-btn btn-danger btn-sm" data-action="addModelPreset" data-arg0="claude-haiku*">+ haiku</button>
            <button class="hv-btn btn-danger btn-sm" data-action="addModelPreset" data-arg0="gpt-4o*">+ gpt-4o</button>
            <button class="hv-btn btn-danger btn-sm" data-action="addModelPreset" data-arg0="gemini*">+ gemini</button>
            <button class="hv-btn btn-danger btn-sm" data-action="addModelPreset" data-arg0="deepseek*">+ deepseek</button>
          </div>
          <div style="display:flex;gap:var(--sp-2)"><input type="text" id="hub-allow-model-input" placeholder="e.g. claude-opus*, /gemini-\\d/" style="flex:1" data-keydown-action="addHubFilter" data-arg0="contribute_allow_models" data-arg1="hub-allow-model-input" data-prevent="1" data-keys="Enter"><button class="hv-btn btn-primary btn-sm" data-action="addHubFilter" data-arg0="contribute_allow_models" data-arg1="hub-allow-model-input">Add</button></div>
        </div>

        <div class="config-field">
          <label for="hub-min-reasoning-effort">Effort floor ${settingHelpMark('contributor-relay', '', 'Effort floor', `Minimum reasoning effort a contributor relay must run at, compared per backend on minimal &lt; low &lt; medium &lt; high &lt; xhigh &lt; max. A floor above a backend's highest level is met by that backend's highest level. Relays below the floor are rejected at connect time. None = no floor.`)}</label>
          <select id="hub-min-reasoning-effort" data-change-action="markDirty" data-arg0="hub" data-arg1="contribute_min_reasoning_effort" data-arg-types="s,s,v">
            <option value=""${minEffort ? '' : ' selected'}>none</option>
            ${effortFloorOptions.map(function(e) { return '<option value="' + esc(e) + '"' + (e === minEffort ? ' selected' : '') + '>' + esc(e) + '</option>'; }).join('')}
          </select>
        </div>

        <div class="config-toggle">
          <div class="config-toggle-switch ${rejectUnknownEffort ? 'on danger' : ''}" data-action="toggleConfigSwitch" data-section="hub" data-key="contribute_reject_unknown_effort"></div>
          <span class="config-toggle-label ${rejectUnknownEffort ? 'danger' : ''}">Reject Unknown Effort ${settingHelpMark('contributor-relay', '', 'Reject Unknown Effort', `When enabled and an effort floor is set, contributors whose reasoning effort is empty or not recognised for their backend are rejected at connect time. When off they are admitted.`)}</span>
        </div>

        <hr class="config-section-rule">

        ${renderFilterWithMode({
          label: 'Labels', noun: 'label', unit: 'issue', perLabel: true,
          modeKey: 'contribute_labels_mode', listKey: 'contribute_deny_labels',
          mode: labelsMode, list: denyLabels, inputId: 'hub-deny-label-input',
          placeholder: 'e.g. good-first-issue, help-wanted'
        })}

        <div class="config-field">
          <label>Repos for Contribute ${settingHelpMark('contributor-relay', '', 'Repos for Contribute', `Toggle which repos serve work to contributors. New repos default to on.`)}</label>
          <div style="display:flex;flex-direction:column;gap:var(--sp-2)">${repos.map(function(r) {
            return '<div class="config-toggle" style="padding:var(--sp-1) var(--sp-0)"><div class="config-toggle-switch ' + (r.enabled ? 'on' : '') + '" data-action="toggleContribRepo" data-arg1="' + esc(r.full) + '" data-arg-types="t,s"></div><span style="font-size:0.78rem">' + esc(r.full) + '</span></div>';
          }).join('')}</div>
          <div style="font-size:0.65rem;color:var(--muted);margin-top:var(--sp-3)">Raw disabled_repos: <code>${esc(rawDisabledRepos.length ? rawDisabledRepos.join(', ') : 'none')}</code></div>
        </div>

        <div class="config-field">
          <label>Tier Access &amp; Rate Limits ${settingHelpMark('contributor-trust-and-roles', 'contributor-trust-tiers', 'Tier Access & Rate Limits', `Toggle tiers on/off and set tasks/hour, tasks/day, max concurrent per tier. 0 = unlimited.`)}</label>
          <div style="display:flex;flex-direction:column;gap:var(--sp-3)">${tiers.map(function(t) {
            const enabled = !(hub.disabled_tiers || []).includes(t);
            const defaults = {newcomer:{mph:3,mpd:10,mc:1},contributor:{mph:10,mpd:50,mc:2},trusted:{mph:30,mpd:200,mc:5},merger:{mph:30,mpd:200,mc:5},advisor:{mph:0,mpd:0,mc:0}};
            const def = defaults[t] || {mph:3,mpd:10,mc:1};
            const limits = (hub.tier_limits || {})[t] || {};
            const mph = limits.max_per_hour != null ? limits.max_per_hour : def.mph;
            const mpd = limits.max_per_day != null ? limits.max_per_day : def.mpd;
            const mc = limits.max_concurrent != null ? limits.max_concurrent : def.mc;
            return '<div style="display:flex;align-items:center;gap:var(--sp-4);padding:var(--sp-1) var(--sp-0)"><div class="config-toggle-switch ' + (enabled ? 'on' : '') + '" data-action="toggleContribTier" data-arg1="' + t + '" data-arg-types="t,s" style="flex-shrink:0"></div><span style="font-size:0.78rem;width:80px">' + t + '</span>' +
              '<input type="number" min="0" placeholder="/hr" value="' + mph + '" style="width:50px;padding:var(--sp-1) var(--sp-2);font-size:0.72rem;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);color:var(--text)" data-change-action="setTierLimit" data-arg0="' + t + '" data-arg1="max_per_hour" data-arg-types="s,s,N">' +
              '<input type="number" min="0" placeholder="/day" value="' + mpd + '" style="width:50px;padding:var(--sp-1) var(--sp-2);font-size:0.72rem;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);color:var(--text)" data-change-action="setTierLimit" data-arg0="' + t + '" data-arg1="max_per_day" data-arg-types="s,s,N">' +
              '<input type="number" min="0" placeholder="max" value="' + mc + '" style="width:50px;padding:var(--sp-1) var(--sp-2);font-size:0.72rem;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);color:var(--text)" data-change-action="setTierLimit" data-arg0="' + t + '" data-arg1="max_concurrent" data-arg-types="s,s,N">' +
              '<span style="font-size:0.65rem;color:var(--muted)">/hr · /day · max</span></div>';
          }).join('')}</div>
        </div>
        ${renderHubSettingsParity(hub)}`;
    }

    // ---- Settings → Hub → hub settings ----
    // The scalar hub.* keys, saved with the rest of the Hub tab through
    // PUT /api/config/governor/hub (owner-only; 0 or empty restores the
    // default). cluster_id and hive_type are identity keys written by hub
    // provisioning, and the two deprecated already-done aliases are no longer
    // edited, so those are display-only. The hub-only secrets are write-only:
    // only whether one is set is ever shown.
    function renderHubSettingsParity(hub) {
      const st = hub.settings || {};
      const def = st.defaults || {};
      const ann = st.contribute_announcement_config || {};
      const owner = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const dis = owner ? '' : 'disabled';
      const restart = ' Requires restart.';
      const live = ' Applies without a restart.';
      const mark = (label, tip) => settingHelpMark('settings', 'hub', label, tip);
      const managedNote = st.hub_managed
        ? 'managed by the hub — change it from the hub dashboard'
        : 'set in hive.yaml; requires restart';
      const roRow = (label, path, valueHtml, note) => `<div class="config-field"><label>${esc(label)} ${yamlHint(path)}</label><div class="fs-sm break-all">${valueHtml} <span class="text-muted">— ${esc(note)}</span></div></div>`;
      const shown = v => (v !== undefined && v !== null && v !== '') ? '<code>' + esc(String(v)) + '</code>' : '<span class="text-muted">unset</span>';
      const num = (key, label, path, value, placeholder, min, max, help, tip) => `
          <div class="config-field">
            <label for="hub-set-${key}">${esc(label)} ${yamlHint(path)} ${mark(label, tip)}</label>
            <input id="hub-set-${key}" type="number" ${min !== '' ? `min="${min}"` : ''} max="${max}" step="1" value="${value ? esc(String(value)) : ''}" placeholder="${esc(String(placeholder))}" data-change-action="markDirty" data-arg0="hub" data-arg1="${key}" data-arg-types="s,s,N" ${dis}>
            <div class="config-help">${help}</div>
          </div>`;
      const text = (key, label, path, value, placeholder, help, tip) => `
          <div class="config-field">
            <label for="hub-set-${key}">${esc(label)} ${yamlHint(path)} ${mark(label, tip)}</label>
            <input id="hub-set-${key}" type="text" value="${esc(value || '')}" placeholder="${esc(placeholder)}" data-change-action="markDirty" data-arg0="hub" data-arg1="${key}" data-arg-types="s,s,w" ${dis}>
            <div class="config-help">${help}</div>
          </div>`;
      const toggle = (key, label, path, on, help, tip) => `
          <div class="config-toggle mt-3">
            <div id="hub-set-${key}" class="config-toggle-switch ${on ? 'on' : ''} ${owner ? '' : 'disabled'}" data-action="toggleConfigSwitch" data-section="hub" data-key="${key}" role="switch" aria-checked="${on ? 'true' : 'false'}"></div>
            <span class="config-toggle-label">${esc(label)} ${yamlHint(path)} ${mark(label, tip)} <span class="text-muted">— ${help}</span></span>
          </div>`;
      const secret = (key, label, path, isSet, help, tip) => `
          <div class="config-field">
            <label for="hub-set-${key}">${esc(label)} ${yamlHint(path)} ${mark(label, tip)} <span class="text-muted">${isSet ? 'set (hidden)' : 'not set'}</span></label>
            <input id="hub-set-${key}" type="password" autocomplete="new-password" placeholder="${esc(isSet ? 'Configured — enter a new value to change, or save an empty value to clear.' : 'not set')}" data-change-action="markDirty" data-arg0="hub" data-arg1="${key}" data-arg-types="s,s,v" ${dis}>
            <div class="config-help">${help}</div>
          </div>`;
      return `
        <hr class="config-section-rule">
        <h4 class="settings-subsection-title">Hub settings</h4>
        <div class="card-inset mt-3">
          <div class="settings-subsection-title">Hub identity</div>
          ${roRow('Cluster ID', 'hub.cluster_id', shown(st.cluster_id), managedNote)}
          ${roRow('Hive type', 'hub.hive_type', shown(st.hive_type), managedNote)}
          <div class="config-help">Written by hub provisioning; a hive type of <code>hosted</code> turns the NPS prompt on by default.</div>
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Contributor queue</div>
          ${toggle('contribute_require_explicit_accept', 'Require explicit task acceptance', 'hub.contribute_require_explicit_accept', st.contribute_require_explicit_accept, 'off by default (trusted auto-accept).' + live, 'Off (default): a task that passed admission is auto-accepted when assigned and the scoped GitHub credential is delivered right after. On: the hub withholds the credential until the contributor sends task_accepted; a task never accepted never receives a credential.')}
          <div class="config-field-row">
            ${text('contribute_already_done_label', 'Already-done label', 'hub.contribute_already_done_label', st.contribute_already_done_label, 'Default (' + (def.contribute_already_done_label || 'hive/already-done') + ')', 'Applied to issues a contributor found already resolved. Empty restores the default.' + live, 'Applied to issues a contributor found already resolved. It is also in the default contribute skip label set, so labelled issues stay out of the offer queue until a maintainer removes it. Empty restores the default (hive/already-done).')}
            ${num('contribute_already_done_hold_days', 'Already-done hold (days)', 'hub.contribute_already_done_hold_days', st.contribute_already_done_hold_days, def.contribute_already_done_hold_days || 30, 0, 365, 'Offer suppression after an "already done" verdict, 1–365. 0 restores the default.' + live, 'The longer offer suppression for an already-done verdict while the label/comment path is pending or if cited evidence cannot be verified. Separate from the task cooldown. 0 restores the default (30).')}
            ${num('contribute_wall_retention_days', 'Contributor wall retention (days)', 'hub.contribute_wall_retention_days', hub.contribute_wall_retention_days, def.contribute_wall_retention_days || 90, 0, 3650, 'Older wall posts are pruned, 0–3650. 0 restores the default.' + live, 'Bounds wall posts stored on the hub data volume next to contributor profiles. Older posts are pruned on load/write. 0 means the dashboard default (90 days).')}
          </div>
          ${roRow('Already-done unverified hold (days)', 'hub.contribute_already_done_unverified_hold_days', shown(st.contribute_already_done_unverified_hold_days || ''), 'deprecated alias of contribute_already_done_hold_days; set in hive.yaml')}
          ${roRow('Close already-done issues', 'hub.contribute_close_already_done', shown(st.contribute_close_already_done ? 'true' : 'false'), 'deprecated compatibility key, no longer read; set in hive.yaml')}
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Contributor announcement</div>
          <div class="config-field">
            <label for="hub-set-ann-text">Text ${yamlHint('hub.contribute_announcement.text')} ${mark('Announcement text', 'Operator notice for contributors, shown on /contribute Operations/Profile, above the Onboarding command block, and in connected relay terminals. Server-sanitised plain text capped at 500 characters; empty clears the announcement.')}</label>
            <textarea id="hub-set-ann-text" rows="2" maxlength="500" data-change-action="hubAnnouncementChanged" ${dis}>${esc(ann.text || '')}</textarea>
            <div class="config-help">Empty clears the announcement.${live}</div>
          </div>
          <div class="config-field-row">
            <div class="config-field">
              <label for="hub-set-ann-level">Level ${yamlHint('hub.contribute_announcement.level')} ${mark('Announcement level', 'info or warning.')}</label>
              <select id="hub-set-ann-level" data-change-action="hubAnnouncementChanged" ${dis}>
                ${['info', 'warning'].map(v => `<option value="${v}" ${(ann.level || 'info') === v ? 'selected' : ''}>${v}</option>`).join('')}
              </select>
            </div>
            <div class="config-field">
              <label for="hub-set-ann-expires">Expires at ${yamlHint('hub.contribute_announcement.expires_at')} ${mark('Announcement expiry', 'Optional RFC3339 time after which the announcement is no longer shown, e.g. 2026-12-31T18:00:00Z. Empty never expires.')}</label>
              <input id="hub-set-ann-expires" type="text" value="${esc(ann.expires_at || '')}" placeholder="2026-12-31T18:00:00Z" data-change-action="hubAnnouncementChanged" ${dis}>
            </div>
          </div>
          ${roRow('Announcement ID', 'hub.contribute_announcement.id', shown(ann.id), 'minted by the dashboard whenever the text changes, so dismissed banners reappear')}
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Snapshots, upgrades and telemetry</div>
          <div class="config-field-row">
            ${num('snapshot_interval_min', 'Snapshot interval (minutes)', 'hub.snapshot_interval_min', hub.snapshot_interval_min, def.snapshot_interval_min || 15, 0, 1440, 'How stale /snapshot may get before it is rebuilt, 5–1440. 0 restores the default.' + live, 'The public /snapshot page is rebuilt when it is older than this. Values below 5 use the default (15 minutes).')}
            ${num('upgrade_drain_timeout_s', 'Upgrade drain timeout (seconds)', 'hub.upgrade_drain_timeout_s', st.upgrade_drain_timeout_s, def.upgrade_drain_timeout_s || 2700, '', 86400, 'Self-upgrade waits up to this long for in-flight agent kicks. 0 restores the default; negative disables draining.' + restart, 'A hub-instructed self-upgrade waits up to this long for in-flight agent kicks to finish (new kicks are withheld meanwhile) before exiting anyway. 0/unset = 45 min; a negative value disables draining.')}
          </div>
          ${toggle('task_status_push', 'Push task status to the hub', 'hub.task_status_push', st.task_status_push, 'on by default; HIVE_HUB_TASK_STATUS_PUSH overrides.' + restart, 'Controls the separate task-status push loop, not the core heartbeat or hub-managed upgrade/config delivery. Off stops only the task-status loop.')}
          ${toggle('nps_enabled', 'NPS feedback prompt', 'hub.nps_enabled', st.nps_enabled, 'on by default for hosted spokes, off otherwise; HIVE_NPS_ENABLED overrides.' + live, 'Enables the dashboard NPS feedback prompt, whose responses are forwarded to the hub over the spoke\'s authenticated hub link, without the user\'s identity, and are visible only to hub admins. A hive with no hub link sends nothing unless a relay is configured.')}
        </div>
        <div class="card-inset mt-5">
          <div class="settings-subsection-title">Hub-only secrets</div>
          <div class="config-help">Used only when this hive runs as a hub. Values are write-only and never shown.</div>
          ${secret('notifications_discord_requests_webhook_url', 'Provision request Discord webhook', 'hub.notifications.discord.requests_webhook_url', st.notifications_discord_requests_webhook_set, 'Posts new hosted-hive provision requests to Discord. Must be https. Read by the hub on each request.', 'Discord webhook the hub posts new hosted-hive provision requests to. Wins over notifications.discord.requests_webhook_url. Must start with https:// and not target a private address.')}
          ${secret('nps_relay_pull_secret', 'NPS relay pull secret', 'hub.nps_relay_pull_secret', st.nps_relay_pull_secret_set, 'The credential the hub uses to pull and ack NPS relay entries. HIVE_NPS_RELAY_PULL_SECRET overrides it; prefer the env var.', 'Secret, hub only. The credential the hub uses to pull and ack relay entries. Prefer the HIVE_NPS_RELAY_PULL_SECRET env var.')}
          ${owner ? '' : '<div class="config-help mt-3">Owner role required to change these settings.</div>'}
        </div>`;
    }

    // The announcement is saved as one object; any field edit sends all three.
    function hubAnnouncementChanged() {
      const v = id => ((document.getElementById(id) || {}).value || '').trim();
      markDirty('hub', 'contribute_announcement', {
        text: v('hub-set-ann-text'),
        level: v('hub-set-ann-level') || 'info',
        expires_at: v('hub-set-ann-expires')
      });
    }

    // HEARTBEAT_OMIT_CLASSES mirrors config.HeartbeatOmitAll; tradeoffs match docs/telemetry.md.
    var HEARTBEAT_OMIT_CLASSES = [
      ['repos', 'Repo names', 'Hub cannot list this hive\'s repos or per-repo activity.'],
      ['users', 'Usernames', 'Hub loses owner, leaderboard and per-user time-in-hive.'],
      ['task_titles', 'Task titles', 'Hub leaderboard shows no current task.'],
      ['dashboard_urls', 'Dashboard URLs', 'Hub cannot link to this hive\'s dashboard or snapshot.']
    ];
    function renderHubTelemetryOmit(omitted) {
      var set = {};
      (omitted || []).forEach(function(c) { set[c] = true; });
      return '<div style="display:flex;flex-direction:column;gap:var(--sp-2)">' + HEARTBEAT_OMIT_CLASSES.map(function(c) {
        var off = !!set[c[0]];
        return '<div style="font-size:var(--fs-sm);color:var(--text)"><span style="display:inline-block;min-width:7.5rem">' + esc(c[1]) + '</span>' +
          '<span style="font-weight:600;color:' + (off ? 'var(--green)' : 'var(--muted)') + '">' + (off ? 'Withheld' : 'Sent') + '</span>' +
          '<span style="color:var(--muted)"> — ' + esc(c[2]) + '</span></div>';
      }).join('') + '</div>';
    }

    // Auto-update section helpers (#6962, #6963). autoUpdatePeriodLabel maps
    // the stored auto_upgrade_mode onto a human label; an absent/unknown mode
    // is shown as "Unknown", never guessed. fetchAutoUpdateStatus renders the
    // live status from /api/version's autoUpdate object — the single source of
    // truth built server-side — and NEVER paints a non-healthy state green.
    function autoUpdatePeriodLabel(mode) {
      switch (String(mode || '').toLowerCase()) {
        case 'instant': return 'Instant — as soon as a new version is available';
        case 'daily': return 'Daily';
        case 'weekly': return 'Weekly';
        default: return 'Unknown — not yet reported by the hub';
      }
    }

    // Reflect the hub's upgrade policy (#7262) onto the Hub tab controls. The
    // spoke-local hub.auto_upgrade toggle is only the policy when the hub says
    // the spoke manages its own upgrades (or nobody does); on a hub-managed
    // hive the hub owns it, so lock the switch and label who owns it.
    function applyAutoUpdatePolicyToHubTab(au) {
      var sw = document.getElementById('hub-autoupdate-toggle');
      var who = document.getElementById('hub-autoupdate-managed-by');
      var sched = document.getElementById('hub-autoupdate-schedule');
      if (!au || au.policySource !== 'hub') return;
      if (sw) {
        sw.classList.toggle('on', !!au.enabled);
        var hubOwned = au.managedBy === 'hub';
        sw.style.pointerEvents = hubOwned ? 'none' : '';
        sw.style.opacity = hubOwned ? '0.55' : '';
        sw.title = hubOwned ? 'Managed by the hub — change it from the hub dashboard' : '';
      }
      if (who) {
        var label = au.managedBy === 'hub' ? 'managed by the hub'
          : au.managedBy === 'spoke' ? 'self-managed on hub instruction'
          : 'not managed';
        if (au.paused) label += ', paused fleet-wide';
        who.textContent = '(' + label + ') ';
        who.style.color = 'var(--muted)';
      }
      if (sched) {
        sched.textContent = autoUpdatePeriodLabel(au.period) + (au.paused ? ' — paused fleet-wide by a hub admin' : '');
      }
    }

    function fetchAutoUpdateStatus() {
      fetch('/api/version').then(function(r) { return r.json(); }).then(function(v) {
        var el = document.getElementById('hub-autoupdate-status');
        if (!el) return;
        var au = v && v.autoUpdate;
        if (!au) {
          // Older server without the field: be explicit rather than silent.
          el.style.color = 'var(--muted)';
          el.textContent = 'Auto-update status unavailable from this hive.';
          return;
        }
        // state → colour. Only up_to_date and disabled are non-alarming; every
        // other state (behind, retrying, failed, unknown) is NOT healthy and
        // must not read green — this is the #6963 guard.
        var color = 'var(--muted)', icon = 'ⓘ';
        switch (au.state) {
          case 'up_to_date': color = 'var(--green)'; icon = '✓'; break;
          case 'disabled': color = 'var(--muted)'; icon = '⊘'; break;
          case 'paused': color = 'var(--muted)'; icon = '⏸'; break;
          case 'behind': color = 'var(--yellow, #d4a017)'; icon = '⬆'; break;
          case 'retrying': color = 'var(--yellow, #d4a017)'; icon = '⟳'; break;
          case 'failed': color = 'var(--red)'; icon = '⚠'; break;
          case 'unknown': default: color = 'var(--yellow, #d4a017)'; icon = '❔'; break;
        }
        applyAutoUpdatePolicyToHubTab(au);
        var html = '<div style="color:' + color + ';font-weight:600">' + icon + ' ' + escapeHtml(au.detail || au.state || 'unknown') + '</div>';
        var rows = '';
        if (au.currentCommit) rows += '<div>Current commit: <code>' + escapeHtml(au.currentCommit) + '</code></div>';
        if (au.targetBranch || au.targetChannel) {
          var line = au.targetBranch ? '<code>' + escapeHtml(au.targetBranch) + '</code>' : '';
          if (au.targetChannel) line += (line ? ' via ' : '') + '<code>:' + escapeHtml(au.targetChannel) + '</code> channel';
          var tipWord = au.targetChannel ? 'channel at' : 'tip';
          rows += '<div>Target line: ' + line + (au.targetCommit ? ' (' + tipWord + ' <code>' + escapeHtml(au.targetCommit) + '</code>)' : '') + '</div>';
        }
        if (au.policySource === 'hub') rows += '<div>Policy source: the hub (heartbeat)</div>';
        else rows += '<div>Policy source: this spoke\'s local config (the hub has not reported a policy)</div>';
        if (au.commitsBehind != null) rows += '<div>Commits behind: ' + escapeHtml(String(au.commitsBehind)) + '</div>';
        if (au.state !== 'disabled' && au.state !== 'paused') {
          var next = au.nextUpdateAt || (au.nextUpdateStatus === 'none' ? 'none queued (stable is current)' : (au.nextUpdateStatus === 'paused' ? 'paused' : 'unknown'));
          rows += '<div>Next update: ' + escapeHtml(next) + '</div>';
        }
        if (au.heldReason) rows += '<div class="text-warn">Scheduled update held' + (au.heldAt ? ' at ' + escapeHtml(au.heldAt) : '') + ': ' + escapeHtml(au.heldReason) + '</div>';
        if (au.lastAttemptAt) rows += '<div>Last attempt: ' + escapeHtml(au.lastAttemptAt) + '</div>';
        if (au.lastError) rows += '<div class="text-danger">Last error: ' + escapeHtml(au.lastError) + '</div>';
        el.style.color = 'var(--muted)';
        el.innerHTML = html + '<div style="margin-top:var(--sp-2);line-height:1.5">' + rows + '</div>';
      }).catch(function() {
        var el = document.getElementById('hub-autoupdate-status');
        if (el) { el.style.color = 'var(--muted)'; el.textContent = 'Auto-update status unavailable (could not reach /api/version).'; }
      });
    }

    function fetchQueueCount() {
      // Use the same source as the Operations tab's Ready-work queue
      // (/api/contribute/queue). Both endpoints now describe admitted work, but
      // the queue remains this panel's source of truth because queue_total is
      // uncapped and excludes separately visible operator-held rows.
      fetch('/api/contribute/queue').then(function(r) { return r.json(); }).then(function(d) {
        var el = document.getElementById('hub-queue-count');
        if (el) el.textContent = (d.queue_total != null ? d.queue_total : (d.queue || []).length) + ' items in queue';
      }).catch(function() {
        var el = document.getElementById('hub-queue-count');
        if (el) el.textContent = '—';
      });
    }

    function toggleAutoSnapshot(el) {
      el.classList.toggle('on');
      var isOn = el.classList.contains('on');
      markDirty('hub', 'auto_snapshot', isOn);
      var urlInput = document.getElementById('hub-snapshot-url');
      if (urlInput) {
        urlInput.disabled = isOn;
        urlInput.style.opacity = isOn ? '0.5' : '1';
        urlInput.placeholder = isOn ? '' : 'https://your-site.com/dashboard-preview';
        if (isOn) {
          var dashUrl = (_configState.data.hub || {}).dashboard_url || window.location.origin || '';
          if (dashUrl) {
            var snapUrl = dashUrl.replace(/\/$/, '') + '/snapshot';
            urlInput.value = snapUrl;
            markDirty('hub', 'snapshot_url', snapUrl);
          }
        }
      }
    }

    // toggleCooldownEnabled flips the Task Cooldown switch and live-enables/disables
    // the paired period input (mirrors toggleAutoSnapshot). Marks the section dirty so
    // Save PUTs contribute_cooldown_enabled through /api/config/governor/hub.
    function toggleCooldownEnabled(el) {
      el.classList.toggle('on');
      var isOn = el.classList.contains('on');
      markDirty('hub', 'contribute_cooldown_enabled', isOn);
      var hoursInput = document.getElementById('hub-cooldown-hours');
      if (hoursInput) {
        hoursInput.disabled = !isOn;
        hoursInput.style.opacity = isOn ? '1' : '0.5';
      }
    }

    function toggleDelegatableAgentRole(role, enabled) {
      var hub = (_configState.data && _configState.data.hub) || {};
      var base = ['scanner', 'quality', 'outreach'];
      var roles = (hub.contribute_delegatable_roles || base).map(function(r) { return String(r || '').toLowerCase(); });
      base.forEach(function(r) { if (!roles.includes(r)) roles.push(r); });
      role = String(role || '').toLowerCase();
      roles = roles.filter(function(r) { return r && r !== 'supervisor' && (enabled || r !== role); });
      if (enabled && role && !roles.includes(role)) roles.push(role);
      roles.sort();
      hub.contribute_delegatable_roles = roles;
      markDirty('hub', 'contribute_delegatable_roles', roles);
    }

    // renderFilterWithMode renders one contribute filter as a mode toggle
    // (Allow only / Deny) plus a single list of patterns — the well-known
    // allowlist/denylist pattern. cfg: {label, noun, unit, modeKey, listKey,
    // mode, list, inputId, placeholder, perLabel}. In allow mode the chips are
    // green and the helper says only-these pass; in deny mode they're red and
    // the helper says these-are-skipped. perLabel tweaks the copy for labels
    // (a set) vs a scalar title/author.
    function renderFilterWithMode(cfg) {
      var isAllow = cfg.mode === 'allow';
      var chipBg = isAllow ? 'rgba(34,197,94,0.15)' : 'rgba(248,81,73,0.15)';
      var chipColor = isAllow ? 'var(--green)' : 'var(--red)';
      var chipBorder = isAllow ? 'rgba(34,197,94,0.3)' : 'rgba(248,81,73,0.3)';
      var help;
      if (isAllow) {
        help = cfg.perLabel
          ? 'Only ' + cfg.unit + 's carrying one of these labels are worked; everything else is skipped.'
          : 'Only ' + cfg.unit + 's whose ' + cfg.noun + ' matches are worked; everything else is skipped.';
        if ((cfg.list || []).length === 0) help += ' (List is empty — filter is off until you add a pattern.)';
      } else {
        help = cfg.perLabel
          ? cfg.unit.charAt(0).toUpperCase() + cfg.unit.slice(1) + 's carrying any of these labels are skipped; everything else is worked.'
          : cfg.unit.charAt(0).toUpperCase() + cfg.unit.slice(1) + 's whose ' + cfg.noun + ' matches are skipped; everything else is worked.';
      }
      function seg(m, text) {
        var on = cfg.mode === m;
        return '<button data-action="setHubFilterMode" data-arg0="' + cfg.modeKey + '" data-arg1="' + m + '" ' +
          'style="padding:3px 12px;border:1px solid var(--border);cursor:pointer;font-size:0.72rem;' +
          (m === 'allow' ? 'border-radius:6px 0 0 6px;' : 'border-radius:0 6px 6px 0;border-left:none;') +
          (on ? 'background:' + (m === 'allow' ? 'var(--green)' : 'var(--red)') + ';color:#fff;font-weight:600;' : 'background:var(--bg);color:var(--muted);') +
          '">' + text + '</button>';
      }
      var chips = (cfg.list || []).map(function(v) {
        return '<span style="display:inline-flex;align-items:center;gap:var(--sp-2);padding:var(--sp-1) var(--sp-4);border-radius:var(--r-pill);font-size:0.72rem;background:' + chipBg + ';color:' + chipColor + ';border:1px solid ' + chipBorder + '">' +
          esc(v) + ' <span data-action="removeHubFilter" data-arg0="' + cfg.listKey + '" data-arg1="' + esc(v) + '" style="cursor:pointer;opacity:0.7">✕</span></span>';
      }).join('');
      return '<div class="config-field">' +
        '<label>' + cfg.label + ' ' + settingHelpMark('contributor-relay', '', cfg.label, 'Choose a mode, then add patterns. Supports wildcards (*) and regex (/pattern/).') + '</label>' +
        '<div style="display:inline-flex;margin-bottom:var(--sp-3)">' + seg('allow', 'Allow only these') + seg('deny', 'Deny these') + '</div>' +
        '<div class="fs-xs text-muted mb-3">' + help + '</div>' +
        '<div style="display:flex;flex-wrap:wrap;gap:var(--sp-2);margin-bottom:var(--sp-2)">' + chips + '</div>' +
        '<div style="display:flex;gap:var(--sp-2)"><input type="text" id="' + cfg.inputId + '" placeholder="' + cfg.placeholder + '" style="flex:1" ' +
          'data-enter-action="addHubFilter" data-arg0="' + cfg.listKey + '" data-arg1="' + cfg.inputId + '" data-prevent="1">' +
          '<button class="hv-btn btn-primary btn-sm" data-action="addHubFilter" data-arg0="' + cfg.listKey + '" data-arg1="' + cfg.inputId + '">Add</button></div>' +
        '</div>';
    }

    // setHubFilterMode switches a filter between allow and deny mode.
    function setHubFilterMode(modeKey, mode) {
      if (!_configState.data.hub) _configState.data.hub = {};
      _configState.data.hub[modeKey] = mode;
      markDirty('hub', modeKey, mode);
      renderConfigTab('Hub');
    }

    function addModelPreset(val) {
      if (!_configState.data.hub) _configState.data.hub = {};
      if (!_configState.data.hub.contribute_allow_models) _configState.data.hub.contribute_allow_models = [];
      if (_configState.data.hub.contribute_allow_models.indexOf(val) === -1) {
        _configState.data.hub.contribute_allow_models.push(val);
        markDirty('hub', 'contribute_allow_models', _configState.data.hub.contribute_allow_models);
      }
      renderConfigTab('Hub');
    }

    function addHubFilter(key, inputId) {
      var input = document.getElementById(inputId);
      var val = (input.value || '').trim();
      if (!val) return;
      if (!_configState.data.hub) _configState.data.hub = {};
      if (!_configState.data.hub[key]) _configState.data.hub[key] = [];
      if (_configState.data.hub[key].indexOf(val) === -1) {
        _configState.data.hub[key].push(val);
        markDirty('hub', key, _configState.data.hub[key]);
      }
      input.value = '';
      renderConfigTab('Hub');
    }

    function removeHubFilter(key, val) {
      if (!_configState.data.hub || !_configState.data.hub[key]) return;
      _configState.data.hub[key] = _configState.data.hub[key].filter(function(v) { return v !== val; });
      markDirty('hub', key, _configState.data.hub[key]);
      renderConfigTab('Hub');
    }

    function setSnapshotFrameAncestorsFromText(value) {
      const origins = (value || '').split(/\r?\n|,/).map(function(v) { return v.trim(); }).filter(Boolean);
      if (!_configState.data.hub) _configState.data.hub = {};
      _configState.data.hub.snapshot_frame_ancestors = origins;
      markDirty('hub', 'snapshot_frame_ancestors', origins);
      renderConfigTab('Hub');
    }

    function setContributeHelpLinksFromText(value) {
      const links = (value || '').split(/\r?\n/).map(function(line) {
        line = line.trim();
        if (!line) return null;
        var i = line.indexOf('|');
        if (i < 0) return { label: line, url: '' };
        return { label: line.slice(0, i).trim(), url: line.slice(i + 1).trim() };
      }).filter(Boolean);
      if (!_configState.data.hub) _configState.data.hub = {};
      _configState.data.hub.contribute_help_links = links;
      markDirty('hub', 'contribute_help_links', links);
    }

    function setTierLimit(tier, key, val) {
      if (!_configState.data.hub) _configState.data.hub = {};
      if (!_configState.data.hub.tier_limits) _configState.data.hub.tier_limits = {};
      if (!_configState.data.hub.tier_limits[tier]) _configState.data.hub.tier_limits[tier] = {};
      _configState.data.hub.tier_limits[tier][key] = val;
      markDirty('hub', 'tier_limits', _configState.data.hub.tier_limits);
    }

    function toggleContribRepo(el, repoFull) {
      if (!_configState.data.hub) _configState.data.hub = {};
      var disabled = _configState.data.hub.disabled_repos || [];
      var isOn = el.classList.contains('on');
      if (isOn) {
        if (contribRepoMatchingDisabledEntries(repoFull, disabled).length === 0) disabled.push(repoFull);
      } else {
        var blockers = contribRepoMatchingDisabledEntries(repoFull, disabled);
        var wildcardBlockers = blockers.filter(function(r) { return String(r || '').indexOf('*') >= 0; });
        if (wildcardBlockers.length) {
          showToast('This repo is disabled by wildcard disabled_repos entry: ' + wildcardBlockers.join(', '), 'error');
          renderConfigTab('Hub');
          return;
        }
        disabled = disabled.filter(function(r) { return !contribRepoDisabledEntryMatches(repoFull, r); });
      }
      _configState.data.hub.disabled_repos = disabled;
      markDirty('hub', 'disabled_repos', disabled);
      renderConfigTab('Hub');
    }

    function toggleContribTier(el, tier) {
      el.classList.toggle('on');
      if (!_configState.data.hub) _configState.data.hub = {};
      var disabled = _configState.data.hub.disabled_tiers || [];
      if (el.classList.contains('on')) {
        disabled = disabled.filter(function(t) { return t !== tier; });
      } else {
        if (disabled.indexOf(tier) === -1) disabled.push(tier);
      }
      _configState.data.hub.disabled_tiers = disabled;
      markDirty('hub', 'disabled_tiers', disabled);
    }

