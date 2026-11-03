package spoke

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/hubactivity"
)

var activityTestT0 = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

// activityClock is a hand-driven clock for the publisher.
type activityClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *activityClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *activityClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// activityItemsBox is a tracker whose items a test can swap.
type activityItemsBox struct {
	mu    sync.Mutex
	items []hubactivity.Item
}

func (b *activityItemsBox) Set(items ...hubactivity.Item) {
	b.mu.Lock()
	b.items = items
	b.mu.Unlock()
}

func (b *activityItemsBox) Items(context.Context) ([]hubactivity.Item, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]hubactivity.Item(nil), b.items...), true
}

func activityItem(repo string, n, stage int, mods ...string) hubactivity.Item {
	return hubactivity.Item{Repo: repo, Number: n, Kind: "pr", Title: "item", Stage: "stage", StageNumber: stage, Mods: mods}
}

func testActivityPublisher(opts ActivityOptions, box *activityItemsBox, clock *activityClock) *activityPublisher {
	p := newActivityPublisher(ActivitySource{
		Options: func() ActivityOptions { return opts },
		Items:   box.Items,
	}, "", nil2Logger())
	p.now = clock.Now
	return p
}

func digestNumbers(d *hubactivity.Digest) []int {
	out := make([]int, 0, len(d.Items))
	for _, it := range d.Items {
		out = append(out, it.Number)
	}
	return out
}

func TestActivityDigestCapKeepsTheMostUrgentAndDisclosesTruncation(t *testing.T) {
	box := &activityItemsBox{}
	box.Set(
		activityItem("o/r", 1, 10),
		activityItem("o/r", 2, 90),
		activityItem("o/r", 3, 20, "stalled"),
		activityItem("o/r", 4, 5, "needs-decision"),
		activityItem("o/r", 5, 50),
		activityItem("o/r", 6, 30, "needs-human"),
	)
	p := testActivityPublisher(ActivityOptions{Enabled: true, MaxItems: 4}, box, &activityClock{now: activityTestT0})

	d := p.heartbeatDigestLocked(context.Background())
	if d == nil {
		t.Fatal("enabled publisher produced no digest")
	}
	// needs-human/needs-decision first (furthest stage first among them),
	// then stalled, then furthest stage.
	want := []int{6, 4, 3, 2}
	if got := digestNumbers(d); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("capped items = %v, want %v", got, want)
	}
	if d.Truncated != 2 {
		t.Fatalf("truncated = %d, want the 2 items the cap left out", d.Truncated)
	}
	if d.Totals == nil || d.Totals.Items != 6 {
		t.Fatalf("totals = %+v, want all 6 tracked items counted", d.Totals)
	}
	if !d.GeneratedAt.Equal(activityTestT0) || d.ETag == "" {
		t.Fatalf("digest generated_at=%v etag=%q, want the injected clock and a content etag", d.GeneratedAt, d.ETag)
	}
}

func TestActivityDigestDefaultCapAndCeiling(t *testing.T) {
	box := &activityItemsBox{}
	items := make([]hubactivity.Item, 0, hubactivity.MaxItemsCeiling+5)
	for n := 1; n <= hubactivity.MaxItemsCeiling+5; n++ {
		items = append(items, activityItem("o/r", n, 10))
	}
	box.Set(items...)
	clock := &activityClock{now: activityTestT0}

	d := testActivityPublisher(ActivityOptions{Enabled: true}, box, clock).heartbeatDigestLocked(context.Background())
	if len(d.Items) != hubactivity.DefaultMaxItems || d.Truncated != len(items)-hubactivity.DefaultMaxItems {
		t.Fatalf("default cap: %d items truncated=%d, want %d", len(d.Items), d.Truncated, hubactivity.DefaultMaxItems)
	}
	d = testActivityPublisher(ActivityOptions{Enabled: true, MaxItems: 10_000}, box, clock).heartbeatDigestLocked(context.Background())
	if len(d.Items) != hubactivity.MaxItemsCeiling || d.Truncated != 5 {
		t.Fatalf("over-ceiling cap: %d items truncated=%d, want the %d-item ceiling", len(d.Items), d.Truncated, hubactivity.MaxItemsCeiling)
	}
}

