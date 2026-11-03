package automerge

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/evidence"
)

type intentShadowRecord struct {
	repo   string
	number int
	gate   evidence.ShadowGate
}

func newIntentShadowSweepClient(apiURL string, enforce, shadow bool, records *[]intentShadowRecord) *Engine {
	c := newAutoMergeSweepClient(apiURL)
	c.SetIntentGate(&IntentGate{
		Enforce: func() bool { return enforce },
		Shadow:  func() bool { return shadow },
		RecordShadow: func(fullRepo string, number int, head string, gate evidence.ShadowGate) {
			*records = append(*records, intentShadowRecord{repo: fullRepo, number: number, gate: gate})
		},
	})
	return c
}

// intent.shadow never withholds a merge: with enforce off the sweep merges
// exactly as before and only records what enforce would have done.
func TestSelfAuthoredSweepIntentShadowRecordsWithoutBlocking(t *testing.T) {
	tests := []struct {
		name        string
		fx          selfSweepFixture
		wantBlocked bool
		wantTier    string
		wantReason  string
	}{
		{name: "tier1 without linked issue would have blocked", fx: selfSweepFixture{files: []string{testIntentSourceFile}}, wantBlocked: true, wantTier: "tier1", wantReason: "tier 1 requires linked issue"},
		{name: "tier1 with linked issue would not have blocked", fx: selfSweepFixture{body: testIntentLinkedBody, files: []string{testIntentSourceFile}}, wantBlocked: false, wantTier: "tier1", wantReason: "linked issue evidence present"},
		{name: "evidence unavailable would have blocked", fx: selfSweepFixture{files: []string{testIntentSourceFile}, filesHTTPCode: http.StatusInternalServerError}, wantBlocked: true, wantReason: autoMergeReasonIntentEvidence},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			merges := 0
			tt.fx.mergeCalls = &merges
			tt.fx.mergeApplied = true
			api := newSelfSweepGuardAPI(t, tt.fx)
			defer api.Close()
			var records []intentShadowRecord
			c := newIntentShadowSweepClient(api.URL, false, true, &records)
			event, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
			if err != nil || reason != "" {
				t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want merge under shadow", reason, err)
			}
			if event.MergeSHA != "merge7" || merges != 1 {
				t.Fatalf("event = %+v, merges = %d; shadow must never withhold the merge", event, merges)
			}
			if len(records) != 1 {
				t.Fatalf("records = %+v, want exactly one shadow observation", records)
			}
			r := records[0]
			if r.repo != "acme/widget" || r.number != 7 {
				t.Fatalf("record target = %s#%d, want acme/widget#7", r.repo, r.number)
			}
			if r.gate.WouldHaveBlocked == nil || *r.gate.WouldHaveBlocked != tt.wantBlocked {
				t.Fatalf("would_have_blocked = %v, want %v", r.gate.WouldHaveBlocked, tt.wantBlocked)
			}
			if r.gate.Tier != tt.wantTier || !strings.HasPrefix(r.gate.Reason, tt.wantReason) {
				t.Fatalf("gate = %+v, want tier %q reason prefix %q", r.gate, tt.wantTier, tt.wantReason)
			}
			if r.gate.Confidence != 1 || r.gate.EvaluatedAt.IsZero() || r.gate.WouldHaveHeld != nil {
				t.Fatalf("gate = %+v, want confidence 1, evaluated_at set, no would_have_held", r.gate)
			}
		})
	}
}

// enforce: true is unchanged by shadow: the merge is still withheld and no
// shadow observation is recorded.
func TestSelfAuthoredSweepIntentShadowIgnoredWhenEnforced(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{files: []string{testIntentSourceFile}, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	var records []intentShadowRecord
	c := newIntentShadowSweepClient(api.URL, true, true, &records)
	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true)
	if err != nil || reason != autoMergeReasonIntentUnauthorized {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want %q", reason, err, autoMergeReasonIntentUnauthorized)
	}
	if merges != 0 || len(records) != 0 {
		t.Fatalf("merges = %d, records = %+v; want blocked with no shadow record", merges, records)
	}
}

func TestSelfAuthoredSweepIntentShadowOffRecordsNothing(t *testing.T) {
	merges := 0
	fx := selfSweepFixture{files: []string{testIntentSourceFile}, mergeApplied: true, mergeCalls: &merges}
	api := newSelfSweepGuardAPI(t, fx)
	defer api.Close()
	var records []intentShadowRecord
	c := newIntentShadowSweepClient(api.URL, false, false, &records)
	if _, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 7, true); err != nil || reason != "" {
		t.Fatalf("trySweepSelfAuthoredPR = (reason %q, err %v), want advisory merge", reason, err)
	}
	if merges != 1 || len(records) != 0 {
		t.Fatalf("merges = %d, records = %+v; want merge with no shadow record", merges, records)
	}
}

func TestIntentGateShadowedNilSafety(t *testing.T) {
	var nilGate *IntentGate
	if nilGate.shadowed() {
		t.Fatal("nil gate must not be shadowed")
	}
	if (&IntentGate{Shadow: func() bool { return true }}).shadowed() {
		t.Fatal("gate without RecordShadow must not be shadowed")
	}
}
