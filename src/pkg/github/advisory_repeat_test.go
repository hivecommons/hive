package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/advisory"
)

// End-to-end coverage for #5507 through PostAdvisoryDigest: the ~250-comment
// runaway on ibm/alchemy-logging#686 must not be reproducible.

// repeatDigestServer models a target issue that already carries one digest
// comment and records every write the client attempts. Unlike
// digestCountingServer it serves the CURRENT body back on the comment list, so
// the #5507 suppression has the real baseline it uses in production.
type repeatDigestServer struct {
	*httptest.Server
	body       string    // current digest comment body on the issue
	updatedAt  time.Time // when it was last written
	editCalls  int
	createDone int
}

func newRepeatDigestServer(t *testing.T, org, repo, initialBody string, updatedAt time.Time) *repeatDigestServer {
	t.Helper()
	s := &repeatDigestServer{body: initialBody, updatedAt: updatedAt}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "GET":
			out := []map[string]any{}
			if s.body != "" {
				out = append(out, map[string]any{
					"id":         float64(555),
					"body":       s.body,
					"updated_at": s.updatedAt.UTC().Format(time.RFC3339),
					"created_at": s.updatedAt.UTC().Format(time.RFC3339),
				})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "POST":
			s.createDone++
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if b, ok := in["body"].(string); ok {
				s.body = b
			}
			s.updatedAt = time.Now()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 556})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/555", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			s.editCalls++
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if b, ok := in["body"].(string); ok {
				s.body = b
			}
			s.updatedAt = time.Now()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 555})
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// writes is every forge write the client performed.
func (s *repeatDigestServer) writes() int { return s.editCalls + s.createDone }

func emptyDigestAt(at time.Time) string {
	return advisory.FormatDigestMarkdown(&advisory.Digest{
		GeneratedAt: at,
		Mode:        "advisory",
		ByAgent:     map[string][]advisory.Finding{},
		TotalCount:  0,
	}, advisory.DigestOptions{ShowEmpty: true, Org: "testorg", PrimaryRepo: "testrepo"})
}

// TestPostAdvisoryDigest_LoginBlockedSpokeStopsRepeating reproduces the
// reported conditions: nothing changes for days, and the governor renders a
// fresh zero-finding digest every 4h. Before #5507 every cycle wrote, because
// the GeneratedAt stamp made each body's hash unique. Now only the first
// writes.
func TestPostAdvisoryDigest_LoginBlockedSpokeStopsRepeating(t *testing.T) {
	org, repo := "testorg", "testrepo"
	start := time.Now().Add(-4 * time.Hour)
	srv := newRepeatDigestServer(t, org, repo, emptyDigestAt(start), start)
	c := newTestClient(t, srv.Server, org, []string{repo})

	// 12 cycles at the observed ~4h cadence = two days of a wedged spoke.
	const cycles = 12
	for i := 0; i < cycles; i++ {
		body := emptyDigestAt(start.Add(time.Duration(i+1) * 4 * time.Hour))
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, body); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if srv.writes() != 0 {
		t.Errorf("wedged spoke wrote %d times over %d cycles, want 0 — the repeat-post bug is still live (edits=%d creates=%d)",
			srv.writes(), cycles, srv.editCalls, srv.createDone)
	}
	if srv.createDone != 0 {
		t.Errorf("createDone = %d, want 0 — new comments are exactly what accumulated to ~250 on the reported issue", srv.createDone)
	}
}

// TestPostAdvisoryDigest_RunawayCreateIsSuppressed models the mechanism that
// turned repeats into 250 SEPARATE comments rather than 250 edits: on an issue
// whose comment list no longer surfaces an adoptable digest comment, the post
// path falls to the CREATE branch every cycle. Suppression must stop it there
// too, because that branch is the one that notifies every subscriber.
func TestPostAdvisoryDigest_RunawayCreateIsSuppressed(t *testing.T) {
	org, repo := "testorg", "testrepo"
	at := time.Now().Add(-time.Hour)
	posted := emptyDigestAt(at)
	srv := newRepeatDigestServer(t, org, repo, posted, at)
	c := newTestClient(t, srv.Server, org, []string{repo})

	// Materially identical to what is already there, only a later stamp.
	next := emptyDigestAt(time.Now())
	if next == posted {
		t.Fatal("fixture bodies are identical; the test would pass without exercising the fingerprint")
	}
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, next); err != nil {
		t.Fatalf("post: %v", err)
	}
	if srv.writes() != 0 {
		t.Errorf("writes = %d, want 0 (edits=%d creates=%d)", srv.writes(), srv.editCalls, srv.createDone)
	}
}

// findingDigestAt renders a digest carrying one UNCHANGING finding at the
// given generation time. The finding set never varies, so successive renders
// differ only by the GeneratedAt stamp.
func findingDigestAt(at time.Time) string {
	return advisory.FormatDigestMarkdown(&advisory.Digest{
		GeneratedAt: at,
		Mode:        "advisory",
		ByAgent: map[string][]advisory.Finding{"quality": {{
			Agent: "quality", Timestamp: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			Type: "coverage-gap", Severity: "high",
			Title: "uncovered error path in PostAdvisoryDigest",
		}}},
		TotalCount: 1,
	}, advisory.DigestOptions{Org: "testorg", PrimaryRepo: "testrepo"})
}

