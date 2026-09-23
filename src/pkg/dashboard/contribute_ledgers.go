package dashboard

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/worksource"
)

// completedTaskCooldownHours is the cooldown applied when a task completes
// having actually shipped a pull request: real work landed, so we should not
// re-dispatch the same issue for a week.
const completedTaskCooldownHours = 168

// completedNoPRCooldownHours is the cooldown applied when a task "completes"
// only because the agent went idle WITHOUT reporting a PR (the common case the
// old code could not distinguish — see kubestellar/hive#2393 item 7). Nothing
// shipped, so locking the issue for a full week wrongly starves it: another
// contributor should be able to pick it up soon. We keep a short, non-zero
// cooldown (not zero) so the very next selector pass does not instantly hand
// the same untouched issue back to the same idle contributor in a tight loop;
// a few hours is long enough to break that loop while still freeing the issue
// the same day.
const completedNoPRCooldownHours = 4

// noPRStreakResetAfter is how long a no-PR completion streak (#3980) survives
// without another no-PR completion before it forgets itself and the next
// no-PR cooldown starts back at completedNoPRCooldownHours. It must be LONGER
// than the streak's own cooldown cap (the with-PR cooldown, default
// completedTaskCooldownHours = 168h): the whole point is that the issue is
// only re-dispatched AFTER its escalated cooldown expires, so a reset window
// shorter than the cap would wipe the streak before the capped re-offer ever
// happened and the backoff could never reach — or hold — its ceiling. Two
// weeks = 2× the default cap, so one full capped cycle plus slack.
const noPRStreakResetAfter = 14 * 24 * time.Hour

// completionVerdict* are the recognized values of WSMessage.Verdict on
// task_complete (#3987). Anything else — including an absent field, which is
// what every relay written before this sends — normalizes to idle, i.e.
// byte-for-byte today's behavior (see normalizeCompletionVerdict).
//
// completionVerdictBlocked (hivecommons/hive#7924) is no_work_needed's
// sibling: nothing in the task's repo can change until something OUTSIDE it
// lands — another repo's release or factory build, a dependency that has not
// published. utah#100 reached exactly that conclusion and, booked as a plain
// no_work_needed, was re-offered on the short no-PR backoff to re-discover
// "still no factory build". A blocked verdict is held for the FULL with-PR
// cooldown instead (markTaskCompletedVerdictKeySignal) and its ledger row
// carries the marker, so an operator can see what the issue is waiting on.
const (
	completionVerdictShipped       = "shipped"
	completionVerdictIdle          = "idle"
	completionVerdictNoWorkNeeded  = "no_work_needed"
	completionVerdictBlocked       = "blocked"
	completionVerdictNeedsDecision = "needs_decision"
)

// noWorkReasonMaxLen caps the client-supplied VerdictReason before it is stored
// in the PVC-backed verdict ledger and echoed into logs: the field is scraped
// from arbitrary agent output, so an unbounded value would let a confused (or
// hostile) relay bloat the ledger file. 200 runes comfortably fits the
// machine-readable reasons ("maintainer_gated", "already_covered") plus a short
// free-text explanation.
const noWorkReasonMaxLen = 200

var completedTasksFile = "/data/contributors/completed-tasks.json"

// completedTaskRecord is the on-disk shape of one completed-task entry. It
// carries the completion time plus, since #2393 item 7, the per-task cooldown
// and the PR URL that decided it, so a hub restart preserves whether an issue
// got the short no-PR cooldown or the full one. Entries written by older builds
// were a bare RFC3339 timestamp string; loadCompletedTasks still accepts that
// legacy form and treats it as a full-cooldown, no-PR entry.
type completedTaskRecord struct {
	CompletedAt   time.Time `json:"completed_at"`
	CooldownHours float64   `json:"cooldown_hours,omitempty"`
	PRURL         string    `json:"pr_url,omitempty"`
}

var failedTasksFile = "/data/contributors/failed-tasks.json"

var noPRStreaksFile = "/data/contributors/no-pr-streaks.json"

// noPRStreakRecord is the in-memory and on-disk shape of one no-PR completion
// streak (#3980). LastAt is the most recent no-PR completion; the streak is
// discarded once it is older than noPRStreakResetAfter — enforced lazily on
// advance, on save, and on load, so a hub bounce cannot resurrect a stale
// streak and the map cannot grow without bound.
type noPRStreakRecord struct {
	Count  int       `json:"count"`
	LastAt time.Time `json:"last_at"`
}

