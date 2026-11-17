    // ── Inbox: ● Record a meeting or note into the Inbox ──
    // The browser records with MediaRecorder and uploads one chunk every
    // INBOX_REC_CHUNK_MS (POST /api/inbox/recordings/{id}/chunks?seq=N). Each
    // chunk sits in an IndexedDB queue until the hive answers 2xx, so a
    // network drop or a reload resumes the upload instead of losing audio.
    // The consent notice is shown before every recording; nothing remembers
    // that it was accepted. A red ● Recording indicator stays on screen for
    // the whole recording. See src/docs/inbox-recording.md.
    const INBOX_REC_CONSENT_VERSION = '2026-11-v1';
    const INBOX_REC_CHUNK_MS = 10000;
    const INBOX_REC_BITS_PER_SECOND = 32000;
    const INBOX_REC_MAX_CHUNK_BYTES = 524288;
    const INBOX_REC_DB = 'hive-inbox-recorder';
    const INBOX_REC_DB_VERSION = 1;
    const INBOX_REC_CHUNKS = 'chunks';
    const INBOX_REC_SESSIONS = 'sessions';
    // Upload retry: doubles from INBOX_REC_RETRY_BASE_MS up to INBOX_REC_RETRY_MAX_MS.
    const INBOX_REC_RETRY_BASE_MS = 2000;
    const INBOX_REC_RETRY_MAX_MS = 30000;
    // The timer and meters redraw this often while recording.
    const INBOX_REC_TICK_MS = 250;
    const INBOX_REC_MS_PER_SECOND = 1000;
    const INBOX_REC_SECONDS_PER_MINUTE = 60;
    const INBOX_REC_MINUTES_PER_HOUR = 60;
    // Analyser FFT size for the level meters, and the gain that maps speech
    // RMS (about 0.05 to 0.3) onto most of the meter.
    const INBOX_REC_FFT_SIZE = 1024;
    const INBOX_REC_LEVEL_GAIN = 3;
    // Byte time-domain samples are centred on 128.
    const INBOX_REC_BYTE_MID = 128;
    // MediaRecorder types in order of preference: WebM/Opus (Chromium,
    // Firefox), then MP4 (Safari).
    const INBOX_REC_MIME_PREFERENCE = ['audio/webm;codecs=opus', 'audio/webm', 'audio/mp4;codecs=mp4a.40.2', 'audio/mp4'];
    // HTTP statuses the uploader retries (the rest stop the recording).
    const INBOX_REC_RETRY_STATUSES = [0, 408, 429, 500, 502, 503, 504];
    // Interrupted uploads are resumed this long after the page loads (the
    // viewer's role is known by then).
    const INBOX_REC_RESUME_DELAY_MS = 5000;
    // A live recorder holds a Web Lock with this prefix, so another tab of
    // the same browser never finalizes a recording that is still running.
    const INBOX_REC_LOCK_PREFIX = 'hive-inbox-recording-';
    // After a recording is saved the panel follows its Inbox item: the
    // Inbox is re-read this often while the item shows no transcription
    // state yet (a running job is followed by the transcription poll), for
    // at most INBOX_REC_DONE_WAIT_MS before the panel stops waiting.
    const INBOX_REC_DONE_POLL_MS = 5000;
    const INBOX_REC_DONE_WAIT_MS = 120000;

    let _inboxRec = null;

    // inboxRecSupport reports what this browser can record. record needs
    // MediaRecorder and getUserMedia; mime is the first preferred type the
    // recorder supports ('' lets the browser choose).
    function inboxRecSupport(win) {
      const w = win || {};
      const nav = w.navigator || {};
      const MR = w.MediaRecorder;
      const md = nav.mediaDevices;
      if (typeof MR !== 'function') return { record: false, reason: 'This browser cannot record audio (no MediaRecorder).', mime: '' };
      if (!md || typeof md.getUserMedia !== 'function') return { record: false, reason: 'This browser does not give pages microphone access here (a secure https page is required).', mime: '' };
      let mime = '';
      if (typeof MR.isTypeSupported === 'function') mime = INBOX_REC_MIME_PREFERENCE.find(t => { try { return MR.isTypeSupported(t); } catch (e) { return false; } }) || '';
      return { record: true, reason: '', mime };
    }

    // inboxRecRoleCanRecord: owners and read-write members record; read
    // members do not see the control.
    function inboxRecRoleCanRecord(role) { return role === 'owner' || role === 'read-write'; }

    // inboxRecAvailable decides whether ● Record shows: the hive enables it,
    // the viewer's role allows it and the browser can record.
    function inboxRecAvailable(settings, role, support) {
      const rec = (settings && settings.recording) || {};
      return !!rec.enabled && inboxRecRoleCanRecord(role) && !!(support && support.record);
    }

    function inboxRecFmtClock(ms) {
      const total = Math.max(0, Math.floor((Number(ms) || 0) / INBOX_REC_MS_PER_SECOND));
      const s = total % INBOX_REC_SECONDS_PER_MINUTE;
      const m = Math.floor(total / INBOX_REC_SECONDS_PER_MINUTE) % INBOX_REC_MINUTES_PER_HOUR;
      const h = Math.floor(total / (INBOX_REC_SECONDS_PER_MINUTE * INBOX_REC_MINUTES_PER_HOUR));
      const pad = n => (n < 10 ? '0' : '') + n;
      return (h ? h + ':' + pad(m) : String(m)) + ':' + pad(s);
    }

    // inboxRecLevel turns byte time-domain samples into a 0..1 level (RMS,
    // scaled so speech fills most of the meter).
    function inboxRecLevel(samples) {
      const n = samples ? samples.length : 0;
      if (!n) return 0;
      let sum = 0;
      for (let i = 0; i < n; i++) { const v = (samples[i] - INBOX_REC_BYTE_MID) / INBOX_REC_BYTE_MID; sum += v * v; }
      return Math.min(1, Math.sqrt(sum / n) * INBOX_REC_LEVEL_GAIN);
    }

    // inboxRecSplit cuts one recorder blob into pieces of at most max bytes
    // (the hive accepts chunks up to max_chunk_bytes; concatenation is by
    // byte, so a split is lossless).
    function inboxRecSplit(blob, max) {
      const size = blob && typeof blob.size === 'number' ? blob.size : 0;
      if (!size) return [];
      if (!(max > 0) || size <= max || typeof blob.slice !== 'function') return [blob];
      const out = [];
      for (let off = 0; off < size; off += max) out.push(blob.slice(off, Math.min(size, off + max)));
      return out;
    }

    // inboxRecConsentText is the notice shown before every recording.
    function inboxRecConsentText(opts) {
      const o = opts || {};
      const days = Number(o.retentionDays) || 5;
      const parts = [
        'This records audio from your microphone' + (o.tab ? ' and from the browser tab you choose' : '') + ' and uploads it to this hive while you record.',
        'Everyone you record must know that you are recording. Recording laws differ between countries and states, and many require everyone\'s agreement: making sure this recording is lawful is your responsibility.',
      ];
      if (o.tab) parts.push('Including tab audio captures the remote participants of that meeting too: they must have been told that you are recording.');
      parts.push('The raw audio is kept on this hive for ' + days + ' day' + (days === 1 ? '' : 's') + ' and then deleted; its transcript and Knowledge fact are kept.');
      parts.push(o.transcribes ? 'When you stop, this hive transcribes the recording automatically.' : 'Transcription is off on this hive, so this recording is not transcribed until an owner enables it in Inbox ⚙ → Transcription.');
      return parts;
    }
    const INBOX_REC_CONSENT_ACK = 'I have told everyone who will be recorded, and I am responsible for this recording.';

    function inboxRecDefaultTitle(now) {
      const d = now ? new Date(now) : new Date();
      const pad = n => (n < 10 ? '0' : '') + n;
      return 'Recording ' + d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate()) + ' ' + pad(d.getHours()) + ':' + pad(d.getMinutes());
    }

    // inboxRecConsentHtml is the modal body: source choice, title, the
    // notice, the acknowledgement checkbox and Start (disabled until ticked).
    function inboxRecConsentHtml(opts) {
      const o = opts || {};
      const notice = inboxRecConsentText(o).map(p => '<p class="muted-note">' + escapeHtml(p) + '</p>').join('');
      return '<div class="inbox-rec-consent" data-inbox-rec-consent>' +
        (o.sourcesHtml || '') +
        '<label class="muted-note inbox-field" for="inbox-rec-title">Title<input type="text" id="inbox-rec-title" class="field-input field-input-block mt-2" maxlength="200" value="' + escapeHtml(o.title || '') + '"></label>' +
        '<div class="inbox-rec-notice" id="inbox-rec-notice" role="note">' + notice + '</div>' +
        '<label class="inbox-rec-ack"><input type="checkbox" id="inbox-rec-ack" data-change-action="inboxRecConsentToggle"> <span>' + escapeHtml(INBOX_REC_CONSENT_ACK) + '</span></label>' +
        '<div class="hive-dialog-error" role="alert" id="inbox-rec-error"></div>' +
        '<div class="hive-dialog-buttons"><button type="button" class="hv-btn btn-secondary hive-dialog-btn" data-action="kbCloseModal">Cancel</button>' +
        '<button type="button" class="hv-btn btn-primary hive-dialog-btn primary" id="inbox-rec-start" data-action="inboxRecConfirmStart" disabled aria-disabled="true">● Start recording</button></div></div>';
    }

    // inboxRecMemoryQueue is the queue's in-memory fallback (and the test
    // double): chunks keyed by recording id and seq, plus one session row
    // per recording.
    function inboxRecMemoryQueue() {
      const chunks = new Map();
      const sessions = new Map();
      const key = (id, seq) => id + '#' + String(seq).padStart(8, '0');
      return {
        durable: false,
        putChunk: async (id, seq, blob) => { chunks.set(key(id, seq), { id, seq, blob }); },
        chunks: async (id) => Array.from(chunks.values()).filter(c => c.id === id).sort((a, b) => a.seq - b.seq),
        deleteChunk: async (id, seq) => { chunks.delete(key(id, seq)); },
        putSession: async (row) => { sessions.set(row.id, Object.assign({}, row)); },
        sessions: async () => Array.from(sessions.values()),
        deleteSession: async (id) => { sessions.delete(id); for (const k of Array.from(chunks.keys())) if (k.startsWith(id + '#')) chunks.delete(k); },
      };
    }

    // inboxRecIDBQueue keeps the queue in IndexedDB so chunks survive a
    // reload; it falls back to memory when IndexedDB is unavailable.
    function inboxRecIDBQueue(idb) {
      if (!idb || typeof idb.open !== 'function') return inboxRecMemoryQueue();
      let dbp = null;
      const open = () => dbp || (dbp = new Promise((resolve, reject) => {
        let req;
        try { req = idb.open(INBOX_REC_DB, INBOX_REC_DB_VERSION); } catch (e) { reject(e); return; }
        req.onupgradeneeded = () => {
          const db = req.result;
          if (!db.objectStoreNames.contains(INBOX_REC_CHUNKS)) db.createObjectStore(INBOX_REC_CHUNKS, { keyPath: ['id', 'seq'] });
          if (!db.objectStoreNames.contains(INBOX_REC_SESSIONS)) db.createObjectStore(INBOX_REC_SESSIONS, { keyPath: 'id' });
        };
        req.onsuccess = () => resolve(req.result);
        req.onerror = () => reject(req.error || new Error('IndexedDB open failed'));
      }));
      const tx = (store, mode, run) => open().then(db => new Promise((resolve, reject) => {
        const t = db.transaction(store, mode);
        const req = run(t.objectStore(store));
        t.oncomplete = () => resolve(req ? req.result : undefined);
        t.onerror = () => reject(t.error || new Error('IndexedDB transaction failed'));
        t.onabort = t.onerror;
      }));
      return {
        durable: true,
        putChunk: (id, seq, blob) => tx(INBOX_REC_CHUNKS, 'readwrite', s => s.put({ id, seq, blob })),
        chunks: (id) => tx(INBOX_REC_CHUNKS, 'readonly', s => s.getAll(IDBKeyRange.bound([id, 0], [id, Number.MAX_SAFE_INTEGER]))).then(rows => (rows || []).sort((a, b) => a.seq - b.seq)),
        deleteChunk: (id, seq) => tx(INBOX_REC_CHUNKS, 'readwrite', s => s.delete([id, seq])),
        putSession: (row) => tx(INBOX_REC_SESSIONS, 'readwrite', s => s.put(row)),
        sessions: () => tx(INBOX_REC_SESSIONS, 'readonly', s => s.getAll()).then(rows => rows || []),
        deleteSession: (id) => tx(INBOX_REC_CHUNKS, 'readwrite', s => s.delete(IDBKeyRange.bound([id, 0], [id, Number.MAX_SAFE_INTEGER]))).then(() => tx(INBOX_REC_SESSIONS, 'readwrite', s => s.delete(id))),
      };
    }

    // inboxRecUploader drains one recording's queue in seq order. A chunk
    // leaves the queue only on a 2xx (a re-send the hive already has answers
    // 200 duplicate). A 409 with expected_seq beyond a chunk means the hive
    // stored it before the ack was lost; network errors and 5xx retry with
    // backoff; other refusals are fatal.
    function inboxRecUploader(env) {
      const e = env || {};
      const state = { id: e.id, pending: 0, adding: 0, failing: false, retryInMs: 0, attempt: 0, fatal: '', busy: false, timer: null, closed: false };
      const changed = () => { if (e.onChange) e.onChange(state); };
      const count = async () => { state.pending = (await e.queue.chunks(state.id)).length; };
      async function pump() {
        if (state.busy) { state.again = true; return; }
        // While a retry is scheduled, new chunks wait for it (no bypass of the backoff).
        if (state.fatal || state.closed || state.timer) return;
        state.busy = true;
        try {
          for (;;) {
            const list = await e.queue.chunks(state.id);
            state.pending = list.length;
            changed();
            if (!list.length) break;
            const c = list[0];
            let res = null;
            try {
              res = await e.fetch('/api/inbox/recordings/' + encodeURIComponent(state.id) + '/chunks?seq=' + c.seq, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: c.blob });
            } catch (err) { res = { ok: false, status: 0, json: async () => ({ error: (err && err.message) || 'network error' }) }; }
            if (res.ok) { await e.queue.deleteChunk(state.id, c.seq); state.failing = false; state.attempt = 0; continue; }
            const body = await (res.json ? res.json().catch(() => ({})) : Promise.resolve({}));
            if (res.status === 409 && typeof body.expected_seq === 'number' && body.expected_seq > c.seq) {
              // Stored by the hive already (the ack was lost): drop it.
              await e.queue.deleteChunk(state.id, c.seq);
              continue;
            }
            if (INBOX_REC_RETRY_STATUSES.indexOf(res.status) >= 0) {
              state.failing = true;
              state.retryInMs = Math.min(INBOX_REC_RETRY_MAX_MS, INBOX_REC_RETRY_BASE_MS * Math.pow(2, state.attempt));
              state.attempt++;
              changed();
              state.timer = e.setTimeout(() => { state.timer = null; pump(); }, state.retryInMs);
              break;
            }
            state.fatal = body.error || ('HTTP ' + res.status);
            changed();
            break;
          }
        } finally { state.busy = false; }
        if (state.again && !state.timer) { state.again = false; return pump(); }
        state.again = false;
        if (!state.fatal && !state.failing && e.onDrained && state.pending === 0 && state.adding === 0) e.onDrained();
      }
      return {
        state,
        pump,
        count,
        add: async (seq, blob) => { state.adding++; try { await e.queue.putChunk(state.id, seq, blob); } finally { state.adding--; } await count(); changed(); return pump(); },
        close: () => { state.closed = true; if (state.timer) e.clearTimeout(state.timer); state.timer = null; },
        retryNow: () => { if (state.timer) e.clearTimeout(state.timer); state.timer = null; state.failing = false; pump(); },
      };
    }

    // inboxRecorderMachine runs one recording:
    //   idle → starting → recording ⇄ paused → stopping → finishing → done
    // (error from any state). start() needs the consent version of the
    // notice the user just acknowledged; it is never remembered between
    // recordings. env injects the browser: navigator, MediaRecorder,
    // AudioContext, fetch, queue, now, setTimeout, clearTimeout, onChange.
    function inboxRecorderMachine(env) {
      const e = env || {};
      const m = {
        state: 'idle', id: '', title: '', error: '', warning: '', mime: '',
        startedAt: 0, pausedAt: 0, pausedMs: 0, maxSeconds: 0, retentionDays: 0, transcribes: false,
        seq: 0, streams: [], recorder: null, uploader: null, levels: { mic: 0, tab: 0 }, analysers: {}, audioCtx: null, tab: false,
      };
      const changed = () => { if (e.onChange) e.onChange(m); };
      const now = () => (e.now ? e.now() : Date.now());
      m.elapsedMs = () => {
        if (!m.startedAt) return 0;
        const end = m.state === 'paused' ? m.pausedAt : (m.stoppedAt || now());
        return Math.max(0, end - m.startedAt - m.pausedMs);
      };
      const fail = (msg) => { m.state = 'error'; m.error = msg; cleanup(); changed(); };
      function cleanup() {
        for (const s of m.streams) for (const t of (s.getTracks ? s.getTracks() : [])) { try { t.stop(); } catch (err) { /* already stopped */ } }
        m.streams = [];
        if (m.audioCtx && m.audioCtx.close) { try { m.audioCtx.close(); } catch (err) { /* closed */ } }
        m.audioCtx = null;
        m.analysers = {};
      }
      async function captureStream(opts) {
        const md = e.navigator.mediaDevices;
        const mic = await md.getUserMedia({ audio: { echoCancellation: true, noiseSuppression: true } });
        m.streams.push(mic);
        if (!opts.tab || typeof e.captureTab !== 'function') return { stream: mic, mic, tab: null };
        return e.captureTab(m, mic, e);
      }
      async function start(opts) {
        const o = opts || {};
        if (m.state !== 'idle') throw new Error('a recording is already running');
        if (o.consentVersion !== INBOX_REC_CONSENT_VERSION) throw new Error('the consent notice must be acknowledged before every recording');
        m.state = 'starting'; m.title = o.title || ''; m.tab = !!o.tab; changed();
        let captured;
        try { captured = await captureStream(o); } catch (err) {
          fail(err && err.name === 'NotAllowedError' ? 'Microphone permission was refused: allow the microphone for this site to record.' : 'Could not start the microphone: ' + ((err && err.message) || err));
          return m;
        }
        if (captured.warning) m.warning = captured.warning;
        let res;
        try {
          res = await e.fetch('/api/inbox/recordings', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: m.title, mime: o.mime || '', consent_version: o.consentVersion, includes_tab_audio: !!captured.tab }) });
        } catch (err) { fail('Could not reach the hive: ' + ((err && err.message) || err)); return m; }
        const body = await res.json().catch(() => ({}));
        if (!res.ok) { m.activeId = body.active_id || ''; fail(body.error || ('HTTP ' + res.status)); return m; }
        m.id = body.id; m.maxSeconds = Number(body.max_seconds) || 0; m.retentionDays = Number(body.retention_days) || 0; m.transcribes = !!body.transcribes;
        const maxChunk = Number(body.max_chunk_bytes) || INBOX_REC_MAX_CHUNK_BYTES;
        m.uploader = inboxRecUploader({ id: m.id, queue: e.queue, fetch: e.fetch, setTimeout: e.setTimeout, clearTimeout: e.clearTimeout, onChange: changed, onDrained: () => { if (m.state === 'finishing') finish(); } });
        await e.queue.putSession({ id: m.id, title: m.title, startedAt: now(), stopped: false, durationMs: 0 });
        let rec;
        try {
          rec = new e.MediaRecorder(captured.stream, o.mime ? { mimeType: o.mime, audioBitsPerSecond: INBOX_REC_BITS_PER_SECOND } : { audioBitsPerSecond: INBOX_REC_BITS_PER_SECOND });
        } catch (err) {
          fail('This browser cannot record this audio: ' + ((err && err.message) || err));
          // Release the hive-side recording at once instead of leaving it to the idle sweep.
          await e.queue.deleteSession(m.id);
          try { await e.fetch('/api/inbox/recordings/' + encodeURIComponent(m.id), { method: 'DELETE' }); } catch (derr) { /* the idle sweep removes it */ }
          return m;
        }
        m.recorder = rec;
        m.releaseLock = e.holdLock ? e.holdLock(INBOX_REC_LOCK_PREFIX + m.id) : null;
        m.mime = rec.mimeType || o.mime || '';
        rec.ondataavailable = (ev) => {
          for (const piece of inboxRecSplit(ev && ev.data, maxChunk)) m.uploader.add(m.seq++, piece);
        };
        rec.onstop = () => { m.stoppedAt = m.stoppedAt || now(); afterStop(); };
        rec.onerror = (ev) => { m.warning = 'The recorder reported an error: ' + ((ev && ev.error && ev.error.message) || 'unknown'); changed(); };
        for (const t of (captured.mic && captured.mic.getAudioTracks ? captured.mic.getAudioTracks() : [])) {
          // iOS stops capture when the screen locks: end the recording with what arrived.
          t.onended = () => { if (m.state === 'recording' || m.state === 'paused') { m.warning = 'The microphone stopped (the screen locked or another app took it); the recording ends here.'; stop(); } };
        }
        m.analysers = e.analyse ? e.analyse(m, captured) : {};
        rec.start(INBOX_REC_CHUNK_MS);
        m.startedAt = now();
        m.state = 'recording';
        changed();
        return m;
      }
      function pause() {
        if (m.state !== 'recording') return;
        m.recorder.pause();
        m.pausedAt = now();
        m.state = 'paused';
        changed();
      }
      function resume() {
        if (m.state !== 'paused') return;
        m.recorder.resume();
        m.pausedMs += now() - m.pausedAt;
        m.pausedAt = 0;
        m.state = 'recording';
        changed();
      }
      function stop() {
        if (m.state !== 'recording' && m.state !== 'paused') return;
        if (m.state === 'paused') { m.pausedMs += now() - m.pausedAt; m.pausedAt = 0; }
        m.stoppedAt = now();
        m.state = 'stopping';
        changed();
        try { m.recorder.stop(); } catch (err) { afterStop(); }
      }
      async function afterStop() {
        if (m.state !== 'stopping') return;
        cleanup();
        m.state = 'finishing';
        await e.queue.putSession({ id: m.id, title: m.title, stopped: true, durationMs: m.elapsedMs() });
        changed();
        await m.uploader.pump();
      }
      async function finish() {
        if (m.state !== 'finishing' || m.finalizing) return;
        m.finalizing = true;
        let res;
        try {
          res = await e.fetch('/api/inbox/recordings/' + encodeURIComponent(m.id) + '/finalize', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ duration_ms: m.elapsedMs() }) });
        } catch (err) { res = { ok: false, status: 0, json: async () => ({}) }; }
        m.finalizing = false;
        if (!res.ok && res.status !== 409) {
          m.warning = 'Finishing the recording failed; it is retried.';
          changed();
          e.setTimeout(finish, INBOX_REC_RETRY_BASE_MS);
          return;
        }
        await e.queue.deleteSession(m.id);
        m.uploader.close();
        if (m.releaseLock) m.releaseLock();
        m.state = 'done';
        changed();
      }
      async function abort() {
        const id = m.id;
        if (m.recorder && (m.state === 'recording' || m.state === 'paused')) { m.state = 'aborting'; m.recorder.ondataavailable = null; try { m.recorder.stop(); } catch (err) { /* stopped */ } }
        cleanup();
        if (m.uploader) m.uploader.close();
        if (id) {
          await e.queue.deleteSession(id);
          try { await e.fetch('/api/inbox/recordings/' + encodeURIComponent(id), { method: 'DELETE' }); } catch (err) { /* the idle sweep removes it */ }
        }
        if (m.releaseLock) m.releaseLock();
        m.state = 'idle'; m.id = '';
        changed();
      }
      m.start = start; m.pause = pause; m.resume = resume; m.stop = stop; m.abort = abort; m.finish = finish;
      return m;
    }

    // inboxRecResumePending finishes recordings a reload interrupted: their
    // queued chunks are uploaded, then the recording is finalized.
    async function inboxRecResumePending(env) {
      const e = env || {};
      const rows = await e.queue.sessions();
      const live = e.liveIds ? await e.liveIds() : [];
      const done = [];
      for (const row of rows) {
        if (live.indexOf(row.id) >= 0) continue;
        await new Promise(resolve => {
          const up = inboxRecUploader({ id: row.id, queue: e.queue, fetch: e.fetch, setTimeout: e.setTimeout, clearTimeout: e.clearTimeout, onChange: e.onChange, onDrained: resolve });
          up.pump().then(() => { if (up.state.fatal) resolve(); });
        });
        const res = await e.fetch('/api/inbox/recordings/' + encodeURIComponent(row.id) + '/finalize', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ duration_ms: row.durationMs || 0 }) }).catch(() => ({ ok: false, status: 0 }));
        if (res.ok || res.status === 404 || res.status === 409 || res.status === 400) { await e.queue.deleteSession(row.id); done.push(row.id); }
      }
      return done;
    }

    // ── Panel, indicator and wiring ──
    function inboxRecBrowserSupport() { return inboxRecSupport(typeof window !== 'undefined' ? window : {}); }
    function inboxRecordButtonHtml(settings) {
      if (!inboxRecAvailable(settings, window._hiveRole, inboxRecBrowserSupport())) return '';
      const busy = _inboxRec && _inboxRec.state !== 'idle' && _inboxRec.state !== 'done' && _inboxRec.state !== 'error';
      return '<button type="button" class="hv-btn btn-secondary btn-sm inbox-rec-btn" data-action="' + (busy ? 'inboxRecFocus' : 'inboxRecOpen') + '" title="Record a meeting or a note into the Inbox">' + (busy ? '● Recording…' : '● Record') + '</button>';
    }
    // inboxRecordShortcutHtml is the Inception (spek) shortcut.
    function inboxRecordShortcutHtml() {
      const settings = (_inboxCache || {}).settings || {};
      if (!inboxRecAvailable(settings, window._hiveRole, inboxRecBrowserSupport())) return '';
      return '<button type="button" class="hv-btn btn-secondary btn-sm inbox-rec-btn" data-action="inboxRecOpen" title="Record a meeting or a note; it lands in the Inbox">● Record into Inbox</button>';
    }
    // inboxRecTabSupport: tab audio needs getDisplayMedia with audio, which
    // only Chromium browsers (Chrome, Edge, Brave, ...) on a computer offer:
    // Safari and Firefox share no tab audio, phones have no tab sharing.
    function inboxRecTabSupport(win) {
      const w = win || {};
      const nav = w.navigator || {};
      const md = nav.mediaDevices;
      if (!md || typeof md.getDisplayMedia !== 'function') return { tab: false, reason: 'This browser cannot share a tab (phones and tablets have no tab sharing), so only the microphone is recorded.' };
      const brands = (nav.userAgentData && Array.isArray(nav.userAgentData.brands)) ? nav.userAgentData.brands.map(b => String(b.brand || '')) : [];
      const ua = String(nav.userAgent || '');
      const chromium = brands.some(b => /Chromium|Google Chrome|Microsoft Edge/.test(b)) || (/(Chrome|Chromium|Edg)\//.test(ua) && !/Firefox\//.test(ua));
      const mobile = (nav.userAgentData && nav.userAgentData.mobile) || /Android|iPhone|iPad|Mobile/.test(ua);
      if (!chromium || mobile) return { tab: false, reason: 'Recording a tab\'s audio needs Chrome or Edge on a computer (Safari and Firefox do not share tab audio), so only the microphone is recorded here.' };
      return { tab: true, reason: '' };
    }
    function inboxRecSourcesHtml(tabSupport) {
      const t = tabSupport || { tab: false, reason: '' };
      let html = '<fieldset class="inbox-rec-sources"><legend class="muted-note">Record</legend>' +
        '<label class="muted-note"><input type="radio" name="inbox-rec-source" value="mic" data-change-action="inboxRecSourceChange" checked> Microphone only</label>';
      if (t.tab) html += '<label class="muted-note"><input type="radio" name="inbox-rec-source" value="tab" data-change-action="inboxRecSourceChange"> Microphone and a meeting tab\'s audio (you pick the tab and tick “Share tab audio”)</label>';
      else html += '<p class="muted-xs" data-inbox-rec-tab-unsupported>' + escapeHtml(t.reason) + '</p>';
      return html + '</fieldset>';
    }
    // inboxRecSourceChange redraws the notice for the chosen source: with
    // tab audio it names the remote participants.
    function inboxRecSourceChange() {
      const settings = ((_inboxCache || {}).settings || {}).recording || {};
      const el = document.getElementById('inbox-rec-notice');
      if (!el) return;
      el.innerHTML = inboxRecConsentText({ tab: inboxRecWantsTab(), retentionDays: settings.retention_days, transcribes: !!settings.transcribes }).map(p => '<p class="muted-note">' + escapeHtml(p) + '</p>').join('');
      const ack = document.getElementById('inbox-rec-ack');
      if (ack) { ack.checked = false; inboxRecConsentToggle(); }
    }
    // inboxRecCaptureTab asks for a tab with getDisplayMedia({audio, video}),
    // drops the video track, and mixes the tab audio with the microphone
    // through Web Audio (MediaStreamAudioDestinationNode). A share without an
    // audio track (the user did not tick "Share tab audio", or picked a
    // window) records the microphone only, with a warning.
    async function inboxRecCaptureTab(m, mic, env) {
      const e = env || {};
      let display;
      try {
        display = await e.navigator.mediaDevices.getDisplayMedia({ audio: true, video: true });
      } catch (err) {
        m.warning = 'No tab was shared, so only the microphone is recorded.';
        return { stream: mic, mic, tab: null };
      }
      m.streams.push(display);
      for (const v of display.getVideoTracks()) { try { v.stop(); } catch (err) { /* stopped */ } display.removeTrack(v); }
      const audio = display.getAudioTracks();
      if (!audio.length) {
        m.warning = 'The shared tab has no audio: share a browser tab and tick “Share tab audio”. Only the microphone is recorded.';
        return { stream: mic, mic, tab: null };
      }
      const AC = e.AudioContext;
      if (!AC) { m.warning = 'This browser cannot mix tab audio; only the microphone is recorded.'; return { stream: mic, mic, tab: null }; }
      const ctx = new AC();
      m.audioCtx = ctx;
      const dest = ctx.createMediaStreamDestination();
      ctx.createMediaStreamSource(mic).connect(dest);
      ctx.createMediaStreamSource(display).connect(dest);
      for (const t of audio) t.onended = () => { if (m.state === 'recording' || m.state === 'paused') { m.warning = 'Tab sharing stopped; the recording continues with the microphone only.'; if (e.onChange) e.onChange(m); } };
      return { stream: dest.stream, mic, tab: display };
    }
    function inboxRecOpen() {
      if (_inboxRec && ['starting', 'recording', 'paused', 'stopping', 'finishing'].indexOf(_inboxRec.state) >= 0) { inboxRecFocus(); return; }
      const settings = ((_inboxCache || {}).settings || {}).recording || {};
      const body = inboxRecConsentHtml({ title: inboxRecDefaultTitle(), retentionDays: settings.retention_days, transcribes: !!settings.transcribes, tab: false, sourcesHtml: inboxRecSourcesHtml(inboxRecTabSupport(window)) });
      kbShowModal('● Record into the Inbox', body, { width: '560px' });
      const ack = document.getElementById('inbox-rec-ack');
      if (ack) ack.checked = false;
    }
    function inboxRecConsentToggle() {
      const ack = document.getElementById('inbox-rec-ack');
      const btn = document.getElementById('inbox-rec-start');
      if (!btn) return;
      const on = !!(ack && ack.checked);
      btn.disabled = !on;
      btn.setAttribute('aria-disabled', on ? 'false' : 'true');
    }
    function inboxRecWantsTab() {
      const el = document.querySelector('input[name="inbox-rec-source"]:checked');
      return !!(el && el.value === 'tab');
    }
    async function inboxRecConfirmStart() {
      const ack = document.getElementById('inbox-rec-ack');
      const errEl = document.getElementById('inbox-rec-error');
      if (!ack || !ack.checked) { if (errEl) errEl.textContent = 'Tick the acknowledgement to start recording.'; return; }
      const titleEl = document.getElementById('inbox-rec-title');
      const title = titleEl ? titleEl.value.trim() : '';
      const tab = inboxRecWantsTab();
      kbCloseModal();
      const support = inboxRecBrowserSupport();
      _inboxRec = inboxRecorderMachine(inboxRecEnv());
      inboxRecRender();
      await _inboxRec.start({ title, consentVersion: INBOX_REC_CONSENT_VERSION, mime: support.mime, tab });
      if (_inboxRec.state === 'recording') { inboxRecWakeLock(true); inboxRecArmMaxStop(_inboxRec); }
      inboxRecRender();
    }
    let _inboxRecQueue = null;
    function inboxRecQueue() { return _inboxRecQueue || (_inboxRecQueue = inboxRecIDBQueue(typeof indexedDB !== 'undefined' ? indexedDB : null)); }
    function inboxRecEnv() {
      return {
        navigator, MediaRecorder: window.MediaRecorder, fetch: (u, o) => fetch(u, o), queue: inboxRecQueue(), now: () => Date.now(),
        setTimeout: (f, ms) => setTimeout(f, ms), clearTimeout: t => clearTimeout(t), onChange: () => inboxRecRender(),
        holdLock: inboxRecHoldLock, analyse: inboxRecAnalyse, captureTab: inboxRecCaptureTab, AudioContext: window.AudioContext || window.webkitAudioContext,
      };
    }
    // inboxRecAnalyse attaches an AnalyserNode to each captured stream for
    // the level meters (no audio is played back or processed otherwise).
    function inboxRecAnalyse(m, captured) {
      const AC = window.AudioContext || window.webkitAudioContext;
      if (!AC) return {};
      try {
        const ctx = m.audioCtx || new AC();
        m.audioCtx = ctx;
        const out = {};
        for (const [name, stream] of [['mic', captured.mic], ['tab', captured.tab]]) {
          if (!stream) continue;
          const a = ctx.createAnalyser();
          a.fftSize = INBOX_REC_FFT_SIZE;
          ctx.createMediaStreamSource(stream).connect(a);
          out[name] = { node: a, buf: new Uint8Array(a.fftSize) };
        }
        return out;
      } catch (e) { return {}; }
    }
    function inboxRecReadLevels(m) {
      for (const name of Object.keys(m.analysers || {})) {
        const a = m.analysers[name];
        a.node.getByteTimeDomainData(a.buf);
        m.levels[name] = m.state === 'recording' ? inboxRecLevel(a.buf) : 0;
      }
    }
    let _inboxRecWake = null;
    function inboxRecWakeLock(on) {
      if (!on) { const s = _inboxRecWake; _inboxRecWake = null; try { if (s && s.release) s.release().catch(() => {}); } catch (e) { /* released */ } return; }
      if (typeof navigator === 'undefined' || !navigator.wakeLock || typeof navigator.wakeLock.request !== 'function') return;
      try { navigator.wakeLock.request('screen').then(s => { _inboxRecWake = s; }, () => {}); } catch (e) { /* refused */ }
    }
    // inboxRecUploadText is the upload status line.
    function inboxRecUploadText(up) {
      if (!up) return '';
      if (up.fatal) return 'Upload stopped: ' + up.fatal;
      if (!up.pending) return 'All chunks saved on the hive';
      const n = up.pending + ' chunk' + (up.pending === 1 ? '' : 's') + ' waiting to upload';
      return up.failing ? n + ' (connection problem; retrying in ' + Math.ceil(up.retryInMs / INBOX_REC_MS_PER_SECOND) + ' s)' : n;
    }
    function inboxRecMeterHtml(name, label, level) {
      return '<label class="inbox-rec-meter-row muted-xs"><span>' + escapeHtml(label) + '</span><meter class="inbox-rec-meter" data-inbox-rec-meter="' + name + '" min="0" max="1" low="0.1" high="0.85" optimum="0.5" value="' + (Number(level) || 0).toFixed(2) + '" aria-label="' + escapeHtml(label + ' level') + '"></meter></label>';
    }
    // inboxRecPanelHtml renders the recording panel for machine state m.
    function inboxRecPanelHtml(m) {
      if (!m || m.state === 'idle') return '';
      const live = m.state === 'recording' || m.state === 'paused';
      let html = '<div class="inbox-rec-head"><span class="inbox-rec-dot' + (m.state === 'recording' ? ' is-live' : '') + '" aria-hidden="true">●</span> <strong>' + escapeHtml(m.title || 'Recording') + '</strong>';
      const status = { starting: 'Starting…', recording: 'Recording', paused: 'Paused', stopping: 'Stopping…', finishing: 'Saving…', done: 'Saved to the Inbox', error: 'Not recording' }[m.state] || m.state;
      html += ' <span class="inbox-rec-time" data-inbox-rec-time>' + escapeHtml(inboxRecFmtClock(m.elapsedMs ? m.elapsedMs() : 0)) + '</span> <span class="muted-xs" role="status">' + escapeHtml(status) + '</span></div>';
      if (live) {
        html += '<div class="inbox-rec-meters">' + inboxRecMeterHtml('mic', 'Microphone', m.levels.mic) + (m.analysers && m.analysers.tab ? inboxRecMeterHtml('tab', 'Tab audio', m.levels.tab) : '') + '</div>';
        if (m.maxSeconds) html += '<div class="muted-xs">Stops by itself at ' + escapeHtml(inboxRecFmtClock(m.maxSeconds * INBOX_REC_MS_PER_SECOND)) + '. Keep this screen open: phones stop recording when the screen locks.</div>';
      }
      if (m.uploader) html += '<div class="muted-xs inbox-rec-upload" data-inbox-rec-upload aria-live="polite">' + escapeHtml(inboxRecUploadText(m.uploader.state)) + '</div>';
      if (m.warning) html += '<div class="kc-error muted-xs" role="alert">' + escapeHtml(m.warning) + '</div>';
      if (m.error) html += '<div class="kc-error muted-xs" role="alert">' + escapeHtml(m.error) + '</div>';
      if (m.state === 'done') {
        const o = m.outcome || inboxRecDoneOutcome(null, m.transcribes, '', false);
        html += '<div class="' + (o.state === 'failed' ? 'kc-error ' : '') + 'muted-xs" role="status" data-inbox-rec-outcome="' + escapeHtml(o.state) + '">' + escapeHtml(inboxRecDoneText(o)) + '</div>';
      }
      html += '<div class="kc-actions mt-2">';
      if (m.state === 'recording') html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxRecPause">⏸ Pause</button>';
      if (m.state === 'paused') html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxRecResume">⏺ Resume</button>';
      if (live) html += '<button type="button" class="hv-btn btn-primary btn-sm" data-action="inboxRecStop">⏹ Stop and save</button><button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxRecAbort">Discard</button>';
      if (m.uploader && m.uploader.state.failing) html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxRecRetry">Retry upload now</button>';
      if (m.state === 'error' && m.activeId) html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxRecFinishEarlier" data-arg0="' + escapeHtml(m.activeId) + '">Finish the earlier recording</button>';
      if (m.state === 'done' || m.state === 'error') html += '<button type="button" class="hv-btn btn-secondary btn-sm" data-action="inboxRecClose">Close</button>';
      return html + '</div>';
    }
    // inboxRecDoneOutcome is where a saved recording's Inbox item stands:
    // transcribed (the item has text), failed (the job failed or was
    // skipped, or the hive cannot transcribe it; reason says why), off
    // (transcription is off on this hive), stalled (no job appeared within
    // INBOX_REC_DONE_WAIT_MS: waitedOut) or waiting. Every state but waiting
    // starts the panel's auto-dismiss countdown.
    function inboxRecDoneOutcome(it, transcribes, unavailableText, waitedOut) {
      const tx = (it && it.transcription) || null;
      if (it && (it.has_transcript || it.fact_state === 'machine' || (tx && tx.status === 'done'))) return { state: 'transcribed', reason: '' };
      if (tx && (tx.status === 'failed' || tx.status === 'skipped')) return { state: 'failed', reason: tx.reason || '' };
      if (tx && (tx.status === 'queued' || tx.status === 'running')) return { state: 'waiting', reason: '' };
      if (!transcribes || (it && it.transcribe_unavailable === 'disabled')) return { state: 'off', reason: '' };
      if (it && it.transcribe_unavailable) return { state: 'failed', reason: unavailableText || '' };
      if (waitedOut) return { state: 'stalled', reason: '' };
      return { state: 'waiting', reason: '' };
    }
    // inboxRecDoneText is the panel line for an outcome.
    function inboxRecDoneText(o) {
      switch (o && o.state) {
        case 'transcribed': return 'Transcribed. See the item below.';
        case 'failed': return 'Transcription failed' + (o.reason ? ': ' + o.reason : '') + '. The recording is saved in the Inbox below; retry the transcription from the item.';
        case 'off': return 'It appears in the Inbox below. Transcription is off on this hive, so it is not transcribed yet.';
        case 'stalled': return 'Saved in the Inbox below. Transcription has not started yet; follow it on the item.';
        default: return 'The hive is transcribing it now; follow the progress on the item in the Inbox below.';
      }
    }
    // inboxRecSyncDone runs after every Inbox read: it finds the saved
    // recording's item, updates the panel line, and once the outcome is
    // final starts the same auto-dismiss countdown toasts use (bar, duration
    // and pause on hover or focus). Close still dismisses at once.
    function inboxRecSyncDone(items) {
      const m = _inboxRec;
      if (!m || m.state !== 'done' || !m.id) return;
      if (!m.doneAt) m.doneAt = Date.now();
      const it = (items || []).find(x => x && x.source === 'hive-recording' && x.id === m.id) || null;
      const waitedOut = Date.now() - m.doneAt >= INBOX_REC_DONE_WAIT_MS;
      const why = it && it.transcribe_unavailable && typeof inboxTxUnavailableText === 'function' ? inboxTxUnavailableText(it.transcribe_unavailable) : '';
      const o = inboxRecDoneOutcome(it, m.transcribes, why, waitedOut);
      const changed = !m.outcome || m.outcome.state !== o.state || m.outcome.reason !== o.reason;
      m.outcome = o;
      if (changed) inboxRecRender();
      clearTimeout(m.donePoll);
      m.donePoll = 0;
      if (o.state !== 'waiting') { inboxRecArmDismiss(m); return; }
      // A running job is followed by the Inbox's own transcription poll.
      const busy = it && it.transcription && (it.transcription.status === 'queued' || it.transcription.status === 'running');
      if (!busy && typeof fetchInbox === 'function') m.donePoll = setTimeout(() => { if (_inboxRec === m) fetchInbox(); }, INBOX_REC_DONE_POLL_MS);
    }
    function inboxRecArmDismiss(m) {
      const panel = document.getElementById('inbox-rec-panel');
      if (!panel || m.dismissArmed || typeof toastCountdownBar !== 'function') return;
      m.dismissArmed = true;
      if (m.outcome) panel.dataset.outcome = m.outcome.state;
      toastCountdownBar(panel, TOAST_DURATION_MS, () => { if (_inboxRec === m) inboxRecClose(); });
    }
    // The timer and meters redraw on a dashboard interval (paused while the
    // page is hidden); the length limit has its own timeout so it holds in
    // the background too.
    let _inboxRecTicker = false;
    function inboxRecArmMaxStop(m) {
      if (!m || !m.maxSeconds) return;
      const left = m.maxSeconds * INBOX_REC_MS_PER_SECOND - m.elapsedMs();
      setTimeout(() => {
        if (_inboxRec !== m || (m.state !== 'recording' && m.state !== 'paused')) return;
        if (m.state === 'recording' && m.elapsedMs() >= m.maxSeconds * INBOX_REC_MS_PER_SECOND) { m.warning = 'The recording reached this hive\'s length limit and was stopped.'; m.stop(); return; }
        inboxRecArmMaxStop(m);
      }, Math.max(INBOX_REC_TICK_MS, left));
    }
    function inboxRecRender() {
      const m = _inboxRec;
      const panel = document.getElementById('inbox-rec-panel');
      const html = inboxRecPanelHtml(m);
      if (panel) {
        // A countdown belongs to the saved recording it was armed for.
        if (panel._countdown && !(m && m.state === 'done' && m.dismissArmed) && typeof toastCountdownCancel === 'function') toastCountdownCancel(panel);
        panel.hidden = !html;
        if (m && m.outcome) panel.dataset.outcome = m.outcome.state; else delete panel.dataset.outcome;
        if (panel._html !== html) {
          panel._html = html;
          panel.innerHTML = html;
          // The rewrite dropped the countdown bar; put it back where it was.
          if (panel._countdown && panel._countdown.bar) panel.appendChild(panel._countdown.bar);
        }
      }
      inboxRecIndicator(m);
      const live = m && (m.state === 'recording' || m.state === 'paused');
      if (live && !_inboxRecTicker) { _inboxRecTicker = true; dashboardSetInterval('inbox-recorder', inboxRecTick, INBOX_REC_TICK_MS); }
      if (!live && _inboxRecTicker) { _inboxRecTicker = false; dashboardClearInterval('inbox-recorder'); }
      if (m && m.state === 'done' && !m.listed) {
        m.listed = true; inboxRecWakeLock(false);
        if (!m.transcribes) { m.outcome = inboxRecDoneOutcome(null, false, '', false); inboxRecArmDismiss(m); }
        if (typeof fetchInbox === 'function') fetchInbox();
      }
      if (m && m.state === 'error') inboxRecWakeLock(false);
      const btn = document.querySelector('#inbox-panel .inbox-rec-btn');
      if (btn) btn.outerHTML = inboxRecordButtonHtml((_inboxCache || {}).settings || {});
    }
    // inboxRecTick redraws the timer and meters in place, and stops at max_seconds.
    function inboxRecTick() {
      const m = _inboxRec;
      if (!m) return;
      inboxRecReadLevels(m);
      if (m.maxSeconds && m.state === 'recording' && m.elapsedMs() >= m.maxSeconds * INBOX_REC_MS_PER_SECOND) { m.warning = 'The recording reached this hive\'s length limit and was stopped.'; m.stop(); return; }
      const t = document.querySelector('[data-inbox-rec-time]');
      if (t) t.textContent = inboxRecFmtClock(m.elapsedMs());
      for (const name of Object.keys(m.levels)) {
        const el = document.querySelector('[data-inbox-rec-meter="' + name + '"]');
        if (el) el.value = m.levels[name];
      }
      const ind = document.getElementById('inbox-rec-indicator-time');
      if (ind) ind.textContent = inboxRecFmtClock(m.elapsedMs());
    }
    // inboxRecIndicator keeps a red ● Recording pill on screen, whatever
    // section is in view, for as long as the recording runs.
    function inboxRecIndicator(m) {
      let el = document.getElementById('inbox-rec-indicator');
      const live = m && ['starting', 'recording', 'paused', 'stopping'].indexOf(m.state) >= 0;
      if (!live) { if (el) el.hidden = true; return; }
      if (!el) {
        el = document.createElement('button');
        el.type = 'button';
        el.id = 'inbox-rec-indicator';
        el.className = 'inbox-rec-indicator';
        el.setAttribute('data-action', 'inboxRecFocus');
        document.body.appendChild(el);
      }
      el.hidden = false;
      el.setAttribute('aria-label', (m.state === 'paused' ? 'Recording paused' : 'Recording') + ': open the recorder');
      el.innerHTML = '<span class="inbox-rec-dot' + (m.state === 'recording' ? ' is-live' : '') + '" aria-hidden="true">●</span> ' + (m.state === 'paused' ? 'Paused' : 'Recording') + ' <span id="inbox-rec-indicator-time">' + escapeHtml(inboxRecFmtClock(m.elapsedMs())) + '</span>';
    }
    function inboxRecFocus() {
      if (typeof ocNavigate === 'function') ocNavigate('inbox-section');
      const panel = document.getElementById('inbox-rec-panel');
      if (panel && panel.scrollIntoView) panel.scrollIntoView({ block: 'center' });
    }
    function inboxRecPause() { if (_inboxRec) _inboxRec.pause(); }
    function inboxRecResume() { if (_inboxRec) _inboxRec.resume(); }
    function inboxRecStop() { if (_inboxRec) _inboxRec.stop(); }
    function inboxRecRetry() { if (_inboxRec && _inboxRec.uploader) _inboxRec.uploader.retryNow(); }
    async function inboxRecAbort() {
      if (!_inboxRec) return;
      if (!(await hiveConfirm('Discard this recording? Nothing of it is kept.', 'Discard'))) return;
      await _inboxRec.abort();
      inboxRecWakeLock(false);
      _inboxRec = null;
      inboxRecRender();
    }
    function inboxRecClose() { if (_inboxRec) clearTimeout(_inboxRec.donePoll); _inboxRec = null; inboxRecRender(); }
    // inboxRecFinishEarlier finalizes the user's earlier recording that is
    // still open on the hive (another tab or device stopped sending), so a
    // new one can start.
    async function inboxRecFinishEarlier(id) {
      if (!(await hiveConfirm('Finish the earlier recording with what the hive received so far? If it is still running in another tab or device, that recording stops there.', 'Finish it'))) return;
      try {
        const r = await fetch('/api/inbox/recordings/' + encodeURIComponent(id) + '/finalize', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
        const data = await r.json().catch(() => ({}));
        if (!r.ok && r.status !== 409) throw new Error(data.error || ('HTTP ' + r.status));
        showToast(r.ok ? 'The earlier recording was saved to the Inbox' : 'The earlier recording has already ended', 'success');
        _inboxRec = null;
        inboxRecRender();
        if (typeof fetchInbox === 'function') fetchInbox();
      } catch (e) { showToast('Could not finish it: ' + e.message, 'error'); }
    }
    // inboxRecHoldLock holds a Web Lock until the returned release is
    // called (no-op where Web Locks are missing).
    function inboxRecHoldLock(name) {
      const locks = typeof navigator !== 'undefined' && navigator.locks;
      if (!locks || typeof locks.request !== 'function') return null;
      let release = null;
      const held = new Promise(res => { release = res; });
      try { locks.request(name, () => held).catch(() => {}); } catch (e) { return null; }
      return () => { if (release) release(); };
    }
    // inboxRecLiveIds lists recordings a recorder in this browser holds.
    async function inboxRecLiveIds() {
      const locks = typeof navigator !== 'undefined' && navigator.locks;
      if (!locks || typeof locks.query !== 'function') return [];
      try {
        const st = await locks.query();
        return (st.held || []).map(l => String(l.name || '')).filter(n => n.indexOf(INBOX_REC_LOCK_PREFIX) === 0).map(n => n.slice(INBOX_REC_LOCK_PREFIX.length));
      } catch (e) { return []; }
    }
    // While recording, warn when the tab is hidden (desktop browsers keep
    // recording; phones stop when the screen locks) and take the wake lock
    // back when the page is visible again.
    if (typeof document !== 'undefined') document.addEventListener('visibilitychange', () => {
      const m = _inboxRec;
      if (!m || (m.state !== 'recording' && m.state !== 'paused')) return;
      if (document.visibilityState === 'hidden') { m.warning = 'This page was hidden while recording. A desktop browser keeps recording; on a phone, keep this screen open and unlocked or the recording stops.'; }
      else { inboxRecWakeLock(true); inboxRecRender(); }
    });
    // Leaving the page mid-recording loses what has not been recorded yet;
    // queued chunks survive and upload on the next visit.
    if (typeof window !== 'undefined') window.addEventListener('beforeunload', (ev) => {
      const m = _inboxRec;
      if (m && (m.state === 'recording' || m.state === 'paused' || (m.uploader && m.uploader.state.pending))) { ev.preventDefault(); ev.returnValue = ''; }
    });
    // A reload or a network drop left chunks behind: upload and finish them.
    async function inboxRecResumeOnLoad() {
      try {
        const q = inboxRecQueue();
        const rows = await q.sessions();
        if (!rows.length) return;
        showToast('Finishing ' + rows.length + ' interrupted recording upload' + (rows.length === 1 ? '' : 's') + '…', 'info');
        const done = await inboxRecResumePending({ queue: q, fetch: (u, o) => fetch(u, o), setTimeout: (f, ms) => setTimeout(f, ms), clearTimeout: t => clearTimeout(t), liveIds: inboxRecLiveIds });
        if (done.length) { showToast('Interrupted recording saved to the Inbox', 'success'); if (typeof fetchInbox === 'function') fetchInbox(); }
      } catch (e) { /* IndexedDB unavailable: nothing to resume */ }
    }
    if (typeof window !== 'undefined') setTimeout(() => { if (inboxRecRoleCanRecord(window._hiveRole) && !_inboxRec) inboxRecResumeOnLoad(); }, INBOX_REC_RESUME_DELAY_MS);
