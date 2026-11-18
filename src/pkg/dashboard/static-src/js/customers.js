    // ── Customers: a profile per person (hivecommons/hive#18742) ──
    // GET /api/customers returns the viewer's own profile ("me") and, for
    // the owner and read-write users, every customer and every suggested
    // person. A read-only user sees only "me" and edits it through
    // POST /api/customers/me/edit. Nothing shown here leaves the hive.
    const CUSTOMERS_POLL_MS = 60000;
    const CUSTOMERS_KIND_LABELS = {
      hive_user: 'Hive user', github: 'GitHub', email: 'Email', git_email: 'Git email', discord: 'Discord',
      slack: 'Slack', teams: 'Teams', meet: 'Google Meet', zoom: 'Zoom', name: 'Name'
    };
    // hive_user is what "me" resolves on, so it is never added by hand.
    const CUSTOMERS_ADDABLE_KINDS = ['github', 'email', 'git_email', 'discord', 'slack', 'teams', 'meet', 'zoom', 'name'];
    const CUSTOMERS_LEVEL_LABELS = { unknown: 'Unknown', at_risk: 'At risk', neutral: 'Neutral', satisfied: 'Satisfied' };
    // Satisfaction level to the kc-pill status palette.
    const CUSTOMERS_LEVEL_PILL = { at_risk: 'error', neutral: 'syncing', satisfied: 'ok' };
    const CUSTOMERS_ORIGIN_LABELS = { hive_user: 'hive user', manual: 'added by hand', accepted: 'accepted suggestion' };
    // The short origin pill on a row; the long wording stays in its tooltip.
    const CUSTOMERS_ORIGIN_SHORT = { hive_user: 'hive user', manual: 'added', accepted: 'accepted' };
    // A row shows this many identity pills and tag pills, then "+N".
    const CUSTOMERS_SOURCE_PILLS_SHOWN = 2;
    const CUSTOMERS_TAG_PILLS_SHOWN = 3;
    // A GitHub login: letters, digits and single hyphens, at most 39 long.
    const CUSTOMERS_GITHUB_LOGIN = /^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$/;
    // The avatar box is 28 CSS pixels (see .customers-avatar); the picture is
    // asked for at twice that so it stays sharp on a dense screen.
    const CUSTOMERS_AVATAR_PX = 28;
    const CUSTOMERS_AVATAR_FETCH_PX = 56;
    const CUSTOMERS_EVIDENCE_SHOWN = 5;
    let _customersData = null;
    let _customersError = '';
    // The profile open under its row, or null when every row is collapsed. It
    // survives a refresh: the list is drawn again around it.
    let _customersOpen = null;
    // The open profile's timeline, read from the customer graph
    // (POST /api/graph) a page at a time.
    const CUSTOMERS_TIMELINE_PAGE = 25;
    const CUSTOMERS_TIMELINE_QUERY = 'query Timeline($id: ID!, $first: Int, $after: String) { person(id: $id) { edges(first: $first, after: $after) { nodes { predicate at source artefact { ref kind } } next } } }';
    const CUSTOMERS_REF_KIND_LABELS = { gh: 'GitHub', meeting: 'Meetings', event: 'Calendar', inbox: 'Inbox', fact: 'Knowledge', bead: 'Work items', audit: 'Dashboard' };
    let _customersTimeline = { id: '', nodes: [], next: '', error: '', loading: false };
    // The graph view of the open profile, read from the customer graph:
    // the person, their artefacts, and the other people on those artefacts.
    const CUSTOMERS_GRAPH_EDGES = 25;
    const CUSTOMERS_GRAPH_QUERY = 'query Graph($id: ID!, $first: Int) { person(id: $id) { id name edges(first: $first) { nodes { predicate artefact { ref kind people(first: 5) { predicate person { id name me } } } } } } }';
    let _customersGraph = { id: '', on: false, nodes: null, error: '', loading: false };

    function customersIsEnabled() { return window._customersEnabled === true; }
    function customersIsFull() { return !!(_customersData && _customersData.full); }
    function customersIsOwner() { return !!(_customersData && _customersData.owner); }

    // ── Visibility: the section shows only while customers are enabled ──
    function customersApplyVisibility() {
      const on = customersIsEnabled();
      const el = document.getElementById('customers-section');
      const nav = document.querySelector('.oc-nav-item[data-section="customers-section"]');
      const changed = (el && el.hidden === on) || (nav && nav.hidden === on);
      if (el) el.hidden = !on;
      if (nav) nav.hidden = !on;
      if (changed && typeof dashboardApplyLayout === 'function' && typeof dashboardLayoutNormalize === 'function') {
        dashboardApplyLayout(dashboardLayoutNormalize(dashboardLayoutRead()));
      }
    }

    async function fetchCustomers() {
      try {
        const r = await fetch('/api/customers');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) { _customersError = data.error || ('HTTP ' + r.status); }
        else {
          _customersData = data;
          _customersError = '';
          window._customersEnabled = data.enabled === true;
        }
      } catch (e) {
        if (!_customersData) _customersError = 'Customers are unreachable right now; retrying.';
      }
      customersApplyVisibility();
      renderCustomers();
    }
    dashboardScheduleWork('customers', fetchCustomers).catch(console.error);
    dashboardSetInterval('customers', fetchCustomers, CUSTOMERS_POLL_MS);

    async function customersRequest(method, path, body) {
      const init = { method: method };
      if (body !== undefined) { init.headers = { 'Content-Type': 'application/json' }; init.body = JSON.stringify(body); }
      const r = await fetch(path, init);
      const data = await r.json().catch(() => ({}));
      if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      return data;
    }

    function customersField(name) { return document.querySelector('[data-customers-field="' + name + '"]'); }
    function customersFieldValue(name) { return ((customersField(name) || {}).value || '').trim(); }

    function customersFormatTime(iso) {
      if (!iso) return '';
      const d = new Date(iso);
      if (isNaN(d.getTime()) || d.getFullYear() < 2000) return '';
      try { return new Intl.DateTimeFormat(undefined, { year: 'numeric', month: 'short', day: 'numeric' }).format(d); }
      catch (e) { return d.toISOString().slice(0, 10); }
    }

    // customersActiveTags is the names of the tags that are on.
    function customersActiveTags(p) {
      return ((p && p.tags) || []).filter(t => t && t.state === 'on').map(t => t.name);
    }

    // customersGoalsSplit separates the goals a person stands behind from
    // the ones hive inferred and is offering.
    function customersGoalsSplit(p) {
      const all = ((p && p.goals) || []).filter(g => g && g.id);
      return { confirmed: all.filter(g => g.state === 'confirmed'), suggested: all.filter(g => g.state === 'suggested') };
    }

    // customersSummaryText is the collapsed section's one-line summary.
    function customersSummaryText(data) {
      if (!data || !data.enabled) return 'off';
      if (!data.full) return 'your profile';
      const people = (data.people || []).length + 1;
      const suggested = (data.suggestions || []).length;
      return people + ' customer' + (people === 1 ? '' : 's') + (suggested ? ' · ' + suggested + ' suggested' : '');
    }

    function customersKindOptions() {
      return CUSTOMERS_ADDABLE_KINDS.map(k => '<option value="' + k + '">' + escapeHtml(CUSTOMERS_KIND_LABELS[k] || k) + '</option>').join('');
    }

    function customersLevelPill(level) {
      if (!level || level === 'unknown') return '';
      return '<span class="kc-pill" data-status="' + escapeHtml(CUSTOMERS_LEVEL_PILL[level] || '') + '">' + escapeHtml(CUSTOMERS_LEVEL_LABELS[level] || level) + '</span>';
    }

    // A reference hive keeps for a suggestion or a goal ("gh:pr:org/repo#7",
    // "event:<id>") is shown as something a person can read: an issue or pull
    // request links to GitHub, anything else is named by what it is. The raw
    // reference stays out of the page.
    const CUSTOMERS_REF_GH = /^gh:(issue|pr):([A-Za-z0-9._-]+\/[A-Za-z0-9._-]+)#(\d+)$/;
    const CUSTOMERS_REF_WORDS = { event: 'calendar invite', meeting: 'meeting', audit: 'dashboard activity', fact: 'knowledge entry', bead: 'work item' };
    // How many rows of one source a suggested group shows before "Show more".
    const CUSTOMERS_GROUP_SHOWN = 5;
    const CUSTOMERS_SUGGESTED_GROUPS = { github: 'GitHub people', email: 'Email addresses' };
    const CUSTOMERS_OTHER_GROUP = 'Other people';
    const CUSTOMERS_KIND_ICONS = { github: '🐙', email: '✉️', git_email: '✉️', discord: '💬', slack: '💬', teams: '💬', meet: '🎥', zoom: '🎥' };
    // What the panel remembers across a refresh: which disclosures are open
    // (the Suggested group and each "Why is this suggested?"), which groups
    // are expanded, and which picker is showing.
    let _customersUi = { open: {}, expanded: {}, picker: '', profilePicker: false, focusRow: '' };

    function customersInitial(name) {
      const m = /[A-Za-z0-9]/.exec(String(name || ''));
      return m ? m[0].toUpperCase() : '?';
    }

    // customersAvatarsOn: GitHub avatars show only while the hive says so
    // (customers.avatars.enabled). Off, the list makes no request to github.com.
    function customersAvatarsOn() { return !!(_customersData && _customersData.avatars === true); }

    // customersGithubLogin is the login of a person's confirmed GitHub
    // identity, or '' when they have none. An unconfirmed (claimed) identity
    // never picks a picture: anyone can type any login there.
    function customersGithubLogin(p) {
      const ids = (p && p.identities) || [];
      for (let n = 0; n < ids.length; n++) {
        const i = ids[n];
        if (i && i.kind === 'github' && !i.claimed && CUSTOMERS_GITHUB_LOGIN.test(String(i.value || ''))) return String(i.value);
      }
      return '';
    }

    // customersAvatarHtml draws the initial (or a source icon) in a circle and,
    // for a GitHub login, the picture over it. The box has a fixed size, so
    // nothing moves when the picture arrives; if it fails the picture hides
    // itself and the initial stays. Email-only people never get a picture: no
    // service keyed by an address is asked.
    function customersAvatarHtml(name, login, icon, title) {
      const face = icon || escapeHtml(customersInitial(name));
      let img = '';
      if (login && CUSTOMERS_GITHUB_LOGIN.test(login) && customersAvatarsOn()) {
        img = '<img class="customers-avatar-img" src="https://github.com/' + encodeURIComponent(login) + '.png?size=' + CUSTOMERS_AVATAR_FETCH_PX + '" alt="' + escapeHtml(name) + '" width="' + CUSTOMERS_AVATAR_PX + '" height="' + CUSTOMERS_AVATAR_PX + '" loading="lazy" decoding="async" referrerpolicy="no-referrer" data-error-action="gh19">';
      }
      return '<span class="customers-avatar"' + (title ? ' title="' + escapeHtml(title) + '"' : '') + '><span class="customers-initial" aria-hidden="true">' + face + '</span>' + img + '</span>';
    }

    // customersMorePill is the "+N" pill; its tooltip lists what it hides.
    function customersMorePill(rest) {
      if (!rest.length) return '';
      return '<span class="kc-pill customers-pill customers-more" title="' + escapeHtml(rest.join(', ')) + '">+' + rest.length + '</span>';
    }

    // customersSourcePills: the identities a person is known by, as compact
    // monospace pills. The dashboard-user and name identities say nothing a
    // row does not already say.
    function customersSourcePills(p) {
      const ids = ((p && p.identities) || []).filter(i => i && i.value && i.kind !== 'hive_user' && i.kind !== 'name');
      const label = i => (CUSTOMERS_KIND_LABELS[i.kind] || i.kind) + ': ' + i.value + (i.claimed ? ' (unconfirmed)' : '');
      return ids.slice(0, CUSTOMERS_SOURCE_PILLS_SHOWN).map(i =>
        '<span class="kc-pill customers-pill customers-source" data-kind="' + escapeHtml(i.kind) + '" data-claimed="' + (i.claimed ? 'true' : 'false') + '" title="' + escapeHtml(label(i)) + '">' + escapeHtml(i.value) + '</span>').join('') +
        customersMorePill(ids.slice(CUSTOMERS_SOURCE_PILLS_SHOWN).map(label));
    }

    // The optional descriptive fields of a profile, in display order.
    const CUSTOMERS_PROFILE_FIELDS = [
      { key: 'real_name', label: 'Real name', max: 120 },
      { key: 'organization', label: 'Organization', max: 120 },
      { key: 'location', label: 'Location', max: 120 },
      { key: 'country', label: 'Country (code or name)', max: 120 }
    ];

    function customersProfileValue(p, key) {
      const f = (p && p.profile && p.profile[key]) || {};
      return typeof f.value === 'string' ? f.value : '';
    }

    // customersOrgPill: the organization as one small pill, with where it came
    // from in its tooltip. Nothing is shown when there is none.
    function customersOrgPill(p) {
      const v = customersProfileValue(p, 'organization');
      if (!v) return '';
      const f = p.profile.organization;
      const why = f.source === 'manual' ? 'Set by ' + (f.by || 'a person') : 'From the public GitHub profile';
      return '<span class="kc-pill customers-pill customers-org" data-source="' + (f.source === 'manual' ? 'manual' : 'auto') + '" title="' + escapeHtml('Organization - ' + why) + '">' + escapeHtml(v) + '</span>';
    }

    // customersFlagEmoji: the flag for an ISO 3166 alpha-2 code, or '' when the
    // code is not two letters. Some systems draw the flag as the two letters,
    // so the code is always shown as text beside it.
    function customersFlagEmoji(code) {
      if (typeof code !== 'string' || !/^[A-Z]{2}$/.test(code)) return '';
      return String.fromCodePoint(0x1F1E6 + code.charCodeAt(0) - 65, 0x1F1E6 + code.charCodeAt(1) - 65);
    }

    // customersCountryName: the country's name in the browser language, or the
    // code itself when the browser cannot name it.
    function customersCountryName(code) {
      try {
        const n = new Intl.DisplayNames(undefined, { type: 'region' }).of(code);
        if (n) return n;
      } catch (e) { /* no Intl.DisplayNames: fall back to the code */ }
      return code;
    }

    // customersCountryPill: a compact flag and ISO code on the row. The name is
    // the accessible label, so the meaning never depends on the emoji.
    function customersCountryPill(p) {
      const code = customersProfileValue(p, 'country');
      if (!/^[A-Z]{2}$/.test(code)) return '';
      const name = customersCountryName(code);
      return '<span class="kc-pill customers-pill customers-country" role="img" aria-label="' + escapeHtml('Country: ' + name) + '" title="' + escapeHtml(name) + '">' +
        '<span aria-hidden="true">' + customersFlagEmoji(code) + '</span> ' + escapeHtml(code) + '</span>';
    }

    // customersAffiliationHtml: the likely organization, with its evidence, as
    // a suggestion to accept or dismiss. Nothing is saved until Accept.
    function customersAffiliationHtml(p) {
      const s = p && p.affiliation_suggestion;
      if (!s || !s.value) return '';
      return '<div class="customers-inline" data-customers-field="affiliation-suggestion"><span>Likely: <strong>' + escapeHtml(s.value) + '</strong> <span class="muted-xs">(' + escapeHtml(s.evidence || 'a guess') + ')</span></span>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersAffiliationAccept" data-arg0="' + escapeHtml(s.value) + '">Accept</button>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersAffiliationDismiss" data-arg0="' + escapeHtml(s.value) + '">Dismiss</button></div>';
    }

    // customersOriginPill: where the person came from, in one short word.
    function customersOriginPill(p, updated) {
      const origin = p && p.origin;
      if (!origin) return '';
      const long = (CUSTOMERS_ORIGIN_LABELS[origin] || origin) + (updated ? ', updated ' + updated : '');
      return '<span class="kc-pill customers-pill customers-origin" title="' + escapeHtml(long) + '">' + escapeHtml(CUSTOMERS_ORIGIN_SHORT[origin] || origin) + '</span>';
    }

    // customersTagPill: a tag that is on. A default tag has its own tone; a
    // free-form tag is neutral. A tag a person set carries a dot, and the
    // tooltip says who set it or which rule did.
    function customersTagPill(t) {
      const manual = t.source === 'manual';
      const why = manual ? 'Set by ' + (t.by || 'a person') : (t.rule ? 'Automatic: ' + t.rule : 'Automatic');
      const isDefault = ((_customersData && _customersData.default_tags) || []).indexOf(t.name) >= 0;
      return '<span class="kc-pill customers-pill customers-tag"' + (isDefault ? ' data-tag="' + escapeHtml(t.name) + '"' : '') + ' data-source="' + (manual ? 'manual' : 'auto') + '" title="' + escapeHtml(t.name + ' - ' + why) + '">' +
        (manual ? '<span class="customers-tag-dot" aria-hidden="true"></span>' : '') + escapeHtml(t.name) + (manual ? '<span class="customers-sr"> (set by hand)</span>' : '') + '</span>';
    }

    function customersTagPills(p) {
      const on = ((p && p.tags) || []).filter(t => t && t.name && t.state === 'on');
      return on.slice(0, CUSTOMERS_TAG_PILLS_SHOWN).map(customersTagPill).join('') +
        customersMorePill(on.slice(CUSTOMERS_TAG_PILLS_SHOWN).map(t => t.name));
    }

    function customersEvidenceLabel(ref) {
      const s = String(ref || '');
      const m = CUSTOMERS_REF_GH.exec(s);
      if (m) {
        const href = 'https://github.com/' + m[2] + '/' + (m[1] === 'pr' ? 'pull' : 'issues') + '/' + m[3];
        return '<a href="' + escapeHtml(href) + '" target="_blank" rel="noopener noreferrer" title="' + escapeHtml(m[2] + '#' + m[3]) + '">' + (m[1] === 'pr' ? 'PR' : 'Issue') + ' #' + escapeHtml(m[3]) + '</a>';
      }
      const kind = s.split(':')[0];
      if (kind === 'inbox') return '<span>' + (s.indexOf('inbox:email:') === 0 ? 'mail' : 'inbox file') + '</span>';
      return '<span>' + escapeHtml(CUSTOMERS_REF_WORDS[kind] || 'activity') + '</span>';
    }

    function customersEvidenceHtml(refs) {
      const list = (refs || []).slice(0, CUSTOMERS_EVIDENCE_SHOWN);
      if (!list.length) return '';
      return '<div class="muted-xs customers-evidence" title="What led hive to suggest this">from ' + list.map(customersEvidenceLabel).join(', ') + '</div>';
    }

    // customersWhyHtml hides the evidence behind a disclosure, at most five
    // references, whose open state survives a refresh.
    function customersWhyHtml(sid, refs) {
      const list = (refs || []).slice(0, CUSTOMERS_EVIDENCE_SHOWN);
      if (!list.length) return '';
      const key = 'why:' + sid;
      return '<details class="customers-why" data-ui-key="' + escapeHtml(key) + '"' + (_customersUi.open[key] ? ' open' : '') + '>' +
        '<summary class="muted-xs">Why is this suggested?</summary>' +
        '<div class="muted-xs customers-why-body">Hive saw this person on ' + list.map(customersEvidenceLabel).join(', ') + '.</div></details>';
    }

    // ── List ──
    // One slim line per customer: chevron, picture, name, then pills
    // (identities, origin, tags, goals to review, satisfaction) and the date.
    // The chevron and the row both open the profile in place, under the row;
    // the open row stays open across a refresh. On a narrow screen the pills
    // wrap under the name.
    function customersRowHtml(p, isMe) {
      const goals = customersGoalsSplit(p);
      const pill = customersLevelPill(p.satisfaction && p.satisfaction.level);
      const pending = goals.suggested.length + (p.satisfaction && p.satisfaction.suggestion ? 1 : 0);
      const extra = [];
      // No goals is the usual case, so it is not said on every row.
      if (goals.confirmed.length) extra.push('<span class="kc-pill customers-pill">' + goals.confirmed.length + ' goal' + (goals.confirmed.length === 1 ? '' : 's') + '</span>');
      if (pending) extra.push('<span class="kc-pill customers-pill" title="' + pending + ' suggestion' + (pending === 1 ? '' : 's') + ' to review">' + pending + ' to review</span>');
      const updated = customersFormatTime(p.updated_at);
      const name = p.name || p.id || '';
      const rid = escapeHtml(isMe ? 'me' : p.id);
      const open = !!(_customersOpen && (isMe ? _customersOpen.me : (!_customersOpen.me && _customersOpen.id === p.id)));
      const pid = 'customers-profile-' + String(isMe ? 'me' : p.id).replace(/[^A-Za-z0-9_-]/g, '_');
      return '<div class="customers-item" data-customer-item="' + rid + '">' +
        '<div class="customers-card" data-customer="' + escapeHtml(p.id) + '" data-me="' + (isMe ? 'true' : 'false') + '" data-open="' + open + '" data-action="customersToggle" data-arg0="' + rid + '">' +
        rowChevronHtml('customersToggle', isMe ? 'me' : p.id, open, 'profile of ' + name, pid) +
        customersAvatarHtml(name, customersGithubLogin(p), '', '') +
        '<div class="customers-row-main"><span class="customers-name" title="' + escapeHtml(name) + '">' + escapeHtml(name) + '</span>' + (isMe ? '<span class="badge customers-me">me</span>' : '') +
        customersSourcePills(p) + customersOriginPill(p, updated) + customersOrgPill(p) + customersCountryPill(p) + customersTagPills(p) + extra.join('') + pill + '</div>' +
        (updated ? '<span class="muted-xs customers-updated" title="Last updated">' + escapeHtml(updated) + '</span>' : '') + '</div>' +
        (open ? '<div class="customers-profile" id="' + pid + '" role="region" aria-label="Profile of ' + escapeHtml(name) + '">' + customersProfileHtml(_customersOpen) + '</div>' : '') + '</div>';
    }

    // customersPickerHtml lists the current customers, Me first, for a
    // suggestion to be attached to. The filter narrows the list by name.
    function customersPickerHtml(sid) {
      const data = _customersData || {};
      const me = data.me || {};
      const people = [{ id: 'me', name: 'Me' + (me.name ? ' (' + me.name + ')' : '') }].concat((data.people || []).filter(p => p && p.id).map(p => ({ id: p.id, name: p.name || p.id })));
      return '<div class="customers-picker" data-customers-picker="' + escapeHtml(sid) + '">' +
        '<label class="customers-picker-filter">Find a customer<input class="kb-search-box" type="text" maxlength="120" data-customers-field="picker-filter" autocomplete="off"></label>' +
        '<div class="customers-picker-list">' + people.map(p => '<button type="button" class="hv-btn btn-secondary btn-sm customers-pick" data-action="customersAttach" data-arg0="' + escapeHtml(sid) + '" data-arg1="' + escapeHtml(p.id) + '" data-name="' + escapeHtml(p.name.toLowerCase()) + '" title="' + escapeHtml(p.name) + '">' + escapeHtml(p.name) + '</button>').join('') + '</div>' +
        '<div class="customers-picker-foot"><span class="muted-xs">Their identity joins that customer. You can undo it from their profile, under Merge.</span>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersPickerClose">Cancel</button></div></div>';
    }

    function customersSuggestionRowHtml(s) {
      const id = escapeHtml(s.id);
      const countList = Object.keys(s.counts || {}).sort().map(k => s.counts[k] + ' ' + k.replace(/_/g, ' '));
      const seen = customersFormatTime(s.last_seen);
      const value = s.value || '';
      const kind = CUSTOMERS_KIND_LABELS[s.kind] || s.kind || '';
      const login = s.kind === 'github' ? value : '';
      return '<div class="customers-card" data-kind="suggestion" data-customer-suggestion="' + id + '">' +
        customersAvatarHtml(value, login, CUSTOMERS_KIND_ICONS[s.kind] || '', kind) +
        '<div class="customers-row-main"><span class="customers-name" title="' + escapeHtml(value) + '">' + escapeHtml(value) + '</span>' +
        countList.slice(0, CUSTOMERS_SOURCE_PILLS_SHOWN).map(c => '<span class="kc-pill customers-pill">' + escapeHtml(c) + '</span>').join('') +
        customersMorePill(countList.slice(CUSTOMERS_SOURCE_PILLS_SHOWN)) +
        customersWhyHtml(s.id, s.evidence) + '</div>' +
        (seen ? '<span class="muted-xs customers-updated" title="Last seen">' + escapeHtml(seen) + '</span>' : '') +
        '<div class="customers-row-actions">' +
        '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersAccept" data-arg0="' + id + '" title="Make this person a customer and build their profile">Accept</button>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersPickerOpen" data-arg0="' + id + '" title="Add this identity to a customer you already have, instead of making a new one">Add to existing…</button>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersDismiss" data-arg0="' + id + '" title="Do not suggest this person again; hive keeps no readable trace of them">Dismiss</button></div>' +
        (_customersUi.picker === s.id ? customersPickerHtml(s.id) : '') + '</div>';
    }

    // customersSuggestionGroups orders suggestions by where hive saw them:
    // GitHub people, then email addresses, then anyone else. Each group keeps
    // the order the hive sent.
    function customersSuggestionGroups(list) {
      const by = {};
      const order = [];
      (list || []).forEach(s => {
        if (!s || !s.id) return;
        const key = CUSTOMERS_SUGGESTED_GROUPS[s.kind] ? s.kind : 'other';
        if (!by[key]) { by[key] = []; order.push(key); }
        by[key].push(s);
      });
      const rank = k => k === 'github' ? 0 : (k === 'email' ? 1 : 2);
      order.sort((a, b) => rank(a) - rank(b));
      return order.map(k => ({ key: k, label: CUSTOMERS_SUGGESTED_GROUPS[k] || CUSTOMERS_OTHER_GROUP, items: by[k] }));
    }

    function customersSuggestionGroupHtml(g) {
      const open = !!_customersUi.expanded[g.key];
      const shown = open ? g.items : g.items.slice(0, CUSTOMERS_GROUP_SHOWN);
      const hidden = g.items.length - shown.length;
      return '<div class="customers-group" data-customers-group="' + escapeHtml(g.key) + '"><div class="customers-group-title">' + escapeHtml(g.label) + ' <span class="muted-xs">(' + g.items.length + ')</span></div>' +
        '<div class="customers-list">' + shown.map(customersSuggestionRowHtml).join('') + '</div>' +
        (hidden > 0 ? '<div class="customers-row-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersShowMore" data-arg0="' + escapeHtml(g.key) + '">Show ' + hidden + ' more</button></div>' :
          (open && g.items.length > CUSTOMERS_GROUP_SHOWN ? '<div class="customers-row-actions"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersShowMore" data-arg0="' + escapeHtml(g.key) + '">Show fewer</button></div>' : '')) + '</div>';
    }

    function customersAddFormHtml() {
      return '<details class="customers-add"><summary class="muted-xs">Add customer</summary><div class="customers-form mt-2">' +
        '<label>Name<input class="kb-search-box" type="text" maxlength="120" data-customers-field="add-name" placeholder="Ada Lovelace" autocomplete="off"></label>' +
        '<label>Known by (optional)<select class="kb-search-box" data-customers-field="add-kind">' + customersKindOptions() + '</select></label>' +
        '<label>Login, address or id<input class="kb-search-box" type="text" maxlength="254" data-customers-field="add-value" placeholder="octocat" autocomplete="off"></label>' +
        '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersAdd">Add customer</button></div>' +
        '<div class="hive-dialog-error" role="alert" data-customers-field="add-error"></div></details>';
    }

    function customersListHtml(data) {
      const me = data.me || {};
      let out = '<div class="customers-block"><div class="section-label m-0">Me</div><div class="customers-list">' + customersRowHtml(me, true) + '</div></div>';
      if (!data.full) {
        return out + '<p class="muted-xs">You see only your own profile. The hive owner and read-write users can see the other customers.</p>';
      }
      const people = data.people || [];
      const suggestions = data.suggestions || [];
      out += customersAddFormHtml();
      out += '<div class="customers-block"><div class="section-label m-0">Customers</div><div class="customers-list">' +
        (people.length ? people.map(p => customersRowHtml(p, false)).join('') : '<p class="empty-dashed">No other customers yet. Add one, or accept a suggestion.</p>') + '</div></div>';
      out += '<details class="customers-block customers-suggested" data-ui-key="suggested"' + (_customersUi.open.suggested ? ' open' : '') + '><summary class="customers-summary">Suggested (' + suggestions.length + ')</summary>' +
        '<p class="muted-xs customers-hint">People hive has seen in issues, pull requests, meetings and mail who are not customers. Hive keeps only a key, counts and a few references for them until you accept one.</p>' +
        (suggestions.length ? customersSuggestionGroups(suggestions).map(customersSuggestionGroupHtml).join('') : '<p class="empty-dashed">Nobody to suggest.</p>') + '</details>';
      return out;
    }

    // ── Profile ──
    function customersIdentityRowHtml(p, i) {
      const key = i.kind + ':' + i.value;
      const fixed = i.kind === 'hive_user';
      const meta = [];
      if (i.claimed) meta.push('unconfirmed');
      else if (i.manual) meta.push('added by ' + (i.by || 'a person'));
      else if (i.source) meta.push(String(i.source).replace(/_/g, ' '));
      let buttons = '';
      if (i.claimed && customersIsFull()) {
        buttons += '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersIdentityConfirm" data-arg0="' + escapeHtml(i.kind) + '" data-arg1="' + escapeHtml(i.value) + '" title="Confirm this is theirs: it then links their activity">Confirm</button>';
      }
      if (customersIsFull() && !fixed && !i.claimed && i.kind !== 'name' && (p.identities || []).length > 1) {
        buttons += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersSplit" data-arg0="' + escapeHtml(key) + '" title="Move this identity, and the activity it produced, to a new customer">Split off</button>';
      }
      if (!fixed) {
        buttons += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersIdentityRemove" data-arg0="' + escapeHtml(key) + '">Remove</button>';
      }
      return '<tr data-claimed="' + (i.claimed ? 'true' : 'false') + '"><td><span class="badge">' + escapeHtml(CUSTOMERS_KIND_LABELS[i.kind] || i.kind) + '</span></td>' +
        '<th scope="row" class="customers-ident-value">' + escapeHtml(i.value) + '</th>' +
        '<td class="muted-xs">' + escapeHtml(meta.join(' · ')) + '</td>' +
        '<td class="customers-ident-actions">' + buttons + '</td></tr>';
    }

    function customersIdentitiesHtml(p) {
      const ids = p.identities || [];
      const note = customersIsFull() ? '' : '<p class="muted-xs">What you add here is shown as unconfirmed until the hive owner or a read-write user confirms it.</p>';
      return '<div class="customers-block"><div class="section-label m-0">Identities</div>' + note +
        (ids.length ? '<div class="customers-table-wrap"><table class="customers-ident-table"><thead><tr><th scope="col">Type</th><th scope="col">Identity</th><th scope="col">Source</th><th scope="col" aria-label="Actions"></th></tr></thead><tbody>' + ids.map(i => customersIdentityRowHtml(p, i)).join('') + '</tbody></table></div>' : '<p class="empty-dashed">No identities.</p>') +
        '<div class="customers-inline"><label>Known by<select class="kb-search-box" data-customers-field="identity-kind">' + customersKindOptions() + '</select></label>' +
        '<label>Login, address or id<input class="kb-search-box" type="text" maxlength="254" data-customers-field="identity-value" autocomplete="off"></label>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersIdentityAdd">Add identity</button></div></div>';
    }

    // customersTagChipHtml is the toggle form of the row's tag pill: the same
    // pill classes and tones, as a button. Off is dimmed; a tag hive set carries
    // a small "auto" marker, a tag a person set carries the dot the row shows.
    function customersTagChipHtml(name, tag) {
      const state = tag ? tag.state : 'off';
      const source = tag ? tag.source : 'auto';
      const on = state === 'on';
      const manual = !!tag && source === 'manual';
      const isDefault = ((_customersData && _customersData.default_tags) || []).indexOf(name) >= 0;
      const why = !tag ? 'Not set' : (manual ? 'Set by ' + (tag.by || 'a person') : (tag.rule ? 'Automatic: ' + tag.rule : 'Automatic'));
      let out = '<button type="button" class="kc-pill customers-pill customers-tag customers-tag-toggle"' + (isDefault ? ' data-tag="' + escapeHtml(name) + '"' : '') + ' data-state="' + state + '" data-source="' + escapeHtml(source) + '" aria-pressed="' + on + '" title="' + escapeHtml(why) + '"' +
        ' data-action="customersTagSet" data-arg0="' + escapeHtml(name) + '" data-arg1="' + (on ? 'off' : 'on') + '">' +
        (manual ? '<span class="customers-tag-dot" aria-hidden="true"></span>' : '') + escapeHtml(name) +
        (tag && !manual ? '<span class="customers-tag-auto">auto</span>' : '') + (manual ? '<span class="customers-sr"> (set by hand)</span>' : '') + '</button>';
      if (manual) {
        out += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersTagSet" data-arg0="' + escapeHtml(name) + '" data-arg1="auto" title="Hand this tag back to the automatic rules">Reset to automatic</button>';
      }
      return out;
    }

    function customersTagsHtml(p) {
      const byName = {};
      (p.tags || []).forEach(t => { if (t && t.name) byName[t.name] = t; });
      const names = ((_customersData && _customersData.default_tags) || []).slice();
      Object.keys(byName).forEach(n => { if (names.indexOf(n) < 0) names.push(n); });
      return '<div class="customers-block"><div class="section-label m-0">Tags</div>' +
        '<p class="muted-xs">Click a tag to turn it on or off. A tag you set stays as you set it; hive only changes the ones marked auto.</p>' +
        '<div class="customers-chips">' + names.map(n => customersTagChipHtml(n, byName[n])).join('') + '</div>' +
        '<div class="customers-inline"><label>New tag<input class="kb-search-box" type="text" maxlength="40" data-customers-field="tag-name" placeholder="early adopter" autocomplete="off"></label>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersTagAdd">Add tag</button></div></div>';
    }

    function customersGoalRowHtml(g) {
      const id = escapeHtml(g.id);
      const suggested = g.state === 'suggested';
      const meta = suggested ? 'Suggested by hive' : (g.source === 'manual' ? 'Written by ' + (g.by || 'a person') : 'Suggested by hive, confirmed by ' + (g.by || 'a person'));
      return '<div class="customers-row" data-goal-state="' + escapeHtml(g.state) + '"><div class="customers-row-main"><div>' + escapeHtml(g.text || '') + '</div>' +
        '<div class="muted-xs">' + escapeHtml(meta) + '</div>' + customersEvidenceHtml(g.evidence) + '</div>' +
        (suggested ? '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersGoalConfirm" data-arg0="' + id + '">Confirm</button>' : '') +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersGoalEdit" data-arg0="' + id + '">Edit</button>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersGoalReject" data-arg0="' + id + '">' + (suggested ? 'Reject' : 'Remove') + '</button></div>';
    }

    function customersGoalsHtml(p) {
      const goals = customersGoalsSplit(p);
      return '<div class="customers-block"><div class="section-label m-0">Goals</div>' +
        '<p class="muted-xs">What this person is trying to get done. Hive suggests goals from their issues, pull requests, meetings and mail; a goal counts only once a person confirms it.</p>' +
        '<div class="customers-list">' + (goals.confirmed.length ? goals.confirmed.map(customersGoalRowHtml).join('') : '<p class="empty-dashed">No goal yet. Add one below, or confirm one hive suggests.</p>') + '</div>' +
        (goals.suggested.length ? '<div class="muted-xs">Suggested</div><div class="customers-list">' + goals.suggested.map(customersGoalRowHtml).join('') + '</div>' : '') +
        '<div class="customers-inline"><label>New goal<input class="kb-search-box" type="text" maxlength="280" data-customers-field="goal-text" placeholder="Get CSV exports working for their team" autocomplete="off"></label>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersGoalAdd">Add goal</button></div></div>';
    }

    // Satisfaction is the maintainers' assessment: full view only.
    function customersSatisfactionHtml(p) {
      if (!customersIsFull()) return '';
      const sat = p.satisfaction || {};
      const level = sat.level || 'unknown';
      const levels = (_customersData && _customersData.levels) || Object.keys(CUSTOMERS_LEVEL_LABELS);
      const who = sat.source === 'manual' ? 'Set by ' + (sat.by || 'a person') : (sat.source === 'auto' ? 'Suggested by hive, confirmed by ' + (sat.by || 'a person') : 'Not assessed yet');
      const buttons = levels.map(l => '<button type="button" class="hv-btn ' + (l === level ? 'btn-primary' : 'btn-secondary') + ' btn-sm" aria-pressed="' + (l === level) + '" data-action="customersSatisfactionSet" data-arg0="' + escapeHtml(l) + '">' + escapeHtml(CUSTOMERS_LEVEL_LABELS[l] || l) + '</button>').join('');
      const sg = sat.suggestion;
      return '<div class="customers-block"><div class="section-label m-0">Satisfaction</div>' +
        '<p class="muted-xs">' + escapeHtml(who) + '. There is no score: only a level, and what led to it.</p>' +
        '<div class="customers-chips">' + buttons + '</div>' + customersEvidenceHtml(sat.evidence) +
        (sg ? '<div class="customers-row" data-satisfaction-suggestion="true"><div class="customers-row-main"><div>Hive suggests ' + customersLevelPill(sg.level) + '</div>' + customersEvidenceHtml(sg.evidence) + '</div>' +
          '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersSatisfactionConfirm">Confirm</button>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersSatisfactionReject">Reject</button></div>' : '') + '</div>';
    }

    // customersDetailsHtml: real name, organization and location, compact.
    // Each says whether GitHub or a person set it; a field set by hand has a
    // button that hands it back to the automatic source. Everyone who can
    // open the profile may edit these, a read-only user on their own.
    function customersDetailsHtml(p) {
      const rows = CUSTOMERS_PROFILE_FIELDS.map(function (f) {
        const cur = (p.profile && p.profile[f.key]) || {};
        const manual = cur.source === 'manual';
        const mark = cur.value ? (manual ? 'set by hand' : (cur.rule === 'location' ? 'from location' : 'from GitHub')) : (manual ? 'cleared by hand' : 'not known');
        return '<label>' + escapeHtml(f.label) + ' <span class="muted-xs">(' + mark + ')</span>' +
          '<input class="kb-search-box" type="text" maxlength="' + f.max + '" data-customers-field="profile-' + f.key + '" value="' + escapeHtml(cur.value || '') + '"></label>' +
          (manual ? '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersProfileReset" data-arg0="' + f.key + '" title="Let GitHub fill this in again">Auto</button>' : '');
      }).join('');
      const cc = customersProfileValue(p, 'country');
      const ccLine = /^[A-Z]{2}$/.test(cc) ? '<div class="muted-xs" data-customers-field="country-line"><span aria-hidden="true">' + customersFlagEmoji(cc) + '</span> ' + escapeHtml(cc) + ' ' + escapeHtml(customersCountryName(cc)) + '</div>' : '';
      return '<div class="customers-block"><div class="section-label m-0">Details</div>' + ccLine +
        '<div class="customers-inline">' + rows + '</div>' + customersAffiliationHtml(p) +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersProfileSave">Save details</button></div></div>';
    }

    function customersNotesHtml(p) {
      if (!customersIsFull()) return '';
      return '<div class="customers-block"><div class="section-label m-0">Notes</div>' +
        '<textarea class="kb-search-box customers-notes" maxlength="4000" data-customers-field="notes" placeholder="Notes for the owner and read-write users. The person does not see these.">' + escapeHtml(p.notes || '') + '</textarea>' +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersNotesSave">Save notes</button></div></div>';
    }

    // customersTimelineGroups orders timeline entries by the kind of thing
    // they point at, keeping each group newest first.
    function customersTimelineGroups(nodes) {
      const groups = {};
      const order = [];
      (nodes || []).forEach(n => {
        const kind = (n && n.artefact && n.artefact.kind) || 'other';
        if (!groups[kind]) { groups[kind] = []; order.push(kind); }
        groups[kind].push(n);
      });
      return order.map(kind => ({ kind: kind, label: CUSTOMERS_REF_KIND_LABELS[kind] || kind, nodes: groups[kind] }));
    }

    function customersTimelineRowHtml(n) {
      const ref = (n.artefact && n.artefact.ref) || '';
      return '<div class="customers-row"><span class="badge">' + escapeHtml(String(n.predicate || '').replace(/_/g, ' ')) + '</span>' +
        '<div class="customers-row-main"><div class="customers-evidence">' + escapeHtml(ref) + '</div></div>' +
        '<span class="muted-xs">' + escapeHtml(customersFormatTime(n.at)) + '</span></div>';
    }

    // The timeline holds references only: an issue, a meeting or a mail item
    // is named by its id, never quoted.
    function customersTimelineHtml() {
      const t = _customersTimeline;
      if (t.error) return '<p class="kc-error muted-xs">' + escapeHtml(t.error) + '</p>';
      if (t.loading && !t.nodes.length) return '<p class="muted-xs">Loading activity…</p>';
      if (!t.nodes.length) return '<p class="empty-dashed">No activity recorded yet.</p>';
      return customersTimelineGroups(t.nodes).map(g => '<div class="muted-xs">' + escapeHtml(g.label) + ' (' + g.nodes.length + ')</div>' +
        '<div class="customers-list">' + g.nodes.map(customersTimelineRowHtml).join('') + '</div>').join('') +
        (t.next ? '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersTimelineMore">Show more</button></div>' : '');
    }

    // A "Same person?" hint names a suggested identity that may belong to this
    // customer. Yes makes it theirs; No closes the question for good. A hint
    // between two customers is answered by merging them, so it has no Yes here.
    function customersHintRowHtml(h) {
      const id = escapeHtml(h.id);
      const what = h.other_kind === 'customer' ? 'the customer ' + (h.other_value || '') : (CUSTOMERS_KIND_LABELS[h.other_kind] || h.other_kind || '') + ' ' + (h.other_value || '');
      const yes = h.other_kind === 'customer' ? '' : '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersHintAnswer" data-arg0="' + id + '" data-arg1="yes" title="Make this identity theirs: it then links its activity">Yes</button>';
      return '<div class="customers-row" data-hint="' + id + '"><div class="customers-row-main"><div>Same person? ' + escapeHtml(what.trim()) + '</div>' +
        '<div class="muted-xs">' + escapeHtml(h.reason || '') + '. Nothing is linked until you answer.</div></div>' + yes +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersHintAnswer" data-arg0="' + id + '" data-arg1="no" title="Not the same person: do not ask again">No</button></div>';
    }

    function customersActivityHtml(p) {
      if (!customersIsFull()) return '';
      const counts = p.counts || {};
      const keys = Object.keys(counts).sort();
      const hints = p.hints || [];
      return '<div class="customers-block"><div class="section-label m-0">Activity</div>' +
        (keys.length ? '<div class="customers-chips">' + keys.map(k => '<span class="badge">' + escapeHtml(k.replace(/_/g, ' ')) + ' ' + counts[k] + '</span>').join('') + '</div>' : '') +
        '<div id="customers-timeline" class="customers-block">' + customersTimelineHtml() + '</div>' +
        (hints.length ? '<div class="customers-list">' + hints.map(customersHintRowHtml).join('') + '</div>' : '') + '</div>';
    }

    // customersAttachHtml lets the owner and read-write users pull a suggested
    // person into the open profile: the same action as "Add to existing" on a
    // suggestion, from the other side.
    function customersAttachHtml(p) {
      if (!customersIsFull()) return '';
      const list = (_customersData && _customersData.suggestions) || [];
      let body;
      if (!list.length) body = '<p class="empty-dashed">Nobody is suggested right now.</p>';
      else if (!_customersUi.profilePicker) body = '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersProfilePickerOpen">Add a suggested person here</button></div>';
      else body = '<div class="customers-picker" data-customers-picker="profile">' +
        '<label class="customers-picker-filter">Find a suggested person<input class="kb-search-box" type="text" maxlength="120" data-customers-field="picker-filter" autocomplete="off"></label>' +
        '<div class="customers-picker-list">' + list.filter(s => s && s.id).map(s => '<button type="button" class="hv-btn btn-secondary btn-sm customers-pick" data-action="customersAttach" data-arg0="' + escapeHtml(s.id) + '" data-arg1="' + escapeHtml(p.me ? 'me' : p.id) + '" data-name="' + escapeHtml(String(s.value || '').toLowerCase()) + '" title="' + escapeHtml((CUSTOMERS_KIND_LABELS[s.kind] || s.kind || '') + ': ' + (s.value || '')) + '">' + escapeHtml((CUSTOMERS_KIND_ICONS[s.kind] || '') + ' ' + (s.value || '')) + '</button>').join('') + '</div>' +
        '<div class="customers-picker-foot"><span class="muted-xs">Their identity joins this customer. You can undo it under Merge.</span>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersPickerClose">Cancel</button></div></div>';
      return '<div class="customers-block"><div class="section-label m-0">Suggested people</div>' +
        '<p class="muted-xs">Someone hive saw who is really this customer, under another login or address.</p>' + body + '</div>';
    }

    // customersMergeHtml: fold another customer into this one, and undo an
    // earlier merge. Full view only. Two dashboard users cannot be merged; the
    // server refuses it.
    function customersMergeHtml(p) {
      if (!customersIsFull()) return '';
      const others = ((_customersData && _customersData.people) || []).filter(o => o && o.id && o.id !== p.id);
      const merged = (p.merged_from || []).filter(m => m && m.person && m.person.id);
      const options = others.map(o => '<option value="' + escapeHtml(o.id) + '">' + escapeHtml(o.name || o.id) + '</option>').join('');
      return '<div class="customers-block"><div class="section-label m-0">Merge</div>' +
        '<p class="muted-xs">If two customers are the same person, fold one into this profile. Their identities, tags, goals and activity come across, and you can undo it.</p>' +
        (others.length ? '<div class="customers-inline"><label>Customer to fold in<select class="kb-search-box" data-customers-field="merge-from">' + options + '</select></label>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersMerge">Merge into this profile</button></div>' : '<p class="empty-dashed">No other customers to merge.</p>') +
        (merged.length ? '<div class="muted-xs">Merged into this profile</div><div class="customers-list">' + merged.map(m =>
          '<div class="customers-row"><div class="customers-row-main"><div>' + escapeHtml(m.person.name || m.person.id) + '</div><div class="muted-xs">merged by ' + escapeHtml(m.by || 'someone') + ' ' + escapeHtml(customersFormatTime(m.at)) + '</div></div>' +
          '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersUnmerge" data-arg0="' + escapeHtml(m.person.id) + '" title="Give back its identities, tags, goals and activity">Undo merge</button></div>').join('') + '</div>' : '') + '</div>';
    }

    // The graph view: a drawing of the person, what they touched and who
    // else was on it. Everyone who can open a profile can open its graph; a
    // read-only user's graph holds only themselves.
    function customersGraphBodyHtml(p) {
      const g = _customersGraph;
      if (!g.on) return '';
      if (g.error) return '<p class="kc-error muted-xs">' + escapeHtml(g.error) + '</p>';
      if (g.loading && !g.nodes) return '<p class="muted-xs">Loading the graph…</p>';
      if (!g.nodes || !g.nodes.length) return '<p class="empty-dashed">Nothing to draw yet.</p>';
      return graphcanvasSvg(graphcanvasLayout({ id: p.id, name: p.name }, g.nodes)) +
        '<p class="muted-xs">Green dots are other people on the same things. Click one to open their profile.</p>';
    }

    function customersGraphHtml(p) {
      const g = _customersGraph;
      return '<div class="customers-block"><div class="section-label m-0">Graph</div>' +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" aria-pressed="' + !!g.on + '" data-action="customersGraphToggle">' + (g.on ? 'Hide graph' : 'Show graph') + '</button></div>' +
        '<div id="customers-graph">' + customersGraphBodyHtml(p) + '</div></div>';
    }

    function customersForgetHtml(p) {
      if (!customersIsOwner()) return '';
      return '<div class="customers-block"><div class="section-label m-0">Export and forget</div>' +
        '<p class="muted-xs">Export shows everything this hive holds about this person, in a viewer you can copy from: the profile, the merge history and the references behind their activity.</p>' +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersExport">Export this person</button></div>' +
        '<p class="muted-xs">Deletes this person, their identities and their activity links from this hive, and stops hive rebuilding the profile. It cannot be undone.</p>' +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-danger btn-sm" data-action="customersForget">Forget this person</button></div></div>';
    }

    // The profile opens under its row, so the row already shows the picture,
    // the name and the pills; the profile adds only Rename and its sections.
    function customersProfileHtml(p) {
      return '<div class="customers-profile-head"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersRename">Rename</button></div>' +
        '<div class="hive-dialog-error" role="alert" data-customers-field="profile-error"></div>' +
        customersDetailsHtml(p) + customersGoalsHtml(p) + customersSatisfactionHtml(p) + customersTagsHtml(p) + customersIdentitiesHtml(p) + customersAttachHtml(p) +
        customersActivityHtml(p) + customersGraphHtml(p) + customersMergeHtml(p) + customersNotesHtml(p) + customersForgetHtml(p);
    }

    function renderCustomers() {
      refreshSectionCardShell('customers-section');
      const host = document.getElementById('customers-panel');
      if (!host) return;
      if (_customersError && !_customersData) {
        host.innerHTML = '<p class="kc-error muted-xs">' + escapeHtml(_customersError) + '</p>';
        setSectionSummary('customers-section', 'unavailable');
        return;
      }
      const data = _customersData || {};
      setSectionSummary('customers-section', customersSummaryText(data));
      if (!data.enabled) { host.innerHTML = '<p class="empty-dashed">Customers are turned off on this hive.</p>'; return; }
      // Typing in a form must survive the background refresh.
      const active = document.activeElement;
      if (active && host.contains(active) && (active.tagName === 'INPUT' || active.tagName === 'TEXTAREA' || active.tagName === 'SELECT')) return;
      // A refresh keeps the page where the person was reading.
      const y = window.scrollY;
      host.innerHTML = customersListHtml(data);
      if (Math.abs(window.scrollY - y) > 1) window.scrollTo(window.scrollX, y);
      const pick = host.querySelector('[data-customers-picker] [data-customers-field="picker-filter"]');
      if (pick && _customersUi.focusPicker) { _customersUi.focusPicker = false; pick.focus(); }
      // A toggle re-draws the list; the focus goes back to the chevron it came from.
      if (_customersUi.focusRow) {
        const want = _customersUi.focusRow;
        _customersUi.focusRow = '';
        const chev = Array.prototype.find.call(host.querySelectorAll('.row-chevron'), c => c.getAttribute('data-arg0') === want);
        if (chev) chev.focus();
      }
    }

    // The open state of each disclosure is kept, so a refresh or an action
    // does not close what the person opened. toggle does not bubble, so it is
    // caught on the way down.
    document.addEventListener('toggle', function (ev) {
      const el = ev.target;
      if (!el || !el.matches || !el.matches('details[data-ui-key]') || !el.closest('#customers-panel')) return;
      _customersUi.open[el.getAttribute('data-ui-key')] = !!el.open;
    }, true);

    // The picker's filter narrows its list in place, without a re-render that
    // would take the focus away.
    document.addEventListener('input', function (ev) {
      const el = ev.target;
      if (!el || !el.matches || !el.matches('[data-customers-field="picker-filter"]')) return;
      const picker = el.closest('[data-customers-picker]');
      if (!picker) return;
      const q = (el.value || '').trim().toLowerCase();
      picker.querySelectorAll('.customers-pick').forEach(b => { b.hidden = !!q && (b.getAttribute('data-name') || '').indexOf(q) < 0; });
    });

    // customersSuggestionOrder is the suggestions in the order the list shows
    // them, so the next one can be found after an action.
    function customersSuggestionOrder() {
      return customersSuggestionGroups((_customersData && _customersData.suggestions) || []).reduce((acc, g) => acc.concat(g.items.map(s => s.id)), []);
    }

    // customersNeighbour is the suggestion to put the focus on once sid is
    // gone: the one after it, else the one before.
    function customersNeighbour(sid) {
      const order = customersSuggestionOrder();
      const i = order.indexOf(sid);
      if (i < 0) return '';
      return order[i + 1] || order[i - 1] || '';
    }

    function customersFocusSuggestion(id) {
      const host = document.getElementById('customers-panel');
      if (!host) return;
      const row = id ? host.querySelector('[data-customer-suggestion="' + id + '"] [data-action="customersAccept"]') : null;
      const target = row || host.querySelector('summary.customers-summary');
      if (target && typeof target.focus === 'function') target.focus();
    }

    // customersAfterSuggestion refreshes the data and repaints with every
    // disclosure as it was, then moves the focus to the next suggestion.
    async function customersAfterSuggestion(next, gone) {
      // The row goes at once; the refresh that follows confirms it.
      if (gone && _customersData && _customersData.suggestions) _customersData.suggestions = _customersData.suggestions.filter(x => x && x.id !== gone);
      customersShow();
      await fetchCustomers();
      customersFocusSuggestion(next);
    }

    // customersShow replaces the panel after an action, even when a field in
    // it has the focus.
    function customersShow() {
      if (document.activeElement && typeof document.activeElement.blur === 'function') document.activeElement.blur();
      renderCustomers();
    }

    function customersProfileError(msg) {
      const el = customersField('profile-error');
      if (el) el.textContent = msg || '';
      else if (msg) showToast(msg, 'error');
    }

    // customersTimelineLoad reads one page of the open profile's edges from
    // the customer graph. reset starts over; otherwise it appends.
    async function customersTimelineLoad(reset) {
      const p = _customersOpen;
      if (!p || !customersIsFull()) return;
      if (reset || _customersTimeline.id !== p.id) _customersTimeline = { id: p.id, nodes: [], next: '', error: '', loading: false };
      const t = _customersTimeline;
      if (t.loading) return;
      t.loading = true;
      customersTimelinePaint();
      try {
        const variables = { id: p.id, first: CUSTOMERS_TIMELINE_PAGE };
        if (t.next) variables.after = t.next;
        const data = await customersRequest('POST', '/api/graph', { query: CUSTOMERS_TIMELINE_QUERY, operationName: 'Timeline', variables: variables });
        if (data.errors && data.errors.length) throw new Error(data.errors[0].message || 'query failed');
        const edges = (data.data && data.data.person && data.data.person.edges) || {};
        t.nodes = t.nodes.concat(edges.nodes || []);
        t.next = edges.next || '';
        t.error = '';
      } catch (e) { t.error = 'Could not load activity: ' + e.message; }
      t.loading = false;
      if (_customersTimeline === t) customersTimelinePaint();
    }

    // customersTimelinePaint updates only the timeline, so a field being
    // typed in elsewhere on the profile keeps its text and its focus.
    function customersTimelinePaint() {
      const el = document.getElementById('customers-timeline');
      if (el) el.innerHTML = customersTimelineHtml();
    }

    function customersTimelineMore() { customersTimelineLoad(false); }

    // customersGraphLoad reads the open profile's graph: one query for the
    // edges and, under each, the other people on that artefact.
    async function customersGraphLoad() {
      const p = _customersOpen;
      if (!p) return;
      if (_customersGraph.id !== p.id) _customersGraph = { id: p.id, on: _customersGraph.on, nodes: null, error: '', loading: false };
      const g = _customersGraph;
      g.loading = true;
      customersGraphPaint();
      try {
        const data = await customersRequest('POST', '/api/graph', { query: CUSTOMERS_GRAPH_QUERY, operationName: 'Graph', variables: { id: p.id, first: CUSTOMERS_GRAPH_EDGES } });
        if (data.errors && data.errors.length) throw new Error(data.errors[0].message || 'query failed');
        g.nodes = (data.data && data.data.person && data.data.person.edges && data.data.person.edges.nodes) || [];
        g.error = '';
      } catch (e) { g.error = 'Could not load the graph: ' + e.message; }
      g.loading = false;
      if (_customersGraph === g) customersGraphPaint();
    }

    function customersGraphPaint() {
      const el = document.getElementById('customers-graph');
      if (el && _customersOpen) el.innerHTML = customersGraphBodyHtml(_customersOpen);
    }

    function customersGraphToggle() {
      _customersGraph.on = !_customersGraph.on;
      customersShow();
      if (_customersGraph.on) customersGraphLoad();
    }

    // ── Actions ──
    async function customersOpen(id) {
      _customersUi.profilePicker = false;
      try {
        _customersOpen = await customersRequest('GET', '/api/customers/' + encodeURIComponent(id));
        customersShow();
        customersTimelineLoad(true);
        const item = document.querySelector('#customers-panel [data-customer-item="' + (_customersOpen && _customersOpen.me ? 'me' : id) + '"]');
        if (item && typeof item.scrollIntoView === 'function') item.scrollIntoView({ block: 'nearest' });
      } catch (e) { _customersUi.focusRow = ''; showToast('Could not open the profile: ' + e.message, 'error'); }
    }

    // customersToggle opens the profile under its row, or folds it when it is
    // already open. The chevron and the row both call it.
    function customersToggle(id) {
      const open = !!(_customersOpen && (id === 'me' ? _customersOpen.me : (!_customersOpen.me && _customersOpen.id === id)));
      _customersUi.focusRow = id;
      if (open) customersClose(); else customersOpen(id);
    }

    function customersClose() {
      _customersOpen = null;
      _customersUi.profilePicker = false;
      customersShow();
      fetchCustomers();
    }

    // customersEdit applies one change to the open profile. A viewer's own
    // profile is always addressed as "me": that is the only path a read-only
    // user may write to.
    async function customersEdit(body, done) {
      if (!_customersOpen) return;
      const id = _customersOpen.me ? 'me' : _customersOpen.id;
      try {
        const p = await customersRequest('POST', '/api/customers/' + encodeURIComponent(id) + '/edit', body);
        _customersOpen = p;
        customersShow();
        if (p.hinted) showToast('That identity already belongs to another customer. Nothing was linked; it is now a "Same person?" question on both profiles.', 'warning');
        else if (done) showToast(done, 'success');
        return true;
      } catch (e) { customersProfileError(e.message); return false; }
    }

    async function customersAdd() {
      const err = customersField('add-error');
      const name = customersFieldValue('add-name');
      const value = customersFieldValue('add-value');
      if (!name) { if (err) err.textContent = 'A name is required.'; return; }
      const body = { name: name };
      if (value) body.identities = [{ kind: customersFieldValue('add-kind'), value: value }];
      try {
        const p = await customersRequest('POST', '/api/customers', body);
        showToast('Customer added', 'success');
        _customersOpen = p;
        customersShow();
        fetchCustomers();
      } catch (e) { if (err) err.textContent = e.message; }
    }

    async function customersAccept(sid) {
      const next = customersNeighbour(sid);
      try {
        const p = await customersRequest('POST', '/api/customers/suggestions/' + encodeURIComponent(sid) + '/accept');
        showToast('Accepted: ' + (p.name || 'customer') + ' now has a profile', 'success');
      } catch (e) { showToast('Could not accept: ' + e.message, 'error'); }
      if (_customersUi.picker === sid) _customersUi.picker = '';
      await customersAfterSuggestion(next, sid);
    }

    async function customersDismiss(sid) {
      const ok = await hiveConfirm({ title: 'Dismiss this suggestion?', message: 'Hive will not suggest this person again and keeps no readable trace of them. You can still add them by hand later.', confirmLabel: 'Dismiss' });
      if (!ok) return;
      const next = customersNeighbour(sid);
      try { await customersRequest('POST', '/api/customers/suggestions/' + encodeURIComponent(sid) + '/dismiss'); }
      catch (e) { showToast('Could not dismiss: ' + e.message, 'error'); }
      if (_customersUi.picker === sid) _customersUi.picker = '';
      await customersAfterSuggestion(next, sid);
    }

    // customersShowMore expands or folds one source group of the suggestions.
    function customersShowMore(group) {
      _customersUi.expanded[group] = !_customersUi.expanded[group];
      renderCustomers();
    }

    function customersPickerOpen(sid) {
      _customersUi.picker = sid;
      _customersUi.focusPicker = true;
      customersShow();
    }

    function customersProfilePickerOpen() {
      _customersUi.profilePicker = true;
      _customersUi.focusPicker = true;
      customersShow();
    }

    function customersPickerClose() {
      _customersUi.picker = '';
      _customersUi.profilePicker = false;
      customersShow();
    }

    // customersAttach adds a suggested person's identity to an existing
    // customer (an id, or "me") instead of accepting them as a new one. The
    // server folds it in with the merge, so Undo merge on that customer's
    // profile takes it back.
    // customersIsOpenTarget: is this customer (an id, or "me") the profile
    // that is open? A suggestion's picker can name any customer now that the
    // list and an open profile are on the page together.
    function customersIsOpenTarget(into) {
      return !!(_customersOpen && (into === 'me' ? _customersOpen.me : (!_customersOpen.me && _customersOpen.id === into)));
    }

    async function customersAttach(sid, into) {
      const next = customersNeighbour(sid);
      try {
        const p = await customersRequest('POST', '/api/customers/suggestions/' + encodeURIComponent(sid) + '/accept', { into: into });
        showToast('Added to ' + (p.name || 'the customer') + '. Undo it from their profile, under Merge.', 'success');
        if (customersIsOpenTarget(into)) {
          _customersOpen = p;
          _customersTimeline = { id: '', nodes: [], next: '', error: '', loading: false };
          customersTimelineLoad(true);
        }
      } catch (e) {
        if (customersIsOpenTarget(into)) customersProfileError(e.message); else showToast('Could not add: ' + e.message, 'error');
      }
      _customersUi.picker = '';
      _customersUi.profilePicker = false;
      await customersAfterSuggestion(next, sid);
    }

    async function customersRename() {
      if (!_customersOpen) return;
      const name = await hivePrompt({ title: 'Rename', label: 'Name', defaultValue: _customersOpen.name || '', confirmLabel: 'Save' });
      if (!name || name === _customersOpen.name) return;
      customersEdit({ op: 'name', name: name });
    }

    function customersIdentityAdd() {
      const value = customersFieldValue('identity-value');
      if (!value) { customersProfileError('Enter a login, an address or an id.'); return; }
      customersEdit({ op: 'identity_add', kind: customersFieldValue('identity-kind'), value: value });
    }

    function customersIdentityConfirm(kind, value) {
      customersEdit({ op: 'identity_add', kind: kind, value: value }, 'Identity confirmed');
    }

    function customersIdentityRemove(key) { customersEdit({ op: 'identity_remove', key: key }); }

    function customersTagSet(tag, state) { customersEdit({ op: 'tag', tag: tag, state: state }); }

    function customersTagAdd() {
      const tag = customersFieldValue('tag-name');
      if (!tag) { customersProfileError('Enter a tag.'); return; }
      customersEdit({ op: 'tag', tag: tag, state: 'on' });
    }

    function customersGoalAdd() {
      const text = customersFieldValue('goal-text');
      if (!text) { customersProfileError('Write the goal first.'); return; }
      customersEdit({ op: 'goal_add', text: text });
    }

    async function customersGoalEdit(gid) {
      const goal = ((_customersOpen && _customersOpen.goals) || []).find(g => g && g.id === gid);
      if (!goal) return;
      const text = await hivePrompt({ title: 'Edit goal', label: 'What are they trying to get done?', defaultValue: goal.text || '', confirmLabel: 'Save' });
      if (!text || text === goal.text) return;
      customersEdit({ op: 'goal_edit', goal_id: gid, text: text });
    }

    function customersGoalConfirm(gid) { customersEdit({ op: 'goal_confirm', goal_id: gid }, 'Goal confirmed'); }

    function customersGoalReject(gid) { customersEdit({ op: 'goal_reject', goal_id: gid }); }

    function customersSatisfactionSet(level) { customersEdit({ op: 'satisfaction_set', level: level }); }

    function customersSatisfactionConfirm() { customersEdit({ op: 'satisfaction_confirm' }, 'Satisfaction confirmed'); }

    function customersSatisfactionReject() { customersEdit({ op: 'satisfaction_reject' }); }

    // customersProfileSave saves the details that changed, one edit each.
    async function customersProfileSave() {
      if (!_customersOpen) return;
      let saved = 0;
      for (const f of CUSTOMERS_PROFILE_FIELDS) {
        if (!_customersOpen) return;
        const next = customersFieldValue('profile-' + f.key);
        if (next === customersProfileValue(_customersOpen, f.key)) continue;
        if (!(await customersEdit({ op: 'profile', field: f.key, value: next }))) return;
        saved++;
      }
      if (saved) showToast('Details saved', 'success');
    }

    function customersProfileReset(key) { customersEdit({ op: 'profile', field: key, state: 'auto' }, 'Handed back to GitHub'); }

    function customersAffiliationAccept(value) { customersEdit({ op: 'profile', field: 'organization', value: value }, 'Organization set'); }

    function customersAffiliationDismiss(value) { customersEdit({ op: 'affiliation_dismiss', value: value }, 'Suggestion dismissed'); }

    function customersNotesSave() { customersEdit({ op: 'notes', notes: (customersField('notes') || {}).value || '' }, 'Notes saved'); }

    // customersHintAnswer answers a "Same person?" question about the open
    // profile. Only the owner and read-write users see hints.
    async function customersHintAnswer(hid, answer) {
      const hints = ((_customersOpen && _customersOpen.hints) || []).map(h => h && h.id).filter(Boolean);
      const at = hints.indexOf(hid);
      const nextHint = hints[at + 1] || hints[at - 1] || '';
      try {
        _customersOpen = await customersRequest('POST', '/api/customers/hints/' + encodeURIComponent(hid) + '/answer', { same: answer === 'yes' });
        customersShow();
        showToast(answer === 'yes' ? 'Identity added to this profile' : 'Noted: not the same person', 'success');
        customersTimelineLoad(true);
        await fetchCustomers();
        // The focus moves on to the next question, else back to the profile.
        const host = document.getElementById('customers-panel');
        const target = host && ((nextHint && host.querySelector('[data-hint="' + nextHint + '"] button')) || host.querySelector('.row-chevron[aria-expanded="true"]'));
        if (target) target.focus();
      } catch (e) { customersProfileError(e.message); }
    }

    // customersMerge folds the chosen customer into the open profile.
    async function customersMerge() {
      if (!_customersOpen) return;
      const from = customersFieldValue('merge-from');
      if (!from) { customersProfileError('Choose a customer to fold in.'); return; }
      const other = ((_customersData && _customersData.people) || []).find(o => o && o.id === from);
      const ok = await hiveConfirm({
        title: 'Merge ' + ((other && other.name) || 'this customer') + ' into ' + (_customersOpen.name || 'this profile') + '?',
        message: 'Their identities, tags, goals and activity move to this profile and the other profile is removed. You can undo it from this profile afterwards.',
        confirmLabel: 'Merge'
      });
      if (!ok) return;
      customersMergeRequest('merge', from, 'Merged');
    }

    function customersUnmerge(from) { customersMergeRequest('unmerge', from, 'Merge undone'); }

    async function customersMergeRequest(kind, from, done) {
      if (!_customersOpen) return;
      const id = _customersOpen.me ? 'me' : _customersOpen.id;
      try {
        _customersOpen = await customersRequest('POST', '/api/customers/' + encodeURIComponent(id) + '/' + kind, { from: from });
        _customersTimeline = { id: '', nodes: [], next: '', error: '', loading: false };
        customersShow();
        showToast(done, 'success');
        customersTimelineLoad(true);
        if (_customersGraph.on) customersGraphLoad();
        fetchCustomers();
      } catch (e) { customersProfileError(e.message); }
    }

    // customersSplit moves one identity, and the activity it produced, to a
    // new customer. The open profile stays open.
    async function customersSplit(key) {
      if (!_customersOpen) return;
      const value = String(key).slice(String(key).indexOf(':') + 1);
      const name = await hivePrompt({ title: 'Split off ' + value, label: 'Name for the new customer', defaultValue: value, confirmLabel: 'Split off' });
      if (!name) return;
      const id = _customersOpen.me ? 'me' : _customersOpen.id;
      try {
        const fresh = await customersRequest('POST', '/api/customers/' + encodeURIComponent(id) + '/split', { identities: [key], name: name });
        showToast('Split off as ' + (fresh.name || 'a new customer'), 'success');
        await customersOpen(id);
        fetchCustomers();
      } catch (e) { customersProfileError(e.message); }
    }

    // customersExport shows everything held about the open person in the
    // in-app export viewer, which has copy and save affordances. The page
    // never forces a browser download.
    async function customersExport() {
      if (!_customersOpen) return;
      const path = '/api/customers/' + encodeURIComponent(_customersOpen.me ? 'me' : _customersOpen.id) + '/export';
      try {
        const r = await fetch(path);
        if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || ('HTTP ' + r.status)); }
        hiveExportViewer({ title: 'Customer export: ' + (_customersOpen.name || ''), text: await r.text(), mime: 'application/json', sourceURL: path });
      } catch (e) { customersProfileError('Could not export: ' + e.message); }
    }

    async function customersForget() {
      if (!_customersOpen) return;
      const p = _customersOpen;
      const ok = await hiveConfirm({
        title: 'Forget ' + (p.name || 'this person') + '?',
        message: 'This deletes their profile, identities and activity links from this hive, and stops hive rebuilding the profile from what it sees later. It cannot be undone.',
        confirmLabel: 'Forget', danger: true
      });
      if (!ok) return;
      try {
        await customersRequest('DELETE', '/api/customers/' + encodeURIComponent(p.id));
        showToast('Forgotten', 'success');
        _customersOpen = null;
      } catch (e) { showToast('Could not forget: ' + e.message, 'error'); }
      customersShow();
      fetchCustomers();
    }
