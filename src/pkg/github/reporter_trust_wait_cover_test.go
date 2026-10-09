package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

type plainWaitAdmitter struct{}

func (plainWaitAdmitter) AdmitsReporter([]string, string, string) bool { return false }
func (plainWaitAdmitter) ReporterTrustEnabled() bool                   { return true }

type noticeWaitAdmitter struct {
	plainWaitAdmitter
	associations []string
	required     []string
	awaiting     string
	comment      bool
}

func (a noticeWaitAdmitter) ReporterTrustTrustedAssociationsForNotice() []string {
	return a.associations
}
func (a noticeWaitAdmitter) ReporterTrustRequiredLabelsForNotice() []string { return a.required }
func (a noticeWaitAdmitter) ReporterTrustAwaitingLabel() string             { return a.awaiting }
func (a noticeWaitAdmitter) ReporterTrustCommentEnabled() bool              { return a.comment }

func TestReporterTrustNoticeDefaults(t *testing.T) {
	ra := plainWaitAdmitter{}
	if got := reporterTrustRequiredLabelsForNotice(ra); !reflect.DeepEqual(got, []string{"triage/accepted"}) {
		t.Errorf("required labels = %v", got)
	}
	if got := reporterTrustTrustedAssociationsForNotice(ra); !reflect.DeepEqual(got, []string{"OWNER", "MEMBER", "COLLABORATOR"}) {
		t.Errorf("associations = %v", got)
	}
	if got := reporterTrustAwaitingLabel(ra); got != "needs-triage" {
		t.Errorf("awaiting label = %q", got)
	}
	if !reporterTrustCommentEnabled(ra) {
		t.Error("comment should default to enabled")
	}

	blank := noticeWaitAdmitter{associations: []string{" "}, required: []string{""}, awaiting: " x ", comment: false}
	if got := reporterTrustRequiredLabelsForNotice(blank); !reflect.DeepEqual(got, []string{"triage/accepted"}) {
		t.Errorf("blank required labels = %v", got)
	}
	if got := reporterTrustTrustedAssociationsForNotice(blank); len(got) != 3 {
		t.Errorf("blank associations = %v", got)
	}
	if got := reporterTrustAwaitingLabel(blank); got != "x" {
		t.Errorf("awaiting label = %q", got)
	}
	if reporterTrustCommentEnabled(blank) {
		t.Error("comment should follow config")
	}
}

func TestReporterTrustWaitComment(t *testing.T) {
	tests := []struct {
		name     string
		ra       ReporterAdmitter
		label    string
		contains []string
	}{
		{"default single label", plainWaitAdmitter{}, "", []string{"the label `triage/accepted`", "repo=o/r -->"}},
		{"multiple labels", noticeWaitAdmitter{required: []string{"a", "b"}, comment: true}, "needs-triage",
			[]string{"one of the labels `a`, `b`", "added-label=needs-triage"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := reporterTrustWaitComment("o/r", tt.label, tt.ra)
			for _, want := range tt.contains {
				if !strings.Contains(got, want) {
					t.Errorf("comment missing %q:\n%s", want, got)
				}
			}
		})
	}
}

