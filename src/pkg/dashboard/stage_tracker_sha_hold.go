package dashboard

import (
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// The SHA-hold gate keeps the hive off a primary-repo bug until a commit SHA
// is posted (labels-and-control-signals reference: `needs-sha`). Before
// hivecommons/hive#18866 the row said only "Open · since 11h" with every later
// stage empty; this is the pill, hover and chevron note that say why, and how
// to lift it, plus the waived state a maintainer's override leaves behind.

const (
	// SHA hold states on StageTrackerSHAHold.State.
	stageTrackerSHAHeld   = "held"
	stageTrackerSHAWaived = "waived"

	stageTrackerSHAHeldPill   = "Held: needs commit SHA"
	stageTrackerSHAHeldTip    = "Post a commit SHA in the issue, or waive this requirement."
	stageTrackerSHAHeldNote   = "held: post a commit SHA"
	stageTrackerSHAWaivedTip  = "A maintainer waived the commit-SHA requirement; the hive no longer holds this issue for a SHA. Restore the requirement from the row's menu."
	stageTrackerSHAWaivedNote = "SHA waived"
)

// StageTrackerSHAHold is the row's SHA-hold state in payload shape. State is
// "held" (needs-sha is on and nobody waived it) or "waived" (sha-waived is on;
// By names the maintainer when known). The strings are ready to draw.
type StageTrackerSHAHold struct {
	State string `json:"state"`
	// Label is the label the state stands for (needs-sha or sha-waived).
	Label string `json:"label"`
	By    string `json:"by,omitempty"`
	Pill  string `json:"pill"`
	Tip   string `json:"tip"`
	Note  string `json:"note"`
}

func (s *Server) shaHoldLabelName() string {
	if s != nil && s.deps != nil && s.deps.Config != nil {
		return s.deps.Config.Issues.EffectiveSHAHoldTrustedLabel()
	}
	return config.DefaultSHAHoldTrustedLabel
}

func shaLabelOn(labels []string, want string) bool {
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), want) {
			return true
		}
	}
	return false
}

// stageTrackerSHAHold derives the row's SHA-hold state from its labels; nil
// when the issue is neither held by the gate nor waived.
func (s *Server) stageTrackerSHAHold(repo string, number int, labels []string) *StageTrackerSHAHold {
	if shaLabelOn(labels, github.SHAWaivedLabel) {
		by := ""
		if s != nil && s.deps != nil {
			by = s.deps.GHClient.SHAWaivedBy(repo, number)
		}
		pill := "SHA waived"
		if by != "" {
			pill += " by " + by
		}
		return &StageTrackerSHAHold{State: stageTrackerSHAWaived, Label: github.SHAWaivedLabel, By: by, Pill: pill, Tip: stageTrackerSHAWaivedTip, Note: stageTrackerSHAWaivedNote}
	}
	name := s.shaHoldLabelName()
	if shaLabelOn(labels, name) || shaLabelOn(labels, config.DefaultSHAHoldTrustedLabel) {
		return &StageTrackerSHAHold{State: stageTrackerSHAHeld, Label: name, Pill: stageTrackerSHAHeldPill, Tip: stageTrackerSHAHeldTip, Note: stageTrackerSHAHeldNote}
	}
	return nil
}
