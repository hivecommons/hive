package cruxhost

import (
	"context"
	"testing"

	"github.com/hivecommons/hive/crux/api/callback"
)

// splitSink records the cached split the way *tokens.InferenceSink does.
type splitSink struct {
	fakeDeps
	in, cacheRead int64
}

func (s *splitSink) RecordWithCache(agent, model string, in, out, cacheRead, cacheCreation int64) {
	s.in, s.cacheRead = in, cacheRead
}

// gateway_cache records reach the sink's cached split; a sink without one
// gets the whole prompt (input + cached) as input rather than losing tokens.
func TestRecordUsageGatewayCache(t *testing.T) {
	ctx := context.Background()
	rec := callback.UsageRecord{Agent: "a", Model: "m", Kind: callback.UsageKindGatewayCache, InputTokens: 200, OutputTokens: 5, CacheRead: 800}

	split := &splitSink{}
	if err := New(Deps{Usage: split}).RecordUsage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if split.in != 200 || split.cacheRead != 800 {
		t.Fatalf("split = in %d cache %d", split.in, split.cacheRead)
	}

	plain := &fakeDeps{}
	if err := New(Deps{Usage: plain}).RecordUsage(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if len(plain.usage) != 1 || plain.usage[0] != "gateway" {
		t.Fatalf("fallback usage = %v", plain.usage)
	}
}
