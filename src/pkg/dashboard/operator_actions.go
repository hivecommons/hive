package dashboard

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Operator action ledger (hivecommons/hive: "I need a way to see if I
// selected an action on an item so I can keep track of whether or not I
// addressed it"). Every owner row action the activity monitor submits —
// approve, give-direction, close-superseded, wontfix, let-hive-decide,
// hold / release, accept-triage, mark-needs-human — is appended here on the
// server, keyed by repo#number, so the ✓ badge survives reloads and shows in
// every browser. The record is written by wrapping the existing endpoints
// (recordOperatorAction): it never changes what they do, only remembers
// that they were asked and what they answered. The stage tracker derives
// the row's `last_operator_action` from it (stageTrackerOperatorAction),
// and GET /api/repos/{owner}/{repo}/items/{number} lists the whole history.

const (
	// Canonical action names (the dashboard's menu ids map onto these).
	operatorActionApprove         = "approve"
	operatorActionGiveDirection   = "give-direction"
	operatorActionCloseSuperseded = "close-superseded"
	operatorActionWontFix         = "wontfix"
	operatorActionLetHiveDecide   = "let-hive-decide"
	operatorActionHold            = "hold"
	operatorActionRelease         = "release"
	operatorActionAcceptTriage    = "accept-triage"
	operatorActionMarkNeedsHuman  = "mark-needs-human"
	// Merge-gate answers on a 090 Ready-for-human-merge PR
	// (hivecommons/hive#15078): merged by the operator through the hive's
	// App, or queued for the hive's merge sweep.
	operatorActionMergeNow   = "merge-now"
	operatorActionQueueMerge = "queue-merge"

	// Stored outcomes: what the endpoint answered when the action was
	// submitted. `pending` is a delegated decision still running; the job's
	// end settles it (settleOperatorAction).
	operatorActionOutcomePending = "pending"
	operatorActionOutcomeApplied = "applied"
	operatorActionOutcomeNoOp    = "no-op"
	operatorActionOutcomeFailed  = "failed"
	// Derived only: a failed / no-op action whose item moved stage anyway.
	operatorActionOutcomeSuperseded = "superseded-by-state-change"

	// Derived row states (StageTrackerOperatorAction.State).
	operatorActionStateWaiting   = "waiting"
	operatorActionStateAddressed = "addressed"
	operatorActionStateStuck     = "stuck"
	operatorActionStateFailed    = "failed"
	operatorActionStatePending   = "pending"

	operatorActionsMaxPerItem       = 20
	operatorActionNoteMax           = 280
	operatorActionBodyMax           = 64 << 10
	operatorActionStuckAfterDefault = 30 * time.Minute
	// HIVE_OPERATOR_ACTION_STUCK_AFTER (Go duration) overrides how long a
	// recorded action may sit at the same stage before the badge turns amber.
	operatorActionStuckAfterEnv = "HIVE_OPERATOR_ACTION_STUCK_AFTER"
)

// operatorActionsPath is where the ledger persists; tests point it at a
// temp file (or "" for memory only).
var operatorActionsPath = "/data/operator-actions.json"

// OperatorAction is one recorded row action.
type OperatorAction struct {
	Action string    `json:"action"`
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
	// Note is the comment / direction / reason the operator typed
	// (truncated); Target names what the action pointed at (the superseding
	// PR of a close-superseded).
	Note    string `json:"note,omitempty"`
	Target  string `json:"target,omitempty"`
	Outcome string `json:"outcome"`
	// Detail is the endpoint's status word (unparked, approval-commented,
	// closed, …), the delegated verdict, or the error on a failure.
	Detail  string `json:"detail,omitempty"`
	HeadSHA string `json:"head_sha,omitempty"`
	// Stage is the item's stage number when the action was recorded (0 when
	// unknown), so the tracker can tell "still at 085" from "moved on".
	Stage int `json:"stage,omitempty"`
}

type operatorActionStore struct {
	mu     sync.Mutex
	path   string
	loaded bool
	items  map[string][]OperatorAction
}

func newOperatorActionStore(path string) *operatorActionStore {
	return &operatorActionStore{path: path, items: map[string][]OperatorAction{}}
}

func operatorActionKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func (st *operatorActionStore) load() {
	if st.loaded {
		return
	}
	st.loaded = true
	if st.path == "" {
		return
	}
	data, err := os.ReadFile(st.path)
	if err != nil {
		return
	}
	var items map[string][]OperatorAction
	if json.Unmarshal(data, &items) == nil && items != nil {
		st.items = items
	}
}

func (st *operatorActionStore) persist() error {
	if st.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(st.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(st.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := st.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, st.path)
}

// Record appends one action for repo#number, keeping the newest
// operatorActionsMaxPerItem.
func (st *operatorActionStore) Record(repo string, number int, a OperatorAction) error {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	key := operatorActionKey(repo, number)
	list := append(st.items[key], a)
	if len(list) > operatorActionsMaxPerItem {
		list = list[len(list)-operatorActionsMaxPerItem:]
	}
	st.items[key] = list
	return st.persist()
}

// Settle updates the newest pending record of `action` on repo#number
// (a delegated decision whose job just ended). Returns false when there is
// none.
func (st *operatorActionStore) Settle(repo string, number int, action, outcome, detail string) (bool, error) {
	if st == nil {
		return false, nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	list := st.items[operatorActionKey(repo, number)]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Action == action && list[i].Outcome == operatorActionOutcomePending {
			list[i].Outcome = outcome
			list[i].Detail = detail
			return true, st.persist()
		}
	}
	return false, nil
}

// List returns a copy of the actions recorded for repo#number, oldest first.
func (st *operatorActionStore) List(repo string, number int) []OperatorAction {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	return slices.Clone(st.items[operatorActionKey(repo, number)])
}

// Keys returns every repo#number with at least one record.
func (st *operatorActionStore) Keys() []string {
	if st == nil {
		return nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	st.load()
	out := make([]string, 0, len(st.items))
	for k, v := range st.items {
		if len(v) > 0 {
			out = append(out, k)
		}
	}
	return out
}

// operatorActions is the server's ledger, opened on first use.
func (s *Server) operatorActions() *operatorActionStore {
	if s == nil {
		return nil
	}
	s.operatorActionsOnce.Do(func() {
		if s.operatorActionStore == nil {
			s.operatorActionStore = newOperatorActionStore(operatorActionsPath)
		}
	})
	return s.operatorActionStore
}

// operatorActionStuckAfter is how long a recorded action may sit at the
// same stage before the row's badge turns amber.
func operatorActionStuckAfter() time.Duration {
	if raw := strings.TrimSpace(os.Getenv(operatorActionStuckAfterEnv)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return operatorActionStuckAfterDefault
}

// operatorActionRequest is the request-side shape of a recorded action:
// which canonical name it takes and what the operator attached to it.
type operatorActionRequest struct {
	Action string
	Note   string
	Target string
	Head   string
}

// operatorActionFromBody maps one endpoint's request body onto a canonical
// action. endpoint is the route's last path segment. ok=false means the
// request names no recordable action (an unknown decision verb, say).
func operatorActionFromBody(endpoint string, body []byte) (operatorActionRequest, bool) {
	var raw struct {
		Decision     string `json:"decision"`
		Comment      string `json:"comment"`
		SupersededBy string `json:"superseded_by"`
		Held         *bool  `json:"held"`
		Reason       string `json:"reason"`
		Head         string `json:"head"`
	}
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &raw)
	}
	out := operatorActionRequest{Head: strings.TrimSpace(raw.Head)}
	switch endpoint {
	case "decision":
		switch strings.ToLower(strings.TrimSpace(raw.Decision)) {
		case repoItemDecisionApprove:
			out.Action = operatorActionApprove
		case repoItemDecisionDirection:
			out.Action = operatorActionGiveDirection
		case repoItemDecisionCloseSuperseded:
			out.Action = operatorActionCloseSuperseded
			out.Target = strings.TrimSpace(raw.SupersededBy)
		case repoItemDecisionCloseWontFix:
			out.Action = operatorActionWontFix
		case repoItemDecisionMergeNow:
			out.Action = operatorActionMergeNow
		case repoItemDecisionQueueMerge:
			out.Action = operatorActionQueueMerge
		default:
			return out, false
		}
		out.Note = truncateOperatorNote(raw.Comment)
	case "decide-delegated":
		out.Action = operatorActionLetHiveDecide
	case "hold":
		if raw.Held == nil {
			return out, false
		}
		out.Action = operatorActionRelease
		if *raw.Held {
			out.Action = operatorActionHold
		}
	case "accept-triage":
		out.Action = operatorActionAcceptTriage
	case "send-to-human":
		out.Action = operatorActionMarkNeedsHuman
		out.Note = truncateOperatorNote(raw.Reason)
	default:
		return out, false
	}
	return out, true
}

func truncateOperatorNote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > operatorActionNoteMax {
		return s[:operatorActionNoteMax-1] + "…"
	}
	return s
}

// operatorActionOutcome reads the endpoint's answer: HTTP status plus the
// JSON it wrote. record=false means the request never became an action
// (rejected before anything was asked of GitHub: bad body, no permission,
// unknown item).
func operatorActionOutcome(status int, body []byte) (outcome, detail, head string, record bool) {
	var resp map[string]any
	if len(bytes.TrimSpace(body)) > 0 {
		_ = json.Unmarshal(body, &resp)
	}
	str := func(k string) string {
		v, _ := resp[k].(string)
		return strings.TrimSpace(v)
	}
	head = str("head")
	switch {
	case status == http.StatusAccepted:
		return operatorActionOutcomePending, str("status"), head, true
	case status >= 200 && status < 300:
		if already, _ := resp["alreadyAccepted"].(bool); already {
			return operatorActionOutcomeNoOp, "already accepted", head, true
		}
		detail = str("status")
		if detail == "" {
			if released, ok := resp["released"].([]any); ok && len(released) > 0 {
				parts := make([]string, 0, len(released))
				for _, r := range released {
					if s, ok := r.(string); ok {
						parts = append(parts, s)
					}
				}
				detail = strings.Join(parts, ", ")
			}
		}
		if res, ok := resp["result"].(map[string]any); ok {
			if action, _ := res["action"].(string); action == rowActionNoop {
				verdict := ""
				if d, ok := resp["decision"].(map[string]any); ok {
					verdict, _ = d["verdict"].(string)
				}
				var rr RowActionResult
				if raw, err := json.Marshal(res); err == nil {
					_ = json.Unmarshal(raw, &rr)
				}
				return operatorActionOutcomeNoOp, operatorActionNoopDetail(verdict, &rr), head, true
			}
		}
		if d, ok := resp["decision"].(map[string]any); ok && status == http.StatusOK {
			if v, _ := d["verdict"].(string); v != "" {
				detail = "hive decided: " + v
				if rule, _ := d["rule"].(string); rule != "" {
					detail += " (" + rule + ")"
				}
			}
			if st, _ := resp["status"].(string); st == delegatedJobStatusFailed {
				return operatorActionOutcomeFailed, str("error"), head, true
			}
		}
		return operatorActionOutcomeApplied, detail, head, true
	case status == http.StatusConflict:
		return operatorActionOutcomeNoOp, str("error"), head, true
	case status >= 500:
		return operatorActionOutcomeFailed, str("error"), head, true
	default:
		return "", "", head, false
	}
}

// operatorActionWriter captures what an item-action handler answers so the
// ledger can record it after the fact.
type operatorActionWriter struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
	// afterRecord starts delegated work only after the pending row is written.
	afterRecord func()
}

