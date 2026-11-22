package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestReleaseHumanMergePathHold(t *testing.T) {
	oldHead := strings.Repeat("a", 40)
	head := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name          string
		actor         string
		extraEvent    bool
		timelineError bool
		qualified     bool
		legacy        bool
		edited        bool
		changedHead   bool
		noGate        bool
		mode          writeback.RepoMode
		paused        bool
		want          bool
	}{
		{name: "App-only stale path hold", actor: "hive[bot]", qualified: true, want: true},
		{name: "missing writeback gate", actor: "hive[bot]", qualified: true, noGate: true},
		{name: "writeback off", actor: "hive[bot]", qualified: true, mode: writeback.RepoModeOff},
		{name: "writeback issues only", actor: "hive[bot]", qualified: true, mode: writeback.RepoModeIssuesOnly},
		{name: "writeback paused", actor: "hive[bot]", qualified: true, paused: true},
		{name: "human hold", actor: "person", qualified: true},
		{name: "human re-added hold", actor: "hive[bot]", extraEvent: true, qualified: true},
		{name: "non-qualifying head", actor: "hive[bot]"},
		{name: "unknown timeline", actor: "hive[bot]", timelineError: true, qualified: true},
		{name: "legacy marker", actor: "hive[bot]", legacy: true, qualified: true},
		{name: "edited marker", actor: "hive[bot]", edited: true, qualified: true},
		{name: "changed head", actor: "hive[bot]", changedHead: true, qualified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletes, calls := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1"):
					currentHead := head
					if tc.changedHead {
						currentHead = oldHead
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": currentHead}, "labels": []map[string]string{{"name": HumanMergePathLabel}}})
				case strings.HasSuffix(r.URL.Path, "/comments"):
					body := HumanMergePathComment([]string{"Containerfile"})
					if !tc.legacy {
						body += HumanMergeHeadMarker(oldHead)
					}
					updated := "2026-11-02T12:00:02Z"
					if tc.edited {
						updated = "2026-11-02T12:00:03Z"
					}
					_ = json.NewEncoder(w).Encode([]map[string]any{{"body": body, "created_at": "2026-11-02T12:00:02Z", "updated_at": updated, "user": map[string]string{"login": "hive[bot]", "type": "Bot"}}})
				case strings.HasSuffix(r.URL.Path, "/events"):
					if tc.timelineError {
						w.WriteHeader(500)
						return
					}
					events := []map[string]any{{"event": "labeled", "created_at": "2026-11-02T12:00:01Z", "label": map[string]string{"name": HumanMergePathLabel}, "actor": map[string]string{"login": tc.actor, "type": "Bot"}}}
					if tc.extraEvent {
						events = append(events, map[string]any{"event": "labeled", "created_at": "2026-11-02T12:00:03Z", "label": map[string]string{"name": HumanMergePathLabel}, "actor": map[string]string{"login": "person", "type": "User"}})
					}
					_ = json.NewEncoder(w).Encode(events)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/"+HumanMergePathLabel):
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			mode := tc.mode
			if mode == writeback.RepoModeDefault {
				mode = writeback.RepoModeFull
			}
			gate := writeback.New(writeback.Config{
				PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: mode}},
			}, writeback.Deps{
				RepoPause: func(string) (writeback.RepoPauseInfo, bool) {
					return writeback.RepoPauseInfo{}, tc.paused
				},
			}, "")
			if tc.noGate {
				gate = nil
			}
			released, err := ReleaseHumanMergePathHold(context.Background(), client, gate, "o", "r", 1, "hive[bot]", head, tc.qualified)
			if released != tc.want || (deletes == 1) != tc.want {
				t.Fatalf("released=%v deletes=%d err=%v", released, deletes, err)
			}
			if tc.want && err != nil {
				t.Fatalf("successful release returned an error: %v", err)
			}
			if tc.timelineError && err == nil {
				t.Fatal("timeline failure must be reported")
			}
			if (!tc.qualified || tc.noGate) && calls != 0 {
				t.Fatalf("non-qualifying request made %d calls", calls)
			}
		})
	}
}

