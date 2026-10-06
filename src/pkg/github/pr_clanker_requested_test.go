package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

const clankerTestAppBot = "hive-app[bot]"

type clankerTestPR struct {
	number      int
	login       string
	userType    string
	association string
	labels      []string
}

type clankerTestComment struct {
	login string
	body  string
}

type clankerTestEvent struct {
	event string
	label string
	actor string
}

// clankerPRHarness is a stateful fake of the endpoints the clanker-requested
// parking touches: the open-PR list, comments, issue events and labels.
type clankerPRHarness struct {
	t        *testing.T
	mu       sync.Mutex
	prs      []clankerTestPR
	labels   map[int]map[string]bool
	comments map[int][]clankerTestComment
	events   map[int][]clankerTestEvent
	fail     map[string]bool
	calls    []string
}

func newClankerPRHarness(t *testing.T, prs ...clankerTestPR) *clankerPRHarness {
	t.Helper()
	h := &clankerPRHarness{
		t:        t,
		prs:      prs,
		labels:   map[int]map[string]bool{},
		comments: map[int][]clankerTestComment{},
		events:   map[int][]clankerTestEvent{},
		fail:     map[string]bool{},
	}
	for _, pr := range prs {
		h.labels[pr.number] = map[string]bool{}
		for _, l := range pr.labels {
			h.labels[pr.number][l] = true
		}
	}
	return h
}

func (h *clankerPRHarness) record(op string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, op)
	return h.fail[op]
}

func (h *clankerPRHarness) called(op string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == op {
			n++
		}
	}
	return n
}

func (h *clankerPRHarness) hasLabel(number int, label string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.labels[number][label]
}

func (h *clankerPRHarness) commentCount(number int) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.comments[number])
}

// humanRemove simulates a maintainer removing the label in the GitHub UI.
func (h *clankerPRHarness) humanRemove(number int, label, actor string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.labels[number], label)
	h.events[number] = append(h.events[number], clankerTestEvent{event: "unlabeled", label: label, actor: actor})
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func failJSON(w http.ResponseWriter) {
	writeJSONStatus(w, http.StatusInternalServerError, map[string]string{"message": "nope"})
}

func (h *clankerPRHarness) number(r *http.Request) int {
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		h.t.Errorf("bad issue number %q", r.PathValue("n"))
	}
	return n
}

func (h *clankerPRHarness) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues", func(w http.ResponseWriter, _ *http.Request) {
		writeJSONStatus(w, http.StatusOK, []any{})
	})
	mux.HandleFunc("GET /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") == "closed" {
			writeJSONStatus(w, http.StatusOK, []any{})
			return
		}
		h.mu.Lock()
		out := make([]map[string]any, 0, len(h.prs))
		for _, pr := range h.prs {
			labels := []map[string]string{}
			for l := range h.labels[pr.number] {
				labels = append(labels, map[string]string{"name": l})
			}
			out = append(out, map[string]any{
				"number": pr.number, "title": "change " + strconv.Itoa(pr.number), "state": "open",
				"user":               map[string]string{"login": pr.login, "type": pr.userType},
				"author_association": pr.association, "labels": labels, "created_at": hoursAgo(1),
			})
		}
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusOK, out)
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		if h.record("list-comments") {
			failJSON(w)
			return
		}
		n := h.number(r)
		h.mu.Lock()
		out := []map[string]any{}
		for _, c := range h.comments[n] {
			out = append(out, map[string]any{"body": c.body, "user": map[string]string{"login": c.login}})
		}
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /repos/o/r/issues/{n}/comments", func(w http.ResponseWriter, r *http.Request) {
		if h.record("post-comment") {
			failJSON(w)
			return
		}
		var req struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.t.Errorf("decode comment: %v", err)
		}
		n := h.number(r)
		h.mu.Lock()
		h.comments[n] = append(h.comments[n], clankerTestComment{login: clankerTestAppBot, body: req.Body})
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusCreated, map[string]string{"html_url": "https://example.test/c"})
	})
	mux.HandleFunc("GET /repos/o/r/issues/{n}/events", func(w http.ResponseWriter, r *http.Request) {
		if h.record("list-events") {
			failJSON(w)
			return
		}
		n := h.number(r)
		h.mu.Lock()
		out := []map[string]any{}
		for _, e := range h.events[n] {
			out = append(out, map[string]any{
				"event": e.event, "label": map[string]string{"name": e.label},
				"actor": map[string]string{"login": e.actor}, "created_at": hoursAgo(0.5),
			})
		}
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusOK, out)
	})
	mux.HandleFunc("POST /repos/o/r/issues/{n}/labels", func(w http.ResponseWriter, r *http.Request) {
		if h.record("add-label") {
			failJSON(w)
			return
		}
		var labels []string
		if err := json.NewDecoder(r.Body).Decode(&labels); err != nil {
			h.t.Errorf("decode labels: %v", err)
		}
		n := h.number(r)
		h.mu.Lock()
		for _, l := range labels {
			h.labels[n][l] = true
			h.events[n] = append(h.events[n], clankerTestEvent{event: "labeled", label: l, actor: clankerTestAppBot})
		}
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusOK, []any{})
	})
	mux.HandleFunc("DELETE /repos/o/r/issues/{n}/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		if h.record("remove-label") {
			failJSON(w)
			return
		}
		n := h.number(r)
		label := r.PathValue("label")
		h.mu.Lock()
		delete(h.labels[n], label)
		h.events[n] = append(h.events[n], clankerTestEvent{event: "unlabeled", label: label, actor: clankerTestAppBot})
		h.mu.Unlock()
		writeJSONStatus(w, http.StatusOK, []any{})
	})
	mux.HandleFunc("GET /repos/o/r/labels/{label}", func(w http.ResponseWriter, r *http.Request) {
		if h.record("get-label") {
			failJSON(w)
			return
		}
		writeJSONStatus(w, http.StatusOK, map[string]string{"name": r.PathValue("label")})
	})
	server := httptest.NewServer(mux)
	h.t.Cleanup(server.Close)
	return server
}