// TestPostAdvisoryDigest_UnchangedFindingsStopRepeating is the timestamp-
// exclusion assertion at the POST-PATH level. It deliberately uses a
// NON-zero-finding digest so the zero-finding cap cannot fire: the only thing
// that can suppress these repeats is the material fingerprint ignoring
// GeneratedAt. If a timestamp ever leaks back into the hash, this fails.
func TestPostAdvisoryDigest_UnchangedFindingsStopRepeating(t *testing.T) {
	org, repo := "testorg", "testrepo"
	start := time.Now().Add(-24 * time.Hour)
	posted := findingDigestAt(start)
	srv := newRepeatDigestServer(t, org, repo, posted, start)
	c := newTestClient(t, srv.Server, org, []string{repo})

	if isZeroFindingDigest(posted) {
		t.Fatal("fixture is a zero-finding digest; the zero-finding cap would mask a timestamp leak and this test would prove nothing")
	}

	const cycles = 6
	for i := 0; i < cycles; i++ {
		body := findingDigestAt(start.Add(time.Duration(i+1) * 4 * time.Hour))
		if body == posted {
			t.Fatal("renders are byte-identical; the #4818 in-memory hash would skip these and the material fingerprint would go untested")
		}
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, body); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
	}
	if srv.writes() != 0 {
		t.Errorf("an unchanging 1-finding digest wrote %d times over %d cycles, want 0 — a timestamp is leaking into the material fingerprint (edits=%d creates=%d)",
			srv.writes(), cycles, srv.editCalls, srv.createDone)
	}
}

// TestPostAdvisoryDigest_NewFindingsBreakThroughSuppression is the safety
// counterweight through the real post path: when findings actually appear, the
// digest must reach the issue immediately.
func TestPostAdvisoryDigest_NewFindingsBreakThroughSuppression(t *testing.T) {
	org, repo := "testorg", "testrepo"
	at := time.Now().Add(-time.Hour)
	srv := newRepeatDigestServer(t, org, repo, emptyDigestAt(at), at)
	c := newTestClient(t, srv.Server, org, []string{repo})

	withFindings := advisory.FormatDigestMarkdown(&advisory.Digest{
		GeneratedAt: time.Now(),
		Mode:        "advisory",
		ByAgent: map[string][]advisory.Finding{"quality": {{
			Agent: "quality", Timestamp: time.Now(), Type: "coverage-gap",
			Severity: "high", Title: "uncovered error path in PostAdvisoryDigest",
		}}},
		TotalCount: 1,
	}, advisory.DigestOptions{Org: org, PrimaryRepo: repo})

	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, withFindings); err != nil {
		t.Fatalf("post with findings: %v", err)
	}
	if srv.writes() != 1 {
		t.Fatalf("a digest that gained a finding wrote %d times, want 1 — real findings must never be suppressed", srv.writes())
	}
}

// TestPostAdvisoryDigest_FirstDigestOnEmptyTargetPosts guards the other
// direction: a target with no digest comment at all must always receive one,
// or a fresh advisory issue would stay permanently empty.
func TestPostAdvisoryDigest_FirstDigestOnEmptyTargetPosts(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newRepeatDigestServer(t, org, repo, "", time.Time{})
	c := newTestClient(t, srv.Server, org, []string{repo})

	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, emptyDigestAt(time.Now())); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if srv.createDone != 1 {
		t.Errorf("createDone = %d, want 1 — the first digest on a bare target must post", srv.createDone)
	}
}

// TestPostAdvisoryDigest_WriteThroughOverridesSuppression protects the #4818
// permission-regression probe. That probe works by performing a REAL write
// every advisoryDigestWriteThroughInterval cycles, so a 403 (App dropped from
// the installation, issues:write revoked) surfaces instead of hiding behind a
// quiet digest. #5507 suppression would otherwise swallow exactly that write —
// it is the most suppressible cycle there is, since the body is unchanged —
// and the regression would go invisible forever.
func TestPostAdvisoryDigest_WriteThroughOverridesSuppression(t *testing.T) {
	org, repo := "testorg", "testrepo"
	at := time.Now().Add(-time.Hour)
	body := findingDigestAt(at)
	srv := newRepeatDigestServer(t, org, repo, body, at)
	c := newTestClient(t, srv.Server, org, []string{repo})

	// Feed the SAME body repeatedly so the #4818 in-memory hash matches and
	// the skip streak advances to the write-through boundary.
	total := advisoryDigestWriteThroughInterval + 1
	for i := 0; i < total; i++ {
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, body); err != nil {
			t.Fatalf("cycle %d: %v", i+1, err)
		}
	}
	if srv.writes() == 0 {
		t.Fatalf("after %d unchanged cycles the periodic write-through never wrote — a 403 permission regression would now be invisible", total)
	}
}

// TestPostAdvisoryDigest_SuppressionSurvivesRestart is the restart-behaviour
// assertion from the issue. State kept only in process memory would be lost by
// a restarting governor — and login-blocked spokes, the reported failure
// environment, restart often. A brand-new Client (a fresh process, empty
// in-memory maps) must reach the same suppression decision, because the
// baseline is the comment on the target rather than anything remembered.
func TestPostAdvisoryDigest_SuppressionSurvivesRestart(t *testing.T) {
	org, repo := "testorg", "testrepo"
	at := time.Now().Add(-2 * time.Hour)
	srv := newRepeatDigestServer(t, org, repo, emptyDigestAt(at), at)

	for i := 0; i < 5; i++ {
		// A NEW client each iteration: every in-memory guard (#4818 hashes,
		// skip counters) starts empty, exactly as after a process restart.
		c := newTestClient(t, srv.Server, org, []string{repo})
		if c.advisoryDigestHashes != nil {
			t.Fatal("fresh client already carries digest hashes; this test would not model a restart")
		}
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, emptyDigestAt(time.Now())); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
	}
	if srv.writes() != 0 {
		t.Errorf("after 5 restarts: writes = %d, want 0 — suppression is not surviving restart, so a crash-looping spoke resumes spamming", srv.writes())
	}
}
