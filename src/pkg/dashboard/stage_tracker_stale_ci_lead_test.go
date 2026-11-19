package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// hivecommons/promptargs#467 at stage/060-reviewed with every check on the
// head concluded (one required name never reported, Tide pending): the row
// lights Review, not CI, and a backend `unknown` word beside a 2/3 ring is
// not inferred motion (hivecommons/hive#17508). A red check at 060 still
// makes CI the blocker.
func TestStageTrackerReviewedRowWithConcludedChecksLightsReview(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	html := indexHTML(t)
	script := strings.Join([]string{
		"const assert = require('assert');",
		jsFunc(t, html, "stageTrackerWaitingOnPerson"),
		jsFunc(t, html, "stageTrackerTrackRunning"),
		jsFunc(t, html, "stageTrackerStageTrack"),
		jsFunc(t, html, "stageTrackerStalledTrack"),
		jsFunc(t, html, "stageTrackerTrackWouldLight"),
		jsFunc(t, html, "stageTrackerLeadTrack"),
	}, "\n") + `
const ci = state => ({ head: '045b8e1', state, active: false, checks_total: 3, checks_passed: 2, checks_failed: 0, checks_pending: ['build-and-test (22)'], blocker: 'build-and-test (22)' });
const row = (stage, ciState, reviewState) => ({ kind: 'pr', repo: 'hivecommons/promptargs', number: 467, head: '045b8e1', stage, ci_activity: ci(ciState), review_activity: { head: '045b8e1', state: reviewState } });

for (const st of ['missing', 'unknown', 'passing']) {
  const item = row(60, st, 'approved');
  assert.strictEqual(stageTrackerTrackRunning(item, 'ci'), false, 'CI ' + st + ' with a 2/3 ring is not motion at 060');
  assert.strictEqual(stageTrackerStalledTrack(item), 'review', 'at 060 with CI ' + st + ' the stalled track is Review');
  assert.strictEqual(stageTrackerLeadTrack(item, true, stageTrackerStalledTrack(item)), 'review', 'Review is the one lit chevron at 060 with CI ' + st);
}
assert.strictEqual(stageTrackerStalledTrack(row(60, 'failing', 'approved')), 'ci', 'a red check at 060 keeps CI the blocker');
// 050 keeps its rule: CI not green is the blocker when the review has settled.
assert.strictEqual(stageTrackerStalledTrack(row(50, 'missing', 'approved')), 'ci');
// Genuine motion still lights CI at 060.
const live = row(60, 'running', 'approved'); live.ci_activity.active = true;
assert.strictEqual(stageTrackerTrackRunning(live, 'ci'), true);
assert.strictEqual(stageTrackerLeadTrack(live, true, ''), 'ci');
`
	if out, err := runNodeStdin(node, script); err != nil {
		t.Fatalf("node fixture failed: %v\n%s", err, out)
	}
}
