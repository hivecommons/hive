package github

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Request-issue notice for derived reference repos that could not get a
// write credential (hivecommons/hive#18158).
//
// A derived entry (config.ReferenceRepo.DerivedFrom) was granted by a
// maintainer accepting a request issue in one of the hive's authorized repos.
// When the App is not installed on the requesting project's org the hive can
// still read the repos but cannot open issues or PRs there. That is the
// requesting project's call to make, not the operator's, so the hive says so
// once on the request issue instead of refusing to boot.

// DerivedReadOnlyMarkerPrefix opens the marker the notice carries. The
// complete marker names the read-only repo set, so one notice is posted per
// (issue, repo set): a reload with the same outcome finds it and posts
// nothing, a later grant that adds a repo posts a new one.
const DerivedReadOnlyMarkerPrefix = "<!-- hive:reference-repos-read-only "

// derivedReadOnlyMarkerSuffix closes the marker.
const derivedReadOnlyMarkerSuffix = " -->"

// derivedReadOnlyRepoSeparator joins the repo slugs inside the marker.
const derivedReadOnlyRepoSeparator = ","

// DerivedReadOnlyMarker is the full marker for a read-only repo set, with
// the slugs case-folded and sorted so the set compares as a set.
func DerivedReadOnlyMarker(repos []string) string {
	slugs := make([]string, 0, len(repos))
	seen := make(map[string]bool, len(repos))
	for _, r := range repos {
		slug := strings.ToLower(strings.TrimSpace(r))
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)
	return DerivedReadOnlyMarkerPrefix + "repos=" + strings.Join(slugs, derivedReadOnlyRepoSeparator) +
		" state=" + config.ReferenceCredentialReadOnly + derivedReadOnlyMarkerSuffix
}

// DerivedReadOnlyComment renders the notice body for the read-only entries
// derived from one request issue. It lists each repo with its reason and
// says what the hive can and cannot do; it names no accounts, so nobody is
// notified beyond the issue's subscribers.
func DerivedReadOnlyComment(readOnly []config.ReferenceRepo) string {
	if len(readOnly) == 0 {
		return ""
	}
	repos := make([]string, 0, len(readOnly))
	for _, rr := range readOnly {
		repos = append(repos, rr.Repo)
	}
	var b strings.Builder
	b.WriteString(DerivedReadOnlyMarker(repos))
	b.WriteString("\n")
	if len(readOnly) == 1 {
		b.WriteString("This request grants the hive access to one repository it can read but not write:\n")
	} else {
		b.WriteString("This request grants the hive access to repositories it can read but not write:\n")
	}
	for _, rr := range readOnly {
		reason := strings.TrimSpace(rr.CredentialReason)
		if reason == "" {
			reason = DerivedReadOnlyReason(rr)
		}
		fmt.Fprintf(&b, "- `%s`: %s\n", rr.Repo, reason)
	}
	b.WriteString("\nThe hive will study these repositories (clone, read files, read issues and PRs) and keep its findings in the assessment deliverable here. ")
	b.WriteString("It will not open issues or pull requests on them. ")
	b.WriteString("To let it, an owner of that organization installs the same GitHub App there; the hive picks the installation up on its next config reload without a restart.")
	return AppendTrailer(b.String(), hiveWriteMeta())
}

// CommentDerivedReadOnly posts the read-only notice for one request issue
// (issueRepo#number, an authorized repo of this hive) once per repo set:
// when a comment of the hive's carrying the same marker exists nothing is
// posted and posted is false. readOnly are the derived entries from that
// issue whose CredentialState is read-only; an empty list posts nothing.
func (c *Client) CommentDerivedReadOnly(ctx context.Context, issueRepo string, number int, readOnly []config.ReferenceRepo) (posted bool, err error) {
	if c == nil {
		return false, ErrNoGitHubClient
	}
	if len(readOnly) == 0 {
		return false, nil
	}
	repos := make([]string, 0, len(readOnly))
	for _, rr := range readOnly {
		repos = append(repos, rr.Repo)
	}
	marker := DerivedReadOnlyMarker(repos)
	res, err := c.EditIssueMarkerComment(ctx, issueRepo, number, marker, func(previous string) string {
		if previous != "" {
			// Same repo set already noticed: leave the existing comment alone.
			return previous
		}
		return DerivedReadOnlyComment(readOnly)
	})
	if err != nil {
		return false, fmt.Errorf("derived reference repos read-only notice on %s#%d: %w", issueRepo, number, err)
	}
	if res.Created {
		c.recordWriteAudit(AuditActionReferenceRepoReadOnlyNoticed, hiveWriteMeta(),
			WriteTarget{Repo: c.qualifiedRepo(issueRepo), Number: number}, "repos", strings.Join(repos, derivedReadOnlyRepoSeparator))
	}
	return res.Created, nil
}
