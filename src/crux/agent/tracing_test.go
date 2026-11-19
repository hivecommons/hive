package agent

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hivecommons/hive/crux/config"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSetSpanStarterRoutesRuntimeSpans(t *testing.T) {
	m := NewManager(nil, slog.Default(), ProjectContext{})

	var gotName string
	var gotAttrs []attribute.KeyValue
	m.SetSpanStarter(func(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
		gotName, gotAttrs = name, attrs
		return noop.NewTracerProvider().Tracer("test").Start(ctx, name)
	})
	if err := m.SendKick("missing", "hello"); err == nil {
		t.Fatal("SendKick on an unknown agent must fail")
	}
	if gotName != "agent.send_kick" {
		t.Fatalf("span name = %q, want agent.send_kick", gotName)
	}
	if len(gotAttrs) != 0 {
		t.Fatalf("span attrs = %v, want none before the kick target resolves", gotAttrs)
	}

	m.SetSpanStarter(nil)
	gotName = ""
	if err := m.SendKick("missing", "hello"); err == nil {
		t.Fatal("SendKick on an unknown agent must fail")
	}
	if gotName != "" {
		t.Fatal("cleared SpanStarter must not be called")
	}
	if _, span := m.startSpan(context.Background(), "agent.test"); span == nil {
		t.Fatal("default span starter returned a nil span")
	}
}

func TestSendKickSpanAgentNameOnlyAfterResolve(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	m := NewManager(map[string]config.AgentConfig{
		"known": {Backend: "claude"},
	}, slog.Default(), ProjectContext{})
	m.SetSpanStarter(func(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
		return tp.Tracer("test").Start(ctx, name, trace.WithAttributes(attrs...))
	})

	_ = m.SendKick("caller-garbage-name", "hello")
	_ = m.SendKick("known", "hello")

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want 2", len(spans))
	}
	for _, kv := range spans[0].Attributes() {
		if kv.Key == "agent.name" {
			t.Fatalf("unknown-name span carries agent.name=%q", kv.Value.AsString())
		}
	}
	var got string
	for _, kv := range spans[1].Attributes() {
		if kv.Key == "agent.name" {
			got = kv.Value.AsString()
		}
	}
	if got != "known" {
		t.Fatalf("known-agent span agent.name = %q, want known", got)
	}
}
