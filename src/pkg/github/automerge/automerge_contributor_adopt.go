package automerge

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Stage-074 adoption (hivecommons/hive#12436).
//
// A contributor PR in stage/072-returned-to-contributor whose author has not
// pushed a new head within the grace after the nudge is adopted: the hive
// re-lanes it through the takeover path so the scanner can finish it. The
// grace is contributor_grace_online (default 2h) when the author's relay
// was reachable at the nudge, contributor_grace_offline (1h) otherwise — an
// external author or an offline relay is the offline case, and a relay
// contributor who was over their fix budget ("queued") counts as online
// because adoption is the pressure-relief valve for THEIR overflow too.
//
// Adopting means: push `hive/adopt-<n>` in the base repo at the contributor
// head (the fork's commits, authorship and sign-off preserved byte for byte),
// open an App PR from it that supersedes the original and carries its
// Closes lines, comment once on the original, and move it to
// stage/074-adopted-by-hive. The App PR is red with the same failures, so it
// lands in ci-failing.json and the scanner's FIX-BEFORE-NEW kick (the
// existing red-PR repair route for hive-authored PRs) rebases it on the base
// and fixes CI on that branch — no second kick mechanism. Once the App PR
// merges the original is closed with a note; if the App PR is closed
// unmerged the adoption is forgotten and the original falls back to 072.
//
// One attempt per contributor head, whatever the outcome; a new head from
// the author before adoption re-arms the nudge instead.

const (
	// adoptBranchPrefix names the App branch that adopts contributor PR <n>.
	adoptBranchPrefix = "hive/adopt-"
	// AdoptionMarkerPrefix opens the one adoption comment on the original PR.
	AdoptionMarkerPrefix = "<!-- hive:adopted"
	// adoptionCloseMarker is stamped on the close note after the App PR merged.
	adoptionCloseMarker = "<!-- hive:adopted-merged -->"
	// adoptCheckInterval bounds how often an adopted PR's superseder is polled.
	adoptCheckInterval = 10 * time.Minute
)

// adoptClosesPattern lifts the original PR's closing keywords so the
// superseding PR closes the same issues.
var adoptClosesPattern = regexp.MustCompile(`(?im)^\s*(?:fixes|closes|resolves)\s+(?:[\w.-]+/[\w.-]+)?#\d+\s*$`)

// adoptionGrace returns the grace that applies to the record's first nudge.
func adoptionGrace(rec stagelabels.Record, policy StageLabelPolicy) time.Duration {
	if rec.NudgeOnline {
		if policy.GraceOnline > 0 {
			return policy.GraceOnline
		}
		return 2 * time.Hour
	}
	if policy.GraceOffline > 0 {
		return policy.GraceOffline
	}
	return 15 * time.Minute
}

// ContributorPresence is what the hub knows about a contributor's relay:
// whether they have a contributor profile on this hive at all, whether a
// relay heartbeat is within the presence window right now, and the last
// heartbeat seen (zero when none this process).
type ContributorPresence struct {
	Registered bool
	Online     bool
	LastSeen   time.Time
}

// ContributorPresenceSource is the optional second face of the nudge sink:
// live presence, re-read every sweep, so adoption reacts to a relay that went
// away AFTER the nudge instead of trusting the snapshot taken at nudge time.
type ContributorPresenceSource interface {
	Presence(ctx context.Context, login string) ContributorPresence
}

// Adoption reasons recorded on the store and in the adoption marker.
const (
	AdoptReasonOffline      = "contributor-offline"
	AdoptReasonUnregistered = "contributor-unregistered"
	AdoptReasonGrace        = "grace-elapsed"
)

// contributorPresence asks the sink for live presence; ok is false when the
// sink does not provide it.
func (c *Engine) contributorPresence(ctx context.Context, login string) (ContributorPresence, bool) {
	src, ok := c.contributorNudge.(ContributorPresenceSource)
	if !ok || src == nil || strings.TrimSpace(login) == "" {
		return ContributorPresence{}, false
	}
	return src.Presence(ctx, login), true
}

// adoptionDue reports whether the PR at head has sat nudged long enough with
// no new head, and was not already adopted at this head; reason says which
// rule fired. With live presence: a registered relay that is online keeps
// the online grace (its hive may be mid-fix); a registered relay that is
// offline is adopted once it has been gone AdoptAfterOffline (counted from
// its last heartbeat or the first nudge, whichever is later); an author with
// no profile here (another hive posted the PR) is adopted after
// AdoptAfterUnregistered. Without presence the nudge-time graces apply.
func adoptionDue(rec stagelabels.Record, head string, policy StageLabelPolicy, now time.Time, presence ContributorPresence, hasPresence bool) (bool, string) {
	start := rec.NudgeStart()
	if head == "" || rec.NudgeHead != head || rec.NudgeCount == 0 || start.IsZero() {
		return false, ""
	}
	if rec.Adopted(head) || rec.AdoptedInPlace() {
		return false, ""
	}
	if !hasPresence {
		return now.Sub(start) >= adoptionGrace(rec, policy), AdoptReasonGrace
	}
	switch {
	case presence.Registered && presence.Online:
		grace := policy.GraceOnline
		if grace <= 0 {
			grace = 2 * time.Hour
		}
		return now.Sub(start) >= grace, AdoptReasonGrace
	case presence.Registered:
		wait := policy.AdoptAfterOffline
		if wait <= 0 {
			wait = 15 * time.Minute
		}
		since := start
		if presence.LastSeen.After(since) {
			since = presence.LastSeen
		}
		return now.Sub(since) >= wait, AdoptReasonOffline
	default:
		wait := policy.AdoptAfterUnregistered
		if wait <= 0 {
			wait = 15 * time.Minute
		}
		return now.Sub(start) >= wait, AdoptReasonUnregistered
	}
}

// adoptReasonText is the human-readable half of the adoption note.
func adoptReasonText(reason string, grace time.Duration, online bool) string {
	switch reason {
	case AdoptReasonOffline:
		return "your hive has been offline since the nudge"
	case AdoptReasonUnregistered:
		return "no hive of yours is registered here and nothing moved after the nudge"
	}
	if online {
		return fmt.Sprintf("CI stayed red for %s after the nudge and the relay nudge went unanswered", formatGrace(grace))
	}
	return fmt.Sprintf("CI stayed red for %s after the nudge and no relay session was reachable", formatGrace(grace))
}

// adoptionBlocked reports a label that forbids adoption (the hive must not
// take over a PR a human parked).
func adoptionBlocked(labels []string) string {
	for _, l := range labels {
		switch strings.ToLower(strings.TrimSpace(l)) {
		case "hold", "do-not-merge", "do-not-merge/hold":
			return strings.TrimSpace(l)
		}
	}
	return ""
}

// adoptionCloses extracts the closing-keyword lines from the original body.
func adoptionCloses(body string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range adoptClosesPattern.FindAllString(body, -1) {
		line := strings.TrimSpace(m)
		if !seen[strings.ToLower(line)] {
			seen[strings.ToLower(line)] = true
			out = append(out, line)
		}
	}
	return out
}

// AdoptionPRBody is the superseding App PR's body.
func AdoptionPRBody(obs stageObservation, rec stagelabels.Record) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Supersedes #%d (adopted after CI went red and the contributor was unavailable; original commits by @%s preserved).\n\n", obs.Number, obs.Author)
	for _, c := range adoptionCloses(obs.Body) {
		b.WriteString(c)
		b.WriteString("\n")
	}
	if len(adoptionCloses(obs.Body)) > 0 {
		b.WriteString("\n")
	}
	fmt.Fprintf(&b, "This branch is #%d's head `%s` as pushed by @%s", obs.Number, shortSHA(obs.HeadSHA), obs.Author)
	if obs.HeadRepo != "" && !strings.EqualFold(obs.HeadRepo, obs.Display) {
		fmt.Fprintf(&b, " (fork `%s`)", obs.HeadRepo)
	}
	b.WriteString(", unchanged. Last lane reason on the original: `")
	b.WriteString(nonEmpty(rec.Reason, obs.Reason))
	b.WriteString("`.\n\n")
	b.WriteString("**Hive fix agent:** rebase this branch on `")
	b.WriteString(nonEmpty(obs.BaseRef, "the base branch"))
	b.WriteString("`, fix the failing checks / review asks, and push to THIS branch. Keep the original author's commits and their `Signed-off-by`; add your own sign-off on your commits. Do not open another PR.\n")
	return b.String()
}

