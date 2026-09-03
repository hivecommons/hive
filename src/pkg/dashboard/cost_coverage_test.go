package dashboard

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/tokens"
)

func covLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// covServer builds a Server with registered API routes over testDeps.
func covServer(t *testing.T) (*Server, *Dependencies) {
	t.Helper()
	s := NewServer(0, covLogger())
	deps := testDeps(t)
	s.RegisterAPI(deps)
	return s, deps
}

func TestCov_HandleCost_NoGateways(t *testing.T) {
	s, _ := covServer(t)
	rec := doGet(s, "/api/cost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Disclaimer == "" {
		t.Error("expected disclaimer")
	}
	if resp.Gateways == nil {
		t.Error("expected non-nil gateways slice")
	}
}

func TestCov_HandleCost_WithGateways(t *testing.T) {
	// OpenRouter-style server
	or := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"data":{"usage":1.5,"limit":10,"limit_remaining":8.5}}`))
	}))
	defer or.Close()
	// LiteLLM-style server
	ll := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"info":{"spend":2.0,"max_budget":5.0}}`))
	}))
	defer ll.Close()

	t.Setenv("COV_OR_KEY", "sk-or-xyz")
	t.Setenv("COV_LL_KEY", "sk-ll-xyz")

	s, deps := covServer(t)
	deps.Config.Governor.Gateways = []config.GatewayConfig{
		{Name: "or", Kind: config.GatewayKindOpenRouter, Endpoint: or.URL, APIKeyEnv: "COV_OR_KEY"},
		{Name: "ll", Kind: config.GatewayKindLiteLLM, Endpoint: ll.URL, APIKeyEnv: "COV_LL_KEY"},
		{Name: "vllm", Kind: config.GatewayKindVLLM, Endpoint: "http://localhost:1", APIKeyEnv: "COV_OR_KEY"},
	}

	rec := doGet(s, "/api/cost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Two metered gateways probed (vllm contributes none).
	if len(resp.Gateways) != 2 {
		t.Fatalf("gateways len = %d, want 2: %+v", len(resp.Gateways), resp.Gateways)
	}
}

