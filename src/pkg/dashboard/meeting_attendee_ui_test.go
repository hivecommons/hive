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
		"fetch('/api/meetings/persona/sample', { method: 'POST'",
		`data-action="maPickPreset"`,
		`data-action="maPlaySample"`,
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

// jsConstBlock returns a const declaration whose value may span lines (an
// object or array literal), through its closing bracket and semicolon.
func jsConstBlock(t *testing.T, html, name string) string {
	t.Helper()
	start := strings.Index(html, "const "+name+" = ")
	if start < 0 {
		t.Fatalf("index.html does not declare const %s", name)
	}
	body := start + len("const "+name+" = ")
	if c := html[body]; c != '{' && c != '[' {
		end := strings.Index(html[body:], ";")
		return html[start : body+end+1]
	}
	depth := 0
	for i := body; i < len(html); i++ {
		switch html[i] {
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return html[start:i+1] + ";"
			}
		}
	}
	t.Fatalf("const %s never closes", name)
	return ""
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
		"const document = { getElementById: () => null, querySelector: () => null };",
		"let _maState = null;",
	}
	for _, c := range []string{"MA_LANGUAGE_LABELS", "MA_FLAG_BASE", "MA_PERSONA_FIELDS", "MA_LIST_FIELDS", "MA_CAP_FIELDS"} {
		parts = append(parts, jsConstBlock(t, html, c))
	}
	for _, f := range []string{"maLanguageLabel", "maFlag", "maRenderIntro", "renderGovMeetingAttendee", "maSetData", "maPreset",
		"maOverrideNote", "maPresetCardHtml", "maField", "maTextInput", "maVoiceOptions", "maLanguageOptions", "maVoiceTypeOptions",
		"maPersonaHtml", "maBehaviourHtml", "maEmailHtml", "maRender", "maRefreshPreview", "maInput", "maCapInput",
		"maLanguageChanged", "maPickPreset", "maRequestBody"} {
		parts = append(parts, jsFunc(t, html, f))
	}
	parts = append(parts,
		"const data = "+string(data)+";",
		`assert.strictEqual(maFlag('en-GB'), '🇬🇧'); assert.strictEqual(maFlag('pt-BR'), '🇧🇷'); assert.strictEqual(maFlag('es'), '');`,
		`assert.ok(renderGovMeetingAttendee().includes('meeting-attendee-inline'));`,
		`assert.strictEqual(maRenderIntro({intro_template: 'I am {display_name} ({pronouns}); say "{leave}". {nope}', display_name: 'Betty Bee', pronouns: 'she/her', leave_phrases: ['betty leave']}), 'I am Betty Bee (she/her); say "betty leave". {nope}');`,
		`maSetData(data);`,
		`let f = _maState.form;`,
		`assert.strictEqual(f.preset, 'betty'); assert.strictEqual(f.voice, 'af_heart');`,
		`let html = maPersonaHtml(f);`,
		`assert.ok(html.includes('data-arg0="betty" aria-pressed="true"'), 'betty card selected');`,
		`assert.ok(html.includes('value="af_heart" selected'), 'current voice selected');`,
		`assert.ok(!html.includes('value="bf_emma"'), 'voices filtered to the language');`,
		`assert.ok(/value="bot" disabled>Bot-style \(coming later\)/.test(html), 'bot-style listed as coming later');`,
		`assert.ok(html.includes('disabled title="Set meetings.tts_url'), 'sample disabled without TTS');`,
		`maPickPreset('melie'); f = _maState.form;`,
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
		`assert.ok(email.includes('data-ma-email="calendar_invites" disabled>'), 'toggles disabled until email-in');`,
		`_maState.data.email = { available: true, address: 'hive@h.example', calendar_invites: true, inviters: [{login: 'bob', role: 'read-write', display_name: 'Bob', emails: [{address: 'bob@example.com', proof: 'verified'}]}], approvals: [{kind: 'invite', from: 'x@y', subject: 'Sync'}] };`,
		`email = maEmailHtml();`,
		`assert.ok(!email.includes('Available once email-in'), 'no pending note once available');`,
		`assert.ok(email.includes('data-ma-email="calendar_invites" checked>'), 'toggle enabled');`,
		`assert.ok(email.includes('bob@example.com') && email.includes('ma-verified') && email.includes('invite from x@y'), email);`,
		`_maState.data.overridden = { name: 'env', pronouns: 'config' };`,
		`assert.ok(maOverrideNote('name').includes('env') && maOverrideNote('pronouns').includes('custom') && maOverrideNote('voice') === '');`,
		`assert.ok(maBehaviourHtml(_maState.form).includes('data-ma-cap="answer_wait_s"'));`,
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
