package scheduler

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// TestPrimeKnowledge_ByteCapIsVisibleInKickAndLog proves the #12124 contract
// end to end: with knowledge.primer.max_bytes set below what the wiki returns,
// the kick text is bounded, whole facts are dropped (never clipped), the
// render ends with the "omitted" marker, and the scheduler logs the drop.
func TestPrimeKnowledge_ByteCapIsVisibleInKickAndLog(t *testing.T) {
	const factBody = "Always use (arr || []).join() because undefined.join throws at runtime. "
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var results []map[string]interface{}
		for i := 0; i < 20; i++ {
			results = append(results, map[string]interface{}{
				"slug": "guard-join-" + string(rune('a'+i)), "title": "Guard .join() variant " + string(rune('A'+i)),
				"score": 0.95 - float64(i)*0.01, "type": "gotcha", "confidence": 0.95 - float64(i)*0.01,
				"tags": []string{"typescript"}, "snippet": strings.Repeat(factBody, 4),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"total": len(results), "results": results})
	}))
	defer srv.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	const capBytes = 2048
	primer := knowledge.NewPrimer([]knowledge.LayerConfig{{Type: knowledge.LayerProject, URL: srv.URL, Shared: true}},
		knowledge.PrimerConfig{MaxFacts: 25, MaxBytes: capBytes}, logger)

	s := newScheduler()
	s.logger = logger
	s.SetPrimer(primer)
	issues := []github.Issue{makeIssue("org/repo", 1, "Fix hook crash", "", 5, []string{"typescript", "hooks"}, false)}

	got := s.primeKnowledge("quality", issues)
	if got == "" {
		t.Fatal("expected primed knowledge")
	}
	if len(got) > capBytes {
		t.Errorf("kick primer %d bytes exceeds cap %d", len(got), capBytes)
	}
	if !strings.Contains(got, knowledge.PrimerTruncatedMarker) {
		t.Errorf("kick should carry the truncation marker:\n%s", got)
	}
	if !strings.Contains(got, "facts / ") || !strings.Contains(got, "bytes omitted (size cap)") {
		t.Errorf("marker should name facts and bytes omitted:\n%s", got)
	}
	// Whole-entry drop: every rendered body is the full snippet.
	full := strings.Repeat(factBody, 4)
	if kept := strings.Count(got, "**Guard .join()"); kept == 0 || strings.Count(got, full) != kept {
		t.Errorf("kept=%d but %d intact bodies; a fact was clipped:\n%s", kept, strings.Count(got, full), got)
	}
	if !strings.Contains(logBuf.String(), "dropped facts over the byte cap") {
		t.Errorf("scheduler should log the drop, got:\n%s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "cap_bytes=2048") {
		t.Errorf("log should name the cap, got:\n%s", logBuf.String())
	}
}
