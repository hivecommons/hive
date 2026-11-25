package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// auto_merge.human_merge_owner_approval (Danathar/goodreads-mcp#463): on a
// listed repository a trusted merger's explicit approval of the current head
// releases the hive's human-merge-path label; everywhere else, and for any
// approval that is not bound to the head, the label stays.

var ownerApprovalHead = strings.Repeat("d", 40)

func TestHumanMergeApproveCommandActor(t *testing.T) {
	head := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *gh.Timestamp { return &gh.Timestamp{Time: head.Add(d)} }
	comment := func(login, typ, body string, created, updated *gh.Timestamp) *gh.IssueComment {
		return &gh.IssueComment{User: &gh.User{Login: gh.Ptr(login), Type: gh.Ptr(typ)}, Body: gh.Ptr(body), CreatedAt: created, UpdatedAt: updated}
	}
	for _, tc := range []struct {
		name    string
		comment *gh.IssueComment
		headAt  time.Time
		want    string
	}{
		{name: "after the head", comment: comment("owner", "User", "/hive approve", at(time.Hour), at(time.Hour)), headAt: head, want: "owner"},
		{name: "trailing text on later lines", comment: comment("owner", "User", "/Hive  Approve\n\nship it", at(time.Hour), at(time.Hour)), headAt: head, want: "owner"},
		{name: "before the head", comment: comment("owner", "User", "/hive approve", at(-time.Hour), at(-time.Hour)), headAt: head},
		{name: "head time unknown", comment: comment("owner", "User", "/hive approve", at(time.Hour), at(time.Hour))},
		{name: "edited", comment: comment("owner", "User", "/hive approve", at(time.Hour), at(2*time.Hour)), headAt: head},
		{name: "the App", comment: comment(testHiveAppBotLogin, "Bot", "/hive approve", at(time.Hour), at(time.Hour)), headAt: head},
		{name: "another bot", comment: comment("renovate[bot]", "Bot", "/hive approve", at(time.Hour), at(time.Hour)), headAt: head},
		{name: "a decision is the fence's", comment: comment("owner", "User", "/hive decision approve", at(time.Hour), at(time.Hour)), headAt: head},
		{name: "not on the first line", comment: comment("owner", "User", "LGTM\n/hive approve", at(time.Hour), at(time.Hour)), headAt: head},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := humanMergeApproveCommandActor(tc.comment, tc.headAt, testHiveAppBotLogin)
			if got != tc.want || ok != (tc.want != "") {
				t.Fatalf("actor = %q/%v, want %q", got, ok, tc.want)
			}
		})
	}
}

func TestHumanMergeSelfApproval(t *testing.T) {
	for _, tc := range []struct {
		approver, author string
		want             bool
	}{
		{"owner", "owner", true},
		{"Owner", " owner ", true},
		{"owner", "contributor", false},
		{"owner", testHiveAppBotLogin, false},
		{"", "", false},
		{"owner", "", false},
	} {
		if got := humanMergeSelfApproval(tc.approver, tc.author); got != tc.want {
			t.Errorf("humanMergeSelfApproval(%q, %q) = %v, want %v", tc.approver, tc.author, got, tc.want)
		}
	}
}

func TestHumanMergeApprovalCommentNamesApproverAndHead(t *testing.T) {
	body := humanMergeApprovalComment(ownerApprovalHead, workflowApproval{Login: "owner"})
	for _, want := range []string{
		humanMergeApprovalMarker(ownerApprovalHead),
		"`needs-owner-merge` released for `ddddddd`:",
		"`owner` approved this head",
		humanMergeOwnerApprovalConfigKey,
		"not a merge override",
		"A new push puts the label back",
		"A `hold` a person adds is never lifted",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("audit comment lacks %q:\n%s", want, body)
		}
	}
}

// ownerApprovalStageAPI is the human-merge-path lift fixture with a real
// 40-character head, the App's head-stamped notice and hold event as the
// gate writes them, and reviews served for the PR.
func ownerApprovalStageAPI(t *testing.T, reviews, events []map[string]any) *stageAPI {
	t.Helper()
	return ownerApprovalStageAPIFor(t, "shared-pat-account", "", reviews, events, nil)
}

