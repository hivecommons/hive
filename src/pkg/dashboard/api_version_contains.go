package dashboard

import (
	"context"
	"net/http"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/buildcontains"
	spoke "github.com/hivecommons/hive/pkg/hub/spoke"
)

// versionContainsTimeout bounds one lookup (a PR read plus one compare).
const versionContainsTimeout = 15 * time.Second

// versionContainsOwner and versionContainsRepo name the repository whose
// history the lookup reads: the one Hive builds are cut from.
const (
	versionContainsOwner = "hivecommons"
	versionContainsRepo  = "hive"
)

// reasonNoGitHub is the answer when this hive has no GitHub client to read
// history with. It is unknown, never a guess.
const reasonNoGitHub = "this hive has no GitHub access to read repository history"

// versionContainsResponse is the body of GET /api/version/contains.
type versionContainsResponse struct {
	Query  buildcontains.Query `json:"query"`
	Commit string              `json:"commit,omitempty"`
	// Running is the commit this hive reports running (the same value
	// /api/version reports as hash).
	Running  string `json:"running,omitempty"`
	Channel  string `json:"channel,omitempty"`
	Contains string `json:"contains"`
	Reason   string `json:"reason,omitempty"`
}

// ghContainsSource reads ancestry through the hive's existing GitHub client.
// It requests only commit SHAs and a PR number of the public repository.
type ghContainsSource struct{ client *gh.Client }

func (g ghContainsSource) Compare(ctx context.Context, base, head string) (string, bool, error) {
	cmp, resp, err := g.client.Repositories.CompareCommits(ctx, versionContainsOwner, versionContainsRepo, base, head, &gh.ListOptions{PerPage: 1})
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity) {
			return "", false, nil
		}
		return "", false, err
	}
	if cmp.GetStatus() == "" {
		return "", false, nil
	}
	return cmp.GetStatus(), true, nil
}

func (g ghContainsSource) PullRequest(ctx context.Context, number int) (buildcontains.PullRequest, bool, error) {
	pr, resp, err := g.client.PullRequests.Get(ctx, versionContainsOwner, versionContainsRepo, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return buildcontains.PullRequest{}, false, nil
		}
		return buildcontains.PullRequest{}, false, err
	}
	return buildcontains.PullRequest{Merged: pr.GetMerged(), MergeCommit: pr.GetMergeCommitSHA()}, true, nil
}

// versionContainsAnswer resolves the query and tests the running commit. Pure
// over src: an unresolvable commit is unknown, a PR outside the build's
// history is no.
func versionContainsAnswer(ctx context.Context, src buildcontains.Source, q buildcontains.Query, running, channel string) versionContainsResponse {
	tgt := buildcontains.Resolve(ctx, src, q)
	res := tgt.Check(ctx, src, running)
	return versionContainsResponse{
		Query:    q,
		Commit:   tgt.Commit,
		Running:  running,
		Channel:  channel,
		Contains: string(res.Contains),
		Reason:   res.Reason,
	}
}

// handleVersionContains answers GET /api/version/contains?q=<PR number or
// commit SHA>: does the build this hive is running contain it?
func (s *Server) handleVersionContains(w http.ResponseWriter, r *http.Request) {
	q, err := buildcontains.ParseQuery(r.URL.Query().Get("q"))
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	channel := versionChannel
	if versionImageSource != nil {
		channel = spoke.ImageReleaseChannel(versionImageSource())
	}
	if s.deps == nil || s.deps.GHClient == nil || s.deps.GHClient.GoGitHub() == nil {
		jsonResponse(w, versionContainsResponse{
			Query: q, Running: versionHash, Channel: channel,
			Contains: string(buildcontains.Unknown), Reason: reasonNoGitHub,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), versionContainsTimeout)
	defer cancel()
	src := ghContainsSource{client: s.deps.GHClient.GoGitHub()}
	jsonResponse(w, versionContainsAnswer(ctx, src, q, versionHash, channel))
}

// registerVersionContainsRoute wires the lookup. Kept apart from RegisterAPI's
// route table so the line numbers the API reference cites do not shift.
func (s *Server) registerVersionContainsRoute() {
	s.mux.HandleFunc("GET /api/version/contains", s.handleVersionContains)
}
