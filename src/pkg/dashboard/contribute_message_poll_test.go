package dashboard

import (
	"os/exec"
	"testing"
)

// Exercise the real render and form handlers with a minimal DOM: a poll must
// never replace a draft, and closing the final form must replay the latest data.
func TestContributorMessageSurvivesFleetPoll(t *testing.T) {
	body := opsPageBody(t)
	script := `
const assert = require('node:assert/strict');
var ccDeferredClankers = null;
const forms = [];
let replacements = 0, summaries = 0, polls = 0, responseOK = false, networkError = false;
const fleet = {
  querySelector() { return forms.find(f => f.host !== runs) || null; },
  set innerHTML(value) {
    replacements++; this.html = value;
    for (let i = forms.length - 1; i >= 0; i--) if (forms[i].host !== runs) forms.splice(i, 1);
  }
};
const count = {};
function host() { return {
  querySelector() { return forms.find(f => f.host === this) || null; },
  appendChild(f) { f.host = this; forms.push(f); }
}; }
const runs = host();
const document = {
  getElementById(id) { return {'clanker-list': fleet, 'clanker-count': count, 'runs-message-form': runs}[id]; },
  createElement() {
    const textarea = {value: '', selectionStart: 2};
    const button = {addEventListener(event, fn) { this.click = fn; }};
    return {querySelector(s) { return s === 'textarea' ? textarea : button; },
      remove() { const i = forms.indexOf(this); if (i >= 0) forms.splice(i, 1); }};
  }
};
function ccUpdateArmy() { summaries++; }
function ccRenderInterestRoster() {}
function esc(s) { return s; }
function toast() {}
function opsPoll() { polls++; }
function ccLoadOperatorMessages() {}
let sent;
function fetch(url, options) {
  sent = JSON.parse(options.body);
  return networkError ? Promise.reject(new Error('offline')) : Promise.resolve({ok: responseOK, json: () => Promise.resolve({error: 'failed'})});
}
` + jsFunc(t, body, "renderClankers") + "\n" +
		jsFunc(t, body, "ccCloseMessageForm") + "\n" +
		jsFunc(t, body, "ccToggleMessageForm") + `
(async function() {
  const a = host(), b = host();
  const buttonA = {parentNode: a, getAttribute() { return 'message'; }};
  const buttonB = {parentNode: b, getAttribute() { return 'message'; }};
  ccToggleMessageForm('a', 'alice', buttonA);
  const draft = a.querySelector();
  draft.querySelector('textarea').value = 'unfinished note';
  document.activeElement = draft.querySelector('textarea');
  ccToggleMessageForm('b', 'bob', buttonB);
  renderClankers([]);
  renderClankers([]);
  assert.equal(replacements, 0, 'poll replaced open drafts');
  assert.equal(summaries, 2, 'summary stopped polling');
  assert.equal(a.querySelector(), draft);
  assert.equal(document.activeElement.value, 'unfinished note');
  assert.equal(document.activeElement.selectionStart, 2);
  // Blurring the textarea must not make a draft expendable.
  document.activeElement = null;
  renderClankers([]);
  ccToggleMessageForm('b', 'bob', buttonB);
  assert.equal(replacements, 0, 'closing another form wiped the draft');
  // Both server and network failures retain the form and its text.
  draft.querySelector('button').click();
  await new Promise(setImmediate);
  assert.equal(a.querySelector(), draft);
  networkError = true;
  draft.querySelector('button').click();
  await new Promise(setImmediate);
  assert.equal(a.querySelector(), draft);
  assert.equal(draft.querySelector('textarea').value, 'unfinished note');
  networkError = false;
  responseOK = true;
  draft.querySelector('button').click();
  await new Promise(setImmediate);
  assert.deepEqual(sent, {contributor: 'a', text: 'unfinished note'});
  assert.equal(replacements, 1, 'successful send did not replay deferred empty fleet');
  assert.match(fleet.html, /No contributor agents/);
  assert.equal(polls, 1);
  // Toggle-close also catches up immediately, using the most recent snapshot.
  ccToggleMessageForm('a', 'alice', buttonA);
  const older = [{github_username: 'old'}];
  renderClankers(older);
  renderClankers([]);
  ccToggleMessageForm('a', 'alice', buttonA);
  assert.equal(replacements, 2);
  assert.match(fleet.html, /No contributor agents/);
  // The separate run-history form must not defer fleet updates.
  ccToggleMessageForm('alice', 'alice', {getAttribute() { return 'message-runs'; }});
  renderClankers([]);
  assert.equal(replacements, 3);
  assert.ok(runs.querySelector(), 'fleet update removed run-history form');
})().catch(err => { console.error(err); process.exitCode = 1; });
`
	out, err := exec.Command("node", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("message form poll regression: %v\n%s", err, out)
	}
}
