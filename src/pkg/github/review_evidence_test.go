package github

// Tests for the review evidence writer (review_evidence.go): one bundle per
// head under ReviewEvidenceRoot, idempotent re-runs, optional signing, the
// previous-head link, and nothing written when the client is not wired.

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review"
)

type evidencePRFixture struct {
	mu     sync.Mutex
	head   string
	status int
	gets   int
}

func (f *evidencePRFixture) fetches() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets
}

func newEvidencePRServer(t *testing.T, f *evidencePRFixture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Method != http.MethodGet || r.URL.Path != "/repos/o/r/pulls/7" {
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.gets++
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"user":   map[string]any{"login": "alice", "type": "User"},
			"base":   map[string]any{"sha": "base000"},
			"head":   map[string]any{"sha": f.head},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// withEvidenceRoot points the writer and the author audit lookup at temp
// paths so no test touches /data.
func withEvidenceRoot(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "evidence")
	oldRoot, oldAudit := ReviewEvidenceRoot, reviewVerdictAuditPath
	ReviewEvidenceRoot = root
	reviewVerdictAuditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	t.Cleanup(func() { ReviewEvidenceRoot, reviewVerdictAuditPath = oldRoot, oldAudit })
	return root
}

func evidenceTestClient(t *testing.T, f *evidencePRFixture, settings *ReviewEvidenceSettings) *Client {
	t.Helper()
	c := newTestClient(t, newEvidencePRServer(t, f), "o", []string{"o/r"})
	if settings != nil {
		s := *settings
		c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return s })
	}
	return c
}

func evidenceReport(perspective review.Perspective, verdict review.Verdict, head string, findings ...outputschema.Finding) review.PerspectiveReport {
	r := review.PerspectiveReport{Perspective: perspective, Verdict: verdict, Repo: "o/r", Number: 7, HeadSHA: head, ReviewModel: "model-x"}
	r.Findings = findings
	return r
}

func mustLoadBundle(t *testing.T, root, head string) *evidence.Bundle {
	t.Helper()
	b, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 7, head))
	if err != nil {
		t.Fatalf("load bundle for %s: %v", head, err)
	}
	return b
}

func TestRecordReviewEvidence(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	posted := &gh.PullRequestReview{HTMLURL: gh.Ptr("https://github.com/o/r/pull/7#pullrequestreview-1"), CommitID: gh.Ptr("head111")}
	medium := outputschema.Finding{Title: "nil deref", Severity: outputschema.SeverityMedium, Summary: "x may be nil", File: "a.go", Line: 3}
	general := outputschema.Finding{Title: "missing docs", Severity: outputschema.SeverityLow}

	tests := []struct {
		name         string
		settings     *ReviewEvidenceSettings
		reports      []review.PerspectiveReport
		posted       *gh.PullRequestReview
		prHead       string
		wantHead     string
		wantVerdicts int
		wantPosted   int
		wantNoBundle bool
	}{
		{
			name:         "not wired writes nothing",
			reports:      []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "head111")},
			posted:       posted,
			wantNoBundle: true,
		},
		{
			name:         "disabled writes nothing",
			settings:     &ReviewEvidenceSettings{Enabled: false},
			reports:      []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "head111")},
			wantNoBundle: true,
		},
		{
			name:         "verdict and posted review",
			settings:     &ReviewEvidenceSettings{Enabled: true},
			reports:      []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "head111", medium, general)},
			posted:       posted,
			wantHead:     "head111",
			wantVerdicts: 1,
			wantPosted:   1,
		},
		{
			name:         "record-only verdict without head uses the PR head",
			settings:     &ReviewEvidenceSettings{Enabled: true},
			reports:      []review.PerspectiveReport{evidenceReport(review.PerspectiveSecurity, review.VerdictRequiresHuman, "")},
			prHead:       "head222",
			wantHead:     "head222",
			wantVerdicts: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			f := &evidencePRFixture{head: tt.prHead}
			c := evidenceTestClient(t, f, tt.settings)

			c.recordReviewEvidence(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer"}, tt.reports, tt.posted, now)

			if tt.wantNoBundle {
				if _, err := os.Stat(root); !os.IsNotExist(err) {
					t.Fatalf("evidence root exists, want nothing written: %v", err)
				}
				if f.fetches() != 0 {
					t.Fatalf("PR fetched %d times, want 0", f.fetches())
				}
				return
			}
			b := mustLoadBundle(t, root, tt.wantHead)
			if err := b.Validate(); err != nil {
				t.Fatalf("bundle invalid: %v", err)
			}
			if b.ID != evidence.BundleID("o/r", 7, tt.wantHead) || b.BaseSHA != "base000" {
				t.Fatalf("id/base = %q/%q", b.ID, b.BaseSHA)
			}
			if b.Author != (evidence.Author{Login: "alice", Kind: evidence.AuthorHuman}) {
				t.Fatalf("author = %+v", b.Author)
			}
			if len(b.Verdicts) != tt.wantVerdicts || len(b.PostedReviews) != tt.wantPosted {
				t.Fatalf("verdicts=%d posted=%d, want %d/%d", len(b.Verdicts), len(b.PostedReviews), tt.wantVerdicts, tt.wantPosted)
			}
			if b.Signed || b.Signature != "" {
				t.Fatalf("bundle signed without a key: %+v", b)
			}
			if h, err := evidence.Hash(b); err != nil || h != b.Hash {
				t.Fatalf("hash = %q (%v), stored %q", h, err, b.Hash)
			}
			if len(b.Policy.Perspectives) != len(review.DefaultPerspectives) {
				t.Fatalf("policy perspectives = %v, want the default set", b.Policy.Perspectives)
			}
			if f.fetches() != 1 {
				t.Fatalf("PR fetched %d times, want 1", f.fetches())
			}
		})
	}
}