func (w *operatorActionWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *operatorActionWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.body.Len() < operatorActionBodyMax {
		w.body.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

func (w *operatorActionWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recordOperatorAction wraps an item-action handler so the action it
// performs lands in the ledger with the outcome the handler answered. The
// handler itself is untouched: the body is re-supplied to it and the
// response passes straight through.
func (s *Server) recordOperatorAction(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, operatorActionBodyMax))
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		segs := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		endpoint := segs[len(segs)-1]
		req, known := operatorActionFromBody(endpoint, body)
		cw := &operatorActionWriter{ResponseWriter: w}
		defer func() {
			if cw.afterRecord != nil {
				cw.afterRecord()
			}
		}()
		h(cw, r)
		if !known {
			return
		}
		outcome, detail, head, record := operatorActionOutcome(cw.status, cw.body.Bytes())
		if !record {
			return
		}
		owner, repoName := r.PathValue("owner"), r.PathValue("repo")
		number, err := strconv.Atoi(r.PathValue("number"))
		if owner == "" || repoName == "" || err != nil || number <= 0 {
			return
		}
		repoFull := owner + "/" + repoName
		if head == "" {
			head = req.Head
		}
		a := OperatorAction{
			Action: req.Action, Actor: requestUser(r), At: time.Now().UTC(),
			Note: req.Note, Target: req.Target, Outcome: outcome, Detail: detail, HeadSHA: head,
		}
		if rec, ok := s.stageStoreOrShared().Get(stagelabels.Key(repoFull, number)); ok {
			a.Stage = rec.Stage.Number()
		}
		if err := s.operatorActions().Record(repoFull, number, a); err != nil && s.logger != nil {
			s.logger.Warn("operator action ledger write failed", "path", operatorActionsPath, "error", err)
		}
		s.invalidateStageTrackerCache()
	}
}