func TestActivityDigestDropsPrivateItemsButCountsThem(t *testing.T) {
	box := &activityItemsBox{}
	priv := activityItem("o/secret", 2, 40, "needs-human")
	priv.Private = true
	box.Set(activityItem("o/public", 1, 10), priv)
	clock := &activityClock{now: activityTestT0}

	d := testActivityPublisher(ActivityOptions{Enabled: true}, box, clock).heartbeatDigestLocked(context.Background())
	if got := digestNumbers(d); len(got) != 1 || got[0] != 1 {
		t.Fatalf("items = %v, want only the public item", got)
	}
	if d.Truncated != 0 {
		t.Fatalf("truncated = %d: a dropped private item is not a capped one", d.Truncated)
	}
	if d.Totals.Items != 2 || d.Totals.ByModifier["needs-human"] != 1 {
		t.Fatalf("totals = %+v, want the private item still counted", d.Totals)
	}

	shared := testActivityPublisher(ActivityOptions{Enabled: true, SharePrivate: true}, box, clock).heartbeatDigestLocked(context.Background())
	if len(shared.Items) != 2 || shared.Items[0].Number != 2 || !shared.Items[0].Private {
		t.Fatalf("share_private items = %+v, want the private item kept and marked", shared.Items)
	}
}

func TestActivityDigestSendsOnlyTheEtagOnceAcknowledged(t *testing.T) {
	box := &activityItemsBox{}
	box.Set(activityItem("o/r", 1, 10))
	clock := &activityClock{now: activityTestT0}
	p := testActivityPublisher(ActivityOptions{Enabled: true}, box, clock)
	ctx := context.Background()

	full := p.heartbeatDigestLocked(ctx)
	if full.IsUnchangedMarker() {
		t.Fatal("first digest must be the full snapshot")
	}
	p.acknowledgeLocked(full.ETag)

	clock.Advance(2 * time.Minute)
	marker := p.heartbeatDigestLocked(ctx)
	if !marker.IsUnchangedMarker() || marker.ETag != full.ETag || !marker.GeneratedAt.Equal(activityTestT0.Add(2*time.Minute)) {
		t.Fatalf("acknowledged digest = %+v, want only etag %q and the new generated_at", marker, full.ETag)
	}
	raw, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys["etag"] == nil || keys["generated_at"] == nil {
		t.Fatalf("marker wire form = %s, want only etag and generated_at", raw)
	}

	box.Set(activityItem("o/r", 1, 10), activityItem("o/r", 2, 20))
	if changed := p.heartbeatDigestLocked(ctx); changed.IsUnchangedMarker() || changed.ETag == full.ETag {
		t.Fatalf("changed tracker produced %+v, want a new full digest", changed)
	}

	// A hub that lost the block (restart) acknowledges nothing: resend.
	p.acknowledgeLocked("")
	box.Set(activityItem("o/r", 1, 10))
	if resent := p.heartbeatDigestLocked(ctx); resent.IsUnchangedMarker() {
		t.Fatal("a digest the hub no longer holds must be sent in full")
	}
}

func TestActivityDigestAbsentWhenDisabledOrUnavailable(t *testing.T) {
	ctx := context.Background()
	box := &activityItemsBox{}
	clock := &activityClock{now: activityTestT0}
	if d := testActivityPublisher(ActivityOptions{}, box, clock).heartbeatDigestLocked(ctx); d != nil {
		t.Fatalf("disabled publisher sent %+v", d)
	}
	if d := newActivityPublisher(ActivitySource{}, "", nil).heartbeatDigestLocked(ctx); d != nil {
		t.Fatalf("publisher without a source sent %+v", d)
	}
	none := newActivityPublisher(ActivitySource{
		Options: func() ActivityOptions { return ActivityOptions{Enabled: true} },
		Items:   func(context.Context) ([]hubactivity.Item, bool) { return nil, false },
	}, "", nil2Logger())
	if d := none.heartbeatDigestLocked(ctx); d != nil {
		t.Fatalf("publisher without a tracker sent %+v", d)
	}
}

func TestActivityItemsWithTimeoutBoundsASlowOrPanickingTracker(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	slow := func(context.Context) ([]hubactivity.Item, bool) {
		<-release
		return []hubactivity.Item{activityItem("o/r", 1, 1)}, true
	}
	if items, ok := activityItemsWithTimeout(context.Background(), slow, time.Millisecond, nil2Logger()); ok || items != nil {
		t.Fatalf("slow tracker = %v, %v; want no items", items, ok)
	}
	panicky := func(context.Context) ([]hubactivity.Item, bool) { panic("boom") }
	if items, ok := activityItemsWithTimeout(context.Background(), panicky, time.Minute, nil2Logger()); ok || items != nil {
		t.Fatalf("panicking tracker = %v, %v; want no items", items, ok)
	}
}

