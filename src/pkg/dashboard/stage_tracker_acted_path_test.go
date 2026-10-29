package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// The ✓ hover for a delegated 090 decision reads the path in plain words
// (hivecommons/hive#15173): "the hive decided: approve · review passed, CI
// green, mergeable (rule: human merge approve)" — no slugs, no adoption
// words on the approve path.
func TestStageTrackerActedPlainDecisionPathJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH — acted-plain decision path was NOT executed by this run")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const window = {};",
		"const assert = require('assert');",
		jsConstBlock(t, html, "STAGE_TRACKER_ACTED_PLAIN"),
		jsFunc(t, html, "stageTrackerActedPlain"),
	}, "\n") + `
const approve = stageTrackerActedPlain(` + "`" + operatorActionDecidedDetail("approve", "human-merge-approve", "review passed, CI green, mergeable") + "`" + `);
assert.strictEqual(approve, 'the hive decided: approve · review passed, CI green, mergeable (rule: human merge approve)');
assert.ok(!/adopt/i.test(approve), approve);
const adopt = stageTrackerActedPlain(` + "`" + operatorActionDecidedDetail("adopt-supersede", "human-merge-adopt", "CI red, conflicting, needs rebase") + "`" + `);
assert.strictEqual(adopt, 'the hive decided: adopt-supersede · CI red, conflicting, needs rebase (rule: human merge adopt)');
assert.strictEqual(stageTrackerActedPlain(` + "`" + operatorActionDecidedDetail("approve", "default-release", "") + "`" + `), 'the hive decided: approve (rule: default release)');
console.log('ok');
`
	cmd := exec.Command(node, "-")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "ok") {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
