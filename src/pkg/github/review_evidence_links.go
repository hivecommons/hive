package github

import (
	"crypto/ed25519"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
)

// Change-failure links (hivecommons/hive#12067). When a PR merges, its title
// and body are scanned for references to earlier PRs of the same repo; a
// referenced PR whose bundle records a merge within reviewEvidenceLinkWindow
// before this one is linked both ways: revert_of / followup_of on the new
// bundle, reverted_by / followed_up_by on the old one. It is a proxy for the
// change failure rate, recorded best-effort and never consulted by a merge.

// reviewEvidenceLinkWindow is how far back a referenced PR's merge may lie.
const reviewEvidenceLinkWindow = evidence.LinkWindow

// reviewEvidenceMaxLinkRefs bounds how many referenced PRs one merge looks up.
const reviewEvidenceMaxLinkRefs = evidence.MaxLinkRefs

// reviewEvidenceChangeRefs returns the PR numbers text names as reverted and
// as followed up. The rules live in evidence.ChangeRefs so the history
// backfill (hivectl evidence backfill) links exactly as the live recorder.
func reviewEvidenceChangeRefs(repo string, number int, text string) (reverts, followups []int) {
	return evidence.ChangeRefs(repo, number, text)
}

// linkReviewEvidenceChanges sets b.RevertOf and b.FollowupOf from the PRs text
// references that merged within reviewEvidenceLinkWindow before mergedAt. It
// only reads other bundles; the reverse links are written by
// recordReviewEvidenceBacklinks once b is sealed.
func linkReviewEvidenceChanges(b *evidence.Bundle, text string, mergedAt time.Time) {
	if b == nil || strings.TrimSpace(text) == "" {
		return
	}
	reverts, followups := reviewEvidenceChangeRefs(b.Repo, b.Number, text)
	looked := 0
	link := func(have []string, numbers []int) []string {
		for _, n := range numbers {
			if looked >= reviewEvidenceMaxLinkRefs {
				break
			}
			looked++
			if id := mergedReviewEvidenceID(b.Repo, n, mergedAt); id != "" && !slices.Contains(have, id) {
				have = append(have, id)
			}
		}
		return have
	}
	b.RevertOf = link(b.RevertOf, reverts)
	b.FollowupOf = link(b.FollowupOf, followups)
}

// mergedReviewEvidenceID is the id of the bundle recording repo#number's
// merge, when that merge lies within reviewEvidenceLinkWindow before
// mergedAt, else "". A number with no bundle (an issue, or a PR Hive never
// reviewed) links nothing.
func mergedReviewEvidenceID(repo string, number int, mergedAt time.Time) string {
	entries, err := ListReviewEvidence("", repo, number)
	if err != nil {
		return ""
	}
	for i := len(entries) - 1; i >= 0; i-- {
		m := entries[i].Bundle.Merge
		if m == nil {
			continue
		}
		if age := mergedAt.Sub(m.At); age < 0 || age > reviewEvidenceLinkWindow {
			return ""
		}
		return entries[i].Bundle.ID
	}
	return ""
}

// recordReviewEvidenceBacklinks writes reverted_by / followed_up_by naming b
// on every bundle b reverts or follows up. A link already present is not
// added again, so a replayed merge changes nothing. Every failure is logged
// and swallowed: the merge has already happened.
func (c *Client) recordReviewEvidenceBacklinks(b *evidence.Bundle, settings ReviewEvidenceSettings) {
	if b == nil || len(b.RevertOf)+len(b.FollowupOf) == 0 {
		return
	}
	at := b.UpdatedAt
	if b.Merge != nil {
		at = b.Merge.At
	}
	key, err := evidence.LoadSigningKey(settings.SigningKeyFile)
	if err != nil {
		c.logger.Warn("review evidence: signing key unusable, writing unsigned",
			slog.String("error", err.Error()))
		key = nil
	}
	reviewEvidenceMu.Lock()
	defer reviewEvidenceMu.Unlock()
	for _, id := range b.RevertOf {
		c.addReviewEvidenceBacklink(id, b.ID, true, key, at)
	}
	for _, id := range b.FollowupOf {
		c.addReviewEvidenceBacklink(id, b.ID, false, key, at)
	}
}

// addReviewEvidenceBacklink adds from to the reverted_by (revert) or
// followed_up_by list of the bundle id names. Caller holds reviewEvidenceMu.
func (c *Client) addReviewEvidenceBacklink(id, from string, revert bool, key ed25519.PrivateKey, at time.Time) {
	repo, number, head, ok := parseReviewEvidenceID(id)
	if !ok {
		return
	}
	path := ReviewEvidencePath("", repo, number, head)
	b, err := LoadVerifiedReviewEvidence(path, reviewEvidenceVerifierForKey(key))
	if err != nil {
		c.logger.Warn("review evidence: linked bundle unreadable, back-link not written",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	links := &b.FollowedUpBy
	if revert {
		links = &b.RevertedBy
	}
	if slices.Contains(*links, from) {
		return
	}
	*links = append(*links, from)
	b.UpdatedAt = at
	if err := b.Validate(); err != nil {
		c.logger.Warn("review evidence: linked bundle invalid, not written",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := evidence.Seal(b, key); err != nil {
		c.logger.Warn("review evidence: could not seal linked bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	if err := writeReviewEvidence(path, b); err != nil {
		c.logger.Warn("review evidence: could not write linked bundle",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	c.logger.Info("review evidence: change link recorded",
		slog.String("id", b.ID), slog.String("by", from), slog.Bool("revert", revert))
}

// parseReviewEvidenceID splits a bundle id owner/repo#number@head.
func parseReviewEvidenceID(id string) (repo string, number int, head string, ok bool) {
	repo, rest, ok := strings.Cut(id, "#")
	if !ok {
		return "", 0, "", false
	}
	num, head, ok := strings.Cut(rest, "@")
	if !ok || head == "" {
		return "", 0, "", false
	}
	number, err := strconv.Atoi(num)
	if err != nil || number <= 0 {
		return "", 0, "", false
	}
	return repo, number, head, true
}
