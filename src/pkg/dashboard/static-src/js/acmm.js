    function acmmTrackerAvailable() {
      return !!(_acmmEvalData && _acmmEvalData.work_source_type === 'linear');
    }
    function acmmEffectiveTracker() {
      if (_acmmTracker) return _acmmTracker;
      return (_acmmEvalData && _acmmEvalData.issue_tracker) || 'github';
    }
    // The API's `tracker` field speaks github|work_source; the UI speaks the
    // destination (github|linear). Omit the field to keep the server default.
    function acmmIssuePayload(repo, criterionId, level) {
      const body = {repo, criterion_id: criterionId, criterion_level: level};
      if (_acmmTracker && acmmTrackerAvailable()) body.tracker = _acmmTracker === 'linear' ? 'work_source' : 'github';
      return body;
    }
    function acmmRememberIssue(key, data) {
      _acmmOpenedIssues[key] = {number: data.issue_number, url: data.issue_url, identifier: data.identifier || '', tracker: data.tracker || 'github'};
      return _acmmOpenedIssues[key];
    }
    // "ENG-42" for Linear, "#42" for GitHub.
    function acmmIssueRef(issue) {
      return issue.identifier ? issue.identifier : ('#' + issue.number);
    }
    function acmmTrackerName(tracker) { return tracker === 'linear' ? 'Linear' : 'GitHub'; }
    function acmmTrackerSelector() {
      if (!acmmTrackerAvailable()) return '';
      const cur = acmmEffectiveTracker();
      return '<label class="muted-xs" style="margin-left:var(--sp-4);display:inline-flex;align-items:center;gap:var(--sp-2)" title="Where Open Issue / Open All file the gap issue. Default comes from governor.acmm.issue_tracker.">File in ' +
        '<select data-change-action="acmmSetTracker" data-arg-types="t" style="font-size:0.72rem;padding:var(--sp-1) var(--sp-2);background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);color:var(--text)">' +
        '<option value="github"' + (cur === 'github' ? ' selected' : '') + '>GitHub</option>' +
        '<option value="linear"' + (cur === 'linear' ? ' selected' : '') + '>Linear</option>' +
        '</select></label>';
    }
    function acmmSetTracker(el) {
      _acmmTracker = el && el.value === 'linear' ? 'linear' : 'github';
    }

    function acmmGetActiveData() {
      if (!_acmmEvalData) return null;
      if (!_acmmSelectedRepo || !_acmmEvalData.repo_results) return _acmmEvalData;
      return (_acmmEvalData.repo_results || []).find(r => r.repo === _acmmSelectedRepo) || _acmmEvalData;
    }

    // acmmLevelWaivedCriteria returns the waived criteria at one level within
    // a scope (a repo_results entry, or the aggregate).
    function acmmLevelWaivedCriteria(source, level) {
      if (!source) return [];
      return (source.criteria_results || source.CriteriaResults || [])
        .filter(c => c.level === level && c.waived);
    }

    // acmmLevelWaiverMark is the asterisk that rides beside a level's
    // pass/fail glyph.
    //
    // A waived criterion scores, so a level can read a clean 9/9 in green
    // while part of that nine is a declaration rather than a detection. The
    // level bars are the altitude most operators actually read — the criterion
    // rows are two clicks down — so without a mark here the waiver is
    // invisible exactly where the summary judgement gets made. Red against the
    // green bar because it should not blend in: the point is to interrupt
    // someone skimming for green.
    function acmmLevelWaiverMark(source, level) {
      const waived = acmmLevelWaivedCriteria(source, level);
      if (!waived.length) return '';
      const names = waived.map(c =>
        c.name + (c.waiver_satisfied_by ? ' (satisfied by ' + c.waiver_satisfied_by + ')' : '')).join('; ');
      const lead = waived.length === 1
        ? '1 criterion at this level is waived, not detected: '
        : waived.length + ' criteria at this level are waived, not detected: ';

      // The level's own pass/fail ignores waivers, so a level can show a
      // full ratio and still not pass. Say so here rather than leaving the
      // reader to reconcile "9/9" with "✗" on their own.
      const lvl = (source && (source.levels || source.Levels) || []).find(l => l.level === level);
      let rule = ' Waivers restore full green but never advance a level.';
      if (lvl && !lvl.passed && lvl.detected !== undefined) {
        rule = ' Level not passed: ' + lvl.detected + '/' + lvl.total + ' detected, below the ' +
          Math.round((lvl.threshold || 0.7) * 100) + '% threshold. Waivers cannot advance a level.';
      }
      return '<span style="color:var(--red);font-weight:700;margin-left:3px;cursor:help" title="' +
        escapeHtml(lead + names + '.' + rule) + '">*</span>';
    }

    // acmmWaiverDetail renders the justification a repo committed in
    // .acmm.yml. It is shown inside the expanded row, above the pattern
    // list, so a reader who opens a green row that matches none of its
    // patterns finds the explanation in the same place they found the
    // contradiction.
    function acmmWaiverDetail(c) {
      if (!c.waived) return '';
      const where = c.waiver_satisfied_by || 'declared in .acmm.yml';
      const inRepo = c.waiver_repo ? ' (waived in ' + escapeHtml(c.waiver_repo) + ')' : '';
      return '<div style="font-size:0.72rem;border-left:2px solid var(--green);padding:var(--sp-1) var(--sp-0) var(--sp-1) var(--sp-4);margin-bottom:var(--sp-4)">' +
        '<div style="color:var(--green);font-weight:500">⌾ Waived — satisfied by ' + escapeHtml(where) + inRepo + '</div>' +
        (c.waiver_reason ? '<div style="color:var(--muted);margin-top:var(--sp-1);white-space:pre-wrap">' + escapeHtml(c.waiver_reason) + '</div>' : '') +
        '</div>';
    }

    // acmmCreditChip names what satisfied a criterion when it was not a plain
    // file detection: Hive-verified evidence (satisfied_by, which counts toward
    // the level) or a marker file Hive could not verify (file_only).
    function acmmCreditChip(c) {
      if (!c || !c.passed) return '';
      if (c.satisfied_by) {
        return '<span style="font-size:var(--fs-2xs);padding:1px 6px;border-radius:9px;border:1px solid var(--green);color:var(--green);white-space:nowrap"' +
          ' title="' + escapeHtml(c.satisfied_reason || ('Satisfied by ' + c.satisfied_by)) + '">✓ satisfied by ' + escapeHtml(c.satisfied_by) + '</span>';
      }
      if (c.file_only) {
        return '<span style="font-size:var(--fs-2xs);padding:1px 6px;border-radius:9px;border:1px solid var(--border);color:var(--muted);white-space:nowrap"' +
          ' title="Passes because a marker file exists; Hive could not verify the capability on the default branch">file only, not verified</span>';
      }
      return '';
    }

    // acmmMergeLaneAction offers an owner the serialized merge lane on a
    // failing acmm:merge-queue row (POST /api/repos/merge-strategy). When the
    // lane is already on, the row says what is still missing instead.
    function acmmMergeLaneAction(c, repo) {
      if (!c || c.passed || c.id !== 'acmm:merge-queue' || !repo) return '';
      if (window._hiveRole !== 'owner') return '';
      const rr = ((_acmmEvalData && _acmmEvalData.repo_results) || []).find(r => r.repo === repo);
      if (rr && rr.merge_strategy === 'hive-serialized') {
        const own = (rr.criteria_results || []).find(r => r.id === c.id);
        const why = c.unsatisfied_reason || (own && own.unsatisfied_reason) || '';
        const title = 'The serialized merge lane is on; the default branch also needs at least one required status check' + (why ? ' — ' + why : '');
        return '<span class="muted-xs" style="white-space:nowrap" title="' + escapeHtml(title) + '">Lane on — needs required checks' + (why ? ' (' + escapeHtml(why) + ')' : '') + '</span>';
      }
      return '<button data-action="acmmUseSerializedLane" data-arg0="' + escapeHtml(repo) + '" data-arg-types="s,t" data-stop="1" ' +
        'style="padding:3px 10px;font-size:0.7rem;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);cursor:pointer;color:var(--muted);white-space:nowrap" ' +
        'title="Set this repository\'s merge strategy to hive-serialized. With required checks on the default branch this satisfies the check.">🔀 Use serialized merge lane</button>';
    }

    // acmmLabelNote describes the labels a created GitHub issue carries, from
    // the server's response when it reports them.
    function acmmLabelNote(labels, fallback) {
      if (!Array.isArray(labels)) return fallback;
      if (!labels.length) return '.';
      return ' with ' + labels.map(l => '<code>' + escapeHtml(l) + '</code>').join(' + ') + (labels.length === 1 ? ' label.' : ' labels.');
    }

    async function acmmUseSerializedLane(repo, btn) {
      const origText = btn.textContent;
      btn.textContent = '⏳ Saving...';
      btn.disabled = true;
      try {
        const r = await fetch('/api/repos/merge-strategy', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({repo: repo, merge_strategy: 'hive-serialized'})
        });
        if (!r.ok) {
          btn.textContent = '⚠ Failed';
          btn.title = (await r.text()).trim();
          setTimeout(() => { btn.textContent = origText; btn.disabled = false; }, 3000);
          return;
        }
        const rr = ((_acmmEvalData && _acmmEvalData.repo_results) || []).find(x => x.repo === repo);
        if (rr) rr.merge_strategy = 'hive-serialized';
        btn.textContent = '✅ Serialized merge lane on';
        btn.title = 'Re-evaluate once the default branch has required checks';
      } catch (e) {
        btn.textContent = '⚠ Error';
        setTimeout(() => { btn.textContent = origText; btn.disabled = false; }, 3000);
      }
    }

    function acmmCriterionRow(c, repoName) {
      const icon = c.passed ? '✅' : '❌';
      const tip = ACMM_CRITERION_TIPS[c.id] || '';
      // A waived criterion is passed — it scores, and the level reads full
      // green — but the row says the evidence is off-repo. Rendering it
      // identically to a detection would make the panel unable to tell the
      // operator why nothing here matches the pattern list below.
      const waivedChip = c.waived
        ? '<span style="font-size:var(--fs-2xs);padding:1px 6px;border-radius:9px;border:1px solid var(--green);color:var(--green);white-space:nowrap"' +
          ' title="' + escapeHtml('Satisfied by ' + (c.waiver_satisfied_by || 'a declaration in .acmm.yml') + ' rather than by a file in this repo') + '">⌾ waived</span>'
        : '';
      const patternItems = (c.patterns || []).map(p => '<span style="display:inline-block;font-size:var(--fs-xs);background:var(--bg);padding:1px 6px;border-radius:3px;border:1px solid var(--border);margin:1px 2px;font-family:var(--font-mono)">' + escapeHtml(p) + '</span>').join('');
      let issueBtn = '';
      if (!c.passed) {
        const repo = repoName || _acmmSelectedRepo || '';
        const issueKey = repo + ':' + c.id;
        const existing = _acmmOpenedIssues[issueKey];
        if (existing) {
          issueBtn = '<span style="margin-left:auto;padding:3px 10px;font-size:0.7rem;border:1px solid var(--green);border-radius:var(--r-sm);color:var(--muted);white-space:nowrap">✅ <a href="' + escapeHtml(existing.url) + '" target="_blank" class="muted-xs">' + escapeHtml(acmmIssueRef(existing)) + '</a></span>';
        } else {
          const escapedId = escapeHtml(c.id).replace(/'/g, "\\'");
          const escapedRepo = escapeHtml(repo).replace(/'/g, "\\'");
          issueBtn = '<button data-action="acmmCreateIssue" data-arg0="' + escapeHtml(repo) + '" data-arg1="' + escapeHtml(c.id) + '" data-arg2="' + c.level + '" data-arg-types="s,s,j,t" data-stop="1" ' +
            'style="margin-left:auto;padding:3px 10px;font-size:0.7rem;background:var(--bg);border:1px solid var(--border);border-radius:var(--r-sm);cursor:pointer;color:var(--muted);white-space:nowrap" ' +
            'title="Open a ' + acmmTrackerName(acmmEffectiveTracker()) + ' issue for agents to fix this">📋 Open Issue</button>';
        }
      }
      const rowId = 'acmm-row-' + c.id.replace(/[^a-zA-Z0-9]/g, '-');
      return '<div style="padding:8px 10px;margin-bottom:var(--sp-2);background:var(--bg);border-radius:var(--r);border:1px solid var(--border)"' +
        (tip ? ' title="' + escapeHtml(tip) + '"' : '') + '>' +
        '<div style="display:flex;align-items:center;gap:var(--sp-4);cursor:pointer" data-action="ghToggleDisplayById" data-arg0="' + rowId + '">' +
        '<span style="font-size:0.9rem">' + icon + '</span>' +
        '<span style="font-size:var(--fs-base);font-weight:500;flex:1">' + escapeHtml(c.name) + '</span>' +
        waivedChip +
        acmmCreditChip(c) +
        acmmMergeLaneAction(c, repoName || _acmmSelectedRepo || ((_acmmEvalData && (_acmmEvalData.repo_results || []).length === 1) ? _acmmEvalData.repo_results[0].repo : '')) +
        issueBtn +
        '</div>' +
        '<div id="' + rowId + '" style="display:none;margin-top:var(--sp-3);padding-top:var(--sp-3);border-top:1px solid var(--border)">' +
        (tip ? '<div style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:var(--sp-3);font-style:italic">' + escapeHtml(tip) + '</div>' : '') +
        acmmWaiverDetail(c) +
        (c.passed && c.satisfied_by ? '<div style="font-size:0.72rem;color:var(--green);margin-bottom:var(--sp-4)">✓ Satisfied by ' + escapeHtml(c.satisfied_by) + (c.satisfied_reason ? ' — ' + escapeHtml(c.satisfied_reason) : '') + '</div>' : '') +
        '<div style="font-size:var(--fs-xs);color:var(--muted);margin-bottom:3px">Looks for any of:</div>' +
        '<div style="line-height:1.6">' + patternItems + '</div>' +
        '</div></div>';
    }

    async function acmmCreateIssue(repo, criterionId, level, btn) {
      if (!repo) {
        const repos = (_acmmEvalData.repo_results || []).map(r => r.repo);
        if (repos.length === 1) {
          repo = repos[0];
        } else {
          acmmShowRepoPickerForIssue(criterionId, level);
          return;
        }
      }
      const origText = btn.textContent;
      btn.textContent = '⏳ Creating...';
      btn.disabled = true;
      try {
        const r = await fetch('/api/acmm/issue', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(acmmIssuePayload(repo, criterionId, level))
        });
        if (!r.ok) {
          const errText = await r.text();
          btn.textContent = '⚠ Failed';
          btn.title = errText;
          setTimeout(() => { btn.textContent = origText; btn.disabled = false; }, 3000);
          return;
        }
        const data = await r.json();
        const opened = acmmRememberIssue(repo + ':' + criterionId, data);
        btn.innerHTML = '✅ <a href="' + escapeHtml(data.issue_url) + '" target="_blank" class="muted-xs">' + escapeHtml(acmmIssueRef(opened)) + '</a>';
        btn.title = 'Opened on ' + acmmTrackerName(opened.tracker);
        btn.style.borderColor = 'var(--green)';
      } catch (e) {
        btn.textContent = '⚠ Error';
        setTimeout(() => { btn.textContent = origText; btn.disabled = false; }, 3000);
      }
    }

    function acmmShowRepoPickerForIssue(criterionId, level) {
      const repos = (_acmmEvalData.repo_results || []).map(r => r.repo);
      let html = '<p class="dialog-note">Which repository should this issue be opened on?</p>';
      for (const repo of repos) {
        const repoData = (_acmmEvalData.repo_results || []).find(r => r.repo === repo);
        const criterion = repoData ? (repoData.criteria_results || []).find(c => c.id === criterionId) : null;
        const already = criterion && criterion.passed;
        const escapedId = escapeHtml(criterionId).replace(/'/g, "\\'");
        const escapedRepo = escapeHtml(repo).replace(/'/g, "\\'");
        if (already) {
          html += '<div style="padding:var(--sp-4);margin-bottom:var(--sp-2);background:var(--bg);border-radius:var(--r);border:1px solid var(--border);opacity:0.5">' +
            '<span class="text-base">✅ ' + escapeHtml(repo) + '</span>' +
            '<span style="font-size:0.72rem;color:var(--muted);margin-left:var(--sp-4)">(already passing)</span></div>';
        } else {
          html += '<div data-action="ghAcmmCreateIssueDirect" data-arg0="' + escapeHtml(repo) + '" data-arg1="' + escapeHtml(criterionId) + '" data-arg2="' + level + '" data-arg-types="s,s,j" ' +
            'class="row-card" ' +
            'data-mouseover-action="gh22" data-mouseout-action="gh21">' +
            '<span class="text-base">❌ ' + escapeHtml(repo) + '</span></div>';
        }
      }
      acmmShowDialog('Open Issue — Select Repository', html);
    }

    async function acmmCreateIssueDirectFromPicker(repo, criterionId, level) {
      try {
        const r = await fetch('/api/acmm/issue', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify(acmmIssuePayload(repo, criterionId, level))
        });
        if (!r.ok) {
          const errText = await r.text();
          acmmShowDialog('Issue Creation Failed', '<p class="error-note">' + escapeHtml(errText) + '</p>');
          return;
        }
        const data = await r.json();
        const opened = acmmRememberIssue(repo + ':' + criterionId, data);
        const where = opened.tracker === 'linear'
          ? ' in Linear (team <code>' + escapeHtml(data.team || '') + '</code>) for <strong>' + escapeHtml(repo) + '</strong>.'
          : ' created on <strong>' + escapeHtml(repo) + '</strong>' + acmmLabelNote(data.labels, ' with <code>acmm</code> and <code>ai-fix-requested</code> labels.');
        acmmShowDialog('Issue Created', '<p class="text-base">Issue <a class="text-info" href="' + escapeHtml(data.issue_url) + '" target="_blank">' + escapeHtml(acmmIssueRef(opened)) + '</a>' + where + '</p>');
      } catch (e) {
        acmmShowDialog('Issue Creation Failed', '<p class="error-note">Network error: ' + escapeHtml(e.message) + '</p>');
      }
    }

    function acmmOpenAllIssuesForLevel(level) {
      if (!_acmmEvalData) return;
      const active = acmmGetActiveData();
      const criteria = (active.criteria_results || active.CriteriaResults || []).filter(c => c.level === level && !c.passed);
      if (criteria.length === 0) return;
      const repos = (_acmmEvalData.repo_results || []);
      const hasMultiRepo = repos.length > 1;
      const targetRepo = _acmmSelectedRepo || (repos.length === 1 ? repos[0].repo : null);
      const newCriteria = criteria.filter(c => !_acmmOpenedIssues[(targetRepo || '') + ':' + c.id]);
      const alreadyOpened = criteria.length - newCriteria.length;
      let html = '';
      if (newCriteria.length === 0) {
        html += '<p class="dialog-note">All ' + criteria.length + ' failing criteria already have issues opened.</p>';
        html += '<div class="row-end">' +
          '<button data-action="acmmCloseDialog" class="dialog-btn">Close</button></div>';
      } else {
        html += '<p class="dialog-note">This will create ' + newCriteria.length + ' ' + acmmTrackerName(acmmEffectiveTracker()) + ' issue(s) for failing criteria at L' + level + '.' +
          (alreadyOpened > 0 ? ' (' + alreadyOpened + ' already opened, will be skipped)' : '') + '</p>';
        if (hasMultiRepo && !_acmmSelectedRepo) {
          html += '<p class="dialog-note">Select a target repo first, or issues will be created for each repo where the criterion fails.</p>';
        }
        html += '<div style="max-height:200px;overflow-y:auto;margin-bottom:var(--sp-5)">';
        for (const c of criteria) {
          const opened = _acmmOpenedIssues[(targetRepo || '') + ':' + c.id];
          if (opened) {
            html += '<div style="font-size:0.78rem;padding:var(--sp-1) var(--sp-0);color:var(--muted)">✅ ' + escapeHtml(c.name) + ' <a href="' + escapeHtml(opened.url) + '" target="_blank" class="muted-xs">' + escapeHtml(acmmIssueRef(opened)) + '</a></div>';
          } else {
            html += '<div style="font-size:0.78rem;padding:var(--sp-1) var(--sp-0)">❌ ' + escapeHtml(c.name) + '</div>';
          }
        }
        html += '</div>';
        html += '<div class="row-end">' +
          '<button data-action="acmmCloseDialog" class="dialog-btn">Cancel</button>' +
          '<button class="hv-btn btn-primary btn-sm" data-action="acmmBatchCreateIssues" data-arg0="' + level + '" data-arg-types="j">Create ' + newCriteria.length + ' Issue(s)</button></div>';
      }
      acmmShowDialog('Open Issues for L' + level + ' ' + (ACMM_LEVEL_NAMES[level] || ''), html);
    }

    async function acmmBatchCreateIssues(level) {
      const active = acmmGetActiveData();
      const criteria = (active.criteria_results || []).filter(c => c.level === level && !c.passed);
      const repos = (_acmmEvalData.repo_results || []);
      const targetRepo = _acmmSelectedRepo || (repos.length === 1 ? repos[0].repo : null);

      let created = 0, failed = 0, skipped = 0;
      // #5873: every failure used to collapse into a bare `failed++` while the
      // server's response body — which carries the actual GitHub error, e.g.
      // "410 Issues has been disabled in this repository" — was discarded
      // unread. The summary then appended fixed template text about the acmm +
      // ai-fix-requested labels, which reads as a diagnosis and sent operators
      // chasing labels that were never the problem. Collect the real reason
      // per failing repo:criterion instead, deduped, exactly as the
      // single-issue path already shows it.
      const failures = new Map(); // reason -> [repo:criterion, ...]
      // Labels the server actually applied: a personal-account merge-queue
      // ticket carries acmm without ai-fix-requested.
      const batchLabels = new Set();
      const noteLabels = (data) => { (data && data.labels || []).forEach(l => batchLabels.add(l)); return data; };
      const recordFailure = async (where, r) => {
        failed++;
        let reason;
        if (r instanceof Response) {
          try { reason = (await r.text()).trim(); } catch { /* body unreadable */ }
          reason = reason || ('HTTP ' + r.status + ' ' + r.statusText);
        } else {
          reason = 'network error: ' + (r && r.message ? r.message : r);
        }
        if (!failures.has(reason)) failures.set(reason, []);
        failures.get(reason).push(where);
      };
      acmmShowDialog('Creating Issues...', '<p id="acmm-batch-progress" class="text-base">0/' + criteria.length + ' created...</p>');

      for (const c of criteria) {
        if (targetRepo) {
          const issueKey = targetRepo + ':' + c.id;
          if (_acmmOpenedIssues[issueKey]) { skipped++; continue; }
          try {
            const r = await fetch('/api/acmm/issue', {
              method: 'POST', headers: {'Content-Type': 'application/json'},
              body: JSON.stringify(acmmIssuePayload(targetRepo, c.id, level))
            });
            if (r.ok) {
              acmmRememberIssue(issueKey, noteLabels(await r.json()));
              created++;
            } else { await recordFailure(issueKey, r); }
          } catch (e) { await recordFailure(targetRepo + ':' + c.id, e); }
        } else {
          for (const rr of repos) {
            const issueKey = rr.repo + ':' + c.id;
            if (_acmmOpenedIssues[issueKey]) { skipped++; continue; }
            const rc = (rr.criteria_results || []).find(x => x.id === c.id);
            if (rc && !rc.passed) {
              try {
                const r = await fetch('/api/acmm/issue', {
                  method: 'POST', headers: {'Content-Type': 'application/json'},
                  body: JSON.stringify(acmmIssuePayload(rr.repo, c.id, level))
                });
                if (r.ok) {
                  acmmRememberIssue(issueKey, noteLabels(await r.json()));
                  created++;
                } else { await recordFailure(issueKey, r); }
              } catch (e) { await recordFailure(issueKey, e); }
            }
          }
        }
        const prog = document.getElementById('acmm-batch-progress');
        if (prog) prog.textContent = created + '/' + criteria.length + ' created' + (failed > 0 ? ' (' + failed + ' failed)' : '') + (skipped > 0 ? ' (' + skipped + ' skipped)' : '') + '...';
      }
      // The label note is a description of what a SUCCESSFUL create did, not a
      // diagnosis — show it only when something was actually created (#5873).
      const labelNote = acmmEffectiveTracker() === 'linear' ? ' in Linear.' : acmmLabelNote(batchLabels.size ? Array.from(batchLabels) : null, ' with <code>acmm</code> + <code>ai-fix-requested</code> labels.');
      let html = '<p class="text-base">' + created + ' issue(s) created' + (skipped > 0 ? ', ' + skipped + ' already existed' : '') + (created > 0 ? labelNote : '.') + '</p>';
      if (failed > 0) {
        html += '<p class="text-base mt-4">' + failed + ' failed:</p>';
        for (const [reason, wheres] of failures) {
          html += '<p class="error-note mt-3">' + escapeHtml(reason) + '<br><span style="font-size:0.7rem;color:var(--muted)">' + escapeHtml(wheres.join(', ')) + '</span></p>';
        }
      }
      acmmShowDialog(failed > 0 ? 'Issues Created (' + failed + ' failed)' : 'Issues Created', html);
    }

    function acmmShowLevelDialog(level, repoName) {
      if (!_acmmEvalData) return;
      const source = repoName ? (_acmmEvalData.repo_results || []).find(r => r.repo === repoName) : acmmGetActiveData();
      if (!source) return;
      const lvlData = (source.levels || source.Levels || []).find(l => l.level === level);
      const criteria = (source.criteria_results || source.CriteriaResults || []).filter(c => c.level === level);
      const name = ACMM_LEVEL_NAMES[level] || ('L' + level);
      const desc = ACMM_LEVEL_DESCRIPTIONS[level] || '';
      const pct = lvlData ? Math.round(lvlData.score * 100) : 0;
      const passed = lvlData ? lvlData.passed : false;
      const statusColor = passed ? 'var(--green)' : (pct > 40 ? 'var(--yellow)' : 'var(--red)');
      const statusText = passed ? '✓ Passed' : '✗ Not passed';
      const threshold = lvlData ? Math.round(lvlData.threshold * 100) : 70;
      const failCount = criteria.filter(c => !c.passed).length;

      const byCategory = {};
      for (const c of criteria) {
        const cat = c.category || 'general';
        if (!byCategory[cat]) byCategory[cat] = [];
        byCategory[cat].push(c);
      }

      let criteriaHtml = '';
      for (const [cat, items] of Object.entries(byCategory)) {
        criteriaHtml += '<div style="margin-top:14px"><div style="font-size:var(--fs-xs);text-transform:uppercase;letter-spacing:0.05em;color:var(--muted);margin-bottom:var(--sp-3);padding-bottom:var(--sp-2);border-bottom:1px solid var(--border);font-weight:600">' + escapeHtml(cat) + '</div>';
        for (const c of items) {
          criteriaHtml += acmmCriterionRow(c, repoName);
        }
        criteriaHtml += '</div>';
      }

      let batchBtn = '';
      if (failCount > 0) {
        batchBtn = '<button class="hv-btn btn-primary btn-sm" data-action="acmmOpenAllIssuesForLevel" data-arg0="' + level + '" data-arg-types="j" title="Open issues for all failing criteria at this level">📋 Open All (' + failCount + ')</button>';
      }

      acmmShowDialog(
        'L' + level + ' ' + name + (repoName ? ' — ' + repoName : ''),
        '<div class="mb-5">' +
        '<div style="display:flex;align-items:center;gap:10px;margin-bottom:var(--sp-4)">' +
        '<span style="font-size:var(--fs-2xl);font-weight:700;color:' + statusColor + '">' + pct + '%</span>' +
        '<span style="font-size:var(--fs-base);color:' + statusColor + ';font-weight:600">' + statusText + '</span>' +
        '<span class="muted-xs">(threshold: ' + threshold + '%)</span>' +
        batchBtn +
        (failCount > 0 ? acmmTrackerSelector() : '') +
        '</div>' +
        '<p style="font-size:0.8rem;color:var(--muted);margin:var(--sp-0) var(--sp-0) var(--sp-4)">' + escapeHtml(desc) + '</p>' +
        '</div>' +
        '<div style="font-size:var(--fs-base);font-weight:600;margin-bottom:var(--sp-1);padding-top:var(--sp-2)">Criteria (' + (lvlData ? lvlData.matched : 0) + '/' + (lvlData ? lvlData.total : 0) + ')</div>' +
        criteriaHtml
      );
    }

    function acmmShowCodebaseDialog() {
      if (!_acmmEvalData) return;
      const d = _acmmEvalData;
      const hasMultiRepo = (d.repo_results || []).length > 1;
      const active = acmmGetActiveData();
      const codeLevel = active.codebase_level !== undefined ? active.codebase_level : active.CodebaseLevel;
      const name = ACMM_LEVEL_NAMES[codeLevel] || 'None';
      const scopeNote = _acmmSelectedRepo ? ' Showing ' + escapeHtml(_acmmSelectedRepo) + '.' : (hasMultiRepo ? ' A criterion passes if it exists in <em>any</em> repo.' : '');

      let html = '<div style="margin-bottom:14px">' +
        '<p style="font-size:0.78rem;color:var(--muted);margin:0 0 10px">Each level requires ≥70% of its criteria to pass. Levels must pass in order.' +
        scopeNote + '</p></div>';

      // Find first failing level for the "blocked at" callout
      const levels = active.levels || active.Levels || [];
      let firstFail = null;
      for (const lvl of levels) { if (!lvl.passed && !firstFail) firstFail = lvl; }

      if (firstFail) {
        const needed = Math.ceil(firstFail.total * acmmLevelThresholdPct / 100) - firstFail.matched;
        const fpct = Math.round(firstFail.score * 100);
        html += '<div style="margin-bottom:var(--sp-5);padding:8px 10px;background:var(--bg);border-radius:var(--r);font-size:0.78rem;color:var(--muted);border-left:3px solid var(--yellow)">' +
          '<strong>Blocked at L' + firstFail.level + ' ' + escapeHtml(firstFail.name) + '</strong> — ' + fpct + '%, need ' + needed + ' more criteria to reach 70%. ' +
          '<span data-action="acmmShowLevelDialog" data-arg0="' + firstFail.level + '" data-arg-types="j" style="color:var(--blue);cursor:pointer;text-decoration:underline">View missing criteria →</span></div>';
      }

      if (hasMultiRepo && !_acmmSelectedRepo) {
        // Multi-repo: level bars only inside each repo's expandable
        html += '<div>';
        for (let ri = 0; ri < d.repo_results.length; ri++) {
          const rr = d.repo_results[ri];
          const rPct = rr.criteria_total > 0 ? Math.round(rr.criteria_passed / rr.criteria_total * 100) : 0;
          const rColor = rr.codebase_level >= 2 ? 'var(--green)' : (rr.criteria_passed > rr.criteria_total * 0.5 ? 'var(--yellow)' : 'var(--red)');
          const expandId = 'acmm-repo-expand-' + ri;
          html += '<div style="margin-bottom:3px">' +
            '<div data-action="ghToggleDisplayById" data-arg0="' + expandId + '" ' +
            'style="cursor:pointer;padding:var(--sp-3) var(--sp-4);background:var(--bg);border-radius:var(--r);border:1px solid var(--border);display:flex;align-items:center;justify-content:space-between;transition:border-color .15s" ' +
            'data-mouseover-action="gh22" data-mouseout-action="gh21">' +
            '<span style="font-size:0.8rem"><span class="acmm-chevron" style="display:inline-block;transition:transform .15s;font-size:0.65rem;margin-right:var(--sp-2)">▶</span>' + escapeHtml(rr.repo) + '</span>' +
            '<span style="font-size:0.72rem;color:' + rColor + ';font-weight:600">' + rr.criteria_passed + '/' + rr.criteria_total + ' (' + rPct + '%)</span></div>' +
            '<div id="' + expandId + '" style="display:none;padding:var(--sp-3) var(--sp-4) var(--sp-4);border:1px solid var(--border);border-top:none;border-radius:0 0 6px 6px;background:var(--surface)">';
          for (const lvl of (rr.levels || [])) {
            const pct = Math.round(lvl.score * 100);
            const color = lvl.passed ? 'var(--green)' : (pct > 40 ? 'var(--yellow)' : 'var(--red)');
            const safeRepo = escapeHtml(rr.repo).replace(/'/g, "\\'");
            html += '<div data-action="acmmShowLevelDialog" data-arg0="' + lvl.level + '" data-arg1="' + escapeHtml(rr.repo) + '" data-arg-types="j,s" data-stop="1" ' +
              'style="cursor:pointer;padding:5px 6px;margin-bottom:3px;background:var(--bg);border-radius:var(--r-sm);border:1px solid var(--border);transition:border-color .15s" ' +
              'data-mouseover-action="gh22" data-mouseout-action="gh21">' +
              '<div class="row-between">' +
              '<span style="font-size:var(--fs-sm);font-weight:500">' + (lvl.passed ? '✓' : '✗') + ' L' + lvl.level + ' ' + escapeHtml(lvl.name) + acmmLevelWaiverMark(rr, lvl.level) + '</span>' +
              '<span style="font-size:0.7rem;color:' + color + ';font-weight:600">' + lvl.matched + '/' + lvl.total + '</span></div>' +
              '<div style="margin-top:3px;height:3px;background:var(--border);border-radius:2px;overflow:hidden"><div style="width:' + pct + '%;height:100%;background:' + color + ';border-radius:2px"></div></div></div>';
          }
          html += '</div></div>';
        }
        html += '</div>';
      } else {
        // Single repo: show level bars directly
        for (const lvl of levels) {
          const pct = Math.round(lvl.score * 100);
          const color = lvl.passed ? 'var(--green)' : (pct > 40 ? 'var(--yellow)' : 'var(--red)');
          html += '<div data-action="acmmShowLevelDialog" data-arg0="' + lvl.level + '" data-arg-types="j" class="row-card" data-mouseover-action="gh22" data-mouseout-action="gh21">' +
            '<div class="row-between">' +
            '<span style="font-size:var(--fs-base);font-weight:600">' + (lvl.passed ? '✓' : '✗') + ' L' + lvl.level + ' ' + escapeHtml(lvl.name) + acmmLevelWaiverMark(active, lvl.level) + '</span>' +
            '<span style="font-size:var(--fs-sm);color:' + color + ';font-weight:600">' + lvl.matched + '/' + lvl.total + ' (' + pct + '%)</span></div>' +
            '<div style="margin-top:var(--sp-2);height:4px;background:var(--bg);border-radius:2px;overflow:hidden"><div style="width:' + pct + '%;height:100%;background:' + color + ';border-radius:2px"></div></div></div>';
        }
      }

      acmmShowDialog('Codebase Readiness', html);
    }

    const acmmLevelThresholdPct = 70;

    function acmmShowOpsDialog() {
      if (!_acmmEvalData) return;
      const d = _acmmEvalData;
      const name = ACMM_LEVEL_NAMES[d.operational_level] || 'Unknown';
      const opsDescriptions = {
        1: 'Manual operation — agents run one-off tasks with no persistent configuration.',
        2: 'Agents follow explicit instructions (CLAUDE.md, copilot-instructions) but require human initiation.',
        3: 'Observability in place — metrics, dashboards, and review rubrics track agent quality.',
        4: 'Self-tuning — agents adapt their behavior based on feedback loops and compliance checks.',
        5: 'Semi-autonomous — CI/CD triggers agents, auto-QA self-tunes, policies enforced as code.',
        6: 'Autonomous — multi-agent orchestration, auto issue generation, strategic oversight.'
      };
      const desc = opsDescriptions[d.operational_level] || 'Level not recognized.';
      acmmShowDialog('Operational Autonomy — L' + d.operational_level + ' ' + escapeHtml(name),
        '<p class="dialog-note">Operational autonomy reflects the hive\'s current pack configuration — how much the system automates without human intervention.</p>' +
        '<div style="padding:var(--sp-5);background:var(--bg);border-radius:var(--r);border:1px solid var(--border)">' +
        '<div style="font-size:1.1rem;font-weight:700;margin-bottom:var(--sp-2)">L' + d.operational_level + ' — ' + escapeHtml(name) + '</div>' +
        '<div class="fs-base text-muted">' + escapeHtml(desc) + '</div></div>' +
        '<p class="fs-sm text-muted mt-5">This is set by the <code>acmm_level</code> field in your hive config, or determined by your active pack.</p>'
      );
    }

    function acmmShowOverallDialog() {
      if (!_acmmEvalData) return;
      const d = _acmmEvalData;
      const active = acmmGetActiveData();
      const codebaseLevel = Number.isFinite(active.codebase_level) ? active.codebase_level : d.codebase_level;
      const overallLevel = Math.min(codebaseLevel, d.operational_level);
      const scope = _acmmSelectedRepo ? escapeHtml(_acmmSelectedRepo) : 'all repos (union aggregate)';
      const overallName = ACMM_LEVEL_NAMES[overallLevel] || 'None';
      const lower = codebaseLevel <= d.operational_level ? 'codebase readiness' : 'operational autonomy';
      acmmShowDialog('Overall ACMM Level — L' + overallLevel + ' ' + escapeHtml(overallName),
        '<p class="dialog-note">The overall ACMM level is the <strong>minimum</strong> of codebase readiness and operational autonomy. Both dimensions must align — a high operational level means nothing if the repo lacks foundational tooling, and vice versa.</p>' +
        '<p class="dialog-note">Codebase scope: ' + scope + '. Operational autonomy is hive-wide.</p>' +
        '<div style="display:flex;gap:var(--sp-5);margin-bottom:var(--sp-5)">' +
        '<div class="card-tile" data-action="gh23">' +
        '<div class="stat-num">L' + codebaseLevel + '</div><div class="muted-xs">Codebase</div></div>' +
        '<div class="card-tile" data-action="gh24">' +
        '<div class="stat-num">L' + d.operational_level + '</div><div class="muted-xs">Operational</div></div>' +
        '</div>' +
        '<div style="padding:10px;background:var(--bg);border-radius:var(--r);border:1px solid var(--border);font-size:0.8rem">' +
        '→ Overall is <strong>L' + overallLevel + '</strong> because ' + lower + ' is the limiting factor.</div>'
      );
    }

    function acmmShowAllCriteriaDialog() {
      if (!_acmmEvalData) return;
      const d = _acmmEvalData;
      const active = acmmGetActiveData();
      const results = active.criteria_results || active.CriteriaResults || [];
      const levels = active.levels || active.Levels || [];
      const totalPassed = results.filter(c => c.passed).length;
      const hasMultiRepo = (d.repo_results || []).length > 1;

      const criteriaScope = _acmmSelectedRepo ? ' for ' + escapeHtml(_acmmSelectedRepo) : (hasMultiRepo ? ' (union aggregate across ' + d.repo_results.length + ' repos)' : '');
      let html = '<p class="dialog-note">' + totalPassed + ' of ' + results.length + ' criteria detected' + criteriaScope + '. Click a level row to see details.</p>';

      for (const lvl of levels) {
        const criteria = results.filter(c => c.level === lvl.level);
        const pct = Math.round(lvl.score * 100);
        const color = lvl.passed ? 'var(--green)' : (pct > 40 ? 'var(--yellow)' : 'var(--red)');
        const failCount = criteria.filter(c => !c.passed).length;
        html += '<div class="mb-4">' +
          '<div data-action="ghAcmmCloseAndShowLevel" data-arg0="' + lvl.level + '" data-arg-types="j" style="cursor:pointer;display:flex;align-items:center;justify-content:space-between;padding:var(--sp-3) var(--sp-4);background:var(--bg);border-radius:var(--r);border:1px solid var(--border);transition:border-color .15s" data-mouseover-action="gh22" data-mouseout-action="gh21">' +
          '<span style="font-size:var(--fs-base);font-weight:600">L' + lvl.level + ' ' + escapeHtml(lvl.name) + acmmLevelWaiverMark(active, lvl.level) + '</span>' +
          '<div style="display:flex;align-items:center;gap:var(--sp-3)">';
        if (failCount > 0) {
          html += '<button class="hv-btn btn-danger btn-sm" data-action="acmmOpenAllIssuesForLevel" data-arg0="' + lvl.level + '" data-arg-types="j" data-stop="1">📋 ' + failCount + '</button>';
        }
        html += '<span style="font-size:var(--fs-sm);color:' + color + '">' + lvl.matched + '/' + lvl.total + '</span></div></div>';
        for (const c of criteria) {
          html += '<div style="display:flex;align-items:center;gap:var(--sp-3);padding:3px 8px 3px 20px;font-size:0.78rem">' +
            '<span>' + (c.passed ? '✅' : '❌') + '</span>' +
            '<span>' + escapeHtml(c.name) + '</span>' +
            (c.waived ? '<span class="fs-2xs text-success" title="' + escapeHtml('Satisfied by ' + (c.waiver_satisfied_by || '.acmm.yml') + ', not by a file in this repo') + '">⌾ waived</span>' : '') +
            '</div>';
        }
        html += '</div>';
      }
      acmmShowDialog('All Criteria — ' + totalPassed + '/' + results.length, html);
    }

    function acmmShowDialog(title, bodyHtml) {
      let overlay = document.getElementById('acmm-dialog-overlay');
      if (!overlay) {
        overlay = document.createElement('div');
        overlay.id = 'acmm-dialog-overlay';
        overlay.className = 'acmm-dialog-overlay'; // modal recipe (Phase 2); z-index kept in its own rule
        overlay.addEventListener('click', (e) => { if (e.target === overlay) acmmCloseDialog(); });
        document.body.appendChild(overlay);
      }
      overlay.style.display = 'flex';
      hiveRememberModalOpen(overlay);
      // Body scroll-lock comes from the shared installModalScrollLock observer
      // (#4805) — no bespoke inline body-overflow write here, which used to
      // fight the shared class-based lock.
      overlay.innerHTML = '<div class="acmm-dialog modal-scroll">' +
        '<div class="row-between mb-5">' +
        '<h3 style="margin:var(--sp-0);font-size:1rem;font-weight:700;color:var(--text)">' + title + '</h3>' +
        '<button class="hv-btn btn-icon btn-sm" data-action="acmmCloseDialog">✕</button></div>' +
        '<div>' + bodyHtml + '</div></div>';
    }

    function acmmCloseDialog() {
      const overlay = document.getElementById('acmm-dialog-overlay');
      if (overlay) overlay.style.display = 'none';
    }

    function acmmSelectRepo(repoName) {
      _acmmSelectedRepo = repoName;
      acmmRenderCard();
    }

    // #5822. Everything below answers one question the panel could not answer:
    // WHICH repo is lagging, and by how much.
    //
    // The aggregate the card headlines is a UNION — evaluateAllRepos marks a
    // criterion passed if ANY repo has it (api_acmm_eval.go). That is a
    // defensible number for "what has this hive demonstrated anywhere" and the
    // wrong one for "is my fleet ready": a repo meeting none of the 43 criteria
    // does not move the headline at all, so it is invisible. The per-repo data
    // to say otherwise was already in the payload; nothing here fetches
    // anything new.

    // acmmFirstFailingLevel returns the lowest level a repo has not passed —
    // the one it is BLOCKED at. Levels must pass in order, so the first failure
    // is the whole story; reporting the count of failures instead would rank a
    // repo stuck at L2 below one that merely has gaps at L5.
    function acmmFirstFailingLevel(rr) {
      for (const lvl of (rr.levels || [])) {
        if (!lvl.passed) return lvl;
      }
      return null;
    }

    // acmmRepoGapRows ranks repos worst-first.
    //
    // The primary key is the GAP (unmet criteria), not the percentage: with a
    // fixed criteria set the two agree, and the raw count is what an operator
    // acts on ("14 things to fix" is a work estimate; "67%" is not). Ties break
    // on the lower level first, then on name — a stable order matters because
    // this table is read repeatedly and a shuffling list reads as churn that is
    // not there.
    function acmmRepoGapRows(repos) {
      const rows = (repos || []).map(rr => {
        const total = rr.criteria_total || 0;
        const passed = rr.criteria_passed || 0;
        return {
          repo: rr.repo,
          level: rr.codebase_level || 0,
          passed: passed,
          total: total,
          gap: Math.max(0, total - passed),
          pct: total > 0 ? Math.round(passed / total * 100) : 0,
          blocked: acmmFirstFailingLevel(rr)
        };
      });
      rows.sort((a, b) => (b.gap - a.gap) || (a.level - b.level) || a.repo.localeCompare(b.repo));
      return rows;
    }

    // acmmRenderRepoTable draws the ranking. It renders only for a multi-repo
    // hive: with one repo the table would restate the tiles directly above it.
    function acmmRenderRepoTable(repos) {
      const el = document.getElementById('acmm-repo-table');
      if (!el) return;
      if (!repos || repos.length < 2) {
        el.style.display = 'none';
        el.innerHTML = '';
        return;
      }
      const rows = acmmRepoGapRows(repos);
      // Only call something "weakest" when the repos actually differ. On a
      // fleet where every repo is level, singling one out would invent a
      // problem — the same reason the union number needed qualifying.
      const spread = rows.length > 1 && rows[0].gap > rows[rows.length - 1].gap;

      let html = '<div style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-2)">Per-repo readiness — weakest first</div>' +
        '<div style="border:1px solid var(--border);border-radius:var(--r);overflow:hidden">';
      for (let i = 0; i < rows.length; i++) {
        const r = rows[i];
        const selected = _acmmSelectedRepo === r.repo;
        const color = r.pct >= 70 ? 'var(--green)' : (r.pct > 40 ? 'var(--yellow)' : 'var(--red)');
        const blockedText = r.blocked
          ? 'blocked at L' + r.blocked.level + ' ' + escapeHtml(r.blocked.name)
          : 'all levels passed';
        // Selection is carried by the left accent and the weight, NOT by the
        // background: the hover handlers (gh25/gh26) own the background, and a
        // selected row would lose its highlight the moment the pointer left it.
        html += '<div class="acmm-readiness-row" data-action="acmmSelectRepo" data-arg0="' + escapeHtml(r.repo) + '" data-arg-types="s" ' +
          'style="' +
          'border-left:3px solid ' + (selected ? 'var(--accent)' : 'transparent') + ';' +
          (i > 0 ? 'border-top:1px solid var(--border);' : '') + '" ' +
          'data-mouseover-action="gh25" data-mouseout-action="gh26">' +
          '<span class="acmm-readiness-repo" style="font-weight:' + (selected ? '600' : '400') + '">' +
          escapeHtml(r.repo) +
          (spread && i === 0 ? '<span title="Largest gap of any watched repo" style="margin-left:var(--sp-3);padding:1px 6px;border-radius:var(--r-pill);font-size:var(--fs-2xs);font-weight:600;background:color-mix(in srgb, var(--red) 16%, transparent);color:var(--red)">weakest</span>' : '') +
          '</span>' +
          '<span style="width:34px;text-align:right;font-weight:600">L' + r.level + '</span>' +
          '<span style="width:74px;text-align:right;color:' + color + ';font-weight:600">' + r.passed + '/' + r.total + '</span>' +
          '<span class="acmm-readiness-blocker">' + blockedText + '</span>' +
          '</div>';
      }
      html += '</div>';
      el.innerHTML = html;
      el.style.display = 'block';
    }

    function acmmRenderCard() {
      refreshSectionCardShell('acmm-eval-section');
      const d = _acmmEvalData;
      if (!d) return;

      // #5822: the active view is resolved HERE, before the tiles are written.
      // It used to be computed further down and consumed only by the level
      // bars, so clicking a repo chip moved the bars while the four tiles above
      // them kept reporting hive-wide numbers — two halves of one card
      // disagreeing, with nothing to say which of them followed the click.
      const active = acmmGetActiveData() || d;
      const repos = d.repo_results || [];
      const multiRepo = repos.length > 1;
      const scoped = !!_acmmSelectedRepo;

      const fmtLevel = (lvl) => {
        const name = ACMM_LEVEL_NAMES[lvl] || ('L'+lvl);
        return '<span class="stat-num">L' + lvl + '</span><br><span style="font-size:0.7rem;color:var(--muted)">' + escapeHtml(name) + '</span>';
      };
      // A RepoEvaluation and the aggregate carry the same field names, but a
      // repo missing a field must fall back to the aggregate rather than render
      // "undefined" or silently read as zero.
      const pick = (v, fallback) => (typeof v === 'number' ? v : fallback);
      const codebaseLevel = pick(active.codebase_level, d.codebase_level);

      // Overall is min(codebase, operational). The server computes it against
      // the UNION codebase level, so with a repo selected it must be recomputed
      // — otherwise the Overall tile silently keeps the fleet-wide reading
      // while the tile beside it shows the repo's. Unscoped, the server's value
      // stands, so the panel never disagrees with the payload it was given.
      const overallLevel = scoped ? Math.min(codebaseLevel, d.operational_level) : d.overall_level;

      setIfChanged(document.getElementById('acmm-codebase-level'), fmtLevel(codebaseLevel));
      setIfChanged(document.getElementById('acmm-ops-level'), fmtLevel(d.operational_level));
      setIfChanged(document.getElementById('acmm-overall-level'), fmtLevel(overallLevel));
      setIfChanged(document.getElementById('acmm-criteria-ratio'),
        '<span class="stat-num">' + pick(active.criteria_passed, d.criteria_passed) + '/' + pick(active.criteria_total, d.criteria_total) + '</span>');

      // Scope tags. Operational Autonomy is genuinely hive-wide — it measures
      // the hive's own operating behaviour, not a repository — so it does NOT
      // follow the selector. That makes the card's scope mixed, which is fine
      // as long as it is stated: an unlabelled tile that ignores the click is
      // indistinguishable from one that failed to follow it.
      const scopeTag = (txt, title) => '<span title="' + escapeHtml(title) + '" style="display:inline-block;margin-left:var(--sp-2);padding:0 5px;border-radius:var(--r-pill);font-size:var(--fs-2xs);font-weight:600;background:color-mix(in srgb, var(--muted) 18%, transparent);color:var(--muted)">' + escapeHtml(txt) + '</span>';
      const repoTag = scoped
        ? scopeTag(_acmmSelectedRepo, 'Showing only ' + _acmmSelectedRepo)
        : (multiRepo ? scopeTag('all repos', 'Union across all ' + repos.length + ' watched repos: a criterion counts as passed if ANY repo has it') : '');
      const setLbl = (id, text, tag) => {
        const el = document.getElementById(id);
        if (el) setIfChanged(el, escapeHtml(text) + (tag || ''));
      };
      setLbl('acmm-lbl-codebase', 'Codebase Readiness', repoTag);
      setLbl('acmm-lbl-criteria', 'Criteria Passed', repoTag);
      setLbl('acmm-lbl-overall', 'Overall ACMM', repoTag);
      setLbl('acmm-lbl-ops', 'Operational Autonomy',
        scoped ? scopeTag('hive-wide', 'Operational autonomy measures the hive, not a repository — it does not change with the repo selection') : '');

      // Say what the aggregate IS, in the panel and not only in a dialog.
      const noteEl = document.getElementById('acmm-scope-note');
      if (noteEl) {
        if (multiRepo && !scoped) {
          setIfChanged(noteEl, 'Aggregate across ' + repos.length + ' repos: a criterion counts as passed if <em>any</em> repo has it, so a repo meeting none of them does not lower these numbers. Pick a repo above, or read the table below.');
          noteEl.style.display = 'block';
        } else {
          noteEl.style.display = 'none';
        }
      }

      // Repo selector
      const selectorEl = document.getElementById('acmm-repo-selector');
      if (selectorEl && multiRepo) {
        selectorEl.style.display = 'flex';
        const allBtn = document.getElementById('acmm-repo-btn-all');
        if (allBtn) allBtn.style.fontWeight = _acmmSelectedRepo ? '400' : '600';
        const existing = selectorEl.querySelectorAll('.acmm-repo-btn-repo');
        existing.forEach(b => b.remove());
        for (const rr of d.repo_results) {
          let btn = selectorEl.querySelector('[data-acmm-repo="' + rr.repo + '"]');
          if (!btn) {
            btn = document.createElement('button');
            btn.className = 'acmm-repo-btn-repo repo-chip-btn';
            btn.setAttribute('data-acmm-repo', rr.repo);
            btn.onclick = () => acmmSelectRepo(rr.repo);
            selectorEl.appendChild(btn);
          }
          // The chip carries the ratio as well as the level (#5822): "lacking,
          // and by how much" was previously N clicks away for N repos.
          btn.textContent = rr.repo + ' (L' + rr.codebase_level + ' · ' + rr.criteria_passed + '/' + rr.criteria_total + ')';
          btn.style.fontWeight = _acmmSelectedRepo === rr.repo ? '600' : '400';
          selectorEl.appendChild(btn);
        }
      } else if (selectorEl) {
        selectorEl.style.display = 'none';
      }

      acmmRenderRepoTable(repos);

      // Per-level bars from the same active view the tiles above now use.
      const levels = active.levels || active.Levels || d.levels || [];
      const barsEl = document.getElementById('acmm-level-bars');
      if (barsEl) {
        let html = '';
        for (const lvl of levels) {
          const pct = Math.round(lvl.score * 100);
          const color = lvl.passed ? 'var(--green)' : (pct > 40 ? 'var(--yellow)' : 'var(--red)');
          html += '<div data-action="acmmShowLevelDialog" data-arg0="' + lvl.level + '" data-arg-types="j" style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-2);font-size:var(--fs-sm);cursor:pointer;padding:var(--sp-1) var(--sp-2);border-radius:var(--r-sm);transition:background .15s" data-mouseover-action="gh25" data-mouseout-action="gh26">' +
            '<span style="width:100px;color:var(--muted)">' + escapeHtml(lvl.name) + '</span>' +
            '<div style="flex:1;height:8px;background:var(--bg);border-radius:var(--r-sm);overflow:hidden">' +
            '<div style="width:' + pct + '%;height:100%;background:' + color + ';border-radius:var(--r-sm);transition:width .3s"></div></div>' +
            '<span style="width:60px;text-align:right;color:var(--muted)">' + lvl.matched + '/' + lvl.total + '</span></div>';
        }
        barsEl.innerHTML = html;
      }

      const tsEl = document.getElementById('acmm-eval-timestamp');
      if (tsEl) {
        // #5877: relative age, so it is legible that the numbers above may be
        // an hour-old cache. Absolute timestamp stays in the tooltip.
        let tsText = '';
        if (d.last_evaluated_at) {
          tsText = 'Evaluated ' + acmmRelativeAge(d.last_evaluated_at);
          tsEl.title = formatDashboardDateTime(d.last_evaluated_at);
        }
        if (d.repo_results && d.repo_results.length > 1) tsText += ' | ' + d.repo_results.length + ' repos';
        tsEl.textContent = tsText;
      }

      const warnEl = document.getElementById('acmm-eval-warning');
      if (warnEl) {
        if (d.error) {
          warnEl.style.display = 'block';
          warnEl.textContent = d.error;
        } else {
          warnEl.style.display = 'none';
        }
      }
    }

    function acmmRelativeAge(iso) {
      const ms = Date.now() - new Date(iso).getTime();
      if (!Number.isFinite(ms) || ms < 0) return 'just now';
      const mins = Math.floor(ms / 60000);
      if (mins < 1) return 'just now';
      if (mins < 60) return mins + 'm ago';
      const hrs = Math.floor(mins / 60);
      if (hrs < 24) return hrs + 'h ' + (mins % 60) + 'm ago';
      return Math.floor(hrs / 24) + 'd ago';
    }

    // #5877: operator-initiated re-evaluation. The server enforces its own
    // debounce (a refresh within a minute of the last evaluation returns the
    // cached result), so this button cannot spend the API budget.
    async function acmmForceRefresh() {
      const btn = document.getElementById('acmm-refresh-btn');
      if (btn && btn.disabled) return;
      const prev = btn ? btn.textContent : '';
      if (btn) { btn.disabled = true; btn.textContent = '⏳ Re-evaluating...'; }
      try {
        await fetchACMMEvaluation(true);
      } finally {
        if (btn) { btn.disabled = false; btn.textContent = prev; }
      }
    }

    async function fetchACMMEvaluation(force) {
      try {
        const r = await fetch(force ? '/api/acmm/evaluation?refresh=1' : '/api/acmm/evaluation');
        if (!r.ok) return;
        _acmmEvalData = await r.json();
        acmmRenderCard();
      } catch (e) {
        console.error('acmm eval fetch failed', e);
      }
    }

    // Git version indicator
    const GIT_VERSION_POLL_MS = 300000;
    const VERSION_UPGRADE_STORAGE_KEY = 'hive.version.upgradeProgress';
    // Last hub-initiated upgrade attempt adopted into the progress record
    // (#12055), so an attempt that was dismissed/expired is not re-shown.
    const VERSION_UPGRADE_HUB_SEEN_KEY = 'hive.version.upgradeHubAttempt';
    const VERSION_UPGRADE_DONE_MS = 30000;
    const VERSION_UPGRADE_LONG_MS = 15 * 60 * 1000;
    const VERSION_UPGRADE_POLL_MS = 5000;
    let _versionUpgradePollTimer = null;