func TestReporterTrustWaitMarkedAdded(t *testing.T) {
	tests := []struct {
		name     string
		comments []string
		want     bool
	}{
		{"no marker prefix", []string{"plain body"}, false},
		{"unterminated marker", []string{reporterTrustWaitMarkerPrefix + "added-label=needs-triage"}, false},
		{"marker without added label", []string{reporterTrustWaitMarker("o/r", "")}, false},
		{"other label", []string{reporterTrustWaitMarker("o/r", "other")}, false},
		{"match", []string{"x", reporterTrustWaitMarker("o/r", "needs-triage")}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := reporterTrustWaitMarkedAdded(tt.comments, "needs-triage"); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWarnReporterTrustWait(t *testing.T) {
	err := errors.New("boom")
	(*Client)(nil).warnReporterTrustWait("m", "o/r", 1, err)
	(&Client{}).warnReporterTrustWait("m", "o/r", 1, err)

	var buf bytes.Buffer
	c := &Client{logger: slog.New(slog.NewTextHandler(&buf, nil))}
	c.warnReporterTrustWait("m", "o/r", 1, nil)
	if buf.Len() != 0 {
		t.Fatalf("nil error must not log, got %q", buf.String())
	}
	c.warnReporterTrustWait("scan failed", "o/r", 1, err)
	if !strings.Contains(buf.String(), "reporter trust wait scan failed") || !strings.Contains(buf.String(), "boom") {
		t.Errorf("unexpected log output %q", buf.String())
	}
}

func TestReporterTrustNilClient(t *testing.T) {
	ctx := context.Background()
	issue := &gh.Issue{Number: gh.Ptr(1)}
	if _, err := (*Client)(nil).reporterTrustWaitComments(ctx, "o/r", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Errorf("err = %v, want ErrNoGitHubClient", err)
	}
	(*Client)(nil).markReporterTrustAwaiting(ctx, "o/r", issue, nil, plainWaitAdmitter{}, nil)
	(*Client)(nil).markReporterTrustAwaiting(ctx, "o/r", issue, nil, noticeWaitAdmitter{comment: false}, nil)

	labels := []string{"needs-triage"}
	if got := (*Client)(nil).clearReporterTrustAwaiting(ctx, "o/r", issue, labels, plainWaitAdmitter{}); !reflect.DeepEqual(got, labels) {
		t.Errorf("labels = %v, want unchanged", got)
	}
	if got := (*Client)(nil).clearReporterTrustAwaiting(ctx, "o/r", issue, []string{"bug"}, plainWaitAdmitter{}); len(got) != 1 {
		t.Errorf("labels = %v, want unchanged", got)
	}
}

type waitCall struct {
	mu    sync.Mutex
	calls []string
}

func (w *waitCall) add(s string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, s)
}

func (w *waitCall) has(s string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, c := range w.calls {
		if c == s {
			return true
		}
	}
	return false
}

// newWaitTestServer routes "METHOD /path" to a status code; unlisted routes 404.
func newWaitTestServer(t *testing.T, statuses map[string]int, handlers map[string]http.HandlerFunc) (*httptest.Server, *waitCall) {
	t.Helper()
	rec := &waitCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		rec.add(key)
		if h, ok := handlers[key]; ok {
			h(w, r)
			return
		}
		status, ok := statuses[key]
		if !ok {
			status = http.StatusNotFound
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch {
		case status >= 400:
			fmt.Fprint(w, `{"message":"nope"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/labels"):
			fmt.Fprint(w, `[]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(server.Close)
	return server, rec
}

func waitTestClient(t *testing.T, server *httptest.Server) (*Client, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	c := NewClientForTest(server.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(&buf, nil)))
	return c, &buf
}

func TestMarkReporterTrustAwaiting_HTTP(t *testing.T) {
	const (
		listComments = "GET /repos/o/r/issues/1/comments"
		getLabel     = "GET /repos/o/r/labels/needs-triage"
		addLabels    = "POST /repos/o/r/issues/1/labels"
		postComment  = "POST /repos/o/r/issues/1/comments"
		removeLabel  = "DELETE /repos/o/r/issues/1/labels/needs-triage"
	)
	tests := []struct {
		name     string
		statuses map[string]int
		labels   []string
		ra       ReporterAdmitter
		wantLog  string
		wantCall []string
		noCall   []string
	}{
		{
			name:     "comments disabled does nothing",
			statuses: map[string]int{},
			ra:       noticeWaitAdmitter{awaiting: "needs-triage", comment: false},
			noCall:   []string{listComments},
		},
		{
			name:     "comment scan error",
			statuses: map[string]int{listComments: 500},
			ra:       plainWaitAdmitter{},
			wantLog:  "comment scan failed",
			noCall:   []string{postComment},
		},
		{
			name:     "label ensure error still comments",
			statuses: map[string]int{listComments: 200, getLabel: 500, postComment: 201},
			ra:       plainWaitAdmitter{},
			wantLog:  "label ensure failed",
			wantCall: []string{postComment},
			noCall:   []string{addLabels},
		},
		{
			name:     "label add error still comments",
			statuses: map[string]int{listComments: 200, getLabel: 200, addLabels: 500, postComment: 201},
			ra:       plainWaitAdmitter{},
			wantLog:  "label add failed",
			wantCall: []string{postComment},
		},
		{
			name:     "label already present skips label calls",
			statuses: map[string]int{listComments: 200, postComment: 201},
			labels:   []string{"needs-triage"},
			ra:       plainWaitAdmitter{},
			wantLog:  "reporter_trust_wait_noticed",
			wantCall: []string{postComment},
			noCall:   []string{getLabel, addLabels},
		},
		{
			name:     "comment failure removes added label",
			statuses: map[string]int{listComments: 200, getLabel: 200, addLabels: 200, postComment: 500, removeLabel: 204},
			ra:       plainWaitAdmitter{},
			wantLog:  "comment post failed",
			wantCall: []string{removeLabel},
		},
		{
			name:     "comment failure and cleanup failure",
			statuses: map[string]int{listComments: 200, getLabel: 200, addLabels: 200, postComment: 500, removeLabel: 500},
			ra:       plainWaitAdmitter{},
			wantLog:  "label cleanup failed",
			wantCall: []string{removeLabel},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, rec := newWaitTestServer(t, tt.statuses, nil)
			c, buf := waitTestClient(t, server)
			c.markReporterTrustAwaiting(context.Background(), "o/r", &gh.Issue{Number: gh.Ptr(1)}, tt.labels, tt.ra, nil)
			if tt.wantLog != "" && !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log %q missing %q", buf.String(), tt.wantLog)
			}
			if tt.wantLog == "" && buf.Len() != 0 {
				t.Errorf("unexpected log %q", buf.String())
			}
			for _, call := range tt.wantCall {
				if !rec.has(call) {
					t.Errorf("expected call %q, got %v", call, rec.calls)
				}
			}
			for _, call := range tt.noCall {
				if rec.has(call) {
					t.Errorf("unexpected call %q", call)
				}
			}
		})
	}
}

func TestMarkReporterTrustAwaiting_ExistingNoticeSkips(t *testing.T) {
	marked := reporterTrustWaitComment("o/r", "needs-triage", plainWaitAdmitter{})
	handlers := map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/1/comments": func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `[{"body":%q}]`, marked)
		},
	}
	server, rec := newWaitTestServer(t, nil, handlers)
	c, _ := waitTestClient(t, server)
	c.markReporterTrustAwaiting(context.Background(), "o/r", &gh.Issue{Number: gh.Ptr(1)}, nil, plainWaitAdmitter{}, nil)
	if rec.has("POST /repos/o/r/issues/1/comments") {
		t.Error("a second notice was posted")
	}
}

