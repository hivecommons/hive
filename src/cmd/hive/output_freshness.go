package main

import (
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
	"github.com/hivecommons/hive/pkg/hub"
)

func outputFreshnessHeartbeatFields(acmmLevel int, govState governor.State, agents []hub.AgentSummary) (lastWriteKickAt, disposition, reason string, notWritableQueued int) {
	notWritableQueued = govState.QueueHold
	var newest time.Time
	for _, a := range agents {
		if !agentCanProduceJudgedOutput(acmmLevel, a) {
			continue
		}
		if t := govState.LastKick[a.Name]; !t.IsZero() && t.After(newest) {
			newest = t
		}
	}
	if !newest.IsZero() {
		lastWriteKickAt = newest.UTC().Format(time.RFC3339)
	}
	switch {
	case acmmLevel > 0 && acmmLevel <= 2:
		disposition = "advisory-only"
		reason = "ACMM advisory band produces advisory output, not writes"
	case govState.BudgetExhausted:
		disposition = "budget-suppressed"
		reason = "governor budget exhausted"
	case govState.QueueIssues+govState.QueuePRs == 0 && govState.QueueHold > 0:
		disposition = "agent-decided-not-writable"
		reason = "queued items are held or otherwise not writable"
	case govState.QueueIssues+govState.QueuePRs == 0:
		disposition = "idle"
		reason = "no actionable work queued"
	case len(govState.Cadences) == 0:
		disposition = "no-due-agents"
		reason = "no agents due in the current governor mode"
	default:
		dueCapable := false
		now := time.Now()
		for _, a := range agents {
			if !agentCanProduceJudgedOutput(acmmLevel, a) {
				continue
			}
			if cad, ok := govState.Cadences[a.Name]; ok && !cad.Paused {
				last := govState.LastKick[a.Name]
				if cad.Schedule.Mode() != config.CadenceModeInterval {
					if _, ok := cad.Schedule.DueOccurrence(last, now, config.CadenceCatchUpWindow); ok {
						dueCapable = true
						break
					}
					continue
				}
				if cad.Interval <= 0 || last.IsZero() || now.Sub(last) >= cad.Interval {
					dueCapable = true
					break
				}
			}
		}
		if !dueCapable {
			disposition = "no-due-agents"
			reason = "no write-capable agents due in the current governor mode"
		} else {
			disposition = "kick-capable"
			reason = "write-capable agents are eligible to kick"
		}
	}
	return lastWriteKickAt, disposition, reason, notWritableQueued
}

func agentCanProduceJudgedOutput(acmmLevel int, a hub.AgentSummary) bool {
	switch {
	case acmmLevel >= 6:
		return a.CanMerge
	case acmmLevel >= 3:
		return a.CanOpenIssue || a.CanOpenPR
	default:
		return false
	}
}
