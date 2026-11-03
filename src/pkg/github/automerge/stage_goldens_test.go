package automerge

// Retain this gathering/facts parity check for one release after ADR-0025 PR-F.
import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// stageGoldenVerdict is the serialised ContributorLaneVerdict.
type stageGoldenVerdict struct {
	Configured         bool     `json:"configured,omitempty"`
	Approved           bool     `json:"approved,omitempty"`
	Verdict            string   `json:"verdict,omitempty"`
	DecisionRequested  bool     `json:"decision_requested,omitempty"`
	DecisionQuestion   string   `json:"decision_question,omitempty"`
	HumanSummaries     []string `json:"human_summaries,omitempty"`
	HiveAuthoredKnown  bool     `json:"hive_authored_known,omitempty"`
	HiveAuthored       bool     `json:"hive_authored,omitempty"`
	FixCycleCapReached bool     `json:"fix_cycle_cap_reached,omitempty"`
}

func (v *stageGoldenVerdict) toVerdict() ContributorLaneVerdict {
	if v == nil {
		return ContributorLaneVerdict{}
	}
	return ContributorLaneVerdict{Configured: v.Configured, Approved: v.Approved, Verdict: v.Verdict, DecisionRequested: v.DecisionRequested, DecisionQuestion: v.DecisionQuestion, HumanSummaries: v.HumanSummaries, HiveAuthoredKnown: v.HiveAuthoredKnown, HiveAuthored: v.HiveAuthored, FixCycleCapReached: v.FixCycleCapReached}
}

// stageGoldenDetail is the serialised stageDetail (the fields classifyStage
// and the post-steps read). A nil Verdict means VerdictKnown == false.
type stageGoldenDetail struct {
	Verdict           *stageGoldenVerdict `json:"verdict,omitempty"`
	HumanApprover     string              `json:"human_approver,omitempty"`
	HumanApprovalHead string              `json:"human_approval_head,omitempty"`
	Role              string              `json:"role,omitempty"`
	RedChecks         []string            `json:"red_checks,omitempty"`
	RedChecksHead     string              `json:"red_checks_head,omitempty"`
	DCOGate           bool                `json:"dco_gate,omitempty"`
	DCOGateHead       string              `json:"dco_gate_head,omitempty"`
}

// stageGoldenObservation is the serialised stageObservation.
type stageGoldenObservation struct {
	Lane             string            `json:"lane"`
	Reason           string            `json:"reason"`
	HeadSHA          string            `json:"head_sha,omitempty"`
	Labels           []string          `json:"labels,omitempty"`
	Merged           bool              `json:"merged,omitempty"`
	Err              bool              `json:"err,omitempty"`
	SharedCIIncident int               `json:"shared_ci_incident,omitempty"`
	Detail           stageGoldenDetail `json:"detail"`
}

var stageGoldenRoles = map[string]stagelabels.Role{
	"": stagelabels.RoleNone, "trusted": stagelabels.RoleTrusted, "contributor": stagelabels.RoleContributor,
	"newcomer": stagelabels.RoleNewcomer, "untrusted": stagelabels.RoleUntrusted,
}

func (o stageGoldenObservation) toObservation() stageObservation {
	obs := stageObservation{
		Lane: o.Lane, Reason: o.Reason, HeadSHA: o.HeadSHA, Labels: o.Labels, Merged: o.Merged, Err: o.Err,
		SharedCIIncident: o.SharedCIIncident,
		Display:          "hivecommons/hive", Owner: "hivecommons", Repo: "hive", Number: 1,
		Detail: stageDetail{
			HumanApprover: o.Detail.HumanApprover, HumanApprovalHead: o.Detail.HumanApprovalHead,
			Role: stageGoldenRoles[o.Detail.Role], RedChecks: o.Detail.RedChecks, RedChecksHead: o.Detail.RedChecksHead,
			DCOGate: o.Detail.DCOGate, DCOGateHead: o.Detail.DCOGateHead,
		},
	}
	if o.Detail.Verdict != nil {
		obs.Detail.VerdictKnown = true
		obs.Detail.Verdict = o.Detail.Verdict.toVerdict()
	}
	return obs
}

func TestStageGoldensEngineParity(t *testing.T) {
	files, err := filepath.Glob("../../stageengine/testdata/stage_goldens/*.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("missing PR golden fixtures")
	}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		var cases []struct {
			Name  string `json:"name"`
			Input struct {
				Observations []stageGoldenObservation `json:"observations"`
			} `json:"input"`
		}
		if err := json.Unmarshal(raw, &cases); err != nil {
			t.Fatal(err)
		}
		for _, c := range cases {
			list := make([]stageObservation, 0, len(c.Input.Observations))
			for _, o := range c.Input.Observations {
				list = append(list, o.toObservation())
			}
			verdict, role, obs := resolveStage(list)
			got := stageengine.DerivePR(factsFrom(list))
			if verdict.Rank == stageRankNone {
				if !got.Abstain {
					t.Errorf("%s: wanted abstain: %+v", c.Name, got)
				}
				continue
			}
			reason := verdict.Reason
			if reason == "" {
				reason = obs.Reason
			}
			if got.Abstain || got.Stage != verdict.Stage || got.Role != role || got.Terminal != verdict.Terminal || got.Reason != reason {
				t.Errorf("%s: gathered facts derived %+v, preview %+v role=%s reason=%s", c.Name, got, verdict, role, reason)
			}
		}
	}
}