// AdoptionComment is the one note left on the original PR when adopted by
// supersession.
func AdoptionComment(obs stageObservation, hiveID string, superseding int, reason string, grace time.Duration, online bool) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s superseded_by=%d head=%s -->\n@%s the hive has adopted this PR (`%s`): %s. "+
		"It is being finished as #%d, which carries your commits (and sign-off) unchanged; the hive will rebase and fix CI there. "+
		"This PR is closed automatically once #%d merges. If you would rather finish it yourself, push a new head here and say so on #%d.",
		AdoptionMarkerPrefix, nonEmpty(hiveID, "hive"), reason, stagelabels.AdoptModeSupersede, superseding, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), adoptReasonText(reason, grace, online), superseding, superseding, superseding)
}

// AdoptionInPlaceComment is the one note left when the hive adopts the PR on
// its own branch.
func AdoptionInPlaceComment(obs stageObservation, hiveID, reason string, grace time.Duration, online bool) string {
	return fmt.Sprintf("%s by=%s reason=%s mode=%s head=%s -->\n@%s the hive has adopted this PR (`%s`): %s. "+
		"The hive will rebase this branch on `%s` and fix CI here; your commits and sign-off stay. "+
		"Push a new head yourself at any time and the hive yields it back to you.",
		AdoptionMarkerPrefix, nonEmpty(hiveID, "hive"), reason, stagelabels.AdoptModeInPlace, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), adoptReasonText(reason, grace, online), nonEmpty(obs.BaseRef, "the base branch"))
}