func clankerFilter(on bool, trustedLogins ...string) config.IssueFilterConfig {
	label := "clanker-requested"
	return config.IssueFilterConfig{ReporterTrust: config.ReporterTrustConfig{
		ClankerRequested:         &on,
		ClankerRequestedLabel:    &label,
		ClankerRequestedAddendum: "See CONTRIBUTING.md.",
		TrustedLogins:            trustedLogins,
	}}
}

func clankerTestClient(t *testing.T, h *clankerPRHarness, f IssueAdmitter) (*Client, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	c := NewClientForTest(h.server().URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&buf, nil)))
	c.SetAppBotLogin(clankerTestAppBot)
	c.SetIssueFilter(f)
	c.SetHoldLabels(config.ReporterTrustConfig{ClankerRequested: gh.Ptr(true)}.ExtraHoldLabels())
	return c, &buf
}

func strangerPR(number int) clankerTestPR {
	return clankerTestPR{number: number, login: "stranger", userType: "User", association: "NONE"}
}

func ghPR(p clankerTestPR) *gh.PullRequest {
	return &gh.PullRequest{
		Number:            gh.Ptr(p.number),
		User:              &gh.User{Login: gh.Ptr(p.login), Type: gh.Ptr(p.userType)},
		AuthorAssociation: gh.Ptr(p.association),
	}
}

// TestFetchPRs_ClankerRequestedParksUntrustedDirectPROnce is the acceptance
// path: within one poll the stranger's PR is labelled, commented once and
// lands in the held partition; later polls neither re-comment nor re-label.
func TestFetchPRs_ClankerRequestedParksUntrustedDirectPROnce(t *testing.T) {
	h := newClankerPRHarness(t, strangerPR(7))
	c, _ := clankerTestClient(t, h, clankerFilter(true))
	recs := captureAudit(c)

	actionable, held, _, _, _, _, _, err := c.fetchPRs(t.Context(), "r", newClankerRequestedBudget())
	if err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if len(actionable) != 0 {
		t.Fatalf("parked PR must not be actionable: %+v", actionable)
	}
	if len(held) != 1 || held[0].Number != 7 {
		t.Fatalf("held = %+v, want #7 in the held partition on the same poll", held)
	}
	if !h.hasLabel(7, "clanker-requested") {
		t.Fatal("clanker-requested label not applied")
	}
	if got := h.commentCount(7); got != 1 {
		t.Fatalf("comments = %d, want 1", got)
	}
	body := h.comments[7][0].body
	for _, want := range []string{ClankerRequestedNoticeMarker, "parked with the `clanker-requested` label until it is resubmitted through the relay or a maintainer removes the label",
		clankerRelayDocURL, "ClankeR", "See CONTRIBUTING.md."} {
		if !strings.Contains(body, want) {
			t.Errorf("notice missing %q:\n%s", want, body)
		}
	}
	if rec, ok := findAudit(*recs, AuditActionHiveLabelApplied); !ok || !strings.Contains(rec.Detail, "reason=clanker_requested") {
		t.Errorf("missing clanker_requested label audit: %#v", *recs)
	}

	for i := 0; i < 2; i++ {
		if _, held, _, _, _, _, _, err := c.fetchPRs(t.Context(), "r", newClankerRequestedBudget()); err != nil || len(held) != 1 {
			t.Fatalf("poll %d: held=%+v err=%v", i, held, err)
		}
	}
	if got := h.commentCount(7); got != 1 {
		t.Fatalf("later polls re-commented: %d comments", got)
	}
	if got := h.called("add-label"); got != 1 {
		t.Fatalf("label applied %d times, want 1", got)
	}
}

