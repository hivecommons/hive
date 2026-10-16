package github

import (
	"context"
	"log/slog"
	"strings"

	"github.com/hivecommons/hive/pkg/claims"
)

// The `hive/claimed-by-<who>` "who" modifier. One rule for the suffix lives
// in pkg/claims (ClaimedByPrefix): the GitHub login of the principal for any
// claim with a human behind it, the agent name for hive-agent claims, never
// a backend name. The writers here enforce it; the issue reconciler heals the
// malformed backend-named form a hand-applied label can introduce.

// ApplyClaimedByLabel puts exactly one claimed-by label for claim on the
// issue: any other `hive/claimed-by-*` label is removed first (a stale
// holder, or a backend-named suffix) and the derived one is added when
// absent. A claim with no usable holder (claims.ClaimedByLabel == "") only
// strips backend-named labels. Returns the labels it removed and whether it
// added one.
func (c *Client) ApplyClaimedByLabel(ctx context.Context, repo string, number int, claim claims.Claim) (removed []string, added bool, err error) {
	if c == nil {
		return nil, false, ErrNoGitHubClient
	}
	want := claims.ClaimedByLabel(claim)
	owner, name := c.splitRepo(repo)
	labeler := c.StageLabeler()
	current, err := labeler.Labels(ctx, owner, name, number)
	if err != nil {
		return nil, false, err
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
		if want == "" && !claims.ClaimedByLabelIsBackend(l) {
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

// RemoveClaimedByLabel removes the claimed-by label derived from claim. It
// never touches another holder's label: a human's `hive/claimed-by-<login>`
// survives the release of a contributor claim on the same issue.
func (c *Client) RemoveClaimedByLabel(ctx context.Context, repo string, number int, claim claims.Claim) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	label := claims.ClaimedByLabel(claim)
	if label == "" {
		return nil
	}
	return c.RemoveLabel(ctx, repo, number, label)
}

// healBackendClaimedByLabels replaces a backend-named `hive/claimed-by-*`
// label (`-copilot`, `-claude`, `-pi`, …) on an issue the reconciler sees
// with the label of its live claim holder, or just removes it when the issue
// carries no live claim. Only the malformed form is touched: a label naming
// a principal is left alone even when it is not the live holder, because an
// issue may legitimately carry one label per party (a human's and a
// contributor's). Returns how many labels it rewrote.
func healBackendClaimedByLabels(ctx context.Context, client *Client, in IssueStageInput, logger *slog.Logger) int {
	var bad []string
	for _, l := range in.Labels {
		if claims.ClaimedByLabelIsBackend(l) {
			bad = append(bad, l)
		}
	}
	if len(bad) == 0 || client == nil {
		return 0
	}
	want := ""
	if in.Bucket == workBucketClaimed && in.ClaimedBy != "" {
		want = claims.ClaimedByLabel(claims.Claim{Holder: in.ClaimedBy, Kind: claims.Kind(strings.ToLower(in.ClaimKind))})
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
