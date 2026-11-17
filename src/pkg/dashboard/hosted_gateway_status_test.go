package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A JSON 502 the spoke answers on a hosted request must reach the owner with
// its own body, not the Ingress's generic "hive unavailable" page
// (hivecommons/hive#18598).
func TestHostedGatewayPassthroughStatus(t *testing.T) {
	s := NewServer(0, nil)
	const refusal = "quorum meeting.join: blocked: the bot has no permission to connect to that voice channel"
	jsonFail := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { jsonError(w, refusal, code) })
	}
	htmlFail := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html>upstream</html>"))
	})
	implicitOK := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"ok":true}`)) })

	cases := []struct {
		name       string
		h          http.Handler
		path       string
		hosted     bool
		wantStatus int
		wantOrig   string
	}{
		{"hosted JSON 502 passes through", jsonFail(http.StatusBadGateway), "/api/meetings/mtg-1/approve", true, hostedGatewayPassthroughStatus, "502"},
		{"self-hosted keeps 502", jsonFail(http.StatusBadGateway), "/api/meetings/mtg-1/approve", false, http.StatusBadGateway, ""},
		{"non-API path keeps 502", jsonFail(http.StatusBadGateway), "/snapshot", true, http.StatusBadGateway, ""},
		{"non-JSON 502 keeps 502", htmlFail, "/api/x", true, http.StatusBadGateway, ""},
		{"503 is left alone", jsonFail(http.StatusServiceUnavailable), "/api/meetings", true, http.StatusServiceUnavailable, ""},
		{"other errors unchanged", jsonFail(http.StatusConflict), "/api/meetings", true, http.StatusConflict, ""},
		{"implicit 200 unchanged", implicitOK, "/api/meetings", true, http.StatusOK, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, c.path, nil)
			if c.hosted {
				req.Header.Set(proxyAuthHeader, "proof")
			}
			rec := httptest.NewRecorder()
			s.withHostedGatewayPassthrough(c.h).ServeHTTP(rec, req)
			if rec.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, c.wantStatus)
			}
			if got := rec.Header().Get(hostedGatewayOriginalStatusHeader); got != c.wantOrig {
				t.Errorf("%s = %q, want %q", hostedGatewayOriginalStatusHeader, got, c.wantOrig)
			}
			if c.wantOrig != "" && !strings.Contains(rec.Body.String(), refusal) {
				t.Errorf("body lost the spoke's error: %s", rec.Body.String())
			}
		})
	}
}

// The wrapper is in the served handler chain, and keeps Flush and Unwrap
// working for streaming handlers.
func TestHostedGatewayPassthroughWiredAndStreams(t *testing.T) {
	s := newMinimalServer(t)
	s.mux.HandleFunc("GET /api/test-gateway-502", func(w http.ResponseWriter, _ *http.Request) {
		jsonError(w, "upstream said no", http.StatusBadGateway)
	})
	req := httptest.NewRequest(http.MethodGet, "/api/test-gateway-502", nil)
	req.Header.Set(proxyAuthHeader, "proof")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// authenticate may refuse a wrong proof first; either way the Ingress
	// must never see a JSON 502 from a hosted request.
	if rec.Code == http.StatusBadGateway {
		t.Fatalf("hosted JSON 502 reached the Ingress unchanged: %s", rec.Body.String())
	}

	gw := &hostedGatewayStatusWriter{ResponseWriter: httptest.NewRecorder()}
	gw.Flush()
	if !gw.wrote {
		t.Error("Flush before WriteHeader must commit a 200")
	}
	if _, ok := gw.Unwrap().(*httptest.ResponseRecorder); !ok {
		t.Error("Unwrap must return the wrapped writer")
	}
	if _, _, err := gw.Hijack(); err == nil {
		t.Error("Hijack on a non-hijacker must fail")
	}
	n, err := (&hostedGatewayStatusWriter{ResponseWriter: httptest.NewRecorder()}).ReadFrom(strings.NewReader("abc"))
	if err != nil || n != 3 {
		t.Errorf("ReadFrom = %d, %v", n, err)
	}
}
