package review

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// HumanChangeRequestsFile is the durable record of maintainer change requests
// on hive-authored PRs (hivecommons/hive#13041). It lives next to the dispatch
// state on the durable data dir for the same reason: a pod restart between a
// maintainer's review and the next planner cycle must not lose the request.
const HumanChangeRequestsFile = "review-human-change-requests.json"

// HumanChangeRequestsPath is where DefaultHumanChangeRequests persists. A
// var so tests can point it elsewhere.
var HumanChangeRequestsPath = filepath.Join(DefaultDispatchStateDir, HumanChangeRequestsFile)

// Sources of a HumanChangeRequest.
const (
	// HumanChangeRequestSourceReview is a trusted human's CHANGES_REQUESTED
	// review on the PR head.
	HumanChangeRequestSourceReview = "review"
	// HumanChangeRequestSourceDecision is a trusted human's `/hive decision
	// <text>` comment on the PR while it sat at stage 085.
	HumanChangeRequestSourceDecision = "decision"
	// HumanChangeRequestSourceStageLabel is a trusted human moving the PR
	// back to its author by hand — adding stage/071-needs-author or
	// stage/072-returned-to-contributor — while the hive had it parked for
	// a person (hivecommons/hive#15159). SourceID is the label event.
	HumanChangeRequestSourceStageLabel = "stage-label"
)

// Kinds of a HumanChangeRequest.
const (
	// HumanChangeRequestKindChanges (the zero value, "" in older files) is
	// an instruction the author agent must act on.
	HumanChangeRequestKindChanges = ""
	// HumanChangeRequestKindApprove is a bare `/hive decision approve`: the
	// owner answered the swarm's question with "yes" and gave nothing to
	// change. It supersedes the requires_human verdict on Head so the lane
	// stops reporting decision-requested, but it never kicks a fix
	// (hivecommons/hive#14679).
	HumanChangeRequestKindApprove = "approve"
)

