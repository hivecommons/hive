// Settings, Meeting attendee: clicking a persona preset card selects it and
// plays its introduction in the preset's own voice (hivecommons/hive#18710).
// Select and play, switch, stop, and the "Voice preview is unavailable"
// message with no fallback to the browser's speech synthesis.
// Run: node --test src/pkg/dashboard/*.test.js
const test = require('node:test');
const assert = require('node:assert');
const fs = require('node:fs');
const path = require('node:path');

const html = fs.readFileSync(path.join(__dirname, 'static', 'index.html'), 'utf8');
const ASYNC_PREFIX = 'async ';
const fnSrc = name => {
  let start = html.indexOf('function ' + name + '(');
  assert.ok(start >= 0, name + ' not found');
  if (html.slice(start - ASYNC_PREFIX.length, start) === ASYNC_PREFIX) start -= ASYNC_PREFIX.length;
  let depth = 0;
  for (let i = html.indexOf('{', start); i < html.length; i++) {
    if (html[i] === '{') depth++;
    else if (html[i] === '}' && --depth === 0) return html.slice(start, i + 1);
  }
  throw new Error('unbalanced ' + name);
};
const constSrc = name => {
  const start = html.indexOf('const ' + name + ' = ');
  assert.ok(start >= 0, name + ' not found');
  return html.slice(start, html.indexOf(';\n', start) + 1);
};
const escapeHtml = s => String(s).replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const CONSTS = ['MA_LANGUAGE_LABELS', 'MA_FLAG_BASE', 'MA_INTRO_PLAY_ICON', 'MA_INTRO_STOP_ICON', 'MA_INTRO_UNAVAILABLE'];
const FUNCS = ['maLanguageLabel', 'maFlag', 'maPreset', 'maPresetCardHtml', 'maIntroSrc', 'maIntroState', 'maIntroLabel', 'maIntroSet',
  'maIntroSync', 'maStopIntro', 'maPlayIntro', 'maIntroFailed', 'maTogglePresetIntro', 'maPickPreset'];
const PREVIEW_SOURCE = FUNCS.map(fnSrc).join('\n');

const PRESETS = [
  { id: 'betty', name: 'Betty', display_name: 'Betty Bee from the hive', language: 'en-US', accent: 'American', voice: 'af_heart', voice_type: 'female', humor: 'light', intro_template: 'Hi', aliases: ['betty'], leave_phrases: ['betty leave'] },
  { id: 'bertie', name: 'Bertie', display_name: 'Bertie Bee from the hive', language: 'en-GB', accent: 'British', voice: 'bm_george', voice_type: 'male', humor: 'light', intro_template: 'Hello', aliases: ['bertie'], leave_phrases: ['bertie leave'] },
  { id: 'melie', name: 'Mélie', display_name: 'Mélie', language: 'fr-FR', accent: 'French', voice: 'ff_siwis', voice_type: 'female', humor: 'light', intro_template: 'Bonjour', aliases: ['melie'], leave_phrases: ['melie pars'] },
];

// setup builds the tab's preview code around a fake Audio, fetch and DOM.
function setup(fetchImpl) {
  const audios = [];
  class FakeAudio {
    constructor(src) {
      this.src = src;
      this.plays = 0;
      this.pauses = 0;
      this.listeners = {};
      this.playResult = FakeAudio.nextPlay || Promise.resolve();
      FakeAudio.nextPlay = null;
      audios.push(this);
    }
    addEventListener(name, fn) { (this.listeners[name] = this.listeners[name] || []).push(fn); }
    play() { this.plays++; return this.playResult; }
    pause() { this.pauses++; }
    fire(name) { (this.listeners[name] || []).forEach(fn => fn()); }
  }
  const fetched = [];
  const fetch = url => { fetched.push(url); return fetchImpl(url); };
  const document = { querySelectorAll: () => [], querySelector: () => null };
  const state = { data: { presets: PRESETS }, form: { preset: 'betty', humor: 'light' }, dirty: false };
  const lib = new Function('escapeHtml', 'document', 'Audio', 'fetch', 'STATE',
    CONSTS.map(constSrc).join('\n') + '\n' +
    'let _maState = STATE; let _maIntro = { id: "", state: "", error: "" }; let _maIntroAudio = null;\n' +
    'let renders = 0; function maRender() { renders++; }\n' +
    PREVIEW_SOURCE +
    '\nreturn { maPresetCardHtml, maPickPreset, maTogglePresetIntro, maStopIntro, maPreset,' +
    ' get intro() { return _maIntro; }, get audio() { return _maIntroAudio; }, get renders() { return renders; } };');
  return { m: lib(escapeHtml, document, FakeAudio, fetch, state), audios, fetched, state, FakeAudio };
}

