package automerge

// Tests for the contributor-lane auto-merge tier: the eligible path, every
// distinct skip reason, soak timing through an injected clock, and the policy
// gates on the sweep entry point. No test sleeps; time advances by mutating
// the fixture clock.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/intent"
	"github.com/hivecommons/hive/pkg/review"
	hreview "github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	contributorLaneTestIssue  = 42
	contributorLaneTestHive   = "hive-prod"
	contributorLaneTestAuthor = "mrbobbytables"
	contributorLaneTestSoak   = 2 * time.Hour
)

// contributorClaimMarker is a vetted contributor claim: an operator-granted
// tier and a GitHub login proven via the reissue-token flow.
func contributorClaimMarker(who, kind, hive string) string {
	return contributorClaimMarkerTier(who, kind, hive, " tier=trusted verified=true")
}

// contributorClaimMarkerTier renders a claim marker with the given raw
// tier=/verified= fields (empty for a marker written before they existed).
func contributorClaimMarkerTier(who, kind, hive, fields string) string {
	return "<!-- hive:claim who=" + who + " kind=" + kind + " id=abc hive=" + hive + fields + " until=2030-01-01T00:00:00Z -->\nClaimed."
}

type contributorLaneFixture struct {
	nodeID          string
	author          string
	body            string
	labels          []string
	draft           bool
	mergeableState  string
	headRepo        string
	association     string // author_association; "" means CONTRIBUTOR
	omitAssociation bool
	files           []map[string]any
	filesStatus     int
	listStatus      int
	getStatus       int
	recheckHeadSHA  string
	issueComments   []map[string]any // comments on the linked issue
	prComments      []map[string]any // comments on the PR
	// otherPRComments holds the comments of acme/widget PRs other than #7,
	// keyed by number, for tests that admit several PRs by one author.
	otherPRComments map[int][]map[string]any
	// commentTargets lists the PR numbers comments were posted on, in order.
	commentTargets []int
	mergedHistory  []map[string]any
	// otherHistory is the closed-PR list of the hive's other repositories
	// (acme/<name>), served to the hive-wide auto-promotion walk; the fake
	// search/issues endpoint reports a repo only when it holds a merged PR
	// by the searched author.
	otherHistory    map[string][]map[string]any
	searchStatus    int
	searches        int
	searchQuery     string
	otherLists      []string
	issueCommentsSt int
	reviews         []map[string]any
	mergeStatus     int
	mergeMessage    string
	mergeNotApplied bool
	commentStatus   int
	merges          int
	comments        int
	updates         int
	lastComment     string
	allComments     []string
	// checkCompletedAt, when set, is the completed_at the check-runs
	// endpoint reports for sha7 (RFC 3339); empty omits the field.
	checkCompletedAt string
	// headCommittedAt, when set, is the committer date the git/commits
	// endpoint reports for sha7 (RFC 3339) and the PR's updated_at; empty
	// leaves the route unserved and the timestamp omitted.
	headCommittedAt string
	// prUpdatedAt, when set, overrides the PR's updated_at (RFC 3339) so a
	// test can give the head a committer date that disagrees with when
	// GitHub actually saw it pushed.
	prUpdatedAt string
	// headStatus, when set, is the combined-status state and check-run
	// conclusion reported for sha7; empty means "success".
	headStatus string
	// baseRef, when set, gives the PR base a branch name and serves the
	// branch, protection and compare routes the current-base gate
	// (automerge_current_base.go) and release hold need; empty leaves the
	// base nameless as before.
	baseRef  string
	behindBy int // behind_by reported by compare/<baseRef>...sha7
	compares int
	// requiredContexts, when set with baseRef, is what branch protection
	// declares as required_status_checks.contexts; empty keeps the branch
	// unprotected. The status/check-runs routes only ever report ci/build,
	// so any other name yields required-check-missing:<name>.
	requiredContexts []string
	// rulesetContexts, when set with baseRef, is what the branch's rulesets
	// (rules/branches) list as required_status_checks; empty serves [].
	rulesetContexts []string
	// noCIEvidence makes the status and check-runs routes for sha7 report
	// nothing, as for a head whose only workflow runs GitHub is holding.
	noCIEvidence bool
	// workflowRuns is the payload of GET actions/runs?head_sha=sha7; nil
	// serves an empty list.
	workflowRuns []map[string]any
	// approvedRuns records POST actions/runs/{id}/approve calls;
	// approveStatus, when set, is the status every approve returns.
	approvedRuns  []int64
	approveStatus int
	// issueAuthor is who GET issues/42 reports as the linked issue's
	// author (hivecommons/hive#14581); empty serves the PR author, so the
	// merge credits nobody and sends GitHub's default message.
	issueAuthor     string
	issueAuthorID   int64
	issueAuthorType string
	issueStatus     int
	issueGets       int
	// commitMessages are the branch commits GET pulls/7/commits serves;
	// nil serves one signed commit.
	commitMessages []string
	commitLists    int
	// mergeBody is the JSON body of the last PUT pulls/7/merge.
	mergeBody map[string]any
	// labelsCreated counts POST labels (repo label definitions);
	// labelsAdded / labelsRemoved record issue label writes on PR 7. An
	// added label joins fx.labels so later ticks see it on the PR.
	labelsCreated int
	labelsAdded   []string
	labelsRemoved []string
}