func (h *ContributeWSHub) loadNoPRStreaks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.noPRStreaksPath())
	if err != nil {
		return
	}
	records := make(map[string]noPRStreakRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for k, rec := range records {
		if rec.Count <= 0 || rec.LastAt.IsZero() {
			continue
		}
		if time.Since(rec.LastAt) >= noPRStreakResetAfter {
			continue
		}
		if h.noPRStreaks != nil {
			h.noPRStreaks[k] = rec
		}
	}
	h.logger.Info("[contribute-ws] loaded no-PR streaks", "count", len(h.noPRStreaks))
}

func (h *ContributeWSHub) saveNoPRStreaks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]noPRStreakRecord, len(h.noPRStreaks))
	for k, rec := range h.noPRStreaks {
		if time.Since(rec.LastAt) >= noPRStreakResetAfter {
			continue
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks marshal failed", "error", err)
		return
	}
	path := h.noPRStreaksPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] no-PR streaks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) noPRStreaksPath() string {
	if h != nil && h.noPRStreaksFile != "" {
		return h.noPRStreaksFile
	}
	return noPRStreaksFile
}

// advanceNoPRStreakLocked books one more consecutive no-PR completion against
// key and returns the escalated cooldown to apply (#3980): the base
// completedNoPRCooldownHours doubled per prior streak entry, capped at the
// operator's with-PR cooldown. 4h → 8h → 16h → … keeps the original "another
// contributor can retry soon" behavior for a first idle completion, while an
// issue that repeatedly yields "nothing to ship" — however a future parsing
// gap lets that happen — converges to at most one wasted task cycle per
// with-PR cooldown period instead of one every 4h forever. Callers must hold
// completedMu.
func (h *ContributeWSHub) advanceNoPRStreakLocked(key string, ceiling time.Duration) time.Duration {
	if h.noPRStreaks == nil {
		h.noPRStreaks = make(map[string]noPRStreakRecord)
	}
	rec := h.noPRStreaks[key]
	if rec.Count > 0 && time.Since(rec.LastAt) >= noPRStreakResetAfter {
		rec = noPRStreakRecord{}
	}
	rec.Count++
	rec.LastAt = time.Now()
	h.noPRStreaks[key] = rec

	cooldown := completedNoPRCooldownHours * time.Hour
	for i := 1; i < rec.Count; i++ {
		if cooldown >= ceiling {
			break
		}
		cooldown *= 2
	}
	if cooldown > ceiling {
		cooldown = ceiling
	}
	return cooldown
}

// noWorkVerdictsFileName is the on-disk ledger for no_work_needed completion
// verdicts (#3987), living in the same PVC-backed contributors dir as the
// cooldown/streak ledgers so a hub pod restart does not forget a verdict.
// The full path comes from noWorkVerdictsPath(), which honours
// HIVE_CONTRIBUTORS_DIR exactly as the contributor profiles do — that is what
// lets the persistence round-trip test point it at a temp dir.
const noWorkVerdictsFileName = "no-work-verdicts.json"

func noWorkVerdictsPath() string {
	return filepath.Join(getContributorsDir(), noWorkVerdictsFileName)
}

func (h *ContributeWSHub) noWorkVerdictsPath() string {
	if h != nil && h.noWorkVerdictsFile != "" {
		return h.noWorkVerdictsFile
	}
	return noWorkVerdictsPath()
}

// noWorkVerdictRecord is the in-memory and on-disk shape of one no_work_needed
// verdict (#3987). RecordedAt anchors both the suppression window and the
// invalidation comparison (issue updated_at newer than RecordedAt voids the
// verdict). SuppressHours snapshots the operator's with-PR cooldown at record
// time so a later config change cannot retroactively stretch an old verdict;
// 0 (an older entry) falls back to the completedTaskCooldownHours default.
// Reporter/Reason are audit-only. Blocked marks a blocked verdict (#7924):
// the reason names an external dependency the issue is waiting on, not a
// settlement — an operator reading the ledger sees which it is.
type noWorkVerdictRecord struct {
	RecordedAt            time.Time `json:"recorded_at"`
	SuppressHours         float64   `json:"suppress_hours,omitempty"`
	Reporter              string    `json:"reporter,omitempty"`
	Reason                string    `json:"reason,omitempty"`
	NeedsDecision         bool      `json:"needs_decision,omitempty"`
	ReasonKind            string    `json:"reason_kind,omitempty"`
	EvidencePR            int       `json:"evidence_pr,omitempty"`
	EvidenceCommit        string    `json:"evidence_commit,omitempty"`
	AlreadyDoneUnverified bool      `json:"already_done_unverified,omitempty"`
	Blocked               bool      `json:"blocked,omitempty"`
}

