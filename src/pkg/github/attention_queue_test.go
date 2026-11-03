package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/attention"
	"github.com/hivecommons/hive/pkg/prstatus"
	"github.com/hivecommons/hive/pkg/writeback"
)

// attentionQueueServer fakes the endpoints the attention pass touches: the
// items, their comments, timelines and reviews, and the tracking issue.
type attentionQueueServer struct {
	mu             sync.Mutex
	items          map[int]map[string]any
	comments       map[int][]map[string]any
	timeline       map[int][]map[string]any
	reviews        map[int][]map[string]any
	queue          []map[string]any
	queueCreates   int
	queueEdits     int
	commentCreates map[int]int
	reviewWrites   []string
	nextID         int
}

var (
	aqItemRe     = regexp.MustCompile(`^/repos/o/r/issues/(\d+)$`)
	aqCommentsRe = regexp.MustCompile(`^/repos/o/r/issues/(\d+)/comments$`)
	aqTimelineRe = regexp.MustCompile(`^/repos/o/r/issues/(\d+)/timeline$`)
	aqReviewsRe  = regexp.MustCompile(`^/repos/o/r/pulls/(\d+)/reviews$`)
)

func newAttentionQueueServer() *attentionQueueServer {
	return &attentionQueueServer{
		items:          map[int]map[string]any{},
		comments:       map[int][]map[string]any{},
		timeline:       map[int][]map[string]any{},
		reviews:        map[int][]map[string]any{},
		commentCreates: map[int]int{},
		nextID:         100,
	}
}

func (s *attentionQueueServer) openItem(n int, pr bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	it := map[string]any{"number": n, "state": "open", "user": map[string]any{"login": "carol", "type": "User"}, "labels": []any{}}
	if pr {
		it["pull_request"] = map[string]any{"url": "https://example.test/pulls/" + strconv.Itoa(n)}
	}
	s.items[n] = it
}

func (s *attentionQueueServer) closeItem(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.items[n]["state"] = "closed"
}

func (s *attentionQueueServer) queueBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.queue) == 0 {
		return ""
	}
	b, _ := s.queue[0]["body"].(string)
	return b
}

func (s *attentionQueueServer) commentBodies(n int) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, cm := range s.comments[n] {
		b, _ := cm["body"].(string)
		out = append(out, b)
	}
	return out
}

func (s *attentionQueueServer) reminders(n int) int {
	count := 0
	for _, b := range s.commentBodies(n) {
		if strings.Contains(b, "<!-- hive:attention-reminder ") {
			count++
		}
	}
	return count
}

