package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompareAheadBy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/hivecommons/hive/compare/base111...head999", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"ahead_by": 4})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, slog.Default())
	got, err := c.CompareAheadBy(context.Background(), "hivecommons", "hive", "base111", "head999")
	if err != nil {
		t.Fatalf("CompareAheadBy: %v", err)
	}
	if got != 4 {
		t.Fatalf("CompareAheadBy = %d, want 4", got)
	}
}