// suppressWindow is how long, from RecordedAt, this verdict withholds the
// issue from the contribute offer pool (absent earlier invalidation).
func (r noWorkVerdictRecord) suppressWindow() time.Duration {
	if r.SuppressHours > 0 {
		return time.Duration(r.SuppressHours * float64(time.Hour))
	}
	return completedTaskCooldownHours * time.Hour
}

func (h *ContributeWSHub) configuredAlreadyDoneUnverifiedHold() time.Duration {
	days := 30
	if h != nil && h.server != nil && h.server.deps != nil && h.server.deps.Config != nil {
		days = h.server.deps.Config.Hub.ContributeAlreadyDoneHoldDaysOrDefault()
	}
	return time.Duration(days) * 24 * time.Hour
}

func (h *ContributeWSHub) loadNoWorkVerdicts() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.noWorkVerdictsPath())
	if err != nil {
		return
	}
	records := make(map[string]noWorkVerdictRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for k, rec := range records {
		if rec.RecordedAt.IsZero() {
			continue
		}
		// Drop entries already past their own suppression window so a stale
		// verdict is never resurrected across a restart.
		if time.Since(rec.RecordedAt) >= rec.suppressWindow() {
			continue
		}
		if h.noWorkVerdicts != nil {
			h.noWorkVerdicts[k] = rec
		}
	}
	h.logger.Info("[contribute-ws] loaded no-work verdicts", "count", len(h.noWorkVerdicts))
}

func (h *ContributeWSHub) saveNoWorkVerdicts() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]noWorkVerdictRecord, len(h.noWorkVerdicts))
	for k, rec := range h.noWorkVerdicts {
		if time.Since(rec.RecordedAt) >= rec.suppressWindow() {
			continue
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts marshal failed", "error", err)
		return
	}
	path := h.noWorkVerdictsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] no-work verdicts rename failed", "error", err)
	}
}

// normalizeCompletionVerdict maps the client-reported verdict plus the
// SERVER-side PR verification outcome to the verdict the hub acts on (#3987).
// A VERIFIED PR always wins ("shipped") regardless of what the client claimed —
// the self-reported field can neither hide nor fabricate shipped work. With no
// verified PR, only an exact (case-insensitive) no_work_needed or blocked is
// honoured; anything else — absent, unknown, or a claimed "shipped" whose PR
// failed verification — normalizes to idle, which is byte-for-byte today's
// behavior, so relays that never learn the field keep working unchanged.
//
// blocked (#7924) is accepted in two spellings: the verdict itself, or
// no_work_needed with the WSMessage.VerdictBlocked marker set. The relay sends
// the second — a hub older than this one then still sees the no_work_needed it
// already books, rather than an unknown token it would normalize to idle.
func normalizeCompletionVerdict(reported string, blocked bool, needsDecision bool, verifiedPR string) string {
	if verifiedPR != "" {
		return completionVerdictShipped
	}
	reported = strings.TrimSpace(reported)
	if strings.EqualFold(reported, completionVerdictBlocked) {
		return completionVerdictBlocked
	}
	if strings.EqualFold(reported, completionVerdictNoWorkNeeded) {
		if blocked {
			return completionVerdictBlocked
		}
		if needsDecision {
			return completionVerdictNeedsDecision
		}
		return completionVerdictNoWorkNeeded
	}
	return completionVerdictIdle
}

// isNoWorkFamilyVerdict reports whether a normalized verdict is an affirmative
// "nothing to ship" conclusion — no_work_needed or its blocked sibling — as
// opposed to shipped work or a bare return to idle.
func isNoWorkFamilyVerdict(verdict string) bool {
	return verdict == completionVerdictNoWorkNeeded || verdict == completionVerdictBlocked || verdict == completionVerdictNeedsDecision
}