func TestRecordReviewEvidenceVerdictMapping(t *testing.T) {
	root := withEvidenceRoot(t)
	c := evidenceTestClient(t, &evidencePRFixture{}, &ReviewEvidenceSettings{Enabled: true})
	medium := outputschema.Finding{Title: "nil deref", Severity: outputschema.SeverityMedium, Summary: "x may be nil", File: "a.go", Line: 3}
	general := outputschema.Finding{Title: "missing docs", Severity: outputschema.SeverityLow}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	c.recordReviewEvidence(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer"},
		[]review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "h1", medium, general)}, nil, now)

	v := mustLoadBundle(t, root, "h1").Verdicts[0]
	want := evidence.Verdict{
		Perspective: "correctness", Model: "model-x", Verdict: "approve", Confidence: 0.8, RecordedAt: now,
		Findings: []evidence.Finding{
			{Path: "a.go", Line: 3, Severity: "medium", Summary: "x may be nil"},
			{Path: reviewEvidenceGeneralPath, Severity: "low", Summary: "missing docs"},
		},
	}
	if !containsEvidenceVerdict([]evidence.Verdict{v}, want) || !v.RecordedAt.Equal(now) {
		t.Fatalf("verdict = %+v, want %+v", v, want)
	}
}

func TestRecordReviewEvidenceIdempotent(t *testing.T) {
	root := withEvidenceRoot(t)
	f := &evidencePRFixture{}
	c := evidenceTestClient(t, f, &ReviewEvidenceSettings{Enabled: true})
	req := ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer"}
	reports := []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "h1")}
	posted := &gh.PullRequestReview{HTMLURL: gh.Ptr("https://github.com/o/r/pull/7#pullrequestreview-1"), CommitID: gh.Ptr("h1")}
	first := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	c.recordReviewEvidence(context.Background(), req, reports, posted, first)
	path := ReviewEvidencePath(root, "o/r", 7, "h1")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	c.recordReviewEvidence(context.Background(), req, reports, posted, first.Add(time.Hour))
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatalf("re-run rewrote the bundle:\n%s\n---\n%s", before, after)
	}

	// A changed verdict for the same perspective is new evidence, kept beside
	// the old one.
	changed := []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictChangesRequested, "h1")}
	c.recordReviewEvidence(context.Background(), req, changed, nil, first.Add(2*time.Hour))
	b := mustLoadBundle(t, root, "h1")
	if len(b.Verdicts) != 2 || len(b.PostedReviews) != 1 {
		t.Fatalf("verdicts=%d posted=%d, want 2/1", len(b.Verdicts), len(b.PostedReviews))
	}
	if !b.CreatedAt.Equal(first) || !b.UpdatedAt.Equal(first.Add(2*time.Hour)) {
		t.Fatalf("created/updated = %v/%v", b.CreatedAt, b.UpdatedAt)
	}
	if f.fetches() != 1 {
		t.Fatalf("PR fetched %d times, want 1 (only when starting the bundle)", f.fetches())
	}
}

func TestRecordReviewEvidenceSigned(t *testing.T) {
	root := withEvidenceRoot(t)
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	keyFile := filepath.Join(t.TempDir(), "evidence.key")
	if err := os.WriteFile(keyFile, []byte(hex.EncodeToString(seed)), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	pub := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	tests := []struct {
		name       string
		keyFile    string
		wantSigned bool
	}{
		{name: "valid key signs", keyFile: keyFile, wantSigned: true},
		{name: "absent key is unsigned", keyFile: filepath.Join(t.TempDir(), "missing.key")},
		{name: "unusable key is unsigned", keyFile: writeEvidenceFile(t, "not a key")},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := evidenceTestClient(t, &evidencePRFixture{}, &ReviewEvidenceSettings{Enabled: true, SigningKeyFile: tt.keyFile})
			head := "h" + string(rune('a'+i))
			c.recordReviewEvidence(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer"},
				[]review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, head)}, nil, time.Now())
			b := mustLoadBundle(t, root, head)
			if b.Signed != tt.wantSigned {
				t.Fatalf("signed = %v, want %v", b.Signed, tt.wantSigned)
			}
			if tt.wantSigned {
				if err := evidence.Verify(b, pub); err != nil {
					t.Fatalf("Verify: %v", err)
				}
			}
		})
	}
}

func writeEvidenceFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

