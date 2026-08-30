package hub

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kubestellar/hive/pkg/config"
	ghauth "github.com/kubestellar/hive/pkg/github"
)

const ghSetupPath = "/gh-setup"

func (s *HubServer) handleGitHubAppSetupRouter(w http.ResponseWriter, r *http.Request) {
	action := strings.TrimSpace(r.URL.Query().Get("setup_action"))
	rawInstallationID := strings.TrimSpace(r.URL.Query().Get("installation_id"))
	if rawInstallationID == "" {
		s.renderGitHubSetupInfo(w, http.StatusOK, "Install requested", "Install requested/received. Your hive will configure itself automatically within a few minutes.")
		return
	}

	installationID, err := strconv.ParseInt(rawInstallationID, 10, 64)
	if err != nil || installationID <= 0 || strconv.FormatInt(installationID, 10) != rawInstallationID {
		s.logger.Warn("rejected GitHub App setup callback with invalid installation_id")
		s.renderGitHubSetupInfo(w, http.StatusBadRequest, "Invalid setup callback", "This GitHub App setup callback could not be routed. Please return to your hive and try the install link again.")
		return
	}

	org, err := s.lookupGitHubInstallationAccount(r.Context(), installationID)
	if err != nil {
		s.logger.Warn("could not resolve GitHub App installation for setup callback", "installation_id", installationID, "setup_action", action, "error", err)
		s.renderGitHubSetupInfo(w, http.StatusOK, "Install received", "Install requested/received. Your hive will configure itself automatically within a few minutes.")
		return
	}

	matches := s.githubSetupMatches(org)
	switch len(matches) {
	case 0:
		s.renderGitHubSetupInfo(w, http.StatusOK, "No matching hive yet", fmt.Sprintf("No hive registered for org %s — auto-discovery will configure the right hive within minutes once it registers.", org))
	case 1:
		target, ok := spokeGitHubSetupURL(matches[0], r.URL.RawQuery)
		if !ok {
			s.logger.Warn("matching hive has no routable dashboard URL for GitHub App setup", "hive", matches[0].ID, "org", org)
			s.renderGitHubSetupInfo(w, http.StatusOK, "Install received", "Install requested/received. Your hive will configure itself automatically within a few minutes.")
			return
		}
		http.Redirect(w, r, target, http.StatusFound)
	default:
		s.renderGitHubSetupChooser(w, matches, r.URL.RawQuery)
	}
}

func (s *HubServer) lookupGitHubInstallationAccount(ctx context.Context, installationID int64) (string, error) {
	if s.githubInstallationAccount != nil {
		return s.githubInstallationAccount(ctx, installationID)
	}
	key := s.appKeysByAppID()[config.PublicGitHubAppID]
	if key.PrivateKey == "" {
		return "", fmt.Errorf("public GitHub App key is not available")
	}
	auth, err := ghauth.NewAppAuthFromPEM(config.PublicGitHubAppID, 0, []byte(key.PrivateKey), s.logger, "")
	if err != nil {
		return "", fmt.Errorf("loading public GitHub App key: %w", err)
	}
	return auth.InstallationAccountLogin(ctx, installationID)
}

func (s *HubServer) githubSetupMatches(org string) []RegistryEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var matches []RegistryEntry
	for _, hive := range s.registry.Hives {
		if !strings.EqualFold(strings.TrimSpace(hive.Org), strings.TrimSpace(org)) {
			continue
		}
		if hive.GitHubAppID != 0 && hive.GitHubAppID != config.PublicGitHubAppID {
			continue
		}
		if hive.GitHubHost != "" && !sameGitHubHost(hive.GitHubHost, publicGitHubHost) {
			continue
		}
		if hive.GitHubBaseURL != "" && !sameGitHubHost(githubHostLabel(hive.GitHubBaseURL), publicGitHubHost) {
			continue
		}
		matches = append(matches, hive)
	}
	return matches
}

func spokeGitHubSetupURL(hive RegistryEntry, rawQuery string) (string, bool) {
	base := strings.TrimSpace(hive.DashboardURL)
	if base == "" {
		base = strings.TrimSpace(hive.SnapshotURL)
	}
	if base == "" {
		return "", false
	}
	if !strings.Contains(base, "://") {
		base = "https://" + base
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", false
	}
	u.Path = ghSetupPath
	u.RawPath = ""
	u.RawQuery = rawQuery
	u.Fragment = ""
	return u.String(), true
}

func (s *HubServer) renderGitHubSetupInfo(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="font-family:system-ui,sans-serif;max-width:720px;margin:3rem auto;padding:0 1rem;line-height:1.5"><h1>%s</h1><p>%s</p></body></html>`,
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(message))
}

func (s *HubServer) renderGitHubSetupChooser(w http.ResponseWriter, matches []RegistryEntry, rawQuery string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, `<!doctype html><html><head><meta charset="utf-8"><title>Choose hive</title><meta name="viewport" content="width=device-width,initial-scale=1"></head><body style="font-family:system-ui,sans-serif;max-width:720px;margin:3rem auto;padding:0 1rem;line-height:1.5"><h1>Choose a hive</h1><p>More than one registered hive matches this GitHub organization. Choose the hive to finish setup.</p><ul>`)
	for _, hive := range matches {
		target, ok := spokeGitHubSetupURL(hive, rawQuery)
		if !ok {
			continue
		}
		label := strings.TrimSpace(hive.ProjectName)
		if label == "" {
			label = strings.TrimSpace(hive.Name)
		}
		if label == "" {
			label = strings.TrimSpace(hive.ID)
		}
		_, _ = fmt.Fprintf(w, `<li><a href="%s">%s</a></li>`, html.EscapeString(target), html.EscapeString(label))
	}
	_, _ = fmt.Fprint(w, `</ul></body></html>`)
}
