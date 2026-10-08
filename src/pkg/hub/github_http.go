package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	hubGitHubAppIDEnv             = "HIVE_HUB_GITHUB_APP_ID"
	hubGitHubInstallationIDEnv    = "HIVE_HUB_GITHUB_INSTALLATION_ID"
	hubGitHubAppKeyFileEnv        = "HIVE_HUB_GITHUB_APP_KEY_FILE"
	hubGitHubIdentityLogTimeout   = 10 * time.Second
	hubGitHubRateLimitHTTPTimeout = 10 * time.Second
)

func hubGitHubHTTPClient() *http.Client {
	return &http.Client{Transport: hgithub.NewHTTPTransport(nil)}
}

func hubGitHubCallerContext(parent context.Context, caller string) context.Context {
	if parent == nil {
		parent = context.Background()
	}
	return hgithub.WithRESTCaller(parent, "hub:"+caller)
}

type hubGitHubAppEnv struct {
	appID          string
	installationID string
	keyFile        string
}

func readHubGitHubAppEnv() hubGitHubAppEnv {
	return hubGitHubAppEnv{
		appID:          strings.TrimSpace(os.Getenv(hubGitHubAppIDEnv)),
		installationID: strings.TrimSpace(os.Getenv(hubGitHubInstallationIDEnv)),
		keyFile:        strings.TrimSpace(os.Getenv(hubGitHubAppKeyFileEnv)),
	}
}

func (e hubGitHubAppEnv) configured() bool {
	return e.appID != "" && e.installationID != "" && e.keyFile != ""
}

func (e hubGitHubAppEnv) cacheKey() string {
	return e.appID + "\x00" + e.installationID + "\x00" + e.keyFile + "\x00" + githubAPIBase
}

var hubGitHubAppAuthCache = struct {
	sync.Mutex
	key  string
	auth *hgithub.AppAuth
	err  error
}{}

func hubGitHubAppAuth(logger *slog.Logger) (*hgithub.AppAuth, error) {
	env := readHubGitHubAppEnv()
	if !env.configured() {
		return nil, nil
	}
	key := env.cacheKey()

	hubGitHubAppAuthCache.Lock()
	defer hubGitHubAppAuthCache.Unlock()
	if hubGitHubAppAuthCache.key == key {
		return hubGitHubAppAuthCache.auth, hubGitHubAppAuthCache.err
	}

	appID, err := strconv.ParseInt(env.appID, 10, 64)
	if err != nil {
		hubGitHubAppAuthCache.key = key
		hubGitHubAppAuthCache.auth = nil
		hubGitHubAppAuthCache.err = fmt.Errorf("%s: %w", hubGitHubAppIDEnv, err)
		return nil, hubGitHubAppAuthCache.err
	}
	installationID, err := strconv.ParseInt(env.installationID, 10, 64)
	if err != nil {
		hubGitHubAppAuthCache.key = key
		hubGitHubAppAuthCache.auth = nil
		hubGitHubAppAuthCache.err = fmt.Errorf("%s: %w", hubGitHubInstallationIDEnv, err)
		return nil, hubGitHubAppAuthCache.err
	}
	if logger == nil {
		logger = slog.Default()
	}
	auth, err := hgithub.NewAppAuth(appID, installationID, env.keyFile, logger, githubAPIBase)
	hubGitHubAppAuthCache.key = key
	hubGitHubAppAuthCache.auth = auth
	hubGitHubAppAuthCache.err = err
	return auth, err
}

func resetHubGitHubAppAuthForTest() {
	hubGitHubAppAuthCache.Lock()
	hubGitHubAppAuthCache.key = ""
	hubGitHubAppAuthCache.auth = nil
	hubGitHubAppAuthCache.err = nil
	hubGitHubAppAuthCache.Unlock()
}

func hubGitHubAppToken(ctx context.Context, logger *slog.Logger) (string, error) {
	auth, err := hubGitHubAppAuth(logger)
	if err != nil || auth == nil {
		return "", err
	}
	return auth.Token(ctx)
}

func hubGitHubIdentityMode(ctx context.Context, logger *slog.Logger) string {
	if tok, err := hubGitHubAppToken(ctx, logger); err == nil && strings.TrimSpace(tok) != "" {
		return "app"
	}
	if tok := hubGitHubToken(); tok != "" {
		login := hubGitHubResolveTokenLogin(ctx, tok)
		if login == "" {
			login = "unknown"
		}
		return "token:" + maskHubGitHubLogin(login)
	}
	return "anonymous"
}

func logHubGitHubIdentityMode(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithTimeout(hubGitHubCallerContext(context.Background(), "identity"), hubGitHubIdentityLogTimeout)
	defer cancel()
	mode := hubGitHubIdentityMode(ctx, logger)
	if strings.HasPrefix(mode, "token:") {
		logger.Warn("hub GitHub API identity uses HIVE_HUB_GITHUB_TOKEN and shares the operator's personal quota; configure the hub GitHub App env vars and remove the PAT", "mode", mode)
		return
	}
	logger.Info("hub GitHub API identity selected", "mode", mode)
}

func maskHubGitHubLogin(login string) string {
	login = strings.TrimSpace(login)
	if login == "" {
		return "unknown"
	}
	if len(login) <= 2 {
		return strings.Repeat("•", len(login))
	}
	return login[:1] + strings.Repeat("•", len(login)-2) + login[len(login)-1:]
}

func hubGitHubResolveTokenLogin(ctx context.Context, token string) string {
	if strings.TrimSpace(token) == "" {
		return ""
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(githubAPIBase, "/")+"/user", nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := hubGitHubHTTPClient().Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Login)
}

func hubGitHubRateLimits(ctx context.Context) (*hgithub.RateLimitInfo, error) {
	ctx, cancel := context.WithTimeout(hubGitHubCallerContext(ctx, "rate_limits"), hubGitHubRateLimitHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(githubAPIBase, "/")+"/rate_limit", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	resp, err := hubGitHubHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("github rate limit: HTTP %d", resp.StatusCode)
	}
	type rawRateLimitEntry struct {
		Limit     int   `json:"limit"`
		Remaining int   `json:"remaining"`
		Reset     int64 `json:"reset"`
	}
	toEntry := func(raw rawRateLimitEntry) hgithub.RateLimitEntry {
		var reset time.Time
		if raw.Reset > 0 {
			reset = time.Unix(raw.Reset, 0).UTC()
		}
		return hgithub.RateLimitEntry{Limit: raw.Limit, Remaining: raw.Remaining, Reset: reset, ObservedAt: time.Now().UTC()}
	}
	var body struct {
		Resources struct {
			Core    rawRateLimitEntry `json:"core"`
			Search  rawRateLimitEntry `json:"search"`
			GraphQL rawRateLimitEntry `json:"graphql"`
		} `json:"resources"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	hits, misses, entries := hgithub.ETagCacheStats()
	return &hgithub.RateLimitInfo{
		Core:         toEntry(body.Resources.Core),
		Search:       toEntry(body.Resources.Search),
		GraphQL:      toEntry(body.Resources.GraphQL),
		TopConsumers: hgithub.RESTTopConsumers(10),
		ETagCache:    hgithub.ETagCacheInfo{Hits: hits, Misses: misses, Entries: entries},
	}, nil
}
