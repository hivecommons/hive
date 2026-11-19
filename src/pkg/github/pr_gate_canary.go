package github

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/prgate"
)

// The fork-head canary answers, from the hive's own writes, the question the
// enforce switch depends on: does a status posted with the base repository's
// installation token land on a fork PR head and on a Dependabot head? Every
// gate write is classified by the head it went on and the latest result per
// origin is kept, so `/api/status` (prGate.canary) shows the evidence before
// a repository goes to enforce.

// Head origins of a PR as the canary classifies them.
const (
	PRGateOriginSameRepo   = "same-repo"
	PRGateOriginFork       = "fork"
	PRGateOriginDependabot = "dependabot"
)

// prGateDependabotLogin is the author login of Dependabot PRs.
const prGateDependabotLogin = "dependabot[bot]"

// maxPRGateCanaryHeads bounds the head-to-origin memory between a re-read and
// its writes. Past it the memory restarts empty: the worst case is one write
// left unclassified.
const maxPRGateCanaryHeads = 1024

// Canary results.
const (
	PRGateCanaryPosted    = "posted"
	PRGateCanaryForbidden = "forbidden"
	PRGateCanaryError     = "error"
)

// PRGateCanaryResult is the latest gate write on one head origin.
type PRGateCanaryResult struct {
	Repo    string    `json:"repo"`
	Head    string    `json:"head"`
	Number  int       `json:"number"`
	Context string    `json:"context"`
	Result  string    `json:"result"`
	At      time.Time `json:"at"`
	// LastSuccessHead and LastSuccessAt are the latest write GitHub accepted
	// on this origin; empty means none since the hive started. A non-empty
	// fork entry is the proof that fork heads get the status.
	LastSuccessHead string    `json:"lastSuccessHead,omitempty"`
	LastSuccessAt   time.Time `json:"lastSuccessAt,omitzero"`
}

// PRGateOriginOf classifies a PR head of repo (owner/name).
func PRGateOriginOf(repo string, s PRGateSubject) string {
	switch {
	case strings.EqualFold(s.Author, prGateDependabotLogin):
		return PRGateOriginDependabot
	case s.HeadRepo == "" || !strings.EqualFold(s.HeadRepo, repo):
		// A deleted fork has no head repository; its head lives only in the
		// base repository's pull ref, like any fork head.
		return PRGateOriginFork
	}
	return PRGateOriginSameRepo
}

type prGateCanary struct {
	log *slog.Logger
	now func() time.Time

	mu      sync.Mutex
	origins map[string]string // repo@sha -> origin
	latest  map[string]PRGateCanaryResult
}

func newPRGateCanary(log *slog.Logger) *prGateCanary {
	if log == nil {
		log = slog.Default()
	}
	return &prGateCanary{log: log, now: time.Now, origins: map[string]string{}, latest: map[string]PRGateCanaryResult{}}
}

func canaryKey(repo, sha string) string { return strings.ToLower(repo) + "@" + sha }

func (k *prGateCanary) noteHead(repo, sha, origin string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.origins) >= maxPRGateCanaryHeads {
		k.origins = map[string]string{}
	}
	k.origins[canaryKey(repo, sha)] = origin
}

// record classifies one gate write by the head it went on.
func (k *prGateCanary) record(repo string, number int, sha, statusContext string, err error) {
	k.mu.Lock()
	origin, ok := k.origins[canaryKey(repo, sha)]
	if !ok {
		k.mu.Unlock()
		return
	}
	r := k.latest[origin]
	first := r.LastSuccessHead == ""
	r.Repo, r.Head, r.Number, r.Context, r.At = repo, sha, number, statusContext, k.now()
	switch {
	case err == nil:
		r.Result = PRGateCanaryPosted
		r.LastSuccessHead, r.LastSuccessAt = sha, r.At
	case errors.Is(err, prgate.ErrForbidden):
		r.Result = PRGateCanaryForbidden
	default:
		r.Result = PRGateCanaryError
	}
	k.latest[origin] = r
	k.mu.Unlock()
	switch {
	case err == nil && first:
		k.log.Info("pr-gate: canary: a gate status landed on a head of this origin", "origin", origin, "repo", repo, "number", number, "head", sha, "context", statusContext)
	case err != nil:
		k.log.Warn("pr-gate: canary: a gate status write failed", "origin", origin, "repo", repo, "number", number, "head", sha, "context", statusContext, "error", err)
	}
}

// snapshot returns the latest result per origin (nil when there is none).
func (k *prGateCanary) snapshot() map[string]PRGateCanaryResult {
	if k == nil {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if len(k.latest) == 0 {
		return nil
	}
	out := make(map[string]PRGateCanaryResult, len(k.latest))
	for o, r := range k.latest {
		out[o] = r
	}
	return out
}

// prGateCanaryAPI is the gate's API with the canary watching it: the re-read
// classifies the head, and each write on it is recorded under that origin.
type prGateCanaryAPI struct {
	inner  prGateAPI
	c      *Client
	canary *prGateCanary
}

func (a prGateCanaryAPI) ReadSubject(ctx context.Context, repo string, number int) (prgate.Subject, error) {
	s, err := a.c.ReadPRGateSubject(ctx, repo, number)
	if err != nil {
		return prgate.Subject{}, err
	}
	a.canary.noteHead(repo, s.HeadSHA, PRGateOriginOf(repo, s))
	return prgate.Subject{HeadSHA: s.HeadSHA, Title: s.Title, State: s.State, Draft: s.Draft}, nil
}

func (a prGateCanaryAPI) PostStatus(ctx context.Context, repo string, number int, sha, statusContext, state, description string) error {
	err := a.inner.PostStatus(ctx, repo, number, sha, statusContext, state, description)
	if errors.Is(err, prgate.ErrContextNotAllowed) {
		return err
	}
	a.canary.record(repo, number, sha, statusContext, err)
	return err
}