func TestHumanMergeHoldHeadAttribution(t *testing.T) {
	head := strings.Repeat("a", 40)
	for _, existingHold := range []bool{false, true} {
		t.Run(map[bool]string{false: "new label", true: "existing label"}[existingHold], func(t *testing.T) {
			var posted string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case strings.HasSuffix(r.URL.Path, "/pulls/1"):
					labels := []map[string]string{}
					if existingHold {
						labels = append(labels, map[string]string{"name": HumanMergePathLabel})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": head}, "labels": labels})
				case strings.HasSuffix(r.URL.Path, "/labels"):
					if existingHold {
						t.Error("a PR already wearing the label must not be labelled again")
					}
					_, _ = w.Write([]byte(`[{"name":"needs-owner-merge"}]`))
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
					_, _ = w.Write([]byte(`[]`))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
					var comment gh.IssueComment
					_ = json.NewDecoder(r.Body).Decode(&comment)
					posted = comment.GetBody()
					_, _ = w.Write([]byte(`{"id":1}`))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			if err := LabelForHumanMergePaths(context.Background(), client, "o", "r", 1, []string{"Containerfile"}, head); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(posted, HumanMergePathMarker) {
				t.Fatal("legacy marker must remain matchable")
			}
			if strings.Contains(posted, HumanMergeHeadMarker(head)) == existingHold {
				t.Fatalf("wrong attribution: %s", posted)
			}
		})
	}
}

// The owner-approval release (auto_merge.human_merge_owner_approval) may lift
// the hold on the head it was put on, and a hold the gate re-added on a later
// head with no second notice; a person's application anywhere in the history,
// a history that ends removed, or a write-back refusal keeps the hold. The
// pin-only release still refuses the same head.
func TestReleaseApprovedHumanMergePathHold(t *testing.T) {
	head := strings.Repeat("b", 40)
	oldHead := strings.Repeat("a", 40)
	app := map[string]string{"login": "hive[bot]", "type": "Bot"}
	person := map[string]string{"login": "person", "type": "User"}
	event := func(kind, at string, actor map[string]string) map[string]any {
		return map[string]any{"event": kind, "created_at": at, "label": map[string]string{"name": HumanMergePathLabel}, "actor": actor}
	}
	appHold := event("labeled", "2026-11-02T12:00:01Z", app)
	for _, tc := range []struct {
		name       string
		noticeHead string
		events     []map[string]any
		pinOnly    bool
		refuse     bool
		want       bool
	}{
		{name: "hold on the approved head", noticeHead: head, events: []map[string]any{appHold}, want: true},
		{name: "pin-only keeps the same-head hold", noticeHead: head, events: []map[string]any{appHold}, pinOnly: true},
		{name: "App re-held a later head", noticeHead: oldHead, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app), event("labeled", "2026-11-03T10:00:00Z", app)}, want: true},
		{name: "person re-added the hold", noticeHead: head, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", person), event("labeled", "2026-11-03T10:00:00Z", person)}},
		{name: "person's hold first", noticeHead: head, events: []map[string]any{event("labeled", "2026-11-02T12:00:01Z", person)}},
		{name: "history ends removed", noticeHead: head, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app)}},
		{name: "write-back gate refuses", noticeHead: head, events: []map[string]any{appHold}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1"):
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": head}, "labels": []map[string]string{{"name": HumanMergePathLabel}}})
				case strings.HasSuffix(r.URL.Path, "/comments"):
					body := HumanMergePathComment([]string{"Containerfile"}) + HumanMergeHeadMarker(tc.noticeHead)
					_ = json.NewEncoder(w).Encode([]map[string]any{{"body": body, "created_at": "2026-11-02T12:00:02Z", "updated_at": "2026-11-02T12:00:02Z", "user": app}})
				case strings.HasSuffix(r.URL.Path, "/events"):
					_ = json.NewEncoder(w).Encode(tc.events)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/"+HumanMergePathLabel):
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			var released bool
			if tc.pinOnly {
				gate := writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: writeback.RepoModeFull}}}, writeback.Deps{}, "")
				released, err = ReleaseHumanMergePathHold(context.Background(), client, gate, "o", "r", 1, "hive[bot]", head, true)
			} else {
				allowed := func(writeback.Item) bool { return !tc.refuse }
				released, err = ReleaseApprovedHumanMergePathHold(context.Background(), client, allowed, "o", "r", 1, "hive[bot]", head)
			}
			if err != nil {
				t.Fatal(err)
			}
			if released != tc.want || (deletes == 1) != tc.want {
				t.Fatalf("released=%v deletes=%d, want %v", released, deletes, tc.want)
			}
		})
	}

	if released, err := ReleaseApprovedHumanMergePathHold(context.Background(), gh.NewClient(nil), nil, "o", "r", 1, "hive[bot]", head); released || err != nil {
		t.Fatalf("nil write-back decision: released=%v err=%v, want a refusal with no call", released, err)
	}
}