// AdoptionYieldMarker opens the note posted when the contributor pushes over
// an in-place adoption.
const AdoptionYieldMarker = "<!-- hive:adoption-yield"

// AdoptionYieldComment is that note.
func AdoptionYieldComment(obs stageObservation) string {
	return fmt.Sprintf("%s head=%s -->\n@%s this PR has a new head without confirmed hive push evidence, so the hive has handed this PR back to you (`%s` → `%s`). It will nudge again if CI stays red.",
		AdoptionYieldMarker, obs.HeadSHA, obs.Author, stagelabels.AdoptedByHive.Label(), stagelabels.ReturnedToContributor.Label())
}

func formatGrace(d time.Duration) string {
	if d%time.Hour == 0 {
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return d.Round(time.Minute).String()
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// adoptContributorPR runs the stage-074 check for one clanker PR in stage
// 072 and, when adoption is due, performs it: in place (the hive's fix lane
// pushes to the PR's own branch) when GitHub permits and the policy allows,
// else by supersession. It returns the number of comments posted. Failures
// are logged and the attempt is still recorded (one per head) so a broken
// API never loops on the same PR.
func (c *Engine) adoptContributorPR(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy, presence ContributorPresence, hasPresence bool, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || obs.Lane == stageLaneSelf || obs.Author == "" || obs.BaseRef == "" {
		return 0, nil
	}
	due, reason := adoptionDue(rec, obs.HeadSHA, policy, now, presence, hasPresence)
	if !due {
		return 0, nil
	}
	if l := adoptionBlocked(obs.Labels); l != "" {
		c.info("stage labels: adoption due but blocked by label", "repo", obs.Display, "pr", obs.Number, "label", l)
		return 0, nil
	}
	grace := adoptionGrace(rec, policy)
	appLogin := strings.TrimSpace(c.transport.AppBotLogin())
	// Record the attempt first: whatever happens below, this head is tried once.
	store.MarkAdopted(key, obs.HeadSHA, 0, now)
	if appLogin == "" {
		c.warn("stage labels: cannot adopt contributor PR, App login unknown", "repo", obs.Display, "pr", obs.Number)
		return 0, nil
	}
	c.info("stage labels: adopting contributor PR", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "reason", reason, "registered", presence.Registered, "online", presence.Online, "lane_reason", obs.Reason)

	if policy.AdoptInPlace && c.canPushToPRBranch(ctx, obs) {
		store.MarkAdoptedInPlace(key, obs.HeadSHA, reason, now)
		body := AdoptionInPlaceComment(obs, policy.HiveID, reason, grace, rec.NudgeOnline)
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: adoption comment failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 0, err
		}
		c.info("stage labels: contributor PR adopted in place", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "reason", reason)
		return 1, nil
	}

	owner, repo, number := obs.Owner, obs.Repo, obs.Number
	branch := adoptBranchPrefix + strconv.Itoa(number)
	fullRepo := owner + "/" + repo
	_, err := effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: fullRepo, Kind: effects.KindBranchPush, Target: branch, Actor: "stage_labels",
		Inputs: map[string]string{"pr": strconv.Itoa(number), "head_sha": obs.HeadSHA, "adopt": "true"},
	}, func(ctx context.Context) (effects.Result, error) {
		_, _, apiErr := c.gh.Git.CreateRef(ctx, owner, repo, &gh.Reference{
			Ref:    gh.Ptr("refs/heads/" + branch),
			Object: &gh.GitObject{SHA: gh.Ptr(obs.HeadSHA)},
		})
		if apiErr != nil && isGitHubStatus(apiErr, http.StatusUnprocessableEntity) {
			// The branch exists from an earlier attempt: point it at this head.
			_, _, apiErr = c.gh.Git.UpdateRef(ctx, owner, repo, &gh.Reference{
				Ref:    gh.Ptr("refs/heads/" + branch),
				Object: &gh.GitObject{SHA: gh.Ptr(obs.HeadSHA)},
			}, true)
		}
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: obs.HeadSHA}, nil
	})
	if err != nil {
		c.warn("stage labels: adoption failed to push branch", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}

	var created *gh.PullRequest
	_, err = effects.Execute(ctx, c.mutation, effects.Claim{
		Repo: fullRepo, Kind: effects.KindPullRequestCreate, Target: branch, Actor: "stage_labels",
		Inputs: map[string]string{"base": obs.BaseRef, "supersedes": strconv.Itoa(number), "head_sha": obs.HeadSHA},
	}, func(ctx context.Context) (effects.Result, error) {
		var apiErr error
		created, _, apiErr = c.gh.PullRequests.Create(ctx, owner, repo, &gh.NewPullRequest{
			Title: gh.Ptr(nonEmpty(strings.TrimSpace(obs.Title), fmt.Sprintf("Adopt #%d", number))),
			Head:  gh.Ptr(branch),
			Base:  gh.Ptr(obs.BaseRef),
			Body:  gh.Ptr(AdoptionPRBody(obs, rec)),
		})
		if apiErr != nil {
			return effects.Result{}, effects.NotApplied(apiErr)
		}
		return effects.Result{Provenance: created.GetHTMLURL()}, nil
	})
	if err != nil || created == nil {
		c.warn("stage labels: adoption failed to open App PR", "repo", obs.Display, "pr", number, "branch", branch, "error", err)
		return 0, err
	}
	superseding := created.GetNumber()
	store.MarkAdopted(key, obs.HeadSHA, superseding, now)
	c.info("stage labels: contributor PR adopted", "repo", obs.Display, "pr", number, "head", obs.HeadSHA, "superseded_by", superseding, "branch", branch)

	body := AdoptionComment(obs, policy.HiveID, superseding, reason, grace, rec.NudgeOnline)
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("stage labels: adoption comment failed", "repo", obs.Display, "pr", number, "superseded_by", superseding, "error", err)
		return 0, err
	}
	return 1, nil
}

