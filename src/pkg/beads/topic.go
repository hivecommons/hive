package beads

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Topic claims (hivecommons/hive#18493).
//
// A long-lived agent that owns one ongoing topic (a dependency-bump PR, a
// release PR) used to find its own PR by branch name. A renamed or deleted
// branch, or a second branch, lost its place and it opened a duplicate PR.
// A topic claim is a durable record in the agent's own bead store:
//
//	topic key -> {repo, PR number, branch, state}
//
// stored as one in_progress chore bead whose ExternalRef is "topic:<key>".
// The PR number is the identity; the branch is informational and is
// refreshed by the governor when the PR's head ref changes. At most one OPEN
// claim exists per key per store, and a claim for a different PR under an
// open key is refused. The governor releases the claim (closes the bead with
// topic_state merged/closed) when the PR merges or closes.

const (
	// TopicRefPrefix prefixes the ExternalRef of a topic-claim bead.
	TopicRefPrefix = "topic:"

	// Metadata keys a topic-claim bead carries.
	MetaTopicKey    = "topic_key"
	MetaTopicRepo   = "topic_repo"
	MetaTopicPR     = "topic_pr"
	MetaTopicBranch = "topic_branch"
	MetaTopicState  = "topic_state"

	// TopicStateOpen is a live claim; the others record why it ended.
	TopicStateOpen     = "open"
	TopicStateMerged   = "merged"
	TopicStateClosed   = "closed"
	TopicStateReleased = "released"

	// maxTopicKeyLen bounds a topic key so a pasted paragraph is rejected
	// instead of becoming a key.
	maxTopicKeyLen = 128
)

// ErrTopicClaimed is returned when a topic already has an open claim for a
// different pull request.
var ErrTopicClaimed = errors.New("topic already claimed")

// TopicClaim is one topic-claim record, projected from its bead.
type TopicClaim struct {
	Key       string    `json:"key"`
	Repo      string    `json:"repo"`
	PR        int       `json:"pr"`
	Branch    string    `json:"branch,omitempty"`
	State     string    `json:"state"`
	Actor     string    `json:"actor,omitempty"`
	BeadID    string    `json:"bead_id"`
	UpdatedAt time.Time `json:"updated_at"`
}

// NormalizeTopicKey trims and lowercases a topic key and rejects an empty,
// over-long or whitespace-carrying one.
func NormalizeTopicKey(key string) (string, error) {
	k := strings.ToLower(strings.TrimSpace(key))
	if k == "" {
		return "", errors.New("topic key is empty")
	}
	if len(k) > maxTopicKeyLen {
		return "", fmt.Errorf("topic key longer than %d bytes", maxTopicKeyLen)
	}
	if strings.ContainsAny(k, " \t\r\n") {
		return "", errors.New("topic key must not contain whitespace")
	}
	return k, nil
}

// SameTopicRepo reports whether two repository spellings name the same repo.
// "owner/name" is compared whole when both sides carry an owner; otherwise
// only the name part is compared, because some hive surfaces carry the bare
// repo name.
func SameTopicRepo(a, b string) bool {
	a, b = strings.ToLower(strings.TrimSpace(a)), strings.ToLower(strings.TrimSpace(b))
	if a == "" || b == "" {
		return false
	}
	if strings.Contains(a, "/") && strings.Contains(b, "/") {
		return a == b
	}
	return repoName(a) == repoName(b)
}

func repoName(r string) string {
	if i := strings.LastIndex(r, "/"); i >= 0 {
		return r[i+1:]
	}
	return r
}

func isTopicBead(b *Bead) bool {
	return b != nil && strings.HasPrefix(b.ExternalRef, TopicRefPrefix)
}

func isOpenTopicBead(b *Bead) bool {
	return isTopicBead(b) && b.Status != StatusClosed && b.Status != StatusDone
}

func topicFromBead(b *Bead) TopicClaim {
	pr, _ := strconv.Atoi(b.Meta(MetaTopicPR))
	state := b.Meta(MetaTopicState)
	if state == "" {
		state = TopicStateOpen
	}
	return TopicClaim{
		Key:       strings.TrimPrefix(b.ExternalRef, TopicRefPrefix),
		Repo:      b.Meta(MetaTopicRepo),
		PR:        pr,
		Branch:    b.Meta(MetaTopicBranch),
		State:     state,
		Actor:     b.Actor,
		BeadID:    b.ID,
		UpdatedAt: b.UpdatedAt.Time,
	}
}

// openTopicBead returns the open bead for key. Caller holds s.mu.
func (s *Store) openTopicBead(key string) *Bead {
	ref := TopicRefPrefix + key
	for _, b := range s.beads {
		if b.ExternalRef == ref && isOpenTopicBead(b) {
			return b
		}
	}
	return nil
}

// ClaimTopic records that actor owns topic c.Key through pull request
// c.Repo#c.PR. Claiming an open topic again for the same PR is idempotent and
// refreshes its branch (a branch rename); claiming it for a different PR
// returns ErrTopicClaimed together with the existing claim. A pending claim
// (ClaimTopicForBranch) in the same repo holds no PR yet, so a claim with a
// PR number binds it.
func (s *Store) ClaimTopic(actor string, c TopicClaim) (TopicClaim, error) {
	key, err := NormalizeTopicKey(c.Key)
	if err != nil {
		return TopicClaim{}, err
	}
	repo := strings.TrimSpace(c.Repo)
	if repo == "" {
		return TopicClaim{}, errors.New("topic claim needs a repo")
	}
	if c.PR <= 0 {
		return TopicClaim{}, errors.New("topic claim needs a pull request number")
	}
	branch := strings.TrimSpace(c.Branch)

	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	now := flexTime{time.Now().UTC()}
	if b := s.openTopicBead(key); b != nil {
		existing := topicFromBead(b)
		if existing.PR == 0 && SameTopicRepo(existing.Repo, repo) {
			bindTopicBead(b, c.PR, branch, now)
			if err := s.persist(b); err != nil {
				return existing, err
			}
			return topicFromBead(b), nil
		}
		if existing.PR != c.PR || !SameTopicRepo(existing.Repo, repo) {
			return existing, fmt.Errorf("%w: %s is held by %s#%d", ErrTopicClaimed, key, existing.Repo, existing.PR)
		}
		if branch != "" && branch != existing.Branch {
			if b.Metadata == nil {
				b.Metadata = make(map[string]interface{})
			}
			b.Metadata[MetaTopicBranch] = branch
			b.UpdatedAt = now
			if err := s.persist(b); err != nil {
				return existing, err
			}
		}
		return topicFromBead(b), nil
	}

	b := s.newTopicBead(actor, key, repo, c.PR, branch, now)
	s.beads[b.ID] = b
	s.evictOldClosed()
	if err := s.persist(b); err != nil {
		return TopicClaim{}, err
	}
	return topicFromBead(b), nil
}

// ClaimTopicForBranch records a PENDING claim (hivecommons/hive#18572): actor
// owns topic key through the pull request hive will open from branch in
// repo, whose number is not known yet. A sandboxed kick makes this claim;
// the governor binds it to the PR once one with that head branch appears
// (BindTopicBranch). Claiming again with the same branch is idempotent, and
// a pending claim on another branch (an earlier kick that opened no PR) is
// moved to this one. An open claim already bound to a PR is kept when its
// branch is this branch, since the kick is pushing onto that PR, and refused
// with ErrTopicClaimed otherwise.
func (s *Store) ClaimTopicForBranch(actor, key, repo, branch string) (TopicClaim, error) {
	k, err := NormalizeTopicKey(key)
	if err != nil {
		return TopicClaim{}, err
	}
	repo, branch = strings.TrimSpace(repo), strings.TrimSpace(branch)
	if repo == "" {
		return TopicClaim{}, errors.New("topic claim needs a repo")
	}
	if branch == "" {
		return TopicClaim{}, errors.New("a pending topic claim needs the branch its pull request is opened from")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	now := flexTime{time.Now().UTC()}
	if b := s.openTopicBead(k); b != nil {
		existing := topicFromBead(b)
		if !SameTopicRepo(existing.Repo, repo) {
			return existing, fmt.Errorf("%w: %s is held in %s", ErrTopicClaimed, k, existing.Repo)
		}
		if existing.PR > 0 {
			if existing.Branch == branch {
				return existing, nil
			}
			return existing, fmt.Errorf("%w: %s is held by %s#%d", ErrTopicClaimed, k, existing.Repo, existing.PR)
		}
		if existing.Branch == branch {
			return existing, nil
		}
		bindTopicBead(b, 0, branch, now)
		if err := s.persist(b); err != nil {
			return existing, err
		}
		return topicFromBead(b), nil
	}
	b := s.newTopicBead(actor, k, repo, 0, branch, now)
	s.beads[b.ID] = b
	s.evictOldClosed()
	if err := s.persist(b); err != nil {
		return TopicClaim{}, err
	}
	return topicFromBead(b), nil
}

// BindTopicBranch binds every open pending claim on repo's branch to pull
// request pr, the PR hive opened from that branch. It returns how many
// claims it bound.
func (s *Store) BindTopicBranch(repo, branch string, pr int) (int, error) {
	branch = strings.TrimSpace(branch)
	if pr <= 0 || branch == "" {
		return 0, nil
	}
	n := 0
	_, err := s.mutateOpenTopics(func(b *Bead) bool {
		got, _ := strconv.Atoi(b.Meta(MetaTopicPR))
		return got == 0 && b.Meta(MetaTopicBranch) == branch && SameTopicRepo(b.Meta(MetaTopicRepo), repo)
	}, func(b *Bead, now flexTime) {
		bindTopicBead(b, pr, branch, now)
		n++
	})
	return n, err
}

// bindTopicBead sets a claim's PR (0 keeps it pending) and, when given, its
// branch. Caller holds s.mu.
func bindTopicBead(b *Bead, pr int, branch string, now flexTime) {
	if b.Metadata == nil {
		b.Metadata = make(map[string]interface{})
	}
	b.Metadata[MetaTopicPR] = strconv.Itoa(pr)
	if branch != "" {
		b.Metadata[MetaTopicBranch] = branch
	}
	b.UpdatedAt = now
}

// newTopicBead builds an open topic-claim bead. Caller holds s.mu.
func (s *Store) newTopicBead(actor, key, repo string, pr int, branch string, now flexTime) *Bead {
	metadata := map[string]interface{}{
		MetaTopicKey:    key,
		MetaTopicRepo:   repo,
		MetaTopicPR:     strconv.Itoa(pr),
		MetaTopicBranch: branch,
		MetaTopicState:  TopicStateOpen,
	}
	if s.hiveID != "" {
		metadata[hiveIDMetadataKey] = s.hiveID
	}
	return &Bead{
		ID:          uuid.New().String()[:12],
		Title:       "topic: " + key,
		Type:        TypeChore,
		Status:      StatusInProgress,
		Priority:    PriorityMedium,
		Actor:       actor,
		ExternalRef: TopicRefPrefix + key,
		Metadata:    metadata,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// Topic returns the open claim for key.
func (s *Store) Topic(key string) (TopicClaim, bool) {
	k, err := NormalizeTopicKey(key)
	if err != nil {
		return TopicClaim{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if b := s.openTopicBead(k); b != nil {
		return topicFromBead(b), true
	}
	return TopicClaim{}, false
}

// OpenTopics returns every open topic claim, sorted by key.
func (s *Store) OpenTopics() []TopicClaim {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []TopicClaim
	for _, b := range s.beads {
		if isOpenTopicBead(b) {
			out = append(out, topicFromBead(b))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ReleaseTopic ends the open claim for key, recording state (merged, closed
// or released). It reports whether a claim was open.
func (s *Store) ReleaseTopic(key, state string) (bool, error) {
	k, err := NormalizeTopicKey(key)
	if err != nil {
		return false, err
	}
	return s.mutateOpenTopics(func(b *Bead) bool { return strings.TrimPrefix(b.ExternalRef, TopicRefPrefix) == k },
		func(b *Bead, now flexTime) { closeTopicBead(b, state, now) })
}

// ReleaseTopicsForPR ends every open claim held through repo#pr, recording
// state. It returns how many were released.
func (s *Store) ReleaseTopicsForPR(repo string, pr int, state string) (int, error) {
	if pr <= 0 {
		return 0, nil
	}
	n := 0
	_, err := s.mutateOpenTopics(func(b *Bead) bool { return topicMatchesPR(b, repo, pr) },
		func(b *Bead, now flexTime) { closeTopicBead(b, state, now); n++ })
	return n, err
}

// SetTopicBranch records the current head branch of repo#pr on every open
// claim held through it, so the record follows a renamed branch. It returns
// how many claims changed.
func (s *Store) SetTopicBranch(repo string, pr int, branch string) (int, error) {
	branch = strings.TrimSpace(branch)
	if pr <= 0 || branch == "" {
		return 0, nil
	}
	n := 0
	_, err := s.mutateOpenTopics(func(b *Bead) bool {
		return topicMatchesPR(b, repo, pr) && b.Meta(MetaTopicBranch) != branch
	}, func(b *Bead, now flexTime) {
		b.Metadata[MetaTopicBranch] = branch
		b.UpdatedAt = now
		n++
	})
	return n, err
}

func topicMatchesPR(b *Bead, repo string, pr int) bool {
	got, _ := strconv.Atoi(b.Meta(MetaTopicPR))
	return got == pr && SameTopicRepo(b.Meta(MetaTopicRepo), repo)
}

func closeTopicBead(b *Bead, state string, now flexTime) {
	if state == "" {
		state = TopicStateReleased
	}
	b.Metadata[MetaTopicState] = state
	b.Status = StatusClosed
	b.ClosedAt = &now
	b.UpdatedAt = now
}

// mutateOpenTopics applies fn to every open topic bead match accepts, under
// the store and cross-process locks, and persists once if any changed.
func (s *Store) mutateOpenTopics(match func(*Bead) bool, fn func(*Bead, flexTime)) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.lockAndRefresh()()

	now := flexTime{time.Now().UTC()}
	changed := false
	for _, b := range s.beads {
		if !isOpenTopicBead(b) || !match(b) {
			continue
		}
		if b.Metadata == nil {
			b.Metadata = make(map[string]interface{})
		}
		fn(b, now)
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, s.persist(nil)
}
