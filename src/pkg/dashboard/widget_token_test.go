package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func isolateWidgetTokenFile(t *testing.T) {
	t.Helper()
	old := widgetTokenPath
	widgetTokenPath = filepath.Join(t.TempDir(), "widget-token.json")
	t.Cleanup(func() { widgetTokenPath = old })
}

func serverWithWidgetToken(t *testing.T) *Server {
	t.Helper()
	isolateWidgetTokenFile(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	s := NewServerWithAuth(0, "dashboard-secret", logger)
	s.RegisterAPI(testDeps(t))
	return s
}

func mintWidgetTokenAsOwner(t *testing.T, s *Server) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/widget/token", strings.NewReader(`{"hiveUrl":"https://hive.example.test/some/path"}`))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	s.handleWidgetTokenMint(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("owner mint = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode mint response: %v", err)
	}
	return body
}

func TestWidgetTokenMintRequiresOwner(t *testing.T) {
	s := serverWithWidgetToken(t)
	for _, role := range []string{"", "read", "readwrite", "merger"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/widget/token", nil)
		if role != "" {
			req.Header.Set("X-Hive-Role", role)
		}
		s.handleWidgetTokenMint(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("role %q mint = %d, want 403", role, rec.Code)
		}
	}
	// An unverified owner header (no server-set marker) is not an owner.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/widget/token", nil)
	req.Header.Set("X-Hive-Role", "owner")
	s.handleWidgetTokenMint(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("unverified owner mint = %d, want 403", rec.Code)
	}
	if rec, _ := loadWidgetTokenRecord(); rec != nil {
		t.Fatal("non-owner request must not create a widget token")
	}
}

func TestWidgetTokenRevokeAndStatusRequireOwner(t *testing.T) {
	s := serverWithWidgetToken(t)
	for _, h := range []http.HandlerFunc{s.handleWidgetTokenRevoke, s.handleWidgetTokenStatus} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/widget/token", nil)
		req.Header.Set("X-Hive-Role", "read")
		h(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("read role = %d, want 403", rec.Code)
		}
	}
}

func TestWidgetTokenMintStoresOnlyHashAndReturnsQR(t *testing.T) {
	s := serverWithWidgetToken(t)
	body := mintWidgetTokenAsOwner(t, s)

	token, _ := body["token"].(string)
	if !strings.HasPrefix(token, widgetTokenPrefix) || len(token) != len(widgetTokenPrefix)+2*widgetTokenBytes {
		t.Fatalf("token = %q, want %s + %d hex chars", token, widgetTokenPrefix, 2*widgetTokenBytes)
	}
	if got := body["hiveUrl"]; got != "https://hive.example.test" {
		t.Errorf("hiveUrl = %v, want origin only", got)
	}
	setupURL, _ := body["setupUrl"].(string)
	u, err := url.Parse(setupURL)
	if err != nil || u.Scheme != "scriptable" {
		t.Fatalf("setupUrl = %q, want a scriptable:// deep link", setupURL)
	}
	if u.Query().Get("widgetToken") != token || u.Query().Get("url") != "https://hive.example.test" {
		t.Errorf("setupUrl query = %v, want url + widgetToken", u.Query())
	}
	if qr, _ := body["qrDataUrl"].(string); !strings.HasPrefix(qr, "data:image/png;base64,") {
		t.Errorf("qrDataUrl = %.40q, want a PNG data URL", qr)
	}

	raw, err := os.ReadFile(widgetTokenFile())
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if strings.Contains(string(raw), token) {
		t.Fatal("token file must not contain the plaintext token")
	}
	if !strings.Contains(string(raw), sha256Hex(token)) {
		t.Fatal("token file must contain the token hash")
	}
	if info, err := os.Stat(widgetTokenFile()); err == nil && info.Mode().Perm() != 0o600 {
		t.Errorf("token file mode = %o, want 600", info.Mode().Perm())
	}
}

func TestWidgetTokenReadsOnlyTheWidgetSummary(t *testing.T) {
	s := serverWithWidgetToken(t)
	token := mintWidgetTokenAsOwner(t, s)["token"].(string)
	h := s.Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, widgetSummaryPath, nil)
	req.Header.Set(widgetTokenHeader, token)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("widget summary with token = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	var summary map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	for _, key := range []string{"hiveId", "history", "agents"} {
		if _, ok := summary[key]; !ok {
			t.Errorf("summary missing %q: %v", key, summary)
		}
	}

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodGet, "/api/status"},
		{http.MethodGet, "/api/status/summary"},
		{http.MethodGet, "/api/history"},
		{http.MethodGet, "/api/widget"},
		{http.MethodGet, "/api/widget/token"},
		{http.MethodPost, "/api/widget/token"},
		{http.MethodDelete, "/api/widget/token"},
		{http.MethodPost, "/api/pause"},
		{http.MethodPost, widgetSummaryPath},
	} {
		for _, viaAuthorization := range []bool{false, true} {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, nil)
			if viaAuthorization {
				req.Header.Set("Authorization", "Bearer "+token)
			} else {
				req.Header.Set(widgetTokenHeader, token)
			}
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s with widget token (authorization=%v) = %d, want 401", tc.method, tc.path, viaAuthorization, rec.Code)
			}
		}
	}

	// The widget token in Authorization is not accepted on the summary either:
	// only the dedicated header, which the hub never forwards to GitHub.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, widgetSummaryPath, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("summary with widget token in Authorization = %d, want 401", rec.Code)
	}
}

