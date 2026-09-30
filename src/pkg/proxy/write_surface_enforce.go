package proxy

import (
	"encoding/json"
	"strings"
)

// Per-lane write-surface enforcement (hivecommons/hive#9772, part of #9587).
//
// The audited relays (hive-open-pr, hive-open-issue, hive-review, hive-merge,
// hive-push-branch) are the fixed GitHub write surface. For a lane the operator
// has listed under write_surface.enforce, the proxy refuses every DIRECT GitHub
// write from that lane's sandbox — REST write methods, GraphQL mutations and
// git push — so the relays become the only way that lane can write. Reads stay
// untouched. Every other lane, and every hive that sets nothing, keeps its
// direct access exactly as before.

// WriteSurfaceKindREST, WriteSurfaceKindGraphQL and WriteSurfaceKindGitPush
// classify a refused direct write for the audit entry.
const (
	WriteSurfaceKindREST    = "rest"
	WriteSurfaceKindGraphQL = "graphql"
	WriteSurfaceKindGitPush = "git_push"
)

// writeSurfaceExemptWritePaths are POSTs to a GitHub host that are not GitHub
// writes: the device-flow login a CLI needs to authenticate at all. Refusing
// them would break the agent's CLI without closing any write path.
var writeSurfaceExemptWritePaths = map[string]bool{
	"/login/device/code":        true,
	"/login/oauth/access_token": true,
}

// WriteSurfaceEnforceRefusal reports whether a request is a direct GitHub
// write from a lane under write_surface.enforce, and if so returns the
// agent-facing directive for the 403 body and the kind of write refused.
//
// Covered: any POST/PUT/PATCH/DELETE (except git fetch, which is a POST that
// reads, and the device-flow login), and the git push ref advertisement
// (GET .../info/refs?service=git-receive-pack) so a push fails at its first
// round trip rather than after the pack is uploaded. GraphQL is NOT decided
// here: a GraphQL query is a POST that reads, so the caller asks
// WriteSurfaceGraphQLRefusal once it has the body.
//
// enforced is a live predicate (config's WriteSurfaceEnforced), so listing a
// lane in config takes effect on the next request. Nil, or an unnamed agent,
// never refuses.
func WriteSurfaceEnforceRefusal(enforced func(agent string) bool, agent, method, path, rawQuery string) (reason, kind string, refused bool) {
	if enforced == nil || agent == "" {
		return "", "", false
	}
	switch {
	case isGitReceivePack(path):
		kind = WriteSurfaceKindGitPush
	case strings.HasSuffix(path, "/info/refs") && gitServiceIsReceivePack(rawQuery):
		kind = WriteSurfaceKindGitPush
	case !writeMethods[method]:
		return "", "", false
	case strings.HasSuffix(path, "/git-upload-pack"), writeSurfaceExemptWritePaths[path]:
		return "", "", false
	case IsGraphQLPath(path):
		return "", "", false
	default:
		kind = WriteSurfaceKindREST
	}
	if !enforced(agent) {
		return "", "", false
	}
	return WriteSurfaceEnforceReason(agent, kind), kind, true
}

// WriteSurfaceGraphQLRefusal is WriteSurfaceEnforceRefusal for a GraphQL POST
// whose body has been read: a mutation is a write and is refused for an
// enforced lane; a query is a read and passes. A body that is not a GraphQL
// request cannot be shown to be a query, so it is refused too (fail closed).
func WriteSurfaceGraphQLRefusal(enforced func(agent string) bool, agent string, body []byte) (string, bool) {
	if enforced == nil || agent == "" {
		return "", false
	}
	var req graphQLRequest
	if err := json.Unmarshal(body, &req); err == nil && !graphQLMutationRe.MatchString(strings.TrimSpace(req.Query)) {
		return "", false
	}
	if !enforced(agent) {
		return "", false
	}
	return WriteSurfaceEnforceReason(agent, WriteSurfaceKindGraphQL), true
}

// WriteSurfaceEnforceReason is the agent-facing directive for a refused direct
// write. It names the relay to use instead, so the agent reroutes rather than
// hunting for a permissions bug.
func WriteSurfaceEnforceReason(agent, kind string) string {
	instead := "use the hive relays instead: `hive-open-pr` (open a PR), `hive-open-issue` (issue, comment, claim, label, close, request-review), `hive-review`, `hive-merge`, `hive-push-branch` (push a branch)"
	if kind == WriteSurfaceKindGitPush {
		instead = "use `hive-push-branch --repo <owner/repo>` to push your branch, then `hive-open-pr` to open the PR"
	}
	return "agent " + agent + " is under write_surface.enforce: direct GitHub writes (" + kind + ") from the agent sandbox are refused and the audited relays are this lane's only write path. Reads still work. Do NOT retry or route around this with another CLI or the GitHub MCP; " + instead + "."
}

// gitServiceIsReceivePack reports whether a smart-HTTP query string asks for
// the push service.
func gitServiceIsReceivePack(rawQuery string) bool {
	for _, kv := range strings.Split(rawQuery, "&") {
		if strings.EqualFold(kv, "service=git-receive-pack") {
			return true
		}
	}
	return false
}

// WriteSurfaceRepo names the repository a refused direct write targeted, for
// the audit entry's typed repo field: the REST /repos/{o}/{r} prefix, or the
// {o}/{r} of a git smart-HTTP path with or without the ".git" suffix. Empty
// when the path carries none (GraphQL, /user/...).
func WriteSurfaceRepo(path string) string {
	if repo := ExtractRepo(path); repo != "" {
		return repo
	}
	for _, suffix := range []string{"/git-receive-pack", "/info/refs"} {
		if trimmed, ok := strings.CutSuffix(path, suffix); ok {
			parts := strings.Split(strings.Trim(trimmed, "/"), "/")
			if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
				return parts[0] + "/" + strings.TrimSuffix(parts[1], ".git")
			}
		}
	}
	return ""
}
