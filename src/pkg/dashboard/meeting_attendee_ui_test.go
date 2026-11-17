package dashboard

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// TestSettingsMeetingAttendeeTabWiring pins Settings, Meeting attendee
// (hivecommons/hive#18396): the tab right after Meetings, its render and
// lazy load, the search entry, the persona API calls and the three sections.
func TestSettingsMeetingAttendeeTabWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		"'Project Observability', 'Meetings', 'Meeting attendee', 'Advisory'",
		"case 'Meeting attendee': return renderGovMeetingAttendee();",
		"if (tabId === 'Meeting attendee') loadMeetingAttendeeInline();",
		"{ tab: 'Meeting attendee', section: 'Meeting attendee', label: 'Meeting attendee persona', key: 'meetings.persona.preset', lazy: true",
		"yamlHint('meetings.persona')",
		`id="meeting-attendee-inline"`,
		"fetch('/api/meetings/persona')",
		"fetch('/api/meetings/persona', { method: 'PUT'",
		"return '/api/meetings/persona/sample?' + q.toString();",
		`data-action="maPickPreset"`,
		`data-action="maPlaySample"`,
		`data-action="maTogglePresetIntro"`,
		"return '/api/meetings/persona/preview?preset=' + encodeURIComponent(id);",
		"'Play introduction for '",
		"const MA_INTRO_UNAVAILABLE = 'Voice preview is unavailable: ';",
		`role="alert" data-ma-intro-status`,
		".ma-preset.is-playing {",
		`data-action="maCopyAddress"`,
		`data-action="maSave"`,
		`data-change-action="maLanguageChanged"`,
		`aria-labelledby="ma-persona-h"`,
		`aria-labelledby="ma-behaviour-h"`,
		`aria-labelledby="ma-email-h"`,
		"Available once email-in is enabled.",
		".ma-presets {",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("Meeting attendee wiring missing %q", want)
		}
	}
}

