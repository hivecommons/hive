package dashboard

import (
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// delegatedForkGuard is the fork guard on the head under decision
// (hivecommons/hive#14273): the red checks that refuse the PR only because
// its head is in a fork, so neither its author nor the hive's fixer can turn
// them green. It reads what the stage engine reads, with no new GitHub call:
//
//   - the status snapshot's copy of the PR at this head, through the same
//     rule the stage engine's index applies to the enumeration's failure
//     excerpt (github.PullRequest.ForkGuardChecks);
//   - the guard the stage record remembers for the PR (its fork-guard
//     checks, else a `fork-guard:` park reason), which, as in the stage
//     engine, covers a later head red on nothing else.
//
// It answers only for a fork PR and names the red checks on this head that
// either source knows as guarded; the delegate then owes the rest of red as
// usual (delegatedecision.Input.ForkGuarded splits them). So a PR parked for
// another reason (an adoption that stalled on the fork) still carries the
// fact, and a guard beside another red check is still named as the
// maintainer's step. A head neither source has classified is read as
// ordinary red: the stage engine's own annotation read is not repeated here.
func (s *Server) delegatedForkGuard(repoFull string, number int, head string, fork bool, red []string) []string {
	head = strings.TrimSpace(head)
	if !fork || head == "" || len(red) == 0 {
		return nil
	}
	var known []string
	if pr, ok := s.delegatedSnapshotPR(repoFull, number); ok && strings.TrimSpace(pr.HeadSHA) == head {
		known = append(known, pr.ForkGuardChecks()...)
	}
	if rec, ok := s.stageStoreOrShared().Get(stagelabels.Key(repoFull, number)); ok {
		known = append(known, rec.ForkGuardChecks...)
		if list, found := strings.CutPrefix(strings.TrimSpace(rec.Reason), delegatedecision.ReasonForkGuardPrefix); found {
			for _, name := range strings.Split(list, ",") {
				if name = strings.TrimSpace(name); name != "" {
					known = append(known, name)
				}
			}
		}
	}
	var guarded []string
	for _, name := range red {
		if slices.Contains(known, name) {
			guarded = append(guarded, name)
		}
	}
	return guarded
}

// delegatedSnapshotPR is repoFull#number as the latest status snapshot
// enumerated it (open or held), with its CI evidence.
func (s *Server) delegatedSnapshotPR(repoFull string, number int) (github.PullRequest, bool) {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	if s.status == nil {
		return github.PullRequest{}, false
	}
	for _, repo := range s.status.Repos {
		if !strings.EqualFold(overviewRepoName(repo), repoFull) && !strings.EqualFold(repo.Full, repoFull) {
			continue
		}
		for _, held := range []bool{false, true} {
			for _, fp := range frontendRepoPRs(repo, held) {
				if fp.pr.Number == number {
					return fp.pr, true
				}
			}
		}
	}
	return github.PullRequest{}, false
}
