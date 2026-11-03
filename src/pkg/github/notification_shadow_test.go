package github

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// writeRecorder is a GitHub stub that answers every read with an empty list
// and records every write (method, path and exact body bytes).
type writeRecorder struct {
	mu     sync.Mutex
	writes []string
}

func (w *writeRecorder) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = rw.Write([]byte("[]"))
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.mu.Lock()
		w.writes = append(w.writes, r.Method+" "+r.URL.Path+" "+string(body))
		n := len(w.writes)
		w.mu.Unlock()
		rw.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(rw).Encode(map[string]any{"id": n, "html_url": "https://example.test/c/" + r.URL.Path})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// runShadowScenario drives a fixed mix of notification-capable writes:
// a status note posted for two causes (the second a repeat of the same
// condition), a claim marker twice (protocol command), a review request,
// and a direct comment with @-mentions.
func runShadowScenario(t *testing.T, shadow bool) (*Client, []string) {
	t.Helper()
	rec := &writeRecorder{}
	srv := rec.start(t)
	c := testClient(t, srv.URL)
	c.SetNotificationsShadow(func() bool { return shadow })
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	c.postUnblockedNote(ctx, "o/r", 5, blockedUnblockedMarker+"\nUnblocked: o/r#9 is no longer open.", logger)
	c.postUnblockedNote(ctx, "o/r", 5, blockedUnblockedMarker+"\nUnblocked: o/r#12 is no longer open.", logger)
	for i := 0; i < 2; i++ {
		if err := c.PostClaimNote(ctx, "o/r", 6, "<!-- hive:claim who=scanner -->\nClaimed "+strconv.Itoa(i)); err != nil {
			t.Fatalf("claim note: %v", err)
		}
	}
	if err := c.RequestReviewers(ctx, "o/r", 7, []string{"alice"}, nil); err != nil {
		t.Fatalf("request reviewers: %v", err)
	}
	if err := c.CreateIssueComment(ctx, "o/r", 8, "ping @alice and @bob"); err != nil {
		t.Fatalf("comment: %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return c, append([]string(nil), rec.writes...)
}

// Shadow off and shadow on produce byte-for-byte the same GitHub writes;
// shadow off records nothing.
func TestNotificationShadowDoesNotChangeWrites(t *testing.T) {
	off, offWrites := runShadowScenario(t, false)
	_, onWrites := runShadowScenario(t, true)
	if len(offWrites) == 0 {
		t.Fatal("scenario made no writes")
	}
	if strings.Join(offWrites, "\n") != strings.Join(onWrites, "\n") {
		t.Fatalf("writes differ with shadow on:\noff:\n%s\non:\n%s", strings.Join(offWrites, "\n"), strings.Join(onWrites, "\n"))
	}
	if got := off.NotificationShadowCounts(); len(got) != 0 {
		t.Fatalf("shadow off recorded counters: %+v", got)
	}
	if got := off.NotificationShadowRecords(); len(got) != 0 {
		t.Fatalf("shadow off recorded would-notify records: %+v", got)
	}
	if off.NotificationShadowActiveItems() != 0 {
		t.Fatal("shadow off counted active items")
	}
}

// Shadow on: every write is attributed, classified and counted; a repeat
// of a status condition is would-suppress, a repeated protocol command is
// not; no record carries a comment body.
func TestNotificationShadowRecordsDecisions(t *testing.T) {
	c, _ := runShadowScenario(t, true)
	counts := map[string]int64{}
	for _, cnt := range c.NotificationShadowCounts() {
		counts[cnt.Metric+"|"+cnt.Producer+"|"+string(cnt.Class)] += cnt.Value
	}
	want := map[string]int64{
		NotificationMetricCommentsCreated + "|github.(*Client).postUnblockedNote|status":        2,
		NotificationMetricWouldSuppressRepeats + "|github.(*Client).postUnblockedNote|status":   1,
		NotificationMetricCommentsCreated + "|github.(*Client).PostClaimNote|protocol_command":  2,
		NotificationMetricReviewRequests + "|github.(*Client).RequestReviewers|required_review": 1,
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("counter %s = %d, want %d (all: %v)", k, counts[k], v, counts)
		}
	}
	var mentions int64
	for k, v := range counts {
		if strings.HasPrefix(k, NotificationMetricMentions+"|") {
			mentions += v
		}
		if strings.HasPrefix(k, NotificationMetricWouldSuppressRepeats+"|") && !strings.HasSuffix(k, "|status") && !strings.HasSuffix(k, "|attention") {
			t.Errorf("non-suppressible class marked would-suppress: %s", k)
		}
	}
	if mentions != 2 {
		t.Errorf("mentions = %d, want 2", mentions)
	}
	if got := c.NotificationShadowActiveItems(); got != 4 {
		t.Errorf("active items = %d, want 4", got)
	}

	records := c.NotificationShadowRecords()
	if len(records) != 6 {
		t.Fatalf("records = %d, want 6: %+v", len(records), records)
	}
	raw, _ := json.Marshal(records)
	for _, secret := range []string{"Unblocked:", "hive:claim", "ping"} {
		if bytes.Contains(raw, []byte(secret)) {
			t.Fatalf("record leaked comment body text %q: %s", secret, raw)
		}
	}
	if r := records[1]; r.WouldNotify || r.Reason != NotificationReasonRepeatCondition || r.ConditionKey != records[0].ConditionKey {
		t.Errorf("repeat status note = %+v, want would-suppress on the same condition key as %+v", r, records[0])
	}
	if r := records[3]; !r.WouldNotify || r.Class != NotificationClassProtocolCommand || r.Reason != NotificationReasonExemptRepeat {
		t.Errorf("repeat claim marker = %+v, want delivered as exempt protocol command", r)
	}
	if r := records[4]; r.Kind != string(notificationWriteReviewRequest) || r.Class != NotificationClassRequiredReview || strings.Join(r.Logins, ",") != "alice" {
		t.Errorf("review request = %+v", r)
	}
	if r := records[5]; strings.Join(r.Logins, ",") != "alice,bob" {
		t.Errorf("mention logins = %v, want alice,bob", r.Logins)
	}
}

// A failed delivery is counted as such and never as a created comment.
func TestNotificationShadowDeliveryFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		rw.WriteHeader(http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	c := testClient(t, srv.URL)
	c.SetNotificationsShadow(func() bool { return true })
	if err := c.CreateIssueComment(context.Background(), "o/r", 1, "x"); err == nil {
		t.Fatal("want an error from a 403")
	}
	var failures, created int64
	for _, cnt := range c.NotificationShadowCounts() {
		switch cnt.Metric {
		case NotificationMetricDeliveryFailures:
			failures += cnt.Value
		case NotificationMetricCommentsCreated:
			created += cnt.Value
		}
	}
	if failures != 1 || created != 0 {
		t.Fatalf("failures=%d created=%d, want 1 and 0", failures, created)
	}
}
