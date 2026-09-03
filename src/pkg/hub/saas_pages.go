// Static SaaS page serving (dashboard shell, access denied) and
// the hub broadcast banner endpoints.
package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func isUnfurlBot(ua string) bool {
	bots := []string{"Slackbot", "Slack-ImgProxy", "Discordbot", "Twitterbot", "facebookexternalhit", "LinkedInBot", "WhatsApp", "TelegramBot"}
	for _, b := range bots {
		if strings.Contains(ua, b) {
			return true
		}
	}
	return false
}

const ogFallbackHTML = `<!DOCTYPE html><html><head>
<meta charset="utf-8">
<meta property="og:title" content="My Hives — Hive Hub">
<meta property="og:description" content="AI Agent Orchestration for Open Source. Manage your hive instances — monitor agents, governor mode, issues, PRs, and contributor activity.">
<meta property="og:type" content="website">
<meta property="og:site_name" content="Hive Hub">
<meta property="og:url" content="https://hive.kubestellar.io/dashboard">
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🍯</text></svg>">
<title>My Hives — Hive Hub</title>
</head><body></body></html>`

func (s *HubServer) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if isUnfurlBot(r.UserAgent()) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(ogFallbackHTML))
		return
	}
	cookie, err := r.Cookie("hive_hub_user")
	if err != nil || cookie.Value == "" {
		http.Redirect(w, r, "/login", http.StatusTemporaryRedirect)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprint(w, dashboardHTML)
}

