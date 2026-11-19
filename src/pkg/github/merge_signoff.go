package github

import (
	"context"
	"errors"
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
// when the branch carries none at all. It never signs off on anyone's behalf.

// ErrSquashUnsigned is returned when a squash merge would land a commit with
// no Signed-off-by: none of the branch commits carries one.
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
// PR-time DCO gate already ran); a branch read with no sign-off at all
// returns ErrSquashUnsigned and the caller must not merge.
func (m *MergeCreditor) SquashSignoff(ctx context.Context, owner, repo string, number int, method, message string) (string, error) {
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
	return squashSignoffMessage(commits, message)
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