func newContributorLaneAPI(t *testing.T, fx *contributorLaneFixture) *httptest.Server {
	t.Helper()
	fetches := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := fx.mergeableState
		if state == "" {
			state = "clean"
		}
		author := fx.author
		if author == "" {
			author = contributorLaneTestAuthor
		}
		headRepo := fx.headRepo
		if headRepo == "" {
			headRepo = "acme/widget"
		}
		labels := make([]map[string]string, 0, len(fx.labels))
		for _, l := range fx.labels {
			labels = append(labels, map[string]string{"name": l})
		}
		association := fx.association
		if association == "" {
			association = "CONTRIBUTOR"
		}
		headStatus := fx.headStatus
		if headStatus == "" {
			headStatus = "success"
		}
		pull := func(sha string) map[string]any {
			p := map[string]any{
				"node_id":         fx.nodeID,
				"number":          7,
				"state":           "open",
				"draft":           fx.draft,
				"body":            fx.body,
				"mergeable_state": state,
				"mergeable":       state == "clean",
				"user":            map[string]string{"login": author},
				"head":            map[string]any{"sha": sha, "repo": map[string]string{"full_name": headRepo}},
				"base":            map[string]any{"repo": map[string]string{"full_name": "acme/widget"}},
				"labels":          labels,
			}
			if fx.baseRef != "" {
				p["base"].(map[string]any)["ref"] = fx.baseRef
			}
			if !fx.omitAssociation {
				p["author_association"] = association
			}
			if fx.headCommittedAt != "" {
				p["updated_at"] = fx.headCommittedAt
			}
			if fx.prUpdatedAt != "" {
				p["updated_at"] = fx.prUpdatedAt
			}
			return p
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls":
			if fx.listStatus != 0 {
				http.Error(w, "list error", fx.listStatus)
				return
			}
			if r.URL.Query().Get("state") == "closed" {
				json.NewEncoder(w).Encode(fx.mergedHistory)
				return
			}
			json.NewEncoder(w).Encode([]map[string]any{pull("sha7")})
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			fx.searches++
			fx.searchQuery = r.URL.Query().Get("q")
			if fx.searchStatus != 0 {
				http.Error(w, "search error", fx.searchStatus)
				return
			}
			searchedAuthor := ""
			var searchedRepos []string
			for _, term := range strings.Fields(fx.searchQuery) {
				switch {
				case strings.HasPrefix(term, "author:"):
					searchedAuthor = strings.TrimPrefix(term, "author:")
				case strings.HasPrefix(term, "repo:"):
					searchedRepos = append(searchedRepos, strings.TrimPrefix(term, "repo:"))
				}
			}
			items := []map[string]any{}
			for _, full := range searchedRepos {
				name := strings.TrimPrefix(full, "acme/")
				for _, pr := range fx.otherHistory[name] {
					user, _ := pr["user"].(map[string]string)
					if merged, _ := pr["merged_at"].(string); merged != "" && strings.EqualFold(user["login"], searchedAuthor) {
						items = append(items, map[string]any{"number": pr["number"], "repository_url": "https://api.github.com/repos/" + full, "pull_request": map[string]any{}})
						break
					}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(items), "incomplete_results": false, "items": items})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/") && strings.HasSuffix(r.URL.Path, "/pulls") && r.URL.Path != "/repos/acme/widget/pulls" && r.URL.Query().Get("state") == "closed":
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/"), "/pulls")
			fx.otherLists = append(fx.otherLists, name)
			history := fx.otherHistory[name]
			if history == nil {
				history = []map[string]any{}
			}
			json.NewEncoder(w).Encode(history)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			fetches++
			if fetches == 1 && fx.getStatus != 0 {
				http.Error(w, "get error", fx.getStatus)
				return
			}
			sha := "sha7"
			if fetches > 1 && fx.recheckHeadSHA != "" {
				sha = fx.recheckHeadSHA
			}
			json.NewEncoder(w).Encode(pull(sha))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/42":
			fx.issueGets++
			if fx.issueStatus != 0 {
				http.Error(w, "issue error", fx.issueStatus)
				return
			}
			issueAuthor := fx.issueAuthor
			if issueAuthor == "" {
				issueAuthor = author
			}
			userType := fx.issueAuthorType
			if userType == "" {
				userType = "User"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"number": 42, "state": "open",
				"user": map[string]any{"login": issueAuthor, "id": fx.issueAuthorID, "type": userType},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/commits":
			fx.commitLists++
			msgs := fx.commitMessages
			if msgs == nil {
				msgs = []string{"Fix the widget\n\nSigned-off-by: " + author + " <" + author + "@example.com>"}
			}
			commits := make([]map[string]any, 0, len(msgs))
			for i, msg := range msgs {
				commits = append(commits, map[string]any{"sha": fmt.Sprintf("c%d", i), "commit": map[string]any{"message": msg}, "parents": []map[string]any{{"sha": "p"}}})
			}
			json.NewEncoder(w).Encode(commits)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/42/comments":
			if fx.issueCommentsSt != 0 {
				http.Error(w, "comments error", fx.issueCommentsSt)
				return
			}
			json.NewEncoder(w).Encode(fx.issueComments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			json.NewEncoder(w).Encode(fx.prComments)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/42":
			// The linked issue the body closes, read by the linked-issue
			// gate: open and unlabelled, so it never blocks.
			json.NewEncoder(w).Encode(map[string]any{"number": 42, "state": "open", "labels": []any{}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/files":
			if fx.filesStatus != 0 {
				http.Error(w, "files error", fx.filesStatus)
				return
			}
			files := fx.files
			if files == nil {
				files = []map[string]any{{"filename": "pkg/widget/widget.go", "status": "modified", "additions": 5, "deletions": 2}}
			}
			json.NewEncoder(w).Encode(files)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			json.NewEncoder(w).Encode(fx.reviews)
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/acme/widget/commits/sha7/status" ||
			(fx.recheckHeadSHA != "" && r.URL.Path == "/repos/acme/widget/commits/"+fx.recheckHeadSHA+"/status")):
			if fx.noCIEvidence {
				json.NewEncoder(w).Encode(map[string]any{"state": "pending", "total_count": 0, "statuses": []any{}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"state":       headStatus,
				"total_count": 1,
				"statuses":    []map[string]string{{"context": "ci/build", "state": headStatus}},
			})
		case r.Method == http.MethodGet && (r.URL.Path == "/repos/acme/widget/commits/sha7/check-runs" ||
			(fx.recheckHeadSHA != "" && r.URL.Path == "/repos/acme/widget/commits/"+fx.recheckHeadSHA+"/check-runs")):
			if fx.noCIEvidence {
				json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
				return
			}
			run := map[string]string{"name": "build", "status": "completed", "conclusion": headStatus}
			if fx.checkCompletedAt != "" {
				run["completed_at"] = fx.checkCompletedAt
			}
			json.NewEncoder(w).Encode(map[string]any{
				"total_count": 1,
				"check_runs":  []map[string]string{run},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/git/commits/sha7" && fx.headCommittedAt != "":
			json.NewEncoder(w).Encode(map[string]any{"sha": "sha7", "committer": map[string]string{"name": "c", "date": fx.headCommittedAt}})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/merge":
			fx.merges++
			fx.mergeBody = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&fx.mergeBody)
			if fx.mergeStatus != 0 {
				w.WriteHeader(fx.mergeStatus)
				msg := fx.mergeMessage
				if msg == "" {
					msg = "merge error"
				}
				json.NewEncoder(w).Encode(map[string]string{"message": msg})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"merged": !fx.mergeNotApplied, "sha": "merge7"})
		case fx.baseRef != "" && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/"+fx.baseRef:
			json.NewEncoder(w).Encode(map[string]any{"name": fx.baseRef, "commit": map[string]string{"sha": "base1"}})
		case fx.baseRef != "" && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/"+fx.baseRef+"/protection/required_status_checks":
			if len(fx.requiredContexts) > 0 {
				json.NewEncoder(w).Encode(map[string]any{"strict": false, "contexts": fx.requiredContexts})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "Branch not protected"})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/actions/runs":
			runs := fx.workflowRuns
			if runs == nil {
				runs = []map[string]any{}
			}
			json.NewEncoder(w).Encode(map[string]any{"total_count": len(runs), "workflow_runs": runs})
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/actions/runs/") && strings.HasSuffix(r.URL.Path, "/approve"):
			id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/actions/runs/"), "/approve"), 10, 64)
			if err != nil {
				t.Fatalf("bad approve path %s: %v", r.URL.Path, err)
			}
			fx.approvedRuns = append(fx.approvedRuns, id)
			if fx.approveStatus != 0 {
				w.WriteHeader(fx.approveStatus)
				json.NewEncoder(w).Encode(map[string]string{"message": "Resource not accessible by integration"})
				return
			}
			w.WriteHeader(http.StatusCreated)
		case fx.baseRef != "" && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/rules/branches/"+fx.baseRef:
			if len(fx.rulesetContexts) > 0 {
				checks := make([]map[string]string, 0, len(fx.rulesetContexts))
				for _, name := range fx.rulesetContexts {
					checks = append(checks, map[string]string{"context": name})
				}
				json.NewEncoder(w).Encode([]any{map[string]any{"type": "required_status_checks", "parameters": map[string]any{"required_status_checks": checks}}})
				return
			}
			json.NewEncoder(w).Encode([]any{})
		case fx.baseRef != "" && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/commits/base1/status":
			json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
		case fx.baseRef != "" && r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/"+fx.baseRef+"...sha7":
			fx.compares++
			json.NewEncoder(w).Encode(map[string]any{"behind_by": fx.behindBy, "ahead_by": 1})
		case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
			fx.updates++
			json.NewEncoder(w).Encode(map[string]any{"message": "Updating pull request branch."})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/labels":
			fx.labelsCreated++
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": "created"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			var names []string
			_ = json.NewDecoder(r.Body).Decode(&names)
			for _, n := range names {
				fx.labelsAdded = append(fx.labelsAdded, n)
				if !slices.Contains(fx.labels, n) {
					fx.labels = append(fx.labels, n)
				}
			}
			json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/issues/7/labels/"):
			name := strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/issues/7/labels/")
			fx.labelsRemoved = append(fx.labelsRemoved, name)
			fx.labels = slices.DeleteFunc(slices.Clone(fx.labels), func(l string) bool { return l == name })
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			fx.comments++
			fx.commentTargets = append(fx.commentTargets, 7)
			if fx.commentStatus != 0 {
				http.Error(w, "comment error", fx.commentStatus)
				return
			}
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			fx.lastComment = payload.Body
			fx.allComments = append(fx.allComments, payload.Body)
			// Posted comments become visible to later GETs so once-per-PR
			// markers can be exercised across ticks.
			fx.prComments = append(fx.prComments, map[string]any{
				"id":   1000 + fx.comments,
				"user": map[string]string{"login": testHiveAppBotLogin},
				"body": payload.Body,
			})
			json.NewEncoder(w).Encode(map[string]any{"id": 1000 + fx.comments})
		case r.Method == http.MethodGet && otherPRCommentsNumber(r.URL.Path) > 0:
			comments := fx.otherPRComments[otherPRCommentsNumber(r.URL.Path)]
			if comments == nil {
				comments = []map[string]any{}
			}
			json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodPost && otherPRCommentsNumber(r.URL.Path) > 0:
			number := otherPRCommentsNumber(r.URL.Path)
			fx.comments++
			fx.commentTargets = append(fx.commentTargets, number)
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			fx.lastComment = payload.Body
			fx.allComments = append(fx.allComments, payload.Body)
			if fx.otherPRComments == nil {
				fx.otherPRComments = map[int][]map[string]any{}
			}
			fx.otherPRComments[number] = append(fx.otherPRComments[number], map[string]any{
				"id":   1000 + fx.comments,
				"user": map[string]string{"login": testHiveAppBotLogin},
				"body": payload.Body,
			})
			json.NewEncoder(w).Encode(map[string]any{"id": 1000 + fx.comments})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
}

// otherPRCommentsNumber returns the PR number of an acme/widget
// issues/<n>/comments path for PRs other than #7 and the linked issue #42,
// or 0 when the path is something else.
func otherPRCommentsNumber(path string) int {
	rest, ok := strings.CutPrefix(path, "/repos/acme/widget/issues/")
	if !ok {
		return 0
	}
	numText, ok := strings.CutSuffix(rest, "/comments")
	if !ok {
		return 0
	}
	number, err := strconv.Atoi(numText)
	if err != nil || number == 7 || number == contributorLaneTestIssue {
		return 0
	}
	return number
}

// contributorLaneClock is a mutable fake clock injected via Options.Now.
type contributorLaneClock struct{ at time.Time }

func (c *contributorLaneClock) now() time.Time { return c.at }

func (c *contributorLaneClock) advance(d time.Duration) { c.at = c.at.Add(d) }

func defaultContributorLanePolicy() ContributorLanePolicy {
	return ContributorLanePolicy{
		Enabled:                true,
		HiveID:                 contributorLaneTestHive,
		Soak:                   contributorLaneTestSoak,
		RequireReviewerVerdict: true,
		ExcludeLabels:          map[string]bool{"hold": true, "do-not-merge": true, "needs-human": true, "blocked": true, "needs-rebase": true},
		RequireTiers:           map[string]bool{"trusted": true, "merger": true},
	}
}

func validContributorFixture() contributorLaneFixture {
	return contributorLaneFixture{
		body: "Fixes #42\n\nSome change.",
		issueComments: []map[string]any{{
			"id":   1,
			"user": map[string]string{"login": testHiveAppBotLogin},
			"body": contributorClaimMarker(contributorLaneTestAuthor, "contributor", contributorLaneTestHive),
		}},
	}
}

func approvedVerdict(string, int, string) ContributorLaneVerdict {
	return ContributorLaneVerdict{Configured: true, Approved: true, Verdict: "approve"}
}

func newContributorLaneEngine(apiURL string, policy ContributorLanePolicy, clock *contributorLaneClock, opts Options) *Engine {
	return newContributorLaneEngineRepos(apiURL, []string{"widget"}, policy, clock, opts)
}

// newContributorLaneEngineRepos is newContributorLaneEngine for a hive that
// manages repos (acme/<name>…); the PR under test still lives in acme/widget.
func newContributorLaneEngineRepos(apiURL string, repos []string, policy ContributorLanePolicy, clock *contributorLaneClock, opts Options) *Engine {
	client := hgithub.NewClient("token", "acme", repos, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	opts.ContributorLanePolicy = func() ContributorLanePolicy { return policy }
	if opts.ContributorLaneVerdict == nil {
		opts.ContributorLaneVerdict = approvedVerdict
	}
	if opts.ContributorSoak == nil {
		opts.ContributorSoak = NewContributorSoakTracker()
	}
	// Every engine otherwise shares the process-wide head tracker, which pins
	// the first updated_at seen for acme/widget#7@sha7 across tests.
	if opts.HeadFirstSeen == nil {
		opts.HeadFirstSeen = NewHeadFirstSeenTracker()
	}
	// Likewise the process-wide stage store, which since
	// hivecommons/hive#15443 remembers which authors' auto-promotion was
	// announced; sharing it would leak one test's announcement into the next.
	if opts.StageStore == nil {
		opts.StageStore = stagelabels.NewStore("", nil)
	}
	if clock != nil {
		opts.Now = clock.now
	}
	if opts.SentinelLabel == nil {
		opts.SentinelLabel = func() string { return "hive-sentinel" }
	}
	return New(client, opts)
}

// soakedTracker returns a tracker that already recorded sha7 going green a
// full soak window before the clock, so the eligible path merges immediately.
func soakedTracker(clock *contributorLaneClock) *ContributorSoakTracker {
	tracker := NewContributorSoakTracker()
	tracker.observeGreen(contributorSoakKey("acme", "widget", 7, "sha7"), clock.at.Add(-contributorLaneTestSoak))
	return tracker
}

func TestTrySweepContributorLanePRSkipReasons(t *testing.T) {
	appClaim := func(who, hive string) []map[string]any {
		return []map[string]any{{
			"id":   1,
			"user": map[string]string{"login": testHiveAppBotLogin},
			"body": contributorClaimMarker(who, "contributor", hive),
		}}
	}
	tierClaim := func(fields string) func(fx *contributorLaneFixture) {
		return func(fx *contributorLaneFixture) {
			fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, fields)
		}
	}
	file := func(name, status string, adds, dels int) map[string]any {
		return map[string]any{"filename": name, "status": status, "additions": adds, "deletions": dels}
	}
	for _, tc := range []struct {
		name       string
		mutate     func(fx *contributorLaneFixture)
		policy     func(p *ContributorLanePolicy)
		verdict    ContributorLaneVerdictFunc
		wantReason string
		wantErr    bool
		wantMerges int
		wantUpdate int
	}{
		{name: "eligible merges", wantMerges: 1},
		{name: "pr gone", mutate: func(fx *contributorLaneFixture) { fx.getStatus = http.StatusNotFound }, wantReason: "gone"},
		{name: "fetch error", mutate: func(fx *contributorLaneFixture) { fx.getStatus = http.StatusInternalServerError }, wantReason: "fetch-pr", wantErr: true},
		{name: "draft", mutate: func(fx *contributorLaneFixture) { fx.draft = true }, wantReason: "draft"},
		{name: "app-authored belongs to self lane", mutate: func(fx *contributorLaneFixture) { fx.author = testHiveAppBotLogin }, wantReason: contributorLaneReasonAppAuthored},
		{name: "sentinel label blocks", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"hive-sentinel"} }, wantReason: contributorLaneReasonSentinelBlocked},
		{name: "hold label", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"hold"} }, wantReason: "label:hold"},
		{name: "do-not-merge slash label", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"do-not-merge/wip"} }, wantReason: "label:do-not-merge/wip"},
		{name: "needs-rebase label returns author fix", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"needs-rebase"} }, wantReason: "label:needs-rebase"},
		{name: "policy exclude label", mutate: func(fx *contributorLaneFixture) { fx.labels = []string{"blocked"} }, wantReason: "excluded-label:blocked"},
		{name: "conflict outranks untrusted claim tier", mutate: func(fx *contributorLaneFixture) {
			fx.mergeableState = "dirty"
			tierClaim(" tier=newcomer verified=true")(fx)
		}, wantReason: "conflicting"},
		{name: "no linked issue", mutate: func(fx *contributorLaneFixture) { fx.body = "No issue here." }, wantReason: contributorLaneReasonNoLinkedIssue},
		{name: "refs keyword is not closing", mutate: func(fx *contributorLaneFixture) { fx.body = "Refs #42" }, wantReason: contributorLaneReasonNoLinkedIssue},
		{name: "no claim marker", mutate: func(fx *contributorLaneFixture) { fx.issueComments = nil }, wantReason: contributorLaneReasonNoClaim},
		{
			name:       "claim from non-app author is ignored",
			mutate:     func(fx *contributorLaneFixture) { fx.issueComments[0]["user"] = map[string]string{"login": "someone"} },
			wantReason: contributorLaneReasonNoClaim,
		},
		{
			name: "maintainer-kind claim is not a contributor claim",
			mutate: func(fx *contributorLaneFixture) {
				fx.issueComments[0]["body"] = contributorClaimMarker(contributorLaneTestAuthor, "maintainer", contributorLaneTestHive)
			},
			wantReason: contributorLaneReasonNoClaim,
		},
		{name: "claim author mismatch", mutate: func(fx *contributorLaneFixture) { fx.issueComments = appClaim("someoneelse", contributorLaneTestHive) }, wantReason: contributorLaneReasonClaimAuthor},
		{name: "claim for other hive", mutate: func(fx *contributorLaneFixture) {
			fx.issueComments = appClaim(contributorLaneTestAuthor, "hive-staging")
		}, wantReason: contributorLaneReasonClaimOtherHive},
		{
			name: "other-hive outranks author mismatch",
			mutate: func(fx *contributorLaneFixture) {
				fx.issueComments = append(appClaim("someoneelse", contributorLaneTestHive), appClaim(contributorLaneTestAuthor, "hive-staging")...)
			},
			wantReason: contributorLaneReasonClaimOtherHive,
		},
		{name: "newcomer claim is untrusted", mutate: tierClaim(" tier=newcomer verified=true"), wantReason: contributorLaneReasonClaimTier},
		{name: "auto-promoted contributor claim is untrusted", mutate: tierClaim(" tier=contributor verified=true"), wantReason: contributorLaneReasonClaimTier},
		{name: "claim without tier fails closed", mutate: tierClaim(""), wantReason: contributorLaneReasonClaimTier},
		{name: "trusted but unverified login", mutate: tierClaim(" tier=trusted verified=false"), wantReason: contributorLaneReasonClaimUnverified},
		{name: "trusted without verified field", mutate: tierClaim(" tier=trusted"), wantReason: contributorLaneReasonClaimUnverified},
		{name: "verified merger merges", mutate: tierClaim(" tier=merger verified=true"), wantMerges: 1},
		{
			name:       "tier outside require_tiers",
			policy:     func(p *ContributorLanePolicy) { p.RequireTiers = map[string]bool{"merger": true} },
			wantReason: contributorLaneReasonClaimTier,
		},
		{
			name:       "empty require_tiers admits nobody",
			policy:     func(p *ContributorLanePolicy) { p.RequireTiers = nil },
			wantReason: contributorLaneReasonClaimTier,
		},
		{
			name: "untrusted tier outranks other hive",
			mutate: func(fx *contributorLaneFixture) {
				fx.issueComments = append(appClaim(contributorLaneTestAuthor, "hive-staging"), map[string]any{
					"id": 2, "user": map[string]string{"login": testHiveAppBotLogin},
					"body": contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=newcomer verified=false"),
				})
			},
			wantReason: contributorLaneReasonClaimTier,
		},
		// A verified claim marker admits regardless of GitHub's association
		// (#12195, #12290: relay contributors are FIRST_TIME_CONTRIBUTOR on
		// their first PR to a repo).
		{name: "author association none with claim merges", mutate: func(fx *contributorLaneFixture) { fx.association = "NONE" }, wantMerges: 1},
		{name: "author association first-time contributor with claim merges", mutate: func(fx *contributorLaneFixture) { fx.association = "FIRST_TIME_CONTRIBUTOR" }, wantMerges: 1},
		{name: "author association first-timer with claim merges", mutate: func(fx *contributorLaneFixture) { fx.association = "FIRST_TIMER" }, wantMerges: 1},
		{name: "author association missing with claim merges", mutate: func(fx *contributorLaneFixture) { fx.omitAssociation = true }, wantMerges: 1},
		{name: "author association member merges", mutate: func(fx *contributorLaneFixture) { fx.association = "MEMBER" }, wantMerges: 1},
		// Without a marker the association is the only evidence left and an
		// untrusted one outranks the marker-path reason.
		{name: "first-time contributor without claim", mutate: func(fx *contributorLaneFixture) { fx.association = "FIRST_TIME_CONTRIBUTOR"; fx.issueComments = nil }, wantReason: contributorLaneReasonAuthorAssociation},
		{name: "first-time contributor without linked issue", mutate: func(fx *contributorLaneFixture) { fx.association = "FIRST_TIME_CONTRIBUTOR"; fx.body = "Part of #42" }, wantReason: contributorLaneReasonAuthorAssociation},
		{name: "association none with other-author claim", mutate: func(fx *contributorLaneFixture) {
			fx.association = "NONE"
			fx.issueComments = appClaim("someoneelse", contributorLaneTestHive)
		}, wantReason: contributorLaneReasonAuthorAssociation},
		{name: "association missing with unverified claim", mutate: func(fx *contributorLaneFixture) {
			fx.omitAssociation = true
			tierClaim(" tier=trusted verified=false")(fx)
		}, wantReason: contributorLaneReasonAuthorAssociation},
		{name: "first-time contributor with newcomer claim", mutate: func(fx *contributorLaneFixture) {
			fx.association = "FIRST_TIME_CONTRIBUTOR"
			tierClaim(" tier=newcomer verified=true")(fx)
		}, wantReason: contributorLaneReasonAuthorAssociation},
		{
			name: "workflow change",
			mutate: func(fx *contributorLaneFixture) {
				fx.files = []map[string]any{file(".github/workflows/ci.yml", "modified", 1, 1)}
			},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name: "test file deleted",
			mutate: func(fx *contributorLaneFixture) {
				fx.files = []map[string]any{file("pkg/widget/widget_test.go", "removed", 0, 3)}
			},
			wantReason: contributorLaneReasonTestsRemoved,
		},
		{
			name: "test lines gutted",
			mutate: func(fx *contributorLaneFixture) {
				fx.files = []map[string]any{file("pkg/widget/widget.go", "modified", 2, 0), file("pkg/widget/widget_test.go", "modified", 2, 40)}
			},
			wantReason: contributorLaneReasonTestsRemoved,
		},
		{
			name: "test edits that add coverage merge",
			mutate: func(fx *contributorLaneFixture) {
				fx.files = []map[string]any{file("pkg/widget/widget_test.go", "modified", 30, 26)}
			},
			wantMerges: 1,
		},
		{name: "files fetch error", mutate: func(fx *contributorLaneFixture) { fx.filesStatus = http.StatusInternalServerError }, wantReason: contributorLaneReasonFilesEvidence, wantErr: true},
		{name: "claim comments fetch error", mutate: func(fx *contributorLaneFixture) { fx.issueCommentsSt = http.StatusInternalServerError }, wantReason: "claim-comments-fetch", wantErr: true},
		{
			name: "reporter-trust hold",
			mutate: func(fx *contributorLaneFixture) {
				fx.prComments = []map[string]any{{"id": 9, "user": map[string]string{"login": testHiveAppBotLogin}, "body": hgithub.ReporterTrustNoticeMarker + " held"}}
			},
			wantReason: contributorLaneReasonReporterTrustHold,
		},
		{
			name: "changes requested",
			mutate: func(fx *contributorLaneFixture) {
				fx.reviews = []map[string]any{{"id": 1, "state": "CHANGES_REQUESTED", "user": map[string]string{"login": "maintainer"}}}
			},
			wantReason: contributorLaneReasonChangesRequested,
		},
		{name: "behind updates branch", mutate: func(fx *contributorLaneFixture) { fx.mergeableState = "behind" }, wantReason: "check-pending", wantUpdate: 1},
		{name: "conflicting", mutate: func(fx *contributorLaneFixture) { fx.mergeableState = "dirty" }, wantReason: "conflicting"},
		{name: "unknown mergeability waits", mutate: func(fx *contributorLaneFixture) { fx.mergeableState = "unknown" }, wantReason: "mergeable-unknown"},
		{
			name:       "reviewer configured without approval",
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{Configured: true} },
			wantReason: contributorLaneReasonNoVerdict,
		},
		{
			name:       "no reviewer configured skips",
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{} },
			wantReason: contributorLaneReasonNoReviewer,
		},
		{
			name:       "no reviewer skips even when verdict not required",
			policy:     func(p *ContributorLanePolicy) { p.RequireReviewerVerdict = false },
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{} },
			wantReason: contributorLaneReasonNoReviewer,
		},
		{
			name:       "verdict not required ignores reviewer",
			policy:     func(p *ContributorLanePolicy) { p.RequireReviewerVerdict = false },
			verdict:    func(string, int, string) ContributorLaneVerdict { return ContributorLaneVerdict{Configured: true} },
			wantMerges: 1,
		},
		{name: "head changed since eval", mutate: func(fx *contributorLaneFixture) { fx.recheckHeadSHA = "sha8" }, wantReason: "head-changed-since-eval"},
		{name: "merge api error", mutate: func(fx *contributorLaneFixture) { fx.mergeStatus = http.StatusConflict }, wantReason: "merge-failed", wantErr: true, wantMerges: 1},
		{
			name: "merge queue required is a skip not an error",
			mutate: func(fx *contributorLaneFixture) {
				fx.mergeStatus = http.StatusMethodNotAllowed
				fx.mergeMessage = mergeQueueRequiredTestMessage
			},
			wantReason: autoMergeReasonMergeQueueRequired, wantMerges: 1,
		},
		{name: "merge not applied", mutate: func(fx *contributorLaneFixture) { fx.mergeNotApplied = true }, wantReason: "merge-not-applied", wantMerges: 1},
		{name: "comment failure does not undo merge", mutate: func(fx *contributorLaneFixture) { fx.commentStatus = http.StatusInternalServerError }, wantMerges: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			if tc.policy != nil {
				tc.policy(&policy)
			}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorLaneVerdict: tc.verdict, ContributorSoak: soakedTracker(clock)})
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges || fx.updates != tc.wantUpdate {
				t.Fatalf("merges=%d updates=%d, want %d/%d", fx.merges, fx.updates, tc.wantMerges, tc.wantUpdate)
			}
			if tc.wantReason == "" && !tc.wantErr {
				if event.Tier != contributorLaneTier || event.Number != 7 || event.MergeSHA != "merge7" || event.Author != contributorLaneTestAuthor {
					t.Fatalf("event = %+v, want contributor-lane merge of PR 7", event)
				}
			}
		})
	}
}

