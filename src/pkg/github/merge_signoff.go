package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Signed-off-by through the hive's squash merges (hivecommons/hive#18203).
//
// When the hive sent an empty commit message, GitHub wrote its own default
// squash body, and for #18079 that body was only Co-authored-by lines: the
// branch commit's Signed-off-by never reached v5 (f33758f5a), and every
// branch that later merged v5 forward failed DCO on it. A squash is the one
// place the trailer can be lost after the PR-time DCO check passed, so the
// hive no longer leaves the body to GitHub: it rebuilds it from the branch
// commits, makes sure every branch sign-off is in it, and refuses to squash
// when the branch carries none at all in a repo that requires DCO. It never
// signs off on anyone's behalf.
//
// A repo requires DCO by the same rule the stage router routes a missing
// sign-off on (dcoRequired in pkg/github/automerge): the per-repo
// governor.labels.stage.dco.required override, else a detected `dco`/`DCO`
// gate. Refusing everywhere stalled every unsigned PR in repos with no DCO
// gate at all, Renovate and Dependabot PRs included, at 080 with nothing a
// person could do (projectbluefin/bluefin-report#91).

// ErrSquashUnsigned is returned when a squash merge would land a commit with
// no Signed-off-by in a repo that requires DCO: none of the branch commits
// carries one.
var ErrSquashUnsigned = errors.New("squash refused: no branch commit carries a Signed-off-by trailer, the landing commit would fail DCO")