// settleOperatorAction closes the pending let-hive-decide record once the
// delegated job ends.
func (s *Server) settleOperatorAction(repoFull string, number int, job *delegatedDecisionJob) {
	if s == nil || job == nil {
		return
	}
	outcome, detail := operatorActionOutcomeApplied, ""
	if job.Decision != nil {
		detail = "hive decided: " + string(job.Decision.Verdict)
		if job.Decision.Rule != "" {
			detail += " (" + job.Decision.Rule + ")"
		}
		if job.Decision.KeepsPark {
			detail += " — kept for the owner"
		}
	}
	if job.Result != nil && job.Result.Action == rowActionNoop {
		verdict := ""
		if job.Decision != nil {
			verdict = string(job.Decision.Verdict)
		}
		outcome, detail = operatorActionOutcomeNoOp, operatorActionNoopDetail(verdict, job.Result)
	}
	if job.Status == delegatedJobStatusFailed {
		outcome, detail = operatorActionOutcomeFailed, job.Error
	}
	if _, err := s.operatorActions().Settle(repoFull, number, operatorActionLetHiveDecide, outcome, detail); err != nil && s.logger != nil {
		s.logger.Warn("operator action ledger write failed", "path", operatorActionsPath, "error", err)
	}
	s.invalidateStageTrackerCache()
}

