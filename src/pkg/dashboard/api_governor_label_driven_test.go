package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The Repos tab edits the hivecommons/hive#11313 label-driven opt-in and its
// trust knobs through PUT /api/config/governor/repos; GET must echo them so
// the tab renders the state actually in force.
func TestHandleGovernorRepos_LabelDrivenRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"alpha", "beta"}
	srv.deps.Config.Project.PrimaryRepo = "alpha"

	w := httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos",
		`{"repoLabelDriven":{"beta":{"enabled":true,"approvalLabels":[" accepted ",""]},"alpha":null},`+
			`"labelDrivenApproval":{"approvalLabels":["triage/accepted","ok-to-work"],"trustOrgMembers":false,"trustedBots":["prow[bot]"]}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	cfg := srv.deps.Config
	if !cfg.RepoLabelDriven("beta") || cfg.RepoLabelDriven("alpha") {
		t.Fatalf("label_driven: beta=%v alpha=%v, want true/false", cfg.RepoLabelDriven("beta"), cfg.RepoLabelDriven("alpha"))
	}
	if got := cfg.LabelDrivenApprovalLabels("beta"); len(got) != 1 || got[0] != "accepted" {
		t.Errorf("beta approval labels = %v, want [accepted]", got)
	}
	if cfg.LabelDrivenApprovalLabels("alpha") != nil {
		t.Error("alpha is not label-driven and must resolve no approval labels")
	}
	if cfg.LabelDrivenTrustOrgMembers() {
		t.Error("trustOrgMembers=false was not applied")
	}
	if bots := cfg.LabelDrivenTrustedBots(); len(bots) != 1 || bots[0] != "prow[bot]" {
		t.Errorf("trusted bots = %v", bots)
	}
	if got := cfg.LabelDrivenApprovalLabelsDefault(); len(got) != 2 || got[1] != "ok-to-work" {
		t.Errorf("default approval labels = %v", got)
	}

	result := decodeGovernorConfigGet(t, srv)
	repoMap, ok := result["repoLabelDriven"].(map[string]any)
	if !ok {
		t.Fatalf("repoLabelDriven missing: %v", result["repoLabelDriven"])
	}
	beta := repoMap["testorg/beta"].(map[string]any)
	if beta["enabled"] != true || !equalStringSlices(toStringSlice(t, beta["approvalLabels"]), []string{"accepted"}) || !equalStringSlices(toStringSlice(t, beta["effectiveLabels"]), []string{"accepted"}) {
		t.Errorf("beta payload = %v", beta)
	}
	alpha := repoMap["testorg/alpha"].(map[string]any)
	if alpha["enabled"] != false {
		t.Errorf("alpha payload = %v, want enabled=false", alpha)
	}
	if _, present := alpha["effectiveLabels"]; present {
		t.Error("a non-label-driven repo must not advertise effective approval labels")
	}
	la := result["labelDrivenApproval"].(map[string]any)
	if la["trustOrgMembers"] != false || la["trustOrgMembersSet"] != true || la["trustedBotsSet"] != true || la["approvalLabelsSet"] != true {
		t.Errorf("labelDrivenApproval payload = %v", la)
	}

	// Switching the repo back off drops its labels too, and restoring the
	// trust knobs to their defaults clears the overrides (empty list =
	// default, true = default).
	w = httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos",
		`{"repoLabelDriven":{"beta":{"enabled":false}},"labelDrivenApproval":{"approvalLabels":[],"trustOrgMembers":true,"trustedBots":[]}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if cfg.RepoLabelDriven("beta") || len(cfg.Project.RepoPolicies) != 0 {
		t.Errorf("beta opt-out should prune its policy row: %+v", cfg.Project.RepoPolicies)
	}
	if cfg.Project.LabelDrivenApprovalLabels != nil || cfg.Project.LabelDrivenTrustOrgMembers != nil || cfg.Project.LabelDrivenTrustedBots != nil {
		t.Errorf("defaults must clear the overrides: labels=%v org=%v bots=%v", cfg.Project.LabelDrivenApprovalLabels, cfg.Project.LabelDrivenTrustOrgMembers, cfg.Project.LabelDrivenTrustedBots)
	}
	if !cfg.LabelDrivenTrustOrgMembers() {
		t.Error("trustOrgMembers must default back to true")
	}
	if len(cfg.LabelDrivenTrustedBots()) != len(config.DefaultLabelDrivenTrustedBots) {
		t.Errorf("trusted bots = %v, want built-in default", cfg.LabelDrivenTrustedBots())
	}
}

// A label-driven-only save is a legitimate body, not "no repos supplied".
func TestHandleGovernorRepos_LabelDrivenOnlyBodyAccepted(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.Org = "testorg"
	srv.deps.Config.Project.Repos = []string{"alpha"}
	srv.deps.Config.Project.PrimaryRepo = "alpha"
	w := httptest.NewRecorder()
	srv.handleGovernorRepos(w, ownerPUT(t, "/api/config/governor/repos", `{"labelDrivenApproval":{"trustOrgMembers":false}}`))
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.LabelDrivenTrustOrgMembers() {
		t.Error("trustOrgMembers=false was not applied")
	}
}

// The Repos tab wires the controls through the shared data-action dispatcher,
// so every handler the markup names must exist as a top-level function and
// the sub-row must only render while the toggle is on.
func TestReposTabLabelDrivenControls(t *testing.T) {
	raw, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(raw)
	for _, want := range []string{
		`data-change-action="toggleRepoLabelDriven"`,
		`data-change-action="setRepoLabelDrivenLabels"`,
		`toggleLabelDrivenTrustOrgMembers`,
		`data-change-action="setLabelDrivenApprovalDefault"`,
		`data-change-action="setLabelDrivenTrustedBots"`,
		`${ldRow}${ldOn ? `,
		`dashboardDocsHref('maintainer-commands', 'label-driven-repositories')`,
		`Label-driven approvals (Prow-style)`,
		`id="cfg-label-driven-defaults"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("index.html is missing %q", want)
		}
	}
	for _, fn := range []string{"toggleRepoLabelDriven", "setRepoLabelDrivenLabels", "toggleLabelDrivenTrustOrgMembers", "setLabelDrivenApprovalDefault", "setLabelDrivenTrustedBots"} {
		if !strings.Contains(html, "function "+fn+"(") {
			t.Errorf("handler %s is referenced by markup but not defined", fn)
		}
	}
	block := html[strings.Index(html, "function renderGovRepos("):]
	block = block[:strings.Index(block, "function setDefaultRepo(")]
	for _, forbidden := range []string{"alert(", "confirm(", "prompt("} {
		if strings.Contains(block, forbidden) {
			t.Errorf("renderGovRepos must not use native dialogs (%s)", forbidden)
		}
	}
	if strings.Contains(block[strings.Index(block, "const ldRow"):strings.Index(block, "// Fully forge-qualified")], "style=") {
		t.Error("label-driven controls must reuse form-control classes, not inline styles")
	}
}
