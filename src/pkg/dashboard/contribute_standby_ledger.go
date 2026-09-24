package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	standbypkg "github.com/hivecommons/hive/pkg/standby"
)

const standbyOutcomesFileName = "standby-outcomes.json"

type standbyOutcomeRecord = standbypkg.Outcome

func standbyOutcomeKey(contributor string, c standbypkg.Configuration) string {
	return standbypkg.LedgerKey(contributor, c)
}

func standbyConfigFromState(state StandbyConnectionState) standbypkg.Configuration {
	return standbypkg.Configuration{
		Backend:         state.CLIBackend,
		Model:           state.Model,
		ReasoningEffort: state.ReasoningEffort,
		AdvisorModel:    state.AdvisorModel,
		AdvisorEffort:   state.AdvisorEffort,
	}
}

func standbyConfigFromConnection(c *ContributorConnection) standbypkg.Configuration {
	if c == nil {
		return standbypkg.Configuration{}
	}
	return standbypkg.Configuration{
		Backend:         c.cliBackend,
		Model:           c.model,
		ReasoningEffort: c.reasoningEffort,
		AdvisorModel:    c.advisorModel,
		AdvisorEffort:   c.advisorEffort,
	}
}

func (h *ContributeWSHub) standbyOutcomesPath() string {
	if h != nil && h.standbyOutcomesFile != "" {
		return h.standbyOutcomesFile
	}
	return filepath.Join(getContributorsDir(), standbyOutcomesFileName)
}

func (h *ContributeWSHub) loadStandbyOutcomes() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	data, err := os.ReadFile(h.standbyOutcomesPath())
	if err != nil {
		return
	}
	var records []standbyOutcomeRecord
	if json.Unmarshal(data, &records) != nil {
		return
	}
	h.completedMu.Lock()
	h.standbyOutcomes = records
	h.completedMu.Unlock()
}

func (h *ContributeWSHub) saveStandbyOutcomes() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	records := append([]standbyOutcomeRecord(nil), h.standbyOutcomes...)
	h.completedMu.Unlock()
	data, err := json.Marshal(records)
	if err != nil {
		h.logger.Warn("[contribute-ws] standby outcomes marshal failed", "error", err)
		return
	}
	path := h.standbyOutcomesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] standby outcomes directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] standby outcomes write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] standby outcomes rename failed", "error", err)
	}
}

func (h *ContributeWSHub) appendStandbyOutcome(rec standbyOutcomeRecord) {
	if rec.OutcomeAt.IsZero() {
		rec.OutcomeAt = time.Now().UTC()
	}
	h.completedMu.Lock()
	h.standbyOutcomes = append(h.standbyOutcomes, rec)
	h.completedMu.Unlock()
	h.saveStandbyOutcomes()
}

func (h *ContributeWSHub) recordVerifiedStandbyPR(contributor, lane, repo, prURL string, assignedAt time.Time) {
	if h == nil || contributor == "" || repo == "" || prURL == "" {
		return
	}
	ref, err := ghpkg.ParsePRURL(prURL)
	if err != nil {
		return
	}
	cfg := standbypkg.Configuration{}
	h.mu.RLock()
	for _, conn := range h.connections {
		if conn != nil && conn.profile != nil && strings.EqualFold(conn.profile.GitHubUsername, contributor) {
			cfg = standbyConfigFromConnection(conn)
			break
		}
	}
	h.mu.RUnlock()
	h.appendStandbyOutcome(standbyOutcomeRecord{
		Key:          standbyOutcomeKey(contributor, cfg),
		Lane:         lane,
		Repo:         repo,
		Number:       ref.Number,
		DispatchedAt: assignedAt.UTC(),
		Kind:         standbypkg.OutcomeOpen,
	})
}