func TestEnumerateActionable_ClankerRequestedHeld(t *testing.T) {
	h := newClankerPRHarness(t, strangerPR(8))
	c, _ := clankerTestClient(t, h, clankerFilter(true))
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("EnumerateActionable: %v", err)
	}
	found := false
	for _, item := range result.Hold.Items {
		if item.Type == "pr" && item.Number == 8 {
			found = true
		}
	}
	if !found {
		t.Fatalf("parked PR not in the hold list: %+v", result.Hold.Items)
	}
	if !h.hasLabel(8, "clanker-requested") || h.commentCount(8) != 1 {
		t.Fatalf("label=%v comments=%d", h.hasLabel(8, "clanker-requested"), h.commentCount(8))
	}
}

func TestParkClankerRequestedPR_Untouched(t *testing.T) {
	tests := []struct {
		name   string
		pr     clankerTestPR
		filter IssueAdmitter
		relay  func(string) bool
	}{
		{"switch off", strangerPR(1), clankerFilter(false), nil},
		{"filter without clanker settings", strangerPR(1), admitAllIssues{}, nil},
		{"trusted login", strangerPR(1), clankerFilter(true, "Stranger"), nil},
		{"trusted association", clankerTestPR{number: 1, login: "member", userType: "User", association: "MEMBER"}, clankerFilter(true), nil},
		{"relay contributor", strangerPR(1), clankerFilter(true), func(login string) bool { return login == "stranger" }},
		{"app-authored hive-open-pr", clankerTestPR{number: 1, login: clankerTestAppBot, userType: "Bot", association: "NONE"}, clankerFilter(true), nil},
		{"other bot", clankerTestPR{number: 1, login: "dependabot[bot]", userType: "Bot", association: "NONE"}, clankerFilter(true), nil},
		{"already labelled", clankerTestPR{number: 1, login: "stranger", userType: "User", association: "NONE", labels: []string{"Clanker-Requested"}}, clankerFilter(true), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newClankerPRHarness(t, tt.pr)
			c, _ := clankerTestClient(t, h, tt.filter)
			c.SetRelayContributor(tt.relay)
			in := append([]string{}, tt.pr.labels...)
			got := c.parkClankerRequestedPR(t.Context(), "r", ghPR(tt.pr), in, nil)
			if len(got) != len(tt.pr.labels) {
				t.Errorf("labels = %v, want unchanged %v", got, tt.pr.labels)
			}
			if n := len(h.calls); n != 0 {
				t.Errorf("expected no API calls, got %v", h.calls)
			}
		})
	}
}

// TestParkClankerRequestedPR_HumanRemovalIsPermanent pins the release: once a
// maintainer removes the label it is never re-applied to that PR.
func TestParkClankerRequestedPR_HumanRemovalIsPermanent(t *testing.T) {
	h := newClankerPRHarness(t, strangerPR(9))
	c, _ := clankerTestClient(t, h, clankerFilter(true))
	if _, _, _, _, _, _, _, err := c.fetchPRs(t.Context(), "r", nil); err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if !h.hasLabel(9, "clanker-requested") {
		t.Fatal("first poll did not park")
	}
	h.humanRemove(9, "clanker-requested", "maintainer")

	for i := 0; i < 2; i++ {
		actionable, held, _, _, _, _, _, err := c.fetchPRs(t.Context(), "r", nil)
		if err != nil {
			t.Fatalf("fetchPRs: %v", err)
		}
		if len(held) != 0 || len(actionable) != 1 {
			t.Fatalf("released PR re-held: held=%+v actionable=%+v", held, actionable)
		}
	}
	if h.hasLabel(9, "clanker-requested") || h.called("add-label") != 1 || h.commentCount(9) != 1 {
		t.Fatalf("label re-applied or re-commented: adds=%d comments=%d", h.called("add-label"), h.commentCount(9))
	}
}