// ownerApprovalStageAPIFor is ownerApprovalStageAPI with the PR's author,
// its updated_at (which pins when the hive first observed the head, the time
// a typed approval must follow; "" serves none, so nothing typed binds) and
// the people's comments served after the App's notice.
func ownerApprovalStageAPIFor(t *testing.T, author, updatedAt string, reviews, events, typed []map[string]any) *stageAPI {
	t.Helper()
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	notice := map[string]any{
		"id":         11,
		"body":       hgithub.HumanMergePathComment([]string{".github/workflows/release.yml"}) + "\n<!-- hive-human-merge-path-head: " + ownerApprovalHead + " -->",
		"user":       app,
		"created_at": "2026-11-10T16:00:03Z",
		"updated_at": "2026-11-10T16:00:03Z",
	}
	if events == nil {
		events = []map[string]any{{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": humanMergePathLiftHoldAt}}
	}
	files := append([]string{".github/workflows/release.yml"}, humanMergePathLiftCleanFiles...)
	api := newHumanMergePathLiftAPI(t, 21, files, append([]map[string]any{notice}, typed...), events)
	api.extra = func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/21":
			body := map[string]any{
				"number": 21, "head": map[string]string{"sha": ownerApprovalHead}, "base": map[string]string{"ref": "main"},
				"user": map[string]string{"login": author, "type": "User"}, "changed_files": len(files),
				"labels": []map[string]string{{"name": hgithub.HumanMergePathLabel}},
			}
			if updatedAt != "" {
				body["updated_at"] = updatedAt
			}
			_ = json.NewEncoder(w).Encode(body)
			return true
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/21/reviews":
			if reviews == nil {
				reviews = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(reviews)
			return true
		}
		return false
	}
	return api
}

func ownerApprovalReview(login, commit string) map[string]any {
	return map[string]any{"id": 1, "state": "APPROVED", "commit_id": commit, "user": map[string]string{"login": login, "type": "User"}}
}

func ownerApprovalNotices(api *stageAPI) []string {
	api.mu.Lock()
	defer api.mu.Unlock()
	var out []string
	for _, body := range api.comments {
		if strings.HasPrefix(body, humanMergeApprovalMarkerPrefix) {
			out = append(out, body)
		}
	}
	return out
}

func TestApplyStageLabels_OwnerApprovalReleasesHumanMergePathHold(t *testing.T) {
	app := map[string]string{"login": testHiveAppBotLogin, "type": "Bot"}
	appHold := map[string]any{"event": "labeled", "label": map[string]string{"name": hgithub.HumanMergePathLabel}, "actor": app, "created_at": humanMergePathLiftHoldAt}
	for _, tc := range []struct {
		name     string
		unlisted bool
		reviews  []map[string]any
		events   []map[string]any
		released bool
	}{
		{name: "trusted merger approved the head", reviews: []map[string]any{ownerApprovalReview("maintainer", ownerApprovalHead)}, released: true},
		{name: "repository not listed", unlisted: true, reviews: []map[string]any{ownerApprovalReview("maintainer", ownerApprovalHead)}},
		{name: "no approval", reviews: nil},
		{name: "approval by someone who is not a trusted merger", reviews: []map[string]any{ownerApprovalReview("passerby", ownerApprovalHead)}},
		{name: "approval of an older head", reviews: []map[string]any{ownerApprovalReview("maintainer", strings.Repeat("e", 40))}},
		{name: "a person re-added the label", reviews: []map[string]any{ownerApprovalReview("maintainer", ownerApprovalHead)}, events: []map[string]any{
			appHold,
			pathLabelEvent("unlabeled", "maintainer", "2026-11-10T17:00:00Z"),
			pathLabelEvent("labeled", "maintainer", "2026-11-10T17:00:05Z"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := ownerApprovalStageAPI(t, tc.reviews, tc.events)
			e, store := newHumanMergePathLiftEngine(t, api)
			e.SetMergerAuthorizer(func(login string) bool { return login == "maintainer" })
			e.humanMergeOwnerApproval = func(repo string) bool { return !tc.unlisted && strings.EqualFold(repo, "acme/widget") }

			pr := stagePR(21, hgithub.HumanMergePathLabel, "author/human", "role/contributor", "stage/050-review-pending")
			pr.Head.SHA = gh.Ptr(ownerApprovalHead)
			pr.User = &gh.User{Login: gh.Ptr("shared-pat-account")}
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, autoMergeReasonHumanMergeRequired, false, nil)
			e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			removed := removedPathLabel(api)
			notices := ownerApprovalNotices(api)
			if h := removedHold(api); h != 0 {
				t.Fatalf("hold removed %d times: the release never touches hold", h)
			}
			if !tc.released {
				if removed != 0 || len(notices) != 0 {
					t.Fatalf("label removed %d times, audit comments %q; want the label kept", removed, notices)
				}
				return
			}
			if removed != 1 {
				t.Fatalf("label removed %d times (removed=%v), want once", removed, api.removed)
			}
			if len(notices) != 1 || !strings.HasPrefix(notices[0], humanMergeApprovalMarker(ownerApprovalHead)) || !strings.Contains(notices[0], "`maintainer` approved this head") {
				t.Fatalf("audit comments = %q, want one naming the approver and head", notices)
			}
			if rec, ok := store.Get(stageKey("acme", "widget", 21)); ok && rec.Hold != nil {
				t.Fatalf("stored hold provenance survived the release: %+v", rec.Hold)
			}
		})
	}
}

// A typed approval by a trusted merger who opened the PR never releases its
// human-merge-path label, and since `/hive decision approve <why>` counts as
// an approval, the author's own reasoned approval (typed to answer the
// verdict on their PR) must not hide another merger's earlier approval of
// the same head either: the newest approval found is the author's, so the
// gate looks again without the author's comments and finds the other
// merger's.
func TestApplyStageLabels_OwnerApprovalTypedBehindTheAuthorsOwnApproval(t *testing.T) {
	const (
		author    = "owner-a"
		merger    = "maintainer-b"
		updatedAt = "2026-11-10T16:30:00Z"
	)
	typed := func(id int, login, body, at string) map[string]any {
		return map[string]any{"id": id, "user": map[string]string{"login": login, "type": "User"}, "body": body, "created_at": at, "updated_at": at}
	}
	others := typed(31, merger, "/hive decision approve", "2026-11-10T17:00:00Z")
	own := typed(32, author, "/hive decision approve, thanks "+merger, "2026-11-10T17:10:00Z")
	for _, tc := range []struct {
		name     string
		typed    []map[string]any
		released bool
	}{
		{name: "another merger's approval behind the author's reasoned one", typed: []map[string]any{others, own}, released: true},
		{name: "the author's reasoned approval alone", typed: []map[string]any{own}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := ownerApprovalStageAPIFor(t, author, updatedAt, nil, nil, tc.typed)
			e, _ := newHumanMergePathLiftEngine(t, api)
			e.headFirstSeen = NewHeadFirstSeenTracker()
			e.SetMergerAuthorizer(func(login string) bool { return login == author || login == merger })
			e.humanMergeOwnerApproval = func(repo string) bool { return strings.EqualFold(repo, "acme/widget") }

			pr := stagePR(21, hgithub.HumanMergePathLabel, "author/human", "role/contributor", "stage/050-review-pending")
			pr.Head.SHA = gh.Ptr(ownerApprovalHead)
			pr.User = &gh.User{Login: gh.Ptr(author)}
			e.recordStage(stageLaneContributor, "acme/widget", "acme", "widget", pr, autoMergeReasonHumanMergeRequired, false, nil)
			e.noteStageRole("acme", "widget", 21, stagelabels.RoleContributor)
			if _, err := e.ApplyStageLabels(context.Background()); err != nil {
				t.Fatal(err)
			}
			removed := removedPathLabel(api)
			notices := ownerApprovalNotices(api)
			if !tc.released {
				if removed != 0 || len(notices) != 0 {
					t.Fatalf("label removed %d times, audit comments %q; want the author's own approval to keep the label", removed, notices)
				}
				return
			}
			if removed != 1 {
				t.Fatalf("label removed %d times (removed=%v), want once", removed, api.removed)
			}
			if len(notices) != 1 || !strings.Contains(notices[0], "`"+merger+"` approved this head") || strings.Contains(notices[0], "`"+author+"` approved") {
				t.Fatalf("audit comments = %q, want one naming %s, not the author", notices, merger)
			}
		})
	}
}

