package github

import (
	"context"
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Serialized lane outcomes (hivecommons/hive#10884). They mirror the
// mergelane package's outcomes; LaneOutcomeRefused is a refusal before the
// lane runs (for example a branch with GitHub's native merge queue).
const (
	LaneOutcomeFront    = "front"
	LaneOutcomeDeferred = "deferred"
	LaneOutcomeWaiting  = "waiting"
	LaneOutcomeUpdated  = "updated"
	LaneOutcomeLeft     = "left"
	LaneOutcomeMerged   = "merged"
	LaneOutcomeRefused  = "refused"
)

// LaneMergeRequest asks the serialized lane to merge one pull request into a
// hive-serialized repository. Every merge path applies all of its own gates
// first; the lane only adds.
type LaneMergeRequest struct {
	// Repo is "owner/repo".
	Repo string
	// Branch is the target branch; empty means the lane reads it from the PR.
	Branch string
	Number int
	// Path names the asking merge path (PRAuditPathSweep, PRAuditPathQueue,
	// PRAuditPathRelay).
	Path string
	// Method is the merge method; empty means squash (merge for forward-merge
	// PRs), as the sweep merges today.
	Method string
	// Authorize re-checks the path's own authorization for head at the final
	// re-check. nil fails closed.
	Authorize func(ctx context.Context, head string) error
}

// LaneMergeResult is what the lane did. Reason is set for every no-merge.
type LaneMergeResult struct {
	Outcome string
	Reason  string
	SHA     string
	// Method is the merge method the lane used (set when merged).
	Method string
}

// Merged reports whether the lane merged the pull request.
func (r LaneMergeResult) Merged() bool { return r.Outcome == LaneOutcomeMerged }

// SerializedLaneGate is the lane gate the three merge paths consult for a
// hive-serialized repository (hivecommons/hive#10889).
type SerializedLaneGate func(ctx context.Context, req LaneMergeRequest) (LaneMergeResult, error)

// ReasonLaneUnavailable is the refusal when a repository is hive-serialized
// but no lane gate could be built: Hive makes no merge rather than merging
// directly.
const ReasonLaneUnavailable = "serialized merge lane unavailable: no merge"

// SetSerializedLane installs the per-repo merge strategy resolver and the
// serialized lane gate. strategy is read on every merge decision from
// configuration alone (no GitHub call); a nil strategy means every repo is
// direct and merges exactly as before.
func (c *Client) SetSerializedLane(strategy func(repo string) string, gate SerializedLaneGate) {
	if c == nil {
		return
	}
	c.mergePolicyMu.Lock()
	defer c.mergePolicyMu.Unlock()
	c.laneStrategy = strategy
	c.laneGate = gate
}

// SerializedLane returns the installed strategy resolver and lane gate. The
// automerge sweeps read it through their transport.
func (c *Client) SerializedLane() (strategy func(repo string) string, gate SerializedLaneGate) {
	if c == nil {
		return nil, nil
	}
	c.mergePolicyMu.RLock()
	defer c.mergePolicyMu.RUnlock()
	return c.laneStrategy, c.laneGate
}

// SerializedStrategy reports whether strategy puts repo on the serialized
// lane. It is the single branch point every merge path takes; it reads
// configuration only, so deciding direct needs no GitHub call.
func SerializedStrategy(strategy func(repo string) string, repo string) bool {
	return strategy != nil && strategy(repo) == config.MergeStrategyHiveSerialized
}

// RunSerializedLane sends req through gate, failing closed when no gate is
// installed. Callers reach it only after deciding the repository is
// hive-serialized.
func RunSerializedLane(ctx context.Context, gate SerializedLaneGate, req LaneMergeRequest) (LaneMergeResult, error) {
	if gate == nil {
		return LaneMergeResult{Outcome: LaneOutcomeRefused, Reason: ReasonLaneUnavailable}, nil
	}
	res, err := gate(ctx, req)
	if err != nil && strings.TrimSpace(res.Reason) == "" {
		res.Reason = fmt.Sprintf("serialized merge lane error: %v", err)
	}
	return res, err
}
