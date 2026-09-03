package dashboard

// Tests for the bob "Test key" endpoint (handleGovernorBobKeyTest /
// probeBobAPIKey). Every test runs against an httptest bob backend via the
// HIVE_BOB_API_URL override, and the leak assertions capture the server's
// logger output so "the key never appears in responses OR logs" is enforced
// mechanically, not by inspection.

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// newBobProbeServer builds a Server whose logger writes into the returned
// buffer at Info level, so tests can assert what was (not) logged.
func newBobProbeServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	s := NewServer(0, slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	s.deps = &Dependencies{Config: &config.Config{}}
	return s, &logBuf
}

// isolateBobKeySources makes saved-key resolution deterministic: the PVC file
// points into a temp dir and both env fallbacks are blanked.
func isolateBobKeySources(t *testing.T) string {
	t.Helper()
	path := pointBobKeyAtTempDir(t)
	t.Setenv(config.DefaultBobAPIKeyEnv, "")
	t.Setenv("BOBSHELL_API_KEY", "")
	return path
}

// postBobKeyTest exercises the handler and decodes the verdict.
func postBobKeyTest(t *testing.T, s *Server, body string) (*httptest.ResponseRecorder, map[string]interface{}) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/config/governor/bob/test", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.handleGovernorBobKeyTest(rec, req)
	var out map[string]interface{}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("response is not JSON: %v (body: %s)", err, rec.Body.String())
		}
	}
	return rec, out
}

