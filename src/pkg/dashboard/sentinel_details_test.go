package dashboard

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/sentinel"
)

// TestSentinelDetailBoundsAndText: a finding's paths are sorted, bounded
// to SentinelDetailMaxPaths with the rest counted, and rendered on one line
// per rule for the hive's comment.
func TestSentinelDetailBoundsAndText(t *testing.T) {
	var many []string
	for i := 0; i < SentinelDetailMaxPaths+3; i++ {
		many = append(many, fmt.Sprintf("p/%02d.yml", SentinelDetailMaxPaths+3-i))
	}
	d := sentinelDetailOf(sentinel.RuleSensitivePath, fmt.Sprintf("changes %d sensitive path(s)", len(many)), many)
	if len(d.Paths) != SentinelDetailMaxPaths || d.MorePaths != 3 {
		t.Fatalf("bound: %d paths, %d more", len(d.Paths), d.MorePaths)
	}
	if d.Paths[0] != "p/01.yml" || d.Paths[SentinelDetailMaxPaths-1] != fmt.Sprintf("p/%02d.yml", SentinelDetailMaxPaths) {
		t.Fatalf("paths not sorted/bounded: %v", d.Paths)
	}
	if got := sentinelDetailsText([]SentinelDetail{d}); !strings.HasPrefix(got, "sensitive_path: p/01.yml, p/02.yml") || !strings.HasSuffix(got, " (+3 more)") {
		t.Fatalf("text = %q", got)
	}

	two := sentinelDetailsFromSweep(github.SentinelSweepEvent{Findings: []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "changes 2 sensitive path(s)", Paths: []string{"OWNERS", ".github/workflows/ci.yml"}},
		{Rule: sentinel.RulePermissionEscalation, Summary: "widens workflow permissions"},
	}})
	want := "sensitive_path: .github/workflows/ci.yml, OWNERS; permission_escalation (widens workflow permissions)"
	if got := sentinelDetailsText(two); got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	if sentinelDetailsText(nil) != "" || sentinelDetailsFromSweep(github.SentinelSweepEvent{}) != nil || sentinelDetailsFromEvidence(nil) != nil {
		t.Fatal("empty input must render nothing")
	}
}

// TestSentinelRingCarriesPathsForDecision: the sweep's audit hook stores
// each finding's paths; /api/sentinel serves them and the decision path
// reads them back by repo#number@head without touching GitHub.
func TestSentinelRingCarriesPathsForDecision(t *testing.T) {
	s := covApiServer(t)
	ev := github.SentinelSweepEvent{Repo: "acme/console", Number: 16230, Author: "m", HeadSHA: "4396f46abc", Blocked: true, Findings: []sentinel.Finding{
		{Rule: sentinel.RuleSensitivePath, Summary: "changes 1 sensitive path(s)", Paths: []string{".github/workflows/release.yml"}},
	}}
	s.RecordSentinelFinding(ev)

	rec := doGet(s, "/api/sentinel?repo=acme/console")
	var payload struct {
		Findings []SentinelFinding `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || len(payload.Findings) != 1 {
		t.Fatalf("sentinel = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if got := payload.Findings[0].Findings; len(got) != 1 || got[0].Rule != sentinel.RuleSensitivePath || !reflect.DeepEqual(got[0].Paths, []string{".github/workflows/release.yml"}) {
		t.Fatalf("api findings = %+v", got)
	}

	if got := s.sentinelDetailsFor("ACME/console", 16230, "4396F46ABC"); len(got) != 1 || got[0].Paths[0] != ".github/workflows/release.yml" {
		t.Fatalf("detailsFor = %+v", got)
	}
	if got := s.sentinelDetailsFor("acme/console", 16230, "other"); got != nil {
		t.Fatalf("other head = %+v", got)
	}
	// The delegated decision's comment names the files after the row's words.
	want := "changes 1 sensitive path(s) — sensitive_path: .github/workflows/release.yml"
	if got := s.delegatedSentinelReason("acme/console", 16230, "4396f46abc", "changes 1 sensitive path(s)"); got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if got := s.delegatedSentinelReason("acme/console", 16230, "4396f46abc", ""); got != "sensitive_path: .github/workflows/release.yml" {
		t.Fatalf("reason without row words = %q", got)
	}
	if got := s.delegatedSentinelReason("acme/console", 1, "nohead", "changes 1 sensitive path(s)"); got != "changes 1 sensitive path(s)" {
		t.Fatalf("unknown head keeps the row's words: %q", got)
	}
}

// TestStageTrackerSentinelDetailsFromBundleOrRing: the tracker row's
// sentinel_details come from the verified evidence bundle for the head,
// or from the sweep's ring when no bundle was written, so the decision
// dialog can name the files either way.
func TestStageTrackerSentinelDetailsFromBundleOrRing(t *testing.T) {
	prev := github.ReviewEvidenceRoot
	root := filepath.Join(t.TempDir(), "evidence")
	github.ReviewEvidenceRoot = root
	t.Cleanup(func() { github.ReviewEvidenceRoot = prev })
	s := newTestServer()
	verifier := &github.ReviewEvidenceVerifier{}
	const head = "4396f46aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	writeSentinelBundle(t, root, "acme/console", 501, head, []evidence.SentinelFinding{
		{Rule: "sensitive_path", Summary: "changes 2 sensitive path(s)", Paths: []string{"OWNERS", ".github/workflows/ci.yml"}},
	})
	_, _, _, details := s.stageTrackerSentinel(verifier, "acme/console", 501, head, []string{config.DefaultSentinelLabel})
	if len(details) != 1 || !reflect.DeepEqual(details[0].Paths, []string{".github/workflows/ci.yml", "OWNERS"}) || details[0].Rule != "sensitive_path" {
		t.Fatalf("bundle details = %+v", details)
	}
	if got := s.sentinelDetailsForHead("acme/console", 501, head); !reflect.DeepEqual(got, details) {
		t.Fatalf("detailsForHead via bundle = %+v, want %+v", got, details)
	}

	s.RecordSentinelFinding(github.SentinelSweepEvent{Repo: "acme/console", Number: 502, HeadSHA: head, Blocked: true, Findings: []sentinel.Finding{
		{Rule: sentinel.RuleCIGateWeakening, Summary: "softens CI gates", Paths: []string{".github/workflows/ci.yml"}},
	}})
	_, n, reason, details := s.stageTrackerSentinel(verifier, "acme/console", 502, head, []string{config.DefaultSentinelLabel})
	if n != 0 || reason != "" || len(details) != 1 || details[0].Rule != sentinel.RuleCIGateWeakening {
		t.Fatalf("ring fallback = %d %q %+v", n, reason, details)
	}
	if _, _, _, details := s.stageTrackerSentinel(verifier, "acme/console", 503, head, nil); details != nil {
		t.Fatalf("unknown PR = %+v", details)
	}

	var it StageTrackerItem
	it.SentinelDetails = details
	raw, _ := json.Marshal(it)
	if !strings.Contains(string(raw), `"sentinel_details":[{"rule":"ci_gate_weakening","summary":"softens CI gates","paths":[".github/workflows/ci.yml"]}]`) {
		t.Fatalf("payload = %s", raw)
	}
}
