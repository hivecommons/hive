package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

func (h *ContributeWSHub) standbySuspended(key string) (bool, int) {
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
