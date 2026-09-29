package dashboard

import (
	"net/http"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/questionclose"
)

// Question auto-close (hivecommons/hive#9584) dashboard surface — the
// deferred follow-up the merged PR (#9599) left for later: a Settings
// switch + hours field for governor.question_autoclose, and a read-only
// view of the live schedule questionclose.Manager is holding in memory.
//
// This mirrors the stall-replan lane's tab (handleGovernorReplanGet/Put +
// replanSectionResponse in api_governor_features.go): a small standalone
// GET/PUT pair under its own config key, rather than folding into the large
// governor.features blob, because question_autoclose is already its own
// top-level governor.question_autoclose config block.

// minQuestionAutocloseHours is the smallest objection window an operator may
// set from the dashboard. 0 or negative would mean "close immediately",
// which defeats the whole point of giving the author a chance to react.
const minQuestionAutocloseHours = 1

// handleGovernorQuestionAutocloseGet returns governor.question_autoclose so
// the Governor dialog can prefill its Features tab controls. OWNER-ONLY,
// matching the rest of the governor-config surface (F16).
func (s *Server) handleGovernorQuestionAutocloseGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, questionAutocloseSectionResponse(s.deps.Config))
}

// handleGovernorQuestionAutocloseSchedule is a read-only view of the live
// in-memory schedule questionclose.Manager is holding — one row per issue
// the hive has answered and is waiting out the objection window for. It
// carries no secrets and no write path; requireOwnerRole still gates it
// because the schedule reveals which issues and repos the hive is watching,
// consistent with every other governor-config read.
//
// A nil Manager (feature off, or not yet started this process) renders as
// an empty schedule rather than an error — matches the "off = quietly does
// nothing" contract the rest of question_autoclose follows.
func (s *Server) handleGovernorQuestionAutocloseSchedule(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	jsonResponse(w, questionAutocloseScheduleResponse(s.deps))
}

// handleGovernorQuestionAutocloseSet updates governor.question_autoclose.
// Every field is a pointer, so an absent key leaves that setting untouched
// — the same "only what you send is changed" contract the other
// governor-config writers use.
func (s *Server) handleGovernorQuestionAutocloseSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled *bool `json:"enabled"`
		Hours   *int  `json:"hours"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}

	// --- validate before mutating anything ---
	if body.Hours != nil && *body.Hours < minQuestionAutocloseHours {
		jsonError(w, "hours must be 1 or greater", http.StatusBadRequest)
		return
	}

	// --- apply ---
	cfg := s.deps.Config
	if body.Enabled != nil {
		cfg.Governor.QuestionAutoclose.Enabled = *body.Enabled
	}
	if body.Hours != nil {
		cfg.Governor.QuestionAutoclose.Hours = *body.Hours
	}

	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after question-autoclose update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_question_autoclose", auditDetail("section", "question_autoclose"), "")
	s.refreshAndPersist()
	jsonResponse(w, questionAutocloseSectionResponse(cfg))
}

// questionAutocloseSectionResponse renders QuestionAutocloseConfig for the
// dashboard, resolving enabled/hours to the values the hive actually acts
// on (env overrides included) so the UI never has to know the env-var names
// or the default.
func questionAutocloseSectionResponse(cfg *config.Config) map[string]interface{} {
	qc := cfg.Governor.QuestionAutoclose
	return map[string]interface{}{
		"enabled": qc.IsEnabled(),
		"hours":   int(qc.EffectiveWindow().Hours()),
	}
}

// questionAutocloseScheduleEntry is one row of the live schedule table —
// exactly the fields the PR's follow-up asked for (issue repo/number,
// answered_at, closes_at, state). "state" is always "scheduled": a settled
// (closed/kept-open) entry has already left the in-memory schedule, so
// there is nothing further to poll here for it.
type questionAutocloseScheduleEntry struct {
	Repo       string `json:"repo"`
	Issue      int    `json:"issue"`
	AnsweredAt string `json:"answered_at"`
	ClosesAt   string `json:"closes_at"`
	State      string `json:"state"`
}

// questionAutocloseScheduleResponse reads the live schedule off the wired
// *questionclose.Manager. deps.QuestionAutoclose is nil when the feature is
// off (wireQuestionAutoclose never constructs a Manager in that case) or
// when bare test Dependencies never set it — either way Scheduled() on a
// nil Manager returns nil, so this renders an empty list rather than
// erroring.
func questionAutocloseScheduleResponse(deps *Dependencies) map[string]interface{} {
	var mgr *questionclose.Manager
	if deps != nil {
		mgr = deps.QuestionAutoclose
	}
	entries := mgr.Scheduled()
	out := make([]questionAutocloseScheduleEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, questionAutocloseScheduleEntry{
			Repo:       e.Repo,
			Issue:      e.Issue,
			AnsweredAt: e.AnsweredAt.UTC().Format(rfc3339Millis),
			ClosesAt:   e.Deadline.UTC().Format(rfc3339Millis),
			State:      "scheduled",
		})
	}
	return map[string]interface{}{
		"enabled": mgr.Enabled(),
		"entries": out,
	}
}

// rfc3339Millis matches the timestamp format the rest of the dashboard API
// uses for schedule/audit timestamps.
const rfc3339Millis = "2006-01-02T15:04:05.000Z07:00"
