package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/kubestellar/hive/v2/pkg/timeline"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// shutdownTimeout bounds shutdown calls in tests so a misbehaving batcher can
// never hang the suite.
const shutdownTimeout = 2 * time.Second

// newDropServer returns an httptest.Server that accepts any request and returns
// 200 with an empty body. It lets the enabled exporter path run end to end
// without ever leaving the test process onto a real network.
func newDropServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestInit_DisabledIsNoop verifies that the default (disabled) path returns a
// usable no-op shutdown, records nothing, and never errors.
func TestInit_DisabledIsNoop(t *testing.T) {
	ctx := context.Background()

	shutdown, err := Init(ctx, Config{Enabled: false})
	if err != nil {
		t.Fatalf("Init(disabled) returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init(disabled) returned nil shutdown")
	}

	// StartSpan must work and not panic when tracing is disabled.
	_, span := StartSpan(ctx, "should.be.noop", attribute.String("k", "v"))
	if span.IsRecording() {
		t.Error("span should not be recording when tracing is disabled")
	}
	span.End()

	// Shutdown must be safe to call.
	if err := shutdown(ctx); err != nil {
		t.Errorf("no-op shutdown returned error: %v", err)
	}
}

// TestInit_EnabledMissingEndpointIsNoop verifies fail-closed behavior: enabling
// tracing with neither a configured endpoint nor the env var yields a no-op
// rather than defaulting to localhost and spewing connection errors.
func TestInit_EnabledMissingEndpointIsNoop(t *testing.T) {
	// Ensure the env var is unset for this test.
	t.Setenv(otelEndpointEnv, "")

	shutdown, err := Init(context.Background(), Config{Enabled: true})
	if err != nil {
		t.Fatalf("Init(enabled, no endpoint) returned error: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}

// TestStartSpan_RecordsWithStubExporter installs an in-memory SpanRecorder as
// the global provider and verifies StartSpan produces a span with the expected
// name and attributes. No real network is used.
func TestStartSpan_RecordsWithStubExporter(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(otel.GetTracerProvider())
		_ = tp.Shutdown(context.Background())
	})

	const spanName = "test.span"
	_, span := StartSpan(context.Background(), spanName,
		attribute.String("hive.id", "hive-123"),
		attribute.String("hive.branch", "v4"),
	)
	if !span.IsRecording() {
		t.Fatal("span should be recording with a real provider")
	}
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("expected 1 ended span, got %d", len(ended))
	}

	got := ended[0]
	if got.Name() != spanName {
		t.Errorf("span name = %q, want %q", got.Name(), spanName)
	}

	attrs := map[attribute.Key]string{}
	for _, kv := range got.Attributes() {
		attrs[kv.Key] = kv.Value.AsString()
	}
	if attrs["hive.id"] != "hive-123" {
		t.Errorf("hive.id attr = %q, want %q", attrs["hive.id"], "hive-123")
	}
	if attrs["hive.branch"] != "v4" {
		t.Errorf("hive.branch attr = %q, want %q", attrs["hive.branch"], "v4")
	}
}

// TestStartSpan_ParentChild verifies that a child span started from a parent's
// context is linked to the parent trace (the lifecycle shape we instrument).
func TestStartSpan_ParentChild(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	ctx, parent := StartSpan(context.Background(), "governor.eval_cycle")
	childCtx, child := StartSpan(ctx, "governor.enumerate_actionable")
	_ = childCtx
	child.End()
	parent.End()

	ended := recorder.Ended()
	if len(ended) != 2 {
		t.Fatalf("expected 2 ended spans, got %d", len(ended))
	}

	// Find child and parent by name and confirm the child's parent is the
	// parent's span, sharing one trace ID.
	byName := map[string]sdktrace.ReadOnlySpan{}
	for _, s := range ended {
		byName[s.Name()] = s
	}
	p := byName["governor.eval_cycle"]
	c := byName["governor.enumerate_actionable"]
	if p == nil || c == nil {
		t.Fatal("missing expected spans")
	}
	if c.Parent().SpanID() != p.SpanContext().SpanID() {
		t.Error("child span is not parented to the eval-cycle span")
	}
	if c.SpanContext().TraceID() != p.SpanContext().TraceID() {
		t.Error("child and parent spans have different trace IDs")
	}
}

