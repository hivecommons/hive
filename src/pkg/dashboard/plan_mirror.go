package dashboard

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/planning"
)

// planMirrorTimeout bounds the GitHub comment post so a slow forge never
// holds the approve response hostage — the approval itself already landed.
const planMirrorTimeout = 15 * time.Second

// planIssueCommenter is the one GitHub capability the mirror needs, so tests
// can substitute a recorder for the real client.
type planIssueCommenter interface {
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error
}

// mirrorPlanToIssue posts the approved plan's checklist on the epic's source
// issue when planning.mirror_to_issue is on (hivecommons/hive#8011). Best
// effort: a failure is logged and audited but never fails the approval — the
// dashboard is the source of truth, the comment is a courtesy copy. Returns
// false when nothing was posted (knob off, bd-created epic, no client).
func (s *Server) mirrorPlanToIssue(r *http.Request, store *beads.Store, tree *planning.PlanTree, agentName string) bool {
	if s.deps == nil || s.deps.Config == nil || !s.deps.Config.Planning.MirrorToIssue || tree == nil {
		return false
	}
	var commenter planIssueCommenter
	if s.deps.GHClient != nil {
		commenter = s.deps.GHClient
	}
	return s.mirrorPlanWith(r, commenter, store, tree, agentName)
}

func (s *Server) mirrorPlanWith(r *http.Request, commenter planIssueCommenter, store *beads.Store, tree *planning.PlanTree, agentName string) bool {
	if commenter == nil || store == nil {
		return false
	}
	epic, err := store.Get(tree.EpicID)
	if err != nil {
		return false
	}
	repo := epic.Meta(planning.MetaIssueRepo)
	number, _ := strconv.Atoi(epic.Meta(planning.MetaIssueNumber))
	if repo == "" || number <= 0 {
		return false // bd-created epic: nowhere to mirror to
	}
	link := ""
	if origin := s.oauthPublicOrigin(r); origin != "" {
		link = origin + "/#plan=" + tree.EpicID
	}
	body := planning.MirrorChecklist(tree, link)
	ctx, cancel := context.WithTimeout(r.Context(), planMirrorTimeout)
	defer cancel()
	if err := commenter.CreateIssueComment(ctx, repo, number, body); err != nil {
		if s.deps != nil && s.deps.Logger != nil {
			s.deps.Logger.Warn("plan mirror comment failed", "epic", tree.EpicID, "issue", repo+"#"+strconv.Itoa(number), "error", err)
		}
		s.auditFromRequest(r, "plan_mirror_failed", auditDetail("epic", tree.EpicID, "issue", repo+"#"+strconv.Itoa(number), "error", err.Error()), agentName)
		return false
	}
	s.auditFromRequest(r, "plan_mirrored", auditDetail("epic", tree.EpicID, "issue", repo+"#"+strconv.Itoa(number)), agentName)
	return true
}
