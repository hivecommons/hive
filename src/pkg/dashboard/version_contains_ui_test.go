package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The version menu's "Does this build contain PR #N?" block (#18915) is
// rendered by functions inside the generated index.html. This runs the shipped
// functions in node. The slice needs stubs: escapeHtml and versionSameCommit
// are real; window stands in for the browser global the block keeps its state in.
func TestVersionContainsUIRenders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: version contains rendering was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString("const window = {};\n")
	for _, name := range []string{"escapeHtml", "versionShortSHA", "versionSameCommit", "versionContainsResultHTML", "versionBuildBlockHTML", "versionContainsDraft"} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`
const assert = (cond, msg) => { if (!cond) throw new Error(msg); };

const yes = versionContainsResultHTML({contains: 'yes', query: {kind: 'pr', pr: 7}, commit: '1a2b3c4d5e6f'});
assert(yes.includes('is-yes') && yes.includes('PR #7') && yes.includes('1a2b3c4'), 'yes: ' + yes);

const no = versionContainsResultHTML({contains: 'no', query: {kind: 'commit', commit: 'abcdef1234'}});
assert(no.includes('is-no') && !no.includes('is-yes'), 'no: ' + no);

// Anything that is not an explicit yes or no must read as unknown, never as a confident answer.
for (const odd of [undefined, '', 'maybe', 'YES', null]) {
  const out = versionContainsResultHTML({contains: odd, query: {kind: 'pr', pr: 9}, reason: 'the commit is not in the repository history'});
  assert(out.includes('is-unknown') && !out.includes('is-yes') && !out.includes('is-no'), 'odd verdict ' + odd + ': ' + out);
  assert(out.includes('not in the repository history'), 'reason must be shown: ' + out);
}

const failed = versionContainsResultHTML({error: 'Lookup failed; try again.'});
assert(!failed.includes('oc-version-contains-chip'), 'a failed lookup must show no verdict: ' + failed);

const escaped = versionContainsResultHTML({contains: 'unknown', query: {kind: 'pr', pr: 1}, reason: '<img src=x onerror=1>'});
assert(!escaped.includes('<img'), 'reason must be escaped: ' + escaped);

// Newest image and newest merged are separate rows; a merged commit that is
// not the image commit is called out.
const split = versionBuildBlockHTML({target: {short: '1111111'}, latestShort: '2222222'});
assert(split.includes('Newest image') && split.includes('1111111'), 'image row: ' + split);
assert(split.includes('Newest merged') && split.includes('2222222 (not in the newest image)'), 'merged row: ' + split);
const same = versionBuildBlockHTML({target: {short: '1111111'}, latestShort: '1111111'});
assert(!same.includes('not in the newest image'), 'equal commits must not be flagged: ' + same);

// The typed query survives a re-render, which happens on every version poll.
versionContainsDraft('1234');
assert(versionBuildBlockHTML({}).includes('value="1234"'), 'draft query must persist');
assert(versionBuildBlockHTML({}).includes('data-action="runVersionContains"'), 'check button must dispatch through data-action');
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("version contains renderer failed: %v\n%s", err, out)
	}
}
