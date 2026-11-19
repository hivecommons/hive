package askai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestKapaClientRejectsBadResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/threads/") {
			_, _ = io.WriteString(w, `{"answer":"a","thread_id":"../escape","relevant_sources":[]}`)
			return
		}
		_, _ = io.WriteString(w, `not json`)
	}))
	defer srv.Close()
	c, err := NewKapaClient(srv.URL, testProjectID, testIntegrationID, testAPIKey, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), "", "q"); err == nil || strings.Contains(err.Error(), testProjectID) {
		t.Fatalf("invalid JSON: %v", err)
	}
	got, err := c.Chat(context.Background(), "thread-1", "q")
	if err != nil || got.ThreadID != "" {
		t.Fatalf("a thread id that is not a vendor id must be dropped: %+v %v", got, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Chat(ctx, "", "q"); err == nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	def, err := NewKapaClient("", testProjectID, testIntegrationID, testAPIKey, nil)
	if err != nil || def.baseURL != KapaDefaultBaseURL || def.http.Timeout != DefaultUpstreamTimeout {
		t.Fatalf("defaults: %+v %v", def, err)
	}
}

func TestBreakerHalfOpenAdmitsOneTrial(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	b := newBreaker(0, 0)
	if b.threshold != DefaultBreakerFailures || b.cooldown != DefaultBreakerCooldown {
		t.Fatal("defaults")
	}
	for i := 0; i < DefaultBreakerFailures-1; i++ {
		if b.failure(now) != breakerNoChange {
			t.Fatal("opened early")
		}
	}
	if b.failure(now) != breakerOpened || !b.isOpen() {
		t.Fatal("did not open")
	}
	if ok, wait := b.allow(now); ok || wait != DefaultBreakerCooldown {
		t.Fatalf("open breaker admitted a call (wait %v)", wait)
	}
	later := now.Add(DefaultBreakerCooldown)
	if ok, _ := b.allow(later); !ok {
		t.Fatal("no trial after the cool-down")
	}
	if ok, _ := b.allow(later); ok {
		t.Fatal("a second trial while the first is in flight")
	}
	b.cancelTrial()
	if ok, _ := b.allow(later); !ok {
		t.Fatal("a cancelled trial must free the slot")
	}
	if b.failure(later) != breakerNoChange || !b.isOpen() {
		t.Fatal("a failed trial must keep the breaker open")
	}
	if ok, _ := b.allow(later); ok {
		t.Fatal("a failed trial must start a new cool-down")
	}
	if b.success() != breakerClosed || b.isOpen() || b.success() != breakerNoChange {
		t.Fatal("success must close once")
	}
}

func TestConversationsEvictWhenFull(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	c := newConversations(0)
	if c.ttl != DefaultConversationTTL {
		t.Fatal("default ttl")
	}
	for i := 0; i < maxConversations; i++ {
		exp := now.Add(time.Minute + time.Duration(i)*time.Second)
		if i%2 == 0 {
			exp = now.Add(-time.Minute)
		}
		c.entries["k"+strconv.Itoa(i)] = conversation{person: "p", thread: "t", expires: exp}
	}
	h := c.remember("", "alice", "", "thread", now)
	if h == "" || len(c.entries) > maxConversations {
		t.Fatalf("handle %q, %d entries", h, len(c.entries))
	}
	if c.remember("", "alice", "", "", now) != "" {
		t.Fatal("no thread, no handle")
	}
	if c.thread("", "alice", "", now) != "" || c.thread("missing", "alice", "", now) != "" {
		t.Fatal("unknown handles resolve to nothing")
	}
	if c.thread(h, "alice", "", now.Add(DefaultConversationTTL)) != "" {
		t.Fatal("an expired handle must not resolve")
	}

	full := newConversations(time.Hour)
	for i := 0; i < maxConversations; i++ {
		full.entries["k"+strconv.Itoa(i)] = conversation{expires: now.Add(time.Duration(i+1) * time.Second)}
	}
	full.remember("", "alice", "", "thread", now)
	if _, ok := full.entries["k0"]; ok || len(full.entries) != maxConversations {
		t.Fatalf("the soonest-expiring entry must be evicted (%d entries)", len(full.entries))
	}
}

func TestLimiterPrunesIdleCounters(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	l := newLimiter(generous)
	old := now.Add(-2 * time.Hour)
	for i := 0; i < maxTrackedKeys; i++ {
		k := "p" + strconv.Itoa(i)
		l.persons[k] = &personState{hour: window{start: old}}
		l.hives[k] = &window{start: old}
	}
	if d := l.reserve(now, "alice", "hive"); !d.ok {
		t.Fatal("reserve")
	}
	if len(l.persons) > 1 || len(l.hives) > 1 {
		t.Fatalf("idle counters kept: %d persons, %d hives", len(l.persons), len(l.hives))
	}
	l.release("alice")
	l.release("nobody")
	if l.inFlight != 0 {
		t.Fatal("release")
	}
}

func TestConsentStoreReportsCorruptRecords(t *testing.T) {
	dir := t.TempDir()
	store := &ConsentStore{Dir: dir}
	if err := os.WriteFile(filepath.Join(dir, "alice.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get("alice"); err == nil || store.Has("alice") {
		t.Fatal("a corrupt record must be an error and never count as consent")
	}
	if _, err := store.Grant("alice", ConsentTextVersion, time.Now()); err == nil {
		t.Fatal("grant over a corrupt record")
	}
	if _, err := store.Withdraw("alice", time.Now()); err == nil {
		t.Fatal("withdraw over a corrupt record")
	}
	if err := store.Delete("../x"); !errors.Is(err, ErrInvalidPerson) {
		t.Fatalf("delete invalid: %v", err)
	}
	var none *ConsentStore
	if none.Has("alice") {
		t.Fatal("a nil store holds no consent")
	}
	blocked := &ConsentStore{Dir: filepath.Join(dir, "alice.json", "sub")}
	if _, err := blocked.Grant("bob", ConsentTextVersion, time.Now()); err == nil {
		t.Fatal("grant into an unusable directory")
	}
}

func TestServiceDefaultsAndHelpers(t *testing.T) {
	svc, err := NewService(Options{Upstream: &fakeUpstream{answer: citedAnswer}, Limits: generous})
	if err != nil {
		t.Fatal(err)
	}
	if svc.opts.DocsURL != DocsHomeURL || svc.opts.UpstreamTimeout != DefaultUpstreamTimeout || svc.logger == nil {
		t.Fatalf("defaults %+v", svc.opts)
	}
	if st := svc.Stats(); st.Today != 0 || st.DailyBudget != generous.Daily || st.BreakerOpen || len(st.ByOutcome) != 0 {
		t.Fatalf("stats %+v", st)
	}
	if seconds(0) != 0 || seconds(1500*time.Millisecond) != 2 {
		t.Fatal("seconds rounds up")
	}
	if got := cleanText("ok\xffbad​\x01", 100, ""); got != "okbad" {
		t.Fatalf("cleanText %q", got)
	}
	if _, ok := AllowedSourceURL("https:opaque"); ok {
		t.Fatal("opaque URL")
	}
	if len(SectionIDs()) != len(sectionLabels) {
		t.Fatal("SectionIDs")
	}
	if (&UpstreamStatusError{Status: http.StatusTooManyRequests}).Error() == "" {
		t.Fatal("error text")
	}
}