func (s *attentionQueueServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		num := func(re *regexp.Regexp) int {
			n, _ := strconv.Atoi(re.FindStringSubmatch(p)[1])
			return n
		}
		var in map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&in)
		}
		switch {
		case strings.Contains(p, "/requested_reviewers") || (r.Method != http.MethodGet && aqReviewsRe.MatchString(p)):
			s.reviewWrites = append(s.reviewWrites, r.Method+" "+p)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && p == "/repos/o/r/issues":
			_ = json.NewEncoder(w).Encode(s.queue)
		case r.Method == http.MethodPost && p == "/repos/o/r/issues":
			s.queueCreates++
			is := map[string]any{"number": 900 + s.queueCreates, "state": "open", "title": in["title"], "body": in["body"], "labels": []any{map[string]any{"name": "origin/governor"}}, "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}}
			s.queue = append(s.queue, is)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(is)
		case aqItemRe.MatchString(p):
			n := num(aqItemRe)
			for _, is := range s.queue {
				if is["number"] == n {
					if r.Method == http.MethodPatch {
						s.queueEdits++
						is["body"] = in["body"]
					}
					_ = json.NewEncoder(w).Encode(is)
					return
				}
			}
			if it, ok := s.items[n]; ok && r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(it)
				return
			}
			w.WriteHeader(http.StatusNotFound)
		case aqCommentsRe.MatchString(p) && r.Method == http.MethodGet:
			cms := s.comments[num(aqCommentsRe)]
			if cms == nil {
				cms = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(cms)
		case aqCommentsRe.MatchString(p) && r.Method == http.MethodPost:
			n := num(aqCommentsRe)
			s.nextID++
			s.commentCreates[n]++
			cm := map[string]any{"id": s.nextID, "body": in["body"], "user": map[string]any{"login": "hive-app[bot]", "type": "Bot"}}
			s.comments[n] = append(s.comments[n], cm)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(cm)
		case r.Method == http.MethodPatch && strings.HasPrefix(p, "/repos/o/r/issues/comments/"):
			id := p[strings.LastIndex(p, "/")+1:]
			for _, cms := range s.comments {
				for _, cm := range cms {
					if jsonNumber(cm["id"]) == id {
						cm["body"] = in["body"]
					}
				}
			}
			_, _ = w.Write([]byte(`{"id":1}`))
		case aqTimelineRe.MatchString(p):
			ev := s.timeline[num(aqTimelineRe)]
			if ev == nil {
				ev = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(ev)
		case aqReviewsRe.MatchString(p):
			rv := s.reviews[num(aqReviewsRe)]
			if rv == nil {
				rv = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(rv)
		case r.Method == http.MethodPost && p == "/repos/o/r/labels":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (s *attentionQueueServer) created(n int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commentCreates[n]
}

type attentionQueueCounts struct {
	queueCreates, queueEdits int
	queueTitle               string
	reviewWrites             []string
}

func (s *attentionQueueServer) counts() attentionQueueCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := attentionQueueCounts{queueCreates: s.queueCreates, queueEdits: s.queueEdits, reviewWrites: append([]string(nil), s.reviewWrites...)}
	if len(s.queue) > 0 {
		out.queueTitle, _ = s.queue[0]["title"].(string)
	}
	return out
}

type attentionQueueFixture struct {
	s     *attentionQueueServer
	c     *Client
	store *attention.Store
	cfg   *attention.Config
	pilot *bool
}

func newAttentionQueueFixture(t *testing.T) *attentionQueueFixture {
	t.Helper()
	f := &attentionQueueFixture{s: newAttentionQueueServer(), cfg: &attention.Config{Admitter: map[string]string{"o/r": "hive-a"}}, pilot: new(bool)}
	*f.pilot = true
	f.c = testClient(t, f.s.start(t).URL)
	f.c.SetAppBotLogin("hive-app[bot]")
	f.c.SetWritebackGate(writeback.New(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: writeback.RepoModeFull}}}, writeback.Deps{}, ""))
	f.c.SetAttention("hive-a", func() attention.Config { return *f.cfg })
	f.c.SetAttentionPilot(func() bool { return *f.pilot })
	store, err := f.c.WritebackGate().AttentionStore()
	if err != nil {
		t.Fatal(err)
	}
	f.store = store
	old := attentionNow
	t.Cleanup(func() { attentionNow = old })
	return f
}

// page opens item n and pages one episode on it; it returns the episode.
func (f *attentionQueueFixture) page(t *testing.T, n int, pr bool, recipient, action string) attention.Episode {
	t.Helper()
	f.s.openItem(n, pr)
	key := attention.Key{Repo: "o/r", Item: n, Action: action, Recipient: recipient, Subject: attention.DecisionSubject("d" + strconv.Itoa(n))}
	es, err := f.store.Report(context.Background(), *f.cfg, "hive-a", "o/r", n, []attention.Observation{{Key: key, Actionable: true, Body: "x"}}, nopAttentionDelivery{})
	if err != nil || len(es) != 1 || es[0].State != attention.Notified {
		t.Fatalf("page #%d: %+v err=%v", n, es, err)
	}
	return es[0]
}

func (f *attentionQueueFixture) sweepAt(t *testing.T, at time.Time) AttentionQueueResult {
	t.Helper()
	attentionNow = func() time.Time { return at }
	res, err := f.c.SweepAttentionQueue(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("sweep at %s: %v", at, err)
	}
	return res
}

