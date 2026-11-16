    // ── Settings → Meeting attendee (#18396) ──
    // The persona hive attends meetings as (Betty by default): a preset or a
    // customized persona, its behaviour caps, and the attendee's email
    // address and inviters. GET/PUT /api/meetings/persona, owner or
    // read-write; POST /api/meetings/persona/sample plays a line through the
    // meetings TTS service. The Email section stays disabled (layout only)
    // until the server reports email.available (email-in, #18395).
    let _maState = null; // { data, form }
    // Language tags the presets and Kokoro voices speak, for labels.
    const MA_LANGUAGE_LABELS = {
      'en-US': 'English (US)', 'en-GB': 'English (UK)', 'es-ES': 'Spanish', 'fr-FR': 'French', 'it-IT': 'Italian',
      'pt-BR': 'Portuguese (Brazil)', 'hi-IN': 'Hindi', 'ja-JP': 'Japanese', 'zh-CN': 'Mandarin Chinese',
    };
    // Regional indicator symbols start here; 'A' maps to U+1F1E6.
    const MA_FLAG_BASE = 0x1F1E6;
    const MA_PERSONA_FIELDS = ['name', 'display_name', 'pronouns', 'language', 'voice', 'voice_type', 'accent', 'humor', 'intro_template'];
    const MA_LIST_FIELDS = ['aliases', 'leave_phrases'];
    const MA_CAP_FIELDS = [
      ['per_10m', 'Unprompted answers per 10 minutes', '1'],
      ['min_gap_s', 'Seconds between unprompted answers', '1'],
      ['per_meeting', 'Unprompted answers per meeting', '1'],
      ['answer_wait_s', 'Answer wait (seconds a person has to answer first)', '1'],
      ['min_confidence', 'Confidence needed to answer unprompted (0 to 1)', '0.05'],
      ['queue_depth', 'Room questions waiting at once', '1'],
      ['stale_s', 'Drop a question after (seconds)', '1'],
    ];

    function maLanguageLabel(tag) { return MA_LANGUAGE_LABELS[tag] || tag || ''; }

    function maFlag(tag) {
      const region = String(tag || '').split('-')[1] || '';
      if (!/^[A-Za-z]{2}$/.test(region)) return '';
      const up = region.toUpperCase();
      return String.fromCodePoint(MA_FLAG_BASE + up.charCodeAt(0) - 65, MA_FLAG_BASE + up.charCodeAt(1) - 65);
    }

    // maRenderIntro fills the intro placeholders the way the server does
    // (ResolvedMeetingsPersona.Intro).
    function maRenderIntro(f) {
      const leave = (f.leave_phrases || [])[0] || '';
      const subs = { '{name}': f.name, '{display_name}': f.display_name, '{pronouns}': f.pronouns, '{leave}': leave,
        '{accent}': f.accent, '{voice_type}': f.voice_type, '{language}': f.language };
      return String(f.intro_template || '').replace(/\{(name|display_name|pronouns|leave|accent|voice_type|language)\}/g, m => subs[m] || '');
    }

    function renderGovMeetingAttendee() {
      return '<div class="config-section" data-config-section="Meeting attendee"><h3>Meeting attendee</h3>' +
        '<p class="text-muted fs-base">Who hive is in meetings: the persona\'s name, voice, language and manners, how often she speaks up unprompted, and how people invite her. ' +
        yamlHint('meetings.persona') + ' Changes apply after a restart.</p>' +
        '<div id="meeting-attendee-inline"><p class="muted-xs">Loading…</p></div></div>';
    }

    async function loadMeetingAttendeeInline() {
      const host = document.getElementById('meeting-attendee-inline');
      if (!host) return;
      try {
        const r = await fetch('/api/meetings/persona');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        maSetData(data);
        maRender();
      } catch (e) {
        host.innerHTML = '<p class="kc-error muted-xs">Meeting attendee settings unavailable: ' + escapeHtml(e.message) + '</p>';
      }
    }

    function maSetData(data) {
      const e = data.effective || {};
      const form = { enabled: !!e.enabled, preset: e.preset || '', voice_on_address: !!e.voice_on_address, unprompted: Object.assign({}, e.unprompted || {}) };
      MA_PERSONA_FIELDS.forEach(k => { form[k] = e[k] || ''; });
      MA_LIST_FIELDS.forEach(k => { form[k] = (e[k] || []).slice(); });
      _maState = { data: data, form: form, dirty: false };
    }

    function maPreset(id) { return ((_maState && _maState.data.presets) || []).find(p => p.id === id) || null; }

    function maOverrideNote(key) {
      const src = ((_maState && _maState.data.overridden) || {})[key];
      if (src === 'env') return ' <span class="badge ma-env" title="An HIVE_MEETINGS_PERSONA_* variable sets this; edits here have no effect while it is set">env</span>';
      if (src === 'config') return ' <span class="badge" title="Set in hive.yaml; differs from the preset">custom</span>';
      return '';
    }

    function maPresetCardHtml(p, selected) {
      const vt = (p.voice_type || '').replace(/^./, c => c.toUpperCase());
      return '<button type="button" class="ma-preset' + (selected ? ' is-selected' : '') + '" data-action="maPickPreset" data-arg0="' + escapeHtml(p.id) + '" aria-pressed="' + (selected ? 'true' : 'false') + '">' +
        '<span class="ma-preset-flag" aria-hidden="true">' + maFlag(p.language) + '</span>' +
        '<span class="ma-preset-name">' + escapeHtml(p.display_name) + '</span>' +
        '<span class="muted-xs">' + escapeHtml(maLanguageLabel(p.language)) + ' · ' + escapeHtml(p.accent) + '</span>' +
        '<span class="muted-xs">' + escapeHtml(vt) + ' voice · <code>' + escapeHtml(p.voice) + '</code></span></button>';
    }

    function maField(label, key, inner) {
      return '<label class="muted-note inbox-field">' + escapeHtml(label) + maOverrideNote(key) + inner + '</label>';
    }

    function maTextInput(key, value, placeholder) {
      return '<input class="kb-search-box mt-2" type="text" data-ma-field="' + key + '" data-input-action="maInput" data-arg0="' + key + '" data-arg-types="s,v" value="' + escapeHtml(value) + '" placeholder="' + escapeHtml(placeholder || '') + '" autocomplete="off">';
    }

    function maVoiceOptions(f) {
      const voices = ((_maState.data.voices) || []).filter(v => v.language === f.language);
      let opts = voices.map(v => '<option value="' + escapeHtml(v.id) + '"' + (v.id === f.voice ? ' selected' : '') + '>' + escapeHtml(v.id + ' (' + v.voice_type + ', ' + v.accent + ')') + '</option>').join('');
      if (f.voice && !voices.some(v => v.id === f.voice)) opts = '<option value="' + escapeHtml(f.voice) + '" selected>' + escapeHtml(f.voice + ' (custom)') + '</option>' + opts;
      return opts;
    }

    function maLanguageOptions(f) {
      const langs = [];
      ((_maState.data.voices) || []).forEach(v => { if (langs.indexOf(v.language) < 0) langs.push(v.language); });
      if (f.language && langs.indexOf(f.language) < 0) langs.unshift(f.language);
      return langs.map(l => '<option value="' + escapeHtml(l) + '"' + (l === f.language ? ' selected' : '') + '>' + escapeHtml(maFlag(l) + ' ' + maLanguageLabel(l)) + '</option>').join('');
    }

    function maVoiceTypeOptions(f) {
      return ((_maState.data.voice_types) || []).map(t => {
        const sel = t.id === f.voice_type;
        // A type no shipped voice provides is listed but not selectable
        // (bot-style: coming later), unless the config already uses it.
        const disabled = !t.available && !sel;
        return '<option value="' + escapeHtml(t.id) + '"' + (sel ? ' selected' : '') + (disabled ? ' disabled' : '') + '>' +
          escapeHtml(t.label + (t.available ? '' : ' (coming later)')) + '</option>';
      }).join('');
    }

    function maPersonaHtml(f) {
      const presets = (_maState.data.presets || []).map(p => maPresetCardHtml(p, p.id === f.preset)).join('');
      return '<section class="ma-section" aria-labelledby="ma-persona-h"><h4 id="ma-persona-h">Persona</h4>' +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-field="enabled" data-change-action="maInput" data-arg0="enabled" data-arg-types="s,c"' + (f.enabled ? ' checked' : '') + '> Attend meetings as this persona' + maOverrideNote('enabled') + '</label>' +
        '<p class="muted-xs">Presets use voices the self-hosted Kokoro TTS provides. Pick one, then adjust any field.' + maOverrideNote('preset') + '</p>' +
        '<div class="ma-presets" role="group" aria-label="Persona presets">' + presets + '</div>' +
        '<div class="meetings-settings-grid">' +
        maField('Name', 'name', maTextInput('name', f.name, 'Betty')) +
        maField('Display name', 'display_name', maTextInput('display_name', f.display_name, 'Betty Bee from the hive')) +
        maField('Aliases (comma separated)', 'aliases', maTextInput('aliases', f.aliases.join(', '), 'betty, betty bee, hive')) +
        maField('Pronouns', 'pronouns', maTextInput('pronouns', f.pronouns, 'she/her')) +
        maField('Voice type', 'voice_type', '<select class="kb-search-box mt-2" data-ma-field="voice_type" data-change-action="maInput" data-arg0="voice_type" data-arg-types="s,v">' + maVoiceTypeOptions(f) + '</select>') +
        maField('Language', 'language', '<select class="kb-search-box mt-2" data-ma-field="language" data-change-action="maLanguageChanged" data-arg-types="v">' + maLanguageOptions(f) + '</select>') +
        maField('Accent', 'accent', maTextInput('accent', f.accent, 'American')) +
        maField('Voice', 'voice', '<select class="kb-search-box mt-2" data-ma-field="voice" data-change-action="maInput" data-arg0="voice" data-arg-types="s,v">' + maVoiceOptions(f) + '</select>') +
        maField('Humor', 'humor', '<select class="kb-search-box mt-2" data-ma-field="humor" data-change-action="maInput" data-arg0="humor" data-arg-types="s,v">' +
          ['light', 'off'].map(h => '<option value="' + h + '"' + (h === f.humor ? ' selected' : '') + '>' + (h === 'light' ? 'Light' : 'Off') + '</option>').join('') + '</select>') +
        maField('Leave phrases (comma separated)', 'leave_phrases', maTextInput('leave_phrases', f.leave_phrases.join(', '), 'betty leave, hive leave')) +
        '</div>' +
        maField('Intro (placeholders: {name}, {display_name}, {pronouns}, {leave}, {accent}, {voice_type}, {language})', 'intro_template',
          '<textarea class="kb-search-box mt-2 ma-intro" rows="4" data-ma-field="intro_template" data-input-action="maInput" data-arg0="intro_template" data-arg-types="s,v">' + escapeHtml(f.intro_template) + '</textarea>') +
        '<div class="ma-preview"><div class="section-label m-0">Preview</div><p class="ma-preview-text" data-ma-preview>' + escapeHtml(maRenderIntro(f)) + '</p></div>' +
        '<div class="ma-sample"><button type="button" class="hv-btn btn-secondary btn-sm" data-action="maPlaySample"' + (_maState.data.tts_configured ? '' : ' disabled title="Set meetings.tts_url to play samples"') + '>Play sample</button>' +
        '<audio controls class="ma-audio" data-ma-audio hidden></audio>' +
        (_maState.data.tts_configured ? '' : '<span class="muted-xs">TTS is not configured on this hive.</span>') +
        '<span class="muted-xs" role="status" data-ma-sample-status></span></div></section>';
    }

    function maBehaviourHtml(f) {
      const caps = MA_CAP_FIELDS.map(c => maField(c[1], 'unprompted.' + c[0],
        '<input class="kb-search-box mt-2" type="number" min="0" step="' + c[2] + '" data-ma-cap="' + c[0] + '" data-input-action="maCapInput" data-arg0="' + c[0] + '" data-arg-types="s,v" value="' + escapeHtml(String(f.unprompted[c[0]] != null ? f.unprompted[c[0]] : '')) + '">')).join('');
      return '<section class="ma-section" aria-labelledby="ma-behaviour-h"><h4 id="ma-behaviour-h">Behaviour</h4>' +
        '<p class="muted-xs">Unprompted answers are chat replies to room questions nobody asked the persona directly. Addressed requests skip these caps but not the meeting\'s rate limit and budget.</p>' +
        '<div class="meetings-settings-grid">' + caps + '</div>' +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-field="voice_on_address" data-change-action="maInput" data-arg0="voice_on_address" data-arg-types="s,c"' + (f.voice_on_address ? ' checked' : '') + '> Speak aloud only when addressed by name (never unprompted; off sends every reply to chat)</label></section>';
    }

    function maEmailHtml() {
      const em = (_maState.data.email) || {};
      const on = !!em.available;
      const dis = on ? '' : ' disabled';
      const people = (em.inviters || []).map(p => {
        const emails = (p.emails || []).map(x => '<span class="ma-email">' + escapeHtml(x.address) + ' <span class="badge' + (x.proof === 'verified' ? ' ma-verified' : '') + '">' + escapeHtml(x.proof || 'pending') + '</span></span>').join('') ||
          '<span class="muted-xs">no calendar email registered</span>';
        return '<tr><td>' + escapeHtml(p.display_name || p.login) + (p.display_name ? ' <span class="muted-xs">' + escapeHtml(p.login) + '</span>' : '') + '</td><td>' + escapeHtml(p.role) + '</td><td>' + emails + '</td></tr>';
      }).join('');
      const approvals = (em.approvals || []).map(a => '<li>' + escapeHtml((a.kind || '') + ' from ' + (a.from || '')) + (a.subject ? ': ' + escapeHtml(a.subject) : '') + '</li>').join('');
      return '<section class="ma-section' + (on ? '' : ' is-pending') + '" aria-labelledby="ma-email-h"><h4 id="ma-email-h">Email</h4>' +
        (on ? '' : '<p class="muted-xs" data-ma-email-pending>Available once email-in is enabled. Until then, add meetings from the Calendar.</p>') +
        (em.error ? '<p class="kc-error muted-xs">' + escapeHtml(em.error) + '</p>' : '') +
        '<div class="ma-address"><span>Attendee address</span> <code data-ma-address>' + escapeHtml(em.address || '') + '</code>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="maCopyAddress"' + (em.address ? '' : ' disabled') + '>Copy</button></div>' +
        '<p class="muted-xs">Add this address to a calendar invite. Invites from people with access to this hive whose calendar email is verified are scheduled; anything else waits for the owner.</p>' +
        '<table class="meetings-effective ma-inviters"><thead><tr><th>Person</th><th>Access</th><th>Calendar emails</th></tr></thead><tbody>' +
        (people || '<tr><td colspan="3" class="muted-xs">No access list on this hive.</td></tr>') + '</tbody></table>' +
        '<div class="section-label m-0">Waiting for approval</div>' + (approvals ? '<ul class="ma-approvals">' + approvals + '</ul>' : '<p class="muted-xs">Nothing waiting.</p>') +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-email="calendar_invites"' + (em.calendar_invites ? ' checked' : '') + dis + '> Accept calendar invites</label>' +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-email="mail_in_files"' + (em.mail_in_files ? ' checked' : '') + dis + '> Accept mailed-in files (Markdown, audio, images) into the Inbox</label></section>';
    }

    function maRender() {
      const host = document.getElementById('meeting-attendee-inline');
      if (!host || !_maState) return;
      const d = _maState.data;
      const f = _maState.form;
      host.innerHTML =
        (d.restart_required ? '<p class="cmp-banner" role="status">Saved. Restart the hive to apply the new persona.</p>' : '') +
        (d.running ? '' : '<p class="muted-xs">Meetings are not running on this hive; the persona applies once meetings are on (Settings, Meetings).</p>') +
        maPersonaHtml(f) + maBehaviourHtml(f) + maEmailHtml() +
        '<div class="hive-dialog-error" role="alert" data-ma-error></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-primary hive-dialog-btn primary" data-action="maSave">Save</button></div>';
    }

    function maRefreshPreview() {
      const el = document.querySelector('[data-ma-preview]');
      if (el && _maState) el.textContent = maRenderIntro(_maState.form);
    }

    function maInput(key, value) {
      if (!_maState) return;
      const f = _maState.form;
      if (MA_LIST_FIELDS.indexOf(key) >= 0) f[key] = String(value || '').split(',').map(s => s.trim()).filter(Boolean);
      else if (key === 'enabled' || key === 'voice_on_address') f[key] = !!value;
      else f[key] = value;
      _maState.dirty = true;
      maRefreshPreview();
    }

    function maCapInput(key, value) {
      if (!_maState) return;
      const n = Number(value);
      _maState.form.unprompted[key] = value === '' || !isFinite(n) ? 0 : n;
      _maState.dirty = true;
    }

    // maLanguageChanged keeps the voice in the new language: the current
    // voice when it speaks it, else the first of the same voice type.
    function maLanguageChanged(lang) {
      if (!_maState) return;
      const f = _maState.form;
      f.language = lang;
      const voices = (_maState.data.voices || []).filter(v => v.language === lang);
      if (voices.length && !voices.some(v => v.id === f.voice)) {
        f.voice = (voices.find(v => v.voice_type === f.voice_type) || voices[0]).id;
        const known = voices.find(v => v.id === f.voice);
        if (known) f.accent = known.accent;
      }
      _maState.dirty = true;
      maRender();
    }

    // maPickPreset fills the form from a preset; Save stores it.
    function maPickPreset(id) {
      const p = maPreset(id);
      if (!p || !_maState) return;
      const f = _maState.form;
      f.preset = p.id;
      ['name', 'display_name', 'pronouns', 'language', 'voice', 'voice_type', 'accent', 'humor'].forEach(k => { f[k] = p[k] || ''; });
      f.intro_template = f.humor === 'off' && p.plain_intro_template ? p.plain_intro_template : p.intro_template;
      f.aliases = (p.aliases || []).slice();
      f.leave_phrases = (p.leave_phrases || []).slice();
      _maState.dirty = true;
      maRender();
    }

    function maRequestBody() {
      const f = _maState.form;
      const body = { enabled: f.enabled, preset: f.preset, voice_on_address: f.voice_on_address, unprompted: Object.assign({}, f.unprompted) };
      MA_PERSONA_FIELDS.forEach(k => { body[k] = f[k]; });
      MA_LIST_FIELDS.forEach(k => { body[k] = f[k]; });
      return body;
    }

    async function maSave() {
      if (!_maState) return;
      const errEl = document.querySelector('[data-ma-error]');
      if (errEl) errEl.textContent = '';
      try {
        const r = await fetch('/api/meetings/persona', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(maRequestBody()) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        maSetData(data);
        maRender();
        showToast(data.restart_required ? 'Meeting attendee saved; restart the hive to apply' : 'Meeting attendee saved', 'success');
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }

    // maSampleSrc is the same-origin sample URL for the form's unsaved
    // voice, language and name. The dashboard CSP has no media-src, so the
    // <audio> element plays the GET endpoint directly.
    function maSampleSrc(f) {
      const q = new URLSearchParams({ voice: f.voice || '', language: f.language || '', display_name: f.display_name || '' });
      return '/api/meetings/persona/sample?' + q.toString();
    }

    function maPlaySample() {
      if (!_maState) return;
      const status = document.querySelector('[data-ma-sample-status]');
      const audio = document.querySelector('[data-ma-audio]');
      if (!audio) return;
      if (status) status.textContent = 'Generating…';
      audio.hidden = false;
      audio.addEventListener('playing', () => { if (status) status.textContent = ''; }, { once: true });
      audio.addEventListener('error', () => { if (status) status.textContent = 'Sample failed: check that the TTS service is reachable and the voice exists.'; }, { once: true });
      audio.src = maSampleSrc(_maState.form);
      const p = audio.play();
      if (p && p.catch) p.catch(() => {});
    }

    async function maCopyAddress() {
      const addr = (((_maState || {}).data || {}).email || {}).address || '';
      if (!addr) return;
      try { await navigator.clipboard.writeText(addr); showToast('Copied ' + addr, 'success'); }
      catch (e) { showToast('Copy failed: ' + e.message, 'error'); }
    }
