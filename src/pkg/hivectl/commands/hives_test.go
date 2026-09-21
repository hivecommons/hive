package commands

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hivectl/profiles"
)

func executeHives(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	command := NewRootCommand(strings.NewReader(""), &stdout, &stderr)
	command.SetArgs(args)
	err := command.Execute()
	return stdout.String(), stderr.String(), err
}

func seedProfiles(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	err := profiles.Save(dir, &profiles.File{
		Active:   "alpha",
		Username: "octocat",
		Profiles: []profiles.Profile{
			{Name: "alpha", Hub: "wss://alpha.dev/contribute", ContributorID: "c-1", RegistrationToken: "tok-1"},
			{Name: "beta", Hub: "wss://beta.dev/contribute", ContributorID: "c-2", RegistrationToken: "tok-2"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHivesListMarksActiveAndHidesTokens(t *testing.T) {
	dir := seedProfiles(t)
	stdout, _, err := executeHives(t, "--output", "json", "hives", "list", "--config-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"name": "alpha"`) || !strings.Contains(stdout, `"active": true`) {
		t.Fatalf("stdout = %q", stdout)
	}
	if strings.Contains(stdout, "tok-1") {
		t.Fatalf("list output leaked a registration token: %q", stdout)
	}
}

func TestHivesListEmptyAndInvalidOutput(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := executeHives(t, "hives", "list", "--config-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "No hive profiles saved") {
		t.Fatalf("stdout = %q", stdout)
	}
	if _, _, err := executeHives(t, "--output", "nope", "hives", "list", "--config-dir", dir); err == nil {
		t.Fatal("invalid --output accepted")
	}
}

func TestHivesListMigratesPositionalEnv(t *testing.T) {
	dir := t.TempDir()
	envContent := "HIVE_REGISTRATION_TOKEN=tok-1,tok-2\nHIVE_HUB=wss://a.dev/contribute,wss://b.dev/contribute\nCONTRIBUTOR_ID=c-1,c-2\nCONTRIBUTOR_USERNAME=octocat\n"
	if err := os.WriteFile(filepath.Join(dir, "contributor.env"), []byte(envContent), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, _, err := executeHives(t, "--output", "json", "hives", "list", "--config-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "a.dev") || !strings.Contains(stdout, "b.dev") {
		t.Fatalf("migration did not surface both hubs: %q", stdout)
	}
	if _, err := profiles.Load(dir); err != nil {
		t.Fatalf("profiles.yml not written by migration: %v", err)
	}
}

func TestHivesUseProjectsActiveFirst(t *testing.T) {
	dir := seedProfiles(t)
	stdout, _, err := executeHives(t, "hives", "use", "beta", "--config-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, `"beta"`) {
		t.Fatalf("stdout = %q", stdout)
	}
	data, err := os.ReadFile(filepath.Join(dir, "contributor.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "HIVE_HUB=wss://beta.dev/contribute,wss://alpha.dev/contribute") {
		t.Fatalf("projection does not lead with the active hub:\n%s", data)
	}
	if _, _, err := executeHives(t, "hives", "use", "ghost", "--config-dir", dir); err == nil {
		t.Fatal("use ghost succeeded")
	}
}

func TestHivesRemoveAndRename(t *testing.T) {
	dir := seedProfiles(t)
	if _, _, err := executeHives(t, "hives", "remove", "alpha", "--config-dir", dir); err != nil {
		t.Fatal(err)
	}
	f, err := profiles.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Profiles) != 1 || f.Active != "beta" {
		t.Fatalf("after remove: %+v", f)
	}
	if _, _, err := executeHives(t, "hives", "rename", "beta", "prod", "--config-dir", dir); err != nil {
		t.Fatal(err)
	}
	f, err = profiles.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Active != "prod" || f.Profiles[0].Name != "prod" {
		t.Fatalf("after rename: %+v", f)
	}
	if _, _, err := executeHives(t, "hives", "remove", "ghost", "--config-dir", dir); err == nil {
		t.Fatal("remove ghost succeeded")
	}
	if _, _, err := executeHives(t, "hives", "rename", "ghost", "x", "--config-dir", dir); err == nil {
		t.Fatal("rename ghost succeeded")
	}
	// Removing the last profile leaves no projection requirement.
	if _, _, err := executeHives(t, "hives", "remove", "prod", "--config-dir", dir); err != nil {
		t.Fatal(err)
	}
}

func TestHivesAddRegistersAndProjects(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/contribute/register" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		io.WriteString(w, `{"registration_token":"tok-new","contributor_id":"c-new","message":"registered"}`)
	}))
	defer hub.Close()

	dir := t.TempDir()
	stdout, _, err := executeHives(t, "hives", "add", "myhive", "--hub", hub.URL, "--username", "octocat", "--config-dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout, "c-new") {
		t.Fatalf("stdout = %q", stdout)
	}
	f, err := profiles.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if f.Active != "myhive" || f.Username != "octocat" || f.Profiles[0].RegistrationToken != "tok-new" {
		t.Fatalf("saved file: %+v", f)
	}
	if !strings.HasPrefix(f.Profiles[0].Hub, "ws://") || !strings.HasSuffix(f.Profiles[0].Hub, "/contribute") {
		t.Fatalf("hub not normalized to a contribute WS URL: %q", f.Profiles[0].Hub)
	}
	data, err := os.ReadFile(filepath.Join(dir, "contributor.env"))
	if err != nil {
		t.Fatalf("projection not written: %v", err)
	}
	if !strings.Contains(string(data), "HIVE_REGISTRATION_TOKEN=tok-new") {
		t.Fatalf("projection missing token:\n%s", data)
	}
	// A second add against the same file reuses the saved username.
	if _, _, err := executeHives(t, "hives", "add", "second", "--hub", hub.URL, "--config-dir", dir); err != nil {
		t.Fatalf("second add: %v", err)
	}
	// Duplicate names are refused.
	if _, _, err := executeHives(t, "hives", "add", "myhive", "--hub", hub.URL, "--config-dir", dir); err == nil {
		t.Fatal("duplicate add succeeded")
	}
}

func TestHivesAddUsageErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := executeHives(t, "hives", "add", "x", "--config-dir", dir); err == nil {
		t.Fatal("add without --hub succeeded")
	}
	if _, _, err := executeHives(t, "hives", "add", "bad,name", "--hub", "wss://h.dev/contribute", "--config-dir", dir); err == nil {
		t.Fatal("add with invalid name succeeded")
	}
	t.Setenv("CONTRIBUTOR_USERNAME", "")
	if _, _, err := executeHives(t, "hives", "add", "x", "--hub", "wss://h.dev/contribute", "--config-dir", dir); err == nil || !strings.Contains(err.Error(), "--username") {
		t.Fatalf("add without username = %v, want username usage error", err)
	}
}

func TestHivesAddSurfacesHubRefusal(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		io.WriteString(w, `{"message":"Already registered"}`)
	}))
	defer hub.Close()
	dir := t.TempDir()
	_, _, err := executeHives(t, "hives", "add", "x", "--hub", hub.URL, "--username", "octocat", "--config-dir", dir)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("err = %v", err)
	}
}
