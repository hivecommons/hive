// Package effects declares the small external-mutation boundary seam shared by
// callers that must not import the convergence implementation.
package effects

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
)

const (
	KindPullRequestCreate = "pull_request_create"
	KindIssueCreate       = "issue_create"
	KindIssueComment      = "issue_comment"
	KindPullRequestMerge  = "pull_request_merge"
	KindLabelMutation     = "label_mutation"
	KindReviewSubmit      = "review_submit"
	KindBranchPush        = "branch_push"
	KindBranchUpdate      = "branch_update"
)

var ErrDenied = errors.New("mutation boundary denied")

type Claim struct {
	Repo   string
	Kind   string
	Target string
	Actor  string
	Inputs map[string]string
}

type Result struct {
	Provenance string
}

type Boundary interface {
	Execute(ctx context.Context, claim Claim, effect func(context.Context) (Result, error)) (Result, error)
}

type NoopBoundary struct{}

func (NoopBoundary) Execute(ctx context.Context, _ Claim, effect func(context.Context) (Result, error)) (Result, error) {
	return effect(ctx)
}

func Execute(ctx context.Context, boundary Boundary, claim Claim, effect func(context.Context) (Result, error)) (Result, error) {
	if boundary == nil {
		boundary = NoopBoundary{}
	}
	return boundary.Execute(ctx, claim, effect)
}

func StableDigest(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func StableInputs(inputs map[string]string) string {
	if len(inputs) == 0 {
		return ""
	}
	keys := make([]string, 0, len(inputs))
	for k := range inputs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(inputs[k])
		b.WriteByte('\n')
	}
	return b.String()
}

type Stats struct {
	Mode      string `json:"mode"`
	Journaled int64  `json:"journaled"`
	Fenced    int64  `json:"fenced"`
	Denied    int64  `json:"denied"`
}

type Recorder struct {
	mu    sync.Mutex
	stats Stats
}

func (r *Recorder) SetMode(mode string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stats.Mode = mode
}

func (r *Recorder) IncJournaled() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.Journaled++
	r.mu.Unlock()
}

func (r *Recorder) IncFenced() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.Fenced++
	r.mu.Unlock()
}

func (r *Recorder) IncDenied() {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.stats.Denied++
	r.mu.Unlock()
}

func (r *Recorder) Snapshot() Stats {
	if r == nil {
		return Stats{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stats
}

type LoggingBoundary struct {
	Next     Boundary
	Logger   *slog.Logger
	Recorder *Recorder
}

func (b LoggingBoundary) Execute(ctx context.Context, claim Claim, effect func(context.Context) (Result, error)) (Result, error) {
	res, err := Execute(ctx, b.Next, claim, effect)
	if err != nil && errors.Is(err, ErrDenied) && b.Logger != nil {
		b.Logger.Warn("mutation boundary denied external effect",
			"repo", claim.Repo, "kind", claim.Kind, "target", claim.Target, "error", err)
	}
	if err == nil && b.Recorder != nil {
		b.Recorder.IncJournaled()
	}
	return res, err
}

func (c Claim) Validate() error {
	if strings.TrimSpace(c.Repo) == "" || !strings.Contains(c.Repo, "/") {
		return fmt.Errorf("repo must be owner/name")
	}
	if strings.TrimSpace(c.Kind) == "" {
		return fmt.Errorf("kind is required")
	}
	if strings.TrimSpace(c.Target) == "" {
		return fmt.Errorf("target is required")
	}
	return nil
}
