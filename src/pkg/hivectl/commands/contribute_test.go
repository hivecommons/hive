package commands

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestContributeTokenCommands(t *testing.T) {
	for _, tc := range []struct {
		args         []string
		method, path string
	}{
		{[]string{"list"}, http.MethodGet, "/api/contributors"},
		{[]string{"revoke", "alice"}, http.MethodPost, "/api/contributors/alice/token/revoke"},
		{[]string{"revoke", "--all", "--yes"}, http.MethodPost, "/api/contributors/tokens/revoke-all"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != tc.method || r.URL.Path != tc.path {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"revoked":1}`))
			}))
			defer server.Close()
			args := append([]string{"--output", "json", "contribute", "tokens"}, tc.args...)
			if _, _, err := execute(t, server, "", args...); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestContributeBulkRevokeRequiresConfirmation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unconfirmed revoke reached API") }))
	defer server.Close()
	for _, args := range [][]string{{"--all"}, {"--all", "--yes", "alice"}, {}} {
		cmd := append([]string{"contribute", "tokens", "revoke"}, args...)
		if _, _, err := execute(t, server, "", cmd...); err == nil {
			t.Fatal("invalid revoke accepted")
		}
	}
}
