package apphealth

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
)

// These verdicts were unreachable from outside cmd/hive before #7238. A nil
// AppAuth is diagnosed as healthy with no API round-trip, which makes the
// attribution rules -- the part that decides what an operator is told to go
// fix -- testable without a GitHub server.

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestClassifyWriteForbiddenAttributesHealthyInstallationToRepoScope is the
// #2353 rule. A write returned 403 while the installation itself diagnoses
// healthy, so the cause must be attributed to repo scope -- and the copy must
// NOT invent a permission gap the diagnosis just proved absent, which is the
// false "lacks Issues: Read & Write" banner that rule exists to prevent.
func TestClassifyWriteForbiddenAttributesHealthyInstallationToRepoScope(t *testing.T) {
	var c Checker
	msg, state := c.ClassifyWriteForbidden(context.Background(), nil, "hivecommons", "hive")

	if state != github.AppStateWriteForbidden {
		t.Fatalf("a real write 403 against a healthy installation must be AppStateWriteForbidden, got %v", state)
	}
	if !strings.Contains(msg, "hive") {
		t.Fatalf("the verdict must name the repo so the fix is concrete, got %q", msg)
	}
	if !strings.Contains(msg, "selected repositories") {
		t.Fatalf("the verdict must attribute the failure to repo scope, got %q", msg)
	}
	// The copy affirms the permission IS held ("holds Issues: Read & Write").
	// What it must never do is claim one is absent -- that is the false
	// permission-gap banner #2353 replaced -- or send the operator off to
	// re-upload a key the diagnosis just proved fine.
	for _, invented := range []string{"lacks", "missing", "does not have", "private key", "installation_id"} {
		if strings.Contains(msg, invented) {
			t.Fatalf("must not invent a cause the diagnosis disproved (%q) in: %q", invented, msg)
		}
	}
}

// TestClassifyWriteForbiddenNeverReportsHealthy guards the other half of
// #2353: the write failure is real, so it must never come back clean.
func TestClassifyWriteForbiddenNeverReportsHealthy(t *testing.T) {
	var c Checker
	msg, state := c.ClassifyWriteForbidden(context.Background(), nil, "hivecommons", "hive")

	if state == github.AppStateOK {
		t.Fatal("a write that returned 403 must not be reported as healthy")
	}
	if msg == "" {
		t.Fatal("a raised verdict with no message leaves the operator nothing to act on")
	}
}

func TestClassifyRepoCoverageDefersWithoutInputs(t *testing.T) {
	tests := []struct {
		name  string
		repos []string
	}{
		{"nil app auth and no repos", nil},
		{"nil app auth with repos", []string{"hive"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raise, msg, state := ClassifyRepoCoverage(context.Background(), nil, "hivecommons", tt.repos, quietLogger())
			if raise {
				t.Fatalf("coverage cannot be judged without an installation; must defer, got msg=%q", msg)
			}
			if state != github.AppStateUnknown {
				t.Fatalf("deferring means AppStateUnknown, got %v", state)
			}
			if msg != "" {
				t.Fatalf("a deferred verdict must carry no banner copy, got %q", msg)
			}
		})
	}
}

// TestClassifyRepoCoverageEmptyRepoListIsNotAVerdict pins #4360's guard: a
// hive configured with no repos has nothing to be missing, and must not be
// told its App is broken.
func TestClassifyRepoCoverageEmptyRepoListIsNotAVerdict(t *testing.T) {
	raise, _, _ := ClassifyRepoCoverage(context.Background(), nil, "hivecommons", []string{}, quietLogger())
	if raise {
		t.Fatal("no configured repos means no coverage claim to make")
	}
}

func TestDiagnoseNilAppAuthIsHealthy(t *testing.T) {
	var c Checker
	d := c.Diagnose(context.Background(), nil, "hivecommons")

	if d.State != github.AppStateOK {
		t.Fatalf("a token-authenticated hive has no App to fault, got %v", d.State)
	}
	if d.ExpectedAccount != "hivecommons" {
		t.Fatalf("the expected account must be carried through, got %q", d.ExpectedAccount)
	}
}

func TestDiagnoseMessageNilAppAuthIsSilent(t *testing.T) {
	var c Checker
	msg, state := c.DiagnoseMessage(context.Background(), nil, "hivecommons")

	if state != github.AppStateOK {
		t.Fatalf("want AppStateOK, got %v", state)
	}
	if msg != "" {
		t.Fatalf("a healthy hive must raise no banner copy, got %q", msg)
	}
}

// TestClassifyFailureNeverRaisesOnHealthy is rule 1 of the #2224 design: only
// a genuinely actionable state raises the banner.
func TestClassifyFailureNeverRaisesOnHealthy(t *testing.T) {
	var c Checker
	raise, msg, state := c.ClassifyFailure(context.Background(), nil, "hivecommons", quietLogger())

	if raise {
		t.Fatalf("a healthy App must not raise the banner, got msg=%q", msg)
	}
	if state != github.AppStateOK {
		t.Fatalf("want AppStateOK, got %v", state)
	}
	if msg != "" {
		t.Fatalf("no banner means no copy, got %q", msg)
	}
}

// TestClassifyFailureHonorsCancelledContext proves the retry loop cannot pin a
// shutting-down process for the full backoff.
func TestClassifyFailureHonorsCancelledContext(t *testing.T) {
	c := Checker{BannerAttempts: 5, RetryDelay: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		c.ClassifyFailure(ctx, nil, "hivecommons", quietLogger())
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ClassifyFailure ignored a cancelled context and waited on the retry backoff")
	}
}

func TestCheckerDefaultsAreApplied(t *testing.T) {
	var zero Checker
	if got := zero.attempts(); got != defaultBannerAttempts {
		t.Fatalf("zero Checker must probe %d times, got %d", defaultBannerAttempts, got)
	}
	if got := zero.retryDelay(); got != defaultBannerRetryDelay {
		t.Fatalf("zero Checker must use the default delay %v, got %v", defaultBannerRetryDelay, got)
	}

	override := Checker{BannerAttempts: 7, RetryDelay: 250 * time.Millisecond}
	if got := override.attempts(); got != 7 {
		t.Fatalf("override ignored: attempts=%d", got)
	}
	if got := override.retryDelay(); got != 250*time.Millisecond {
		t.Fatalf("override ignored: delay=%v", got)
	}
}

// TestDefaultBannerAttemptsRetriesAtLeastOnce pins the reason the constant is
// 2 and not 1: a cold start races DNS/egress readiness, and one retry is what
// converts that transient into a correct verdict. Asserted as a literal so
// changing the constant cannot move both sides of the check.
func TestDefaultBannerAttemptsRetriesAtLeastOnce(t *testing.T) {
	if defaultBannerAttempts < 2 {
		t.Fatalf("a single attempt makes every cold-start blip a false verdict; got %d", defaultBannerAttempts)
	}
}
