package integrated

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	hivegithub "github.com/kubestellar/hive/pkg/github"
)

func TestResolveSetupAuthorizationIdentitiesSeparatesHumanFromAppWriter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || !strings.EqualFold(request.URL.Path, "/users/DavidDiaz0317") {
			http.Error(writer, "unexpected request", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":101,"login":"daviddiaz0317","type":"User"}`)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	app := setupIdentityTestApp()
	actor, writer, resolvedApp, err := resolveSetupAuthorizationIdentities(context.Background(), SetupOptions{
		Repository: "Owner/Repository", GitHub: client,
		AuthorizationActor:  hivegithub.AuthenticatedUserIdentity{ID: 101, Login: "DavidDiaz0317", Type: "User"},
		AuthorizationWriter: hivegithub.AuthenticatedUserIdentity{ID: 202, Login: "hive-test[bot]", Type: "Bot"},
		AuthorizationApp:    app,
	})
	if err != nil {
		t.Fatal(err)
	}
	if actor.ID != 101 || actor.Login != "daviddiaz0317" || writer.ID != 202 || resolvedApp.AppID != app.AppID {
		t.Fatalf("setup identity split was not preserved: actor=%+v writer=%+v app=%+v", actor, writer, resolvedApp)
	}
}

func TestResolveSetupAuthorizationIdentitiesFailsClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{"id": 101, "login": "owner", "type": "User"})
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	actor := hivegithub.AuthenticatedUserIdentity{ID: 101, Login: "owner", Type: "User"}
	writer := hivegithub.AuthenticatedUserIdentity{ID: 202, Login: "hive-test[bot]", Type: "Bot"}

	for _, test := range []struct {
		name string
		edit func(*SetupOptions)
	}{
		{name: "wrong repository", edit: func(options *SetupOptions) { options.AuthorizationApp.Repository = "owner/other" }},
		{name: "wrong writer", edit: func(options *SetupOptions) { options.AuthorizationWriter.ID++ }},
		{name: "missing actions permission", edit: func(options *SetupOptions) { options.AuthorizationApp.Permissions["actions"] = "read" }},
		{name: "empty actor", edit: func(options *SetupOptions) { options.AuthorizationActor = hivegithub.AuthenticatedUserIdentity{} }},
		{name: "forged actor id", edit: func(options *SetupOptions) { options.AuthorizationActor.ID++ }},
	} {
		t.Run(test.name, func(t *testing.T) {
			options := SetupOptions{Repository: "owner/repository", GitHub: client, AuthorizationActor: actor, AuthorizationWriter: writer, AuthorizationApp: setupIdentityTestApp()}
			test.edit(&options)
			if _, _, _, err := resolveSetupAuthorizationIdentities(context.Background(), options); err == nil {
				t.Fatal("invalid setup identity binding was accepted")
			}
		})
	}
}

func TestResolveSetupAuthorizationIdentitiesPreservesPATCompatibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/user" {
			http.Error(writer, "unexpected request", http.StatusNotFound)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":303,"login":"standalone-owner","type":"User"}`)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	actor, writer, app, err := resolveSetupAuthorizationIdentities(context.Background(), SetupOptions{Repository: "owner/repository", GitHub: client})
	if err != nil {
		t.Fatal(err)
	}
	if actor != writer || actor.ID != 303 || app.AppID != 0 {
		t.Fatalf("standalone PAT identity changed: actor=%+v writer=%+v app=%+v", actor, writer, app)
	}
}

func TestResolveManagedOperatorRequiresInstalledAppWriterBinding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":101,"login":"owner","type":"User"}`)
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	app := setupIdentityTestApp()
	writer := hivegithub.AuthenticatedUserIdentity{ID: app.BotID, Login: app.BotLogin, Type: app.BotType}
	if err := client.SetVerifiedAppWriter(writer, app.BindingDigest); err != nil {
		t.Fatal(err)
	}
	config := Config{
		Repository: "owner/repository", SetupAuthorizationActorID: 101, SetupAuthorizationActorLogin: "Owner",
		SetupAuthorizationWriterID: writer.ID, SetupAuthorizationWriterLogin: writer.Login, SetupAuthorizationWriterType: writer.Type,
		SetupAuthorizationAppID: app.AppID, SetupAuthorizationInstallationID: app.InstallationID, SetupAuthorizationAppBindingDigest: app.BindingDigest,
	}
	resolved, err := resolveManagedOperatorIdentity(context.Background(), client, config, hivegithub.AuthenticatedUserIdentity{ID: 101, Login: "OWNER", Type: "User"}, "status")
	if err != nil || resolved.ID != 101 {
		t.Fatalf("installed App-backed operator was rejected: identity=%+v err=%v", resolved, err)
	}
	config.SetupAuthorizationAppBindingDigest = strings.Repeat("f", 64)
	if _, err := resolveManagedOperatorIdentity(context.Background(), client, config, hivegithub.AuthenticatedUserIdentity{ID: 101, Login: "owner", Type: "User"}, "status"); err == nil {
		t.Fatal("wrong installed App writer binding was accepted")
	}
}

func TestResolveManagedOperatorPreservesExplicitPATCompatibility(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch strings.ToLower(request.URL.Path) {
		case "/user", "/users/standalone-owner":
			_, _ = io.WriteString(writer, `{"id":303,"login":"standalone-owner","type":"User"}`)
		default:
			http.Error(writer, `{"message":"not found"}`, http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := hivegithub.NewClientForTest(server.URL, "", nil, slog.Default())
	config := Config{
		Repository: "owner/repository", SetupAuthorizationActorID: 303, SetupAuthorizationActorLogin: "Standalone-Owner",
		SetupAuthorizationWriterID: 303, SetupAuthorizationWriterLogin: "standalone-owner", SetupAuthorizationWriterType: "User",
	}
	explicit := hivegithub.AuthenticatedUserIdentity{ID: 303, Login: "STANDALONE-OWNER", Type: "User"}
	resolved, err := resolveManagedOperatorIdentity(context.Background(), client, config, explicit, "uninstall")
	if err != nil || resolved.ID != 303 || !strings.EqualFold(resolved.Login, explicit.Login) {
		t.Fatalf("standalone PAT-backed managed operator was rejected: identity=%+v err=%v", resolved, err)
	}

	config.SetupAuthorizationWriterID = 304
	if _, err := resolveManagedOperatorIdentity(context.Background(), client, config, explicit, "uninstall"); err == nil || !strings.Contains(err.Error(), "operator and PAT writer to be identical") {
		t.Fatalf("mismatched standalone PAT writer was accepted: %v", err)
	}
	config.SetupAuthorizationWriterID = 303
	config.SetupAuthorizationAppID = 11
	if _, err := resolveManagedOperatorIdentity(context.Background(), client, config, explicit, "uninstall"); err == nil || !strings.Contains(err.Error(), "operator and PAT writer to be identical") {
		t.Fatalf("User writer with an App binding was accepted: %v", err)
	}
}

func setupIdentityTestApp() hivegithub.AppRuntimeIdentity {
	return hivegithub.AppRuntimeIdentity{
		AppID: 11, InstallationID: 22, BotID: 202, BotLogin: "hive-test[bot]", BotType: "Bot",
		Repository: "owner/repository", RepositoryID: 33,
		Permissions:      map[string]string{"actions": "write", "workflows": "write", "checks": "read", "statuses": "write", "contents": "write", "issues": "write", "pull_requests": "write", "metadata": "read"},
		PermissionDigest: strings.Repeat("a", 64), BindingDigest: strings.Repeat("b", 64),
	}
}