// operatorActionNoopDetail spells a repeat decision out in plain words:
// what the hive had already decided, when, and what still blocks it.
func operatorActionNoopDetail(verdict string, res *RowActionResult) string {
	out := "the hive had already decided"
	if verdict != "" {
		out += " '" + verdict + "'"
	}
	if res != nil && res.AppliedAt != nil && !res.AppliedAt.IsZero() {
		out += " at " + res.AppliedAt.UTC().Format("15:04 UTC")
	}
	if res != nil {
		if b, ok := strings.CutPrefix(res.Next, "Still blocked by "); ok {
			out += " and it has not taken effect — still blocked: " + strings.TrimSuffix(b, ".")
		}
	}
	return out
}

// StageTrackerOperatorAction is the row-payload view of the newest recorded
// action: the record plus what the tracker can say about it now.
type StageTrackerOperatorAction struct {
	Action  string    `json:"action"`
	Actor   string    `json:"actor"`
	At      time.Time `json:"at"`
	Note    string    `json:"note,omitempty"`
	Target  string    `json:"target,omitempty"`
	Outcome string    `json:"outcome"`
	Detail  string    `json:"detail,omitempty"`
	HeadSHA string    `json:"head_sha,omitempty"`
	// StageAt is the stage the item was on when the action was recorded.
	StageAt int `json:"stage_at,omitempty"`
	// State is the at-a-glance verdict: addressed (the item moved on, or
	// the control the action asked for is in effect), waiting (same stage,
	// recently), stuck (same stage past the stuck window), pending (a
	// delegated decision still running) or failed.
	State string `json:"state"`
	// StuckAfterSeconds is the window `stuck` is judged against.
	StuckAfterSeconds int64 `json:"stuck_after_seconds,omitempty"`
	// Followed is what the hive did next: the stage hops recorded after the
	// action, oldest first (capped), so the row can tell the decision trail
	// — owner's choice, then the hive's moves — in order.
	Followed []StageTrackerHop `json:"followed,omitempty"`
}

const operatorActionFollowedMax = 5

// operatorActionFollowed picks the hops that came after the action.
func operatorActionFollowed(a OperatorAction, history []StageTrackerHop) []StageTrackerHop {
	var out []StageTrackerHop
	for _, h := range history {
		if !a.At.IsZero() && h.At.After(a.At) {
			out = append(out, h)
		}
	}
	if len(out) > operatorActionFollowedMax {
		out = out[len(out)-operatorActionFollowedMax:]
	}
	return out
}

// operatorActionAddressed says whether the item now shows what the action
// asked for: a hold / release / needs-human by its modifier, anything else
// by having left the stage it was recorded on (a close leaves the list).
func operatorActionAddressed(a OperatorAction, stage int, modifiers []string) bool {
	switch a.Action {
	case operatorActionHold:
		return slices.Contains(modifiers, modHold)
	case operatorActionRelease:
		return !slices.Contains(modifiers, modHold)
	case operatorActionMarkNeedsHuman:
		return slices.Contains(modifiers, modNeedsHuman)
	case operatorActionAcceptTriage:
		return !slices.Contains(modifiers, modNeedsTriage) && stage != stagelabels.TriageNeeded.Number()
	}
	return a.Stage != 0 && stage != a.Stage
}