// Completion-signal vocabulary (kubestellar/hive#5376). Originally diagnostic
// only; since #6723 an explicit chrome_idle also marks a completion as
// evidence-less, which bounds its cooldown and preserves failure history (see
// isEvidenceLessCompletion). It still grants no trust and no selection.
const (
	// completionSignalVerdict — the agent printed its own HIVE_VERDICT: line.
	// This is the trustworthy signal.
	completionSignalVerdict = "verdict"
	// completionSignalChromeIdle — the agent never printed one and the relay
	// fell back to a bounded grace period of idle-looking terminal chrome.
	// Chrome inference is the mechanism behind thirteen separate
	// false-completion issues, so a rising count here for some backend is the
	// operator's cue that that backend is not honouring the sentinel.
	completionSignalChromeIdle = "chrome_idle"
	// completionSignalUnknown — the field was absent (a relay predating #5376,
	// or the headless path, which takes the CLI's exit code) or carried a
	// value the hub does not recognise.
	completionSignalUnknown = "unknown"
)

// isEvidenceLessCompletion reports whether a task_complete carries NO evidence
// that the work was attempted (#6723).
//
// The hub accepts three kinds of evidence, and this returns false if ANY is
// present:
//
//   - a verified PR URL — the strongest, and the only one that is not
//     self-reported;
//   - an affirmative no_work_needed (or blocked, #7924) verdict — the agent
//     reached a conclusion and said so;
//   - an "unknown" completion signal. This is the conservative half of the
//     predicate and it is deliberate: the field is absent from every relay
//     predating #5376 and from the headless path, both of which normalize
//     to "unknown". Keying off the two AFFIRMATIVE signals means this policy
//     changes behaviour only for a relay that has told us how it decided the
//     task was over. No existing relay silently changes behaviour by
//     upgrading the hub.
//
// What remains is a completion with no PR and no conclusion from a relay that
// said how it got there — and neither way is evidence:
//
//   - chrome_idle: the relay inferred completion from idle terminal chrome.
//     That is the #6717 shape exactly: a prompt that was never submitted,
//     reported as done.
//   - verdict: the agent printed `HIVE_VERDICT: complete` but opened nothing.
//     The prompt defines `complete` as "the PR is open"; a `complete` with no
//     PR is a sentence, not a result (#7862 — three in nine tasks from one
//     model, every one of them 30+ read-only tool calls and then the
//     sentinel). It used to book a real completion: TasksCompleted++ and the
//     escalating no-PR cooldown on the issue, parking live work behind prose.
func isEvidenceLessCompletion(prURL, verdict, signal string) bool {
	if strings.TrimSpace(prURL) != "" {
		return false
	}
	if isNoWorkFamilyVerdict(verdict) {
		return false
	}
	switch normalizeCompletionSignal(signal) {
	case completionSignalChromeIdle, completionSignalVerdict:
		return true
	}
	return false
}

// normalizeCompletionSignal maps a client-reported completion_signal onto the
// closed vocabulary above. Client-supplied free text never reaches the hub's
// structured logs: an unrecognised value is reported as unknown, exactly as an
// absent one is.
func normalizeCompletionSignal(reported string) string {
	switch strings.ToLower(strings.TrimSpace(reported)) {
	case completionSignalVerdict:
		return completionSignalVerdict
	case completionSignalChromeIdle:
		return completionSignalChromeIdle
	default:
		return completionSignalUnknown
	}
}

// isSuppressedByNoWorkVerdict reports whether a live no_work_needed verdict
// (#3987) withholds this issue from the contribute OFFER pool. It is consulted
// ONLY by the two offer surfaces — selectTask and ReadyQueue — never by the
// hive agent pipeline, and it can only ever exclude an offer: it closes and
// labels nothing.
//
// Invalidation beats suppression. The verdict is discarded (and the issue
// offerable again) when:
//   - the issue shows activity NEWER than the verdict (issueUpdatedAt after
//     RecordedAt): the maintainer answered, commits or comments landed, the
//     decision the remainder was gated on may have arrived; or
//   - the suppression window (the with-PR cooldown snapshotted at record time)
//     has elapsed.
//
// When the issue's updated_at is UNKNOWN (zero — an older status producer that
// does not carry the field), the check fails OPEN: we cannot prove the issue
// has been quiet since the verdict, and suppressing genuinely reopened work is
// the one failure mode this feature must not have. The record is kept (not
// discarded) so a later snapshot that does carry updated_at can still honour
// or void it. Like every completion cooldown, the operator kill-switch
// disables the exclusion entirely.
func (h *ContributeWSHub) isSuppressedByNoWorkVerdict(repo string, number int, issueUpdatedAt time.Time) bool {
	return h.isSuppressedByNoWorkVerdictKey(worksource.Ref{Repo: repo, Number: number}.Key(), issueUpdatedAt)
}