func TestWidgetSummaryRejectsMissingWrongAndRevokedTokens(t *testing.T) {
	s := serverWithWidgetToken(t)
	h := s.Handler()
	get := func(token string) int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, widgetSummaryPath, nil)
		if token != "" {
			req.Header.Set(widgetTokenHeader, token)
		}
		h.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := get(""); code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	if code := get(widgetTokenPrefix + strings.Repeat("0", 64)); code != http.StatusUnauthorized {
		t.Errorf("token before any mint = %d, want 401", code)
	}

	first := mintWidgetTokenAsOwner(t, s)["token"].(string)
	if code := get(first); code != http.StatusOK {
		t.Fatalf("fresh token = %d, want 200", code)
	}
	if code := get("dashboard-secret"); code != http.StatusUnauthorized {
		t.Errorf("shared dashboard token in widget header = %d, want 401", code)
	}
	if code := get(strings.TrimPrefix(first, widgetTokenPrefix)); code != http.StatusUnauthorized {
		t.Errorf("token without prefix = %d, want 401", code)
	}

	second := mintWidgetTokenAsOwner(t, s)["token"].(string)
	if code := get(first); code != http.StatusUnauthorized {
		t.Errorf("replaced token = %d, want 401", code)
	}
	if code := get(second); code != http.StatusOK {
		t.Fatalf("replacement token = %d, want 200", code)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/widget/token", nil)
	markOwnerRequest(req)
	s.handleWidgetTokenRevoke(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"revoked":true`) {
		t.Fatalf("revoke = %d %q", rec.Code, rec.Body.String())
	}
	if code := get(second); code != http.StatusUnauthorized {
		t.Errorf("revoked token = %d, want 401", code)
	}
	if removed, err := revokeWidgetToken(); err != nil || removed {
		t.Errorf("second revoke = (%v, %v), want idempotent (false, nil)", removed, err)
	}
}

func TestWidgetTokenStatusReportsConfigured(t *testing.T) {
	s := serverWithWidgetToken(t)
	status := func() map[string]any {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/widget/token", nil)
		markOwnerRequest(req)
		s.handleWidgetTokenStatus(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return body
	}
	if got := status()["configured"]; got != false {
		t.Fatalf("configured before mint = %v, want false", got)
	}
	token := mintWidgetTokenAsOwner(t, s)["token"].(string)
	body := status()
	if body["configured"] != true {
		t.Fatalf("configured after mint = %v, want true", body["configured"])
	}
	if strings.Contains(fmtJSON(t, body), token) {
		t.Fatal("status must never echo the token")
	}
}

func TestWidgetHiveURLFallsBackToRequestOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/widget/token", nil)
	req.Host = "spoke.example.test"
	req.Header.Set("X-Forwarded-Proto", "https")
	for _, claimed := range []string{"", "javascript:alert(1)", "scriptable:///run/x", "https://user:pw@evil.test"} {
		if got := widgetHiveURL(req, claimed); got != "https://spoke.example.test" {
			t.Errorf("widgetHiveURL(%q) = %q, want request origin", claimed, got)
		}
	}
}

func TestMintWidgetTokenIsUnique(t *testing.T) {
	isolateWidgetTokenFile(t)
	a, _, err := mintWidgetToken("owner", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := mintWidgetToken("owner", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two mints returned the same token")
	}
	if validWidgetToken(a) || !validWidgetToken(b) {
		t.Fatal("only the latest minted token may validate")
	}
}

func TestWidgetTokenRecordsLastUseWithoutResurrectingRevokedTokens(t *testing.T) {
	isolateWidgetTokenFile(t)
	token, _, err := mintWidgetToken("owner", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !validWidgetToken(token) {
		t.Fatal("fresh token did not validate")
	}
	rec, err := loadWidgetTokenRecord()
	if err != nil || rec == nil || rec.LastUsedAt == "" {
		t.Fatalf("lastUsedAt not recorded after use: %+v, %v", rec, err)
	}

	if _, err := revokeWidgetToken(); err != nil {
		t.Fatal(err)
	}
	touchWidgetToken(sha256Hex(token), time.Now().Add(time.Hour))
	if _, err := os.Stat(widgetTokenFile()); !os.IsNotExist(err) {
		t.Fatalf("touch after revoke recreated the token file (stat err=%v)", err)
	}
	if validWidgetToken(token) {
		t.Fatal("revoked token validated after a concurrent touch")
	}
}

// The "Add to Home Screen" bookmark gets the bee icon and a proper name.
func TestPWAManifestAndTouchIconEmbeddedAndLinked(t *testing.T) {
	raw, err := fs.ReadFile(staticFS, "static/manifest.webmanifest")
	if err != nil {
		t.Fatalf("manifest not embedded: %v", err)
	}
	var manifest struct {
		ShortName string `json:"short_name"`
		StartURL  string `json:"start_url"`
		Icons     []struct {
			Src string `json:"src"`
		} `json:"icons"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if manifest.ShortName != "Hive" || manifest.StartURL != "/" || len(manifest.Icons) == 0 || manifest.Icons[0].Src != "/apple-touch-icon.png" {
		t.Errorf("unexpected manifest: %+v", manifest)
	}
	icon, err := fs.ReadFile(staticFS, "static/apple-touch-icon.png")
	if err != nil {
		t.Fatalf("apple-touch-icon not embedded: %v", err)
	}
	if !bytes.HasPrefix(icon, []byte("\x89PNG\r\n\x1a\n")) {
		t.Error("apple-touch-icon.png is not a PNG")
	}
	index, err := fs.ReadFile(staticFS, "static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`<meta name="apple-mobile-web-app-title" content="Hive">`,
	} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html missing %s", want)
		}
	}
}

func fmtJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
