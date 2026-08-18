package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVaultConnectRejectsNonOwnerBeforeConnecting(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/knowledge/vaults", strings.NewReader(`{"path":"/safe/path"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "read-write")
	w := httptest.NewRecorder()

	(&Server{}).handleVaultsConnect(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner vault connect status = %d, want 403", w.Code)
	}
}

func TestVaultConnectRejectsSensitivePrefixes(t *testing.T) {
	s, _, wiki := apiServerWithKnowledge(t)
	defer wiki.Close()

	for _, path := range []string{"/etc", "/etc/hive", "/proc/self", "/var/run/secrets"} {
		t.Run(path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/knowledge/vaults", strings.NewReader(`{"path":"`+path+`"}`))
			req.Header.Set("Content-Type", "application/json")
			markOwnerRequest(req)
			w := httptest.NewRecorder()

			s.handleVaultsConnect(w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("sensitive vault path %q status = %d, want 400", path, w.Code)
			}
		})
	}
}
