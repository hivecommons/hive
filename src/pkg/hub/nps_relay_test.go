package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// Hub side of the standalone NPS relay (issue #9619).

// npsTestPullSecret is the hub pull secret the fake relay expects.
const npsTestPullSecret = "nps-test-pull-secret"

// npsTestRelayBase is the relay base path the fake relay serves under.
const npsTestRelayBase = "/api/nps"

// npsFakeRelay is an in-memory relay: pending entries, the hub-only pull and
// ack endpoints, and a switch to make acks fail.
type npsFakeRelay struct {
	mu        sync.Mutex
	entries   []npsRelayEntry
	failAcks  int // fail this many upcoming acks with 500
	pulls     int
	acks      int
	ackedIDs  []string
	badAuth   int
	lastLimit string
	srv       *httptest.Server
}

func newNPSFakeRelay(t *testing.T, entries ...npsRelayEntry) *npsFakeRelay {
	t.Helper()
	f := &npsFakeRelay{entries: entries}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *npsFakeRelay) url() string { return f.srv.URL + npsTestRelayBase }

func (f *npsFakeRelay) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+npsTestPullSecret {
		f.badAuth++
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == npsTestRelayBase+npsRelayPendingPath:
		f.pulls++
		f.lastLimit = r.URL.Query().Get(npsRelayLimitParam)
		limit, _ := strconv.Atoi(f.lastLimit)
		out := f.entries
		if limit > 0 && len(out) > limit {
			out = out[:limit]
		}
		_ = json.NewEncoder(w).Encode(npsRelayPendingResponse{Entries: out})
	case r.Method == http.MethodPost && r.URL.Path == npsTestRelayBase+npsRelayAckPath:
		f.acks++
		if f.failAcks > 0 {
			f.failAcks--
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var req npsRelayAckRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad ack", http.StatusBadRequest)
			return
		}
		drop := map[string]bool{}
		for _, id := range req.IDs {
			drop[id] = true
		}
		f.ackedIDs = append(f.ackedIDs, req.IDs...)
		kept := f.entries[:0:0]
		for _, e := range f.entries {
			if !drop[e.ID] {
				kept = append(kept, e)
			}
		}
		f.entries = kept
		_ = json.NewEncoder(w).Encode(map[string]int{"deleted": len(req.IDs)})
	default:
		http.NotFound(w, r)
	}
}

func (f *npsFakeRelay) pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// npsFakeRelayStats is a locked copy of the fake relay's counters (the
// handler runs on server goroutines, so tests never read the fields raw).
type npsFakeRelayStats struct {
	pulls, acks, badAuth int
	lastLimit            string
	ackedIDs             []string
}

func (f *npsFakeRelay) stats() npsFakeRelayStats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return npsFakeRelayStats{
		pulls: f.pulls, acks: f.acks, badAuth: f.badAuth,
		lastLimit: f.lastLimit,
		ackedIDs:  append([]string(nil), f.ackedIDs...),
	}
}

func (f *npsFakeRelay) setFailAcks(n int) {
	f.mu.Lock()
	f.failAcks = n
	f.mu.Unlock()
}

// npsTestRelayInstallID is the self-registered install id the fake relay
// stamps on entries (a lowercase UUID, as the relay issues them).
const npsTestRelayInstallID = "0f8e1c2a-3b4d-4e5f-8a6b-7c8d9e0f1a2b"

func npsRelayTestEntry(id, hiveID string, score int, feedback string) npsRelayEntry {
	return npsRelayEntry{
		ID:               id,
		InstallID:        npsTestRelayInstallID,
		HiveID:           hiveID,
		Score:            score,
		Feedback:         feedback,
		DashboardVersion: "abc1234",
		Timestamp:        time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
	}
}

func npsPullOnce(t *testing.T, s *HubServer, relay *npsFakeRelay) (int, error) {
	t.Helper()
	return s.pullNPSRelay(context.Background(), npsRelayHTTPClient(), relay.url(), npsTestPullSecret)
}

