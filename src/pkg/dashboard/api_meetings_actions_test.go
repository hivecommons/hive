package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

type fakeActionsService struct {
	err    error
	edits  []string
	acted  []string
	listed int
}

func (f *fakeActionsService) List() (any, error) {
	f.listed++
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"drafts": []any{map[string]string{"id": "act_1", "status": "pending"}}, "pending": 1}, nil
}

func (f *fakeActionsService) Edit(_ context.Context, id string, req MeetingActionEditRequest, by string) (any, error) {
	title := ""
	if req.Title != nil {
		title = *req.Title
	}
	f.edits = append(f.edits, id+":"+title+":"+by)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": id, "title": title}, nil
}

func (f *fakeActionsService) Act(_ context.Context, id, action, by string) (any, error) {
	f.acted = append(f.acted, id+":"+action+":"+by)
	if f.err != nil {
		return nil, f.err
	}
	return map[string]any{"id": id, "status": action}, nil
}

func actionsRequest(s *Server, method, path, body, role string, verified bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-User", "octo")
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

var actionDraftPaths = []string{
	"/api/meetings/actions/act_1/approve", "/api/meetings/actions/act_1/reject", "/api/meetings/actions/act_1/edit",
}

func TestMeetingsActionsAPIAuth(t *testing.T) {
	s, _ := apiServer(t)
	if rec := doOwnerGet(s, "/api/meetings/actions"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no service = %d", rec.Code)
	}
	for _, p := range actionDraftPaths {
		if rec := actionsRequest(s, http.MethodPost, p, `{}`, "owner", true); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s without service = %d", p, rec.Code)
		}
	}
	f := &fakeActionsService{}
	s.SetMeetingsActionsService(f)
	// Anonymous, read-only and unverified-owner callers can neither see nor
	// decide drafts: nothing is created without an owner or read-write user.
	for _, c := range []struct {
		role     string
		verified bool
	}{{"", false}, {"read", false}, {"owner", false}} {
		if rec := actionsRequest(s, http.MethodGet, "/api/meetings/actions", ``, c.role, c.verified); rec.Code != http.StatusForbidden {
			t.Errorf("GET as %q = %d, want 403", c.role, rec.Code)
		}
		for _, p := range actionDraftPaths {
			if rec := actionsRequest(s, http.MethodPost, p, `{"title":"x"}`, c.role, c.verified); rec.Code != http.StatusForbidden {
				t.Errorf("POST %s as %q = %d, want 403", p, c.role, rec.Code)
			}
		}
	}
	if len(f.acted) != 0 || len(f.edits) != 0 || f.listed != 0 {
		t.Fatalf("refused callers reached the queue: %+v", f)
	}
	// Owner and read-write may list, edit, approve and reject.
	for _, c := range []struct {
		role     string
		verified bool
	}{{"owner", true}, {"read-write", false}} {
		if rec := actionsRequest(s, http.MethodGet, "/api/meetings/actions", ``, c.role, c.verified); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pending":1`) {
			t.Fatalf("list as %s = %d %s", c.role, rec.Code, rec.Body)
		}
		if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/approve", ``, c.role, c.verified); rec.Code != http.StatusOK {
			t.Fatalf("approve as %s = %d %s", c.role, rec.Code, rec.Body)
		}
		if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/reject", ``, c.role, c.verified); rec.Code != http.StatusOK {
			t.Fatalf("reject as %s = %d", c.role, rec.Code)
		}
		if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/edit", `{"title":"New"}`, c.role, c.verified); rec.Code != http.StatusOK {
			t.Fatalf("edit as %s = %d", c.role, rec.Code)
		}
	}
	if strings.Join(f.acted, ",") != "act_1:approve:octo,act_1:reject:octo,act_1:approve:octo,act_1:reject:octo" || f.edits[0] != "act_1:New:octo" {
		t.Fatalf("acted %v edits %v", f.acted, f.edits)
	}
	if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/edit", `{bad`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
}

func TestMeetingsActionsAPIErrors(t *testing.T) {
	s, _ := apiServer(t)
	cases := map[error]int{
		fmt.Errorf("%w: x", ErrMeetingNotFound):        http.StatusNotFound,
		fmt.Errorf("%w: x", ErrMeetingInvalid):         http.StatusBadRequest,
		fmt.Errorf("%w: x", ErrMeetingConflict):        http.StatusConflict,
		fmt.Errorf("%w: x", ErrMeetingActionsDisabled): http.StatusConflict,
		errors.New("disk full"):                        http.StatusBadGateway,
	}
	for err, code := range cases {
		s.SetMeetingsActionsService(&fakeActionsService{err: err})
		if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/approve", ``, "owner", true); rec.Code != code {
			t.Errorf("approve %v = %d, want %d", err, rec.Code, code)
		}
		if rec := actionsRequest(s, http.MethodPost, "/api/meetings/actions/act_1/edit", `{}`, "owner", true); rec.Code != code {
			t.Errorf("edit %v = %d, want %d", err, rec.Code, code)
		}
		if rec := doOwnerGet(s, "/api/meetings/actions"); rec.Code != code {
			t.Errorf("list %v = %d, want %d", err, rec.Code, code)
		}
	}
	// The literal routes win over /api/meetings/{id}.
	s.SetMeetingsActionsService(&fakeActionsService{})
	if rec := doOwnerGet(s, "/api/meetings/actions"); rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
}

