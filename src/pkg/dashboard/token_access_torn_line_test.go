package dashboard

// Regression test for #6407: handleTokenAccess must tolerate a torn (partially
// written) final line in token-access.jsonl instead of letting json.RawMessage
// pass an invalid fragment straight through to jsonResponse, which previously
// caused the whole encode to fail and the endpoint to return an empty body.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenAccessSkipsTornTailLine(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	orig := tokenAccessLogPath
	tokenAccessLogPath = filepath.Join(dir, "token-access.jsonl")
	t.Cleanup(func() { tokenAccessLogPath = orig })

	// Two valid entries, a stray blank line, then a torn/truncated final
	// line as would be observed mid-append by a concurrent writer.
	content := `{"seq":1,"cmd":"gh pr view"}
{"seq":2,"cmd":"gh pr list"}

{"seq":3,"cmd":"gh pr crea`
	if err := os.WriteFile(tokenAccessLogPath, []byte(content), 0o600); err != nil {
		t.Fatalf("write token access log: %v", err)
	}

	rec := doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("response body is not valid JSON: %s", rec.Body.String())
	}

	var got struct {
		Entries []map[string]any `json:"entries"`
		Skipped int              `json:"skipped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Entries) != 2 {
		t.Fatalf("entries = %d, want 2; got=%+v", len(got.Entries), got.Entries)
	}
	if got.Skipped != 1 {
		t.Fatalf("skipped = %d, want 1", got.Skipped)
	}
}
