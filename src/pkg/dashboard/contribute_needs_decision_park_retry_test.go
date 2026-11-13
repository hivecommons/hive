package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// A contributor relay parked a person's issue at 045. Eleven seconds later
// the hub's one attempt to post the park question was refused by GitHub for
// quota ("403 API rate limit … still exceeded"); the hub logged a warning
// and never tried again, so the issue sat parked with no question from the
// hive. These tests drive the retry with an injected clock and scheduler.

var parkRetryStart = time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)

// parkRetryClock is the injected clock and scheduler: advance moves the
// clock and runs every timer that has come due, in the test's goroutine.
type parkRetryClock struct {
	now    time.Time
	timers []parkRetryTimer
}

type parkRetryTimer struct {
	at time.Time
	f  func()
}

func (c *parkRetryClock) after(d time.Duration, f func()) {
	c.timers = append(c.timers, parkRetryTimer{at: c.now.Add(d), f: f})
}

// advance moves the clock by d and reports how many timers ran.
func (c *parkRetryClock) advance(d time.Duration) int {
	c.now = c.now.Add(d)
	ran := 0
	for {
		due := -1
		for i, tm := range c.timers {
			if !tm.at.After(c.now) {
				due = i
				break
			}
		}
		if due < 0 {
			return ran
		}
		tm := c.timers[due]
		c.timers = append(c.timers[:due], c.timers[due+1:]...)
		tm.f()
		ran++
	}
}

func parkRetryResponse(method string, status int, header http.Header) *http.Response {
	if header == nil {
		header = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: header, Request: &http.Request{Method: method, URL: &url.URL{Path: "/repos/acme/widgets/issues/77/comments"}}}
}

// parkPrimaryLimit is GitHub's primary rate limit, lifting at reset.
func parkPrimaryLimit(reset time.Time) error {
	return fmt.Errorf("listing comments: %w", &gh.RateLimitError{
		Rate:     gh.Rate{Limit: 5000, Reset: gh.Timestamp{Time: reset}},
		Response: parkRetryResponse(http.MethodGet, http.StatusForbidden, nil),
		Message:  "API rate limit of 5000 still exceeded",
	})
}

// parkSecondaryLimit is GitHub's secondary rate limit; retryAfter 0 is one
// that names no time.
func parkSecondaryLimit(retryAfter time.Duration) error {
	e := &gh.AbuseRateLimitError{Response: parkRetryResponse(http.MethodPost, http.StatusForbidden, nil), Message: "You have exceeded a secondary rate limit"}
	if retryAfter > 0 {
		e.RetryAfter = &retryAfter
	}
	return e
}

// parkRetryHarness is a hub whose park poster, still-parked read, clock and
// scheduler are the test's. errs are what the poster returns, call by call;
// once they run out it posts.
type parkRetryHarness struct {
	hub   *ContributeWSHub
	clock *parkRetryClock
	logs  *lockedBuffer

	errs   func(call int) error
	calls  []string // one "repo#number since body" per poster call
	posted int

	parked     bool
	parkedErr  error
	parkChecks int
}

const (
	parkRetryRepo   = "acme/widgets"
	parkRetryNumber = 77
)

var parkRetryAssigned = parkRetryStart.Add(-10 * time.Minute)

func newParkRetryHarness(t *testing.T) *parkRetryHarness {
	t.Helper()
	hub, _ := covK2Hub(t)
	h := &parkRetryHarness{hub: hub, clock: &parkRetryClock{now: parkRetryStart}, logs: &lockedBuffer{}, parked: true}
	hub.logger = slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hub.needsDecisionParkRetry.after = h.clock.after
	hub.needsDecisionParkRetry.now = func() time.Time { return h.clock.now }
	hub.needsDecisionParkRetry.stillParked = func(context.Context, string, int) (bool, error) {
		h.parkChecks++
		return h.parked, h.parkedErr
	}
	hub.needsDecisionParkPoster = func(_ context.Context, repo string, number int, since time.Time, body string) error {
		call := len(h.calls)
		h.calls = append(h.calls, fmt.Sprintf("%s#%d %s %s", repo, number, since.UTC().Format(time.RFC3339), body))
		if h.errs != nil {
			if err := h.errs(call); err != nil {
				return err
			}
		}
		h.posted++
		return nil
	}
	return h
}