func TestMeetingsActionsSettingsAPI(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Project = config.ProjectConfig{Org: "hivecommons", Repos: []string{"hive", "spektacular"}}
	for _, role := range []string{"", "read"} {
		if rec := actionsRequest(s, http.MethodGet, "/api/meetings/actions/settings", ``, role, false); rec.Code != http.StatusForbidden {
			t.Errorf("GET settings as %q = %d", role, rec.Code)
		}
	}
	rec := actionsRequest(s, http.MethodGet, "/api/meetings/actions/settings", ``, "read-write", false)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET settings = %d", rec.Code)
	}
	var v MeetingsActionsSettingsView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !v.Enabled || len(v.Types) != 4 || v.EffectiveDefaultRepo != "hivecommons/hive" || len(v.Repos) != 2 ||
		v.AutoApprove["knowledge"] || len(v.AutoApprovable) != 1 || v.AutoApprovable[0] != "knowledge" {
		t.Fatalf("defaults %+v", v)
	}
	// Saving is the owner's.
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{"enabled":false}`, "read-write", false); rec.Code != http.StatusForbidden {
		t.Fatalf("read-write PUT = %d", rec.Code)
	}
	// Only knowledge notes may be auto-approved.
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{"auto_approve":{"issue":true}}`, "owner", true); rec.Code != http.StatusBadRequest ||
		!strings.Contains(rec.Body.String(), "only knowledge notes") {
		t.Fatalf("auto-approve issue = %d %s", rec.Code, rec.Body)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{"types":["issue","pr"]}`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad type = %d", rec.Code)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{"default_repo":"not a repo"}`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad repo = %d", rec.Code)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{"types":[" "]}`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("no types = %d", rec.Code)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{bad`, "owner", true); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad body = %d", rec.Code)
	}
	rec = actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings",
		`{"enabled":true,"types":["Issue","knowledge","issue"],"default_repo":"hivecommons/spektacular/","auto_approve":{"Knowledge":true,"rfc":false}}`, "owner", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", rec.Code, rec.Body)
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if strings.Join(v.Types, ",") != "issue,knowledge" || v.DefaultRepo != "hivecommons/spektacular" || v.EffectiveDefaultRepo != "hivecommons/spektacular" || !v.AutoApprove["knowledge"] {
		t.Fatalf("saved %+v", v)
	}
	a := deps.Config.Meetings.Persona.Actions
	if a.Enabled == nil || !*a.Enabled || len(a.AutoApprove) != 1 || !a.AutoApprove["knowledge"] {
		t.Fatalf("config %+v", a)
	}
	t.Setenv(config.MeetingsActionsEnabledEnv, "false")
	rec = actionsRequest(s, http.MethodGet, "/api/meetings/actions/settings", ``, "owner", true)
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.Enabled || len(v.EnvOverrides) != 1 || v.EnvOverrides[0] != config.MeetingsActionsEnabledEnv {
		t.Fatalf("env override %+v", v)
	}
	if got := normalizeMeetingActionTypes([]string{" ", "RFC"}); len(got) != 1 || got[0] != "rfc" {
		t.Fatalf("normalize %v", got)
	}
}

func TestMeetingsActionsSettingsNoConfig(t *testing.T) {
	s := NewServer(0, slog.New(slog.DiscardHandler))
	s.registerMeetingsActionsRoutes()
	if rec := actionsRequest(s, http.MethodGet, "/api/meetings/actions/settings", ``, "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no config = %d", rec.Code)
	}
	if rec := actionsRequest(s, http.MethodPut, "/api/meetings/actions/settings", `{}`, "owner", true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no config PUT = %d", rec.Code)
	}
}

// The Meeting attendee tab reads meetings.persona.actions from the persona
// settings JSON.
func TestMeetingsPersonaSettingsCarryActions(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Meetings.Persona.Actions.AutoApprove = map[string]bool{"knowledge": true}
	got := personaGet(t, s)
	a := got.Actions
	if !a.Enabled || len(a.AllTypes) != 4 || !a.AutoApprove["knowledge"] || len(a.Repos) == 0 {
		t.Fatalf("persona actions %+v", a)
	}
}