func TestCov_OpenRouterCost(t *testing.T) {
	t.Setenv("COV_OR_KEY", "sk-or")
	s := NewServer(0, covLogger())

	t.Run("success", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"data":{"usage":3.25,"limit":20,"limit_remaining":16.75}}`))
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "or", Kind: config.GatewayKindOpenRouter, Endpoint: ts.URL, APIKeyEnv: "COV_OR_KEY"}
		out := s.openRouterCost(context.Background(), gw)
		if out.Error != "" {
			t.Fatalf("unexpected error: %s", out.Error)
		}
		if out.SpentUSD == nil || *out.SpentUSD != 3.25 {
			t.Errorf("SpentUSD = %v, want 3.25", out.SpentUSD)
		}
		if out.Source != "native" {
			t.Errorf("Source = %q", out.Source)
		}
	})

	t.Run("no_key", func(t *testing.T) {
		gw := config.GatewayConfig{Name: "or", Kind: config.GatewayKindOpenRouter, Endpoint: "http://x"}
		out := s.openRouterCost(context.Background(), gw)
		if out.Error == "" {
			t.Error("expected no-key error")
		}
	})

	t.Run("non_2xx", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom sk-or leaked", http.StatusInternalServerError)
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "or", Kind: config.GatewayKindOpenRouter, Endpoint: ts.URL, APIKeyEnv: "COV_OR_KEY"}
		out := s.openRouterCost(context.Background(), gw)
		if out.Error == "" {
			t.Error("expected error on 500")
		}
	})

	t.Run("malformed_json", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`not json`))
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "or", Kind: config.GatewayKindOpenRouter, Endpoint: ts.URL, APIKeyEnv: "COV_OR_KEY"}
		out := s.openRouterCost(context.Background(), gw)
		if out.Error == "" {
			t.Error("expected malformed-json error")
		}
	})
}

func TestCov_LiteLLMCost(t *testing.T) {
	t.Setenv("COV_LL_KEY", "sk-ll")
	s := NewServer(0, covLogger())

	t.Run("nested_shape", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"info":{"spend":2.5,"max_budget":10.0}}`))
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "ll", Kind: config.GatewayKindLiteLLM, Endpoint: ts.URL, APIKeyEnv: "COV_LL_KEY"}
		out := s.liteLLMCost(context.Background(), gw)
		if out.Error != "" {
			t.Fatalf("error: %s", out.Error)
		}
		if out.SpentUSD == nil || *out.SpentUSD != 2.5 {
			t.Errorf("SpentUSD = %v", out.SpentUSD)
		}
		if out.RemainUSD == nil || *out.RemainUSD != 7.5 {
			t.Errorf("RemainUSD = %v, want 7.5", out.RemainUSD)
		}
	})

	t.Run("flat_shape", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"spend":4.0}`))
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "ll", Kind: config.GatewayKindLiteLLM, Endpoint: ts.URL, APIKeyEnv: "COV_LL_KEY"}
		out := s.liteLLMCost(context.Background(), gw)
		if out.Error != "" {
			t.Fatalf("error: %s", out.Error)
		}
		if out.SpentUSD == nil || *out.SpentUSD != 4.0 {
			t.Errorf("SpentUSD = %v", out.SpentUSD)
		}
		if out.RemainUSD != nil {
			t.Errorf("RemainUSD should be nil without max_budget")
		}
	})

	t.Run("missing_spend", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"info":{"max_budget":10.0}}`))
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "ll", Kind: config.GatewayKindLiteLLM, Endpoint: ts.URL, APIKeyEnv: "COV_LL_KEY"}
		out := s.liteLLMCost(context.Background(), gw)
		if out.Error == "" {
			t.Error("expected missing-spend error")
		}
	})

	t.Run("no_key", func(t *testing.T) {
		gw := config.GatewayConfig{Name: "ll", Kind: config.GatewayKindLiteLLM, Endpoint: "http://x"}
		out := s.liteLLMCost(context.Background(), gw)
		if out.Error == "" {
			t.Error("expected no-key error")
		}
	})

	t.Run("malformed", func(t *testing.T) {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`}{`))
		}))
		defer ts.Close()
		gw := config.GatewayConfig{Name: "ll", Kind: config.GatewayKindLiteLLM, Endpoint: ts.URL, APIKeyEnv: "COV_LL_KEY"}
		out := s.liteLLMCost(context.Background(), gw)
		if out.Error == "" {
			t.Error("expected malformed error")
		}
	})
}

func TestCov_FetchCostJSON_TransportError(t *testing.T) {
	s := NewServer(0, covLogger())
	// Unreachable endpoint → transport error branch.
	_, err := s.fetchCostJSON(context.Background(), "http://127.0.0.1:1/key", "k", "")
	if err == nil {
		t.Error("expected transport error")
	}
}

func TestCov_CostHTTPClient(t *testing.T) {
	if c := costHTTPClient(""); c == nil {
		t.Fatal("nil client for empty bundle")
	}
	// Unreadable / nonexistent file → falls back to default client.
	if c := costHTTPClient(filepath.Join(t.TempDir(), "nope.pem")); c == nil {
		t.Fatal("nil client for missing bundle")
	}
	// Existing but non-PEM file → AppendCertsFromPEM fails, falls back.
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := costHTTPClient(bad); c == nil {
		t.Fatal("nil client for bad-pem bundle")
	}
	// Valid PEM cert → custom transport branch.
	good := filepath.Join(t.TempDir(), "good.pem")
	if err := os.WriteFile(good, []byte(covSelfSignedPEM), 0o600); err != nil {
		t.Fatal(err)
	}
	c := costHTTPClient(good)
	if c == nil {
		t.Fatal("nil client for valid-pem bundle")
	}
	if c.Transport == nil {
		t.Error("expected custom transport for valid CA bundle")
	}
}

func TestCov_EstimatedCost_NoTokens(t *testing.T) {
	s := NewServer(0, covLogger())
	est := s.estimatedCost()
	if est.ByModel == nil || est.ByAgent == nil || est.UnpricedModels == nil {
		t.Error("expected non-nil slices from empty estimated cost")
	}
}