// signedOffByRE matches one Signed-off-by trailer line anywhere in a body
// (the post-merge DCO check scans every line, not only the last paragraph).
var signedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by:\s*(\S.*?)\s*$`)

// SquashSignoff returns the commit message to send with merging number by
// method so a squash keeps the branch's Signed-off-by trailers. message is
// what the merge would otherwise send ("" for GitHub's default). A merge
// commit or rebase lands the branch commits themselves and is returned
// unchanged. A message that already carries a sign-off was rebuilt from the
// branch commits (squashCreditMessage keeps every line) and costs no request.
//
// It applies whether or not issue-author credit is on for the repo. If the
// branch commits cannot be read the message is returned unchanged (the
// PR-time DCO gate already ran). A branch read with no sign-off at all
// returns ErrSquashUnsigned, and the caller must not merge, only when the
// repo requires DCO (requiresDCO, for base and head); elsewhere the message
// goes out as built, as it did before #18203.
func (m *MergeCreditor) SquashSignoff(ctx context.Context, owner, repo string, number int, base, head, method, message string) (string, error) {
	if m == nil || m.gh == nil || number <= 0 {
		return message, nil
	}
	method = strings.ToLower(strings.TrimSpace(method))
	if method != "" && method != "squash" {
		return message, nil
	}
	if signedOffByRE.MatchString(message) {
		return message, nil
	}
	commits, err := m.listCommits(WithRESTCaller(ctx, mergeCreditCaller), owner, repo, number)
	if err != nil {
		m.logger.Warn("squash sign-off: could not read PR commits, merging with the message as built",
			slog.String("repo", owner+"/"+repo), slog.Int("number", number), slog.String("error", err.Error()))
		return message, nil
	}
	out, err := squashSignoffMessage(commits, message)
	if !errors.Is(err, ErrSquashUnsigned) {
		return out, err
	}
	if m.requiresDCO(ctx, owner, repo, base, head) {
		return "", err
	}
	m.logger.Info("squash sign-off: no branch sign-off and the repo does not require DCO, merging as built",
		slog.String("repo", owner+"/"+repo), slog.Int("number", number))
	return message, nil
}

// dcoEvidenceMaxPages bounds the head's status and check-run reads.
const dcoEvidenceMaxPages = 3

// requiresDCO reports whether owner/repo requires a sign-off on a squash of
// head onto base: the dco.required override when one is set for the repo,
// else a `dco`/`DCO` context in base's required checks or reported on head
// (the DCO App, Prow's dco plugin). A head it cannot read counts as
// required, so a failed read keeps the refusal.
func (m *MergeCreditor) requiresDCO(ctx context.Context, owner, repo, base, head string) bool {
	if m.dcoRequired != nil {
		if required, ok := m.dcoRequired(owner + "/" + repo); ok {
			return required
		}
	}
	ctx = WithRESTCaller(ctx, mergeCreditCaller)
	if base != "" {
		if required, known := RequiredStatusCheckContexts(ctx, m.gh, owner, repo, base, nil, false); known {
			for name := range required {
				if IsDCOContext(name) {
					return true
				}
			}
		}
	}
	if head == "" {
		return true
	}
	reported, err := headContextNames(ctx, m.gh, owner, repo, head)
	if err != nil {
		m.logger.Warn("squash sign-off: could not read the head's checks, treating the repo as requiring DCO",
			slog.String("repo", owner+"/"+repo), slog.String("head", head), slog.String("error", err.Error()))
		return true
	}
	for _, name := range reported {
		if IsDCOContext(name) {
			return true
		}
	}
	return false
}

// headContextNames lists the commit-status contexts and check-run names
// reported on ref. More than dcoEvidenceMaxPages pages of either is an
// error: the list would be incomplete.
func headContextNames(ctx context.Context, client *gh.Client, owner, repo, ref string) ([]string, error) {
	var names []string
	statusOpts := &gh.ListOptions{PerPage: 100}
	for page := 0; ; page++ {
		status, resp, err := client.Repositories.GetCombinedStatus(ctx, owner, repo, ref, statusOpts)
		if err != nil {
			return nil, err
		}
		if status == nil {
			return nil, fmt.Errorf("no combined status for %s", ref)
		}
		for _, s := range status.Statuses {
			names = append(names, s.GetContext())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		if page+1 >= dcoEvidenceMaxPages {
			return nil, fmt.Errorf("more than %d pages of statuses on %s", dcoEvidenceMaxPages, ref)
		}
		statusOpts.Page = resp.NextPage
	}
	checkOpts := &gh.ListCheckRunsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; ; page++ {
		runs, resp, err := client.Checks.ListCheckRunsForRef(ctx, owner, repo, ref, checkOpts)
		if err != nil {
			return nil, err
		}
		if runs == nil {
			return nil, fmt.Errorf("no check runs answer for %s", ref)
		}
		for _, run := range runs.CheckRuns {
			names = append(names, run.GetName())
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		if page+1 >= dcoEvidenceMaxPages {
			return nil, fmt.Errorf("more than %d pages of check runs on %s", dcoEvidenceMaxPages, ref)
		}
		checkOpts.Page = resp.NextPage
	}
	return names, nil
}

// squashSignoffMessage is SquashSignoff's decision over commits already read:
// an empty message becomes the rebuilt default squash body, and any branch
// sign-off the message lacks is appended as a trailer.
func squashSignoffMessage(commits []*gh.RepositoryCommit, message string) (string, error) {
	signoffs := branchSignoffs(commits)
	if len(signoffs) == 0 {
		return "", ErrSquashUnsigned
	}
	if strings.TrimSpace(message) == "" {
		message = squashCreditMessage(commits, nil)
	}
	present := map[string]bool{}
	for _, m := range signedOffByRE.FindAllStringSubmatch(message, -1) {
		present[strings.ToLower(m[1])] = true
	}
	var missing []string
	for _, s := range signoffs {
		if !present[strings.ToLower(s)] {
			missing = append(missing, "Signed-off-by: "+s)
		}
	}
	if len(missing) == 0 {
		return message, nil
	}
	if message = strings.TrimSpace(message); message == "" {
		return strings.Join(missing, "\n"), nil
	}
	return message + "\n\n" + strings.Join(missing, "\n"), nil
}

// branchSignoffs returns the distinct Signed-off-by identities on the
// branch's own commits, in order; merge-from-base commits are left out, as
// the squash body leaves them out.
func branchSignoffs(commits []*gh.RepositoryCommit) []string {
	seen := map[string]bool{}
	var out []string
	for _, rc := range commits {
		if rc == nil || len(rc.Parents) > 1 {
			continue
		}
		for _, m := range signedOffByRE.FindAllStringSubmatch(rc.GetCommit().GetMessage(), -1) {
			key := strings.ToLower(m[1])
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, m[1])
		}
	}
	return out
}