// A pasted opaque key is sent to the backend with the confirmed wire scheme
// (`Authorization: Apikey <key>`) against the model-info path, a 200 maps to
// status ok, and the key is not persisted anywhere.
func TestBobKeyTest_PastedKeyOK_NotPersisted(t *testing.T) {
	path := isolateBobKeySources(t)
	var gotAuth, gotPath, gotUA string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotUA = r.Header.Get("User-Agent")
		if gotAuth != "Apikey "+bobTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"model":"auto"}`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, logBuf := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body: %s)", rec.Code, rec.Body.String())
	}
	if out["status"] != "ok" {
		t.Fatalf("verdict = %v, want ok (body: %s)", out["status"], rec.Body.String())
	}
	// Opaque (non-JWT) keys must use the Apikey scheme — the Bearer scheme is
	// a guaranteed "invalid jwt string" 401 for them.
	if gotAuth != "Apikey "+bobTestKey {
		t.Errorf("backend saw Authorization %q, want %q", gotAuth, "Apikey "+bobTestKey)
	}
	if gotPath != bobProbePath {
		t.Errorf("probe hit %q, want %q", gotPath, bobProbePath)
	}
	// The Cloudflare WAF in front of the bob backend blocks Go's default
	// User-Agent with an HTML 403 — the probe must identify as bobshell.
	if gotUA != bobProbeUserAgent {
		t.Errorf("backend saw User-Agent %q, want %q", gotUA, bobProbeUserAgent)
	}
	// Pre-save validation must not persist the key or touch config.
	if _, err := os.Stat(path); err == nil {
		t.Error("tested key was written to the PVC key file")
	}
	if s.deps.Config.Governor.Bob.APIKeyFile != "" {
		t.Error("test mutated governor.bob.api_key_file")
	}
	if strings.Contains(rec.Body.String(), bobTestKey) {
		t.Error("response body leaks the key")
	}
	if strings.Contains(logBuf.String(), bobTestKey) {
		t.Error("logs leak the key")
	}
}

// A 401 maps to reason "unauthorized", the backend's own words (body,
// WWW-Authenticate, request id) surface in the detail, and the key never
// appears in the response or logs even when the BACKEND echoes it back.
func TestBobKeyTest_Unauthorized_RichDetail_NoKeyMaterial(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", "APIKey realm=bob")
		w.Header().Set("X-Request-Id", "req-42")
		w.WriteHeader(http.StatusUnauthorized)
		// Hostile-backend case: the error body echoes the submitted key.
		w.Write([]byte(`{"error":"invalid api key ` + bobTestKey + ` — wrong scope: needs Inference"}`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, logBuf := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)

	if out["status"] != "error" || out["reason"] != bobTestReasonUnauthorized {
		t.Fatalf("verdict = %v/%v, want error/unauthorized (body: %s)", out["status"], out["reason"], rec.Body.String())
	}
	detail, _ := out["detail"].(string)
	// The backend's own words must be surfaced, not just the classification.
	for _, want := range []string{"invalid api key", "wrong scope: needs Inference", "WWW-Authenticate", "req-42"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q does not surface backend info %q", detail, want)
		}
	}
	if strings.Contains(rec.Body.String(), bobTestKey) {
		t.Error("response body leaks the key the backend echoed")
	}
	if strings.Contains(logBuf.String(), bobTestKey) {
		t.Error("logs leak the key")
	}
}

// A connection-refused backend maps to reason "unreachable".
func TestBobKeyTest_Unreachable(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := backend.URL
	backend.Close() // now refuses connections
	t.Setenv(bobAPIBaseURLEnv, deadURL)

	s, logBuf := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)

	if out["status"] != "error" || out["reason"] != bobTestReasonUnreachable {
		t.Fatalf("verdict = %v/%v, want error/unreachable (body: %s)", out["status"], out["reason"], rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), bobTestKey) || strings.Contains(logBuf.String(), bobTestKey) {
		t.Error("key material leaked on transport failure")
	}
}

// An empty body tests the SAVED key: the handler resolves it through the
// same BobConfig.ResolveAPIKey the agents use, reads the key file, and the
// value stays inside the process (never echoed).
func TestBobKeyTest_SavedKeyMode_ReadsKeyFile(t *testing.T) {
	path := isolateBobKeySources(t)
	if err := writeBobKeyFile(bobTestKey); err != nil {
		t.Fatal(err)
	}

	var gotAuth string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte(`{"model":"auto"}`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, logBuf := newBobProbeServer(t)
	s.deps.Config.Governor.Bob.APIKeyFile = path

	rec, out := postBobKeyTest(t, s, `{}`)
	if out["status"] != "ok" {
		t.Fatalf("verdict = %v, want ok (body: %s)", out["status"], rec.Body.String())
	}
	if gotAuth != "Apikey "+bobTestKey {
		t.Errorf("backend saw Authorization %q, want the saved key with the Apikey scheme", gotAuth)
	}
	if strings.Contains(rec.Body.String(), bobTestKey) || strings.Contains(logBuf.String(), bobTestKey) {
		t.Error("saved-key test leaked key material")
	}
}

// No pasted key and no saved key is a distinct, actionable verdict — not a
// backend round-trip.
func TestBobKeyTest_NoKeyConfigured(t *testing.T) {
	isolateBobKeySources(t)
	// Any backend hit here is a bug: there is no key to test with.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("probe reached the backend without a key")
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{}`)
	if out["status"] != "error" || out["reason"] != bobTestReasonNoKey {
		t.Fatalf("verdict = %v/%v, want error/no_key (body: %s)", out["status"], out["reason"], rec.Body.String())
	}
	_ = rec
}

// Guardrails: oversized pasted keys and malformed bodies are rejected before
// any network I/O; an absent body behaves like {} (saved-key mode).
func TestBobKeyTest_BadRequests(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("rejected request must not reach the backend")
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)

	rec, _ := postBobKeyTest(t, s, `{"apiKey":"`+strings.Repeat("x", bobKeyMaxLen+1)+`"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized key: status = %d, want 400", rec.Code)
	}

	rec, _ = postBobKeyTest(t, s, `not json`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed body: status = %d, want 400", rec.Code)
	}

	// Absent body == saved-key mode; with no key saved that is the no_key
	// verdict, not a 400.
	req := httptest.NewRequest(http.MethodPost, "/api/config/governor/bob/test", nil)
	recNil := httptest.NewRecorder()
	s.handleGovernorBobKeyTest(recNil, req)
	if recNil.Code != http.StatusOK || !strings.Contains(recNil.Body.String(), bobTestReasonNoKey) {
		t.Errorf("absent body: status = %d body = %s, want 200/no_key", recNil.Code, recNil.Body.String())
	}
}

// The backend's "invalid jwt string" 401 is DECODED, not just surfaced: it
// means the key's delivery was broken (scheme/newline/perms), not that the
// key is bad, and the detail must say so.
func TestBobKeyTest_JWTVerdictDecoded(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"unauthorized","message":"Token verification failed: invalid jwt string"}`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)
	if out["reason"] != bobTestReasonUnauthorized {
		t.Fatalf("reason = %v, want unauthorized (body: %s)", out["reason"], rec.Body.String())
	}
	detail, _ := out["detail"].(string)
	for _, want := range []string{"invalid jwt string", "stray newline", "agent UID"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing decoded explanation fragment %q", detail, want)
		}
	}
}