func (h *ContributeWSHub) isSuppressedByNoWorkVerdictKey(key string, issueUpdatedAt time.Time) bool {
	if !h.cooldownEnabled() {
		return false
	}
	if key == "" {
		return false
	}
	h.completedMu.Lock()
	rec, ok := h.noWorkVerdicts[key]
	if !ok {
		h.completedMu.Unlock()
		return false
	}
	expired := time.Since(rec.RecordedAt) >= rec.suppressWindow()
	voided := !issueUpdatedAt.IsZero() && issueUpdatedAt.After(rec.RecordedAt)
	if expired || voided {
		delete(h.noWorkVerdicts, key)
		h.completedMu.Unlock()
		h.saveNoWorkVerdicts()
		h.logger.Info("[contribute-ws] no-work verdict lifted",
			"key", key, "expired", expired, "voided_by_activity", voided)
		return false
	}
	h.completedMu.Unlock()
	if issueUpdatedAt.IsZero() {
		// Activity unknown → fail open. See doc comment.
		return false
	}
	return true
}

func (h *ContributeWSHub) noWorkVerdictRecordKey(key string) (noWorkVerdictRecord, bool) {
	if h == nil || key == "" {
		return noWorkVerdictRecord{}, false
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	rec, ok := h.noWorkVerdicts[key]
	return rec, ok
}

// issueUpdatedAtFromMap extracts the issue's GitHub updated_at from the
// map[string]any shape selectTask/ReadyQueue read out of ActionableIssues.
// ghpkg.Issue.UpdatedAt marshals as an RFC3339 string; anything absent or
// unparseable yields the zero time, which isSuppressedByNoWorkVerdict treats
// as "activity unknown" (fail open).
func issueUpdatedAtFromMap(issue map[string]any) time.Time {
	s, _ := issue["updated_at"].(string)
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// failedTaskRecord is the on-disk shape of one failed-task entry (#2435). It
// preserves the last-failure time and the running consecutive-failure count so a
// hub restart does not reset a quarantine — otherwise a bounce would re-admit a
// poison issue that had already earned its longer parking window.
type failedTaskRecord struct {
	FailedAt            time.Time `json:"failed_at"`
	ConsecutiveFailures int       `json:"consecutive_failures,omitempty"`
}

func (h *ContributeWSHub) loadFailedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.failedTasksPath())
	if err != nil {
		return
	}
	records := make(map[string]failedTaskRecord)
	if json.Unmarshal(data, &records) != nil {
		return
	}
	for k, rec := range records {
		if rec.FailedAt.IsZero() {
			continue
		}
		// Drop entries already past the longest failure-side window we could have
		// applied (the quarantine cooldown) so we never resurrect a stale park.
		if time.Since(rec.FailedAt) >= quarantineCooldownHours*time.Hour {
			continue
		}
		if h.failedTasks != nil {
			h.failedTasks[k] = rec.FailedAt
		}
		if h.consecutiveFailures != nil && rec.ConsecutiveFailures > 0 {
			h.consecutiveFailures[k] = rec.ConsecutiveFailures
		}
	}
	h.logger.Info("[contribute-ws] loaded failed tasks", "count", len(h.failedTasks))
}

func (h *ContributeWSHub) saveFailedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]failedTaskRecord, len(h.failedTasks))
	for k, t := range h.failedTasks {
		rec := failedTaskRecord{FailedAt: t}
		if h.consecutiveFailures != nil {
			rec.ConsecutiveFailures = h.consecutiveFailures[k]
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] failed tasks marshal failed", "error", err)
		return
	}
	path := h.failedTasksPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] failed tasks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) failedTasksPath() string {
	if h != nil && h.failedTasksFile != "" {
		return h.failedTasksFile
	}
	return failedTasksFile
}

