package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Per-repo agent pause (#6203).
//
// An operator could already stop one AGENT everywhere, or stop EVERYTHING with
// the fleet breaker. There was no way to say "quiet this one repo, leave the
// rest of the hive running" — so a release freeze, an incident, or a repo whose
// CI is red for an unrelated reason cost the whole fleet, or cost the repo its
// place in project.repos.
//
// These two handlers are the dashboard half of that. The enforcement half is
// deliberately elsewhere and deterministic: the MITM proxy refuses agent writes
// to a paused repo, the hive-open-pr / hive-merge relays refuse to fulfil one,
// and work enumeration produces nothing for it. Nothing here relies on an agent
// reading a prompt and choosing to comply — a prompt-only pause has already
// been observed to fail when an agent's model changes.

// repoPauseRequest is the body of POST /api/repos/pause and /api/repos/resume.
//
// The repo travels in the BODY rather than a path segment because a repos entry
// may be an explicit cross-org reference ("laredo/cuga-agent"), and a
// slash-bearing name cannot be a single {repo} path value.
type repoPauseRequest struct {
	Repo   string `json:"repo"`
	Reason string `json:"reason,omitempty"`
}

// RepoPauseState is one repo's pause as the dashboard reports it.
type RepoPauseState struct {
	Repo   string `json:"repo"`
	Paused bool   `json:"paused"`
	By     string `json:"by,omitempty"`
	At     string `json:"at,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// repoPauseStateOf renders a config.RepoPause for the API, with At as RFC3339
// (or empty when the pause was hand-written into the config and carries no
// timestamp — "unknown" and "the zero time" must stay distinguishable).
func repoPauseStateOf(repo string, rp config.RepoPause, paused bool) RepoPauseState {
	out := RepoPauseState{Repo: repo, Paused: paused}
	if !paused {
		return out
	}
	out.By = rp.By
	out.Reason = rp.Reason
	if rp.At != nil && !rp.At.IsZero() {
		out.At = rp.At.UTC().Format(time.RFC3339)
	}
	return out
}

// repoPauseToggleResponse mirrors pauseToggleResponse for agents: `changed`
// distinguishes a real transition from a no-op so a dashboard with a stale
// belief cannot silently re-pause a repo the operator was trying to resume, and
// `state` is the authoritative post-request value the client must render from.
//
// `persisted` is the one field the agent version does not need. A repo pause
// takes effect in memory immediately — every enforcement point reads live
// config — so a failed write to a read-only config mount leaves the pause
// working but not surviving a restart. That is a real and different state, and
// saying "ok" without it would be a lie in the direction that matters.
func repoPauseToggleResponse(w http.ResponseWriter, status string, changed bool, state RepoPauseState, persisted bool, warning string) {
	body := map[string]any{
		"ok":        true,
		"status":    status,
		"repo":      state.Repo,
		"changed":   changed,
		"state":     pauseStateLabel(state.Paused),
		"pause":     state,
		"persisted": persisted,
	}
	if warning != "" {
		body["warning"] = warning
	}
	jsonResponse(w, body)
}

// decodeRepoPauseRequest reads and validates the request body, answering the
// client directly on failure. The bool reports whether the caller may continue.
func decodeRepoPauseRequest(w http.ResponseWriter, r *http.Request) (repoPauseRequest, bool) {
	var body repoPauseRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&body); err != nil {
		jsonError(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return body, false
	}
	body.Repo = strings.TrimSpace(body.Repo)
	body.Reason = strings.TrimSpace(body.Reason)
	if body.Repo == "" {
		jsonError(w, "repo is required", http.StatusBadRequest)
		return body, false
	}
	return body, true
}

// watchesRepo reports whether repo is one this hive lists in project.repos,
// comparing org-qualified and case-folded so "Console", "console" and
// "acme/console" all resolve to the same entry.
func (s *Server) watchesRepo(repo string) bool {
	cfg := s.deps.Config
	if cfg == nil {
		return false
	}
	want := strings.ToLower(config.QualifyRepo(cfg.Project.Org, repo))
	for _, watched := range cfg.Project.Repos {
		if strings.ToLower(config.QualifyRepo(cfg.Project.Org, watched)) == want {
			return true
		}
	}
	return false
}

// handleRepoPause quiets one repository: agents stop writing to it and stop
// being handed work on it, while the repo keeps its dashboard card and its ACMM
// evaluation. Owner-gated, like the agent pause it is modelled on.
func (s *Server) handleRepoPause(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	body, ok := decodeRepoPauseRequest(w, r)
	if !ok {
		return
	}
	// Refuse to pause a repo the hive does not watch. Accepting it would write
	// an entry that matches nothing and report success — the operator would
	// believe a repo was quiet when no enforcement point had ever heard of it.
	if !s.watchesRepo(body.Repo) {
		jsonError(w, "repo "+body.Repo+" is not in project.repos — nothing would be paused", http.StatusBadRequest)
		return
	}

	cfg := s.deps.Config
	// No-op guard, mirroring handlePause: re-pausing must NOT restamp the
	// original who/when/why. A stale dashboard re-issuing a pause would
	// otherwise quietly rewrite the record of why this repo has been quiet
	// since Tuesday, which is the provenance failure #4041 was about.
	if existing, paused := cfg.RepoPauseFor(body.Repo); paused {
		s.auditFromRequest(r, "repo_pause", auditDetail("repo", body.Repo, "result", "noop-already-paused"), "")
		repoPauseToggleResponse(w, "paused", false, repoPauseStateOf(body.Repo, existing, true), true, "")
		return
	}

	changed, err := cfg.SetRepoPausedAndSave(body.Repo, true, requestUser(r), body.Reason)
	if err != nil {
		s.persistRepoPauseFailure(body.Repo, "paused", err)
	}
	state, _ := cfg.RepoPauseFor(body.Repo)
	s.auditFromRequest(r, "repo_pause", auditDetail("repo", body.Repo, "reason", body.Reason), "")
	s.refreshAndPersist()
	repoPauseToggleResponse(w, "paused", changed, repoPauseStateOf(body.Repo, state, true), err == nil, repoPausePersistWarning(err))
}

// handleRepoResume lifts a repository's pause.
//
// Unlike pause, this does NOT require the repo to be in project.repos: an entry
// left behind by a repo that was removed while paused must still be clearable,
// or the only way to tidy it is to hand-edit the config file.
func (s *Server) handleRepoResume(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	body, ok := decodeRepoPauseRequest(w, r)
	if !ok {
		return
	}

	cfg := s.deps.Config
	if _, paused := cfg.RepoPauseFor(body.Repo); !paused {
		s.auditFromRequest(r, "repo_resume", auditDetail("repo", body.Repo, "result", "noop-not-paused"), "")
		repoPauseToggleResponse(w, "resumed", false, RepoPauseState{Repo: body.Repo}, true, "")
		return
	}

	changed, err := cfg.SetRepoPausedAndSave(body.Repo, false, requestUser(r), "")
	if err != nil {
		s.persistRepoPauseFailure(body.Repo, "resumed", err)
	}
	s.auditFromRequest(r, "repo_resume", auditDetail("repo", body.Repo), "")
	s.refreshAndPersist()
	repoPauseToggleResponse(w, "resumed", changed, RepoPauseState{Repo: body.Repo}, err == nil, repoPausePersistWarning(err))
}

// handleRepoPauses lists every repo pause with its provenance. Read-only, so
// any authenticated role may call it — the same state is already visible on the
// repository cards.
func (s *Server) handleRepoPauses(w http.ResponseWriter, r *http.Request) {
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	cfg := s.deps.Config
	out := make([]RepoPauseState, 0, len(cfg.PausedRepoNames()))
	for _, repo := range cfg.PausedRepoNames() {
		rp, paused := cfg.RepoPauseFor(repo)
		if !paused {
			continue
		}
		out = append(out, repoPauseStateOf(repo, rp, true))
	}
	jsonResponse(w, map[string]any{"ok": true, "pauses": out})
}

// repoPausePersistWarning turns a save failure into operator-facing prose. The
// pause itself is already in force; only its survival across a restart is at
// risk, and the message says exactly that rather than implying nothing happened.
func repoPausePersistWarning(err error) string {
	if err == nil {
		return ""
	}
	return "the change is in effect now but could not be written to the config — it will not survive a restart: " + err.Error()
}

func (s *Server) persistRepoPauseFailure(repo, verb string, err error) {
	if s.deps != nil && s.deps.Logger != nil {
		s.deps.Logger.Error("failed to persist repo pause state", "repo", repo, "state", verb, "error", err)
	}
	s.AddSystemAlert("repo-pause-save-failed", "error",
		"Could not record that you "+verb+" "+repo+" — the change is active now but will be lost on the next restart: "+err.Error())
}
