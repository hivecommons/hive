package dashboard

import "strings"

// nousStatusKeyResearchQuestion is the GET /api/nous/status key carrying the
// research question (#12089). The Experiment Lab header shows it in place of
// the retired mode×scope pill; it is an empty string when no question is set
// so the header renders nothing.
const nousStatusKeyResearchQuestion = "researchQuestion"

// Keys under Config["goals"] that may hold the repo research question. The
// dashboard dialog writes repoGoal (PUT /api/nous/config/goals); the bare
// repo spelling is accepted for hand-written panel configs. The Experiment
// Lab is repo-only (#12207): a legacy governorGoal/governor key is ignored.
var nousRepoGoalKeys = []string{"repoGoal", "repo"}

// researchQuestionLocked returns the repo research question, or "" when none
// is set. The stored Scope is ignored: the lab only runs repo experiments.
// Mu must be held.
func (ns *NousState) researchQuestionLocked() string {
	if ns == nil {
		return ""
	}
	goals, _ := ns.Config["goals"].(map[string]interface{})
	if goals == nil {
		return ""
	}
	return normalizeNousResearchQuestion(firstGoalString(goals, nousRepoGoalKeys))
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