func (h *ContributeWSHub) loadCompletedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	defer h.completedMu.Unlock()
	data, err := os.ReadFile(h.completedTasksPath())
	if err != nil {
		return
	}

	// Accept both the current object form and the legacy map[string]string
	// (key -> RFC3339 timestamp) form so an upgrade never drops cooldowns.
	records := make(map[string]completedTaskRecord)
	if json.Unmarshal(data, &records) != nil {
		var legacy map[string]string
		if json.Unmarshal(data, &legacy) != nil {
			return
		}
		records = make(map[string]completedTaskRecord, len(legacy))
		for k, v := range legacy {
			if t, err := time.Parse(time.RFC3339, v); err == nil {
				records[k] = completedTaskRecord{CompletedAt: t}
			}
		}
	}

	for k, rec := range records {
		if rec.CompletedAt.IsZero() {
			continue
		}
		cooldown := completedTaskCooldownHours * time.Hour
		if rec.CooldownHours > 0 {
			cooldown = time.Duration(rec.CooldownHours * float64(time.Hour))
		}
		// Skip anything already past its own cooldown so we don't resurrect
		// stale locks.
		if time.Since(rec.CompletedAt) >= cooldown {
			continue
		}
		h.completedTasks[k] = rec.CompletedAt
		if h.completedTaskCooldown != nil {
			h.completedTaskCooldown[k] = cooldown
		}
		if h.completedTaskPRURL != nil {
			h.completedTaskPRURL[k] = rec.PRURL
		}
	}
	h.logger.Info("[contribute-ws] loaded completed tasks", "count", len(h.completedTasks))
}

func (h *ContributeWSHub) saveCompletedTasks() {
	if h != nil && !h.persistTaskLedgers {
		return
	}
	h.completedMu.Lock()
	saved := make(map[string]completedTaskRecord, len(h.completedTasks))
	for k, t := range h.completedTasks {
		rec := completedTaskRecord{CompletedAt: t}
		if h.completedTaskCooldown != nil {
			if d, ok := h.completedTaskCooldown[k]; ok {
				rec.CooldownHours = d.Hours()
			}
		}
		if h.completedTaskPRURL != nil {
			rec.PRURL = h.completedTaskPRURL[k]
		}
		saved[k] = rec
	}
	h.completedMu.Unlock()
	data, err := json.Marshal(saved)
	if err != nil {
		h.logger.Warn("[contribute-ws] completed tasks marshal failed", "error", err)
		return
	}
	path := h.completedTasksPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks directory creation failed", "error", err)
		return
	}
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, 0o644); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks write failed", "error", err)
		return
	}
	if err := os.Rename(tmpPath, path); err != nil {
		h.logger.Warn("[contribute-ws] completed tasks rename failed", "error", err)
	}
}

func (h *ContributeWSHub) completedTasksPath() string {
	if h != nil && h.completedTasksFile != "" {
		return h.completedTasksFile
	}
	return completedTasksFile
}

// markTaskCompleted records a completed task and starts its issue cooldown.
//
// The cooldown length is conditional on whether the completion actually shipped
// a pull request (kubestellar/hive#2393 item 7): a completion WITH a prURL gets
// the full completedTaskCooldownHours (real work landed — don't re-dispatch for
// a week), while a completion WITHOUT one — the agent merely returned to idle —
// gets the short completedNoPRCooldownHours so an issue where nothing shipped
// is not locked out for a week. Since #3980 that short cooldown escalates on
// CONSECUTIVE no-PR completions of the same issue (see advanceNoPRStreakLocked)
// so a correct "nothing to ship" verdict cannot loop every 4h indefinitely.
// The chosen expiry is stored per task and honored
// by isTaskInCooldown; the prURL is retained for stats/audit and #2356
// duplicate detection.
func (h *ContributeWSHub) markTaskCompleted(repo string, number int, prURL string) {
	h.markTaskCompletedVerdict(repo, number, prURL, completionVerdictIdle, "", "")
}

// markTaskCompletedVerdict is markTaskCompleted plus the completion's
// normalized verdict (#3987). A no_work_needed verdict — the agent
// affirmatively determined nothing is shippable, as opposed to merely going
// idle — additionally records a durable entry in the noWorkVerdicts ledger,
// which withholds the issue from the OFFER pool for the full with-PR cooldown
// window unless/until the issue shows newer activity (see
// isSuppressedByNoWorkVerdict). The regular escalating no-PR cooldown is still
// booked as the backstop, so behavior for relays that never learn the verdict
// field — and for hubs where the verdict is later voided — is unchanged.
// A blocked verdict (#7924) books the full with-PR cooldown outright instead
// of the ladder. reporter/reason are audit-only and stored with the verdict.
func (h *ContributeWSHub) markTaskCompletedVerdict(repo string, number int, prURL, verdict, reporter, reason string) {
	h.markTaskCompletedVerdictKey(worksource.Ref{Repo: repo, Number: number}.Key(), prURL, verdict, reporter, reason)
}

