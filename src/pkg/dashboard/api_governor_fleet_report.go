package dashboard

import (
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Self-report settings (hivecommons/hive#11868): a Settings switch, repo
// field and daily cap for governor.fleet_report — the keys
// governor.fleet_report.enabled, governor.fleet_report.repo,
// governor.fleet_report.max_per_day and the legacy
// governor.fleet_report.file_upstream opt-in. Mirrors the
// question-autoclose GET/PUT pair: its own small endpoint under its own
// config block, owner-only.

// maxFleetReportPerDay bounds the daily cap an operator may set; above this
// a misbehaving hive would be spamming the report repo.
const maxFleetReportPerDay = 50

func (s *Server) handleGovernorFleetReportGet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, s.fleetReportSectionResponse(s.deps.Config))
}

func (s *Server) handleGovernorFleetReportSet(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	if s.deps == nil || s.deps.Config == nil {
		jsonError(w, "config unavailable", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Enabled   *bool   `json:"enabled"`
		Repo      *string `json:"repo"`
		MaxPerDay *int    `json:"max_per_day"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	if body.Repo != nil {
		repo := strings.TrimSpace(*body.Repo)
		if repo != "" && !validFleetReportRepo(repo) {
			jsonError(w, "repo must be owner/name", http.StatusBadRequest)
			return
		}
	}
	if body.MaxPerDay != nil && (*body.MaxPerDay < 0 || *body.MaxPerDay > maxFleetReportPerDay) {
		jsonError(w, "max_per_day must be between 0 and 50", http.StatusBadRequest)
		return
	}
	cfg := s.deps.Config
	if body.Enabled != nil {
		v := *body.Enabled
		cfg.Governor.FleetReport.Enabled = &v
	}
	if body.Repo != nil {
		cfg.Governor.FleetReport.Repo = strings.TrimSpace(*body.Repo)
	}
	if body.MaxPerDay != nil {
		cfg.Governor.FleetReport.MaxPerDay = *body.MaxPerDay
	}
	if err := s.saveConfig(); err != nil {
		s.logger.Error("failed to persist config after fleet-report update", "error", err)
	}
	s.auditFromRequest(r, "config_governor_fleet_report", auditDetail("section", "fleet_report"), "")
	s.refreshAndPersist()
	jsonResponse(w, s.fleetReportSectionResponse(cfg))
}

func validFleetReportRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	for _, r := range repo {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.' || r == '/') {
			return false
		}
	}
	return true
}

// fleetReportSectionResponse renders governor.fleet_report with the tri-state
// resolved to the value the hive acts on, plus the raw explicit setting so
// the UI can show "default (hosted: on)" vs "set".
func (s *Server) fleetReportSectionResponse(cfg *config.Config) map[string]interface{} {
	fr := cfg.Governor.FleetReport
	hubManaged := s.FleetReportHubManaged()
	out := map[string]interface{}{
		"enabled":      fr.FilesUpstream(hubManaged),
		"explicit":     fr.Enabled != nil || fr.FileUpstream,
		"hub_managed":  hubManaged,
		"repo":         fr.EffectiveRepo(),
		"max_per_day":  fr.EffectiveMaxPerDay(),
		"hive_slug":    s.FleetReportHiveSlug(),
		"open_reports": s.fleetReportOpenSummary(),
	}
	return out
}

// FleetReportOpenEntry is one issue this hive has filed or joined.
type FleetReportOpenEntry struct {
	Fingerprint string `json:"fingerprint"`
	Class       string `json:"class,omitempty"`
	Trigger     string `json:"trigger,omitempty"`
	Number      int    `json:"number"`
	URL         string `json:"url,omitempty"`
	Joined      bool   `json:"joined,omitempty"`
	PostedAt    string `json:"posted_at,omitempty"`
}

// fleetReportOpenSummary lists the issues this hive has written to and not
// yet seen recover, newest first.
func (s *Server) fleetReportOpenSummary() []FleetReportOpenEntry {
	st := s.loadFleetReportState()
	out := make([]FleetReportOpenEntry, 0, len(st.Open))
	for fp, open := range st.Open {
		if open.Number <= 0 || open.Recovered || !open.ClosedAt.IsZero() {
			continue
		}
		e := FleetReportOpenEntry{Fingerprint: fp, Class: open.Class, Trigger: open.Trigger, Number: open.Number, URL: open.URL, Joined: open.Joined}
		if !open.PostedAt.IsZero() {
			e.PostedAt = open.PostedAt.UTC().Format(rfc3339Millis)
		}
		out = append(out, e)
	}
	sortFleetReportEntries(out)
	return out
}

func sortFleetReportEntries(entries []FleetReportOpenEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].PostedAt > entries[j-1].PostedAt; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}
