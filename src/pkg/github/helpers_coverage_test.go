package github

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func TestCIActivityMotionAndStale(t *testing.T) {
	if CIActivityClaimsMotion(nil) || CIActivityStale(nil, time.Now(), time.Hour) {
		t.Fatal("nil activity is neither in motion nor stale")
	}
	observed := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a := &PRCIActivity{Head: "abc", ObservedAt: &observed, Checks: []PRActivityCheck{
		{Name: "unit", State: "completed", Conclusion: "success"},
		{Name: "tide", State: "pending"},
		{Name: "DCO", State: "in_progress"},
		{Name: "stale-head", State: "in_progress", Head: "old"},
		{Name: "unknown", State: "unknown"},
		{Name: "blank", State: ""},
	}}
	if CIActivityClaimsMotion(a) {
		t.Fatal("merge gates, other heads and unknown states do not claim motion")
	}
	a.Checks = append(a.Checks, PRActivityCheck{Name: "lint", State: "queued", Head: "ABC"})
	if !CIActivityClaimsMotion(a) {
		t.Fatal("a queued check on the head claims motion")
	}
	later := observed.Add(2 * time.Hour)
	if !CIActivityStale(a, later, time.Hour) {
		t.Fatal("two hours of claimed motion is stale")
	}
	if CIActivityStale(a, later, 0) || CIActivityStale(a, observed.Add(time.Minute), time.Hour) {
		t.Fatal("disabled guard or fresh snapshot is not stale")
	}
	none := &PRCIActivity{Head: "abc", ObservedAt: &observed, Checks: []PRActivityCheck{{Name: "unit", State: "completed"}}}
	if CIActivityStale(none, later, time.Hour) {
		t.Fatal("a finished snapshot never goes stale")
	}
	zero := time.Time{}
	if CIActivityStale(&PRCIActivity{ObservedAt: &zero, Checks: a.Checks}, later, time.Hour) || CIActivityStale(&PRCIActivity{Checks: a.Checks}, later, time.Hour) {
		t.Fatal("an unobserved snapshot cannot be stale")
	}
}

func TestExportedCIContextHelpers(t *testing.T) {
	if !IsDCOContext(" DCO ") || !IsDCOContext("dco") || IsDCOContext("docs") {
		t.Fatal("dco context")
	}
	if !CommitCIReasonIsPending("pending: unit") {
		t.Fatal("pending reason")
	}
	if CommitCIReasonIsPending("checks failing: unit") {
		t.Fatal("failing reason is not pending")
	}
	if !(PRMergeState{MergeableState: " Dirty "}).Conflicting() || (PRMergeState{MergeableState: "clean"}).Conflicting() || (PRMergeState{}).Conflicting() {
		t.Fatal("conflicting")
	}
	if !IsForwardMergeHead("sync-v4-to-v5") || !IsForwardMergeHead("topup/v5") || IsForwardMergeHead("feature/topup") || IsForwardMergeHead("main") {
		t.Fatal("forward-merge head")
	}
}

func TestIsPersonLogin(t *testing.T) {
	str := func(s string) *string { return &s }
	cases := []struct {
		u    *gh.User
		app  string
		want bool
	}{
		{nil, "", false},
		{&gh.User{Login: str("")}, "", false},
		{&gh.User{Login: str("andy")}, "", true},
		{&gh.User{Login: str("andy")}, "hive-app[bot]", true},
		{&gh.User{Login: str("Hive-App[bot]")}, "", false},
		{&gh.User{Login: str("robot"), Type: str("Bot")}, "", false},
		{&gh.User{Login: str("hive")}, " HIVE ", false},
	}
	for i, c := range cases {
		if got := IsPersonLogin(c.u, c.app); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

type fakeNetErr struct{}

func (fakeNetErr) Error() string   { return "net" }
func (fakeNetErr) Timeout() bool   { return true }
func (fakeNetErr) Temporary() bool { return true }

var _ net.Error = fakeNetErr{}

func TestIsRetryableError(t *testing.T) {
	resp := func(code int) *http.Response { return &http.Response{StatusCode: code} }
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("nope"), false},
		{context.DeadlineExceeded, true},
		{fakeNetErr{}, true},
		{&gh.RateLimitError{}, true},
		{&gh.AbuseRateLimitError{}, true},
		{&gh.ErrorResponse{Response: resp(http.StatusTooManyRequests)}, true},
		{&gh.ErrorResponse{Response: resp(http.StatusBadGateway)}, true},
		{&gh.ErrorResponse{Response: resp(http.StatusNotFound)}, false},
		{&gh.ErrorResponse{}, false},
	}
	for i, c := range cases {
		if got := IsRetryableError(c.err); got != c.want {
			t.Errorf("case %d (%v): %v", i, c.err, got)
		}
	}
}

func TestDefaultMergeAttemptsRegistry(t *testing.T) {
	repo, number := "acme/widget-merge-attempts-test", 4242
	if _, ok := MergeAttemptFor(repo, number); ok {
		t.Fatal("fresh key already present")
	}
	BeginMergeAttempt(repo, number, "lane")
	got, ok := MergeAttemptFor(repo, number)
	if !ok || got.Repo != repo || got.Number != number {
		t.Fatalf("after begin: %+v %v", got, ok)
	}
	found := false
	for _, a := range MergeAttemptsSnapshot() {
		if a.Repo == repo && a.Number == number {
			found = true
		}
	}
	if !found || DefaultMergeAttempts() == nil {
		t.Fatal("snapshot lacks the attempt")
	}
	EndMergeAttempt(repo, number, true, nil)
}
