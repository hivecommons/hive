package dashboard

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestIntegrationExecutionCaptionPreservesExactRowsAndOff(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	source, err := os.ReadFile("static-src/js/tracker-integration-execution.js")
	if err != nil {
		t.Fatal(err)
	}
	script := `
const assert = require('node:assert/strict');
function esc(value){return String(value).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;');}
` + string(source) + `
const now=Date.parse('2026-10-31T04:00:00Z');
const item={kind:'pr',repo:'o/r',number:2,head:'bbb',base_ref:'v5',stage:60};
const original=JSON.stringify(item);
const candidate={kind:'candidate',repo:'o/r',base:'v5',subject_id:'subject',state:'passed',observed_at:new Date(now).toISOString(),members:[{pr:1,head:'aaa'},{pr:2,head:'bbb'}],tested_commit:'exact'};
const data={integration_execution:{mode:'enforce',observed_at:new Date(now).toISOString(),observation_age_seconds:60,candidates:[candidate]}};
stageTrackerExecutionSync(null,now);
assert.equal(stageTrackerExecutionHtml(item),'');
stageTrackerExecutionSync(data,now);
assert.match(stageTrackerExecutionHtml(item),/Integration CI passed/);
assert.match(stageTrackerExecutionHtml(item),/live merge guards still required/);
assert.equal(stageTrackerExecutionHtml({...item,number:1,head:'aaa'}),'','AB proof must not become A proof');
assert.equal(stageTrackerExecutionHtml({...item,head:'changed'}),'');
assert.equal(stageTrackerExecutionHtml({...item,base_ref:'other'}),'');
assert.equal(stageTrackerExecutionHtml({...item,base_ref:''}),'','unknown scope must not borrow a prefix receipt');
assert.equal(stageTrackerExecutionHtml({...item,kind:'issue'}),'');
assert.equal(JSON.stringify(item),original,'caption must not alter row/stage/order/filter facts');
stageTrackerExecutionSync(data,now+61000);
assert.match(stageTrackerExecutionHtml(item),/unknown \(stale observation\)/);
stageTrackerExecutionSync({integration_execution:{...data.integration_execution,mode:'advisory'}},now);
assert.match(stageTrackerExecutionHtml(item),/advisory, no writer/);
stageTrackerExecutionSync({integration_execution:{...data.integration_execution,mode:'off'}},now);
assert.equal(stageTrackerExecutionHtml(item),'');
candidate.reason='<script>unsafe</script>';
stageTrackerExecutionSync(data,now);
assert.ok(!stageTrackerExecutionHtml(item).includes('<script>'));
`
	command := exec.Command(node, "-")
	command.Stdin = strings.NewReader(script)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("execution caption regression: %v\n%s", err, output)
	}
	layout, err := os.ReadFile("static-src/js/tracker-row-layout.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, seam := range []string{"stageTrackerExecutionHtml(pr)", "stageTrackerExecutionHtml(shown)", "stageTrackerPreparationNodeHtml", "stageTrackerPreparationCommentHtml"} {
		if !strings.Contains(string(layout), seam) {
			t.Fatalf("lost root/nested/preparation seam %s", seam)
		}
	}
}