const flush = () => new Promise(resolve => setImmediate(resolve));
const jsonReply = (status, body) => Promise.resolve({ ok: status >= 200 && status < 300, status, json: () => Promise.resolve(body) });

test('a card is a select button plus a labelled speaker button, and nothing plays on render', () => {
  const { m, audios } = setup(() => jsonReply(200, {}));
  const card = m.maPresetCardHtml(PRESETS[0], true);
  assert.match(card, /^<div class="ma-preset is-selected" data-ma-preset="betty">/);
  assert.match(card, /<button type="button" class="ma-preset-pick" data-action="maPickPreset" data-arg0="betty" aria-pressed="true"/);
  assert.match(card, /<button type="button" class="ma-preset-play" data-action="maTogglePresetIntro" data-arg0="betty" data-ma-preset-play aria-label="Play introduction for Betty Bee from the hive"/);
  assert.ok(card.includes('🔊') && !card.includes('is-playing') && !card.includes('is-loading'));
  assert.strictEqual((card.match(/<button /g) || []).length, 2, 'two sibling buttons, none nested');
  assert.strictEqual(audios.length, 0, 'rendering a card never creates audio: no autoplay');
  assert.strictEqual(m.audio, null);
});

test('clicking a card selects the preset and plays its introduction from the preview route', () => {
  const { m, audios, state } = setup(() => jsonReply(200, {}));
  m.maPickPreset('melie');
  assert.strictEqual(state.form.preset, 'melie');
  assert.strictEqual(state.form.voice, 'ff_siwis');
  assert.strictEqual(m.renders, 1);
  assert.strictEqual(audios.length, 1);
  assert.strictEqual(audios[0].src, '/api/meetings/persona/preview?preset=melie');
  assert.strictEqual(audios[0].plays, 1, 'play() runs inside the click');
  assert.deepStrictEqual(m.intro, { id: 'melie', state: 'loading', error: '' });
  assert.ok(m.maPresetCardHtml(PRESETS[2], true).includes('ma-preset is-selected is-loading'));
  audios[0].fire('playing');
  assert.deepStrictEqual(m.intro, { id: 'melie', state: 'playing', error: '' });
  const card = m.maPresetCardHtml(PRESETS[2], true);
  assert.ok(card.includes('ma-preset is-selected is-playing'), card);
  assert.ok(card.includes('aria-label="Stop introduction for Mélie"') && card.includes('⏹'), card);
  assert.ok(m.maPresetCardHtml(PRESETS[0], false).includes('aria-label="Play introduction for Betty Bee from the hive"'), 'other cards still offer play');
  audios[0].fire('ended');
  assert.deepStrictEqual(m.intro, { id: '', state: '', error: '' });
  assert.strictEqual(m.audio, null);
});

test('clicking another card stops the current clip and plays the new one', () => {
  const { m, audios, state } = setup(() => jsonReply(200, {}));
  m.maPickPreset('betty');
  audios[0].fire('playing');
  m.maPickPreset('bertie');
  assert.strictEqual(audios[0].pauses, 1, 'the first clip is stopped');
  assert.strictEqual(audios.length, 2);
  assert.strictEqual(audios[1].src, '/api/meetings/persona/preview?preset=bertie');
  assert.strictEqual(audios[1].plays, 1);
  assert.strictEqual(state.form.preset, 'bertie');
  assert.strictEqual(m.intro.id, 'bertie');
  // Late events from the stopped clip change nothing.
  audios[0].fire('playing');
  audios[0].fire('ended');
  audios[0].fire('error');
  assert.deepStrictEqual(m.intro, { id: 'bertie', state: 'loading', error: '' });
  // The speaker button switches the same way, without selecting.
  m.maTogglePresetIntro('melie');
  assert.strictEqual(audios[1].pauses, 1);
  assert.strictEqual(audios[2].src, '/api/meetings/persona/preview?preset=melie');
  assert.strictEqual(state.form.preset, 'bertie', 'the speaker does not select');
});