func TestTrySweepContributorLanePRMergeCommentText(t *testing.T) {
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
	if _, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy); err != nil || reason != "" {
		t.Fatalf("reason=%q err=%v, want merge", reason, err)
	}
	want := "Merged by hive contributor-lane auto-merge: CI green for ≥120m, reviewer verdict approve, claim by @mrbobbytables on #42 verified."
	if fx.comments != 1 || fx.lastComment != want {
		t.Fatalf("comment = %q (n=%d), want %q", fx.lastComment, fx.comments, want)
	}
	noReviewer := contributorLaneMergeComment("@bob", contributorClaimMatch{Issue: 5}, 30*time.Minute, ContributorLaneVerdict{})
	if !strings.Contains(noReviewer, "≥30m, no reviewer agent configured, claim by @bob on #5") {
		t.Fatalf("no-reviewer comment = %q", noReviewer)
	}
	notRequired := contributorLaneMergeComment("bob", contributorClaimMatch{Issue: 5}, 30*time.Minute, ContributorLaneVerdict{Configured: true})
	if !strings.Contains(notRequired, "≥30m, reviewer verdict not required, claim by @bob on #5") {
		t.Fatalf("verdict-not-required comment = %q", notRequired)
	}
	registry := contributorLaneMergeComment("@bob", contributorClaimMatch{RegistryTier: "trusted"}, 30*time.Minute, ContributorLaneVerdict{Configured: true, Approved: true})
	if !strings.Contains(registry, "≥30m, reviewer verdict approve, @bob holds the trusted tier in this hive's contributor registry.") {
		t.Fatalf("registry comment = %q", registry)
	}
}

func TestTrySweepContributorLaneTrustedHumanApprovalClearsRequiresHuman(t *testing.T) {
	for _, tt := range []struct {
		name       string
		verdict    ContributorLaneVerdict
		wantReason string
		wantMerges int
	}{
		{
			name:       "requires human cleared",
			verdict:    ContributorLaneVerdict{Configured: true, Verdict: "requires_human"},
			wantMerges: 1,
		},
		{
			name:       "changes requested not cleared",
			verdict:    ContributorLaneVerdict{Configured: true, Verdict: "changes_requested"},
			wantReason: contributorLaneReasonChangesRequested,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.reviews = []map[string]any{{
				"id": 1, "state": "APPROVED", "commit_id": "sha7", "user": map[string]string{"login": "clubanderson"},
			}}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{
				ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict { return tt.verdict },
				ContributorSoak:        soakedTracker(clock),
			})
			engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.wantReason || fx.merges != tt.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d", reason, fx.merges, tt.wantReason, tt.wantMerges)
			}
		})
	}
}

