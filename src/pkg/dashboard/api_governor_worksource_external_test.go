package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"
)

func externalWorkSourceBody() map[string]any {
	return map[string]any{
		"name":            "acme",
		"display_name":    "Acme Tracker",
		"base_url":        "https://acme-shim.internal:8443",
		"auth_token":      "$ACME_WORKSOURCE_TOKEN",
		"repos":           []string{"your-org/app", "your-org/platform"},
		"hold_labels":     []string{"hold", "blocked"},
		"timeout_seconds": 20,
	}
}

func externalWorkSourceSection(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := doOwnerGet(s, "/api/config/governor/work-source")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET work-source: %d — %s", rec.Code, rec.Body.String())
	}
	var raw struct {
		External map[string]any `json:"external"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode work-source settings: %v", err)
	}
	return raw.External
}

// TestGovWorkSource_ExternalRoundTrip is the ADR-0020 overlay requirement: the
// external block survives PUT → GET, and what is persisted is the ${VAR}
// reference, never a resolved credential.
func TestGovWorkSource_ExternalRoundTrip(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":     "external",
		"external": externalWorkSourceBody(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("put external work-source: %d — %s", rec.Code, rec.Body.String())
	}

	got := externalWorkSourceSection(t, s)
	if got["name"] != "acme" || got["display_name"] != "Acme Tracker" {
		t.Errorf("external section = %v", got)
	}
	if got["base_url"] != "https://acme-shim.internal:8443" {
		t.Errorf("base_url = %v", got["base_url"])
	}
	if got["auth_token"] != "$ACME_WORKSOURCE_TOKEN" {
		t.Errorf("auth_token = %v, want the reference echoed back", got["auth_token"])
	}
	if n, _ := got["timeout_seconds"].(float64); n != 20 {
		t.Errorf("timeout_seconds = %v", got["timeout_seconds"])
	}

	stored := s.deps.Config.Governor.WorkSource.External
	if stored.AuthToken != "$ACME_WORKSOURCE_TOKEN" {
		t.Errorf("stored auth_token = %q, want the reference", stored.AuthToken)
	}
	if len(stored.Repos) != 2 || stored.Repos[0] != "your-org/app" {
		t.Errorf("stored repos = %v", stored.Repos)
	}
	if len(stored.HoldLabels) != 2 {
		t.Errorf("stored hold_labels = %v", stored.HoldLabels)
	}
}

// TestGovWorkSource_ExternalRejectsLiteralToken is the reason the rule exists:
// a literal credential would be written into the overlay on disk.
func TestGovWorkSource_ExternalRejectsLiteralToken(t *testing.T) {
	s := govServer(t)
	const secret = "acme_live_1234567890SECRET"
	body := externalWorkSourceBody()
	body["auth_token"] = secret

	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":     "external",
		"external": body,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT with a literal token = %d, want 400 — %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Governor.WorkSource.External.AuthToken == secret {
		t.Fatal("the rejected literal token was stored anyway")
	}
	if s.deps.Config.Governor.WorkSource.Type == "external" {
		t.Fatal("a rejected PUT must not switch the active work source")
	}
}

// TestGovWorkSource_ExternalRejectsLiteralTokenWhileStaging: the secrets rule
// applies even when the operator has not switched the type over yet.
func TestGovWorkSource_ExternalRejectsLiteralTokenWhileStaging(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"external": map[string]any{"auth_token": "acme_live_SECRET"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("staging PUT with a literal token = %d, want 400 — %s", rec.Code, rec.Body.String())
	}
}

// TestGovWorkSource_ExternalStagingAllowsIncompleteBlock keeps the pre-switch
// workflow the other adapters have: settings can be filled in over several
// writes as long as the credentials stay references.
func TestGovWorkSource_ExternalStagingAllowsIncompleteBlock(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"external": map[string]any{"name": "acme", "auth_token": "${ACME_WORKSOURCE_TOKEN}"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("staging PUT = %d, want 200 — %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.WorkSource.External.Name; got != "acme" {
		t.Errorf("stored name = %q", got)
	}
}

// TestGovWorkSource_ExternalRejectsInvalidActivation: switching the active
// source to an invalid block would make the governor fail closed on every
// cycle with nothing in the UI saying why.
func TestGovWorkSource_ExternalRejectsInvalidActivation(t *testing.T) {
	cases := map[string]func(map[string]any){
		"reserved name":     func(b map[string]any) { b["name"] = "github" },
		"plain http":        func(b map[string]any) { b["base_url"] = "http://acme.example" },
		"no repos":          func(b map[string]any) { b["repos"] = []string{} },
		"bad repo spelling": func(b map[string]any) { b["repos"] = []string{"app"} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := govServer(t)
			body := externalWorkSourceBody()
			mutate(body)
			rec := doPut(s, "/api/config/governor/work-source", map[string]any{
				"type":     "external",
				"external": body,
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("PUT = %d, want 400 — %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestGovWorkSource_ExternalIsAnAcceptedType guards the type allow-list.
func TestGovWorkSource_ExternalIsAnAcceptedType(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/governor/work-source", map[string]any{
		"type":     "external",
		"external": externalWorkSourceBody(),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT type=external: %d — %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.WorkSource.Type; got != "external" {
		t.Errorf("stored type = %q", got)
	}
	if rec := doPut(s, "/api/config/governor/work-source", map[string]any{"type": "not-a-source"}); rec.Code != http.StatusBadRequest {
		t.Errorf("unknown type = %d, want 400", rec.Code)
	}
}
