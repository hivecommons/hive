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
    const CUSTOMERS_EVIDENCE_SHOWN = 5;
    let _customersData = null;
    let _customersError = '';
    // The open profile; null shows the list.
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

    function customersEvidenceHtml(refs) {
      const list = (refs || []).slice(0, CUSTOMERS_EVIDENCE_SHOWN);
      if (!list.length) return '';
      return '<div class="customers-evidence" title="What led hive to suggest this">from ' + list.map(escapeHtml).join(', ') + '</div>';
    }

    // ── List ──
    function customersRowHtml(p, isMe) {
      const goals = customersGoalsSplit(p);
      const tags = customersActiveTags(p).map(t => '<span class="badge customers-chip">' + escapeHtml(t) + '</span>').join('');
      const pending = goals.suggested.length + (p.satisfaction && p.satisfaction.suggestion ? 1 : 0);
      const meta = [CUSTOMERS_ORIGIN_LABELS[p.origin] || p.origin || '', goals.confirmed.length + ' goal' + (goals.confirmed.length === 1 ? '' : 's')];
      if (pending) meta.push(pending + ' suggestion' + (pending === 1 ? '' : 's') + ' to review');
      const updated = customersFormatTime(p.updated_at);
      if (updated) meta.push('updated ' + updated);
      return '<div class="customers-row" data-customer="' + escapeHtml(p.id) + '" data-me="' + (isMe ? 'true' : 'false') + '">' +
        '<div class="customers-row-main"><div class="customers-row-title">' + escapeHtml(p.name || p.id) + (isMe ? ' <span class="badge">me</span>' : '') + '</div>' +
        '<div class="muted-xs">' + escapeHtml(meta.filter(Boolean).join(' · ')) + '</div></div>' +
        '<div class="customers-chips">' + tags + customersLevelPill(p.satisfaction && p.satisfaction.level) + '</div>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersOpen" data-arg0="' + (isMe ? 'me' : escapeHtml(p.id)) + '">' + (isMe ? 'My profile' : 'Open') + '</button></div>';
    }

    function customersSuggestionRowHtml(s) {
      const id = escapeHtml(s.id);
      const counts = Object.keys(s.counts || {}).sort().map(k => s.counts[k] + ' ' + k.replace(/_/g, ' ')).join(', ');
      const seen = customersFormatTime(s.last_seen);
      return '<div class="customers-row" data-customer-suggestion="' + id + '">' +
        '<span class="badge">' + escapeHtml(CUSTOMERS_KIND_LABELS[s.kind] || s.kind || '') + '</span>' +
        '<div class="customers-row-main"><div class="customers-row-title">' + escapeHtml(s.value || '') + '</div>' +
        '<div class="muted-xs">' + escapeHtml([counts, seen ? 'last seen ' + seen : ''].filter(Boolean).join(' · ')) + '</div>' +
        customersEvidenceHtml(s.evidence) + '</div>' +
        '<button type="button" class="hv-btn btn-primary btn-sm" data-action="customersAccept" data-arg0="' + id + '" title="Make this person a customer and build their profile">Accept</button>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersDismiss" data-arg0="' + id + '" title="Do not suggest this person again; hive keeps no readable trace of them">Dismiss</button></div>';
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
      out += '<details class="customers-block"><summary class="muted-xs">Suggested (' + suggestions.length + ')</summary>' +
        '<p class="muted-xs mt-2">People hive has seen in issues, pull requests, meetings and mail who are not customers. Hive keeps only a key, counts and a few references for them until you accept one.</p>' +
        '<div class="customers-list mt-2">' + (suggestions.length ? suggestions.map(customersSuggestionRowHtml).join('') : '<p class="empty-dashed">Nobody to suggest.</p>') + '</div></details>';
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
      return '<div class="customers-row" data-claimed="' + (i.claimed ? 'true' : 'false') + '"><span class="badge">' + escapeHtml(CUSTOMERS_KIND_LABELS[i.kind] || i.kind) + '</span>' +
        '<div class="customers-row-main"><div class="customers-row-title">' + escapeHtml(i.value) + '</div>' +
        (meta.length ? '<div class="muted-xs">' + escapeHtml(meta.join(' · ')) + '</div>' : '') + '</div>' + buttons + '</div>';
    }

    function customersIdentitiesHtml(p) {
      const ids = p.identities || [];
      const note = customersIsFull() ? '' : '<p class="muted-xs">What you add here is shown as unconfirmed until the hive owner or a read-write user confirms it.</p>';
      return '<div class="customers-block"><div class="section-label m-0">Identities</div>' + note +
        '<div class="customers-list">' + (ids.length ? ids.map(i => customersIdentityRowHtml(p, i)).join('') : '<p class="empty-dashed">No identities.</p>') + '</div>' +
        '<div class="customers-inline"><label>Known by<select class="kb-search-box" data-customers-field="identity-kind">' + customersKindOptions() + '</select></label>' +
        '<label>Login, address or id<input class="kb-search-box" type="text" maxlength="254" data-customers-field="identity-value" autocomplete="off"></label>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersIdentityAdd">Add identity</button></div></div>';
    }

    function customersTagChipHtml(name, tag) {
      const state = tag ? tag.state : 'off';
      const source = tag ? tag.source : 'auto';
      const on = state === 'on';
      const why = !tag ? 'Not set' : (source === 'manual' ? 'Set by ' + (tag.by || 'a person') : (tag.rule ? 'Automatic: ' + tag.rule : 'Automatic'));
      let out = '<button type="button" class="badge customers-chip" data-state="' + state + '" data-source="' + escapeHtml(source) + '" aria-pressed="' + on + '" title="' + escapeHtml(why) + '"' +
        ' data-action="customersTagSet" data-arg0="' + escapeHtml(name) + '" data-arg1="' + (on ? 'off' : 'on') + '">' + escapeHtml(name) + (tag ? ' · ' + (source === 'manual' ? 'manual' : 'auto') : '') + '</button>';
      if (tag && source === 'manual') {
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
        '<div class="customers-list">' + (goals.confirmed.length ? goals.confirmed.map(customersGoalRowHtml).join('') : '<p class="empty-dashed">No goals yet.</p>') + '</div>' +
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
        '<p class="muted-xs">Export downloads everything this hive holds about this person: the profile, the merge history and the references behind their activity.</p>' +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersExport">Export this person</button></div>' +
        '<p class="muted-xs">Deletes this person, their identities and their activity links from this hive, and stops hive rebuilding the profile. It cannot be undone.</p>' +
        '<div class="customers-inline"><button type="button" class="hv-btn btn-danger btn-sm" data-action="customersForget">Forget this person</button></div></div>';
    }

    function customersProfileHtml(p) {
      const tags = customersActiveTags(p).map(t => '<span class="badge customers-chip">' + escapeHtml(t) + '</span>').join('');
      return '<div class="customers-profile-head"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersClose">← Customers</button>' +
        '<div class="customers-profile-name">' + escapeHtml(p.name || p.id) + (p.me ? ' <span class="badge">me</span>' : '') + '</div>' +
        '<div class="customers-chips">' + tags + customersLevelPill(p.satisfaction && p.satisfaction.level) + '</div>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="customersRename">Rename</button></div>' +
        '<div class="hive-dialog-error" role="alert" data-customers-field="profile-error"></div>' +
        customersGoalsHtml(p) + customersSatisfactionHtml(p) + customersTagsHtml(p) + customersIdentitiesHtml(p) +
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
      host.innerHTML = _customersOpen ? customersProfileHtml(_customersOpen) : customersListHtml(data);
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
      try {
        _customersOpen = await customersRequest('GET', '/api/customers/' + encodeURIComponent(id));
        customersShow();
        customersTimelineLoad(true);
      } catch (e) { showToast('Could not open the profile: ' + e.message, 'error'); }
    }

    function customersClose() {
      _customersOpen = null;
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
      } catch (e) { customersProfileError(e.message); }
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
      try {
        const p = await customersRequest('POST', '/api/customers/suggestions/' + encodeURIComponent(sid) + '/accept');
        showToast('Accepted: ' + (p.name || 'customer') + ' now has a profile', 'success');
      } catch (e) { showToast('Could not accept: ' + e.message, 'error'); }
      customersShow();
      fetchCustomers();
    }

    async function customersDismiss(sid) {
      const ok = await hiveConfirm({ title: 'Dismiss this suggestion?', message: 'Hive will not suggest this person again and keeps no readable trace of them. You can still add them by hand later.', confirmLabel: 'Dismiss' });
      if (!ok) return;
      try { await customersRequest('POST', '/api/customers/suggestions/' + encodeURIComponent(sid) + '/dismiss'); }
      catch (e) { showToast('Could not dismiss: ' + e.message, 'error'); }
      customersShow();
      fetchCustomers();
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

    function customersNotesSave() { customersEdit({ op: 'notes', notes: (customersField('notes') || {}).value || '' }, 'Notes saved'); }

    // customersHintAnswer answers a "Same person?" question about the open
    // profile. Only the owner and read-write users see hints.
    async function customersHintAnswer(hid, answer) {
      try {
        _customersOpen = await customersRequest('POST', '/api/customers/hints/' + encodeURIComponent(hid) + '/answer', { same: answer === 'yes' });
        customersShow();
        showToast(answer === 'yes' ? 'Identity added to this profile' : 'Noted: not the same person', 'success');
        customersTimelineLoad(true);
        fetchCustomers();
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

    // customersExport downloads everything held about the open person.
    async function customersExport() {
      if (!_customersOpen) return;
      try {
        const r = await fetch('/api/customers/' + encodeURIComponent(_customersOpen.me ? 'me' : _customersOpen.id) + '/export');
        if (!r.ok) { const d = await r.json().catch(() => ({})); throw new Error(d.error || ('HTTP ' + r.status)); }
        const blob = new Blob([await r.text()], { type: 'application/json' });
        const a = document.createElement('a');
        a.href = URL.createObjectURL(blob);
        a.download = hiveFilenameFromDisposition(r.headers.get('Content-Disposition'), 'customer-export.json');
        document.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(() => URL.revokeObjectURL(a.href), 0);
        showToast('Exported', 'success');
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