func TestRedactActivityHonoursHeartbeatOmit(t *testing.T) {
	d := hubactivity.Build([]hubactivity.Item{activityItem("o/r", 1, 10), activityItem("o/r", 2, 20)}, hubactivity.BuildOptions{MaxItems: 1, Now: activityTestT0})

	if same := redactActivity(d, nil); same.ETag != d.ETag || len(same.Items) != 1 {
		t.Fatalf("no omit classes changed the digest: %+v", same)
	}
	noRepos := redactActivity(d, map[string]bool{"repos": true})
	if len(noRepos.Items) != 0 || noRepos.Truncated != 0 || noRepos.Totals.Items != 2 || noRepos.ETag == d.ETag || noRepos.ETag == "" {
		t.Fatalf("withheld repos = %+v, want totals only under a new etag", noRepos)
	}
	noTitles := redactActivity(d, map[string]bool{"task_titles": true})
	if len(noTitles.Items) != 1 || noTitles.Items[0].Title != "" || noTitles.ETag == d.ETag {
		t.Fatalf("withheld titles = %+v, want blank titles under a new etag", noTitles)
	}
	if d.Items[0].Title == "" {
		t.Fatal("redaction must not modify the input digest")
	}
}

func TestActivityDigestAppliesConfiguredOmitClasses(t *testing.T) {
	SetHeartbeatOmit([]string{"repos"})
	t.Cleanup(func() { SetHeartbeatOmit(nil) })
	box := &activityItemsBox{}
	box.Set(activityItem("o/r", 1, 10))
	d := testActivityPublisher(ActivityOptions{Enabled: true}, box, &activityClock{now: activityTestT0}).heartbeatDigestLocked(context.Background())
	if len(d.Items) != 0 || d.Totals.Items != 1 {
		t.Fatalf("hub.heartbeat_omit [repos] digest = %+v, want totals without items", d)
	}
}

// recordingPost is an activityPostFunc that reports each send and, when
// gate is non-nil, holds the send in flight until the test releases it.
type recordingPost struct {
	sent   chan *hubactivity.Digest
	gate   chan struct{}
	status int
	ok     bool
}

func (r *recordingPost) post(_ context.Context, d *hubactivity.Digest) (activityAck, int, bool) {
	r.sent <- d
	if r.gate != nil {
		<-r.gate
	}
	if !r.ok {
		return activityAck{}, r.status, false
	}
	ack := activityAck{ETag: d.ETag}
	if n := len(d.Events); n > 0 {
		ack.Events = d.Events[n-1].Seq
	}
	return ack, r.status, true
}

func TestActivityUpdatesCoalesceToOnePer15sWithBackpressure(t *testing.T) {
	box := &activityItemsBox{}
	box.Set(activityItem("o/r", 1, 10))
	clock := &activityClock{now: activityTestT0}
	p := testActivityPublisher(ActivityOptions{Enabled: true}, box, clock)
	rec := &recordingPost{sent: make(chan *hubactivity.Digest, 8), gate: make(chan struct{}), status: http.StatusOK, ok: true}
	p.post = rec.post
	// built reports each digest build. run drains a pending kick just
	// before it builds, so a build observed here proves a later notify
	// is not swallowed by that drain.
	built := make(chan struct{}, 8)
	items := p.src.Items
	p.src.Items = func(ctx context.Context) ([]hubactivity.Item, bool) {
		built <- struct{}{}
		return items(ctx)
	}
	waits := make(chan time.Duration, 8)
	fire := make(chan time.Time)
	p.after = func(d time.Duration) <-chan time.Time {
		waits <- d
		return fire
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.run(ctx)
	}()
	defer func() {
		cancel()
		<-done
	}()

	// First publish: nothing sent recently, so the update goes out at once.
	p.notify()
	<-built
	first := <-rec.sent
	if digestNumbers(first)[0] != 1 {
		t.Fatalf("first update = %v", digestNumbers(first))
	}

	// While it is in flight, a burst of publishes merges into one pending
	// signal; notify never blocks.
	box.Set(activityItem("o/r", 1, 10), activityItem("o/r", 2, 20))
	for range 5 {
		p.notify()
	}
	rec.gate <- struct{}{}

	// The merged signal waits out the rest of the 15 s window.
	if wait := <-waits; wait != activityUpdateMinInterval {
		t.Fatalf("second update waited %v, want %v after the first", wait, activityUpdateMinInterval)
	}
	p.notify()
	p.notify()
	clock.Advance(activityUpdateMinInterval)
	fire <- clock.Now()
	<-built
	second := <-rec.sent
	rec.gate <- struct{}{}
	if got := digestNumbers(second); len(got) != 2 {
		t.Fatalf("second update = %v, want the merged latest tracker", got)
	}

	// A publish with no change since the acknowledged etag sends nothing.
	p.notify()
	if wait := <-waits; wait != activityUpdateMinInterval {
		t.Fatalf("third signal waited %v, want %v", wait, activityUpdateMinInterval)
	}
	clock.Advance(time.Second)
	fire <- clock.Now()
	// Wait for the unchanged pass to build before signalling again,
	// otherwise the kick-drain ahead of that build swallows the signal.
	<-built
	// An unchanged pass does not restart the window, so the next signal
	// waits out the remainder of the last real update's 15 s.
	p.notify()
	if wait := <-waits; wait != activityUpdateMinInterval-time.Second {
		t.Fatalf("signal after an unchanged pass waited %v, want %v", wait, activityUpdateMinInterval-time.Second)
	}
	select {
	case d := <-rec.sent:
		t.Fatalf("unchanged tracker sent a redundant update: %+v", d)
	default:
	}
}

