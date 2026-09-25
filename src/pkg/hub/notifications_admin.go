package hub

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

var factoryActivityEvents = []string{
	"issue_opened", "issue_closed", "issue_reopened", "issue_claimed", "issue_released",
	"pr_opened", "pr_ready_for_review", "pr_review", "pr_ci_status", "pr_merged", "pr_closed", "hourly_digest",
}

type hubNotificationsPayload struct {
	FactoryWebhook string   `json:"factoryWebhook"`
	HasFactory     bool     `json:"hasFactoryWebhook"`
	Enabled        bool     `json:"enabled"`
	RepoScope      string   `json:"repoScope"`
	Repos          []string `json:"repos"`
	Events         []string `json:"events"`
	FilterBots     bool     `json:"filterBots"`
}

func (s *HubServer) SetHubConfigPath(configPath, envToken string) {
	if s == nil {
		return
	}
	s.configPath = configPath
	s.envGitHubToken = envToken
}

func (s *HubServer) ReloadGitHubActivityFromConfig(logger *slog.Logger) error {
	if s == nil || strings.TrimSpace(s.configPath) == "" {
		return nil
	}
	cfg, err := config.LoadWithDashboardOverlayForHub(s.configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.SetGitHubActivityFeed(nil)
			return nil
		}
		return err
	}
	activity := cfg.Notifications.GitHubActivity
	if activity == nil || !activity.Enabled {
		s.SetGitHubActivityFeed(nil)
		return nil
	}
	if cfg.Notifications.Discord == nil || cfg.Notifications.Discord.FactoryWebhook == "" {
		s.SetGitHubActivityFeed(nil)
		return fmt.Errorf("notifications.discord.factory_webhook is not configured")
	}
	token := cfg.GitHub.Token
	if token == "" {
		token = s.envGitHubToken
	}
	if token == "" {
		s.SetGitHubActivityFeed(nil)
		return fmt.Errorf("no GitHub token configured")
	}
	apiURL := activity.APIURL
	if apiURL == "" {
		apiURL = cfg.GitHub.ResolvedAPIURL()
	}
	pollInterval := time.Duration(activity.PollIntervalS) * time.Second
	s.SetGitHubActivityFeed(NewGitHubActivityFeed(GitHubActivityOptions{
		Org:              activity.EffectiveOrg(cfg.Project.Org),
		APIURL:           apiURL,
		Token:            token,
		WebhookURL:       cfg.Notifications.Discord.FactoryWebhook,
		DataDir:          filepath.Dir(cfg.Data.AgentsDir),
		PollInterval:     pollInterval,
		Repos:            activity.Repos,
		Events:           activity.Events,
		AllowAuthors:     activity.AllowAuthors,
		DenyAuthors:      activity.DenyAuthors,
		FilterBots:       activity.BotsFiltered(),
		FilterDependabot: activity.DependabotFiltered(),
	}, logger))
	return nil
}

func (s *HubServer) handleGetAdminNotifications(w http.ResponseWriter, r *http.Request) {
	payload, err := s.loadAdminNotificationsPayload()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeHubJSON(w, payload)
}

func (s *HubServer) handlePutAdminNotifications(w http.ResponseWriter, r *http.Request) {
	var body hubNotificationsPayload
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid body", http.StatusBadRequest)
		return
	}
	if err := validateHubNotificationURL(body.FactoryWebhook, "factoryWebhook"); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if body.Enabled && len(body.Events) == 0 {
		http.Error(w, "at least one event must be selected when notifications are enabled", http.StatusBadRequest)
		return
	}
	cfg, err := config.LoadWithDashboardOverlayForHub(s.configPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg.Notifications.Discord == nil {
		cfg.Notifications.Discord = &config.DiscordConfig{}
	}
	if body.FactoryWebhook != "" && !strings.HasPrefix(body.FactoryWebhook, "•") {
		cfg.Notifications.Discord.FactoryWebhook = body.FactoryWebhook
	}
	if cfg.Notifications.GitHubActivity == nil {
		cfg.Notifications.GitHubActivity = &config.GitHubActivityConfig{}
	}
	activity := cfg.Notifications.GitHubActivity
	activity.Enabled = body.Enabled
	activity.Events = sanitizeEventList(body.Events, factoryActivityEvents)
	activity.FilterBots = boolPtr(body.FilterBots)
	activity.FilterDependabot = boolPtr(body.FilterBots)
	if body.RepoScope == "all" {
		activity.Repos = nil
	} else {
		activity.Repos = sanitizeStrings(body.Repos)
	}
	if err := cfg.Save(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := s.ReloadGitHubActivityFromConfig(s.logger); err != nil && body.Enabled {
		s.logger.Warn("github activity hot reload failed", "error", err)
	}
	payload, _ := s.loadAdminNotificationsPayload()
	writeHubJSON(w, payload)
}

func (s *HubServer) loadAdminNotificationsPayload() (hubNotificationsPayload, error) {
	cfg, err := config.LoadWithDashboardOverlayForHub(s.configPath)
	if err != nil {
		return hubNotificationsPayload{}, err
	}
	p := hubNotificationsPayload{RepoScope: "all", FilterBots: true, Events: append([]string(nil), factoryActivityEvents...)}
	if cfg.Notifications.Discord != nil {
		p.HasFactory = cfg.Notifications.Discord.FactoryWebhook != ""
		if p.HasFactory {
			p.FactoryWebhook = "••••••••"
		}
	}
	if a := cfg.Notifications.GitHubActivity; a != nil {
		p.Enabled = a.Enabled
		if len(a.Repos) > 0 {
			p.RepoScope = "selected"
			p.Repos = append([]string(nil), a.Repos...)
		}
		if len(a.Events) > 0 {
			p.Events = append([]string(nil), a.Events...)
		}
		p.FilterBots = a.BotsFiltered()
	}
	return p, nil
}

func sanitizeEventList(in, allowed []string) []string {
	allowedSet := map[string]bool{}
	for _, e := range allowed {
		allowedSet[e] = true
	}
	seen := map[string]bool{}
	out := []string{}
	for _, e := range in {
		e = strings.TrimSpace(e)
		if allowedSet[e] && !seen[e] {
			seen[e] = true
			out = append(out, e)
		}
	}
	return out
}

func sanitizeStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func boolPtr(v bool) *bool { return &v }

func validateHubNotificationURL(rawURL, fieldName string) error {
	if rawURL == "" || strings.HasPrefix(rawURL, "•") {
		return nil
	}
	if !strings.HasPrefix(rawURL, "https://") {
		return fmt.Errorf("%s must start with https:// or be empty", fieldName)
	}
	host := strings.TrimPrefix(rawURL, "https://")
	if idx := strings.IndexAny(host, ":/"); idx >= 0 {
		host = host[:idx]
	}
	host = strings.ToLower(host)
	for _, p := range []string{"localhost", "127.", "10.", "172.16.", "172.17.", "172.18.", "172.19.", "172.20.", "172.21.", "172.22.", "172.23.", "172.24.", "172.25.", "172.26.", "172.27.", "172.28.", "172.29.", "172.30.", "172.31.", "192.168.", "169.254.", "[::1]", "0.0.0.0"} {
		if strings.HasPrefix(host, p) {
			return fmt.Errorf("%s must not target private/internal addresses", fieldName)
		}
	}
	return nil
}

func writeHubJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
