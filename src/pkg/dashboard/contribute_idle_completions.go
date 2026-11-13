package dashboard

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/worksource"
)

// The reason behind a no-PR idle completion.
//
// A relay that drops an issue reports task_complete with no PR and no
// no-work-family verdict, and the hub books it idle. Until this ledger the
// only trace was a log line ending `verdict=idle verdict_reason=""`: the issue
// showed "Completion cooldown until …" in the withheld list, was offered
// again when the cooldown lapsed, and was dropped again, with nothing on the
// hub saying why. The reason was kept only for no-work-family verdicts
// (noWorkVerdicts).
//
// idleCompletions keeps the LATEST such reason per work item, with when it was
// reported and by whom. It is explanation only: it never withholds an issue,
// never changes a cooldown and never counts toward trust. The completion
// cooldown row of the withheld list and the Activity monitor's issue row read
// it.
//
// The text is agent-written (the relay forwards the agent's own sentence), so
// it is shown only where the verdict reasons already are, single-line, token
// redacted and capped at noWorkReasonMaxLen.

// idleCompletionsFileName is the on-disk ledger, beside the no-PR streak ledger.
const idleCompletionsFileName = "idle-completions.json"

// The hub's own words for a no-PR idle that arrived without a reason. Which
// one applies is decided by what the relay said about how the task ended
// (normalizeIdleCompletionReason).
const (
	// idleCompletionNoConclusion: the relay inferred the end from idle terminal
	// chrome; the agent printed no verdict.
	idleCompletionNoConclusion = "agent exited without a conclusion"
	// idleCompletionCompleteNoReason: the agent printed `HIVE_VERDICT: complete`
	// with no PR, and the relay passed no sentence along. The hub cannot say
	// the agent gave none: a relay older than idleReasonWireFields discards
	// the agent's sentence, so the wording stays with what the hub knows. (A
	// newer relay whose agent really gave none says so itself, in its own
	// verdict_reason.)
	idleCompletionCompleteNoReason = "agent reported complete without a pull request; the relay passed no reason along"
	// idleCompletionBlockedNoReason: the agent reported `blocked` and said
	// nothing about what the issue waits on (empty, or only footer lines and
	// punctuation), so the hub demoted the verdict to idle
	// (reclassifyToolingBlockedVerdict). The agent did not report complete.
	idleCompletionBlockedNoReason = "agent reported blocked without saying what it waits on"
	// idleCompletionNotGiven: a relay that sends neither a reason nor a
	// completion signal (one predating both fields, or the headless path).
	idleCompletionNotGiven = "no reason given"
	// idleCompletionUnverifiedPR: the relay reported a pull request and the
	// hub could not verify it (a GitHub lookup that failed, another author,
	// another repository), so the completion was booked with no PR. The agent
	// did not drop the issue, and saying it "gave no reason" would be false.
	idleCompletionUnverifiedPR = "relay reported a pull request the hub could not verify"
	// idleCompletionUnsettledPR: the reported pull request verified, but the
	// hub could not settle the completion as shipped (a donated standby PR
	// whose required hold label could not be applied).
	idleCompletionUnsettledPR = "relay reported a pull request the hub verified but could not settle as shipped"
)

// idleCompletionReportedPRReason is the reason for a completion booked idle
// although the relay reported a pull request: what the hub made of that pull
// request, with the verifier's own reason when it has one. It is used only
// when the relay sent no reason of its own.
func idleCompletionReportedPRReason(verified bool, verifyReason string) string {
	if verified {
		return idleCompletionUnsettledPR
	}
	verifyReason = strings.Join(strings.Fields(verifyReason), " ")
	if verifyReason == "" {
		return idleCompletionUnverifiedPR
	}
	return idleCompletionUnverifiedPR + ": " + verifyReason
}

// idleCompletionRetention is how long an idle reason is kept without a newer
// completion replacing it. It matches the no-PR streak's own reset window, so
// the reason outlives every cooldown the streak can book.
const idleCompletionRetention = noPRStreakResetAfter

