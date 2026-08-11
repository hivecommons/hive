package proxy

import (
	"net/http"
	"testing"
)

// TestApplyInferenceAuth covers applyInferenceAuth, whose ExtraHeaders and
// Authorization-guard branches were previously uncovered (#3311). The security
// contract: the bearer comes only from APIKey, and ExtraHeaders (watsonx's
// X-IBM-Project-ID) can never override or inject the Authorization header.
func TestApplyInferenceAuth(t *testing.T) {
	newReq := func() *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "http://upstream/v1/chat/completions", nil)
		return r
	}

	t.Run("api key sets the bearer", func(t *testing.T) {
		req := newReq()
		applyInferenceAuth(req, &InferenceRoute{APIKey: "sk-secret"})
		if got := req.Header.Get("Authorization"); got != "Bearer sk-secret" {
			t.Errorf("Authorization = %q, want the bearer", got)
		}
	})

	t.Run("no api key leaves Authorization unset", func(t *testing.T) {
		req := newReq()
		applyInferenceAuth(req, &InferenceRoute{})
		if got := req.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization = %q, want empty", got)
		}
	})

	t.Run("extra headers are applied (watsonx project id)", func(t *testing.T) {
		req := newReq()
		applyInferenceAuth(req, &InferenceRoute{
			ExtraHeaders: map[string]string{"X-IBM-Project-ID": "proj-123"},
		})
		if got := req.Header.Get("X-IBM-Project-ID"); got != "proj-123" {
			t.Errorf("X-IBM-Project-ID = %q, want \"proj-123\"", got)
		}
	})

	t.Run("extra headers cannot override Authorization", func(t *testing.T) {
		req := newReq()
		applyInferenceAuth(req, &InferenceRoute{
			APIKey:       "sk-real",
			ExtraHeaders: map[string]string{"Authorization": "Bearer sk-injected", "authorization": "Bearer sk-lower"},
		})
		if got := req.Header.Get("Authorization"); got != "Bearer sk-real" {
			t.Errorf("Authorization = %q, want the APIKey bearer (ExtraHeaders must not override it)", got)
		}
	})

	t.Run("empty header name or value is skipped", func(t *testing.T) {
		req := newReq()
		applyInferenceAuth(req, &InferenceRoute{
			ExtraHeaders: map[string]string{"": "novalue", "X-Empty": ""},
		})
		if got := req.Header.Get("X-Empty"); got != "" {
			t.Errorf("empty-valued header was set = %q, want skipped", got)
		}
	})
}