// An owner's dashboard Approve admits the untrusted-author head
// (hivecommons/hive#15136) once the sentinel it released stays released:
// the recorded approval by a trusted merger admits the newcomer-tier claim
// the lane would otherwise refuse, and the PR merges. While the alert label
// is back on the PR — the sentinel re-applying it undid the approval on
// dibs#510 — the lane still stands down with sentinel-blocked, so the label
// memory, not the admission, is what the owner's approval depends on.
func TestTrySweepContributorLaneOperatorApprovalAdmitsReleasedSentinel(t *testing.T) {
	for _, tt := range []struct {
		name       string
		labels     []string
		wantReason string
		wantMerges int
	}{
		{name: "sentinel released", labels: []string{"stage/085-needs-decision"}, wantMerges: 1},
		{name: "sentinel re-applied", labels: []string{"stage/085-needs-decision", "hive-sentinel"}, wantReason: contributorLaneReasonSentinelBlocked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.labels = tt.labels
			fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=newcomer verified=true")
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			store := review.NewHumanChangeRequests("")
			if err := store.Record(review.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: "sha7", Actor: "clubanderson", Kind: review.HumanChangeRequestKindApprove, Source: review.HumanChangeRequestSourceDecision, SourceID: 1}); err != nil {
				t.Fatal(err)
			}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), HumanChangeRequests: store})
			engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.wantReason || fx.merges != tt.wantMerges {
				t.Fatalf("reason=%q merges=%d, want %q/%d", reason, fx.merges, tt.wantReason, tt.wantMerges)
			}
			if tt.wantMerges == 1 && (!strings.Contains(fx.lastComment, "operator-approval:clubanderson") || event.Number != 7) {
				t.Fatalf("merge comment must name the admission: %q (event %+v)", fx.lastComment, event)
			}
		})
	}
}

// registryTiers returns a ContributorRegistryTierFunc over a fixed
// login→tier map (case-insensitive on login), standing in for the
// contributor registry on the hive's PVC.
func registryTiers(tiers map[string]string) ContributorRegistryTierFunc {
	return func(login string) (string, bool) {
		for k, v := range tiers {
			if strings.EqualFold(k, login) {
				return v, true
			}
		}
		return "", false
	}
}

func mergedHistoryPR(author, title string, labels ...string) map[string]any {
	outLabels := make([]map[string]string, 0, len(labels))
	for _, label := range labels {
		outLabels = append(outLabels, map[string]string{"name": label})
	}
	return map[string]any{
		"number":    100,
		"title":     title,
		"body":      "Done.",
		"merged_at": "2026-10-15T12:00:00Z",
		"user":      map[string]string{"login": author},
		"labels":    outLabels,
	}
}

// Regression for the contributor-lane registry admission path: an operator
// marked mrbobbytables `trusted` in the contributor registry, yet #12045
// (`Part of #12002`, a reference not a closing keyword) skipped with
// no-linked-issue and #11756 (claimed outside the relay) with no-claim. A
// RequireTiers tier in the registry must admit the PR without a marker,
// while a non-trusted or absent registry entry leaves the marker path and
// its skip reasons untouched.
func TestTrySweepContributorLanePRRegistryAdmission(t *testing.T) {
	noMarker := func(fx *contributorLaneFixture) {
		fx.body = "Part of #42\n\nSome change."
		fx.issueComments = nil
	}
	for _, tc := range []struct {
		name       string
		registry   ContributorRegistryTierFunc
		mutate     func(fx *contributorLaneFixture)
		policy     func(p *ContributorLanePolicy)
		wantReason string
		wantMerges int
		wantAdmit  string // "registry" or "claim" when a merge is expected
	}{
		{
			name:       "registry trusted admits without linked issue",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate:     noMarker,
			wantMerges: 1,
			wantAdmit:  "registry",
		},
		{
			name:       "registry merger admits without claim marker",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "merger"}),
			mutate:     func(fx *contributorLaneFixture) { fx.issueComments = nil },
			wantMerges: 1,
			wantAdmit:  "registry",
		},
		{
			name:       "registry login match is case-insensitive",
			registry:   registryTiers(map[string]string{"MrBobbyTables": "Trusted"}),
			mutate:     noMarker,
			wantMerges: 1,
			wantAdmit:  "registry",
		},
		{
			name:     "registry trusted still passes the diff fences",
			registry: registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate: func(fx *contributorLaneFixture) {
				noMarker(fx)
				fx.files = []map[string]any{{"filename": ".github/workflows/ci.yml", "status": "modified", "additions": 1, "deletions": 1}}
			},
			wantReason: contributorLaneReasonWorkflowPath,
		},
		{
			name:       "registry trusted admits a first-time contributor",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.association = "FIRST_TIME_CONTRIBUTOR" },
			wantMerges: 1,
			wantAdmit:  "registry",
		},
		{
			name:       "registry trusted admits association none",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.association = "NONE" },
			wantMerges: 1,
			wantAdmit:  "registry",
		},
		{
			name:       "registry newcomer first-timer without marker skips on association",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "newcomer"}),
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.association = "FIRST_TIME_CONTRIBUTOR" },
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "no registry entry first-timer without marker skips on association",
			registry:   registryTiers(nil),
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.association = "FIRST_TIME_CONTRIBUTOR" },
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "registry path disabled first-timer without marker skips on association",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate:     func(fx *contributorLaneFixture) { noMarker(fx); fx.association = "FIRST_TIME_CONTRIBUTOR" },
			policy:     func(p *ContributorLanePolicy) { p.RegistryTiersSufficient = false },
			wantReason: contributorLaneReasonAuthorAssociation,
		},
		{
			name:       "registry newcomer falls through to no-linked-issue",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "newcomer"}),
			mutate:     noMarker,
			wantReason: contributorLaneReasonNoLinkedIssue,
		},
		{
			name:       "registry contributor falls through to no-claim",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "contributor"}),
			mutate:     func(fx *contributorLaneFixture) { fx.issueComments = nil },
			wantReason: contributorLaneReasonNoClaim,
		},
		{
			name:       "registry revoked falls through to marker path",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "revoked"}),
			mutate:     noMarker,
			wantReason: contributorLaneReasonNoLinkedIssue,
		},
		{
			name:       "registry tier outside require_tiers falls through",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate:     noMarker,
			policy:     func(p *ContributorLanePolicy) { p.RequireTiers = map[string]bool{"merger": true} },
			wantReason: contributorLaneReasonNoLinkedIssue,
		},
		{
			name:       "no registry entry keeps marker path",
			registry:   registryTiers(map[string]string{"someoneelse": "trusted"}),
			mutate:     noMarker,
			wantReason: contributorLaneReasonNoLinkedIssue,
		},
		{
			name:       "no registry entry with valid claim merges via marker",
			registry:   registryTiers(nil),
			wantMerges: 1,
			wantAdmit:  "claim",
		},
		{
			name:       "nil registry lookup keeps marker path",
			mutate:     noMarker,
			wantReason: contributorLaneReasonNoLinkedIssue,
		},
		{
			name:       "registry path disabled by policy",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			mutate:     noMarker,
			policy:     func(p *ContributorLanePolicy) { p.RegistryTiersSufficient = false },
			wantReason: contributorLaneReasonNoLinkedIssue,
		},
		{
			name:       "registry trusted with a valid claim still merges",
			registry:   registryTiers(map[string]string{contributorLaneTestAuthor: "trusted"}),
			wantMerges: 1,
			wantAdmit:  "registry",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.RegistryTiersSufficient = true
			if tc.policy != nil {
				tc.policy(&policy)
			}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: tc.registry, ContributorSoak: soakedTracker(clock)})
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merges = %d, want %d", fx.merges, tc.wantMerges)
			}
			if tc.wantMerges == 0 {
				return
			}
			if event.Tier != contributorLaneTier || event.Author != contributorLaneTestAuthor || event.MergeSHA != "merge7" {
				t.Fatalf("event = %+v, want contributor-lane merge of PR 7", event)
			}
			switch tc.wantAdmit {
			case "registry":
				if !strings.Contains(fx.lastComment, "holds the") || !strings.Contains(fx.lastComment, "contributor registry") {
					t.Fatalf("comment = %q, want registry admission", fx.lastComment)
				}
			case "claim":
				if !strings.Contains(fx.lastComment, "claim by @"+contributorLaneTestAuthor+" on #42 verified") {
					t.Fatalf("comment = %q, want claim admission", fx.lastComment)
				}
			}
		})
	}
}

func TestTrySweepContributorLanePRAutoPromotion(t *testing.T) {
	cleanHistory := func(n int) []map[string]any {
		history := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			history = append(history, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return history
	}
	// withMergeSHA stamps the merge commit a later revert PR can name.
	withMergeSHA := func(pr map[string]any, sha string) map[string]any {
		pr["merge_commit_sha"] = sha
		return pr
	}
	// maintainerRevert is a merged revert PR by someone other than the author.
	maintainerRevert := func(title, body string) map[string]any {
		pr := mergedHistoryPR("maintainer", title)
		pr["body"] = body
		return pr
	}
	for _, tc := range []struct {
		name         string
		history      []map[string]any
		registry     ContributorRegistryTierFunc
		threshold    int
		wantReason   string
		wantMerges   int
		wantComments int
	}{
		{name: "five clean merged PRs admit and audit", history: cleanHistory(5), threshold: 5, wantMerges: 1, wantComments: 2},
		{name: "below threshold stays untrusted", history: cleanHistory(4), threshold: 5, wantReason: contributorLaneReasonClaimTier},
		{name: "revert blocks promotion", history: append(cleanHistory(2), mergedHistoryPR(contributorLaneTestAuthor, "Revert \"fix: clean change\"")), threshold: 3, wantReason: contributorLaneReasonClaimTier},
		{name: "sentinel blocks promotion", history: append(cleanHistory(2), mergedHistoryPR(contributorLaneTestAuthor, "fix: risky", defaultSentinelLabel)), threshold: 3, wantReason: contributorLaneReasonClaimTier},
		// A clean history never outranks the operator's explicit revocation.
		{name: "revoked registry tier blocks promotion", history: cleanHistory(5), registry: registryTiers(map[string]string{contributorLaneTestAuthor: "revoked"}), threshold: 5, wantReason: contributorLaneReasonClaimTier},
		{name: "other registry tier still promotes", history: cleanHistory(5), registry: registryTiers(map[string]string{contributorLaneTestAuthor: "contributor"}), threshold: 5, wantMerges: 1, wantComments: 2},
		// The author's PR was undone by a maintainer's revert: evidence lives on
		// the revert PR, not on the author's own title or labels.
		{name: "sha-reverted history blocks promotion", history: append(append([]map[string]any{maintainerRevert("Revert \"something else\"", "This reverts commit abc1234def5678.")}, cleanHistory(2)...), withMergeSHA(mergedHistoryPR(contributorLaneTestAuthor, "fix: bad change"), "abc1234def56789012345678901234567890abcd")), threshold: 3, wantReason: contributorLaneReasonClaimTier},
		{name: "title-reverted history blocks promotion", history: append(append([]map[string]any{maintainerRevert("Revert \"fix: bad change (#100)\"", "")}, cleanHistory(2)...), mergedHistoryPR(contributorLaneTestAuthor, "fix: bad change")), threshold: 3, wantReason: contributorLaneReasonClaimTier},
		{name: "maintainer revert of someone else does not block", history: append([]map[string]any{maintainerRevert("Revert \"chore: unrelated\"", "This reverts commit 0000000.")}, cleanHistory(3)...), threshold: 3, wantMerges: 1, wantComments: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
			fx.mergedHistory = tc.history
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.AutoPromoteAfterMergedPR = tc.threshold
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorRegistryTier: tc.registry, ContributorSoak: soakedTracker(clock)})
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merges = %d, want %d", fx.merges, tc.wantMerges)
			}
			if fx.comments != tc.wantComments {
				t.Fatalf("comments = %d, want %d last=%q", fx.comments, tc.wantComments, fx.lastComment)
			}
			if tc.wantMerges > 0 {
				if event.Tier != contributorLaneTier || !strings.Contains(fx.lastComment, fmt.Sprintf("auto-promoted after %d clean merged PRs", tc.threshold)) {
					t.Fatalf("event=%+v comment=%q", event, fx.lastComment)
				}
			}
		})
	}
}