// A 402 is a valid credential with no inference entitlement — its own reason
// with the create-a-scoped-key fix in the detail.
func TestBobKeyTest_EntitlementFailureDecoded(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusPaymentRequired)
		w.Write([]byte(`{"type":"insufficient_quota_or_plan_expired","message":"team user not found"}`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)
	if out["reason"] != bobTestReasonNoEntitlement {
		t.Fatalf("reason = %v, want no_entitlement (body: %s)", out["reason"], rec.Body.String())
	}
	detail, _ := out["detail"].(string)
	for _, want := range []string{"Scope: Inference", "bob.ibm.com/admin/apikeys", "team user not found"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q missing %q", detail, want)
		}
	}
}

// A Cloudflare-style HTML 403 is a network/edge block, not a key verdict —
// it must classify as unreachable, never unauthorized.
func TestBobKeyTest_EdgeHTML403IsNotAKeyVerdict(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`<!DOCTYPE html><html><body>Access denied | cloudflare</body></html>`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)
	if out["reason"] != bobTestReasonUnreachable {
		t.Fatalf("reason = %v, want unreachable for an HTML edge 403 (body: %s)", out["reason"], rec.Body.String())
	}
	detail, _ := out["detail"].(string)
	if !strings.Contains(detail, "network edge") {
		t.Errorf("detail %q does not explain the edge block", detail)
	}
}

