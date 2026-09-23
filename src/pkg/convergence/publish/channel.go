package publish

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/forge"
)

// Disclosure is what a private channel receives for a sensitive finding.
type Disclosure struct {
	Finding Finding
	// Body is the rendered evidence; a channel that cannot hold evidence
	// privately (the notify channel) must not send it.
	Body string
	// Marker is the finding marker the channel should carry so an uncertain
	// disclosure can be found again.
	Marker string
}

// DisclosureRef is the bounded provenance of a private disclosure: a
// reference string for the publication record and, when the channel produced
// one, a number for the publication proof. Never the finding body.
type DisclosureRef struct {
	Ref    string
	Number int
}

// PrivateChannel is where security-sensitive findings go instead of a public
// issue.
type PrivateChannel interface {
	// Disclose delivers the finding privately, exactly once per call.
	Disclose(ctx context.Context, d Disclosure) (DisclosureRef, error)
}

// ReadyChecker is the optional pre-flight seam a channel implements so the
// publisher can refuse an unroutable channel BEFORE any effect is journaled:
// a refusal raised inside the effect would be recorded as an uncertain
// outcome needing reconciliation, which is the wrong status for "nowhere to
// send this".
type ReadyChecker interface {
	Ready() error
}

// DisclosureFinder is the optional reconciliation seam a channel implements
// when it can find an earlier disclosure by marker.
type DisclosureFinder interface {
	FindDisclosure(ctx context.Context, marker string) (DisclosureRef, bool, error)
}

// RepoChannel files sensitive findings as issues in a configured PRIVATE
// repository through the same forge seam as public issues. The evidence is
// included because the repository is private by configuration.
type RepoChannel struct {
	Repo   string
	Issues forge.IssueSeam
}

// repoChannelLabel marks issues the repo channel files.
const repoChannelLabel = "security-disclosure"

// Ready reports whether the repo channel has an issue seam to file through.
func (c RepoChannel) Ready() error {
	if c.Issues == nil {
		return fmt.Errorf("%w: private repo channel has no issue seam", ErrNoPrivateChannel)
	}
	if c.Repo == "" {
		return fmt.Errorf("%w: private repo channel names no repository", ErrNoPrivateChannel)
	}
	return nil
}

// Disclose files the finding in the private repository.
func (c RepoChannel) Disclose(ctx context.Context, d Disclosure) (DisclosureRef, error) {
	if c.Issues == nil {
		return DisclosureRef{}, fmt.Errorf("%w: private repo channel has no issue seam", ErrNoPrivateChannel)
	}
	ref, err := c.Issues.CreateIssue(ctx, c.Repo, d.Finding.Title, d.Body, []string{repoChannelLabel})
	if err != nil {
		return DisclosureRef{}, err
	}
	return DisclosureRef{Ref: repoRef(c.Repo, ref.Number), Number: ref.Number}, nil
}

// FindDisclosure finds an earlier disclosure by marker in the private repo.
func (c RepoChannel) FindDisclosure(ctx context.Context, marker string) (DisclosureRef, bool, error) {
	if c.Issues == nil {
		return DisclosureRef{}, false, fmt.Errorf("%w: private repo channel has no issue seam", ErrNoPrivateChannel)
	}
	ref, ok, err := c.Issues.FindIssueByMarker(ctx, c.Repo, marker)
	if err != nil || !ok {
		return DisclosureRef{}, false, err
	}
	return DisclosureRef{Ref: repoRef(c.Repo, ref.Number), Number: ref.Number}, true, nil
}

func repoRef(repo string, number int) string {
	return "repo:" + repo + "#" + strconv.Itoa(number)
}

// Notifier is the operator notification seam the notify channel sends to.
type Notifier interface {
	Send(title, message string)
}

// NotifyChannel routes sensitive findings to the hive's operator notification
// channel. It sends only the title and the finding hash: notifications are
// not a private evidence store, so the body never leaves the hive.
type NotifyChannel struct {
	Notifier Notifier
}

// notifyRefPrefix opens the reference a notify disclosure records.
const notifyRefPrefix = "notify:"

// Ready reports whether the notify channel has a notifier to send through.
func (c NotifyChannel) Ready() error {
	if c.Notifier == nil {
		return fmt.Errorf("%w: notify channel has no notifier", ErrNoPrivateChannel)
	}
	return nil
}

// Disclose notifies the operator that a sensitive finding awaits them.
func (c NotifyChannel) Disclose(ctx context.Context, d Disclosure) (DisclosureRef, error) {
	if c.Notifier == nil {
		return DisclosureRef{}, fmt.Errorf("%w: notify channel has no notifier", ErrNoPrivateChannel)
	}
	c.Notifier.Send("Security-sensitive audit finding withheld from public issues",
		strings.TrimSpace(d.Finding.Title)+" (finding "+d.Finding.ContentHash+", campaign "+d.Finding.Campaign+")")
	return DisclosureRef{Ref: notifyRefPrefix + d.Finding.ContentHash}, nil
}
