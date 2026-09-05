package hub

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// mkSpark builds a synthetic series of n points at the real 15-minute sampling
// interval, with a value that varies per index so downsampling is observable.
func mkSpark(n int) []SparkPoint {
	now := time.Now().Unix()
	s := make([]SparkPoint, n)
	for i := range s {
		s[i] = SparkPoint{T: now - int64((n-i)*900), V: i}
	}
	return s
}

func TestDownsampleSpark(t *testing.T) {
	tests := []struct {
		name    string
		in      []SparkPoint
		max     int
		wantLen int
	}{
		{"nil series stays nil", nil, sparkWirePoints, 0},
		{"empty series stays empty", []SparkPoint{}, sparkWirePoints, 0},
		{"single point is untouched", mkSpark(1), sparkWirePoints, 1},
		{"under the cap is untouched", mkSpark(10), sparkWirePoints, 10},
		{"exactly at the cap is untouched", mkSpark(sparkWirePoints), sparkWirePoints, sparkWirePoints},
		{"one over the cap is reduced", mkSpark(sparkWirePoints + 1), sparkWirePoints, sparkWirePoints},
		{"full 7-day series is reduced", mkSpark(672), sparkWirePoints, sparkWirePoints},
		{"max of zero yields nothing", mkSpark(672), 0, 0},
		{"negative max yields nothing", mkSpark(672), -1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := downsampleSpark(tc.in, tc.max)
			if len(got) != tc.wantLen {
				t.Fatalf("len = %d, want %d", len(got), tc.wantLen)
			}
			if len(got) == 0 {
				return
			}
			// Endpoints must be preserved exactly: the sparkline's first and
			// last points anchor the trend, and the last value is the one
			// rendered as a number beside the chart.
			if got[0] != tc.in[0] {
				t.Errorf("first point = %+v, want %+v", got[0], tc.in[0])
			}
			last := tc.in[len(tc.in)-1]
			if got[len(got)-1] != last {
				t.Errorf("last point = %+v, want %+v", got[len(got)-1], last)
			}
			// Order must be preserved — a scrambled series would draw a
			// meaningless zig-zag.
			for i := 1; i < len(got); i++ {
				if got[i].T < got[i-1].T {
					t.Fatalf("series not monotonic at %d: %d < %d", i, got[i].T, got[i-1].T)
				}
			}
		})
	}
}

// The caller marshals the result while the registry may still be appending to
// the stored series, so a downsampled series must not alias its input.
func TestDownsampleSparkDoesNotAliasInput(t *testing.T) {
	in := mkSpark(672)
	got := downsampleSpark(in, sparkWirePoints)
	got[0].V = 999999
	if in[0].V == 999999 {
		t.Fatal("downsampleSpark returned a slice aliasing the caller's series")
	}
}

// Guards the reason this exists: sparkline history was 92% of the my-hives
// payload. If someone raises sparkWirePoints back toward sparkMaxPoints this
// fails loudly rather than silently restoring an 800 KB response.
func TestDownsampleSparkShrinksPayload(t *testing.T) {
	const fleet = 42
	full := mkSpark(672)
	var before, after int
	for i := 0; i < fleet; i++ {
		b, err := json.Marshal(map[string]any{"issueHistory": full, "prHistory": full})
		if err != nil {
			t.Fatalf("marshal full: %v", err)
		}
		before += len(b)
		a, err := json.Marshal(map[string]any{
			"issueHistory": downsampleSpark(full, sparkWirePoints),
			"prHistory":    downsampleSpark(full, sparkWirePoints),
		})
		if err != nil {
			t.Fatalf("marshal downsampled: %v", err)
		}
		after += len(a)
	}
	reduction := 100 * (1 - float64(after)/float64(before))
	t.Logf("history bytes across %d hives: before=%d after=%d (%.1f%% smaller)", fleet, before, after, reduction)
	if reduction < 80 {
		t.Errorf("history downsampling only saved %.1f%%, want >=80%% — sparkWirePoints (%d) may have crept up", reduction, sparkWirePoints)
	}
}

// sparkWirePoints must stay well under what the registry stores, otherwise the
// wire trim is a no-op.
func TestSparkWirePointsBelowStoredMax(t *testing.T) {
	const storedMax = 672 // sparkMaxPoints in handleHeartbeat
	if sparkWirePoints >= storedMax {
		t.Fatalf("sparkWirePoints = %d must be < stored max %d", sparkWirePoints, storedMax)
	}
}

// The eternal-spinner regression: renderHives() threw, but loadHives' catch
// only painted a message when _allDashHives was empty — and it is assigned
// before the render. These assert the surfaced-error path stays wired up.
func TestDashboardSurfacesRenderFailures(t *testing.T) {
	tests := []struct {
		name    string
		snippet string
	}{
		{"logs the real error", "console.error('[hive] my-hives load/render failed:', e);"},
		{"calls the error renderer", "renderHivesError(e);"},
		{"error renderer exists", "function renderHivesError(err) {"},
		{"offers a retry affordance", "btn.textContent = 'Retry';"},
		{"retry clears the render signature", "_lastHivesJSON = '';"},
		{"no longer gates on an empty list", "if (_allDashHives.length && !/Loading your hives/.test(container.innerHTML)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}

// Instant-paint cache: must be bounded, versioned, and never throw.
func TestDashboardHivesCacheIsBoundedAndVersioned(t *testing.T) {
	tests := []struct {
		name    string
		snippet string
	}{
		{"cache key", "var LS_HIVES_CACHE = 'hive-my-hives-cache';"},
		{"schema version", "var HIVES_CACHE_VERSION = 6;"},
		{"named TTL constant", "var HIVES_CACHE_TTL_MS = 10 * 60 * 1000;"},
		{"named row cap", "var HIVES_CACHE_MAX_ROWS = 200;"},
		{"version is enforced on read", "if (!c || c.version !== HIVES_CACHE_VERSION) return null;"},
		{"TTL is enforced on read", "if (!c.savedAt || (Date.now() - c.savedAt) > HIVES_CACHE_TTL_MS) return null;"},
		{"reads cannot throw", "console.warn('[hive] hive cache read failed, using network only:', e);"},
		{"writes cannot throw", "console.warn('[hive] hive cache write failed:', e);"},
		{"cached render is guarded", "console.error('[hive] cached render failed, falling back to network:', e);"},
		{"painted before the network resolves", "paintCachedHives();"},
		{"only successful renders are cached", "writeHivesCache(data.hives || []);"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(dashboardHTML, tc.snippet) {
				t.Errorf("dashboardHTML is missing %q", tc.snippet)
			}
		})
	}
}
