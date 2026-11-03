package collect

import (
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/tokens"
)

func lcLaunch(agent, trigger string, at time.Time, refs ...tokens.LaunchRef) tokens.LaunchRecord {
	return tokens.LaunchRecord{Agent: agent, Trigger: trigger, LaunchedAtMs: at.UnixMilli(), Refs: refs}
}

func lcIssue(repo string, n int) tokens.LaunchRef {
	r, _ := tokens.ParseLaunchRef(repo + "#" + itoa(n))
	return r
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestLaunchCostPartitionInvariant: Σ by_launch + unattributed +
// backend_unsupported == hive total, through every branch of the join.
func TestLaunchCostPartitionInvariant(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-12 * time.Hour)

	launches := []tokens.LaunchRecord{
		lcLaunch("scanner", "governor-eval", rcTime(base, 10), lcIssue("o/r", 1), lcIssue("o/r", 2)),
		lcLaunch("scanner", "governor-eval", rcTime(base, 30), lcIssue("o/r", 1)),
		lcLaunch("reviewer", "review-review", rcTime(base, 10), tokens.PRLaunchRef("o/r", 9, "abc")),
		// Same PR, new head: repeat spend on one ref, two heads.
		lcLaunch("reviewer", "review-review", rcTime(base, 20), tokens.PRLaunchRef("o/r", 9, "def")),
		// Launch with no usage at all: still counts as a launch for its ref.
		lcLaunch("quality", "governor-eval", rcTime(base, 10), lcIssue("o/r", 3)),
	}
	summary := &tokens.AggregateSummary{Sessions: []tokens.SessionSummary{
		rcSession("s1", "scanner",
			rcUsage(rcTime(base, 5), 100, 10),         // before first launch -> unattributed
			rcUsage(rcTime(base, 15), 200, 20),        // launch@10 (2 refs)
			rcUsage(rcTime(base, 35), 300, 30),        // launch@30 (1 ref)
			rcUsage(rcTime(base, 30+7*60), 1000, 100), // past 6h horizon -> unattributed
		),
		rcSession("s2", "reviewer",
			rcUsage(rcTime(base, 12), 50, 5), // head abc
			rcUsage(rcTime(base, 25), 70, 7), // head def
		),
		rcSession("s3", "lonely", rcUsage(rcTime(base, 15), 400, 40)), // no launches
		{SessionID: "s4", Agent: "x", Model: "gpt-5", Backend: tokens.BackendCopilot,
			InputTokens: 500, OutputTokens: 50, TotalTokens: 550},
		{SessionID: "s5", Agent: "scanner", Model: "claude-opus-4-1", Backend: tokens.BackendClaude,
			InputTokens: 600, OutputTokens: 60, TotalTokens: 660},
	}}
	var hiveTotal int64
	for _, s := range summary.Sessions {
		hiveTotal += s.TotalTokens
	}

	resp := ComputeLaunchCost(summary, launches, now)
	if !resp.Ready {
		t.Fatal("ready=false with a non-nil summary")
	}
	var attributed int64
	for _, l := range resp.ByLaunch {
		attributed += l.Tokens
	}
	if attributed != resp.AttributedTokens {
		t.Fatalf("attributed_tokens %d != Σby_launch %d", resp.AttributedTokens, attributed)
	}
	if got := attributed + resp.Unattributed.Tokens + resp.BackendUnsupported.Tokens; got != hiveTotal || resp.TotalTokens != hiveTotal {
		t.Fatalf("partition %d (total_tokens %d) != hive total %d", got, resp.TotalTokens, hiveTotal)
	}
	if resp.Unattributed.Tokens != 110+1100+440 {
		t.Fatalf("unattributed = %d, want leading+horizon+lonely = %d", resp.Unattributed.Tokens, 110+1100+440)
	}
	if resp.BackendUnsupported.Tokens != 550+660 {
		t.Fatalf("backend_unsupported = %d", resp.BackendUnsupported.Tokens)
	}
	if len(resp.ByLaunch) != 4 {
		t.Fatalf("by_launch has %d entries, want 4 (quality launch got no usage): %+v", len(resp.ByLaunch), resp.ByLaunch)
	}
	if resp.LaunchesRetained != len(launches) {
		t.Fatalf("launches_retained = %d", resp.LaunchesRetained)
	}

	byRef := map[string]RefCostEntry{}
	for _, r := range resp.ByRef {
		byRef[r.Ref] = r
	}
	r1 := byRef["o/r#1"]
	if r1.Launches != 2 || r1.Kind != tokens.LaunchRefKindIssue || r1.Number != 1 {
		t.Fatalf("o/r#1 = %+v", r1)
	}
	if r1.LaunchTokens != 220+330 || r1.ShareTokens != 110+330 {
		t.Fatalf("o/r#1 tokens launch=%d share=%d, want 550/440", r1.LaunchTokens, r1.ShareTokens)
	}
	if r1.ShareUSD == nil || r1.LaunchUSD == nil || *r1.ShareUSD > *r1.LaunchUSD {
		t.Fatalf("o/r#1 usd share=%v launch=%v", r1.ShareUSD, r1.LaunchUSD)
	}
	r9 := byRef["o/r#9"]
	if r9.Kind != tokens.LaunchRefKindPR || r9.Launches != 2 || len(r9.HeadSHAs) != 2 || r9.HeadSHAs[0] != "abc" || r9.HeadSHAs[1] != "def" {
		t.Fatalf("o/r#9 = %+v", r9)
	}
	if r9.LaunchTokens != 55+77 || r9.ShareTokens != 55+77 {
		t.Fatalf("o/r#9 tokens = %d/%d", r9.LaunchTokens, r9.ShareTokens)
	}
	r3 := byRef["o/r#3"]
	if r3.Launches != 1 || r3.LaunchTokens != 0 || r3.LaunchUSD != nil {
		t.Fatalf("o/r#3 (launched, no usage) = %+v; want launches=1, nil usd", r3)
	}
	if r3.FirstLaunchedAt == "" || r3.LastLaunchedAt == "" {
		t.Fatalf("o/r#3 timestamps missing: %+v", r3)
	}
	if resp.ByRef[0].Ref != "o/r#1" {
		t.Fatalf("by_ref should sort by launch_tokens desc, got %s first", resp.ByRef[0].Ref)
	}
}

