    function renderRepos(repos) {
      refreshSectionCardShell('repos-section');
      if (_repoResizeDrag || _repoOrderDrag) return;
      repos = repoOrderApply(repos || []);
      const el = document.getElementById('repos');
      const showPlan = planningAllowed();
      const showDesign = showPlan && dashboardRoleAtLeast(window._hiveRole || 'read', 'owner');
      const planLinks = planIssueLinks();
      const status = window._lastStatus || {};
      const pillFilter = repoPillFilterState();
      const pillFilterIsActive = repoPillFilterActive(pillFilter);
      const overviewFilter = overviewProjectFilterState();
      const overviewFilterSummary = overviewFilter ? overviewProjectFilterSummary(overviewFilter.key, repos) : null;
      const visibleRepos = (repos || []).filter(r => repoPillFilterRepoMatches(r && (r.full || r.name), pillFilter)).filter(r => !overviewFilter || overviewProjectFilterRepoMatches(r, overviewFilter).count > 0);
      renderRepoLegend();
      renderOverviewCharts(repos);
      const activeSwarmRepo = ((status.swarm || {}).active || {}).repo || '';
      // Definitive single-host badge in the section header: every repo in this
      // hive is on ONE GitHub host (github.com or a GHE instance). Green =
      // github.com, blue = GitHub Enterprise — matches the hub spokes-table pill.
      // hostLabel is the hive's forge. window._githubBaseUrl now comes from the
      // backend's ResolvedBaseURL() (base_url, else the api_url host), so a
      // pooled GHE hive carrying base_url: "" + api_url github.ibm.com resolves
      // to github.ibm.com here — not github.com. This is the fix for the pill
      // that read "GITHUB.COM" on a github.ibm.com hive.
      const hostLabel = (window._githubBaseUrl || 'https://github.com').replace(/^https?:\/\//, '').replace(/\/$/, '') || 'github.com';
      const isPublic = hostLabel === 'github.com';
      // Each card carries its own forge (#14115), and repo_policies[].forge can
      // put one repo on a different forge from the rest. The badge names the
      // hive's forge only when every card agrees; a GitLab or Gitea/Forgejo
      // hive gets its own host here instead of the GitHub host the badge would
      // otherwise claim, and a mixed hive says so instead of claiming one host.
      const repoForges = (repos || []).map(r => repoForgeWeb(r, hostLabel));
      const forgeHosts = [...new Set(repoForges.map(f => f.name + ' · ' + f.host))];
      const hiveForge = repoForges[0] || repoForgeWeb(undefined, hostLabel);
      const badge = document.getElementById('repos-host-badge');
      if (badge && forgeHosts.length > 1) {
        badge.textContent = forgeHosts.length + ' forges';
        badge.style.background = 'color-mix(in srgb, var(--indigo) 15%, transparent)';
        badge.style.color = 'var(--indigo)';
        badge.title = 'Projects in this hive are on ' + forgeHosts.join(', ') + ' — each card names its own forge';
      } else if (badge && hiveForge.kind !== 'github') {
        badge.textContent = hiveForge.name + ' · ' + hiveForge.host;
        badge.style.background = 'color-mix(in srgb, var(--indigo) 15%, transparent)';
        badge.style.color = 'var(--indigo)';
        badge.title = 'Every project in this hive is on ' + hiveForge.name + ' at ' + hiveForge.host + ' — this hive is single-host';
      } else if (badge) {
        // Definitive single-host badge in the section header: every repo in this
        // hive is on ONE GitHub host. Green = github.com, indigo = GHE — GHE also
        // gets an "Enterprise" tag so an operator instantly sees it is not public.
        badge.textContent = isPublic ? 'github.com' : hostLabel + ' · Enterprise';
        badge.style.background = isPublic ? 'color-mix(in srgb, var(--green) 15%, transparent)' : 'color-mix(in srgb, var(--indigo) 15%, transparent)';
        badge.style.color = isPublic ? 'var(--green)' : 'var(--indigo)';
        badge.title = 'Every project in this hive is on ' + hostLabel + (isPublic ? '' : ' (GitHub Enterprise)') + ' — this hive is single-host';
      }
      // Section-header escalation counter: "· N need human review". Escalated
      // PRs (needs-human label from the escalation ledger) previously had no
      // section-level signal — nine parked PRs sat invisible to the operator
      // for a week (kubestellar/console, 2026-09-01). Hidden at zero so a
      // healthy hive shows nothing extra.
      // Held PRs count too: a hold+needs-human PR (a Renovate bump the
      // ledger gave up on) is waiting on a person exactly as much as an
      // unheld one is, and it now has a pill on the card to match.
      const needHumanTotal = repos.reduce((n, r) => n + (r.openPrs || []).concat(r.heldPrs || []).filter(p => (p.labels || []).map(l => String(l)).includes('needs-human')).length
        + (r.actionableIssues || []).concat(r.heldIssues || [], r.nonActionableIssues || []).filter(repoItemNeedsHuman).length, 0);
      const nhEl = document.getElementById('repos-needs-human');
      if (nhEl) {
        if (needHumanTotal > 0) {
          nhEl.textContent = '· ' + needHumanTotal + ' need' + (needHumanTotal === 1 ? 's' : '') + ' human review';
          nhEl.style.display = 'inline';
        } else {
          nhEl.textContent = '';
          nhEl.style.display = 'none';
        }
      }
      // "Reset layout" only appears once there is a layout to reset (#8000) —
      // a hive whose cards are all at the default has nothing for it to do.
      const resetBtn = document.getElementById('repos-reset-layout-btn');
      if (resetBtn) resetBtn.style.display = (Object.keys(repoCardWidths()).length || repoOrderHasCustom(repos)) ? 'inline-block' : 'none';
      const clearFilterBtn = document.getElementById('repos-clear-pill-filter-btn');
      if (clearFilterBtn) {
        clearFilterBtn.hidden = !(pillFilterIsActive || overviewFilter);
        clearFilterBtn.dataset.action = overviewFilter ? 'clearProjectsFilter' : 'clearRepoPillFilter';
        clearFilterBtn.title = overviewFilter ? 'Clear the active Projects filter' : 'Clear the active repository pill filter';
      }
      const overviewFilterChip = document.getElementById('repos-overview-filter-chip');
      if (overviewFilterChip) {
        overviewFilterChip.hidden = !overviewFilter;
        setIfChanged(overviewFilterChip, overviewFilter ? overviewProjectFilterChip(overviewFilter, Number.isFinite(Number(overviewFilter.selectedCount)) ? Number(overviewFilter.selectedCount) : (overviewFilterSummary && overviewFilterSummary.count)) : '');
      }
      setIfChanged(el, visibleRepos.map(r => {
        const iSpark = sparkSvg(historyData.map(s => s.repos?.[r.name]?.issues ?? 0), 'var(--blue)', getHistoryTimes(), `repo:${r.name}:issues`);
        const pSpark = sparkSvg(historyData.map(s => s.repos?.[r.name]?.prs ?? 0), 'var(--purple)', getHistoryTimes(), `repo:${r.name}:prs`);
        const cardForge = repoForgeWeb(r, hostLabel);
        const repoUrl = cardForge.base + '/' + (r.full || r.name);
        // The card's width comes from the stored map on EVERY repaint, not
        // from a one-time assignment on the element: this HTML replaces the
        // grid's contents on every governor cycle, so a width set once on the
        // node would be thrown away within the minute (#8000).
        const cardRepo = r.full || r.name || '';
        const issueBreakdown = formatRepoWorkBreakdown(r.workBreakdown?.issues, r.issues, 'issues', repoStaleIssueCount(r.actionableIssues || []));
        const prBreakdown = formatRepoWorkBreakdown(r.workBreakdown?.prs, r.prs, 'prs');
        ensureRepoHoldPermission(cardRepo);
        const canToggleHold = repoHoldPermissionKnown(cardRepo);
        const cardRepoKey = repoOrderName(cardRepo);
        const cardW = repoCardWidth(cardRepo);
        const overviewRepoMatches = overviewFilter ? overviewProjectFilterRepoMatches(r, overviewFilter) : null;
        const filteredIssues = overviewRepoMatches ? overviewRepoMatches.issues.map(entry => entry.issue) : (r.actionableIssues || []).concat(r.heldIssues || []);
        const filteredNonActionableIssues = overviewRepoMatches ? [] : (r.nonActionableIssues || []);
        const filteredOpenPrs = overviewRepoMatches ? overviewRepoMatches.prs.filter(entry => !entry.held).map(entry => entry.pr) : (r.openPrs || []);
        const filteredHeldPrs = overviewRepoMatches ? overviewRepoMatches.prs.filter(entry => entry.held).map(entry => entry.pr) : (r.heldPrs || []);
        const filteredBuckets = overviewRepoMatches ? overviewRepoMatches.buckets : [];
        const overviewIssueKeys = overviewRepoMatches ? new Set(filteredIssues.map(i => String(i.number))) : null;
        const overviewPRKeys = overviewRepoMatches ? new Set(filteredOpenPrs.concat(filteredHeldPrs).map(p => String(p.number))) : null;
        const heldIssueKeys = new Set((r.heldIssues || []).map(i => String(i.number)));
        const repoIssueFilterRows = filteredIssues.map(i => ({ kinds: repoIssueFilterKinds(i, i, heldIssueKeys.has(String(i.number))) }));
        const repoNonActionableFilterRows = filteredNonActionableIssues.map(i => ({ kinds: ['issue:non-actionable', 'issue:' + repoNonActionableIssueBucketSpec(i).key].concat(issueCoveringPR(i) ? ['issue:linked-pr'] : []) }));
        const repoPRFilterRows = filteredOpenPrs.map(pr => ({ kinds: repoPRFilterKinds(pr, false, pr) })).concat(filteredHeldPrs.map(pr => ({ kinds: repoPRFilterKinds(pr, true, pr) })));
        const repoFilterCounts = repoPillFilterRowCounts(repoIssueFilterRows.concat(repoNonActionableFilterRows, repoPRFilterRows), pillFilter);
        const repoIssueGroups = repoCardIssueGroups(r, overviewIssueKeys);
        const renderIssuePill = (i, info) => {
          const title = i.title || '';
          const repoFull = r.full || r.name || '';
          const isHeldIssue = heldIssueKeys.has(String(i.number));
          const heldTip = isHeldIssue ? heldReason(i) : '';
          // Plan action (Phase 4): mint an epic from this issue and request
          // decomposition. data-action wires into the delegated click handler.
          // Only rendered at ACMM L5+, where the architect that decomposes the
          // epic is actually scheduled — below that the button is hidden.
          // An issue that already has a plan shows the plan's STATE instead of
          // the Plan button, and opens the plan review — so "is this epic
          // being worked, and by whom?" is answered from the pill, not by
          // hunting through the Plans modal (hivecommons/hive#8011). The
          // button stays only for issues with no plan yet.
          const linked = planLinks[repoFull + '#' + i.number];
          const planBtn = linked && !isHeldIssue
            ? planIssueChip(linked)
            : (!isHeldIssue && showPlan ? `<button class="repo-issue-plan pill-icon" data-action="planIssue" data-repo="${esc(repoFull)}" data-number="${i.number}" data-url="${esc(i.url || '')}" data-title="${esc(i.title || '')}" title="Plan this issue" aria-label="Plan this issue">📋</button>` : '');
          const designBtn = (!isHeldIssue && !linked && showDesign) ? `<button class="repo-issue-plan pill-icon" data-action="designIssue" data-repo="${esc(repoFull)}" data-number="${i.number}" data-url="${esc(i.url || '')}" data-title="${esc(i.title || '')}" title="Design before planning" aria-label="Design before planning">📐</button>` : '';
          const holdBtn = holdToggleChip(cardRepo, i, 'issue', isHeldIssue, canToggleHold, heldTip);
          const prBadge = isHeldIssue ? '' : issueCoveringPRBadge(i);
          const bandClass = info.band === 'ready' ? '' : ' ' + info.band;
          const heldClass = isHeldIssue ? ' held' : '';
          const staleClass = issueIsStale(i) ? ' stale' : '';
          const statusTip = (isHeldIssue ? heldTip + ' — band: ' : '') + issueBandTip(info.band) + (info.signals.length ? ' — ' + info.signals.map(s => s.label).join(', ') : '');
          const issueUrl = i.url || (repoUrl + cardForge.issuesPath + '/' + i.number);
          const rowKinds = repoIssueFilterKinds(i, info, isHeldIssue);
          const statusKind = isHeldIssue ? 'issue:held' : ('issue:' + info.band);
          return `<span class="repo-issue-pill-wrap repo-pill-row${repoPillFilterHiddenClass(rowKinds, pillFilter)}"${repoPillKindAttr(rowKinds)}><button type="button" class="repo-pill-status repo-issue-pill${bandClass}${heldClass}${staleClass} repo-pill-filter-toggle" title="${esc(statusTip)}"${repoPillFilterAttrs(statusKind)}>${esc(isHeldIssue ? 'held' : issueBandShortLabel(info.band))}</button><a class="repo-issue-pill repo-pill-main${bandClass}${heldClass}${staleClass}" href="${esc(issueUrl)}" target="_blank" rel="noopener" title="${esc(issueTip(i, info) + (isHeldIssue ? ' — ' + heldTip : ''))}"><span class="pill-num">#${i.number}</span><span class="pill-title">${esc(title)}</span>${issueSignalHTML(info)}</a>${repoPillActionCluster([prBadge, planBtn, designBtn, holdBtn])}</span>`;
        };
        const issuePills = repoIssueGroups.issueGroups.map(g => {
          const items = g.issues.map(entry => renderIssuePill(entry.issue, entry.info)).join('');
          return `<div class="repo-issue-band repo-issue-band-${esc(g.band)}"><div class="repo-issue-band-title" title="${esc(g.tip)}">${esc(g.label)} · ${g.issues.length}</div><div class="repo-issue-band-items">${items}</div></div>`;
        }).join('');
        const renderNonActionableIssuePill = (i) => {
          const spec = repoNonActionableIssueBucketSpec(i);
          const title = i.title || '';
          const issueUrl = i.url || (repoUrl + cardForge.issuesPath + '/' + i.number);
          const labels = (i.labels || []).map(l => String(l)).filter(Boolean).join(', ');
          const reason = i.reason || spec.tip;
          const tip = (i.title || '') + (i.author ? ' — @' + i.author : '') + (labels ? ' — labels: ' + labels : '') + ' — ' + reason;
          const prBadge = issueCoveringPRBadge(i);
          const rowKinds = ['issue:non-actionable', 'issue:' + spec.key].concat(prBadge ? ['issue:linked-pr'] : []);
          const nhClass = spec.key === 'needs_human' ? 'waiting' : 'held';
          return `<span class="repo-issue-pill-wrap repo-pill-row${repoPillFilterHiddenClass(rowKinds, pillFilter)}"${repoPillKindAttr(rowKinds)}><button type="button" class="repo-pill-status repo-issue-pill ${nhClass} repo-pill-filter-toggle" title="${esc(spec.tip)}"${repoPillFilterAttrs('issue:' + spec.key)}>${esc(spec.short)}</button><a class="repo-issue-pill repo-pill-main ${nhClass}" href="${esc(issueUrl)}" target="_blank" rel="noopener" title="${esc(tip)}"><span class="pill-num">#${i.number}</span><span class="pill-title">${esc(title)}</span><span class="pill-state" title="${esc(reason)}">ⓘ</span></a>${prBadge ? repoPillActionCluster([prBadge]) : ''}</span>`;
        };
        const nonActionableIssuePills = repoIssueGroups.nonActionableIssueGroups.map(g => {
          const items = g.issues.map(renderNonActionableIssuePill).join('');
          return `<div class="repo-issue-band repo-issue-band-${esc(g.key)}"><div class="repo-issue-band-title" title="${esc(g.tip)}">${esc(g.label)} · ${g.issues.length}</div><div class="repo-issue-band-items">${items}</div></div>`;
        }).join('');
        const renderPrPill = (p, held, info) => {
          const title = p.title || '';
          // Green ✓ only on the sweep's own verdict; amber ◐ for mergeable
          // on GitHub but not yet eligible; nothing otherwise (#7478).
          const mergeState = prMergeState(p);
          const mergeIcon = mergeState === 'eligible' ? '<span class="pill-merge-icon" title="Merge-eligible on the sweep verdict">✓</span>'
            : mergeState === 'outstanding' ? '<span class="pill-merge-icon" title="GitHub says mergeable; sweep still wants something">◐</span>' : '';
          const mergeClass = mergeState === 'eligible' ? ' mergeable' : mergeState === 'outstanding' ? ' merge-outstanding' : '';
          const prAge = pillAge(p.created_at);
          const activityAge = pillAge(p.updated_at || p.created_at);
          const labels = (p.labels || []).map(l => String(l));
          const queued = prQueued(p);
          // Escalated out of the automated fix lane: the escalation ledger
          // (pkg/escalation) applies the `needs-human` label once automated
          // fix attempts are exhausted. The label list is the single source
          // of truth — the same signal the ledger writes to GitHub — so this
          // view cannot drift from the ledger.
          const needsHuman = labels.map(l => l.toLowerCase()).includes('needs-human');
          const sentinelFlagged = prSentinelFlagged(p);
          // review_class is the presentational triage class the snapshot is
          // ordered by (fix > refactor-docs > tests, then age; #6183). It is
          // derived server-side from the title prefix and lane label only.
          const reviewClass = p.review_class ? String(p.review_class) : '';
          const bandInfo = info || p;
          const bandTip = ' [band: ' + prBandLabel(bandInfo.band) + ']';
          const activityTip = p.updated_at ? ' [last activity: ' + p.updated_at + (activityAge ? ' / ' + activityAge : '') + ']' : '';
          // GitHub review signals (#8968): decision, who a review is requested
          // from, conversation volume, and the issues the PR closes. All read
          // from the snapshot; the tooltip is where the full lists live.
          const review = prGitHubReview(p);
          const reviewDecisionTip = review ? ' [review: ' + review.label + ']' : '';
          const requested = prRequestedReviews(p);
          const requestedTip = requested.length ? ' [review requested: ' + requested.join(', ') + ']' : '';
          const convo = prConversation(p);
          const convoTip = convo ? ' [' + convo.label + ']' : '';
          const linkedIssues = prLinkedIssues(p);
          const linkedIssueText = linkedIssues.map(i => (i.repo && i.repo !== cardRepo ? i.repo : '') + '#' + i.number + (i.state ? ' ' + i.state : '')).join(', ');
          const linkedTip = linkedIssueText ? ' [closes: ' + linkedIssueText + ']' : '';
          const prTip = (p.title || '') + (p.author ? ' — @' + p.author : '') + (prAge ? ' (' + prAge + ')' : '') + bandTip + activityTip + (reviewClass ? ' [review class: ' + reviewClass + ']' : '') + reviewDecisionTip + requestedTip + convoTip + linkedTip + prMergeNote(p) + (queued ? ' (queued for auto-merge)' : '') + (needsHuman ? ' (needs human review)' : '') + (sentinelFlagged ? ' (sentinel flagged — not merged until a maintainer removes the label)' : '') + (held ? ' (' + heldReason(p) + ')' : '');
          const canQueue = dashboardRoleAtLeast(window._hiveRole || 'read', 'merger') && (window._hiveUser || dashboardRoleAtLeast(window._hiveRole || 'read', 'owner')) && !(p.author && window._hiveUser && p.author.toLowerCase() === window._hiveUser.toLowerCase());
          const parts = (r.full || r.name || '').split('/');
          const owner = parts.length > 1 ? parts[0] : '';
          const repoName = parts.length > 1 ? parts.slice(1).join('/') : (r.name || '');
          const notMergeable = p.mergeable === 'no';
          const notMergeableTip = 'Not mergeable on GitHub' + (p.mergeable_state ? ' (' + String(p.mergeable_state) + ')' : '') + ' — resolve that before queueing for auto-merge';
          const reviewTip = reviewPillTip(p);
          const needsHumanTip = 'Automated fix attempts exhausted — a human must review this PR. Remove the needs-human label after addressing the root cause to return it to the automated fix lane.';
          const reviewPill = p.review_url
            ? `<a class="repo-pr-pill pill-reviewed pill-icon" href="${esc(p.review_url)}" target="_blank" rel="noopener" data-stop="1" title="${esc(reviewTip)}" aria-label="${esc(reviewTip)}">💬${p.review_count > 1 ? '×' + p.review_count : ''}</a>`
            : '';
          const queueBtn = held
            ? ''
            : (needsHuman
                ? `<button type="button" class="repo-pr-pill needs-human pill-needs-human-badge pill-icon repo-pill-filter-toggle" title="${esc(needsHumanTip)}" aria-label="${esc(needsHumanTip)}"${repoPillFilterAttrs('needs-human')}>⚠</button>`
                : (canQueue
                    ? (queued
                        ? '<span class="repo-pr-pill mergeable pill-icon" title="Queued for Hive auto-merge" aria-label="Queued for Hive auto-merge">🔀</span>'
                        : (notMergeable
                            ? `<button class="repo-pr-pill pill-icon" disabled title="${esc(notMergeableTip)}" aria-label="${esc(notMergeableTip)}">🔀</button>`
                            : `<button class="hv-btn repo-pr-pill pill-icon btn-primary" data-action="queuePRAutoMerge" data-arg0="${esc(owner)}" data-arg1="${esc(repoName)}" data-arg2="${p.number}" data-arg-types="s,s,j,t" data-stop="1" data-prevent="1" title="Queue auto-merge when checks pass" aria-label="Queue auto-merge when checks pass">🔀</button>`))
                    : ''));
          const heldTip = held ? heldReason(p) : '';
          const holdBtn = holdToggleChip(cardRepo, p, 'pr', held, canToggleHold, heldTip);
          const stateBadge = held && needsHuman
            ? `<button type="button" class="repo-pr-pill needs-human pill-needs-human-badge pill-icon repo-pill-filter-toggle" title="${esc(heldTip)}" aria-label="${esc(heldTip)}"${repoPillFilterAttrs('needs-human')}>⚠</button>`
            : '';
          // 🔗 #N badge: the PR-side mirror of the issue pill's linked-PR
          // badge. The primary target is the first still-open issue, so the
          // link points at the work the PR is for; the tooltip lists all.
          const primaryIssue = linkedIssues.find(i => i.state === 'open') || linkedIssues[0];
          const issueBadgeTip = 'Closes issue' + (linkedIssues.length > 1 ? 's' : '') + ': ' + linkedIssueText;
          const issueBadge = primaryIssue
            ? `<a class="repo-pr-pill pill-icon" href="${esc(primaryIssue.url || (repoUrl + cardForge.issuesPath + '/' + primaryIssue.number))}" target="_blank" rel="noopener" data-stop="1" title="${esc(issueBadgeTip)}" aria-label="${esc('Linked issue #' + primaryIssue.number)}">🔗</a>`
            : '';
          const url = p.url || (repoUrl + cardForge.changePath + '/' + p.number);
          const heldClass = held ? ' held' : '';
          const staleClass = prIsStale(p) ? ' stale' : '';
          // needs-human wins the pill tint over mergeable green: a PR can be
          // technically mergeable AND escalated, and the escalation is what
          // the operator must see. Held adds the dashed outline without making
          // the PR a trailing append; it bands with other human gates.
          // A sentinel flag wins over everything, held included: Hive will
          // not merge the PR until a maintainer removes the label.
          const tintClass = sentinelFlagged ? ' sentinel-flagged' : (needsHuman ? ' needs-human' : mergeClass);
          const prStatusText = reviewClass ? (reviewClass === 'refactor-docs' ? 'refactor' : reviewClass) : (sentinelFlagged ? 'sentinel' : (held ? 'held' : 'PR'));
          const sentinelTip = sentinelFlagged ? 'Sentinel flagged: possible security override, privilege escalation or codebase damage — Hive will not merge it until a maintainer reviews it and removes the label. ' : '';
          const prStatusTip = sentinelTip + (reviewClass ? ((held ? 'Held PR — review class: ' : 'Review class: ') + reviewClass) : (held ? heldTip : 'Open pull request'));
          const rowKinds = repoPRFilterKinds(p, held, bandInfo);
          const statusKind = sentinelFlagged ? 'pr:sentinel' : held ? 'pr:held' : (needsHuman ? 'needs-human' : (mergeState === 'eligible' ? 'pr:eligible' : (mergeState === 'outstanding' ? 'pr:outstanding' : ((reviewClass === 'fix') ? 'pr:fix' : 'pr:open'))));
          return `<span class="repo-pr-pill-wrap repo-pill-row${repoPillFilterHiddenClass(rowKinds, pillFilter)}"${repoPillKindAttr(rowKinds)}><button type="button" class="repo-pill-status repo-pr-pill${heldClass}${tintClass} repo-pill-filter-toggle" title="${esc(prStatusTip)}"${repoPillFilterAttrs(statusKind)}>${esc(prStatusText)}</button><a class="repo-pr-pill repo-pill-main${heldClass}${tintClass}${staleClass}" href="${esc(url)}" target="_blank" rel="noopener" title="${esc(prTip)}"><span class="pill-num">#${p.number}</span><span class="pill-title">${esc(title)}</span>${mergeIcon}${prSignalHTML(bandInfo)}</a>${repoPillActionCluster([reviewPill, queueBtn + stateBadge, issueBadge, holdBtn])}</span>`;
        };
        const prPills = groupedRepoPRs(r.openPrs || [], r.heldPrs || []).map(g => {
          const shown = overviewPRKeys ? g.prs.filter(entry => overviewPRKeys.has(String(entry.pr.number))) : g.prs;
          if (!shown.length) return '';
          const items = shown.map(entry => renderPrPill(entry.pr, entry.held, entry.info)).join('');
          return `<div class="repo-pr-band repo-pr-band-${esc(g.band)}"><div class="repo-pr-band-title" title="${esc(g.tip)}">${esc(g.label)} · ${shown.length}</div><div class="repo-pr-band-items">${items}</div></div>`;
        }).join('');
        // Two columns under the two numbers: issue pills (actionable and held
        // grouped into the same display bands) under "issues", PR pills grouped
        // into state bands under "PRs". Position says what a pill is; tint says
        // what state it is in.
        const issueCol = (issuePills || nonActionableIssuePills) ? `<div class="repo-pill-run-title">ISSUES</div>${issuePills}${nonActionableIssuePills}` : '';
        const prCol = prPills ? `<div class="repo-pill-run-title">PULL REQUESTS</div>${prPills}` : '';
        const pillColClass = issueCol && prCol ? '' : (issueCol ? ' repo-pills-issues-only' : ' repo-pills-prs-only');
        const bucketHtml = overviewProjectBucketRowsHTML(filteredBuckets);
        const pillsHtml = ((issueCol || prCol)
          ? `<div class="repo-pills${pillColClass}"><div class="repo-pill-col repo-pill-col-issues">${issueCol}</div><div class="repo-pill-col repo-pill-col-prs">${prCol}</div></div>`
          : '') + (bucketHtml ? `<div class="repo-pills repo-pills-issues-only"><div class="repo-pill-col repo-pill-col-issues">${bucketHtml}</div></div>` : '');
        // Per-repo agent pause (#6203). The tooltip carries the provenance the
        // pill has no room for; without it a paused repo raises "who did this
        // and why" every time someone new looks at the dashboard.
        const pauseTip = r.paused
          ? 'Agents are paused on this repo'
            + (r.pausedBy ? ' by ' + r.pausedBy : '')
            + (r.pausedAt ? ' at ' + r.pausedAt : '')
            + (r.pauseReason ? ' — ' + r.pauseReason : '')
            + '. No agent writes to it and none is handed work on it; it is still watched, scored and counted here.'
          : '';
        const pausedPill = r.paused ? `<span class="repo-paused-pill" title="${esc(pauseTip)}">⏸ paused</span>` : '';
        // Fork badge (#11433): which upstream the fork follows, whether the
        // upstream watch is on for it, and when it was last checked.
        const forkPill = r.upstream || r.upstreamWatchOn ? (() => {
          const checked = r.upstreamLastCheck ? 'last checked ' + r.upstreamLastCheck : 'never checked';
          const tip = 'Fork of ' + (r.upstream || 'its upstream') + '. Upstream watch is ' + (r.upstreamWatchOn ? 'on' : 'off') + (r.upstreamWatchOn ? '; ' + checked : '') + '.';
          const label = '🍴 ' + (r.upstream || 'fork') + (r.upstreamWatchOn ? ' · watch on · ' + (r.upstreamLastCheck ? formatDashboardDateTime(r.upstreamLastCheck) : 'never checked') : ' · watch off');
          return `<span class="repo-fork-pill${r.upstreamWatchOn ? '' : ' off'}" title="${esc(tip)}">${esc(label)}</span>`;
        })() : '';
        const autoMergeOn = r.autoMerge !== false;
        const autoMergeLevelOK = Number((status && status.acmmLevel) || 0) >= 6;
        const swarming = activeSwarmRepo && activeSwarmRepo.toLowerCase() === cardRepo.toLowerCase();
        const swarmPill = swarming ? `<span class="repo-swarm-pill" title="${esc('This repository is being prioritized by the active swarm')}">🐝 swarm</span>` : '';
        // #15335: the effective current-base merge gate for this repo
        // (Features default or the repo's own override on the Repos tab).
        const currentBasePill = r.requireCurrentBase ? `<span class="repo-current-base-pill" title="${esc('Merge only on current base: a green PR whose base has moved is updated and re-tested before Hive merges it (Settings → Features, per-repo override on the Repos tab).')}">current-base</span>` : '';
        const canPauseRepo = canToggleHold;
        // Asymmetric gate (#9070): repo-write users may switch auto-merge OFF,
        // but only a verified owner may switch it back ON — mirror the server.
        const canSetAutoMerge = autoMergeLevelOK && (autoMergeOn ? canPauseRepo : dashboardRoleAtLeast(window._hiveRole || 'read', 'owner'));
        const autoMergeTip = !autoMergeLevelOK
          ? 'Requires Level 6'
          : (canSetAutoMerge
              ? (autoMergeOn ? 'Disable Hive auto-merge for this repo' : 'Enable Hive auto-merge for this repo')
              : (autoMergeOn ? 'Requires repo write or owner role to disable Hive auto-merge' : 'Requires owner role to enable Hive auto-merge'));
        const autoMergeBtn = `<label class="config-toggle repo-automerge-toggle" title="${esc(autoMergeTip)}"><span class="config-toggle-label">auto-merge</span><span class="config-toggle-switch repo-automerge-switch ${autoMergeOn ? 'on' : ''} ${canSetAutoMerge ? '' : 'disabled'}" role="switch" tabindex="0" aria-checked="${autoMergeOn ? 'true' : 'false'}" aria-label="${esc('auto-merge for ' + cardRepo)}" data-disabled="${canSetAutoMerge ? 'false' : 'true'}" ${canSetAutoMerge ? `data-action="toggleRepoAutoMerge" data-keydown-action="toggleRepoAutoMerge" data-keys="Enter, " data-arg0="${esc(cardRepo)}" data-arg1="${autoMergeOn ? 'true' : 'false'}" data-arg-types="s,j,t" data-stop="1" data-prevent="1"` : ''}></span></label>`;
        const writebackCtl = repoWritebackControlHtml(cardRepo, status);
        const pauseBtn = canPauseRepo
          ? `<button class="repo-pause-btn" data-action="toggleRepoPause" data-arg0="${esc(cardRepo)}" data-arg1="${r.paused ? 'true' : 'false'}" data-arg-types="s,j,t" data-stop="1" data-prevent="1" title="${r.paused ? 'Resume agent activity on this repo' : 'Quiet this repo — agents stop writing to it and stop being handed work on it — without stopping the rest of the hive'}">${r.paused ? '▶ resume' : '⏸ pause'}</button>`
          : '';
        const swarmLocked = status.swarm && status.swarm.unlocked === false;
        const swarmLockTip = swarmLocked
          ? `locked — ${Number(status.swarm.idle_pct || 0)}% idle / ${Number(status.swarm.unlock_idle_pct || 0)}% needed`
          : 'Start a 24h swarm prioritizing this repository';
        const swarmBtn = canPauseRepo && !activeSwarmRepo
          ? (swarmLocked
              ? `<button class="repo-swarm-btn" disabled aria-disabled="true" title="${esc(swarmLockTip)}">🐝 swarm</button><button class="repo-swarm-btn" data-action="startSwarm" data-arg0="${esc(cardRepo)}" data-arg2="true" data-arg-types="s,t,j" data-stop="1" data-prevent="1" title="${esc('Owner force start: ' + swarmLockTip)}">force swarm</button>`
              : `<button class="repo-swarm-btn" data-action="startSwarm" data-arg0="${esc(cardRepo)}" data-arg-types="s,t" data-stop="1" data-prevent="1" title="${esc(swarmLockTip)}">🐝 swarm</button>`)
          : '';
        // role="separator" + aria-orientation="vertical" is the window-splitter
        // pattern: a focusable handle that reports the width it controls, so
        // the arrow-key nudge is announced rather than silent.
        const resizeTip = 'Drag to set the width of the ' + cardRepo + ' card — double-click to reset it; ← / → nudge it when focused (Shift for a bigger step, Esc resets)';
        const resizeHandle = `<div class="repo-card-resize" role="separator" aria-orientation="vertical" tabindex="0" data-repo="${esc(cardRepo)}" aria-label="${esc(resizeTip)}" title="${esc(resizeTip)}" aria-valuemin="${REPO_CARD_MIN_W}" aria-valuemax="${REPO_CARD_MAX_W}" aria-valuenow="${cardW || REPO_CARD_DEFAULT_W}"></div>`;
        const orderHandle = `<button type="button" class="dashboard-grip repo-card-order-handle" draggable="true" data-repo-order-grip data-repo="${esc(cardRepoKey)}" data-stop="1" aria-label="Drag to reorder" title="Drag to reorder" aria-pressed="false">⠿</button>`;
        // Per-repo pause (#6203) styling keys off the pill rendered inside the
        // card (.repo-card:has(.repo-paused-pill)); the card's open tag is pinned
        // verbatim by TestRepoCardResizeHandleMarkup (#8000).
        return `
        <div class="repo-card${cardW ? ' repo-card-sized' : ''}" data-repo-order-key="${esc(cardRepoKey)}"${cardW ? ` style="flex-basis:${cardW}px"` : ''}>
          ${resizeHandle}
          ${cardForge.kind === 'github'
            ? `<div class="repo-provider-label" style="color:${isPublic ? 'var(--muted)' : 'var(--indigo)'}" title="This repo lives on ${esc(hostLabel)}${isPublic ? '' : ' (GitHub Enterprise), not public github.com'}">${esc(hostLabel)}${isPublic ? '' : ' · Enterprise'}</div>`
            : `<div class="repo-provider-label" data-forge="${esc(cardForge.kind)}" style="color:var(--indigo)" title="This repo lives on ${esc(cardForge.name)} at ${esc(cardForge.host)}">${cardForge.icon} ${esc(cardForge.name)} · ${esc(cardForge.host)}</div>`}
          <div class="repo-name">${orderHandle}<a href="${esc(repoUrl)}" target="_blank" rel="noopener" title="${esc(r.full || r.name)}" style="color:inherit;text-decoration:none">${esc(r.full || r.name)}</a><span class="repo-header-badges">${overviewFilter && overviewRepoMatches ? `<span class="repo-filtered-chip">${overviewRepoMatches.count} matching</span>` : ''}${pillFilterIsActive ? repoPillFilterClearChip(repoFilterCounts) : ''}${swarmPill}${currentBasePill}${forkPill}${pausedPill}</span>${autoMergeBtn}<span class="repo-header-spacer"></span><span class="repo-header-actions">${writebackCtl}${swarmBtn}${pauseBtn}</span></div>
          <div class="repo-stats">
            <div class="repo-stat"><a href="${esc(repoUrl + cardForge.issuesPath)}" target="_blank" rel="noopener" style="color:inherit;text-decoration:none"><div class="spark-row"><span class="num">${r.issues >= 0 ? r.issues : '?'}</span>${iSpark}</div><div class="label">issues</div></a>${issueBreakdown}</div>
            <div class="repo-stat"><a href="${esc(repoUrl + cardForge.changesPath)}" target="_blank" rel="noopener" style="color:inherit;text-decoration:none"><div class="spark-row"><span class="num">${r.prs >= 0 ? r.prs : '?'}</span>${pSpark}</div><div class="label">PRs</div></a>${prBreakdown}</div>
          </div>${pillsHtml}${hfRepoSectionHtml(r)}
        </div>`;
      }).join(''));
      hfRenderEditor();
      hfRenderPinned();
      hfRenderCrossRepo();
    }

    /**
     * Where a repo card points and what it calls its host (#14115). r.forge is
     * the repo's forge kind from /api/status; GitHub (and an older hive that
     * reports no kind) keeps the GitHub host from /api/config, while GitLab and
     * Gitea/Forgejo use r.forgeUrl and their own issue and change-request paths.
     */
    function repoForgeWeb(r, githubHost) {
      const kind = String((r && r.forge) || 'github').toLowerCase();
      const base = String((r && r.forgeUrl) || '').replace(/\/+$/, '');
      if (kind === 'gitlab' && base) {
        return { kind, icon: '🦊', name: 'GitLab', host: base.replace(/^https?:\/\//, ''), base, issuesPath: '/-/issues', changesPath: '/-/merge_requests', changePath: '/-/merge_requests' };
      }
      if (kind === 'gitea' && base) {
        return { kind, icon: '🍵', name: 'Gitea/Forgejo', host: base.replace(/^https?:\/\//, ''), base, issuesPath: '/issues', changesPath: '/pulls', changePath: '/pulls' };
      }
      return { kind: 'github', icon: '🐙', name: 'GitHub', host: githubHost, base: window._githubBaseUrl || 'https://github.com', issuesPath: '/issues', changesPath: '/pulls', changePath: '/pull' };
    }

    /**
     * Paint one card at w px (0 = back to the default) and keep its handle's
     * reported value in step. This is the live half of a drag; the stored map
     * is what survives the repaint.
     */
    function applyRepoCardWidth(card, w) {
      if (!card) return 0;
      const clamped = clampRepoCardWidth(w);
      if (clamped) {
        card.style.flexBasis = clamped + 'px';
        card.classList.add('repo-card-sized');
      } else {
        card.style.flexBasis = '';
        card.classList.remove('repo-card-sized');
      }
      const handle = card.querySelector('.repo-card-resize');
      if (handle) handle.setAttribute('aria-valuenow', String(clamped || REPO_CARD_DEFAULT_W));
      return clamped;
    }

    // Repaint from the last status snapshot so the pill titles re-cut at the
    // new width. It goes through renderRepos(), which re-reads the stored
    // map — the same path the governor's repaint takes — so a width has
    // exactly one source of truth however it was changed.
    function repaintReposForWidth() {
      renderRepos(((window._lastStatus || {}).repos) || []);
    }

    /** Header action: put every card back to the default width and order. */
    function resetRepoCardWidths() {
      try { localStorage.removeItem(repoCardWidthKey()); localStorage.removeItem(REPO_ORDER_KEY); } catch {}
      repaintReposForWidth();
      dashboardUpdateLayoutMenuState();
      showToast('Project card layout reset to the default', 'info');
    }

    function repoOrderSaveFromDom(grid) {
      const order = Array.from(grid.querySelectorAll('.repo-card[data-repo-order-key]')).map(card => card.getAttribute('data-repo-order-key')).filter(Boolean);
      repoOrderWrite(order, repoOrderDefault(((window._lastStatus || {}).repos) || []));
      const resetBtn = document.getElementById('repos-reset-layout-btn');
      if (resetBtn) resetBtn.style.display = (Object.keys(repoCardWidths()).length || repoOrderHasCustom(((window._lastStatus || {}).repos) || [])) ? 'inline-block' : 'none';
    }

    function repoOrderNextCard(card) {
      let next = card ? card.nextElementSibling : null;
      while (next && !next.classList.contains('repo-card')) next = next.nextElementSibling;
      return next;
    }

    function repoOrderDropSlot(grid, x, y, skip) {
      const cards = Array.from(grid.querySelectorAll('.repo-card[data-repo-order-key]')).filter(card => card !== skip);
      let best = null, bestDist = Infinity;
      cards.forEach(card => {
        const rect = card.getBoundingClientRect();
        const horizontal = Math.abs(y - (rect.top + rect.height / 2)) < rect.height;
        const before = horizontal ? x < (rect.left + rect.width / 2) : y < (rect.top + rect.height / 2);
        const anchorX = before ? rect.left : rect.right;
        const anchorY = rect.top + rect.height / 2;
        const dist = Math.abs(x - anchorX) + Math.abs(y - anchorY);
        if (dist < bestDist) { bestDist = dist; best = { before: before ? card : repoOrderNextCard(card) }; }
      });
      return best || { before: null };
    }

    function repoOrderMoveIndicator(grid, indicator, slot) {
      if (!grid || !indicator || !slot) return;
      grid.insertBefore(indicator, slot.before || null);
    }

    function repoOrderClearIndicator(grid) {
      if (grid) grid.querySelectorAll(':scope > .dashboard-drop-indicator').forEach(el => el.remove());
    }

    (function initRepoCardOrder() {
      const grid = document.getElementById('repos');
      if (!grid) return;
      const handleOf = (e) => (e.target && e.target.closest) ? e.target.closest('[data-repo-order-grip]') : null;
      function cleanup() {
        if (!_repoOrderDrag) return;
        _repoOrderDrag.card.classList.remove('dashboard-drag-source');
        _repoOrderDrag.handle.classList.remove('dragging');
        _repoOrderDrag.handle.setAttribute('aria-pressed', 'false');
        repoOrderClearIndicator(grid);
        _repoOrderDrag = null;
      }
      grid.addEventListener('dragstart', (e) => {
        const handle = handleOf(e);
        if (!handle) return;
        const card = handle.closest('.repo-card');
        if (!card) return;
        const indicator = document.createElement('div');
        indicator.className = 'dashboard-drop-indicator';
        _repoOrderDrag = { handle, card, indicator };
        card.classList.add('dashboard-drag-source');
        handle.classList.add('dragging');
        handle.setAttribute('aria-pressed', 'true');
        e.dataTransfer.effectAllowed = 'move';
        e.dataTransfer.setData('text/plain', card.getAttribute('data-repo-order-key') || '');
      });
      grid.addEventListener('dragover', (e) => {
        if (!_repoOrderDrag) return;
        e.preventDefault();
        e.dataTransfer.dropEffect = 'move';
        const slot = repoOrderDropSlot(grid, e.clientX, e.clientY, _repoOrderDrag.card);
        _repoOrderDrag.slot = slot;
        repoOrderMoveIndicator(grid, _repoOrderDrag.indicator, slot);
      });
      grid.addEventListener('drop', (e) => {
        if (!_repoOrderDrag) return;
        e.preventDefault();
        const drag = _repoOrderDrag;
        const slot = drag.slot || repoOrderDropSlot(grid, e.clientX, e.clientY, drag.card);
        repoOrderClearIndicator(grid);
        grid.insertBefore(drag.card, slot.before || null);
        repoOrderSaveFromDom(grid);
        showToast('Project tile order saved', 'info');
        cleanup();
      });
      grid.addEventListener('dragend', cleanup);
      grid.addEventListener('keydown', (e) => {
        const handle = handleOf(e);
        if (!handle) return;
        const card = handle.closest('.repo-card');
        if (!card) return;
        if (e.key === ' ' || e.key === 'Enter') {
          e.preventDefault();
          handle.setAttribute('aria-pressed', handle.getAttribute('aria-pressed') === 'true' ? 'false' : 'true');
          return;
        }
        if (e.key !== 'ArrowLeft' && e.key !== 'ArrowUp' && e.key !== 'ArrowRight' && e.key !== 'ArrowDown') return;
        e.preventDefault();
        if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') {
          const prev = card.previousElementSibling && card.previousElementSibling.classList.contains('repo-card') ? card.previousElementSibling : null;
          if (prev) grid.insertBefore(card, prev);
        } else {
          const next = card.nextElementSibling && card.nextElementSibling.classList.contains('repo-card') ? card.nextElementSibling : null;
          if (next) grid.insertBefore(card, next.nextElementSibling);
        }
        repoOrderSaveFromDom(grid);
        handle.focus();
      });
    })();

    (function initRepoCardResize() {
      const grid = document.getElementById('repos');
      if (!grid) return;
      const handleOf = (e) => (e.target && e.target.closest) ? e.target.closest('.repo-card-resize') : null;

      // A repaint replaces every handle node, so a keyboard user would lose
      // focus mid-nudge. Debounce it, then put focus back on the handle for
      // the same repo. Held-down arrow keys therefore repaint once, not once
      // per key repeat.
      let repaintTimer = 0;
      function scheduleRepaint(repo, refocus) {
        clearTimeout(repaintTimer);
        repaintTimer = setTimeout(() => {
          repaintReposForWidth();
          if (!refocus) return;
          const again = Array.from(grid.querySelectorAll('.repo-card-resize')).find(h => h.dataset.repo === repo);
          if (again) again.focus();
        }, 200);
      }

      grid.addEventListener('pointerdown', (e) => {
        const handle = handleOf(e);
        if (!handle || e.button > 0) return;
        const card = handle.closest('.repo-card');
        if (!card) return;
        _repoResizeDrag = {
          handle, card,
          repo: handle.dataset.repo || '',
          startX: e.clientX,
          startW: card.getBoundingClientRect().width,
        };
        try { handle.setPointerCapture(e.pointerId); } catch {}
        handle.classList.add('dragging');
        e.preventDefault();
      });

      grid.addEventListener('pointermove', (e) => {
        if (!_repoResizeDrag) return;
        // Persist what the drag ASKED for, not what the card measures: a card
        // alone on a line narrower than the width chosen renders shrunk, and
        // storing that measurement would silently ratchet the width down on
        // every drag at a narrow viewport.
        // Floored at the minimum here rather than left to clampRepoCardWidth:
        // a drag that overshoots past zero means "as narrow as it goes", not
        // "put this card back to the default".
        const want = Math.max(REPO_CARD_MIN_W, _repoResizeDrag.startW + (e.clientX - _repoResizeDrag.startX));
        _repoResizeDrag.w = applyRepoCardWidth(_repoResizeDrag.card, want);
      });

      function endDrag() {
        const drag = _repoResizeDrag;
        if (!drag) return;
        _repoResizeDrag = null;
        drag.handle.classList.remove('dragging');
        // No pointermove at all is a click on the handle, not a resize: leave
        // the card as it was rather than pinning it at its current width.
        if (drag.w === undefined) return;
        setRepoCardWidth(drag.repo, drag.w);
        scheduleRepaint(drag.repo, false);
      }
      grid.addEventListener('pointerup', endDrag);
      grid.addEventListener('pointercancel', endDrag);

      // Double-click the handle resets just that card.
      grid.addEventListener('dblclick', (e) => {
        const handle = handleOf(e);
        if (!handle) return;
        e.preventDefault();
        const repo = handle.dataset.repo || '';
        applyRepoCardWidth(handle.closest('.repo-card'), 0);
        setRepoCardWidth(repo, 0);
        scheduleRepaint(repo, false);
      });

      // The handle is focusable, so it has to be operable from the keyboard —
      // the same reason the rest of this page's controls are.
      grid.addEventListener('keydown', (e) => {
        const handle = handleOf(e);
        if (!handle) return;
        const card = handle.closest('.repo-card');
        if (!card) return;
        const step = e.shiftKey ? REPO_CARD_NUDGE_BIG_W : REPO_CARD_NUDGE_W;
        const cur = card.getBoundingClientRect().width;
        let next;
        if (e.key === 'ArrowRight') next = cur + step;
        else if (e.key === 'ArrowLeft') next = Math.max(REPO_CARD_MIN_W, cur - step);
        else if (e.key === 'Home') next = REPO_CARD_MIN_W;
        else if (e.key === 'End') next = REPO_CARD_MAX_W;
        else if (e.key === 'Escape') next = 0;
        else return;
        e.preventDefault();
        const repo = handle.dataset.repo || '';
        applyRepoCardWidth(card, next);
        setRepoCardWidth(repo, next);
        scheduleRepaint(repo, true);
      });
    })();

    // Operator-initiated repository rescan. The REPOSITORIES cards repaint
    // from the governor's eval cycle, whose cadence stretches to minutes on a
    // quiet hive — so an operator who has just closed an issue or opened a PR
    // is stuck looking at stale pills with no way to ask for a fresh read.
    // This button asks for one.
    //
    // Modelled on the ACMM Re-evaluate button next door: the server enforces
    // its own debounce, so holding this down cannot spend the GitHub API
    // budget, and a debounced reply means "already this fresh" — it is
    // reported as such, never as a failure. The scan is read-only: it
    // re-enumerates issues and PRs and does NOT kick any agent.
    async function reposForceRescan() {
      const btn = document.getElementById('repos-rescan-btn');
      if (btn && btn.disabled) return;
      const prev = btn ? btn.textContent : '';
      if (btn) { btn.disabled = true; btn.textContent = '\u23f3 Scanning...'; }
      try {
        const r = await fetch('/api/repos/rescan', { method: 'POST', headers: _inceptionAuthHeaders() });
        let d = {};
        try { d = await r.json(); } catch { d = {}; }
        if (!r.ok) {
          showToast('Project rescan failed: ' + (d.error || ('HTTP ' + r.status)), 'error');
          return;
        }
        // The server republishes the status snapshot as part of the scan, so
        // re-fetch it here rather than waiting for the next poll to repaint.
        await refreshStatus();
        if (d.status === 'debounced') {
          showToast('Projects are already up to date - next scan in ' + (d.retry_after_s || 0) + 's', 'info');
        } else if (d.status === 'in-progress') {
          showToast('A project scan is already running', 'info');
        } else {
          const n = d.repos || 0;
          showToast('Scanned ' + n + ' project' + (n === 1 ? '' : 's') + ' - ' + (d.issues || 0) + ' issues, ' + (d.prs || 0) + ' PRs', 'success');
        }
      } catch (e) {
        showToast('Project rescan failed: ' + e.message, 'error');
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = prev; }
      }
    }

    // Contributors-style empty-state card, reused by sections that would
    // otherwise show bare/blank content on a new hive.
    function emptyStateCard(icon, title, bodyHtml) {
      return '<div class="empty-state">' +
        `<h3><span class="empty-state-icon">${icon}</span> ${title}</h3>` +
        `<p>${bodyHtml}</p>` +
      '</div>';
    }

    // fmtTokens keeps at least two significant digits at every magnitude, so
    // 176,372 output tokens read "176K" rather than the "0.000B" the
    // billions-only scale produced for every sub-million count (#13484).
    function fmtTokens(n) {
      n = Number(n) || 0;
      if (n === 0) return '0';
      const scale = (v, unit) => {
        if (v >= 100) return v.toFixed(0) + unit;
        if (v >= 10) return v.toFixed(1) + unit;
        return v.toFixed(2) + unit;
      };
      if (n >= 1e9) return scale(n / 1e9, 'B');
      if (n >= 1e6) return scale(n / 1e6, 'M');
      if (n >= 1e3) return scale(n / 1e3, 'K');
      return String(Math.round(n));
    }
    function pillAge(createdAt) {
      if (!createdAt) return '';
      const MS_PER_MIN = 60000;
      const MS_PER_HOUR = 3600000;
      const MS_PER_DAY = 86400000;
      const ms = Date.now() - new Date(createdAt).getTime();
      if (ms < 0 || isNaN(ms)) return '';
      if (ms < MS_PER_HOUR) return Math.floor(ms / MS_PER_MIN) + 'm ago';
      if (ms < MS_PER_DAY) return Math.floor(ms / MS_PER_HOUR) + 'h ago';
      return Math.floor(ms / MS_PER_DAY) + 'd ago';
    }

    // reviewPillTip explains the 💬 pill: which review it links to and when.
    // A count above 1 is named explicitly — that is the duplicate-review
    // symptom, and an operator should see it rather than have it hidden
    // behind a pill that looks identical either way.
    function reviewPillTip(p) {
      const state = p.review_state ? String(p.review_state).replace(/_/g, ' ') : 'commented';
      const when = pillAge(p.reviewed_at);
      let tip = 'Hive reviewed this PR (' + state + ')' + (when ? ' ' + when : '') + ' — click to open the review';
      if (p.review_count > 1) {
        tip += '. The hive has posted ' + p.review_count + ' reviews on this PR; the link goes to the most recent.';
      }
      return tip;
    }

    let budgetIgnored = false;
    // fetch('/api/budget-ignore') — deferred to _deferredInit

    // Re-fetch /api/status and re-render so the budget bar/banners reflect the
    // new server state without a full page reload. Render errors are swallowed:
    // callers use this after an action already succeeded, so a refresh hiccup
    // must never be reported as a failure of that action (the next SSE/poll
    // cycle repaints anyway).
    function refreshStatus() {
      return fetch('/api/status').then(r => r.json()).then(render).catch(() => {});
    }

    function toggleBudgetIgnore() {
      budgetIgnored = !budgetIgnored;
      fetch('/api/budget-ignore', { method: 'POST', headers: _inceptionAuthHeaders(), body: JSON.stringify({ ignored: budgetIgnored }) })
        .then(r => r.json()).then(d => { budgetIgnored = d.ignored; refreshStatus(); });
    }

    // Opens a fresh budget window: spend re-anchors at 0, the exhausted gate
    // reopens, budgeting stays ON. The recovery path when a mis-sized (or
    // genuinely spent) budget is suppressing kicks — unlike "ignore budget",
    // which turns enforcement off entirely.
    async function resetBudgetWindow() {
      // The try/catch covers only the API call: once the server has reset the
      // window, a post-success UI refresh failure must not be mislabeled as
      // "Failed to reset budget window" (that exact double-toast shipped once,
      // when the success path called an undefined render helper).
      let r;
      try {
        r = await fetch('/api/config/governor/budget/reset', { method: 'POST', headers: _inceptionAuthHeaders() });
      } catch (e) {
        showToast('Failed to reset budget window: ' + e.message, 'error');
        return;
      }
      if (!r.ok) {
        const d = await r.json().catch(() => ({}));
        showToast(d.error || ('Failed to reset budget window (HTTP ' + r.status + ')'), 'error');
        return;
      }
      // Same stale-snapshot race as the restart counter (#4348): raise the
      // floor so a pre-reset poll can't repaint the exhausted budget bar
      // before the post-reset snapshot lands. Body parse is best-effort.
      const ok = await r.json().catch(() => ({}));
      noteStatusMutation(ok.minStatusSeq);
      showToast('Budget window reset — spend restarts at 0, suppressed kicks resume', 'success');
      fetchBudgetHistory(); // a reset closes a window → new history row; refetch so the chart shows it
      refreshStatus(); // repaint budget bar/banners in place; errors swallowed inside
    }

