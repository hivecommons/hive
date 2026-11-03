package github

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The global OTel providers delegate to the first real provider installed,
// so the test binary installs exactly one in-memory pipeline and every test
// reads from it.
var (
	otelTestOnce   sync.Once
	otelTestSpans  *tracetest.InMemoryExporter
	otelTestReader *sdkmetric.ManualReader
)

func otelTestPipeline(t *testing.T) (*tracetest.InMemoryExporter, *sdkmetric.ManualReader) {
	t.Helper()
	otelTestOnce.Do(func() {
		otelTestSpans = tracetest.NewInMemoryExporter()
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(otelTestSpans)))
		otelTestReader = sdkmetric.NewManualReader()
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(otelTestReader)))
	})
	otelTestSpans.Reset()
	return otelTestSpans, otelTestReader
}

func spanAttr(t *testing.T, s tracetest.SpanStub, key string) attribute.Value {
	t.Helper()
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value
		}
	}
	t.Fatalf("span %q has no attribute %q (have %v)", s.Name, key, s.Attributes)
	return attribute.Value{}
}

func collectMetrics(t *testing.T, reader *sdkmetric.ManualReader) map[string]metricdata.Metrics {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	out := map[string]metricdata.Metrics{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m
		}
	}
	return out
}

func sumInt64(t *testing.T, m metricdata.Metrics, want ...attribute.KeyValue) int64 {
	t.Helper()
	sum, ok := m.Data.(metricdata.Sum[int64])
	if !ok {
		t.Fatalf("%s: data = %T, want Sum[int64]", m.Name, m.Data)
	}
	var total int64
	for _, dp := range sum.DataPoints {
		match := true
		for _, kv := range want {
			if v, ok := dp.Attributes.Value(kv.Key); !ok || v != kv.Value {
				match = false
				break
			}
		}
		if match {
			total += dp.Value
		}
	}
	return total
}

func TestOTelTransport_SpanAttributesAndMetrics(t *testing.T) {
	spans, reader := otelTestPipeline(t)
	ResetRateLimitBudgetForTest()
	ResetRateLimitPacingForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4321")
		w.Header().Set("X-RateLimit-Resource", "core")
		w.Header().Set("ETag", `"abc"`)
		if r.Header.Get("If-None-Match") == `"abc"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":42}`))
	}))
	defer srv.Close()

	chain := githubTransportChain(http.DefaultTransport)
	client := &http.Client{Transport: chain}
	ctx := WithRESTCaller(context.Background(), "governor")
	url := srv.URL + "/repos/hivecommons/hive/issues/42"
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("request %d: %v", i, err)
		}
		_ = resp.Body.Close()
	}

	got := spans.GetSpans()
	if len(got) != 2 {
		t.Fatalf("spans = %d, want 2", len(got))
	}
	first, second := got[0], got[1]
	if first.Name != SpanGitHubRequest {
		t.Fatalf("span name = %q, want %q", first.Name, SpanGitHubRequest)
	}
	if v := spanAttr(t, first, AttrGitHubEndpoint).AsString(); v != "/repos/{owner}/{repo}/issues/{number}" {
		t.Errorf("github.endpoint = %q", v)
	}
	if v := spanAttr(t, first, AttrGitHubRepo).AsString(); v != "hivecommons/hive" {
		t.Errorf("github.repo = %q", v)
	}
	if v := spanAttr(t, first, AttrHiveCaller).AsString(); v != "governor" {
		t.Errorf("hive.caller = %q", v)
	}
	if v := spanAttr(t, first, AttrGitHubRateLimitRemaining).AsInt64(); v != 4321 {
		t.Errorf("github.ratelimit.remaining = %d", v)
	}
	if v := spanAttr(t, first, AttrGitHubRateLimitBucket).AsString(); v != "core" {
		t.Errorf("github.ratelimit.bucket = %q", v)
	}
	if spanAttr(t, first, AttrGitHubCacheHit).AsBool() {
		t.Error("first request should not be a cache hit")
	}
	if !spanAttr(t, second, AttrGitHubCacheHit).AsBool() {
		t.Error("second request (304 replay) should be a cache hit")
	}
	if spanAttr(t, second, AttrGitHubShed).AsBool() {
		t.Error("github.shed should be false for a served request")
	}
	if v := spanAttr(t, first, "hive.component").AsString(); v != "github" {
		t.Errorf("hive.component = %q", v)
	}

	metrics := collectMetrics(t, reader)
	reqs, ok := metrics[MetricGitHubRequests]
	if !ok {
		t.Fatalf("metric %s not recorded; have %v", MetricGitHubRequests, metricNames(metrics))
	}
	if n := sumInt64(t, reqs,
		attribute.String("caller", "governor"),
		attribute.String("endpoint", "/repos/{owner}/{repo}/issues/{number}"),
		attribute.String("status", "200")); n < 2 {
		t.Errorf("github.requests{governor,...,200} = %d, want >= 2", n)
	}
	rem, ok := metrics[MetricGitHubRateLimitRemaining]
	if !ok {
		t.Fatalf("metric %s not recorded", MetricGitHubRateLimitRemaining)
	}
	g, ok := rem.Data.(metricdata.Gauge[int64])
	if !ok {
		t.Fatalf("%s data = %T, want Gauge[int64]", rem.Name, rem.Data)
	}
	found := false
	for _, dp := range g.DataPoints {
		if v, _ := dp.Attributes.Value("bucket"); v.AsString() == "core" && dp.Value == 4321 {
			found = true
		}
	}
	if !found {
		t.Errorf("github.ratelimit.remaining{bucket=core} = 4321 not found in %+v", g.DataPoints)
	}
	if r, l, ok := RateLimitBudget("core"); !ok || r != 4321 || l != 5000 {
		t.Errorf("RateLimitBudget(core) = %d/%d ok=%v", r, l, ok)
	}
}

