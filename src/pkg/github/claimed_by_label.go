package github

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/issueclaim"
)

// The `hive/claimed-by-<who>` "who" modifier. One rule for the suffix lives
// in pkg/claims (ClaimedByPrefix): the generic `human` for any claim with a
// human behind it, the agent name for hive-agent claims, never a login and
// never a backend name. The writers here enforce it; the issue reconciler
// heals the malformed backend-named form a hand-applied label can introduce
// and swaps the retired per-login spelling of the live holder for the
// generic label.

// ApplyClaimedByLabel puts the claim's own claimed-by label on the issue (one
// label per party): backend-named `hive/claimed-by-*` labels and this
// holder's retired per-login label are removed and the derived one is added
// when absent. Other parties' labels are left alone — a person's label
// survives a contributor's claim, matching healBackendClaimedByLabels and
// RemoveClaimedByLabel. An agent claim also clears the generic human label
// when the latest human-principal marker has a matching release; a missing
// or unreleased marker is not evidence that another party's label is stale.
// A claim with no usable holder (claims.ClaimedByLabel == "") only
// strips backend-named labels. Returns the labels it removed and whether it
// added one.
func (c *Client) ApplyClaimedByLabel(ctx context.Context, repo string, number int, claim claims.Claim) (removed []string, added bool, err error) {
	if c == nil {
		return nil, false, ErrNoGitHubClient
	}
	want := claims.ClaimedByLabel(claim)
	legacy := ""
	if claim.Kind != claims.KindAgent {
		legacy = claims.LegacyClaimedByLabel(claim.Holder)
	}
	owner, name := c.splitRepo(repo)
	labeler := c.StageLabeler()
	current, err := labeler.Labels(ctx, owner, name, number)
	if err != nil {
		return nil, false, err
	}
	removeReleasedHuman := false
	if claim.Kind == claims.KindAgent && want != "" && issueHasLabel(current, claims.ClaimedByHumanLabel) {
		// Read comments fresh: the new agent's marker may already be the
		// latest overall, and the claim cache may predate the human release.
		comments, readErr := c.listIssueComments(ctx, owner, name, number)
		if readErr != nil {
			return nil, false, readErr
		}
		var bodies []string
		for _, comment := range comments {
			body := comment.GetBody()
			if marker, ok := issueclaim.ParseMarker(body); ok && strings.EqualFold(marker.Kind, string(claims.KindAgent)) {
				continue
			}
			bodies = append(bodies, body)
		}
		previous, found := issueclaim.Latest(bodies, issueclaim.DefaultTTL)
		removeReleasedHuman = found && previous.Released
	}
	have := false
	for _, l := range current {
		if !claims.IsClaimedByLabel(l) {
			continue
		}
		if want != "" && strings.EqualFold(l, want) {
			have = true
			continue
		}
		if !claims.ClaimedByLabelIsBackend(l) && !(legacy != "" && strings.EqualFold(l, legacy)) && !(removeReleasedHuman && claims.IsClaimedByHumanLabel(l)) {
			continue
		}
		if rerr := labeler.RemoveLabel(ctx, owner, name, number, l); rerr != nil {
			return removed, false, rerr
		}
		removed = append(removed, l)
	}
	if want == "" || have {
		return removed, false, nil
	}
	if err := labeler.AddLabels(ctx, owner, name, number, []string{want}); err != nil {
		return removed, false, err
	}
	return removed, true, nil
}

// RemoveClaimedByLabel removes the claimed-by label(s) derived from claim:
// the current one and, for a human-principal claim, the retired per-login
// spelling. It never touches another holder's label: an agent's
// `hive/claimed-by-scanner` survives the release of a human claim on the
// same issue.
func (c *Client) RemoveClaimedByLabel(ctx context.Context, repo string, number int, claim claims.Claim) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	for _, label := range claims.ClaimedByLabels(claim) {
		if err := c.RemoveLabel(ctx, repo, number, label); err != nil {
			return err
		}
	}
	return nil
}

// healBackendClaimedByLabels replaces a backend-named `hive/claimed-by-*`
// label (`-copilot`, `-claude`, `-pi`, …) on an issue the reconciler sees
// with the label of its live claim holder, or just removes it when the issue
// carries no live claim. When the live holder is a human principal, its
// retired per-login label (`hive/claimed-by-<login>`) is swapped for the
// generic `hive/claimed-by-human` the same way. Only those forms are
// touched: an unrelated label is left alone even when it is not the live
// holder, because an issue may legitimately carry one label per party (a
// human's and a contributor's). Returns how many labels it rewrote.
func healBackendClaimedByLabels(ctx context.Context, client *Client, in IssueStageInput, logger *slog.Logger) int {
	want, legacy := "", ""
	if in.Bucket == workBucketClaimed && in.ClaimedBy != "" {
		live := claims.Claim{Holder: in.ClaimedBy, Kind: claims.Kind(strings.ToLower(in.ClaimKind))}
		want = claims.ClaimedByLabel(live)
		if live.Kind != claims.KindAgent {
			legacy = claims.LegacyClaimedByLabel(live.Holder)
		}
	}
	var bad []string
	for _, l := range in.Labels {
		if claims.ClaimedByLabelIsBackend(l) || (legacy != "" && strings.EqualFold(l, legacy)) {
			bad = append(bad, l)
		}
	}
	if len(bad) == 0 || client == nil {
		return 0
	}
	healed := 0
	for _, l := range bad {
		if err := client.RemoveLabel(ctx, in.Repo, in.Number, l); err != nil {
			if logger != nil {
				logger.Warn("claimed-by label: removing backend-named label failed", "repo", in.Repo, "issue", in.Number, "label", l, "error", err)
			}
			continue
		}
		healed++
	}
	if want != "" && !issueHasLabel(in.Labels, want) {
		if err := client.AddLabels(ctx, in.Repo, in.Number, []string{want}); err != nil && logger != nil {
			logger.Warn("claimed-by label: adding holder label failed", "repo", in.Repo, "issue", in.Number, "label", want, "error", err)
		}
	}
	if logger != nil && healed > 0 {
		logger.Info("claimed-by label healed", "repo", in.Repo, "issue", in.Number, "removed", bad, "added", want)
	}
	return healed
}
