package dashboard

import (
	"os/exec"
	"testing"
)

// Exercise the shipped lookup against proxy/login HTML, not just source markers.
func TestContributeDecisionsResponseHandling(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for the decisions response regression test")
	}
	body := renderContributePage(t)
	script := `
const assert = require('node:assert/strict');
const list = {}, count = {};
const document = {getElementById(id) { return id === 'decisions-list' ? list : count; }};
let ccDecUser, response, parsed, rendered;
function esc(s) { return String(s).replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
function ccRenderDecisions(user, data) { rendered = {user, data}; }
function fetch(url, options) {
  assert.equal(url, '/api/contribute/decisions?username=arteeh&limit=100');
  assert.equal(options.headers.Accept, 'application/json');
  return Promise.resolve(response);
}
` + jsFunc(t, body, "ccLookupDecisions") + `
async function lookup({status = 200, type = 'application/json', redirected = false, data = {decisions: []}, invalid = false} = {}) {
  parsed = false; rendered = null;
  response = {status, ok: status === 200, redirected,
    headers: {get() { return type; }},
    json() { parsed = true; return invalid ? Promise.reject(new SyntaxError("Unexpected token '<'")) : Promise.resolve(data); }};
  await ccLookupDecisions('arteeh');
}
(async () => {
  for (const redirected of [false, true]) {
    await lookup({type: 'text/html; charset=utf-8', redirected, invalid: true});
    assert.equal(parsed, false, 'HTML must not reach the JSON parser');
    assert.equal(rendered, null, 'HTML must not become an empty list');
    assert.match(list.innerHTML, /API routing/);
    assert.doesNotMatch(list.innerHTML, /Unexpected token/);
    if (redirected) assert.match(list.innerHTML, /Sign in again/);
  }
  await lookup({type: ''});
  assert.equal(parsed, false);
  assert.match(list.innerHTML, /unexpected response/);
  await lookup({status: 401});
  assert.equal(parsed, false);
  assert.match(list.innerHTML, /Sign in to view hub decisions/);
  await lookup({status: 403});
  assert.equal(parsed, false);
  assert.match(list.innerHTML, /owner and read-write users/);
  await lookup({status: 503});
  assert.equal(parsed, false);
  assert.match(list.innerHTML, /HTTP 503/);
  await lookup({invalid: true});
  assert.match(list.innerHTML, /returned invalid JSON/);
  assert.doesNotMatch(list.innerHTML, /Unexpected token/);
  await lookup({data: {}});
  assert.equal(rendered, null);
  assert.match(list.innerHTML, /unexpected response/);
  await lookup({type: 'Application/JSON; charset=utf-8'});
  assert.deepEqual(rendered, {user: 'arteeh', data: {decisions: []}});
  const data = {decisions: [{event: 'refused', detail: 'capacity'}]};
  await lookup({data});
  assert.deepEqual(rendered, {user: 'arteeh', data});
  // A response for an old lookup cannot overwrite the current contributor.
  rendered = null;
  const pending = ccLookupDecisions('arteeh');
  ccDecUser = 'another-user';
  await pending;
  assert.equal(rendered, null);
})().catch(err => { console.error(err); process.exit(1); });
`
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("decisions response regression: %v\n%s", err, out)
	}
}
