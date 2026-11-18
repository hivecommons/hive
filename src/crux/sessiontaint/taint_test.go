package sessiontaint

import (
	"reflect"
	"testing"
	"time"
)

func lab(l Label, origin string) LabelRecord {
	return LabelRecord{Label: l, Origin: origin, Source: SourceCanaryHit, FirstSeen: time.Unix(1, 0)}
}

func TestEvaluateRuleTable(t *testing.T) {
	cases := []struct {
		name   string
		labels []LabelRecord
		sink   Sink
		want   []string
	}{
		{"no labels", nil, Sink{Kind: SinkRelayComment, Repo: "o/a", Audience: AudiencePublic}, nil},
		{"secret to public", []LabelRecord{lab(LabelSecret, "hive-canary")}, Sink{Kind: SinkRelayIssue, Repo: "o/a", Audience: AudiencePublic}, []string{RuleSecretPublicWrite}},
		{"secret to private", []LabelRecord{lab(LabelSecret, "hive-canary")}, Sink{Kind: SinkRelayIssue, Repo: "o/a", Audience: AudiencePrivate}, nil},
		{"unresolved audience is public", []LabelRecord{lab(LabelSecret, "hive-canary")}, Sink{Kind: SinkRelayPR, Repo: "o/a"}, []string{RuleSecretPublicWrite}},
		{"private repo elsewhere", []LabelRecord{lab(LabelPrivateRepo, "o/secret")}, Sink{Kind: SinkRelayComment, Repo: "o/a", Audience: AudiencePrivate}, []string{RulePrivateAudience}},
		{"private repo same repo", []LabelRecord{lab(LabelPrivateRepo, "o/secret")}, Sink{Kind: SinkRelayComment, Repo: "o/secret", Audience: AudiencePrivate}, nil},
		{"private repo same repo other case", []LabelRecord{lab(LabelPrivateRepo, "O/Secret")}, Sink{Kind: SinkRelayComment, Repo: "o/secret", Audience: AudiencePrivate}, nil},
		{"credentialed to public", []LabelRecord{lab(LabelCredentialedResponse, "host.example")}, Sink{Kind: SinkRelayReview, Repo: "o/a", Audience: AudiencePublic}, []string{RuleCredPublicWrite}},
		{"credentialed to private", []LabelRecord{lab(LabelCredentialedResponse, "host.example")}, Sink{Kind: SinkRelayReview, Repo: "o/a", Audience: AudiencePrivate}, nil},
		{"unknown any write", []LabelRecord{lab(LabelUnknown, "state-lost")}, Sink{Kind: SinkRelayComment, Repo: "o/a", Audience: AudiencePrivate}, []string{RuleUnknownAnyWrite}},
		{"several labels several rules", []LabelRecord{lab(LabelSecret, "jwt"), lab(LabelPrivateRepo, "o/x")}, Sink{Kind: SinkRelayIssue, Repo: "o/a", Audience: AudiencePublic}, []string{RuleSecretPublicWrite, RulePrivateAudience}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, f := range Evaluate(tc.labels, tc.sink) {
				got = append(got, f.Rule)
				if f.Verdict != VerdictHold {
					t.Errorf("rule %s verdict = %q, want hold", f.Rule, f.Verdict)
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("rules = %v, want %v", got, tc.want)
			}
		})
	}
}

// The observe-only invariant at the evaluator: across every label kind, every
// sink kind and both audiences, a verdict is only ever hold, and a session
// with no label never produces one. There is no deny verdict in this phase.
func TestEvaluateNeverDeniesAndCleanSessionNeverFires(t *testing.T) {
	kinds := []Label{LabelSecret, LabelPrivateRepo, LabelCredentialedResponse, LabelUnknown}
	sinks := []SinkKind{SinkRelayIssue, SinkRelayComment, SinkRelayPR, SinkRelayReview}
	for _, l := range kinds {
		for _, k := range sinks {
			for _, a := range []Audience{AudiencePublic, AudiencePrivate} {
				for _, f := range Evaluate([]LabelRecord{lab(l, "o/x")}, Sink{Kind: k, Repo: "o/a", Audience: a}) {
					if f.Verdict != VerdictHold {
						t.Fatalf("label %s sink %s audience %d: verdict %q, want hold only", l, k, a, f.Verdict)
					}
				}
				if got := Evaluate(nil, Sink{Kind: k, Repo: "o/a", Audience: a}); len(got) != 0 {
					t.Fatalf("clean session fired %v", got)
				}
			}
		}
	}
}

func TestEvaluateIsPure(t *testing.T) {
	in := []LabelRecord{lab(LabelSecret, "jwt"), lab(LabelPrivateRepo, "o/x")}
	snapshot := append([]LabelRecord(nil), in...)
	sink := Sink{Kind: SinkRelayIssue, Repo: "o/a", Audience: AudiencePublic}
	first := Evaluate(in, sink)
	second := Evaluate(in, sink)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("not deterministic: %v vs %v", first, second)
	}
	if !reflect.DeepEqual(in, snapshot) {
		t.Fatalf("input labels mutated")
	}
}
