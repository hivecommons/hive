package forgestate

import (
	"context"
	"errors"
	"testing"
	"time"
)

func hook(id string, k Kind, n int, action string) WebhookEvent {
	return WebhookEvent{DeliveryID: id, Event: "issues", Action: action, Kind: k, Number: n}
}

// Acceptance: an issue closed on GitHub leaves the cache on its webhook.
func TestWebhookClosedIssueRemoved(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0)}, nil, "")
	mustReconcile(t, h, "o/r")

	h.fetcher.setItem(closedIssue(1, t0.Add(time.Minute)))
	if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("d-close", KindIssue, 1, "closed")); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.Issue("o/r", 1); ok {
		t.Fatal("closed issue still cached")
	}
	snap, _ := h.store.Snapshot("o/r")
	if snap.IssueCount() != 1 || snap.LastDeliveryID != "d-close" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if got := h.store.Deliveries("o/r"); len(got) != 1 || got[0] != "d-close" {
		t.Fatalf("Deliveries = %v", got)
	}
	if _, n := h.fetcher.calls(); n != 1 {
		t.Fatalf("webhook patch made %d item calls, want 1", n)
	}
	if got := h.metrics.Value(MetricWebhookPatch, "repo", "o/r", "result", resultRemoved); got != 1 {
		t.Errorf("removed metric = %d", got)
	}
}

// Acceptance: out-of-order opened/closed deliveries end in the right state.
func TestWebhookOutOfOrderDeliveries(t *testing.T) {
	t.Run("closed delivered before opened, reads lag", func(t *testing.T) {
		h := newHarness(t)
		h.fetcher.setList(nil, nil, "")
		mustReconcile(t, h, "o/r")
		// GitHub truth: opened at t0+1m, closed at t0+2m. The "opened"
		// delivery arrives last and its confirming read hits a lagging
		// replica that still says open@t0+1m.
		h.fetcher.enqueue(closedIssue(5, t0.Add(2*time.Minute)), issue(5, t0.Add(time.Minute)))
		if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("d2", KindIssue, 5, "closed")); err != nil {
			t.Fatal(err)
		}
		if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("d1", KindIssue, 5, "opened")); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.store.Issue("o/r", 5); ok {
			t.Fatal("an older open read resurrected a closed issue")
		}
		if got := h.metrics.Value(MetricWebhookPatch, "repo", "o/r", "result", resultIgnored); got != 1 {
			t.Errorf("ignored metric = %d", got)
		}
		// A genuine reopen is newer and re-adds it.
		h.fetcher.enqueue(issue(5, t0.Add(3*time.Minute)))
		if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("d3", KindIssue, 5, "reopened")); err != nil {
			t.Fatal(err)
		}
		if _, ok := h.store.Issue("o/r", 5); !ok {
			t.Fatal("reopened issue not re-added")
		}
	})

	t.Run("closed delivered before opened, reads current", func(t *testing.T) {
		h := newHarness(t)
		h.fetcher.setList(nil, nil, "")
		mustReconcile(t, h, "o/r")
		h.fetcher.setItem(closedIssue(6, t0.Add(2*time.Minute)))
		for _, ev := range []WebhookEvent{hook("d2", KindIssue, 6, "closed"), hook("d1", KindIssue, 6, "opened")} {
			if err := h.store.ApplyWebhook(context.Background(), "o/r", ev); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := h.store.Issue("o/r", 6); ok {
			t.Fatal("issue closed on GitHub is cached")
		}
	})

	t.Run("opened then closed in order", func(t *testing.T) {
		h := newHarness(t)
		h.fetcher.setList(nil, nil, "")
		mustReconcile(t, h, "o/r")
		h.fetcher.enqueue(issue(7, t0.Add(time.Minute)), closedIssue(7, t0.Add(2*time.Minute)))
		_ = h.store.ApplyWebhook(context.Background(), "o/r", hook("d1", KindIssue, 7, "opened"))
		if _, ok := h.store.Issue("o/r", 7); !ok {
			t.Fatal("opened issue not added")
		}
		_ = h.store.ApplyWebhook(context.Background(), "o/r", hook("d2", KindIssue, 7, "closed"))
		if _, ok := h.store.Issue("o/r", 7); ok {
			t.Fatal("closed issue still cached")
		}
	})

	t.Run("pr synchronize out of order", func(t *testing.T) {
		h := newHarness(t)
		h.fetcher.setList(nil, []Item{pr(9, t0, "a")}, "")
		mustReconcile(t, h, "o/r")
		h.fetcher.enqueue(pr(9, t0.Add(2*time.Minute), "c"), pr(9, t0.Add(time.Minute), "b"))
		_ = h.store.ApplyWebhook(context.Background(), "o/r", WebhookEvent{DeliveryID: "s2", Kind: KindPR, Number: 9, Action: "synchronize"})
		_ = h.store.ApplyWebhook(context.Background(), "o/r", WebhookEvent{DeliveryID: "s1", Kind: KindPR, Number: 9, Action: "synchronize"})
		if p, _ := h.store.PR("o/r", 9); p.HeadSHA != "c" || p.Source != SourceWebhook {
			t.Fatalf("PR 9 = %+v, want the newest head", p)
		}
	})
}

