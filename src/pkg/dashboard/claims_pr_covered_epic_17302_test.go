package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/claims"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/issueclaim"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The numbers and times of the 2026-11-08 incident (hivecommons/hive#17292):
// the hive filed #17243, a contributor claimed it, their #17248 covered it,
// the lease was revoked, and an agent working from epic #17292 opened and
// merged #17250 on the same issue.
const (
	coveredRepo      = "myorg/repo1"
	coveredIssue     = 17243
	contributorPR    = 17248
	epicIssue        = 17292
	contributorLogin = "Karibhou"
)

// claimTargetForge is the slice of GitHub the PR-request watcher reads and
// writes for coveredRepo. It records what the watcher would publish: every
// PR it opens and every comment it posts, keyed by issue/PR number. List
// endpoints answer empty, so CreatePR's head-branch and duplicate-PR checks
// stay out of the story — the claim-target gate must refuse before CreatePR.
type claimTargetForge struct {
	mu       sync.Mutex
	opened   []string
	comments map[int][]string
}

func (f *claimTargetForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const repoPath = "/repos/" + coveredRepo
	w.Header().Set("Content-Type", "application/json")
	i := strings.Index(r.URL.Path, repoPath)
	if i < 0 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rest := r.URL.Path[i+len(repoPath):]
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && rest == "":
		_, _ = io.WriteString(w, `{"name":"repo1","full_name":"`+coveredRepo+`","default_branch":"main"}`)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/compare/"):
		_, _ = io.WriteString(w, `{"files":[]}`)
	case r.Method == http.MethodGet && rest == "/pulls":
		_, _ = io.WriteString(w, `[]`)
	case r.Method == http.MethodGet && rest == "/pulls/"+strconv.Itoa(contributorPR):
		_, _ = fmt.Fprintf(w, `{"number":%d,"state":"open","merged":false,"user":{"login":%q},"html_url":"https://github.com/%s/pull/%d"}`,
			contributorPR, contributorLogin, coveredRepo, contributorPR)
	case r.Method == http.MethodPost && rest == "/pulls":
		var pr struct {
			Title string `json:"title"`
		}
		_ = json.NewDecoder(r.Body).Decode(&pr)
		f.opened = append(f.opened, pr.Title)
		_, _ = fmt.Fprintf(w, `{"number":17250,"html_url":"https://github.com/%s/pull/17250"}`, coveredRepo)
	case strings.HasPrefix(rest, "/issues/") && strings.HasSuffix(rest, "/comments"):
		n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(rest, "/issues/"), "/comments"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		var c struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&c)
		f.comments[n] = append(f.comments[n], c.Body)
		_, _ = fmt.Fprintf(w, `{"id":%d,"body":%q}`, len(f.comments[n]), c.Body)
	case r.Method == http.MethodPost && strings.HasSuffix(rest, "/labels"):
		_, _ = io.WriteString(w, `[]`)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/issues/"):
		n, err := strconv.Atoi(strings.TrimPrefix(rest, "/issues/"))
		if err != nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		label := "help wanted"
		if n == epicIssue {
			label = "epic"
		}
		_, _ = fmt.Fprintf(w, `{"number":%d,"title":"crux: per-run command log","body":"Keep an append-only command log for each contained run.","state":"open","labels":[{"name":%q}],"user":{"login":"hivecommons-hive[bot]","type":"Bot"}}`, n, label)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *claimTargetForge) snapshot() (opened []string, comments map[int][]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	comments = make(map[int][]string, len(f.comments))
	for n, bodies := range f.comments {
		comments[n] = append([]string(nil), bodies...)
	}
	return append([]string(nil), f.opened...), comments
}

