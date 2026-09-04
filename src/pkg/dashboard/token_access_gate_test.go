package dashboard

// Security regression tests for #3936: GET /api/token-access must be gated at
// owner role so read-only and read-write authenticated users cannot enumerate the
// full gh-command audit log (which contains full argument lists: --repo, --title,
// --body, etc.).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTokenAccessRequiresOwnerRole is the direct behavioural guard.
func TestTokenAccessRequiresOwnerRole(t *testing.T) {
	s, _ := apiServer(t)

	// Non-owner GET — must be 403.
	rec := doGet(s, "/api/token-access")
	if rec.Code != http.StatusForbidden {
		t.Errorf("non-owner GET /api/token-access = %d, want 403 — any authenticated "+
			"user can read the full gh-command audit log (sec-check #3936)", rec.Code)
	}

	// Owner GET — must succeed (no file in tests → no-log fallback, still 200).
	rec = doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Errorf("owner GET /api/token-access = %d, want 200", rec.Code)
	}
}

// TestTokenAccessReadWriteIsRejected pins that a contributor (read-write) role
// is NOT sufficient — contributors must not see the token access log.
func TestTokenAccessReadWriteIsRejected(t *testing.T) {
	s, _ := apiServer(t)

	req := httptest.NewRequest(http.MethodGet, "/api/token-access", nil)
	req.Header.Set("X-Hive-Role", "read-write")
	// Deliberately no X-Hive-Owner-Role-Verified header.
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("read-write GET /api/token-access = %d, want 403 — contributor can "+
			"read full gh-command history (sec-check #3936)", rec.Code)
	}
}

// TestTokenAccessSourceGate is the source-level invariant: handleTokenAccess
// must contain requireOwnerRole so a sync merge cannot silently drop the gate.
// Mirrors the pattern established in f16_owner_gate_test.go.
func TestTokenAccessSourceGate(t *testing.T) {
	body := f16HandlerBody(t, f16ReadSource(t, "api.go"), "handleTokenAccess")
	if !strings.Contains(body, "requireOwnerRole(w, r)") {
		t.Error("handleTokenAccess in api.go has no requireOwnerRole gate — " +
			"any authenticated user can read the full gh-command audit log (#3936). " +
			"Restore the gate; do not remove this test.")
	}
}

func TestTokenAccessSuccessSkipsBlankLinesAndCapsEntries(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	orig := tokenAccessLogPath
	tokenAccessLogPath = filepath.Join(dir, "token-access.jsonl")
	t.Cleanup(func() { tokenAccessLogPath = orig })

	var b strings.Builder
	b.WriteString("\n")
	const extraEntries = 5
	for i := 0; i < tokenAccessMaxEntries+extraEntries; i++ {
		if i == 3 {
			b.WriteString("\n")
		}
		b.WriteString(`{"seq":`)
		b.WriteString(fmt.Sprint(i))
		b.WriteString(`,"cmd":"gh pr view"}` + "\n")
	}
	if err := os.WriteFile(tokenAccessLogPath, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write token access log: %v", err)
	}

	rec := doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Entries) != tokenAccessMaxEntries {
		t.Fatalf("entries = %d, want %d", len(got.Entries), tokenAccessMaxEntries)
	}
	firstSeq, ok := got.Entries[0]["seq"].(float64)
	if !ok {
		t.Fatalf("first seq has type %T, want number", got.Entries[0]["seq"])
	}
	if int(firstSeq) != extraEntries {
		t.Fatalf("first seq = %v, want %d", firstSeq, extraEntries)
	}
}

func TestTokenAccessMissingLogReturnsEmptyEntriesAndError(t *testing.T) {
	s, _ := apiServer(t)
	dir := t.TempDir()
	orig := tokenAccessLogPath
	tokenAccessLogPath = filepath.Join(dir, "missing.jsonl")
	t.Cleanup(func() { tokenAccessLogPath = orig })

	rec := doOwnerGet(s, "/api/token-access")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got struct {
		Entries []json.RawMessage `json:"entries"`
		Error   string            `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Entries) != 0 || got.Error != "no audit log" {
		t.Fatalf("response = %+v, want empty entries with no audit log error", got)
	}
}
