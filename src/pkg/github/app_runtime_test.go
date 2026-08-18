package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestResolveAppRuntimeIdentityBindsInstallationRepositoryWriterAndPermissions(t *testing.T) {
	permissions := map[string]string{
		"actions": "write", "workflows": "write", "statuses": "write", "contents": "write",
		"issues": "write", "pull_requests": "write", "checks": "read", "metadata": "read",
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/app/installations/22":
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"id": 22, "app_id": 11, "app_slug": "hive-test",
				"account": map[string]any{"login": "owner"}, "permissions": permissions,
			})
		case "/repos/owner/repository":
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 33, "full_name": "Owner/Repository", "default_branch": "main"})
		case "/users/hive-test[bot]":
			_ = json.NewEncoder(writer).Encode(map[string]any{"id": 44, "login": "hive-test[bot]", "type": "Bot"})
		default:
			http.Error(writer, "unexpected path "+request.URL.Path, http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	auth := &AppAuth{
		appID: 11, installationID: 22, key: discoveryTestKey(t), logger: discoveryTestLogger(), apiURL: server.URL + "/",
		cachedToken: "installation-token", tokenExpiry: time.Now().Add(time.Hour),
	}
	client := NewClientFromAppWithBotLogin(auth, "owner", []string{"repository"}, discoveryTestLogger(), "hive-test[bot]")
	identity, err := client.ResolveAppRuntimeIdentity(context.Background(), "owner/repository")
	if err != nil {
		t.Fatal(err)
	}
	if identity.AppID != 11 || identity.InstallationID != 22 || identity.RepositoryID != 33 || identity.BotID != 44 ||
		!strings.EqualFold(identity.Repository, "owner/repository") || identity.PermissionDigest == "" || identity.BindingDigest == "" {
		t.Fatalf("unexpected App runtime identity: %+v", identity)
	}
	if err := identity.RequireVisualHivePermissions(); err != nil {
		t.Fatal(err)
	}
	if err := client.SetVerifiedAppWriter(AuthenticatedUserIdentity{ID: identity.BotID, Login: identity.BotLogin, Type: identity.BotType}, identity.BindingDigest); err != nil {
		t.Fatal(err)
	}
	if err := client.VerifyAppWriterBinding(AuthenticatedUserIdentity{ID: identity.BotID, Login: strings.ToUpper(identity.BotLogin), Type: "bot"}, identity.BindingDigest); err != nil {
		t.Fatalf("case-insensitive exact writer did not verify: %v", err)
	}
}

func TestVerifiedAppWriterConcurrentRefreshAndRead(t *testing.T) {
	client := &Client{}
	writer := AuthenticatedUserIdentity{ID: 44, Login: "hive-test[bot]", Type: "Bot"}
	binding := strings.Repeat("a", 64)
	if err := client.SetVerifiedAppWriter(writer, binding); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for index := 0; index < 8; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for attempt := 0; attempt < 250; attempt++ {
				if err := client.SetVerifiedAppWriter(writer, binding); err != nil {
					t.Errorf("refresh writer: %v", err)
					return
				}
				if client.AppBotLogin() != writer.Login {
					t.Errorf("writer login changed during refresh")
					return
				}
				if err := client.VerifyAppWriterBinding(writer, binding); err != nil {
					t.Errorf("verify writer: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func TestAppRuntimePermissionDigestChangesWithoutChangingStructuralBinding(t *testing.T) {
	base := AppRuntimeIdentity{
		AppID: 11, InstallationID: 22, BotID: 44, BotLogin: "hive-test[bot]", BotType: "Bot",
		Repository: "owner/repository", RepositoryID: 33,
	}
	firstPermissions := map[string]string{"actions": "read", "statuses": "write"}
	secondPermissions := map[string]string{"actions": "write", "statuses": "write"}
	firstPermissionDigest, err := digestAppRuntimeValue(firstPermissions)
	if err != nil {
		t.Fatal(err)
	}
	secondPermissionDigest, err := digestAppRuntimeValue(secondPermissions)
	if err != nil {
		t.Fatal(err)
	}
	structural := struct {
		AppID          int64  `json:"app_id"`
		InstallationID int64  `json:"installation_id"`
		BotID          int64  `json:"bot_id"`
		BotLogin       string `json:"bot_login"`
		Repository     string `json:"repository"`
		RepositoryID   int64  `json:"repository_id"`
	}{base.AppID, base.InstallationID, base.BotID, strings.ToLower(base.BotLogin), strings.ToLower(base.Repository), base.RepositoryID}
	binding, err := digestAppRuntimeValue(structural)
	if err != nil {
		t.Fatal(err)
	}
	if firstPermissionDigest == secondPermissionDigest || binding == "" {
		t.Fatalf("permission and structural digests were not independent: first=%s second=%s binding=%s", firstPermissionDigest, secondPermissionDigest, binding)
	}
	base.Permissions = firstPermissions
	if err := base.RequireVisualHivePermissions(); err == nil || !strings.Contains(err.Error(), "actions") {
		t.Fatalf("missing Actions write permission was accepted: %v", err)
	}
	withoutWorkflows := map[string]string{
		"actions": "write", "statuses": "write", "contents": "write", "issues": "write", "pull_requests": "write", "checks": "read", "metadata": "read",
	}
	base.Permissions = withoutWorkflows
	if err := base.RequireVisualHivePermissions(); err == nil || !strings.Contains(err.Error(), "workflows") {
		t.Fatalf("missing Workflows write permission was accepted: %v", err)
	}
	complete := map[string]string{
		"actions": "write", "workflows": "write", "statuses": "write", "contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read",
	}
	base.Permissions = complete
	if err := base.RequireVisualHivePermissions(); err == nil || !strings.Contains(err.Error(), "checks") {
		t.Fatalf("missing Checks read permission was accepted: %v", err)
	}
}