// TestNPSRelayPullMergesIntoAdminView is the end-to-end acceptance: relay
// entries land in the store tagged source=relay, show up in
// GET /api/admin/nps next to direct responses, and are acked off the relay.
func TestNPSRelayPullMergesIntoAdminView(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("h1")
	if rec := npsIngest(s, `{"hive_id":"h1","score":4}`, s.heartbeatKeyFor("h1")); rec.Code != http.StatusCreated {
		t.Fatalf("seed direct response: %d", rec.Code)
	}
	relay := newNPSFakeRelay(t,
		npsRelayTestEntry("r-1", "solo-hive", 1, "crashes on start"),
		npsRelayTestEntry("r-2", "solo-hive", 3, ""),
	)

	merged, err := npsPullOnce(t, s, relay)
	if err != nil || merged != 2 {
		t.Fatalf("pull = (%d, %v), want (2, nil)", merged, err)
	}
	if relay.pending() != 0 {
		t.Errorf("relay still holds %d entries after a successful pull", relay.pending())
	}
	if relay.stats().lastLimit != strconv.Itoa(npsRelayPullBatchSize) {
		t.Errorf("pull limit = %q, want %d", relay.stats().lastLimit, npsRelayPullBatchSize)
	}

	rec := httptest.NewRecorder()
	s.handleAdminNPS(rec, httptest.NewRequest(http.MethodGet, npsAdminPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin view: %d", rec.Code)
	}
	var agg npsAggregation
	if err := json.Unmarshal(rec.Body.Bytes(), &agg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if agg.Total != 3 {
		t.Fatalf("admin total = %d, want 3 (1 direct + 2 relay)", agg.Total)
	}
	relayed := 0
	for _, r := range agg.Recent {
		if r.Source == npsSourceRelay {
			relayed++
			if r.HiveID != "solo-hive" {
				t.Errorf("relay response hive = %q", r.HiveID)
			}
			// The admin view must be able to label it as an unverified,
			// self-registered install.
			if r.InstallID != npsTestRelayInstallID {
				t.Errorf("relay response install_id = %q, want %q", r.InstallID, npsTestRelayInstallID)
			}
		} else {
			if r.HiveID != "h1" {
				t.Errorf("direct response hive = %q", r.HiveID)
			}
			if r.InstallID != "" {
				t.Errorf("direct response carries an install_id: %q", r.InstallID)
			}
		}
	}
	if relayed != 2 {
		t.Errorf("recent relay responses = %d, want 2: %+v", relayed, agg.Recent)
	}
	var solo *npsHiveBreakdown
	for i := range agg.PerHive {
		if agg.PerHive[i].HiveID == "solo-hive" {
			solo = &agg.PerHive[i]
		}
	}
	if solo == nil || solo.Total != 2 || solo.Source != npsSourceRelay {
		t.Fatalf("per-hive breakdown for the relay hive = %+v", solo)
	}
	for _, r := range npsStored(t) {
		if r.Source == npsSourceRelay && (r.RelayID == "" || r.InstallID != npsTestRelayInstallID) {
			t.Errorf("stored relay record missing its relay id or install id: %+v", r)
		}
	}
}

// TestNPSRelayPullIdempotentOnRetry: when the ack fails after a successful
// merge, the relay returns the same entries next time, and the second pull
// must not duplicate them.
func TestNPSRelayPullIdempotentOnRetry(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub()
	relay := newNPSFakeRelay(t,
		npsRelayTestEntry("r-1", "solo-hive", 4, "love it"),
		npsRelayTestEntry("r-2", "other-solo", 2, ""),
	)
	relay.setFailAcks(1)

	merged, err := npsPullOnce(t, s, relay)
	if err == nil {
		t.Fatal("a failed ack must surface as an error")
	}
	if merged != 2 || len(npsStored(t)) != 2 {
		t.Fatalf("first pull merged %d, stored %d; want 2 and 2", merged, len(npsStored(t)))
	}
	if relay.pending() != 2 {
		t.Fatalf("relay dropped entries without an ack: %d pending", relay.pending())
	}

	merged, err = npsPullOnce(t, s, relay)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if merged != 0 {
		t.Errorf("retry merged %d new responses, want 0", merged)
	}
	if got := len(npsStored(t)); got != 2 {
		t.Fatalf("retry duplicated responses: %d stored, want 2", got)
	}
	if relay.pending() != 0 {
		t.Errorf("retry did not ack: %d still pending", relay.pending())
	}

	// A third pull against an empty relay is a clean no-op.
	if merged, err := npsPullOnce(t, s, relay); err != nil || merged != 0 {
		t.Fatalf("empty relay pull = (%d, %v)", merged, err)
	}
	if got := len(npsStored(t)); got != 2 {
		t.Fatalf("stored = %d after an empty pull", got)
	}
}

// TestNPSRelayPullDropsInvalidEntries: entries that fail validation are
// acked (so they do not clog the relay) but never stored; an entry claiming a
// hub-registered hive is treated the same way; an entry with a malformed id
// is neither stored nor echoed back.
func TestNPSRelayPullDropsInvalidEntries(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub("registered")
	good := npsRelayTestEntry("ok-1", "solo-hive", 4, "<script>x</script>\x07 fine")
	future := npsRelayTestEntry("ok-2", "solo-hive", 2, "")
	future.Timestamp = time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)
	relay := newNPSFakeRelay(t,
		good,
		future,
		npsRelayTestEntry("bad-score", "solo-hive", 7, ""),
		npsRelayTestEntry("bad-hive", "solo hive!", 3, ""),
		npsRelayTestEntry("spoof", "registered", 1, "fake"),
		npsRelayTestEntry("bad id!", "solo-hive", 3, ""),
	)

	merged, err := npsPullOnce(t, s, relay)
	if err != nil || merged != 2 {
		t.Fatalf("pull = (%d, %v), want (2, nil)", merged, err)
	}
	stored := npsStored(t)
	ids := map[string]npsRecord{}
	for _, r := range stored {
		ids[r.RelayID] = r
		if r.HiveID == "registered" {
			t.Errorf("a relay entry was stored under a registered hive: %+v", r)
		}
	}
	if len(stored) != 2 || ids["ok-1"].Score != 4 || ids["ok-2"].Score != 2 {
		t.Fatalf("stored = %+v", stored)
	}
	if fb := ids["ok-1"].Feedback; strings.ContainsRune(fb, '\x07') || !strings.Contains(fb, "<script>x</script>") {
		t.Errorf("feedback not sanitized like direct ingest: %q", fb)
	}
	if ts, err := time.Parse(time.RFC3339, ids["ok-2"].Timestamp); err != nil || ts.After(time.Now().Add(npsRelayMaxClockSkew)) {
		t.Errorf("a future relay timestamp was stored as-is: %q", ids["ok-2"].Timestamp)
	}

	acked := strings.Join(relay.stats().ackedIDs, ",")
	for _, want := range []string{"ok-1", "ok-2", "bad-score", "bad-hive", "spoof"} {
		if !strings.Contains(","+acked+",", ","+want+",") {
			t.Errorf("entry %q was not acked (acked: %s)", want, acked)
		}
	}
	if strings.Contains(acked, "bad id!") {
		t.Errorf("a malformed id was echoed back in the ack: %s", acked)
	}
}

// TestNPSRelayPullAuthenticates: every relay call carries the pull secret,
// and a wrong secret stores nothing.
func TestNPSRelayPullAuthenticates(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub()
	relay := newNPSFakeRelay(t, npsRelayTestEntry("r-1", "solo-hive", 4, ""))

	merged, err := s.pullNPSRelay(context.Background(), npsRelayHTTPClient(), relay.url(), "wrong-secret")
	if err == nil || merged != 0 {
		t.Fatalf("wrong secret: (%d, %v), want an error and nothing merged", merged, err)
	}
	if st := relay.stats(); len(npsStored(t)) != 0 || st.badAuth != 1 {
		t.Fatalf("wrong secret: stored %d, bad-auth hits %d", len(npsStored(t)), st.badAuth)
	}

	if _, err := npsPullOnce(t, s, relay); err != nil {
		t.Fatalf("right secret: %v", err)
	}
	if st := relay.stats(); st.badAuth != 1 || st.pulls != 1 || st.acks != 1 {
		t.Errorf("right secret: badAuth=%d pulls=%d acks=%d", st.badAuth, st.pulls, st.acks)
	}
}

// TestNPSRelayPullRejectsOversizedResponse: a pull response is capped by
// bytes read, and an oversized one stores nothing.
func TestNPSRelayPullRejectsOversizedResponse(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub()
	var acks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == npsTestRelayBase+npsRelayAckPath {
			acks.Add(1)
			return
		}
		fmt.Fprintf(w, `{"entries":[],"pad":"%s"}`, strings.Repeat("x", npsRelayMaxPullResponseBytes))
	}))
	t.Cleanup(srv.Close)
	merged, err := s.pullNPSRelay(context.Background(), npsRelayHTTPClient(), srv.URL+npsTestRelayBase, npsTestPullSecret)
	if err == nil || merged != 0 || acks.Load() != 0 {
		t.Fatalf("oversized pull: (%d, %v), acks %d; want an error, nothing merged, no ack", merged, err, acks.Load())
	}
}

