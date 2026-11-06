package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
)

type mcpCaller func(body string) string

// adminMCPAs returns a caller that posts JSON-RPC to the admin MCP endpoint
// through the full auth chain with hdr set, and returns the tool text (or
// the raw body when the call failed).
func adminMCPAs(t *testing.T, h http.Handler, hdr map[string]string) mcpCaller {
	t.Helper()
	return func(body string) string {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, adminmcp.EndpointPath, strings.NewReader(body))
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("admin MCP status = %d body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &resp) == nil && len(resp.Result.Content) == 1 {
			return resp.Result.Content[0].Text
		}
		return rec.Body.String()
	}
}

func confirmationID(t *testing.T, text string) string {
	t.Helper()
	var out struct {
		Data struct {
			ID string `json:"confirmation_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &out); err != nil || out.Data.ID == "" {
		t.Fatalf("no confirmation in %s", text)
	}
	return out.Data.ID
}

const rescanPreview = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"write_preview","arguments":{"operation":"repository.rescan","args":{}}}}`

func rescanConfirm(id string) string {
	return `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write_confirm","arguments":{"confirmation_id":"` + id + `"}}}`
}

// A hub-proxied caller (the hub's proof plus its identity headers) writes as
// that caller: the endpoint sees their login and role, not the configured
// owner, and only they can confirm what they previewed.
func TestAdminMCPWritesRunAsVerifiedCaller(t *testing.T) {
	t.Setenv("HIVE_ADMIN_MCP_ENABLE_WRITES", "1")
	t.Setenv("HIVE_ADMIN_MCP_PENDING_FILE", t.TempDir()+"/pending.json")
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	var seen []string
	s.mux.HandleFunc("POST /api/repos/rescan", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get("X-Hive-User")+"/"+r.Header.Get("X-Hive-Role")+"/"+r.Header.Get(ownerRoleVerifiedHeader))
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "started"})
	})
	h := s.authenticate(s.roleEnforcement(s.securityHeaders(s.mux)))
	hub := func(user, role string) mcpCaller {
		return adminMCPAs(t, h, map[string]string{"X-Hive-User": user, "X-Hive-Role": role, proxyAuthHeader: "secret"})
	}
	alice, bob := hub("alice", config.RoleReadWrite), hub("bob", config.RoleReadWrite)

	id := confirmationID(t, alice(rescanPreview))
	if got := bob(rescanConfirm(id)); !strings.Contains(got, "previewed by another caller") {
		t.Fatalf("bob confirmed alice's write: %s", got)
	}
	if got := alice(rescanConfirm(id)); !strings.Contains(got, `"write_result"`) {
		t.Fatalf("alice's confirm = %s", got)
	}
	if len(seen) != 1 || seen[0] != "alice/"+config.RoleReadWrite+"/" {
		t.Fatalf("write ran as %v, want alice at her own role, unverified as owner", seen)
	}

	// The dashboard token still writes as the configured owner identity.
	bearer := adminMCPAs(t, h, map[string]string{"Authorization": "Bearer secret"})
	if got := bearer(rescanConfirm(confirmationID(t, bearer(rescanPreview)))); !strings.Contains(got, `"write_result"`) {
		t.Fatalf("bearer confirm = %s", got)
	}
	if len(seen) != 2 || seen[1] != dashboardFallbackOwnerUser+"/"+config.RoleOwner+"/true" {
		t.Fatalf("bearer write ran as %v", seen)
	}

	// Admitted through the internal header: previously refused up front.
	internal := adminMCPAs(t, h, map[string]string{"X-Hive-Internal": "secret"})
	if got := internal(rescanConfirm(confirmationID(t, internal(rescanPreview)))); !strings.Contains(got, `"write_result"`) {
		t.Fatalf("internal confirm = %s", got)
	}
}

func TestAdminMCPWritesUnavailableReason(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	for _, tc := range []struct {
		name string
		p    dashboardAdminMCPProvider
		want string
	}{
		{"read-write", dashboardAdminMCPProvider{user: "alice", role: config.RoleReadWrite}, ""},
		{"owner", dashboardAdminMCPProvider{user: "owner", role: config.RoleOwner}, ""},
		{"read-only", dashboardAdminMCPProvider{user: "carol", role: "read"}, "read-only"},
		{"no identity", dashboardAdminMCPProvider{}, "no identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := s.adminMCPWritesUnavailableReason(tc.p)
			if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
	open := NewServerWithAuth(0, "", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := open.adminMCPWritesUnavailableReason(dashboardAdminMCPProvider{}); got != "" {
		t.Fatalf("open spoke reason = %q", got)
	}
}

// The verified-owner marker rides along only when the outer request was
// verified as owner, never on a claimed role.
func TestAdminMCPWriteCarriesOwnerMarkerOnlyWhenVerified(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	var marker []string
	s.mux.HandleFunc("POST /api/admin-mcp-test/who", func(w http.ResponseWriter, r *http.Request) {
		marker = append(marker, r.Header.Get(ownerRoleVerifiedHeader))
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	for _, p := range []dashboardAdminMCPProvider{
		{server: s, user: "jo", role: config.RoleOwner, ownerVerified: true},
		{server: s, user: "jo", role: config.RoleOwner},
		{server: s, user: "jo", role: config.RoleReadWrite, ownerVerified: true},
	} {
		if _, err := p.ExecuteWrite(context.Background(), adminmcp.WriteRequest{Method: http.MethodPost, Path: "/api/admin-mcp-test/who"}); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(marker, ",") != "true,," {
		t.Fatalf("owner markers = %v", marker)
	}
}

func TestAdminMCPStageTrackerRead(t *testing.T) {
	s := NewServerWithAuth(0, "secret", slog.New(slog.NewTextHandler(io.Discard, nil)))
	s.mux.HandleFunc("GET /api/stage-tracker", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"stages":[{"number":40,"slug":"covered-by-pr","label":"Covered"}],"items":[
			{"repo":"o/r","number":488,"kind":"issue","title":"gimp","stage":40,"linked_prs":[{"number":892}],"history":[{}]},
			{"repo":"o/r","number":5,"kind":"issue","title":"other","stage":40}]}`)
	})
	s.mux.HandleFunc("GET /api/repos/o/r/items/488/decide-delegated", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"done","requester":"`+r.Header.Get("X-Hive-User")+`"}`)
	})
	p := dashboardAdminMCPProvider{server: s, user: "alice", role: config.RoleReadWrite}
	got, err := p.Read(context.Background(), adminmcp.ToolStageTrackerRead, map[string]any{"q": "892"})
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	items := out["items"].([]any)
	if out["matched"] != 1 || len(items) != 1 || items[0].(map[string]any)["number"] != float64(488) {
		t.Fatalf("stage_tracker_read = %#v", out)
	}
	if _, ok := items[0].(map[string]any)["history"]; ok {
		t.Fatal("summary view kept history")
	}
	status, err := p.Read(context.Background(), adminmcp.ToolDelegatedDecisionStatus, map[string]any{"owner": "o", "repo": "r", "number": float64(488)})
	if err != nil {
		t.Fatal(err)
	}
	if m := status.(map[string]any); m["status"] != "done" || m["requester"] != "alice" {
		t.Fatalf("delegated_decision_status = %#v", status)
	}
	if _, err := p.Read(context.Background(), adminmcp.ToolDelegatedDecisionStatus, map[string]any{"owner": "o"}); err == nil {
		t.Fatal("delegated_decision_status without an item succeeded")
	}
}