func (h *ContributeWSHub) reconcileOpenStandbyOutcomes() {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return
	}
	h.completedMu.Lock()
	rows := append([]standbyOutcomeRecord(nil), h.standbyOutcomes...)
	h.completedMu.Unlock()

	settled := map[string]bool{}
	for _, rec := range rows {
		if rec.Kind == standbypkg.OutcomeMerged || rec.Kind == standbypkg.OutcomeClosedUnmerged || rec.Kind == standbypkg.OutcomeMergedAfterRework {
			settled[standbyOutcomePRKey(rec)] = true
		}
	}

	var add []standbyOutcomeRecord
	for _, rec := range rows {
		if rec.Kind != standbypkg.OutcomeOpen || rec.Repo == "" || rec.Number <= 0 || settled[standbyOutcomePRKey(rec)] {
			continue
		}
		contributor, _, _ := strings.Cut(rec.Key, "|")
		prURL := "https://github.com/" + rec.Repo + "/pull/" + strconv.Itoa(rec.Number)
		detail := h.verifyReportedPRDetail(rec.Repo, prURL, contributor)
		if !detail.Verified {
			continue
		}
		var kind standbypkg.OutcomeKind
		switch {
		case detail.Merged:
			kind = standbypkg.OutcomeMerged
		case strings.EqualFold(detail.State, "closed"):
			kind = standbypkg.OutcomeClosedUnmerged
		default:
			continue
		}
		add = append(add, standbyOutcomeRecord{
			Key:          rec.Key,
			Lane:         rec.Lane,
			Repo:         rec.Repo,
			Number:       rec.Number,
			DispatchedAt: rec.DispatchedAt,
			Kind:         kind,
			OutcomeAt:    time.Now().UTC(),
		})
		settled[standbyOutcomePRKey(rec)] = true
	}
	if len(add) == 0 {
		return
	}
	h.completedMu.Lock()
	h.standbyOutcomes = append(h.standbyOutcomes, add...)
	h.completedMu.Unlock()
	h.saveStandbyOutcomes()
}

func standbyOutcomePRKey(rec standbyOutcomeRecord) string {
	return rec.Key + "|" + rec.Repo + "#" + strconv.Itoa(rec.Number)
}

func (h *ContributeWSHub) standbySuspended(key string) (bool, int) {
	h.reconcileOpenStandbyOutcomes()
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	rows := standbypkg.OutcomesFor(h.standbyOutcomes, key)
	return standbypkg.SuspendState(rows, standbypkg.DefaultSuspendThreshold)
}

func (h *ContributeWSHub) clearStandbySuspension(contributor string, cfg standbypkg.Configuration) {
	key := standbyOutcomeKey(contributor, cfg)
	h.appendStandbyOutcome(standbyOutcomeRecord{
		Key:  key,
		Kind: standbypkg.OutcomeCleared,
	})
}

func (s *Server) handleContributeStandbyClear(w http.ResponseWriter, r *http.Request) {
	if !s.requireContributorWrite(w, r) {
		return
	}
	if s.contributeHub == nil {
		jsonError(w, "contribute hub not ready", http.StatusServiceUnavailable)
		return
	}
	var body struct {
		Contributor     string `json:"contributor"`
		Backend         string `json:"backend"`
		Model           string `json:"model"`
		ReasoningEffort string `json:"reasoning_effort"`
		AdvisorModel    string `json:"advisor_model"`
		AdvisorEffort   string `json:"advisor_reasoning_effort"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := jsonDecode(r.Body, &body); err != nil {
		jsonError(w, "invalid request", http.StatusBadRequest)
		return
	}
	contributor := strings.TrimSpace(body.Contributor)
	if contributor == "" {
		jsonError(w, "contributor required", http.StatusBadRequest)
		return
	}
	cfg := standbypkg.Configuration{
		Backend:         strings.TrimSpace(body.Backend),
		Model:           strings.TrimSpace(body.Model),
		ReasoningEffort: strings.TrimSpace(body.ReasoningEffort),
		AdvisorModel:    strings.TrimSpace(body.AdvisorModel),
		AdvisorEffort:   strings.TrimSpace(body.AdvisorEffort),
	}
	s.contributeHub.clearStandbySuspension(contributor, cfg)
	s.auditFromRequest(r, "contribute_standby_clear", auditDetail("contributor", contributor), "")
	jsonResponse(w, map[string]any{"ok": true, "contributor": contributor})
}

func standbyPRNumber(prURL string) int {
	idx := strings.LastIndex(strings.TrimSpace(prURL), "/")
	if idx < 0 {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(prURL[idx+1:]))
	return n
}
