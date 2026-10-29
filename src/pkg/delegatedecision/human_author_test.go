package delegatedecision

import (
	"strings"
	"testing"
)

func TestProtectedAuthorsReturnInsteadOfAdoption(t *testing.T) {
	for _, author := range []struct {
		name         string
		human, owner bool
		labels       []string
	}{
		{name: "human", human: true}, {name: "owner", owner: true}, {name: "human label", labels: []string{"author/human"}},
	} {
		for _, condition := range []string{"question", "conflict", "CI", "DCO", "stalled", "competing"} {
			t.Run(author.name+"/"+condition, func(t *testing.T) {
				in := Input{Kind: KindPR, Number: 8, Author: "alice", HumanAuthor: author.human, OwnerAuthor: author.owner, Labels: author.labels, CIGreen: true, Question: "Should this retry change?"}
				switch condition {
				case "conflict":
					in.Conflicting = true
				case "CI":
					in.CIGreen = false
				case "DCO":
					in.DCOMissing = true
				case "stalled":
					in.AdoptionStalled = true
				case "competing":
					in.Competing = []CompetingPR{{Number: 9, HiveAuthored: true}}
				}
				d := Run(in)
				if d.Rule != "return-to-author" || d.Verdict != VerdictDirection || !strings.Contains(d.Text, "@alice") || !strings.Contains(d.Text, in.Question) {
					t.Fatalf("decision: %+v", d)
				}
				found := false
				for _, a := range d.Actions {
					if a.Kind == ActionAdoptSupersede || a.Kind == ActionApproveReview || a.Kind == ActionCloseDuplicate {
						t.Fatalf("unsafe action: %+v", a)
					}
					if a.Kind == ActionLabelsAdd && hasLabel(a.Labels, "stage/071-needs-author") {
						found = true
					}
				}
				if !found {
					t.Fatalf("not returned to 071: %+v", d)
				}
				if in.Conflicting && !strings.Contains(d.Text, "needs rebase by @alice") {
					t.Fatalf("missing rebase direction: %+v", d)
				}
			})
		}
	}
}

func TestProtectedAuthorReviewedHeadAndHold(t *testing.T) {
	for _, owner := range []bool{false, true} {
		in := Input{Kind: KindPR, HumanAuthor: !owner, OwnerAuthor: owner, CIGreen: true, ReviewApproved: true, Mergeable: true}
		if d := Run(in); d.Verdict != VerdictApprove || d.Rule != "human-reviewed" {
			t.Fatalf("ready head: %+v", d)
		}
		in.Mergeable = false
		if d := Run(in); !d.KeepsPark || d.Verdict != VerdictRecommendOnly {
			t.Fatalf("unknown mergeability: %+v", d)
		}
		in.Labels = []string{LabelHold}
		if d := Run(in); d.Verdict != VerdictRefused || len(d.Actions) != 0 {
			t.Fatalf("hold: %+v", d)
		}
	}
	// The offline rule continues to apply to a contributor's hive PR.
	if d := Run(Input{Kind: KindPR, Conflicting: true}); d.Verdict != VerdictAdoptSupersede {
		t.Fatalf("clanker: %+v", d)
	}
}

func TestProtectedAuthorAdoptionRails(t *testing.T) {
	for _, a := range []Action{{Kind: ActionAdoptSupersede}, {Kind: ActionLabelsAdd, Labels: []string{LabelAdoptSupersede}}} {
		d := ApplyRails(Decision{Verdict: VerdictAdoptSupersede, Actions: []Action{decisionComment("adopt"), a}}, Rails{ProtectedAuthor: true})
		if d.Verdict != VerdictRefused || !d.KeepsPark || len(d.Actions) != 0 {
			t.Fatalf("rail: %+v", d)
		}
	}
	for _, in := range []Input{{Kind: KindPR, HumanAuthor: true}, {Kind: KindPR, OwnerAuthor: true}} {
		_, err := ParseLLMDecision(in, `{"verdict":"adopt-supersede","text":"adopt","rationale":"offline","actions":[{"kind":"adopt-supersede"}]}`)
		if err == nil {
			t.Fatal("LLM adoption accepted for protected author")
		}
	}
}
