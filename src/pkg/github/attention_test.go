package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/attention"
	"github.com/hivecommons/hive/pkg/writeback"
)

func TestAttentionMarkerLookupPaginatesAndRequiresTrustedAuthor(t *testing.T) {
	e := attention.Episode{Key: attention.Key{Repo: "o/r", Item: 5, Action: "repair", Recipient: "owner", Subject: "condition"}, Attempt: strings.Repeat("a", 32)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		author := "untrusted"
		if r.URL.Query().Get("page") == "2" {
			author = "hive[bot]"
		} else {
			w.Header().Set("Link", "<http://"+r.Host+r.URL.Path+"?page=2>; rel=\"next\"")
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"body": e.Marker(), "user": map[string]string{"login": author}}})
	}))
	defer srv.Close()
	c := testClient(t, srv.URL)
	c.appBotLogin = "hive[bot]"
	d := attentionDelivery{c: c}
	found, err := d.Find(context.Background(), e)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	keys, err := d.History(context.Background(), "o/r", 5)
	if err != nil || len(keys) != 1 || keys[0] != e.Key.Hash() {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
}

func TestAttentionNonAdmitterOnlyFoldsStatus(t *testing.T) {
	server := &statusCommentServer{}
	srv := server.start(t)
	c := testClient(t, srv.URL)
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	c.SetAttention("other-hive", func() attention.Config { return attention.Config{Admitter: map[string]string{"o/r": "owner-hive"}} })
	item := writeback.Item{Number: 5, HiveAuthored: true}
	ctx := context.Background()
	if _, err := c.ReportAttention(ctx, "o/r", item, writeback.KindSentinel, nil); err != nil {
		t.Fatal(err)
	}
	o := attention.Observation{Key: attention.Key{Repo: "o/r", Item: 5, Action: "security-repair", Recipient: "@maintainer", Subject: "finding-identity/v1:new-risk"}, Actionable: true, Evidence: "new SHA", Body: "Please repair this finding"}
	for i := 0; i < 3; i++ {
		if _, err := c.ReportAttention(ctx, "o/r", item, writeback.KindSentinel, []attention.Observation{o}); err != nil {
			t.Fatal(err)
		}
	}
	if server.creates != 1 {
		t.Fatalf("creates=%d, want only status card", server.creates)
	}
	for _, cm := range server.comments {
		body, _ := cm["body"].(string)
		if strings.Contains(body, "<!-- hive:attention ") || strings.Contains(body, "@maintainer") || !strings.Contains(body, "would notify") {
			t.Fatalf("unexpected body: %s", body)
		}
	}
}
func TestAttentionUnsetDoesNotWriteOrCreateLedger(t *testing.T) {
	server := &statusCommentServer{}
	srv := server.start(t)
	c := testClient(t, srv.URL)
	c.SetAttention("owner-hive", func() attention.Config { return attention.Config{} })
	if _, err := c.ReportAttention(context.Background(), "o/r", writeback.Item{Number: 5}, writeback.KindSentinel, nil); err != nil {
		t.Fatal(err)
	}
	if server.creates != 0 || server.edits != 0 {
		t.Fatal("unset admitter changed existing behavior")
	}
}
func TestAttentionRespectsWritebackPause(t *testing.T) {
	server := &statusCommentServer{}
	srv := server.start(t)
	c := testClient(t, srv.URL)
	c.SetAttention("owner", func() attention.Config { return attention.Config{Admitter: map[string]string{"o/r": "owner"}} })
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{RepoPause: func(string) (writeback.RepoPauseInfo, bool) { return writeback.RepoPauseInfo{}, true }}, ""))
	if _, err := c.ReportAttention(context.Background(), "o/r", writeback.Item{Number: 5, HiveAuthored: true}, writeback.KindSentinel, nil); err == nil {
		t.Fatal("pause bypassed")
	}
	if server.creates != 0 {
		t.Fatal("paused repo received a comment")
	}
}
func TestAttentionOwnerCreatesOneEventAcrossReports(t *testing.T) {
	server := &statusCommentServer{}
	srv := server.start(t)
	c := testClient(t, srv.URL)
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	c.SetAttention("owner", func() attention.Config { return attention.Config{Admitter: map[string]string{"o/r": "owner"}} })
	item := writeback.Item{Number: 5, HiveAuthored: true}
	ctx := context.Background()
	if _, err := c.ReportAttention(ctx, "o/r", item, writeback.KindSentinel, nil); err != nil {
		t.Fatal(err)
	}
	o := attention.Observation{Key: attention.Key{Repo: "o/r", Item: 5, Action: "security-repair", Recipient: "maintainer", Subject: "finding-identity/v1:new-risk"}, Actionable: true, Body: "Please repair this finding"}
	for i := 0; i < 3; i++ {
		if _, err := c.ReportAttention(ctx, "o/r", item, writeback.KindSentinel, []attention.Observation{o}); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for _, cm := range server.comments {
		body, _ := cm["body"].(string)
		if strings.Contains(body, "<!-- hive:attention ") {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("attention comments=%d", count)
	}
}
func TestAttentionStatusBoundedAndMentionSafe(t *testing.T) {
	var episodes []attention.Episode
	for i := 0; i < 30; i++ {
		episodes = append(episodes, attention.Episode{Key: attention.Key{Repo: "o/r", Item: i, Action: "repair @everyone", Recipient: "@owner", Subject: "ci"}, State: attention.Actionable, Uncertain: true})
	}
	body := attentionStatus(episodes)
	if strings.Count(body, "<!-- hive:condition ") != 20 || strings.Contains(body, "@") || !strings.Contains(body, "10 further active episodes") {
		t.Fatalf("unbounded/mentioning card: %s", body)
	}
}
