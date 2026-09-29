package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// loadStandbyOutcomes restores the outcome ledger at hub startup. The ledger is
// the record of which configurations are suspended, so an unreadable file is
// reported at error level rather than dropped silently (#9184): starting from an
// empty ledger reinstates every suspension it held.
func (h *ContributeWSHub) loadStandbyOutcomes() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	path := h.standbyOutcomesPath()
	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			h.logger.Error("[contribute-ws] standby outcomes read failed; starting with an empty ledger, suspensions it held are not enforced", "path", path, "error", err)
		}
		return
	}
	var records []standbyOutcomeRecord
	if err := json.Unmarshal(data, &records); err != nil {
		h.logger.Error("[contribute-ws] standby outcomes unreadable; starting with an empty ledger, suspensions it held are not enforced", "path", path, "error", err)
		return
	}
	h.completedMu.Lock()
	h.standbyOutcomes = records
	h.completedMu.Unlock()
}

// saveStandbyOutcomes persists the in-memory ledger. standbyOutcomesSaveMu is
// held from the snapshot through the rename (#9184), so saves land in snapshot
// order and a stale snapshot can never overwrite a newer one.
func (h *ContributeWSHub) saveStandbyOutcomes() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.standbyOutcomesSaveMu.Lock()
	defer h.standbyOutcomesSaveMu.Unlock()
	h.completedMu.Lock()
	records := append([]standbyOutcomeRecord(nil), h.standbyOutcomes...)
	h.completedMu.Unlock()
	data, err := json.Marshal(records)
	if err != nil {
		h.logger.Warn("[contribute-ws] standby outcomes marshal failed", "error", err)
		return
	}
	if err := writeStandbyOutcomesFile(h.standbyOutcomesPath(), data); err != nil {
		h.logger.Warn("[contribute-ws] standby outcomes persist failed", "error", err)
	}
}

// writeStandbyOutcomesFile is the crash-safe persist of the #5625 idiom used
// by saveLeasesLocked: a unique temp name (a fixed name lets two writers
// interleave into one file and rename it torn), an fsync of the bytes before
// the rename, and an fsync of the directory so the rename survives a crash.
func writeStandbyOutcomesFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("directory creation: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("temp creation: %w", err)
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	// CreateTemp makes 0600; keep the mode this ledger has always had.
	if err := tmp.Chmod(0o644); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	keep = true
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("directory open: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("directory sync: %w", err)
	}
	return nil
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

// reconcileOpenStandbyOutcomes settles open donated PRs against GitHub. It is
// reachable concurrently (status build, standby_declare, dispatch), so it is
// serialized on standbyReconcileMu, and settlement is re-checked against the
// live ledger under completedMu right before appending (#9184): a closure
// recorded while this pass was inside its GitHub GETs — by another path such
// as the completion handler's merged row — must not be recorded a second time.
func (h *ContributeWSHub) reconcileOpenStandbyOutcomes() {
	if h == nil || h.server == nil || h.server.deps == nil || h.server.deps.GHClient == nil {
		return
	}
	h.standbyReconcileMu.Lock()
	defer h.standbyReconcileMu.Unlock()

	h.completedMu.Lock()
	rows := append([]standbyOutcomeRecord(nil), h.standbyOutcomes...)
	h.completedMu.Unlock()

	settled := settledStandbyPRs(rows)

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
	live := settledStandbyPRs(h.standbyOutcomes)
	appended := 0
	for _, rec := range add {
		if live[standbyOutcomePRKey(rec)] {
			continue
		}
		h.standbyOutcomes = append(h.standbyOutcomes, rec)
		appended++
	}
	h.completedMu.Unlock()
	if appended > 0 {
		h.saveStandbyOutcomes()
	}
}

// settledStandbyPRs is the set of donated PRs (by standbyOutcomePRKey) that
// already carry a settled outcome row.
func settledStandbyPRs(rows []standbyOutcomeRecord) map[string]bool {
	settled := map[string]bool{}
	for _, rec := range rows {
		if rec.Kind == standbypkg.OutcomeMerged || rec.Kind == standbypkg.OutcomeClosedUnmerged || rec.Kind == standbypkg.OutcomeMergedAfterRework {
			settled[standbyOutcomePRKey(rec)] = true
		}
	}
	return settled
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