func (s *HubServer) handleAccessDenied(w http.ResponseWriter, r *http.Request) {
	hiveID := sanitize(r.URL.Query().Get("hive"))

	ownerLink := ""
	s.mu.RLock()
	for _, h := range s.registry.Hives {
		if h.ID == hiveID && h.Owner != "" {
			safeOwner := sanitize(h.Owner)
			if safeOwner != "" {
				ownerLink = fmt.Sprintf(`<a href="https://github.com/%s" target="_blank" style="color:#58a6ff;text-decoration:underline">the hive owner</a>`, safeOwner)
			}
			break
		}
	}
	s.mu.RUnlock()
	if ownerLink == "" {
		ownerLink = "the hive owner"
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><meta charset="UTF-8"><title>Access Denied — Hive Hub</title>
<script async src="https://www.googletagmanager.com/gtag/js?id=G-4707R797K3"></script><script>window.dataLayer=window.dataLayer||[];function gtag(){dataLayer.push(arguments)}gtag("js",new Date());gtag("config","G-4707R797K3");gtag("event","access_denied",{hive_id:"%s"});</script>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif;background:#0d1117;color:#e6edf3;display:flex;justify-content:center;align-items:center;min-height:100vh}
.card{background:#161b22;border:1px solid #30363d;border-radius:12px;padding:48px;max-width:520px;text-align:center}
h1{font-size:2rem;margin-bottom:8px}
.bee{font-size:3rem;margin-bottom:16px}
.msg{color:#8b949e;margin-bottom:24px;line-height:1.6}
.hive-name{color:#f0883e;font-family:monospace;font-weight:600}
.btn{display:inline-block;padding:10px 24px;border-radius:8px;text-decoration:none;font-weight:600;font-size:0.9rem;margin:6px}
.btn-primary{background:#238636;color:#fff}
.btn-secondary{background:transparent;color:#58a6ff;border:1px solid #30363d}
.help{color:#8b949e;font-size:0.8rem;margin-top:24px}
</style></head><body>
<div class="card">
<div class="bee">🐝</div>
<h1>Access Denied</h1>
<p class="msg">
You don't have access to
<span class="hive-name">%s</span>.<br><br>
Ask %s to grant you access from their
<a href="/dashboard" style="color:#58a6ff">My Hives</a> dashboard.
</p>
<a href="/dashboard" class="btn btn-primary">Go to My Hives</a>
<a href="/" class="btn btn-secondary">Browse Public Hives</a>
<p class="help">If you believe this is an error, <a href="https://github.com/hivecommons/hive/issues" style="color:#58a6ff">file an issue</a>.</p>
</div>
</body></html>`, hiveID, hiveID, ownerLink)
}

const (
	bannerIDPrefix       = "hub-banner-"
	maxBannerMessageLen  = 500
	maxBannerTargetHives = 100
)

var validBannerColors = map[string]bool{
	"green": true,
	"blue":  true,
	"amber": true,
	"gray":  true,
}

func (s *HubServer) handleSendHubBanner(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Message string   `json:"message"`
		Color   string   `json:"color"`
		HiveIDs []string `json:"hive_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid request body"}`, http.StatusBadRequest)
		return
	}
	body.Message = strings.TrimSpace(body.Message)
	if body.Message == "" {
		http.Error(w, `{"error":"message is required"}`, http.StatusBadRequest)
		return
	}
	if len([]rune(body.Message)) > maxBannerMessageLen {
		http.Error(w, fmt.Sprintf(`{"error":"message exceeds %d characters"}`, maxBannerMessageLen), http.StatusBadRequest)
		return
	}
	if body.Color == "" {
		body.Color = "green"
	}
	if !validBannerColors[body.Color] {
		http.Error(w, `{"error":"invalid color; must be green, blue, amber, or gray"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) == 0 {
		http.Error(w, `{"error":"at least one hive must be selected"}`, http.StatusBadRequest)
		return
	}
	if len(body.HiveIDs) > maxBannerTargetHives {
		http.Error(w, fmt.Sprintf(`{"error":"too many hives (max %d)"}`, maxBannerTargetHives), http.StatusBadRequest)
		return
	}

	bannerID := fmt.Sprintf("%s%d", bannerIDPrefix, time.Now().UnixMilli())
	now := time.Now().UTC().Format(time.RFC3339)
	entry := &HubBannerEntry{
		ID:      bannerID,
		Message: body.Message,
		Color:   body.Color,
		SentAt:  now,
	}

	s.hubBannersMu.Lock()
	for _, hiveID := range body.HiveIDs {
		s.hubBanners[hiveID] = entry
	}
	s.hubBannersMu.Unlock()
	// Persist so the banner survives a hub restart/upgrade (the pod roll would
	// otherwise wipe the in-memory map and silently drop it).
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banner sent",
		"banner_id", bannerID,
		"message", body.Message,
		"color", body.Color,
		"hive_count", len(body.HiveIDs),
		"by", username,
	)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"banner_id":%q,"hive_count":%d}`, bannerID, len(body.HiveIDs))
}

func (s *HubServer) handleClearHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.Lock()
	count := len(s.hubBanners)
	s.hubBanners = make(map[string]*HubBannerEntry)
	s.hubBannersMu.Unlock()
	// Persist the cleared (empty) state so banners stay gone across a restart.
	s.saveHubBanners()

	username := s.getAuthUser(r)
	s.logger.Info("hub banners cleared", "cleared_count", count, "by", username)

	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"ok":true,"cleared":%d}`, count)
}

func (s *HubServer) handleGetHubBanner(w http.ResponseWriter, r *http.Request) {
	s.hubBannersMu.RLock()
	defer s.hubBannersMu.RUnlock()

	type bannerStatus struct {
		HiveID  string `json:"hive_id"`
		ID      string `json:"id"`
		Message string `json:"message"`
		Color   string `json:"color"`
		SentAt  string `json:"sent_at"`
	}
	var banners []bannerStatus
	for hiveID, entry := range s.hubBanners {
		banners = append(banners, bannerStatus{
			HiveID:  hiveID,
			ID:      entry.ID,
			Message: entry.Message,
			Color:   entry.Color,
			SentAt:  entry.SentAt,
		})
	}
	if banners == nil {
		banners = []bannerStatus{}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"banners": banners})
}
