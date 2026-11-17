    // ── Settings → Meeting attendee (#18396) ──
    // The persona hive attends meetings as (Betty by default): a preset or a
    // customized persona, its behaviour caps, and the attendee's email
    // address and inviters. GET/PUT /api/meetings/persona, owner or
    // read-write; POST /api/meetings/persona/sample plays a line through the
    // meetings TTS service, and GET /api/meetings/persona/preview plays a
    // preset's introduction when its card is clicked (#18710). The Email section is live once the server reports
    // email.available (email-in, #18395): the address, everyone's calendar
    // emails, the user's own (add, prove, remove) and the owner's switches.
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
    // (ResolvedMeetingsPersona.Intro): the persona never states pronouns,
    // so {pronouns} renders empty and a pronoun parenthetical is dropped.
    function maRenderIntro(f) {
      const leave = (f.leave_phrases || [])[0] || '';
      const subs = { '{name}': f.name, '{display_name}': f.display_name, '{pronouns}': '', '{leave}': leave,
        '{accent}': f.accent, '{voice_type}': f.voice_type, '{language}': f.language };
      return String(f.intro_template || '').replace(/\{(name|display_name|pronouns|leave|accent|voice_type|language)\}/g, m => subs[m] || '')
        .replace(/\s*[(（]\s*(?:(?:she|he|they|xe|ze|it|any|ella|elle|lei|ela|il|er|sie)\s*(?:\/\s*[A-Za-z\u00C0-\u024F]+\s*){0,2})?[)）]/gi, '')
        .replace(/\s+([.,;:!?。，、])/g, '$1');
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
        if ((data.email || {}).available) maLoadMyEmails();
      } catch (e) {
        host.innerHTML = '<p class="kc-error muted-xs">Meeting attendee settings unavailable: ' + escapeHtml(e.message) + '</p>';
      }
    }

    function maSetData(data) {
      const e = data.effective || {};
      const form = { enabled: !!e.enabled, preset: e.preset || '', voice_on_address: !!e.voice_on_address, unprompted: Object.assign({}, e.unprompted || {}) };
      MA_PERSONA_FIELDS.forEach(k => { form[k] = e[k] || ''; });
      MA_LIST_FIELDS.forEach(k => { form[k] = (e[k] || []).slice(); });
      const em = data.email || {};
      form.email = { calendar_invites: !!em.calendar_invites, mail_in_files: !!em.mail_in_files, changed: false };
      _maState = { data: data, form: form, dirty: false, mine: (_maState && _maState.mine) || null };
    }

    function maPreset(id) { return ((_maState && _maState.data.presets) || []).find(p => p.id === id) || null; }

    function maOverrideNote(key) {
      const src = ((_maState && _maState.data.overridden) || {})[key];
      if (src === 'env') return ' <span class="badge ma-env" title="An HIVE_MEETINGS_PERSONA_* variable sets this; edits here have no effect while it is set">env</span>';
      if (src === 'config') return ' <span class="badge" title="Set in hive.yaml; differs from the preset">custom</span>';
      return '';
    }

    // A preset card is two buttons side by side: the card itself (select
    // the preset and play its introduction) and a speaker (play or stop
    // only). Both are real buttons, so Enter and Space work.
    function maPresetCardHtml(p, selected) {
      const vt = (p.voice_type || '').replace(/^./, c => c.toUpperCase());
      const state = maIntroState(p.id);
      const label = escapeHtml(maIntroLabel(p, !!state));
      return '<div class="ma-preset' + (selected ? ' is-selected' : '') + (state ? ' is-' + state : '') + '" data-ma-preset="' + escapeHtml(p.id) + '">' +
        '<button type="button" class="ma-preset-pick" data-action="maPickPreset" data-arg0="' + escapeHtml(p.id) + '" aria-pressed="' + (selected ? 'true' : 'false') + '" title="Select this preset and play its introduction">' +
        '<span class="ma-preset-flag" aria-hidden="true">' + maFlag(p.language) + '</span>' +
        '<span class="ma-preset-name">' + escapeHtml(p.display_name) + '</span>' +
        '<span class="muted-xs">' + escapeHtml(maLanguageLabel(p.language)) + ' · ' + escapeHtml(p.accent) + '</span>' +
        '<span class="muted-xs">' + escapeHtml(vt) + ' voice · <code>' + escapeHtml(p.voice) + '</code></span></button>' +
        '<button type="button" class="ma-preset-play" data-action="maTogglePresetIntro" data-arg0="' + escapeHtml(p.id) + '" data-ma-preset-play aria-label="' + label + '" title="' + label + '">' +
        '<span aria-hidden="true">' + (state ? MA_INTRO_STOP_ICON : MA_INTRO_PLAY_ICON) + '</span></button></div>';
    }

    // ── Preset voice preview (hivecommons/hive#18710) ──
    // Clicking a preset card plays that persona's introduction in its own
    // voice: GET /api/meetings/persona/preview speaks the preset's intro
    // through the meetings TTS service, so the clip is the real voice. The
    // clip only ever starts from a click (never on page load). When it
    // cannot be played the tab says why; it never falls back to the
    // browser's own speech synthesis, which would be a different voice.
    const MA_INTRO_PLAY_ICON = '🔊';
    const MA_INTRO_STOP_ICON = '⏹';
    const MA_INTRO_UNAVAILABLE = 'Voice preview is unavailable: ';
    // The card whose introduction is loading or playing, and the last error.
    let _maIntro = { id: '', state: '', error: '' };
    // The Audio element of the current clip; null when nothing plays.
    let _maIntroAudio = null;

    function maIntroSrc(id) { return '/api/meetings/persona/preview?preset=' + encodeURIComponent(id); }

    // maIntroState is 'loading', 'playing' or '' for a preset's card.
    function maIntroState(id) { return _maIntro.id === id ? _maIntro.state : ''; }

    function maIntroLabel(p, on) { return (on ? 'Stop introduction for ' : 'Play introduction for ') + (p.display_name || p.name || p.id); }

    function maIntroSet(id, state, error) {
      _maIntro = { id: id, state: state, error: error || '' };
      maIntroSync();
    }

    // maIntroSync shows the playing state on the cards in place, so the
    // card that was clicked keeps the keyboard focus.
    function maIntroSync() {
      document.querySelectorAll('[data-ma-preset]').forEach(card => {
        const p = maPreset(card.getAttribute('data-ma-preset'));
        if (!p) return;
        const state = maIntroState(p.id);
        card.classList.toggle('is-playing', state === 'playing');
        card.classList.toggle('is-loading', state === 'loading');
        const btn = card.querySelector('[data-ma-preset-play]');
        if (!btn) return;
        btn.setAttribute('aria-label', maIntroLabel(p, !!state));
        btn.setAttribute('title', maIntroLabel(p, !!state));
        if (btn.firstElementChild) btn.firstElementChild.textContent = state ? MA_INTRO_STOP_ICON : MA_INTRO_PLAY_ICON;
      });
      const status = document.querySelector('[data-ma-intro-status]');
      if (status) status.textContent = _maIntro.error;
    }

    function maStopIntro() {
      const audio = _maIntroAudio;
      _maIntroAudio = null;
      if (audio) { try { audio.pause(); } catch (e) { /* already stopped */ } }
      maIntroSet('', '', '');
    }

    // maPlayIntro stops any clip and plays a preset's introduction. It must
    // run inside the click that asked for it: browsers only allow playback
    // started by the user.
    function maPlayIntro(id) {
      if (!maPreset(id)) return;
      maStopIntro();
      const audio = new Audio(maIntroSrc(id));
      _maIntroAudio = audio;
      const current = () => _maIntroAudio === audio;
      audio.addEventListener('playing', () => { if (current()) maIntroSet(id, 'playing', ''); });
      audio.addEventListener('ended', () => { if (current()) maStopIntro(); });
      audio.addEventListener('error', () => { if (current()) maIntroFailed(id); });
      maIntroSet(id, 'loading', '');
      const started = audio.play();
      if (started && started.catch) started.catch(e => {
        // A clip that cannot load also fires 'error', handled above.
        if (!current() || !e || e.name !== 'NotAllowedError') return;
        _maIntroAudio = null;
        maIntroSet('', '', MA_INTRO_UNAVAILABLE + 'the browser blocked playback. Click the card again.');
      });
    }

    // maIntroFailed asks the server why the clip did not play (the audio
    // element does not expose the error body) and shows the reason.
    async function maIntroFailed(id) {
      _maIntroAudio = null;
      let reason = '';
      try {
        const r = await fetch(maIntroSrc(id));
        if (r.ok) {
          if (r.body && r.body.cancel) r.body.cancel();
          reason = 'the browser could not play the clip';
        } else {
          const data = await r.json().catch(() => ({}));
          reason = data.error || ('HTTP ' + r.status);
        }
      } catch (e) {
        reason = e.message || 'the hive did not answer';
      }
      // Another card was clicked, or this one stopped, while asking.
      if (_maIntroAudio || _maIntro.id !== id) return;
      maIntroSet('', '', MA_INTRO_UNAVAILABLE + reason);
    }

    // maTogglePresetIntro is the speaker button: play, or stop the card
    // that is playing.
    function maTogglePresetIntro(id) {
      if (maIntroState(id)) { maStopIntro(); return; }
      maPlayIntro(id);
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
        '<p class="muted-xs">Presets use voices the self-hosted Kokoro TTS provides. Pick one to hear its introduction, then adjust any field.' + maOverrideNote('preset') + '</p>' +
        '<div class="ma-presets" role="group" aria-label="Persona presets">' + presets + '</div>' +
        '<p class="kc-error muted-xs ma-intro-status" role="alert" data-ma-intro-status>' + escapeHtml(_maIntro.error) + '</p>' +
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

    // ── Research agents (hivecommons/hive#18590): where Betty's deep dives
    // go, how many agents can take one now, and a plain warning when none can.
    function maResearchHtml() {
      const r = (_maState && _maState.data && _maState.data.research) || {};
      const agents = (r.agents || []).join(', ');
      const runs = r.base_agent ? 'Runs on the <code>' + escapeHtml(r.base_agent) + '</code> agent\'s backend' +
        (r.backend ? ' (' + escapeHtml(r.backend) + (r.model ? ', ' + escapeHtml(r.model) : '') + ')' : '') + '.' : '';
      const status = r.wired
        ? '<p class="muted-xs">Agents: ' + escapeHtml(agents || 'none') + ' (' + (r.available || 0) + ' available now). ' + runs + '</p>'
        : '';
      return '<section class="ma-section" aria-labelledby="ma-research-h"><h4 id="ma-research-h">Research</h4>' +
        '<p class="muted-xs">When the persona needs to look something up she says so, sends it to the read-only research agents and reports back in the channel she was asked in. Pool size ' +
        escapeHtml(String(r.pool_size || '')) + ' ' + yamlHint('meetings.research_pool_size') + '</p>' +
        status +
        (r.warning ? '<p class="kc-error muted-xs" role="status" data-ma-research-warning>' + escapeHtml(r.warning) + '</p>' : '') +
        '</section>';
    }

    // ── Action requests (hivecommons/hive#18406): what people may ask the
    // persona to draft in a meeting. Saved on its own through
    // PUT /api/meetings/actions/settings and applied without a restart.
    const MA_ACTION_LABELS = { issue: 'Issues', rfc: 'RFCs', spek: 'Spek sessions', knowledge: 'Knowledge notes' };

    function maActionsHtml() {
      const a = (_maState && _maState.data.actions) || null;
      if (!a) return '';
      const types = (a.all_types || []).map(t => '<label class="inbox-toggle"><input type="checkbox" data-ma-action-type="' + escapeHtml(t) + '"' +
        ((a.types || []).indexOf(t) >= 0 ? ' checked' : '') + '> ' + escapeHtml(MA_ACTION_LABELS[t] || t) + '</label>').join('');
      const repos = (a.repos || []).slice();
      if (a.effective_default_repo && repos.indexOf(a.effective_default_repo) < 0) repos.unshift(a.effective_default_repo);
      const repoOpts = '<option value="">Default (' + escapeHtml(a.effective_default_repo || 'none') + ')</option>' +
        repos.map(r => '<option value="' + escapeHtml(r) + '"' + (r === a.default_repo ? ' selected' : '') + '>' + escapeHtml(r) + '</option>').join('');
      const env = (a.env_overrides || []).length ? '<p class="muted-xs">Set by the environment, which wins over a saved value: ' + escapeHtml(a.env_overrides.join(', ')) + '</p>' : '';
      return '<section class="ma-section" aria-labelledby="ma-actions-h"><h4 id="ma-actions-h">Action requests</h4>' +
        '<p class="muted-xs">People can ask the attendee in a meeting to draft an issue, an RFC, a Spek session or a knowledge note. Every draft waits in Calendar, Drafts from meetings, until the owner or a read-write user approves it.</p>' +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-actions="enabled"' + (a.enabled ? ' checked' : '') + '> Let people ask for drafts</label>' +
        '<div class="meetings-settings-grid">' + types + '</div>' +
        '<label>Default repo<select class="kb-search-box mt-2" data-ma-actions="default_repo">' + repoOpts + '</select></label>' +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-actions="auto_knowledge"' + ((a.auto_approve || {}).knowledge ? ' checked' : '') + '> Save knowledge notes without approval (issues, RFCs and Spek sessions always wait)</label>' +
        env + '<div class="hive-dialog-error" role="alert" data-ma-actions-error></div>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="maSaveActions">Save action requests</button></section>';
    }

    function maActionsBody() {
      const q = sel => document.querySelector(sel);
      const types = Array.from(document.querySelectorAll('[data-ma-action-type]')).filter(el => el.checked).map(el => el.dataset.maActionType);
      return {
        enabled: !!(q('[data-ma-actions="enabled"]') || {}).checked,
        types: types,
        default_repo: (q('[data-ma-actions="default_repo"]') || {}).value || '',
        auto_approve: { knowledge: !!(q('[data-ma-actions="auto_knowledge"]') || {}).checked }
      };
    }

    async function maSaveActions() {
      const errEl = document.querySelector('[data-ma-actions-error]');
      if (errEl) errEl.textContent = '';
      try {
        const r = await fetch('/api/meetings/actions/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(maActionsBody()) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        if (_maState) _maState.data.actions = data;
        maRender();
        showToast('Action requests saved', 'success');
      } catch (e) {
        if (errEl) errEl.textContent = e.message; else showToast('Save failed: ' + e.message, 'error');
      }
    }

    function maEmailHtml() {
      const em = (_maState.data.email) || {};
      const on = !!em.available;
      const dis = on ? '' : ' disabled';
      const fe = _maState.form.email || {};
      const toggleDis = on && viewerIsOwner() ? '' : ' disabled';
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
        maMyEmailsHtml(on) +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-email="calendar_invites" data-change-action="maEmailToggle" data-arg0="calendar_invites" data-arg-types="s,c"' + (fe.calendar_invites ? ' checked' : '') + toggleDis + '> Accept calendar invites ' + yamlHint('meetings.email.invites_enabled') + '</label>' +
        '<label class="inbox-toggle"><input type="checkbox" data-ma-email="mail_in_files" data-change-action="maEmailToggle" data-arg0="mail_in_files" data-arg-types="s,c"' + (fe.mail_in_files ? ' checked' : '') + toggleDis + '> Accept mailed-in files (Markdown, audio, images) into the Inbox ' + yamlHint('meetings.email.files_enabled') + '</label>' +
        (on && !viewerIsOwner() ? '<p class="muted-xs">Only the owner changes these.</p>' : '') + '</section>';
    }

    function maRender() {
      const host = document.getElementById('meeting-attendee-inline');
      if (!host || !_maState) return;
      const d = _maState.data;
      const f = _maState.form;
      host.innerHTML =
        (d.restart_required ? '<p class="cmp-banner" role="status">Saved. Restart the hive to apply the new persona.</p>' : '') +
        (d.running ? '' : '<p class="muted-xs">Meetings are not running on this hive; the persona applies once meetings are on (Settings, Meetings).</p>') +
        maPersonaHtml(f) + maBehaviourHtml(f) + maResearchHtml() + (typeof maModelHtml === 'function' ? maModelHtml() : '') + (typeof maActionsHtml === 'function' ? maActionsHtml() : '') + maEmailHtml() +
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

    // maPickPreset fills the form from a preset (Save stores it) and plays
    // the preset's introduction; picking the card that is playing stops it.
    function maPickPreset(id) {
      const p = maPreset(id);
      if (!p || !_maState) return;
      const wasOn = !!maIntroState(id);
      const f = _maState.form;
      f.preset = p.id;
      ['name', 'display_name', 'pronouns', 'language', 'voice', 'voice_type', 'accent', 'humor'].forEach(k => { f[k] = p[k] || ''; });
      f.intro_template = f.humor === 'off' && p.plain_intro_template ? p.plain_intro_template : p.intro_template;
      f.aliases = (p.aliases || []).slice();
      f.leave_phrases = (p.leave_phrases || []).slice();
      _maState.dirty = true;
      maRender();
      // The render replaced the card: give the keyboard focus back to it.
      document.querySelectorAll('[data-ma-preset]').forEach(card => {
        if (card.getAttribute('data-ma-preset') !== id) return;
        const pick = card.querySelector('.ma-preset-pick');
        if (pick && pick.focus) pick.focus();
      });
      if (wasOn) maStopIntro(); else maPlayIntro(id);
    }

    function maRequestBody() {
      const f = _maState.form;
      const body = { enabled: f.enabled, preset: f.preset, voice_on_address: f.voice_on_address, unprompted: Object.assign({}, f.unprompted) };
      MA_PERSONA_FIELDS.forEach(k => { body[k] = f[k]; });
      MA_LIST_FIELDS.forEach(k => { body[k] = f[k]; });
      if (f.email && f.email.changed) body.email = { calendar_invites: f.email.calendar_invites, mail_in_files: f.email.mail_in_files };
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

    function maEmailToggle(key, checked) {
      if (!_maState || !_maState.form.email) return;
      _maState.form.email[key] = !!checked;
      _maState.form.email.changed = true;
      _maState.dirty = true;
    }

    // ── My calendar emails (#18395): the addresses the signed-in user's
    // calendar sends invites from, proven by mailing a code to hive@<host>.
    // GET/POST/DELETE /api/meetings/calendar-emails.
    function maMyEmailsHtml(on) {
      if (!on) return '';
      const mine = _maState.mine;
      let rows = '';
      if (mine && mine.error) rows = '<p class="kc-error muted-xs">' + escapeHtml(mine.error) + '</p>';
      else if (!mine) rows = '<p class="muted-xs">Loading…</p>';
      else if (!(mine.emails || []).length) rows = '<p class="muted-xs">You have not added a calendar email yet.</p>';
      else rows = '<ul class="ma-my-emails">' + mine.emails.map(e => {
        const state = e.proven ? '<span class="badge ma-verified">verified</span>' : '<span class="badge">pending</span>';
        const how = !e.proven && e.code
          ? '<div class="muted-xs">Send an email from ' + escapeHtml(e.email) + ' to <code>' + escapeHtml(mine.address || '') + '</code> with <code>' + escapeHtml(e.code) + '</code> in the subject.</div>'
          : (!e.proven ? '<div class="muted-xs">The code expired: add the address again for a new one.</div>' : '');
        const err = maProofFailureHtml(e);
        const inactive = e.active === false ? ' <span class="muted-xs">(no access to this hive: not on the allowlist)</span>' : '';
        return '<li><span class="ma-email">' + escapeHtml(e.email) + ' ' + state + '</span>' + inactive +
          ' <button type="button" class="hv-btn btn-secondary btn-sm" data-action="maRemoveMyEmail" data-arg0="' + escapeHtml(e.email) + '"' + (on ? '' : ' disabled') + '>Remove</button>' + how + err + '</li>';
      }).join('') + '</ul>';
      return '<div class="section-label m-0">My calendar emails</div>' + rows + maProofAttemptsHtml(mine) +
        '<div class="ma-add-email"><input class="kb-search-box" type="email" data-ma-new-email placeholder="you@example.com" autocomplete="email" aria-label="Calendar email to add"' + (on ? '' : ' disabled') + '>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="maAddMyEmail"' + (on ? '' : ' disabled') + '>Add</button></div>';
    }

    // Why the latest proof mail of a row did not count (#18579): the
    // server's plain-language reason with what to do, the SPF, DKIM and
    // alignment results hive-mx saw, and a Check again button.
    function maProofFailureHtml(e) {
      const f = e.last_failure;
      if (!f) return e.last_error ? '<div class="kc-error muted-xs">' + escapeHtml(e.last_error) + '</div>' : '';
      const when = f.at ? ' <span class="muted-xs">(' + escapeHtml(formatDashboardDateTime(f.at)) + ')</span>' : '';
      return '<div class="kc-error muted-xs" data-ma-proof-failure="' + escapeHtml(f.reason || '') + '">' + escapeHtml(f.message || e.last_error || '') + when + '</div>' +
        '<div class="muted-xs" data-ma-proof-auth>' + escapeHtml(maProofAuthSummary(f)) + '</div>' +
        '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="maLoadMyEmails">Check again</button>';
    }

    function maProofAuthSummary(f) {
      const yes = b => (b ? 'yes' : 'no');
      const spf = 'SPF ' + (f.spf || 'none') + (f.spf_domain ? ' for ' + f.spf_domain : '');
      const dkim = (f.dkim || []).length
        ? 'DKIM ' + f.dkim.map(d => (d.result || 'none') + ' for ' + d.domain).join(', ')
        : 'DKIM none';
      return 'What the mail server saw: ' + spf + '; ' + dkim + '; aligned with ' + (f.from_domain || 'the From domain') + ': ' + yes(f.aligned) + '.';
    }

    // Owner only: proof mails whose code matched no address (a typo), with
    // the sender masked. The server sends them to the owner alone.
    function maProofAttemptsHtml(mine) {
      const list = (mine && mine.recent_proof_attempts) || [];
      if (!list.length) return '';
      return '<div class="section-label m-0">Recent proof attempts</div><ul class="ma-proof-attempts">' +
        list.map(a => '<li>' + escapeHtml(a.from || 'unknown sender') + ' <span class="muted-xs">' + escapeHtml(a.at ? formatDashboardDateTime(a.at) : '') + '</span>: ' + escapeHtml(a.reason || '') + '</li>').join('') + '</ul>';
    }

    async function maLoadMyEmails() {
      if (!_maState) return;
      try {
        const r = await fetch('/api/meetings/calendar-emails');
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        _maState.mine = data;
      } catch (e) {
        _maState.mine = { error: 'Calendar emails unavailable: ' + e.message };
      }
      maRender();
    }

    async function maAddMyEmail() {
      const input = document.querySelector('[data-ma-new-email]');
      const email = input ? input.value.trim() : '';
      if (!email) return;
      try {
        const r = await fetch('/api/meetings/calendar-emails', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ email }) });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
        if (data.instruction) showToast(data.instruction, 'info');
      } catch (e) {
        showToast('Could not add the calendar email: ' + e.message, 'error');
      }
      await maLoadMyEmails();
    }

    async function maRemoveMyEmail(email) {
      try {
        const r = await fetch('/api/meetings/calendar-emails?email=' + encodeURIComponent(email), { method: 'DELETE' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok) throw new Error(data.error || ('HTTP ' + r.status));
      } catch (e) {
        showToast('Could not remove the calendar email: ' + e.message, 'error');
      }
      await maLoadMyEmails();
    }

    async function maCopyAddress() {
      const addr = (((_maState || {}).data || {}).email || {}).address || '';
      if (!addr) return;
      try { await navigator.clipboard.writeText(addr); showToast('Copied ' + addr, 'success'); }
      catch (e) { showToast('Copy failed: ' + e.message, 'error'); }
    }
