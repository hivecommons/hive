package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type githubActivityRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn githubActivityRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return fn(r)
}

func TestReloadGitHubActivityAcceptsHubOnlyConfig(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-not-a-real-credential")
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	cfgYAML := `hive_id: hub-only
project:
  org: test-org
github:
  token: test-token-not-real
hub:
  enabled: true
data:
  agents_dir: ` + filepath.Join(dir, "agents") + `
notifications:
  discord:
    factory_webhook: https://discord.invalid/webhook
  github_activity:
    enabled: true
    poll_interval_s: 60
`
	if err := os.WriteFile(path, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := NewHubServer(0, slog.Default(), "abc1234", "v5")
	srv.SetHubConfigPath(path, "")
	if err := srv.ReloadGitHubActivityFromConfig(slog.Default()); err != nil {
		t.Fatalf("ReloadGitHubActivityFromConfig: %v", err)
	}
	srv.githubActivityMu.Lock()
	defer srv.githubActivityMu.Unlock()
	if srv.githubActivityFeed == nil {
		t.Fatal("hub-only config with github_activity enabled did not install a feed")
	}
}

func TestGitHubActivityPollerDiffsFormatsAndDedupes(t *testing.T) {
	var mu sync.Mutex
	phase := 0
	posted := []string{}

	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("discord method = %s, want POST", r.Method)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode discord payload: %v", err)
		}
		mu.Lock()
		posted = append(posted, payload["content"].(string))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := phase
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/orgs/hivecommons/repos":
			_, _ = w.Write([]byte(`[{"name":"hive"},{"name":"docs"},{"name":"infra"}]`))
		case "/repos/hivecommons/hive/issues":
			if p == 0 {
				_, _ = w.Write([]byte(`[{"number":1,"title":"old","state":"open","html_url":"https://github.test/hive/1","updated_at":"2026-09-23T01:00:00Z","user":{"login":"alice"},"labels":[],"assignees":[]}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"number":1,"title":"old","state":"open","html_url":"https://github.test/hive/1","updated_at":"2026-09-23T01:00:00Z","user":{"login":"alice"},"labels":[],"assignees":[]},{"number":2,"title":"factory floor","state":"open","html_url":"https://github.test/hive/2","updated_at":"2026-09-23T02:00:00Z","user":{"login":"someone"},"labels":[{"name":"hive/help"}],"assignees":[]}]`))
		case "/repos/hivecommons/hive/pulls":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/hivecommons/docs/issues":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/hivecommons/docs/pulls":
			if p == 0 {
				_, _ = w.Write([]byte(`[{"number":7,"title":"doc fix","state":"open","html_url":"https://github.test/docs/7","updated_at":"2026-09-23T01:00:00Z","user":{"login":"docwriter"},"head":{"sha":"sha-open"},"base":{"ref":"v6"},"draft":false,"requested_reviewers":[]}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"number":7,"title":"doc fix","state":"closed","html_url":"https://github.test/docs/7","updated_at":"2026-09-23T02:00:00Z","merged_at":"2026-09-23T02:00:00Z","merge_commit_sha":"merge-sha","user":{"login":"docwriter"},"head":{"sha":"sha-open"},"base":{"ref":"v6"},"draft":false,"requested_reviewers":[]},{"number":8,"title":"fast fix","state":"closed","html_url":"https://github.test/docs/8","updated_at":"2026-09-23T02:00:00Z","merged_at":"2026-09-23T02:00:00Z","merge_commit_sha":"fast-merge","user":{"login":"fastdev"},"head":{"sha":"fast-head"},"base":{"ref":"v5"},"draft":false,"requested_reviewers":[]}]`))
		case "/repos/hivecommons/infra/issues":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/hivecommons/infra/pulls":
			if p == 0 {
				_, _ = w.Write([]byte(`[{"number":3,"title":"🌱 Forward-merge v5 into v6","state":"open","html_url":"https://github.test/infra/3","updated_at":"2026-09-23T01:00:00Z","user":{"login":"forward-bot[bot]"},"head":{"sha":"fw-open"},"base":{"ref":"v6"},"draft":false,"requested_reviewers":[]}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"number":3,"title":"🌱 Forward-merge v5 into v6","state":"closed","html_url":"https://github.test/infra/3","updated_at":"2026-09-23T02:00:00Z","merged_at":"2026-09-23T02:00:00Z","merge_commit_sha":"fw-merge","user":{"login":"forward-bot[bot]"},"head":{"sha":"fw-open"},"base":{"ref":"v6"},"draft":false,"requested_reviewers":[]},{"number":4,"title":"bump module","state":"open","html_url":"https://github.test/infra/4","updated_at":"2026-09-23T02:00:00Z","user":{"login":"dependabot[bot]"},"head":{"sha":"dep"},"base":{"ref":"main"},"draft":false,"requested_reviewers":[]}]`))
		case "/repos/hivecommons/docs/commits/sha-open/status", "/repos/hivecommons/infra/commits/fw-open/status", "/repos/hivecommons/infra/commits/dep/status":
			_, _ = w.Write([]byte(`{"state":"success"}`))
		case "/repos/hivecommons/docs/pulls/7/reviews", "/repos/hivecommons/infra/pulls/3/reviews", "/repos/hivecommons/infra/pulls/4/reviews":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected GitHub API path %s", r.URL.String())
		}
	}))
	defer api.Close()

	feed := NewGitHubActivityFeed(GitHubActivityOptions{
		Org:              "hivecommons",
		APIURL:           api.URL,
		Token:            "fake-token",
		WebhookURL:       discord.URL,
		DataDir:          t.TempDir(),
		FilterBots:       true,
		FilterDependabot: true,
	}, slog.Default())

	if events, err := feed.PollOnce(context.Background()); err != nil {
		t.Fatalf("first PollOnce: %v", err)
	} else if len(events) != 0 {
		t.Fatalf("first poll should seed state without replaying, got %d events", len(events))
	}

	mu.Lock()
	phase = 1
	mu.Unlock()
	events, err := feed.PollOnce(context.Background())
	if err != nil {
		t.Fatalf("second PollOnce: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("second poll events = %d, want 4 (%v)", len(events), events)
	}
	mu.Lock()
	got := append([]string(nil), posted...)
	mu.Unlock()
	assertContainsLine(t, got, "🐝 [hive] Issue #2 opened - factory floor · @someone <https://github.test/hive/2>")
	assertContainsLine(t, got, "🐝 [docs] PR #7 merged into v6 - doc fix · @docwriter <https://github.test/docs/7>")
	assertContainsLine(t, got, "🐝 [docs] PR #8 merged into v5 - fast fix · @fastdev <https://github.test/docs/8>")
	assertContainsLine(t, got, "🐝 [infra] PR #3 merged into v6 - 🌱 Forward-merge v5 into v6 · @forward-bot[bot] <https://github.test/infra/3>")
	for _, line := range got {
		if strings.Contains(line, "dependabot") {
			t.Fatalf("dependabot noise was not filtered: %q", line)
		}
	}

	restarted := NewGitHubActivityFeed(GitHubActivityOptions{
		Org:              "hivecommons",
		APIURL:           api.URL,
		Token:            "fake-token",
		WebhookURL:       discord.URL,
		DataDir:          feed.opts.DataDir,
		FilterBots:       true,
		FilterDependabot: true,
	}, slog.Default())
	if events, err := restarted.PollOnce(context.Background()); err != nil {
		t.Fatalf("restart PollOnce: %v", err)
	} else if len(events) != 0 {
		t.Fatalf("restart replayed %d events, want 0", len(events))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(posted) != 4 {
		t.Fatalf("posted after restart = %d, want 4", len(posted))
	}
}

func assertContainsLine(t *testing.T, lines []string, want string) {
	t.Helper()
	for _, line := range lines {
		if line == want {
			return
		}
	}
	t.Fatalf("missing line %q in %#v", want, lines)
}

func TestGitHubActivityPersistsSuccessfulSendBeforeLaterFailure(t *testing.T) {
	var mu sync.Mutex
	phase := 0
	attempts := 0
	posted := []string{}

	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode discord payload: %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		attempts++
		if attempts == 2 {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		posted = append(posted, payload["content"].(string))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := phase
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/orgs/hivecommons/repos":
			_, _ = w.Write([]byte(`[{"name":"hive"}]`))
		case "/repos/hivecommons/hive/issues":
			_, _ = w.Write([]byte(`[]`))
		case "/repos/hivecommons/hive/pulls":
			if p == 0 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"number":10,"title":"first","state":"open","html_url":"https://github.test/hive/10","updated_at":"2026-09-23T02:00:00Z","user":{"login":"alice"},"head":{"sha":"sha-10"},"base":{"ref":"v5"},"draft":false,"requested_reviewers":[]},{"number":11,"title":"second","state":"open","html_url":"https://github.test/hive/11","updated_at":"2026-09-23T02:00:00Z","user":{"login":"bob"},"head":{"sha":"sha-11"},"base":{"ref":"v5"},"draft":false,"requested_reviewers":[]}]`))
		case "/repos/hivecommons/hive/commits/sha-10/status", "/repos/hivecommons/hive/commits/sha-11/status":
			_, _ = w.Write([]byte(`{"state":"success"}`))
		case "/repos/hivecommons/hive/pulls/10/reviews", "/repos/hivecommons/hive/pulls/11/reviews":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected GitHub API path %s", r.URL.String())
		}
	}))
	defer api.Close()

	feed := NewGitHubActivityFeed(GitHubActivityOptions{
		Org:              "hivecommons",
		APIURL:           api.URL,
		Token:            "fake-token",
		WebhookURL:       discord.URL,
		DataDir:          t.TempDir(),
		FilterBots:       true,
		FilterDependabot: true,
	}, slog.Default())
	if _, err := feed.PollOnce(context.Background()); err != nil {
		t.Fatalf("seed PollOnce: %v", err)
	}
	mu.Lock()
	phase = 1
	mu.Unlock()
	if _, err := feed.PollOnce(context.Background()); err == nil {
		t.Fatal("second PollOnce succeeded; want Discord failure")
	}
	if _, err := feed.PollOnce(context.Background()); err != nil {
		t.Fatalf("retry PollOnce: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	firstPosts := 0
	for _, line := range posted {
		if strings.Contains(line, "PR #10 opened") {
			firstPosts++
		}
	}
	if firstPosts != 1 {
		t.Fatalf("first successful event posted %d times, want 1; posted=%#v", firstPosts, posted)
	}
}

func TestGitHubActivityFactoryPostOmitsWebhookPath(t *testing.T) {
	var phase int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/orgs/hivecommons/repos":
			_, _ = w.Write([]byte(`[{"name":"hive"}]`))
		case "/repos/hivecommons/hive/issues":
			labels := `[]`
			if phase == 1 {
				labels = `[{"name":"hive/help"}]`
			}
			_, _ = w.Write([]byte(`[
				{"number":10,"title":"factory floor","state":"open","html_url":"https://github.test/hive/10","updated_at":"2026-09-23T02:00:00Z","user":{"login":"alice"},"labels":` + labels + `,"assignees":[]}
			]`))
		case "/repos/hivecommons/hive/pulls":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected GitHub API path %s", r.URL.String())
		}
	}))
	defer api.Close()

	webhookURL := "https://discord.com/api/webhooks/123456789012345678/abcdefghijklmnopqrstuvwxyz_0123456789"
	webhookPath := "/api/webhooks/123456789012345678/abcdefghijklmnopqrstuvwxyz_0123456789"
	var logBuf bytes.Buffer
	feed := NewGitHubActivityFeed(GitHubActivityOptions{
		Org:        "hivecommons",
		APIURL:     api.URL,
		Token:      "fake-token",
		WebhookURL: webhookURL,
		DataDir:    t.TempDir(),
	}, slog.New(slog.NewJSONHandler(&logBuf, nil)))
	if _, err := feed.PollOnce(context.Background()); err != nil {
		t.Fatalf("seed PollOnce: %v", err)
	}

	phase = 1
	feed.client = &http.Client{Transport: githubActivityRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "discord.com" {
			return nil, &url.Error{Op: "Post", URL: r.URL.String(), Err: errors.New("dial failed")}
		}
		return http.DefaultTransport.RoundTrip(r)
	})}

	if _, err := feed.PollOnce(context.Background()); err == nil {
		t.Fatal("PollOnce succeeded; want factory post failure")
	} else if strings.Contains(err.Error(), webhookPath) || strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwxyz_0123456789") {
		t.Fatalf("factory post error included webhook path: %v", err)
	}

	feed.pollAndLog(context.Background())
	logged := logBuf.String()
	if logged == "" {
		t.Fatal("expected pollAndLog to write a line")
	}
	if strings.Contains(logged, webhookPath) || strings.Contains(logged, "abcdefghijklmnopqrstuvwxyz_0123456789") {
		t.Fatalf("factory post log included webhook path: %s", logged)
	}
	if !strings.Contains(logged, "discord.com") {
		t.Fatalf("factory post log missing host context: %s", logged)
	}
}

func TestGitHubActivityFactoryStatusBodyIsScrubbed(t *testing.T) {
	webhookURL := "https://discord.com/api/webhooks/123456789012345678/abcdefghijklmnopqrstuvwxyz_0123456789"
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("failed for " + webhookURL))
	}))
	defer discord.Close()

	feed := NewGitHubActivityFeed(GitHubActivityOptions{WebhookURL: discord.URL, DataDir: t.TempDir()}, slog.Default())
	err := feed.postDiscord(context.Background(), "hello")
	if err == nil {
		t.Fatal("postDiscord succeeded; want status error")
	}
	if strings.Contains(err.Error(), webhookURL) || strings.Contains(err.Error(), "abcdefghijklmnopqrstuvwxyz_0123456789") {
		t.Fatalf("status error included webhook path: %v", err)
	}
}

func TestGitHubActivityReloadAppliesEventFilterWithoutReplay(t *testing.T) {
	var mu sync.Mutex
	phase := 0
	posted := []string{}

	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode discord payload: %v", err)
		}
		mu.Lock()
		posted = append(posted, payload["content"].(string))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer discord.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		p := phase
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/orgs/hivecommons/repos":
			_, _ = w.Write([]byte(`[{"name":"hive"}]`))
		case "/repos/hivecommons/hive/issues":
			if p == 0 {
				_, _ = w.Write([]byte(`[]`))
				return
			}
			_, _ = w.Write([]byte(`[{"number":1,"title":"new issue","state":"open","html_url":"https://github.test/hive/1","updated_at":"2026-09-23T02:00:00Z","user":{"login":"alice"},"labels":[],"assignees":[]}]`))
		case "/repos/hivecommons/hive/pulls":
			if p < 2 {
				_, _ = w.Write([]byte(`[{"number":2,"title":"ship it","state":"open","html_url":"https://github.test/hive/2","updated_at":"2026-09-23T02:00:00Z","user":{"login":"bob"},"head":{"sha":"sha-open"},"base":{"ref":"v5"},"draft":false,"requested_reviewers":[]}]`))
				return
			}
			_, _ = w.Write([]byte(`[{"number":2,"title":"ship it","state":"closed","html_url":"https://github.test/hive/2","updated_at":"2026-09-23T03:00:00Z","merged_at":"2026-09-23T03:00:00Z","merge_commit_sha":"sha-merge","user":{"login":"bob"},"head":{"sha":"sha-open"},"base":{"ref":"v5"},"draft":false,"requested_reviewers":[]}]`))
		case "/repos/hivecommons/hive/commits/sha-open/status":
			_, _ = w.Write([]byte(`{"state":"success"}`))
		case "/repos/hivecommons/hive/pulls/2/reviews":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Fatalf("unexpected GitHub API path %s", r.URL.String())
		}
	}))
	defer api.Close()

	dataDir := t.TempDir()
	feed := NewGitHubActivityFeed(GitHubActivityOptions{Org: "hivecommons", APIURL: api.URL, Token: "fake", WebhookURL: discord.URL, DataDir: dataDir, Events: []string{"issue_opened"}}, slog.Default())
	if _, err := feed.PollOnce(context.Background()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	mu.Lock()
	phase = 1
	mu.Unlock()
	if _, err := feed.PollOnce(context.Background()); err != nil {
		t.Fatalf("issue poll: %v", err)
	}
	reloaded := NewGitHubActivityFeed(GitHubActivityOptions{Org: "hivecommons", APIURL: api.URL, Token: "fake", WebhookURL: discord.URL, DataDir: dataDir, Events: []string{"pr_merged"}}, slog.Default())
	mu.Lock()
	phase = 2
	mu.Unlock()
	if _, err := reloaded.PollOnce(context.Background()); err != nil {
		t.Fatalf("reloaded poll: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), posted...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("posted = %#v, want exactly issue then merged PR", got)
	}
	if !strings.Contains(got[0], "Issue #1 opened") || !strings.Contains(got[1], "PR #2 merged") {
		t.Fatalf("unexpected posts after reload: %#v", got)
	}
}
