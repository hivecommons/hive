package commands

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type fakeEnrollClient struct {
	req       liteEnrollRequest
	repoToken string
}

func (f *fakeEnrollClient) DoWithHeaders(_ context.Context, method, path string, _ url.Values, body any, headers http.Header) (any, error) {
	if method != "POST" || path != "/api/saas/lite/enroll" {
		return nil, errors.New("unexpected request")
	}
	f.req = body.(liteEnrollRequest)
	f.repoToken = headers.Get("X-Hive-GitHub-Token")
	return map[string]any{
		"id":              "lite-o-r",
		"spoke_id":        "spoke-o-r",
		"action":          "spoke_config_update",
		"installation_id": float64(123),
		"dashboard_url":   "https://hub.example/dashboard",
	}, nil
}

type fakeGHVerifier struct {
	err error
}

func (f fakeGHVerifier) Verify(context.Context, string, string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return "gho-test", nil
}

func TestRunEnrollValidation(t *testing.T) {
	tests := []struct {
		name string
		repo string
		opts enrollOptions
		gh   ghVerifier
		want string
	}{
		{name: "repo form required", repo: "owner", opts: enrollOptions{acmmLevel: 2, githubHost: "github.com"}, gh: fakeGHVerifier{}, want: "owner/repo"},
		{name: "acmm positive", repo: "o/r", opts: enrollOptions{acmmLevel: 0, githubHost: "github.com"}, gh: fakeGHVerifier{}, want: "at least 1"},
		{name: "gh failure", repo: "o/r", opts: enrollOptions{acmmLevel: 2, githubHost: "github.com"}, gh: fakeGHVerifier{err: errors.New("no auth")}, want: "no auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := &commandEnv{options: &rootOptions{server: "http://127.0.0.1:1", output: "table"}}
			cmd := NewRootCommand(strings.NewReader(""), &strings.Builder{}, &strings.Builder{})
			err := env.runEnrollCommand(cmd, tt.repo, &tt.opts, tt.gh)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func TestRunEnrollPostsCappedRequest(t *testing.T) {
	client := &fakeEnrollClient{}
	got, err := runEnroll(context.Background(), client, liteEnrollRequest{
		Owner:          "kubestellar",
		Repo:           "hive",
		InstallationID: 456,
		ACMMLevel:      2,
		GitHubHost:     "github.com",
	}, "repo-token")
	if err != nil {
		t.Fatal(err)
	}
	if got["id"] != "lite-o-r" {
		t.Fatalf("response = %#v", got)
	}
	if client.req.Owner != "kubestellar" || client.req.Repo != "hive" || client.req.InstallationID != 456 || client.req.ACMMLevel != 2 {
		t.Fatalf("request = %#v", client.req)
	}
	if client.repoToken != "repo-token" {
		t.Fatalf("repo token header = %q", client.repoToken)
	}
}

func TestEnrollCommandHappyPathCapsACMM(t *testing.T) {
	t.Setenv("HIVE_DASHBOARD_TOKEN", "")
	oldRunGH := runGH
	t.Cleanup(func() { runGH = oldRunGH })
	runGH = func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return "gho-test\n", nil
		}
		return "ok", nil
	}

	var body liteEnrollRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/saas/lite/enroll" || r.Method != http.MethodPost {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer gho-test" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("X-Hive-GitHub-Token") != "gho-test" {
			t.Fatalf("repo token header = %q", r.Header.Get("X-Hive-GitHub-Token"))
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"id":              "lite-kubestellar-hive",
			"spoke_id":        "spoke-kubestellar-hive",
			"action":          "hosted_lite_spoke_requested",
			"installation_id": 123,
			"dashboard_url":   "https://hub.example/dashboard",
		})
	}))
	defer server.Close()

	stdout, _, err := execute(t, server, "", "enroll", "hivecommons/hive", "--acmm-level", "5")
	if err != nil {
		t.Fatal(err)
	}
	if body.ACMMLevel != 2 {
		t.Fatalf("ACMMLevel = %d, want capped L2", body.ACMMLevel)
	}
	if !strings.Contains(stdout, "capped to L2") || !strings.Contains(stdout, "no repository PAT") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestGHVerifierUsesHostQualifiedRepoForGHE(t *testing.T) {
	oldRunGH := runGH
	t.Cleanup(func() { runGH = oldRunGH })
	var repoViewTarget string
	runGH = func(_ context.Context, args ...string) (string, error) {
		if len(args) >= 3 && args[0] == "repo" && args[1] == "view" {
			repoViewTarget = args[2]
		}
		if len(args) >= 2 && args[0] == "auth" && args[1] == "token" {
			return "ghe-token\n", nil
		}
		return "ok", nil
	}
	if _, err := (ghCLI{}).Verify(context.Background(), "github.example.com", "org/repo"); err != nil {
		t.Fatal(err)
	}
	if repoViewTarget != "https://github.example.com/org/repo" {
		t.Fatalf("repo view target = %q", repoViewTarget)
	}
}
