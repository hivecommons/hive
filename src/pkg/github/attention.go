package github

import (
	"context"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/attention"
	"github.com/hivecommons/hive/pkg/writeback"
)

// SetAttention wires live opt-in ownership. Existing note/review/protocol paths
// deliberately do not infer condition identities from prose or change behavior.
func (c *Client) SetAttention(hiveID string, cfg func() attention.Config) {
	if c == nil {
		return
	}
	c.attentionHiveID = hiveID
	c.attentionConfig = cfg
}

// ReportAttention accepts a complete current condition inventory for an item.
// Producers must share structured subjects and action classes. Call with an empty
// inventory after recovery to establish a baseline before observing new risks.
// This path never bypasses writeback policy, including pause and repo mode off.
// Routing existing producers into this API is the separately opted-in pilot.
func (c *Client) ReportAttention(ctx context.Context, repo string, item writeback.Item, kind writeback.Kind, observations []attention.Observation) ([]attention.Episode, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	if c.attentionConfig == nil {
		return nil, nil
	}
	cfg := c.attentionConfig()
	if cfg.Owner(repo) == "" {
		return nil, nil
	}
	if d := c.WritebackAllowed(repo, item, kind, ""); !d.Allowed {
		return nil, fmt.Errorf("%w: %s", writeback.ErrSuppressed, d.Reason)
	}
	store, err := c.WritebackGate().AttentionStore()
	if err != nil {
		return nil, err
	}
	episodes, reportErr := store.Report(ctx, cfg, c.attentionHiveID, repo, item.Number, observations, attentionDelivery{c: c, kind: kind, item: item})
	// Even failed POSTs must remain visible in the GitHub-native status card.
	// Reload the snapshot on failure so uncertain attempts are not lost in UI.
	if reportErr != nil {
		episodes = store.Snapshot(repo, item.Number)
	}
	_, statusErr := c.UpsertPRStatusSection(ctx, repo, item.Number, "attention", attentionStatus(episodes))
	if reportErr != nil {
		return episodes, reportErr
	}
	return episodes, statusErr
}

type attentionDelivery struct {
	c    *Client
	kind writeback.Kind
	item writeback.Item
}

var attentionKeyMarker = regexp.MustCompile(`<!-- hive:(?:attention|condition) key=([a-f0-9]{64})(?: attempt=[a-f0-9]{32})? -->`)

func (d attentionDelivery) History(ctx context.Context, repo string, item int) ([]string, error) {
	owner, name := d.c.splitRepo(repo)
	comments, err := d.c.listIssueComments(ctx, owner, name, item)
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, cm := range comments {
		if cm == nil || !(d.c.isHiveAccount(cm.GetUser()) || d.c.IsHiveAuthorLogin(cm.GetUser().GetLogin())) {
			continue
		}
		for _, m := range attentionKeyMarker.FindAllStringSubmatch(cm.GetBody(), -1) {
			keys = append(keys, m[1])
		}
	}
	return keys, nil
}
func (d attentionDelivery) Find(ctx context.Context, e attention.Episode) (bool, error) {
	owner, name := d.c.splitRepo(e.Key.Repo)
	comments, err := d.c.listIssueComments(ctx, owner, name, e.Key.Item)
	if err != nil {
		return false, err
	}
	for _, cm := range comments {
		if cm != nil && (d.c.isHiveAccount(cm.GetUser()) || d.c.IsHiveAuthorLogin(cm.GetUser().GetLogin())) && strings.Contains(cm.GetBody(), e.Marker()) {
			return true, nil
		}
	}
	return false, nil
}
func (d attentionDelivery) Post(ctx context.Context, e attention.Episode, body string) error {
	// Recheck the live policy immediately before the notifying write.
	if decision := d.c.WritebackAllowed(e.Key.Repo, d.item, d.kind, ""); !decision.Allowed {
		return fmt.Errorf("%w: %s", writeback.ErrSuppressed, decision.Reason)
	}
	if d.c.attentionConfig == nil || d.c.attentionConfig().Owner(e.Key.Repo) != d.c.attentionHiveID {
		return writeback.ErrSuppressed
	}
	return d.c.CreateIssueComment(ctx, e.Key.Repo, e.Key.Item, body)
}
func attentionStatus(episodes []attention.Episode) string {
	sort.Slice(episodes, func(i, j int) bool { return episodes[i].Key.Hash() < episodes[j].Key.Hash() })
	var b strings.Builder
	active := 0
	for _, e := range episodes {
		if e.State == attention.Resolved {
			continue
		}
		active++
		if active > 20 {
			continue
		}
		// Display the current owner/action without copied mentions or Markdown.
		fmt.Fprintf(&b, "<!-- hive:condition key=%s -->\n- `%s`: %s; %s → %s (since %s)", e.Key.Hash(), e.Key.Hash()[:12], e.State, statusText(e.Key.Action), statusText(e.Key.Recipient), e.FirstSeen.UTC().Format("2006-01-02T15:04Z"))
		if !e.SnoozedUntil.IsZero() {
			fmt.Fprintf(&b, "; snoozed until %s", e.SnoozedUntil.UTC().Format("2006-01-02T15:04Z"))
		}
		if e.Evidence != "" {
			fmt.Fprintf(&b, "; evidence: %s", statusText(e.Evidence))
		}
		if e.Baseline || e.Uncertain {
			b.WriteString(" — uncertain; reconciliation required")
		}
		if e.WouldNotify {
			b.WriteString(" — would notify")
		}
		b.WriteByte('\n')
	}
	if active > 20 {
		fmt.Fprintf(&b, "\n%d further active episodes retained in the durable ledger.\n", active-20)
	}
	return b.String()
}

func statusText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 256 {
		s = s[:256] + "…"
	}
	return strings.NewReplacer("@", "＠", "`", "&#96;", "[", "&#91;", "]", "&#93;", "!", "&#33;", "*", "&#42;", "_", "&#95;").Replace(html.EscapeString(s))
}