// park runs the hub's first attempt, as the task_complete handler does.
func (h *parkRetryHarness) park() {
	h.hub.postNeedsDecisionPark(&WSTaskAssign{Kind: "issue", Repo: parkRetryRepo, Number: parkRetryNumber}, "t", "alice", "pick the queue owner", parkRetryAssigned)
}

// logLines counts the park log lines at level that contain want.
func (h *parkRetryHarness) logLines(level, want string) int {
	n := 0
	for _, line := range strings.Split(h.logs.String(), "\n") {
		if strings.Contains(line, "needs-decision park") && strings.Contains(line, "level="+level+" ") && strings.Contains(line, want) {
			n++
		}
	}
	return n
}

const (
	parkRetryHeld     = "the question will be tried again"
	parkRetrySettled  = "question settled on retry"
	parkRetryGaveUp   = "the question was not posted"
	parkRetryNoRetry  = "could not post the question"
	parkRetryUnparked = "no longer open with the park label"
)

// parkFailFirst is the errs function that fails the first call only.
func parkFailFirst(err error) func(int) error {
	return func(call int) error {
		if call == 0 {
			return err
		}
		return nil
	}
}

// A park question refused for a rate limit is posted once the clock passes
// the time GitHub named: the reset of a primary limit, the Retry-After of a
// secondary one, and a minute when neither is named or the hive's own pacer
// shed the request. Nothing runs before that time.
func TestNeedsDecisionParkRetriesAfterRateLimit(t *testing.T) {
	tooMany := func(header http.Header) error {
		return &gh.ErrorResponse{Response: parkRetryResponse(http.MethodPost, http.StatusTooManyRequests, header), Message: "too many requests"}
	}
	for _, tc := range []struct {
		name string
		err  error
		wait time.Duration
	}{
		{"primary limit: after its reset", parkPrimaryLimit(parkRetryStart.Add(20 * time.Minute)), 20*time.Minute + needsDecisionParkRetryMargin},
		{"secondary limit: after its retry-after", parkSecondaryLimit(90 * time.Second), 90*time.Second + needsDecisionParkRetryMargin},
		{"secondary limit naming no time: after a minute", parkSecondaryLimit(0), needsDecisionParkRetryFallback + needsDecisionParkRetryMargin},
		{"429 with Retry-After", tooMany(http.Header{"Retry-After": []string{"120"}}), 2*time.Minute + needsDecisionParkRetryMargin},
		{"bare 429", tooMany(nil), needsDecisionParkRetryFallback + needsDecisionParkRetryMargin},
		{"request shed by the hive's pacer", fmt.Errorf("%w (class normal, queue 31s ahead, limit 30s)", ghpkg.ErrPacingShed), needsDecisionParkRetryFallback + needsDecisionParkRetryMargin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newParkRetryHarness(t)
			h.errs = parkFailFirst(tc.err)
			h.park()
			if len(h.calls) != 1 || h.posted != 0 || len(h.clock.timers) != 1 {
				t.Fatalf("after the refused attempt: %d calls, %d posted, %d timers; want one call and one retry waiting", len(h.calls), h.posted, len(h.clock.timers))
			}
			if h.logLines("INFO", parkRetryHeld) != 1 || h.logLines("WARN", "") != 0 {
				t.Fatalf("a question that will be retried is one info line and no warning:\n%s", h.logs.String())
			}
			if ran := h.clock.advance(tc.wait - time.Second); ran != 0 || len(h.calls) != 1 {
				t.Fatalf("retried %s early (%d timers ran, %d calls)", time.Second, ran, len(h.calls))
			}
			if ran := h.clock.advance(time.Second); ran != 1 || len(h.calls) != 2 || h.posted != 1 {
				t.Fatalf("after %s: %d timers ran, %d calls, %d posted; want the question posted on the retry", tc.wait, ran, len(h.calls), h.posted)
			}
			if h.calls[1] != h.calls[0] {
				t.Fatalf("the retry must post the same question to the same issue:\n first %q\n retry %q", h.calls[0], h.calls[1])
			}
			if h.parkChecks != 1 {
				t.Fatalf("the retry must check once that the issue is still parked, checked %d times", h.parkChecks)
			}
			if h.logLines("INFO", parkRetrySettled) != 1 || h.logLines("WARN", "") != 0 {
				t.Fatalf("want one settled line and no warning:\n%s", h.logs.String())
			}
			if got := h.hub.needsDecisionParkRetry.pending.Load(); got != 0 || len(h.clock.timers) != 0 {
				t.Fatalf("nothing may be left waiting: pending=%d timers=%d", got, len(h.clock.timers))
			}
		})
	}
}