func (h *ContributeWSHub) markAlreadyDoneUnverified(repo string, number int, reporter, reason string, evidence *VerdictEvidence) {
	if h == nil || repo == "" || number <= 0 {
		return
	}
	if len(reason) > noWorkReasonMaxLen {
		reason = reason[:noWorkReasonMaxLen]
	}
	claim := ghpkg.IssueClaim{Repo: repo, Issue: number, Source: ghpkg.ClaimSourceVerdict, SourceReporter: reporter}
	if evidence != nil {
		claim.PRNumber = evidence.PR
		if evidence.PR > 0 {
			claim.PRRepo = repo
		}
	}
	if err := h.markAlreadyDoneIssue(context.Background(), repo, number, claim, reporter, false); err != nil && h.logger != nil && err != ghpkg.ErrNoGitHubClient {
		h.logger.Warn("[contribute-ws] already-done verdict label failed",
			"repo", repo, "number", number, "error", err.Error())
	}
	hold := h.configuredAlreadyDoneUnverifiedHold()
	rec := noWorkVerdictRecord{
		RecordedAt:            time.Now(),
		SuppressHours:         hold.Hours(),
		Reporter:              reporter,
		Reason:                reason,
		ReasonKind:            verdictReasonKindAlreadyDone,
		AlreadyDoneUnverified: true,
	}
	if evidence != nil {
		rec.EvidencePR = evidence.PR
		rec.EvidenceCommit = strings.TrimSpace(evidence.Commit)
	}
	key := worksource.Ref{Repo: repo, Number: number}.Key()
	h.completedMu.Lock()
	h.noWorkVerdicts[key] = rec
	h.completedMu.Unlock()
	h.saveNoWorkVerdicts()
}

// markTaskCompletedVerdictKey books completion against the canonical identity
// (kubestellar/hive#4245), so a Linear or Jira task's cooldown and verdict land
// on that item rather than on a shared "repo#0" record that would suppress
// every other zero-numbered item in the repository.
func (h *ContributeWSHub) markTaskCompletedVerdictKey(key string, prURL, verdict, reporter, reason string) {
	h.markTaskCompletedVerdictKeySignal(key, prURL, verdict, reporter, reason, completionSignalUnknown)
}

