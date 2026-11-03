package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestRepoIssueReleaseUsesRepositoryWriteGate(t *testing.T) {
	s := &Server{}
	request := func(owner bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/repos/o/r/issues/7/release", nil)
		req.SetPathValue("owner", "o")
		req.SetPathValue("repo", "r")
		req.SetPathValue("number", "7")
		if owner {
			markOwnerRequest(req)
		}
		rec := httptest.NewRecorder()
		s.handleRepoIssueRelease(rec, req)
		return rec
	}

	if rec := request(false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous release = %d, want 401", rec.Code)
	}
	if rec := request(true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("verified owner did not pass the write gate: status = %d, want downstream 503", rec.Code)
	}
}

// A label-driven repo refuses `/hive approve` before the release executor; the
// dashboard button must refuse the same way, even with release_on_approve set.
func TestRepoIssueReleaseRefusedOnLabelDrivenRepo12635(t *testing.T) {
	s := &Server{deps: &Dependencies{
		GHClient: &ghpkg.Client{},
		Config: &config.Config{Project: config.ProjectConfig{Org: "o", RepoPolicies: []config.RepoPolicy{
			{Repo: "r", LabelDriven: true, LabelDrivenApprovalLabels: []string{}, ReleaseOnApprove: true},
		}}},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/repos/o/r/issues/7/release", nil)
	req.SetPathValue("owner", "o")
	req.SetPathValue("repo", "r")
	req.SetPathValue("number", "7")
	markOwnerRequest(req)
	rec := httptest.NewRecorder()
	s.handleRepoIssueRelease(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("label-driven release = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "label-driven") {
		t.Fatalf("refusal does not name the label-driven policy: %s", rec.Body.String())
	}
	if labelDrivenRefusesUnparkCommands(s.deps.Config, "o/r") != true {
		t.Fatal("fixture must be the repo shape whose sweep refuses /hive approve")
	}
	if s.deps.Config.RepoReleaseOnApprove("o/r") {
		t.Fatal("release_on_approve must be inert on a label-driven repo so the menu offers no Release action")
	}
}
