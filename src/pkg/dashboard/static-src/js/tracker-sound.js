    // Pure: the master toggle and a visible tab gate every automatic play.
    function stageTrackerSoundGate() { return !!_stageTrackerSoundOn && !document.hidden; }

    // Pure: item key → true for every issue and PR one payload lists.
    function stageTrackerOpenKeys(d) {
      const out = {};
      if (!d || typeof d !== 'object' || !Array.isArray(d.items)) return out;
      for (const it of d.items) if (it && it.kind && Number(it.number) > 0) out[stageTrackerItemKey(it)] = true;
      return out;
    }

    // Pure: keys listed in next that prev did not list, sorted. No prev, or
    // an empty prev (first populated payload), → none: a page load never
    // buzzes for everything already open.
    function stageTrackerNewlyOpened(prev, next) {
      if (!prev || !next || !Object.keys(prev).length) return [];
      return Object.keys(next).filter(k => !(k in prev)).sort();
    }

    function stageTrackerNoteOpened(d) {
      const next = stageTrackerOpenKeys(d);
      const fresh = stageTrackerNewlyOpened(_stageTrackerOpenState, next).filter(k => !_stageTrackerBuzzed[k]);
      _stageTrackerOpenState = next;
      if (!fresh.length) return;
      fresh.forEach(k => { _stageTrackerBuzzed[k] = true; });
      if (stageTrackerSoundGate()) stageTrackerPlayBuzz(Math.min(fresh.length, STAGE_TRACKER_SOUND_MAX_PLAYS));
    }

    // Creates the context on first use; never resumes it — callers go through
    // stageTrackerResumeContext so a still-pending resume() is never mistaken
    // for a blocked one. Clip decoding is best-effort and cannot abort creation.
    function stageTrackerAudioContext() {
      const AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return null;
      if (!_stageTrackerAudioCtx) {
        try { _stageTrackerAudioCtx = new AC(); } catch { return null; }
        try { stageTrackerDecodeClips(); } catch {}
      }
      return _stageTrackerAudioCtx;
    }

    // Resolves with ctx once it is running. Any other state ('suspended',
    // Safari's 'interrupted', …) gets a resume(); the promise rejects when
    // resume() throws or rejects, or when the context is not running within
    // timeoutMs. A resume() that resolves while the state still reads
    // blocked is not a verdict — WebKit can settle the promise a beat before
    // the state flips — so the context's statechange is awaited until the
    // deadline.
    function stageTrackerResumeContext(ctx, timeoutMs) {
      if (!ctx) return Promise.reject(new Error('no AudioContext'));
      if (ctx.state === 'running') return Promise.resolve(ctx);
      return new Promise((resolve, reject) => {
        let settled = false;
        const canListen = typeof ctx.addEventListener === 'function';
        const onState = () => { if (ctx.state === 'running') finish(true); };
        const finish = (ok, err) => {
          if (settled) return;
          settled = true;
          clearTimeout(timer);
          if (canListen) ctx.removeEventListener('statechange', onState);
          if (ok) resolve(ctx); else reject(err || new Error('AudioContext did not resume'));
        };
        const timer = setTimeout(() => finish(false, new Error('AudioContext resume timed out')), timeoutMs);
        if (canListen) ctx.addEventListener('statechange', onState);
        let p;
        try { p = ctx.resume(); } catch (e) { p = Promise.reject(e); }
        Promise.resolve(p).then(() => { if (ctx.state === 'running') finish(true); }, e => finish(false, e));
      });
    }

    // Drops the context so voices queued on it can never burst out later;
    // the next play creates a fresh one. Decoded clips are plain AudioBuffers
    // and survive.
    function stageTrackerDiscardContext(ctx) {
      if (_stageTrackerAudioCtx === ctx) _stageTrackerAudioCtx = null;
      try { const p = ctx && typeof ctx.close === 'function' && ctx.close(); if (p && typeof p.catch === 'function') p.catch(() => {}); } catch {}
    }

    // Prime the context on a gesture after load when sound was left on in a
    // previous visit, so a merge that lands before the user clicks anything
    // in the tracker can still play (autoplay policy). The listeners stay
    // armed until a resume() actually succeeds — the first gesture may be
    // one the browser does not count. Re-armed whenever an automatic play
    // finds the context still blocked.
    function stageTrackerSoundPrime() {
      if (_stageTrackerSoundPrimed) return;
      _stageTrackerSoundPrimed = true;
      let inFlight = false;
      const disarm = () => {
        _stageTrackerSoundPrimed = false;
        STAGE_TRACKER_PRIME_EVENTS.forEach(n => document.removeEventListener(n, prime));
      };
      const prime = () => {
        if (!_stageTrackerSoundOn) { disarm(); return; }
        if (inFlight) return;
        const ctx = stageTrackerAudioContext();
        if (!ctx) { disarm(); return; }
        inFlight = true;
        stageTrackerResumeContext(ctx, STAGE_TRACKER_GESTURE_RESUME_MS).then(() => { disarm(); stageTrackerSoundHeldClear(); }, () => { inFlight = false; });
      };
      STAGE_TRACKER_PRIME_EVENTS.forEach(n => document.addEventListener(n, prime, { passive: true }));
    }

    // Pure: whether this document has had the user activation the autoplay
    // policies want before an AudioContext may run outside a gesture. A
    // browser without navigator.userActivation is assumed not to have it.
    function stageTrackerHasUserActivation(nav) {
      const ua = nav && nav.userActivation;
      return !!(ua && ua.hasBeenActive);
    }

    // The autoplay-policy affordance: the chip turns amber and, once per
    // page load, a toast with an "Enable sounds" action explains that a
    // click or key press on the page is needed. The action runs inside the
    // click, so it is itself the gesture that resumes the context.
    function stageTrackerSoundHeldNotice(skipped) {
      const b = document.getElementById('activity-monitor-sound');
      if (b) {
        b.classList.add(STAGE_TRACKER_SOUND_HELD_CLASS);
        b.setAttribute('title', STAGE_TRACKER_SOUND_HELD_TITLE);
      }
      stageTrackerSoundPrime();
      if (_stageTrackerSoundHeldShown) return;
      _stageTrackerSoundHeldShown = true;
      showToast(STAGE_TRACKER_SOUND_HELD_MSG + (skipped ? STAGE_TRACKER_SOUND_HELD_SKIPPED_MSG : ''), 'warning', {
        autoDismissMs: STAGE_TRACKER_SOUND_HELD_TOAST_MS,
        actionLabel: STAGE_TRACKER_SOUND_HELD_ACTION,
        onAction: () => { stageTrackerPlayOnGesture(() => true).then(ok => { if (ok) stageTrackerSoundHeldClear(); }); },
      });
    }
    function stageTrackerSoundHeldClear() {
      const b = document.getElementById('activity-monitor-sound');
      if (!b) return;
      b.classList.remove(STAGE_TRACKER_SOUND_HELD_CLASS);
      stageTrackerSoundSyncButton();
    }

    // Baked sample by sample like the reference render: the snap is a raw
    // noise impulse; the sizzle (noise + square under the rectified mains
    // pulse train, through the sputter gate) rides the envelope.
    function stageTrackerZapVoice(ctx, out, at) {
      const sr = ctx.sampleRate;
      const buf = ctx.createBuffer(1, Math.ceil(sr * STAGE_TRACKER_ZAP_S), sr);
      const data = buf.getChannelData(0);
      let arcOn = 1;
      for (let i = 0; i < data.length; i++) {
        const t = i / sr;
        const snap = (Math.random() * 2 - 1) * Math.exp(-t * STAGE_TRACKER_ZAP_SNAP_DECAY);
        const pulse = Math.pow(Math.max(0, Math.sin(2 * Math.PI * STAGE_TRACKER_ZAP_MAINS_HZ * t)), STAGE_TRACKER_ZAP_PULSE_POW);
        const square = Math.sin(2 * Math.PI * STAGE_TRACKER_ZAP_SQUARE_HZ * t) > 0 ? 1 : -1;
        if (Math.random() < STAGE_TRACKER_ZAP_SPUTTER_P) arcOn = arcOn ? 0 : 1;
        if (!arcOn && Math.random() < STAGE_TRACKER_ZAP_RESTRIKE_P) arcOn = 1;
        const noise = Math.random() * 2 - 1;
        const sizzle = (STAGE_TRACKER_ZAP_NOISE_MIX * noise + STAGE_TRACKER_ZAP_SQUARE_MIX * square) * pulse * arcOn;
        const env = Math.min(1, t / STAGE_TRACKER_ZAP_ATTACK_S) * Math.exp(-Math.max(0, t - STAGE_TRACKER_ZAP_SUSTAIN_S) * STAGE_TRACKER_ZAP_DECAY);
        data[i] = snap + sizzle * env;
      }
      const src = ctx.createBufferSource();
      src.buffer = buf;
      const high = ctx.createBiquadFilter();
      high.type = 'highpass';
      high.frequency.value = STAGE_TRACKER_ZAP_HIGHPASS_HZ;
      src.connect(high); high.connect(out);
      src.start(at); src.stop(at + STAGE_TRACKER_ZAP_S);
    }

    // A bee passing by: a sawtooth that drifts down a few Hz, chopped by a
    // ~30 Hz tremolo on its gain, rounded off through a lowpass.
    function stageTrackerBuzzVoice(ctx, out, at) {
      const osc = ctx.createOscillator();
      osc.type = 'sawtooth';
      osc.frequency.setValueAtTime(STAGE_TRACKER_BUZZ_HZ + STAGE_TRACKER_BUZZ_DRIFT_HZ, at);
      osc.frequency.linearRampToValueAtTime(STAGE_TRACKER_BUZZ_HZ, at + STAGE_TRACKER_BUZZ_S);
      const low = ctx.createBiquadFilter();
      low.type = 'lowpass';
      low.frequency.value = STAGE_TRACKER_BUZZ_LOWPASS_HZ;
      const trem = ctx.createGain();
      trem.gain.value = 1 - STAGE_TRACKER_BUZZ_TREMOLO_DEPTH;
      const lfo = ctx.createOscillator();
      lfo.type = 'sine';
      lfo.frequency.value = STAGE_TRACKER_BUZZ_TREMOLO_HZ;
      const depth = ctx.createGain();
      depth.gain.value = STAGE_TRACKER_BUZZ_TREMOLO_DEPTH;
      lfo.connect(depth); depth.connect(trem.gain);
      const env = ctx.createGain();
      env.gain.setValueAtTime(STAGE_TRACKER_GAIN_FLOOR, at);
      env.gain.exponentialRampToValueAtTime(1, at + STAGE_TRACKER_BUZZ_ATTACK_S);
      env.gain.setValueAtTime(1, at + STAGE_TRACKER_BUZZ_S * 0.6);
      env.gain.exponentialRampToValueAtTime(STAGE_TRACKER_GAIN_FLOOR, at + STAGE_TRACKER_BUZZ_S);
      osc.connect(low); low.connect(trem); trem.connect(env); env.connect(out);
      osc.start(at); lfo.start(at);
      osc.stop(at + STAGE_TRACKER_BUZZ_S); lfo.stop(at + STAGE_TRACKER_BUZZ_S);
    }

    // Schedules count voices (capped) from startOffsetS under one master
    // gain on a context that is already running.
    function stageTrackerScheduleVoices(ctx, voice, count, gain, startOffsetS) {
      if (!ctx) return false;
      try {
        const master = ctx.createGain();
        master.gain.value = gain;
        master.connect(ctx.destination);
        const n = Math.max(1, Math.min(Number(count) || 1, STAGE_TRACKER_SOUND_MAX_PLAYS));
        const at = ctx.currentTime + (Number(startOffsetS) || 0);
        for (let i = 0; i < n; i++) voice(ctx, master, at + i * STAGE_TRACKER_SOUND_OFFSET_S);
        return true;
      } catch { return false; }
    }

    // Automatic play (a merge or new item landed): schedules at once on a
    // running context. A context the autoplay policy still holds gets one
    // short resume() window; voices that would otherwise burst out together
    // on the next gesture are dropped, gesture priming is re-armed and the
    // held-back affordance is shown instead.
    // Returns true only when the voices were scheduled synchronously.
    function stageTrackerPlayVoices(voice, count, gain, startOffsetS) {
      const ctx = stageTrackerAudioContext();
      if (!ctx) return false;
      if (ctx.state === 'running') return stageTrackerScheduleVoices(ctx, voice, count, gain, startOffsetS);
      stageTrackerResumeContext(ctx, STAGE_TRACKER_AUTO_RESUME_MS).then(
        () => stageTrackerScheduleVoices(ctx, voice, count, gain, startOffsetS),
        () => stageTrackerSoundHeldNotice(true));
      return false;
    }

    // Gesture play (▶ Play, ▶ Play both, turning sound on): must be called
    // from the gesture handler itself. Creates the context there and, when
    // it is not yet running, calls resume() and schedule(ctx) synchronously
    // inside the gesture — WebKit only unlocks a context for a resume() or a
    // node start() made while processing the gesture, and a promise
    // continuation is no longer that. Resolves true once the context runs;
    // when the browser refuses, the context (and the voices queued on it)
    // is discarded, a toast says why and it resolves false — a preview never
    // fails silently.
    function stageTrackerPlayOnGesture(schedule) {
      const ctx = stageTrackerAudioContext();
      if (!ctx) { showToast(STAGE_TRACKER_SOUND_UNSUPPORTED_MSG, 'error'); return Promise.resolve(false); }
      if (ctx.state === 'running') return Promise.resolve(!!schedule(ctx));
      const resumed = stageTrackerResumeContext(ctx, STAGE_TRACKER_GESTURE_RESUME_MS);
      const scheduled = !!schedule(ctx);
      return resumed.then(
        () => { stageTrackerSoundHeldClear(); return scheduled; },
        () => { stageTrackerDiscardContext(ctx); showToast(STAGE_TRACKER_SOUND_BLOCKED_MSG, 'warning'); return false; });
    }

    // Marks btn as playing for the life of promise plus lengthS of audio,
    // then restores its label. No btn → promise unchanged.
    function stageTrackerPlayingState(btn, promise, lengthS) {
      if (!btn || typeof btn.setAttribute !== 'function') return promise;
      const label = btn.textContent;
      btn.textContent = STAGE_TRACKER_SOUND_PLAYING_LABEL;
      btn.setAttribute('aria-busy', 'true');
      btn.classList.add('is-playing');
      const restore = () => { btn.textContent = label; btn.removeAttribute('aria-busy'); btn.classList.remove('is-playing'); };
      return promise.then(ok => { setTimeout(restore, ok ? Math.ceil((Number(lengthS) || 0) * 1000) : 0); return ok; }, e => { restore(); throw e; });
    }

    // Pure: a voice that plays one decoded clip.
    function stageTrackerClipVoice(buffer) {
      return function(ctx, out, at) {
        const src = ctx.createBufferSource();
        src.buffer = buffer;
        src.connect(out);
        src.start(at);
      };
    }

    // Pure: the voice for an event — the user's decoded clip when there is
    // one, else the built-in synth (also the fallback when a stored clip
    // failed to decode).
    function stageTrackerVoiceFor(kind, clips) {
      const clip = clips && clips[kind];
      if (clip && clip.buffer) return stageTrackerClipVoice(clip.buffer);
      return kind === 'merge' ? stageTrackerZapVoice : stageTrackerBuzzVoice;
    }

    // Pure: the master gain for an event at a 0–100 volume. Built-in voices
    // keep their relative loudness; a clip plays at the volume alone.
    function stageTrackerGainFor(kind, clips, volume) {
      const v = Math.max(0, Math.min(Number(volume) || 0, STAGE_TRACKER_VOLUME_MAX)) / STAGE_TRACKER_VOLUME_MAX;
      const clip = clips && clips[kind];
      if (clip && clip.buffer) return v;
      return (kind === 'merge' ? STAGE_TRACKER_SOUND_MASTER_GAIN : STAGE_TRACKER_BUZZ_MASTER_GAIN) * v;
    }

    function stageTrackerScheduleSound(ctx, kind, count, startOffsetS) {
      return stageTrackerScheduleVoices(ctx, stageTrackerVoiceFor(kind, _stageTrackerClips), count, stageTrackerGainFor(kind, _stageTrackerClips, _stageTrackerVolume), startOffsetS);
    }
    function stageTrackerPlaySound(kind, count, startOffsetS) {
      return stageTrackerPlayVoices(stageTrackerVoiceFor(kind, _stageTrackerClips), count, stageTrackerGainFor(kind, _stageTrackerClips, _stageTrackerVolume), startOffsetS);
    }
    function stageTrackerPlayZap(count, startOffsetS) { return stageTrackerPlaySound('merge', count, startOffsetS); }
    function stageTrackerPlayBuzz(count, startOffsetS) { return stageTrackerPlaySound('open', count, startOffsetS); }

    // Length of what plays for kind once, for sequencing the test plays.
    function stageTrackerSoundLengthS(kind) {
      const clip = _stageTrackerClips[kind];
      if (clip && clip.buffer) return Number(clip.buffer.duration) || 0;
      return kind === 'merge' ? STAGE_TRACKER_ZAP_S : STAGE_TRACKER_BUZZ_S;
    }
    // Gesture: the merge sound then the open sound, under one resume().
    function stageTrackerPlayTest(btn) {
      const gap = stageTrackerSoundLengthS('merge') + STAGE_TRACKER_CLIP_PREVIEW_GAP_S;
      const play = stageTrackerPlayOnGesture(ctx => {
        const zap = stageTrackerScheduleSound(ctx, 'merge', 1);
        const buzz = stageTrackerScheduleSound(ctx, 'open', 1, gap);
        return zap || buzz;
      });
      return stageTrackerPlayingState(btn, play, gap + stageTrackerSoundLengthS('open'));
    }

    /* ---- Clip storage (IndexedDB) ---- */
    function stageTrackerClipDB() {
      return new Promise((resolve, reject) => {
        const idb = window.indexedDB;
        if (!idb) { reject(new Error('IndexedDB unavailable')); return; }
        let req;
        try { req = idb.open(STAGE_TRACKER_CLIP_DB, 1); } catch (e) { reject(e); return; }
        req.onupgradeneeded = () => { req.result.createObjectStore(STAGE_TRACKER_CLIP_STORE); };
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => reject(req.error || new Error('IndexedDB open failed'));
      });
    }
    function stageTrackerClipTx(mode, run) {
      return stageTrackerClipDB().then(db => new Promise((resolve, reject) => {
        const tx = db.transaction(STAGE_TRACKER_CLIP_STORE, mode);
        const store = tx.objectStore(STAGE_TRACKER_CLIP_STORE);
        const req = run(store);
        tx.oncomplete = () => { db.close(); resolve(req && req.result); };
        tx.onerror = () => { db.close(); reject(tx.error || new Error('IndexedDB transaction failed')); };
        tx.onabort = tx.onerror;
      }));
    }
    function stageTrackerClipsLoad() {
      return stageTrackerClipTx('readonly', store => store.getAll ? store.getAll() : null).then(rows => {
        // Rows carry their kind; fall back to a per-kind get on old engines.
        const list = Array.isArray(rows) ? rows : [];
        for (const row of list) if (row && STAGE_TRACKER_SOUND_KINDS[row.kind] && row.bytes) _stageTrackerClips[row.kind] = { name: String(row.name || 'clip'), bytes: row.bytes, buffer: null, failed: false };
      }).catch(() => {});
    }
    function stageTrackerClipSave(kind, name, bytes) {
      return stageTrackerClipTx('readwrite', store => store.put({ kind, name, bytes }, kind));
    }
    function stageTrackerClipDelete(kind) {
      return stageTrackerClipTx('readwrite', store => store.delete(kind));
    }

    // Decode every stored clip that has bytes but no buffer yet. A clip
    // that fails to decode is marked so the synth plays instead and the
    // modal says why.
    function stageTrackerDecodeClips() {
      const ctx = _stageTrackerAudioCtx;
      if (!ctx || typeof ctx.decodeAudioData !== 'function') return;
      for (const kind of Object.keys(STAGE_TRACKER_SOUND_KINDS)) {
        const clip = _stageTrackerClips[kind];
        if (!clip || clip.buffer || clip.failed || clip.decoding || !clip.bytes) continue;
        clip.decoding = true;
        let p;
        try { p = ctx.decodeAudioData(clip.bytes.slice(0)); } catch (e) { p = Promise.reject(e); }
        Promise.resolve(p).then(buf => { clip.buffer = buf; }, () => {
          clip.failed = true;
          showToast(STAGE_TRACKER_SOUND_KINDS[kind].label + ': "' + clip.name + '" could not be decoded — playing the built-in sound instead.', 'error');
        }).then(() => { clip.decoding = false; stageTrackerRenderSettings(); });
      }
    }

    /* ---- Settings modal ---- */
    function stageTrackerSettingsGearButton() {
      return document.querySelector('[data-action="toggleStageTrackerSettings"]');
    }
    function stageTrackerSettingsFocusables() {
      const dialog = document.getElementById('activity-monitor-settings');
      if (!dialog) return [];
      return Array.from(dialog.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')).filter(el => !el.disabled && el.offsetParent !== null);
    }
    function stageTrackerClipStatusText(kind) {
      const clip = _stageTrackerClips[kind];
      if (!clip) return STAGE_TRACKER_SOUND_KINDS[kind].builtin;
      if (clip.failed) return clip.name + ' (could not decode — built-in plays)';
      if (!clip.buffer) return clip.name + ' (decodes on first play)';
      return clip.name;
    }
    // Pure: the modal body for the current state.
    function stageTrackerSettingsHtml(state) {
      const on = !!state.on;
      const vol = Math.max(0, Math.min(Number(state.volume) || 0, STAGE_TRACKER_VOLUME_MAX));
      const rows = Object.keys(STAGE_TRACKER_SOUND_KINDS).map(kind => {
        const k = STAGE_TRACKER_SOUND_KINDS[kind];
        const custom = !!(state.clips && state.clips[kind]);
        return `<div class="ham-sound-row" data-sound-kind="${esc(kind)}"><span class="ham-sound-row-label">${esc(k.label)}<span class="text-muted"> — ${esc(k.event)}</span></span>`
          + `<span class="ham-sound-name" title="${esc(state.names[kind])}">${esc(state.names[kind])}</span>`
          + `<span class="ham-sound-actions"><button type="button" class="hv-btn btn-sm" data-action="stageTrackerPreviewSound" data-arg0="${esc(kind)}" data-arg-types="s,t" data-stop="1">▶ Play</button>`
          + `<label class="hv-btn btn-sm ham-sound-file">Choose WAV…<input type="file" accept="audio/wav,audio/x-wav,audio/*" data-change-action="stageTrackerUploadSound" data-arg0="${esc(kind)}" data-arg-types="s,t" aria-label="${esc(k.label)} file"></label>`
          + `<button type="button" class="hv-btn btn-sm" data-action="stageTrackerResetSound" data-arg0="${esc(kind)}" data-stop="1"${custom ? '' : ' disabled'}>Reset to built-in</button></span></div>`;
      }).join('');
      return `<div class="overview-settings-section" id="sts-sounds"><div class="overview-settings-card-title">Sounds</div>`
        + `<div class="overview-settings-grid"><label class="overview-settings-row"><input type="checkbox" data-change-action="stageTrackerSetSoundOn" data-arg-types="c"${on ? ' checked' : ''}>Play sounds</label>`
        + `<label class="overview-settings-field">Volume <span class="ham-sound-volume-value" id="activity-monitor-volume-value">${vol}</span><input type="range" min="0" max="${STAGE_TRACKER_VOLUME_MAX}" step="1" value="${vol}" data-input-action="stageTrackerSetVolume" data-arg-types="N" aria-label="Volume"></label></div>`
        + `<p class="ham-sound-help text-muted">Sounds play for every repo this hive governs when the tab is visible. Replace either with your own WAV (or any audio the browser decodes, up to ${Math.round(STAGE_TRACKER_CLIP_MAX_BYTES / (1024 * 1024))} MB); files stay in this browser.</p>`
        + rows
        + `<div class="overview-settings-links"><button type="button" class="hv-btn btn-sm" data-action="stageTrackerPlayTest" data-arg-types="t" data-stop="1">▶ Play both</button></div></div>`
        + stageTrackerRepoSectionHtml(state.repoChip, state.group)
        + stageTrackerConfirmSectionHtml(state.skippedConfirms)
        + stageTrackerCelebrateSectionHtml(state.celebrate !== false, state.celebrateGlyph)
        + stageTrackerNoteTuckSectionHtml(state.noteTuck !== false)
        + stageTrackerMotionSectionHtml(state.motion, state.stages || []);
    }
    // Pure: the Merges section — the "Celebrate merges" toggle, the glyph.
    function stageTrackerCelebrateSectionHtml(on, glyph) {
      const glyphId = stageTrackerCelebrateGlyphId(glyph);
      const opts = Object.keys(STAGE_TRACKER_CELEBRATE_GLYPHS).map(id => `<option value="${esc(id)}"${id === glyphId ? ' selected' : ''}>${esc(STAGE_TRACKER_CELEBRATE_GLYPHS[id])}${id === STAGE_TRACKER_CELEBRATE_GLYPH_DEFAULT ? ' (default)' : ''}</option>`).join('');
      return `<section class="overview-settings-section" id="sts-celebrate" aria-labelledby="sts-celebrate-title">
          <div class="overview-settings-card-title" id="sts-celebrate-title">Merges</div>
          <p class="sts-hint">When a pull request merges its row stays for a moment, its ✓ cell glows and a glyph (the bee) rises from it towards the top of the window — landing on the "Merged" chip when it is in view, otherwise leaving through the top edge — the count rolls up like an odometer, and a banner at the bottom right names the merge until dismissed (or 30 s). Under reduced motion the glyph fades in place. The chip's number is the hive's all-time count (kept on the server, so it survives a reload); click it for the recent merges.</p>
          <div class="sts-row"><label class="overview-settings-row"><input type="checkbox" id="sts-celebrate-on" data-change-action="stageTrackerCelebrateChange" data-arg-types="c"${on ? ' checked' : ''}>Celebrate merges <span class="text-muted">(default on)</span></label><label class="overview-settings-field">Glyph <select class="ham-select" id="sts-celebrate-glyph" data-change-action="stageTrackerCelebrateGlyphChange" data-arg-types="v" aria-label="Celebration glyph">${opts}</select></label><button type="button" class="hv-btn btn-sm" data-action="stageTrackerTestCelebration" data-stop="1" title="Close this dialog and play one celebration on a row in view — the count does not change">Test celebration</button></div>
        </section>`;
    }
    function stageTrackerCelebrateChange(checked) { stageTrackerSetCelebrate(!!checked); }
    // Pure: the "Hive notes" section — the auto-collapse toggle.
    function stageTrackerNoteTuckSectionHtml(on) {
      return `<section class="overview-settings-section" id="sts-notetuck" aria-labelledby="sts-notetuck-title">
          <div class="overview-settings-card-title" id="sts-notetuck-title">Hive notes</div>
          <p class="sts-hint">The "Hive: …" line under a row's chevrons shows for ${Math.round(STAGE_TRACKER_CAPTION_SHOW_MS / 1000)} s when it is new, then slides left into a small ⬡ icon so the list stays quiet; the icon's tooltip keeps the whole note and its age, and a click (or Enter / Space) opens it for another ${Math.round(STAGE_TRACKER_CAPTION_SHOW_MS / 1000)} s. A hidden tab pauses the clock. Off, the line stays open and fades out after two minutes as before.</p>
          <div class="sts-row"><label class="overview-settings-row"><input type="checkbox" id="sts-notetuck-on" data-change-action="stageTrackerNoteTuckChange" data-arg-types="c"${on ? ' checked' : ''}>Auto-collapse hive notes <span class="text-muted">(default on)</span></label></div>
        </section>`;
    }
    function stageTrackerCelebrateGlyphChange(v) { stageTrackerSetCelebrateGlyph(v); }
    // Pure: the first row whose box is inside the viewport (top edge in
    // view is enough), else the first row at all — so the test plays
    // where the operator is looking rather than 2,000 px above.
    function stageTrackerVisibleRow(rows, vh) {
      let first = null;
      for (const r of rows) {
        if (!first) first = r;
        if (typeof r.getBoundingClientRect !== 'function') continue;
        const b = r.getBoundingClientRect();
        if (b.top >= 0 && b.top + b.height <= vh) return r;
      }
      return first;
    }
    // ⚙ → Merges → Test celebration: close the dialog, then one flight
    // from a row in view (pop + zap, a "(test)" banner, the count itself
    // unchanged) so the effect can be seen without waiting for a merge.
    // Says why when it cannot play.
    function stageTrackerTestCelebration() {
      if (typeof closeStageTrackerSettings === 'function') closeStageTrackerSettings();
      const el = document.getElementById('activity-monitor-panel');
      const rows = el ? Array.from(el.querySelectorAll('.ham-row[data-ham-key] .ham-seg.cap')).map(c => typeof c.closest === 'function' ? c.closest('.ham-row[data-ham-key]') : null).filter(Boolean) : [];
      const rowEl = stageTrackerVisibleRow(rows, window.innerHeight || 0);
      if (!rowEl) { showToast('No row to celebrate on — open the Activity Monitor with at least one item listed.', 'warning'); return { flew: false }; }
      if (typeof rowEl.scrollIntoView === 'function' && typeof rowEl.getBoundingClientRect === 'function') { const b = rowEl.getBoundingClientRect(); if (b.top < 0 || b.top + b.height > (window.innerHeight || 0)) rowEl.scrollIntoView({ block: 'center' }); }
      stageTrackerMergeBanner({ key: STAGE_TRACKER_CELEBRATE_TEST_KEY, rowKey: rowEl.dataset.hamKey, number: 0, repo: 'preview', title: 'Test celebration — nothing merged', at: Date.now(), preview: true });
      const r = celebrateRow(rowEl, { kind: 'merged', key: STAGE_TRACKER_CELEBRATE_TEST_KEY, number: 0, preview: true });
      if (r.faded) showToast('Reduced motion is on: the glyph fades in place, the count pops and the row glows.', 'info');
      else if (!r.flew) showToast('Too many glyphs in flight; the count popped instead.', 'info');
      return r;
    }
    const STAGE_TRACKER_CELEBRATE_TEST_KEY = 'preview/pr#0';
    // Pure: the Confirmations section — which row-action confirms this
    // browser skips, and the one link that turns them all back on.
    function stageTrackerConfirmSectionHtml(skipped) {
      skipped = Array.isArray(skipped) ? skipped : [];
      const names = skipped.map(id => esc(stageTrackerActionLabel(id))).join(', ');
      return `<section class="overview-settings-section" id="sts-confirms" aria-labelledby="sts-confirms-title">
          <div class="overview-settings-card-title" id="sts-confirms-title">Confirmations</div>
          <p class="sts-hint">Row actions ask before they run; each dialog has a "${esc(STAGE_TRACKER_CONFIRM_CHECK_LABEL)}" box that skips it in this browser. A skipped dialog's warnings (the sentinel ⚠ line) still arrive in the result toast.</p>
          <div class="sts-row"><span id="sts-confirms-skipped">${skipped.length ? 'Skipped: ' + names : 'Nothing is skipped — every action asks first.'}</span><button type="button" class="hv-btn btn-sm" data-action="stageTrackerResetConfirms" data-stop="1"${skipped.length ? '' : ' disabled'}>Reset confirmations</button></div>
        </section>`;
    }
    // Pure: the Repositories section — repo chip visibility and Group.
    function stageTrackerRepoSectionHtml(repoChip, group) {
      const opts = STAGE_TRACKER_REPO_CHIP_MODES.map(m => `<label class="overview-settings-row"><input type="radio" name="sts-repochip" value="${esc(m.id)}"${m.id === repoChip ? ' checked' : ''} data-change-action="stageTrackerRepoChipChange" data-arg-types="v">${esc(m.label)}${m.id === STAGE_TRACKER_REPO_CHIP_DEFAULT ? ' <span class="text-muted">(default)</span>' : ''}</label>`).join('');
      return `<section class="overview-settings-section" id="sts-repos" aria-labelledby="sts-repos-title">
          <div class="overview-settings-card-title" id="sts-repos-title">Repositories</div>
          <p class="sts-hint">Each row can carry a small chip naming its repository before the number (the primary repo's chip is muted; hover for owner/repo, click to filter the list to that repo).</p>
          <div class="overview-settings-grid" role="radiogroup" aria-label="Show repo chip">${opts}</div>
          <div class="sts-row"><label class="overview-settings-field">Group rows by <select class="ham-select" id="sts-group" data-change-action="hamGroup" data-arg-types="v"><option value="none"${group === 'repo' ? '' : ' selected'}>None</option><option value="repo"${group === 'repo' ? ' selected' : ''}>Repo</option></select></label></div>
        </section>`;
    }
    function stageTrackerRenderSettings() {
      const body = document.getElementById('activity-monitor-settings-body');
      if (!body) return;
      const names = {};
      for (const kind of Object.keys(STAGE_TRACKER_SOUND_KINDS)) names[kind] = stageTrackerClipStatusText(kind);
      setIfChanged(body, stageTrackerSettingsHtml({ on: _stageTrackerSoundOn, volume: _stageTrackerVolume, clips: _stageTrackerClips, names, motion: stageTrackerMotionSetting(), stages: (_stageTracker && _stageTracker.stages) || [], celebrate: stageTrackerCelebrateSetting(), noteTuck: stageTrackerNoteTuckSetting(), celebrateGlyph: stageTrackerCelebrateGlyphId((() => { try { return localStorage.getItem(STAGE_TRACKER_CELEBRATE_GLYPH_KEY); } catch { return null; } })()), skippedConfirms: stageTrackerSkippedConfirms(), repoChip: stageTrackerRepoChipSetting(), group: _stageTrackerGroup }));
      const rm = document.getElementById('sts-rm-preview');
      if (rm) rm.checked = document.body.classList.contains('ham-rm-preview');
    }
    function openStageTrackerSettings() {
      const overlay = document.getElementById('activity-monitor-settings-overlay');
      const btn = stageTrackerSettingsGearButton();
      if (!overlay) return;
      _stageTrackerSettingsReturnFocus = btn || document.activeElement;
      _stageTrackerPreviewAt = 0;
      stageTrackerRenderSettings();
      overlay.classList.remove('hidden');
      hiveRememberModalOpen(overlay);
      if (btn) btn.setAttribute('aria-expanded', 'true');
      const focusables = stageTrackerSettingsFocusables();
      const initial = focusables.find(el => el.classList && el.classList.contains('config-close')) || focusables[0];
      if (initial) setTimeout(() => initial.focus(), 0);
    }
    function closeStageTrackerSettings() {
      const overlay = document.getElementById('activity-monitor-settings-overlay');
      if (overlay) overlay.classList.add('hidden');
      document.body.classList.remove('ham-rm-preview');
      const btn = stageTrackerSettingsGearButton();
      if (btn) btn.setAttribute('aria-expanded', 'false');
      const ret = _stageTrackerSettingsReturnFocus || btn;
      _stageTrackerSettingsReturnFocus = null;
      if (ret && ret.focus) setTimeout(() => ret.focus({ preventScroll: true }), 0);
    }
    function toggleStageTrackerSettings() {
      const overlay = document.getElementById('activity-monitor-settings-overlay');
      if (!overlay) return;
      if (overlay.classList.contains('hidden')) openStageTrackerSettings();
      else closeStageTrackerSettings();
    }
    document.addEventListener('click', function(e) {
      const overlay = document.getElementById('activity-monitor-settings-overlay');
      if (overlay && e.target === overlay) closeStageTrackerSettings();
    });
    document.addEventListener('keydown', function(e) {
      if (e.key !== 'Tab') return;
      const overlay = document.getElementById('activity-monitor-settings-overlay');
      if (!overlay || overlay.classList.contains('hidden')) return;
      const nodes = stageTrackerSettingsFocusables();
      if (!nodes.length) return;
      const first = nodes[0], last = nodes[nodes.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }, true);

    /* ---- Settings actions ---- */
    function stageTrackerSetSoundOn(on) {
      _stageTrackerSoundOn = !!on;
      try { localStorage.setItem(STAGE_TRACKER_SOUND_KEY, _stageTrackerSoundOn ? '1' : '0'); } catch {}
      stageTrackerSoundHeldClear();
      stageTrackerRenderSettings();
      if (_stageTrackerSoundOn) stageTrackerPlayTest();
    }
    function stageTrackerSetVolume(v) {
      _stageTrackerVolume = Math.max(0, Math.min(Math.round(Number(v) || 0), STAGE_TRACKER_VOLUME_MAX));
      try { localStorage.setItem(STAGE_TRACKER_VOLUME_KEY, String(_stageTrackerVolume)); } catch {}
      const out = document.getElementById('activity-monitor-volume-value');
      if (out) out.textContent = String(_stageTrackerVolume);
    }
    // Preview plays regardless of the master toggle: the user is auditioning.
    // A gesture play: the button shows "Playing…" and a blocked context toasts.
    function stageTrackerPreviewSound(kind, btn) {
      if (!STAGE_TRACKER_SOUND_KINDS[kind]) return Promise.resolve(false);
      return stageTrackerPlayingState(btn, stageTrackerPlayOnGesture(ctx => stageTrackerScheduleSound(ctx, kind, 1)), stageTrackerSoundLengthS(kind));
    }
    function stageTrackerUploadSound(kind, input) {
      if (!STAGE_TRACKER_SOUND_KINDS[kind] || !input || !input.files || !input.files[0]) return;
      const file = input.files[0];
      input.value = '';
      if (file.size > STAGE_TRACKER_CLIP_MAX_BYTES) {
        showToast(STAGE_TRACKER_SOUND_KINDS[kind].label + ': "' + file.name + '" is ' + Math.round(file.size / (1024 * 1024)) + ' MB; the limit is ' + Math.round(STAGE_TRACKER_CLIP_MAX_BYTES / (1024 * 1024)) + ' MB.', 'error');
        return;
      }
      const read = typeof file.arrayBuffer === 'function' ? file.arrayBuffer() : new Promise((resolve, reject) => { const r = new FileReader(); r.onload = () => resolve(r.result); r.onerror = () => reject(r.error); r.readAsArrayBuffer(file); });
      read.then(bytes => {
        _stageTrackerClips[kind] = { name: file.name, bytes, buffer: null, failed: false };
        stageTrackerRenderSettings();
        // Create the context so the clip decodes now (decoding does not
        // need a running context); ▶ Play auditions it.
        stageTrackerAudioContext();
        stageTrackerDecodeClips();
        return stageTrackerClipSave(kind, file.name, bytes).then(() => {
          showToast(STAGE_TRACKER_SOUND_KINDS[kind].label + ' set to "' + file.name + '".', 'success');
        }, () => {
          showToast(STAGE_TRACKER_SOUND_KINDS[kind].label + ': "' + file.name + '" plays this visit but could not be saved in this browser.', 'warning');
        });
      }).catch(() => showToast(STAGE_TRACKER_SOUND_KINDS[kind].label + ': could not read "' + file.name + '".', 'error'));
    }
    function stageTrackerResetSound(kind) {
      if (!STAGE_TRACKER_SOUND_KINDS[kind]) return;
      _stageTrackerClips[kind] = null;
      stageTrackerRenderSettings();
      stageTrackerClipDelete(kind).catch(() => {});
    }

    function stageTrackerSoundSyncButton() {
      const b = document.getElementById('activity-monitor-sound');
      if (!b) return;
      const on = _stageTrackerSoundOn;
      b.setAttribute('aria-pressed', on ? 'true' : 'false');
      b.textContent = on ? '🔊' : '🔇';
      const label = on ? 'Sound on (merge zap + new-item buzz) — turn off' : 'Sound off — turn on (merge zap + new-item buzz; plays a test of each)';
      b.setAttribute('aria-label', label);
      b.setAttribute('title', label);
    }

    function stageTrackerSoundInit() {
      try { _stageTrackerSoundOn = localStorage.getItem(STAGE_TRACKER_SOUND_KEY) === '1'; } catch { _stageTrackerSoundOn = false; }
      try {
        const v = localStorage.getItem(STAGE_TRACKER_VOLUME_KEY);
        _stageTrackerVolume = v === null ? STAGE_TRACKER_VOLUME_DEFAULT : Math.max(0, Math.min(Number(v) || 0, STAGE_TRACKER_VOLUME_MAX));
      } catch { _stageTrackerVolume = STAGE_TRACKER_VOLUME_DEFAULT; }
      stageTrackerSoundSyncButton();
      // Sound left on from a previous visit on a document nobody has clicked
      // yet (every reload, including the automatic one on a new hive
      // version): the browser will hold the audio engine back, so say so
      // now rather than after the first zap has been dropped.
      if (_stageTrackerSoundOn && !stageTrackerHasUserActivation(window.navigator)) stageTrackerSoundHeldNotice(false);
      else if (_stageTrackerSoundOn) stageTrackerSoundPrime();
      stageTrackerClipsLoad().then(() => { stageTrackerDecodeClips(); stageTrackerRenderSettings(); });
    }

    // Toolbar speaker chip: flips the preference, persists it, and plays one
    // test of each sound when turning on so the user hears what they opted
    // into.
    function hamSoundToggle() { stageTrackerSetSoundOn(!_stageTrackerSoundOn); }

    /* ---- Movement animation settings section ------------------------------
       Rendered into the card settings modal after Sounds. Each option has a
       Preview that plays it on a sample row inside the modal. */
    const STAGE_TRACKER_PREVIEW_STAGES = [20, 50];
    var _stageTrackerPreviewAt = 0;
    // Pure: the whole sample — column header plus ONE real tracker row built
    // by stageTrackerRowHtml so its geometry matches the live rows. The
    // preview replaces #sts-sample's content with this (never appends).
    function stageTrackerSampleRowHtml(stage, stages) {
      const sample = { repo: 'hive/sample', number: 1, kind: 'issue', title: 'Sample item', url: '#', stage, stage_since: '' };
      return stageTrackerHeaderHtml(stages) + stageTrackerRowHtml(sample, stages, { nowMs: Date.now() });
    }
    // A row holds two .ham-strip lists (the title/head columns and the
    // pipeline chevrons); only the pipeline one is animated.
    function stageTrackerSampleStrip(sample) {
      return sample ? sample.querySelector('.ham-row .ham-strip[aria-label="Pipeline progress"]') : null;
    }
    // Pure: the #sts-motion section. `motion` is the saved choice, `stages`
    // the live stage list for the sample row.
    function stageTrackerMotionSectionHtml(motion, stages) {
      const opts = STAGE_TRACKER_MOTIONS.map(m => `<li class="sts-motion-item">
          <input type="radio" name="sts-motion" id="sts-motion-${esc(m.id)}" value="${esc(m.id)}"${m.id === motion ? ' checked' : ''} data-change-action="stageTrackerMotionChange" data-arg-types="v">
          <label class="sts-motion-label" for="sts-motion-${esc(m.id)}"><span>${m.key ? `<span class="sts-motion-key">${esc(m.key)}</span>` : ''}${esc(m.label)}${m.id === STAGE_TRACKER_MOTION_DEFAULT ? ' <span class="sts-motion-desc">(default)</span>' : ''}</span><span class="sts-motion-desc">${esc(m.desc)}</span></label>
          ${m.id === STAGE_TRACKER_MOTION_NONE ? '<span></span>' : `<button type="button" class="ham-chip" data-action="stageTrackerPreviewMotion" data-arg0="${esc(m.id)}" data-stop="1" title="Play ${esc(m.label)} on the sample row">Preview</button>`}
        </li>`).join('');
      return `<section class="overview-settings-section" id="sts-motion" aria-labelledby="sts-motion-title">
          <div class="overview-settings-card-title" id="sts-motion-title">Movement animation</div>
          <p class="sts-hint">Plays once on a row when its lit chevron moves to another column between refreshes, forwards or back: the old chevron flashes twice, the chosen effect travels, and the new chevron lands lit. Under reduced motion every choice becomes one soft flash and a plain crossfade.</p>
          <ol class="sts-motion-list" role="radiogroup" aria-label="Movement animation">${opts}</ol>
          <div class="sts-sample" id="sts-sample" aria-label="Preview row">${stageTrackerSampleRowHtml(STAGE_TRACKER_PREVIEW_STAGES[_stageTrackerPreviewAt], stages)}</div>
          <div class="sts-row"><label><input type="checkbox" id="sts-rm-preview" data-change-action="stageTrackerRmPreview" data-arg-types="c"> Preview the reduced-motion fallback</label></div>
        </section>`;
    }
    function stageTrackerMotionChange(value) {
      const v = stageTrackerSetMotion(value);
      if (v !== STAGE_TRACKER_MOTION_NONE) stageTrackerPreviewMotion(v);
    }
    function stageTrackerRmPreview(checked) { document.body.classList.toggle('ham-rm-preview', !!checked); }
    // Walk the sample row between Claimed and CI / Review, alternating
    // direction, and play the named effect (the saved setting is not
    // consulted; reduced motion still is).
    function stageTrackerPreviewMotion(id) {
      const sample = document.getElementById('sts-sample');
      if (!sample) return;
      const stages = (_stageTracker && _stageTracker.stages) || [];
      const fromStage = STAGE_TRACKER_PREVIEW_STAGES[_stageTrackerPreviewAt];
      _stageTrackerPreviewAt = (_stageTrackerPreviewAt + 1) % STAGE_TRACKER_PREVIEW_STAGES.length;
      const toStage = STAGE_TRACKER_PREVIEW_STAGES[_stageTrackerPreviewAt];
      const from = stageTrackerColIndex({ kind: 'issue', stage: fromStage }, stages), to = stageTrackerColIndex({ kind: 'issue', stage: toStage }, stages);
      sample.innerHTML = stageTrackerSampleRowHtml(toStage, stages);
      const strip = stageTrackerSampleStrip(sample);
      if (strip) stageTrackerAnimateMove(strip, from, to, stageTrackerMotionEffect(id, stageTrackerReducedMotion()));
    }