// markTaskCompletedVerdictKeySignal is markTaskCompletedVerdictKey plus the
// completion SIGNAL (#6723), which decides whether this completion carries
// evidence at all.
//
// #6717 showed a task reported complete by the chrome_idle fallback when the
// prompt was never submitted: no commit, no branch, no PR, no HIVE_VERDICT.
// That completion still escalated the no-PR streak and still cleared the
// issue's failure history, so repeated false completions walked the cooldown
// up to the full with-PR window and parked live work. A false completion is
// strictly worse than a failure — failures are re-offered, false completions
// silently strand the issue.
//
// So an EVIDENCE-LESS completion (defined by isEvidenceLessCompletion) books
// only the flat base cooldown, never escalates, and leaves failure history
// intact. It is loop-protected but re-offered, which is the behaviour the
// absent evidence actually justifies.
func (h *ContributeWSHub) markTaskCompletedVerdictKeySignal(key string, prURL, verdict, reporter, reason, signal string) {
	if key == "" {
		return
	}
	// The WITH-PR period is operator-tunable (Config.Hub.ContributeCooldownHours,
	// default completedTaskCooldownHours). We still RECORD this even when cooldown
	// is disabled — isTaskInCooldown short-circuits the gating, so the history is
	// kept for stats/audit but never excludes the issue. It also caps the no-PR
	// escalation below. Read before taking completedMu: it walks server config,
	// which the lock has no business covering.
	withPRCooldown := h.configuredWithPRCooldown()
	evidenceLess := isEvidenceLessCompletion(prURL, verdict, signal)
	h.completedMu.Lock()
	var cooldown time.Duration
	verdictLedgerDirty := false
	if prURL != "" {
		cooldown = withPRCooldown
		// A shipped, verified PR ends any no-PR streak: the issue's next no-PR
		// completion (if any) starts back at the short base cooldown.
		delete(h.noPRStreaks, key)
		// It also voids any standing no_work_needed verdict (#3987): real work
		// just landed, so "nothing is shippable" is no longer true.
		if _, had := h.noWorkVerdicts[key]; had {
			delete(h.noWorkVerdicts, key)
			verdictLedgerDirty = true
		}
	} else if evidenceLess {
		// #6723/#7862: no PR and no affirmative no_work_needed — the relay
		// either fell back to idle-looking terminal chrome or read a bare
		// `complete` with nothing behind it, and has told us which. Neither is
		// evidence that the work was even attempted, so it must not escalate.
		// A flat base cooldown keeps the issue out of a tight re-offer loop
		// (#2492/#2557) while leaving it offerable.
		cooldown = completedNoPRCooldownHours * time.Hour
	} else if verdict == completionVerdictBlocked || verdict == completionVerdictNeedsDecision {
		// #7924: blocked on something outside the repo. Nothing a retry can
		// do until that lands, so the no-PR ladder's first rungs (4h, 8h, …)
		// are pure cost: every retry re-runs the same research to re-find the
		// same external dependency. Book the FULL with-PR cooldown at once —
		// one wasted cycle per cooldown period at worst — and record the
		// verdict with its marker so the ledger says what the issue is
		// waiting on. The relay, when its credential allows, also applies the
		// repo's `blocked` label, and that admission gate then holds the issue
		// past this cooldown until a human clears it. The no-PR streak is
		// left alone: it exists to escalate, and this is already the ceiling.
		cooldown = withPRCooldown
		if len(reason) > noWorkReasonMaxLen {
			reason = reason[:noWorkReasonMaxLen]
		}
		h.noWorkVerdicts[key] = noWorkVerdictRecord{
			RecordedAt:    time.Now(),
			SuppressHours: withPRCooldown.Hours(),
			Reporter:      reporter,
			Reason:        reason,
			Blocked:       verdict == completionVerdictBlocked,
			NeedsDecision: verdict == completionVerdictNeedsDecision,
		}
		verdictLedgerDirty = true
	} else {
		// #3980: repeated no-PR completions escalate geometrically (4h → 8h →
		// …, capped at the with-PR cooldown) so a "nothing to ship" loop —
		// e.g. an issue whose remaining work is maintainer-gated (#2547) —
		// cannot burn a full contributor task cycle every 4h forever.
		cooldown = h.advanceNoPRStreakLocked(key, withPRCooldown)
		// #3987: an affirmative no_work_needed verdict eliminates that loop
		// rather than just bounding it — the issue is withheld from the offer
		// pool for the full with-PR window, subject to invalidation by newer
		// issue activity. Only the OFFER surfaces read this ledger; the
		// escalating cooldown above still stands as the backstop.
		if verdict == completionVerdictNoWorkNeeded {
			if len(reason) > noWorkReasonMaxLen {
				reason = reason[:noWorkReasonMaxLen]
			}
			h.noWorkVerdicts[key] = noWorkVerdictRecord{
				RecordedAt:    time.Now(),
				SuppressHours: withPRCooldown.Hours(),
				Reporter:      reporter,
				Reason:        reason,
			}
			verdictLedgerDirty = true
		}
	}
	h.completedTasks[key] = time.Now()
	if h.completedTaskCooldown != nil {
		h.completedTaskCooldown[key] = cooldown
	}
	if h.completedTaskPRURL != nil {
		h.completedTaskPRURL[key] = prURL
	}
	// A completion clears any failure history for the issue (#2435): the
	// consecutive-failure counter resets so a flaky-then-fixed issue does not
	// carry a stale quarantine, and the short failure cooldown is superseded by
	// the (longer) completion cooldown recorded just above.
	//
	// #6723: an evidence-less completion is exempt. Clearing the quarantine
	// counter on it lets an issue that fails, then false-completes, then fails
	// again evade consecutive-failure quarantine forever — the counter is reset
	// by the very signal that indicates nothing was attempted.
	failureCleared := false
	if !evidenceLess {
		if h.failedTasks != nil {
			if _, ok := h.failedTasks[key]; ok {
				delete(h.failedTasks, key)
				failureCleared = true
			}
		}
		if h.consecutiveFailures != nil {
			if _, ok := h.consecutiveFailures[key]; ok {
				delete(h.consecutiveFailures, key)
				failureCleared = true
			}
		}
	}
	h.completedMu.Unlock()
	h.saveCompletedTasks()
	h.saveNoPRStreaks()
	if verdictLedgerDirty {
		h.saveNoWorkVerdicts()
	}
	if failureCleared {
		h.saveFailedTasks()
	}
}