// Regression for #14517: the clean-merge ratchet counts across every
// repository the hive manages by default, so a contributor trusted in the
// main repo is admitted on their first PR to a satellite repo; scope=repo
// keeps the old per-repo count; revert and sentinel evidence in any
// repository still blocks; a failed repo search degrades to the own-repo
// count instead of blocking the sweep.
func TestTrySweepContributorLanePRAutoPromotionScope(t *testing.T) {
	cleanHistory := func(n int) []map[string]any {
		history := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			history = append(history, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return history
	}
	hiveRepos := []string{"widget", "hive", "docs", "homebrew-hive"}
	for _, tc := range []struct {
		name         string
		scope        string
		own          []map[string]any
		other        map[string][]map[string]any
		searchStatus int
		wantReason   string
		wantMerges   int
		wantSearches int
		wantLists    []string
		wantComment  string
	}{
		{
			name:         "hive scope admits on first PR to a satellite repo",
			own:          nil,
			other:        map[string][]map[string]any{"hive": cleanHistory(9), "docs": cleanHistory(1)},
			wantMerges:   1,
			wantSearches: 1,
			wantLists:    []string{"hive"},
			wantComment:  "merged=5 scope=hive repos=widget:0,hive:5 -->",
		},
		{
			name:         "hive scope sums clean merges across repos",
			own:          cleanHistory(2),
			other:        map[string][]map[string]any{"hive": cleanHistory(2), "docs": cleanHistory(1)},
			wantMerges:   1,
			wantSearches: 1,
			wantLists:    []string{"hive", "docs"},
			wantComment:  "merged=5 scope=hive repos=widget:2,hive:2,docs:1 -->",
		},
		{
			name:         "hive scope skips repos without the author's merges",
			own:          cleanHistory(1),
			other:        map[string][]map[string]any{"hive": cleanHistory(2), "docs": cleanHistory(0), "homebrew-hive": {mergedHistoryPR("someone-else", "fix: theirs")}},
			wantReason:   contributorLaneReasonClaimTier,
			wantSearches: 1,
			wantLists:    []string{"hive"},
		},
		{
			name:         "hive scope blocks on a revert in another repo",
			own:          cleanHistory(2),
			other:        map[string][]map[string]any{"hive": append(cleanHistory(1), mergedHistoryPR(contributorLaneTestAuthor, "Revert \"fix: clean change\"")), "docs": cleanHistory(3)},
			wantReason:   contributorLaneReasonClaimTier,
			wantSearches: 1,
			wantLists:    []string{"hive"},
		},
		{
			name:         "hive scope blocks on a sentinel label in another repo",
			own:          cleanHistory(4),
			other:        map[string][]map[string]any{"docs": {mergedHistoryPR(contributorLaneTestAuthor, "fix: risky", defaultSentinelLabel)}},
			wantReason:   contributorLaneReasonClaimTier,
			wantSearches: 1,
			wantLists:    []string{"docs"},
		},
		{
			name:         "repo scope counts the PR's repository only",
			scope:        "repo",
			own:          cleanHistory(1),
			other:        map[string][]map[string]any{"hive": cleanHistory(9)},
			wantReason:   contributorLaneReasonClaimTier,
			wantSearches: 0,
		},
		{
			name:        "repo scope still admits on own history",
			scope:       "repo",
			own:         cleanHistory(5),
			other:       map[string][]map[string]any{"hive": cleanHistory(9)},
			wantMerges:  1,
			wantComment: "merged=5 scope=repo repos=widget:5 -->",
		},
		{
			name:        "own repository meeting the threshold needs no search",
			own:         cleanHistory(5),
			other:       map[string][]map[string]any{"hive": cleanHistory(9)},
			wantMerges:  1,
			wantComment: "merged=5 scope=hive repos=widget:5 -->",
		},
		{
			name:         "search failure degrades to the PR's repository",
			own:          cleanHistory(1),
			other:        map[string][]map[string]any{"hive": cleanHistory(9)},
			searchStatus: http.StatusUnprocessableEntity,
			wantReason:   contributorLaneReasonClaimTier,
			wantSearches: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
			fx.mergedHistory = tc.own
			fx.otherHistory = tc.other
			fx.searchStatus = tc.searchStatus
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.AutoPromoteAfterMergedPR = 5
			policy.AutoPromotionScope = tc.scope
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngineRepos(api.URL, hiveRepos, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
			event, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if fx.merges != tc.wantMerges {
				t.Fatalf("merges = %d, want %d", fx.merges, tc.wantMerges)
			}
			if fx.searches != tc.wantSearches {
				t.Fatalf("searches = %d, want %d (q=%q)", fx.searches, tc.wantSearches, fx.searchQuery)
			}
			if fx.searches > 0 {
				for _, want := range []string{"is:pr", "is:merged", "author:" + contributorLaneTestAuthor, "repo:acme/hive", "repo:acme/docs", "repo:acme/homebrew-hive"} {
					if !strings.Contains(fx.searchQuery, want) {
						t.Fatalf("search query %q missing %q", fx.searchQuery, want)
					}
				}
				// The answer is cached per author for every active repository,
				// so the PR's own repository is searched too; it is never walked
				// twice (wantLists below).
				if !strings.Contains(fx.searchQuery, "repo:acme/widget") {
					t.Fatalf("search query %q must cover the PR's own repository for the per-author cache", fx.searchQuery)
				}
			}
			if strings.Join(fx.otherLists, ",") != strings.Join(tc.wantLists, ",") {
				t.Fatalf("other repos listed = %v, want %v", fx.otherLists, tc.wantLists)
			}
			if tc.wantMerges > 0 {
				if event.Tier != contributorLaneTier || !strings.Contains(fx.lastComment, "auto-promoted after 5 clean merged PRs") {
					t.Fatalf("event=%+v comment=%q", event, fx.lastComment)
				}
				where := "in this hive's repositories"
				if tc.scope == "repo" {
					where = "in this repository"
				}
				if !strings.Contains(fx.lastComment, where) {
					t.Fatalf("merge comment %q should say %q", fx.lastComment, where)
				}
			}
			if tc.wantComment != "" {
				var auditComment string
				for _, body := range fx.allComments {
					if strings.HasPrefix(body, contributorAutoPromotionMarkerPrefix) {
						auditComment = body
					}
				}
				if !strings.Contains(auditComment, tc.wantComment) {
					t.Fatalf("audit comment %q should contain %q", auditComment, tc.wantComment)
				}
			}
		})
	}
}

// Regression for hivecommons/hive#15443: the promotion announcement is
// author-wide, posted once per (hive, author). Two open PRs by the same
// author in one sweep get one comment; a restart that reloads the stage
// store posts none; revert evidence that demotes the author and a later
// clean history that re-promotes them post exactly one more.
func TestAdmitContributorAnnouncesPromotionOncePerAuthor(t *testing.T) {
	cleanHistory := func(n int) []map[string]any {
		history := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			history = append(history, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return history
	}
	revertedHistory := append(cleanHistory(2), mergedHistoryPR(contributorLaneTestAuthor, "Revert \"fix: clean change\""))
	pr := func(number int) *gh.PullRequest {
		return &gh.PullRequest{
			Number: gh.Ptr(number),
			Body:   gh.Ptr("Fixes #42"),
			User:   &gh.User{Login: gh.Ptr(contributorLaneTestAuthor)},
			Head:   &gh.PullRequestBranch{SHA: gh.Ptr(fmt.Sprintf("sha%d", number))},
		}
	}

	fx := validContributorFixture()
	fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
	fx.mergedHistory = cleanHistory(5)
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	policy.AutoPromoteAfterMergedPR = 5
	clock := &contributorLaneClock{at: time.Date(2026, 10, 29, 17, 20, 0, 0, time.UTC)}
	statePath := filepath.Join(t.TempDir(), "stage.json")
	store := stagelabels.NewStore(statePath, clock.now)
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), StageStore: store})

	for _, step := range []struct {
		name          string
		prs           []int
		history       []map[string]any
		restart       bool
		wantPromoted  bool
		wantComments  int
		wantTargets   []int
		wantAnnounced bool
	}{
		{name: "two PRs by the author in one sweep announce once", prs: []int{7, 8}, wantPromoted: true, wantComments: 1, wantTargets: []int{7}, wantAnnounced: true},
		{name: "a later PR by the author announces nothing", prs: []int{9}, wantPromoted: true, wantComments: 1, wantTargets: []int{7}, wantAnnounced: true},
		{name: "a restart reloading the store announces nothing", prs: []int{8, 9}, restart: true, wantPromoted: true, wantComments: 1, wantTargets: []int{7}, wantAnnounced: true},
		{name: "revert evidence demotes and forgets the announcement", prs: []int{7}, history: revertedHistory, wantComments: 1, wantTargets: []int{7}},
		{name: "regaining promotion announces once more", prs: []int{8, 7}, history: cleanHistory(5), wantPromoted: true, wantComments: 2, wantTargets: []int{7, 8}, wantAnnounced: true},
		{name: "and then stays quiet again", prs: []int{9}, wantPromoted: true, wantComments: 2, wantTargets: []int{7, 8}, wantAnnounced: true},
	} {
		if step.history != nil {
			fx.mergedHistory = step.history
		}
		if step.restart {
			if err := store.Save(); err != nil {
				t.Fatalf("%s: save: %v", step.name, err)
			}
			store = stagelabels.NewStore(statePath, clock.now)
			if err := store.Load(); err != nil {
				t.Fatalf("%s: load: %v", step.name, err)
			}
			engine = newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), StageStore: store})
		}
		for _, number := range step.prs {
			match, err := engine.admitContributor(context.Background(), "acme", "widget", pr(number), policy)
			if err != nil {
				t.Fatalf("%s: admit #%d: %v", step.name, number, err)
			}
			if match.admittedByAutoPromotion() != step.wantPromoted {
				t.Fatalf("%s: admit #%d promoted = %v, want %v (match=%+v)", step.name, number, match.admittedByAutoPromotion(), step.wantPromoted, match)
			}
		}
		if fx.comments != step.wantComments || !slices.Equal(fx.commentTargets, step.wantTargets) {
			t.Fatalf("%s: comments = %d on %v, want %d on %v", step.name, fx.comments, fx.commentTargets, step.wantComments, step.wantTargets)
		}
		if got := store.TrustPromotionAnnounced(contributorLaneTestAuthor); got != step.wantAnnounced {
			t.Fatalf("%s: announced = %v, want %v", step.name, got, step.wantAnnounced)
		}
	}
	for _, want := range []string{
		"<!-- hive:trust-promoted login=" + contributorLaneTestAuthor + " merged=5 scope=hive repos=widget:5 -->",
		"@" + contributorLaneTestAuthor + " is now trusted for contributor-lane merging in this hive (last 5 merged PRs: no reverts or sentinel alerts). Applies to their open and future PRs here; CI, review verdicts, diff fences, soak and branch protection still apply.",
	} {
		if !strings.Contains(fx.lastComment, want) {
			t.Fatalf("announcement %q missing %q", fx.lastComment, want)
		}
	}
	if strings.Contains(fx.lastComment, "on this PR") {
		t.Fatalf("announcement %q must not read as per-PR", fx.lastComment)
	}
}

// A store rebuilt from scratch must not repeat an announcement that is
// already on the PR, in either the current or the pre-#15443 marker form;
// a failed post releases the claim so the next PR announces instead.
func TestCommentContributorAutoPromotionMarkerFallbackAndRelease(t *testing.T) {
	cleanHistory := func(n int) []map[string]any {
		history := make([]map[string]any, 0, n)
		for i := 0; i < n; i++ {
			history = append(history, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return history
	}
	for _, tc := range []struct {
		name          string
		existing      string
		commentStatus int
		wantComments  int
		wantAnnounced bool
	}{
		{name: "current marker on the PR", existing: "<!-- hive:trust-promoted login=" + contributorLaneTestAuthor + " merged=5 scope=hive repos=widget:5 -->\nold", wantAnnounced: true},
		{name: "legacy marker on the PR", existing: "<!-- hive:contributor-auto-promoted author=" + contributorLaneTestAuthor + " merged=5 scope=hive repos=widget:5 -->\nold", wantAnnounced: true},
		{name: "another author's marker does not count", existing: "<!-- hive:trust-promoted login=someone-else merged=5 scope=hive repos=widget:5 -->\nold", wantComments: 1, wantAnnounced: true},
		{name: "failed post releases the claim", commentStatus: http.StatusBadGateway, wantComments: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
			fx.mergedHistory = cleanHistory(5)
			fx.commentStatus = tc.commentStatus
			if tc.existing != "" {
				fx.prComments = []map[string]any{{"id": 1, "user": map[string]string{"login": testHiveAppBotLogin}, "body": tc.existing}}
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.AutoPromoteAfterMergedPR = 5
			clock := &contributorLaneClock{at: time.Date(2026, 10, 29, 17, 20, 0, 0, time.UTC)}
			store := stagelabels.NewStore("", clock.now)
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), StageStore: store})
			pr := &gh.PullRequest{Number: gh.Ptr(7), Body: gh.Ptr("Fixes #42"), User: &gh.User{Login: gh.Ptr(contributorLaneTestAuthor)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}}
			match, err := engine.admitContributor(context.Background(), "acme", "widget", pr, policy)
			if err != nil || !match.admittedByAutoPromotion() {
				t.Fatalf("admit = %+v, %v", match, err)
			}
			if fx.comments != tc.wantComments {
				t.Fatalf("comments = %d, want %d", fx.comments, tc.wantComments)
			}
			if got := store.TrustPromotionAnnounced(contributorLaneTestAuthor); got != tc.wantAnnounced {
				t.Fatalf("announced = %v, want %v", got, tc.wantAnnounced)
			}
		})
	}
}