// TestNPSRelayPullDrainsBatches: a full batch triggers another pull in the
// same cycle, up to the per-cycle ceiling.
func TestNPSRelayPullDrainsBatches(t *testing.T) {
	useTempNPSStore(t)
	s := npsTestHub()
	const extra = 5
	var entries []npsRelayEntry
	for i := 0; i < npsRelayPullBatchSize+extra; i++ {
		entries = append(entries, npsRelayTestEntry(fmt.Sprintf("r-%d", i), fmt.Sprintf("solo-%d", i), 3, ""))
	}
	relay := newNPSFakeRelay(t, entries...)
	merged, err := npsPullOnce(t, s, relay)
	if err != nil || merged != npsRelayPullBatchSize+extra {
		t.Fatalf("pull = (%d, %v), want (%d, nil)", merged, err, npsRelayPullBatchSize+extra)
	}
	if st := relay.stats(); st.pulls != 2 || relay.pending() != 0 {
		t.Errorf("pulls = %d (want 2), pending = %d (want 0)", st.pulls, relay.pending())
	}
}

// TestNPSMergeRelayKeepsOrderAndCaps: merged relay records are placed by
// timestamp among direct ones, and the per-hive rolling cap still applies.
func TestNPSMergeRelayKeepsOrderAndCaps(t *testing.T) {
	useTempNPSStore(t)
	now := time.Now().UTC()
	if err := hubNPS.add(npsRecord{HiveID: "h1", Score: 4, Timestamp: now.Format(time.RFC3339)}, now); err != nil {
		t.Fatal(err)
	}
	older := npsRecord{HiveID: "solo", Score: 1, Timestamp: now.Add(-time.Hour).Format(time.RFC3339), Source: npsSourceRelay, RelayID: "old"}
	if added, err := hubNPS.mergeRelay([]npsRecord{older, older, {HiveID: "solo", Score: 2}}); err != nil || added != 1 {
		t.Fatalf("merge = (%d, %v), want 1 (duplicate and id-less records skipped)", added, err)
	}
	stored := npsStored(t)
	if len(stored) != 2 || stored[0].RelayID != "old" || stored[1].HiveID != "h1" {
		t.Fatalf("store not oldest-first after merge: %+v", stored)
	}

	var many []npsRecord
	for i := 0; i < npsMaxResponsesPerHive+1; i++ {
		many = append(many, npsRecord{
			HiveID: "solo", Score: 3, Source: npsSourceRelay, RelayID: fmt.Sprintf("m-%d", i),
			Timestamp: now.Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		})
	}
	if _, err := hubNPS.mergeRelay(many); err != nil {
		t.Fatal(err)
	}
	solo := 0
	for _, r := range npsStored(t) {
		if r.HiveID == "solo" {
			solo++
		}
	}
	if solo != npsMaxResponsesPerHive {
		t.Errorf("relay hive holds %d records, want the per-hive cap %d", solo, npsMaxResponsesPerHive)
	}
}