// The pin-only release the stage-label pass uses once a later head qualifies
// for auto_merge.human_merge_pin_only: it lifts the App's needs-owner-merge
// put on an older head, including one the App lifted and put back since,
// through the caller's write-back decision. Lift and re-label cycles count on
// every hive: only the gate writes this label as the App (the hive_id rule
// was for the `hold` the dashboard also writes). The same head, a head
// already lifted once, any transition by a person, a history that ends
// removed and a refusal all keep it.
func TestReleasePinOnlyHumanMergePathHold(t *testing.T) {
	head := strings.Repeat("b", 40)
	oldHead := strings.Repeat("a", 40)
	app := map[string]string{"login": "hive[bot]", "type": "Bot"}
	person := map[string]string{"login": "person", "type": "User"}
	event := func(kind, at string, actor map[string]string) map[string]any {
		return map[string]any{"event": kind, "created_at": at, "label": map[string]string{"name": HumanMergePathLabel}, "actor": actor}
	}
	appHold := event("labeled", "2026-11-02T12:00:01Z", app)
	for _, tc := range []struct {
		name       string
		noticeHead string
		events     []map[string]any
		// liftedHead, when set, has a lift notice from the App.
		liftedHead string
		refuse     bool
		want       bool
	}{
		{name: "hold on an older head", noticeHead: oldHead, events: []map[string]any{appHold}, want: true},
		{name: "hold on this head", noticeHead: head, events: []map[string]any{appHold}},
		{name: "the App lifted the hold and put it back", noticeHead: oldHead, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app), event("labeled", "2026-11-03T10:00:00Z", app)}, want: true},
		{name: "the App lifted the label and put it back twice", noticeHead: oldHead, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app), event("labeled", "2026-11-03T10:00:00Z", app), event("unlabeled", "2026-11-04T09:00:00Z", app), event("labeled", "2026-11-04T10:00:00Z", app)}, want: true},
		{name: "this head was lifted once and held again", noticeHead: oldHead, liftedHead: head, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app), event("labeled", "2026-11-03T10:00:00Z", app)}},
		{name: "another head was lifted and held again", noticeHead: oldHead, liftedHead: strings.Repeat("c", 40), events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app), event("labeled", "2026-11-03T10:00:00Z", app)}, want: true},
		{name: "a person removed the hold and the App put it back", noticeHead: oldHead, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", person), event("labeled", "2026-11-03T10:00:00Z", app)}},
		{name: "the App lifted the hold and a person put it back", noticeHead: oldHead, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app), event("labeled", "2026-11-03T10:00:00Z", person)}},
		{name: "the App lifted the hold and it stayed off", noticeHead: oldHead, events: []map[string]any{appHold, event("unlabeled", "2026-11-03T09:00:00Z", app)}},
		{name: "person's hold", noticeHead: oldHead, events: []map[string]any{event("labeled", "2026-11-02T12:00:01Z", person)}},
		{name: "write-back gate refuses", noticeHead: oldHead, events: []map[string]any{appHold}, refuse: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			noticeBody := HumanMergePathComment([]string{".github/workflows/ci.yml"}) + HumanMergeHeadMarker(tc.noticeHead)
			liftBody := ""
			if tc.liftedHead != "" {
				liftBody = HumanMergePathLiftMarker + tc.liftedHead + " -->\nHold lifted."
			}
			deletes, decisions := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1"):
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": head}, "labels": []map[string]string{{"name": HumanMergePathLabel}}})
				case strings.HasSuffix(r.URL.Path, "/comments"):
					list := []map[string]any{{"body": noticeBody, "created_at": "2026-11-02T12:00:02Z", "updated_at": "2026-11-02T12:00:02Z", "user": app}}
					if liftBody != "" {
						list = append(list, map[string]any{"body": liftBody, "created_at": "2026-11-03T09:00:01Z", "updated_at": "2026-11-03T09:00:01Z", "user": app})
					}
					_ = json.NewEncoder(w).Encode(list)
				case strings.HasSuffix(r.URL.Path, "/events"):
					_ = json.NewEncoder(w).Encode(tc.events)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/"+HumanMergePathLabel):
					deletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			allowed := func(writeback.Item) bool { decisions++; return !tc.refuse }
			released, err := ReleasePinOnlyHumanMergePathHold(context.Background(), client, allowed, "o", "r", 1, "hive[bot]", head)
			if err != nil {
				t.Fatal(err)
			}
			if released != tc.want || (deletes == 1) != tc.want {
				t.Fatalf("released=%v deletes=%d, want %v", released, deletes, tc.want)
			}
			if tc.refuse && decisions != 1 {
				t.Fatalf("write-back decisions = %d, want the refusal consulted once", decisions)
			}
			// The read-only pre-check applies the same provenance rules
			// (a write-back refusal is not provenance).
			at := &gh.Timestamp{Time: time.Date(2026, 11, 2, 12, 0, 2, 0, time.UTC)}
			comments := []*gh.IssueComment{{
				Body:      gh.Ptr(noticeBody),
				CreatedAt: at, UpdatedAt: at,
				User: &gh.User{Login: gh.Ptr("hive[bot]"), Type: gh.Ptr("Bot")},
			}}
			if liftBody != "" {
				comments = append(comments, &gh.IssueComment{Body: gh.Ptr(liftBody), CreatedAt: at, UpdatedAt: at, User: &gh.User{Login: gh.Ptr("hive[bot]"), Type: gh.Ptr("Bot")}})
			}
			proven, err := PinOnlyHumanMergePathHoldReleasable(context.Background(), client, "o", "r", 1, "hive[bot]", head, comments)
			if err != nil {
				t.Fatal(err)
			}
			if want := tc.want || tc.refuse; proven != want {
				t.Fatalf("releasable = %v, want %v", proven, want)
			}
		})
	}

	if released, err := ReleasePinOnlyHumanMergePathHold(context.Background(), gh.NewClient(nil), nil, "o", "r", 1, "hive[bot]", head); released || err != nil {
		t.Fatalf("nil write-back decision: released=%v err=%v, want a refusal with no call", released, err)
	}
}

