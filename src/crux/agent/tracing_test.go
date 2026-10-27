package agent

import (
	"context"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/attribute"
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
	if len(gotAttrs) != 1 || gotAttrs[0].Key != "agent.name" || gotAttrs[0].Value.AsString() != "missing" {
		t.Fatalf("span attrs = %v, want agent.name=missing", gotAttrs)
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