// Even without the notice (deleted by someone), a human removal in the label
// history releases the PR.
func TestParkClankerRequestedPR_RemovalHistoryWithoutNotice(t *testing.T) {
	h := newClankerPRHarness(t, strangerPR(10))
	h.events[10] = []clankerTestEvent{
		{event: "labeled", label: "other", actor: "maintainer"},
		{event: "unlabeled", label: "clanker-requested", actor: clankerTestAppBot},
		{event: "unlabeled", label: "Clanker-Requested", actor: "maintainer"},
	}
	c, _ := clankerTestClient(t, h, clankerFilter(true))
	got := c.parkClankerRequestedPR(t.Context(), "r", ghPR(h.prs[0]), nil, nil)
	if len(got) != 0 || h.called("add-label") != 0 || h.called("post-comment") != 0 {
		t.Fatalf("removed label re-applied: labels=%v calls=%v", got, h.calls)
	}
}

// An App-only removal (the comment-failure cleanup) is not a release.
func TestParkClankerRequestedPR_AppRemovalIsNotRelease(t *testing.T) {
	h := newClankerPRHarness(t, strangerPR(11))
	h.events[11] = []clankerTestEvent{{event: "unlabeled", label: "clanker-requested", actor: clankerTestAppBot}}
	c, _ := clankerTestClient(t, h, clankerFilter(true))
	got := c.parkClankerRequestedPR(t.Context(), "r", ghPR(h.prs[0]), nil, nil)
	if len(got) != 1 || got[0] != "clanker-requested" {
		t.Fatalf("labels = %v, want parked", got)
	}
}

func TestParkClankerRequestedPR_ExistingNoticeSkips(t *testing.T) {
	h := newClankerPRHarness(t, strangerPR(12))
	h.comments[12] = []clankerTestComment{
		{login: "stranger", body: ClankerRequestedNoticeMarker + " spoofed"},
		{login: clankerTestAppBot, body: clankerRequestedNotice("clanker-requested", clankerFilter(true))},
	}
	c, _ := clankerTestClient(t, h, clankerFilter(true))
	got := c.parkClankerRequestedPR(t.Context(), "r", ghPR(h.prs[0]), nil, nil)
	if len(got) != 0 || h.called("list-events") != 0 || h.called("add-label") != 0 {
		t.Fatalf("existing notice must release: labels=%v calls=%v", got, h.calls)
	}

	// A notice-looking comment from someone else does not count.
	if hasClankerRequestedNotice([]*gh.IssueComment{nil, {Body: gh.Ptr(ClankerRequestedNoticeMarker), User: &gh.User{Login: gh.Ptr("stranger")}}}, clankerTestAppBot) {
		t.Error("non-App notice must not count")
	}
	if !hasClankerRequestedNotice([]*gh.IssueComment{{Body: gh.Ptr(ClankerRequestedNoticeMarker)}}, "") {
		t.Error("blank bot login accepts any author's notice")
	}
}

func TestParkClankerRequestedPR_Failures(t *testing.T) {
	tests := []struct {
		name       string
		fail       string
		wantLog    string
		wantLabel  bool
		wantRemove bool
	}{
		{"comment scan", "list-comments", "clanker-requested comment scan failed", false, false},
		{"label history", "list-events", "clanker-requested label history scan failed", false, false},
		{"label ensure", "get-label", "clanker-requested label ensure failed", false, false},
		{"label add", "add-label", "clanker-requested label add failed", false, false},
		{"comment post", "post-comment", "clanker-requested comment post failed", false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newClankerPRHarness(t, strangerPR(13))
			h.fail[tt.fail] = true
			c, buf := clankerTestClient(t, h, clankerFilter(true))
			got := c.parkClankerRequestedPR(t.Context(), "r", ghPR(h.prs[0]), nil, nil)
			if len(got) != 0 {
				t.Errorf("labels = %v, want unparked", got)
			}
			if !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log %q missing %q", buf.String(), tt.wantLog)
			}
			if h.hasLabel(13, "clanker-requested") != tt.wantLabel {
				t.Errorf("label present = %v", h.hasLabel(13, "clanker-requested"))
			}
			if (h.called("remove-label") == 1) != tt.wantRemove {
				t.Errorf("remove calls = %d", h.called("remove-label"))
			}
		})
	}

	t.Run("comment post and cleanup", func(t *testing.T) {
		h := newClankerPRHarness(t, strangerPR(14))
		h.fail["post-comment"] = true
		h.fail["remove-label"] = true
		c, buf := clankerTestClient(t, h, clankerFilter(true))
		c.parkClankerRequestedPR(t.Context(), "r", ghPR(h.prs[0]), nil, nil)
		if !strings.Contains(buf.String(), "clanker-requested label cleanup failed") {
			t.Errorf("log %q missing cleanup failure", buf.String())
		}
	})
}