// HumanChangeRequest is one maintainer instruction that sends a hive-authored
// PR head back to its author agent: a CHANGES_REQUESTED review on the head,
// or a `/hive decision <text>` comment while the PR waits at
// stage/085-needs-decision. The planner turns it into a fix kick whose
// prompt quotes Body verbatim; the self-authored automerge lane reads it as
// "changes requested" for Head and never merges that head.
type HumanChangeRequest struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha"`
	// Source is HumanChangeRequestSourceReview, ...Decision or ...StageLabel.
	Source string `json:"source"`
	// SourceID is the review ID or comment ID: with HeadSHA it is the
	// dedupe key, so the same review is never recorded (or kicked) twice.
	SourceID int64 `json:"source_id"`
	// Actor is the maintainer's login.
	Actor string `json:"actor"`
	// Body is the review body or decision text, verbatim.
	Body string `json:"body"`
	// Kind is HumanChangeRequestKindChanges or ...Approve.
	Kind string `json:"kind,omitempty"`
	// URL is the review's or comment's HTML URL, when known.
	URL        string    `json:"url,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
	// CarriedFromHead is the head the person actually decided on when the
	// hive moved this record forward over its own base refresh: the new
	// head is a merge of the base branch into CarriedFromHead that left the
	// PR's diff untouched (hivecommons/hive#15649 → see CarryOverBaseRefresh).
	CarriedFromHead string `json:"carried_from_head,omitempty"`
}

// CarriesOverBaseRefresh reports whether the request may follow the PR onto
// a head that is a pure base refresh of the one it was recorded on. Only a
// person's own instruction travels — a `/hive decision`, a dashboard Approve
// or a hand-applied stage label. A GitHub review names one commit and is
// never moved to another.
func (r HumanChangeRequest) CarriesOverBaseRefresh() bool {
	return r.Source != HumanChangeRequestSourceReview && strings.TrimSpace(r.Actor) != ""
}

// IsApproval reports whether the request is a bare approval rather than an
// instruction to change the head.
func (r HumanChangeRequest) IsApproval() bool {
	return r.Kind == HumanChangeRequestKindApprove
}

// Decision directives: what a recorded `/hive decision` told the hive to do
// with the head it names. They ride on the reviewer verdict for that head
// (prstage.ReviewVerdict.DecisionDirective) so every stage derivation reads
// the requires_human / decision-requested question as answered.
const (
	DecisionDirectiveApprove = "approve"
	DecisionDirectiveChanges = "changes_requested"
)

// Directive is the decision directive the request carries.
func (r HumanChangeRequest) Directive() string {
	if r.IsApproval() {
		return DecisionDirectiveApprove
	}
	return DecisionDirectiveChanges
}

// HumanChangeRequestStore records maintainer change requests and answers
// which one, if any, stands against a PR head. Implementations are safe for
// concurrent use: the automerge sweep records while the planner reads.
type HumanChangeRequestStore interface {
	// Record stores req, replacing an earlier entry with the same
	// (repo, number, head, source id) and dropping entries for the same PR
	// at other heads: a new head supersedes every request on the old one.
	Record(req HumanChangeRequest) error
	// For returns the newest request recorded against repo#number at head.
	For(repo string, number int, head string) (HumanChangeRequest, bool)
	// Forget drops the entry recorded from source/sourceID against
	// repo#number, leaving every other entry (an older instruction on the
	// same head included) standing. Unknown entries are a no-op.
	Forget(repo string, number int, source string, sourceID int64) error
}

// HumanChangeRequests is the file-backed HumanChangeRequestStore. A zero
// path keeps the store in memory only.
type HumanChangeRequests struct {
	path string

	mu     sync.Mutex
	loaded bool
	items  []HumanChangeRequest
}

// NewHumanChangeRequests returns a store persisted at path ("" for memory
// only).
func NewHumanChangeRequests(path string) *HumanChangeRequests {
	return &HumanChangeRequests{path: path}
}

var (
	defaultHumanChangeRequestsOnce sync.Once
	defaultHumanChangeRequests     *HumanChangeRequests
)

// DefaultHumanChangeRequests is the process-wide store at
// HumanChangeRequestsPath, shared by the automerge sweep, the un-park sweep
// and the review planner.
func DefaultHumanChangeRequests() *HumanChangeRequests {
	defaultHumanChangeRequestsOnce.Do(func() {
		defaultHumanChangeRequests = NewHumanChangeRequests(HumanChangeRequestsPath)
	})
	return defaultHumanChangeRequests
}

func (s *HumanChangeRequests) load() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var items []HumanChangeRequest
	if json.Unmarshal(data, &items) == nil {
		s.items = items
	}
}

func (s *HumanChangeRequests) persist() error {
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s.items, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// Record implements HumanChangeRequestStore.
func (s *HumanChangeRequests) Record(req HumanChangeRequest) error {
	if s == nil {
		return errors.New("human change request store is nil")
	}
	req.Repo = strings.TrimSpace(req.Repo)
	req.HeadSHA = strings.TrimSpace(req.HeadSHA)
	if req.Repo == "" || req.Number <= 0 || req.HeadSHA == "" {
		return errors.New("human change request needs repo, number and head")
	}
	if req.RecordedAt.IsZero() {
		req.RecordedAt = time.Now().UTC()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	kept := s.items[:0]
	for _, item := range s.items {
		if item.Number == req.Number && strings.EqualFold(item.Repo, req.Repo) {
			if item.HeadSHA != req.HeadSHA || (item.Source == req.Source && item.SourceID == req.SourceID) {
				continue
			}
		}
		kept = append(kept, item)
	}
	s.items = append(kept, req)
	return s.persist()
}

// For implements HumanChangeRequestStore.
func (s *HumanChangeRequests) For(repo string, number int, head string) (HumanChangeRequest, bool) {
	if s == nil {
		return HumanChangeRequest{}, false
	}
	repo = strings.TrimSpace(repo)
	head = strings.TrimSpace(head)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var newest HumanChangeRequest
	found := false
	for _, item := range s.items {
		if item.Number != number || !strings.EqualFold(item.Repo, repo) || item.HeadSHA != head {
			continue
		}
		if !found || item.RecordedAt.After(newest.RecordedAt) {
			newest = item
			found = true
		}
	}
	return newest, found
}

// Forget implements HumanChangeRequestStore.
func (s *HumanChangeRequests) Forget(repo string, number int, source string, sourceID int64) error {
	if s == nil {
		return errors.New("human change request store is nil")
	}
	repo = strings.TrimSpace(repo)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	kept := s.items[:0]
	dropped := false
	for _, item := range s.items {
		if item.Number == number && strings.EqualFold(item.Repo, repo) && item.Source == source && item.SourceID == sourceID {
			dropped = true
			continue
		}
		kept = append(kept, item)
	}
	if !dropped {
		return nil
	}
	s.items = kept
	return s.persist()
}

// Newest returns the newest request recorded against repo#number at any
// head. It is how the base-refresh carry finds the decision the PR's
// previous head still holds.
func (s *HumanChangeRequests) Newest(repo string, number int) (HumanChangeRequest, bool) {
	if s == nil {
		return HumanChangeRequest{}, false
	}
	repo = strings.TrimSpace(repo)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var newest HumanChangeRequest
	found := false
	for _, item := range s.items {
		if item.Number != number || !strings.EqualFold(item.Repo, repo) {
			continue
		}
		if !found || item.RecordedAt.After(newest.RecordedAt) {
			newest = item
			found = true
		}
	}
	return newest, found
}

// HumanChangeRequestNewestStore is the optional store capability the
// base-refresh carry needs: the PR's newest decision regardless of head.
type HumanChangeRequestNewestStore interface {
	Newest(repo string, number int) (HumanChangeRequest, bool)
}

// Lookup returns the request recorded from source/sourceID against
// repo#number at any head: the cheap "already seen" test the un-park
// sweep's restart reconciliation runs before it spends a PR read.
func (s *HumanChangeRequests) Lookup(repo string, number int, source string, sourceID int64) (HumanChangeRequest, bool) {
	if s == nil {
		return HumanChangeRequest{}, false
	}
	repo = strings.TrimSpace(repo)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	for _, item := range s.items {
		if item.Number == number && strings.EqualFold(item.Repo, repo) && item.Source == source && item.SourceID == sourceID {
			return item, true
		}
	}
	return HumanChangeRequest{}, false
}

// Dismiss removes every request match reports true for and returns the
// removed entries (nil when none). It is the one-time reconcile's lever:
// a changes_requested whose source is the hive's own `/hive decision`
// comment (recorded before the self-decision guard existed,
// hivecommons/hive#14822) comes off the head it was blocking.
func (s *HumanChangeRequests) Dismiss(match func(HumanChangeRequest) bool) []HumanChangeRequest {
	if s == nil || match == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	var removed []HumanChangeRequest
	kept := s.items[:0]
	for _, item := range s.items {
		if match(item) {
			removed = append(removed, item)
			continue
		}
		kept = append(kept, item)
	}
	if len(removed) == 0 {
		return nil
	}
	s.items = kept
	_ = s.persist()
	return removed
}

// HumanChangeRequestDismisser is the optional store capability the
// self-decision reconcile needs.
type HumanChangeRequestDismisser interface {
	Dismiss(match func(HumanChangeRequest) bool) []HumanChangeRequest
}

// Len is the number of recorded requests across all PRs and heads.
func (s *HumanChangeRequests) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.load()
	return len(s.items)
}

// HumanChangeRequestFor resolves the request standing against pr's head
// from store (nil-safe), for callers that build review.PullRequest values.
// A bare approval is not a change request: the planner must not kick a fix
// whose only instruction is "approve".
func HumanChangeRequestFor(store HumanChangeRequestStore, repo string, number int, head string) *HumanChangeRequest {
	if store == nil {
		return nil
	}
	req, ok := store.For(repo, number, head)
	if !ok || req.IsApproval() {
		return nil
	}
	return &req
}

// DecisionAnswerFor returns the newest recorded trusted-human decision —
// instruction or bare approval, from a review or a `/hive decision` — that
// stands against pr's exact head. It is the durable, head-scoped fact that a
// reviewer verdict asking for a person on that head has been ANSWERED
// (hivecommons/hive#14679): a newer head carries no answer until someone
// gives one.
func DecisionAnswerFor(store HumanChangeRequestStore, repo string, number int, head string) (HumanChangeRequest, bool) {
	if store == nil || strings.TrimSpace(head) == "" {
		return HumanChangeRequest{}, false
	}
	req, ok := store.For(repo, number, strings.TrimSpace(head))
	if !ok || strings.TrimSpace(req.Actor) == "" {
		return HumanChangeRequest{}, false
	}
	return req, true
}

// HumanApprovalFor returns the bare `/hive decision approve` standing
// against pr's head, if the newest recorded decision on that head is one.
func HumanApprovalFor(store HumanChangeRequestStore, repo string, number int, head string) *HumanChangeRequest {
	if store == nil {
		return nil
	}
	req, ok := store.For(repo, number, head)
	if !ok || !req.IsApproval() {
		return nil
	}
	return &req
}