func TestOTelTransport_SecondaryLimitHit(t *testing.T) {
	spans, reader := otelTestPipeline(t)
	ResetRateLimitPacingForTest()
	defer ResetRateLimitPacingForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4000")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
	}))
	defer srv.Close()

	before := collectMetrics(t, reader)
	var hitsBefore int64
	if m, ok := before[MetricGitHubSecondaryLimitHits]; ok {
		hitsBefore = sumInt64(t, m)
	}

	client := &http.Client{Transport: githubTransportChain(http.DefaultTransport)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/repos/o/r/pulls", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans = %d, want 1", len(got))
	}
	if got[0].Status.Code != codes.Error {
		t.Errorf("span status = %v, want Error on a secondary-limit 403", got[0].Status.Code)
	}
	if v := spanAttr(t, got[0], AttrGitHubStatus).AsInt64(); v != http.StatusForbidden {
		t.Errorf("status attr = %d", v)
	}
	after := collectMetrics(t, reader)
	m, ok := after[MetricGitHubSecondaryLimitHits]
	if !ok {
		t.Fatalf("metric %s not recorded", MetricGitHubSecondaryLimitHits)
	}
	if n := sumInt64(t, m); n != hitsBefore+1 {
		t.Errorf("secondary_limit_hits = %d, want %d", n, hitsBefore+1)
	}
	if n := sumInt64(t, after[MetricGitHubRequests], attribute.String("status", strconv.Itoa(http.StatusForbidden))); n < 1 {
		t.Errorf("github.requests{status=403} = %d, want >= 1", n)
	}
}

func TestOTelTransport_UnknownCallerAndUnauthenticatedBucketIgnored(t *testing.T) {
	spans, _ := otelTestPipeline(t)
	ResetRateLimitBudgetForTest()
	ResetRateLimitPacingForTest()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "60")
		w.Header().Set("X-RateLimit-Remaining", "12")
		w.Header().Set("X-RateLimit-Resource", "search")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	client := &http.Client{Transport: githubTransportChain(http.DefaultTransport)}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/search/issues?q=repo:o/r+is:open", nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	_ = resp.Body.Close()

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans = %d, want 1", len(got))
	}
	if v := spanAttr(t, got[0], AttrHiveCaller).AsString(); v != unknownCaller {
		t.Errorf("hive.caller = %q, want %q", v, unknownCaller)
	}
	if v := spanAttr(t, got[0], AttrGitHubRepo).AsString(); v != "o/r" {
		t.Errorf("github.repo from search q = %q", v)
	}
	if v := spanAttr(t, got[0], AttrGitHubRateLimitBucket).AsString(); v != "search" {
		t.Errorf("bucket = %q", v)
	}
	if _, _, ok := RateLimitBudget("search"); ok {
		t.Error("a per-IP unauthenticated bucket (limit 60) must not drive the budget")
	}
}

func TestRepoFromRequest(t *testing.T) {
	cases := map[string]string{
		"https://api.github.com/repos/a/b/issues/1":       "a/b",
		"https://api.github.com/search/issues?q=repo:c/d": "c/d",
		"https://api.github.com/rate_limit":               "",
		"https://api.github.com/graphql":                  "",
	}
	for raw, want := range cases {
		req, _ := http.NewRequest(http.MethodGet, raw, nil)
		if got := repoFromRequest(req.URL); got != want {
			t.Errorf("repoFromRequest(%s) = %q, want %q", raw, got, want)
		}
	}
}

func metricNames(m map[string]metricdata.Metrics) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

type shedTransport struct{}

func (shedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, pacingShedError(PacingLow, 2*time.Second, time.Second)
}

// A pacer refusal (ErrPacingShed from #13382) is tagged github.shed=true and
// counted under github.requests{status="shed"} rather than "error".
func TestOTelTransport_PacerShedTagged(t *testing.T) {
	spans, reader := otelTestPipeline(t)
	before := collectMetrics(t, reader)
	var shedBefore int64
	if m, ok := before[MetricGitHubRequests]; ok {
		shedBefore = sumInt64(t, m, attribute.String("status", "shed"))
	}

	ctx := WithPacingClass(WithRESTCaller(context.Background(), "automerge"), PacingLow)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/o/r/pulls/7", nil)
	_, err := otelWrap(shedTransport{}).RoundTrip(req)
	if !errors.Is(err, ErrPacingShed) {
		t.Fatalf("err = %v, want ErrPacingShed", err)
	}

	got := spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("spans = %d, want 1", len(got))
	}
	if !spanAttr(t, got[0], AttrGitHubShed).AsBool() {
		t.Error("github.shed = false, want true")
	}
	if v := spanAttr(t, got[0], AttrHivePacingClass).AsString(); v != "low" {
		t.Errorf("hive.pacing_class = %q, want low", v)
	}
	if v := spanAttr(t, got[0], AttrHiveCaller).AsString(); v != "automerge" {
		t.Errorf("hive.caller = %q, want automerge", v)
	}
	after := collectMetrics(t, reader)
	if n := sumInt64(t, after[MetricGitHubRequests], attribute.String("status", "shed")); n != shedBefore+1 {
		t.Errorf("github.requests{status=shed} = %d, want %d", n, shedBefore+1)
	}
}