// stageTrackerOperatorAction derives the row view of the newest record.
func stageTrackerOperatorAction(actions []OperatorAction, stage int, modifiers []string, history []StageTrackerHop, now time.Time, stuckAfter time.Duration) *StageTrackerOperatorAction {
	if len(actions) == 0 {
		return nil
	}
	a := actions[len(actions)-1]
	out := &StageTrackerOperatorAction{
		Action: a.Action, Actor: a.Actor, At: a.At, Note: a.Note, Target: a.Target, Outcome: a.Outcome, Detail: a.Detail,
		HeadSHA: a.HeadSHA, StageAt: a.Stage, StuckAfterSeconds: int64(stuckAfter / time.Second),
		Followed: operatorActionFollowed(a, history),
	}
	addressed := operatorActionAddressed(a, stage, modifiers)
	switch {
	case a.Outcome == operatorActionOutcomePending:
		out.State = operatorActionStatePending
	case addressed:
		out.State = operatorActionStateAddressed
		if a.Outcome == operatorActionOutcomeFailed || a.Outcome == operatorActionOutcomeNoOp {
			out.Outcome = operatorActionOutcomeSuperseded
		}
	case a.Outcome == operatorActionOutcomeFailed:
		out.State = operatorActionStateFailed
	case !a.At.IsZero() && now.Sub(a.At) >= stuckAfter:
		out.State = operatorActionStateStuck
	default:
		out.State = operatorActionStateWaiting
	}
	return out
}

// applyOperatorActions stamps last_operator_action / operator_action_count
// onto every tracker item with a record.
func (s *Server) applyOperatorActions(items []StageTrackerItem, now time.Time) {
	st := s.operatorActions()
	if st == nil {
		return
	}
	keys := st.Keys()
	if len(keys) == 0 {
		return
	}
	have := map[string]bool{}
	for _, k := range keys {
		have[k] = true
	}
	stuckAfter := operatorActionStuckAfter()
	for i := range items {
		it := &items[i]
		if !have[operatorActionKey(it.Repo, it.Number)] {
			continue
		}
		actions := st.List(it.Repo, it.Number)
		it.OperatorActionCount = len(actions)
		it.LastOperatorAction = stageTrackerOperatorAction(actions, it.Stage, it.Modifiers, it.History, now, stuckAfter)
	}
}

// repoItemDetailResponse is GET /api/repos/{owner}/{repo}/items/{number}.
type repoItemDetailResponse struct {
	Repo                string                      `json:"repo"`
	Number              int                         `json:"number"`
	Stage               int                         `json:"stage,omitempty"`
	StageSlug           string                      `json:"stage_slug,omitempty"`
	OperatorActions     []OperatorAction            `json:"operator_actions"`
	OperatorActionCount int                         `json:"operator_action_count"`
	LastOperatorAction  *StageTrackerOperatorAction `json:"last_operator_action,omitempty"`
}

// handleRepoItemDetail answers the per-item detail: the stage the ledger
// knows and the full operator-action history.
func (s *Server) handleRepoItemDetail(w http.ResponseWriter, r *http.Request) {
	owner, repoName, number, ok := s.parseRepoItemPath(w, r)
	if !ok {
		return
	}
	repoFull := owner + "/" + repoName
	actions := s.operatorActions().List(repoFull, number)
	if actions == nil {
		actions = []OperatorAction{}
	}
	resp := repoItemDetailResponse{Repo: repoFull, Number: number, OperatorActions: actions, OperatorActionCount: len(actions)}
	var modifiers []string
	var history []StageTrackerHop
	if rec, ok := s.stageStoreOrShared().Get(stagelabels.Key(repoFull, number)); ok && rec.Stage != stagelabels.None {
		resp.Stage = rec.Stage.Number()
		resp.StageSlug = rec.Stage.Slug()
		history = stageTrackerHistory(rec)
	}
	if it, ok := s.stageTrackerItemFor(repoFull, number); ok {
		resp.Stage, resp.StageSlug, modifiers, history = it.Stage, it.StageSlug, it.Modifiers, it.History
	}
	resp.LastOperatorAction = stageTrackerOperatorAction(actions, resp.Stage, modifiers, history, time.Now().UTC(), operatorActionStuckAfter())
	jsonResponse(w, resp)
}

// stageTrackerItemFor finds repo#number in the current tracker payload.
func (s *Server) stageTrackerItemFor(repoFull string, number int) (StageTrackerItem, bool) {
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	if status == nil {
		return StageTrackerItem{}, false
	}
	body, _, _, _ := s.stageTrackerPayload(status, time.Now().UTC())
	if body == nil {
		return StageTrackerItem{}, false
	}
	var resp StageTrackerResponse
	if json.Unmarshal(body, &resp) != nil {
		return StageTrackerItem{}, false
	}
	for _, it := range resp.Items {
		if it.Number == number && strings.EqualFold(it.Repo, repoFull) {
			return it, true
		}
	}
	return StageTrackerItem{}, false
}