// Interior whitespace in a pasted key is refused with the real explanation
// before any network I/O — a broken paste is a proven 401.
func TestBobKeyTest_InteriorWhitespaceRefused(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("a malformed key must not reach the backend")
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"bob_prod_abc\ndef"}`)
	if out["reason"] != bobTestReasonBadKeyFormat {
		t.Fatalf("reason = %v, want bad_key_format (body: %s)", out["reason"], rec.Body.String())
	}
	detail, _ := out["detail"].(string)
	if !strings.Contains(detail, "single unbroken token") {
		t.Errorf("detail %q missing the re-copy guidance", detail)
	}
}

// The SAVE handler refuses interior whitespace too — storing a broken paste
// recreates the exact crash-loop the test feature exists to prevent.
func TestHandleGovernorBobKey_InteriorWhitespaceRefused(t *testing.T) {
	path := isolateBobKeySources(t)
	s, _ := newBobProbeServer(t)
	req := httptest.NewRequest(http.MethodPut, "/api/config/governor/bob",
		strings.NewReader(`{"apiKey":"bob_prod_abc def"}`))
	rec := httptest.NewRecorder()
	markOwnerRequest(req)
	s.handleGovernorBobKey(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body: %s)", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("broken key was persisted")
	}
}

// A JWT-shaped credential keeps the Bearer scheme.
func TestBobAuthHeaderValue_SchemeByKeyShape(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJib2IifQ.c2ln"
	if got := bobAuthHeaderValue(jwt); got != "Bearer "+jwt {
		t.Errorf("JWT-shaped key: got %q, want Bearer scheme", got)
	}
	if got := bobAuthHeaderValue("bob_prod_opaque123"); got != "Apikey bob_prod_opaque123" {
		t.Errorf("opaque key: got %q, want Apikey scheme", got)
	}
}

// Non-auth backend failures (e.g. a 500) map to bad_status and still surface
// the backend's message.
func TestBobKeyTest_BadStatusSurfacesBackendMessage(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("bob backend melted"))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	rec, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)
	if out["reason"] != bobTestReasonBadStatus {
		t.Fatalf("reason = %v, want bad_status (body: %s)", out["reason"], rec.Body.String())
	}
	detail, _ := out["detail"].(string)
	if !strings.Contains(detail, "HTTP 500") || !strings.Contains(detail, "bob backend melted") {
		t.Errorf("detail %q missing status or backend message", detail)
	}
}

// bobProbeBaseURL uses the default URL when the env override is not set.
func TestBobProbeBaseURL_DefaultWhenNoEnv(t *testing.T) {
	t.Setenv(bobAPIBaseURLEnv, "")
	got := bobProbeBaseURL()
	if got != defaultBobAPIBaseURL {
		t.Errorf("bobProbeBaseURL() = %q, want default %q", got, defaultBobAPIBaseURL)
	}
}

// A non-HTML 403 from the backend (JSON auth rejection) maps to unauthorized,
// not unreachable — only HTML edge pages get the "network edge" classification.
func TestBobKeyTest_JSON403IsUnauthorized(t *testing.T) {
	isolateBobKeySources(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte(`{"error":"access denied","reason":"key revoked"}`))
	}))
	defer backend.Close()
	t.Setenv(bobAPIBaseURLEnv, backend.URL)

	s, _ := newBobProbeServer(t)
	_, out := postBobKeyTest(t, s, `{"apiKey":"`+bobTestKey+`"}`)
	if out["reason"] != bobTestReasonUnauthorized {
		t.Fatalf("reason = %v, want unauthorized for a JSON 403", out["reason"])
	}
	detail, _ := out["detail"].(string)
	if !strings.Contains(detail, "key revoked") {
		t.Errorf("detail %q missing backend message", detail)
	}
}

// bobLooksLikeEdgeHTML detects an HTML body prefix even without Content-Type.
func TestBobLooksLikeEdgeHTML_BodyPrefix(t *testing.T) {
	tests := []struct {
		name string
		ct   string
		body string
		want bool
	}{
		{"DOCTYPE prefix", "", "<!DOCTYPE html><html>block</html>", true},
		{"html tag prefix", "", "<html><body>blocked</body></html>", true},
		{"JSON body not HTML", "application/json", `{"error":"forbidden"}`, false},
		{"empty body not HTML", "", "", false},
		{"text/html content-type", "text/html", "anything", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := &http.Response{Header: http.Header{}}
			if tt.ct != "" {
				resp.Header.Set("Content-Type", tt.ct)
			}
			got := bobLooksLikeEdgeHTML(resp, tt.body)
			if got != tt.want {
				t.Errorf("bobLooksLikeEdgeHTML(ct=%q, body=%q) = %v, want %v", tt.ct, tt.body, got, tt.want)
			}
		})
	}
}

// A timeout from the backend maps to reason "timeout" with a duration hint.
// This test uses a TCP listener that accepts but never writes — forcing the
// HTTP client to hit its deadline.
func TestBobKeyTest_Timeout(t *testing.T) {
	if testing.Short() {
		t.Skip("timeout test requires 10s+ wait; skipped in -short mode")
	}
	isolateBobKeySources(t)
	// Create a raw TCP listener that accepts connections but never responds,
	// forcing the HTTP client's Timeout to fire.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			// Hold connection open, never write — triggers client timeout.
			defer conn.Close()
		}
	}()
	t.Setenv(bobAPIBaseURLEnv, "http://"+ln.Addr().String())

	reason, detail := probeBobAPIKey(bobTestKey)
	if reason != bobTestReasonTimeout {
		t.Fatalf("reason = %q, want timeout", reason)
	}
	if !strings.Contains(detail, "did not respond within") {
		t.Errorf("detail %q missing timeout explanation", detail)
	}
}
