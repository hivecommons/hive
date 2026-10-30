package dashboard

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaderboardAPIAbandoned(t *testing.T) {
	s := newDossierTestServer(t)
	// Seed persisted profiles, as written by adoption accounting. Keep this
	// reader test independent of the accounting writer's reconciliation rules.
	// Include contributor IDs so listContributorProfiles accepts both records.
	if err := os.MkdirAll(getContributorsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, profile := range map[string]string{
		"alice": `{"github_username":"alice","contributor_id":"c-alice","trust_tier":"contributor","total_tasks_completed":10,"abandoned":7}`,
		"bob":   `{"github_username":"bob","contributor_id":"c-bob","trust_tier":"contributor","total_tasks_completed":5}`,
	} {
		if err := os.WriteFile(filepath.Join(getContributorsDir(), name+".json"), []byte(profile), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	s.handleLeaderboardAPI(w, httptest.NewRequest("GET", "/api/leaderboard", nil))
	var body struct {
		Leaderboard []struct {
			Username  string `json:"github_username"`
			Rank      int    `json:"rank"`
			Abandoned *int   `json:"abandoned"`
		} `json:"leaderboard"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Leaderboard) != 2 {
		t.Fatalf("unexpected leaderboard: %s", w.Body.String())
	}
	for i, want := range []int{7, 0} {
		row := body.Leaderboard[i]
		if row.Abandoned == nil || *row.Abandoned != want || row.Rank != i+1 {
			t.Fatalf("row %d: %+v; want abandoned=%d and unchanged rank", i, row, want)
		}
	}
	if body.Leaderboard[0].Username != "alice" {
		t.Fatalf("ranking must remain based on completed tasks: %s", w.Body.String())
	}
}

func TestLeaderboardAbandonedRendering(t *testing.T) {
	landing := renderContributePage(t)
	dashboard, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	// These assertions still run on CI workers without Node.
	for _, want := range []string{"sortable.header('abandoned','Abandoned')", "var abandoned=(e.abandoned||0);", ".lb-row>:nth-child(8){grid-column:3/-1;grid-row:3}"} {
		if !strings.Contains(landing, want) {
			t.Errorf("landing missing %q", want)
		}
	}
	for _, want := range []string{">Abandoned</th>", "${e.abandoned || 0}"} {
		if !strings.Contains(string(dashboard), want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is required for rendering assertions")
	}
	// Run the shipped renderers with a minimal DOM, rather than a parallel
	// implementation of the rendering logic.
	common := `
const assert = require('node:assert/strict');
const panel = {innerHTML:''};
const document = {getElementById:()=>panel};
function esc(s){return String(s);}
function tierBadge(){return '';}
function socialShareControls(){return '';}
function ccRenderLeaderboardSparklines(){}
var ccMeUsername='', lbLastData, ccMetrics=null;
function opsSortableTable(id,opts){return {rows:opts.rows,ariaSort:()=> 'none',header:(key,label)=>label};}
const entries=[{github_username:'alice',rank:1,tasks_completed:10,tasks_failed:2,abandoned:7},
 {github_username:'bob',rank:2,tasks_completed:5}];
`
	landingJS := common + extractJSFunc(t, landing, "function lbRow(e,rank){") + "\n" +
		extractJSFunc(t, landing, "function renderLeaderboard(contribs){") + `
renderLeaderboard(entries);
assert.match(panel.innerHTML, /Abandoned/);
assert.match(lbRow(entries[0],1), /<div class="lb-stat">7<\/div><\/div>$/);
assert.match(lbRow(entries[1],2), /<div class="lb-stat">0<\/div><\/div>$/);
`
	dashboardJS := common + `
const CONTRIBUTOR_AVATAR_PX=32;
function linkedAvatar(){return '';}
function trustTierBadge(){return '';}
function setIfChanged(el,html){el.innerHTML=html;}
` + extractJSFunc(t, string(dashboard), "function renderLeaderboard(entries) {") + `
renderLeaderboard(entries);
assert.match(panel.innerHTML, />Abandoned<\/th>/);
const rows=panel.innerHTML.match(/<tbody>([\s\S]*)<\/tbody>/)[1].split('</tr>').filter(r=>r.trim());
assert.equal(rows.length,2);
assert.match(rows[0], />7<\/td>\s*$/);
assert.match(rows[1], />0<\/td>\s*$/);
`
	for name, script := range map[string]string{"landing": landingJS, "dashboard": dashboardJS} {
		t.Run(name, func(t *testing.T) {
			if out, err := exec.Command(node, "-e", script).CombinedOutput(); err != nil {
				t.Fatalf("rendering failed: %v\n%s", err, out)
			}
		})
	}
}