// TestMeetingAttendeeTabBehaviour executes the tab's rules against the real
// preset and voice catalog: flags, the intro preview, the voice list
// filtered by language, picking a preset, a language change keeping a voice
// of the same type, bot-style shown as coming later, and the Email section
// disabled until email-in is available.
func TestMeetingAttendeeTabBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — the Meeting attendee rules were NOT executed by this run")
	}
	s, _ := apiServer(t)
	data, err := json.Marshal(personaGet(t, s))
	if err != nil {
		t.Fatal(err)
	}
	html := indexHTML(t)
	parts := []string{
		"const assert = require('assert');",
		"const escapeHtml = s => String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/\"/g, '&quot;');",
		"const yamlHint = p => '<code>' + p + '</code>';",
		"const document = { getElementById: () => null, querySelector: () => null, querySelectorAll: () => [] };",
		"let _maState = null;",
		// The preset voice preview (#18710): a recording Audio stand-in.
		"let _maIntro = { id: '', state: '', error: '' }; let _maIntroAudio = null; const _audios = [];",
		"class Audio { constructor(src) { this.src = src; this.paused = true; _audios.push(this); } addEventListener() {} play() { this.paused = false; return Promise.resolve(); } pause() { this.paused = true; } }",
		"let _owner = true; const viewerIsOwner = () => _owner;",
	}
	for _, c := range []string{"MA_LANGUAGE_LABELS", "MA_FLAG_BASE", "MA_PERSONA_FIELDS", "MA_LIST_FIELDS", "MA_CAP_FIELDS",
		"MA_INTRO_PLAY_ICON", "MA_INTRO_STOP_ICON", "MA_INTRO_UNAVAILABLE"} {
		parts = append(parts, jsConstBlock(t, html, c))
	}
	for _, f := range []string{"maLanguageLabel", "maFlag", "maRenderIntro", "renderGovMeetingAttendee", "maSetData", "maPreset",
		"maOverrideNote", "maPresetCardHtml", "maField", "maTextInput", "maVoiceOptions", "maLanguageOptions", "maVoiceTypeOptions",
		"maPersonaHtml", "maBehaviourHtml", "maEmailHtml", "maRender", "maRefreshPreview", "maInput", "maCapInput",
		"maLanguageChanged", "maPickPreset", "maRequestBody", "maSampleSrc", "maMyEmailsHtml", "maEmailToggle",
		"maProofFailureHtml", "maProofAuthSummary", "maProofAttemptsHtml",
		"maIntroSrc", "maIntroState", "maIntroLabel", "maIntroSet", "maIntroSync", "maStopIntro", "maPlayIntro", "maIntroFailed", "maTogglePresetIntro"} {
		parts = append(parts, jsFunc(t, html, f))
	}
	parts = append(parts,
		"const data = "+string(data)+";",
		`assert.strictEqual(maFlag('en-GB'), '🇬🇧'); assert.strictEqual(maFlag('pt-BR'), '🇧🇷'); assert.strictEqual(maFlag('es'), '');`,
		`assert.ok(renderGovMeetingAttendee().includes('meeting-attendee-inline'));`,
		`assert.strictEqual(maRenderIntro({intro_template: 'I am {display_name} ({pronouns}); say "{leave}". {nope}', display_name: 'Betty Bee', pronouns: 'she/her', leave_phrases: ['betty leave']}), 'I am Betty Bee; say "betty leave". {nope}');`,
		`assert.strictEqual(maRenderIntro({intro_template: 'Hi, I am {display_name} (she/her).', display_name: 'Betty Bee'}), 'Hi, I am Betty Bee.');`,
		`maSetData(data);`,
		`let f = _maState.form;`,
		`assert.strictEqual(f.preset, 'betty'); assert.strictEqual(f.voice, 'af_heart');`,
		`let html = maPersonaHtml(f);`,
		`assert.ok(html.includes('data-arg0="betty" aria-pressed="true"'), 'betty card selected');`,
		`assert.ok(html.includes('value="af_heart" selected'), 'current voice selected');`,
		`assert.ok(!html.includes('value="bf_emma"'), 'voices filtered to the language');`,
		`assert.ok(/value="bot" disabled>Bot-style \(coming later\)/.test(html), 'bot-style listed as coming later');`,
		`assert.ok(html.includes('disabled title="Set meetings.tts_url'), 'sample disabled without TTS');`,
		// Every real preset card offers its introduction; none plays on render.
		`for (const p of data.presets) assert.ok(html.includes('data-action="maTogglePresetIntro" data-arg0="' + p.id + '" data-ma-preset-play aria-label="Play introduction for ' + escapeHtml(p.display_name) + '"'), p.id);`,
		`assert.ok(html.includes('Pick one to hear its introduction') && html.includes('data-ma-intro-status'), 'preview hint and status');`,
		`assert.strictEqual(_audios.length, 0, 'no autoplay on render');`,
		`maPickPreset('melie'); f = _maState.form;`,
		`assert.strictEqual(_audios.length, 1); assert.strictEqual(_audios[0].src, '/api/meetings/persona/preview?preset=melie'); assert.strictEqual(_audios[0].paused, false);`,
		`assert.strictEqual(_maIntro.id, 'melie'); maStopIntro(); assert.strictEqual(_audios[0].paused, true); assert.strictEqual(_maIntro.id, '');`,
		`assert.strictEqual(f.language, 'fr-FR'); assert.strictEqual(f.voice, 'ff_siwis'); assert.ok(f.intro_template.startsWith('Bonjour'));`,
		`assert.strictEqual(f.aliases.join(','), 'mélie,melie,abeille,hive');`,
		`maPickPreset('nobody'); assert.strictEqual(_maState.form.preset, 'melie');`,
		`maLanguageChanged('ja-JP'); f = _maState.form;`,
		`assert.strictEqual(f.voice, 'jf_alpha'); assert.strictEqual(f.accent, 'Standard Japanese');`,
		`f.voice_type = 'male'; maLanguageChanged('en-GB');`,
		`assert.strictEqual(_maState.form.voice.slice(0, 2), 'bm', 'a language change keeps the voice type');`,
		`maInput('aliases', ' a, b ,, '); assert.deepStrictEqual(_maState.form.aliases, ['a', 'b']);`,
		`maInput('enabled', false); assert.strictEqual(_maState.form.enabled, false);`,
		`maInput('name', 'Zed'); maCapInput('per_10m', '5'); maCapInput('stale_s', '');`,
		`const body = maRequestBody();`,
		`assert.strictEqual(body.name, 'Zed'); assert.strictEqual(body.unprompted.per_10m, 5); assert.strictEqual(body.unprompted.stale_s, 0);`,
		`assert.strictEqual(body.preset, 'melie'); assert.strictEqual(body.enabled, false); assert.ok('voice_on_address' in body);`,
		`let email = maEmailHtml();`,
		`assert.ok(email.includes('Available once email-in is enabled'), 'pending note');`,
		`assert.ok(email.includes('data-arg0="calendar_invites" data-arg-types="s,c" disabled>'), 'toggles disabled until email-in');`,
		`assert.ok(!email.includes('My calendar emails'), 'no own-email list until email-in');`,
		`maSetData(Object.assign({}, _maState.data, { email: { available: true, address: 'hive@h.example', calendar_invites: true, inviters: [{login: 'bob', role: 'read-write', display_name: 'Bob', emails: [{address: 'bob@example.com', proof: 'verified'}]}], approvals: [{kind: 'invite', from: 'x@y', subject: 'Sync'}] } }));`,
		`email = maEmailHtml();`,
		`assert.ok(!email.includes('Available once email-in'), 'no pending note once available');`,
		`assert.ok(email.includes('data-arg0="calendar_invites" data-arg-types="s,c" checked>'), 'toggle enabled for the owner');`,
		`assert.ok(email.includes('bob@example.com') && email.includes('ma-verified') && email.includes('invite from x@y'), email);`,
		`assert.ok(email.includes('My calendar emails') && email.includes('Loading'), 'own list loading');`,
		`_maState.mine = { address: 'hive@h.example', emails: [{ email: 'me@x.org', proven: false, code: 'hive-proof-aaaaaaaaaaaa', active: true, last_error: 'the proof mail came from a different address' }, { email: 'old@x.org', proven: true, active: false }] };`,
		`email = maEmailHtml();`,
		`assert.ok(email.includes('hive-proof-aaaaaaaaaaaa') && email.includes('data-action="maRemoveMyEmail" data-arg0="me@x.org"') && email.includes('different address') && email.includes('not on the allowlist'), email);`,
		// A failed proof shows the server's plain-language reason, the auth
		// results and Check again (#18579); the owner's unknown-code attempts
		// show masked.
		`_maState.mine = { address: 'a', emails: [{ email: 'me@x.org', proven: false, code: 'c', last_error: 'raw', last_failure: { reason: 'not_aligned', at: '2026-11-16T12:00:00Z', message: 'Your mail server did not authenticate this address: neither SPF nor DKIM passed for x.org.', from_domain: 'x.org', spf: 'softfail', spf_domain: 'x.org', dkim: [{ domain: 'x.org', result: 'fail' }], aligned: false } }], recent_proof_attempts: [{ at: '2026-11-16T12:01:00Z', from: 't***@y.org', reason: 'code not recognized' }] };`,
		`email = maEmailHtml();`,
		`assert.ok(email.includes('data-ma-proof-failure="not_aligned"') && email.includes('neither SPF nor DKIM passed for x.org') && !email.includes('>raw<'), email);`,
		`assert.ok(email.includes('SPF softfail for x.org; DKIM fail for x.org; aligned with x.org: no.'), email);`,
		`assert.ok(email.includes('data-action="maLoadMyEmails">Check again'), 'check again');`,
		`assert.ok(email.includes('(11/16 12:00') && email.includes('Recent proof attempts') && email.includes('t***@y.org') && email.includes('11/16 12:01') && email.includes('code not recognized'), email);`,
		`assert.strictEqual(maProofAuthSummary({ aligned: true }), 'What the mail server saw: SPF none; DKIM none; aligned with the From domain: yes.');`,
		`assert.strictEqual(maProofFailureHtml({ last_failure: { reason: 'expired', message: 'The code expired. Remove and add the address again.' } }).includes('The code expired'), true);`,
		`assert.strictEqual(maProofFailureHtml({}), ''); assert.ok(maProofAttemptsHtml(null) === '' && maProofAttemptsHtml({ recent_proof_attempts: [{}] }).includes('unknown sender'));`,
		`_maState.mine = { emails: [] }; assert.ok(maEmailHtml().includes('not added a calendar email'));`,
		`_maState.mine = { error: 'boom' }; assert.ok(maEmailHtml().includes('boom'));`,
		`_maState.mine = { address: 'a', emails: [{ email: 'e@x.org', proven: false }] }; assert.ok(maEmailHtml().includes('code expired'));`,
		`assert.ok(!('email' in maRequestBody()), 'untouched switches are not sent');`,
		`maEmailToggle('mail_in_files', true); assert.deepStrictEqual(maRequestBody().email, { calendar_invites: true, mail_in_files: true });`,
		`_owner = false; email = maEmailHtml(); assert.ok(email.includes('Only the owner') && email.includes('data-arg-types="s,c" checked disabled>'), 'switches are owner-only');`,
		`_maState.form.email = null; maEmailToggle('x', true); _owner = true;`,
		`_maState.data.overridden = { name: 'env', pronouns: 'config' };`,
		`assert.ok(maOverrideNote('name').includes('env') && maOverrideNote('pronouns').includes('custom') && maOverrideNote('voice') === '');`,
		`assert.ok(maBehaviourHtml(_maState.form).includes('data-ma-cap="answer_wait_s"'));`,
		"assert.strictEqual(maSampleSrc({voice: 'jf_alpha', language: 'ja-JP', display_name: 'ミツ & co'}), '/api/meetings/persona/sample?voice=jf_alpha&language=ja-JP&display_name=%E3%83%9F%E3%83%84+%26+co');",
		`maRender(); maRefreshPreview();`,
		`console.log('ok');`,
	)
	out, err := runNodeStdin(node, strings.Join(parts, "\n"))
	if err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("Meeting attendee rules failed: %v\n%s", err, out)
	}
	if len(config.KokoroVoices()) == 0 {
		t.Fatal("no voices")
	}
}
