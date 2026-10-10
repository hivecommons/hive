package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// The #7871/#8477 suite drives settleIssueFromVerdict through the injectable
// settleVerifier / issuePRClaimMarker / alreadyDoneMarker hooks. This file
// covers what sits behind those hooks when they are NOT injected: the real
// GHClient-backed markIssuePRClaim and markAlreadyDoneIssue, the nil-guard
// and "already done but unverifiable" early returns, and the evidence text
// that ends up in the issue comment.

// settleGHFixture is a fake GitHub API recording the label, comment and close
// mutations the settle path performs on o/r#7.
type settleGHFixture struct {
	mu            sync.Mutex
	labelsCreated []string
	labelsAdded   []string
	comments      []string
	closed        bool
	stateReason   string

	existingComments []string
	commentsListErr  bool
	issueLabels      []string
}

func (f *settleGHFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7/comments":
			if f.commentsListErr {
				w.WriteHeader(http.StatusInternalServerError)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "boom"})
				return
			}
			out := make([]map[string]any, 0, len(f.existingComments))
			for i, body := range f.existingComments {
				out = append(out, map[string]any{"id": i + 1, "body": body})
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.comments = append(f.comments, payload.Body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": len(f.comments), "html_url": "https://github.com/o/r/issues/7#issuecomment-1"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/o/r/labels/"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/labels":
			var payload struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.labelsCreated = append(f.labelsCreated, payload.Name)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": payload.Name})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/labels":
			var payload []string
			_ = json.NewDecoder(r.Body).Decode(&payload)
			f.labelsAdded = append(f.labelsAdded, payload...)
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/issues/7":
			labels := make([]map[string]any, 0, len(f.issueLabels))
			for _, l := range f.issueLabels {
				labels = append(labels, map[string]any{"name": l})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7,
				"state":  "open",
				"title":  "settle me",
				"body":   "filed by automation",
				"user":   map[string]any{"login": "hive-bot", "type": "Bot"},
				"labels": labels,
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/o/r/issues/7":
			var payload struct {
				State       string `json:"state"`
				StateReason string `json:"state_reason"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload.State == "closed" {
				f.closed = true
				f.stateReason = payload.StateReason
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/repos/o/r/issues/7/labels/"):
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// settleGHHub wires a covK2Hub to a GHClient pointed at the fake API, with no
// marker hooks injected so the real GitHub-backed paths run.
func settleGHHub(t *testing.T, fx *settleGHFixture) (*ContributeWSHub, *Server) {
	t.Helper()
	hub, s := covK2Hub(t)
	srv := fx.server(t)
	s.deps.GHClient = ghpkg.NewClientForTest(srv.URL, "o", []string{"r"}, hub.logger)
	if s.deps.Config == nil {
		s.deps.Config = &config.Config{}
	}
	hub.issuePRClaimMarker = nil
	hub.alreadyDoneMarker = nil
	return hub, s
}

func TestVerdictSettleGH_MarkIssuePRClaim_OpenPRGetsCoveredLabel(t *testing.T) {
	fx := &settleGHFixture{}
	hub, _ := settleGHHub(t, fx)

	err := hub.markIssuePRClaim(context.Background(), "o/r", 7, ghpkg.IssueClaim{PRNumber: 41, PRAuthor: "dev"})
	if err != nil {
		t.Fatalf("markIssuePRClaim: %v", err)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.labelsCreated) != 1 || fx.labelsCreated[0] != ghpkg.CoveredByPRLabel {
		t.Fatalf("labels created = %v, want [%s]", fx.labelsCreated, ghpkg.CoveredByPRLabel)
	}
	if len(fx.labelsAdded) != 1 || fx.labelsAdded[0] != ghpkg.CoveredByPRLabel {
		t.Fatalf("labels added = %v, want [%s]", fx.labelsAdded, ghpkg.CoveredByPRLabel)
	}
}

func TestVerdictSettleGH_MarkIssuePRClaim_MergedPRGetsLikelyDoneLabel(t *testing.T) {
	fx := &settleGHFixture{}
	hub, _ := settleGHHub(t, fx)

	err := hub.markIssuePRClaim(context.Background(), "o/r", 7, ghpkg.IssueClaim{PRNumber: 41, MergedPR: true})
	if err != nil {
		t.Fatalf("markIssuePRClaim: %v", err)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.labelsAdded) != 1 || fx.labelsAdded[0] != ghpkg.LikelyDoneLabel {
		t.Fatalf("labels added = %v, want [%s]", fx.labelsAdded, ghpkg.LikelyDoneLabel)
	}
}

func TestVerdictSettleGH_MarkIssuePRClaim_NoClientIsErrNoGitHubClient(t *testing.T) {
	hub, s := covK2Hub(t)
	hub.issuePRClaimMarker = nil
	s.deps.GHClient = nil

	if err := hub.markIssuePRClaim(context.Background(), "o/r", 7, ghpkg.IssueClaim{PRNumber: 1}); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
	var nilHub *ContributeWSHub
	if err := nilHub.markIssuePRClaim(context.Background(), "o/r", 7, ghpkg.IssueClaim{}); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("nil hub err = %v, want ErrNoGitHubClient", err)
	}
	if err := nilHub.markAlreadyDoneIssue(context.Background(), "o/r", 7, ghpkg.IssueClaim{}, "", false); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("nil hub already-done err = %v, want ErrNoGitHubClient", err)
	}
	hub.alreadyDoneMarker = nil
	if err := hub.markAlreadyDoneIssue(context.Background(), "o/r", 7, ghpkg.IssueClaim{}, "", true); !errors.Is(err, ghpkg.ErrNoGitHubClient) {
		t.Fatalf("no-client already-done err = %v, want ErrNoGitHubClient", err)
	}
}

func TestVerdictSettleGH_MarkAlreadyDone_CommentsLabelsAndCloses(t *testing.T) {
	fx := &settleGHFixture{}
	hub, s := settleGHHub(t, fx)
	s.deps.Config.Hub.ContributeAlreadyDoneLabel = "hive/done-custom"

	claim := ghpkg.IssueClaim{PRNumber: 41, PRURL: "https://github.com/o/r/pull/41", MergedPR: true}
	if err := hub.markAlreadyDoneIssue(context.Background(), "o/r", 7, claim, "danathar", true); err != nil {
		t.Fatalf("markAlreadyDoneIssue: %v", err)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one", fx.comments)
	}
	body := fx.comments[0]
	for _, want := range []string{"already resolved by #41", "found by contributor danathar", "`hive/done-custom`", "closing as completed"} {
		if !strings.Contains(body, want) {
			t.Errorf("comment %q missing %q", body, want)
		}
	}
	if len(fx.labelsCreated) != 1 || fx.labelsCreated[0] != "hive/done-custom" {
		t.Errorf("labels created = %v, want configured label", fx.labelsCreated)
	}
	if len(fx.labelsAdded) != 1 || fx.labelsAdded[0] != "hive/done-custom" {
		t.Errorf("labels added = %v, want configured label", fx.labelsAdded)
	}
	if !fx.closed || fx.stateReason != ghpkg.IssueStateReasonCompleted {
		t.Errorf("closed=%v state_reason=%q, want closed as completed", fx.closed, fx.stateReason)
	}
}

func TestVerdictSettleGH_MarkAlreadyDone_LabelOnlyWithoutReporterUsesDefaultLabel(t *testing.T) {
	fx := &settleGHFixture{}
	hub, s := settleGHHub(t, fx)
	saved := s.deps.Config
	s.deps.Config = nil // falls back to config.HubConfig{} default label
	t.Cleanup(func() { s.deps.Config = saved })

	claim := ghpkg.IssueClaim{PRURL: "https://github.com/o/r/commit/0123456789abcdef0123"}
	if err := hub.markAlreadyDoneIssue(context.Background(), "o/r", 7, claim, "  ", false); err != nil {
		t.Fatalf("markAlreadyDoneIssue: %v", err)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.comments) != 1 {
		t.Fatalf("comments = %v, want exactly one", fx.comments)
	}
	body := fx.comments[0]
	if strings.Contains(body, "found by contributor") {
		t.Errorf("blank reporter must not be attributed: %q", body)
	}
	if !strings.Contains(body, "already resolved by 0123456789ab;") {
		t.Errorf("commit evidence must be the 12-char short SHA: %q", body)
	}
	if !strings.Contains(body, "withheld from contributor offers") || strings.Contains(body, "closing") {
		t.Errorf("label-only action text wrong: %q", body)
	}
	want := config.HubConfig{}.ContributeAlreadyDoneLabelOrDefault()
	if len(fx.labelsAdded) != 1 || fx.labelsAdded[0] != want {
		t.Errorf("labels added = %v, want [%s]", fx.labelsAdded, want)
	}
	if fx.closed {
		t.Error("closeIssue=false must not close the issue")
	}
}

func TestVerdictSettleGH_MarkAlreadyDone_ExistingCommentIsNotReposted(t *testing.T) {
	fx := &settleGHFixture{}
	hub, _ := settleGHHub(t, fx)
	claim := ghpkg.IssueClaim{PRNumber: 41, MergedPR: true}

	// Compute the exact body by running once against an empty issue, then
	// seed it as an existing comment and run again.
	if err := hub.markAlreadyDoneIssue(context.Background(), "o/r", 7, claim, "ct", false); err != nil {
		t.Fatalf("first markAlreadyDoneIssue: %v", err)
	}
	fx.mu.Lock()
	if len(fx.comments) != 1 {
		fx.mu.Unlock()
		t.Fatalf("comments = %v, want one", fx.comments)
	}
	fx.existingComments = []string{"unrelated", fx.comments[0]}
	fx.comments = nil
	fx.mu.Unlock()

	if err := hub.markAlreadyDoneIssue(context.Background(), "o/r", 7, claim, "ct", false); err != nil {
		t.Fatalf("second markAlreadyDoneIssue: %v", err)
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.comments) != 0 {
		t.Fatalf("existing comment must not be reposted, got %v", fx.comments)
	}
	if len(fx.labelsAdded) != 2 {
		t.Fatalf("label is re-applied on every mark, got %v", fx.labelsAdded)
	}
}

func TestVerdictSettleGH_MarkAlreadyDone_CommentListErrorAborts(t *testing.T) {
	fx := &settleGHFixture{commentsListErr: true}
	hub, _ := settleGHHub(t, fx)

	err := hub.markAlreadyDoneIssue(context.Background(), "o/r", 7, ghpkg.IssueClaim{PRNumber: 41}, "ct", true)
	if err == nil {
		t.Fatal("comment lookup failure must surface as an error")
	}
	fx.mu.Lock()
	defer fx.mu.Unlock()
	if len(fx.comments) != 0 || len(fx.labelsAdded) != 0 || fx.closed {
		t.Fatalf("no mutation may follow a failed comment lookup: comments=%v labels=%v closed=%v", fx.comments, fx.labelsAdded, fx.closed)
	}
}

func TestVerdictSettleGH_AlreadyDoneEvidenceText(t *testing.T) {
	long := "https://github.com/o/r/commit/abcdef1234567890abcdef"
	cases := []struct {
		name  string
		claim ghpkg.IssueClaim
		want  string
	}{
		{"pr number wins", ghpkg.IssueClaim{PRNumber: 12, PRURL: long}, "#12"},
		{"long url tail is truncated to 12", ghpkg.IssueClaim{PRURL: long}, "abcdef123456"},
		{"short url tail kept", ghpkg.IssueClaim{PRURL: "https://github.com/o/r/pull/9/"}, "9"},
		{"nothing cited", ghpkg.IssueClaim{}, "the contributor's already-done verdict"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := alreadyDoneEvidenceText(tc.claim); got != tc.want {
				t.Fatalf("alreadyDoneEvidenceText = %q, want %q", got, tc.want)
			}
		})
	}
}

// settleIssueFromVerdict (the no-evidence wrapper) and the early
// already-done-unverified returns in settleIssueFromVerdictWithEvidence.
func TestVerdictSettleGH_WrapperAndUnverifiedEarlyReturns(t *testing.T) {
	var nilHub *ContributeWSHub
	if got := nilHub.settleIssueFromVerdict("o/r", 7, "already fixed", time.Now(), "ct"); got != "" {
		t.Fatalf("nil hub disposition = %q, want empty", got)
	}
	if got := nilHub.settleIssueFromVerdictWithEvidence("o/r", 7, "already fixed", "", nil, time.Now(), "ct"); got != "" {
		t.Fatalf("nil hub (evidence) disposition = %q, want empty", got)
	}

	hub, s := covK2Hub(t)
	if got := hub.settleIssueFromVerdict("o/r", 7, "", time.Now(), "ct"); got != "" {
		t.Fatalf("empty reason disposition = %q, want empty", got)
	}
	if got := hub.settleIssueFromVerdict("", 7, "already fixed", time.Now(), "ct"); got != "" {
		t.Fatalf("empty repo disposition = %q, want empty", got)
	}

	// already-done wording with no reference at all → unverified.
	s.deps.RecordIssueClaim = nil
	if got := hub.settleIssueFromVerdict("o/r", 7, "this was already fixed upstream", time.Now(), "ct"); got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("no-ref disposition = %q, want unverified", got)
	}
	// A reference but no ledger wired → unverified.
	if got := hub.settleIssueFromVerdict("o/r", 7, "already fixed by merged PR #41", time.Now(), "ct"); got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("no-ledger disposition = %q, want unverified", got)
	}
	// Non-already-done wording with no ledger → plain no-op.
	if got := hub.settleIssueFromVerdict("o/r", 7, "see PR #41", time.Now(), "ct"); got != "" {
		t.Fatalf("no-ledger non-already-done disposition = %q, want empty", got)
	}

	// Ledger wired but neither a verifier nor a GitHub client → unverified.
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	s.deps.GHClient = nil
	hub.settleVerifier = nil
	if got := hub.settleIssueFromVerdict("o/r", 7, "already fixed by merged PR #41", time.Now(), "ct"); got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("no-client disposition = %q, want unverified", got)
	}
	if got := hub.settleIssueFromVerdict("o/r", 7, "see PR #41", time.Now(), "ct"); got != "" {
		t.Fatalf("no-client non-already-done disposition = %q, want empty", got)
	}
	if len(fx.recorded) != 0 {
		t.Fatalf("nothing may be recorded without verification: %+v", fx.recorded)
	}
}

// A verifier error that coincides with context cancellation stops the loop
// and, for an already-done reason, lands on the unverified disposition.
func TestVerdictSettleGH_ContextCancelledDuringVerifyIsUnverified(t *testing.T) {
	hub, s := covK2Hub(t)
	fx := &settleFixture{}
	s.deps.RecordIssueClaim = fx.record
	ctx, cancel := context.WithCancel(context.Background())
	s.deps.Ctx = ctx
	hub.settleVerifier = func(_ context.Context, _ string, _ int, _ ghpkg.SettlingRef, _ time.Time) (ghpkg.SettleVerification, error) {
		cancel()
		return ghpkg.SettleVerification{Reason: "boom"}, errors.New("transport")
	}

	got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "already fixed by merged PR #41, see also #42", "", nil, time.Now(), "ct")
	if got != verdictDispositionAlreadyDoneUnverified {
		t.Fatalf("disposition = %q, want unverified", got)
	}
	if len(fx.recorded) != 0 {
		t.Fatalf("nothing may be recorded: %+v", fx.recorded)
	}

	// Same cancellation on a non-already-done reason is a plain no-op.
	ctx2, cancel2 := context.WithCancel(context.Background())
	s.deps.Ctx = ctx2
	hub.settleVerifier = func(_ context.Context, _ string, _ int, _ ghpkg.SettlingRef, _ time.Time) (ghpkg.SettleVerification, error) {
		cancel2()
		return ghpkg.SettleVerification{Reason: "boom"}, errors.New("transport")
	}
	if got := hub.settleIssueFromVerdictWithEvidence("o/r", 7, "see PR #41", "", nil, time.Now(), "ct"); got != "" {
		t.Fatalf("non-already-done disposition = %q, want empty", got)
	}
}