func TestContributorMergedHistoryHelpers(t *testing.T) {
	h := contributorMergedHistory{HiveWide: true, Repos: []contributorRepoMerges{{Repo: "hive", Clean: 8}, {Repo: "docs", Clean: 1}}}
	if got := h.repoBreakdown(); got != "hive:8,docs:1" {
		t.Fatalf("repoBreakdown = %q", got)
	}
	if h.scopeName() != "hive" || (contributorMergedHistory{}).scopeName() != "repo" {
		t.Fatalf("scopeName wrong: %q / %q", h.scopeName(), (contributorMergedHistory{}).scopeName())
	}
	for _, tc := range []struct{ in, want string }{
		{"https://api.github.com/repos/HiveCommons/Docs", "hivecommons/docs"},
		{"https://api.github.com/repos/acme/widget/", "acme/widget"},
		{"", ""},
		{"https://api.github.com/repos/", ""},
		{"nonsense", ""},
	} {
		if got := repoFromRepositoryURL(tc.in); got != tc.want {
			t.Fatalf("repoFromRepositoryURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	for _, tc := range []struct {
		scope string
		want  bool
	}{{"", true}, {"hive", true}, {" HIVE ", true}, {"repo", false}, {" Repo ", false}} {
		if got := (ContributorLanePolicy{AutoPromotionScope: tc.scope}).hiveWideAutoPromotion(); got != tc.want {
			t.Fatalf("hiveWideAutoPromotion(%q) = %v, want %v", tc.scope, got, tc.want)
		}
	}
	if contributorAutoPromotionWhere(true) != "in this hive's repositories" || contributorAutoPromotionWhere(false) != "in this repository" {
		t.Fatal("contributorAutoPromotionWhere wording changed")
	}
	var nilEngine *Engine
	if got := nilEngine.contributorMergedRepos(context.Background(), "acme", "widget", "bob"); got != nil {
		t.Fatalf("nil engine repos = %v", got)
	}
	history, err := nilEngine.contributorCleanMergedHistory(context.Background(), "acme", "widget", "bob", 5, true)
	if err != nil || history.Clean || history.Reason != "disabled" {
		t.Fatalf("nil engine history = %+v err=%v", history, err)
	}
}

func TestContributorLaneAdmitPathAndRegistryTierHelpers(t *testing.T) {
	if got := contributorLaneAdmitPath(contributorClaimMatch{RegistryTier: "trusted"}); got != contributorLaneReasonRegistryTrusted {
		t.Fatalf("admit path = %q, want %q", got, contributorLaneReasonRegistryTrusted)
	}
	if got := contributorLaneAdmitPath(contributorClaimMatch{Issue: 42}); got != "claim" {
		t.Fatalf("admit path = %q, want claim", got)
	}
	// A person's Approve outranks the registry tier for that head (#15136)
	// and the merge comment names who admitted it.
	operator := contributorClaimMatch{OperatorLogin: "octo", RegistryTier: "trusted"}
	if got := contributorLaneAdmitPath(operator); got != contributorLaneReasonOperatorApproval || contributorLaneReasonOperatorApproval != "contributor-lane:operator-approval" {
		t.Fatalf("operator admit path = %q", got)
	}
	if c := contributorLaneMergeComment("mallory", operator, 30*time.Minute, ContributorLaneVerdict{}); !strings.Contains(c, "@octo") || !strings.Contains(c, "operator-approval:octo") {
		t.Fatalf("operator merge comment = %q", c)
	}
	if contributorLaneReasonRegistryTrusted != "contributor-lane:registry-trusted" {
		t.Fatalf("admit reason = %q", contributorLaneReasonRegistryTrusted)
	}
	policy := defaultContributorLanePolicy()
	policy.RegistryTiersSufficient = true
	var nilEngine *Engine
	if got := nilEngine.admittedRegistryTier("bob", policy); got != "" {
		t.Fatalf("nil engine tier = %q, want empty", got)
	}
	engine := &Engine{contributorRegistryTier: registryTiers(map[string]string{"bob": "  TRUSTED "})}
	if got := engine.admittedRegistryTier("@bob", policy); got != "trusted" {
		t.Fatalf("tier = %q, want trusted (trimmed, lower-cased, @ stripped)", got)
	}
	if got := engine.admittedRegistryTier("", policy); got != "" {
		t.Fatalf("empty login tier = %q, want empty", got)
	}
	engine = &Engine{contributorRegistryTier: registryTiers(map[string]string{"bob": ""})}
	if got := engine.admittedRegistryTier("bob", policy); got != "" {
		t.Fatalf("blank registry tier = %q, want empty", got)
	}
}

// Regression for #12041: a self-registered newcomer whose login was never
// proven gets an App-stamped claim on a leased issue, opens a PR from their
// own account, and has green CI, a soaked head and an approving verdict. The
// claim proves only the lease, so the tier must refuse to merge.
func TestContributorLaneRefusesSelfRegisteredNewcomerClaim(t *testing.T) {
	for _, fields := range []string{" tier=newcomer verified=false", ""} {
		fx := validContributorFixture()
		fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, fields)
		api := newContributorLaneAPI(t, &fx)
		policy := defaultContributorLanePolicy()
		clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
		engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})
		result, err := engine.SweepContributorLaneAutoMerges(context.Background(), AutoMergeSweepOptions{})
		api.Close()
		if err != nil {
			t.Fatalf("marker %q: sweep err = %v", fields, err)
		}
		if len(result.Merged) != 0 || fx.merges != 0 || result.Skipped != 1 {
			t.Fatalf("marker %q: newcomer claim merged: result=%+v merges=%d", fields, result, fx.merges)
		}
	}
}

func TestContributorLaneDiffReason(t *testing.T) {
	cf := func(name, status string, adds, dels int) intent.ChangedFile {
		return intent.ChangedFile{Filename: name, Status: status, Additions: adds, Deletions: dels}
	}
	for _, tc := range []struct {
		name  string
		files []intent.ChangedFile
		want  string
	}{
		{name: "no files", want: ""},
		{name: "source only", files: []intent.ChangedFile{cf("main.go", "modified", 10, 10)}, want: ""},
		{name: "workflow", files: []intent.ChangedFile{cf(".github/workflows/ci.yml", "added", 3, 0)}, want: contributorLaneReasonWorkflowPath},
		{name: "nested workflow", files: []intent.ChangedFile{cf("sub/.github/workflows/ci.yml", "modified", 1, 1)}, want: contributorLaneReasonWorkflowPath},
		{name: "composite action", files: []intent.ChangedFile{cf(".github/actions/setup/action.yml", "modified", 1, 1)}, want: contributorLaneReasonWorkflowPath},
		{name: "deleted test", files: []intent.ChangedFile{cf("a/b_test.go", "removed", 0, 1)}, want: contributorLaneReasonTestsRemoved},
		{name: "deleted js spec", files: []intent.ChangedFile{cf("web/app.spec.ts", "removed", 0, 1)}, want: contributorLaneReasonTestsRemoved},
		{name: "small test trim", files: []intent.ChangedFile{cf("a/b_test.go", "modified", 0, 10)}, want: ""},
		{name: "gutted tests", files: []intent.ChangedFile{cf("a/b_test.go", "modified", 5, 30)}, want: contributorLaneReasonTestsRemoved},
		{name: "rewritten tests", files: []intent.ChangedFile{cf("a/b_test.go", "modified", 20, 30)}, want: ""},
	} {
		if got := contributorLaneDiffReason(tc.files); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestTrySweepContributorLanePRSoakTiming(t *testing.T) {
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	tracker := NewContributorSoakTracker()
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: tracker})
	ctx := context.Background()

	// First green observation: soak starts now, nothing merges. The reason
	// is the bare token — the countdown is not in it (hivecommons/hive#15729:
	// a reason that moved every pass re-stamped the stage record); the
	// clock rides on the evaluation.
	eval, reason, err := engine.evaluateContributorLanePR(ctx, "widget", "acme", "widget", 7, policy, false)
	if err != nil || reason != contributorLaneReasonSoaking {
		t.Fatalf("first pass reason=%q err=%v", reason, err)
	}
	if !eval.soakStartedAt.Equal(clock.at) || eval.soakRemaining != 2*time.Hour {
		t.Fatalf("first pass soak clock = %v / %v, want %v / 2h", eval.soakStartedAt, eval.soakRemaining, clock.at)
	}
	firstGreen, ok := tracker.FirstGreen("acme", "widget", 7, "sha7")
	if !ok || !firstGreen.Equal(clock.at) {
		t.Fatalf("first green = %v ok=%v, want %v", firstGreen, ok, clock.at)
	}

	// Half-way through the soak the reason is unchanged; the clock counts.
	clock.advance(time.Hour)
	eval, reason, err = engine.evaluateContributorLanePR(ctx, "widget", "acme", "widget", 7, policy, false)
	if err != nil || reason != contributorLaneReasonSoaking {
		t.Fatalf("mid-soak reason=%q err=%v", reason, err)
	}
	if !eval.soakStartedAt.Equal(firstGreen) || eval.soakRemaining != time.Hour {
		t.Fatalf("mid-soak clock = %v / %v, want %v / 1h", eval.soakStartedAt, eval.soakRemaining, firstGreen)
	}
	if _, reason, err = engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy); err != nil || reason != contributorLaneReasonSoaking {
		t.Fatalf("mid-soak sweep reason=%q err=%v", reason, err)
	}
	if got, _ := tracker.FirstGreen("acme", "widget", 7, "sha7"); !got.Equal(firstGreen) {
		t.Fatalf("first green moved to %v", got)
	}
	if fx.merges != 0 {
		t.Fatalf("merged during soak")
	}

	// Once the soak elapses the PR merges.
	clock.advance(time.Hour)
	event, reason, err := engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy)
	if err != nil || reason != "" || fx.merges != 1 || event.Tier != contributorLaneTier {
		t.Fatalf("post-soak reason=%q err=%v merges=%d event=%+v", reason, err, fx.merges, event)
	}
}

// A head this process has not seen yet measures its soak from when the
// forge says it went green (the check run's completed_at), not from the
// first sighting — so a pod restart or upgrade mid-soak does not restart it.
func TestTrySweepContributorLanePRSoakSurvivesRestart(t *testing.T) {
	fx := validContributorFixture()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	fx.checkCompletedAt = clock.at.Add(-90 * time.Minute).Format(time.RFC3339)
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	tracker := NewContributorSoakTracker()
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: tracker})
	ctx := context.Background()

	eval, reason, err := engine.evaluateContributorLanePR(ctx, "widget", "acme", "widget", 7, policy, false)
	if err != nil || reason != contributorLaneReasonSoaking || eval.soakRemaining != 30*time.Minute {
		t.Fatalf("fresh process reason=%q remaining=%v err=%v, want 30m remaining from the check's completed_at", reason, eval.soakRemaining, err)
	}
	if first, ok := tracker.FirstGreen("acme", "widget", 7, "sha7"); !ok || !first.Equal(clock.at.Add(-90*time.Minute)) {
		t.Fatalf("first green = %v ok=%v, want seeded from completed_at", first, ok)
	}
	// The seed is read once; later sweeps use the pinned value.
	fx.checkCompletedAt = clock.at.Add(-10 * time.Minute).Format(time.RFC3339)
	clock.advance(30 * time.Minute)
	event, reason, err := engine.trySweepContributorLanePR(ctx, "widget", "acme", "widget", 7, policy)
	if err != nil || reason != "" || fx.merges != 1 || event.Tier != contributorLaneTier {
		t.Fatalf("post-soak reason=%q err=%v merges=%d", reason, err, fx.merges)
	}

	// A completed_at in the future (clock skew) is ignored: soak from now.
	fx2 := validContributorFixture()
	fx2.checkCompletedAt = clock.at.Add(time.Hour).Format(time.RFC3339)
	api2 := newContributorLaneAPI(t, &fx2)
	defer api2.Close()
	engine2 := newContributorLaneEngine(api2.URL, policy, clock, Options{ContributorSoak: NewContributorSoakTracker()})
	if eval, reason, err := engine2.evaluateContributorLanePR(ctx, "widget", "acme", "widget", 7, policy, false); err != nil || reason != contributorLaneReasonSoaking || eval.soakRemaining != 2*time.Hour {
		t.Fatalf("future completed_at reason=%q remaining=%v err=%v", reason, eval.soakRemaining, err)
	}
}

