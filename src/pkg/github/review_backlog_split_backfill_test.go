package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/outputschema"
)

// backfillTestServer serves host #42 closed long before any sweep window
// with the given bot-authored folded findings, host #43 still open, and #44
// deleted. The windowed closed-issue listing (the only listing that carries
// `since`) is never consulted by the backfill; the dedup and tombstone
// lookups CreateIssue makes before filing the split are answered empty.
func backfillTestServer(t *testing.T, comments []string, created, hostComments, requests *int32) *httptest.Server {
	t.Helper()
	old := time.Now().Add(-30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(requests, 1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues":
			if r.URL.Query().Has("since") {
				t.Errorf("backfill used the windowed closed-issue listing: %s", r.URL.String())
			}
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42":
			_, _ = io.WriteString(w, `{"number":42,"title":"Review backlog (correctness): something","state":"closed","state_reason":"completed","comments":1,"closed_at":"`+old+`","updated_at":"`+old+`","html_url":"https://github.example/o/r/issues/42","user":{"login":"`+testBot+`"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/43":
			_, _ = io.WriteString(w, `{"number":43,"title":"open host","state":"open","comments":1,"user":{"login":"`+testBot+`"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/42/comments":
			out := make([]map[string]any, 0, len(comments))
			for i, body := range comments {
				out = append(out, map[string]any{"id": i + 1, "body": body, "user": map[string]any{"login": testBot, "type": "Bot"}})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			_, _ = io.WriteString(w, `{"name":"x"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/42/comments":
			atomic.AddInt32(hostComments, 1)
			_, _ = io.WriteString(w, `{"id":3}`)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues":
			atomic.AddInt32(created, 1)
			_, _ = io.WriteString(w, `{"number":99,"html_url":"https://github.example/o/r/issues/99"}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = fmt.Fprintf(w, `{"message":"Not Found","path":%q}`, r.URL.Path)
		}
	}))
}

func saveBackfillTestLedger(t *testing.T, key, fp string) {
	t.Helper()
	ledger := newReviewBacklogLedger()
	ledger.Items[key] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, Fingerprint: fp, FindingKey: key, Destination: config.ReviewBacklogGitHubIssue}
	ledger.Items["open-host"] = reviewBacklogRecord{Repo: "o/r", PRNumber: 6, IssueNumber: 43, FindingKey: "open-host", Destination: config.ReviewBacklogGitHubIssue}
	ledger.Items["deleted-host"] = reviewBacklogRecord{Repo: "o/r", PRNumber: 6, IssueNumber: 44, FindingKey: "deleted-host", Destination: config.ReviewBacklogGitHubIssue}
	if err := ledger.save("", time.Now()); err != nil {
		t.Fatal(err)
	}
}

// A host closed weeks before the sweep window (hivecommons/hive#13947) is
// split by the backfill, which reads hosts from the ledger rather than the
// windowed closed-issue listing; the open and deleted hosts are left alone,
// the ledger records the backfill as done, and a second call makes no API
// requests at all.
func TestBackfillReviewBacklogSplitsSplitsHostsClosedBeforeWindow(t *testing.T) {
	withReviewBacklogState(t)
	f := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
	comment, key, fp := splitTestFolded(t, f)
	saveBackfillTestLedger(t, key, fp)

	var created, hostComments, requests int32
	srv := backfillTestServer(t, []string{comment}, &created, &hostComments, &requests)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetRepos([]string{"o/r"})

	var audited []ReviewBacklogSplitEvent
	res, err := c.BackfillReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{
		Audit: func(ev ReviewBacklogSplitEvent) { audited = append(audited, ev) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hosts != 1 || res.Skipped != 0 || res.Deferred != 0 || len(res.Split) != 1 {
		t.Fatalf("result = %+v, want one host and one split", res)
	}
	if ev := res.Split[0]; ev.Repo != "o/r" || ev.Host != 42 || ev.Number != 99 || ev.Key != key {
		t.Fatalf("split event = %+v", ev)
	}
	if len(audited) != 1 || created != 1 || hostComments != 1 {
		t.Fatalf("audited=%d created=%d hostComments=%d, want one each", len(audited), created, hostComments)
	}
	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.SplitBackfilled || !saved.splitFrom(key, 42) || saved.Items[key].IssueNumber != 99 {
		t.Fatalf("ledger after backfill: backfilled=%v split_keys=%v item=%+v", saved.SplitBackfilled, saved.SplitKeys, saved.Items[key])
	}

	before := atomic.LoadInt32(&requests)
	again, err := c.BackfillReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{})
	if err != nil || again.Hosts != 0 || len(again.Split) != 0 || atomic.LoadInt32(&requests) != before {
		t.Fatalf("second backfill err=%v result=%+v requests=%d->%d, want a no-op", err, again, before, requests)
	}
}

// A pass cut short by MaxActions defers the remaining hosts and leaves the
// backfill flag unset, so the next pass finishes the walk; hosts already
// split are not filed again.
func TestBackfillReviewBacklogSplitsRetriesUntilComplete(t *testing.T) {
	withReviewBacklogState(t)
	f := outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"}
	comment, key, fp := splitTestFolded(t, f)
	saveBackfillTestLedger(t, key, fp)

	var created, hostComments, requests int32
	srv := backfillTestServer(t, []string{comment}, &created, &hostComments, &requests)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetRepos([]string{"o/r"})

	res, err := c.BackfillReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{MaxActions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Split) != 1 || res.Deferred != 2 {
		t.Fatalf("result = %+v, want one split and the two later hosts deferred", res)
	}
	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SplitBackfilled {
		t.Fatal("backfill marked done with hosts deferred")
	}

	res, err = c.BackfillReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{MaxActions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Split) != 0 || res.Deferred != 0 || created != 1 || hostComments != 1 {
		t.Fatalf("second pass result=%+v created=%d hostComments=%d, want no new split", res, created, hostComments)
	}
	saved, err = loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.SplitBackfilled {
		t.Fatal("backfill not marked done after a complete pass")
	}
}

// The host that exhausts the per-pass budget may be the last one walked:
// with two folded findings on #42, the only ledger host, and MaxActions 1,
// the first pass splits one and must not mark the backfill done; the
// second pass splits the other and then sets the flag.
func TestBackfillReviewBacklogSplitsLastHostPartlySplit(t *testing.T) {
	withReviewBacklogState(t)
	first, firstKey, firstFP := splitTestFolded(t, outputschema.Finding{Title: "unused variable", File: "pkg/a.go", Line: 7, Summary: "x is never read"})
	second, secondKey, secondFP := splitTestFolded(t, outputschema.Finding{Title: "shadowed err", File: "pkg/b.go", Line: 3, Summary: "err is shadowed"})
	ledger := newReviewBacklogLedger()
	ledger.Items[firstKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, Fingerprint: firstFP, FindingKey: firstKey, Destination: config.ReviewBacklogGitHubIssue}
	ledger.Items[secondKey] = reviewBacklogRecord{Repo: "o/r", PRNumber: 5, IssueNumber: 42, Fingerprint: secondFP, FindingKey: secondKey, Destination: config.ReviewBacklogGitHubIssue}
	if err := ledger.save("", time.Now()); err != nil {
		t.Fatal(err)
	}

	var created, hostComments, requests int32
	srv := backfillTestServer(t, []string{first, second}, &created, &hostComments, &requests)
	defer srv.Close()
	c := reviewTestClient(t, srv.URL)
	c.appBotLogin = testBot
	c.SetRepos([]string{"o/r"})

	res, err := c.BackfillReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{MaxActions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Split) != 1 || res.Split[0].Key != firstKey || res.Deferred != 0 {
		t.Fatalf("first pass result = %+v, want only the first finding split", res)
	}
	saved, err := loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if saved.SplitBackfilled {
		t.Fatal("backfill marked done with a finding on the last host still unsplit")
	}
	if saved.splitFrom(secondKey, 42) {
		t.Fatal("second finding recorded as split despite the cap")
	}

	res, err = c.BackfillReviewBacklogSplits(context.Background(), ReviewBacklogSplitSweepOptions{MaxActions: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Split) != 1 || res.Split[0].Key != secondKey || created != 2 {
		t.Fatalf("second pass result=%+v created=%d, want the second finding split", res, created)
	}
	saved, err = loadReviewBacklog("")
	if err != nil {
		t.Fatal(err)
	}
	if !saved.SplitBackfilled || !saved.splitFrom(secondKey, 42) {
		t.Fatalf("after second pass: backfilled=%v split_keys=%v, want the flag set", saved.SplitBackfilled, saved.SplitKeys)
	}
}