// idleCompletionRecord is the in-memory and on-disk shape of one work item's
// latest no-PR idle completion.
type idleCompletionRecord struct {
	RecordedAt time.Time `json:"recorded_at"`
	Reporter   string    `json:"reporter,omitempty"`
	Reason     string    `json:"reason"`
	// Signal is the normalized completion signal the reason came with.
	Signal string `json:"signal,omitempty"`
}

func (r idleCompletionRecord) expired(now time.Time) bool {
	return r.RecordedAt.IsZero() || now.Sub(r.RecordedAt) >= idleCompletionRetention
}

// normalizeIdleCompletionReason returns the reason to keep for a no-PR idle completion.
// It is never empty: a relay-supplied reason is token-redacted, flattened to
// one line and capped; an absent one is replaced by what the completion signal
// says about how the task ended.
func normalizeIdleCompletionReason(reported, signal string) string {
	reason := strings.Join(strings.Fields(redactTokens(reported)), " ")
	reason = strings.TrimSpace(truncateReasonBytes(reason))
	if reason != "" {
		return reason
	}
	switch normalizeCompletionSignal(signal) {
	case completionSignalChromeIdle:
		return idleCompletionNoConclusion
	case completionSignalVerdict:
		return idleCompletionCompleteNoReason
	}
	return idleCompletionNotGiven
}

// idleCompletionsPath is where the ledger lives: the per-hub override, else the
// directory the no-PR streak ledger is written to, so the two always travel
// together (and a test that redirects one redirects both).
func (h *ContributeWSHub) idleCompletionsPath() string {
	if h != nil && h.idleCompletionsFile != "" {
		return h.idleCompletionsFile
	}
	return filepath.Join(filepath.Dir(h.noPRStreaksPath()), idleCompletionsFileName)
}

func (h *ContributeWSHub) loadIdleCompletions() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.idleCompletionsPath())
	if err != nil {
		return
	}
	records := make(map[string]idleCompletionRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	now := time.Now()
	for k, rec := range records {
		if rec.expired(now) || rec.Reason == "" {
			continue
		}
		if h.idleCompletions == nil {
			h.idleCompletions = make(map[string]idleCompletionRecord)
		}
		h.idleCompletions[k] = rec
	}
	h.logger.Info("[contribute-ws] loaded idle completions", "count", len(h.idleCompletions))
}

func (h *ContributeWSHub) saveIdleCompletions() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	now := time.Now()
	h.completedMu.Lock()
	saved := make(map[string]idleCompletionRecord, len(h.idleCompletions))
	for k, rec := range h.idleCompletions {
		if rec.expired(now) {
			delete(h.idleCompletions, k)
			continue
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] idle completions marshal failed", "error", err)
		return
	}
	path := h.idleCompletionsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] idle completions directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] idle completions write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] idle completions rename failed", "error", err)
	}
}

// bookIdleCompletionLocked records the reason of a no-PR idle completion against
// key, or forgets the previous one when this completion is anything else: a
// verified PR (where the no-PR streak is cleared too) or a no-work-family
// verdict, which keeps its own reason in noWorkVerdicts. The record therefore
// always describes the LATEST completion. It reports whether the ledger
// changed. Callers must hold completedMu.
func (h *ContributeWSHub) bookIdleCompletionLocked(key, prURL, verdict, reporter, reason, signal string) bool {
	if prURL == "" && verdict == completionVerdictIdle {
		if h.idleCompletions == nil {
			h.idleCompletions = make(map[string]idleCompletionRecord)
		}
		h.idleCompletions[key] = idleCompletionRecord{
			RecordedAt: time.Now(),
			Reporter:   strings.TrimSpace(reporter),
			Reason:     normalizeIdleCompletionReason(reason, signal),
			Signal:     normalizeCompletionSignal(signal),
		}
		return true
	}
	if _, had := h.idleCompletions[key]; had {
		delete(h.idleCompletions, key)
		return true
	}
	return false
}