// TestStartNPSRelayPullDisabledWithoutConfig: with no relay URL or no pull
// secret the poller returns at once (even on a live context) and never
// contacts anything.
func TestStartNPSRelayPullDisabledWithoutConfig(t *testing.T) {
	relay := newNPSFakeRelay(t, npsRelayTestEntry("r-1", "solo-hive", 4, ""))
	cases := map[string][2]string{
		"nothing":      {"", ""},
		"url only":     {relay.url(), ""},
		"secret only":  {"", npsTestPullSecret},
		"insecure url": {"http://relay.example/api/nps", npsTestPullSecret},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv(config.NPSRelayURLEnvVar, c[0])
			t.Setenv(config.NPSRelayPullSecretEnvVar, c[1])
			s := npsTestHub()
			if u, sec := s.npsRelayPullSettings(); u != "" && sec != "" {
				t.Fatalf("settings resolved as enabled: %q", u)
			}
			done := make(chan struct{})
			go func() {
				s.StartNPSRelayPull(context.Background())
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("StartNPSRelayPull did not return with the relay unconfigured")
			}
		})
	}
	if st := relay.stats(); st.pulls != 0 || st.badAuth != 0 {
		t.Fatalf("an unconfigured hub contacted the relay")
	}
}

// TestStartNPSRelayPullStopsOnCancel: a configured poller waits out its
// startup delay and exits on ctx cancel without pulling.
func TestStartNPSRelayPullStopsOnCancel(t *testing.T) {
	relay := newNPSFakeRelay(t)
	t.Setenv(config.NPSRelayURLEnvVar, relay.url())
	t.Setenv(config.NPSRelayPullSecretEnvVar, npsTestPullSecret)
	s := npsTestHub()
	if u, sec := s.npsRelayPullSettings(); u != relay.url() || sec != npsTestPullSecret {
		t.Fatalf("env settings not resolved: %q", u)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		s.StartNPSRelayPull(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartNPSRelayPull did not stop on cancel")
	}
	if st := relay.stats(); st.pulls != 0 {
		t.Errorf("pulled before the startup delay: %d", st.pulls)
	}
}
