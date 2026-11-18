package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/buildcontains"
)

// buildContainsTimeout bounds one whole lookup: the PR read plus every
// ancestry compare. A lookup that runs out of time answers unknown for the
// hives it did not finish, never a guess.
const buildContainsTimeout = 20 * time.Second

// buildContainsRepoPath is the repository whose history is consulted. Hives
// run builds of this repository only; a hive on a fork build resolves to
// unknown rather than being compared against the wrong history.
const buildContainsRepoPath = "/repos/hivecommons/hive"

// buildContainsSource is the history the lookup reads. A var so tests supply
// a fake history without a network round-trip.
var buildContainsSource buildcontains.Source = buildcontains.NewCachingSource(hubContainsSource{})

// hubContainsSource reads ancestry from the GitHub REST API with the hub's
// existing read-only identity (the same client, token and base URL the
// channel-distance and commit-behind lookups use). It sends only commit SHAs
// and a PR number of a public repository.
type hubContainsSource struct{}

func (hubContainsSource) get(ctx context.Context, path string) (*http.Response, error) {
	ctx = hubGitHubCallerContext(ctx, "build_contains")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPIBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	authGitHubRequest(req)
	return hubGitHubHTTPClient().Do(req)
}

// Compare implements buildcontains.Source. per_page=1 keeps the payload to
// the status and counts; the commit list is not needed.
func (hubContainsSource) Compare(ctx context.Context, base, head string) (string, bool, error) {
	resp, err := hubContainsSource{}.get(ctx, fmt.Sprintf("%s/compare/%s...%s?per_page=1",
		buildContainsRepoPath, url.PathEscape(base), url.PathEscape(head)))
	if err != nil {
		return "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound, http.StatusUnprocessableEntity:
		// GitHub answers 404 for an unknown SHA and 422 for an unparseable
		// one: the commit is not resolvable, which is not a failure.
		return "", false, nil
	default:
		return "", false, fmt.Errorf("compare %s...%s: HTTP %d", base, head, resp.StatusCode)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return "", false, err
	}
	if body.Status == "" {
		return "", false, fmt.Errorf("compare %s...%s: empty status", base, head)
	}
	return body.Status, true, nil
}

// PullRequest implements buildcontains.Source.
func (hubContainsSource) PullRequest(ctx context.Context, number int) (buildcontains.PullRequest, bool, error) {
	resp, err := hubContainsSource{}.get(ctx, fmt.Sprintf("%s/pulls/%d", buildContainsRepoPath, number))
	if err != nil {
		return buildcontains.PullRequest{}, false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return buildcontains.PullRequest{}, false, nil
	default:
		return buildcontains.PullRequest{}, false, fmt.Errorf("pull %d: HTTP %d", number, resp.StatusCode)
	}
	var body struct {
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return buildcontains.PullRequest{}, false, err
	}
	return buildcontains.PullRequest{Merged: body.Merged, MergeCommit: body.MergeCommitSHA}, true, nil
}

// buildContainsHive is the slice of a My Hives row the lookup reads. These are
// the commit and image reference the spoke already reports on its heartbeat
// plus the hub-owned tracked channel; nothing new is collected.
type buildContainsHive struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ProjectName    string `json:"projectName"`
	GitHash        string `json:"gitHash"`
	ImageRef       string `json:"imageRef"`
	TrackedChannel string `json:"trackedChannel"`
}

// BuildContainsHive is one hive's row in the lookup response.
type BuildContainsHive struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Commit   string `json:"commit,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Contains string `json:"contains"`
	Reason   string `json:"reason,omitempty"`
}

// BuildContainsResponse is the body of GET /api/saas/build-contains.
type BuildContainsResponse struct {
	Query buildcontains.Query `json:"query"`
	// Commit is the commit whose presence was tested: the PR's merge commit
	// for a PR query, the SHA itself for a commit query. Empty when the answer
	// needed no comparison.
	Commit string `json:"commit,omitempty"`
	// Reason explains a fleet-wide answer (an unmerged or unknown PR).
	Reason string              `json:"reason,omitempty"`
	Hives  []BuildContainsHive `json:"hives"`
}

// buildContainsAnswer resolves the query once and tests every hive's running
// commit against it. Pure over src, so the invariants are testable without a
// network: an unresolvable commit is unknown, a PR outside a build's history
// is no.
func buildContainsAnswer(ctx context.Context, src buildcontains.Source, q buildcontains.Query, hives []buildContainsHive) BuildContainsResponse {
	tgt := buildcontains.Resolve(ctx, src, q)
	resp := BuildContainsResponse{Query: q, Commit: tgt.Commit, Hives: make([]BuildContainsHive, 0, len(hives))}
	if tgt.Final {
		resp.Reason = tgt.Answer.Reason
	}
	running := make([]string, 0, len(hives))
	for _, h := range hives {
		running = append(running, strings.TrimSpace(h.GitHash))
	}
	results := tgt.CheckAll(ctx, src, running)
	for _, h := range hives {
		commit := strings.TrimSpace(h.GitHash)
		res := results[commit]
		if res.Contains == "" {
			res = buildcontains.Result{Contains: buildcontains.Unknown, Reason: buildcontains.ReasonNoRunningCommit}
		}
		name := h.ProjectName
		if name == "" {
			name = h.Name
		}
		resp.Hives = append(resp.Hives, BuildContainsHive{
			ID:       h.ID,
			Name:     name,
			Commit:   commit,
			Channel:  spokeReleaseChannel(h.ImageRef, h.TrackedChannel),
			Contains: string(res.Contains),
			Reason:   res.Reason,
		})
	}
	return resp
}

// handleBuildContains answers GET /api/saas/build-contains?q=<PR number or
// commit SHA> for the hives the signed-in user can already see in My Hives. It
// reads the same per-user snapshot as the list, so it can neither show a hive
// the list would hide nor add a data source.
func (s *HubServer) handleBuildContains(w http.ResponseWriter, r *http.Request) {
	username := s.getAuthUser(r)
	if username == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	q, err := buildcontains.ParseQuery(r.URL.Query().Get("q"))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	hives, err := s.buildContainsVisibleHives(username)
	if err != nil {
		s.logger.Warn("handleBuildContains: could not read hives", "user", username, "error", err)
		writeJSONError(w, http.StatusInternalServerError, "failed to read hives")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), buildContainsTimeout)
	defer cancel()
	resp := buildContainsAnswer(ctx, buildContainsSource, q, hives)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// buildContainsVisibleHives returns the caller's My Hives rows, preferring the
// cached snapshot (even a stale one: a commit that moved since is covered by
// the next heartbeat, and the answer states which commit it tested).
func (s *HubServer) buildContainsVisibleHives(username string) ([]buildContainsHive, error) {
	body, _, ok := s.myHivesCachedResponse(myHivesSnapshotKey(username, ""), true)
	if !ok {
		var err error
		body, err = s.buildMyHivesResponse(username, nil)
		if err != nil {
			return nil, err
		}
		s.storeMyHivesSnapshot(myHivesSnapshotKey(username, ""), body)
	}
	var parsed struct {
		Hives []buildContainsHive `json:"hives"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}
	return parsed.Hives, nil
}

// registerBuildContainsRoutes wires the lookup. Kept apart from
// registerSaaSRoutes so the route table's line numbers (cited by the API
// reference) do not shift.
func (s *HubServer) registerBuildContainsRoutes() {
	s.mux.HandleFunc("GET /api/saas/build-contains", s.requireAuth(s.handleBuildContains))
}
