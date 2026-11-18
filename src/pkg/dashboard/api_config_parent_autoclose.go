package dashboard

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// The per-repo parent auto-close policy (repos.<repo>.parent_autoclose,
// hivecommons/hive#18869) editor on the Settings Repos tab
// (hivecommons/hive#19070).
//
// The policy decides whether the issue-stage pass closes a decomposed parent
// once every child is closed. Both endpoints are OWNER-ONLY: the value changes
// which issues the hive closes on its own. A PUT is validated before anything
// changes, applies without a restart (the stage pass reads the live config
// through ParentAutoCloseFor), is audited, and is rolled back when the config
// file cannot be written.
//
// Tracking issues (hive/tracking label, <!-- hive:tracking --> marker, "Keep
// this issue open") and parents a person reopened are never auto-closed under
// any value; the editor says so, and nothing here can change that.

// parentAutoCloseOption is one choice the editor offers.
type parentAutoCloseOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description"`
}

// parentAutoCloseRepo is one monitored repo and its policy.
type parentAutoCloseRepo struct {
	Repo string `json:"repo"`
	// Value is the effective policy: "true", "false" or "epics_too".
	Value string `json:"value"`
	// Explicit is true when the repo carries its own value, false when it
	// inherits the default.
	Explicit bool `json:"explicit"`
}

// parentAutoCloseResponse is the GET/PUT response body.
type parentAutoCloseResponse struct {
	// Default is the policy a repo with no value gets.
	Default string                  `json:"default"`
	Options []parentAutoCloseOption `json:"options"`
	// Repos lists every monitored repo (project.repos), in config order.
	Repos []parentAutoCloseRepo `json:"repos"`
}

// parentAutoCloseOptions is the vocabulary the editor offers, in display
// order. The wording is the contract the Settings card and the docs share.
var parentAutoCloseOptions = []parentAutoCloseOption{
	{config.ParentAutoCloseOn, "Close finished parents (default)",
		"Close a split-up parent issue once all of its children are closed, except parents labelled epic or hold."},
	{config.ParentAutoCloseEpicsToo, "Also close epics and holds",
		"As above, and also close parents labelled epic or hold when all of their children are closed."},
	{config.ParentAutoCloseOff, "Never auto-close parents",
		"The hive never closes a parent issue itself; a person closes it."},
}

func parentAutoCloseResponseFor(cfg *config.Config) parentAutoCloseResponse {
	out := parentAutoCloseResponse{
		Default: config.ParentAutoCloseOn,
		Options: parentAutoCloseOptions,
		Repos:   make([]parentAutoCloseRepo, 0, len(cfg.Project.Repos)),
	}
	for _, repo := range cfg.Project.Repos {
		repo = strings.TrimSpace(repo)
		if repo == "" {
			continue
		}
		out.Repos = append(out.Repos, parentAutoCloseRepo{
			Repo:     repo,
			Value:    cfg.ParentAutoCloseFor(repo),
			Explicit: cfg.ParentAutoCloseExplicit(repo),
		})
	}
	return out
}

// handleParentAutoCloseGet returns the policy of every monitored repo.
func (s *Server) handleParentAutoCloseGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, parentAutoCloseResponseFor(s.deps.Config))
}

// handleParentAutoClosePut sets one monitored repo's policy. Body:
// {"repo": "owner/name", "value": "true" | "epics_too" | "false"}. Choosing
// "true" (the default) clears the repo's override.
func (s *Server) handleParentAutoClosePut(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Repo  string `json:"repo"`
		Value string `json:"value"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	body.Repo = strings.TrimSpace(body.Repo)
	if body.Repo == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config

	// --- validate before mutating anything ---
	if _, ok := config.NormalizeParentAutoClose(body.Value); !ok {
		jsonError(w, `value must be "true", "epics_too" or "false"`, http.StatusBadRequest)
		return
	}
	previous := cfg.ParentAutoCloseFor(body.Repo)
	prevEntry, hadEntry := cfg.RepoSettingsFor(body.Repo)

	// --- apply ---
	if err := cfg.SetParentAutoClose(body.Repo, body.Value); err != nil {
		if errors.Is(err, config.ErrRepoNotMonitored) {
			jsonError(w, err.Error(), http.StatusNotFound)
			return
		}
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.saveConfig(); err != nil {
		// Memory must not diverge from the file: put the old value back.
		restoreParentAutoClose(cfg, body.Repo, previous, prevEntry, hadEntry)
		if s.logger != nil {
			s.logger.Error("failed to persist config after parent auto-close update", "error", err)
		}
		jsonError(w, "failed to save config; parent auto-close unchanged", http.StatusInternalServerError)
		return
	}
	effective := cfg.ParentAutoCloseFor(body.Repo)
	s.auditFromRequest(r, "config_repo_parent_autoclose", auditDetail(
		"section", "repos",
		"repo", body.Repo,
		"previous", previous,
		"value", effective,
	), "")
	if s.logger != nil {
		s.logger.Info("parent auto-close updated via runtime settings",
			"repo", body.Repo, "previous", previous, "value", effective)
	}
	jsonResponse(w, parentAutoCloseResponseFor(cfg))
}

// restoreParentAutoClose puts a repo's previous parent_autoclose value back
// after a failed save. A repo that had an explicit non-default value gets it
// back; one that inherited the default goes back to inheriting it.
func restoreParentAutoClose(cfg *config.Config, repo, previous string, prev config.RepoSettings, had bool) {
	if had && prev.ParentAutoClose != "" {
		if norm, ok := config.NormalizeParentAutoClose(prev.ParentAutoClose); ok {
			previous = norm
		}
	}
	_ = cfg.SetParentAutoClose(repo, previous)
}
