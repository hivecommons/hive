package dashboard

import (
	"slices"
	"strings"
	"time"
)

// stageTrackerSetActor distinguishes a lane classification from live work.
// Human gates and author returns name the next responsible actor; execution
// stages name an agent only when its current round actually touches the item.
func stageTrackerSetActor(it *StageTrackerItem, agents []FrontendAgent) {
	a := StageTrackerActor{Kind: "none", Why: "waiting for the next pipeline evaluation"}
	switch it.Stage {
	case 15, 17, 45, 85, 90:
		a.Kind, a.Why = "human", "waiting for a human decision"
	case 20:
		if it.ClaimantKind != "" {
			a.Kind, a.Why = it.ClaimantKind, "live claim holder"
		}
	case 30, 40, 70, 71, 72:
		a.Login = it.Author
		switch it.AuthorKind {
		case "human", "clanker":
			a.Kind, a.Why = it.AuthorKind, "waiting for the PR author"
		case "bot":
			a.Why = "bot PR; waiting for the bot's next run"
		default:
			a.Why = "no active agent is working on this PR"
		}
	}
	a.Since = it.StageSince
	// A live worker takes precedence over inferred responsibility, except at
	// explicit human gates where a decision is still required.
	if a.Kind != "human" {
		for _, agent := range agents {
			la := agent.LastAction
			if agent.Busy != "working" || la == nil || !la.Current || la.Number != it.Number {
				continue
			}
			repo := strings.ToLower(la.Repo)
			target := strings.ToLower(it.Repo)
			if repo == "" || (repo != target && !strings.HasSuffix(target, "/"+repo)) {
				continue
			}
			a.Kind, a.Login, a.Why = "hive", agent.Name, "agent is working on this item in its current round"
			if at, err := time.Parse(time.RFC3339, la.At); err == nil {
				a.Since = &at
			}
			break
		}
	}
	it.StageActor = a
	if it.Stage == 70 && a.Kind == "none" {
		it.StageState = "Waiting for fixes"
		if it.AuthorKind == "bot" {
			it.StageState = "Waiting · bot PR"
			if strings.Contains(strings.ToLower(it.Reason), "conflict") || slices.Contains(it.Modifiers, "needs-rebase") {
				it.StageState = "Conflicting · bot PR"
			}
		}
		it.Caption = it.StageState + " — " + a.Why
		it.Actor = ""
	}
}
