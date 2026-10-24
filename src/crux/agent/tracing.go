package agent

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// spanInstrumentationName is the instrumentation scope the runtime's spans
// carry when no SpanStarter is installed.
const spanInstrumentationName = "github.com/hivecommons/hive/crux/agent"

// SpanStarter opens a trace span. It has the shape of pkg/tracing.StartSpan
// so the host can install that function and keep its per-component reach
// bookkeeping, while the runtime itself depends only on the OpenTelemetry
// API (hivecommons/hive#13536).
type SpanStarter func(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span)

// SetSpanStarter installs the function that opens the runtime's trace spans.
// Safe to call at any time; nil restores the default, which starts spans on
// the OpenTelemetry global tracer (a no-op with zero export cost until a
// provider is registered). Never takes m.mu.
func (m *Manager) SetSpanStarter(fn SpanStarter) {
	if fn == nil {
		m.spanStarter.Store(nil)
		return
	}
	m.spanStarter.Store(&fn)
}

// startSpan opens a span through the installed SpanStarter or the global
// tracer.
func (m *Manager) startSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if fn := m.spanStarter.Load(); fn != nil && *fn != nil {
		return (*fn)(ctx, name, attrs...)
	}
	return otel.Tracer(spanInstrumentationName).Start(ctx, name, trace.WithAttributes(attrs...))
}
