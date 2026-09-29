package prfollowup

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/turn"
)

// PR lifecycle states a SweepOptions.State lookup reports.
const (
	PRStateOpen   = "open"
	PRStateMerged = "merged"
	PRStateClosed = "closed"
)

// Prune reasons, recorded in Stats.Pruned and the audit trail.
const (
	PruneMerged  = "merged"
	PruneClosed  = "closed"
	PruneExpired = "retention elapsed"
)

// SweepOptions configures Sweep.
type SweepOptions struct {
	// Dir is the pointer directory.
	Dir string
	// Open holds the PRs known to be open right now, keyed by ThreadsKey. A
	// pointer for one of them is never looked up.
	Open map[string]bool
	// Retention deletes a pointer this long after it was created, whatever
	// its PR's state. Zero or negative disables the age check.
	Retention time.Duration
	// MaxLookups bounds how many State calls one sweep makes; the rest wait
	// for the next sweep. Zero means no lookups.
	MaxLookups int
	// State reports a PR's lifecycle state (PRStateOpen, PRStateMerged,
	// PRStateClosed). An error keeps the pointer.
	State  func(ctx context.Context, repo string, number int) (string, error)
	Audit  AuditFunc
	Logger *slog.Logger
}

// SweepResult counts what one Sweep did.
type SweepResult struct {
	Merged  int
	Closed  int
	Expired int
	Kept    int
}

type sweepCandidate struct {
	path    string
	agent   string
	repo    string
	number  int
	created time.Time
}

// Sweep deletes pointers (and the handoff notes they carry) whose PR merged
// or closed, and any pointer older than Retention. A pointer whose PR is not
// in Open is looked up (at most MaxLookups per sweep, oldest first); a PR the
// lookup cannot classify keeps its pointer.
func Sweep(ctx context.Context, opts SweepOptions, now time.Time) SweepResult {
	storeMu.Lock()
	defer storeMu.Unlock()
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	var res SweepResult
	var delta Stats
	lookups := 0
	for _, c := range sweepCandidates(opts.Dir) {
		if ctx.Err() != nil {
			break
		}
		reason := ""
		switch {
		case opts.Retention > 0 && now.Sub(c.created) > opts.Retention:
			reason = PruneExpired
		case opts.Open[ThreadsKey(c.repo, c.number)]:
		case opts.State != nil && lookups < opts.MaxLookups:
			lookups++
			state, err := opts.State(ctx, c.repo, c.number)
			if err != nil {
				logger.Debug("prfollowup: PR state lookup failed; keeping pointer", "repo", c.repo, "pr", c.number, "error", err)
				break
			}
			switch state {
			case PRStateMerged:
				reason = PruneMerged
			case PRStateClosed:
				reason = PruneClosed
			}
		}
		if reason == "" {
			res.Kept++
			continue
		}
		if err := os.Remove(c.path); err != nil {
			logger.Warn("prfollowup: failed to delete pointer", "repo", c.repo, "pr", c.number, "error", err)
			res.Kept++
			continue
		}
		switch reason {
		case PruneMerged:
			res.Merged++
		case PruneClosed:
			res.Closed++
		default:
			res.Expired++
		}
		delta.addPruned(reason)
		audit(opts.Audit, AuditActionPruned, c.agent, "outcome", "pruned", "reason", reason, "repo", c.repo, "pr", c.number)
		logger.Info("prfollowup: pruned PR follow-up pointer", "repo", c.repo, "pr", c.number, "reason", reason)
	}
	if err := addStats(opts.Dir, delta, now); err != nil {
		logger.Warn("prfollowup: failed to update follow-up counters", "error", err)
	}
	return res
}

// sweepCandidates lists every readable pointer in dir, oldest first. A file
// that does not parse is left alone: deleting what cannot be read would
// hide the corruption rather than surface it.
func sweepCandidates(dir string) []sweepCandidate {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []sweepCandidate
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.HasPrefix(name, pointerFilePrefix) || !strings.HasSuffix(name, pointerFileSuffix) {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		env, err := turn.ParseEnvelope(data)
		if err != nil {
			continue
		}
		number, err := strconv.Atoi(env.Variables[varNumber])
		repo := env.Variables[varRepo]
		if err != nil || number <= 0 || repo == "" {
			continue
		}
		out = append(out, sweepCandidate{path: path, agent: env.Agent.Name, repo: repo, number: number, created: env.CreatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].created.Before(out[j].created) })
	return out
}
