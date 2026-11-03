package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/governor"
)

// hivecommons/hive#15335: the current-base merge gate is a Features toggle
// (hive-wide default, OFF unless switched on) plus a per-repo override on the
// Repos tab, and the repo card shows the effective value.

func TestAutoMergePutRequireCurrentBaseDefault(t *testing.T) {
	s := covApiServer(t)
	if s.deps.Config.AutoMerge.CurrentBaseDefaultEnabled() {
		t.Fatal("current-base default must be OFF out of the box")
	}
	rec := doPut(s, "/api/config/auto-merge", map[string]any{"require_current_base_default": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("auto-merge PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.AutoMerge.CurrentBaseDefaultEnabled() {
		t.Fatal("require_current_base_default=true not applied")
	}
	var am map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &am); err != nil {
		t.Fatal(err)
	}
	if am["require_current_base_default"] != true {
		t.Fatalf("response require_current_base_default = %v", am["require_current_base_default"])
	}
	for _, k := range []string{"require_current_base", "skip_current_base"} {
		if list, ok := am[k].([]any); !ok || len(list) != 0 {
			t.Errorf("response %s = %v, want empty list", k, am[k])
		}
	}
	// A PUT that omits the key leaves it alone (pointer semantics).
	rec = doPut(s, "/api/config/auto-merge", map[string]any{"bot_takeover_enabled": true})
	if rec.Code != http.StatusOK || !s.deps.Config.AutoMerge.CurrentBaseDefaultEnabled() {
		t.Fatalf("omitted key changed the default: code=%d enabled=%v", rec.Code, s.deps.Config.AutoMerge.CurrentBaseDefaultEnabled())
	}
}

func TestHandleGovernorRepos_CurrentBaseOverrides(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"alpha", "beta", "gamma"}
	srv.deps.Config.Project.PrimaryRepo = "alpha"

	// An override-only save must not be rejected as "no repos".
	w := httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos", `{"repoCurrentBase":{"beta":true,"gamma":false}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	am := &srv.deps.Config.AutoMerge
	if got := am.RequireCurrentBase; !reflect.DeepEqual(got, []string{"testorg/beta"}) {
		t.Errorf("require_current_base = %v, want [testorg/beta]", got)
	}
	if got := am.SkipCurrentBase; !reflect.DeepEqual(got, []string{"testorg/gamma"}) {
		t.Errorf("skip_current_base = %v, want [testorg/gamma]", got)
	}
	if am.RequireCurrentBaseFor("testorg/alpha") || !am.RequireCurrentBaseFor("testorg/beta") || am.RequireCurrentBaseFor("testorg/gamma") {
		t.Error("effective values after override save are wrong")
	}

	result := decodeGovernorConfigGet(t, srv)
	if result["currentBaseDefault"] != false {
		t.Errorf("GET currentBaseDefault = %v, want false", result["currentBaseDefault"])
	}
	repoMap := result["repoCurrentBase"].(map[string]any)
	beta := repoMap["testorg/beta"].(map[string]any)
	if beta["value"] != true || beta["effective"] != true {
		t.Errorf("beta entry = %v, want value=true effective=true", beta)
	}
	gamma := repoMap["testorg/gamma"].(map[string]any)
	if gamma["value"] != false || gamma["effective"] != false {
		t.Errorf("gamma entry = %v, want value=false effective=false", gamma)
	}
	alpha := repoMap["testorg/alpha"].(map[string]any)
	if _, hasValue := alpha["value"]; hasValue || alpha["effective"] != false {
		t.Errorf("alpha entry = %v, want no value (inherits) and effective=false", alpha)
	}

	// Flipping an override moves the repo between the two lists; null clears it.
	w = httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos", `{"repoCurrentBase":{"beta":false,"gamma":null}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("flip code = %d, body = %s", w.Code, w.Body.String())
	}
	if len(am.RequireCurrentBase) != 0 {
		t.Errorf("require_current_base = %v, want empty after flip", am.RequireCurrentBase)
	}
	if got := am.SkipCurrentBase; !reflect.DeepEqual(got, []string{"testorg/beta"}) {
		t.Errorf("skip_current_base = %v, want [testorg/beta]", got)
	}
	if v := am.CurrentBaseOverrideFor("testorg/gamma"); v != nil {
		t.Errorf("gamma override = %v, want cleared", *v)
	}
}

func TestStatusRepoExposesRequireCurrentBase(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Project.Org = "testorg"
	s.deps.Config.Project.Repos = []string{"alpha", "beta"}
	s.deps.Config.AutoMerge.RequireCurrentBase = []string{"testorg/beta"}
	repos := buildRepos(s.deps.Config, nil, governor.State{})
	got := map[string]bool{}
	for _, r := range repos {
		got[r.Name] = r.RequireCurrentBase
	}
	if got["alpha"] || !got["beta"] {
		t.Fatalf("requireCurrentBase per repo = %v, want alpha=false beta=true", got)
	}
}

func TestCurrentBaseSettingsMarkupNodeGuard(t *testing.T) {
	html := indexHTML(t)
	script := `
const fs = require('fs');
const html = fs.readFileSync(0, 'utf8');
function fail(msg) { console.error(msg); process.exit(1); }
const features = html.match(/function renderGovFeatures\([\s\S]*?\n    function /);
if (!features) fail('renderGovFeatures not found');
const f = features[0];
const sw = f.match(/<div class="config-toggle-switch[^"]*" data-action="toggleConfigSwitch" data-section="auto-merge" data-key="require_current_base_default"><\/div>\s*<span class="config-toggle-label">Merge only on current base /);
if (!sw) fail('Features toggle "Merge only on current base" bound to auto-merge/require_current_base_default not found');
if (!f.includes("yamlHint('auto_merge.require_current_base_default')")) fail('toggle lacks its YAML hint');
if (!f.includes('OFF by default. Before auto-merging in any lane, require the PR head to contain the current base tip')) fail('toggle help text missing');
if (!f.includes('Per-repo overrides live on the repo card.')) fail('toggle help text does not point at the per-repo override');
if (!/data-change-action="toggleRepoCurrentBase"[^>]*data-arg-types="s,c"> Merge only on current base<\/label>/.test(html)) fail('Repos tab per-repo override checkbox missing');
if (!/data-action="clearRepoCurrentBaseOverride"/.test(html)) fail('Repos tab inherit button missing');
if (!/class="repo-current-base-pill"/.test(html)) fail('repo card current-base chip missing');
`
	cmd := exec.Command("node", "-e", script)
	cmd.Stdin = strings.NewReader(html)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(err.Error(), "executable file not found") {
			t.Skip("node is not installed")
		}
		t.Fatalf("node current-base settings guard failed: %v\n%s", err, string(out))
	}
}