func TestCov_FlattenEstimated(t *testing.T) {
	est := tokens.EstimatedCost{
		TotalUSD: 12.5,
		ByModel: map[string]tokens.ModelCost{
			"sonnet": {USD: 10, Priced: true, Input: 100, Output: 50, CacheRead: 5, CacheCreate: 2},
			"weird":  {USD: 0, Priced: false, Input: 3},
		},
		ByAgent: map[string]tokens.ModelCost{
			"scanner": {USD: 2.5, Priced: true, Input: 20, Output: 10},
		},
		UnpricedModels: []string{"weird"},
	}
	out := flattenEstimated(est)
	if out.TotalUSD != 12.5 {
		t.Errorf("TotalUSD = %v", out.TotalUSD)
	}
	if len(out.ByModel) != 2 {
		t.Errorf("ByModel len = %d, want 2", len(out.ByModel))
	}
	if len(out.ByAgent) != 1 {
		t.Errorf("ByAgent len = %d, want 1", len(out.ByAgent))
	}
	// Verify priced/unpriced source tagging came through.
	var sawUnpriced, sawEstimated bool
	for _, m := range out.ByModel {
		switch m.Source {
		case "unpriced":
			sawUnpriced = true
		case "estimated":
			sawEstimated = true
		}
	}
	if !sawUnpriced || !sawEstimated {
		t.Errorf("expected both estimated and unpriced source tags; got unpriced=%v estimated=%v", sawUnpriced, sawEstimated)
	}
}

func TestCov_SourceForPriced(t *testing.T) {
	if got := sourceForPriced(true); got != "estimated" {
		t.Errorf("priced -> %q", got)
	}
	if got := sourceForPriced(false); got != "unpriced" {
		t.Errorf("unpriced -> %q", got)
	}
}

func TestCov_HandleCost_WithPRIssueCounts(t *testing.T) {
	s, deps := covServer(t)
	deps.MetricsCollector = &MetricsCollector{
		metrics:       make(map[string]any),
		prIssueCounts: &ghpkg.PRIssueCounts{MergedPRs: 9, ClosedIssues: 4, UpdatedAt: "2026-08-18T00:00:00Z"},
	}

	rec := doGet(s, "/api/cost")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MergedPRs != 9 {
		t.Errorf("MergedPRs = %d, want 9", resp.MergedPRs)
	}
	if resp.ClosedIssues != 4 {
		t.Errorf("ClosedIssues = %d, want 4", resp.ClosedIssues)
	}
}

func TestCov_HandleCost_NoPRIssueCounts(t *testing.T) {
	s, _ := covServer(t)
	rec := doGet(s, "/api/cost")
	var resp costResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.MergedPRs != 0 || resp.ClosedIssues != 0 {
		t.Errorf("expected zero counts with no MetricsCollector, got merged=%d closed=%d", resp.MergedPRs, resp.ClosedIssues)
	}
}

// covSelfSignedPEM is a throwaway self-signed CA cert used only to exercise the
// costHTTPClient custom-transport branch (never used to make a real request).
const covSelfSignedPEM = `-----BEGIN CERTIFICATE-----
MIIBhTCCASugAwIBAgIQIRi6zePL6mKjOipn+dNuaTAKBggqhkjOPQQDAjASMRAw
DgYDVQQKEwdBY21lIENvMB4XDTE3MTAyMDE5NDMwNloXDTE4MTAyMDE5NDMwNlow
EjEQMA4GA1UEChMHQWNtZSBDbzBZMBMGByqGSM49AgEGCCqGSM49AwEHA0IABD0d
7VNhbWvZLWPuj/RtHFjvtJBEwOkhbN/BnnE8rnZR8+sbwnc/KhCk3FhnpHZnQz7B
5aETbbIgmuvewdjvSBSjYzBhMA4GA1UdDwEB/wQEAwICpDATBgNVHSUEDDAKBggr
BgEFBQcDATAPBgNVHRMBAf8EBTADAQH/MCkGA1UdEQQiMCCCDmxvY2FsaG9zdDo1
NDUzgg4xMjcuMC4wLjE6NTQ1MzAKBggqhkjOPQQDAgNIADBFAiEA2zpJEPQyz6/l
Wf86aX6PepsntZv2GYlA5UpabfT2EZICICpJ5h/iI+i341gBmLiAFQOyTDT+/wQc
6MF9+Yw1Yy0t
-----END CERTIFICATE-----`

// silence unused import if io is only used transitively
var _ = io.Discard
