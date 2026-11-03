    function versionNowMs() {
      return Date.now();
    }

    function versionReadUpgradeProgress(now) {
      now = now || versionNowMs();
      try {
        const raw = localStorage.getItem(VERSION_UPGRADE_STORAGE_KEY);
        if (!raw) return null;
        const st = JSON.parse(raw);
        if (!st || !st.target || st.target === 'latest' || !st.startedAt) {
          versionClearUpgradeProgress();
          return null;
        }
        st.startedAt = Number(st.startedAt);
        st.completedAt = st.completedAt ? Number(st.completedAt) : 0;
        if (st.completedAt && now - st.completedAt > VERSION_UPGRADE_DONE_MS) {
          versionClearUpgradeProgress();
          return null;
        }
        if (!st.completedAt && now - st.startedAt > VERSION_UPGRADE_LONG_MS) {
          versionClearUpgradeProgress();
          return null;
        }
        st.overdue = false;
        return st;
      } catch (_) {
        try { versionClearUpgradeProgress(); } catch (e) {}
        return null;
      }
    }

    function versionWriteUpgradeProgress(st) {
      if (!st || !st.target || st.target === 'latest') return null;
      const next = {
        target: String(st.target),
        targetShort: versionShortSHA(st.targetShort || st.target),
        startedFrom: String(st.startedFrom || ''),
        startedAt: Number(st.startedAt || versionNowMs()),
        completedAt: st.completedAt ? Number(st.completedAt) : 0
      };
      if (st.source) next.source = String(st.source);
      try { localStorage.setItem(VERSION_UPGRADE_STORAGE_KEY, JSON.stringify(next)); } catch (_) {}
      return next;
    }

    function versionClearUpgradeProgress() {
      try { localStorage.removeItem(VERSION_UPGRADE_STORAGE_KEY); } catch (_) {}
      _upgradeInProgress = false;
      _upgradeTargetHash = null;
    }

    function versionRecordUpgradeStart(targetHash, targetShort, currentHash) {
      const st = versionWriteUpgradeProgress({ target: targetHash || '', targetShort: targetShort || targetHash || '', startedFrom: currentHash || '', startedAt: versionNowMs() });
      _upgradeInProgress = !!st;
      _upgradeTargetHash = st ? targetHash : null;
      return st;
    }

    function versionMarkUpgradeComplete(st, now) {
      if (!st) return null;
      now = now || versionNowMs();
      if (!st.completedAt) {
        st = versionWriteUpgradeProgress({ target: st.target, targetShort: st.targetShort || st.target, startedFrom: st.startedFrom || '', startedAt: st.startedAt, completedAt: now, source: st.source });
      }
      _upgradeInProgress = false;
      _upgradeTargetHash = null;
      return st;
    }

    function versionReconcileUpgradeProgress(v, now) {
      const st = versionReadUpgradeProgress(now);
      if (!st) return null;
      const running = v && (v.hash || v.short);
      const target = v && v.target || {};
      const policy = versionPolicy(v) || {};
      if (target.source === 'hub' && target.resolved === false) {
        versionClearUpgradeProgress();
        return null;
      }
      const markerTarget = v && v.upgradeMarker && v.upgradeMarker.target || '';
      const knownTarget = target.sha || policy.target_sha || policy.armed_target || target.short || v && v.stableV4Hash || v && v.latestHash || v && v.targetHash || markerTarget || '';
      if (versionSameCommit(running, st.target)) {
        if (knownTarget && !versionSameCommit(knownTarget, st.target)) {
          versionClearUpgradeProgress();
          return null;
        }
        return versionMarkUpgradeComplete(st, now);
      }
      if (!st.startedFrom && running && !versionSameCommit(running, st.target)) {
        versionClearUpgradeProgress();
        return null;
      }
      if (st.startedFrom && running && !versionSameCommit(running, st.startedFrom)) {
        versionClearUpgradeProgress();
        return null;
      }
      // A hub-initiated attempt lives only as long as the spoke's upgrade
      // marker: if the marker is gone and the target has not landed, the
      // attempt was abandoned (unpublished target, stale marker cleared).
      if (st.source === 'hub' && !st.completedAt && v && !(v.upgradeMarker && v.upgradeMarker.target)) {
        versionClearUpgradeProgress();
        return null;
      }
      if (!knownTarget) {
        versionClearUpgradeProgress();
        return null;
      }
      if (!st.completedAt) {
        _upgradeInProgress = true;
        _upgradeTargetHash = st.target;
      }
      return st;
    }

    function versionHubUpgradeAttemptKey(um) {
      if (!um || !um.target) return '';
      return [String(um.target), String(um.requestedAt || ''), String(um.attempts || '')].map(function(part) { return part.trim(); }).join('|');
    }

    // Adopt a hub-initiated upgrade (#12055): the spoke's on-PVC upgrade marker
    // reaches the dashboard as v.upgradeMarker, but the progress record that
    // drives the bee animation was only ever written by the local Upgrade
    // button. Mirror a live marker into that record so hub-driven upgrades get
    // the same surfaces, once per attempt.
    function versionAdoptHubUpgradeProgress(v, now) {
      const um = v && v.upgradeMarker;
      if (!um || um.failed || !um.target) return null;
      now = now || versionNowMs();
      const running = v.hash || v.short || '';
      if (running && versionSameCommit(running, um.target)) return null;
      const key = versionHubUpgradeAttemptKey(um);
      let seen = '';
      try { seen = localStorage.getItem(VERSION_UPGRADE_HUB_SEEN_KEY) || ''; } catch (_) {}
      if (seen && seen === key) return null;
      try { localStorage.setItem(VERSION_UPGRADE_HUB_SEEN_KEY, key); } catch (_) {}
      const requestedAt = Date.parse(um.requestedAt || '');
      const startedAt = isFinite(requestedAt) && requestedAt > 0 && now - requestedAt < VERSION_UPGRADE_LONG_MS ? requestedAt : now;
      const st = versionWriteUpgradeProgress({ target: um.target, targetShort: versionShortSHA(um.target), startedFrom: um.current || running, startedAt: startedAt, source: 'hub' });
      if (st) {
        _upgradeInProgress = true;
        _upgradeTargetHash = st.target;
      }
      return st;
    }

    function versionElapsedText(startedAt, now) {
      startedAt = Number(startedAt || 0);
      now = now || versionNowMs();
      if (!startedAt) return '';
      const seconds = Math.max(0, Math.floor((now - startedAt) / 1000));
      if (seconds < 60) return seconds + 's elapsed';
      const minutes = Math.floor(seconds / 60);
      if (minutes < 60) return minutes + 'm elapsed';
      const hours = Math.floor(minutes / 60);
      return hours + 'h ' + (minutes % 60) + 'm elapsed';
    }

    function versionScheduleUpgradePoll(state) {
      const root = typeof window !== 'undefined' ? window : globalThis;
      if (root._versionUpgradePollTimer) {
        (root.clearTimeout || clearTimeout)(root._versionUpgradePollTimer);
        root._versionUpgradePollTimer = null;
      }
      if (!state) return;
      if (typeof root.setTimeout !== 'function') return;
      let delay = VERSION_UPGRADE_POLL_MS;
      if (state !== true && state.completedAt) {
        delay = Math.max(500, VERSION_UPGRADE_DONE_MS - (versionNowMs() - Number(state.completedAt)) + 100);
      }
      root._versionUpgradePollTimer = root.setTimeout(function() { fetchGitVersion(); }, delay);
    }

    function versionDeliveryLabel(v) {
      const branch = v.branch && v.branch !== 'unknown' ? String(v.branch) : '';
      if (v.channel) return branch ? String(v.channel) + ' (' + branch + ')' : String(v.channel);
      return branch;
    }

    function versionTrackingLabel(v) {
      if (v.tracking === 'registry') return 'tracking registry';
      if (v.tracking === 'pinned' && v.imageSource === 'podman-env') return 'tracking pinned';
      if (v.tracking === 'floating' || v.tracking === 'pinned') return v.tracking;
      return 'tracking unknown';
    }

    function versionTrackingTooltip(v) {
      const details = ['Running ' + String(v.short || v.hash || 'unknown')];
      if (v.channel) details.push('Channel: ' + String(v.channel));
      if (v.branch && v.branch !== 'unknown') details.push('Built from: ' + String(v.branch));
      if (v.imageRef) details.push('Image: ' + String(v.imageRef));
      if (v.tracking === 'registry') details.push('Tracking: podman auto-update (registry)');
      else if (v.tracking === 'floating') details.push('Tracking: floating (mutable tag)');
      else if (v.tracking === 'pinned') details.push('Tracking: pinned (immutable tag or digest)');
      else details.push('Tracking: unknown (authoritative image reference unavailable)');
      return details.join('\n');
    }

    // Label for the commit the "N behind" readout is measured against (#7262):
    // the hub-reported target (this spoke's release channel or branch head)
    // when the hub delivered one, else the tip of this build's own branch.
    // Never a hard-wired "Stable v4 tip" — a v5 spoke is not behind v4.
    function upgradeTargetLabel(v) {
      var t = v && v.target;
      if (!t) return 'Upstream tip';
      var branch = t.branch || 'upstream';
      if (t.source === 'channel') return 'Tracked channel :' + t.channel + ' (' + t.ref + ')';
      if (t.channel) return 'Hub target (' + branch + ' via :' + t.channel + ')';
      if (t.source === 'hub') return 'Hub target (' + branch + ' tip)';
      return branch + ' tip';
    }

    function versionCompareURL(currentHash, targetHash) {
      if (!currentHash || !targetHash) return '';
      return 'https://github.com/hivecommons/hive/compare/' + encodeURIComponent(String(currentHash)) + '...' + encodeURIComponent(String(targetHash));
    }

    function versionStatusText(v, offeredUpgradeShort) {
      if (v.commitsBehind != null && v.commitsBehind > 0) return String(v.commitsBehind) + ' behind';
      if (v.behind) return 'Upgrade available' + (offeredUpgradeShort ? ' → ' + offeredUpgradeShort : '');
      if (v.stableV4ImageReady === false) return 'tip not built yet';
      if (v.latestHash) return 'Up to date';
      return 'Version current';
    }

    /* Scheduled-upgrade progress (#14950). /api/version carries the state
       the hub and spoke derive together (upgradeProgress: scheduled →
       draining → pulling → rolling → verifying → done | missed) and the
       hub's history of recent attempts (upgradeHistory). The menu shows the
       state as a pill — with the drain countdown and the agents holding the
       upgrade — and lists the history so a missed window is never silent. */
    function versionStateLabels() {
      return { scheduled: 'Scheduled', delayed: 'Delayed', draining: 'Draining', pulling: 'Pulling', rolling: 'Rolling', verifying: 'Verifying', done: 'Upgraded', missed: 'Missed window' };
    }

    function versionStateLabel(state) {
      return versionStateLabels()[state] || (state ? String(state).charAt(0).toUpperCase() + String(state).slice(1) : '');
    }

    function versionFormatDuration(sec) {
      sec = Math.floor(Number(sec) || 0);
      if (sec < 0) return '';
      if (sec < 60) return sec + 's';
      const min = Math.floor(sec / 60);
      if (min < 60) return min + 'm';
      return Math.floor(min / 60) + 'h' + (min % 60) + 'm';
    }

    function versionAgentsInTurnText(st) {
      const turns = (st && st.agentsInTurn) || [];
      if (!turns.length) return '';
      const parts = turns.map(function(t) {
        const el = versionFormatDuration(t.elapsedS || 0);
        return (t.agent || '?') + (el ? ' ' + el : '');
      });
      return turns.length + (st.agentsTotal ? '/' + st.agentsTotal : '') + ' agent' + (turns.length === 1 && !st.agentsTotal ? '' : 's') + ' in turn: ' + parts.join(', ');
    }

    function versionUpgradeStateText(st) {
      if (!st || !st.state) return '';
      let text = versionStateLabel(st.state);
      if (st.state === 'draining') {
        const agents = versionAgentsInTurnText(st);
        const left = typeof st.drainRemainingS === 'number' && st.drainRemainingS > 0 ? versionFormatDuration(st.drainRemainingS) + ' left' : 'deadline reached';
        text += (agents ? ' · ' + agents : '') + ' · ' + left;
      } else if ((st.state === 'missed' || st.state === 'delayed') && st.reason) {
        text += ' · ' + st.reason;
      } else if (st.state === 'scheduled' && st.nextWindowAt) {
        const at = new Date(st.nextWindowAt);
        if (!isNaN(at.getTime())) text += ' · next window ' + formatDashboardDateTime(at);
      } else if (st.state === 'done' && st.target) {
        text += ' → ' + versionShortSHA(st.target);
      }
      return text;
    }

    function versionUpgradeStatePillHTML(st) {
      const text = versionUpgradeStateText(st);
      if (!text) return '';
      return '<span class="oc-version-state-pill is-' + escapeHtml(st.state) + '" title="' + escapeHtml(st.detail || text) + '">' + escapeHtml(text) + '</span>';
    }

    function versionUpgradeHistoryHTML(v) {
      const rows = (v && v.upgradeHistory) || [];
      if (!rows.length) return '';
      const items = rows.slice(0, 14).map(function(a) {
        const when = a.endedAt && a.endedAt !== '0001-01-01T00:00:00Z' ? a.endedAt : a.startedAt;
        const d = when ? new Date(when) : null;
        const ts = d && !isNaN(d.getTime()) ? formatDashboardDateTime(d) : '?';
        const what = a.outcome ? versionStateLabel(a.outcome) : 'In flight';
        const to = a.target ? ' → ' + versionShortSHA(a.target) : '';
        const why = a.reason ? ' — ' + a.reason : '';
        const text = what + to + (a.trigger ? ' (' + a.trigger + ')' : '') + why;
        return '<li class="' + (a.outcome === 'missed' ? 'is-missed' : '') + '" title="' + escapeHtml(ts + ': ' + text) + '"><time datetime="' + escapeHtml(when || '') + '">' + escapeHtml(ts) + '</time><span>' + escapeHtml(text) + '</span></li>';
      });
      return '<ul class="oc-version-history" aria-label="Upgrade history">' + items.join('') + '</ul>';
    }

    function versionLastUpgradeText(v) {
      const rs = v && v.releaseStatus;
      const at = rs && rs.attempt ? rs.attempt : null;
      if (at && at.state && at.state !== 'never') {
        const when = at.completedAt || at.at || '';
        let label = at.state.replace('_', ' ');
        // A superseded success is history: say so, and name the running build
        // so "succeeded · 1d ago" is not read as the age of what runs.
        if (at.superseded && at.state === 'succeeded') {
          label = 'superseded' + (at.rolledExternally ? ' (rolled externally' + (at.runningCommit ? ', now ' + at.runningCommit : '') + ')' : '');
        }
        return { text: 'Last upgrade: ' + label + (when ? ' · ' + relativeAge(when) : ''), title: when || label };
      }
      const mu = v && v.manualUpgrade;
      if (mu && mu.state) return { text: 'Manual upgrade: ' + mu.state + (mu.updatedAt ? ' · ' + relativeAge(mu.updatedAt) : ''), title: mu.updatedAt || mu.state };
      return null;
    }

    function versionShortSHA(sha) {
      sha = String(sha || '').trim();
      if (!sha) return '';
      return sha.slice(0, 7);
    }

    function versionSameCommit(a, b) {
      a = String(a || '').trim().toLowerCase();
      b = String(b || '').trim().toLowerCase();
      if (!a || !b) return false;
      const n = Math.min(a.length, b.length);
      return a.slice(0, n) === b.slice(0, n);
    }

    function versionDashHTML(reason) {
      const title = reason || 'hub has not sent a policy yet';
      return '<strong title="' + escapeHtml(title) + '">—</strong>';
    }

    function versionPolicy(v) {
      return (v && (v.upgradePolicy || v.hubUpgradePolicy)) || null;
    }

    function versionManagedSuffix(v) {
      const au = v && v.autoUpdate || {};
      const managedBy = (v && v.target && v.target.managedBy) || au.managedBy || '';
      if (managedBy === 'hub') return 'hub-managed';
      if (managedBy === 'spoke') return 'spoke-managed';
      return '';
    }

    function versionTrackingSummary(v, deliveryLabel) {
      const managed = versionManagedSuffix(v);
      let base = '';
      if (v && v.tracking === 'pinned') base = 'pinned ' + versionShortSHA(v.hash || v.short || '');
      else base = String(deliveryLabel || '').trim() || versionDeliveryLabel(v || {}) || versionTrackingLabel(v || {});
      if (!base || base === 'tracking unknown') {
        const channel = v && v.channel ? String(v.channel) : '';
        const branch = v && v.branch && v.branch !== 'unknown' ? String(v.branch) : '';
        if (channel) base = branch ? channel + ' (' + branch + ')' : channel;
        else if (branch) base = branch;
      }
      if (!base) base = 'tracking unknown';
      return [base, managed].filter(Boolean).join(' · ');
    }

    function versionCadenceLabel(v) {
      const p = versionPolicy(v) || {};
      const au = v && v.autoUpdate || {};
      const mode = String(p.schedule || au.period || '').toLowerCase();
      const hour = p.schedule_hour != null ? Number(p.schedule_hour) : null;
      const tz = p.schedule_timezone || '';
      const weekday = p.schedule_weekday || '';
      if (!mode || mode === 'unknown') return '';
      if (mode === 'instant') return 'Instant on new build';
      if (mode === 'daily') return 'Daily at ' + (hour == null || isNaN(hour) ? 'configured time' : String(hour).padStart(2, '0') + ':00') + (tz ? ' ' + tz : '');
      if (mode === 'weekly') return 'Weekly ' + (weekday ? weekday.slice(0, 3) + ' ' : '') + (hour == null || isNaN(hour) ? 'configured time' : String(hour).padStart(2, '0') + ':00') + (tz ? ' ' + tz : '');
      if (mode === 'manual' || (au.enabled === false && au.policySource === 'hub')) return 'Manual';
      return mode;
    }

    function versionStatusSummary(v, targetShort) {
      const au = v && v.autoUpdate || {};
      if (!v) return 'Checking…';
      if (au.state === 'paused') return 'paused by hub';
      if ((v.target && v.target.resolved === false) || au.state === 'unknown') return 'Checking…';
      if (v.stableV4ImageReady === false) {
        const building = ((v.target && (v.target.channel || v.target.branch)) || v.branch || 'target') + ' building';
        const dep = v.deployment || {};
        if (dep.upgradeSupported === false) return 'manual update required (' + (dep.runtime ? String(dep.runtime) : 'unknown runtime') + '); ' + building;
        return 'queued (' + building + ')';
      }
      if (au.state === 'up_to_date' || (!v.behind && targetShort && versionSameCommit(v.hash || v.short, (v.target && v.target.sha) || v.stableV4Hash || v.latestHash))) return 'up to date';
      if (au.state === 'failed') return 'upgrade failed';
      if (au.state === 'retrying') return 'upgrade retrying';
      if (v.behind || au.state === 'behind') return 'upgrade available' + (targetShort ? ' → ' + targetShort : '');
      if (au.state === 'disabled') return 'manual';
      return versionStatusText(v, targetShort);
    }

    function versionUpgradeProgressStatus(v, st) {
      if (!st) return '';
      const target = escapeHtml(st.targetShort || versionShortSHA(st.target || 'target'));
      const elapsed = versionElapsedText(st.startedAt);
      if (st.completedAt) return '✅ Upgraded to ' + target + (elapsed ? ' · ' + elapsed : '');
      const mu = v && v.manualUpgrade;
      const um = v && v.upgradeMarker;
      if ((mu && mu.state === 'failed') || (um && um.failed)) return '⚠ upgrade failed — check Audit Log';
      if (st.overdue) return '⚠ upgrade taking longer than expected — <a href="#audit-section" data-action="ocNavigate" data-arg0="audit-section">check Audit Log</a>' + (elapsed ? ' · ' + escapeHtml(elapsed) : '');
      let phase = 'queued';
      if (v && v.stableV4ImageReady === false) phase = 'building';
      else if (um && !um.failed) phase = 'restarting';
      else if (mu && mu.state === 'started') phase = 'rolling out';
      else if (_upgradeInProgress) phase = 'rolling out';
      return phase + ' → ' + target + (elapsed ? ' · ' + elapsed : '');
    }

    function versionUpgradeKey(st) {
      if (!st) return '';
      return [st.target || st.targetShort || 'target', st.startedAt || '0'].map(function(part) { return String(part || '').trim(); }).join('|');
    }

    function versionBeeProgressHTML(st, v) {
      if (!st) return '';
      const title = st.completedAt ? 'Upgrade complete' : 'Upgrading…';
      const key = versionUpgradeKey(st);
      return '<div class="oc-version-upgrade-progress" data-upgrade-key="' + escapeHtml(key) + '" role="status" aria-live="polite">' +
        versionUpgradeHiveHTML(key) + '<div class="oc-version-progress-copy"><div class="oc-version-progress-title" data-upgrade-title="1">' + escapeHtml(title) + '</div>' +
        '<div class="oc-version-progress-status" data-upgrade-status="1">' + versionUpgradeProgressStatus(v || {}, st) + '</div></div></div>';
    }

    function versionBeeGlyphHTML() {
      return '<svg class="oc-version-bee-glyph" viewBox="0 0 18 12" focusable="false" aria-hidden="true">' +
        '<ellipse class="oc-version-bee-wing" cx="7" cy="3.6" rx="3" ry="1.5" transform="rotate(-18 7 3.6)"/>' +
        '<ellipse class="oc-version-bee-wing" cx="11" cy="3.4" rx="3" ry="1.5" transform="rotate(18 11 3.4)"/>' +
        '<ellipse class="oc-version-bee-body" cx="9.2" cy="7" rx="5.8" ry="3.3"/>' +
        '<path class="oc-version-bee-stripe" d="M7.2 4.2v5.6M10 3.8v6.4"/>' +
        '<circle class="oc-version-bee-head" cx="15" cy="7" r="1.7"/>' +
        '</svg>';
    }

    function versionUpgradeHiveHTML(key) {
      const keyAttr = key ? ' data-upgrade-hive-key="' + escapeHtml(key) + '"' : '';
      const beeA = '<span class="oc-version-bee-orbit oc-version-bee--phase-a" aria-hidden="true"><span class="oc-version-bee">' + versionBeeGlyphHTML() + '</span></span>';
      const beeB = '<span class="oc-version-bee-orbit oc-version-bee--phase-b" aria-hidden="true"><span class="oc-version-bee">' + versionBeeGlyphHTML() + '</span></span>';
      const beeC = '<span class="oc-version-bee-orbit oc-version-bee--phase-c" aria-hidden="true"><span class="oc-version-bee">' + versionBeeGlyphHTML() + '</span></span>';
      return '<span class="oc-version-hive"' + keyAttr + ' aria-hidden="true">' +
        '<svg viewBox="0 0 64 64" focusable="false"><path d="M32 4 56 18v28L32 60 8 46V18Z" fill="none" stroke="currentColor" stroke-width="4"/><path d="M32 4v56M8 18l48 28M56 18 8 46" fill="none" stroke="currentColor" stroke-width="2" opacity="0.38"/></svg>' +
        beeA + beeB + beeC + '<span class="oc-version-honey-dot"></span>' +
        '</span>';
    }

    function versionNavbarUpgradeHTML(st, v) {
      if (!st) return '';
      const target = st.targetShort || versionShortSHA(st.target || '');
      const elapsed = versionElapsedText(st.startedAt);
      const complete = !!st.completedAt;
      const title = complete ? 'Upgrade complete ✓' : 'Upgrading…';
      const status = complete ? (target || '') : ((target ? '→ ' + target : versionUpgradeProgressStatus(v || {}, st)) + (elapsed ? ' · ' + elapsed : ''));
      const key = versionUpgradeKey(st);
      return '<span class="oc-version-navbar-upgrade" data-upgrade-key="' + escapeHtml(key) + '" role="status" aria-live="polite">' +
        versionUpgradeHiveHTML(key) + '<span class="oc-version-progress-copy"><span class="oc-version-progress-title" data-upgrade-title="1">' + escapeHtml(title) + (complete ? '' : ' ') + '</span>' +
        '<span class="oc-version-progress-status" data-upgrade-status="1">' + escapeHtml(status) + '</span></span></span>';
    }

    function versionSetHTMLPreservingUpgradeBee(el, html, upgradeKey) {
      if (!el) return false;
      if (!upgradeKey || !el.querySelector || !el.replaceChildren || !document || !document.createElement) {
        el.innerHTML = html;
        return false;
      }
      const currentProgress = el.querySelector('[data-upgrade-key]');
      if (!currentProgress || currentProgress.getAttribute('data-upgrade-key') !== upgradeKey) {
        el.innerHTML = html;
        return false;
      }
      const currentHive = currentProgress.querySelector && currentProgress.querySelector('.oc-version-hive');
      const template = document.createElement('template');
      template.innerHTML = String(html || '').trim();
      const nextProgress = template.content && template.content.querySelector && template.content.querySelector('[data-upgrade-key]');
      if (!currentHive || !nextProgress || nextProgress.getAttribute('data-upgrade-key') !== upgradeKey) {
        el.innerHTML = html;
        return false;
      }
      const currentTitle = currentProgress.querySelector && currentProgress.querySelector('[data-upgrade-title]');
      const currentStatus = currentProgress.querySelector && currentProgress.querySelector('[data-upgrade-status]');
      const nextTitle = nextProgress.querySelector && nextProgress.querySelector('[data-upgrade-title]');
      const nextStatus = nextProgress.querySelector && nextProgress.querySelector('[data-upgrade-status]');
      if (currentTitle && nextTitle) currentTitle.innerHTML = nextTitle.innerHTML;
      if (currentStatus && nextStatus) currentStatus.innerHTML = nextStatus.innerHTML;
      if (currentProgress.parentNode === el || currentProgress.parentNode) {
        return true;
      }
      const nextHive = nextProgress.querySelector && nextProgress.querySelector('.oc-version-hive');
      if (!nextHive || !nextHive.replaceWith) {
        el.innerHTML = html;
        return false;
      }
      nextHive.replaceWith(currentHive);
      el.replaceChildren.apply(el, Array.from(template.content.childNodes));
      return true;
    }

    function versionButtonHTML(label, title, targetHash, enabled) {
      label = String(label || '').trim();
      if (!label) return '';
      const disabled = enabled ? '' : ' disabled aria-disabled="true"';
      const action = enabled ? ' data-action="gh27" data-arg0="' + escapeHtml(targetHash || '') + '" data-mouseover-action="gh28" data-mouseout-action="gh29"' : '';
      const available = enabled ? ' oc-version-upgrade-available' : '';
      return '<button id="spoke-upgrade-btn" class="hv-btn btn-primary btn-sm' + available + '" type="button"' + action + disabled + ' title="' + escapeHtml(title || label) + '">' + escapeHtml(label) + '</button>';
    }

    function versionHiveDisplayName(v) {
      v = v || {};
      const names = [v.hive_name, v.display_name, v.instance_name, v.spoke_name];
      for (let i = 0; i < names.length; i++) {
        const name = String(names[i] || '').trim();
        if (name) return name;
      }
      return 'Hive';
    }

    function versionRuntimeUpgradeCaveat(runtimeLabel) {
      runtimeLabel = String(runtimeLabel || '').trim();
      if (!runtimeLabel || runtimeLabel === 'unknown runtime') return '';
      return 'Runs on ' + runtimeLabel + ' — the ' + runtimeLabel + ' deployment is restarted with the new image; the cluster itself is not changed.';
    }

    function versionJoinCaveats() {
      return Array.from(arguments).map(function(c) { return String(c || '').trim(); }).filter(Boolean).join(' ');
    }

    function renderVersionUpgradeAction(v, ctx) {
      ctx = ctx || {};
      const targetHash = String(ctx.offeredUpgradeHash || '').trim();
      const targetShort = ctx.offeredUpgradeShort || versionShortSHA(targetHash) || 'target';
      const au = v && v.autoUpdate || {};
      const target = v && v.target || {};
      const dep = v && v.deployment || {};
      const runtimeLabel = dep.runtime ? String(dep.runtime) : 'unknown runtime';
      const runtimeDeploymentSuffix = runtimeLabel === 'unknown runtime' ? '' : ' (' + runtimeLabel + ' deployment)';
      const progress = ctx.upgradeProgress || null;
      if (progress && !progress.completedAt) {
        return versionButtonHTML('Upgrading…', 'Upgrade to ' + (progress.targetShort || targetShort) + ' is in progress', progress.target || targetHash, false) + versionBeeProgressHTML(progress, v || {});
      }
      if (!targetHash || target.resolved === false || au.state === 'unknown') {
        return versionButtonHTML('Checking…', 'Checking upgrade target — the hub has not resolved a target commit yet', '', false);
      }
      if (versionSameCommit(v && (v.hash || v.short), targetHash) || au.state === 'up_to_date') {
        const done = progress && progress.completedAt ? versionBeeProgressHTML(progress, v || {}) : '';
        return versionButtonHTML('Up to date ✓', 'Running the resolved target commit ' + targetShort, targetHash, false) + done;
      }
      if (au.state === 'paused' || target.paused) {
        return versionButtonHTML('Upgrade paused', 'Automatic upgrades are paused by the hub', targetHash, false);
      }
      if (dep.upgradeSupported !== true) {
        const why = dep.reason ? String(dep.reason) : 'deployment runtime is unknown';
        const pill = ctx.manualUpdateHTML || '<span class="oc-version-manual-pill" title="' + escapeHtml(why) + '">manual update required (' + escapeHtml(runtimeLabel) + ')</span>';
        return versionButtonHTML('Upgrade to ' + targetShort, why + ' (' + runtimeLabel + ')', targetHash, false) +
          '<div class="oc-version-upgrade-note" role="note">' + pill + ' ' + escapeHtml(why) + '</div>';
      }
      return versionButtonHTML('Upgrade to ' + targetShort, 'Upgrade this Hive' + runtimeDeploymentSuffix + ' to ' + (ctx.offeredUpgradeLabel || 'target') + ' ' + targetShort + ' now', target.ref || targetHash, true);
    }

    function versionBuildSkew(stdioBuild, hubShort) {
      const stdio = String(stdioBuild || '').trim();
      const hub = String(hubShort || '').trim();
      if (!stdio || !hub || stdio.toLowerCase() === 'unknown' || hub.toLowerCase() === 'unknown') {
        return false;
      }
      if (stdio.toLowerCase().includes('unidentified')) {
        return false;
      }
      let stdioShort = stdio;
      const plusIdx = stdio.indexOf('+');
      if (plusIdx !== -1) {
        stdioShort = stdio.slice(plusIdx + 1).trim();
      }
      if (!stdioShort || stdioShort.toLowerCase() === 'unknown') {
        return false;
      }
      const n = Math.min(stdioShort.length, hub.length, 7);
      if (n === 0) return false;
      return stdioShort.slice(0, n).toLowerCase() !== hub.slice(0, n).toLowerCase();
    }

    function renderVersionDetails(v, ctx) {
      ctx = ctx || {};
      const fullHash = String(v && v.hash || 'unknown');
      const shortHash = versionShortSHA(v && (v.short || v.hash)) || 'unknown';
      const deliveryLabel = ctx.deliveryLabel || versionDeliveryLabel(v || {}) || 'unknown';
      const targetHash = ctx.offeredUpgradeHash || (v && v.latestHash) || '';
      const targetShort = ctx.offeredUpgradeShort || versionShortSHA(targetHash) || '';
      const compareURL = versionCompareURL(fullHash, targetHash);
      const lastUpgrade = versionLastUpgradeText(v);
      const commitURL = 'https://github.com/hivecommons/hive/commit/' + encodeURIComponent(fullHash);
      const trackingText = versionTrackingSummary(v || {}, deliveryLabel);
      const statusText = ctx.statusText || versionStatusSummary(v || {}, targetShort);
      const cadence = versionCadenceLabel(v || {});
      const au = (v && v.autoUpdate) || {};
      const currentHTML = fullHash && fullHash !== 'unknown'
        ? '<strong><a href="' + escapeHtml(commitURL) + '" target="_blank" rel="noopener noreferrer" title="' + escapeHtml(fullHash) + '">' + escapeHtml(shortHash) + '</a></strong>'
        : versionDashHTML('running commit is unavailable');
      const row = function(label, valueHTML, title) { return '<div class="oc-version-row"' + (title ? ' title="' + escapeHtml(title) + '"' : '') + '><span>' + escapeHtml(label) + '</span>' + valueHTML + '</div>'; };
      const value = function(text, title) { return '<strong title="' + escapeHtml(title || text || '') + '">' + escapeHtml(text || '—') + '</strong>'; };
      const upgradeActionHTML = renderVersionUpgradeAction(v || {}, ctx).trim();
      const copyLines = ['Hive ' + shortHash].concat(v && v.release ? ['release ' + v.release] : []).concat(['hash ' + fullHash, 'channel ' + deliveryLabel, 'tracking ' + trackingText, 'cadence ' + (cadence || '—'), statusText]);
      if (v && v.last_seen_stdio_build) {
        copyLines.push('stdio ' + v.last_seen_stdio_build);
      }
      window._versionCopyText = copyLines.join('\n');
      window._versionHashCopyText = fullHash;
      let html = '';
      html += '<div class="avatar-menu-label">Version</div>';
      html += row('Current', currentHTML, fullHash);
      if (v && v.release) html += row('Release', value(v.release), v.release);
      if (v && v.last_seen_stdio_build) {
        const stdioBuild = String(v.last_seen_stdio_build);
        const isSkew = versionBuildSkew(stdioBuild, shortHash || fullHash);
        const warningHTML = isSkew
          ? ' <span class="text-danger oc-version-skew" title="Stdio client build differs from hub build">⚠ build skew</span>'
          : '';
        const stdioAt = v.last_seen_stdio_at ? new Date(v.last_seen_stdio_at) : null;
        const stdioAtOK = stdioAt && !isNaN(stdioAt.getTime());
        const timeTitle = stdioAtOK ? 'Last seen ' + (typeof formatDashboardDateTime === 'function' ? formatDashboardDateTime(stdioAt) : v.last_seen_stdio_at) : '';
        const rel = stdioAtOK && typeof relativeAge === 'function' ? relativeAge(stdioAt) : '';
        const timeSuffix = rel ? ' <span class="text-muted oc-version-stdio-time">(' + escapeHtml(rel) + ')</span>' : '';
        const stdioHTML = '<strong>' + escapeHtml(stdioBuild) + '</strong>' + timeSuffix + warningHTML;
        html += row('Stdio client', stdioHTML, timeTitle || stdioBuild);
      }
      html += row('Channel', deliveryLabel ? value(deliveryLabel) : versionDashHTML('release channel is unavailable'), deliveryLabel || 'release channel is unavailable');
      html += row('Tracking', trackingText ? value(trackingText, versionTrackingTooltip(v || {})) : versionDashHTML('hub has not sent a policy yet'), trackingText || 'hub has not sent a policy yet');
      html += row('Status', statusText ? value(statusText, (v && v.autoUpdate && v.autoUpdate.detail) || statusText) : versionDashHTML('status is still loading'), statusText || 'status is still loading');
      html += row('Cadence', cadence ? value(cadence) : versionDashHTML('hub has not sent a policy yet'), cadence || 'hub has not sent a policy yet');
      if (au.enabled !== false && au.state !== 'disabled' && au.state !== 'paused') {
        const nextAt = au.nextUpdateAt ? new Date(au.nextUpdateAt) : null;
        const nextOK = nextAt && !isNaN(nextAt.getTime());
        const nextStatus = au.nextUpdateStatus || '';
        if (nextOK && nextAt.getTime() <= Date.now()) {
          html += row('Next update', value('overdue since ' + formatDashboardDateTime(nextAt), 'The next build crossed the 24h soak line at ' + au.nextUpdateAt + ' but the hub has not promoted it yet — the promotion workflow runs hourly and may be delayed or failing'), 'overdue since ' + au.nextUpdateAt);
        } else if (nextOK) {
          html += row('Next update', value(formatDashboardDateTime(nextAt), 'Expected ' + au.nextUpdateAt + ' — the hub\'s estimate; it may land later if a promotion check holds'), au.nextUpdateAt);
        } else if (nextStatus === 'none') {
          html += row('Next update', value('none queued (stable is current)', 'Stable and candidate currently point at the same built image'), 'none queued');
        } else if (nextStatus === 'paused') {
          html += row('Next update', value('paused', 'Stable auto-promotion is paused'), 'paused');
        } else {
          html += row('Next update', value('unknown', 'The hub has not reported when the next update is expected'), 'unknown');
        }
      }
      if (au.heldReason) html += row('Held', value(au.heldReason, 'The scheduled update window opened but the hub held the upgrade' + (au.heldAt ? ' at ' + au.heldAt : '') + ': ' + au.heldReason), au.heldReason);
      const statePill = versionUpgradeStatePillHTML(v && v.upgradeProgress);
      if (statePill) html += row('Progress', statePill, (v.upgradeProgress && v.upgradeProgress.detail) || '');
      if (lastUpgrade) html += row('History', value(lastUpgrade.text, lastUpgrade.title || lastUpgrade.text), lastUpgrade.title || lastUpgrade.text);
      const historyHTML = versionUpgradeHistoryHTML(v);
      if (historyHTML) html += '<div class="oc-version-row" title="Recent scheduled and manual upgrade attempts, as recorded by the hub"><span>Upgrade history</span></div>' + historyHTML;
      if (compareURL && (v && (v.behind || (v.commitsBehind != null && v.commitsBehind > 0)))) {
        html += '<a class="avatar-menu-item" href="' + escapeHtml(compareURL) + '" target="_blank" rel="noopener noreferrer">Compare with target ↗</a>';
      }
      html += '<a class="avatar-menu-item" href="https://github.com/hivecommons/hive/releases" target="_blank" rel="noopener noreferrer">Changelog / release notes ↗</a>';
      html += '<div class="avatar-menu-divider"></div>';
      html += '<div class="oc-version-actions">' + upgradeActionHTML + '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="copyVersionHashFromMenu">Copy SHA</button><button type="button" class="hv-btn btn-secondary btn-sm" data-action="copyVersionFromMenu">Copy version details</button></div>';
      return html;
    }

    function renderVersionMenu(v, ctx) {
      const menu = ensureVersionMenuPortal();
      if (!menu) return;
      const html = renderVersionDetails(v, ctx);
      const progress = ctx && ctx.upgradeProgress || null;
      versionSetHTMLPreservingUpgradeBee(menu, html, progress ? versionUpgradeKey(progress) : '');
    }

    function versionManualUpgradeActive(v, offeredTargetHash) {
      const mu = v && v.manualUpgrade;
      if (!mu || mu.state !== 'started' || !mu.target) return false;
      if (versionSameCommit(v.hash || v.short, mu.target)) return false;
      if (offeredTargetHash && !versionSameCommit(offeredTargetHash, mu.target)) return false;
      return true;
    }

    function renderVersionChip(v, ctx) {
      const chip = document.getElementById('oc-version-chip');
      if (!chip) return;
      const fullHash = String(v.hash || '');
      const shortHash = versionShortSHA(v.short || fullHash) || 'unknown';
      const channel = ctx.deliveryLabel || (v.branch && v.branch !== 'unknown' ? String(v.branch) : 'unknown');
      const progress = ctx.upgradeProgress || null;
      const updateDot = ctx.hasUpgrade ? '<span class="oc-version-update-dot" title="Upgrade available">↑</span>' : '';
      const upgrading = progress && !progress.completedAt;
      const serverState = v.upgradeProgress || null;
      const stateChip = !upgrading && serverState && (serverState.state === 'draining' || serverState.state === 'missed')
        ? '<span class="oc-version-state-pill is-' + escapeHtml(serverState.state) + '" title="' + escapeHtml(serverState.detail || versionUpgradeStateText(serverState)) + '">' + escapeHtml(versionStateLabel(serverState.state)) + (serverState.state === 'draining' && serverState.drainRemainingS > 0 ? ' ' + escapeHtml(versionFormatDuration(serverState.drainRemainingS)) : '') + '</span>'
        : '';
      const progressGlyph = (progress && progress.completedAt ? '<span class="text-success" title="Upgrade complete">✓</span>' : '') + stateChip;
      if (chip.classList && chip.classList.toggle) chip.classList.toggle('is-upgrading', !!upgrading);
      const html = upgrading ? versionNavbarUpgradeHTML(progress, v) + '<span class="oc-version-chip-chevron" aria-hidden="true">▼</span>' :
        (v.release ? '<span class="oc-version-release">v' + escapeHtml(String(v.release).replace(/^v/i, '')) + '</span>' : '') +
        '<span class="oc-version-sha" title="' + escapeHtml(fullHash || shortHash) + '">' + escapeHtml(shortHash) + '</span>' +
        (channel ? '<span class="oc-version-channel">' + escapeHtml(channel) + '</span>' : '') + progressGlyph + updateDot + '<span class="oc-version-chip-chevron" aria-hidden="true">▼</span>';
      versionSetHTMLPreservingUpgradeBee(chip, html, upgrading ? versionUpgradeKey(progress) : '');
      chip.title = (v.release ? 'Release ' + v.release + ' — ' : '') + (progress && !progress.completedAt ? 'Upgrade in progress — Version & upgrade details' : 'Version & upgrade details');
      chip.setAttribute('aria-label', upgrading ? 'Upgrade in progress' + (progress.targetShort ? ' to ' + progress.targetShort : '') + ' — Version & upgrade details' : 'Version & upgrade details');
    }

    function renderNavbarUpgradeIndicator(v, ctx) {
      const el = document.getElementById('oc-navbar-upgrade');
      if (!el) return;
      const progress = ctx && ctx.upgradeProgress || null;
      if (!progress) {
        el.hidden = true;
        el.innerHTML = '';
        return;
      }
      const complete = !!progress.completedAt;
      el.hidden = false;
      versionSetHTMLPreservingUpgradeBee(el, versionNavbarUpgradeHTML(progress, v || {}), versionUpgradeKey(progress));
      const detail = versionUpgradeProgressStatus(v || {}, progress);
      el.title = (complete ? 'Upgrade complete' : 'Upgrade in progress') + (detail ? ' — ' + detail : '') + ' — jump to Version details';
      el.setAttribute('aria-label', complete ? 'Upgrade complete — Version details' : 'Upgrade in progress' + (progress.targetShort ? ' to ' + progress.targetShort : '') + ' — Version details');
    }

    function renderVersionSurfaces(v, ctx) {
      renderVersionChip(v, ctx);
      renderVersionMenu(v, ctx);
      renderNavbarUpgradeIndicator(v, ctx);
    }

    function refreshVersionProgressSurfaces() {
      const ctx = window._lastVersionContext;
      if (!ctx || !ctx.upgradeProgress) return;
      ctx.upgradeProgress = versionReadUpgradeProgress();
      renderVersionSurfaces(window._lastVersionData || {}, ctx);
    }

    function focusVersionBlock() {
      const block = document.getElementById('oc-git-version');
      const chip = document.getElementById('oc-version-chip');
      const target = chip || block;
      if (!target) return;
      target.scrollIntoView({ behavior: 'smooth', block: 'center' });
      if (chip && chip.focus) chip.focus({ preventScroll: true });
      if (block) block.classList.add('oc-version-focus-pulse');
      setTimeout(function() { if (block) block.classList.remove('oc-version-focus-pulse'); }, 1200);
    }

    async function fetchGitVersion() {
      try {
        const res = await fetch('/api/version');
        const v = await res.json();
        const el = document.getElementById('git-version');
        const ocEl = document.getElementById('oc-git-version');
        const deliveryLabel = versionDeliveryLabel(v);
        const trackingLabel = versionTrackingLabel(v);
        const um = v.upgradeMarker;
        const mu = v.manualUpgrade;
        const policyTargetHashEarly = v.upgradePolicy && (v.upgradePolicy.target_sha || v.upgradePolicy.armed_target) || '';
        const liveOfferedTargetHash = (v.target && v.target.sha) || policyTargetHashEarly || v.stableV4Hash || v.latestHash || '';
        let manualUpgradeActive = versionManualUpgradeActive(v, liveOfferedTargetHash);
        const commitsBehindText = v.commitsBehind != null && v.commitsBehind > 0
          ? ` <span class="git-behind" title="Current: ${escapeHtml(v.short || '?')} → ${escapeHtml(upgradeTargetLabel(v))}: ${escapeHtml(v.stableV4Short || '?')}">${escapeHtml(String(v.commitsBehind))} behind</span>`
          : (v.commitsBehind == null && v.stableV4Hash && String(v.short || '').toLowerCase() !== String(v.stableV4Short || '').toLowerCase()
            ? (v.stableV4ImageReady === false
              ? ` <span class="text-muted" title="${escapeHtml(upgradeTargetLabel(v))} ${escapeHtml(v.stableV4Short || '?')} has no published image yet — nothing to upgrade to">tip not built yet</span>`
              : ` <span class="git-behind" title="Could not compare this commit with ${escapeHtml(upgradeTargetLabel(v))} ${escapeHtml(v.stableV4Short || '?')}">? behind</span>`)
            : '');
        if (mu && mu.state === 'started' && mu.target) {
          if (!manualUpgradeActive) {
            versionClearUpgradeProgress();
          } else {
            _upgradeInProgress = true;
            _upgradeTargetHash = mu.target || _upgradeTargetHash;
          }
        } else if (mu && mu.state === 'started') {
          versionClearUpgradeProgress();
        } else if (mu && mu.state === 'done') {
          _upgradeInProgress = false;
          _upgradeTargetHash = null;
        }
        let upgradeProgress = typeof versionReconcileUpgradeProgress === 'function' ? versionReconcileUpgradeProgress(v) : null;
        if (!upgradeProgress) upgradeProgress = versionAdoptHubUpgradeProgress(v);
        const upgradeCurrentHash = String(v.hash || '').toLowerCase();
        const upgradeTargetHash = String(_upgradeTargetHash || '').toLowerCase();
        const policyTargetHash = v.upgradePolicy && (v.upgradePolicy.target_sha || v.upgradePolicy.armed_target) || '';
        const hasHubTarget = !!(v.target && v.target.source === 'hub');
        const channelTarget = v.target && v.target.source === 'channel';
        const offeredUpgradeHash = channelTarget ? (v.target.sha || '') : ((v.target && v.target.sha) || policyTargetHash || v.stableV4Hash || (hasHubTarget ? '' : (v.latestHash || '')));
        const offeredUpgradeShort = channelTarget ? (v.target.short || '') : ((v.target && v.target.short) || versionShortSHA(policyTargetHash) || v.stableV4Short || (hasHubTarget ? '' : (v.latestShort || 'latest')));
        const offeredUpgradeRef = (v.target && v.target.ref) || offeredUpgradeHash;
        const offeredUpgradeLabel = upgradeTargetLabel(v) || 'target';
        const upgradeTargetLanded = upgradeTargetHash && (upgradeCurrentHash.startsWith(upgradeTargetHash) || upgradeTargetHash.startsWith(upgradeCurrentHash));
        let compactStatusHTML = '';
        let statusText = '';
        let upgradeHTML = '';
        let manualUpdateHTML = '';
        const dep = v.deployment || {};
        const upgradeSupported = dep.upgradeSupported === true;
        const runtimeLabel = dep.runtime ? String(dep.runtime) : 'unknown runtime';
        const runtimeDeploymentSuffix = runtimeLabel === 'unknown runtime' ? '' : ' (' + runtimeLabel + ' deployment)';
        if (!upgradeSupported) {
          const why = dep.reason ? String(dep.reason) : 'deployment runtime is unknown';
          manualUpdateHTML = `<span class="oc-version-manual-pill" title="${escapeHtml(why)}">manual update required (${escapeHtml(runtimeLabel)})</span>`;
        }
        if (um && um.failed) {
          statusText = 'Auto-update failed' + (um.target ? ' → ' + um.target : '');
          compactStatusHTML = '<span class="git-behind text-danger" title="Auto-update failed">⚠ auto-update failed</span>';
        } else if (um) {
          statusText = 'Auto-update retrying ' + String(um.attempts) + '/' + String(um.maxAttempts);
          compactStatusHTML = '<span class="git-behind" title="Auto-update retrying">⟳ auto-update retrying ' + escapeHtml(String(um.attempts)) + '/' + escapeHtml(String(um.maxAttempts)) + '</span>';
        } else if (mu && mu.state === 'failed') {
          statusText = 'Upgrade failed' + (mu.target ? ' → ' + mu.target : '');
          compactStatusHTML = '<span class="git-behind text-danger" title="Upgrade failed">⚠ upgrade failed</span>';
        } else if (manualUpgradeActive && _upgradeInProgress && !upgradeTargetLanded) {
          statusText = 'Upgrading' + (offeredUpgradeShort ? ' → ' + offeredUpgradeShort : '');
          compactStatusHTML = `<span class="git-behind" title="Upgrading to ${escapeHtml(offeredUpgradeShort || '?')}">⬆ upgrading</span>`;
        } else if (manualUpgradeActive && _upgradeInProgress && upgradeTargetLanded) {
          upgradeProgress = versionMarkUpgradeComplete(upgradeProgress || { target: _upgradeTargetHash, targetShort: offeredUpgradeShort || _upgradeTargetHash, startedAt: versionNowMs() });
          showToast('Upgrade complete!', 'success');
          statusText = 'Up to date';
          compactStatusHTML = '<span class="text-success" title="Up to date with remote">✓</span>';
        } else if (v.behind && !manualUpgradeActive) {
          statusText = versionStatusText(v, offeredUpgradeShort);
          if (v.commitsBehind != null && Number(v.commitsBehind) > 0) statusText = String(v.commitsBehind) + ' behind';
          compactStatusHTML = commitsBehindText || `<span class="git-behind" title="Current: ${escapeHtml(v.short || '?')} → Latest: ${escapeHtml(v.latestShort || '?')}">⬆</span>`;
          if (!upgradeSupported) {
            // manualUpdateHTML (reason pill) is rendered above for every disabled-upgrade state.
          } else if (v.autoUpgrade && !um) {
            compactStatusHTML += ` <span style="color:var(--muted);font-size:var(--fs-xs);margin-left:var(--sp-3);white-space:nowrap;padding:1px 7px;border-radius:var(--r-pill);border:1px solid var(--border)" title="This hive is upgraded automatically by the hub (auto-upgrade). Current: ${escapeHtml(v.short || '?')} → ${escapeHtml(offeredUpgradeLabel)}: ${escapeHtml(offeredUpgradeShort)}">Queued for auto-upgrade</span>`;
            upgradeHTML = `<button id="spoke-upgrade-btn" data-action="gh27" class="hv-btn btn-primary btn-sm" data-arg0="${escapeHtml(offeredUpgradeHash || '')}" data-mouseover-action="gh28" data-mouseout-action="gh29" title="Upgrade this Hive${escapeHtml(runtimeDeploymentSuffix)} to ${escapeHtml(offeredUpgradeLabel)} ${escapeHtml(offeredUpgradeShort)} now instead of waiting for auto-upgrade">Upgrade now</button>`;
          } else {
            upgradeHTML = `<button id="spoke-upgrade-btn" data-action="gh27" class="hv-btn btn-primary btn-sm" data-arg0="${escapeHtml(offeredUpgradeRef || '')}" data-mouseover-action="gh28" data-mouseout-action="gh29" title="Upgrade this Hive${escapeHtml(runtimeDeploymentSuffix)} to ${escapeHtml(offeredUpgradeLabel)} ${escapeHtml(offeredUpgradeShort)} now">↑ Upgrade → ${escapeHtml(offeredUpgradeShort)}</button>`;
          }
        } else if (v.latestHash) {
          statusText = 'Up to date';
          compactStatusHTML = '<span class="text-success" title="Up to date with remote">✓</span>';
        }
        if (!v.behind) compactStatusHTML += commitsBehindText;
        if (!statusText) statusText = versionStatusText(v, offeredUpgradeShort);
        const hasUpgrade = !!(!upgradeProgress && offeredUpgradeHash && !versionSameCommit(v.hash || v.short, offeredUpgradeHash) && !manualUpgradeActive && !(v.target && v.target.resolved === false));
        const legacyHTML = compactStatusHTML;
        if (el) el.innerHTML = '';
        if (ocEl) {
          window._lastVersionData = v;
          window._lastVersionContext = { deliveryLabel, trackingLabel, offeredUpgradeHash, offeredUpgradeShort, offeredUpgradeLabel, upgradeHTML, manualUpdateHTML, hasUpgrade, statusText: versionStatusSummary(v, offeredUpgradeShort), upgradeProgress };
          renderVersionSurfaces(v, window._lastVersionContext);
        }
        if (typeof versionScheduleUpgradePoll === 'function') versionScheduleUpgradePoll(upgradeProgress);
        window._lastVersionHTML = legacyHTML;
        renderReleaseStatus(v.releaseStatus);
      } catch (_) {
        if (typeof versionScheduleUpgradePoll === 'function') versionScheduleUpgradePoll(typeof versionReadUpgradeProgress === 'function' ? versionReadUpgradeProgress() : null);
      }
    }

    function closeVersionMenu() {
      const menu = ensureVersionMenuPortal();
      const chip = document.getElementById('oc-version-chip');
      if (menu) menu.hidden = true;
      if (chip) chip.setAttribute('aria-expanded', 'false');
    }

    function ensureVersionMenuPortal() {
      const menu = document.getElementById('oc-version-menu');
      if (!menu || !document.body) return menu;
      if (menu.parentElement !== document.body) document.body.appendChild(menu);
      return menu;
    }

    function positionVersionMenu() {
      const chip = document.getElementById('oc-version-chip');
      const menu = ensureVersionMenuPortal();
      if (!chip || !menu || typeof window === 'undefined') return;
      const viewportW = Math.max(320, window.innerWidth || document.documentElement.clientWidth || 0);
      const viewportH = Math.max(240, window.innerHeight || document.documentElement.clientHeight || 0);
      const gutter = 12;
      const gap = 10;
      const minUsefulWidth = Math.min(420, Math.max(0, viewportW - gutter * 2));
      let width = Math.min(640, Math.max(0, viewportW - gutter * 2));
      const rect = chip.getBoundingClientRect ? chip.getBoundingClientRect() : { left: gutter, top: gutter, bottom: gutter };
      const sidebar = document.querySelector ? document.querySelector('.oc-sidebar') : null;
      const sidebarRect = sidebar && sidebar.getBoundingClientRect ? sidebar.getBoundingClientRect() : null;
      const besideLeft = sidebarRect ? sidebarRect.right + gap : 0;
      const besideWidth = sidebarRect ? viewportW - besideLeft - gutter : 0;
      const placeBesideSidebar = sidebarRect && besideWidth >= minUsefulWidth;
      let left;
      if (placeBesideSidebar) {
        left = besideLeft;
        width = Math.min(640, Math.max(0, besideWidth));
      } else {
        left = rect.left;
        if (left + width > viewportW - gutter) left = viewportW - gutter - width;
        if (left < gutter) left = gutter;
      }
      menu.style.width = width + 'px';
      menu.style.maxWidth = width + 'px';
      menu.style.maxHeight = Math.max(0, viewportH - gutter * 2) + 'px';
      const height = Math.min(menu.offsetHeight || 0, Math.max(0, viewportH - gutter * 2));
      let top;
      if (placeBesideSidebar) {
        top = rect.top + ((rect.height || 0) / 2) - (height / 2);
      } else {
        top = rect.top - height - gap;
        if (top < gutter) top = rect.bottom + gap;
      }
      if (top + height > viewportH - gutter) top = Math.max(gutter, viewportH - gutter - height);
      if (top < gutter) top = gutter;
      menu.style.left = left + 'px';
      menu.style.top = top + 'px';
      menu.style.right = 'auto';
      menu.style.bottom = 'auto';
    }

    function positionVersionMenuIfOpen() {
      const menu = ensureVersionMenuPortal();
      if (menu && !menu.hidden) positionVersionMenu();
    }

    function toggleVersionMenu() {
      const menu = ensureVersionMenuPortal();
      const chip = document.getElementById('oc-version-chip');
      if (!menu || !chip) return;
      const nextHidden = !menu.hidden ? true : false;
      if (!nextHidden && !menu.innerHTML.trim()) {
        renderVersionMenu(window._lastVersionData || {}, window._lastVersionContext || {});
      }
      menu.hidden = nextHidden;
      chip.setAttribute('aria-expanded', nextHidden ? 'false' : 'true');
      if (!nextHidden) {
        positionVersionMenu();
        const first = menu.querySelector('button, a, [tabindex]:not([tabindex="-1"])');
        if (first) first.focus();
      }
    }

    async function copyVersionFromMenu() {
      const text = window._versionCopyText || '';
      if (!text) return;
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) await navigator.clipboard.writeText(text);
        showToast('Version copied', 'success');
      } catch (e) {
        showToast('Could not copy version', 'error');
      }
    }

    async function copyVersionHashFromMenu() {
      const text = window._versionHashCopyText || '';
      if (!text) return;
      try {
        if (navigator.clipboard && navigator.clipboard.writeText) await navigator.clipboard.writeText(text);
        showToast('SHA copied', 'success');
      } catch (e) {
        showToast('Could not copy SHA', 'error');
      }
    }

    // renderReleaseStatus paints the #7092 panel: the release channel this hive
    // follows and what happened on the last upgrade attempt. Three attempt
    // states are rendered with distinct colour + icon + wording so "never
    // attempted" can never be mistaken for "succeeded", and an unresolved
    // channel is shown honestly (with the image tag actually observed) instead
    // of a fabricated default.
    function renderReleaseStatus(rs) {
      var el = document.getElementById('release-status');
      if (!el) return;
      if (!rs) { el.hidden = true; el.innerHTML = ''; return; }
      el.hidden = false;

      // --- Channel (honest about "unknown") ---
      var ch = rs.channel || {};
      var chTone, chIcon, chLabel;
      if (ch.resolved && ch.channel) {
        chTone = 'info'; chIcon = '📡';
        chLabel = 'Channel <strong>' + escapeHtml(ch.channel) + '</strong>';
        if (ch.pendingChannel && ch.pendingChannel !== ch.channel) {
          chLabel += ' <span class="release-status-row--warn">(switch to <code>' + escapeHtml(ch.pendingChannel) + '</code> requested)</span>';
        }
      } else if (!ch.imageTag && !ch.imageRef && ch.branch) {
        // No readable image reference: name the built-from branch the version
        // menu shows rather than a bare "unknown" that contradicts it.
        chTone = 'info'; chIcon = '📡';
        chLabel = 'Tracking <strong>' + escapeHtml(ch.branch) + '</strong> <span class="text-muted">(build branch; image reference unavailable)</span>';
      } else {
        chTone = 'warn'; chIcon = '❔';
        chLabel = 'Channel <strong>unknown</strong>' +
          (ch.imageTag ? ' <span class="text-muted">(image tag <code>' + escapeHtml(ch.imageTag) + '</code> is not a release channel)</span>' :
          (ch.imageRef ? ' <span class="text-muted">(image <code>' + escapeHtml(ch.imageRef) + '</code> has no release-channel tag)</span>' : ' <span class="text-muted">(image reference unavailable)</span>'));
      }
      var channelHelp = ch.selectorDetail || 'Choose stable, candidate, or edge. The hub records your intent and applies it on rollout or heartbeat.';
      var channelHtml = '<div class="release-status-row release-status-row--' + chTone + '" title="' + escapeHtml(channelHelp) + '">' + chIcon + ' ' + chLabel + '</div>';
      if (ch.selectorEnabled) {
        channelHtml = '<div class="release-status-row release-status-row--selector release-status-row--' + chTone + '" title="' + escapeHtml(channelHelp) + '">' +
          chIcon + ' <label for="release-channel-select">Channel</label>' +
          '<select id="release-channel-select" class="release-channel-select" data-current="' + escapeHtml(ch.channel || '') + '" data-change-action="switchReleaseChannel" data-arg-types="t">' +
          ['stable','candidate','edge'].map(function(c) { return '<option value="' + c + '"' + (c === ch.channel ? ' selected' : '') + '>' + c + '</option>'; }).join('') +
          '</select>' +
          (ch.pendingChannel && ch.pendingChannel !== ch.channel ? '<span class="release-status-row--warn">switch to <code>' + escapeHtml(ch.pendingChannel) + '</code> pending</span>' : '') +
          '<span class="release-status-hint" id="release-channel-switch-msg" title="' + escapeHtml(channelHelp) + '">?</span>' +
          '</div>';
      } else if (ch.selectorDetail) {
        channelHtml += '<div class="release-status-row text-muted">Selection unavailable <span class="release-status-hint" title="' + escapeHtml(ch.selectorDetail) + '">?</span></div>';
      }

      // --- Last upgrade attempt (three distinct states) ---
      var at = rs.attempt || {};
      var aTone = 'muted', aIcon = 'ⓘ', aLabel = '';
      var hideAttemptRow = false;
      var supersededNote = '';
      var targetShort = at.target ? versionShortSHA(at.target) : '';
      switch (at.state) {
        case 'succeeded':
          if (at.superseded && at.runningCommit) {
            // The recorded upgrade landed but the hive has since been rolled to
            // another commit: headline the running commit (the one the version
            // menu shows) and age the RUNNING build, not the superseded
            // attempt. The superseded note moves to the secondary line with
            // its own timestamp so the two ages are never conflated.
            aTone = 'muted'; aIcon = '↷';
            aLabel = 'Running <strong>' + escapeHtml(at.runningCommit) + '</strong>' +
              (at.rolledExternally ? ' <span class="text-muted">(rolled externally)</span>' : '');
            supersededNote = targetShort ? 'Last spoke-side upgrade to ' + targetShort + ' superseded' : '';
            break;
          }
          aTone = 'success'; aIcon = '✅';
          aLabel = 'Last upgrade <strong>' + (targetShort ? escapeHtml(targetShort) : 'completed') + '</strong>';
          break;
        case 'failed':
          aTone = 'danger'; aIcon = '⚠';
          aLabel = 'Last upgrade <strong>FAILED</strong>';
          break;
        case 'in_progress':
          aTone = 'warn'; aIcon = '⟳';
          aLabel = 'Last upgrade <strong>in progress' + (targetShort ? ' → ' + escapeHtml(targetShort) : '') + '</strong>';
          break;
        case 'superseded':
          aTone = 'muted'; aIcon = '↷';
          aLabel = 'Last upgrade <strong>superseded' + (targetShort ? ' → ' + escapeHtml(targetShort) : '') + '</strong>';
          break;
        case 'never':
        default:
          // "never attempted" tells an operator nothing useful and is wrong
          // far more often than it is right: the marker only records upgrades
          // driven THROUGH the hub, so any hive rolled by other means (a
          // direct image bump, a redeploy, a spoke roll) reads as "never"
          // however many times it has actually moved. Rather than assert
          // something false, render nothing for this state.
          hideAttemptRow = true;
          break;
      }
      // Timestamp — relative at a glance, absolute in the tooltip. For a
      // superseded attempt the headline names the running build, so its age
      // must be the running build's (runningSince), never the superseded
      // attempt's completedAt — that is what made a build rolled hours ago
      // read "1d ago".
      var attemptWhen = at.completedAt || at.at || '';
      var when = attemptWhen;
      var whenTitle = '';
      if (supersededNote) {
        when = at.runningSince || '';
        if (when) whenTitle = 'running since ' + when;
      }
      var whenHtml = '';
      if (when) {
        whenHtml = ' · <span class="text-muted" title="' + escapeHtml(whenTitle || when) + '">' + escapeHtml(relativeAge(when)) + '</span>';
      }
      var detail = at.detail || '';
      if (targetShort && /hive is running/i.test(detail) && detail.toLowerCase().indexOf(targetShort.toLowerCase()) >= 0) detail = '';
      var detailHtml = detail ? '<div class="release-status-detail">' + escapeHtml(detail) + '</div>' : '';
      if (supersededNote) {
        // Secondary line: what the spoke upgrader last did, with its own age.
        // Once that attempt predates the running build it is plain history.
        var noteAge = attemptWhen ? relativeAge(attemptWhen) : '';
        var noteOld = attemptWhen && when && Date.parse(attemptWhen) < Date.parse(when);
        detailHtml = '<div class="release-status-detail' + (noteOld ? ' text-muted' : '') + '" title="' + escapeHtml(at.detail || '') + '">' +
          escapeHtml(supersededNote) + (noteAge ? ' · <span title="' + escapeHtml(attemptWhen) + '">' + escapeHtml(noteAge) + '</span>' : '') +
          '</div>';
      }

      // --- Staleness warning: never present stale data as current truth. ---
      var staleHtml = '';
      if (rs.hubReachable === false) {
        staleHtml = '<div class="release-status-detail release-status-row--warn">⚠ This view may be stale: the hub has not been reached recently' +
          (rs.lastHeartbeatAt ? ' (last heartbeat ' + escapeHtml(rs.lastHeartbeatAt) + ')' : '') + '.</div>';
      }

      el.innerHTML =
        channelHtml +
        (hideAttemptRow ? '' :
          '<div class="release-status-row release-status-row--' + aTone + '">' + aIcon + ' ' + aLabel + whenHtml + '</div>' +
          detailHtml) +
        staleHtml;
    }

    async function switchReleaseChannel(sel) {
      var channel = sel && sel.value;
      var msg = document.getElementById('release-channel-switch-msg');
      if (!channel) return;
      var previous = sel.getAttribute('data-current') || '';
      sel.disabled = true;
      if (msg) { msg.classList.remove('text-danger'); msg.classList.add('text-muted'); msg.textContent = 'Requesting switch to ' + channel + '…'; }
      try {
        var r = await fetch('/api/release-channel', {
          method: 'POST',
          headers: {'Content-Type': 'application/json'},
          body: JSON.stringify({channel: channel})
        });
        var d = await r.json().catch(function() { return {}; });
        if (!r.ok) throw new Error(d.error || 'release-channel switch failed');
        if (d.releaseStatus) renderReleaseStatus(d.releaseStatus);
        showToast('Release-channel switch to ' + channel + ' requested', 'success');
      } catch (e) {
        if (previous) sel.value = previous;
        if (msg) { msg.classList.remove('text-muted'); msg.classList.add('text-danger'); msg.textContent = e && e.message ? e.message : 'Release-channel switch failed'; }
        showToast(e && e.message ? e.message : 'Release-channel switch failed', 'error');
      } finally {
        sel.disabled = false;
      }
    }

    // relativeAge renders a coarse "3h ago" style age from an RFC3339 stamp,
    // used only as a readability bonus next to the absolute time.
    function relativeAge(ts) {
      var t = Date.parse(ts);
      if (isNaN(t)) return '';
      var s = Math.max(0, Math.floor((Date.now() - t) / 1000));
      if (s < 60) return s + 's ago';
      var m = Math.floor(s / 60);
      if (m < 60) return m + 'm ago';
      var h = Math.floor(m / 60);
      if (h < 24) return h + 'h ago';
      var d = Math.floor(h / 24);
      return d + 'd ago';
    }
    // fetchGitVersion() — deferred to _deferredInit

    // SSE connection
    // release-notes helpers begin
    var RELEASE_NOTES_CATEGORY_ORDER = ['Added', 'Changed', 'Fixed', 'Security', 'Deprecated'];
    var RELEASE_NOTES_STATE_KEY = 'hive-release-notes-v1';
    var RELEASE_NOTES_REPO_URL = 'https://github.com/hivecommons/hive';
    // Groups a section's categories in the canonical order; unknown categories follow.
    function releaseNotesGroupSection(sec) {
      var cats = (sec && sec.categories) || {};
      var byLower = {};
      Object.keys(cats).forEach(function(k) { byLower[k.toLowerCase()] = k; });
      var out = [];
      var used = {};
      RELEASE_NOTES_CATEGORY_ORDER.forEach(function(name) {
        var key = byLower[name.toLowerCase()];
        if (key && Array.isArray(cats[key]) && cats[key].length) { out.push({ name: name, bullets: cats[key] }); used[key] = true; }
      });
      Object.keys(cats).forEach(function(k) {
        if (!used[k] && Array.isArray(cats[k]) && cats[k].length) out.push({ name: k, bullets: cats[k] });
      });
      return out;
    }
    // Splits text into plain and #NNNN reference parts so callers can build DOM nodes without innerHTML.
    function releaseNotesRefParts(text) {
      var parts = [];
      var re = /#(\d{1,7})\b/g;
      var last = 0;
      var m;
      var str = String(text == null ? '' : text);
      while ((m = re.exec(str)) !== null) {
        if (m.index > last) parts.push({ text: str.slice(last, m.index) });
        parts.push({ text: m[0], ref: m[1] });
        last = m.index + m[0].length;
      }
      if (last < str.length) parts.push({ text: str.slice(last) });
      return parts;
    }
    function releaseNotesSameSha(a, b) {
      a = String(a || '').toLowerCase();
      b = String(b || '').toLowerCase();
      return !!a && !!b && (a.indexOf(b) === 0 || b.indexOf(a) === 0);
    }
    // Decides whether to show the post-upgrade banner. Never on first visit; once per SHA transition.
    function releaseNotesBannerDecision(state, currentSha) {
      var next = { lastSeenSha: String((state && state.lastSeenSha) || '') };
      var cur = String(currentSha || '');
      if (!cur) return { show: false, from: '', state: next };
      if (!next.lastSeenSha) { next.lastSeenSha = cur; return { show: false, from: '', state: next }; }
      if (releaseNotesSameSha(next.lastSeenSha, cur)) return { show: false, from: '', state: next };
      var from = next.lastSeenSha;
      next.lastSeenSha = cur;
      return { show: true, from: from, state: next };
    }
    // release-notes helpers end
    function releaseNotesLoadState() {
      try { return safeJsonParse(localStorage.getItem(RELEASE_NOTES_STATE_KEY) || '{}', {}) || {}; } catch (e) { return {}; }
    }
    function releaseNotesSaveState(state) {
      try { localStorage.setItem(RELEASE_NOTES_STATE_KEY, JSON.stringify(state)); } catch (e) { /* storage blocked — non-fatal */ }
    }
    function rnEl(tag, cls, text) {
      var el = document.createElement(tag);
      if (cls) el.className = cls;
      if (text != null) el.textContent = text;
      return el;
    }
    function releaseNotesBulletNode(text) {
      var li = rnEl('li');
      releaseNotesRefParts(String(text).replace(/^\s*-\s+/, '')).forEach(function(part) {
        if (part.ref) {
          var a = rnEl('a', '', part.text);
          a.href = RELEASE_NOTES_REPO_URL + '/issues/' + part.ref;
          a.target = '_blank';
          a.rel = 'noopener noreferrer';
          li.appendChild(a);
        } else {
          li.appendChild(document.createTextNode(part.text));
        }
      });
      return li;
    }
    function releaseNotesGroupsNode(sec, parent) {
      releaseNotesGroupSection(sec).forEach(function(g) {
        parent.appendChild(rnEl('div', 'release-notes-cat', g.name));
        var ul = rnEl('ul', 'release-notes-list');
        g.bullets.forEach(function(b) { ul.appendChild(releaseNotesBulletNode(b)); });
        parent.appendChild(ul);
      });
    }
    function releaseNotesRender(body, data) {
      body.textContent = '';
      if (!data || data.source === 'unavailable') {
        var reason = (data && data.error) || 'no reason given';
        body.appendChild(rnEl('div', 'muted-xs', 'Release notes unavailable (' + reason + ')'));
        return;
      }
      var sections = Array.isArray(data.sections) ? data.sections : [];
      sections.forEach(function(sec, i) {
        var det = rnEl('details', 'release-notes-release');
        if (i === 0) det.open = true;
        var label = (sec.version || sec.title || 'release') + (sec.date ? ' · ' + sec.date : '');
        det.appendChild(rnEl('summary', 'mono', label));
        var inner = rnEl('div', 'release-notes-release-body');
        releaseNotesGroupsNode(sec, inner);
        det.appendChild(inner);
        body.appendChild(det);
      });
      if (data.unreleased) {
        var also = rnEl('div', 'release-notes-also');
        also.appendChild(rnEl('div', 'hive-dialog-title', 'Also in this build'));
        releaseNotesGroupsNode(data.unreleased, also);
        body.appendChild(also);
      }
      if (!sections.length && !data.unreleased) body.appendChild(rnEl('div', 'muted-xs', 'No release notes between these revisions.'));
      if (data.truncated) body.appendChild(rnEl('div', 'muted-xs', 'Showing the most recent entries only.'));
    }
    function releaseNotesFetch(from, to) {
      if (!from || !to) return Promise.resolve({ source: 'unavailable', error: 'revision unknown', sections: [] });
      return fetch('/api/version/release-notes?from=' + encodeURIComponent(from) + '&to=' + encodeURIComponent(to))
        .then(function(r) { return r.ok ? r.json() : { source: 'unavailable', error: 'HTTP ' + r.status, sections: [] }; })
        .catch(function(e) { return { source: 'unavailable', error: (e && e.message) || 'request failed', sections: [] }; });
    }
    // Opens the release-notes modal. Resolves true when Upgrade is chosen; read-only mode has no Upgrade button.
    function releaseNotesOpenModal(opts) {
      return new Promise(function(resolve) {
        var opener = document.activeElement;
        var overlay = rnEl('div', 'hive-dialog-overlay');
        var dlg = rnEl('div', 'hive-dialog release-notes-dialog');
        dlg.setAttribute('role', 'dialog');
        dlg.setAttribute('aria-modal', 'true');
        dlg.setAttribute('aria-labelledby', 'release-notes-title');
        var title = rnEl('div', 'hive-dialog-title', opts.title);
        title.id = 'release-notes-title';
        dlg.appendChild(title);
        if (opts.caveat) dlg.appendChild(rnEl('div', 'muted-xs release-notes-caveat', opts.caveat));
        var body = rnEl('div', 'release-notes-body modal-scroll');
        body.setAttribute('aria-live', 'polite');
        body.appendChild(rnEl('div', 'muted-xs', 'Loading release notes…'));
        dlg.appendChild(body);
        var buttons = rnEl('div', 'hive-dialog-buttons');
        var cancel = rnEl('button', 'hv-btn btn-secondary hive-dialog-btn', opts.readOnly ? 'Close' : 'Cancel');
        cancel.type = 'button';
        buttons.appendChild(cancel);
        var ok = null;
        if (!opts.readOnly) {
          ok = rnEl('button', 'hv-btn btn-primary hive-dialog-btn', 'Upgrade');
          ok.type = 'button';
          buttons.appendChild(ok);
        }
        dlg.appendChild(buttons);
        overlay.appendChild(dlg);
        var done = false;
        var close = function(val) {
          if (done) return;
          done = true;
          document.removeEventListener('keydown', onKey, true);
          overlay.remove();
          var back = document.getElementById('spoke-upgrade-btn') || opener;
          if (back && back.focus) back.focus();
          resolve(val);
        };
        var onKey = function(e) {
          if (e.key === 'Escape') { e.preventDefault(); close(false); return; }
          if (e.key !== 'Tab') return;
          var nodes = Array.from(dlg.querySelectorAll('button, a[href], summary')).filter(function(el) { return !el.disabled && el.offsetParent !== null; });
          if (!nodes.length) return;
          var first = nodes[0];
          var last = nodes[nodes.length - 1];
          if (e.shiftKey && (document.activeElement === first || !dlg.contains(document.activeElement))) { e.preventDefault(); last.focus(); }
          else if (!e.shiftKey && (document.activeElement === last || !dlg.contains(document.activeElement))) { e.preventDefault(); first.focus(); }
        };
        overlay.addEventListener('click', function(e) { if (e.target === overlay) close(false); });
        cancel.addEventListener('click', function() { close(false); });
        if (ok) ok.addEventListener('click', function() { close(true); });
        document.addEventListener('keydown', onKey, true);
        hiveModalSheetDecorate(overlay, function() { close(false); });
        document.body.appendChild(overlay);
        (ok || cancel).focus();
        releaseNotesFetch(opts.from, opts.to).then(function(data) {
          if (done) return;
          if (opts.onLoaded) opts.onLoaded(data);
          releaseNotesRender(body, data);
        });
      });
    }
    function releaseNotesShowBanner(from, to) {
      var host = document.getElementById('release-notes-banner');
      if (!host) return;
      var fromShort = versionShortSHA(from);
      var toShort = versionShortSHA(to);
      host.textContent = '';
      host.className = 'dashboard-notice dashboard-notice--info release-notes-banner';
      var text = rnEl('div', 'release-notes-banner-text', 'Upgraded ' + fromShort + ' → ' + toShort);
      var open = rnEl('button', 'hv-btn btn-secondary btn-sm', 'What changed ▸');
      open.type = 'button';
      var dismiss = rnEl('button', 'hv-btn btn-secondary btn-sm', 'Dismiss');
      dismiss.type = 'button';
      dismiss.setAttribute('aria-label', 'Dismiss upgrade banner');
      host.appendChild(text);
      host.appendChild(open);
      host.appendChild(dismiss);
      dismiss.addEventListener('click', function() { host.className = 'hidden'; host.textContent = ''; });
      open.addEventListener('click', function() {
        releaseNotesOpenModal({ title: 'What changed: ' + fromShort + ' → ' + toShort, from: from, to: to, readOnly: true });
      });
      releaseNotesFetch(from, to).then(function(data) {
        if (data && data.source !== 'unavailable' && Array.isArray(data.sections)) {
          var n = data.sections.length;
          var summary = n + (n === 1 ? ' release.' : ' releases.');
          if (n === 0) {
            var cats = (data.unreleased && data.unreleased.categories) || {};
            var u = Object.keys(cats).reduce(function(sum, k) { return sum + (Array.isArray(cats[k]) ? cats[k].length : 0); }, 0);
            if (u > 0) summary = u + (u === 1 ? ' unreleased change.' : ' unreleased changes.');
          }
          text.textContent = 'Upgraded ' + fromShort + ' → ' + toShort + ': ' + summary;
        }
      });
    }
    // Owner/merger only, matching the release-notes endpoint; waits for the role to resolve.
    function releaseNotesBannerInit(currentSha) {
      if (!currentSha) return;
      var tries = 0;
      var attempt = function() {
        var role = window._hiveRole;
        if (!role) {
          if (++tries < 20) setTimeout(attempt, 1000);
          return;
        }
        var decision = releaseNotesBannerDecision(releaseNotesLoadState(), currentSha);
        releaseNotesSaveState(decision.state);
        if (decision.show && dashboardRoleAtLeast(role, 'merger')) releaseNotesShowBanner(decision.from, currentSha);
      };
      attempt();
    }
    var _upgradeInProgress = false;
    var _upgradeTargetHash = null;

    async function selfUpgrade(targetHash) {
      const target = window._lastVersionData && window._lastVersionData.target;
      let displayHash = targetHash || '';
      const isChannel = !!(target && target.ref && target.source === 'channel' && (targetHash === target.ref || targetHash === target.sha));
      const lv = window._lastVersionData || {};
      const runtimeLabel = (lv.deployment && lv.deployment.runtime) ? String(lv.deployment.runtime) : 'unknown runtime';
      const fromSha = lv.hash || lv.short || '';
      const notesTo = isChannel ? target.ref : (targetHash || (target && target.sha) || '');
      const targetLabel = isChannel ? target.ref : (versionShortSHA(targetHash) || 'latest');
      const hiveName = versionHiveDisplayName(lv);
      const runtimeCaveat = versionRuntimeUpgradeCaveat(runtimeLabel);
      const channelCaveat = isChannel ? 'Current channel revision ' + (target.short || 'unknown') + '. The channel may advance before the host pulls it.' : '';
      const proceed = await releaseNotesOpenModal({
        title: 'Upgrade ' + hiveName + ': ' + (versionShortSHA(fromSha) || 'unknown') + ' → ' + targetLabel,
        caveat: versionJoinCaveats(runtimeCaveat, channelCaveat),
        from: fromSha,
        to: notesTo
      });
      if (!proceed) return;
      if (isChannel) {
        targetHash = target.ref;
        displayHash = target.sha || '';
      }

      var btn = document.getElementById('spoke-upgrade-btn');
      const targetShort = versionShortSHA(displayHash) || 'latest';
      const currentHash = window._lastVersionData && (window._lastVersionData.hash || window._lastVersionData.short) || '';
      const progress = versionRecordUpgradeStart(displayHash || 'latest', targetShort, currentHash);
      if (btn) { btn.disabled = true; btn.setAttribute('aria-disabled', 'true'); btn.textContent = 'Upgrading…'; btn.style.opacity = '0.6'; }
      if (window._lastVersionData && window._lastVersionContext) {
        window._lastVersionContext.upgradeProgress = progress;
        window._lastVersionContext.hasUpgrade = false;
        renderVersionSurfaces(window._lastVersionData, window._lastVersionContext);
      }
      _upgradeInProgress = true;
      _upgradeTargetHash = targetHash || null;
      versionScheduleUpgradePoll(true);
      showToast('Upgrading to ' + targetShort + '...', 'info');
      return fetch('/api/self-upgrade', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ target: targetHash || '' })
      }).then(function(resp) {
        if (resp.ok) return resp;
        return resp.json().catch(function() { return {}; }).then(function(err) {
          showToast('Upgrade failed: ' + (err.error || resp.statusText), 'error');
          versionClearUpgradeProgress();
          fetchGitVersion();
          return null;
        });
      }).then(function(resp) {
        if (!resp) return null;
        return resp.json().catch(function() { return {}; });
      }).then(function(okBody) {
        if (!okBody) return;
        if (okBody.status === 'current') {
          /* The hub armed nothing: this spoke is already at its reachable
             target. Plain "up to date", never "failed" (#12494). */
          showToast(okBody.message || ('Already up to date at ' + (okBody.current || versionShortSHA(currentHash) || 'the current build')), 'info');
          versionClearUpgradeProgress();
          fetchGitVersion();
          return;
        }
        _upgradeInProgress = true;
        _upgradeTargetHash = displayHash || null;
        showToast('Upgrade started — waiting for rollout to complete', 'success');
        fetchGitVersion();
      }).catch(function(e) {
        showToast('Upgrade error: ' + e.message, 'error');
        versionClearUpgradeProgress();
        fetchGitVersion();
      });
    }

    var _initialVersionHash = null;
    var VERSION_CHECK_INTERVAL_MS = 60000;
    var _versionReloadPending = false;
    // Defer a version-triggered reload while the user is in a form field so
    // the reload doesn't discard what they are typing (#11032).
    function reloadForNewVersion() {
      if (isEditableElement(document.activeElement)) {
        if (!_versionReloadPending) {
          _versionReloadPending = true;
          document.addEventListener('focusout', function onFocusOut() {
            setTimeout(function() {
              if (isEditableElement(document.activeElement)) return;
              document.removeEventListener('focusout', onFocusOut);
              window.location.reload();
            }, 0);
          });
        }
        return;
      }
      window.location.reload();
    }
    function startVersionCheck() {
      fetch('/api/version').then(function(r) { return r.json(); }).then(function(d) {
        _initialVersionHash = d.hash || d.short || '';
        setDashboardDocsLine(d.branch);
        releaseNotesBannerInit(_initialVersionHash);
      }).catch(function() {});
      dashboardSetInterval('version-reload-check', function() {
        fetch('/api/version').then(function(r) { return r.json(); }).then(function(d) {
          var current = d.hash || d.short || '';
          if (_initialVersionHash && current && current !== _initialVersionHash) {
            console.log('[hive] version changed from', _initialVersionHash, 'to', current, '— reloading');
            reloadForNewVersion();
          }
        }).catch(function() {});
      }, VERSION_CHECK_INTERVAL_MS);
    }
    // startVersionCheck() — deferred to _deferredInit
