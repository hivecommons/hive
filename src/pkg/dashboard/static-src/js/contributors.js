    /* ---- Clickable user avatars ---------------------------------------
       Every face on this dashboard links to that person's GitHub profile, so
       the avatar itself is the affordance rather than a separate glyph or a
       second anchor on the username beside it.

       ALWAYS github.com. Contributors, leaderboard entries and allowlisted
       users are identified by the github.com account that authenticated, even
       on a hive whose ORG lives on a GitHub Enterprise host — repo links are
       built from a host field, profile links are not.

       encodeURIComponent for the URL path segment; esc() for the quoted
       attribute values (this file's esc() does escape " ' and backtick, unlike
       the hub's, so it is safe in attributes here). */
    function ghProfileURL(username) {
      return `https://github.com/${encodeURIComponent(String(username || ''))}`;
    }

    /* Wrap avatar markup in a profile anchor.

       display:inline-block with line-height:0 stops the anchor contributing a
       text baseline's worth of height to the table row or flex line that holds
       it; the avatars are 20-28px inline images that keep their own sizing.

       Returns the markup unwrapped when there is no username — an anchor to a
       profile that cannot exist is worse than no anchor at all. */
    function avatarProfileLink(username, label, avatarHTML) {
      const uname = String(username || '');
      if (!uname) return avatarHTML;
      const name = esc(label || uname);
      return `<a href="${esc(ghProfileURL(uname))}" target="_blank" rel="noopener noreferrer" ` +
        `title="${name}" aria-label="${name}" ` +
        `style="display:inline-block;line-height:0;text-decoration:none">${avatarHTML}</a>`;
    }

    /* Round avatar <img>, at the given rendered size in CSS pixels. src may be
       an explicit avatar_url from the API (GitHub's CDN URL, not derivable from
       the login) or, when that is absent, github.com/<login>.png. onerror hides
       a 404 rather than leaving a broken-image glyph — which would read as a
       dead control now that the image is inside a link. */
    function avatarImg(src, px, extraStyle) {
      return `<img src="${esc(src)}" alt="" width="${px}" height="${px}" ` +
        `style="width:${px}px;height:${px}px;border-radius:50%;vertical-align:middle;` +
        `${extraStyle || ''}" data-error-action="gh19">`;
    }

    /* The common case: a linked, round avatar for a github.com login. avatarURL
       is optional and wins when present. */
    function linkedAvatar(username, px, label, avatarURL, extraStyle) {
      const src = avatarURL || `${ghProfileURL(username)}.png`;
      return avatarProfileLink(username, label, avatarImg(src, px, extraStyle));
    }

    /* identityKeyProvider classifies a raw identity key by its prefix —
       "google:…" → google, "ibmid:…" → ibmid, "microsoft:…"/"ms:…" →
       microsoft, anything with no "provider:" prefix (a plain GitHub login,
       or legacy "github:<login>") → github. Mirrors the hub dashboard's
       identityProviderFromKey (pkg/hub/saas.go) so the two badges classify a
       given key identically; kept as a separate copy because this file and
       the hub dashboard are different bundles with no shared JS module. */
    function identityKeyProvider(key) {
      key = String(key || '');
      if (key.indexOf('github.com/') === 0 || key.indexOf('github:') === 0) return 'github';
      var m = key.match(/^([a-z][a-z0-9_-]*):/);
      if (!m) return 'github';
      return m[1] === 'ms' ? 'microsoft' : m[1];
    }

    /* accessRowAvatar renders the avatar for one authorized-users row. A
       GitHub identity keeps the existing linked github.com avatar + profile
       link. A non-GitHub identity (ibmid/google/microsoft) has no github.com
       profile to link to — linking one anyway 404s and points at someone
       else's account by coincidence of URL-shape — so it gets a plain,
       unlinked initials tile instead. displayName (may be '') seeds the
       initial; the raw key is the tooltip either way. */
    function accessRowAvatar(key, displayName, px) {
      var provider = identityKeyProvider(key);
      var title = String(key || '') + (displayName ? ' (' + displayName : '') + (displayName ? ')' : '');
      if (provider === 'github') return linkedAvatar(key, px, title, '', 'margin-right:6px');
      var initial = esc((displayName || key || '?').trim().charAt(0).toUpperCase() || '?');
      return '<span title="' + esc(title) + '" style="display:inline-flex;align-items:center;justify-content:center;' +
        'width:' + px + 'px;height:' + px + 'px;border-radius:50%;margin-right:var(--sp-3);flex:none;' +
        'background:var(--line);color:var(--muted);font-size:' + Math.max(10, Math.round(px * 0.5)) + 'px;font-weight:600">' +
        initial + '</span>';
    }

    /* Rendered avatar sizes, in CSS pixels. The contributor cards and the
       leaderboard rows share a size because they are the same kind of roster;
       the Security tab's Access subsection allowlist is a denser config list. */
    const CONTRIBUTOR_AVATAR_PX = 28;
    const ACCESS_LIST_AVATAR_PX = 20;

    // Active contributor filter set — empty means "all"
    let _contributorFilters = new Set();
    // Cache the last fetched contributor list for re-rendering on filter change
    let _cachedContributors = [];

    const CONTRIBUTOR_FILTER_ALL = 'all';
    const CONTRIBUTOR_FILTER_ACTIVE = 'active';
    const CONTRIBUTOR_FILTER_TIERS = ['newcomer', 'contributor', 'trusted', 'merger', 'advisor', 'revoked'];

    function renderContributorFilters(contributors) {
      const bar = document.getElementById('contributors-filters');
      if (!bar) return;
      // Count per category
      const activeCount = (contributors || []).filter(c => c.active).length;
      const tierCounts = {};
      for (const t of CONTRIBUTOR_FILTER_TIERS) tierCounts[t] = 0;
      for (const c of (contributors || [])) {
        if (tierCounts.hasOwnProperty(c.trust_tier)) tierCounts[c.trust_tier]++;
      }
      const totalCount = (contributors || []).length;

      const isAllActive = _contributorFilters.size === 0;

      function pill(key, label, count) {
        const active = key === CONTRIBUTOR_FILTER_ALL ? isAllActive : _contributorFilters.has(key);
        const bg = active ? 'var(--line)' : 'transparent';
        const fg = active ? 'var(--text)' : 'var(--muted)';
        const border = 'var(--line)';
        return `<button class="hv-btn btn-primary btn-sm" data-action="toggleContributorFilter" data-arg0="${key}" style="display:inline-flex;align-items:center;gap:var(--sp-2);padding:var(--sp-2) var(--sp-5);border-radius:var(--r-pill);font-size:0.72rem;font-weight:600;cursor:pointer;border:1px solid ${border};background:${bg};color:${fg};transition:all 0.15s">${esc(label)} <span style="opacity:0.8">${count}</span></button>`;
      }

      let html = pill(CONTRIBUTOR_FILTER_ALL, 'All', totalCount);
      html += pill(CONTRIBUTOR_FILTER_ACTIVE, 'Active', activeCount);
      for (const t of CONTRIBUTOR_FILTER_TIERS) {
        html += pill(t, t.charAt(0).toUpperCase() + t.slice(1), tierCounts[t]);
      }
      setIfChanged(bar, html);
    }

    // Exposed globally for onclick
    window.toggleContributorFilter = function(key) {
      if (key === CONTRIBUTOR_FILTER_ALL) {
        _contributorFilters.clear();
      } else {
        if (_contributorFilters.has(key)) {
          _contributorFilters.delete(key);
        } else {
          _contributorFilters.add(key);
        }
      }
      renderContributorFilters(_cachedContributors);
      renderContributors(_cachedContributors);
    };

    function matchesContributorFilter(c) {
      // "All" selected — include everyone (revoked will be split out in renderContributors)
      if (_contributorFilters.size === 0) return true;
      for (const f of _contributorFilters) {
        if (f === CONTRIBUTOR_FILTER_ACTIVE && c.active) return true;
        if (f === c.trust_tier) return true;
      }
      return false;
    }

    async function fetchContributors() {
      try {
        const res = await fetch('/api/contributors');
        if (!res.ok) return;
        const data = await res.json();
        _cachedContributors = data.contributors || [];
        renderContributorFilters(_cachedContributors);
        renderContributors(_cachedContributors);
      } catch (e) { /* silently retry on next poll */ }
    }

    const CONTRIB_TINT_CACHE_TTL_MS = 7 * 24 * 60 * 60 * 1000;
    const _contribTintMemory = new Map();

    function contribTintFallback(login) {
      const text = String(login || 'contributor');
      let hash = 0;
      for (let i = 0; i < text.length; i++) hash = ((hash << 5) - hash + text.charCodeAt(i)) | 0;
      const hue = Math.abs(hash) % 360;
      const hue2 = (hue + 38 + (Math.abs(hash >> 8) % 74)) % 360;
      return { accent: hslToHex(hue, 62, 58), accent2: hslToHex(hue2, 54, 48) };
    }

    function hslToHex(h, s, l) {
      s /= 100; l /= 100;
      const k = n => (n + h / 30) % 12;
      const a = s * Math.min(l, 1 - l);
      const f = n => l - a * Math.max(-1, Math.min(k(n) - 3, Math.min(9 - k(n), 1)));
      return '#' + [f(0), f(8), f(4)].map(v => Math.round(255 * v).toString(16).padStart(2, '0')).join('');
    }

    function rgbToHex(r, g, b) {
      return '#' + [r, g, b].map(v => {
        return Math.max(0, Math.min(255, Math.round(v))).toString(16).padStart(2, '0');
      }).join('');
    }

    function rgbSaturation(r, g, b) {
      const max = Math.max(r, g, b);
      const min = Math.min(r, g, b);
      return max === 0 ? 0 : (max - min) / max;
    }

    function contribTintFromPixels(pixels) {
      const buckets = new Map();
      for (let i = 0; i < pixels.length; i += 4) {
        const a = pixels[i + 3] == null ? 255 : pixels[i + 3];
        if (a < 180) continue;
        const r = pixels[i], g = pixels[i + 1], b = pixels[i + 2];
        const lum = 0.2126 * r + 0.7152 * g + 0.0722 * b;
        const sat = rgbSaturation(r, g, b);
        if (lum < 28 || lum > 236 || sat < 0.18) continue;
        const key = [r >> 4, g >> 4, b >> 4].join(',');
        const prev = buckets.get(key) || { count: 0, r: 0, g: 0, b: 0, score: 0 };
        prev.count++;
        prev.r += r; prev.g += g; prev.b += b;
        prev.score += 1 + sat * 2 + (lum > 48 && lum < 210 ? 0.5 : 0);
        buckets.set(key, prev);
      }
      const ranked = Array.from(buckets.values()).map(v => ({ count: v.count, r: v.r / v.count, g: v.g / v.count, b: v.b / v.count, score: v.score })).sort((a, b) => b.score - a.score);
      if (!ranked.length) return null;
      const primary = ranked[0];
      let secondary = ranked.find(c => Math.abs(c.r - primary.r) + Math.abs(c.g - primary.g) + Math.abs(c.b - primary.b) > 96) || ranked[1] || primary;
      return { accent: rgbToHex(primary.r, primary.g, primary.b), accent2: rgbToHex(secondary.r, secondary.g, secondary.b) };
    }

    function contribTintCacheKey(login) { return 'hive-contrib-tint:' + String(login || '').toLowerCase(); }

    function contribTintReadCache(login) {
      const key = contribTintCacheKey(login);
      if (_contribTintMemory.has(key)) return _contribTintMemory.get(key);
      let raw = null;
      try { raw = localStorage.getItem(key); } catch { return null; }
      const parsed = safeJsonParse(raw || '{}', {});
      if (!parsed || !parsed.accent || !parsed.accent2 || Date.now() - (parsed.ts || 0) > CONTRIB_TINT_CACHE_TTL_MS) return null;
      const tint = { accent: parsed.accent, accent2: parsed.accent2 };
      _contribTintMemory.set(key, tint);
      return tint;
    }

    function contribTintWriteCache(login, tint) {
      if (!tint) return;
      const key = contribTintCacheKey(login);
      _contribTintMemory.set(key, tint);
      try { localStorage.setItem(key, JSON.stringify({ accent: tint.accent, accent2: tint.accent2, ts: Date.now() })); } catch {}
    }

    function applyContributorTint(card, tint) {
      if (!card || !tint) return;
      card.style.setProperty('--contrib-accent', tint.accent);
      card.style.setProperty('--contrib-accent-2', tint.accent2);
      card.dataset.contribTintReady = '1';
    }

    function tintContributorCards() {
      document.querySelectorAll('.contrib-card[data-contrib-login]').forEach(card => {
        const login = card.dataset.contribLogin || '';
        const avatar = card.dataset.contribAvatar || '';
        const cached = contribTintReadCache(login);
        if (cached) { applyContributorTint(card, cached); return; }
        const fallback = contribTintFallback(login);
        applyContributorTint(card, fallback);
        if (!avatar) { contribTintWriteCache(login, fallback); return; }
        const img = new Image();
        img.crossOrigin = 'anonymous';
        img.onload = () => {
          try {
            const canvas = document.createElement('canvas');
            canvas.width = 16; canvas.height = 16;
            const ctx = canvas.getContext('2d', { willReadFrequently: true });
            if (!ctx) throw new Error('canvas unavailable');
            ctx.drawImage(img, 0, 0, 16, 16);
            const tint = contribTintFromPixels(ctx.getImageData(0, 0, 16, 16).data) || fallback;
            contribTintWriteCache(login, tint);
            applyContributorTint(card, tint);
          } catch { contribTintWriteCache(login, fallback); }
        };
        img.onerror = () => contribTintWriteCache(login, fallback);
        img.src = avatar;
      });
    }

    function renderContributors(contributors) {
      refreshSectionCardShell('contributors-section');
      setSectionSummary('contributors-section', contributorsSectionSummary(contributors, null));
      const panel = document.getElementById('contributors-panel');
      if (!panel) return;
      if (!contributors.length) {
        const contributeUrl = window.location.origin + '/contribute';
        setIfChanged(panel,
          '<div style="color:var(--text);padding:var(--sp-8);background:var(--card-bg);border:1px solid var(--border);border-radius:var(--radius-lg);max-width:760px">' +
            '<div style="font-weight:600;font-size:var(--fs-md);margin-bottom:var(--sp-4)">\ud83e\udd1d Grow your hive with ClankeR</div>' +
            '<div style="color:var(--muted);font-size:var(--fs-base);line-height:1.6;margin-bottom:var(--sp-5)">' +
              'ClankeR \u2014 the contributor relay \u2014 lets anyone lend their own machine and AI subscription to this hive. ' +
              'Contributors register with their GitHub account, run a lightweight worker locally, and the hive ' +
              'relays tasks (issue fixes, reviews, docs) to their agents over a secure WebSocket \u2014 their compute, your backlog. ' +
              'Completed tasks build trust: contributors are promoted automatically from <strong>Newcomer</strong> to <strong>Contributor</strong>, then maintainers can grant <strong>Trusted</strong>, <strong>Merger</strong>, and <strong>Advisor</strong> tiers.' +
            '</div>' +
            '<div style="color:var(--muted);font-size:var(--fs-base);line-height:1.6;margin-bottom:var(--sp-5)">' +
              '<strong class="text-default">Invite people in three steps:</strong><br>' +
              '1. Share your contribute link: <a href="/contribute" target="_blank" style="color:var(--blue);font-weight:600">' + escapeHtml(contributeUrl) + '</a><br>' +
              '2. They sign in with GitHub and follow the one-command setup shown on that page.<br>' +
              '3. Their agents appear here and start picking up tasks \u2014 you approve what merges.' +
            '</div>' +
            '<div style="display:flex;gap:10px;justify-content:flex-start;flex-wrap:wrap">' +
              '<a href="/contribute" target="_blank" style="font-size:var(--fs-sm);padding:7px 16px;background:var(--amber);color:var(--bg);border-radius:var(--radius);text-decoration:none;font-weight:600">Open /contribute page \u2192</a>' +
              '<button class="hv-btn btn-primary" data-action="ghCopyInviteLink" data-arg0="' + esc(contributeUrl) + '">\ud83d\udccb Copy invite link</button>' +
            '</div>' +
            '<div style="color:var(--muted);font-size:var(--fs-sm);margin-top:10px">Tip: post the link in your project README, Discord, or a pinned GitHub issue to recruit contributors.</div>' +
          '</div>');
        return;
      }
      const filtered = contributors.filter(matchesContributorFilter);
      if (!filtered.length) {
        setIfChanged(panel, '<div style="color:var(--muted);padding:var(--sp-8);text-align:center;background:var(--card-bg);border:1px solid var(--border);border-radius:8px">No contributors match the selected filters.</div>');
        return;
      }

      // Split into active (non-revoked) and revoked lists
      const showingRevokedFilter = _contributorFilters.has('revoked');
      const nonRevoked = contributorsOnlineFirst(filtered.filter(c => c.trust_tier !== 'revoked'));
      const revoked = filtered.filter(c => c.trust_tier === 'revoked');

      // --- Card grid: only non-revoked contributors ---
      let html = '';
      if (nonRevoked.length > 0) {
        const cards = nonRevoked.map(c => {
          const dotClass = c.online ? 'online' : 'offline';
          const dotTitle = c.online ? 'Online — heartbeat within the presence window' : 'Offline — no heartbeat within the presence window';
          // The face is the profile link. The @username beside it used to be a
          // second anchor to that same profile; it stays as plain text so there
          // is one control per destination.
          const avatarHtml = linkedAvatar(c.github_username, CONTRIBUTOR_AVATAR_PX,
            `@${c.github_username}`, c.avatar_url, 'flex-shrink:0');
          const cliLine = c.cli_backend ? `${esc(c.cli_backend)}${c.model ? ' · ' + esc(c.model) : ''}` : '';
          const roleLine = c.preferred_role ? `role: ${esc(c.preferred_role)}` : '';
          const tierOptions = Object.keys(TRUST_TIER_COLORS).filter(t => t !== 'revoked').map(t =>
            `<option value="${t}"${t === c.trust_tier ? ' selected' : ''}>${t}</option>`
          ).join('');
          const trustedEligibleBadge = c.eligible_for_trusted
            ? '<span title="20+ PR tasks; use the tier dropdown to grant trusted" style="display:inline-block;padding:var(--sp-1) var(--sp-4);border-radius:var(--r-pill);font-size:var(--fs-xs);color:var(--yellow);border:1px solid color-mix(in srgb, var(--yellow) 45%, transparent);background:color-mix(in srgb, var(--yellow) 10%, transparent);flex-shrink:0">eligible for trusted</span>'
            : '';
          const login = c.github_username || '';
          const tierProv = tierProvenance(c);
          const initialTint = contribTintReadCache(login) || contribTintFallback(login);
          return `<div class="contrib-card" data-contrib-login="${esc(login)}" data-contrib-avatar="${esc(c.avatar_url || '')}" style="--contrib-accent:${esc(initialTint.accent)};--contrib-accent-2:${esc(initialTint.accent2)}">
            <div style="display:flex;align-items:center;gap:var(--sp-4);margin-bottom:var(--sp-4);flex-wrap:wrap">
              <span class="contrib-status-dot ${dotClass}" title="${dotTitle}"></span>
              ${avatarHtml}
              <span style="font-weight:600">@${esc(c.github_username)}</span>
              <span class="contrib-role-pill" style="flex-shrink:0"${tierProv ? ` title="${esc(tierProv)}"` : ''}>${trustTierBadge(c.trust_tier)}</span>
              ${trustedEligibleBadge}
            </div>
            ${tierProv ? `<div class="fs-sm text-muted mb-3 contrib-tier-provenance">${esc(tierProv)}</div>` : ''}
            ${cliLine ? `<div class="fs-sm text-muted mb-3">${cliLine}${roleLine ? ' · ' + roleLine : ''}</div>` : (roleLine ? `<div class="fs-sm text-muted mb-3">${roleLine}</div>` : '')}
            <div style="display:flex;gap:var(--sp-6);margin-bottom:var(--sp-3);font-size:0.8rem;color:var(--muted)">
              <span>Completed: <strong class="text-default">${c.total_tasks_completed || 0}</strong></span>
              <span>Failed: <strong style="color:${(c.total_tasks_failed || 0) > 0 ? 'var(--red)' : 'var(--fg)'}">${c.total_tasks_failed || 0}</strong></span>
            </div>
            ${(c.active_tasks && c.active_tasks.length > 0) ? '<div style="font-size:0.65rem;color:var(--muted);margin-bottom:var(--sp-1);font-weight:600">Current tasks</div>' + c.active_tasks.map(t => `<div style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:var(--sp-2);padding:6px 10px;background:rgba(56,139,253,0.08);border:1px solid rgba(56,139,253,0.2);border-radius:var(--r-sm);word-wrap:break-word;overflow-wrap:break-word"><span class="text-info">${esc(t.kind)}</span> <a href="https://github.com/${esc(t.repo)}/issues/${t.number}" target="_blank" rel="noopener" style="color:var(--blue);text-decoration:underline;font-weight:600">${esc(t.repo)}#${t.number} ↗</a> — ${esc(t.title)}</div>`).join('') : (c.current_task ? `<div style="font-size:0.65rem;color:var(--muted);margin-bottom:var(--sp-1);font-weight:600">Current task</div><div style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:10px;padding:8px 10px;background:rgba(56,139,253,0.08);border:1px solid rgba(56,139,253,0.2);border-radius:var(--r-sm);word-wrap:break-word;overflow-wrap:break-word"><span class="text-info">${esc(c.current_task.kind)}</span> <a href="https://github.com/${esc(c.current_task.repo)}/issues/${c.current_task.number}" target="_blank" rel="noopener" style="color:var(--blue);text-decoration:underline;font-weight:600">${esc(c.current_task.repo)}#${c.current_task.number} ↗</a> — ${esc(c.current_task.title)}</div>` : (c.last_completed_task ? `<div style="font-size:0.65rem;color:var(--muted);margin-bottom:var(--sp-1);font-weight:600">Last completed</div><div style="font-size:var(--fs-sm);color:var(--muted);margin-bottom:10px;padding:6px 10px;background:rgba(34,197,94,0.08);border:1px solid rgba(34,197,94,0.2);border-radius:var(--r-sm);word-wrap:break-word;overflow-wrap:break-word"><span class="text-success">${esc(c.last_completed_task.kind)}</span> <a href="https://github.com/${esc(c.last_completed_task.repo)}/issues/${c.last_completed_task.number}" target="_blank" rel="noopener" style="color:var(--green);text-decoration:underline">${esc(c.last_completed_task.repo)}#${c.last_completed_task.number} ↗</a> — ${esc(c.last_completed_task.title)}</div>` : (c.active ? '<div style="font-size:var(--fs-sm);color:var(--green);margin-bottom:10px">Idle — waiting for task</div>' : '')))}
            ${c.sessions > 1 ? `<div style="font-size:0.7rem;color:var(--muted);margin-bottom:var(--sp-3)">${c.sessions} active sessions</div>` : ''}
            <div style="display:flex;gap:var(--sp-4);align-items:center;font-size:var(--fs-sm)">
              <select data-change-action="changeContributorTier" data-arg0="${esc(c.contributor_id)}" data-arg-types="s,v" style="background:var(--card-bg);color:var(--fg);border:1px solid var(--border);border-radius:var(--r-sm);padding:var(--sp-1) var(--sp-3);font-size:var(--fs-sm)">${tierOptions}</select>
              <button class="hv-btn btn-danger btn-sm" data-action="revokeContributor" data-arg0="${esc(c.contributor_id)}">Revoke</button>
            </div>
          </div>`;
        }).join('');
        // min(500px,100%) so the track can collapse below 500px. Plain
        // auto-fill,minmax(500px,1fr) never goes under its floor, which forced a
        // 500px column into a ~355px phone viewport — clipped, not scrollable,
        // because body has overflow-x:hidden.
        html += `<div class="contributors-grid">${cards}</div>`;
      } else if (!revoked.length) {
        html += '<div style="color:var(--muted);padding:var(--sp-8);text-align:center;background:var(--card-bg);border:1px solid var(--border);border-radius:8px">No contributors match the selected filters.</div>';
      }

      // --- Revoked section: collapsible, shown when revoked contributors exist ---
      if (revoked.length > 0) {
        const revokedRows = revoked.map(c => {
          const ghLink = `https://github.com/${esc(c.github_username)}`;
          const revokedDate = c.last_heartbeat || c.registered_at || '';
          return `<tr style="border-bottom:1px solid var(--border)">
            <td style="padding:var(--sp-3) var(--sp-5)"><a href="${ghLink}" target="_blank" rel="noopener" style="color:var(--muted);text-decoration:none">@${esc(c.github_username)}</a></td>
            <td style="padding:var(--sp-3) var(--sp-5);color:var(--muted);font-size:var(--fs-sm)">${esc(revokedDate)}</td>
            <td style="padding:var(--sp-3) var(--sp-5);text-align:right">
              <button class="hv-btn btn-primary btn-sm" data-action="changeContributorTier" data-arg0="${esc(c.contributor_id)}" data-arg1="newcomer">Unrevoke</button>
              <button class="hv-btn btn-danger btn-sm" data-action="deleteContributor" data-arg0="${esc(c.contributor_id)}">Remove</button>
            </td>
          </tr>`;
        }).join('');

        // Expand by default when the Revoked filter pill is active
        const openAttr = showingRevokedFilter ? ' open' : '';
        html += `<details class="mt-6"${openAttr}>
          <summary style="cursor:pointer;color:var(--muted);font-size:0.8rem;font-weight:600;padding:var(--sp-4) var(--sp-0);user-select:none">Revoked (${revoked.length})</summary>
          <div style="background:var(--card-bg);border:1px solid var(--border);border-radius:8px;overflow:hidden;margin-top:var(--sp-4)">
            <table style="width:100%;border-collapse:collapse;font-size:0.78rem">
              <thead><tr style="background:var(--bg);border-bottom:1px solid var(--border)">
                <th style="padding:var(--sp-3) var(--sp-5);text-align:left;color:var(--muted);font-weight:600">User</th>
                <th style="padding:var(--sp-3) var(--sp-5);text-align:left;color:var(--muted);font-weight:600">Date</th>
                <th style="padding:var(--sp-3) var(--sp-5);text-align:right;color:var(--muted);font-weight:600">Actions</th>
              </tr></thead>
              <tbody>${revokedRows}</tbody>
            </table>
          </div>
        </details>`;
      }

      setIfChanged(panel, html);
      tintContributorCards();
    }

    async function deleteContributor(contributorId) {
      const ok = await hiveConfirm('Permanently remove this contributor? This cannot be undone.');
      if (!ok) return;
      try {
        const res = await fetch('/api/contributors/' + contributorId, { method: 'DELETE' });
        if (!res.ok) { await hiveAlert('Failed to remove: ' + res.statusText); return; }
        fetchContributors();
        fetchLeaderboard();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function fetchLeaderboard() {
      try {
        const res = await fetch('/api/leaderboard');
        if (!res.ok) return;
        const data = await res.json();
        renderLeaderboard(data.leaderboard || []);
      } catch (e) { /* silently retry on next poll */ }
    }

    function renderLeaderboard(entries) {
      const panel = document.getElementById('leaderboard-panel');
      if (!panel) return;
      if (!entries.length) {
        setIfChanged(panel, '<div style="color:var(--muted);padding:var(--sp-8);text-align:center;background:var(--card-bg);border:1px solid var(--border);border-radius:8px">No leaderboard data yet.</div>');
        return;
      }
      const rows = entries.map(e => {
        /* The face links to the profile; the @username beside it is plain text
           now that the avatar carries the same destination. An entry with no
           avatar_url still falls back to github.com/<login>.png, so the grey
           circle is only for entries with no username at all — and that one is
           deliberately NOT wrapped in an anchor, since there is no profile to
           point at. */
        const avatarHtml = e.github_username
          ? linkedAvatar(e.github_username, CONTRIBUTOR_AVATAR_PX, `@${e.github_username}`, e.avatar_url)
          : `<span style="display:inline-block;width:${CONTRIBUTOR_AVATAR_PX}px;height:${CONTRIBUTOR_AVATAR_PX}px;border-radius:50%;background:var(--border)"></span>`;
        return `<tr style="border-bottom:1px solid var(--border)">
          <td style="padding:var(--sp-4) var(--sp-5);font-weight:600;color:var(--muted)">${e.rank}</td>
          <td style="padding:var(--sp-4) var(--sp-5)">${avatarHtml}</td>
          <td style="padding:var(--sp-4) var(--sp-5)">@${esc(e.github_username)}</td>
          <td style="padding:var(--sp-4) var(--sp-5)">${trustTierBadge(e.trust_tier)}</td>
          <td style="padding:var(--sp-4) var(--sp-5);text-align:right">${e.tasks_completed || 0}</td>
          <td style="padding:var(--sp-4) var(--sp-5);text-align:right;color:${(e.tasks_failed || 0) > 0 ? 'var(--red)' : 'var(--muted)'}">${e.tasks_failed || 0}</td>
        </tr>`;
      }).join('');
      setIfChanged(panel, `<div style="background:var(--card-bg);border:1px solid var(--border);border-radius:8px;overflow:hidden">
        <table style="width:100%;border-collapse:collapse;font-size:0.8rem">
          <thead><tr style="background:var(--bg);border-bottom:1px solid var(--border)">
            <th style="padding:var(--sp-4) var(--sp-5);text-align:left;color:var(--muted);font-weight:600">#</th>
            <th style="padding:var(--sp-4) var(--sp-5)"></th>
            <th style="padding:var(--sp-4) var(--sp-5);text-align:left;color:var(--muted);font-weight:600">User</th>
            <th style="padding:var(--sp-4) var(--sp-5);text-align:left;color:var(--muted);font-weight:600">Tier</th>
            <th style="padding:var(--sp-4) var(--sp-5);text-align:right;color:var(--muted);font-weight:600">Completed</th>
            <th style="padding:var(--sp-4) var(--sp-5);text-align:right;color:var(--muted);font-weight:600">Failed</th>
          </tr></thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`);
    }

    async function changeContributorTier(contributorId, tier) {
      try {
        const res = await fetch(`/api/contributors/${contributorId}/trust`, {
          method: 'PUT',
          headers: _inceptionAuthHeaders(),
          body: JSON.stringify({ tier: tier, reason: 'operator set tier from the Contributors view' }),
        });
        if (!res.ok) { const err = await res.json().catch(() => ({})); await hiveAlert('Failed to change tier: ' + (err.error || res.statusText)); return; }
        fetchContributors();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    async function revokeContributor(contributorId) {
      const ok = await hiveConfirm('Revoke this contributor? They will lose access.');
      if (!ok) return;
      try {
        const res = await fetch(`/api/contributors/${contributorId}/revoke`, { method: 'POST' });
        if (!res.ok) { await hiveAlert('Failed to revoke: ' + res.statusText); return; }
        fetchContributors();
        fetchLeaderboard();
      } catch (e) { await hiveAlert('Error: ' + e.message); }
    }

    function updateContributorBadge(pool) {
      refreshSectionCardShell('contributors-section');
      const sidebarBadge = document.getElementById('contributors-sidebar-badge');
      const statusBadge = document.getElementById('contributors-status-badge');
      if (!pool || !Number.isSafeInteger(pool.online) || pool.online < 0 ||
          !Number.isSafeInteger(pool.registered) || pool.registered < 0) {
        navCountDelta(sidebarBadge, null);
        return;
      }
      const onlineCount = pool.online;
      const totalCount = pool.registered;
      const tip = contributorsOnlineTitle(onlineCount, totalCount, pool.presence_window);
      // Same two-part pill as Governor / Overview: "online/total".
      if (sidebarBadge) setSidebarPairBadge(sidebarBadge, onlineCount, totalCount, tip);
      navCountDelta(sidebarBadge, onlineCount, 'Contributors: ' + tip);
      setSectionSummary('contributors-section', contributorsSectionSummary(_cachedContributors || [], pool));
      if (statusBadge) {
        statusBadge.textContent = `${onlineCount} online / ${totalCount} registered`;
        statusBadge.title = tip;
        statusBadge.classList.toggle('is-online', onlineCount > 0);
      }
    }

    // contributorsOnlineTitle is the pill tooltip: "3 online · 12 registered
    // (heartbeat within 15m)". The window is the hub's presence_window, the
    // same one the tracker's "(offline 13m)" caption is judged against.
    function contributorsOnlineTitle(online, total, window) {
      const o = Number(online) || 0;
      const t = Number(total) || 0;
      const w = window ? ` (heartbeat within ${window})` : '';
      return `${o} online · ${t} registered${w}`;
    }

    // contributorsOnlineFirst orders a contributor list online first, then by
    // login, so the people who can take work now sit at the top. Pure; does
    // not mutate its input.
    function contributorsOnlineFirst(list) {
      return (Array.isArray(list) ? list.slice() : []).sort((a, b) => {
        const ao = a && a.online ? 1 : 0;
        const bo = b && b.online ? 1 : 0;
        if (ao !== bo) return bo - ao;
        return String((a && a.github_username) || '').localeCompare(String((b && b.github_username) || ''));
      });
    }

    // fetchContributors() + fetchLeaderboard() — deferred to _deferredInit
