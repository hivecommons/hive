package github

import (
	"context"
	"testing"
	"time"
)

func TestGFIMaintainerAppliedLabels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		actor   string
		removed bool
		want    int
	}{
		{"writer", laWriter, false, 1},
		{"triager", laTriager, false, 1},
		{"org member", laOrgOnly, false, 1},
		{"reader", laReader, false, 0},
		{"github actions relabeled", laProwBot, false, 0},
		{"hive relabeled", laHive, false, 0},
		{"removed", laWriter, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := laServer(t, nil)
			rec.timelines[35] = []unparkWireTimelineEvent{
				labeledAt(1, laWriter, "help wanted", laBase),
				unlabeledAt(2, laWriter, "help wanted", laBase.Add(time.Minute)),
				labeledAt(3, tc.actor, "help wanted", laBase.Add(2*time.Minute)),
			}
			if tc.removed {
				rec.timelines[35] = append(rec.timelines[35], unlabeledAt(4, laWriter, "help wanted", laBase.Add(3*time.Minute)))
			}
			got, err := c.MaintainerAppliedLabels(context.Background(), "org/repo", 35, []string{"help wanted", "size/S"})
			if err != nil || len(got) != tc.want {
				t.Fatalf("labels = %v, err = %v, want %d", got, err, tc.want)
			}
		})
	}
}

func TestGFIMaintainerAppliedLabelsUsesConfiguredPolicy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy func(string) (LabelApprovalPolicy, bool)
		actor  string
		want   int
	}{
		{"unconfigured trusts org members", nil, laOrgOnly, 1},
		{"configured without org trust", laPolicyFn(LabelApprovalPolicy{Labels: []string{"triage/accepted"}}, true), laOrgOnly, 0},
		{"configured trusted bot", laPolicyFn(laPolicy(), true), laProwBot, 1},
		{"configured policy still trusts writers", laPolicyFn(LabelApprovalPolicy{Labels: []string{"triage/accepted"}}, true), laWriter, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, rec := laServer(t, nil)
			if tc.policy != nil {
				c.SetLabelApprovalPolicy(tc.policy)
			}
			rec.timelines[35] = []unparkWireTimelineEvent{labeledAt(1, tc.actor, "help wanted", laBase)}
			got, err := c.MaintainerAppliedLabels(context.Background(), "org/repo", 35, []string{"help wanted"})
			if err != nil || len(got) != tc.want {
				t.Fatalf("labels = %v, err = %v, want %d", got, err, tc.want)
			}
		})
	}
}