// One tracking issue per repo lists the open episodes. Creating it is the
// only notifying write; a changed list edits it in place, an unchanged one
// is not even an edit, and no comment is ever posted on it.
func TestAttentionQueueTrackingIssueCreatedOnceEditedInPlace(t *testing.T) {
	f := newAttentionQueueFixture(t)
	e7 := f.page(t, 7, false, "@alice", "security-repair")
	at := e7.PagedAt.Add(time.Hour)

	res := f.sweepAt(t, at)
	if !res.QueueCreated || res.QueueIssue != 901 || f.s.counts().queueCreates != 1 || f.s.counts().queueEdits != 0 {
		t.Fatalf("first pass: %+v creates=%d edits=%d", res, f.s.counts().queueCreates, f.s.counts().queueEdits)
	}
	body := f.s.queueBody()
	for _, want := range []string{attentionQueueMarker, "`#7`", "security-repair", "since ", "Acknowledgement is not approval"} {
		if !strings.Contains(body, want) {
			t.Fatalf("queue body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "@alice") || regexp.MustCompile(`(^|[^`+"`"+`])#7`).MatchString(body) {
		t.Fatalf("queue body mentions or cross-references:\n%s", body)
	}
	if title := f.s.counts().queueTitle; !strings.Contains(title, AttentionQueueTitle) {
		t.Fatalf("title = %q", title)
	}

	res = f.sweepAt(t, at)
	if res.QueueCreated || res.QueueEdited || f.s.counts().queueCreates != 1 || f.s.counts().queueEdits != 0 {
		t.Fatalf("unchanged pass wrote: %+v creates=%d edits=%d", res, f.s.counts().queueCreates, f.s.counts().queueEdits)
	}

	f.page(t, 8, false, "bob", "confirm-fix")
	if _, err := f.store.Apply("o/r", 8, "bob", false, attention.Command{Kind: attention.CommandSnooze, Snooze: 48 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	res = f.sweepAt(t, at)
	if res.QueueCreated || !res.QueueEdited || f.s.counts().queueCreates != 1 || f.s.counts().queueEdits != 1 {
		t.Fatalf("changed pass: %+v creates=%d edits=%d", res, f.s.counts().queueCreates, f.s.counts().queueEdits)
	}
	body = f.s.queueBody()
	if !strings.Contains(body, "`#7`") || !strings.Contains(body, "`#8`") || !strings.Contains(body, "snoozed until") {
		t.Fatalf("edited queue body:\n%s", body)
	}
	if n := f.s.created(901); n != 0 {
		t.Fatalf("%d comments on the tracking issue", n)
	}
	if f.s.created(7) != 0 || f.s.created(8) != 0 {
		t.Fatal("queue pass commented on items")
	}
}

// Closing an item resolves its open episodes, clears its Attention section
// and removes it from the tracking issue; no reminder fires for it.
func TestAttentionQueueClosingItemClearsSectionAndQueueEntry(t *testing.T) {
	f := newAttentionQueueFixture(t)
	e7 := f.page(t, 7, false, "alice", "security-repair")
	f.page(t, 8, false, "bob", "confirm-fix")
	ctx := context.Background()
	if _, err := f.c.UpsertPRStatusSection(ctx, "o/r", 7, "attention", attentionStatus(f.store.Snapshot("o/r", 7))); err != nil {
		t.Fatal(err)
	}
	if card := f.s.commentBodies(7)[0]; !strings.Contains(card, "**Attention**") {
		t.Fatalf("card has no Attention section:\n%s", card)
	}
	f.sweepAt(t, e7.PagedAt.Add(time.Hour))
	if !strings.Contains(f.s.queueBody(), "`#7`") {
		t.Fatal("queue does not list #7")
	}

	f.s.closeItem(7)
	res := f.sweepAt(t, e7.PagedAt.Add(72*time.Hour))
	if res.Resolved != 1 || !res.QueueEdited {
		t.Fatalf("closure pass: %+v", res)
	}
	if len(f.store.Snapshot("o/r", 7)) != 1 || f.store.Snapshot("o/r", 7)[0].State != attention.Resolved {
		t.Fatalf("episode not resolved: %+v", f.store.Snapshot("o/r", 7))
	}
	cards := f.s.commentBodies(7)
	if len(cards) != 1 || f.s.created(7) != 1 {
		t.Fatalf("closed item got a new comment (stale reminder?): %q", cards)
	}
	if _, ok := prstatus.Get(cards[0], "attention"); ok {
		t.Fatalf("closed item's card still has its Attention section:\n%s", cards[0])
	}
	if body := f.s.queueBody(); strings.Contains(body, "`#7`") || !strings.Contains(body, "`#8`") {
		t.Fatalf("queue after closure:\n%s", body)
	}
	// Nothing open any more: the queue says so rather than going stale.
	f.s.closeItem(8)
	f.sweepAt(t, e7.PagedAt.Add(73*time.Hour))
	if body := f.s.queueBody(); !strings.Contains(body, "No open attention episodes") {
		t.Fatalf("empty queue:\n%s", body)
	}
	if f.s.counts().queueCreates != 1 {
		t.Fatalf("queue created %d times", f.s.counts().queueCreates)
	}
}

// Pilot off, or another hive admits the repo: the pass makes no request.
func TestAttentionQueueGatedByPilotAndAdmitter(t *testing.T) {
	for name, set := range map[string]func(*attentionQueueFixture){
		"pilot-off":     func(f *attentionQueueFixture) { *f.pilot = false },
		"other-hive":    func(f *attentionQueueFixture) { f.cfg.Admitter = map[string]string{"o/r": "hive-b"} },
		"no-attention":  func(f *attentionQueueFixture) { f.c.SetAttention("", nil) },
		"repo-unlisted": func(f *attentionQueueFixture) { f.cfg.Admitter = map[string]string{"o/other": "hive-a"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newAttentionQueueFixture(t)
			e := f.page(t, 7, false, "alice", "security-repair")
			set(f)
			res := f.sweepAt(t, e.PagedAt.Add(72*time.Hour))
			if res != (AttentionQueueResult{}) || f.s.counts().queueCreates != 0 || len(f.s.commentBodies(7)) != 0 {
				t.Fatalf("gated pass wrote: %+v creates=%d comments=%v", res, f.s.counts().queueCreates, f.s.commentBodies(7))
			}
		})
	}
}

// At most one reminder per episode per 24h by default: none before the
// interval, one at it, none again until the next interval. Each reminder is
// one comment through CreateIssueComment (counted by the shadow
// instrumentation under remindAttention), refreshes the Attention section,
// and never touches reviewers.
func TestAttentionReminderDefault24h(t *testing.T) {
	f := newAttentionQueueFixture(t)
	f.c.SetNotificationsShadow(func() bool { return true })
	e := f.page(t, 9, true, "@alice", "review")
	t0 := e.PagedAt

	if res := f.sweepAt(t, t0.Add(23*time.Hour)); res.Reminders != 0 {
		t.Fatalf("reminder before 24h: %+v", res)
	}
	if res := f.sweepAt(t, t0.Add(24*time.Hour)); res.Reminders != 1 {
		t.Fatalf("no reminder at 24h: %+v", res)
	}
	if res := f.sweepAt(t, t0.Add(47*time.Hour)); res.Reminders != 0 {
		t.Fatalf("second reminder inside 24h: %+v", res)
	}
	if res := f.sweepAt(t, t0.Add(48*time.Hour)); res.Reminders != 1 {
		t.Fatalf("no reminder 24h after the first: %+v", res)
	}
	var reminders []string
	card := ""
	for _, b := range f.s.commentBodies(9) {
		if strings.Contains(b, "<!-- hive:attention-reminder ") {
			reminders = append(reminders, b)
		}
		if prstatus.IsStatusComment(b) {
			card = b
		}
	}
	if len(reminders) != 2 || !strings.Contains(reminders[0], "@alice reminder 1") || !strings.Contains(reminders[1], "@alice reminder 2") || !strings.Contains(reminders[1], attention.NotApproval) {
		t.Fatalf("reminders: %q", reminders)
	}
	if !strings.Contains(card, "reminded 2×") {
		t.Fatalf("card not refreshed:\n%s", card)
	}
	if len(f.s.counts().reviewWrites) != 0 {
		t.Fatalf("reminder touched reviewers: %v", f.s.counts().reviewWrites)
	}
	counted := 0
	for _, rec := range f.c.NotificationShadowRecords() {
		if rec.Producer == "github.(*Client).remindAttention" && rec.Class == NotificationClassAttention && rec.Item == 9 {
			counted++
		}
	}
	if counted != 2 {
		t.Fatalf("shadow counted %d reminders, want 2: %+v", counted, f.c.NotificationShadowRecords())
	}
}

func TestAttentionReminderIntervalConfigurable(t *testing.T) {
	f := newAttentionQueueFixture(t)
	f.cfg.ReminderInterval = 2 * time.Hour
	e := f.page(t, 7, false, "alice", "security-repair")
	if res := f.sweepAt(t, e.PagedAt.Add(time.Hour)); res.Reminders != 0 {
		t.Fatalf("reminder before the configured interval: %+v", res)
	}
	if res := f.sweepAt(t, e.PagedAt.Add(2*time.Hour)); res.Reminders != 1 {
		t.Fatalf("no reminder at the configured interval: %+v", res)
	}
}

// No reminder while acknowledged or snoozed; an expired snooze resumes them.
func TestAttentionReminderSkippedWhileAckedOrSnoozed(t *testing.T) {
	f := newAttentionQueueFixture(t)
	e7 := f.page(t, 7, false, "alice", "security-repair")
	f.page(t, 8, false, "bob", "confirm-fix")
	if _, err := f.store.Apply("o/r", 7, "alice", false, attention.Command{Kind: attention.CommandAck}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Apply("o/r", 8, "bob", false, attention.Command{Kind: attention.CommandSnooze, Snooze: 4 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	if res := f.sweepAt(t, e7.PagedAt.Add(72*time.Hour)); res.Reminders != 0 {
		t.Fatalf("reminded an acked/snoozed episode: %+v", res)
	}
	if f.s.created(7) != 0 || f.s.created(8) != 0 {
		t.Fatal("acked/snoozed items got comments")
	}
	if res := f.sweepAt(t, e7.PagedAt.Add(5*24*time.Hour)); res.Reminders != 1 || f.s.reminders(8) != 1 || f.s.created(7) != 0 {
		t.Fatalf("after snooze expiry: %+v", res)
	}
}

// A comment, label change or review by the recipient since the last ping
// skips the reminder and restarts the interval from that action; another
// person's activity does not.
func TestAttentionReminderSkippedAfterRecipientAction(t *testing.T) {
	f := newAttentionQueueFixture(t)
	e7 := f.page(t, 7, false, "@alice", "security-repair")
	f.page(t, 8, false, "alice", "triage")
	f.page(t, 9, true, "alice", "review")
	f.page(t, 10, false, "alice", "decide")
	t0 := e7.PagedAt
	ts := func(d time.Duration) string { return t0.Add(d).UTC().Format(time.RFC3339) }
	f.s.mu.Lock()
	f.s.timeline[7] = []map[string]any{{"event": "commented", "actor": map[string]any{"login": "Alice", "type": "User"}, "created_at": ts(2 * time.Hour)}}
	f.s.timeline[8] = []map[string]any{{"event": "labeled", "actor": map[string]any{"login": "alice", "type": "User"}, "label": map[string]any{"name": "triage/accepted"}, "created_at": ts(3 * time.Hour)}}
	f.s.reviews[9] = []map[string]any{{"id": 1, "state": "COMMENTED", "user": map[string]any{"login": "alice"}, "submitted_at": ts(4 * time.Hour)}}
	f.s.timeline[10] = []map[string]any{{"event": "commented", "actor": map[string]any{"login": "bob", "type": "User"}, "created_at": ts(2 * time.Hour)}}
	f.s.mu.Unlock()

	res := f.sweepAt(t, t0.Add(25*time.Hour))
	if res.Reminders != 1 || f.s.reminders(10) != 1 {
		t.Fatalf("want only #10 (someone else acted) reminded: %+v", res)
	}
	for _, n := range []int{7, 8, 9} {
		if f.s.created(n) != 0 {
			t.Fatalf("#%d reminded after the recipient acted", n)
		}
	}
	if got := f.store.Snapshot("o/r", 7)[0].RecipientActedAt; !got.Equal(t0.Add(2 * time.Hour).Truncate(time.Second)) {
		t.Fatalf("recipient action not recorded: %s", got)
	}
	// The interval restarts from the recipient's action.
	if res := f.sweepAt(t, t0.Add(25*time.Hour+59*time.Minute)); res.Reminders != 0 {
		t.Fatalf("reminded inside the interval after the action: %+v", res)
	}
	if res := f.sweepAt(t, t0.Add(26*time.Hour)); f.s.reminders(7) != 1 {
		t.Fatalf("no reminder 24h after the recipient's action: %+v", res)
	}
	if len(f.s.counts().reviewWrites) != 0 {
		t.Fatalf("reminder touched reviewers: %v", f.s.counts().reviewWrites)
	}
}

// A paused repo refuses the reminder before anything is written; it is
// retried on a later pass.
func TestAttentionReminderRespectsWritebackGate(t *testing.T) {
	f := newAttentionQueueFixture(t)
	e := f.page(t, 7, false, "alice", "security-repair")
	f.c.WritebackGate().SetConfig(writeback.Config{PerRepo: map[string]writeback.RepoOverride{"o/r": {Mode: writeback.RepoModeOff}}})
	attentionNow = func() time.Time { return e.PagedAt.Add(25 * time.Hour) }
	res, err := f.c.SweepAttentionQueue(context.Background(), "o/r")
	if res.Reminders != 0 || res.Suppressed != 1 || f.s.created(7) != 0 || f.s.counts().queueCreates != 0 {
		t.Fatalf("refused pass wrote: %+v err=%v", res, err)
	}
	if f.store.Snapshot("o/r", 7)[0].Reminders != 0 {
		t.Fatal("refused reminder recorded as sent")
	}
}
