package knowledge

import (
	"fmt"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

func TestCleanBeadSynthPublishPathCov(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    string
		wantErr bool
	}{
		{"empty", "", "", false},
		{"trimmed and cleaned", " docs/./kb/ ", "docs/kb", false},
		{"absolute", "/docs", "", true},
		{"backslash", `docs\kb`, "", true},
		{"colon", "c:docs", "", true},
		{"parent component", "docs/../kb", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := cleanBeadSynthPublishPath(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBeadSynthPublishTextHelpersCov(t *testing.T) {
	paths := sortedBeadSynthPaths(map[string]string{"b.md": "1", "a.md": "2"})
	if len(paths) != 2 || paths[0] != "a.md" || paths[1] != "b.md" {
		t.Fatalf("unexpected sorted paths: %v", paths)
	}
	if got := beadSynthFileList(paths); got != "- a.md\n- b.md\n" {
		t.Fatalf("unexpected file list: %q", got)
	}
	if got := beadSynthFileList(nil); got != "" {
		t.Fatalf("empty list should render empty, got %q", got)
	}
	body := beadSynthPRBody(paths)
	want := beadSynthFileList(paths)
	if len(body) <= len(want) || body[len(body)-len(want):] != want {
		t.Fatalf("PR body should end with file list: %q", body)
	}
}

func TestBeadSynthMissTTLCov(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset", "", prEnricherDefaultMissTTL},
		{"duration", "90m", 90 * time.Minute},
		{"hours integer", "3", 3 * time.Hour},
		{"negative duration", "-5m", prEnricherDefaultMissTTL},
		{"negative integer", "-3", prEnricherDefaultMissTTL},
		{"garbage", "soon", prEnricherDefaultMissTTL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(prEnricherMissTTLEnv, tt.env)
			if got := beadSynthMissTTL(); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBeadSynthMaxAPICallsEnvFallbackCov(t *testing.T) {
	tests := []struct {
		name       string
		env        string
		configured int
		want       int
	}{
		{"garbage uses configured", "abc", 9, 9},
		{"negative uses default", "-1", 0, prEnricherDefaultMaxAPICalls},
		{"zero env is unlimited", "0", 9, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(prEnricherMaxAPICallsEnv, tt.env)
			if got := beadSynthMaxAPICalls(tt.configured); got != tt.want {
				t.Fatalf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestShouldSkipSynthesisCandidateCov(t *testing.T) {
	t.Setenv(prEnricherMissTTLEnv, "24h")
	s := &BeadSynthesizer{}
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	old := time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)
	tests := []struct {
		name string
		bead *beads.Bead
		want bool
	}{
		{"nil bead", nil, true},
		{"no marker", &beads.Bead{}, false},
		{"blank marker", &beads.Bead{Metadata: map[string]interface{}{beadSynthMetaKeySkippedAt: "  "}}, false},
		{"unparseable marker", &beads.Bead{Metadata: map[string]interface{}{beadSynthMetaKeySkippedAt: "yesterday"}}, false},
		{"recent marker", &beads.Bead{Metadata: map[string]interface{}{beadSynthMetaKeySkippedAt: recent}}, true},
		{"expired marker", &beads.Bead{Metadata: map[string]interface{}{beadSynthMetaKeySkippedAt: old}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := s.shouldSkipSynthesisCandidate(tt.bead); got != tt.want {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMarkSynthesisSkippedNoStoreCov(t *testing.T) {
	s := &BeadSynthesizer{beadStores: map[string]*beads.Store{}}
	s.markSynthesisSkipped(beadCandidate{agent: "missing", bead: &beads.Bead{ID: "b-1"}})
}

func TestPREnricherMissCacheCov(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	newEnricher := func(ttl time.Duration) *PREnricher {
		return &PREnricher{misses: make(map[string]time.Time), missTTL: ttl, now: func() time.Time { return now }}
	}

	t.Run("zero ttl disables caching", func(t *testing.T) {
		e := newEnricher(0)
		e.cacheMissLocked("k", now)
		if len(e.misses) != 0 {
			t.Fatalf("expected no misses cached, got %d", len(e.misses))
		}
	})

	t.Run("cached then expired", func(t *testing.T) {
		e := newEnricher(time.Hour)
		if e.isMissCachedLocked("k", now) {
			t.Fatal("unexpected miss before caching")
		}
		e.cacheMissLocked("k", now)
		if !e.isMissCachedLocked("k", now.Add(30*time.Minute)) {
			t.Fatal("expected miss to be cached within ttl")
		}
		if e.isMissCachedLocked("k", now.Add(2*time.Hour)) {
			t.Fatal("expected miss to expire")
		}
		if _, ok := e.misses["k"]; ok {
			t.Fatal("expired miss should be deleted")
		}
	})

	t.Run("prune drops only expired", func(t *testing.T) {
		e := newEnricher(time.Hour)
		e.misses["old"] = now.Add(-time.Minute)
		e.misses["live"] = now.Add(time.Minute)
		e.pruneExpiredMissesLocked(now)
		if _, ok := e.misses["old"]; ok {
			t.Fatal("expired entry not pruned")
		}
		if _, ok := e.misses["live"]; !ok {
			t.Fatal("live entry pruned")
		}
	})

	t.Run("eviction bounds cache and drops oldest", func(t *testing.T) {
		e := newEnricher(time.Hour)
		for i := 0; i <= prEnricherMissCacheSize; i++ {
			e.misses[fmt.Sprintf("k%d", i)] = now.Add(time.Duration(i+1) * time.Minute)
		}
		e.evictOldestMissesLocked()
		if len(e.misses) != prEnricherMissCacheSize {
			t.Fatalf("got %d entries, want %d", len(e.misses), prEnricherMissCacheSize)
		}
		if _, ok := e.misses["k0"]; ok {
			t.Fatal("oldest entry should have been evicted")
		}
	})
}

func TestPREnricherReserveAPICallCov(t *testing.T) {
	e := &PREnricher{maxAPICalls: prEnricherUnlimitedAPICalls}
	for i := 0; i < 3; i++ {
		if !e.reserveAPICallLocked() {
			t.Fatal("unlimited enricher should always reserve")
		}
	}
	e = &PREnricher{maxAPICalls: 2}
	for i := 0; i < 2; i++ {
		if !e.reserveAPICallLocked() {
			t.Fatalf("call %d should be reserved within the cap", i+1)
		}
	}
	if e.reserveAPICallLocked() {
		t.Fatal("third call should exceed the cap")
	}
	e.BeginCycle()
	if e.apiCalls != 0 || !e.reserveAPICallLocked() {
		t.Fatal("BeginCycle should reset the budget")
	}
}