func TestCommentsNotBy(t *testing.T) {
	by := func(id int64, login string) *gh.IssueComment {
		return &gh.IssueComment{ID: gh.Ptr(id), User: &gh.User{Login: gh.Ptr(login)}}
	}
	comments := []*gh.IssueComment{by(1, "Owner-A"), by(2, "maintainer-b"), {ID: gh.Ptr(int64(3))}, by(4, "owner-a"), by(5, "app[bot]")}
	got := commentsNotBy(comments, "owner-a")
	var ids []int64
	for _, c := range got {
		ids = append(ids, c.GetID())
	}
	if want := []int64{2, 3, 5}; len(ids) != len(want) || ids[0] != want[0] || ids[1] != want[1] || ids[2] != want[2] {
		t.Fatalf("commentsNotBy kept %v, want %v (case-insensitive, order kept, a comment without a user kept)", ids, want)
	}
	if len(comments) != 5 || comments[0].GetID() != 1 {
		t.Fatalf("commentsNotBy changed its input: %v", comments)
	}
}

// The `/hive approve` reply on a listed repository says the hold is released
// for the head (or which approval it still needs); on any other repository
// it is today's reply word for word.
func TestPRApproveReplyHumanMergeOwnerApproval(t *testing.T) {
	const unlistedReply = "`/hive approve` acknowledged, but this PR changes files that its lane requires a person to merge manually. The command cannot override that rule; a maintainer must review and merge it."
	for _, tc := range []struct {
		name     string
		unlisted bool
		author   string
		reviews  []*gh.PullRequestReview
		comments []*gh.IssueComment
		want     string
		exact    bool
	}{
		{name: "unlisted repository keeps today's reply", unlisted: true, reviews: []*gh.PullRequestReview{{ID: gh.Ptr(int64(1)), State: gh.Ptr("APPROVED"), CommitID: gh.Ptr("sha7"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}}}, want: unlistedReply, exact: true},
		{name: "Approve review on the head", reviews: []*gh.PullRequestReview{{ID: gh.Ptr(int64(1)), State: gh.Ptr("APPROVED"), CommitID: gh.Ptr("sha7"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}}}, want: "released for `sha7`. "},
		{name: "this /hive approve after the head", comments: []*gh.IssueComment{{ID: gh.Ptr(int64(5)), Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}, CreatedAt: &gh.Timestamp{Time: time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)}}}, want: "released for `sha7`. "},
		{name: "/hive approve before the head", comments: []*gh.IssueComment{{ID: gh.Ptr(int64(5)), Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}, CreatedAt: &gh.Timestamp{Time: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}}}, want: "no approval of the current head (`sha7`) is recorded yet"},
		{name: "the PR author's own /hive approve does not count", author: "owner", comments: []*gh.IssueComment{{ID: gh.Ptr(int64(5)), Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}, CreatedAt: &gh.Timestamp{Time: time.Date(2026, 11, 18, 11, 0, 0, 0, time.UTC)}}}, want: "cannot enable an automerge lane"},
		{name: "approval by someone who is not a trusted merger", reviews: []*gh.PullRequestReview{{ID: gh.Ptr(int64(1)), State: gh.Ptr("APPROVED"), CommitID: gh.Ptr("sha7"), User: &gh.User{Login: gh.Ptr("passerby"), Type: gh.Ptr("User")}}}, want: "no approval of the current head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("feedback must not mutate: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusForbidden)
					return
				}
				switch r.URL.Path {
				case "/repos/acme/widget/pulls/7":
					author := tc.author
					if author == "" {
						author = testHiveAppBotLogin
					}
					_ = json.NewEncoder(w).Encode(&gh.PullRequest{Number: gh.Ptr(7), State: gh.Ptr("open"), User: &gh.User{Login: gh.Ptr(author)}, Head: &gh.PullRequestBranch{SHA: gh.Ptr("sha7")}, UpdatedAt: &gh.Timestamp{Time: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}})
				case "/repos/acme/widget/pulls/7/files":
					_ = json.NewEncoder(w).Encode([]*gh.CommitFile{{Filename: gh.Ptr("sensitive/config.yaml"), Status: gh.Ptr("modified")}})
				case "/repos/acme/widget/pulls/7/reviews":
					reviews := tc.reviews
					if reviews == nil {
						reviews = []*gh.PullRequestReview{}
					}
					_ = json.NewEncoder(w).Encode(reviews)
				case "/repos/acme/widget/issues/7/comments":
					comments := tc.comments
					if comments == nil {
						comments = []*gh.IssueComment{}
					}
					_ = json.NewEncoder(w).Encode(comments)
				default:
					t.Errorf("unexpected read: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer api.Close()
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, api.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			e := New(client, Options{
				Now:                     func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
				HeadFirstSeen:           NewHeadFirstSeenTracker(),
				MergerAuthorizer:        func(login string) bool { return login == "owner" },
				HumanMergePaths:         func(string) []string { return []string{"sensitive/**"} },
				HumanMergeOwnerApproval: func(repo string) bool { return !tc.unlisted && repo == "acme/widget" },
				RepoAutoMergeEnabled:    func(string) bool { return true },
			})
			reply, err := e.PRApproveReply(context.Background(), "acme/widget", 7, &gh.IssueComment{Body: gh.Ptr("/hive approve"), User: &gh.User{Login: gh.Ptr("owner"), Type: gh.Ptr("User")}})
			if err != nil {
				t.Fatal(err)
			}
			if tc.exact && !strings.HasPrefix(reply, tc.want) {
				t.Fatalf("reply = %q, want today's reply %q", reply, tc.want)
			}
			if !strings.Contains(reply, tc.want) {
				t.Fatalf("reply = %q, want it to contain %q", reply, tc.want)
			}
			if !tc.unlisted && strings.Contains(reply, "cannot override") {
				t.Fatalf("listed repository still says the command cannot override: %q", reply)
			}
		})
	}
}
