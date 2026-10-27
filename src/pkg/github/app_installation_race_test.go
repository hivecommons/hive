package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestInstallationID_ReadsRaceAdoptInstallationID is the regression test for
// hivecommons/hive#14123: AdoptInstallationID rewrites installationID under
// a.mu, so every reader must hold a.mu too. Before the fix ScopedTokenForRepos,
// MintInstallationToken, VerifyInstallation and InstallationID() read the
// field bare, which `go test -race` reports as a data race here. Each call
// must also send one installation ID end to end — never 0, never a value
// that was not adopted.
func TestInstallationID_ReadsRaceAdoptInstallationID(t *testing.T) {
	key, _ := generateTestKey(t)

	var mu sync.Mutex
	var badPaths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// /app/installations/{id} or /app/installations/{id}/access_tokens
		rest := strings.TrimPrefix(r.URL.Path, "/app/installations/")
		id, err := strconv.ParseInt(strings.TrimSuffix(rest, "/access_tokens"), 10, 64)
		if err != nil || (id != 100 && id != 200) {
			mu.Lock()
			badPaths = append(badPaths, r.URL.Path)
			mu.Unlock()
		}
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			json.NewEncoder(w).Encode(map[string]any{
				"token":      "tok",
				"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "account": map[string]any{"login": "org"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := &AppAuth{appID: 1, installationID: 100, key: key, logger: quietLogger(), apiURL: server.URL}

	const rounds = 20
	ctx := context.Background()
	var wg sync.WaitGroup
	wg.Add(5)
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if i%2 == 0 {
				auth.AdoptInstallationID(200)
			} else {
				auth.AdoptInstallationID(100)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, err := auth.ScopedTokenForRepos(ctx, "contributor", nil); err != nil {
				t.Errorf("ScopedTokenForRepos: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if _, _, err := auth.MintInstallationToken(ctx, nil); err != nil {
				t.Errorf("MintInstallationToken: %v", err)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			info, err := auth.VerifyInstallation(ctx)
			if err != nil {
				t.Errorf("VerifyInstallation: %v", err)
				continue
			}
			if info.InstallationID != 100 && info.InstallationID != 200 {
				t.Errorf("VerifyInstallation reported installation %d, want 100 or 200", info.InstallationID)
			}
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < rounds; i++ {
			if id := auth.InstallationID(); id != 100 && id != 200 {
				t.Errorf("InstallationID() = %d, want 100 or 200", id)
			}
		}
	}()
	wg.Wait()

	if len(badPaths) > 0 {
		t.Errorf("requests named an installation that was never adopted: %v", badPaths)
	}
}
