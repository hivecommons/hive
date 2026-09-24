package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

const repoHoldPermissionCacheTTL = 2 * time.Minute

type repoHoldRequest struct {
	Held bool `json:"held"`
}

type repoHoldPermissionCacheEntry struct {
	allowed bool
	at      time.Time
}

func (s *Server) canonicalHoldLabel() string {
	if s == nil || s.deps == nil || s.deps.Config == nil {
		return github.CanonicalHiveHoldLabel("")
	}
	return github.CanonicalHiveHoldLabel(s.deps.Config.HiveID)
}

func repoHoldKey(owner, repo, user string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo) + "\x00" + strings.ToLower(user)
}

func repoHoldPermissionLevelAllows(level string) bool {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "admin", "maintain", "write":
		return true
	default:
		return false
	}
}

func (s *Server) canToggleRepoHold(r *http.Request, owner, repo string) (bool, int, string) {
	if isOwnerRole(r.Header.Get("X-Hive-Role")) && r.Header.Get(ownerRoleVerifiedHeader) == "true" {
		return true, http.StatusOK, ""
	}
	user := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if user == "" {
		return false, http.StatusUnauthorized, "GitHub user session required"
	}
	if strings.EqualFold(owner, user) {
		return true, http.StatusOK, ""
	}
	if s == nil || s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		return false, http.StatusServiceUnavailable, "GitHub client not configured"
	}
	key := repoHoldKey(owner, repo, user)
	now := time.Now()
	s.repoHoldPermMu.Lock()
	if s.repoHoldPermCache == nil {
		s.repoHoldPermCache = make(map[string]repoHoldPermissionCacheEntry)
	}
	if ent, ok := s.repoHoldPermCache[key]; ok && now.Sub(ent.at) < repoHoldPermissionCacheTTL {
		s.repoHoldPermMu.Unlock()
		if ent.allowed {
			return true, http.StatusOK, ""
		}
		return false, http.StatusForbidden, "write permission required"
	}
	s.repoHoldPermMu.Unlock()

	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	ctx, cancel := context.WithTimeout(ctx, repoPermissionTimeout)
	defer cancel()
	level, _, err := s.deps.GHClient.GoGitHub().Repositories.GetPermissionLevel(ctx, owner, repo, user)
	if err != nil {
		if s.logger != nil {
			s.logger.Warn("repo hold permission lookup failed", "repo", owner+"/"+repo, "user", user, "error", err)
		}
		return false, http.StatusForbidden, "write permission required"
	}
	allowed := repoHoldPermissionLevelAllows(level.GetPermission())
	s.repoHoldPermMu.Lock()
	s.repoHoldPermCache[key] = repoHoldPermissionCacheEntry{allowed: allowed, at: now}
	s.repoHoldPermMu.Unlock()
	if !allowed {
		return false, http.StatusForbidden, "write permission required"
	}
	return true, http.StatusOK, ""
}

func (s *Server) parseRepoItemPath(w http.ResponseWriter, r *http.Request) (owner, repo string, number int, ok bool) {
	owner = strings.TrimSpace(r.PathValue("owner"))
	repo = strings.TrimSpace(r.PathValue("repo"))
	n, err := strconv.Atoi(r.PathValue("number"))
	if owner == "" || repo == "" || strings.Contains(owner, "/") || strings.Contains(repo, "/") || err != nil || n <= 0 {
		jsonError(w, "invalid repository item", http.StatusBadRequest)
		return "", "", 0, false
	}
	return owner, repo, n, true
}

func holdCausingLabels(labels []string, canonical string) []string {
	out := make([]string, 0, len(labels))
	seen := make(map[string]bool)
	for _, label := range labels {
		trimmed := strings.TrimSpace(label)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		if strings.EqualFold(trimmed, canonical) || github.HasHoldLabel([]string{trimmed}) {
			if !seen[lower] {
				seen[lower] = true
				out = append(out, trimmed)
			}
		}
	}
	return out
}

func (s *Server) currentItemLabels(ctx context.Context, owner, repo string, number int) ([]string, error) {
	issue, _, err := s.deps.GHClient.GoGitHub().Issues.Get(ctx, owner, repo, number)
	if err != nil {
		return nil, err
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		labels = append(labels, label.GetName())
	}
	return labels, nil
}

func (s *Server) handleRepoHoldPermission(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.PathValue("owner"))
	repo := strings.TrimSpace(r.PathValue("repo"))
	if owner == "" || repo == "" || strings.Contains(owner, "/") || strings.Contains(repo, "/") {
		jsonError(w, "invalid repository", http.StatusBadRequest)
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repo)
	if !allowed && status != http.StatusForbidden {
		jsonError(w, msg, status)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "allowed": allowed})
}

func (s *Server) handleRepoItemHold(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	allowed, status, msg := s.canToggleRepoHold(r, owner, repoName)
	if !allowed {
		jsonError(w, msg, status)
		return
	}
	if s.deps == nil || s.deps.GHClient == nil {
		jsonError(w, "GitHub client not configured", http.StatusServiceUnavailable)
		return
	}
	var body repoHoldRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	ctx := context.Background()
	if s.deps.Ctx != nil {
		ctx = s.deps.Ctx
	}
	canonical := s.canonicalHoldLabel()
	repoFull := owner + "/" + repoName
	var removed []string
	if body.Held {
		if err := s.deps.GHClient.EnsureIssueLabel(ctx, repoFull, canonical, "8250df", "Hive hold: agents will not act on this item until the label is removed."); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		if err := s.deps.GHClient.AddLabels(ctx, repoFull, number, []string{canonical}); err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		s.auditFromRequest(r, "repo_item_hold_add", auditDetail("repo", repoFull, "number", strconv.Itoa(number), "label", canonical), "")
	} else {
		labels, err := s.currentItemLabels(ctx, owner, repoName, number)
		if err != nil {
			jsonError(w, err.Error(), http.StatusBadGateway)
			return
		}
		removed = holdCausingLabels(labels, canonical)
		for _, label := range removed {
			if err := s.deps.GHClient.RemoveLabel(ctx, repoFull, number, label); err != nil {
				jsonError(w, err.Error(), http.StatusBadGateway)
				return
			}
		}
		s.auditFromRequest(r, "repo_item_hold_remove", auditDetail("repo", repoFull, "number", strconv.Itoa(number), "labels", strings.Join(removed, ",")), "")
	}
	jsonResponse(w, map[string]any{
		"ok":         true,
		"repo":       repoFull,
		"number":     number,
		"held":       body.Held,
		"hold_label": canonical,
		"removed":    removed,
	})
}