func TestContributorSoakTrackerNewHeadRestartsAndPrunes(t *testing.T) {
	tracker := NewContributorSoakTracker()
	t0 := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	key1 := contributorSoakKey("acme", "widget", 7, "sha7")
	key2 := contributorSoakKey("acme", "widget", 7, "sha8")
	if got := tracker.observeGreen(key1, t0); !got.Equal(t0) {
		t.Fatalf("first observe = %v", got)
	}
	if got := tracker.observeGreen(key2, t0.Add(time.Hour)); !got.Equal(t0.Add(time.Hour)) {
		t.Fatalf("new head should restart soak, got %v", got)
	}
	if _, ok := tracker.FirstGreen("acme", "widget", 7, "sha7"); !ok {
		t.Fatalf("old head should still be tracked before prune")
	}
	tracker.observeGreen(key2, t0.Add(contributorSoakPruneAfter+2*time.Hour))
	if _, ok := tracker.FirstGreen("acme", "widget", 7, "sha7"); ok {
		t.Fatalf("stale head should be pruned")
	}
	if _, ok := tracker.FirstGreen("acme", "widget", 7, "sha8"); !ok {
		t.Fatalf("live head must survive prune")
	}
	var nilTracker *ContributorSoakTracker
	if got := nilTracker.observeGreen(key1, t0); !got.Equal(t0) {
		t.Fatalf("nil tracker observe = %v", got)
	}
	if _, ok := nilTracker.FirstGreen("acme", "widget", 7, "sha7"); ok {
		t.Fatalf("nil tracker should report nothing")
	}
}

func TestSweepContributorLaneAutoMergesPolicyGates(t *testing.T) {
	for _, tc := range []struct {
		name        string
		policy      func(p *ContributorLanePolicy)
		opts        Options
		mutate      func(fx *contributorLaneFixture)
		wantErr     bool
		wantSeen    int
		wantSkipped int
		wantMerged  int
	}{
		{name: "tier disabled", policy: func(p *ContributorLanePolicy) { p.Enabled = false }},
		{name: "repo outside allow-list", policy: func(p *ContributorLanePolicy) { p.Repos = map[string]bool{"other": true} }},
		{name: "repo inside allow-list merges", policy: func(p *ContributorLanePolicy) { p.Repos = map[string]bool{"widget": true} }, wantSeen: 1, wantMerged: 1},
		{name: "repo auto-merge disabled", opts: Options{RepoAutoMergeEnabled: func(string) bool { return false }}},
		{name: "list error is returned", mutate: func(fx *contributorLaneFixture) { fx.listStatus = http.StatusInternalServerError }, wantErr: true},
		{name: "prefilter skip counts", mutate: func(fx *contributorLaneFixture) { fx.draft = true }, wantSeen: 1, wantSkipped: 1},
		{name: "per-PR skip counts", mutate: func(fx *contributorLaneFixture) { fx.issueComments = nil }, wantSeen: 1, wantSkipped: 1},
		{name: "per-PR error is skipped not fatal", mutate: func(fx *contributorLaneFixture) { fx.issueCommentsSt = http.StatusInternalServerError }, wantSeen: 1, wantSkipped: 1},
		{name: "eligible merges and audits", wantSeen: 1, wantMerged: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := validContributorFixture()
			if tc.mutate != nil {
				tc.mutate(&fx)
			}
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			if tc.policy != nil {
				tc.policy(&policy)
			}
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			opts := tc.opts
			opts.ContributorSoak = soakedTracker(clock)
			engine := newContributorLaneEngine(api.URL, policy, clock, opts)
			audited := 0
			result, err := engine.SweepContributorLaneAutoMerges(context.Background(), AutoMergeSweepOptions{Audit: func(AutoMergeSweepEvent) { audited++ }})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if result.Seen != tc.wantSeen || result.Skipped != tc.wantSkipped || len(result.Merged) != tc.wantMerged {
				t.Fatalf("seen=%d skipped=%d merged=%d, want %d/%d/%d", result.Seen, result.Skipped, len(result.Merged), tc.wantSeen, tc.wantSkipped, tc.wantMerged)
			}
			if audited != tc.wantMerged || fx.merges != tc.wantMerged {
				t.Fatalf("audited=%d merges=%d, want %d", audited, fx.merges, tc.wantMerged)
			}
		})
	}
}

func TestSweepContributorLaneAutoMergesPackageFuncAndNoClient(t *testing.T) {
	var nilEngine *Engine
	if _, err := nilEngine.SweepContributorLaneAutoMerges(context.Background(), AutoMergeSweepOptions{}); err == nil {
		t.Fatalf("nil engine should fail")
	}
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	policy := defaultContributorLanePolicy()
	opts := Options{
		ContributorLanePolicy:  func() ContributorLanePolicy { return policy },
		ContributorLaneVerdict: approvedVerdict,
		ContributorSoak:        soakedTracker(clock),
		Now:                    clock.now,
	}
	result, err := SweepContributorLaneAutoMerges(context.Background(), client, opts, AutoMergeSweepOptions{})
	if err != nil || len(result.Merged) != 1 {
		t.Fatalf("package func: err=%v result=%+v", err, result)
	}
}

func TestContributorLaneClaimRequiresAppBotLogin(t *testing.T) {
	fx := validContributorFixture()
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	policy := defaultContributorLanePolicy()
	engine := New(client, Options{ContributorLanePolicy: func() ContributorLanePolicy { return policy }})
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonNoAppBotLogin {
		t.Fatalf("reason=%q err=%v, want %q", reason, err, contributorLaneReasonNoAppBotLogin)
	}
}

func TestContributorLaneLinkedIssues(t *testing.T) {
	for _, tc := range []struct {
		body string
		want []int
	}{
		{body: "Fixes #42", want: []int{42}},
		{body: "Closes #9 and fixes #3", want: []int{3, 9}},
		{body: "Resolves acme/widget#5", want: []int{5}},
		{body: "Fixes other/repo#5", want: nil},
		{body: "Refs #42", want: nil},
		{body: "Fixes #42\nFixes #42", want: []int{42}},
		{body: "", want: nil},
	} {
		got := contributorLaneLinkedIssues(tc.body, "acme/widget")
		if len(got) != len(tc.want) {
			t.Fatalf("%q: got %v want %v", tc.body, got, tc.want)
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Fatalf("%q: got %v want %v", tc.body, got, tc.want)
			}
		}
	}
}

func TestContributorLanePolicyHelpers(t *testing.T) {
	p := ContributorLanePolicy{Enabled: true, Repos: map[string]bool{"widget": true}}
	if !p.repoAllowed("widget") || !p.repoAllowed(" Widget ") || p.repoAllowed("other") {
		t.Fatalf("repoAllowed mismatch")
	}
	if !(ContributorLanePolicy{Enabled: true}).repoAllowed("anything") {
		t.Fatalf("empty allow-list should allow all")
	}
	if (ContributorLanePolicy{}).repoAllowed("anything") {
		t.Fatalf("disabled policy should allow nothing")
	}
	p.ExcludeLabels = map[string]bool{"blocked": true}
	if got := p.excludedLabel([]string{"ok", " Blocked "}); got != "blocked" {
		t.Fatalf("excludedLabel = %q", got)
	}
	if got := p.excludedLabel([]string{"ok"}); got != "" {
		t.Fatalf("excludedLabel = %q, want empty", got)
	}
	var nilEngine *Engine
	if nilEngine.currentContributorLanePolicy().Enabled {
		t.Fatalf("nil engine policy should be disabled")
	}
	if nilEngine.contributorLaneVerdictFor("acme/widget", 1, "sha").Configured {
		t.Fatalf("nil engine verdict should be unconfigured")
	}
}

