package dashboard

import (
	"os/exec"
	"strings"
	"testing"
)

// #14707: a disabled_repos entry that is a prefix of another tracked repo's
// name disables only the named repo. dakota-iso keeps serving work while
// dakota is withheld with disabled_repo.
func TestReadyQueue_DisabledRepoPrefixDoesNotDisableLongerName(t *testing.T) {
	for _, entry := range []string{"projectbluefin/dakota", "dakota", "ProjectBluefin/Dakota"} {
		t.Run(entry, func(t *testing.T) {
			hub, s := withheldHub(t, nil)
			s.statusMu.Lock()
			s.status.Repos = append(s.status.Repos, FrontendRepo{
				Name: "dakota-iso",
				Full: "projectbluefin/dakota-iso",
				ActionableIssues: []any{map[string]any{
					"number": float64(800),
					"title":  "iso work",
					"url":    "https://github.com/projectbluefin/dakota-iso/issues/800",
					"author": "someone",
				}},
			})
			s.statusMu.Unlock()
			s.deps.Config.Hub.DisabledRepos = []string{entry}

			assertQueue(t, hub, 800)
			assertAssigns(t, hub, 800)

			snap := hub.admissionQueueSnapshot(readyQueueDefaultLimit, withheldAll)
			for _, key := range []string{"projectbluefin/dakota#601", "projectbluefin/dakota#700"} {
				item, ok := findWithheld(snap.withheld, key)
				if !ok {
					t.Fatalf("%s is disabled but unexplained; collected %v", key, snap.withheld)
				}
				if item.Reason != withheldReasonDisabledRepo {
					t.Errorf("%s reason = %q, want %q", key, item.Reason, withheldReasonDisabledRepo)
				}
			}
			if item, ok := findWithheld(snap.withheld, "projectbluefin/dakota-iso#800"); ok {
				t.Errorf("dakota-iso#800 must not be withheld, got reason %q", item.Reason)
			}
		})
	}
}

// Execute the shipped Repos-for-contribute matcher and toggle: prefix-sharing
// names get their own state, a regex entry is honoured, and turning dakota-iso
// on keeps the projectbluefin/dakota entry in place.
func TestContribRepoToggleKeepsPrefixSharingEntries(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: contribute repo toggle was not executed")
	}
	html := indexHTML(t)
	var source strings.Builder
	source.WriteString(`const assert = require('node:assert/strict');
let toasts = [];
let dirty = [];
function showToast(msg) { toasts.push(msg); }
function markDirty(section, key, val) { dirty.push([section, key, val]); }
function renderConfigTab() {}
const _configState = { data: { hub: {} } };
function onEl() { return { classList: { contains: (c) => c === 'on' } }; }
function offEl() { return { classList: { contains: () => false } }; }
`)
	for _, name := range []string{
		"contribRepoShortName",
		"contribRepoWildcardMatch",
		"contribRepoIsRegexEntry",
		"contribRepoDisabledEntryMatches",
		"contribRepoMatchingDisabledEntries",
		"toggleContribRepo",
	} {
		source.WriteString(jsFunc(t, html, name))
		source.WriteByte('\n')
	}
	source.WriteString(`const m = contribRepoDisabledEntryMatches;
assert.equal(m('projectbluefin/dakota', 'projectbluefin/dakota'), true);
assert.equal(m('projectbluefin/dakota-iso', 'projectbluefin/dakota'), false);
assert.equal(m('projectbluefin/dakota', 'dakota'), true);
assert.equal(m('projectbluefin/dakota-iso', 'dakota'), false);
assert.equal(m('projectbluefin/dakota', 'ProjectBluefin/Dakota'), true);
assert.equal(m('projectbluefin/dakota-iso', 'projectbluefin/dakota*'), true);
assert.equal(m('projectbluefin/dakota', '/^projectbluefin\\/dakota$/'), true);
assert.equal(m('projectbluefin/dakota-iso', '/^projectbluefin\\/dakota$/'), false);
assert.equal(m('projectbluefin/dakota', '/dakota(/'), false);
assert.equal(m('projectbluefin/dakota', ''), false);

// Turning dakota-iso on leaves the dakota entry alone.
_configState.data.hub.disabled_repos = ['projectbluefin/dakota', 'projectbluefin/dakota-iso'];
toggleContribRepo(offEl(), 'projectbluefin/dakota-iso');
assert.deepEqual(_configState.data.hub.disabled_repos, ['projectbluefin/dakota']);

// Turning dakota-iso off adds its own entry even though dakota is disabled.
toggleContribRepo(onEl(), 'projectbluefin/dakota-iso');
assert.deepEqual(_configState.data.hub.disabled_repos, ['projectbluefin/dakota', 'projectbluefin/dakota-iso']);

// A regex blocker is reported, not deleted.
_configState.data.hub.disabled_repos = ['/^projectbluefin\\/dakota$/'];
toasts = [];
toggleContribRepo(offEl(), 'projectbluefin/dakota');
assert.deepEqual(_configState.data.hub.disabled_repos, ['/^projectbluefin\\/dakota$/']);
assert.equal(toasts.length, 1);
assert.match(toasts[0], /regex/);
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("contribute repo toggle failed: %v\n%s", err, out)
	}
}

// TestAdminRepoDisabledEntryMatchesExactPlainEntries pins the Contribute
// landing admin page to the same rule as config.RepoMatchesAny, so its
// "Repos for Contribute" toggles cannot disagree with the hub or strip a
// prefix-sharing entry when another repo is enabled.
func TestAdminRepoDisabledEntryMatchesExactPlainEntries(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: admin repo matcher was not executed")
	}
	body := renderContributePage(t)
	var source strings.Builder
	source.WriteString("const assert = require('node:assert/strict');\n")
	for _, name := range []string{
		"adminRepoShortName",
		"adminRepoWildcardMatch",
		"adminRepoIsRegexEntry",
		"adminRepoDisabledEntryMatches",
		"adminRepoMatchingDisabledEntries",
	} {
		source.WriteString(jsFunc(t, body, name))
		source.WriteByte('\n')
	}
	source.WriteString(`const m = adminRepoDisabledEntryMatches;
assert.equal(m('projectbluefin/dakota', 'projectbluefin/dakota'), true);
assert.equal(m('projectbluefin/dakota-iso', 'projectbluefin/dakota'), false);
assert.equal(m('projectbluefin/dakota', 'dakota'), true);
assert.equal(m('projectbluefin/dakota-iso', 'dakota'), false);
assert.equal(m('projectbluefin/dakota', 'ProjectBluefin/Dakota'), true);
assert.equal(m('projectbluefin/dakota-iso', 'projectbluefin/dakota*'), true);
assert.equal(m('projectbluefin/dakota', '/^projectbluefin\\/dakota$/'), true);
assert.equal(m('projectbluefin/dakota-iso', '/^projectbluefin\\/dakota$/'), false);
assert.equal(m('projectbluefin/dakota', '/dakota(/'), false);
assert.equal(m('projectbluefin/dakota', ''), false);
assert.deepEqual(
  adminRepoMatchingDisabledEntries('projectbluefin/dakota-iso', ['projectbluefin/dakota', 'projectbluefin/dakota-iso']),
  ['projectbluefin/dakota-iso']);
`)
	if out, err := exec.Command(node, "-e", source.String()).CombinedOutput(); err != nil {
		t.Fatalf("admin repo matcher failed: %v\n%s", err, out)
	}
}