// An error that is not a rate limit is not retried: one warning, as before.
func TestNeedsDecisionParkDoesNotRetryOtherErrors(t *testing.T) {
	notFound := &gh.ErrorResponse{Response: parkRetryResponse(http.MethodGet, http.StatusNotFound, nil), Message: "Not Found"}
	forbidden := &gh.ErrorResponse{Response: parkRetryResponse(http.MethodPost, http.StatusForbidden, nil), Message: "Resource not accessible by integration"}
	for name, err := range map[string]error{
		"plain error":          errors.New("connection reset by peer"),
		"404":                  notFound,
		"permission 403":       forbidden,
		"deadline exceeded":    context.DeadlineExceeded,
		"no GitHub client":     ghpkg.ErrNoGitHubClient,
		"comment budget spent": ghpkg.ErrIssueCommentBudget,
	} {
		t.Run(name, func(t *testing.T) {
			h := newParkRetryHarness(t)
			h.errs = func(int) error { return err }
			h.park()
			if len(h.calls) != 1 || len(h.clock.timers) != 0 || h.hub.needsDecisionParkRetry.pending.Load() != 0 {
				t.Fatalf("%d calls, %d timers; want one attempt and no retry", len(h.calls), len(h.clock.timers))
			}
			if h.logLines("WARN", parkRetryNoRetry) != 1 || h.logLines("WARN", "") != 1 || h.logLines("INFO", "") != 0 {
				t.Fatalf("want exactly the one warning the hub logged before:\n%s", h.logs.String())
			}
			if ran := h.clock.advance(24 * time.Hour); ran != 0 || len(h.calls) != 1 {
				t.Fatalf("retried later: %d timers ran, %d calls", ran, len(h.calls))
			}
		})
	}
}

// The attempts are bounded. A limit that never lifts gets the first attempt
// and needsDecisionParkMaxRetries more, then one warning that the question
// was not posted, and nothing after that.
func TestNeedsDecisionParkRetryGivesUpOnce(t *testing.T) {
	h := newParkRetryHarness(t)
	h.errs = func(int) error { return parkPrimaryLimit(h.clock.now.Add(10 * time.Minute)) }
	h.park()
	for retry := 1; retry <= needsDecisionParkMaxRetries; retry++ {
		if ran := h.clock.advance(10*time.Minute + needsDecisionParkRetryMargin); ran != 1 || len(h.calls) != retry+1 {
			t.Fatalf("retry %d: %d timers ran, %d calls", retry, ran, len(h.calls))
		}
	}
	if want := needsDecisionParkMaxRetries + 1; len(h.calls) != want || h.posted != 0 {
		t.Fatalf("%d attempts, %d posted; want %d attempts and no post", len(h.calls), h.posted, want)
	}
	if len(h.clock.timers) != 0 || h.hub.needsDecisionParkRetry.pending.Load() != 0 {
		t.Fatalf("a retry is still waiting after the last attempt: timers=%d", len(h.clock.timers))
	}
	if h.logLines("WARN", parkRetryGaveUp) != 1 || h.logLines("WARN", "") != 1 {
		t.Fatalf("want exactly one warning, saying the question was not posted:\n%s", h.logs.String())
	}
	if !strings.Contains(h.logs.String(), fmt.Sprintf("attempts=%d", needsDecisionParkMaxRetries+1)) {
		t.Fatalf("the warning must say how many attempts were made:\n%s", h.logs.String())
	}
	if h.logLines("INFO", parkRetryHeld) != needsDecisionParkMaxRetries {
		t.Fatalf("want one retry line per retry:\n%s", h.logs.String())
	}
	if ran := h.clock.advance(7 * 24 * time.Hour); ran != 0 || len(h.calls) != needsDecisionParkMaxRetries+1 {
		t.Fatalf("something ran after the hub gave up: %d timers, %d calls", ran, len(h.calls))
	}
}

