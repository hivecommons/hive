package types

// This file holds the two contracts between the runtime and the host's forge
// integration that used to make pkg/agent import pkg/github
// (hivecommons/hive#13536): the shape of a created pull request, and the
// on-disk location of a per-agent token cache. Both are plain values; the
// forge client stays on the host side and is injected.

// PRResult is what a host-injected pull-request creator returns to the
// sandbox executor: the opened (or pre-existing) PR. pkg/github's CreatePR
// returns this type under its original name, CreatePRResult, so a
// *github.Client satisfies the runtime's PRCreator seam directly.
type PRResult struct {
	Number int
	URL    string
	// Author is the login the forge recorded on the created (or pre-existing)
	// PR — the App bot the hive relayed as (e.g. "kubestellar-hive[bot]").
	// Taken from the API response, not assumed, so the audit log records the
	// true author.
	Author string
	// AlreadyExisted is true when an open PR already covers this request, so
	// the creator returned it instead of opening a duplicate. Two different
	// findings set it: an open PR for the same HEAD BRANCH, and (see
	// DuplicateTree) an open PR carrying the same CONTENT from a different
	// branch.
	AlreadyExisted bool
	// DuplicateTree narrows AlreadyExisted to the content case (#5111): the
	// reuse was decided because an open PR on the same base has an identical
	// tree SHA, not because the head branch matched. Callers that report the
	// outcome keep the two apart because they mean different things to
	// whoever reads the result — "your retry was absorbed" versus "this
	// change is already proposed as #N, go comment on it".
	DuplicateTree bool
	// DuplicateReason is set (with AlreadyExisted) when the PR-side guard
	// redirected the request to an existing PR; the vocabulary is the
	// creator's (pkg/github's DuplicateReason* constants). The returned PR is
	// the one to continue on.
	DuplicateReason string
}

// IsDuplicate reports whether the returned PR was reused because it already
// covers this request's content or issue, rather than because it is this
// request's own earlier PR.
func (r PRResult) IsDuplicate() bool {
	return r.DuplicateTree || r.DuplicateReason != ""
}

// DefaultAgentTokenCacheDir is the directory holding per-agent GitHub token
// caches: the host's token deliverer writes there and the runtime reads from
// it (and exports the path to the agent as HIVE_AGENT_TOKEN_CACHE). The
// constant lives in this leaf so both sides agree on it without the runtime
// importing the forge client.
const DefaultAgentTokenCacheDir = "/var/run/hive-metrics/agent-tokens"

// AgentTokenCachePath is the per-agent token cache file under dir.
func AgentTokenCachePath(dir, agentName string) string {
	return dir + "/gh-token-" + agentName + ".cache"
}