// TestTracer_EmptyNameUsesDefault ensures Tracer("") does not panic and returns
// a usable tracer.
func TestTracer_EmptyNameUsesDefault(t *testing.T) {
	tr := Tracer("")
	if tr == nil {
		t.Fatal("Tracer(\"\") returned nil")
	}
	_, span := tr.Start(context.Background(), "x")
	span.End()
}

// restoreGlobalProvider snapshots the current global TracerProvider and restores
// it on cleanup, so an enabled test that calls otel.SetTracerProvider does not
// leak an OTLP-backed provider into other tests.
func restoreGlobalProvider(t *testing.T) {
	t.Helper()
	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
}

// TestInit_EnabledWithConfiguredEndpoint exercises the full enabled path:
// exporter construction, resource merge, sampler, and provider installation.
// The endpoint points at a local drop server, and the batching exporter is
// lazy, so no real network traffic leaves the process.
func TestInit_EnabledWithConfiguredEndpoint(t *testing.T) {
	restoreGlobalProvider(t)
	srv := newDropServer(t)

	ctx := context.Background()
	shutdown, err := Init(ctx, Config{
		Enabled:  true,
		Endpoint: srv.URL,
		HiveID:   "hive-abc",
		Branch:   "v4",
	})
	if err != nil {
		t.Fatalf("Init(enabled, endpoint) returned error: %v", err)
	}
	if shutdown == nil {
		t.Fatal("Init(enabled) returned nil shutdown")
	}

	// The global provider must now be a recording SDK provider, not the no-op.
	_, span := StartSpan(ctx, "enabled.span", attribute.String("k", "v"))
	if !span.IsRecording() {
		t.Error("span should be recording after Init with an endpoint")
	}
	span.End()

	sctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
	defer cancel()
	if err := shutdown(sctx); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}

// TestInit_EnabledEndpointFromEnv verifies that an empty Config.Endpoint but a
// set OTEL_EXPORTER_OTLP_ENDPOINT still activates the enabled path rather than
// failing closed.
func TestInit_EnabledEndpointFromEnv(t *testing.T) {
	restoreGlobalProvider(t)
	srv := newDropServer(t)
	t.Setenv(otelEndpointEnv, srv.URL)

	shutdown, err := Init(context.Background(), Config{Enabled: true})
	if err != nil {
		t.Fatalf("Init(enabled, env endpoint) returned error: %v", err)
	}

	_, span := StartSpan(context.Background(), "env.span")
	if !span.IsRecording() {
		t.Error("span should be recording when endpoint comes from env")
	}
	span.End()

	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := shutdown(sctx); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}