// dropIdleCompletionLocked forgets the idle reason on record for key and
// reports whether there was one. The failure and release paths call it
// (contribute_cooldowns.go): a task that failed, or was let go, on the item
// after the idle completion means that completion is no longer what the last
// relay did, and the completion cooldown it booked can still be running
// (recordTaskFailureKey leaves completedTasks alone). Callers must hold
// completedMu.
func (h *ContributeWSHub) dropIdleCompletionLocked(key string) bool {
	if _, had := h.idleCompletions[key]; !had {
		return false
	}
	delete(h.idleCompletions, key)
	return true
}

// idleCompletionKey returns the live idle reason on record for key. Read-only: an
// expired record is reported as absent and left for the next save to drop.
func (h *ContributeWSHub) idleCompletionKey(key string) (idleCompletionRecord, bool) {
	if h == nil || key == "" {
		return idleCompletionRecord{}, false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	rec, ok := h.idleCompletions[key]
	if !ok || rec.Reason == "" || rec.expired(time.Now()) {
		return idleCompletionRecord{}, false
	}
	return rec, true
}

// idleCompletionNote is the one clause every surface shows for an idle reason, so
// the withheld list, the Activity monitor and the admin MCP read the same
// words.
func idleCompletionNote(rec idleCompletionRecord) string {
	return "last relay: " + rec.Reason
}

// withheldCooldownIdleItem is withheldCooldownItem for a completion cooldown
// whose completion was a no-PR idle: the row also says why the relay dropped
// the issue ("Completion cooldown until 15:04 UTC — last relay: <reason>").
func withheldCooldownIdleItem(item AdmissionWithheldItem, rec idleCompletionRecord, ok bool) AdmissionWithheldItem {
	if !ok {
		return item
	}
	item.Detail = fmt.Sprintf("%s — %s", item.Detail, idleCompletionNote(rec))
	return item
}

// StageTrackerLastIdle is the Activity monitor's note on an issue row whose
// latest contributor-relay task ended with no pull request and no verdict,
// while the completion cooldown that task booked is still running: the
// reason on record, who reported it and when. Text is the clause the
// row's tooltip shows, the same one the withheld list ends its completion
// cooldown row with.
type StageTrackerLastIdle struct {
	Text        string     `json:"text"`
	Reason      string     `json:"reason"`
	Contributor string     `json:"contributor,omitempty"`
	At          *time.Time `json:"at,omitempty"`
}

// stageTrackerLastIdle returns the last-idle note for an issue, or nil when
// the hub has none on record or the completion cooldown that idle booked is
// no longer in force.
//
// The cooldown is the note's lifetime, by construction: the row shows it under
// exactly the condition the withheld list shows its completion cooldown row
// (isTaskInCooldownKey), so a note cannot sit on a row for the ledger's whole
// retention with nothing saying how old it is. isTaskInCooldownKey answers
// false when the operator has switched the completion cooldown off; while
// that stays off the Activity monitor shows no note at all, and the reason is
// still in the hub log, the run record and the ledger.
//
// The two reads each take completedMu on their own and are never nested. A
// completion booked between them either replaces the record or removes it, so
// the pair cannot show a reason for a cooldown it does not belong to.
func (s *Server) stageTrackerLastIdle(repo string, number int) *StageTrackerLastIdle {
	if s == nil || s.contributeHub == nil || repo == "" || number <= 0 {
		return nil
	}
	key := worksource.Ref{Repo: repo, Number: number}.Key()
	if !s.contributeHub.isTaskInCooldownKey(key) {
		return nil
	}
	rec, ok := s.contributeHub.idleCompletionKey(key)
	if !ok {
		return nil
	}
	at := rec.RecordedAt.UTC()
	return &StageTrackerLastIdle{Text: idleCompletionNote(rec), Reason: rec.Reason, Contributor: rec.Reporter, At: &at}
}
