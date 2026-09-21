package profiles

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestProjectActiveFirstAndAligned(t *testing.T) {
	dir := t.TempDir()
	f := sample()
	f.Active = "beta"
	if err := Project(dir, f); err != nil {
		t.Fatalf("Project: %v", err)
	}
	info, err := os.Stat(EnvPath(dir))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("contributor.env mode = %o, want 600", perm)
	}
	data, _ := os.ReadFile(EnvPath(dir))
	content := string(data)
	assertLine := func(key, value string) {
		t.Helper()
		if !strings.Contains(content, key+"="+value+"\n") {
			t.Errorf("projection missing %s=%s\n---\n%s", key, value, content)
		}
	}
	// Active profile (beta) leads every positional list, keeping index
	// pairing intact.
	assertLine("HIVE_HUB", "wss://beta.example.dev/contribute,wss://alpha.example.dev/contribute")
	assertLine("HIVE_REGISTRATION_TOKEN", "tok-2,tok-1")
	assertLine("CONTRIBUTOR_ID", "c-2,c-1")
	assertLine("CONTRIBUTOR_USERNAME", "octocat")
	// beta has no backend of its own; the projection must not invent one.
	if strings.Contains(content, "AGENT_BACKEND=") {
		t.Errorf("projection invented AGENT_BACKEND for a profile without one\n%s", content)
	}
	assertLine("HIVE_SESSION", "goose")
}

func TestProjectPreservesUnownedKeys(t *testing.T) {
	dir := t.TempDir()
	writeEnv(t, dir, `HIVE_HUB=wss://old.dev/contribute
HIVE_REGISTRATION_TOKEN=old-tok
CONTRIBUTOR_ID=old-id
HIVE_LITELLM_ENDPOINT=https://llm.example.dev
`)
	f := sample()
	if err := Project(dir, f); err != nil {
		t.Fatalf("Project: %v", err)
	}
	data, _ := os.ReadFile(EnvPath(dir))
	content := string(data)
	if !strings.Contains(content, "HIVE_LITELLM_ENDPOINT=https://llm.example.dev\n") {
		t.Errorf("projection dropped an unowned key\n%s", content)
	}
	if strings.Contains(content, "old-tok") {
		t.Errorf("projection kept a stale owned value\n%s", content)
	}
	// Backend comes from the active profile.
	if !strings.Contains(content, "AGENT_BACKEND=claude\n") {
		t.Errorf("projection missing active profile backend\n%s", content)
	}
}

func TestProjectRefusesEmpty(t *testing.T) {
	if err := Project(t.TempDir(), &File{}); err == nil {
		t.Fatal("Project with no profiles succeeded")
	}
}

// The acceptance line in #8097: the relay's misalignment FATAL cannot be
// reached from a file this tool wrote. Simulate the relay's check.
func TestProjectionSatisfiesRelayAlignment(t *testing.T) {
	dir := t.TempDir()
	f := sample()
	if err := Project(dir, f); err != nil {
		t.Fatalf("Project: %v", err)
	}
	env, err := parseEnvFile(EnvPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	hubs := splitList(env.values["HIVE_HUB"])
	tokens := splitList(env.values["HIVE_REGISTRATION_TOKEN"])
	if len(hubs) > 1 && len(tokens) != len(hubs) {
		t.Fatalf("projection is misaligned: %d hubs, %d tokens", len(hubs), len(tokens))
	}
}

func TestHubURLConversions(t *testing.T) {
	for in, want := range map[string]string{
		"wss://h.dev/contribute": "https://h.dev",
		"ws://h.dev/contribute":  "http://h.dev",
		"https://h.dev":          "https://h.dev",
	} {
		if got := HubHTTPBase(in); got != want {
			t.Errorf("HubHTTPBase(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"https://h.dev":          "wss://h.dev/contribute",
		"http://h.dev":           "ws://h.dev/contribute",
		"wss://h.dev/contribute": "wss://h.dev/contribute",
		"h.dev":                  "h.dev",
	} {
		if got := HubWSURL(in); got != want {
			t.Errorf("HubWSURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRegisterSuccess(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		gotBody = string(buf[:n])
		if r.Header.Get("Authorization") != "" {
			t.Error("register sent an Authorization header; the endpoint must receive no bearer token")
		}
		w.Write([]byte(`{"registration_token":"tok-x","contributor_id":"c-x","message":"ok"}`))
	}))
	defer srv.Close()
	reg, err := Register(context.Background(), srv.Client(), srv.URL, "octocat")
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if reg.RegistrationToken != "tok-x" || reg.ContributorID != "c-x" {
		t.Errorf("Register = %+v", reg)
	}
	if gotPath != "/api/contribute/register" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"github_username":"octocat"`) {
		t.Errorf("body = %q", gotBody)
	}
}

func TestRegisterErrors(t *testing.T) {
	cases := map[string]struct {
		response string
		wantSub  string
	}{
		"already registered": {`{"message":"Already registered"}`, "already registered"},
		"refusal":            {`{"message":"contributions closed"}`, "contributions closed"},
		"empty":              {`{}`, "no registration token"},
		"not json":           {`<html>`, "invalid register response"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Write([]byte(tc.response))
			}))
			defer srv.Close()
			_, err := Register(context.Background(), srv.Client(), srv.URL, "octocat")
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("Register = %v, want error containing %q", err, tc.wantSub)
			}
		})
	}
}

func TestRegisterConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close() // refuse connections
	if _, err := Register(context.Background(), http.DefaultClient, srv.URL, "octocat"); err == nil {
		t.Fatal("Register against a closed server succeeded")
	}
}
