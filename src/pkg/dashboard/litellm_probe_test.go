package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The Test Connection probe must accept the exact OpenAI-shaped /v1/models
// payload the live LiteLLM gateway returns — including per-item object /
// created / owned_by fields and provider-prefixed, hyphenated model ids — and
// report the real model count. Before the fix the probe read only a truncated
// prefix of the body, so a large valid list decoded as invalid JSON and
// produced a false "non-OpenAI response" negative.
func TestProbeLiteLLMModels_AcceptsFullOpenAIPayload(t *testing.T) {
	// Captured-shape payload: top-level {"data":[...],"object":"list"}, each
	// item id/object/created/owned_by, ids with Azure/ aws/ azure/ prefixes
	// and hyphenated versions. Padded well past the old 512-byte read cap.
	const modelCount = 40
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i := 0; i < modelCount; i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b,
				`{"id":"Azure/gpt-5.1-codex-2025-11-13-variant-%02d","object":"model","created":1677610602,"owned_by":"openai"}`, i)
		}
		b.WriteString(`],"object":"list"}`)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, b.String())
	}))
	defer srv.Close()

	n, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err != nil {
		t.Fatalf("probe failed on a valid OpenAI payload: %v", err)
	}
	if n != modelCount {
		t.Errorf("probe reported %d models, want %d", n, modelCount)
	}
}

// The probe must not require top-level object=="list" (some gateways omit it)
// nor a specific field order, and must tolerate unknown fields — exactly what
// the discovery dropdown parser accepts.
func TestProbeLiteLLMModels_LenientShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No top-level "object"; extra unknown top-level + per-item fields;
		// provider-prefixed and hyphenated ids.
		fmt.Fprint(w, `{"unexpected_top":true,"data":[`+
			`{"owned_by":"aws","id":"aws/claude-opus-4-7","object":"model","extra":123},`+
			`{"id":"azure/gemini-2.5-flash"}`+
			`]}`)
	}))
	defer srv.Close()

	n, err := probeLiteLLMModels(srv.URL, "")
	if err != nil {
		t.Fatalf("probe rejected a lenient-but-valid payload: %v", err)
	}
	if n != 2 {
		t.Errorf("probe reported %d models, want 2", n)
	}
}

func TestProbeLiteLLMModelsRejectsRedirectToPrivateHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/meta-data/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"stolen-token-sink"}]}`)
			return
		}
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/v1/models", http.StatusFound)
	}))
	defer srv.Close()
	routeImportFetchesToServer(t, srv)

	if _, err := probeLiteLLMModels("https://gateway.example.invalid", "sk-livekeyvalue"); err == nil {
		t.Fatal("expected redirect to private host to be blocked")
	} else if !strings.Contains(err.Error(), "redirect to private/internal host blocked") {
		t.Fatalf("expected private redirect error, got %v", err)
	}
}

// parseModelsResponse is the shared source of truth; verify it returns
// provider-prefixed ids verbatim and skips id-less entries.
func TestParseModelsResponse_VerbatimIDs(t *testing.T) {
	body := `{"object":"list","data":[` +
		`{"id":"Azure/gpt-5.1-codex-2025-11-13"},` +
		`{"id":"claude-opus-4-7"},` +
		`{"object":"model"}` + // no id — skipped
		`]}`
	models, err := parseModelsResponse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"Azure/gpt-5.1-codex-2025-11-13", "claude-opus-4-7"}
	if len(models) != len(want) {
		t.Fatalf("got %v, want %v", models, want)
	}
	for i, m := range models {
		if m != want[i] {
			t.Errorf("models[%d] = %q, want %q", i, m, want[i])
		}
	}
}

// An HTML "403 Forbidden" page comes from an ingress/WAF/VPN proxy in front of
// the gateway, not from the gateway's auth layer: the probe must say so —
// naming the URL it tried — instead of claiming the configured key was
// rejected, which sent a reporter chasing a key that worked fine for
// /v1/completions (hivecommons/hive#9945).
func TestProbeLiteLLMModels_HTMLForbiddenBlamesProxyNotKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("Server", "nginx")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<html>\n<head><title>403 Forbidden</title></head>\n<body><center><h1>403 Forbidden</h1></center></body>\n</html>")
	}))
	defer srv.Close()

	_, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err == nil {
		t.Fatal("expected an error for an HTTP 403 probe response")
	}
	msg := err.Error()
	if strings.Contains(msg, "gateway rejected the configured key") {
		t.Errorf("HTML 403 must not be attributed to the key: %v", err)
	}
	for _, want := range []string{"proxy in front of the gateway", srv.URL + "/v1/models", "nginx", "403 Forbidden"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

// A JSON auth error IS the gateway speaking, so the key-specific message (and
// the probed URL) must survive.
func TestProbeLiteLLMModels_JSONForbiddenStillBlamesKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":{"message":"token not found","type":"auth_error"}}`)
	}))
	defer srv.Close()

	_, err := probeLiteLLMModels(srv.URL, "sk-livekeyvalue")
	if err == nil {
		t.Fatal("expected an error for an HTTP 403 probe response")
	}
	for _, want := range []string{"gateway rejected the configured key", srv.URL + "/v1/models", "token not found"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestLooksLikeIntermediaryRejection(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		want        bool
	}{
		{"html page", "text/html", "<html><title>403 Forbidden</title></html>", true},
		{"html body without content type", "", "<html>403</html>", true},
		{"empty body", "", "", true},
		{"plain text refusal", "text/plain", "access denied", false},
		{"gateway json", "application/json", `{"error":"token not found"}`, false},
		{"json body without content type", "", `{"error":"token not found"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeIntermediaryRejection(tc.contentType, tc.body); got != tc.want {
				t.Errorf("looksLikeIntermediaryRejection(%q, %q) = %v, want %v", tc.contentType, tc.body, got, tc.want)
			}
		})
	}
}