func TestWebhookDuplicateDeliveryNotRefetched(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	ev := hook("dup", KindIssue, 1, "labeled")
	for i := 0; i < 3; i++ {
		if err := h.store.ApplyWebhook(context.Background(), "o/r", ev); err != nil {
			t.Fatal(err)
		}
	}
	if _, n := h.fetcher.calls(); n != 1 {
		t.Fatalf("redelivered event fetched %d times, want 1", n)
	}
}

func TestWebhookDeliveryRingIsBounded(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	for i := 0; i < maxDeliveries+5; i++ {
		id := "d" + time.Duration(i).String()
		if err := h.store.ApplyWebhook(context.Background(), "o/r", hook(id, KindIssue, 1, "edited")); err != nil {
			t.Fatal(err)
		}
	}
	got := h.store.Deliveries("o/r")
	if len(got) != maxDeliveries || got[0] != "d"+time.Duration(5).String() {
		t.Fatalf("Deliveries len=%d first=%q", len(got), got[0])
	}
}

func TestWebhookWithoutItemIsRecordedOnly(t *testing.T) {
	h := newHarness(t)
	if err := h.store.ApplyWebhook(context.Background(), "o/r", WebhookEvent{DeliveryID: "push-1", Event: "push"}); err != nil {
		t.Fatal(err)
	}
	if _, n := h.fetcher.calls(); n != 0 {
		t.Fatal("an event naming no item must not fetch")
	}
	if snap, _ := h.store.Snapshot("o/r"); snap.LastDeliveryID != "push-1" {
		t.Fatalf("LastDeliveryID = %q", snap.LastDeliveryID)
	}
	if err := h.store.ApplyWebhook(context.Background(), "x/y", hook("d", KindIssue, 1, "opened")); !errors.Is(err, ErrUnknownRepo) {
		t.Fatalf("unknown repo err = %v", err)
	}
}

func TestWebhookNotFoundRemoves(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	h.fetcher.mu.Lock()
	delete(h.fetcher.items, itemKey{KindIssue, 1})
	h.fetcher.mu.Unlock()
	if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("t", KindIssue, 1, "transferred")); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.Issue("o/r", 1); ok {
		t.Fatal("transferred issue still cached")
	}
}