// settleAdoption polls the superseding App PR for a PR in stage 074 (at
// most every adoptCheckInterval): merged → close the original with a note;
// closed unmerged → forget the adoption so the original returns to 072.
func (c *Engine) settleAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, now time.Time) (int, error) {
	rec, ok := store.Get(key)
	if !ok || rec.AdoptedBy <= 0 {
		return 0, nil
	}
	if !rec.AdoptCheckedAt.IsZero() && now.Sub(rec.AdoptCheckedAt) < adoptCheckInterval {
		return 0, nil
	}
	store.MarkAdoptChecked(key, now)
	sup, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, rec.AdoptedBy)
	if err != nil {
		c.warn("stage labels: adoption superseder lookup failed", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy, "error", err)
		return 0, err
	}
	switch {
	case sup.GetMerged():
		body := fmt.Sprintf("%s\nClosing: #%d (the hive's adoption of this PR, with @%s's commits) has merged. Thank you!", adoptionCloseMarker, rec.AdoptedBy, obs.Author)
		if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			c.warn("stage labels: adoption close note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 0, err
		}
		if _, _, err := c.gh.PullRequests.Edit(ctx, obs.Owner, obs.Repo, obs.Number, &gh.PullRequest{State: gh.Ptr("closed")}); err != nil {
			c.warn("stage labels: closing adopted PR failed", "repo", obs.Display, "pr", obs.Number, "error", err)
			return 1, err
		}
		c.info("stage labels: adopted PR closed after superseder merged", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy)
		return 1, nil
	case strings.EqualFold(sup.GetState(), "closed"):
		c.warn("stage labels: adoption superseder closed unmerged; returning PR to the contributor", "repo", obs.Display, "pr", obs.Number, "superseded_by", rec.AdoptedBy)
		store.ClearAdoption(key)
	}
	return 0, nil
}