func TestClearReporterTrustAwaiting_HTTP(t *testing.T) {
	const removeLabel = "DELETE /repos/o/r/issues/1/labels/needs-triage"
	marked := reporterTrustWaitComment("o/r", "needs-triage", plainWaitAdmitter{})
	unmarked := reporterTrustWaitComment("o/r", "", plainWaitAdmitter{})
	commentsFor := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			fmt.Fprintf(w, `[{"body":%q},{"body":"unrelated"}]`, body)
		}
	}
	failComments := func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"nope"}`, http.StatusInternalServerError)
	}

	tests := []struct {
		name     string
		list     http.HandlerFunc
		removeSt int
		want     []string
		wantLog  string
		wantDel  bool
	}{
		{"scan error keeps labels", failComments, 204, []string{"needs-triage", "bug"}, "comment scan failed", false},
		{"notice without added label keeps labels", commentsFor(unmarked), 204, []string{"needs-triage", "bug"}, "", false},
		{"remove error keeps labels", commentsFor(marked), 500, []string{"needs-triage", "bug"}, "label remove failed", true},
		{"removed", commentsFor(marked), 204, []string{"bug"}, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server, rec := newWaitTestServer(t, map[string]int{removeLabel: tt.removeSt},
				map[string]http.HandlerFunc{"GET /repos/o/r/issues/1/comments": tt.list})
			c, buf := waitTestClient(t, server)
			got := c.clearReporterTrustAwaiting(context.Background(), "o/r", &gh.Issue{Number: gh.Ptr(1)},
				[]string{"needs-triage", "bug"}, plainWaitAdmitter{})
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("labels = %v, want %v", got, tt.want)
			}
			if tt.wantLog != "" && !strings.Contains(buf.String(), tt.wantLog) {
				t.Errorf("log %q missing %q", buf.String(), tt.wantLog)
			}
			if rec.has(removeLabel) != tt.wantDel {
				t.Errorf("remove call = %v, want %v", rec.has(removeLabel), tt.wantDel)
			}
		})
	}
}

func TestReporterTrustWaitComments_Pagination(t *testing.T) {
	marked := reporterTrustWaitMarker("o/r", "needs-triage")
	var server *httptest.Server
	handlers := map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/1/comments": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				fmt.Fprintf(w, `[{"body":%q}]`, marked)
				return
			}
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues/1/comments?page=2>; rel="next"`, server.URL))
			fmt.Fprint(w, `[{"body":"first page, no marker"}]`)
		},
	}
	server, _ = newWaitTestServer(t, nil, handlers)
	c, _ := waitTestClient(t, server)
	got, err := c.reporterTrustWaitComments(context.Background(), "o/r", 1)
	if err != nil {
		t.Fatalf("reporterTrustWaitComments: %v", err)
	}
	if len(got) != 1 || got[0] != marked {
		t.Errorf("comments = %v, want only the page-2 marker comment", got)
	}
}

type clankerWaitAdmitter struct {
	plainWaitAdmitter
	on       bool
	addendum string
}

