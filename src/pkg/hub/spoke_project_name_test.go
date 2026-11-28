package hub

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

func TestHeartbeatSpokeProjectName(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHeartbeatHub()
	if err := saveSaaSHive(&SaaSHive{ID: "hosted-name", ProjectName: "Hosted name"}); err != nil {
		t.Fatal(err)
	}
	beat := func(id, name string) {
		t.Helper()
		body, err := json.Marshal(spoke.HeartbeatPayload{HiveID: id, Org: "org", PrimaryRepo: "repo", ProjectName: name})
		if err != nil {
			t.Fatal(err)
		}
		if rec := postHeartbeat(t, s, string(body)); rec.Code != 200 {
			t.Fatalf("heartbeat: %d %s", rec.Code, rec.Body.String())
		}
	}
	name := func(id string) string {
		t.Helper()
		for _, h := range s.registry.Hives {
			if h.ID == id {
				return h.ProjectName
			}
		}
		t.Fatalf("missing hive %s", id)
		return ""
	}
	beat("local-name", "My project")
	if got := name("local-name"); got != "My project" {
		t.Fatalf("project name = %q", got)
	}
	if rec := postHeartbeat(t, s, `{"hive_id":"local-name"}`); rec.Code != 200 {
		t.Fatalf("legacy heartbeat: %d %s", rec.Code, rec.Body.String())
	}
	if got := name("local-name"); got != "My project" {
		t.Fatalf("legacy heartbeat cleared name: %q", got)
	}
	beat("local-name", "")
	if got := name("local-name"); got != "" {
		t.Fatalf("cleared name = %q", got)
	}
	beat("hosted-name", "Spoke name")
	if got := name("hosted-name"); got != "Hosted name" {
		t.Fatalf("hosted name = %q", got)
	}
}

func TestHiveLabelDisambiguatesDuplicateTitles(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable")
	}
	script := strings.Join([]string{
		"const assert = require('node:assert/strict');",
		"var _allDashHives = [];",
		jsFunc(t, "hiveBaseLabel"),
		jsFunc(t, "hiveLabel"),
		jsFunc(t, "hiveNameSortValue"),
		`const a = {id:'one', org:'org', primaryRepo:'a'};
const b = {id:'two', org:'org', primaryRepo:'b'};
_allDashHives = [a, b];
assert.equal(hiveLabel(a).line1, 'org — org/a');
assert.equal(hiveLabel(b).line1, 'org — org/b');
assert.ok(hiveNameSortValue(a).startsWith(hiveLabel(a).line1));
b.primaryRepo = 'a';
assert.equal(hiveLabel(a).line1, 'org — one');
assert.equal(hiveLabel(b).line1, 'org — two');
a.projectName = 'First';
assert.equal(hiveLabel(a).line1, 'First');
assert.equal(hiveLabel(b).line1, 'org');`,
	}, "\n")
	if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
		t.Fatalf("hiveLabel: %v\n%s", err, out)
	}
}