// A retry runs long after the park. An issue that is no longer open with
// the park label gets no late "parked for a maintainer decision" comment; a
// rate-limited check is one more attempt; any other failed check stops.
func TestNeedsDecisionParkRetryChecksTheIssueIsStillParked(t *testing.T) {
	wait := time.Minute + needsDecisionParkRetryMargin
	t.Run("un-parked meanwhile", func(t *testing.T) {
		h := newParkRetryHarness(t)
		h.errs = parkFailFirst(parkSecondaryLimit(time.Minute))
		h.park()
		h.parked = false
		if ran := h.clock.advance(wait); ran != 1 || len(h.calls) != 1 || h.posted != 0 {
			t.Fatalf("%d timers ran, %d calls, %d posted; want the retry to post nothing", ran, len(h.calls), h.posted)
		}
		if h.logLines("INFO", parkRetryUnparked) != 1 || h.logLines("WARN", "") != 0 || len(h.clock.timers) != 0 {
			t.Fatalf("want one info line and nothing waiting:\n%s", h.logs.String())
		}
	})
	t.Run("the check is rate limited too", func(t *testing.T) {
		h := newParkRetryHarness(t)
		h.errs = parkFailFirst(parkSecondaryLimit(time.Minute))
		h.park()
		h.parkedErr = parkSecondaryLimit(time.Minute)
		if ran := h.clock.advance(wait); ran != 1 || len(h.calls) != 1 || len(h.clock.timers) != 1 {
			t.Fatalf("%d timers ran, %d calls, %d waiting; want the question kept for another retry", ran, len(h.calls), len(h.clock.timers))
		}
		h.parkedErr = nil
		if ran := h.clock.advance(wait); ran != 1 || len(h.calls) != 2 || h.posted != 1 {
			t.Fatalf("%d timers ran, %d calls, %d posted; want the question posted on the second retry", ran, len(h.calls), h.posted)
		}
	})
	t.Run("the check fails for another reason", func(t *testing.T) {
		h := newParkRetryHarness(t)
		h.errs = parkFailFirst(parkSecondaryLimit(time.Minute))
		h.park()
		h.parkedErr = errors.New("connection reset by peer")
		if ran := h.clock.advance(wait); ran != 1 || len(h.calls) != 1 || len(h.clock.timers) != 0 {
			t.Fatalf("%d timers ran, %d calls, %d waiting; want no post and no further retry", ran, len(h.calls), len(h.clock.timers))
		}
		if h.logLines("WARN", parkRetryNoRetry) != 1 || h.logLines("WARN", "") != 1 {
			t.Fatalf("want the one warning:\n%s", h.logs.String())
		}
	})
}

// The questions waiting for a retry are bounded; one more is reported not
// posted instead of being kept. A hub that closed while a question waited
// posts nothing.
func TestNeedsDecisionParkRetryWaitingIsBoundedAndStopsWithTheHub(t *testing.T) {
	t.Run("bounded", func(t *testing.T) {
		h := newParkRetryHarness(t)
		h.errs = func(int) error { return parkSecondaryLimit(time.Minute) }
		h.hub.needsDecisionParkRetry.pending.Store(needsDecisionParkMaxPending)
		h.park()
		if len(h.clock.timers) != 0 || h.hub.needsDecisionParkRetry.pending.Load() != needsDecisionParkMaxPending {
			t.Fatalf("a question was kept past the bound: timers=%d pending=%d", len(h.clock.timers), h.hub.needsDecisionParkRetry.pending.Load())
		}
		if h.logLines("WARN", parkRetryGaveUp) != 1 || h.logLines("WARN", "") != 1 {
			t.Fatalf("want one warning that the question was not posted:\n%s", h.logs.String())
		}
	})
	t.Run("hub closed", func(t *testing.T) {
		h := newParkRetryHarness(t)
		h.errs = parkFailFirst(parkSecondaryLimit(time.Minute))
		h.park()
		h.hub.Close()
		if ran := h.clock.advance(time.Hour); ran != 1 || len(h.calls) != 1 || h.parkChecks != 0 {
			t.Fatalf("%d timers ran, %d calls, %d checks; a closed hub must not post", ran, len(h.calls), h.parkChecks)
		}
	})
}

