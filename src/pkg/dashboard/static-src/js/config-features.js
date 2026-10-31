    function featuresSectionDomId(id) { return 'features-section-' + id; }

    function featuresSectionHtml(id, inner) {
      const s = FEATURES_SECTIONS.find(x => x.id === id);
      if (!s) throw new Error('unknown Features section ' + id);
      return `<section class="config-section features-section" id="${featuresSectionDomId(s.id)}" data-settings-section="${esc(s.id)}">
          <h3 class="features-section-title">${esc(s.title)}</h3>
          <p class="features-section-desc text-muted fs-base">${esc(s.desc)}</p>
          ${inner}
        </section>`;
    }

    function featuresSectionTocHtml() {
      return `<nav class="features-toc" aria-label="Features sections">${FEATURES_SECTIONS.map(s =>
        `<a href="#${featuresSectionDomId(s.id)}" class="features-toc-chip" data-action="settingsScrollToSection" data-prevent="1" data-arg0="${esc(s.id)}" data-arg-types="s">${esc(s.title)}</a>`).join('')}</nav>`;
    }

    const FEATURES_SECTION_SCROLL_PAD_PX = 8;

    // settingsScrollToSection scrolls the Settings dialog body (the scroll
    // container — not the window) to a [data-settings-section] heading.
    function settingsScrollToSection(id) {
      const body = document.getElementById('config-body');
      if (!body) return false;
      const target = body.querySelector('[data-settings-section="' + String(id).replace(/"/g, '\\"') + '"]');
      return configScrollBodyTo(body, target);
    }

    // configScrollBodyTo scrolls the dialog body (not the page) so `target`
    // sits just under the body's top edge.
    function configScrollBodyTo(body, target) {
      if (!body || !target) return false;
      const top = target.getBoundingClientRect().top - body.getBoundingClientRect().top + body.scrollTop;
      body.scrollTo({ top: Math.max(0, top - FEATURES_SECTION_SCROLL_PAD_PX), behavior: 'smooth' });
      return true;
    }

    // Section rail (#12169): the column left of the dialog body lists the active
    // tab's `.config-section > h3` headings (the same ones the sticky
    // section bar shows) and tracks which one is in view. It is derived from
    // the rendered DOM — a MutationObserver re-syncs it after every tab
    // render, async panel fill-in and search-results swap — so hidden
    // headings (search results, owner-only panels) drop out automatically.
    const CONFIG_RAIL_HEADING_SELECTOR = '.config-section > h3';
    const CONFIG_RAIL_MIN_SECTIONS = 2;
    // A section counts as "current" once its top is within this many px of
    // the body's top edge — just under the sticky heading band.
    const CONFIG_RAIL_ACTIVE_OFFSET_PX = 48;
    let _configRail = { bound: false, seq: 0, syncRaf: 0, scrollRaf: 0 };

    function configRailHeadingText(h) {
      let txt = settingsSearchText(h);
      h.querySelectorAll('.settings-yaml-hint, .config-info').forEach(extra => { txt = txt.replace(settingsSearchText(extra), ''); });
      return txt.replace(/\s+/g, ' ').trim();
    }

    function configRailHeadings() {
      const body = document.getElementById('config-body');
      if (!body) return [];
      return Array.from(body.querySelectorAll(CONFIG_RAIL_HEADING_SELECTOR))
        .filter(h => h.getClientRects().length > 0 && configRailHeadingText(h));
    }

    function configRailSync() {
      const rail = document.getElementById('config-rail');
      if (!rail) return;
      const heads = configRailHeadings();
      if (heads.length < CONFIG_RAIL_MIN_SECTIONS) {
        rail.hidden = true;
        rail.innerHTML = '';
        rail.dataset.sig = '';
        return;
      }
      const items = heads.map(h => {
        const section = h.parentElement;
        if (!section.id) section.id = 'config-rail-section-' + (++_configRail.seq);
        return { id: section.id, title: configRailHeadingText(h) };
      });
      const sig = items.map(x => x.id + '\n' + x.title).join('\u0000');
      if (rail.dataset.sig !== sig) {
        rail.dataset.sig = sig;
        rail.innerHTML = '<div class="config-rail-title">On this tab</div><ul class="config-rail-list">' +
          items.map(x => '<li><button type="button" class="config-rail-link" data-action="configRailGo" data-arg0="' + esc(x.id) + '">' + esc(x.title) + '</button></li>').join('') +
          '</ul>';
      }
      rail.hidden = false;
      configRailHighlight();
    }

    function configRailHighlight() {
      const rail = document.getElementById('config-rail');
      const body = document.getElementById('config-body');
      if (!rail || rail.hidden || !body) return;
      const links = Array.from(rail.querySelectorAll('.config-rail-link'));
      if (!links.length) return;
      const bodyTop = body.getBoundingClientRect().top;
      let current = null;
      links.forEach(link => {
        const section = document.getElementById(link.dataset.arg0);
        if (section && section.getBoundingClientRect().top - bodyTop <= CONFIG_RAIL_ACTIVE_OFFSET_PX) current = link;
      });
      // Scrolled to the end: the last section may be too short to ever reach
      // the top edge, so it is the current one.
      if (body.scrollTop + body.clientHeight >= body.scrollHeight - 1) current = links[links.length - 1];
      if (!current) current = links[0];
      links.forEach(link => {
        if (link === current) link.setAttribute('aria-current', 'true');
        else link.removeAttribute('aria-current');
      });
    }

    function configRailGo(id) {
      const body = document.getElementById('config-body');
      const target = document.getElementById(id);
      if (!body || !target || !body.contains(target)) return false;
      return configScrollBodyTo(body, target);
    }

    function configRailBind() {
      if (_configRail.bound) return;
      const body = document.getElementById('config-body');
      if (!body || typeof MutationObserver === 'undefined') return;
      _configRail.bound = true;
      const scheduleSync = () => {
        if (_configRail.syncRaf) return;
        _configRail.syncRaf = requestAnimationFrame(() => { _configRail.syncRaf = 0; configRailSync(); });
      };
      new MutationObserver(scheduleSync).observe(body, { childList: true, subtree: true, attributes: true, attributeFilter: ['hidden', 'open'] });
      body.addEventListener('scroll', () => {
        if (_configRail.scrollRaf) return;
        _configRail.scrollRaf = requestAnimationFrame(() => { _configRail.scrollRaf = 0; configRailHighlight(); });
      }, { passive: true });
      scheduleSync();
    }

    // renderGovFeatures renders operational features that are not security posture
    // controls. ioscan moved to Security/Input Defense; OpenTelemetry and retro
    // stay here as observability/quality-loop settings. The tab is organised
    // into FEATURES_SECTIONS; keep new controls inside the section they belong
    // to rather than appending to the end of the template.
    function renderGovFeatures(f, am, rv) {
      f = f || {};
      am = am || {};
      rv = rv || {};
      const cv = (_configState.data || {}).convergence || {};
      const io = (_configState.data || {}).integration_orchestration || { mode: 'off', scopes: [] };
      const integrationExecution = io.execution || { mode: 'off' };
      const integrationExecutionProfile = Object.assign({}, integrationExecution);
      delete integrationExecutionProfile.mode;
      const planOn = f.planFromLabel === true;
      const formalAvailable = f.formalAvailable === true;
      const formalMinACMMLevel = f.formalMinACMMLevel || 5;
      const retroDefaults = f.retroDefaults || {};
      const issuesDefaults = f.issuesDefaults || {};
      const jevDefaults = f.jevDefaults || {};
      const jevProviders = Array.isArray(f.jevProviders) ? f.jevProviders : ['openrouter', 'typesafe'];
      const dupSweepDefaults = f.duplicateSweepDefaults || {};
      const gfiDefaults = f.goodFirstIssueDefaults || {};
      const gfiStats = f.goodFirstIssueStats || null;
      const prFollowUpDefaults = f.prFollowUpDefaults || {};
      const platformDefaults = f.platformDefaults || {};
      const runsDefaults = f.runsDefaults || {};
      const publicationAvailable = f.publicationAvailable === true;
      const publicationMinACMMLevel = f.publicationMinACMMLevel || 3;
      const cls = (_configState.data || {}).classifier || {};
      const amSelfOn = am.self_authored !== false; // nil/undefined = enabled
      const amChecks = Array.isArray(am.required_checks) ? am.required_checks : [];
      const amBots = Array.isArray(am.bot_authors) ? am.bot_authors : [];
      const amTrustedAuthors = am.trusted_authors || {};
      const amTrustedAuthorRoles = Array.isArray(amTrustedAuthors.roles) && amTrustedAuthors.roles.length ? amTrustedAuthors.roles : ['merger','owner'];
      const amTrustedAuthorRole = amTrustedAuthors.require_role || 'merger';
      const amTrustedAuthorLabels = Array.isArray(amTrustedAuthors.exclude_labels) ? amTrustedAuthors.exclude_labels : ['hold','do-not-merge','needs-human'];
      const amTrustedAuthorRepos = Array.isArray(amTrustedAuthors.repos) ? amTrustedAuthors.repos : [];
      const watchedRepos = Array.isArray((_configState.data || {}).repos) ? (_configState.data || {}).repos : [];
      const amTrustedAuthorRepoSet = new Set(amTrustedAuthorRepos.map(r => String(r).toLowerCase()));
      const amTrustedAuthorRoleOptions = amTrustedAuthorRoles.map(r => `<option value="${esc(r)}"${r === amTrustedAuthorRole ? ' selected' : ''}>${esc(r)}</option>`).join('');
      const amTrustedAuthorRepoOptions = watchedRepos.map(r => `<option value="${esc(r)}"${amTrustedAuthorRepoSet.has(String(r).toLowerCase()) ? ' selected' : ''}>${esc(r)}</option>`).join('');
      const amContributorLane = am.contributor_lane || {};
      const amContributorLaneOn = amContributorLane.enabled !== false; // nil/undefined = enabled
      const amContributorLaneVerdict = amContributorLane.require_reviewer_verdict !== false;
      const amContributorLaneRegistry = amContributorLane.registry_tiers_sufficient !== false;
      const amContributorLaneHeldRuns = amContributorLane.approve_held_runs_for_registry_trusted !== false;
      const amContributorLaneWorkflowApproval = amContributorLane.owner_approval_releases_workflow_fence !== false;
      const amContributorLaneSoak = Number.isFinite(Number(amContributorLane.soak_minutes)) ? Number(amContributorLane.soak_minutes) : 30;
      const amContributorLaneLabels = Array.isArray(amContributorLane.exclude_labels) ? amContributorLane.exclude_labels : ['hold','do-not-merge','needs-human','blocked','needs-rebase'];
      const amContributorLaneTiers = Array.isArray(amContributorLane.require_tiers) && amContributorLane.require_tiers.length ? amContributorLane.require_tiers : ['merger','trusted'];
      const amContributorLaneGrantable = Array.isArray(amContributorLane.grantable_tiers) ? amContributorLane.grantable_tiers : ['trusted','merger','advisor'];
      const amContributorLaneScope = typeof amContributorLane.auto_promotion_scope === 'string' && amContributorLane.auto_promotion_scope ? amContributorLane.auto_promotion_scope : 'hive';
      const amContributorLaneScopes = Array.isArray(amContributorLane.auto_promotion_scopes) && amContributorLane.auto_promotion_scopes.length ? amContributorLane.auto_promotion_scopes : ['hive','repo'];
      const amContributorLaneScopeLabel = { hive: "hive — every repository this hive manages", repo: "repo — the PR's repository only" };
      const amContributorLaneScopeOptions = amContributorLaneScopes.map(v => `<option value="${esc(v)}"${v === amContributorLaneScope ? ' selected' : ''}>${esc(amContributorLaneScopeLabel[v] || v)}</option>`).join('');
      const amContributorLaneRepoSet = new Set((Array.isArray(amContributorLane.repos) ? amContributorLane.repos : []).map(r => String(r).toLowerCase()));
      const amContributorLaneRepoOptions = watchedRepos.map(r => `<option value="${esc(r)}"${amContributorLaneRepoSet.has(String(r).toLowerCase()) ? ' selected' : ''}>${esc(r)}</option>`).join('');
      const amBotSourceLabel = { known: 'known bot', discovered: 'seen in open PRs', custom: 'added manually' };
      const amBotRow = (b) => `<div class="config-toggle am-bot-row" data-bot-login="${esc(b.login)}">
              <div class="config-toggle-switch am-bot-switch ${b.trusted ? 'on' : ''}" data-action="gh97" data-arg0="${esc(b.login)}" data-arg-types="s" role="switch" aria-checked="${b.trusted ? 'true' : 'false'}"></div>
              <span class="config-toggle-label"><code>${esc(b.login)}</code> <span class="am-bot-source">${esc(amBotSourceLabel[b.source] || b.source || '')}</span> ${settingHelpMark('hive-merge', 'trusted-bot-authors', b.login, `On: PRs authored by ${esc(b.login)} are merged by the self-authored sweep once every gate passes (required CI green, mergeable, no hold label, intent tier, approval desk). Off: the sweep skips them and they wait for a human.`)}${b.source === 'custom' ? ` <button type="button" class="hv-btn btn-secondary btn-sm btn-small" data-action="gh99" data-arg0="${esc(b.login)}" data-arg-types="s">remove</button>` : ''}</span>
            </div>`;
      const rb = (_configState.data || {}).review_bots;
      const rbLogins = rb && Array.isArray(rb.logins) ? rb.logins : [];
      const rvAgents = Array.isArray(rv.reviewer_agents) ? rv.reviewer_agents : [];
      const rvCandidates = Array.isArray(rv.reviewer_candidates) ? rv.reviewer_candidates : [];
      const rvCanEdit = dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      // Names in reviewer_agents that no configured agent carries: still shown
      // so a stale entry is visible and removable rather than silently kept.
      const rvUnknownAgents = rvAgents.filter(n => !rvCandidates.some(c => c.name === n));
      const rvCandidateRow = (c) => {
        const engine = [c.backend, c.model].filter(Boolean).join(' / ') || 'engine not set';
        const state = !c.available ? (c.enabled ? 'paused or on-demand' : 'disabled') : (c.keyword_match ? 'review keyword' : '');
        return `<label class="rv-reviewer-row${c.available ? '' : ' rv-reviewer-unavailable'}" data-reviewer="${esc(c.name)}" data-engine="${esc(c.backend || '')}" data-available="${c.available ? '1' : '0'}" data-keyword="${c.keyword_match ? '1' : '0'}">
            <input type="checkbox" class="rv-reviewer-on" ${rvAgents.includes(c.name) ? 'checked' : ''}${rvCanEdit ? '' : ' disabled'} data-change-action="gh74">
            <code>${esc(c.name)}</code><span class="rv-reviewer-engine">&mdash; ${esc(engine)}</span>${state ? `<span class="rv-reviewer-state">(${esc(state)})</span>` : ''}
          </label>`;
      };
      const rvUnknownRow = (n) => `<label class="rv-reviewer-row rv-reviewer-unavailable" data-reviewer="${esc(n)}" data-engine="" data-available="0" data-keyword="0">
            <input type="checkbox" class="rv-reviewer-on" checked${rvCanEdit ? '' : ' disabled'} data-change-action="gh74">
            <code>${esc(n)}</code><span class="rv-reviewer-state">(not a configured agent)</span>
          </label>`;
      const rvBuiltin = Array.isArray(rv.builtin_perspectives) ? rv.builtin_perspectives : [];
      const rvSelected = Array.isArray(rv.perspectives) ? rv.perspectives : [];
      const rvPrompts = (rv.perspective_prompts && typeof rv.perspective_prompts === 'object') ? rv.perspective_prompts : {};
      // Empty selection means "all built-ins" server-side; render it that way
      // so the checkboxes show what will actually run, not an empty list.
      const rvBuiltinNames = rvBuiltin.map(b => b.name);
      const rvCustomNames = Object.keys(rvPrompts).filter(n => !rvBuiltinNames.includes(n))
        .concat(rvSelected.filter(n => !rvBuiltinNames.includes(n)))
        .filter((n, i, a) => a.indexOf(n) === i);
      const rvIsOn = (name) => rvSelected.length === 0 ? rvBuiltinNames.includes(name) || rvCustomNames.includes(name) : rvSelected.includes(name);
      const rvPerspectiveRow = (name, defaultFocus) => {
        const custom = rvPrompts[name] || '';
        const isCustom = !rvBuiltinNames.includes(name);
        return `<div class="rv-perspective" data-perspective="${esc(name)}" style="margin-top:var(--sp-4);padding:8px 10px;border:1px solid var(--border);border-radius:var(--r)">
            <label style="display:flex;align-items:center;gap:var(--sp-4);cursor:pointer;font-weight:600;font-size:0.8rem">
              <input type="checkbox" class="rv-perspective-on" ${rvIsOn(name) ? 'checked' : ''} data-change-action="gh91">
              <code>${esc(name)}</code>${isCustom ? ' <span style="font-size:var(--fs-xs);color:var(--muted);font-weight:400">custom</span>' : ''}
              ${isCustom ? `<button type="button" class="hv-btn btn-secondary btn-sm btn-small btn-primary" data-action="gh93" data-arg0="${esc(name)}" data-arg-types="s">remove</button>` : ''}
            </label>
            <textarea class="rv-perspective-focus" rows="2" style="width:100%;margin-top:var(--sp-3);font-size:0.76rem" placeholder="${esc(defaultFocus || 'what should this perspective look for?')}" data-change-action="gh92">${esc(custom)}</textarea>
            ${defaultFocus ? `<p style="font-size:var(--fs-xs);color:var(--muted);margin:3px 0 0">Built-in: ${esc(defaultFocus)}${custom ? ' — <a href="#" data-action="gh94" data-prevent="1" data-arg0="' + esc(name) + '" data-arg-types="s">use built-in</a>' : ''}</p>` : ''}
          </div>`;
      };
      const rvRec = rv.recommendations || {};
      const rvRecRepos = Array.isArray(rvRec.repos) ? rvRec.repos : [];
      const traj = (_configState.data || {}).trajectory || {};
      const trajOn = traj.enabled !== false; // default on
      const trajReady = traj.reviewerReady === true;
      const trajEndpoint = traj.effectiveEndpoint || '';
      const trajModel = traj.effectiveModel || '';
      const trajInterval = traj.intervalS || 0;
      const trajDiv = traj.onDivergence || 'pause';
      const trajStats = traj.stats || null;
      const trajSkipReasons = trajStats && trajStats.skippedByReason ? Object.entries(trajStats.skippedByReason).map(([r, n]) => `${esc(r)}: ${n}`).join(', ') : '';
      const trajStatsHtml = trajStats
        ? `<div class="fs-xs text-muted mt-2">Reviews run: <b>${trajStats.reviewed || 0}</b> · skipped: <b>${trajStats.skipped || 0}</b>${trajSkipReasons ? ` (${trajSkipReasons})` : ''} · lane passes: ${trajStats.runs || 0}. Skips only happen when the agent is proven idle and the review is not overdue — never on an unchanged transcript.</div>`
        : '';
      const rotationOn = f.rotationEnabled === true;
      const rotationProviders = (f.rotationProviders && typeof f.rotationProviders === 'object') ? f.rotationProviders : {};
      const rotationAgents = (f.rotationAgents && typeof f.rotationAgents === 'object') ? f.rotationAgents : {};
      const rotationAgentNames = ((_configState.data || {}).agents || []).map(a => typeof a === 'string' ? a : (a && a.name) || '').filter(Boolean).sort();
      const rotationAgentRows = rotationAgentNames.map(name => {
        const tier = rotationAgents[name] || 'T2';
        return `<tr><td style="padding:var(--sp-2) var(--sp-4) var(--sp-2) var(--sp-0)"><code>${esc(name)}</code></td><td><select data-change-action="rotationAgentTierChanged" data-arg0="${esc(name)}" data-arg-types="s,v">
          <option value="T1" ${tier === 'T1' ? 'selected' : ''}>T1</option>
          <option value="T2" ${tier === 'T2' ? 'selected' : ''}>T2</option>
          <option value="T3" ${tier === 'T3' ? 'selected' : ''}>T3</option>
        </select></td></tr>`;
      }).join('');
      setTimeout(() => { if (rotationOn) loadRotationHeadroom(); }, 0);
      // Status line: distinguish ON+running from ON+not-configured from OFF.
      let trajStatus, trajStatusColor;
      if (!trajOn) { trajStatus = 'Off'; trajStatusColor = 'var(--muted)'; }
      else if (trajReady) { trajStatus = 'On — active'; trajStatusColor = 'var(--green)'; }
      else { trajStatus = 'On — no reviewer endpoint (not running)'; trajStatusColor = 'var(--amber)'; }
      return `
        ${securityModelDatalistHtml()}
        <p class="text-muted fs-sm mb-5">Opt-in capabilities, grouped by what they govern. Each section below has a stable heading; use the chips to jump to one. Hive's own telemetry export lives under <b>Integrations &amp; observability</b>; use <b>Project Observability</b> to choose what telemetry and operations agents target in managed repositories.</p>
        <div style="font-size:0.72rem;color:var(--muted);background:var(--surface);border:1px solid var(--border);border-radius:8px;padding:10px;margin-bottom:14px">
          Input/Output Scanning (ioscan) moved to <b>Security → Input Defense</b> so its enablement, fail mode, canaries, and classifier settings have one canonical control surface.
        </div>
        ${featuresSectionTocHtml()}

        ${featuresSectionHtml('auto-merge', `
        <div class="settings-subsection">
          <div class="settings-subsection-title">Auto-Merge ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Auto-Merge', `Controls the self-authored sweep: Hive merges its own green PRs directly since Prow cannot self-approve.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${amSelfOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="auto-merge" data-key="self_authored"></div>
            <span class="config-toggle-label">Auto-merge App-authored PRs ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Auto-merge App-authored PRs', `Controls the self-authored sweep: Hive merges its own green PRs directly since Prow cannot self-approve.`)}</span>
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Max merges per sweep ${yamlHint('auto_merge.max_merges')} ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Max merges per sweep', `Caps how many PRs one sweep pass may merge. 0 or empty uses the default.`)}</label>
            <input type="number" min="0" step="1" value="${am.max_merges || ''}" placeholder="3" data-change-action="gh71">
          </div>
          <div class="config-field mt-4">
            <label>Minimum head age ${yamlHint('auto_merge.min_head_age')} ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Minimum head age', `How old a PR head must be before the sweep trusts an unknown required-check set. Closes the window after a push where slow GitHub Actions checks have not registered yet. Positive Go duration such as 3m.`)}</label>
            <input type="text" value="${esc(am.min_head_age || '')}" placeholder="3m0s" data-change-action="markDirty" data-arg0="auto-merge" data-arg1="min_head_age" data-arg-types="s,s,w">
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Required CI checks ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Required CI checks', `Comma-separated default status-check/check-run names the sweep must see green before merging, e.g. build-gate. For heterogeneous hives use auto_merge.repos.<repo>.required_checks per repository; keys may be bare repo names or owner/repo.`)}</label>
            <input type="text" value="${esc(amChecks.join(', '))}" placeholder="build-gate, lint" data-change-action="gh72">
            <p class="config-help">Applies to repos without an override. Use <code>auto_merge.repos.&lt;repo&gt;.required_checks</code> for per-repo required checks.</p>
          </div>
          <div class="config-field am-trusted-bots" id="am-trusted-bots">
            <label>Trusted bot authors ${settingHelpMark('hive-merge', 'trusted-bot-authors', 'Trusted bot authors', `PRs from these bots are merged by the self-authored sweep through exactly the same gates as Hive&apos;s own PRs: required CI green, mergeable, no hold label, intent tier, approval desk, head SHA re-verified at merge. Known bots are listed first; bots currently authoring open PRs in your repos are discovered automatically. Turn every bot off to keep the sweep App-only.`)}</label>
            ${amBots.length ? amBots.map(amBotRow).join('') : '<p class="am-bot-empty">No bots known or discovered yet.</p>'}
            <div class="am-bot-add">
              <input type="text" id="am-new-bot" placeholder="another bot login, e.g. snyk-bot">
              <button type="button" class="hv-btn btn-secondary btn-sm btn-small" data-action="gh98">Add bot</button>
            </div>
          </div>
          <div class="config-toggle mt-4">
            <div class="config-toggle-switch ${am.bot_takeover_enabled !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="auto-merge" data-key="bot_takeover_enabled"></div>
            <span class="config-toggle-label">Take over github-actions[bot] PRs whose checks cannot run ${yamlHint('auto_merge.bot_takeover.enabled')} ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Take over github-actions[bot] PRs', `On by default. Applies only when github-actions[bot] is a trusted bot author above. A PR that bot opens with a workflow GITHUB_TOKEN never triggers pull_request checks, so it can never go green. Once its head is older than both the minimum head age and 30 minutes with no pull_request run, the App pushes the same change as one signed-off commit to hive/takeover-&lt;n&gt;, opens a PR that supersedes it and closes the bot PR. One attempt per bot head SHA. Applies on the next sweep.`)}</span>
          </div>
          <div class="config-field" id="am-trusted-authors">
            <label>Trusted-author auto-merge ${settingHelpMark('hive-merge', 'trusted-author-auto-merge', 'Trusted-author auto-merge', `Operator opt-in for human-authored PRs: Hive may merge a green PR only when the author holds the configured hive role and, when required, GitHub says the author has push/maintain/admin permission on that repository. Fork PRs from non-members are blocked, excluded labels and CHANGES_REQUESTED reviews block, and the head SHA is re-checked at merge.`)}</label>
            <div class="config-toggle mt-3">
              <div id="am-trusted-authors-enabled" class="config-toggle-switch ${amTrustedAuthors.enabled ? 'on' : ''}" data-action="gh100" role="switch" aria-checked="${amTrustedAuthors.enabled ? 'true' : 'false'}"></div>
              <span class="config-toggle-label">Enable trusted-author tier ${settingHelpMark('hive-merge', 'trusted-author-auto-merge', 'Enable trusted-author tier', `Default off. Lets Hive merge green human-authored PRs whose author already holds merge authority in authorized_users at the configured role; it never creates new authority.`)}</span>
            </div>
            <div class="config-field-row mt-4">
              <div class="config-field">
                <label for="am-trusted-authors-require-role">Required author role ${yamlHint('auto_merge.trusted_authors.require_role')}</label>
                <select id="am-trusted-authors-require-role" data-change-action="gh101">${amTrustedAuthorRoleOptions}</select>
                <p class="am-trusted-authors-note">Who holds these roles is defined by Authorized users on the Security tab; hosted hives manage read, read-write, merger, and owner roles on the hub's Manage Access screen — <a class="text-accent" href="#" data-action="openTrustedAuthorSecurityAccess" data-prevent="1">Open Security tab</a>.</p>
              </div>
              <div class="config-field">
                <label>GitHub permission check ${settingHelpMark('hive-merge', 'trusted-author-auto-merge', 'GitHub permission check', `When on (the default), GitHub must also report push, maintain, or admin permission for the author on the repository.`)}</label>
                <div class="config-toggle">
                  <div id="am-trusted-authors-require-github-permission" class="config-toggle-switch ${amTrustedAuthors.require_github_permission !== false ? 'on' : ''}" data-action="gh100" role="switch" aria-checked="${amTrustedAuthors.require_github_permission !== false ? 'true' : 'false'}"></div>
                  <span class="config-toggle-label">Require push/maintain/admin ${settingHelpMark('hive-merge', 'trusted-author-auto-merge', 'Require push/maintain/admin', `When on (the default), GitHub must also report push, maintain, or admin permission for the author on the repository.`)}</span>
                </div>
              </div>
            </div>
            <div class="config-field mt-4">
              <label for="am-trusted-authors-exclude-labels">Excluded labels</label>
              <input id="am-trusted-authors-exclude-labels" type="text" value="${esc(amTrustedAuthorLabels.join(', '))}" placeholder="hold, do-not-merge, needs-human" data-change-action="gh102">
              <p class="am-trusted-authors-note">Any listed label keeps the PR out of this tier; defaults are shown when unset.</p>
            </div>
            <div class="config-field mt-4">
              <label for="am-trusted-authors-repos">Repositories</label>
              <select id="am-trusted-authors-repos" multiple size="${Math.min(Math.max(watchedRepos.length || 3, 3), 8)}" data-change-action="gh102" class="am-trusted-authors-repos">${amTrustedAuthorRepoOptions}</select>
              <p class="am-trusted-authors-note">Leave every repo unselected to allow all watched repositories. Eligibility still requires green CI, mergeable state, no excluded label, no outstanding CHANGES_REQUESTED review, and no fork non-member.</p>
            </div>
          </div>
          <div class="config-field" id="am-contributor-lane">
            <label>Contributor-lane auto-merge ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Contributor-lane auto-merge', `Merges PRs that this hive's own contributor lane produced: the linked issue must carry a hive:claim marker (kind=contributor) posted by this hive's App naming the PR author with an operator-granted tier and a GitHub login proven via the reissue-token flow, CI must be green on the current head for the soak window, a reviewer agent must be configured and approve, the diff must not touch CI workflows or delete tests, and Sentinel must have no blocking finding. Default on: only contributors you grant a required tier in Contributors get unattended merges.`)}</label>
            <div class="config-toggle mt-3">
              <div id="am-contributor-lane-enabled" class="config-toggle-switch ${amContributorLaneOn ? 'on' : ''}" data-action="gh103" role="switch" aria-checked="${amContributorLaneOn ? 'true' : 'false'}"></div>
              <span class="config-toggle-label">Enable contributor-lane tier ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Enable contributor-lane tier', `Default on. Turn off to make hive-produced contributor PRs wait for a human /lgtm + /approve again.`)}</span>
            </div>
            <p class="am-trusted-authors-note">⚠️ Grants unattended merge, under this hive's App identity, to every relay contributor whose claimed PR passes these gates. Only contributors holding a tier listed below (granted by an operator in Contributors) and with a verified GitHub login qualify.</p>
            <div class="config-field mt-4">
              <label for="am-contributor-lane-require-tiers">Required contributor tiers ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Required contributor tiers', `The claim's contributor must hold one of these operator-granted tiers (default trusted, merger). Newcomer and contributor are reached without an operator and are never accepted.`)}</label>
              <input id="am-contributor-lane-require-tiers" type="text" value="${esc(amContributorLaneTiers.join(', '))}" placeholder="trusted, merger" data-change-action="gh104">
              <p class="am-trusted-authors-note">Allowed values: ${esc(amContributorLaneGrantable.join(', '))}. Leave empty for the default (trusted, merger).</p>
            </div>
            <div class="config-field-row mt-4">
              <div class="config-field">
                <label for="am-contributor-lane-soak-minutes">Soak minutes ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Soak minutes', `How long the current head must stay green before the tier merges it. 0 (or unset) means the built-in default of 30 minutes, not an instant merge; the smallest real soak is 1 minute. A new push restarts the soak.`)}</label>
                <input id="am-contributor-lane-soak-minutes" type="number" min="0" step="1" value="${amContributorLaneSoak}" data-change-action="gh104">
                <p class="config-help">0 = default (30 min). Use 1 or more for a shorter soak; there is no zero-wait setting.</p>
              </div>
              <div class="config-field">
                <label>Reviewer verdict ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Reviewer verdict', `When on (the default), a configured reviewer agent must have recorded an approving verdict for the current head. Hives with no reviewer agent never merge through this tier.`)}</label>
                <div class="config-toggle">
                  <div id="am-contributor-lane-require-reviewer-verdict" class="config-toggle-switch ${amContributorLaneVerdict ? 'on' : ''}" data-action="gh103" role="switch" aria-checked="${amContributorLaneVerdict ? 'true' : 'false'}"></div>
                  <span class="config-toggle-label">Require approving verdict ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Require approving verdict', `When on (the default), the configured reviewer agent must have recorded an approving verdict for the current head before the contributor-lane tier merges.`)}</span>
                </div>
              </div>
              <div class="config-field">
                <label>Registry tier admits ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Registry tier admits', `When on (the default), a PR whose author holds a required tier on their contributor-registry profile is admitted without a linked issue or hive:claim marker (logged as contributor-lane:registry-trusted). Off requires the claim marker for everyone.`)}</label>
                <div class="config-toggle">
                  <div id="am-contributor-lane-registry-tiers-sufficient" class="config-toggle-switch ${amContributorLaneRegistry ? 'on' : ''}" data-action="gh103" role="switch" aria-checked="${amContributorLaneRegistry ? 'true' : 'false'}"></div>
                  <span class="config-toggle-label">Admit by registry tier ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Admit by registry tier', `When on (the default), the operator-set trust tier on the author's contributor profile satisfies the tier requirement on its own; every other gate (CI, reviewer verdict, soak, diff fences) still applies.`)}</span>
                </div>
              </div>
              <div class="config-field">
                <label>Approve held runs for registry-trusted ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Approve held runs for registry-trusted', `When on (the default), a PR admitted by registry tier that is stuck at required-check-missing has its action_required workflow runs approved once per head by the App (fork heads included), with one audit comment. Claim-marker, auto-promoted and association admissions, sentinel/excluded-label PRs and diffs touching .github/workflows are never approved. Needs the App's Actions: Read and write permission.`)}</label>
                <div class="config-toggle">
                  <div id="am-contributor-lane-approve-held-runs" class="config-toggle-switch ${amContributorLaneHeldRuns ? 'on' : ''}" data-action="gh103" role="switch" aria-checked="${amContributorLaneHeldRuns ? 'true' : 'false'}"></div>
                  <span class="config-toggle-label">Start CI held by GitHub's fork-approval policy ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Start CI held by fork-approval policy', `GitHub holds a first-time contributor's pull_request runs until someone clicks Approve and run. If the operator already trusts the author to merge via the registry, holding their CI is pure drag; the hive approves the head's held runs once and logs contributor-lane approved held runs.`)}</span>
                </div>
              </div>
              <div class="config-field">
                <label>Owner approval releases the workflow-path fence ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Owner approval releases the workflow-path fence', `When on (the default), a trusted merger's GitHub Approve review on the current head — or a dashboard /hive decision approve bound to it — lets the contributor lane merge a PR that edits .github/workflows/** or .github/actions/** itself once CI, soak and the other gates are clear. Approval on an earlier head does not count. Needs the App's Workflows: Read and write; without it the merge is refused, the PR is marked hive:unauthorized and parks at 090 for a person's Merge click. Off keeps every workflow-path PR a person's merge.`)}</label>
                <div class="config-toggle">
                  <div id="am-contributor-lane-owner-approval-workflow-fence" class="config-toggle-switch ${amContributorLaneWorkflowApproval ? 'on' : ''}" data-action="gh103" role="switch" aria-checked="${amContributorLaneWorkflowApproval ? 'true' : 'false'}"></div>
                  <span class="config-toggle-label">Merge owner-approved workflow-file PRs ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Merge owner-approved workflow-file PRs', `YAML: auto_merge.contributor_lane.owner_approval_releases_workflow_fence. If you approved it, the hive merges it; a stale approval re-arms the fence on the next push.`)}</span>
                </div>
              </div>
            </div>
            <div class="config-field mt-4">
              <label for="am-contributor-lane-auto-promotion-scope">Auto-promotion scope ${settingHelpMark('hive-merge', 'contributor-lane-auto-merge', 'Auto-promotion scope', `Where the clean-merged-PR ratchet (auto_merge.contributor_tier.auto_promote_after_merged) counts an author's merges. hive (the default) counts clean merged PRs across every repository this hive manages, so a contributor trusted in the main repo is trusted in the small satellite repos too; repo counts only the PR's own repository. Revert and sentinel-alert evidence blocks promotion in either scope.`)}</label>
              <select id="am-contributor-lane-auto-promotion-scope" data-change-action="gh104">${amContributorLaneScopeOptions}</select>
              <p class="am-trusted-authors-note">A contributor's trust is a property of the person, not the repository: with <code>hive</code>, nine clean merges in the main repo also admit their first PR to a docs repo.</p>
            </div>
            <div class="config-field mt-4">
              <label for="am-contributor-lane-exclude-labels">Excluded labels</label>
              <input id="am-contributor-lane-exclude-labels" type="text" value="${esc(amContributorLaneLabels.join(', '))}" placeholder="hold, do-not-merge, needs-human, blocked, needs-rebase" data-change-action="gh104">
              <p class="am-trusted-authors-note">Any listed label keeps the PR out of this tier; defaults are shown when unset.</p>
            </div>
            <div class="config-field mt-4">
              <label for="am-contributor-lane-repos">Repositories</label>
              <select id="am-contributor-lane-repos" multiple size="${Math.min(Math.max(watchedRepos.length || 3, 3), 8)}" data-change-action="gh104" class="am-trusted-authors-repos">${amContributorLaneRepoOptions}</select>
              <p class="am-trusted-authors-note">Leave every repo unselected to allow all watched repositories. Skips are logged as <code>contributor-lane:*</code> reasons (no-claim, no-linked-issue, claim-author-mismatch, claim-other-hive, claim-tier-untrusted, claim-unverified, author-association, workflow-path, tests-removed, no-reviewer, no-verdict, soaking, sentinel-blocked); a registry-admitted PR logs <code>contributor-lane:registry-trusted</code>.</p>
              <p class="config-help">YAML keys: <code>auto_merge.contributor_lane.enabled</code>, <code>auto_merge.contributor_lane.soak_minutes</code>, <code>auto_merge.contributor_lane.require_reviewer_verdict</code>, <code>auto_merge.contributor_lane.registry_tiers_sufficient</code>, <code>auto_merge.contributor_lane.approve_held_runs_for_registry_trusted</code>, <code>auto_merge.contributor_lane.owner_approval_releases_workflow_fence</code>, <code>auto_merge.contributor_lane.auto_promotion_scope</code>, <code>auto_merge.contributor_lane.repos[]</code>, <code>auto_merge.contributor_lane.exclude_labels[]</code>, <code>auto_merge.contributor_lane.require_tiers[]</code>.</p>
            </div>
          </div>
        </div>
        `)}

        ${featuresSectionHtml('review', `
        <div class="settings-subsection">
          <div class="settings-subsection-title">Review Gate ${settingHelpMark('review-swarm', 'configuration', 'Review Gate', `The review swarm gate runs designated reviewer agents on each PR and blocks merge until they approve. Used at ACMM L4+.`)}</div>
          <div class="config-subgroup">Merge queue</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${rv.require_approval ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="require_approval"></div>
            <span class="config-toggle-label">Require review-swarm approval before merge ${settingHelpMark('review-swarm', 'configuration', 'Require review-swarm approval before merge', `When on, a PR is not merge-eligible until the reviewer agents record an aggregate approval. Off by default, which preserves the existing behaviour of not consulting review verdicts at all. Typically used at ACMM L4 and above.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${rv.shadow ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="shadow"></div>
            <span class="config-toggle-label">Shadow-evaluate the review gate (record only) ${settingHelpMark('review-swarm', 'configuration', 'Shadow-evaluate the review gate', `Evaluates the review-approval gate without enforcing it and records whether it would have held each merge-eligible PR on the PR&apos;s evidence bundle. Never blocks a merge, and never weakens Require review-swarm approval when that is on. Off by default.`)}</span>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${rv.fan_out ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="fan_out"></div>
            <span class="config-toggle-label">Fan out to multiple reviewers ${yamlHint('review.fan_out')} ${settingHelpMark('review-swarm', 'reviewer-selection', 'Fan out to multiple reviewers', `Send each perspective to every eligible reviewer so PRs get independent reviews from different agents/models, running concurrently and bounded by Max parallel reviews. This is the switch that makes multi-model review happen: pick the reviewers under Reviewers below. Off, one reviewer takes each perspective.`)}</span>
          </div>
          <p class="config-help rv-reviewer-help">Send each perspective to every eligible reviewer so PRs get independent reviews from different agents/models.</p>
          <div class="config-field" style="margin-top:10px">
            <label>Max parallel reviews ${yamlHint('review.max_parallel_reviews')} ${settingHelpMark('review-swarm', 'configuration', 'Max parallel reviews', `Caps how many reviewer agents run concurrently when fan-out is on. 0 or empty uses the default.`)}</label>
            <input type="number" min="0" step="1" value="${rv.max_parallel_reviews || ''}" placeholder="3" data-change-action="gh73">
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Max perspectives per PR ${yamlHint('review.max_perspectives_per_pr')} ${settingHelpMark('review-swarm', 'configuration', 'Max perspectives per PR', `Caps how many review perspectives ONE PR may be given in a single cycle. Parallel review slots are a fixed budget spent in PR order, so without a cap the first PR in a deep queue absorbs every slot for its own perspectives &mdash; more opinions on one PR instead of coverage across many. Capping spends the same budget breadth-first. No coverage is lost: perspectives skipped this cycle are still missing next cycle and get dispatched then, so this schedules depth rather than dropping it. It also bounds how many comments one PR can collect at once, which matters once reviewers publish. 0 or empty means no cap (the default).`)}</label>
            <input type="number" min="0" step="1" value="${rv.max_perspectives_per_pr || ''}" placeholder="0 — no cap" data-change-action="gh81">
          </div>
          <div class="config-subgroup">Perspectives</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${rv.combined_perspectives ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="combined_perspectives"></div>
            <span class="config-toggle-label">Review every perspective in one session, one comment ${yamlHint('review.combined_perspectives')} ${settingHelpMark('review-swarm', 'configuration', 'Review every perspective in one session, one comment', `Off: each perspective is its own agent session and its own review comment, and &quot;Max perspectives per PR&quot; caps how many a PR gets &mdash; which is how a fleet ends up reviewing only correctness. On: ONE session judges every enabled perspective and leaves ONE comment, with findings grouped under the perspective they belong to. The diff and surrounding tree are read once instead of once per perspective. Each perspective still records its own verdict, so any one of them can still hold a PR for a human. The per-PR cap does not apply in this mode: it exists to bound comments, and a combined review is always exactly one.`)}</span>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${f.planMatchEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="planMatchEnabled"></div>
            <span class="config-toggle-label">Match PRs against their approved plan (plan_match) ${settingHelpMark('review-swarm', 'configuration', 'Match PRs against their approved plan (plan_match)', `OFF by default. Adds the built-in plan_match perspective, which reads the approved plan wave named by a PR&apos;s Hive-Run / Hive-Plan trailers and reports scope creep (changes no planned item covers) and planned items the diff leaves out. A high-severity scope finding holds the PR for a human like any other blocking finding. PRs without a run trailer get a not-applicable verdict that neither raises nor lowers their confidence score. Needs run trailers on implementation PRs and plans in this hive&apos;s bead stores; otherwise it costs a perspective for nothing.`)}</span>
          </div>
          <div id="rv-perspectives" style="margin-top:10px">
            <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-2)">Which questions the reviewer asks of every PR, and what each one looks for. Edit the text to tell this hive&apos;s reviewer what matters in <em>your</em> repos &mdash; e.g. name the conventions doc for <code>style</code>. Blank means the built-in. Unchecking a perspective stops it running and stops its verdicts being accepted.</p>
            ${rvBuiltin.map(b => rvPerspectiveRow(b.name, b.focus)).join('')}
            ${rvCustomNames.map(n => rvPerspectiveRow(n, '')).join('')}
            <div style="display:flex;gap:var(--sp-3);margin-top:var(--sp-4);align-items:center">
              <input type="text" id="rv-new-perspective" placeholder="new perspective, e.g. api-compatibility" style="flex:1;font-size:0.76rem" pattern="[a-z][a-z0-9]*(-[a-z0-9]+)*">
              <button type="button" class="hv-btn btn-secondary btn-sm btn-small" data-action="gh95">Add perspective</button>
            </div>
            <p style="font-size:var(--fs-xs);color:var(--muted);margin:var(--sp-2) var(--sp-0) var(--sp-0)">A new perspective needs focus text before it can be saved. Names: lowercase letters, digits, single dashes.</p>
          </div>
          <div class="config-subgroup">Reviewers</div>
          <div class="config-field" id="rv-reviewer-agents">
            <label>Reviewer agents ${yamlHint('review.reviewer_agents')} ${settingHelpMark('review-swarm', 'reviewer-selection', 'Reviewer agents', `The agents that review each PR. Empty means any enabled agent whose name, role, aliases or keywords contain &quot;review&quot;. Ticking one or more agents makes this an explicit allow-list and the keyword scan is skipped. Disabled, paused and on-demand agents never review even when ticked. Each agent shows its engine and model: pick agents on different engines for multi-model review.`)}</label>
            <p class="config-help rv-reviewer-help-above">Empty = any enabled agent whose name, role or keywords contain &quot;review&quot;. Pick agents on different engines for multi-model review.</p>
            <div class="rv-reviewer-list">
              ${rvCandidates.length || rvUnknownAgents.length ? rvCandidates.map(rvCandidateRow).join('') + rvUnknownAgents.map(rvUnknownRow).join('') : '<span class="rv-reviewer-empty">No agents are configured on this hive yet.</span>'}
            </div>
            <p class="am-trusted-authors-note" id="rv-reviewer-summary"></p>
            <p class="am-trusted-authors-note" id="rv-reviewer-engine-warn" hidden>⚠️ Only one engine will review &mdash; add a reviewer on a different engine for independent review.</p>
            <p class="am-trusted-authors-note" id="rv-reviewer-combined-note" hidden>Combined mode (&quot;Review every perspective in one session&quot;) sends the whole review to the <em>first</em> eligible reviewer only, so fan-out does not give you a second opinion while it is on. See <a href="https://github.com/hivecommons/hive/blob/v5/src/docs/review-swarm.md#reviewer-selection" target="_blank" rel="noopener noreferrer">Reviewer selection</a>.</p>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${rv.all_authors ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="all_authors"></div>
            <span class="config-toggle-label">Review every PR, not just agent-authored (deprecated &mdash; always on) ${yamlHint('review.all_authors')} ${settingHelpMark('review-swarm', 'configuration', 'Review every PR, not just agent-authored', `Deprecated and no longer does anything. The review swarm reviews EVERY open PR regardless of who opened it &mdash; hive agents, humans, clankers, dependabot and other bots &mdash; whenever PR review is on (require_approval and fan_out). The only switch is the review feature itself; there is no per-author opt-out. The toggle is kept so existing configs still load and show their stored value; turn it off and remove review.all_authors from hive.yaml. Whether Hive may also push fix commits to someone else's PR is the next toggle, and the answer there is still no.`)}</span>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${rv.fix_human_prs ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="fix_human_prs"></div>
            <span class="config-toggle-label">Push fix commits to PRs Hive did not open ${yamlHint('review.fix_human_prs')} ${settingHelpMark('review-swarm', 'configuration', 'Push fix commits to PRs Hive did not open', `OFF by default. Whichever way this is set, Hive no longer pushes review fixes onto a PR opened by a person, a maintainer, or another bot: the commit would be signed off by Hive rather than the PR author, and the &quot;Sign-off survives the squash&quot; check rejects it as bot-signoff, a failure only the author can clear. The reviewer posts its findings with the proposed fix as a suggestion or patch block in the comment, and the author decides what to change; each withheld push is recorded in the audit log. On, Hive also answers review-bot threads on those PRs, again with suggestions rather than commits. Hive's own agents' PRs are fixed either way. &quot;Review every PR&quot; never turns this on by itself. Owner only.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${rv.contributor_prs && rv.contributor_prs.base_sync ? 'on' : ''}" data-action="toggleContributorPRBaseSync"></div>
            <span class="config-toggle-label">Sync lane-authored contributor fork PRs with base ${settingHelpMark('review-swarm', 'configuration', 'Sync lane-authored contributor fork PRs with base', `OFF by default. Owner gate for review.contributor_prs.base_sync. When a Hive-authored lane PR lives on a contributor fork, is behind or dirty, and has &quot;allow edits by maintainers&quot; enabled, Hive may call GitHub&apos;s update-branch API. Clean BEHIND PRs get a GitHub-authored merge commit; DIRTY PRs fall back to the base-moved repair note and kick. Requires the agent mode that permits update-branch (ISSUES_PRS_MERGE).`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${am.require_current_base_default ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="auto-merge" data-key="require_current_base_default"></div>
            <span class="config-toggle-label">Merge only on current base ${yamlHint('auto_merge.require_current_base_default')} ${settingHelpMark('operator-reference', 'app-self-merge-sweep-auto_merge', 'Merge only on current base', `OFF by default. Before auto-merging in any lane, require the PR head to contain the current base tip; a green head tested on an older base is updated with GitHub update-branch and merges once CI is green on the new head. Costs one compare call per merge candidate. Per-repo overrides live on the repo card.`)}</span>
          </div>
          <div class="config-field">
            <label>Native GitHub merge queue ${yamlHint('auto_merge.native_merge_queue')} ${settingHelpMark('native-merge-queue', 'opting-in', 'Native GitHub merge queue', `Default off. Hive controls admission and repair; GitHub tests and merges the synthetic group. Settings requires explicit repositories and exact repo#base pairs. Existing CI, trust, review, hold and attribution gates stay in force.`)}</label>
            <div class="config-toggle">
              <div class="config-toggle-switch ${am.native_merge_queue && am.native_merge_queue.enabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-keydown-action="toggleConfigSwitch" data-keys="Enter, " data-arg-types="t,s,s" data-arg1="native-queue" data-arg2="enabled" data-prevent="1" data-section="native-queue" data-key="enabled" role="switch" tabindex="0" aria-label="Enable native GitHub merge queue" aria-checked="${am.native_merge_queue && am.native_merge_queue.enabled ? 'true' : 'false'}"></div>
              <span class="config-toggle-label">Enable native GitHub merge queue (default OFF) ${settingHelpMark('native-merge-queue', 'pilot-rollout', 'Enable native GitHub merge queue', `Operator opt-in only after the GitHub prerequisites below. Save applies the Hive hand-off policy; it does not create queues or change branch protection. Off preserves conventional direct merge on branches without a required queue; a queue-required branch remains parked, never bypassed.`)}</span>
            </div>
            <label>Native queue repositories ${settingHelpMark('native-merge-queue', 'opting-in', 'Native queue repositories', `Comma-separated owner/repo allow-list. Settings refuses enabled queues with an empty list. Start with hivecommons/hive only; other repositories retain direct mode.`)}</label>
            <input type="text" value="${esc(((am.native_merge_queue || {}).repos || []).join(', '))}" placeholder="hivecommons/hive" data-change-action="markDirtyNativeQueue" data-arg0="repos" data-arg-types="s,w">
            <label>Native queue bases ${yamlHint('auto_merge.native_merge_queue.bases[]')} ${settingHelpMark('native-merge-queue', 'opting-in', 'Native queue bases', `Comma-separated exact owner/repo#base pairs. Repository names ignore case; branch names preserve case. No wildcards. Start with hivecommons/hive#v5. Every pair must belong to a listed repository.`)}</label>
            <input type="text" value="${esc(((am.native_merge_queue || {}).bases || []).join(', '))}" placeholder="hivecommons/hive#v5" data-change-action="markDirtyNativeQueue" data-arg0="bases" data-arg-types="s,w">
            <div class="config-toggle">
              <div class="config-toggle-switch ${am.native_merge_queue && am.native_merge_queue.skip_current_base_gate ? 'on' : ''}" data-action="toggleConfigSwitch" data-keydown-action="toggleConfigSwitch" data-keys="Enter, " data-arg-types="t,s,s" data-arg1="native-queue" data-arg2="skip_current_base_gate" data-prevent="1" data-section="native-queue" data-key="skip_current_base_gate" role="switch" tabindex="0" aria-label="Native queue owns freshness" aria-checked="${am.native_merge_queue && am.native_merge_queue.skip_current_base_gate ? 'true' : 'false'}"></div>
              <span class="config-toggle-label">Queue owns freshness (default OFF) ${settingHelpMark('native-merge-queue', 'current-base-gate', 'Queue owns freshness', `Explicitly bypasses current-base and behind refresh only for opted-in repo/base pairs with an actual GitHub queue. Leave off until merge-group CI is proven and HIVE_NATIVE_QUEUE_FRESHNESS suppresses the external branch updater for the same pairs. Other branches retain coordinated refresh.`)}</span>
            </div>
            <label>Native queue probe TTL ${settingHelpMark('native-merge-queue', 'api-cost', 'Native queue probe TTL', `How long GitHub queue detection is cached per repo/base. Default 30m; 0 uses that default. Enter a non-negative Go duration, for example 30m.`)}</label>
            <input type="text" value="${esc((am.native_merge_queue || {}).probe_ttl || '30m0s')}" data-change-action="markDirtyNativeQueue" data-arg0="probe_ttl" data-arg-types="s,w">
            <p class="config-help">Configured: <strong>${am.native_merge_queue && am.native_merge_queue.enabled ? 'enabled' : 'disabled (default)'}</strong>.
              Repositories: ${esc(((am.native_merge_queue || {}).repos || []).join(', ') || 'all watched repositories once enabled')}.
              Bases: ${esc(((am.native_merge_queue || {}).bases || []).join(', ') || 'all queue bases (legacy YAML only)')}.
              Queue owns freshness: ${am.native_merge_queue && am.native_merge_queue.skip_current_base_gate ? 'explicitly opted in' : 'no (default)'}.
              Probe TTL: ${esc((am.native_merge_queue || {}).probe_ttl || '30m0s')}.</p>
            <p class="config-help">Owner-only: stage these controls and click Save only after the
              <a href="https://github.com/hivecommons/hive/blob/v5/src/docs/native-merge-queue.md#pilot-rollout" target="_blank" rel="noopener noreferrer">checks and branch-rule rollout</a>.
              Prerequisites: App installation authentication, withdrawal guards, trusted-base policy proof, equivalent required-check migration, MERGE method and a bounded pilot group.
              Enabled here does not mean deployed or enabled in GitHub. Turning it off does not disable GitHub's queue or withdraw existing entries; drain those before rollback. Direct merge with coordinated refresh remains available; this is not Hive's visual train.
              Integration-aware multi-agent orchestration is planned, not a selectable implementation.</p>
          </div>
          <div class="config-toggle mt-4">
            <div class="config-toggle-switch ${rv.post_comments ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="post_comments"></div>
            <span class="config-toggle-label">Post review comments ${yamlHint('review.post_comments')} ${settingHelpMark('review-swarm', 'configuration', 'Post review comments', `OFF by default. Reviewers publish their verdict as a PR comment through the hive-review relay, in addition to returning it to the merge queue. Off, the verdict stays internal, which on a hive without auto-merge means nobody reads it. Turn this on so a review reaches the human who has to decide.`)}</span>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${rv.acknowledge_no_findings ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="acknowledge_no_findings"></div>
            <span class="config-toggle-label">Comment even when there are no findings ${yamlHint('review.acknowledge_no_findings')} ${settingHelpMark('review-swarm', 'configuration', 'Comment even when there are no findings', `A reviewer with nothing useful to say stays silent by default, which keeps PRs uncluttered but is indistinguishable from a reviewer that never ran. Turn this on to have a clean review leave one short line recording that it reviewed and found nothing, so review coverage is visible on the PR itself. The acknowledgement is capped at a single line &mdash; it is evidence, not a report. Requires &quot;post review comments&quot;.`)}</span>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${rv.confidence_score ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="confidence_score"></div>
            <span class="config-toggle-label">Show a mergeability confidence score ${yamlHint('review.confidence_score')} ${settingHelpMark('review-swarm', 'configuration', 'Show a mergeability confidence score', `Appends one line to each review comment: <b>Confidence: 4/5</b> (safe) &mdash; 1 medium finding. The score is derived from the verdicts and finding severities the perspectives reported, never asked of the model, so it means the same thing on every PR: 4&ndash;5 safe, 1&ndash;3 needs attention, 0 do not merge. Always recorded in the verdict artifact; this only controls whether the comment shows it.`)}</span>
          </div>
          <div class="config-toggle" style="margin-top:10px">
            <div class="config-toggle-switch ${rv.priority_labels ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="priority_labels"></div>
            <span class="config-toggle-label">Label PRs with their review-queue priority ${yamlHint('review.priority_labels')} ${settingHelpMark('review-swarm', 'configuration', 'Label PRs with their review-queue priority', `OFF by default. Mirrors each open PR's place in the review queue (GET /api/review/queue) onto exactly one of review-priority/high, review-priority/normal or review-priority/low - high for fixes, normal for refactors, docs and unclassified PRs, low for test-only PRs - so the order shows up in plain GitHub searches. Applied by the hive from the computed rank, never by the PR's author, on contributor PRs too. The labels are never created: create them in each repo first; a repo without them is skipped.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${(rv.require_labels && rv.require_labels.length) ? 'on' : ''}" data-action="toggleReviewRequireLabels"></div>
            <span class="config-toggle-label">Only review labelled PRs ${yamlHint('review.require_labels[]')} ${settingHelpMark('review-swarm', 'configuration', 'Only review labelled PRs', `OFF by default: every open PR is reviewable. ON: the hive reviews a PR only when it carries at least one of the labels below (case-insensitive exact match), the PR-side counterpart of the issue approval gate (project.issue_filter.require_labels). Enforced server-side: unlabelled PRs never appear in a reviewer kick or the review dispatch, and the hive-review relay refuses to post on one, so a prompt cannot bypass it. The labels are never created: use a label the repo already maintains. Exempt and hold labels still exclude a PR that also carries an approval label.`)}</span>
          </div>
          <div class="config-field" id="rv-require-labels-field" ${(rv.require_labels && rv.require_labels.length) ? '' : 'hidden'}>
            <label>Review approval labels (comma-separated) ${settingHelpMark('review-swarm', 'configuration', 'Review approval labels', `The labels that admit a PR to review when "Only review labelled PRs" is on, comma-separated; a PR needs at least one of them (case-insensitive exact match). Saved as review.require_labels; clearing the list or switching the toggle off disables the gate.`)}</label>
            <input type="text" id="rv-require-labels" value="${esc((rv.require_labels || []).join(', '))}" placeholder="e.g. hive-review" spellcheck="false" data-change-action="markDirtyReviewRequireLabels" data-arg-types="w">
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Fixer agent ${yamlHint('review.fixer_agent')} ${settingHelpMark('review-swarm', 'configuration', 'Fixer agent', `The agent kicked to address reviewer findings before re-review. Use the agent ID (the key in the roster, not its display name). Blank falls back to the PR's authoring agent, then scanner.`)}</label>
            <input type="text" value="${esc(rv.fixer_agent || '')}" placeholder="scanner (default)" data-change-action="markDirty" data-arg0="review" data-arg1="fixer_agent" data-arg-types="s,s,w">
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Human decision label ${yamlHint('review.human_decision_label')} ${settingHelpMark('review-swarm', 'configuration', 'Human decision label', `An EXISTING repo label applied when the review swarm holds a PR for a human, so those PRs can be filtered from the list. There is no fleet-wide default and no standard name &mdash; use whatever the governed repos already maintain (this fleet's bluefin spoke uses 3-human-queue). The label is never created: if it is blank, misspelled, or absent from a repo, labeling is skipped there and the review comment's in-body marker still carries the signal.`)}</label>
            <input type="text" value="${esc(rv.human_decision_label || '')}" placeholder="(none) — must already exist in the repo" data-change-action="markDirty" data-arg0="review" data-arg1="human_decision_label" data-arg-types="s,s,w">
          </div>
          <div class="config-field">
            <label>Hive-authored decision questions ${yamlHint('review.self_authored_decision_policy')} ${settingHelpMark('review-swarm', 'configuration', 'Hive-authored decision questions', `resolve (default): decision questions on Hive-authored PRs are treated as advisory and the aggregate can approve, unless they match a risk class (permissions/credentials, security, sensitive paths, data loss/irreversible, merge/trust policy, external side effects, licensing/legal, unverifiable) or carry the hive-decision-risk: high marker, which always escalate. escalate: every decision marker parks the PR for a person.`)}</label>
            <select data-change-action="markDirty" data-arg0="review" data-arg1="self_authored_decision_policy" data-arg-types="s,s,v">${['resolve', 'escalate'].map(v => `<option value="${v}"${((rv.self_authored_decision_policy || 'resolve') === v) ? ' selected' : ''}>${v}</option>`).join('')}</select>
          </div>
          <div class="config-field-row mt-4">
            <div class="config-field"><label>Max reviews per head ${yamlHint('review.max_reviews_per_head')} ${settingHelpMark('review-swarm', 'configuration', 'Max reviews per head', `Caps how many top-level hive reviews one PR head may receive before the relay refuses more (revisions and thread replies excepted). 0 or empty derives the cap: 1 with combined perspectives, one per perspective otherwise. A negative value disables the backstop.`)}</label><input type="number" step="1" value="${esc(String(rv.max_reviews_per_head || ''))}" placeholder="0 — derived" data-change-action="markDirty" data-arg0="review" data-arg1="max_reviews_per_head" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Re-review verdicts before ${yamlHint('review.revise_verdicts_before')} ${settingHelpMark('review-swarm', 'configuration', 'Re-review verdicts before', `RFC 3339 instant, e.g. 2026-09-19T14:00:00Z. PRs in the revise repos whose verdict was recorded before it are reviewed once more even though the head has not moved, for when the reviewer itself was wrong. Each PR is revisited at most once per bump. The revise repo list (review.revise_repos) is yaml-only. Empty clears it.`)}</label><input type="text" value="${esc(rv.revise_verdicts_before || '')}" placeholder="not set" spellcheck="false" data-change-action="markDirty" data-arg0="review" data-arg1="revise_verdicts_before" data-arg-types="s,s,w"></div>
          </div>
          ${renderReviewerAutoscale(rv)}
          <div class="config-subgroup">Dispatch</div>
          <div class="config-field-row">
            <div class="config-field"><label>Webhook-triggered dispatch ${yamlHint('review.event_driven')} ${settingHelpMark('review-swarm', 'configuration', 'Webhook-triggered dispatch', `Lets pull_request webhooks start a review early instead of waiting for the next governor cycle. auto (the default) is on exactly when this hive receives signed GitHub webhooks. Always off while the review swarm itself is off (require approval and fan-out).`)}</label><select data-change-action="markDirty" data-arg0="review" data-arg1="event_driven_mode" data-arg-types="s,s,v">${['auto', 'on', 'off'].map(v => `<option value="${v}"${(rv.event_driven === true ? 'on' : rv.event_driven === false ? 'off' : 'auto') === v ? ' selected' : ''}>${v}</option>`).join('')}</select></div>
            <div class="config-field"><label>Webhook debounce (s) ${yamlHint('review.event_debounce_s')} ${settingHelpMark('review-swarm', 'configuration', 'Webhook debounce', `Per-PR quiet period before a webhook-triggered review fires, so a burst of pushes yields one review (default 90 seconds, at most 3600). 0 uses the default.`)}</label><input type="number" min="0" max="3600" step="1" value="${esc(String(rv.event_debounce_s || ''))}" placeholder="90" data-change-action="markDirty" data-arg0="review" data-arg1="event_debounce_s" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Pending review TTL ${yamlHint('review.pending_ttl')} ${settingHelpMark('review-swarm', 'configuration', 'Pending review TTL', `How long a dispatched review may go without a verdict before it expires and its perspectives are asked again (default 45m). A reviewer turn that dies posts no verdict; without expiry the PR would look covered until the next push. Go duration; empty or 0 uses the default and a negative value disables expiry.`)}</label><input type="text" value="${esc(rv.pending_ttl || '')}" placeholder="45m0s" spellcheck="false" data-change-action="markDirty" data-arg0="review" data-arg1="pending_ttl" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Max re-dispatches ${yamlHint('review.max_redispatch')} ${settingHelpMark('review-swarm', 'configuration', 'Max re-dispatches', `How many times one PR head's pending reviews may expire before the planner stops re-kicking and holds the PR for a human (default 3). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(rv.max_redispatch || ''))}" placeholder="3" data-change-action="markDirty" data-arg0="review" data-arg1="max_redispatch" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Max head retries ${yamlHint('review.head_max_retries')} ${settingHelpMark('review-swarm', 'configuration', 'Max head retries', `With agents.<name>.review_heads, how many times one head's review of a perspective may be re-sent when it posts no verdict. 0 uses Max re-dispatches.`)}</label><input type="number" min="0" step="1" value="${esc(String(rv.head_max_retries || ''))}" placeholder="0" data-change-action="markDirty" data-arg0="review" data-arg1="head_max_retries" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-subgroup">Out-of-scope findings</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${rv.out_of_scope_backlog_disabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="review" data-key="out_of_scope_backlog_disabled"></div>
            <span class="config-toggle-label">Do not file out-of-scope findings as issues ${yamlHint('review.out_of_scope_backlog_disabled')} ${settingHelpMark('review-swarm', 'configuration', 'Do not file out-of-scope findings as issues', `OFF by default, so cited findings outside a PR's scope are filed as follow-up issues and the review can stay narrow without losing real adjacent defects. Turn this on to opt out.`)}</span>
          </div>
          <div class="config-field mt-4">
            <label>Max out-of-scope issues per review ${yamlHint('review.max_out_of_scope_backlog_issues')} ${settingHelpMark('review-swarm', 'configuration', 'Max out-of-scope issues per review', `Caps how many follow-up issues one PR review may file (default 3). 0 or empty uses the default.`)}</label>
            <input type="number" min="0" step="1" value="${esc(String(rv.max_out_of_scope_backlog_issues || ''))}" placeholder="3" data-change-action="markDirty" data-arg0="review" data-arg1="max_out_of_scope_backlog_issues" data-arg-types="s,s,N">
          </div>
          ${renderReviewSeverityBacklog(rv)}
          <div class="config-subgroup">External review bots ${settingHelpMark('review-bot-threads', '', 'External review bots', `Effective classification.review_bots settings from hive.yaml, falling back to hive-project.yaml. Listed logins grant agents the right to address and resolve those accounts&apos; threads; changing this trust boundary requires a config-file edit. This is separate from the review swarm gate.`)}</div>
          <div id="review-bots-settings" class="fs-sm">
            ${!rb ? '<p>Review-bot settings unavailable from this server.</p>' : rb.load_error ? `<p>Unable to load review-bot settings: ${esc(rb.load_error)}</p>` : `
              <p>Status: ${rb.enabled ? 'Enabled' : 'Disabled (no bot logins configured)'}</p>
              <p>Bot logins (read-only): ${rbLogins.length ? rbLogins.map(login => `<code>${esc(login)}</code>`).join(', ') : 'None'}</p>
              <p>Max attempts per thread: ${esc(String(rb.max_attempts_per_thread))}</p>
              <p>Resolve after fix: ${rb.resolve_after_fix ? 'Yes' : 'No — leave open for a human'}</p>
            `}
          </div>
          ${rb && !rb.load_error ? `<div class="config-field mt-3" id="review-bots-min-priority-field">
            <label>Minimum bot-finding priority ${settingHelpMark('review-bot-threads', '', 'Minimum bot-finding priority', `Which review-bot threads are routed to an agent. With P1, threads whose first comment carries a P0 or P1 badge (e.g. Codex&apos;s ![P1 Badge]) are routed to the agent that opened the PR; P2/P3 threads are left open for a human and are not auto-resolved. P0 is most urgent. Threads in an unknown or badge-less format (Copilot, CodeRabbit) are always routed, so a new bot cannot silently hide findings. All routes every thread. Saved to hive.yaml&apos;s classification.review_bots.min_priority, which overrides hive-project.yaml&apos;s threshold without touching its logins.`)}</label>
            <select id="review-bots-min-priority" data-change-action="markDirtyReviewBotsMinPriority" data-arg-types="v">
              ${['all', 'P0', 'P1', 'P2', 'P3'].map(p => `<option value="${p}"${(rb.min_priority || 'all') === p ? ' selected' : ''}>${p === 'all' ? 'All findings' : p + (p === 'P0' ? ' only' : ' and above')}</option>`).join('')}
            </select>
          </div>` : ''}
          <div class="config-subgroup" style="margin-top:14px">Effectiveness ${settingHelpMark('review-swarm', 'measuring-effectiveness', 'Effectiveness', `Does reviewing PRs actually shrink the queue? Every eval cycle the hive records each open PR it sees and, when it later leaves the queue, whether it merged or closed. PRs the hive reviewed before their outcome are compared against the ones it did not (the control group) on merge rate and median time to merge. Verdict counts alone cannot answer this; the comparison can.`)}</div>
          <div class="config-field">
            <button class="hv-btn btn-secondary btn" data-action="loadReviewOutcomes">Load 30-day outcomes</button>
            <div id="review-outcomes-summary" style="font-size:0.72rem;color:var(--muted);margin-top:var(--sp-3)"></div>
          </div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Merge Recommendations ${settingHelpMark('settings', 'features', 'Merge Recommendations', `A digest of what is worth merging next, written into one issue per repository and rewritten in place each cycle. It is separate from the review gate above: the gate decides whether a PR may merge, while this only reports what already can. It never merges, approves, closes or labels anything.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${rvRec.enabled ? 'on' : ''}" id="rec-enabled-switch" data-action="gh82"></div>
            <span class="config-toggle-label">Open a &quot;what to merge next&quot; issue ${yamlHint('review.recommendations.enabled')} ${settingHelpMark('settings', 'features', 'Open a "what to merge next" issue', `For a queue nobody can read top to bottom. The hive reads every open PR it already tracks, works out which ones are genuinely mergeable right now, and writes the answer into ONE issue per repository titled &quot;🐝 Hive recommendations&quot;. That issue is rewritten in place on each cycle, so it stays current without ever sending a second notification. It recommends only &mdash; nothing is merged, approved, closed or labelled, and PRs a maintainer put on hold are left out entirely. Off by default: opening an issue in someone&apos;s repository is not something to do uninvited.`)}</span>
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Recommendations repos ${settingHelpMark('settings', 'features', 'Recommendations repos', `Comma-separated owner/repo names that get an issue, e.g. projectbluefin/common. Leave blank for every watched repository with open PRs &mdash; start with one repo to see what the issue looks like before widening it.`)}</label>
            <input type="text" id="rec-repos-input" value="${esc(rvRecRepos.join(', '))}" placeholder="(blank) — every watched repo" data-change-action="gh83">
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Min mergeable PRs to open ${yamlHint('review.recommendations.min_ready_to_open')} ${settingHelpMark('settings', 'features', 'Min mergeable PRs to open', `How many ready-to-merge PRs must exist before the issue is opened the FIRST time &mdash; an issue that opens saying &quot;nothing is ready&quot; is noise. Once it exists it is kept current regardless, including when the answer becomes &quot;nothing right now&quot;, which is itself worth being able to read. 0 or empty uses the default of 1.`)}</label>
            <input type="number" min="0" step="1" id="rec-minready-input" value="${rvRec.min_ready_to_open || ''}" placeholder="1" data-change-action="gh84">
          </div>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Review evidence ${settingHelpMark('settings', 'features', 'Review evidence', `Evidence bundles record what each hive review saw and decided, so a merge can be audited later.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.evidenceEnabled !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="evidenceEnabled"></div>
            <span class="config-toggle-label">Write evidence bundles ${yamlHint('evidence.enabled')} ${settingHelpMark('settings', 'features', 'Write evidence bundles', `On by default. Off stops writing review evidence bundles. Applies to the next review.`)}</span>
          </div>
          <div class="config-field settings-subsection-row"><label>Signing key file ${yamlHint('evidence.signing_key_file')} ${settingHelpMark('settings', 'features', 'Evidence signing key file', `Path to an Ed25519 private key used to sign bundles. Empty means bundles are written unsigned. Only the path is shown; the key is never read by this dialog.`)}</label><input type="text" value="${esc(f.evidenceSigningKeyFile || '')}" placeholder="not set (bundles unsigned)" readonly aria-readonly="true"><div class="config-help">Read-only here: signing keys are provisioned on disk and set in hive.yaml.</div></div>
        </div>
        `)}

        ${featuresSectionHtml('autonomy', `
        <div class="qa-section">
          <div class="qa-section-title">Autonomy policy ${settingHelpMark('settings', 'features', 'Autonomy policy', `Opt-in automatic ACMM level changes driven by retro evidence (<code>autonomy.*</code> in hive.yaml). Both toggles default off; the advisor otherwise only recommends and a human applies the change.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.autonomyAutoPromote ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="autonomyAutoPromote"></div>
            <span class="config-toggle-label">Auto-promote <code>autonomy.auto_promote</code> ${settingHelpMark('settings', 'features', 'Auto-promote', `When on, the hive raises its own ACMM level once the retro evidence clears the thresholds below. Off by default.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.autonomyAutoDemote ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="autonomyAutoDemote"></div>
            <span class="config-toggle-label">Auto-demote <code>autonomy.auto_demote</code> ${settingHelpMark('settings', 'features', 'Auto-demote', `When on, the hive lowers its own ACMM level after the retro pattern selected in "Demote on". Off by default.`)}</span>
          </div>
          <div class="config-field-row">
            <div class="config-field">
              <label>Promote after (clean signals) ${settingHelpMark('settings', 'features', 'Promote after', `Consecutive qualifying retro signals required before an automatic promotion (default 3). <code>autonomy.promote_after</code>`)}</label>
              <input type="number" min="1" step="1" value="${esc(String(f.autonomyPromoteAfter || 3))}" data-change-action="markDirty" data-arg0="features" data-arg1="autonomyPromoteAfter" data-arg-types="s,s,N">
            </div>
            <div class="config-field">
              <label>Cooldown (days) ${settingHelpMark('settings', 'features', 'Autonomy cooldown', `Minimum days between automatic level changes (default 7). <code>autonomy.cooldown_days</code>`)}</label>
              <input type="number" min="1" step="1" value="${esc(String(f.autonomyCooldownDays || 7))}" data-change-action="markDirty" data-arg0="features" data-arg1="autonomyCooldownDays" data-arg-types="s,s,N">
            </div>
            <div class="config-field">
              <label>Max level ${settingHelpMark('settings', 'features', 'Autonomy max level', `Highest ACMM level automatic promotion may reach (1-6, default 6; capped by the hive ceiling). <code>autonomy.max_level</code>`)}</label>
              <input type="number" min="1" max="6" step="1" value="${esc(String(f.autonomyMaxLevel || 6))}" data-change-action="markDirty" data-arg0="features" data-arg1="autonomyMaxLevel" data-arg-types="s,s,N">
            </div>
            <div class="config-field">
              <label>Demote on ${settingHelpMark('settings', 'features', 'Autonomy demote on', `Which retro pattern triggers an automatic demotion: a rolled-back run, a PR reworked after review, or either (default rollback). <code>autonomy.demote_on</code>`)}</label>
              <select data-change-action="markDirty" data-arg0="features" data-arg1="autonomyDemoteOn" data-arg-types="s,s,v">
                ${['rollback', 'rework', 'either'].map(v => `<option value="${v}"${(f.autonomyDemoteOn || 'rollback') === v ? ' selected' : ''}>${v}</option>`).join('')}
              </select>
            </div>
          </div>
          <div class="qa-note">Both toggles are opt-in; the ACMM advisor keeps recommending changes for a human to apply while they are off.</div>
        </div>
        <div class="settings-subsection">
          <div class="config-toggle">
            <div class="config-toggle-switch ${planOn ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="planFromLabel"></div>
            <span class="config-toggle-label">Auto-plan from label ${yamlHint('planning.plan_from_label')} ${settingHelpMark('settings', 'features', 'Auto-plan from label', `When on, an actionable issue carrying the plan/epic label auto-mints an epic and requests decomposition — no per-kick review. Opt-in for safety.`)}</span>
          </div>
          <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Auto-plan issues carrying the plan/epic label — L5+ only.</p>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Design-first planning ${settingHelpMark('settings', 'features', 'Design-first planning', `Issues with a design label get a design from the architect first; a human approves it with the approved label before it is broken down.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Design-approved label ${yamlHint('planning.design_approved_label')} ${settingHelpMark('settings', 'features', 'Design-approved label', `The label a human applies to approve a posted design. Applying a label needs triage on the repo. Empty uses ${esc(platformDefaults.planningDesignApprovedLabel || 'design-approved')}.`)}</label><input type="text" value="${esc(f.planningDesignApprovedLabel || '')}" placeholder="${esc(platformDefaults.planningDesignApprovedLabel || 'design-approved')}" data-change-action="markDirty" data-arg0="features" data-arg1="planningDesignApprovedLabel" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Max concurrent designs ${yamlHint('planning.max_concurrent_designs')} ${settingHelpMark('settings', 'features', 'Max concurrent designs', `How many designs may be posted and awaiting approval at once, so labeling a backlog does not starve the architect. 0 uses the default (${esc(String(platformDefaults.planningMaxConcurrentDesigns || 3))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.planningMaxConcurrentDesigns || ''))}" placeholder="${esc(String(platformDefaults.planningMaxConcurrentDesigns || 3))}" data-change-action="markDirty" data-arg0="features" data-arg1="planningMaxConcurrentDesigns" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Max design revisions ${yamlHint('planning.max_design_revisions')} ${settingHelpMark('settings', 'features', 'Max design revisions', `How many times the architect is asked to revise one design; past it the epic waits on a human. 0 uses the default (${esc(String(platformDefaults.planningMaxDesignRevisions || 3))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.planningMaxDesignRevisions || ''))}" placeholder="${esc(String(platformDefaults.planningMaxDesignRevisions || 3))}" data-change-action="markDirty" data-arg0="features" data-arg1="planningMaxDesignRevisions" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-help">Applies on the next planning pass. Plan and design label lists stay yaml-only.</div>
        </div>
        ${renderWritebackContainmentSection(f, platformDefaults)}
        `)}

        ${featuresSectionHtml('work-intake', `
        ${renderEventTriggersSection()}
        <div class="settings-subsection">
          <div class="settings-subsection-title">Tier classifier</div>
          <p class="config-help">Override the issue-title keywords that map work to Simple or Complex tiers. Empty lists keep the built-in defaults.</p>
          <div class="config-field-row">
            <div class="config-field"><label>Simple keywords ${yamlHint('classifier.simple_keywords[]')} ${settingHelpMark('settings', 'features', 'Simple keywords', `Title substrings that classify an issue as Tier Simple. Leave empty to keep the built-in default set.`)}</label><textarea rows="4" data-change-action="markClassifierList" data-arg0="simple_keywords" data-arg-types="s,v">${esc((cls.simple_keywords || []).join('\n'))}</textarea><div class="config-help">Effective defaults: ${esc((cls.simpleKeywords || []).join(', ') || 'none')}</div></div>
            <div class="config-field"><label>Complex signals ${yamlHint('classifier.complex_signals[]')} ${settingHelpMark('settings', 'features', 'Complex signals', `Title substrings that classify an issue as Tier Complex. Leave empty to keep the built-in default set.`)}</label><textarea rows="4" data-change-action="markClassifierList" data-arg0="complex_signals" data-arg-types="s,v">${esc((cls.complex_signals || []).join('\n'))}</textarea><div class="config-help">Effective defaults: ${esc((cls.complexSignals || []).join(', ') || 'none')}</div></div>
          </div>
        </div>
        <div class="settings-subsection">
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.claimsEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="claimsEnabled"></div>
            <span class="config-toggle-label">Issue claims ${settingHelpMark('settings', 'features', 'Issue claims', `OFF by default. When on, the hub recognises a claim on an issue itself (a hive:claim marker comment, or an assignee) and withholds the issue from contributors and kick prompts while the claim is live; claims expire on their own after the TTL (default 4h). Relay agents post a claim comment when they take a lease, at tiers that may write issue comments; below that the claim is recorded on the lease only. Covers the window before a PR exists that the PR claim ledger cannot see; see src/docs/contributor-relay.md.`)}</span>
          </div>
          ${renderClaimsTTLFields(f)}
          <div class="config-toggle mt-3">
            <div class="config-toggle-switch ${f.claimsComment !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="claimsComment"></div>
            <span class="config-toggle-label">Post claim comments ${yamlHint('governor.claims.comment')} ${settingHelpMark('settings', 'features', 'Post claim comments', `On by default when claims are enabled. Posts the claim, takeover and release comments on the GitHub issue. Off keeps claims in the ledger only.`)} <span class="text-muted">— requires restart.</span></span>
          </div>
          <div class="config-toggle mt-3">
            <div class="config-toggle-switch ${f.claimsLabel !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="claimsLabel"></div>
            <span class="config-toggle-label">Apply claim labels ${yamlHint('governor.claims.label')} ${settingHelpMark('settings', 'features', 'Apply claim labels', `On by default when claims are enabled. Adds and removes the claimed and preempted labels on the GitHub issue as claims change hands.`)} <span class="text-muted">— requires restart.</span></span>
          </div>
          <div class="config-field mt-3">
            <label>Escalate after claims ${yamlHint('governor.claims.escalate_after_claims')} ${settingHelpMark('settings', 'features', 'Escalate after claims', `How many claims one agent may make on an issue with nothing moving in between (no linked PR, referencing commit, label or assignee change, or close/reopen) before the next is refused and the issue is labelled needs-human with one summary comment. 0 uses the default; a negative number turns the gate off.`)}</label>
            <input type="number" step="1" value="${f.claimsEscalateAfterClaims ? esc(String(f.claimsEscalateAfterClaims)) : ''}" placeholder="${esc(String(f.claimsEscalateAfterDefault || 2))}" data-change-action="markDirty" data-arg0="features" data-arg1="claimsEscalateAfterClaims" data-arg-types="s,s,N">
            <div class="config-help">0 uses the default (${esc(String(f.claimsEscalateAfterDefault || 2))}); -1 turns the gate off. Requires restart.</div>
          </div>
          <div class="config-field mt-3">
            <label>Never-claim labels ${yamlHint('governor.claims.never_claim_labels')} ${settingHelpMark('settings', 'features', 'Never-claim labels', `Issues wearing any of these labels (case-insensitive, comma-separated) are never claimed by agents or people and never derive a claimed or in-progress stage: umbrella issues such as epics stay at their tracking stage while their children carry the work. Blank uses the default; "none" turns the gate off.`)}</label>
            <input type="text" value="${esc(f.claimsNeverClaimLabels || '')}" placeholder="${esc(f.claimsNeverClaimDefault || 'epic')}" data-change-action="markDirty" data-arg0="features" data-arg1="claimsNeverClaimLabels" data-arg-types="s,s,s">
            <div class="config-help">Blank uses the default (${esc(f.claimsNeverClaimDefault || 'epic')}); "none" turns the gate off. Applies immediately.</div>
          </div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Issue lifecycle ${settingHelpMark('settings', 'features', 'Issue lifecycle', `When a PR that fixes an issue merges, Hive posts a close marker and closes the fixed issue as completed itself, because GitHub only auto-closes keyword-linked PRs that target the repository default branch.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.issuesCloseOnMerge !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="issuesCloseOnMerge"></div>
            <span class="config-toggle-label">Close issues on merge ${yamlHint('issues.close_on_merge')} ${settingHelpMark('settings', 'features', 'Close issues on merge', `On by default. Closes an issue as completed once the PR that fixes it merges, even when that PR targets a non-default branch.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.issuesReporterConfirmation ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="issuesReporterConfirmation"></div>
            <span class="config-toggle-label">Require reporter confirmation ${yamlHint('issues.reporter_confirmation')} ${settingHelpMark('settings', 'features', 'Require reporter confirmation', `Off by default. Restores the legacy gate where human-filed bug issues wait for reporter verification before closing. To hold a single issue instead, add the hive: needs-confirmation marker to it.`)}</span>
          </div>
          <div class="config-field-row settings-subsection-row">
            <div class="config-field"><label>Ledger sweep interval ${yamlHint('issues.close_on_merge_interval')} ${settingHelpMark('settings', 'features', 'Close-on-merge sweep interval', `How often the close-on-merge sweep walks the whole claim ledger (default 10m). Between sweeps only claims whose PR merged since the last sweep are checked. HIVE_CLOSE_ON_MERGE_INTERVAL overrides it.`)}</label><input type="text" value="${esc(f.issuesCloseOnMergeInterval || '')}" placeholder="${esc(issuesDefaults.issuesCloseOnMergeInterval || '10m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="issuesCloseOnMergeInterval" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Backfill interval ${yamlHint('issues.close_on_merge_backfill_interval')} ${settingHelpMark('settings', 'features', 'Close-on-merge backfill interval', `How often merged PRs are backfilled for issues the incremental sweep missed (default 1h).`)}</label><input type="text" value="${esc(f.issuesCloseOnMergeBackfillInterval || '')}" placeholder="${esc(issuesDefaults.issuesCloseOnMergeBackfillInterval || '1h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="issuesCloseOnMergeBackfillInterval" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Settled recheck ${yamlHint('issues.close_on_merge_recheck')} ${settingHelpMark('settings', 'features', 'Close-on-merge recheck', `How long a claim with a settled close-on-merge verdict is skipped before it is re-verified once (default 24h). HIVE_CLOSE_ON_MERGE_RECHECK overrides it.`)}</label><input type="text" value="${esc(f.issuesCloseOnMergeRecheck || '')}" placeholder="${esc(issuesDefaults.issuesCloseOnMergeRecheck || '24h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="issuesCloseOnMergeRecheck" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-help">Go durations; empty uses the default. Applies on the next sweep.</div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Duplicate PR sweep ${settingHelpMark('settings', 'features', 'Duplicate PR sweep', `Finds open PRs that change the same files and suggests which one to keep. It never closes, labels, approves or merges anything: the result is a suggestion a human acts on.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.duplicateSweepEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="duplicateSweepEnabled"></div>
            <span class="config-toggle-label">Duplicate sweep enabled ${yamlHint('duplicate_sweep.enabled')} ${settingHelpMark('settings', 'features', 'Duplicate sweep enabled', `Turns the sweep on in report-only mode. Off by default.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.duplicateSweepPostComments ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="duplicateSweepPostComments"></div>
            <span class="config-toggle-label">Post suggestion comments ${yamlHint('duplicate_sweep.post_comments')} ${settingHelpMark('settings', 'features', 'Post suggestion comments', `Additionally authorizes the suggestion comment on the PRs. Has no effect unless the sweep itself is enabled.`)}</span>
          </div>
          <div class="config-field-row settings-subsection-row">
            <div class="config-field"><label>Max comments per pass ${yamlHint('duplicate_sweep.max_comments')} ${settingHelpMark('settings', 'features', 'Duplicate sweep max comments', `Caps comments written per pass. 0 uses the default (${esc(String(dupSweepDefaults.duplicateSweepMaxComments || 5))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.duplicateSweepMaxComments || ''))}" placeholder="${esc(String(dupSweepDefaults.duplicateSweepMaxComments || 5))}" data-change-action="markDirty" data-arg0="features" data-arg1="duplicateSweepMaxComments" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Max PRs per repo ${yamlHint('duplicate_sweep.max_prs_per_repo')} ${settingHelpMark('settings', 'features', 'Duplicate sweep max PRs per repo', `How many open PRs are fingerprinted per repo per pass, the sweep's API budget. 0 uses the default (${esc(String(dupSweepDefaults.duplicateSweepMaxPRsPerRepo || 100))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.duplicateSweepMaxPRsPerRepo || ''))}" placeholder="${esc(String(dupSweepDefaults.duplicateSweepMaxPRsPerRepo || 100))}" data-change-action="markDirty" data-arg0="features" data-arg1="duplicateSweepMaxPRsPerRepo" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-help">0 = default. Applies on the next pass. Extra bot logins stay yaml-only.</div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Good first issue curation ${settingHelpMark('settings', 'features', 'Good first issue curation', `Advertises a few open, unclaimed issues to new contributors who bring their own coding agent: hive scores the issue, applies GitHub's canonical good first issue and help wanted labels, and leaves one welcome comment linking this hive's contributor relay. Not a hold: if a hive agent starts the issue first the label comes off and the comment is marked superseded.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.goodFirstIssueEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="goodFirstIssueEnabled"></div>
            <span class="config-toggle-label">Curate good first issues ${yamlHint('good_first_issue.enabled')} ${settingHelpMark('settings', 'features', 'Curate good first issues', `On by default; turn off to opt out. Needs dashboard.public_url (or a hub-assigned dashboard URL) so the welcome comment can link this hive's relay page; without one the sweep logs gfi skip reason=no-public-url and does nothing. Not a hold: a hive agent starting a labelled issue removes the label and marks the comment superseded.`)}</span>
          </div>
          <div class="config-field-row settings-subsection-row">
            <div class="config-field"><label>Max labelled per repo ${yamlHint('good_first_issue.max_per_repo')} ${settingHelpMark('settings', 'features', 'Good first issue max per repo', `How many issues may wear the label in one repository at once, counting ones a maintainer labelled by hand. 0 uses the default (${esc(String(gfiDefaults.goodFirstIssueMaxPerRepo || 5))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.goodFirstIssueMaxPerRepo || ''))}" placeholder="${esc(String(gfiDefaults.goodFirstIssueMaxPerRepo || 5))}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMaxPerRepo" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Max new per sweep ${yamlHint('good_first_issue.max_per_sweep')} ${settingHelpMark('settings', 'features', 'Good first issue max per sweep', `How many issues one pass may label per repository, so a sweep never floods a tracker with welcome comments. 0 uses the default (${esc(String(gfiDefaults.goodFirstIssueMaxPerSweep || 2))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.goodFirstIssueMaxPerSweep || ''))}" placeholder="${esc(String(gfiDefaults.goodFirstIssueMaxPerSweep || 2))}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMaxPerSweep" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Minimum issue age ${yamlHint('good_first_issue.min_age')} ${settingHelpMark('settings', 'features', 'Good first issue minimum age', `How old an issue must be before hive advertises it, so it never races a reporter who is still editing or about to fix it. Empty uses the default (${esc(gfiDefaults.goodFirstIssueMinAge || '1h0m0s')}).`)}</label><input type="text" value="${esc(f.goodFirstIssueMinAge || '')}" placeholder="${esc(gfiDefaults.goodFirstIssueMinAge || '1h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMinAge" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Sweep interval ${yamlHint('good_first_issue.interval')} ${settingHelpMark('settings', 'features', 'Good first issue sweep interval', `How often the curation pass runs over the last enumeration. Empty uses the default (${esc(gfiDefaults.goodFirstIssueInterval || '15m0s')}).`)}</label><input type="text" value="${esc(f.goodFirstIssueInterval || '')}" placeholder="${esc(gfiDefaults.goodFirstIssueInterval || '15m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueInterval" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-subgroup">Scorer tuning</div>
          <div class="config-field-row settings-subsection-row">
            <div class="config-field"><label>Minimum body length ${yamlHint('good_first_issue.min_body_chars')} ${settingHelpMark('settings', 'features', 'Good first issue minimum body length', `Shortest issue body, in characters, the scorer accepts: a one-line issue rarely says enough for a newcomer to start. 0 uses the default (${esc(String(gfiDefaults.goodFirstIssueMinBodyChars || 80))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.goodFirstIssueMinBodyChars || ''))}" placeholder="${esc(String(gfiDefaults.goodFirstIssueMinBodyChars || 80))}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMinBodyChars" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Maximum body length ${yamlHint('good_first_issue.max_body_chars')} ${settingHelpMark('settings', 'features', 'Good first issue maximum body length', `Longest issue body, in characters, the scorer accepts: a very long issue is usually a design discussion rather than a first task. Must not be below the minimum. 0 uses the default (${esc(String(gfiDefaults.goodFirstIssueMaxBodyChars || 4000))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.goodFirstIssueMaxBodyChars || ''))}" placeholder="${esc(String(gfiDefaults.goodFirstIssueMaxBodyChars || 4000))}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMaxBodyChars" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Maximum packages mentioned ${yamlHint('good_first_issue.max_packages')} ${settingHelpMark('settings', 'features', 'Good first issue maximum packages', `How many source directories an issue may mention before it counts as too wide for a first contribution. 0 uses the default (${esc(String(gfiDefaults.goodFirstIssueMaxPackages || 2))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.goodFirstIssueMaxPackages || ''))}" placeholder="${esc(String(gfiDefaults.goodFirstIssueMaxPackages || 2))}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMaxPackages" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Minimum score ${yamlHint('good_first_issue.min_score')} ${settingHelpMark('settings', 'features', 'Good first issue minimum score', `How many positive signals (size labels, kind/bug, help wanted, clear steps or acceptance criteria) an issue needs before it is advertised. 0 uses the default (${esc(String(gfiDefaults.goodFirstIssueMinScore || 2))}).`)}</label><input type="number" min="0" step="1" value="${esc(String(f.goodFirstIssueMinScore || ''))}" placeholder="${esc(String(gfiDefaults.goodFirstIssueMinScore || 2))}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueMinScore" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Reselect cooldown ${yamlHint('good_first_issue.reselect_cooldown')} ${settingHelpMark('settings', 'features', 'Good first issue reselect cooldown', `How long an issue hive released (because one of its own agents started it) stays out of the pool before it may be advertised again. Go duration; empty uses the default (${esc(gfiDefaults.goodFirstIssueReselectCooldown || '168h0m0s')}).`)}</label><input type="text" value="${esc(f.goodFirstIssueReselectCooldown || '')}" placeholder="${esc(gfiDefaults.goodFirstIssueReselectCooldown || '168h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueReselectCooldown" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field"><label>Contributor docs URL ${yamlHint('good_first_issue.docs_url')} ${settingHelpMark('settings', 'features', 'Good first issue docs URL', `Base URL the welcome comment links contributor docs under. Absolute http(s) URL; empty uses the default (${esc(gfiDefaults.goodFirstIssueDocsUrl || '')}).`)}</label><input type="url" value="${esc(f.goodFirstIssueDocsUrl || '')}" placeholder="${esc(gfiDefaults.goodFirstIssueDocsUrl || '')}" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueDocsUrl" data-arg-types="s,s,w"></div>
          <div class="config-field"><label>Welcome comment template ${yamlHint('good_first_issue.comment_template')} ${settingHelpMark('settings', 'features', 'Good first issue comment template', `Overrides the welcome comment. Go text/template over the issue, repo, claim URL and docs variables; it must contain {{ .Marker }} so hive can find and supersede its own comment later. A template that does not parse is rejected on save. Empty uses the built-in comment.`)}</label><textarea rows="5" placeholder="Empty uses the built-in welcome comment" data-change-action="markDirty" data-arg0="features" data-arg1="goodFirstIssueCommentTemplate" data-arg-types="s,s,v">${esc(f.goodFirstIssueCommentTemplate || '')}</textarea></div>
          <div class="config-help">${gfiStats ? `Since start: ${esc(String(gfiStats.selected || 0))} selected · ${esc(String(gfiStats.released || 0))} released to hive · ${esc(String(gfiStats.converted || 0))} claimed by contributors. ` : ''}0 or empty = default. Exclude labels and the repo list stay yaml-only.</div>
        </div>
        ${renderGovReplanSection((_configState.data || {}).replan || {})}
        ${renderGovQuestionAutocloseSection((_configState.data || {}).question_autoclose || {})}

        <div class="settings-subsection">
          <div class="settings-subsection-title">Superseded PR grace ${settingHelpMark('settings', 'features', 'Superseded PR grace', `Timing of the superseded-PR sweep.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Supersession grace period ${yamlHint('supersession_sweep.grace_period')} ${settingHelpMark('settings', 'features', 'Supersession grace period', `How long a superseded human-authored PR stays open after the supersession notice before it may be closed automatically. Applies only when supersession_sweep.close_contributor_prs is enabled in hive.yaml; otherwise the Review Queue offers Close now and Keep open without an automatic-close countdown. Go duration; empty uses the default (${esc(platformDefaults.supersessionGracePeriod || '24h0m0s')}).`)}</label><input type="text" value="${esc(f.supersessionGracePeriod || '')}" placeholder="${esc(platformDefaults.supersessionGracePeriod || '24h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="supersessionGracePeriod" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-help">Applies on the next sweep.</div>
        </div>
        `)}

        ${featuresSectionHtml('agents', `
        <div class="config-field settings-subsection">
          <div style="display:flex;align-items:center;justify-content:space-between;gap:var(--sp-5);flex-wrap:wrap">
            <div class="config-toggle m-0">
              <div class="config-toggle-switch ${trajOn ? 'on' : ''}" data-action="toggleTrajectoryReview" data-arg-types="t"></div>
              <span class="config-toggle-label" style="font-weight:600">Trajectory Review ${yamlHint('governor.trajectory.enabled')}</span>
            </div>
            <span style="font-family:monospace;font-size:0.72rem;padding:2px 10px;border-radius:var(--r-pill);background:color-mix(in srgb, ${trajStatusColor} 14%, transparent);border:1px solid color-mix(in srgb, ${trajStatusColor} 45%, transparent);color:${trajStatusColor}">${esc(trajStatus)}</span>
          </div>
          ${trajStatsHtml}
          <div style="font-size:var(--fs-sm);color:var(--muted);line-height:1.6;margin-top:10px">
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-4)"><strong class="text-default">Why it exists.</strong> An agent running for a long time can take a series of individually-harmless steps that add up to something its task never authorized — escaping the sandbox, evading a security scanner, splitting a credential so a scanner won't see it, escalating privileges, or chasing a goal it picked up from a repo file instead of your instruction. A per-action allow/deny check can't catch this, because no single step breaks a rule; only the <em>direction</em> of the sequence does.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-4)"><strong class="text-default">What it does.</strong> On a fixed cadence it reads each running agent's recent transcript, compares it against the task the agent was actually assigned (its last kick), and judges whether the agent is still working toward that task. If the sequence has drifted, it <strong>pauses the agent and alerts you</strong> (or, in alert-only mode, just alerts). Alerts appear in the dashboard banner, the audit log, and ntfy.</p>
            <p style="margin:var(--sp-0) var(--sp-0) var(--sp-4)"><strong class="text-default">How it works.</strong> A second, cheap model gets the assigned task plus a bounded tail of the transcript and returns a one-line JSON verdict (<code>divergent / confidence / reason</code>). It judges direction, not any single line, and defaults to "not divergent" when unsure, so ordinary work never trips it. It reads the transcript every CLI writes to, so it covers Claude, Copilot, Gemini, Goose, and inference backends alike. It <strong>fails open</strong>: if the reviewer is unreachable it returns "not divergent," so a broken reviewer never pauses a working agent.</p>
            <p class="m-0"><strong class="text-default">What it requires.</strong> One OpenAI-compatible <code>/v1/chat/completions</code> endpoint for the reviewer model — a LiteLLM gateway, a vLLM server, or an llm-d front. It uses the Governor's LiteLLM endpoint by default; set a different one below to point at a cheap local model. <strong>Until an endpoint resolves, the lane is inert even when this is on</strong> — the status chip above tells you which state you're in.</p>
          </div>
          <div style="display:grid;grid-template-columns:1fr 1fr;gap:10px;margin-top:14px">
            <div>
              <label style="font-size:0.7rem;color:var(--muted);display:block;margin-bottom:3px">Reviewer model ${yamlHint('governor.trajectory.model')} ${settingHelpMark('trajectory-review', 'configuration', 'Reviewer model', `Model id sent to the reviewer endpoint. Empty = the Governor's LiteLLM default model. A cheap-but-capable model is right; a tiny model gives weaker verdicts (fails open, so it catches less rather than false-pausing).`)}</label>
              <input type="text" value="${esc(traj.model || '')}" placeholder="${esc(trajModel || 'litellm default')}" data-change-action="saveTrajectoryField" data-arg0="model" data-arg-types="s,v" style="width:100%">
            </div>
            <div>
              <label style="font-size:0.7rem;color:var(--muted);display:block;margin-bottom:3px">Review interval (sec) ${yamlHint('governor.trajectory.interval_s')} ${settingHelpMark('trajectory-review', 'configuration', 'Review interval (sec)', `How often the lane reviews running agents. Floored by the Governor eval interval. Empty/0 = default (600s).`)}</label>
              <input type="number" min="0" value="${trajInterval || ''}" placeholder="600" data-change-action="gh42" style="width:100%">
            </div>
            <div class="config-field m-0">
              <label>Transcript lines ${yamlHint('governor.trajectory.transcript_lines')} ${settingHelpMark('trajectory-review', 'configuration', 'Transcript lines', `Caps how many trailing transcript lines are sent to the reviewer. Bounds token cost and keeps the review focused on recent behavior. Empty/0 = default (120).`)}</label>
              <input type="number" min="0" step="1" value="${traj.transcriptLines || ''}" placeholder="120" data-change-action="saveTrajectoryField" data-arg0="transcriptLines" data-arg-types="s,N">
            </div>
            <div style="grid-column:1 / -1">
              <label style="font-size:0.7rem;color:var(--muted);display:block;margin-bottom:3px">Reviewer endpoint ${yamlHint('governor.trajectory.endpoint')} ${settingHelpMark('trajectory-review', 'configuration', 'Reviewer endpoint', `OpenAI-compatible base URL for the reviewer (LiteLLM / vLLM / llm-d). Empty = inherit the Governor's LiteLLM endpoint. The API key is inherited from LiteLLM or set in hive.yaml (governor.trajectory.api_key_env/api_key_file); a bare vLLM server needs none.`)}</label>
              <input type="text" value="${esc(traj.endpoint || '')}" placeholder="${trajEndpoint ? esc(trajEndpoint) + '  (inherited from LiteLLM)' : 'https://litellm-or-vllm-or-llmd:port'}" data-change-action="saveTrajectoryField" data-arg0="endpoint" data-arg-types="s,v" style="width:100%">
              <span style="font-size:var(--fs-2xs);margin-top:3px;display:block;color:${trajOn && !trajEndpoint ? 'var(--amber)' : 'var(--muted)'}">Resolved: ${trajEndpoint ? esc(trajEndpoint) + ' (' + esc(traj.endpointSource || '') + (traj.hasKey ? ', key set' : ', no key') + ')' : (trajOn ? 'none — Trajectory Review is ON but no agents are being reviewed. Set an endpoint above (or a Governor LiteLLM endpoint) to activate it.' : 'none — reviewer will not run')}</span>
            </div>
            <div style="grid-column:1 / -1">
              <label style="font-size:0.7rem;color:var(--muted);display:block;margin-bottom:3px">On divergence ${yamlHint('governor.trajectory.on_divergence')}</label>
              <div style="display:flex;gap:14px;font-size:0.8rem">
                <label style="display:flex;align-items:center;gap:5px;cursor:pointer"><input type="radio" name="traj-div" ${trajDiv === 'pause' ? 'checked' : ''} data-change-action="saveTrajectoryField" data-arg0="onDivergence" data-arg1="pause"> Pause the agent &amp; alert</label>
                <label style="display:flex;align-items:center;gap:5px;cursor:pointer"><input type="radio" name="traj-div" ${trajDiv === 'alert' ? 'checked' : ''} data-change-action="saveTrajectoryField" data-arg0="onDivergence" data-arg1="alert"> Alert only (don't pause)</label>
              </div>
              <span style="font-size:var(--fs-2xs);color:var(--muted);margin-top:3px;display:block">Start in alert-only to calibrate on your agents, then switch to pause. Changes take effect on next restart.</span>
            </div>
          </div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Re-entrant turns ${settingHelpMark('settings', 'features', 'Re-entrant turns', `Opt-in gates for the RFC #4002 re-entrant turn envelope. Both default off.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.reentrantTurnEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="reentrantTurnEnabled"></div>
            <span class="config-toggle-label">Re-entrant turns enabled ${yamlHint('turn.reentrant.enabled')} ${settingHelpMark('settings', 'features', 'Re-entrant turns enabled', `The global safety catch. Off means no agent can enter the envelope runner, even if its per-agent reentrant_turn field is true. HIVE_REENTRANT_TURN_ENABLED overrides it.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.reentrantTurnBackgroundFleetEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="reentrantTurnBackgroundFleetEnabled"></div>
            <span class="config-toggle-label">Include background fleet ${yamlHint('turn.reentrant.background_fleet_enabled')} ${settingHelpMark('settings', 'features', 'Re-entrant background fleet', `Extends the opt-in to every enabled background agent once a soak is underway. Off keeps the rollout to named agents. HIVE_REENTRANT_TURN_BACKGROUND_FLEET overrides it.`)}</span>
          </div>
        </div>
        <div class="settings-subsection">
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.prFollowUpEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="prFollowUpEnabled"></div>
            <span class="config-toggle-label">PR follow-up resume ${settingHelpMark('settings', 'features', 'PR follow-up resume', `OFF by default. When an agent opens a PR, the hive remembers which live session wrote it and keeps a short handoff note (why, approach, rejected alternatives, repro, files touched). Later CI failures, changes-requested reviews, review-bot threads and comments from people with write access on that PR are sent to the same session without clearing it. When that session is gone (a restart or the agent&apos;s next regular kick), the follow-up takes the normal fix-before-new path and the agent&apos;s next kick carries the handoff note. The hive&apos;s own replies and bot comments never trigger a follow-up. Pointers are deleted when the PR merges or closes. See src/docs/design/pr-follow-up-resume.md.`)}</span>
          </div>
          ${f.prFollowUpEffective !== f.prFollowUpEnabled ? `<p style="font-size:var(--fs-sm);color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">HIVE_PR_FOLLOWUP_RESUME overrides this setting: the feature is currently <b>${f.prFollowUpEffective ? 'on' : 'off'}</b>.</p>` : ''}
          <details class="settings-subsection-details"><summary class="settings-subsection-summary">Advanced PR follow-up</summary>
            <p class="config-help">Go durations such as <code>12h</code> or <code>336h</code>; empty uses the default. Read on every follow-up pass, so no restart is needed. The HIVE_PR_FOLLOWUP_MAX_AGE, HIVE_PR_FOLLOWUP_RESUME_ID_MAX_AGE and HIVE_PR_FOLLOWUP_RETENTION environment variables override these values.</p>
            <div class="config-field-row">
              <div class="config-field"><label>Resume window ${yamlHint('turn.pr_follow_up.max_age')} ${settingHelpMark('settings', 'features', 'PR follow-up resume window', `How long after a PR opens its authoring session may still be resumed (default 24h). Older follow-ups fall back to a fresh dispatch.`)}</label><input type="text" value="${esc(f.prFollowUpMaxAge || '')}" placeholder="${esc(prFollowUpDefaults.prFollowUpMaxAge || '24h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="prFollowUpMaxAge" data-arg-types="s,s,w"></div>
              <div class="config-field"><label>Resume-id window ${yamlHint('turn.pr_follow_up.resume_id_max_age')} ${settingHelpMark('settings', 'features', 'PR follow-up resume-id window', `How long a captured backend-native resume id is still offered to a later session (default 72h). Past it only the handoff note is handed on.`)}</label><input type="text" value="${esc(f.prFollowUpResumeIdMaxAge || '')}" placeholder="${esc(prFollowUpDefaults.prFollowUpResumeIdMaxAge || '72h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="prFollowUpResumeIdMaxAge" data-arg-types="s,s,w"></div>
              <div class="config-field"><label>Pointer retention ${yamlHint('turn.pr_follow_up.retention')} ${settingHelpMark('settings', 'features', 'PR follow-up retention', `How long a PR's pointer and handoff note are kept at all (default 336h, two weeks). Pointers are also deleted as soon as their PR merges or closes; this is the backstop for PRs whose end the hive never observes.`)}</label><input type="text" value="${esc(f.prFollowUpRetention || '')}" placeholder="${esc(prFollowUpDefaults.prFollowUpRetention || '336h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="prFollowUpRetention" data-arg-types="s,s,w"></div>
            </div>
          </details>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Provider rotation</div>
          <div class="config-toggle" data-setting-key="governor.rotation.enabled">
            <div class="config-toggle-switch ${rotationOn ? 'on' : ''}" data-action="rotationToggleSwitch"></div>
            <span class="config-toggle-label">Provider rotation ${settingHelpMark('settings', 'features', 'Provider rotation', `Rotate a lane off a provider whose measured usage crosses the threshold. Opt-in; see the backend capacity RFC.`)}</span>
          </div>
          ${rotationOn ? `
            <div class="config-field-row" style="margin-top:10px">
              <div class="config-field"><label>Threshold percent ${yamlHint('governor.rotation.threshold_pct')} ${settingHelpMark('settings', 'features', 'Threshold percent', `Usage percentage from 1 to 100 at which a subscription provider is considered exhausted. Defaults to 85.`)}</label><input type="number" min="1" max="100" step="1" value="${f.rotationThresholdPct || 85}" data-change-action="markDirty" data-arg0="features" data-arg1="rotationThresholdPct" data-arg-types="s,s,N"></div>
              <div class="config-field"><label>High-volume cadence seconds ${yamlHint('governor.rotation.high_volume_cadence_s')} ${settingHelpMark('settings', 'features', 'High-volume cadence seconds', `Agents with a cadence at or below this value never rotate onto subscription providers. Defaults to 1800 seconds.`)}</label><input type="number" min="1" step="1" value="${f.rotationHighVolumeCadenceS || 1800}" data-change-action="markDirty" data-arg0="features" data-arg1="rotationHighVolumeCadenceS" data-arg-types="s,s,N"></div>
            </div>
            <div class="config-field mt-3">
              <label>Headroom source ${yamlHint('governor.rotation.headroom_source')} ${settingHelpMark('settings', 'features', 'Headroom source', `Selects the provider headroom implementation. builtin (default) keeps the in-tree probers; ccleft opts in to ccleft-backed probers where available.`)}</label>
              <select data-change-action="markDirty" data-arg0="features" data-arg1="rotationHeadroomSource" data-arg-types="s,s,v">
                ${(f.rotationHeadroomSourceValues || ['builtin', 'ccleft']).map(v => `<option value="${esc(v)}" ${(f.rotationHeadroomSource || 'builtin') === v ? 'selected' : ''}>${esc(v === 'builtin' ? 'builtin (default)' : v)}</option>`).join('')}
              </select>
              <div class="config-help">Requires restart.</div>
            </div>
            <div class="config-field" style="margin-top:10px">
              <label>Providers ${settingHelpMark('settings', 'features', 'Providers', `Map provider names to their class, backing Hive backend IDs from /api/config/backends, and an optional monthly allowance for plans that report consumption but not entitlement.`)}</label>
              <div id="rotation-headroom-host">${renderRotationProvidersTable(rotationProviders, {})}</div>
              <button type="button" class="hv-btn btn-secondary btn-sm btn-small btn-primary" data-action="rotationAddProvider">Add provider</button>
            </div>
            <div class="config-field mt-5">
              <label>Agent tiers ${settingHelpMark('settings', 'features', 'Agent tiers', `Assign each agent to capability tier T1, T2, or T3. Rotation stays within tier before dropping when nothing has headroom.`)}</label>
              <table style="border-collapse:collapse;font-size:var(--fs-sm)">${rotationAgentRows || '<tr><td class="text-muted">No agents in the roster.</td></tr>'}</table>
            </div>
          ` : ''}
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Agent policy repo ${settingHelpMark('settings', 'features', 'Agent policy repo', `Optional. Pulls agent CLAUDE.md policies from a git config repo and keeps a local checkout up to date. Leave the repo empty to use the built-in policies.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Repo ${yamlHint('policies.repo')} ${settingHelpMark('settings', 'features', 'Policy repo', `Git URL of the policy repo, for example https://github.com/your-org/hive-config. Empty disables the policy watcher.`)}</label><input type="text" value="${esc(f.policiesRepo || '')}" placeholder="https://github.com/your-org/hive-config" data-change-action="markDirty" data-arg0="features" data-arg1="policiesRepo" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Branch ${yamlHint('policies.branch')} ${settingHelpMark('settings', 'features', 'Policy branch', `Branch of the policy repo to follow. Empty uses main.`)}</label><input type="text" value="${esc(f.policiesBranch || '')}" placeholder="main" data-change-action="markDirty" data-arg0="features" data-arg1="policiesBranch" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Path ${yamlHint('policies.path')} ${settingHelpMark('settings', 'features', 'Policy path', `Directory inside the policy repo that holds the agent policies, for example agents/.`)}</label><input type="text" value="${esc(f.policiesPath || '')}" placeholder="agents/" data-change-action="markDirty" data-arg0="features" data-arg1="policiesPath" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Local dir ${yamlHint('policies.local_dir')} ${settingHelpMark('settings', 'features', 'Policy local dir', `Absolute path where the policy repo is checked out, for example /data/policies.`)}</label><input type="text" value="${esc(f.policiesLocalDir || '')}" placeholder="/data/policies" data-change-action="markDirty" data-arg0="features" data-arg1="policiesLocalDir" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Poll interval ${yamlHint('policies.poll_interval')} ${settingHelpMark('settings', 'features', 'Policy poll interval', `How often the policy repo is pulled (default 5m). Go duration; empty uses the default.`)}</label><input type="text" value="${esc(f.policiesPollInterval || '')}" placeholder="5m0s" data-change-action="markDirty" data-arg0="features" data-arg1="policiesPollInterval" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-help">The policy watcher starts at boot, so every policy repo change requires a restart.</div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Jev decisions ${settingHelpMark('settings', 'features', 'Jev decisions', `Where the hive sends Jev typed decisions and which key it uses. Per-agent enablement is the agent's jev_mode. Agents never see the key: every call goes through the hive's local decision endpoint.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Provider ${yamlHint('jev.provider')} ${settingHelpMark('settings', 'features', 'Jev provider', `Selects the hosted endpoint: openrouter (default) or typesafe.`)}</label><select data-change-action="markDirty" data-arg0="features" data-arg1="jevProvider" data-arg-types="s,s,v"><option value=""${!f.jevProvider ? ' selected' : ''}>default (${esc(jevDefaults.jevProvider || 'openrouter')})</option>${jevProviders.map(p => `<option value="${esc(p)}"${f.jevProvider === p ? ' selected' : ''}>${esc(p)}</option>`).join('')}</select></div>
            <div class="config-field"><label>Model ${yamlHint('jev.model')} ${settingHelpMark('settings', 'features', 'Jev model', `The Jev model id. Empty uses the provider default.`)}</label><input type="text" value="${esc(f.jevModel || '')}" placeholder="${esc(jevDefaults.jevModel || '')}" data-change-action="markDirty" data-arg0="features" data-arg1="jevModel" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field"><label>Endpoint ${yamlHint('jev.endpoint')} ${settingHelpMark('settings', 'features', 'Jev endpoint', `Overrides the provider's systemone URL. Must be an http(s) URL; empty uses the provider default.`)}</label><input type="text" value="${esc(f.jevEndpoint || '')}" placeholder="${esc(jevDefaults.jevEndpoint || '')}" data-change-action="markDirty" data-arg0="features" data-arg1="jevEndpoint" data-arg-types="s,s,w"></div>
          <div class="config-field-row">
            <div class="config-field"><label>API key env var ${yamlHint('jev.api_key_env')} ${settingHelpMark('settings', 'features', 'Jev API key env var', `The NAME of the environment variable holding the Jev key, never the key itself (default JEV_API_KEY). When that variable is empty the connected OpenRouter gateway key is used.`)}</label><input type="text" value="${esc(f.jevApiKeyEnv || '')}" placeholder="${esc(jevDefaults.jevApiKeyEnv || 'JEV_API_KEY')}" autocomplete="off" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="jevApiKeyEnv" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Timeout ${yamlHint('jev.timeout')} ${settingHelpMark('settings', 'features', 'Jev timeout', `Bounds one decision round-trip (default 5s). Go duration; empty uses the default.`)}</label><input type="text" value="${esc(f.jevTimeout || '')}" placeholder="${esc(jevDefaults.jevTimeout || '5s')}" data-change-action="markDirty" data-arg0="features" data-arg1="jevTimeout" data-arg-types="s,s,w"><div class="config-help">Requires restart.</div></div>
          </div>
          <div class="config-help">Provider, model, endpoint and key variable apply to the next decision.</div>
        </div>
        <div class="settings-subsection">
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.mintEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="mintEnabled"></div>
            <span class="config-toggle-label">Token Mint (Workload Identity) ${settingHelpMark('token-mint', '', 'Token Mint (Workload Identity)', `Issues short-lived scoped JWTs that downstream cloud/registry WIF providers can trust via the issuer + JWKS.`)}</span>
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Issuer ${settingHelpMark('token-mint', '', 'Issuer', `The 'iss' claim and the identity WIF providers are configured to trust — typically the mint's public URL.`)}</label>
            <input type="text" value="${esc(f.mintIssuer || '')}" placeholder="https://mint.example.com" data-change-action="markDirty" data-arg0="features" data-arg1="mintIssuer" data-arg-types="s,s,w">
            <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-2) var(--sp-0) var(--sp-0)">Signing key is provisioned on-disk, not here.</p>
          </div>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Data directories ${settingHelpMark('settings', 'features', 'Data directories', `Where the hive keeps its persistent state. All paths sit on the persistent volume (/data by default).`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Metrics dir ${yamlHint('data.metrics_dir')} ${settingHelpMark('settings', 'features', 'Metrics dir', `Directory for metrics snapshots (default /data/metrics). Absolute path.`)}</label><input type="text" value="${esc(f.dataMetricsDir || '')}" placeholder="/data/metrics" data-change-action="markDirty" data-arg0="features" data-arg1="dataMetricsDir" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Logs dir ${yamlHint('data.logs_dir')} ${settingHelpMark('settings', 'features', 'Logs dir', `Directory for hive and agent logs (default /data/logs). Absolute path.`)}</label><input type="text" value="${esc(f.dataLogsDir || '')}" placeholder="/data/logs" data-change-action="markDirty" data-arg0="features" data-arg1="dataLogsDir" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Agent configs dir ${yamlHint('data.agents_dir')} ${settingHelpMark('settings', 'features', 'Agent configs dir', `Directory holding per-agent config overlays (default /data/agent-configs). Absolute path.`)}</label><input type="text" value="${esc(f.dataAgentsDir || '')}" placeholder="/data/agent-configs" data-change-action="markDirty" data-arg0="features" data-arg1="dataAgentsDir" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Claude sessions dir ${yamlHint('data.claude_sessions_dir')} ${settingHelpMark('settings', 'features', 'Claude sessions dir', `Where Claude Code keeps its session transcripts (default /data/home/.claude/projects). Absolute path.`)}</label><input type="text" value="${esc(f.dataClaudeSessionsDir || '')}" placeholder="/data/home/.claude/projects" data-change-action="markDirty" data-arg0="features" data-arg1="dataClaudeSessionsDir" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Copilot sessions dir ${yamlHint('data.copilot_sessions_dir')} ${settingHelpMark('settings', 'features', 'Copilot sessions dir', `Where Copilot CLI keeps its session state (default /data/home/.copilot/session-state). The session janitor prunes this directory. Absolute path.`)}</label><input type="text" value="${esc(f.dataCopilotSessionsDir || '')}" placeholder="/data/home/.copilot/session-state" data-change-action="markDirty" data-arg0="features" data-arg1="dataCopilotSessionsDir" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Bob sessions dir ${yamlHint('data.bob_sessions_dir')} ${settingHelpMark('settings', 'features', 'Bob sessions dir', `Where the Bob CLI keeps its session state (default /data/home/.bob). Absolute path.`)}</label><input type="text" value="${esc(f.dataBobSessionsDir || '')}" placeholder="/data/home/.bob" data-change-action="markDirty" data-arg0="features" data-arg1="dataBobSessionsDir" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field"><label>Session retention (days) ${yamlHint('data.session_retention_days')} ${settingHelpMark('settings', 'features', 'Session retention', `How many days of CLI session-state directories to keep before the janitor deletes them. Empty uses the default (${esc(String(platformDefaults.dataSessionRetentionDays || 7))}); 0 turns pruning off, which lets the volume grow without bound.`)}</label><input type="number" min="0" step="1" value="${esc(f.dataSessionRetentionDays || '')}" placeholder="${esc(String(platformDefaults.dataSessionRetentionDays || 7))}" data-change-action="markDirty" data-arg0="features" data-arg1="dataSessionRetentionDays" data-arg-types="s,s,w"></div>
          <div class="config-help">Requires restart: these directories are read at boot. Existing files are not moved.</div>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Token mint limits ${settingHelpMark('token-mint', '', 'Token mint limits', `Key location and lifetime cap for the Token Mint above.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Signing key path ${yamlHint('mint.key_path')} ${settingHelpMark('token-mint', '', 'Mint signing key path', `PEM path of the mint signing key; generated with 0600 permissions if absent. It must be on persistent storage and never readable outside the hive process. Only the path is shown.`)}</label><input type="text" value="${esc(f.mintKeyPath || '')}" placeholder="not set" readonly aria-readonly="true"><div class="config-help">Read-only here: the key is provisioned on disk and set in hive.yaml.</div></div>
            <div class="config-field"><label>Max token TTL (seconds) ${yamlHint('mint.max_ttl_seconds')} ${settingHelpMark('token-mint', '', 'Mint max TTL', `Upper bound on a minted token's lifetime. 0 uses the default (${esc(String(platformDefaults.mintMaxTtlSeconds || 900))}s); the hard cap is ${esc(String(platformDefaults.mintHardCapTtlSeconds || 3600))}s.`)}</label><input type="number" min="0" max="${esc(String(platformDefaults.mintHardCapTtlSeconds || 3600))}" step="1" value="${esc(String(f.mintMaxTtlSeconds || ''))}" placeholder="${esc(String(platformDefaults.mintMaxTtlSeconds || 900))}" data-change-action="markDirty" data-arg0="features" data-arg1="mintMaxTtlSeconds" data-arg-types="s,s,N"><div class="config-help">Requires restart.</div></div>
          </div>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Tool approval inbox ${settingHelpMark('settings', 'features', 'Tool approval inbox', `Where pending operator tool approvals are stored.`)}</div>
          <div class="config-field"><label>Inbox path ${yamlHint('tool_approval.inbox_path')} ${settingHelpMark('settings', 'features', 'Tool approval inbox path', `File that holds pending operator approvals. Empty uses /data/approvals/inbox.json on the persistent volume. Absolute path.`)}</label><input type="text" value="${esc(f.toolApprovalInboxPath || '')}" placeholder="/data/approvals/inbox.json" data-change-action="markDirty" data-arg0="features" data-arg1="toolApprovalInboxPath" data-arg-types="s,s,w"><div class="config-help">Requires restart.</div></div>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Swarm banner ${settingHelpMark('settings', 'features', 'Swarm banner', `Default wording for swarm events. Per-repo themes override it from the swarm view.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Event name ${yamlHint('swarm.event_name')} ${settingHelpMark('settings', 'features', 'Swarm event name', `Name shown for a swarm event. Empty uses the default. HIVE_SWARM_EVENT_NAME overrides it.`)}</label><input type="text" maxlength="200" value="${esc(f.swarmEventName || '')}" placeholder="${esc(platformDefaults.swarmEventName || 'Swarm')}" data-change-action="markDirty" data-arg0="features" data-arg1="swarmEventName" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Call to arms ${yamlHint('swarm.call_to_arms')} ${settingHelpMark('settings', 'features', 'Swarm call to arms', `Rallying line shown when a swarm starts. Empty uses the default. HIVE_SWARM_CALL_TO_ARMS overrides it.`)}</label><input type="text" maxlength="200" value="${esc(f.swarmCallToArms || '')}" placeholder="${esc(platformDefaults.swarmCallToArms || 'All hands on deck!')}" data-change-action="markDirty" data-arg0="features" data-arg1="swarmCallToArms" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Leaderboard title ${yamlHint('swarm.leaderboard_title')} ${settingHelpMark('settings', 'features', 'Swarm leaderboard title', `Heading of the swarm leaderboard. Empty uses the default.`)}</label><input type="text" maxlength="200" value="${esc(f.swarmLeaderboardTitle || '')}" placeholder="${esc(platformDefaults.swarmLeaderboardTitle || 'Swarm leaderboard')}" data-change-action="markDirty" data-arg0="features" data-arg1="swarmLeaderboardTitle" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-help">Requires restart: the swarm view reads its default theme once.</div>
        </div>
        `)}

        ${featuresSectionHtml('quality', `
        <div class="settings-subsection">
          <div class="settings-subsection-title">Quality loops ${settingHelpMark('retro-lane', '', 'Quality loops', `Post-completion feedback lanes that inspect finished work and file advisory findings back into the hive. Off by default, and an absent configuration block is zero behaviour change.`)}</div>
          <div class="config-toggle" data-setting-key="retro.enabled">
            <div class="config-toggle-switch ${f.retroEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="retroEnabled"></div>
            <span class="config-toggle-label">Retro loop enabled ${settingHelpMark('retro-lane', '', 'Retro loop enabled', `Periodically scans done/closed beads that have a closed or merged PR, reconstructs a compact trajectory from local ledgers, and files rule-based findings as advisory beads. Deterministic: no model is called, and no tokens are spent, unless an Analysis model is set below. Its pacing knobs (scan interval, max fix attempts, max kicks, long-stall days, recent-closed window) live under Advanced retro below.`)}</span>
          </div>
          <details style="margin:10px 0 0"><summary style="cursor:pointer;color:var(--muted);font-size:0.78rem;font-weight:600">Advanced retro</summary>
            <p class="config-help">YAML keys: <code>retro.enabled</code>, <code>retro.analysis_model</code>, <code>retro.scan_interval_s</code>, <code>retro.max_fix_attempts</code>, <code>retro.max_kicks</code>, <code>retro.long_stall_days</code>, <code>retro.recent_closed_window_s</code>. The retro lane is built at startup, so every retro change requires a restart.</p>
            <div class="config-field"><label>Analysis model ${yamlHint('retro.analysis_model')} ${settingHelpMark('retro-lane', '', 'Analysis model', `Optional. When set, the retro lane additionally runs a model pass over each reconstructed trajectory using this model. This is the only part of the retro loop that spends tokens — leave it empty for rule-based findings only.`)}</label>${featureModelInput('retroAnalysisModel', f.retroAnalysisModel || '', 'leave empty for deterministic only')}<div class="config-help">Requires restart.</div></div>
            <div class="config-field-row">
              <div class="config-field"><label>Scan interval (s) ${yamlHint('retro.scan_interval_s')} ${settingHelpMark('retro-lane', '', 'Retro scan interval', `How often the retro lane scans recently completed beads with closed or merged PR metadata (default 3600 seconds). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.retroScanIntervalS || ''))}" placeholder="${esc(String(retroDefaults.retroScanIntervalS || 3600))}" data-change-action="markDirty" data-arg0="features" data-arg1="retroScanIntervalS" data-arg-types="s,s,N"><div class="config-help">0 = default. Requires restart.</div></div>
              <div class="config-field"><label>Recent-closed window (s) ${yamlHint('retro.recent_closed_window_s')} ${settingHelpMark('retro-lane', '', 'Retro recent-closed window', `Only beads completed within this many seconds are analysed (hive.yaml.example suggests 604800, one week). 0 applies no window, so every completed bead is eligible.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.retroRecentClosedWindowS || ''))}" placeholder="0 (no window)" data-change-action="markDirty" data-arg0="features" data-arg1="retroRecentClosedWindowS" data-arg-types="s,s,N"><div class="config-help">0 = no window. Requires restart.</div></div>
            </div>
            <div class="config-field-row">
              <div class="config-field"><label>Max fix attempts ${yamlHint('retro.max_fix_attempts')} ${settingHelpMark('retro-lane', '', 'Retro max fix attempts', `A finding is filed when a PR needed at least this many failed CI/fix attempts (default 3). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.retroMaxFixAttempts || ''))}" placeholder="${esc(String(retroDefaults.retroMaxFixAttempts || 3))}" data-change-action="markDirty" data-arg0="features" data-arg1="retroMaxFixAttempts" data-arg-types="s,s,N"><div class="config-help">0 = default. Requires restart.</div></div>
              <div class="config-field"><label>Max kicks ${yamlHint('retro.max_kicks')} ${settingHelpMark('retro-lane', '', 'Retro max kicks', `A finding is filed when a bead received at least this many kicks before completion (default 5). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.retroMaxKicks || ''))}" placeholder="${esc(String(retroDefaults.retroMaxKicks || 5))}" data-change-action="markDirty" data-arg0="features" data-arg1="retroMaxKicks" data-arg-types="s,s,N"><div class="config-help">0 = default. Requires restart.</div></div>
              <div class="config-field"><label>Long-stall days ${yamlHint('retro.long_stall_days')} ${settingHelpMark('retro-lane', '', 'Retro long-stall days', `A finding is filed when a bead took longer than this many days from claim to close (default 7). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.retroLongStallDays || ''))}" placeholder="${esc(String(retroDefaults.retroLongStallDays || 7))}" data-change-action="markDirty" data-arg0="features" data-arg1="retroLongStallDays" data-arg-types="s,s,N"><div class="config-help">0 = default. Requires restart.</div></div>
            </div>
          </details>
          <div class="config-toggle mt-5">
            <div class="config-toggle-switch ${formalAvailable && f.formalEnabled ? 'on' : ''} ${formalAvailable ? '' : 'disabled'}" data-action="${formalAvailable ? 'toggleConfigSwitch' : ''}" data-section="features" data-key="formalEnabled" data-disabled="${formalAvailable ? 'false' : 'true'}" title="${formalAvailable ? '' : 'Requires ACMM L' + formalMinACMMLevel + '+'}"></div>
            <span class="config-toggle-label">Formal verification (quality lane) ${settingHelpMark('formal-verification', '', 'Formal verification (quality lane)', `OFF by default; effective only at ACMM L5 and L6; when on, the quality agent may author Spin/Promela models under formal/&lt;subsystem&gt;/, add a path-gated reporting-only CI job, and keep one deduplicated issue per violated property; see src/docs/formal-verification.md.`)}</span>
          </div>
          ${formalAvailable ? '' : `<p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Requires ACMM L${formalMinACMMLevel}+</p>`}
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Audit publication ${settingHelpMark('audit-campaign', 'publication', 'Audit publication', `The audit campaign's authorized issue publisher: files each validated finding as exactly one issue through the mutation journal, and routes security-sensitive findings to a private channel instead. Off by default; see src/docs/audit-campaign.md.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${publicationAvailable && f.publicationEnabled ? 'on' : ''} ${publicationAvailable ? '' : 'disabled'}" data-action="${publicationAvailable ? 'toggleConfigSwitch' : ''}" data-section="features" data-key="publicationEnabled" data-disabled="${publicationAvailable ? 'false' : 'true'}" title="${publicationAvailable ? '' : 'Requires ACMM L' + publicationMinACMMLevel + '+'}"></div>
            <span class="config-toggle-label">Audit issue publisher ${settingHelpMark('audit-campaign', 'publication', 'Audit issue publisher', `OFF by default; effective only at ACMM L3 and above and only in the enforce convergence mode (shadow and off never write). When on, each validated audit finding is filed once as an issue carrying its finding marker, Hive-Run trailer, and run link; a rerun replays the recorded issue instead of filing again. Requires a campaign owner below; see src/docs/audit-campaign.md.`)}</span>
          </div>
          ${publicationAvailable ? '' : `<p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Requires ACMM L${publicationMinACMMLevel}+</p>`}
          <div class="config-field" style="margin-top:10px">
            <label>Campaign owner ${yamlHint('publication.owner')} ${settingHelpMark('audit-campaign', 'publication', 'Campaign owner', `The maintainer principal every publication runs under and the actor on the outcome ledger. Publication stays off until an owner is named; the engine identity is never used.`)}</label>
            <input type="text" value="${esc(f.publicationOwner || '')}" placeholder="maintainer login" data-change-action="markDirty" data-arg0="features" data-arg1="publicationOwner" data-arg-types="s,s,w">
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Private disclosure channel ${settingHelpMark('audit-campaign', 'publication', 'Private disclosure channel', `Where security-sensitive findings go instead of a public issue: repo:owner/name files them in that private repository, notify sends only the title and finding hash to the operator notifier. A sensitive finding with no channel is refused and audited, never filed publicly.`)}</label>
            <input type="text" value="${esc(f.publicationPrivateChannel || '')}" placeholder="repo:owner/security-intake or notify" data-change-action="markDirty" data-arg0="features" data-arg1="publicationPrivateChannel" data-arg-types="s,s,w">
          </div>
        </div>
        ${renderGovReleaseSentinelSection((_configState.data || {}).release_sentinel || {})}

        <div class="settings-subsection">
          <div class="settings-subsection-title">Compliance posture windows ${settingHelpMark('settings', 'features', 'Compliance posture windows', `Look-back and retention of the compliance posture checks.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Posture look-back (days) ${yamlHint('compliance.posture_checks.window_days')} ${settingHelpMark('settings', 'features', 'Posture look-back', `How far back the posture checks that inspect merged PRs look, 1 to 90 days. 0 uses the default (${esc(String(platformDefaults.compliancePostureWindowDays || 30))}).`)}</label><input type="number" min="0" max="90" step="1" value="${esc(String(f.compliancePostureWindowDays || ''))}" placeholder="${esc(String(platformDefaults.compliancePostureWindowDays || 30))}" data-change-action="markDirty" data-arg0="features" data-arg1="compliancePostureWindowDays" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Posture history (days) ${yamlHint('compliance.posture_checks.history_days')} ${settingHelpMark('settings', 'features', 'Posture history', `How long posture-check results are kept, 30 to 1095 days. 0 uses the default (${esc(String(platformDefaults.compliancePostureHistoryDays || 365))}).`)}</label><input type="number" min="0" max="1095" step="1" value="${esc(String(f.compliancePostureHistoryDays || ''))}" placeholder="${esc(String(platformDefaults.compliancePostureHistoryDays || 365))}" data-change-action="markDirty" data-arg0="features" data-arg1="compliancePostureHistoryDays" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-help">Applies on the next posture pass.</div>
        </div>
        `)}

        ${featuresSectionHtml('integrations', `
        <div class="settings-subsection">
          <div class="settings-subsection-title">Observability</div>
          <p class="config-help">OpenTelemetry YAML keys: <code>otel.enabled</code>, <code>otel.endpoint</code>, <code>otel.headers.&lt;key&gt;</code>, <code>otel.insecure</code>, <code>otel.sample_ratio</code>, <code>otel.service_name</code>.</p>
          <div class="config-toggle" data-setting-key="otel.enabled">
            <div class="config-toggle-switch ${f.tracingEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="tracingEnabled"></div>
            <span class="config-toggle-label">OpenTelemetry export enabled ${settingHelpMark('settings', 'features', 'OpenTelemetry export enabled', `Exports OTLP/HTTP spans for hub/agent activity. Zero overhead when off. When on, traces export to your OTLP collector — view them in Grafana/Jaeger.`)}</span>
          </div>
          <details style="margin:10px 0 0" open><summary style="cursor:pointer;color:var(--muted);font-size:0.78rem;font-weight:600">Advanced OpenTelemetry</summary>
            <div class="config-field" data-setting-key="otel.endpoint"><label>Endpoint ${settingHelpMark('settings', 'features', 'Endpoint', `Collector endpoint (e.g. https://otel-collector:4318). Leave empty to fall back to the standard OTEL_EXPORTER_OTLP_ENDPOINT env var.`)}</label><input type="text" value="${esc(f.tracingEndpoint || '')}" placeholder="https://otel-collector:4318" data-change-action="markDirty" data-arg0="features" data-arg1="tracingEndpoint" data-arg-types="s,s,w"></div>
            <div class="config-field-row">
              <div class="config-field" data-setting-key="otel.service_name"><label>Service name ${settingHelpMark('settings', 'features', 'Service name', `The service.name resource attribute stamped on every exported span. Set it to something that identifies this hive so you can tell it apart from other hives in a shared collector. Defaults to &quot;hive&quot; when empty.`)}</label><input type="text" value="${esc(f.otelServiceName || '')}" placeholder="hive" data-change-action="markDirty" data-arg0="features" data-arg1="otelServiceName" data-arg-types="s,s,w"></div>
              <div class="config-field" data-setting-key="otel.sample_ratio"><label>Sample ratio ${settingHelpMark('settings', 'features', 'Sample ratio', `Head-based sampling ratio between 0 and 1 — the fraction of traces exported. The default 0 is special-cased to mean sample everything, not nothing, so leaving it alone exports all traces.`)}</label><input type="number" min="0" max="1" step="0.01" value="${f.tracingSampleRatio || 0}" data-change-action="gh57"><span style="font-size:0.65rem;color:var(--muted)">0 means sample all.</span></div>
            </div>
            <div class="config-toggle" data-setting-key="otel.insecure"><div class="config-toggle-switch ${f.otelInsecure ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="otelInsecure"></div><span class="config-toggle-label">Allow insecure OTLP transport ${settingHelpMark('settings', 'features', 'Allow insecure OTLP transport', `Permits plain HTTP to the collector instead of TLS. Only appropriate for a collector reached over a trusted network, such as one in the same cluster; on any other path the spans travel unencrypted.`)}</span></div>
            <div class="config-field" data-setting-key="otel.headers.<key>"><label>Headers <span style="font-weight:400;color:var(--muted)">(write-only, one key=value per line)</span> ${settingHelpMark('settings', 'features', 'Headers', `Extra HTTP headers sent with every OTLP export, typically an authorization header for a hosted collector. Write-only: values are stored but never echoed back to this form, so the box reads empty even when headers are set. \${VAR} references are expanded from the hive environment.`)}</label><textarea rows="3" style="width:100%;resize:vertical;font-family:var(--font-mono);font-size:0.74rem" placeholder="authorization=\${OTEL_EXPORTER_TOKEN}" data-change-action="parseOtelHeaders" data-arg-types="v"></textarea><div class="fs-xs text-muted mt-2">${f.otelHasHeaders ? 'Headers are configured; values are never echoed back.' : 'No headers configured.'} <button class="hv-btn btn-primary btn-sm" type="button" data-action="gh58">Clear saved headers</button></div></div>
          </details>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Linear Agent ${settingHelpMark('linear-agent', '', 'Linear Agent', `Connect this hive to a Linear workspace as a first-class agent (RFC #4492 Part 2). Once connected the hive is assignable and @-mentionable in Linear; delegating an issue to it opens an agent session that kicks the configured session agent. Requires LINEAR_CLIENT_ID / LINEAR_CLIENT_SECRET / LINEAR_WEBHOOK_SECRET on the hive.`)}</div>
          <div id="linear-agent-panel" style="font-size:0.72rem;color:var(--muted)">Loading…</div>
          <div class="config-field" style="margin-top:10px">
            <button class="hv-btn btn-secondary btn" data-action="linearAgentConnect">Connect workspace</button>
            <button class="hv-btn btn-secondary btn" data-action="linearAgentDisconnect">Disconnect</button>
            <button class="hv-btn btn-secondary btn" data-action="loadLinearAgentStatus">Refresh</button>
          </div>
        </div>
        <div class="hv-upstream-watch">
          <div class="hv-upstream-watch-title">Upstream Watch ${settingHelpMark('upstream-watch', 'configuration', 'Upstream Watch', `Divergence between each watched fork and the upstream it follows (#9969): the watermark and last run, how much has been surfaced, ported or dismissed, and the recent upstream refs with their fork issue. Switch the watch on, set the poll interval, and add, edit or remove the forks it follows here — changes are saved to upstream_watch in hive.yaml and take effect on the next poll.`)}</div>
          <div id="upstream-watch-panel" class="hv-upstream-watch-panel">Loading…</div>
          <div class="config-field mt-5">
            <button class="hv-btn btn-secondary btn" data-action="loadUpstreamWatch">Refresh</button>
          </div>
        </div>
        ${renderGovFleetReportSection((_configState.data || {}).fleet_report || {})}

        <div class="settings-subsection">
          <div class="settings-subsection-title">GitLab and Gitea forges ${settingHelpMark('settings', 'features', 'GitLab and Gitea forges', `Used only when the project forge is set to gitlab or gitea. Tokens are never stored in config: each forge names the environment variable that holds its token.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>GitLab URL ${yamlHint('gitlab.gitlab_url')} ${settingHelpMark('settings', 'features', 'GitLab URL', `GitLab instance root, e.g. https://gitlab.example.com. Empty uses ${esc(platformDefaults.gitlabUrl || 'https://gitlab.com')}.`)}</label><input type="text" value="${esc(f.gitlabUrl || '')}" placeholder="${esc(platformDefaults.gitlabUrl || 'https://gitlab.com')}" data-change-action="markDirty" data-arg0="features" data-arg1="gitlabUrl" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>GitLab token env var ${yamlHint('gitlab.token_env')} ${settingHelpMark('settings', 'features', 'GitLab token env var', `The NAME of the environment variable holding the GitLab token, never the token itself.`)}</label><input type="text" value="${esc(f.gitlabTokenEnv || '')}" placeholder="${esc(platformDefaults.gitlabTokenEnv || 'GITLAB_TOKEN')}" autocomplete="off" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="gitlabTokenEnv" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Gitea URL ${yamlHint('gitea.gitea_url')} ${settingHelpMark('settings', 'features', 'Gitea URL', `Gitea or Forgejo instance root, e.g. https://codeberg.org. There is no default; it is required for the Gitea forge.`)}</label><input type="text" value="${esc(f.giteaUrl || '')}" placeholder="https://gitea.example.com" data-change-action="markDirty" data-arg0="features" data-arg1="giteaUrl" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Gitea token env var ${yamlHint('gitea.token_env')} ${settingHelpMark('settings', 'features', 'Gitea token env var', `The NAME of the environment variable holding the Gitea token, never the token itself.`)}</label><input type="text" value="${esc(f.giteaTokenEnv || '')}" placeholder="${esc(platformDefaults.giteaTokenEnv || 'GITEA_TOKEN')}" autocomplete="off" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="giteaTokenEnv" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-help">Requires restart: the forge client is built at boot.</div>
        </div>

        <div class="settings-subsection">
          <div class="settings-subsection-title">Standalone deployment ${settingHelpMark('settings', 'features', 'Standalone deployment', `How the dashboard upgrade button reaches a Podman/Quadlet or Docker Compose host. Kubernetes hives upgrade through the hub and ignore these.`)}</div>
          <div class="config-field-row">
            <div class="config-field"><label>Podman mode ${yamlHint('deployment.podman_mode')} ${settingHelpMark('settings', 'features', 'Podman mode', `rootless or rootful, matching how the Quadlet units were installed. Unset means the dashboard cannot tell and shows the host upgrade command instead. HIVE_DEPLOYMENT_PODMAN_MODE overrides it.`)}</label><select data-change-action="markDirty" data-arg0="features" data-arg1="deploymentPodmanMode" data-arg-types="s,s,v"><option value=""${!f.deploymentPodmanMode ? ' selected' : ''}>not set</option>${['rootless', 'rootful'].map(m => `<option value="${m}"${f.deploymentPodmanMode === m ? ' selected' : ''}>${m}</option>`).join('')}</select></div>
            <div class="config-field"><label>Upgrade request dir ${yamlHint('deployment.upgrade_request_dir')} ${settingHelpMark('settings', 'features', 'Upgrade request dir', `Directory shared with the host where the dashboard drops an upgrade request for hive-upgrade.service to pick up. Must match the host side. HIVE_DEPLOYMENT_UPGRADE_REQUEST_DIR overrides it.`)}</label><input type="text" value="${esc(f.deploymentUpgradeRequestDir || '')}" placeholder="not set" data-change-action="markDirty" data-arg0="features" data-arg1="deploymentUpgradeRequestDir" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field"><label>Upgrade helper ${yamlHint('deployment.upgrade_helper')} ${settingHelpMark('settings', 'features', 'Upgrade helper', `Executable the dashboard runs for Docker Compose upgrades (default /usr/local/libexec/hive-dashboard-upgrade-helper). HIVE_DASHBOARD_UPGRADE_HELPER overrides it.`)}</label><input type="text" value="${esc(f.deploymentUpgradeHelper || '')}" placeholder="/usr/local/libexec/hive-dashboard-upgrade-helper" readonly aria-readonly="true"><div class="config-help">Read-only here: it names a program the dashboard executes, so it is changed in hive.yaml only.</div></div>
          <div class="config-help">Read on each upgrade check, so no restart is needed.</div>
        </div>
        `)}

        ${featuresSectionHtml('experimental', `
        <div class="settings-subsection">
          <div class="settings-subsection-title">External execution ${settingHelpMark('settings', 'features', 'External execution', `Report-only pilot of one externally hosted Flue generation workflow admitted through the contributor protocol (#8361). The engine receives a bounded context bundle and returns a stage receipt; it never receives a repository write credential, a dashboard token, or a publication tool.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.extFlueEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="extFlueEnabled"></div>
            <span class="config-toggle-label">Flue binding (runs.external.flue) ${settingHelpMark('settings', 'features', 'Flue binding (runs.external.flue)', `OFF by default. When on with no mode it runs in shadow: Hive persists admissions and observes but performs no external start and no external mutation. Only mode report-only dispatches, and only to relays that declare the ext-exec/flue capability; a relay without it is refused that work, never handed it as local work. Mode, endpoint and workflow version are set below. Requires a build with the extwork_flue tag; see src/docs/design/external-workflow-admission.md.`)}</span>
          </div>
          <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Mode: <b>${esc(f.extFlueMode || 'off')}</b> · adapter ${f.extFlueLinked ? 'linked into this build' : 'not linked into this build (build tag extwork_flue); the toggle stays inert'} · endpoint ${f.extFlueEndpointSet ? 'configured' : 'not configured'}.</p>
          <div class="config-field-row">
            <div class="config-field"><label>Flue mode ${yamlHint('runs.external.flue.mode')} ${settingHelpMark('settings', 'features', 'Flue mode', `shadow (the default when enabled) persists and observes admissions but starts nothing external. report-only dispatches one bounded generation workflow to the endpoint below. Has no effect while the binding is off.`)}</label><select data-change-action="markDirty" data-arg0="features" data-arg1="extFlueMode" data-arg-types="s,s,v">${(Array.isArray(f.extFlueModes) ? f.extFlueModes : ['shadow', 'report-only']).map(m => `<option value="${esc(m)}"${(f.extFlueModeSetting || runsDefaults.extFlueMode || 'shadow') === m ? ' selected' : ''}>${esc(m)}</option>`).join('')}</select></div>
            <div class="config-field"><label>Flue endpoint ${yamlHint('runs.external.flue.endpoint')} ${settingHelpMark('settings', 'features', 'Flue endpoint', `Base URL of the Flue runtime, e.g. http://127.0.0.1:8585. Never a credential: URLs with a user or password are rejected.`)}</label><input type="text" value="${esc(f.extFlueEndpoint || '')}" placeholder="http://127.0.0.1:8585" autocomplete="off" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="extFlueEndpoint" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Flue workflow version ${yamlHint('runs.external.flue.workflow_version')} ${settingHelpMark('settings', 'features', 'Flue workflow version', `The engine workflow version the runtime must advertise, e.g. flue-fixture/1.0.0. A runtime advertising another version is refused.`)}</label><input type="text" value="${esc(f.extFlueWorkflowVersion || '')}" placeholder="not pinned" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="extFlueWorkflowVersion" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.extOmpEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="extOmpEnabled"></div>
            <span class="config-toggle-label">OMP workbench host (runs.external.omp) ${settingHelpMark('settings', 'features', 'OMP workbench host (runs.external.omp)', `OFF by default and independent of the Flue toggle. An already-running OMP workbench takes a lease over the contributor relay it already holds: it sees the assignment summary, must accept before the bounded context bundle is delivered, reports progress, and ends with a stage receipt that Hive verifies. Report-only: no repository credential, no dashboard token; OMP is unconfined (T3), so write-capable stages are refused outright. Only relays declaring ext-exec/omp are offered this work. Requires a build with the extwork_omp tag; see src/docs/design/external-workflow-admission.md.`)}</span>
          </div>
          <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Mode: <b>${esc(f.extOmpMode || 'off')}</b> · adapter ${f.extOmpLinked ? 'linked into this build' : 'not linked into this build (build tag extwork_omp); the toggle stays inert'} · workflow version ${f.extOmpVersionSet ? 'pinned' : 'not pinned'}.</p>
          <div class="config-field-row">
            <div class="config-field"><label>OMP mode ${yamlHint('runs.external.omp.mode')} ${settingHelpMark('settings', 'features', 'OMP mode', `shadow (the default when enabled) observes only. report-only offers read-only stages to workbenches that declared ext-exec/omp. Has no effect while the host is off.`)}</label><select data-change-action="markDirty" data-arg0="features" data-arg1="extOmpMode" data-arg-types="s,s,v">${(Array.isArray(f.extOmpModes) ? f.extOmpModes : ['shadow', 'report-only']).map(m => `<option value="${esc(m)}"${(f.extOmpModeSetting || runsDefaults.extFlueMode || 'shadow') === m ? ' selected' : ''}>${esc(m)}</option>`).join('')}</select></div>
            <div class="config-field"><label>OMP workflow version ${yamlHint('runs.external.omp.workflow_version')} ${settingHelpMark('settings', 'features', 'OMP workflow version', `The workbench workflow version a peer must declare before it is offered work.`)}</label><input type="text" value="${esc(f.extOmpWorkflowVersion || '')}" placeholder="not pinned" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="extOmpWorkflowVersion" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-toggle mt-5">
            <div class="config-toggle-switch ${f.extVibeKanbanEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="extVibeKanbanEnabled"></div>
            <span class="config-toggle-label">vibe-kanban host ${yamlHint('runs.external.vibe_kanban.enabled')} ${settingHelpMark('settings', 'features', 'vibe-kanban host', `OFF by default and independent of the Flue and OMP toggles. Hands report-only stages to a local vibe-kanban board through its MCP server. When on with no mode it runs in shadow and starts nothing. Only relays declaring ext-exec/vibe-kanban are offered this work.`)}</span>
          </div>
          <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-3) var(--sp-0) var(--sp-0)">Mode: <b>${esc(f.extVibeKanbanMode || 'off')}</b> · adapter ${f.extVibeKanbanLinked ? 'linked into this build' : 'not linked into this build; the toggle stays inert'}.</p>
          <div class="config-field-row">
            <div class="config-field"><label>vibe-kanban mode ${yamlHint('runs.external.vibe_kanban.mode')} ${settingHelpMark('settings', 'features', 'vibe-kanban mode', `shadow (the default when enabled) observes only. report-only creates board work for read-only stages. Has no effect while the host is off.`)}</label><select data-change-action="markDirty" data-arg0="features" data-arg1="extVibeKanbanMode" data-arg-types="s,s,v">${(Array.isArray(f.extOmpModes) ? f.extOmpModes : ['shadow', 'report-only']).map(m => `<option value="${esc(m)}"${(f.extVibeKanbanModeSetting || runsDefaults.extFlueMode || 'shadow') === m ? ' selected' : ''}>${esc(m)}</option>`).join('')}</select></div>
            <div class="config-field"><label>Project ID ${yamlHint('runs.external.vibe_kanban.project_id')} ${settingHelpMark('settings', 'features', 'vibe-kanban project ID', `The vibe-kanban project that receives the work. Required for the host to start.`)}</label><input type="text" value="${esc(f.extVibeKanbanProjectId || '')}" placeholder="not set" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="extVibeKanbanProjectId" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Workflow version ${yamlHint('runs.external.vibe_kanban.workflow_version')} ${settingHelpMark('settings', 'features', 'vibe-kanban workflow version', `The workflow version the host must declare.`)}</label><input type="text" value="${esc(f.extVibeKanbanWorkflowVersion || '')}" placeholder="not pinned" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="extVibeKanbanWorkflowVersion" data-arg-types="s,s,w"></div>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>State dir ${yamlHint('runs.external.vibe_kanban.state_dir')} ${settingHelpMark('settings', 'features', 'vibe-kanban state dir', `Absolute directory for the adapter's dispatch records. Empty uses the vibe-kanban folder under the external-execution record dir.`)}</label><input type="text" value="${esc(f.extVibeKanbanStateDir || '')}" placeholder="default" data-change-action="markDirty" data-arg0="features" data-arg1="extVibeKanbanStateDir" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>MCP command ${yamlHint('runs.external.vibe_kanban.mcp_command')} ${settingHelpMark('settings', 'features', 'vibe-kanban MCP command', `The local command that starts the vibe-kanban MCP server. It is split into argv without a shell.`)}</label><input type="text" value="${esc(f.extVibeKanbanMcpCommand || '')}" placeholder="not set" readonly aria-readonly="true"><div class="config-help">Read-only here: it names a program the hive executes, so it is changed in hive.yaml only.</div></div>
          </div>
          <div class="config-help">Applied live: a binding is rebuilt the next time it is used after any of these settings change.</div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Long-running runs ${settingHelpMark('settings', 'features', 'Long-running runs', `The spec, plan, and implement stages on one lease. Off by default; an absent runs block is zero behaviour change.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.spektacularEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="spektacularEnabled"></div>
            <span class="config-toggle-label">Spek stage runner ${settingHelpMark('spektacular', '', 'Spek stage runner', `Polls the spektacular status verb for every active stage lease and advances the lease when the artifact reaches document_status final. A lease that expires without final is retried up to runs.max_stage_retries (${esc(String(f.maxStageRetries || ''))}) and then escalated for a decision. Hive never opens a Spek file; see src/docs/spektacular.md.`)}</span>
          </div>
          <div class="config-field" style="margin-top:10px">
            <label>Spektacular binary ${yamlHint('runs.spektacular.binary')} ${settingHelpMark('spektacular', '', 'Spektacular binary', `Path of the spektacular executable the runner shells out to. Empty resolves spektacular through PATH.`)}</label>
            <input type="text" value="${esc(f.spektacularBinary || '')}" placeholder="spektacular" data-change-action="markDirty" data-arg0="features" data-arg1="spektacularBinary" data-arg-types="s,s,w">
            <p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-2) var(--sp-0) var(--sp-0)">Poll every ${esc(String(f.spektacularPollS || ''))}s (runs.spektacular.poll_interval_s, yaml only).</p>
          </div>
          <div class="settings-subsection-title mt-5">Stage checkpoints ${settingHelpMark('settings', 'features', 'Stage checkpoints', `A checkpoint holds the run at a stage boundary until the owner approves. All three are on when unset.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsCheckpointSpec !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsCheckpointSpec"></div>
            <span class="config-toggle-label">Hold after spec ${yamlHint('runs.checkpoints.spec')} ${settingHelpMark('settings', 'features', 'Hold after spec', `Wait for owner approval before the plan stage starts. On by default.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsCheckpointPlan !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsCheckpointPlan"></div>
            <span class="config-toggle-label">Hold after plan ${yamlHint('runs.checkpoints.plan')} ${settingHelpMark('settings', 'features', 'Hold after plan', `Wait for owner approval before the implement stage starts. On by default.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsCheckpointImplement !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsCheckpointImplement"></div>
            <span class="config-toggle-label">Hold before implement lands ${yamlHint('runs.checkpoints.implement')} ${settingHelpMark('settings', 'features', 'Hold before implement lands', `The last gate in front of code changes. On by default. Turning it off is honoured only at ACMM L${esc(String(runsDefaults.implementCheckpointMinAcmm || 5))}+; below that the checkpoint keeps holding.`)}</span>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Checkpoint wait timeout (s) ${yamlHint('runs.wait_timeout_seconds')} ${settingHelpMark('settings', 'features', 'Checkpoint wait timeout', `How long a checkpoint waits for owner approval before it escalates (default ${esc(String(runsDefaults.runsWaitTimeoutSeconds || 3600))} seconds). It also floors how far a held plan lease is extended. 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.runsWaitTimeoutSeconds || ''))}" placeholder="${esc(String(runsDefaults.runsWaitTimeoutSeconds || 3600))}" data-change-action="markDirty" data-arg0="features" data-arg1="runsWaitTimeoutSeconds" data-arg-types="s,s,N"><div class="config-help">0 = default.</div></div>
            <div class="config-field"><label>Timed-out severity ${yamlHint('runs.wait_severity')} ${settingHelpMark('settings', 'features', 'Timed-out checkpoint severity', `Escalation severity for a checkpoint that waited past the timeout. decision means a person must choose; info is a notice only. Empty uses ${esc(runsDefaults.runsWaitSeverity || 'decision')}.`)}</label><select data-change-action="markDirty" data-arg0="features" data-arg1="runsWaitSeverity" data-arg-types="s,s,v"><option value=""${!f.runsWaitSeverity ? ' selected' : ''}>default (${esc(runsDefaults.runsWaitSeverity || 'decision')})</option>${(Array.isArray(f.runsWaitSeverities) ? f.runsWaitSeverities : ['decision', 'info']).map(v => `<option value="${esc(v)}"${f.runsWaitSeverity === v ? ' selected' : ''}>${esc(v)}</option>`).join('')}</select></div>
          </div>
          <div class="settings-subsection-title mt-5">Incoming issue triage ${settingHelpMark('settings', 'features', 'Incoming issue triage', `Classifies each actionable issue as direct-fix, spec-run or needs-clarification before dispatch. Spec verdicts also need the Spek stage runner. The spec and fix label lists (runs.triage.spec_labels, runs.triage.fix_labels) are yaml-only.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsTriageEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsTriageEnabled"></div>
            <span class="config-toggle-label">Run triage enabled ${yamlHint('runs.triage.enabled')} ${settingHelpMark('settings', 'features', 'Run triage enabled', `Off by default.`)}</span>
          </div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsTriageClarifyComment !== false ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsTriageClarifyComment"></div>
            <span class="config-toggle-label">Post clarification comment ${yamlHint('runs.triage.clarify_comment')} ${settingHelpMark('settings', 'features', 'Post clarification comment', `Post one comment asking the reporter for more detail when an issue needs clarification. On by default.`)}</span>
          </div>
          <div class="config-field"><label>Minimum body length ${yamlHint('runs.triage.min_body_chars')} ${settingHelpMark('settings', 'features', 'Minimum body length', `An issue whose trimmed body is shorter than this many characters is marked needs-clarification (default ${esc(String(runsDefaults.runsTriageMinBodyChars || 80))}). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.runsTriageMinBodyChars || ''))}" placeholder="${esc(String(runsDefaults.runsTriageMinBodyChars || 80))}" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriageMinBodyChars" data-arg-types="s,s,N"><div class="config-help">0 = default.</div></div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsTriageClarityGateEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsTriageClarityGateEnabled"></div>
            <span class="config-toggle-label">Clarity gate ${yamlHint('runs.triage.clarity_gate.enabled')} ${settingHelpMark('settings', 'features', 'Clarity gate', `Before an agent claims a human-filed issue, score how clearly it states a goal, acceptance criteria and scope. Below the threshold Hive asks the author once and parks the issue with needs-spec instead of claiming it. Works without Run triage. Off by default.`)}</span>
          </div>
          <div class="config-field"><label>Clarity threshold ${yamlHint('runs.triage.clarity_gate.threshold')} ${settingHelpMark('settings', 'features', 'Clarity threshold', `Clarity score (0-100) an issue needs to stay in the agent queue (default ${esc(String(runsDefaults.runsTriageClarityGateThreshold || 60))}). Goal 25, acceptance criteria 35, scope 20, detail 20; an open choice or TBD subtracts 30. 0 uses the default.`)}</label><input type="number" min="0" max="100" step="1" value="${esc(String(f.runsTriageClarityGateThreshold || ''))}" placeholder="${esc(String(runsDefaults.runsTriageClarityGateThreshold || 60))}" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriageClarityGateThreshold" data-arg-types="s,s,N"><div class="config-help">0 = default.</div></div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.runsTriagePriorArtEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="runsTriagePriorArtEnabled"></div>
            <span class="config-toggle-label">Prior-art gate ${yamlHint('runs.triage.prior_art.enabled')} ${settingHelpMark('settings', 'features', 'Prior-art gate', `Before an agent claims a feature, RFC or epic issue, search knowledge beads, settings, merged PRs, closed issues, docs and code for something that already covers it. At or above the threshold a human-filed issue is parked at stage/017-possible-existing with the findings until the author replies /hive decision proceed — <gap> or /hive decision exists; a clanker-filed one auto-closes after runs.triage.prior_art.bot_close_ttl; hive-filed issues get a findings note only. On by default.`)}</span>
          </div>
          <div class="config-field"><label>Prior-art threshold ${yamlHint('runs.triage.prior_art.threshold')} ${settingHelpMark('settings', 'features', 'Prior-art threshold', `Match confidence (0-100) the best finding needs before the gate parks an issue (default ${esc(String(runsDefaults.runsTriagePriorArtThreshold || 70))}): the share of the issue's weighted key terms (identifiers 4, title words 3, body words 1) the finding covers. 0 uses the default.`)}</label><input type="number" min="0" max="100" step="1" value="${esc(String(f.runsTriagePriorArtThreshold || ''))}" placeholder="${esc(String(runsDefaults.runsTriagePriorArtThreshold || 70))}" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriagePriorArtThreshold" data-arg-types="s,s,N"><div class="config-help">0 = default.</div></div>
          <div class="config-field"><label>Prior-art kinds ${yamlHint('runs.triage.prior_art.kinds[]')} ${settingHelpMark('settings', 'features', 'Prior-art kinds', `Comma-separated issue kinds the gate judges: feature, rfc, epic, enhancement (default ${esc(runsDefaults.runsTriagePriorArtKinds || 'feature, rfc, epic, enhancement')}). Empty uses the default. Bug reports are never gated.`)}</label><input type="text" value="${esc(f.runsTriagePriorArtKinds || '')}" placeholder="${esc(runsDefaults.runsTriagePriorArtKinds || 'feature, rfc, epic, enhancement')}" autocomplete="off" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriagePriorArtKinds" data-arg-types="s,s,w"></div>
          <div class="config-field-row">
            <div class="config-field"><label>Prior-art max findings ${yamlHint('runs.triage.prior_art.max_findings')} ${settingHelpMark('settings', 'features', 'Prior-art max findings', `Caps the findings listed in a park comment (default ${esc(String(runsDefaults.runsTriagePriorArtMaxFindings || 5))}). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.runsTriagePriorArtMaxFindings || ''))}" placeholder="${esc(String(runsDefaults.runsTriagePriorArtMaxFindings || 5))}" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriagePriorArtMaxFindings" data-arg-types="s,s,N"></div>
            <div class="config-field"><label>Prior-art max parks per cycle ${yamlHint('runs.triage.prior_art.max_parks_per_cycle')} ${settingHelpMark('settings', 'features', 'Prior-art max parks per cycle', `Caps how many issues one enumeration cycle may park, so enabling the gate on a backlog never floods a repo (default ${esc(String(runsDefaults.runsTriagePriorArtMaxParksPerCycle || 5))}). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.runsTriagePriorArtMaxParksPerCycle || ''))}" placeholder="${esc(String(runsDefaults.runsTriagePriorArtMaxParksPerCycle || 5))}" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriagePriorArtMaxParksPerCycle" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-field"><label>Prior-art bot close TTL ${yamlHint('runs.triage.prior_art.bot_close_ttl')} ${settingHelpMark('settings', 'features', 'Prior-art bot close TTL', `How long a clanker-filed park waits for a reply before it is closed as already-exists (default ${esc(runsDefaults.runsTriagePriorArtBotCloseTtl || '72h0m0s')}). Go duration; empty uses the default. A negative value that disables auto-close stays yaml-only.`)}</label><input type="text" value="${esc(f.runsTriagePriorArtBotCloseTtl || '')}" placeholder="${esc(runsDefaults.runsTriagePriorArtBotCloseTtl || '72h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="runsTriagePriorArtBotCloseTtl" data-arg-types="s,s,w"></div>
          <div class="settings-subsection-title mt-5">Spek recheck ${settingHelpMark('spektacular', '', 'Spek recheck', `Continuous convergence: re-runs the spec for opted-in campaigns on a cadence and imports changed or new plan tasks. The source lists (runs.spektacular.recheck.sources, runs.spektacular.recheck.discovery.sources) and runs.spektacular.recheck.egress_allowlist are yaml-only.`)}</div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.spektacularRecheckEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="spektacularRecheckEnabled"></div>
            <span class="config-toggle-label">Recheck cadence enabled ${yamlHint('runs.spektacular.recheck.enabled')} ${settingHelpMark('spektacular', '', 'Recheck cadence enabled', `Lets the scheduler start rechecks on cadence. A manual recheck also needs this unless it is forced.`)}</span>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Default interval ${yamlHint('runs.spektacular.recheck.default_interval')} ${settingHelpMark('spektacular', '', 'Recheck default interval', `Cadence for opted-in campaigns without their own interval (default ${esc(runsDefaults.spektacularRecheckDefaultInterval || '168h0m0s')}). Go duration; empty uses the default.`)}</label><input type="text" value="${esc(f.spektacularRecheckDefaultInterval || '')}" placeholder="${esc(runsDefaults.spektacularRecheckDefaultInterval || '168h0m0s')}" data-change-action="markDirty" data-arg0="features" data-arg1="spektacularRecheckDefaultInterval" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Max delta tasks ${yamlHint('runs.spektacular.recheck.max_delta_tasks')} ${settingHelpMark('spektacular', '', 'Recheck max delta tasks', `Caps how many changed or new plan tasks one recheck imports (default ${esc(String(runsDefaults.spektacularRecheckMaxDeltaTasks || 50))}). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.spektacularRecheckMaxDeltaTasks || ''))}" placeholder="${esc(String(runsDefaults.spektacularRecheckMaxDeltaTasks || 50))}" data-change-action="markDirty" data-arg0="features" data-arg1="spektacularRecheckMaxDeltaTasks" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-field"><label>Discovery proxy ${yamlHint('runs.spektacular.recheck.discovery_proxy')} ${settingHelpMark('spektacular', '', 'Discovery proxy', `The relay egress proxy, e.g. http://relay:18443. Required when exact sources are configured; discovery never falls back to direct network access. No credentials, path, query or fragment.`)}</label><input type="text" value="${esc(f.spektacularRecheckDiscoveryProxy || '')}" placeholder="not set" autocomplete="off" spellcheck="false" data-change-action="markDirty" data-arg0="features" data-arg1="spektacularRecheckDiscoveryProxy" data-arg-types="s,s,w"></div>
          <div class="config-toggle">
            <div class="config-toggle-switch ${f.spektacularDiscoveryEnabled ? 'on' : ''}" data-action="toggleConfigSwitch" data-section="features" data-key="spektacularDiscoveryEnabled"></div>
            <span class="config-toggle-label">Outward discovery ${yamlHint('runs.spektacular.recheck.discovery.enabled')} ${settingHelpMark('spektacular', '', 'Outward discovery', `Gathers bounded evidence from the declared discovery sources before a recheck spec. Does nothing without sources.`)}</span>
          </div>
          <div class="config-field-row">
            <div class="config-field"><label>Discovery timeout ${yamlHint('runs.spektacular.recheck.discovery.timeout')} ${settingHelpMark('spektacular', '', 'Discovery timeout', `Bounds each discovery pass (default ${esc(runsDefaults.spektacularDiscoveryTimeout || '30s')}). Go duration; empty uses the default.`)}</label><input type="text" value="${esc(f.spektacularDiscoveryTimeout || '')}" placeholder="${esc(runsDefaults.spektacularDiscoveryTimeout || '30s')}" data-change-action="markDirty" data-arg0="features" data-arg1="spektacularDiscoveryTimeout" data-arg-types="s,s,w"></div>
            <div class="config-field"><label>Discovery max items ${yamlHint('runs.spektacular.recheck.discovery.max_total_items')} ${settingHelpMark('spektacular', '', 'Discovery max total items', `Caps all discovery evidence for one revision (default ${esc(String(runsDefaults.spektacularDiscoveryMaxTotalItems || 100))}). 0 uses the default.`)}</label><input type="number" min="0" step="1" value="${esc(String(f.spektacularDiscoveryMaxTotalItems || ''))}" placeholder="${esc(String(runsDefaults.spektacularDiscoveryMaxTotalItems || 100))}" data-change-action="markDirty" data-arg0="features" data-arg1="spektacularDiscoveryMaxTotalItems" data-arg-types="s,s,N"></div>
          </div>
          <div class="config-help">Checkpoint, triage and recheck settings are applied live; no restart is needed.</div>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Integration orchestration</div>
          <div class="config-help">Default OFF. Coordinates authoring admission above existing preparation and native queues; never replaces CI, review, holds or current-base guards. Advisory cannot block. Enforce requires explicit repository/base scopes and complete runner telemetry. No native queue jumping or GitHub build-window control. 5,000/day is a capacity target, not a measured result.</div>
          <div class="config-field"><label>Mode ${yamlHint('integration_orchestration.mode')} ${settingHelpMark('settings', 'features', 'Integration mode', `Off preserves existing dispatch. Advisory records observations but cannot block. Enforce applies only enrolled, authoritative constraints; it never replaces merge gates.`)}</label>
            <select data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="mode" data-arg-types="s,s,v">
              ${['off','advisory','enforce'].map(m => `<option value="${esc(m)}" ${io.mode === m ? 'selected' : ''}>${esc(m)}</option>`).join('')}
            </select>
          </div>
          <div class="config-field"><label>Exact scopes (one owner/repo#base per line) ${yamlHint('integration_orchestration.scopes[].repo')} ${yamlHint('integration_orchestration.scopes[].base')} ${settingHelpMark('settings', 'features', 'Integration scopes', `At most eight exact repository/base destinations. No wildcard enrollment. Multiple enrolled bases leave issue destination unknown rather than guessing.`)}</label>
            <textarea data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="scope_lines" data-arg-types="s,s,v">${esc((io.scopes || []).map(s => s.repo + '#' + s.base).join('\n'))}</textarea>
          </div>
          <div class="config-field-row">
            ${[
              ['max_active','Active admissions',2,'integration_orchestration.max_active'],
              ['max_jobs','Executed jobs per window',20,'integration_orchestration.max_jobs'],
              ['max_runner_minutes','Runner minutes per window',60,'integration_orchestration.max_runner_minutes'],
              ['max_attempts','Attempt bound',2,'integration_orchestration.max_attempts'],
              ['admission_jobs','Job allocation per admission',1,'integration_orchestration.admission_jobs'],
              ['admission_runner_minutes','Minute allocation per admission',10,'integration_orchestration.admission_runner_minutes']
            ].map(([key,label,def,path]) =>
              `<div class="config-field"><label>${esc(label)} ${yamlHint(path)} ${settingHelpMark('settings', 'features', label, `Validated static admission bound, not a capacity measurement. Outstanding allocations count conservatively alongside actual assigned-runner execution. Missing accounting freezes growth; retries never rerun or cancel GitHub jobs.`)}</label><input type="number" min="1" value="${esc(String(io[key] || def))}" data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="${esc(key)}" data-arg-types="s,s,N"></div>`).join('')}
          </div>
          <div class="config-field-row">
            ${[
              ['lease_ttl','Reservation TTL (minutes)',5,'integration_orchestration.lease_ttl'],
              ['window','Accounting window (minutes)',60,'integration_orchestration.window'],
              ['max_observation_age','Maximum observation age (minutes)',30,'integration_orchestration.max_observation_age']
            ].map(([key,label,def,path]) =>
              `<div class="config-field"><label>${esc(label)} ${yamlHint(path)} ${settingHelpMark('settings', 'features', label, `Lease TTL is 1–15 minutes and clipped to live ownership. Accounting window is 1 minute–24 hours. Maximum observation age is 1–60 minutes; stale observations are unknown, not zero.`)}</label><input type="number" min="1" value="${esc(String(io[key] ? io[key] / (60 * 1e9) : def))}" data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="${esc(key)}_minutes" data-arg-types="s,s,N"></div>`).join('')}
          </div>
          <div class="config-field"><label>Pinned workflow evidence contracts (JSON array) ${settingHelpMark('settings', 'features', 'Integration workflow contracts', `Pins a separately reviewed workflow blob, event engine, exact required job names and producer App ID. Empty means policy evidence remains unknown. This does not activate native queues or replace full CI, review, DCO or other gates.`)}</label>
            <textarea data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="policy_json" data-arg-types="s,s,v">${esc(JSON.stringify(io.policies || [], null, 2))}</textarea>
          </div>
          <div class="config-help">${[
            'integration_orchestration.policies[].repo', 'integration_orchestration.policies[].base',
            'integration_orchestration.policies[].engine', 'integration_orchestration.policies[].workflow',
            'integration_orchestration.policies[].blob', 'integration_orchestration.policies[].app_id',
            'integration_orchestration.policies[].jobs[]'
          ].map(path => yamlHint(path)).join(' ')} Disabling releases only orchestration reservations; existing claims and native entries remain. Missing group membership, policy or producer proof stays unknown.</div>
          <button class="hv-btn btn-secondary btn" data-action="loadIntegrationOrchestration">Inspect intents, reservations and CI evidence</button>
          <pre id="integration-orchestration-summary" class="config-help" aria-live="polite"></pre>
          <div class="config-field"><label>Candidate execution (independent opt-in) ${yamlHint('integration_orchestration.execution.mode')} ${settingHelpMark('settings', 'features', 'Candidate execution', `Off does not enable a writer. Advisory cannot block or dispatch. Enforce requires exact enrolled repository/base profiles, complete real CI accounting, a designated executor and actual protected qualification. Revoke the old forge epoch before returning Hive authority to legacy flow. This is separate from authoring admission and native queue settings.`)}</label>
            <select data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="execution_mode" data-arg-types="s,s,v">
              ${['off','advisory','enforce'].map(mode => `<option value="${esc(mode)}" ${(integrationExecution.mode || 'off') === mode ? 'selected' : ''}>${esc(mode)}</option>`).join('')}
            </select>
          </div>
          <div class="config-field"><label>Execution profile and bounded enrollment (JSON object) ${settingHelpMark('settings', 'features', 'Execution enrollment', `Explicit scopes, actual repository/actor/App installation/workflow IDs, policy and workflow-tree hashes, immutable authority epoch, isolated qualification ID, job/minute/member/attempt bounds and duration nanoseconds. No inferred actors, spare capacity, wildcard enrollment, automatic growth or failover. The mode selector, not this profile JSON, chooses execution mode.`)}</label>
            <textarea data-change-action="markDirty" data-arg0="integration_orchestration" data-arg1="execution_json" data-arg-types="s,s,v">${esc(JSON.stringify(integrationExecutionProfile, null, 2))}</textarea>
          </div>
          <div class="config-help">Source, published image, running version, desired mode and effective writer are separate receipts. Unknown telemetry is not green or zero. Production requires a real one-member protected qualification and the same complete producer/actor profile. Native authority permits only ordinary Hive enqueue admission, never queue jumping or GitHub build-window control. All participating hives must be upgraded/enrolled or stopped; copied identity and automatic takeover are unsupported.</div>
          <div class="config-help">${[
            'integration_orchestration.execution.scopes[].repo', 'integration_orchestration.execution.scopes[].base',
            'integration_orchestration.execution.scopes[].authority', 'integration_orchestration.execution.scopes[].repository_id',
            'integration_orchestration.execution.scopes[].request_actor_id', 'integration_orchestration.execution.scopes[].candidate_actor_id',
            'integration_orchestration.execution.scopes[].producer_app_id', 'integration_orchestration.execution.scopes[].promotion_app_id',
            'integration_orchestration.execution.scopes[].promotion_installation_id', 'integration_orchestration.execution.scopes[].materializer_id',
            'integration_orchestration.execution.scopes[].candidate_workflow_id', 'integration_orchestration.execution.scopes[].default_branch',
            'integration_orchestration.execution.scopes[].policy', 'integration_orchestration.execution.scopes[].workflow_tree',
            'integration_orchestration.execution.scopes[].required_jobs[]', 'integration_orchestration.execution.scopes[].executor_id',
            'integration_orchestration.execution.scopes[].authority_ref', 'integration_orchestration.execution.scopes[].authority_commit',
            'integration_orchestration.execution.scopes[].qualification_id', 'integration_orchestration.execution.scopes[].max_members',
            'integration_orchestration.execution.scopes[].expected_jobs', 'integration_orchestration.execution.scopes[].expected_minutes',
            'integration_orchestration.execution.scopes[].purpose', 'integration_orchestration.execution.scopes[].qualified_base',
            'integration_orchestration.execution.max_active', 'integration_orchestration.execution.max_runner_minutes',
            'integration_orchestration.execution.max_jobs', 'integration_orchestration.execution.max_attempts',
            'integration_orchestration.execution.observation_age', 'integration_orchestration.execution.window',
            'integration_orchestration.execution.cooldown'
          ].map(path => yamlHint(path)).join(' ')}</div>
          <button class="hv-btn btn-secondary btn" data-action="prepareIntegrationExecutionIdentity">Prepare local execution identity only</button>
          <pre id="integration-execution-identity" class="config-help" aria-live="polite"></pre>
        </div>
        <div class="settings-subsection">
          <div class="settings-subsection-title">Convergence Rollout ${settingHelpMark('settings', 'features', 'Convergence Rollout', `Convergence checks whether work Hive is about to kick is actually admissible right now &mdash; blocker still open, dependency unmerged, evidence stale &mdash; so agents stop burning tokens rediscovering &quot;not ready yet&quot;. It gates candidates only; it never plans or schedules.`)}</div>
          <div class="config-field">
            <label>Mode ${settingHelpMark('settings', 'features', 'Mode', `Shadow (default) evaluates every internal kick decision and records what it WOULD have withheld, without enforcing anything &mdash; dispatch is identical to off, and it costs no tokens and no extra GitHub calls. Enforce withholds blocked/unknown work from internal scheduled/cached kicks. Off is the rollback control arm, and the one posture that records no evidence. Workflow: leave it in shadow, load the soak summary, and if the would-withhold cases look genuine, switch to enforce. Live &mdash; no restart needed.`)}</label>
            <select data-change-action="markDirty" data-arg0="convergence" data-arg1="mode" data-arg-types="s,s,v">
              ${(Array.isArray(cv.modes) ? cv.modes : ['off','shadow','enforce']).map(m => `<option value="${esc(m)}" ${cv.mode === m ? 'selected' : ''}>${esc(m)}</option>`).join('')}
            </select>
            ${cv.env_override ? `<p style="font-size:0.72rem;color:var(--yellow);margin:var(--sp-2) var(--sp-0) var(--sp-0)">HIVE_CONVERGENCE_MODE is set — the effective mode is <strong>${esc(cv.effective_mode || 'off')}</strong> regardless of this setting.</p>` : `<p style="font-size:0.72rem;color:var(--muted);margin:var(--sp-2) var(--sp-0) var(--sp-0)">Effective mode: <strong>${esc(cv.effective_mode || 'off')}</strong> (generation ${cv.generation || 0}).</p>`}
          </div>
          <div class="config-field" style="margin-top:10px">
            <button class="hv-btn btn-secondary btn" data-action="loadConvergenceSoak">Load soak summary</button>
            <div id="convergence-soak-summary" style="font-size:0.72rem;color:var(--muted);margin-top:var(--sp-3)"></div>
          </div>
        </div>
        `)}`;
    }