// The boot migration swaps the rule's old `hold` for needs-owner-merge long
// after the notice; the releases accept that first application only when the
// App's single, unedited migration comment came within a minute after it.
// A `hold` is never removed by them.
func TestHumanMergePathReleaseAcceptsMigratedLabel(t *testing.T) {
	head := strings.Repeat("b", 40)
	oldHead := strings.Repeat("a", 40)
	app := map[string]string{"login": "hive[bot]", "type": "Bot"}
	person := map[string]string{"login": "person", "type": "User"}
	legacyNotice := HumanMergePathMarker + "\nHive added the `hold` label. A maintainer should review and merge this PR by hand.\n" + HumanMergeHeadMarker(oldHead)
	for _, tc := range []struct {
		name      string
		relabel   string // the migration comment's created_at; "" none
		author    map[string]string
		edited    bool
		approved  bool
		holdToo   bool
		want      bool
		wantHolds int
	}{
		{name: "pin-only lifts a migrated label", relabel: "2026-11-20T08:00:05Z", author: app, want: true},
		{name: "owner approval releases a migrated label", relabel: "2026-11-20T08:00:05Z", author: app, approved: true, want: true},
		{name: "no migration comment", author: app},
		{name: "migration comment by someone else", relabel: "2026-11-20T08:00:05Z", author: person},
		{name: "edited migration comment", relabel: "2026-11-20T08:00:05Z", author: app, edited: true},
		{name: "migration comment too late", relabel: "2026-11-20T08:05:00Z", author: app},
		{name: "a person's hold beside the label is not touched", relabel: "2026-11-20T08:00:05Z", author: app, holdToo: true, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deletes, holdDeletes := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/1"):
					labels := []map[string]string{{"name": HumanMergePathLabel}}
					if tc.holdToo {
						labels = append(labels, map[string]string{"name": "hold"})
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": head}, "labels": labels})
				case strings.HasSuffix(r.URL.Path, "/comments"):
					list := []map[string]any{{"body": legacyNotice, "created_at": "2026-11-02T12:00:02Z", "updated_at": "2026-11-02T12:00:02Z", "user": app}}
					if tc.relabel != "" {
						updated := tc.relabel
						if tc.edited {
							updated = "2026-11-21T00:00:00Z"
						}
						list = append(list, map[string]any{"body": HumanMergePathRelabelMarker + "\nswapped", "created_at": tc.relabel, "updated_at": updated, "user": tc.author})
					}
					_ = json.NewEncoder(w).Encode(list)
				case strings.HasSuffix(r.URL.Path, "/events"):
					_ = json.NewEncoder(w).Encode([]map[string]any{
						{"event": "labeled", "created_at": "2026-11-02T12:00:01Z", "label": map[string]string{"name": "hold"}, "actor": app},
						{"event": "labeled", "created_at": "2026-11-20T08:00:03Z", "label": map[string]string{"name": HumanMergePathLabel}, "actor": app},
						{"event": "unlabeled", "created_at": "2026-11-20T08:00:04Z", "label": map[string]string{"name": "hold"}, "actor": app},
					})
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/"+HumanMergePathLabel):
					deletes++
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/labels/hold"):
					holdDeletes++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(404)
				}
			}))
			defer srv.Close()
			client, err := gh.NewClient(srv.Client()).WithEnterpriseURLs(srv.URL, srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			allowed := func(writeback.Item) bool { return true }
			var released bool
			if tc.approved {
				released, err = ReleaseApprovedHumanMergePathHold(context.Background(), client, allowed, "o", "r", 1, "hive[bot]", head)
			} else {
				released, err = ReleasePinOnlyHumanMergePathHold(context.Background(), client, allowed, "o", "r", 1, "hive[bot]", head)
			}
			if err != nil {
				t.Fatal(err)
			}
			if released != tc.want || (deletes == 1) != tc.want {
				t.Fatalf("released=%v deletes=%d, want %v", released, deletes, tc.want)
			}
			if holdDeletes != 0 {
				t.Fatalf("hold deletes = %d: a release never removes hold", holdDeletes)
			}
		})
	}
}
