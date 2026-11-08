package main

import (
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/retro"
	"github.com/hivecommons/hive/pkg/scheduler"
)

// knowledgeAdvisorDecisionRecorder persists the ranking decorator's decisions
// (#17211). retro.AdvisorDecisionStore implements it.
type knowledgeAdvisorDecisionRecorder interface {
	Record(retro.AdvisorDecisionRecord)
}

// knowledgeAdvisorDecisions returns the process-wide decision store, opening
// it on first use: the decorator and the retro lane share one instance.
func (b *boot) knowledgeAdvisorDecisions() *retro.AdvisorDecisionStore {
	b.advisorDecisionsOnce.Do(func() {
		b.advisorDecisions = retro.OpenAdvisorDecisionStore()
	})
	return b.advisorDecisions
}

// recordDecision writes the compact decision record: agent, kick, the kick's
// primed refs, mode, arm, outcome and slug. The model's reason and the pane
// text are never stored. slug is the delivered entry, or the model's valid
// pick when nothing was delivered (a holdout keeps what it withheld).
func (d *knowledgeAdviserLLM) recordDecision(agent string, kick scheduler.KickSummary, r config.ResolvedAdvisorLLM, dec knowledgeAdviserLLMDecision, outcome string, out []crux.KnowledgeAdvice) {
	if d.decisions == nil {
		return
	}
	slug := ""
	if len(out) > 0 {
		slug = out[0].Slug
	} else if dec.verdict == knowledgeVerdictPick {
		slug = dec.pick
	}
	d.decisions.Record(retro.AdvisorDecisionRecord{
		Timestamp: d.now().UTC().Format(time.RFC3339),
		Agent:     agent,
		Kick:      kick.ID,
		Refs:      kick.Refs,
		Mode:      r.Mode,
		Arm:       retro.AdvisorDecisionArm(r.Mode, outcome),
		Outcome:   outcome,
		Slug:      slug,
	})
}
