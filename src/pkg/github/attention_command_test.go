package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/attention"
	"github.com/hivecommons/hive/pkg/writeback"
)

type nopAttentionDelivery struct{}

func (nopAttentionDelivery) History(context.Context, string, int) ([]string, error) { return nil, nil }
func (nopAttentionDelivery) Find(context.Context, attention.Episode) (bool, error)  { return false, nil }
func (nopAttentionDelivery) Post(context.Context, attention.Episode, string) error  { return nil }

// `/hive ack` and `/hive snooze` record reminder state on the episode for the
// recipient or a maintainer, are ignored for anyone else, and never touch a
// hold label, an issue/PR state or a merge endpoint (hivecommons/hive#16359).
func TestSweepAttentionAckSnoozeRecordOnlyAndNeverReleaseHolds(t *testing.T) {
	pr := parkedIssue(7,
		humanComment(20, "bob", "/hive ack"),
		humanComment(21, "alice", "/hive snooze 2d"),
		humanComment(22, "maintainer", "/hive ack"),
	)
	pr.PullRequest = &unparkWirePRLink{URL: "https://example.test/pulls/7"}
	pr.Labels = []wireLabel{{Name: "hold"}, {Name: PRNeedsDecisionStageLabel}, {Name: "needs-human"}}
	inner, rec := newUnparkServer(t, "org", "repo", []unparkWireIssue{pr}, map[string]string{"maintainer": "write"})
	var mu sync.Mutex
	var writes []string
	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mu.Lock()
			writes = append(writes, r.Method+" "+r.URL.Path)
			mu.Unlock()
		}
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	t.Cleanup(outer.Close)
	c := newTestClient(t, outer, "org", []string{"repo"})
	c.SetWritebackGate(writeback.New(writeback.Config{}, writeback.Deps{}, ""))
	cfg := attention.Config{Admitter: map[string]string{"org/repo": "hive-a"}}
	c.SetAttention("hive-a", func() attention.Config { return cfg })
	store, err := c.WritebackGate().AttentionStore()
	if err != nil {
		t.Fatal(err)
	}
	key := attention.Key{Repo: "org/repo", Item: 7, Action: "security-repair", Recipient: "@alice", Subject: "finding-identity/v1:risk"}
	if _, err := store.Report(context.Background(), cfg, "hive-a", "org/repo", 7, []attention.Observation{{Key: key, Actionable: true, Body: "x"}}, nopAttentionDelivery{}); err != nil {
		t.Fatal(err)
	}

	if _, err := c.SweepIssueUnparkCommands(context.Background(), IssueUnparkSweepOptions{}); err != nil {
		t.Fatal(err)
	}

	es := store.Snapshot("org/repo", 7)
	if len(es) != 1 || es[0].State != attention.Acknowledged || es[0].AckedBy != "maintainer" || es[0].SnoozedBy != "alice" || es[0].SnoozedUntil.IsZero() {
		t.Fatalf("episode = %+v", es)
	}
	if len(rec.added[7]) != 0 || len(rec.removed[7]) != 0 || len(rec.patched[7]) != 0 {
		t.Fatalf("labels/state changed: added=%v removed=%v patched=%v", rec.added[7], rec.removed[7], rec.patched[7])
	}
	mu.Lock()
	defer mu.Unlock()
	for _, w := range writes {
		if !strings.HasSuffix(w, "/issues/7/comments") && !strings.Contains(w, "/issues/comments/") {
			t.Fatalf("ack/snooze made a non-comment write: %s (all: %v)", w, writes)
		}
	}
	replies := strings.Join(rec.posted[7], "\n---\n")
	if strings.Contains(replies, unparkReplyMarkerPrefix+"20 ") {
		t.Fatalf("non-recipient non-maintainer was answered:\n%s", replies)
	}
	for _, want := range []string{unparkReplyMarkerPrefix + "21 -->", "@alice snoozed", unparkReplyMarkerPrefix + "22 -->", "@maintainer acknowledged", "Acknowledgement is not approval"} {
		if !strings.Contains(replies, want) {
			t.Fatalf("replies missing %q:\n%s", want, replies)
		}
	}
}