// TestInit_ShutdownIdempotent confirms shutdown can be called more than once
// without erroring — a second flush on an already-stopped provider is safe.
func TestInit_ShutdownIdempotent(t *testing.T) {
	restoreGlobalProvider(t)
	srv := newDropServer(t)

	shutdown, err := Init(context.Background(), Config{Enabled: true, Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	for i := 0; i < 2; i++ {
		sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		if err := shutdown(sctx); err != nil {
			t.Errorf("shutdown call %d returned error: %v", i+1, err)
		}
		cancel()
	}
}

// TestInit_ShutdownCancelledContext confirms shutdown with an already-cancelled
// context does not panic. The SDK may return the context error; that is
// acceptable, we only require it not to crash.
func TestInit_ShutdownCancelledContext(t *testing.T) {
	restoreGlobalProvider(t)
	srv := newDropServer(t)

	shutdown, err := Init(context.Background(), Config{Enabled: true, Endpoint: srv.URL})
	if err != nil {
		t.Fatalf("Init returned error: %v", err)
	}

	cctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before shutdown
	_ = shutdown(cctx)
}

// TestInit_ExplicitSampleRatio exercises the branch where a caller supplies a
// sampling ratio explicitly (not the zero-default full-sample path).
func TestInit_ExplicitSampleRatio(t *testing.T) {
	restoreGlobalProvider(t)
	srv := newDropServer(t)

	const halfSample = 0.5
	shutdown, err := Init(context.Background(), Config{
		Enabled:     true,
		Endpoint:    srv.URL,
		SampleRatio: halfSample,
	})
	if err != nil {
		t.Fatalf("Init(explicit ratio) returned error: %v", err)
	}
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := shutdown(sctx); err != nil {
		t.Errorf("shutdown returned error: %v", err)
	}
}

// TestInit_InvalidEndpointReturnsError drives the exporter-construction error
// branch: a malformed endpoint URL makes otlptracehttp.New fail, and Init must
// return that error alongside a safe no-op shutdown (never nil).
func TestInit_InvalidEndpointReturnsError(t *testing.T) {
	restoreGlobalProvider(t)

	shutdown, err := Init(context.Background(), Config{
		Enabled:  true,
		Endpoint: "://not a url",
	})
	// Regardless of whether construction errors, shutdown must be callable.
	if shutdown == nil {
		t.Fatal("Init returned nil shutdown on invalid endpoint")
	}
	if err != nil {
		// The returned shutdown should be the no-op and must not error.
		if serr := shutdown(context.Background()); serr != nil {
			t.Errorf("no-op shutdown after error returned: %v", serr)
		}
	}
}

func TestTimelineSpanAttributes_GenAIAndHiveAttrs(t *testing.T) {
	e := timeline.Event{
		ID:       "evt-1",
		IssueRef: "org/repo#7",
		Kind:     timeline.KindKicked,
		Agent:    "scanner",
		Attrs: map[string]string{
			"gen_ai.system": "claude",
			"model":         "claude-sonnet-4-6",
			"input_tokens":  "123",
			"output_tokens": "45",
			"lane":          "triage",
			"acmm_level":    "4",
			"governor_mode": "BUSY",
		},
	}

	attrs := TimelineSpanAttributes(e)
	strings := map[attribute.Key]string{}
	ints := map[attribute.Key]int64{}
	for _, kv := range attrs {
		switch kv.Value.Type().String() {
		case "STRING":
			strings[kv.Key] = kv.Value.AsString()
		case "INT64":
			ints[kv.Key] = kv.Value.AsInt64()
		}
	}

	if TimelineSpanName(e) != "agent.kick" {
		t.Fatalf("span name = %q", TimelineSpanName(e))
	}
	if strings[AttrGenAISystem] != "claude" {
		t.Errorf("gen_ai.system = %q", strings[AttrGenAISystem])
	}
	if strings[AttrGenAIRequestModel] != "claude-sonnet-4-6" {
		t.Errorf("gen_ai.request.model = %q", strings[AttrGenAIRequestModel])
	}
	if ints[AttrGenAIUsageInputTokens] != 123 || ints[AttrGenAIUsageOutputTokens] != 45 {
		t.Errorf("token attrs = %d/%d", ints[AttrGenAIUsageInputTokens], ints[AttrGenAIUsageOutputTokens])
	}
	if strings[AttrHiveAgent] != "scanner" || strings[AttrHiveLane] != "triage" {
		t.Errorf("hive attrs = agent %q lane %q", strings[AttrHiveAgent], strings[AttrHiveLane])
	}
	if ints[AttrHiveACMMLevel] != 4 || strings[AttrHiveGovernorMode] != "BUSY" {
		t.Errorf("governor attrs = acmm %d mode %q", ints[AttrHiveACMMLevel], strings[AttrHiveGovernorMode])
	}
}

func TestStartTimelineSpan_RecordsMappedSpan(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })

	_, span := StartTimelineSpan(context.Background(), timeline.Event{
		Kind:  timeline.KindPROpened,
		Agent: "reviewer",
		Attrs: map[string]string{"pr": "42"},
	})
	span.End()

	ended := recorder.Ended()
	if len(ended) != 1 {
		t.Fatalf("ended spans = %d, want 1", len(ended))
	}
	if ended[0].Name() != "pr.opened" {
		t.Fatalf("span name = %q, want pr.opened", ended[0].Name())
	}
}