func TestNeedsDecisionParkRetryDelay(t *testing.T) {
	now := parkRetryStart
	for _, tc := range []struct {
		name    string
		err     error
		want    time.Duration
		limited bool
	}{
		{"primary", parkPrimaryLimit(now.Add(22 * time.Minute)), 22*time.Minute + needsDecisionParkRetryMargin, true},
		{"primary resetting more than an hour out is capped", parkPrimaryLimit(now.Add(3 * time.Hour)), time.Hour + needsDecisionParkRetryMargin, true},
		{"primary whose reset has passed", parkPrimaryLimit(now.Add(-time.Minute)), needsDecisionParkRetryFallback + needsDecisionParkRetryMargin, true},
		{"secondary", parkSecondaryLimit(75 * time.Second), 75*time.Second + needsDecisionParkRetryMargin, true},
		{"pacer shed", ghpkg.ErrPacingShed, needsDecisionParkRetryFallback + needsDecisionParkRetryMargin, true},
		{"other error", errors.New("boom"), 0, false},
		{"permission 403", &gh.ErrorResponse{Response: parkRetryResponse(http.MethodPost, http.StatusForbidden, nil)}, 0, false},
		{"nil", nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, limited := needsDecisionParkRetryDelay(tc.err, now)
			if got != tc.want || limited != tc.limited {
				t.Fatalf("needsDecisionParkRetryDelay = %s, %v; want %s, %v", got, limited, tc.want, tc.limited)
			}
		})
	}
}

// parkRetryGitHub serves issue #77 of acme/widgets, its comments, and
// records the reads and any comment the hub posts.
type parkRetryGitHub struct {
	mu       sync.Mutex
	state    string
	labels   []string
	comments []map[string]any
	gets     int
	lists    int
	posted   []string
}

func (g *parkRetryGitHub) client(t *testing.T) *ghpkg.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.mu.Lock()
		defer g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/77":
			g.gets++
			labels := []map[string]any{}
			for _, name := range g.labels {
				labels = append(labels, map[string]any{"name": name})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 77, "state": g.state, "labels": labels})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widgets/issues/77/comments":
			g.lists++
			_ = json.NewEncoder(w).Encode(g.comments)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widgets/issues/77/comments":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			g.posted = append(g.posted, body.Body)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return ghpkg.NewClientForTest(srv.URL, "acme", []string{"widgets"}, covBLogger())
}

func (g *parkRetryGitHub) counts() (gets, lists, posted int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.gets, g.lists, len(g.posted)
}

// newParkRetryGitHubHarness is a harness whose retry runs the hub's real
// GitHub path against g: only the first attempt is refused, by the seam.
func newParkRetryGitHubHarness(t *testing.T, g *parkRetryGitHub) *parkRetryHarness {
	t.Helper()
	h := newParkRetryHarness(t)
	h.hub.server.deps.GHClient = g.client(t)
	h.hub.needsDecisionParkRetry.stillParked = nil
	h.hub.needsDecisionParkPoster = func(context.Context, string, int, time.Time, string) error {
		h.hub.needsDecisionParkPoster = nil
		return parkSecondaryLimit(time.Minute)
	}
	return h
}

// The post stays idempotent across the wait: a retry that finds the park
// explained by someone meanwhile posts nothing, and one that finds no
// explanation posts the question.
func TestNeedsDecisionParkRetryPostsOnlyWhenTheParkIsStillUnexplained(t *testing.T) {
	wait := time.Minute + needsDecisionParkRetryMargin
	explained := map[string]any{"id": 3, "body": "Parking: the owner must choose who admits pull requests to the merge queue. Recommended: A.", "created_at": parkRetryAssigned.Add(2 * time.Minute).Format(time.RFC3339)}
	for _, tc := range []struct {
		name     string
		comments []map[string]any
		posts    int
	}{
		{"explained meanwhile: nothing is posted", []map[string]any{explained}, 0},
		{"still unexplained: the question is posted", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &parkRetryGitHub{state: "open", comments: tc.comments}
			h := newParkRetryGitHubHarness(t, g)
			g.labels = []string{h.hub.configuredNeedsDecisionLabel()}
			h.park()
			if gets, lists, posted := g.counts(); gets+lists+posted != 0 || len(h.clock.timers) != 1 {
				t.Fatalf("before the retry: %d gets, %d lists, %d posts, %d timers", gets, lists, posted, len(h.clock.timers))
			}
			if ran := h.clock.advance(wait); ran != 1 {
				t.Fatalf("%d timers ran, want the one retry", ran)
			}
			gets, lists, posted := g.counts()
			if gets != 1 || lists != 1 || posted != tc.posts {
				t.Fatalf("retry made %d issue reads, %d comment listings and %d posts; want 1, 1 and %d", gets, lists, posted, tc.posts)
			}
			if tc.posts == 1 && !strings.Contains(g.posted[0], "**Question:** pick the queue owner") {
				t.Fatalf("posted the wrong body:\n%s", g.posted[0])
			}
			if h.logLines("INFO", parkRetrySettled) != 1 || h.logLines("WARN", "") != 0 || len(h.clock.timers) != 0 {
				t.Fatalf("want one settled line, no warning and nothing waiting:\n%s", h.logs.String())
			}
		})
	}
}

