package dashboard

import (
	"net/http"
	"testing"
)

func TestClassifierSettingsRoundTrip(t *testing.T) {
	s := govServer(t)
	rec := doPut(s, "/api/config/classifier", map[string]any{
		"simple_keywords": []string{"nit", "typo"},
		"complex_signals": []string{"distributed consensus"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d: %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Classifier.SimpleKeywords; len(got) != 2 || got[0] != "nit" || got[1] != "typo" {
		t.Fatalf("simple keywords = %#v", got)
	}
	if got := s.deps.Config.Classifier.ComplexSignals; len(got) != 1 || got[0] != "distributed consensus" {
		t.Fatalf("complex signals = %#v", got)
	}
}