func TestRecordReviewEvidenceNewHeadLinksPrevious(t *testing.T) {
	root := withEvidenceRoot(t)
	c := evidenceTestClient(t, &evidencePRFixture{}, &ReviewEvidenceSettings{Enabled: true})
	req := ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer"}
	t0 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	for i, head := range []string{"h1", "h2", "h3"} {
		c.recordReviewEvidence(context.Background(), req,
			[]review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, head)}, nil, t0.Add(time.Duration(i)*time.Hour))
	}
	for head, wantPrev := range map[string]string{"h1": "", "h2": "o/r#7@h1", "h3": "o/r#7@h2"} {
		if got := mustLoadBundle(t, root, head).PreviousBundleID; got != wantPrev {
			t.Errorf("%s previous = %q, want %q", head, got, wantPrev)
		}
	}
	// The earlier bundle is untouched by the later heads.
	if b := mustLoadBundle(t, root, "h1"); !b.UpdatedAt.Equal(t0) {
		t.Fatalf("h1 updated_at = %v, want %v", b.UpdatedAt, t0)
	}
}

func TestRecordReviewEvidenceFailuresWriteNothing(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		reports []review.PerspectiveReport
		corrupt bool
	}{
		{name: "PR fetch fails for a new bundle", status: http.StatusInternalServerError,
			reports: []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "h1")}},
		{name: "head unknown and PR fetch fails", status: http.StatusNotFound,
			reports: []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "")}},
		{name: "corrupt existing bundle is left alone", corrupt: true,
			reports: []review.PerspectiveReport{evidenceReport(review.PerspectiveCorrectness, review.VerdictApprove, "h1")}},
		{name: "nothing to record"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := withEvidenceRoot(t)
			path := ReviewEvidencePath(root, "o/r", 7, "h1")
			if tt.corrupt {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			c := evidenceTestClient(t, &evidencePRFixture{status: tt.status}, &ReviewEvidenceSettings{Enabled: true})

			c.recordReviewEvidence(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer"}, tt.reports, nil, time.Now())

			data, err := os.ReadFile(path)
			switch {
			case tt.corrupt:
				if string(data) != "{not json" {
					t.Fatalf("corrupt bundle was overwritten: %q", data)
				}
			case !os.IsNotExist(err):
				t.Fatalf("bundle written, want none: %v %q", err, data)
			}
		})
	}
}

func TestEvidenceAuthor(t *testing.T) {
	withEvidenceRoot(t)
	line := `{"ts":"` + time.Now().UTC().Format(time.RFC3339) + `","action":"agent_pr_created","detail":"repo=o/r, number=9","agent":"coder"}` + "\n"
	if err := os.WriteFile(reviewVerdictAuditPath, []byte(line), 0o644); err != nil {
		t.Fatalf("write audit: %v", err)
	}
	tests := []struct {
		name   string
		number int
		user   *gh.User
		want   string
	}{
		{name: "hive agent PR", number: 9, user: &gh.User{Login: gh.Ptr("hive[bot]"), Type: gh.Ptr("Bot")}, want: evidence.AuthorAgent},
		{name: "other bot", number: 7, user: &gh.User{Login: gh.Ptr("dependabot[bot]"), Type: gh.Ptr("Bot")}, want: evidence.AuthorBot},
		{name: "bot by login suffix", number: 7, user: &gh.User{Login: gh.Ptr("renovate[bot]")}, want: evidence.AuthorBot},
		{name: "human", number: 7, user: &gh.User{Login: gh.Ptr("alice"), Type: gh.Ptr("User")}, want: evidence.AuthorHuman},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evidenceAuthor("o/r", tt.number, tt.user)
			if got.Kind != tt.want || got.Login != tt.user.GetLogin() {
				t.Fatalf("author = %+v, want kind %s", got, tt.want)
			}
		})
	}
}

func TestReviewEvidencePathStaysUnderRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "evidence")
	tests := []struct {
		name string
		repo string
		head string
		want string
	}{
		{name: "plain", repo: "o/r", head: "abc", want: filepath.Join(root, "o", "r", "7", "abc.json")},
		{name: "dots and dashes kept", repo: "my-org/repo.name", head: "abc", want: filepath.Join(root, "my-org", "repo.name", "7", "abc.json")},
		{name: "traversal collapsed", repo: "../../etc", head: "../x", want: filepath.Join(root, "unknown", "..-etc", "7", "..-x.json")},
		{name: "empty segments", repo: "", head: "", want: filepath.Join(root, "unknown", "unknown", "7", "unknown.json")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ReviewEvidencePath(root, tt.repo, 7, tt.head)
			if got != tt.want {
				t.Fatalf("path = %q, want %q", got, tt.want)
			}
			if !strings.HasPrefix(got, root+string(filepath.Separator)) {
				t.Fatalf("path %q escapes root %q", got, root)
			}
		})
	}
}

func TestSetReviewEvidenceNilClient(t *testing.T) {
	var c *Client
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })
	if _, ok := c.reviewEvidenceSettings("o/r"); ok {
		t.Fatal("nil client reported evidence enabled")
	}
}