test('clicking the playing card, or its speaker, stops it', () => {
  const { m, audios } = setup(() => jsonReply(200, {}));
  m.maTogglePresetIntro('betty');
  audios[0].fire('playing');
  m.maTogglePresetIntro('betty');
  assert.strictEqual(audios[0].pauses, 1);
  assert.deepStrictEqual(m.intro, { id: '', state: '', error: '' });
  assert.strictEqual(audios.length, 1, 'stopping starts nothing');
  m.maPickPreset('betty');
  audios[1].fire('playing');
  m.maPickPreset('betty');
  assert.strictEqual(audios[1].pauses, 1);
  assert.strictEqual(audios.length, 2, 'picking the playing card stops it instead of replaying');
  assert.strictEqual(m.audio, null);
  m.maPickPreset('nobody');
  m.maTogglePresetIntro('nobody');
  assert.strictEqual(audios.length, 2, 'an unknown preset plays nothing');
});

test('a clip that cannot play shows the server reason and never uses speech synthesis', async () => {
  const { m, audios, fetched } = setup(() => jsonReply(503, { ok: false, error: 'TTS not configured: set meetings.tts_url (and its token) to play samples' }));
  m.maPickPreset('betty');
  audios[0].fire('error');
  await flush();
  assert.deepStrictEqual(fetched, ['/api/meetings/persona/preview?preset=betty']);
  assert.deepStrictEqual(m.intro, { id: '', state: '', error: 'Voice preview is unavailable: TTS not configured: set meetings.tts_url (and its token) to play samples' });
  assert.strictEqual(m.audio, null);
  assert.ok(!m.maPresetCardHtml(PRESETS[0], true).includes('is-playing'));
  assert.ok(!/speechSynthesis|SpeechSynthesisUtterance/.test(PREVIEW_SOURCE), 'no browser speech synthesis fallback');
  assert.ok(html.includes('role="alert" data-ma-intro-status'), 'the reason is announced');
});

test('other failures are reported plainly', async () => {
  let s = setup(() => jsonReply(429, {}));
  s.m.maTogglePresetIntro('betty');
  s.audios[0].fire('error');
  await flush();
  assert.strictEqual(s.m.intro.error, 'Voice preview is unavailable: HTTP 429');

  s = setup(() => jsonReply(200, {}));
  s.m.maTogglePresetIntro('betty');
  s.audios[0].fire('error');
  await flush();
  assert.strictEqual(s.m.intro.error, 'Voice preview is unavailable: the browser could not play the clip');

  s = setup(() => Promise.reject(new Error('Failed to fetch')));
  s.m.maTogglePresetIntro('betty');
  s.audios[0].fire('error');
  await flush();
  assert.strictEqual(s.m.intro.error, 'Voice preview is unavailable: Failed to fetch');

  // The browser refusing playback is reported, not worked around.
  s = setup(() => jsonReply(200, {}));
  s.FakeAudio.nextPlay = Promise.reject(Object.assign(new Error('blocked'), { name: 'NotAllowedError' }));
  s.m.maTogglePresetIntro('betty');
  await flush();
  assert.strictEqual(s.m.intro.error, 'Voice preview is unavailable: the browser blocked playback. Click the card again.');
  assert.strictEqual(s.fetched.length, 0);

  // Stopping a clip rejects play() with AbortError: not an error to show.
  s = setup(() => jsonReply(200, {}));
  s.FakeAudio.nextPlay = Promise.reject(Object.assign(new Error('aborted'), { name: 'AbortError' }));
  s.m.maTogglePresetIntro('betty');
  await flush();
  assert.deepStrictEqual(s.m.intro, { id: 'betty', state: 'loading', error: '' });
});

test('a failure is dropped when another card was clicked meanwhile, and a new click clears the message', async () => {
  let release;
  const s = setup(() => new Promise(resolve => { release = () => resolve({ ok: false, status: 502, json: () => Promise.resolve({ error: 'TTS service: connection refused' }) }); }));
  s.m.maTogglePresetIntro('betty');
  s.audios[0].fire('error');
  s.m.maTogglePresetIntro('bertie');
  release();
  await flush();
  assert.deepStrictEqual(s.m.intro, { id: 'bertie', state: 'loading', error: '' });

  s.audios[1].fire('error');
  await flush();
  release();
  await flush();
  assert.strictEqual(s.m.intro.error, 'Voice preview is unavailable: TTS service: connection refused');
  s.m.maTogglePresetIntro('melie');
  assert.strictEqual(s.m.intro.error, '', 'a new click clears the old message');
});
