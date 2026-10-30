package dashboard

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/github/automerge"
)

// ContributorAbandonRecord is one PR of a contributor the hive adopted
// (hivecommons/hive#15782). AdoptedIntoPR is the hive's superseding PR, 0
// while the adoption is in place on the contributor's own branch.
type ContributorAbandonRecord struct {
	Repo          string `json:"repo"`
	PR            int    `json:"pr"`
	AdoptedIntoPR int    `json:"adopted_into_pr"`
	Reason        string `json:"reason"`
	At            string `json:"at"`
}

func contributorAbandonKey(repo string, pr int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(pr)
}

// applyContributorAbandon adds rec to p once per PR. A later adoption of an
// already-counted PR (an in-place adoption re-homed onto hive/adopt-<n>, a
// re-adoption after an orphaned copy) only fills in the superseding PR the
// first record lacked; it never counts again. changed reports whether p
// needs saving, counted whether the counter moved.
func applyContributorAbandon(p *ContributorProfile, rec ContributorAbandonRecord) (changed, counted bool) {
	key := contributorAbandonKey(rec.Repo, rec.PR)
	for i := range p.AbandonedPRs {
		if contributorAbandonKey(p.AbandonedPRs[i].Repo, p.AbandonedPRs[i].PR) != key {
			continue
		}
		if p.AbandonedPRs[i].AdoptedIntoPR == 0 && rec.AdoptedIntoPR > 0 {
			p.AbandonedPRs[i].AdoptedIntoPR = rec.AdoptedIntoPR
			return true, false
		}
		return false, false
	}
	p.AbandonedPRs = append(p.AbandonedPRs, rec)
	p.Abandoned = len(p.AbandonedPRs)
	return true, true
}

// mergeContributorAbandons is the save-time reconcile: every record on disk
// survives, plus the incoming ones not already there (deduped per PR).
func mergeContributorAbandons(disk, incoming []ContributorAbandonRecord) []ContributorAbandonRecord {
	if len(disk) == 0 && len(incoming) == 0 {
		return nil
	}
	merged := &ContributorProfile{AbandonedPRs: append([]ContributorAbandonRecord(nil), disk...)}
	for _, rec := range incoming {
		applyContributorAbandon(merged, rec)
	}
	return merged.AbandonedPRs
}

// recordContributorAbandon persists one adoption against the author's
// contributor profile. Authors without a profile on this hive (people,
// other hives' clankers, bots) are not recorded.
func recordContributorAbandon(a automerge.ContributorAbandon) (counted bool, err error) {
	login := strings.TrimSpace(strings.TrimPrefix(a.Author, "@"))
	repo := strings.TrimSpace(a.Repo)
	if login == "" || repo == "" || a.PR <= 0 {
		return false, nil
	}
	at := a.At
	if at.IsZero() {
		at = time.Now()
	}
	rec := ContributorAbandonRecord{Repo: repo, PR: a.PR, AdoptedIntoPR: a.AdoptedIntoPR, Reason: a.Reason, At: at.UTC().Format(time.RFC3339)}
	for attempt := 0; attempt < 3; attempt++ {
		p := findContributor(login)
		if p == nil || !strings.EqualFold(p.GitHubUsername, login) {
			return false, nil
		}
		changed, counted := applyContributorAbandon(p, rec)
		if !changed {
			return false, nil
		}
		err = saveContributorProfile(p)
		if errors.Is(err, errProfileConflict) {
			continue
		}
		return counted, err
	}
	return false, err
}

// Abandoned implements automerge.ContributorAbandonSink.
func (h *ContributeWSHub) Abandoned(_ context.Context, a automerge.ContributorAbandon) {
	counted, err := recordContributorAbandon(a)
	if h == nil || h.logger == nil {
		return
	}
	if err != nil {
		h.logger.Warn("[contribute-ws] abandoned PR not recorded", "username", a.Author, "repo", a.Repo, "pr", a.PR, "error", err)
		return
	}
	if counted {
		h.logger.Info("[contribute-ws] abandoned PR recorded", "username", a.Author, "repo", a.Repo, "pr", a.PR, "adopted_into_pr", a.AdoptedIntoPR, "reason", a.Reason)
	}
}

var _ automerge.ContributorAbandonSink = (*ContributeWSHub)(nil)
