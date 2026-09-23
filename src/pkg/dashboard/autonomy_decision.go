package dashboard

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/retro"
)

type AutonomyNotifyFunc func(title, message string, demote bool)
type AutonomyApplyFunc func(repo string, level int)

type AutonomyDecisionSink struct {
	srv    *Server
	notify AutonomyNotifyFunc
	apply  AutonomyApplyFunc
}

func NewAutonomyDecisionSink(srv *Server, notify AutonomyNotifyFunc, apply AutonomyApplyFunc) *AutonomyDecisionSink {
	return &AutonomyDecisionSink{srv: srv, notify: notify, apply: apply}
}

func (s *AutonomyDecisionSink) RecordAutonomyDecision(d retro.AutonomyDecision) {
	if s == nil || s.srv == nil {
		return
	}
	detail := auditDetail(
		"repo", d.Repo,
		"direction", d.Direction,
		"from", strconv.Itoa(d.From),
		"to", strconv.Itoa(d.To),
		"evidence", strings.Join(d.EvidenceIDs, "|"),
		"reason", d.Reason,
	)
	if s.srv.audit != nil {
		s.srv.audit.Log("system", "autonomy_acmm_"+d.Direction, detail, retro.Actor)
	}
	if s.apply != nil {
		s.apply(d.Repo, d.To)
	}
	s.recordAdvisory(d)
	if s.notify != nil {
		s.notify("Automatic ACMM "+d.Direction, fmt.Sprintf("%s moved L%d → L%d (%s)", d.Repo, d.From, d.To, d.Reason), d.Direction == "demote")
	}
}

func (s *AutonomyDecisionSink) recordAdvisory(d retro.AutonomyDecision) {
	if s.srv.deps == nil || s.srv.deps.BeadStores == nil {
		return
	}
	store := s.srv.deps.BeadStores[retro.Actor]
	if store == nil {
		return
	}
	priority := beads.PriorityMedium
	if d.Direction == "demote" {
		priority = beads.PriorityHigh
	}
	title := fmt.Sprintf("autonomy decision: %s %sd to L%d", d.Repo, d.Direction, d.To)
	b, err := store.Create(title, beads.TypeDecision, priority, retro.Actor, "")
	if err != nil {
		return
	}
	_ = store.SetMetadata(b.ID, "autonomy_decision", d.Direction)
	_ = store.SetMetadata(b.ID, "autonomy_scope_type", "repo")
	_ = store.SetMetadata(b.ID, "autonomy_scope_value", d.Repo)
	_ = store.SetMetadata(b.ID, "autonomy_from_level", strconv.Itoa(d.From))
	_ = store.SetMetadata(b.ID, "autonomy_to_level", strconv.Itoa(d.To))
	_ = store.SetMetadata(b.ID, "autonomy_evidence_ids", strings.Join(d.EvidenceIDs, ","))
	_ = store.SetMetadata(b.ID, "detail", d.Reason)
}

var _ retro.AutonomyDecisionSink = (*AutonomyDecisionSink)(nil)