// hubLedgerClaimLookup hands the hub's claim ledger to the GitHub client the
// way cmd/hive wires it (claimswire.go, ledgerClaimLookup): the live claim on
// an issue, read per call.
func hubLedgerClaimLookup(l *claims.Ledger) func(repo string, number int) (issueclaim.Claim, bool) {
	return func(repo string, number int) (issueclaim.Claim, bool) {
		c, ok := l.Lookup(repo, number)
		if !ok {
			return issueclaim.Claim{}, false
		}
		return issueclaim.Claim{Identity: c.Holder, StartedAt: c.ClaimedAt, ExpiresAt: c.ExpiresAt,
			Source: issueclaim.SourceLease, Kind: string(c.Kind), Hive: c.Hive, Tier: c.Tier, Verified: c.Verified}, true
	}
}

// hivecommons/hive#17302 (from #17292): replays the incident end to end
// against the hub's claim ledger, the contributor lease hub and the
// PR-request watcher. A contributor claims a help-wanted issue, their PR
// covers it, their lease completes — and a hive agent working from an epic
// then asks to open a PR that closes the same issue. The claim must outlive
// the lease while the contributor's PR covers the issue (#17300), and the
// watcher must refuse the epic-driven PR with one notice on the issue naming
// the contributor (#17298). Before both fixes the watcher opened the second
// implementation (#17250).
func TestClaims_ContributorPRCoveredIssueRefusesEpicDrivenPR(t *testing.T) {
	// One injected clock drives the hub ledger, the duplicate-PR ledger and
	// the watcher; hooks may read it off the test goroutine, hence atomic.
	var nowNanos atomic.Int64
	setNow := func(hh, mm, ss int) time.Time {
		at := time.Date(2026, 11, 8, hh, mm, ss, 0, time.UTC)
		nowNanos.Store(at.UnixNano())
		return at
	}
	clock := func() time.Time { return time.Unix(0, nowNanos.Load()).UTC() }
	now := setNow(15, 11, 2)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	hub, s, ledger := claimsHub(t)
	ledger.SetNow(clock)
	prLedger := ghpkg.NewClaimLedger(filepath.Join(t.TempDir(), "pr-claims.json"), logger)
	prLedger.SetClock(clock)
	s.deps.IssueClaimed = prLedger.Lookup

	forge := &claimTargetForge{comments: map[int][]string{}}
	srv := httptest.NewServer(forge)
	t.Cleanup(srv.Close)
	client := ghpkg.NewClientForTest(srv.URL, "myorg", []string{coveredRepo}, logger)
	client.SetIssueClaimLedger(hubLedgerClaimLookup(ledger))
	client.SetPRClaimLookup(prLedger.Lookup)

	// The stage the enumeration derives for the issue from the duplicate-PR
	// ledger: the duplicate-PR guard, then the stage reconciler's input.
	stageOfIssue := func() stagelabels.Stage {
		t.Helper()
		result := &ghpkg.ActionableResult{}
		result.Issues = ghpkg.IssueResultFromItems([]ghpkg.Issue{{Repo: coveredRepo, Number: coveredIssue, Labels: []string{"help wanted"}}})
		ghpkg.FilterClaimedIssues(result, prLedger, nil, logger)
		inputs := ghpkg.IssueStageInputs(result)
		if len(inputs) != 1 {
			t.Fatalf("stage inputs = %+v, want one row for #%d", inputs, coveredIssue)
		}
		stage, _, _ := ghpkg.IssueStageFor(inputs[0], func(string, int) stagelabels.Stage { return stagelabels.ReviewPending })
		return stage
	}

	// 1. The hive filed the issue: unclaimed, help wanted.
	if c, ok := ledger.Lookup(coveredRepo, coveredIssue); ok {
		t.Fatalf("issue starts claimed: %+v", c)
	}

	// 2. The contributor is assigned the issue: lease plus ledger claim.
	conn := &ContributorConnection{
		profile:  &ContributorProfile{GitHubUsername: contributorLogin, ContributorID: "c-742b084db8ab", TrustTier: "contributor"},
		session:  "omp",
		lastPong: now,
	}
	identity := identityOf(conn)
	if err := hub.recordLease(identity, "t-17243", coveredRepo, coveredIssue, "contributor", 1, now); err != nil {
		t.Fatal(err)
	}
	hub.claimIssueForContributor(conn, coveredRepo, coveredIssue)
	if c, ok := ledger.Lookup(coveredRepo, coveredIssue); !ok || c.HolderID != identity {
		t.Fatalf("contributor claim = %+v ok=%v, want held by %s", c, ok, identity)
	}

	// 3. The contributor's PR opens with Closes #N; the issue is covered.
	now = setNow(15, 26, 49)
	delivery := ghpkg.PRClaimDelivery{
		Repo: coveredRepo, Number: contributorPR, Action: "opened", State: ghpkg.PRStateOpen, At: now,
		Claims: ghpkg.ClaimsFromPROpened(ghpkg.PROpenedDetail{
			Repo: coveredRepo, Number: contributorPR, Author: contributorLogin, Head: "karibhou/crux-cmdlog",
			URL:   fmt.Sprintf("https://github.com/%s/pull/%d", coveredRepo, contributorPR),
			Title: "✨ crux: per-run command log", Body: fmt.Sprintf("Closes #%d", coveredIssue),
		}, []string{coveredRepo}, ghpkg.HiveIdentity{AppLogin: "hivecommons-hive[bot]"}, now),
	}
	if recorded, _ := prLedger.ApplyPRDelivery(delivery); recorded != 1 {
		t.Fatalf("duplicate-PR ledger recorded %d claims from #%d, want 1", recorded, contributorPR)
	}
	if stage := stageOfIssue(); stage != stagelabels.CoveredByPR {
		t.Fatalf("stage with the contributor's PR open = %s, want %s", stage, stagelabels.CoveredByPR)
	}

	// 4. The contributor's lease completes. The work is not abandoned — their
	// PR covers it — so the ledger still shows the issue held by them.
	setNow(15, 27, 45)
	hub.revokeLease(identity, "t-17243")
	if c, ok := ledger.Lookup(coveredRepo, coveredIssue); !ok || c.HolderID != identity || c.Holder != contributorLogin ||
		c.Kind != claims.KindContributor || !c.PRStarted {
		t.Errorf("claim after the lease completed = %+v ok=%v, want a PR-held claim by %s (#17300)", c, ok, contributorLogin)
	}
	if stage := stageOfIssue(); stage != stagelabels.CoveredByPR {
		t.Errorf("stage after the lease completed = %s, want %s", stage, stagelabels.CoveredByPR)
	}

	// 5. A hive agent working from the epic asks to open a PR that closes the
	// issue. It never claimed the issue (no --issues), so nothing upstream of
	// the watcher stopped it.
	setNow(15, 34, 26)
	dir := t.TempDir()
	reqPath, err := ghpkg.WritePRRequest(dir, ghpkg.PRRequest{
		Repo: coveredRepo, Head: "scanner/crux-cmdlog-s4", Agent: "scanner",
		Title: "✨ crux: per-run command log",
		Body:  fmt.Sprintf("Fixes #%d\n\nPart of #%d (S4 — command log).", coveredIssue, epicIssue),
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ProcessPRRequestDirOnce(context.Background(), dir, func(string, int) error { return nil }, clock)

	opened, comments := forge.snapshot()
	if len(opened) != 0 {
		t.Errorf("watcher opened %q although %s's claim and PR #%d cover #%d (#17298)", opened, contributorLogin, contributorPR, coveredIssue)
	}
	raw, err := os.ReadFile(strings.TrimSuffix(reqPath, ".json") + ".result.json")
	if err != nil {
		t.Fatalf("request result: %v", err)
	}
	var res ghpkg.PRResponse
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if res.OK || !strings.Contains(res.Error, "claim") {
		t.Errorf("request result = %+v, want a claim refusal", res)
	}
	notices := comments[coveredIssue]
	if len(notices) != 1 || !strings.Contains(strings.ToLower(notices[0]), strings.ToLower(contributorLogin)) {
		t.Errorf("comments on #%d = %q, want one notice naming %s", coveredIssue, notices, contributorLogin)
	}
	for n, bodies := range comments {
		if n != coveredIssue {
			t.Errorf("unexpected comments on #%d: %q", n, bodies)
		}
	}
}