// canPushToPRBranch reports whether the App can push to the PR's head: a
// head in the base repo always; a fork head only with "allow edits by
// maintainers", which the list payload omits, so one GET resolves it.
func (c *Engine) canPushToPRBranch(ctx context.Context, obs stageObservation) bool {
	if obs.HeadRepo == "" || strings.EqualFold(obs.HeadRepo, obs.Owner+"/"+obs.Repo) {
		return obs.HeadRepo != ""
	}
	pr, _, err := c.gh.PullRequests.Get(ctx, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		c.warn("stage labels: adoption could not read maintainer_can_modify; superseding", "repo", obs.Display, "pr", obs.Number, "error", err)
		return false
	}
	return pr.GetMaintainerCanModify()
}

// settleInPlaceAdoption handles a head change on an in-place-adopted PR with a
// fail-closed attribution rule. The hive keeps ownership only when the new
// commit carries GitHub-controlled evidence linking it to the App bot; the
// stage store remembers heads that already passed that check so a re-observed
// SHA is not re-fetched, but it is not a separate evidence source. Forgeable
// git author/committer names and emails never count. If the commit lookup
// fails, the pusher is unknown and the hive keeps adoption to avoid bounce
// loops. The first deploy after this rule also keeps adoption until this repo
// has recorded any App-linked head; after that, missing hive evidence means
// the contributor is back, so the hive yields once and the ordinary 072 flow
// re-arms.
// Returns the number of comments posted.
func (c *Engine) settleInPlaceAdoption(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, prev stagelabels.Record, policy StageLabelPolicy) (int, error) {
	if !prev.AdoptedInPlace() || obs.HeadSHA == "" || obs.HeadSHA == prev.AdoptedHead {
		return 0, nil
	}
	if prev.YieldedHead == obs.HeadSHA {
		return 0, nil
	}
	if prev.HivePushedHead(obs.HeadSHA) {
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.info("stage labels: adopted PR advanced by recorded hive push", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA)
		return 0, nil
	}
	app := c.transport.AppBotLogin()
	commit, _, err := c.gh.Repositories.GetCommit(ctx, obs.Owner, obs.Repo, obs.HeadSHA, nil)
	if err != nil {
		// Unknown pusher: keep the adoption rather than bounce the PR.
		c.warn("stage labels: adopted head changed; pusher unknown, keeping adoption", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "error", err)
		store.MarkAdoptedHead(key, obs.HeadSHA)
		return 0, nil
	}
	if commitByApp(commit, app) {
		store.MarkHivePushedHead(key, obs.HeadSHA)
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.info("stage labels: adopted PR advanced by the hive", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
		return 0, nil
	}
	if !policy.AdoptionStrictAttribution {
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.warn("stage labels: adopted head changed without hive evidence; strict attribution disabled, keeping adoption", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
		return 0, nil
	}
	if !store.HasHivePushEvidence(obs.Owner + "/" + obs.Repo) {
		store.MarkAdoptedHead(key, obs.HeadSHA)
		c.warn("stage labels: adopted head changed without hive evidence; no repo evidence recorded yet, keeping adoption", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
		return 0, nil
	}
	store.MarkYielded(key, obs.HeadSHA)
	c.info("stage labels: head changed without hive push evidence; yielding", "repo", obs.Display, "pr", obs.Number, "author", obs.Author, "head", obs.HeadSHA, "pusher", commitIdentity(commit))
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(AdoptionYieldComment(obs))}); err != nil {
		c.warn("stage labels: adoption yield note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	return 1, nil
}

// commitIdentity renders the commit's pusher for logs: the GitHub login when
// linked, else the git author name/email.
func commitIdentity(commit *gh.RepositoryCommit) string {
	if commit == nil {
		return ""
	}
	if l := commit.GetAuthor().GetLogin(); l != "" {
		return l
	}
	if l := commit.GetCommitter().GetLogin(); l != "" {
		return l
	}
	if c := commit.GetCommit(); c != nil {
		if a := c.GetAuthor(); a != nil {
			if name := strings.TrimSpace(a.GetName()); name != "" {
				return name
			}
			return strings.TrimSpace(a.GetEmail())
		}
	}
	return ""
}

// commitByApp reports whether GitHub linked the commit to the App's bot login.
// A verified signature is useful only with that GitHub-controlled linkage; git
// names/emails and unsigned linked contributor commits never establish hive
// ownership.
func commitByApp(commit *gh.RepositoryCommit, login string) bool {
	if commit == nil || login == "" {
		return false
	}
	if commitByLogin(commit, login) {
		return true
	}
	if v := commit.GetCommit().GetVerification(); v != nil && v.GetVerified() {
		return commitByLogin(commit, login)
	}
	return false
}

// commitByLogin reports whether the commit's GitHub author or committer is
// login ([bot] suffix tolerated).
func commitByLogin(commit *gh.RepositoryCommit, login string) bool {
	if commit == nil {
		return false
	}
	want := normalizeBotLogin(login)
	if want == "" {
		return false
	}
	return normalizeBotLogin(commit.GetAuthor().GetLogin()) == want || normalizeBotLogin(commit.GetCommitter().GetLogin()) == want
}

func normalizeBotLogin(login string) string {
	login = strings.ToLower(strings.TrimSpace(login))
	return strings.TrimSuffix(login, "[bot]")
}
