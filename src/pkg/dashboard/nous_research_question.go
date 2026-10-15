package dashboard

import "strings"

// nousStatusKeyResearchQuestion is the GET /api/nous/status key carrying the
// research question for the active scope (#12089). The Experiment Lab header
// shows it in place of the retired mode×scope pill; it is omitted when no
// question is set so the header renders nothing.
const nousStatusKeyResearchQuestion = "researchQuestion"

// Keys under Config["goals"] that may hold each scope's research question.
// The dashboard dialog writes governorGoal/repoGoal (PUT /api/nous/config/goals);
// the bare governor/repo spellings are accepted for hand-written panel configs.
var (
	nousGovernorGoalKeys = []string{"governorGoal", "governor", "research_question"}
	nousRepoGoalKeys     = []string{"repoGoal", "repo"}
)

// researchQuestionLocked returns the research question for the current scope:
// the governor question for governor (and both, falling back to the repo
// question), the repo question for repo. Empty when none is set. Mu must be held.
func (ns *NousState) researchQuestionLocked() string {
	if ns == nil {
		return ""
	}
	goals, _ := ns.Config["goals"].(map[string]interface{})
	if goals == nil {
		return ""
	}
	governor := firstGoalString(goals, nousGovernorGoalKeys)
	repo := firstGoalString(goals, nousRepoGoalKeys)
	switch strings.ToLower(strings.TrimSpace(ns.Scope)) {
	case "repo":
		return repo
	case "both":
		if governor != "" {
			return governor
		}
		return repo
	default:
		return governor
	}
}

func firstGoalString(goals map[string]interface{}, keys []string) string {
	for _, k := range keys {
		if s, ok := goals[k].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}