func TestLaunchCostNilSummaryNotReady(t *testing.T) {
	resp := ComputeLaunchCost(nil, []tokens.LaunchRecord{{Agent: "a", LaunchedAtMs: 1}}, time.Now())
	if resp.Ready || len(resp.ByLaunch) != 0 || len(resp.ByRef) != 0 || resp.ByLaunch == nil || resp.ByRef == nil {
		t.Fatalf("nil summary must yield ready=false with empty (non-nil) lists: %+v", resp)
	}
	if resp.Unattributed.USD != nil || resp.BackendUnsupported.USD != nil {
		t.Fatal("untouched buckets must keep nil USD")
	}
	if len(resp.Limitations) == 0 || resp.Disclaimer == "" || resp.PriceTableDate == "" {
		t.Fatal("honesty block missing")
	}
}

func TestAttributeLaunchRules(t *testing.T) {
	ls := []tokens.LaunchRecord{{Agent: "a", LaunchedAtMs: 1000}, {Agent: "a", LaunchedAtMs: 5000}}
	own := int64(2000)
	cases := []struct {
		ts   int64
		want int64
		ok   bool
	}{
		{0, 0, false},      // no timestamp
		{500, 0, false},    // before first launch
		{1000, 1000, true}, // exactly at launch
		{2500, 1000, true},
		{3500, 0, false}, // past ownership of launch@1000, before launch@5000
		{5000, 5000, true},
		{7000, 5000, true}, // trailing usage IS owned (launch is an opening event)
		{7001, 0, false},   // past horizon
	}
	for _, c := range cases {
		got, ok := attributeLaunch(ls, c.ts, 0, own)
		if ok != c.ok || (ok && got.LaunchedAtMs != c.want) {
			t.Errorf("attributeLaunch(ts=%d) = %d,%v want %d,%v", c.ts, got.LaunchedAtMs, ok, c.want, c.ok)
		}
	}
	if _, ok := attributeLaunch(ls, 2500, 3000, own); ok {
		t.Error("usage before the window start must be unattributed")
	}
}