func (a clankerWaitAdmitter) ReporterTrustClankerRequestedOn() bool         { return a.on }
func (a clankerWaitAdmitter) ReporterTrustClankerRequestedLabel() string    { return "clanker-requested" }
func (a clankerWaitAdmitter) ReporterTrustClankerRequestedAddendum() string { return a.addendum }
func (a clankerWaitAdmitter) ReporterTrustTrusts(string, string) bool       { return false }

// reporterTrustWaitGolden is the wait comment as it stood before the
// clanker-requested policy (hivecommons/hive#10780), plus the governance
// pointer (hivecommons/hive#11018). With the switch off the output must stay
// byte-identical to it.
const reporterTrustWaitGolden = "<!-- hive:reporter-trust-wait repo=o/r added-label=needs-triage -->\n" +
	"Thanks — this hive only works issues from OWNER, MEMBER, COLLABORATOR automatically. " +
	"A maintainer can admit this one by adding the label `triage/accepted` (configured in `issue_filter.reporter_trust.untrusted_require_labels`). " +
	"Until then the hive will not claim, label, or open PRs for it. " +
	"See https://github.com/o/r/blob/HEAD/GOVERNANCE.md#reporter-trust-and-escalation for what these roles mean and how to escalate."

func TestReporterTrustWaitComment_ClankerOffGolden(t *testing.T) {
	for name, ra := range map[string]ReporterAdmitter{
		"no clanker config": plainWaitAdmitter{},
		"clanker off":       clankerWaitAdmitter{on: false, addendum: "ignored while off"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := reporterTrustWaitComment("o/r", "needs-triage", ra); got != reporterTrustWaitGolden {
				t.Errorf("comment changed with the switch off:\n got %q\nwant %q", got, reporterTrustWaitGolden)
			}
		})
	}
}

func TestReporterTrustWaitComment_ClankerOn(t *testing.T) {
	got := reporterTrustWaitComment("o/r", "needs-triage", clankerWaitAdmitter{on: true, addendum: "  See CONTRIBUTING.md.  "})
	if !strings.HasPrefix(got, reporterTrustWaitGolden+"\n\n") {
		t.Fatalf("first paragraph must be unchanged:\n%s", got)
	}
	for _, want := range []string{"ClankeR", clankerRelayDocURL, "contributor-relay.md#basic-setup",
		"this issue will be offered to it", "\n\nSee CONTRIBUTING.md."} {
		if !strings.Contains(got, want) {
			t.Errorf("comment missing %q:\n%s", want, got)
		}
	}
	if !strings.HasSuffix(got, "See CONTRIBUTING.md.") {
		t.Errorf("addendum must close the comment:\n%s", got)
	}
	noAddendum := reporterTrustWaitComment("o/r", "", clankerWaitAdmitter{on: true})
	if !strings.HasSuffix(noAddendum, "like any other queued work.") {
		t.Errorf("blank addendum must add nothing:\n%s", noAddendum)
	}
	if !strings.Contains(noAddendum, reporterTrustWaitMarker("o/r", "")) {
		t.Errorf("marker missing:\n%s", noAddendum)
	}
}

func TestMarkReporterTrustAwaiting_ClankerOnPostsOnce(t *testing.T) {
	ra := clankerWaitAdmitter{on: true, addendum: "Extra."}
	var mu sync.Mutex
	var posted []string
	handlers := map[string]http.HandlerFunc{
		"GET /repos/o/r/issues/1/comments": func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			defer mu.Unlock()
			out := make([]map[string]string, 0, len(posted))
			for _, b := range posted {
				out = append(out, map[string]string{"body": b})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
		},
		"POST /repos/o/r/issues/1/comments": func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			mu.Lock()
			posted = append(posted, req.Body)
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{}`)
		},
	}
	server, _ := newWaitTestServer(t, map[string]int{"GET /repos/o/r/labels/needs-triage": 200, "POST /repos/o/r/issues/1/labels": 200}, handlers)
	c, _ := waitTestClient(t, server)
	for i := 0; i < 2; i++ {
		c.markReporterTrustAwaiting(context.Background(), "o/r", &gh.Issue{Number: gh.Ptr(1)}, nil, ra, nil)
	}
	if len(posted) != 1 {
		t.Fatalf("posted %d comments, want exactly one", len(posted))
	}
	if !strings.Contains(posted[0], clankerRelayDocURL) || !strings.HasSuffix(posted[0], "\n\nExtra.") ||
		!strings.Contains(posted[0], "added-label=needs-triage") {
		t.Errorf("posted comment missing relay link, addendum or marker:\n%s", posted[0])
	}
}