func TestWebhookFetchFailure(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0.Add(time.Hour))}, nil, "")
	mustReconcile(t, h, "o/r")
	h.fetcher.failItems(errors.New("502 bad gateway"))

	// Non-close: nothing trusted without a read.
	if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("a", KindIssue, 1, "labeled")); err == nil {
		t.Fatal("want the fetch error")
	}
	if _, ok := h.store.Issue("o/r", 1); !ok {
		t.Fatal("a failed non-close patch must not drop the item")
	}
	if got := h.metrics.Value(MetricWebhookPatch, "repo", "o/r", "result", resultError); got != 1 {
		t.Errorf("error metric = %d", got)
	}

	// Close: applied from the hint.
	ev := hook("b", KindIssue, 1, "closed")
	ev.UpdatedAt = t0.Add(time.Minute)
	if err := h.store.ApplyWebhook(context.Background(), "o/r", ev); err == nil {
		t.Fatal("want the fetch error reported")
	}
	if _, ok := h.store.Issue("o/r", 1); ok {
		t.Fatal("a close hint must drop the item even when the confirming read fails")
	}

	// An older close hint than the cached item (reopened since) is ignored.
	ev = hook("c", KindIssue, 2, "closed")
	ev.UpdatedAt = t0
	_ = h.store.ApplyWebhook(context.Background(), "o/r", ev)
	if _, ok := h.store.Issue("o/r", 2); !ok {
		t.Fatal("a stale close hint dropped a newer item")
	}

	// A failed delivery is retried on redelivery.
	h.fetcher.failItems(nil)
	h.fetcher.setItem(Item{Kind: KindIssue, Number: 1, State: "open", UpdatedAt: t0, Labels: []string{"x"}})
	_ = h.store.ApplyWebhook(context.Background(), "o/r", hook("a", KindIssue, 1, "labeled"))
	if _, n := h.fetcher.calls(); n != 4 {
		t.Fatalf("item calls = %d, want the redelivery to fetch again", n)
	}
}

func TestWebhookGateRefusal(t *testing.T) {
	h := newHarness(t)
	h.store.opts.Gate = denyGate{}
	if err := h.store.ApplyWebhook(context.Background(), "o/r", hook("g", KindIssue, 1, "edited")); !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("err = %v, want ErrBudgetExhausted", err)
	}
	if _, n := h.fetcher.calls(); n != 0 {
		t.Fatal("a refused patch must not reach the fetcher")
	}
}

func TestParseWebhook(t *testing.T) {
	cases := []struct {
		name, event, body string
		wantOK            bool
		wantKind          Kind
		wantNumber        int
		wantSHA           string
	}{
		{"issue closed", "issues", `{"action":"closed","repository":{"full_name":"o/r"},"issue":{"number":3,"updated_at":"2026-10-21T10:00:00Z"}}`, true, KindIssue, 3, ""},
		{"comment on pr", "issue_comment", `{"action":"created","repository":{"full_name":"o/r"},"issue":{"number":4,"pull_request":{"url":"u"}}}`, true, KindPR, 4, ""},
		{"comment on issue", "issue_comment", `{"action":"created","repository":{"full_name":"o/r"},"issue":{"number":4,"pull_request":null}}`, true, KindIssue, 4, ""},
		{"pr sync", "pull_request", `{"action":"synchronize","number":8,"repository":{"full_name":"o/r"},"pull_request":{"number":8,"head":{"sha":"abc"}}}`, true, KindPR, 8, "abc"},
		{"review", "pull_request_review", `{"action":"submitted","repository":{"full_name":"o/r"},"pull_request":{"number":9}}`, true, KindPR, 9, ""},
		{"no item", "issues", `{"action":"opened","repository":{"full_name":"o/r"}}`, false, "", 0, ""},
		{"other event", "push", `{}`, false, "", 0, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, ev, ok, err := ParseWebhook(c.event, "del-1", []byte(c.body))
			if err != nil {
				t.Fatal(err)
			}
			if ok != c.wantOK {
				t.Fatalf("ok = %v, want %v", ok, c.wantOK)
			}
			if !ok {
				return
			}
			if repo != "o/r" || ev.Kind != c.wantKind || ev.Number != c.wantNumber || ev.HeadSHA != c.wantSHA || ev.DeliveryID != "del-1" || ev.Event != c.event {
				t.Fatalf("ParseWebhook = %q %+v", repo, ev)
			}
		})
	}
	if _, _, _, err := ParseWebhook("issues", "d", []byte("{")); err == nil {
		t.Fatal("bad JSON must error")
	}
	_, ev, _, _ := ParseWebhook("issues", "d", []byte(`{"action":"closed","issue":{"number":1,"updated_at":"2026-10-21T10:00:00Z"}}`))
	if !ev.closes() || !ev.UpdatedAt.Equal(t0) {
		t.Fatalf("closed event = %+v", ev)
	}
}