func TestActivityUpdateBacksOffWhenTheHubDoesNotSupportIt(t *testing.T) {
	box := &activityItemsBox{}
	box.Set(activityItem("o/r", 1, 10))
	clock := &activityClock{now: activityTestT0}
	p := testActivityPublisher(ActivityOptions{Enabled: true}, box, clock)
	rec := &recordingPost{sent: make(chan *hubactivity.Digest, 8), status: http.StatusBadRequest}
	p.post = rec.post
	ctx := context.Background()

	if p.update(ctx) {
		t.Fatal("a rejected update reported success")
	}
	<-rec.sent
	if p.update(ctx) || len(rec.sent) != 0 {
		t.Fatal("updates continued during the unsupported-hub backoff")
	}
	clock.Advance(activityUpdateBackoff)
	rec.status, rec.ok = http.StatusOK, true
	if !p.update(ctx) {
		t.Fatal("update did not resume after the backoff")
	}
	<-rec.sent
	if p.update(ctx) || len(rec.sent) != 0 {
		t.Fatal("an acknowledged, unchanged digest was sent again")
	}

	// Not-yet-registered (404) is transient: no backoff.
	box.Set(activityItem("o/r", 2, 10))
	rec.status, rec.ok = http.StatusNotFound, false
	p.update(ctx)
	<-rec.sent
	clock.Advance(time.Second)
	p.update(ctx)
	if len(rec.sent) != 1 {
		t.Fatal("a 404 must not pause updates")
	}
}

func TestActivityUpdateUnsupportedStatuses(t *testing.T) {
	for status, want := range map[int]bool{
		http.StatusBadRequest:          true,
		http.StatusMethodNotAllowed:    true,
		http.StatusNotImplemented:      true,
		http.StatusNotFound:            false,
		http.StatusUnauthorized:        false,
		http.StatusInternalServerError: false,
		0:                              false,
	} {
		if got := activityUpdateUnsupported(status); got != want {
			t.Errorf("activityUpdateUnsupported(%d) = %v, want %v", status, got, want)
		}
	}
}

func TestPostActivityUpdateCarriesOnlyTheActivityBlock(t *testing.T) {
	type seen struct {
		header string
		body   map[string]json.RawMessage
		update ActivityUpdatePayload
	}
	got := make(chan seen, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var s seen
		s.header = r.Header.Get(ActivityUpdateHeader)
		raw := json.RawMessage{}
		_ = json.NewDecoder(r.Body).Decode(&raw)
		_ = json.Unmarshal(raw, &s.body)
		_ = json.Unmarshal(raw, &s.update)
		got <- s
		if r.URL.Path != "/api/heartbeat" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{OK: true, ActivityETag: s.update.Update.Activity.ETag})
	}))
	defer srv.Close()

	d := hubactivity.Build([]hubactivity.Item{activityItem("o/r", 1, 10)}, hubactivity.BuildOptions{Now: activityTestT0})
	ack, status, ok := postActivityUpdate(context.Background(), srv.URL, "hive-act", &d, nil2Logger())
	if !ok || status != http.StatusOK || ack != d.ETag {
		t.Fatalf("postActivityUpdate = %q, %d, %v; want the acknowledged etag", ack, status, ok)
	}
	s := <-got
	if s.header == "" {
		t.Fatal("update not marked with the activity-update header")
	}
	if len(s.body) != 1 || s.body["activity_update"] == nil {
		t.Fatalf("update body keys = %v, want only activity_update (no top-level hive_id)", s.body)
	}
	if s.update.Update.HiveID != "hive-act" || s.update.Update.Activity.ETag != d.ETag {
		t.Fatalf("update = %+v", s.update.Update)
	}

	if _, _, ok := postActivityUpdate(context.Background(), srv.URL, "", &d, nil2Logger()); ok {
		t.Fatal("an update without a hive identity must not be sent")
	}
}

func TestPostActivityUpdateReportsRejectionStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "hive_id required", http.StatusBadRequest)
	}))
	defer srv.Close()
	d := hubactivity.Build(nil, hubactivity.BuildOptions{Now: activityTestT0})
	if _, status, ok := postActivityUpdate(context.Background(), srv.URL, "hive-act", &d, nil2Logger()); ok || status != http.StatusBadRequest {
		t.Fatalf("rejected update = %d, %v; want the 400 reported", status, ok)
	}
	if _, status, ok := postActivityUpdate(context.Background(), "http://127.0.0.1:0", "hive-act", &d, nil2Logger()); ok || status != 0 {
		t.Fatalf("unreachable hub = %d, %v; want no status", status, ok)
	}
}

func TestPostHeartbeatToHubAttachesTheDigestAndHonoursTheAck(t *testing.T) {
	box := &activityItemsBox{}
	box.Set(activityItem("o/r", 1, 10))
	p := testActivityPublisher(ActivityOptions{Enabled: true}, box, &activityClock{now: activityTestT0})
	activityPub.Store(p)
	t.Cleanup(func() { activityPub.Store(nil) })

	blocks := make(chan *hubactivity.Digest, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload HeartbeatPayload
		_ = json.NewDecoder(r.Body).Decode(&payload)
		blocks <- payload.Activity
		ack := ""
		if payload.Activity != nil {
			ack = payload.Activity.ETag
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{OK: true, ActivityETag: ack})
	}))
	defer srv.Close()

	cached := &HeartbeatPayload{HiveID: "hive-act"}
	if resp := postHeartbeatToHub(context.Background(), srv.URL, cached, nil2Logger()); resp == nil || resp.ActivityETag == "" {
		t.Fatalf("first beat response = %+v, want an acknowledged etag", resp)
	}
	first := <-blocks
	if first == nil || first.IsUnchangedMarker() || len(first.Items) != 1 {
		t.Fatalf("first beat activity = %+v, want the full digest", first)
	}
	if cached.Activity != nil {
		t.Fatal("the caller's (cached) payload must not be modified")
	}

	postHeartbeatToHub(context.Background(), srv.URL, cached, nil2Logger())
	if second := <-blocks; second == nil || !second.IsUnchangedMarker() || second.ETag != first.ETag {
		t.Fatalf("second beat activity = %+v, want only the acknowledged etag", second)
	}
}

func TestStartHeartbeatWiresTheActivitySource(t *testing.T) {
	resetSingleLoopGuard(t)
	collapseReadinessWait(t)

	beats := make(chan *hubactivity.Digest, 8)
	updates := make(chan ActivityUpdate, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(ActivityUpdateHeader) != "" {
			var u ActivityUpdatePayload
			_ = json.NewDecoder(r.Body).Decode(&u)
			if u.Update != nil {
				updates <- *u.Update
			}
			_ = json.NewEncoder(w).Encode(HeartbeatResponse{OK: true})
			return
		}
		var payload HeartbeatPayload
		_ = json.NewDecoder(r.Body).Decode(&payload)
		beats <- payload.Activity
		// Acknowledge nothing, so the out-of-band update has news to send.
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{OK: true})
	}))
	defer srv.Close()

	box := &activityItemsBox{}
	box.Set(activityItem("o/r", 7, 10))
	subscribed := make(chan func(), 1)
	src := ActivitySource{
		Options:   func() ActivityOptions { return ActivityOptions{Enabled: true} },
		Items:     box.Items,
		Subscribe: func(notify func()) { subscribed <- notify },
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		StartHeartbeat(ctx, srv.URL, func() *HeartbeatPayload { return &HeartbeatPayload{HiveID: "hive-act"} }, time.Hour, nil2Logger(), src)
	}()

	notify := <-subscribed
	if beat := <-beats; beat == nil || len(beat.Items) != 1 || beat.Items[0].Number != 7 {
		t.Fatalf("heartbeat activity = %+v, want the tracker digest", beat)
	}
	notify()
	select {
	case u := <-updates:
		if u.HiveID != "hive-act" || u.Activity == nil || len(u.Activity.Items) != 1 {
			t.Fatalf("activity update = %+v", u)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("tracker publish sent no activity update")
	}
	cancel()
	<-done
	if activityPub.Load() != nil {
		t.Fatal("the stopped loop left its publisher installed")
	}
}
