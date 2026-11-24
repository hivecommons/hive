package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

type openAIOnlyUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newOpenAIOnlyUpstream(t *testing.T) *openAIOnlyUpstream {
	t.Helper()
	u := &openAIOnlyUpstream{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.paths = append(u.paths, r.URL.Path)
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chat/completions":
			_, _ = io.WriteString(w, keyResolverOKResponse)
		case "/v1/models":
			_, _ = io.WriteString(w, `{"data":[{"id":"m","max_model_len":4096}]}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(u.srv.Close)
	return u
}

func (u *openAIOnlyUpstream) lastPath() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.paths) == 0 {
		return ""
	}
	return u.paths[len(u.paths)-1]
}

func (u *openAIOnlyUpstream) endpoints() map[string]string {
	return map[string]string{"root": u.srv.URL, "v1": u.srv.URL + "/v1", "v1 slash": u.srv.URL + "/v1/"}
}

func TestInferenceProxyAcceptsV1Endpoint(t *testing.T) {
	up := newOpenAIOnlyUpstream(t)
	for form, endpoint := range up.endpoints() {
		route := &InferenceRoute{Backend: "litellm", Endpoint: endpoint, Model: "m", APIKey: "k"}
		t.Run(form+"/translator", func(t *testing.T) {
			var logs bytes.Buffer
			if code := serveTranslator(t, newKeyResolverTestProxy(&logs), route); code != http.StatusOK || up.lastPath() != "/v1/chat/completions" {
				t.Fatalf("status %d, upstream path %q, want 200 on /v1/chat/completions", code, up.lastPath())
			}
		})
		t.Run(form+"/mitm", func(t *testing.T) {
			var logs bytes.Buffer
			if code := serveMITM(t, newKeyResolverTestProxy(&logs), route); code != http.StatusOK || up.lastPath() != "/v1/chat/completions" {
				t.Fatalf("status %d, upstream path %q, want 200 on /v1/chat/completions", code, up.lastPath())
			}
		})
		t.Run(form+"/forward", func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(keyResolverTestBody))
			w := httptest.NewRecorder()
			if err := forwardToInference(req, []byte(keyResolverTestBody), w, route, "scanner"); err != nil || up.lastPath() != "/v1/chat/completions" {
				t.Fatalf("err %v, upstream path %q, want /v1/chat/completions", err, up.lastPath())
			}
		})
		t.Run(form+"/models", func(t *testing.T) {
			if got := queryMaxModelLen(endpoint, "m", "", ""); got != 4096 || up.lastPath() != "/v1/models" {
				t.Fatalf("max_model_len %d, upstream path %q, want 4096 from /v1/models", got, up.lastPath())
			}
			if got := FindEndpointForModel([]string{endpoint}, "m", "", ""); got != endpoint {
				t.Fatalf("FindEndpointForModel = %q, want %q", got, endpoint)
			}
		})
	}
}
