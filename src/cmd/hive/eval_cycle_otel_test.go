package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// installTestTracer routes spans into an in-memory exporter for the test.
func installTestTracer(t *testing.T) *tracetest.InMemoryExporter {
	t.Helper()
	exp := tracetest.NewInMemoryExporter()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSyncer(exp)))
	t.Cleanup(func() { otel.SetTracerProvider(tracenoop.NewTracerProvider()) })
	return exp
}

// The automerge sweep opens a governor.automerge span whose GitHub calls
// hang off it as github.request children (hivecommons/hive#13380).
func TestRunAutoMergeSweepIfDue_EmitsGovernorAutomergeSpan(t *testing.T) {
	exp := installTestTracer(t)
	var requests atomic.Int64
	srv := newSweepAPI(t, `[]`, http.StatusOK, &requests)
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget"}, sweepTestLogger(nil))

	var lastRun time.Time
	runAutoMergeSweepIfDue(context.Background(), ghClient, nil, nil, &lastRun, sweepTestLogger(nil))

	var sweep *tracetest.SpanStub
	var requestChildren int
	spans := exp.GetSpans()
	for i := range spans {
		if spans[i].Name == "governor.automerge" {
			sweep = &spans[i]
		}
	}
	if sweep == nil {
		t.Fatalf("no governor.automerge span among %d spans", len(spans))
	}
	for _, s := range spans {
		if s.Name == github.SpanGitHubRequest && s.Parent.SpanID() == sweep.SpanContext.SpanID() {
			requestChildren++
		}
	}
	if requests.Load() > 0 && requestChildren == 0 {
		t.Fatalf("sweep made %d API calls but no github.request span is a child of governor.automerge", requests.Load())
	}
	for _, kv := range sweep.Attributes {
		if string(kv.Key) == "hive.automerge.seen" && kv.Value.AsInt64() != 0 {
			t.Fatalf("hive.automerge.seen = %d, want 0 for an empty queue", kv.Value.AsInt64())
		}
	}
}