func TestParkClankerRequestedPR_Budget(t *testing.T) {
	prs := make([]clankerTestPR, 0, DefaultClankerRequestedMaxNotices+1)
	for i := 1; i <= DefaultClankerRequestedMaxNotices+1; i++ {
		prs = append(prs, strangerPR(100+i))
	}
	h := newClankerPRHarness(t, prs...)
	c, buf := clankerTestClient(t, h, clankerFilter(true))
	if _, _, _, _, _, _, _, err := c.fetchPRs(t.Context(), "r", newClankerRequestedBudget()); err != nil {
		t.Fatalf("fetchPRs: %v", err)
	}
	if got := h.called("post-comment"); got != DefaultClankerRequestedMaxNotices {
		t.Fatalf("notices = %d, want budget %d", got, DefaultClankerRequestedMaxNotices)
	}
	if !strings.Contains(buf.String(), "clanker-requested notice budget exhausted") {
		t.Errorf("budget exhaustion not logged: %q", buf.String())
	}
	if _, _, _, _, _, _, _, err := c.fetchPRs(t.Context(), "r", newClankerRequestedBudget()); err != nil {
		t.Fatalf("second fetchPRs: %v", err)
	}
	if got := h.called("post-comment"); got != DefaultClankerRequestedMaxNotices+1 {
		t.Fatalf("next poll must park the remainder: notices = %d", got)
	}
}

func TestClankerRequestedNilAndHelpers(t *testing.T) {
	var nilClient *Client
	nilClient.SetRelayContributor(func(string) bool { return true })
	if nilClient.isRelayContributor("x") {
		t.Error("nil client knows no relay contributors")
	}
	if got := nilClient.parkClankerRequestedPR(t.Context(), "r", ghPR(strangerPR(1)), []string{"a"}, nil); len(got) != 1 {
		t.Errorf("nil client must return labels unchanged, got %v", got)
	}
	c := &Client{}
	if c.isRelayContributor("x") {
		t.Error("unset predicate knows no relay contributors")
	}
	if got := c.parkClankerRequestedPR(t.Context(), "r", nil, nil, nil); got != nil {
		t.Errorf("nil PR = %v", got)
	}
	c.SetRelayContributor(func(login string) bool { return login == "x" })
	if !c.isRelayContributor("x") || c.isRelayContributor("y") {
		t.Error("predicate not consulted")
	}
	if !IsClankerRequestedNotice("a "+ClankerRequestedNoticeMarker) || IsClankerRequestedNotice("plain") {
		t.Error("IsClankerRequestedNotice")
	}
	if got := clankerRequestedAddendum(clankerFilter(true)); got != "\n\nSee CONTRIBUTING.md." {
		t.Errorf("addendum = %q", got)
	}
	blank := clankerFilter(true)
	blank.ReporterTrust.ClankerRequestedAddendum = "   "
	if got := clankerRequestedAddendum(blank); got != "" {
		t.Errorf("blank addendum = %q", got)
	}
	if b := newClankerRequestedBudget(); b.remaining != DefaultClankerRequestedMaxNotices {
		t.Errorf("budget = %d", b.remaining)
	}

	nilClient.warnClankerRequested("m", "o/r", 1, errors.New("boom"))
	var buf bytes.Buffer
	lc := &Client{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	lc.warnClankerRequested("m", "o/r", 1, nil)
	if buf.Len() != 0 {
		t.Errorf("nil error logged %q", buf.String())
	}
}

func TestClankerRequestedLabelRemovedByHuman_Pagination(t *testing.T) {
	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/issues/1/events", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "2" {
			writeJSONStatus(w, http.StatusOK, []map[string]any{{
				"event": "unlabeled", "label": map[string]string{"name": "clanker-requested"}, "actor": map[string]string{"login": "maintainer"},
			}})
			return
		}
		w.Header().Set("Link", `<`+server.URL+`/repos/o/r/issues/1/events?page=2>; rel="next"`)
		writeJSONStatus(w, http.StatusOK, []any{nil})
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	c := NewClientForTest(server.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)))
	got, err := c.clankerRequestedLabelRemovedByHuman(t.Context(), "o", "r", 1, "clanker-requested")
	if err != nil || !got {
		t.Fatalf("removed=%v err=%v, want the page-2 human removal", got, err)
	}
}