func TestPrefilterContributorLanePR(t *testing.T) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, "")
	client.SetAppBotLogin(testHiveAppBotLogin)
	client.SetExemptLabels([]string{"lfx"})
	policy := defaultContributorLanePolicy()
	engine := New(client, Options{ContributorLanePolicy: func() ContributorLanePolicy { return policy }})
	pr := func(mutate func(p *gh.PullRequest)) *gh.PullRequest {
		p := &gh.PullRequest{
			Number: gh.Ptr(7),
			State:  gh.Ptr("open"),
			User:   &gh.User{Login: gh.Ptr(contributorLaneTestAuthor)},
			Head:   &gh.PullRequestBranch{SHA: gh.Ptr("sha7")},

			AuthorAssociation: gh.Ptr("CONTRIBUTOR"),
		}
		if mutate != nil {
			mutate(p)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		pr   *gh.PullRequest
		want string
	}{
		{name: "nil", pr: nil, want: "missing-head-sha"},
		{name: "eligible", pr: pr(nil), want: ""},
		{name: "closed", pr: pr(func(p *gh.PullRequest) { p.State = gh.Ptr("closed") }), want: "closed"},
		{name: "no author", pr: pr(func(p *gh.PullRequest) { p.User = nil }), want: contributorLaneReasonNoAuthor},
		{name: "exempt label", pr: pr(func(p *gh.PullRequest) { p.Labels = []*gh.Label{{Name: gh.Ptr("lfx")}} }), want: "exempt-label"},
		{name: "needs-rebase", pr: pr(func(p *gh.PullRequest) { p.Labels = []*gh.Label{{Name: gh.Ptr("needs-rebase")}} }), want: "label:needs-rebase"},
		{name: "missing head", pr: pr(func(p *gh.PullRequest) { p.Head = nil }), want: "missing-head-sha"},
		// Association is decided after admission (marker/registry), not here.
		{name: "association none passes prefilter", pr: pr(func(p *gh.PullRequest) { p.AuthorAssociation = gh.Ptr("NONE") }), want: ""},
		{name: "association first-time passes prefilter", pr: pr(func(p *gh.PullRequest) { p.AuthorAssociation = gh.Ptr("FIRST_TIME_CONTRIBUTOR") }), want: ""},
		{name: "association missing passes prefilter", pr: pr(func(p *gh.PullRequest) { p.AuthorAssociation = nil }), want: ""},
		{name: "association lower-case collaborator", pr: pr(func(p *gh.PullRequest) { p.AuthorAssociation = gh.Ptr("collaborator") }), want: ""},
	} {
		if got := engine.prefilterContributorLanePR(tc.pr, policy); got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestContributorLaneAssociationTrusted(t *testing.T) {
	for _, tc := range []struct {
		assoc *string
		want  bool
	}{
		{gh.Ptr("OWNER"), true}, {gh.Ptr("MEMBER"), true}, {gh.Ptr("COLLABORATOR"), true},
		{gh.Ptr(" contributor "), true}, {gh.Ptr("CONTRIBUTOR"), true},
		{gh.Ptr("NONE"), false}, {gh.Ptr("FIRST_TIMER"), false}, {gh.Ptr("FIRST_TIME_CONTRIBUTOR"), false},
		{gh.Ptr("MANNEQUIN"), false}, {gh.Ptr(""), false}, {nil, false},
	} {
		pr := &gh.PullRequest{AuthorAssociation: tc.assoc}
		if got := contributorLaneAssociationTrusted(pr); got != tc.want {
			t.Fatalf("association %v: got %v want %v", tc.assoc, got, tc.want)
		}
	}
	if contributorLaneAssociationTrusted(nil) {
		t.Fatal("nil PR must not be trusted")
	}
}

// contributorPromotionSearchEngine builds an engine whose hive-wide
// auto-promotion walk needs other repositories: widget holds one clean merge
// by each author, hive holds several.
func contributorPromotionSearchEngine(t *testing.T, fx *contributorLaneFixture) (*Engine, *contributorLaneClock) {
	t.Helper()
	fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
	api := newContributorLaneAPI(t, fx)
	t.Cleanup(api.Close)
	policy := defaultContributorLanePolicy()
	policy.AutoPromoteAfterMergedPR = 5
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	return newContributorLaneEngineRepos(api.URL, []string{"widget", "hive"}, policy, clock, Options{}), clock
}

func TestContributorPromotionSearchCachePerSweep(t *testing.T) {
	fx := validContributorFixture()
	engine, clock := contributorPromotionSearchEngine(t, &fx)
	history := func(author string) contributorMergedHistory {
		t.Helper()
		h, err := engine.contributorCleanMergedHistory(context.Background(), "acme", "widget", author, 5, true)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, author := range []string{"alice", "alice", "alice", "bob"} {
		history(author)
	}
	if fx.searches != 2 {
		t.Fatalf("searches = %d, want 2", fx.searches)
	}
	// A later sweep inside the TTL reuses alice's answer (#15088).
	clock.advance(contributorMergedReposTTL - time.Minute)
	history("alice")
	if fx.searches != 2 {
		t.Fatalf("searches inside the TTL = %d, want 2", fx.searches)
	}
	clock.advance(time.Minute)
	history("alice")
	if fx.searches != 3 {
		t.Fatalf("searches after the TTL = %d, want 3", fx.searches)
	}
}

// TestContributorPromotionSearchCacheWalksListedReposFresh pins the
// staleness argument: only the list of repositories is cached, so a revert
// that lands in a listed repository after the search still blocks promotion
// on the next sweep, without a new search.
func TestContributorPromotionSearchCacheWalksListedReposFresh(t *testing.T) {
	fx := validContributorFixture()
	fx.mergedHistory = nil
	clean := func(n int) []map[string]any {
		var out []map[string]any
		for i := 0; i < n; i++ {
			out = append(out, mergedHistoryPR(contributorLaneTestAuthor, "fix: clean change"))
		}
		return out
	}
	fx.otherHistory = map[string][]map[string]any{"hive": clean(5)}
	engine, clock := contributorPromotionSearchEngine(t, &fx)
	h, err := engine.contributorCleanMergedHistory(context.Background(), "acme", "widget", contributorLaneTestAuthor, 5, true)
	if err != nil || !h.Clean {
		t.Fatalf("first sweep = %+v, %v; want clean", h, err)
	}
	fx.otherHistory["hive"] = append([]map[string]any{mergedHistoryPR(contributorLaneTestAuthor, "Revert \"fix: clean change\"")}, clean(5)...)
	clock.advance(time.Minute)
	h, err = engine.contributorCleanMergedHistory(context.Background(), "acme", "widget", contributorLaneTestAuthor, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	if h.Clean || h.Reason != "revert-or-sentinel" {
		t.Fatalf("second sweep = %+v; want blocked by the new revert", h)
	}
	if fx.searches != 1 {
		t.Fatalf("searches = %d, want 1", fx.searches)
	}
}

// TestContributorPromotionSearchCacheIsPerAuthor checks that one search
// serves the author's PRs in every repository: each PR's own repository is
// left out of its answer.
func TestContributorPromotionSearchCacheIsPerAuthor(t *testing.T) {
	fx := validContributorFixture()
	fx.otherHistory = map[string][]map[string]any{
		"widget": {mergedHistoryPR(contributorLaneTestAuthor, "fix: a")},
		"hive":   {mergedHistoryPR(contributorLaneTestAuthor, "fix: b")},
	}
	engine, _ := contributorPromotionSearchEngine(t, &fx)
	got := func(repo string) string {
		var names []string
		for _, ref := range engine.contributorMergedRepos(context.Background(), "acme", repo, contributorLaneTestAuthor) {
			names = append(names, ref.Repo)
		}
		return strings.Join(names, ",")
	}
	if g := got("widget"); g != "hive" {
		t.Fatalf("from widget = %q, want hive", g)
	}
	if g := got("hive"); g != "widget" {
		t.Fatalf("from hive = %q, want widget", g)
	}
	if fx.searches != 1 {
		t.Fatalf("searches = %d, want 1", fx.searches)
	}
	// A changed active-repository set is a miss: the cached answer does not
	// cover the new repository.
	engine.transport.(*hgithub.Client).SetRepos([]string{"acme/widget", "acme/hive", "acme/docs"})
	got("widget")
	if fx.searches != 2 {
		t.Fatalf("searches after the repo set changed = %d, want 2", fx.searches)
	}
}

func TestContributorPromotionSearchFailureCached(t *testing.T) {
	fx := validContributorFixture()
	fx.searchStatus = http.StatusForbidden
	fx.otherHistory = map[string][]map[string]any{"hive": {mergedHistoryPR(contributorLaneTestAuthor, "fix: b")}}
	engine, clock := contributorPromotionSearchEngine(t, &fx)
	lookup := func() int {
		return len(engine.contributorMergedRepos(context.Background(), "acme", "widget", contributorLaneTestAuthor))
	}
	if n := lookup(); n != 0 || fx.searches != 1 {
		t.Fatalf("failing search: %d repos, %d searches; want 0, 1", n, fx.searches)
	}
	fx.searchStatus = 0
	clock.advance(contributorMergedReposFailureTTL - time.Minute)
	if n := lookup(); n != 0 || fx.searches != 1 {
		t.Fatalf("inside the failure TTL: %d repos, %d searches; want 0, 1", n, fx.searches)
	}
	clock.advance(time.Minute)
	if n := lookup(); n != 1 || fx.searches != 2 {
		t.Fatalf("after the failure TTL: %d repos, %d searches; want 1, 2", n, fx.searches)
	}
	// The success is now kept for the full TTL, not the failure one.
	clock.advance(contributorMergedReposFailureTTL)
	if n := lookup(); n != 1 || fx.searches != 2 {
		t.Fatalf("success past the failure TTL: %d repos, %d searches; want 1, 2", n, fx.searches)
	}
}

func TestContributorPromotionSkipsBots(t *testing.T) {
	for _, author := range []string{"github-actions[bot]", "mergeraptor[bot]", "other[bot]"} {
		t.Run(author, func(t *testing.T) {
			fx := validContributorFixture()
			fx.author = author
			fx.issueComments[0]["body"] = contributorClaimMarkerTier(author, "contributor", contributorLaneTestHive, " tier=contributor verified=true")
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			policy.AutoPromoteAfterMergedPR = 5
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngineRepos(api.URL, []string{"widget", "hive"}, policy, clock, Options{})
			_, _, _ = engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if fx.searches != 0 {
				t.Fatalf("bot triggered %d searches", fx.searches)
			}
		})
	}
}

func TestContributorTrustedChangeRequestAnswersRequiresHuman(t *testing.T) {
	fx := validContributorFixture()
	fx.reviews = []map[string]any{humanReview(1, "clubanderson", "CHANGES_REQUESTED", "sha7", "please fix the metadata")}
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{
		ContributorLaneVerdict: func(string, int, string) ContributorLaneVerdict {
			return ContributorLaneVerdict{Configured: true, Verdict: "requires_human", DecisionRequested: true}
		},
	})
	engine.SetMergerAuthorizer(func(login string) bool { return login == "clubanderson" })
	engine.stageTracker = NewStageTracker()
	engine.stageLabelPolicy = func() StageLabelPolicy { return StageLabelPolicy{Enabled: true} }
	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonChangesRequested || fx.merges != 0 {
		t.Fatalf("reason=%s err=%v merges=%d", reason, err, fx.merges)
	}
	detail := engine.stageTracker.details[stageKey("acme", "widget", 7)]
	// Production observations include the author kind; the lane alone does
	// not distinguish an automated contributor (072) from a human (071).
	o := stageObservation{AuthorKind: authorkind.Clanker, Lane: stageLaneContributor, HeadSHA: "sha7", Reason: reason, Detail: detail}
	if v := classifyStage(o); v.Stage != stagelabels.ReturnedToContributor {
		t.Fatalf("stage=%+v, want returned to contributor", v)
	}
}

// TestTrySweepContributorLaneSentinelYieldsToHumanChangesRequested: the
// sentinel alert parks a contributor PR for a human; a trusted human's
// CHANGES_REQUESTED review on the current head is that human. The lane
// reports human-changes-requested (→ 071/072) instead of sentinel-blocked
// (→ 085), reads the standing request from the durable store too, and
// re-arms the park on a new head (hivecommons/hive#15159).
func TestTrySweepContributorLaneSentinelYieldsToHumanChangesRequested(t *testing.T) {
	review := func(state, commit, login string) []map[string]any {
		return []map[string]any{{"id": 1, "state": state, "commit_id": commit, "user": map[string]string{"login": login}, "body": "drop the workflow change"}}
	}
	for _, tt := range []struct {
		name       string
		reviews    []map[string]any
		trusted    bool
		store      *hreview.HumanChangeRequests
		wantReason string
	}{
		{name: "no review stays parked", trusted: true, wantReason: contributorLaneReasonSentinelBlocked},
		{name: "trusted changes requested on head", reviews: review("CHANGES_REQUESTED", "sha7", "clubanderson"), trusted: true, wantReason: autoMergeReasonHumanChangesRequested},
		{name: "untrusted reviewer stays parked", reviews: review("CHANGES_REQUESTED", "sha7", "clubanderson"), wantReason: contributorLaneReasonSentinelBlocked},
		{name: "review on an older head re-arms", reviews: review("CHANGES_REQUESTED", "sha6", "clubanderson"), trusted: true, wantReason: contributorLaneReasonSentinelBlocked},
		{name: "approval is not a change request", reviews: review("APPROVED", "sha7", "clubanderson"), trusted: true, wantReason: contributorLaneReasonSentinelBlocked},
		{name: "standing request in the store", trusted: true, store: storedChangeRequest(t, "sha7"), wantReason: autoMergeReasonHumanChangesRequested},
		{name: "stored request on an older head re-arms", trusted: true, store: storedChangeRequest(t, "sha6"), wantReason: contributorLaneReasonSentinelBlocked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fx := validContributorFixture()
			fx.labels = []string{"hive-sentinel"}
			fx.reviews = tt.reviews
			api := newContributorLaneAPI(t, &fx)
			defer api.Close()
			policy := defaultContributorLanePolicy()
			clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
			engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock), HumanChangeRequests: tt.store})
			if tt.trusted {
				engine.SetMergerAuthorizer(func(login string) bool { return strings.EqualFold(login, "clubanderson") })
			}
			_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
			if err != nil {
				t.Fatal(err)
			}
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tt.wantReason)
			}
			if fx.merges != 0 {
				t.Fatalf("merged %d, want 0", fx.merges)
			}
		})
	}
}

func storedChangeRequest(t *testing.T, head string) *hreview.HumanChangeRequests {
	t.Helper()
	store := hreview.NewHumanChangeRequests("")
	if err := store.Record(hreview.HumanChangeRequest{Repo: "acme/widget", Number: 7, HeadSHA: head, Source: hreview.HumanChangeRequestSourceDecision, Actor: "clubanderson", Body: "drop it"}); err != nil {
		t.Fatal(err)
	}
	return store
}

// Owner invariant (hivecommons/hive#15640), verbatim: "i always have the
// last word when it comes to merge of untrusted". A green, approved,
// soaked PR from a contributor the lane cannot vouch for (self-registered
// newcomer, untrusted tier) is never auto-merged: the lane refuses with
// claim-tier-untrusted and the stage engine parks it at 090
// ready-for-human-merge — the merge gate, not an 085 decision in review.
func TestContributorLaneUntrustedGreenApprovedParksForHumanMerge(t *testing.T) {
	fx := validContributorFixture()
	fx.issueComments[0]["body"] = contributorClaimMarkerTier(contributorLaneTestAuthor, "contributor", contributorLaneTestHive, " tier=newcomer verified=false")
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	policy := defaultContributorLanePolicy()
	clock := &contributorLaneClock{at: time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)}
	engine := newContributorLaneEngine(api.URL, policy, clock, Options{ContributorSoak: soakedTracker(clock)})

	_, reason, err := engine.trySweepContributorLanePR(context.Background(), "widget", "acme", "widget", 7, policy)
	if err != nil || reason != contributorLaneReasonClaimTier {
		t.Fatalf("reason = %q err = %v, want %q", reason, err, contributorLaneReasonClaimTier)
	}
	if fx.merges != 0 {
		t.Fatalf("untrusted author auto-merged (%d merges)", fx.merges)
	}
	v := stageengine.ClassifyStage(prstage.Observation{Lane: stageLaneContributor, Reason: reason, HeadSHA: "sha7", Detail: prstage.Detail{VerdictKnown: true, Verdict: approvedVerdict("acme/widget", 7, "sha7")}})
	if v.Stage != stagelabels.ReadyForHumanMerge || !v.Human {
		t.Fatalf("stage = %+v, want 090 ready-for-human-merge awaiting the operator", v)
	}
}
