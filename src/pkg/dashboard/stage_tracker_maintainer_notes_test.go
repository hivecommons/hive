package dashboard

import (
	"strings"
	"testing"
)

// Maintainer notes on the Activity Monitor rows: the markup, CSS and SSE
// wiring are in the page, and the pure renderers behave (count badge only
// when ≥1, nothing for a signed-out visitor, Delete only for the author
// or an owner, composer only with write on the repo) — executed under node.
func TestStageTrackerMaintainerNotesWiring(t *testing.T) {
	html := indexHTML(t)
	for _, want := range []string{
		`class="ham-notes-btn${n ? ' ham-notes-has' : ''}" data-action="hamNotesToggle" data-arg-types="e,t"`,
		`<span class="ham-notes-count">${n}</span>`,
		`stageTrackerNotesButtonHtml(item, hamNotesCtx(item))`,
		`stageTrackerNotesButtonHtml(pr, hamNotesCtx(pr))`,
		`stageTrackerNotesPanelHtml(item, hamNotesCtx(item))`,
		`stageTrackerNotesPanelHtml(pr, hamNotesCtx(pr))`,
		`<textarea class="ham-notes-input" rows="1" maxlength="${MAINTAINER_NOTES_MAX}"`,
		`data-keydown-action="hamNotesKeydown" data-keys="Enter"`,
		`class="hv-btn btn-secondary ham-notes-submit" data-action="hamNoteAdd"`,
		`class="ham-note-del" data-action="hamNoteDelete"`,
		`fetch('/api/notes', { method: 'POST'`,
		`if (id === 'notes') {`,
		`#activity-monitor-section .ham-notes-btn:not(.ham-notes-has) { display: none; }`,
		`fetch('/api/notes/' + encodeURIComponent(id), { method: 'DELETE' })`,
		`source.addEventListener('maintainer-notes', (e) => {`,
		`lines.push(nn + ' maintainer note' + (nn === 1 ? '' : 's') + ' (private to this hive)')`,
		`stageTrackerRowHoldsComposer(prev)`,
		`.ham-notes-btn {`, `.ham-notes {`, `.ham-notes-input {`,
		`#activity-monitor-section .ham-notes-btn, #activity-monitor-section .ham-note-del, #activity-monitor-section .ham-notes-submit, #activity-monitor-section .ham-notes-input {`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html lacks %q", want)
		}
	}
}

