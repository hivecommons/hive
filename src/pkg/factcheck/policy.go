package factcheck

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/modelfamily"
)

// Independence is how different the reviewer must be from the author.
type Independence string

// Independence levels, weakest first.
const (
	IndependenceAgent  Independence = "agent"
	IndependenceModel  Independence = "model"
	IndependenceFamily Independence = "family"
)

// ReadyPolicy says who moves a draft PR to ready after a passing round.
type ReadyPolicy string

// Ready policies.
const (
	ReadyHuman ReadyPolicy = "human"
	ReadyHive  ReadyPolicy = "hive"
)

// Policy is the resolved review.analysis_factcheck block.
type Policy struct {
	AuthorAgents []string
	Labels       []string
	Independence Independence
	// ReviewerModel, when set, is the model the review is pinned to and is
	// the one judged for independence.
	ReviewerModel string
	MaxRounds     int
	ReadyPolicy   ReadyPolicy
	RevisionWait  time.Duration
}

// PR is the slice of a pull request's state the decisions need.
type PR struct {
	Repo    string
	Number  int
	Open    bool
	Draft   bool
	HeadSHA string
	Labels  []string
}

// Qualifies reports whether a PR an agent opened is covered by the pass. The
// PR qualifies when its authoring agent is in AuthorAgents or it carries one
// of Labels (case-insensitive). The reason is for the log.
func Qualifies(p Policy, pr PR, authorAgent string) (bool, string) {
	if !pr.Open {
		return false, "pull request is not open"
	}
	for _, a := range p.AuthorAgents {
		if authorAgent != "" && sameAgent(a, authorAgent) {
			return true, "authored by qualifying agent " + a
		}
	}
	for _, want := range p.Labels {
		for _, have := range pr.Labels {
			if strings.EqualFold(strings.TrimSpace(want), strings.TrimSpace(have)) {
				return true, "carries qualifying label " + want
			}
		}
	}
	return false, "neither the authoring agent nor a label qualifies"
}

// Agent is an agent as the independence rule sees it.
type Agent struct {
	Name    string
	Backend string
	Model   string
}

var replicaSuffix = regexp.MustCompile(`-\d+$`)

// baseName strips a replica suffix so reviewer-2 is the same agent as reviewer.
func baseName(name string) string {
	return strings.ToLower(replicaSuffix.ReplaceAllString(strings.TrimSpace(name), ""))
}

func sameAgent(a, b string) bool {
	return baseName(a) != "" && baseName(a) == baseName(b)
}

func modelKnown(model string) bool {
	return modelfamily.Canonical(model) != modelfamily.Unknown
}

// CheckIndependent reports whether reviewer may review author's work under the
// level. The author being an agent is what makes independence meaningful: a
// PR with no agent author (author.Name empty) accepts any reviewer. Where the
// level needs a model and either side's is unknown the answer is no, because
// independence that cannot be shown is not independence.
func CheckIndependent(level Independence, author, reviewer Agent, reviewerModel string) (bool, string) {
	if strings.TrimSpace(author.Name) == "" {
		return true, ""
	}
	if sameAgent(author.Name, reviewer.Name) {
		return false, fmt.Sprintf("%s authored the PR and may not review it", reviewer.Name)
	}
	if level == IndependenceAgent {
		return true, ""
	}
	model := strings.TrimSpace(reviewerModel)
	if model == "" {
		model = reviewer.Model
	}
	if !modelKnown(author.Model) || !modelKnown(model) {
		return false, fmt.Sprintf("the model of %s or %s is unknown, so independence cannot be shown", author.Name, reviewer.Name)
	}
	if level == IndependenceFamily {
		same, known := modelfamily.SameFamily(author.Model, model)
		if !known || same {
			return false, fmt.Sprintf("%s (%s) is in the same model family as the author %s (%s) or the family is unknown", reviewer.Name, model, author.Name, author.Model)
		}
		return true, ""
	}
	if modelfamily.Canonical(author.Model) == modelfamily.Canonical(model) {
		return false, fmt.Sprintf("%s runs the author's model %s", reviewer.Name, model)
	}
	return true, ""
}

// PickReviewer returns the first candidate that is independent of the author
// under the policy. When none is, ok is false and reason names why the first
// candidate was refused, for the hand-off note.
func PickReviewer(p Policy, author Agent, candidates []Agent) (Agent, string, bool) {
	reason := "no reviewer agent is configured"
	for i, c := range candidates {
		ok, why := CheckIndependent(p.Independence, author, c, p.ReviewerModel)
		if ok {
			return c, "", true
		}
		if i == 0 {
			reason = why
		}
	}
	return Agent{}, reason, false
}