// Observe lets a reader that already fetched an item (the dashboard's
// webhook re-read, a freshness confirmation) hand it to the cache without
// a second fetch: newer wins, an unchanged re-read refreshes the fetch
// time, an older copy is ignored, and a delivery id is recorded once.
func TestObserveUpsertsWithoutFetching(t *testing.T) {
	h := newHarness(t)
	h.fetcher.setList([]Item{issue(1, t0), issue(2, t0)}, nil, "")
	mustReconcile(t, h, "o/r")
	h.clock.Advance(time.Minute)

	changed, err := h.store.Observe("o/r", issue(1, t0.Add(time.Second), "bug"), SourceWebhook, "d-1")
	if err != nil || !changed {
		t.Fatalf("Observe newer = (%v, %v), want (true, nil)", changed, err)
	}
	it, _ := h.store.Issue("o/r", 1)
	if len(it.Labels) != 1 || it.Labels[0] != "bug" || it.Source != SourceWebhook || !it.FetchedAt.Equal(h.clock.Now()) {
		t.Fatalf("item after newer observe = %+v", it)
	}

	h.clock.Advance(time.Minute)
	changed, err = h.store.Observe("o/r", issue(2, t0), SourceReadThrough, "")
	if err != nil || !changed {
		t.Fatalf("Observe unchanged = (%v, %v), want (true, nil)", changed, err)
	}
	it, _ = h.store.Issue("o/r", 2)
	if !it.FetchedAt.Equal(h.clock.Now()) || it.Source != SourceReadThrough {
		t.Fatalf("unchanged re-read did not refresh fetch time: %+v", it)
	}

	changed, err = h.store.Observe("o/r", issue(1, t0.Add(-time.Hour)), SourceReadThrough, "")
	if err != nil || changed {
		t.Fatalf("Observe older = (%v, %v), want (false, nil)", changed, err)
	}
	if it, _ = h.store.Issue("o/r", 1); len(it.Labels) != 1 {
		t.Fatalf("older observe overwrote labels: %+v", it)
	}

	if _, err := h.store.Observe("o/r", closedIssue(2, t0.Add(time.Hour)), SourceWebhook, "d-1"); err != nil {
		t.Fatal(err)
	}
	if _, ok := h.store.Issue("o/r", 2); ok {
		t.Fatal("closed observation left the issue cached")
	}
	if got := h.store.Deliveries("o/r"); len(got) != 1 || got[0] != "d-1" {
		t.Fatalf("Deliveries = %v, want [d-1] recorded once", got)
	}
	if list, item := h.fetcher.calls(); list != 1 || item != 0 {
		t.Fatalf("fetcher calls = (%d list, %d item), want (1, 0)", list, item)
	}
	if got := h.metrics.Value(MetricWebhookPatch, "repo", "o/r", "result", resultOK); got != 1 {
		t.Errorf("webhook patch ok metric = %d, want 1", got)
	}
	if _, err := h.store.Observe("o/r", Item{Kind: KindIssue}, SourceWebhook, ""); err == nil {
		t.Error("Observe without a number succeeded")
	}
	if _, err := h.store.Observe("o/other", issue(1, t0), SourceWebhook, ""); !errors.Is(err, ErrUnknownRepo) {
		t.Errorf("Observe unknown repo = %v, want ErrUnknownRepo", err)
	}
}
