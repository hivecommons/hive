package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// SplitParentsFile is the durable record of which hive-filed issues the
// issue-request relay itself linked as GitHub sub-issues of which parent
// (hivecommons/hive#9840).
//
// When a person files an issue and an agent splits it into smaller ones, the
// children are filed by the hive, so ranking and the #5117 self-authorization
// gate treat them like any idea the hive came up with on its own: they sink to
// the bottom of the kick list and every PR on them is held until a person adds
// `approved-direction` to each child by hand. The approval was already given on
// the parent; splitting it for size must not take it away.
//
// GitHub's sub-issue link is the relation that carries that approval down, but
// a link alone is not enough: anyone who can edit issues can attach unrelated
// work under a human-filed parent, and then the hive would be approving work it
// invented — exactly what #5117 exists to prevent. So only links the relay
// MADE count, and this ledger is how the hive remembers which ones those were.
// It lives on the durable data dir, like review-links.json, so a spoke restart
// does not forget which children were split from approved work.
const SplitParentsFile = "split-parents.json"

// SplitParentsPath is a var so tests can point it at a temp dir.
var SplitParentsPath = filepath.Join(ReviewLinksDir, SplitParentsFile)

var splitParentsMu sync.Mutex

// maxSplitParents bounds the ledger. Entries are pruned oldest-first; a split
// child is normally closed within days, and an entry for a closed child is
// never consulted again, so a few thousand is months of history.
const maxSplitParents = 4000

// SplitParent records that the relay linked one child issue under Parent.
type SplitParent struct {
	Parent int       `json:"parent"`
	Agent  string    `json:"agent,omitempty"`
	At     time.Time `json:"at"`
}

type splitParentLedger struct {
	GeneratedAt time.Time              `json:"generated_at"`
	Children    map[string]SplitParent `json:"children"`
}

// splitParentKey is the ledger key for a child: "owner/repo#number", the same
// shape as ReviewLinkKey. The repo is lower-cased because callers reach the
// ledger with the full name from different sources (the relay request, the
// configured repo list split by splitRepo, the PR gate's owner+name) and
// GitHub repo names are case-insensitive.
func splitParentKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

// LoadSplitParents reads the ledger. A missing file is an empty ledger, not an
// error: most hives never split an issue.
func LoadSplitParents(path string) (map[string]SplitParent, error) {
	if path == "" {
		path = SplitParentsPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]SplitParent{}, nil
		}
		return nil, err
	}
	var ledger splitParentLedger
	if err := json.Unmarshal(data, &ledger); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if ledger.Children == nil {
		return map[string]SplitParent{}, nil
	}
	return ledger.Children, nil
}

// RecordSplitParent stores that the relay linked child under parent in repo
// (full "owner/repo"). Called only after AddSubIssue succeeded, so the ledger
// never claims a link GitHub does not show.
func RecordSplitParent(path, repo string, child, parent int, agent string) error {
	if path == "" {
		path = SplitParentsPath
	}
	if child <= 0 || parent <= 0 || child == parent {
		return nil
	}
	splitParentsMu.Lock()
	defer splitParentsMu.Unlock()

	children, err := LoadSplitParents(path)
	if err != nil {
		// A corrupt ledger must not block the create that already happened on
		// GitHub; start a fresh one.
		children = map[string]SplitParent{}
	}
	children[splitParentKey(repo, child)] = SplitParent{Parent: parent, Agent: strings.TrimSpace(agent), At: time.Now().UTC()}
	pruneSplitParents(children)

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(splitParentLedger{GeneratedAt: time.Now().UTC(), Children: children}, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func pruneSplitParents(children map[string]SplitParent) {
	if len(children) <= maxSplitParents {
		return
	}
	keys := make([]string, 0, len(children))
	for k := range children {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := children[keys[i]].At, children[keys[j]].At
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.Before(b)
	})
	for _, k := range keys[:len(children)-maxSplitParents] {
		delete(children, k)
	}
}

// splitParentOf returns the relay-recorded parent number of repo#number, or 0.
func splitParentOf(children map[string]SplitParent, repo string, number int) int {
	if len(children) == 0 {
		return 0
	}
	if sp, ok := children[splitParentKey(repo, number)]; ok && sp.Parent > 0 && sp.Parent != number {
		return sp.Parent
	}
	return 0
}

// parentConfersAcknowledgement reports whether parent is a source of
// acknowledgement its relay-linked children may inherit. The parent must be
// open — a closed parent is finished work, and anything still filed under it
// needs its own look — and must carry its OWN signal: a human author, or the
// cheap acknowledgement (#5117 label, human assignee) a person gave it. The
// parent's own inherited acknowledgement is deliberately not consulted, so
// inheritance is one level deep: a grandchild needs a human-filed or
// human-acknowledged immediate parent. A held parent confers nothing: a hold
// is a maintainer saying "not now" about everything under it.
func (c *Client) parentConfersAcknowledgement(parent *gh.Issue) bool {
	if parent == nil || parent.IsPullRequest() {
		return false
	}
	if !strings.EqualFold(parent.GetState(), "open") {
		return false
	}
	if c.isHeld(extractLabels(parent.Labels)) {
		return false
	}
	return c.isHumanAuthor(parent.GetUser()) || c.issueHasCheapHumanAcknowledgement(parent)
}

// ackSourceForParent is the AckSource an inheriting child carries: it names
// the parent so the kick list and the dashboard can show WHY a hive-filed
// issue ranks as acknowledged, and a reviewer can check the claim.
func ackSourceForParent(parent int) string {
	return "parent #" + strconv.Itoa(parent)
}

// inheritedAcknowledgement resolves, during enumeration, whether the hive-filed
// child issue inherits acknowledgement from a relay-recorded parent that is in
// the same open-issue snapshot. The snapshot is the only source consulted — no
// extra API call per child — which also encodes "only while the parent is
// open": a closed parent is not in the open list.
func (c *Client) inheritedAcknowledgement(children map[string]SplitParent, repo string, child *gh.Issue, open map[int]*gh.Issue) (int, bool) {
	parentNum := splitParentOf(children, repo, child.GetNumber())
	if parentNum == 0 {
		return 0, false
	}
	parent, ok := open[parentNum]
	if !ok {
		return parentNum, false
	}
	return parentNum, c.parentConfersAcknowledgement(parent)
}

// inheritedAcknowledgementLive is the #5117 PR-gate counterpart of
// inheritedAcknowledgement: there is no snapshot, so when the ledger names a
// parent for the cited issue, the parent is fetched once. A fetch error is
// returned so the caller can hold rather than guess, matching how it treats an
// unreadable comment list.
func (c *Client) inheritedAcknowledgementLive(ctx context.Context, owner, repo string, child *gh.Issue) (bool, error) {
	children, err := LoadSplitParents("")
	if err != nil {
		// An unreadable ledger is not evidence either way; the child simply
		// has no recorded parent.
		return false, nil
	}
	parentNum := splitParentOf(children, owner+"/"+repo, child.GetNumber())
	if parentNum == 0 {
		return false, nil
	}
	parent, _, err := c.client.Issues.Get(ctx, owner, repo, parentNum)
	if err != nil {
		return false, fmt.Errorf("reading split parent #%d: %w", parentNum, err)
	}
	return c.parentConfersAcknowledgement(parent), nil
}