func TestStageTrackerMaintainerNotesRenderNode(t *testing.T) {
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = { _hiveRole: 'write' };",
		"function esc(s){return String(s==null?'':s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/\"/g,'&quot;');}",
		"var _ctx = { user: true, login: 'alice', canHold: true };",
		"function stageTrackerActionCtx(){ return _ctx; }",
		"function dashboardRoleAtLeast(role, want){ return role === 'owner' && want === 'owner'; }",
		jsConstLine(t, html, "STAGE_TRACKER_WHY_WRITE"),
		jsFunc(t, html, "stageTrackerRefOf"),
		jsConstLine(t, html, "MAINTAINER_NOTES_MAX"),
		jsConstLine(t, html, "MAINTAINER_NOTES_GLYPH"),
		jsConstLine(t, html, "MAINTAINER_NOTES_AVATAR_PX"),
		jsConstLine(t, html, "MAINTAINER_NOTES_PRIVACY"),
		"var _hamNotes = {}; var _hamNotesOpen = new Set(); var _hamNotesDraft = {};",
		jsFunc(t, html, "hamNotesKey"),
		jsFunc(t, html, "hamNotesPanelId"),
		jsFunc(t, html, "hamNotesIndex"),
		jsFunc(t, html, "hamNotesFor"),
		jsFunc(t, html, "hamNotesCount"),
		jsFunc(t, html, "hamNotesCtx"),
		jsFunc(t, html, "hamNotesPhoneTier"),
		jsFunc(t, html, "stageTrackerNotesButtonHtml"),
		jsFunc(t, html, "stageTrackerNotesMenuItem"),
		jsFunc(t, html, "stageTrackerNoteHtml"),
		jsFunc(t, html, "stageTrackerNotesPanelHtml"),
		`
const assert = require('assert');
const item = { repo: 'HiveCommons/Hive', number: 42, kind: 'issue' };
const pr = { repo: 'hivecommons/hive', number: 43, kind: 'pr' };

// Empty: a button without a badge; the tooltip names the privacy.
let b = stageTrackerNotesButtonHtml(item, hamNotesCtx(item));
assert.ok(b.includes('class="ham-notes-btn"'), 'plain button: ' + b);
assert.ok(!b.includes('ham-notes-count'), 'no badge when empty');
assert.ok(b.includes('aria-expanded="false"'));
assert.ok(b.includes('not posted to GitHub'), 'privacy in tooltip');

// Signed out: nothing at all.
_ctx = { user: false, login: '', canHold: null };
assert.strictEqual(stageTrackerNotesButtonHtml(item, hamNotesCtx(item)), '');
assert.strictEqual(stageTrackerNotesPanelHtml(item, hamNotesCtx(item)), '');
_ctx = { user: true, login: 'alice', canHold: true };

// Index: keyed case-insensitively, oldest first, bad rows dropped.
_hamNotes = hamNotesIndex([
  { id: 'n2', repo: 'hivecommons/hive', number: 42, author: 'bob', text: 'second', created_at: '2026-01-02T00:00:00Z' },
  { id: 'n1', repo: 'hivecommons/hive', number: 42, author: 'alice', text: 'first <b>', created_at: '2026-01-01T00:00:00Z' },
  { id: 'n3', repo: 'hivecommons/hive', number: 43, author: 'carol', text: 'pr note', created_at: '2026-01-03T00:00:00Z' },
  { id: 'bad', repo: '', number: 0 },
]);
assert.strictEqual(hamNotesCount(item), 2);
assert.strictEqual(hamNotesCount(pr), 1);
assert.deepStrictEqual(hamNotesFor(item).map(n => n.id), ['n1', 'n2']);

b = stageTrackerNotesButtonHtml(item, hamNotesCtx(item));
assert.ok(b.includes('class="ham-notes-btn ham-notes-has"'), 'has-class: ' + b);
assert.ok(b.includes('<span class="ham-notes-count">2</span>'), 'badge 2: ' + b);
assert.ok(b.includes('2 maintainer notes.'), 'tooltip count: ' + b);

// The ⋯ menu entry carries the count; a phone card without notes draws
// no badge at all (the menu is the way in there).
assert.strictEqual(stageTrackerNotesMenuItem(item).label, 'Maintainer notes (2)\u2026');
assert.strictEqual(stageTrackerNotesMenuItem(pr).id, 'notes');
global.matchMedia = () => ({ matches: true });
assert.strictEqual(stageTrackerNotesButtonHtml({ repo: 'hivecommons/hive', number: 99 }, hamNotesCtx(item)), '', 'phone: unnoted row has no badge');
assert.ok(stageTrackerNotesButtonHtml(item, hamNotesCtx(item)).includes('ham-notes-count'), 'phone: noted row keeps its badge');
delete global.matchMedia;

// Closed: no panel. Open: list + composer; alice deletes hers only.
assert.strictEqual(stageTrackerNotesPanelHtml(item, hamNotesCtx(item)), '');
_hamNotesOpen.add(hamNotesKey(item.repo, item.number));
let p = stageTrackerNotesPanelHtml(item, hamNotesCtx(item));
assert.ok(p.includes('id="' + hamNotesPanelId('hivecommons/hive#42') + '"'), 'panel id: ' + p);
assert.ok(p.includes('data-ham-notes="2"'));
assert.ok(p.includes('first &lt;b&gt;'), 'escaped text');
assert.ok(p.includes('@alice') && p.includes('@bob'));
assert.ok(p.includes('github.com/alice.png'), 'avatar');
assert.ok(p.includes('class="cue-event-age ham-note-when" data-at="' + Date.parse('2026-01-01T00:00:00Z') + '"'), 'relative time hook');
assert.strictEqual((p.match(/class="ham-note-del"/g) || []).length, 1, 'alice deletes only her own: ' + p);
assert.ok(p.includes('aria-label="Delete note by @alice"'));
assert.ok(p.includes('class="ham-notes-input"') && p.includes('ham-notes-submit'), 'composer');
assert.ok(stageTrackerNotesButtonHtml(item, hamNotesCtx(item)).includes('aria-expanded="true"'));

// An owner may delete every note.
window._hiveRole = 'owner';
p = stageTrackerNotesPanelHtml(item, hamNotesCtx(item));
assert.strictEqual((p.match(/class="ham-note-del"/g) || []).length, 2, 'owner deletes all');
window._hiveRole = 'write';

// Read-only maintainer: sees the notes, no composer, told why.
_ctx = { user: true, login: 'dave', canHold: false };
p = stageTrackerNotesPanelHtml(item, hamNotesCtx(item));
assert.ok(!p.includes('ham-notes-input'), 'no composer for reader');
assert.ok(p.includes('to add a note.'), 'why-not text');
assert.strictEqual((p.match(/class="ham-note-del"/g) || []).length, 0);
_ctx = { user: true, login: 'alice', canHold: true };

// Draft survives a re-render, escaped.
_hamNotesDraft['hivecommons/hive#42'] = 'wip <draft>';
p = stageTrackerNotesPanelHtml(item, hamNotesCtx(item));
assert.ok(p.includes('>wip &lt;draft&gt;</textarea>'), 'draft kept: ' + p);

// Empty, open: the empty line with the ref.
_hamNotesOpen.add('hivecommons/hive#44');
p = stageTrackerNotesPanelHtml({ repo: 'hivecommons/hive', number: 44 }, hamNotesCtx(item));
assert.ok(p.includes('No maintainer notes on hivecommons/hive#44 yet.'), p);
`,
	}, "\n")
	runStageTrackerRowActionsScript(t, script)
}