// The still-parked read: open with the park label is parked; closed, or
// open without the label, is not. A hive configured to apply no label can
// only check that the issue is open.
func TestNeedsDecisionParkStillParkedRead(t *testing.T) {
	hub, _ := covK2Hub(t)
	label := hub.configuredNeedsDecisionLabel()
	if label == "" {
		t.Fatal("the test hub must have a decision label configured")
	}
	for _, tc := range []struct {
		name   string
		state  string
		labels []string
		want   bool
	}{
		{"open with the park label", "open", []string{"bug", strings.ToUpper(label)}, true},
		{"open, label removed", "open", []string{"bug"}, false},
		{"closed with the park label", "closed", []string{label}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &parkRetryGitHub{state: tc.state, labels: tc.labels}
			hub.server.deps.GHClient = g.client(t)
			got, err := hub.needsDecisionParkStillParked(context.Background(), parkRetryRepo, parkRetryNumber)
			if err != nil || got != tc.want {
				t.Fatalf("still parked = %v, %v; want %v", got, err, tc.want)
			}
		})
	}

	empty := ""
	hub.server.deps.Config.Hub.ContributeNeedsDecisionLabel = &empty
	hub.server.deps.GHClient = (&parkRetryGitHub{state: "open"}).client(t)
	if got, err := hub.needsDecisionParkStillParked(context.Background(), parkRetryRepo, parkRetryNumber); err != nil || !got {
		t.Fatalf("no label configured, open issue: still parked = %v, %v; want true", got, err)
	}

	if _, err := hub.needsDecisionParkStillParked(context.Background(), "widgets", parkRetryNumber); err == nil {
		t.Fatal("a repo that is not owner/name must be an error")
	}
	hub.server.deps.GHClient = nil
	if _, err := hub.needsDecisionParkStillParked(context.Background(), parkRetryRepo, parkRetryNumber); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("no client: err = %v, want ErrNoGitHubClient", err)
	}
}

// A hive whose relay applies a legacy decision label has it replaced with
// stage/045-needs-decision by the stage reconciler. A retry that runs after
// that still finds the park and posts the question; an issue with neither
// label is no longer parked.
func TestNeedsDecisionParkRetryFindsAParkTheReconcilerRelabelled(t *testing.T) {
	wait := time.Minute + needsDecisionParkRetryMargin
	for _, tc := range []struct {
		name   string
		labels []string
		posts  int
	}{
		{"the stage label in place of the configured one", []string{"bug", strings.ToUpper(ghpkg.NeedsDecisionStageLabel)}, 1},
		{"the configured label still on the issue", []string{"needs-decision"}, 1},
		{"neither label", []string{"bug"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &parkRetryGitHub{state: "open", labels: tc.labels}
			h := newParkRetryGitHubHarness(t, g)
			legacy := "needs-decision"
			h.hub.server.deps.Config.Hub.ContributeNeedsDecisionLabel = &legacy
			if got := h.hub.configuredNeedsDecisionLabel(); got != legacy || strings.EqualFold(got, ghpkg.NeedsDecisionStageLabel) {
				t.Fatalf("the test hub's decision label is %q, want the legacy spelling", got)
			}
			h.park()
			if ran := h.clock.advance(wait); ran != 1 {
				t.Fatalf("%d timers ran, want the one retry", ran)
			}
			gets, lists, posted := g.counts()
			if gets != 1 || lists != tc.posts || posted != tc.posts {
				t.Fatalf("retry made %d issue reads, %d comment listings and %d posts; want 1, %d and %d", gets, lists, posted, tc.posts, tc.posts)
			}
			if want := 1 - tc.posts; h.logLines("INFO", parkRetryUnparked) != want || h.logLines("INFO", parkRetrySettled) != tc.posts || h.logLines("WARN", "") != 0 {
				t.Fatalf("want %d not-parked and %d settled lines and no warning:\n%s", want, tc.posts, h.logs.String())
			}
		})
	}
}
