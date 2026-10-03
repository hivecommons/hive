package dashboard

import (
	"encoding/json"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/dashboard/collect"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// prThroughputCountersPath is where the all-time PR throughput counters
// persist. The audit log cannot answer "all time": it rotates at
// auditMaxSizeMB, keeps auditMaxBackups files and drops anything older than
// auditMaxAgeDays, and its in-memory ring is emptied by a restart. A var (not
// const) only so tests can redirect it, mirroring auditLogPath.
var prThroughputCountersPath = "/data/pr-throughput-counters.json"

// prThroughputMergePathUnknown buckets pr_merged entries that carry no path=
// field (written before the field existed).
const prThroughputMergePathUnknown = "unknown"

const (
	prThroughputMergePathSweep           = "sweep"
	prThroughputMergePathRelay           = "relay"
	prThroughputMergePathHuman           = "human"
	prThroughputMergePathOtherAutomation = "other_automation"
)

const (
	prThroughputKindPR    = "pr"
	prThroughputKindIssue = "issue"

	prThroughputRoleCreated  = "created"
	prThroughputRoleReviewed = "reviewed"
	prThroughputRoleMerged   = "merged"
	prThroughputRoleClosed   = "closed"

	prThroughputActorHive            = "hive"
	prThroughputActorHuman           = "human"
	prThroughputActorOtherAutomation = "other"
	prThroughputActorUnknown         = "unknown"

	prThroughputCounterVersion = 2
)

// prThroughputActions are the audit actions the PR throughput counts are
// built from: changes the hive created/reviewed plus terminal states observed
// for PRs and issues.
//
// Actor buckets are intentionally narrow:
//   - hive: this hive's audited merge paths (sweep, queue, relay) and terminal
//     PR observations whose actor is this hive identity or configured App bot.
//   - human: non-bot GitHub logins, including maintainers using gh pr merge.
//   - other: non-hive bot logins such as dependabot, renovate, GitHub Actions,
//     and Copilot coding agents.
//   - unknown: terminal observations without an actor/path. Unknown is counted
//     in by_actor for transparency but excluded from share trend math.
var prThroughputActions = map[string]bool{
	ghpkg.AuditActionAgentPRCreated:      true,
	ghpkg.AuditActionAgentIssueCreated:   true,
	ghpkg.AuditActionHiveIssueCreated:    true,
	ghpkg.AuditActionPRReviewed:          true,
	ghpkg.AuditActionAgentCommentCreated: true,
	ghpkg.AuditActionIssueClaimed:        true,
	ghpkg.AuditActionPRMerged:            true,
	ghpkg.AuditActionPRClosed:            true,
	ghpkg.AuditActionIssueClosed:         true,
}

// PRThroughputCounters are the durable all-time totals behind the "all time"
// row of the PR throughput card. They are bumped once per audit entry at
// write time (AuditLog.Log), never recomputed from the audit files, so audit
// log rotation cannot double-count or lose them.
type PRThroughputCounters struct {
	Version      int                                `json:"version,omitempty"`
	Opened       int                                `json:"opened"`
	Merged       int                                `json:"merged"`
	MergedByPath map[string]int                     `json:"merged_by_path,omitempty"`
	Closed       int                                `json:"closed"`
	ByActor      PRThroughputActorMatrix            `json:"by_actor,omitempty"`
	ActorBuckets map[string]PRThroughputActorMatrix `json:"actor_buckets,omitempty"`
	// Terminal remembers merged/closed PRs already counted, keyed as
	// lower(repo)#number, so a forge-observed terminal state cannot double-count
	// a merge/close the hive already audited through a relay or sweep path.
	Terminal map[string]string `json:"terminal,omitempty"`
	// Since is the RFC3339 timestamp of the oldest event the counters cover —
	// how far back "all time" actually goes.
	Since string `json:"since,omitempty"`
	// ByRepo mirrors the all-time counters per repository for repo-filtered
	// all-time dashboard views. It is additive; older files simply load without it.
	ByRepo map[string]PRThroughputRepoCounters `json:"by_repo,omitempty"`
}

type PRThroughputRepoCounters struct {
	Opened       int                                `json:"opened"`
	Merged       int                                `json:"merged"`
	MergedByPath map[string]int                     `json:"merged_by_path,omitempty"`
	Closed       int                                `json:"closed"`
	ByActor      PRThroughputActorMatrix            `json:"by_actor,omitempty"`
	ActorBuckets map[string]PRThroughputActorMatrix `json:"actor_buckets,omitempty"`
	Terminal     map[string]string                  `json:"terminal,omitempty"`
	Since        string                             `json:"since,omitempty"`
}

type PRThroughputActorMatrix map[string]map[string]map[string]int

type PRThroughputActorPoint struct {
	T     string `json:"t"`
	Hive  int    `json:"hive"`
	Human int    `json:"human"`
	Other int    `json:"other"`
}

// clone returns a deep copy safe to hand out of the AuditLog lock.
func (c PRThroughputCounters) clone() PRThroughputCounters {
	out := c
	out.MergedByPath = make(map[string]int, len(c.MergedByPath))
	for k, v := range c.MergedByPath {
		out.MergedByPath[k] = v
	}
	out.Terminal = cloneStringMap(c.Terminal)
	out.ByActor = cloneActorMatrix(c.ByActor)
	out.ActorBuckets = cloneActorBuckets(c.ActorBuckets)
	if len(c.ByRepo) > 0 {
		out.ByRepo = make(map[string]PRThroughputRepoCounters, len(c.ByRepo))
		for k, v := range c.ByRepo {
			v.MergedByPath = cloneIntMap(v.MergedByPath)
			v.ByActor = cloneActorMatrix(v.ByActor)
			v.ActorBuckets = cloneActorBuckets(v.ActorBuckets)
			v.Terminal = cloneStringMap(v.Terminal)
			out.ByRepo[k] = v
		}
	}
	return out
}

// add counts one audit entry; it reports whether the entry was a PR
// throughput action at all.
func (c *PRThroughputCounters) add(e AuditEntry) bool {
	if !prThroughputActions[e.Action] {
		return false
	}
	if !c.rememberTerminal(e) {
		return false
	}
	c.addTotals(e)
	if repo, ok := collect.AuditEntryRepo(e); ok {
		if c.ByRepo == nil {
			c.ByRepo = map[string]PRThroughputRepoCounters{}
		}
		key := strings.ToLower(repo)
		rc := c.ByRepo[key]
		rc.add(e)
		c.ByRepo[key] = rc
	}
	return true
}

func (c *PRThroughputCounters) addTotals(e AuditEntry) {
	c.Version = prThroughputCounterVersion
	switch e.Action {
	case ghpkg.AuditActionAgentPRCreated:
		c.Opened++
	case ghpkg.AuditActionPRMerged:
		c.Merged++
		if c.MergedByPath == nil {
			c.MergedByPath = map[string]int{}
		}
		c.MergedByPath[prThroughputMergePath(e.Detail)]++
	case ghpkg.AuditActionPRClosed:
		c.Closed++
	}
	if kind, role, actor, ok := prThroughputActorAttribution(e); ok {
		c.ByActor = c.ByActor.ensure()
		addActorMatrix(c.ByActor, kind, role, actor, 1)
		if c.ActorBuckets == nil {
			c.ActorBuckets = map[string]PRThroughputActorMatrix{}
		}
		bucket := prThroughputCounterBucket(e.Timestamp)
		if bucket != "" {
			m := c.ActorBuckets[bucket].ensure()
			addActorMatrix(m, kind, role, actor, 1)
			c.ActorBuckets[bucket] = m
		}
	}
	if c.Since == "" || (e.Timestamp != "" && e.Timestamp < c.Since) {
		c.Since = e.Timestamp
	}
}

func (c *PRThroughputRepoCounters) add(e AuditEntry) {
	if !c.rememberTerminal(e) {
		return
	}
	switch e.Action {
	case ghpkg.AuditActionAgentPRCreated:
		c.Opened++
	case ghpkg.AuditActionPRMerged:
		c.Merged++
		if c.MergedByPath == nil {
			c.MergedByPath = map[string]int{}
		}
		c.MergedByPath[prThroughputMergePath(e.Detail)]++
	case ghpkg.AuditActionPRClosed:
		c.Closed++
	}
	if kind, role, actor, ok := prThroughputActorAttribution(e); ok {
		c.ByActor = c.ByActor.ensure()
		addActorMatrix(c.ByActor, kind, role, actor, 1)
		if c.ActorBuckets == nil {
			c.ActorBuckets = map[string]PRThroughputActorMatrix{}
		}
		bucket := prThroughputCounterBucket(e.Timestamp)
		if bucket != "" {
			m := c.ActorBuckets[bucket].ensure()
			addActorMatrix(m, kind, role, actor, 1)
			c.ActorBuckets[bucket] = m
		}
	}
	if c.Since == "" || (e.Timestamp != "" && e.Timestamp < c.Since) {
		c.Since = e.Timestamp
	}
}

func (c PRThroughputCounters) forRepo(repo string) PRThroughputCounters {
	rc := c.ByRepo[strings.ToLower(repo)]
	return PRThroughputCounters{Version: c.Version, Opened: rc.Opened, Merged: rc.Merged, MergedByPath: cloneIntMap(rc.MergedByPath), Closed: rc.Closed, ByActor: cloneActorMatrix(rc.ByActor), ActorBuckets: cloneActorBuckets(rc.ActorBuckets), Terminal: cloneStringMap(rc.Terminal), Since: rc.Since}
}

func cloneIntMap(in map[string]int) map[string]int {
	out := make(map[string]int, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// prThroughputMergePath extracts the path= field of a pr_merged detail
// (sweep / queue / relay), or prThroughputMergePathUnknown when absent.
func prThroughputMergePath(detail string) string {
	if p := strings.TrimSpace(parseAuditDetailAttrs(detail)["path"]); p != "" {
		if p == ghpkg.PRAuditPathQueue {
			return prThroughputMergePathSweep
		}
		return p
	}
	return prThroughputMergePathUnknown
}

func prThroughputActorAttribution(e AuditEntry) (kind, role, actor string, ok bool) {
	attrs := parseAuditDetailAttrs(e.Detail)
	switch e.Action {
	case ghpkg.AuditActionAgentPRCreated:
		return prThroughputKindPR, prThroughputRoleCreated, prThroughputActorHive, true
	case ghpkg.AuditActionAgentIssueCreated, ghpkg.AuditActionHiveIssueCreated:
		return prThroughputKindIssue, prThroughputRoleCreated, prThroughputActorHive, true
	case ghpkg.AuditActionPRReviewed:
		return prThroughputKindPR, prThroughputRoleReviewed, prThroughputActorFromAttrs(attrs, prThroughputActorHive), true
	case ghpkg.AuditActionAgentCommentCreated, ghpkg.AuditActionIssueClaimed:
		return prThroughputKindIssue, prThroughputRoleReviewed, prThroughputActorHive, true
	case ghpkg.AuditActionPRMerged:
		return prThroughputKindPR, prThroughputRoleMerged, prThroughputActorFromAttrs(attrs, actorFromPRPath(prThroughputMergePath(e.Detail))), true
	case ghpkg.AuditActionPRClosed:
		return prThroughputKindPR, prThroughputRoleClosed, prThroughputActorFromAttrs(attrs, actorFromPRPath(prThroughputMergePath(e.Detail))), true
	case ghpkg.AuditActionIssueClosed:
		return prThroughputKindIssue, prThroughputRoleClosed, prThroughputActorFromAttrs(attrs, prThroughputActorHive), true
	default:
		return "", "", "", false
	}
}

func actorFromPRPath(path string) string {
	switch path {
	case prThroughputMergePathSweep, prThroughputMergePathRelay, ghpkg.PRAuditPathQueue:
		return prThroughputActorHive
	case prThroughputMergePathHuman:
		return prThroughputActorHuman
	case prThroughputMergePathOtherAutomation:
		return prThroughputActorOtherAutomation
	case prThroughputMergePathUnknown:
		return prThroughputActorUnknown
	default:
		return prThroughputActorUnknown
	}
}

func prThroughputActorFromAttrs(attrs map[string]string, fallback string) string {
	for _, key := range []string{"actor_class", "actor", "path"} {
		switch strings.ToLower(strings.TrimSpace(attrs[key])) {
		case prThroughputActorHive, "sweep", "relay", "queue":
			return prThroughputActorHive
		case prThroughputActorHuman:
			return prThroughputActorHuman
		case "other_automation", "other", "bot", "automation":
			return prThroughputActorOtherAutomation
		case prThroughputActorUnknown, "unrecorded":
			return prThroughputActorUnknown
		}
	}
	if fallback == "" {
		return prThroughputActorUnknown
	}
	return fallback
}

func (m PRThroughputActorMatrix) ensure() PRThroughputActorMatrix {
	if m == nil {
		return PRThroughputActorMatrix{}
	}
	return m
}

func addActorMatrix(m PRThroughputActorMatrix, kind, role, actor string, n int) {
	if kind == "" || role == "" || actor == "" || n == 0 {
		return
	}
	if m[kind] == nil {
		m[kind] = map[string]map[string]int{}
	}
	if m[kind][role] == nil {
		m[kind][role] = map[string]int{}
	}
	m[kind][role][actor] += n
}

func cloneActorMatrix(in PRThroughputActorMatrix) PRThroughputActorMatrix {
	out := PRThroughputActorMatrix{}
	for kind, roles := range in {
		out[kind] = map[string]map[string]int{}
		for role, actors := range roles {
			out[kind][role] = cloneIntMap(actors)
		}
	}
	return out
}

func cloneActorBuckets(in map[string]PRThroughputActorMatrix) map[string]PRThroughputActorMatrix {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]PRThroughputActorMatrix, len(in))
	for k, v := range in {
		out[k] = cloneActorMatrix(v)
	}
	return out
}

func prThroughputCounterBucket(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	return t.UTC().Truncate(time.Hour).Format(time.RFC3339)
}

func prThroughputActorSeriesFromBuckets(buckets map[string]PRThroughputActorMatrix, role string) []PRThroughputActorPoint {
	if len(buckets) == 0 {
		return nil
	}
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]PRThroughputActorPoint, 0, len(keys))
	for _, k := range keys {
		p := PRThroughputActorPoint{T: k}
		addActorPoint(&p, buckets[k], role)
		if p.Hive+p.Human+p.Other > 0 {
			out = append(out, p)
		}
	}
	return out
}

func addActorPoint(p *PRThroughputActorPoint, m PRThroughputActorMatrix, role string) {
	for _, kind := range []string{prThroughputKindPR, prThroughputKindIssue} {
		gotRole := role
		if role == prThroughputRoleMerged && kind == prThroughputKindIssue {
			gotRole = prThroughputRoleClosed
		}
		actors := m[kind][gotRole]
		p.Hive += actors[prThroughputActorHive]
		p.Human += actors[prThroughputActorHuman]
		p.Other += actors[prThroughputActorOtherAutomation]
	}
}

func prThroughputTerminalAction(action string) bool {
	return action == ghpkg.AuditActionPRMerged || action == ghpkg.AuditActionPRClosed
}

func (c *PRThroughputCounters) rememberTerminal(e AuditEntry) bool {
	if !prThroughputTerminalAction(e.Action) {
		return true
	}
	key := prThroughputPRKey(e)
	if key == "" {
		return true
	}
	if c.Terminal == nil {
		c.Terminal = map[string]string{}
	}
	if _, exists := c.Terminal[key]; exists {
		return false
	}
	c.Terminal[key] = e.Action
	return true
}

func (c *PRThroughputRepoCounters) rememberTerminal(e AuditEntry) bool {
	if !prThroughputTerminalAction(e.Action) {
		return true
	}
	key := prThroughputPRKey(e)
	if key == "" {
		return true
	}
	if c.Terminal == nil {
		c.Terminal = map[string]string{}
	}
	if _, exists := c.Terminal[key]; exists {
		return false
	}
	c.Terminal[key] = e.Action
	return true
}

// loadPRThroughputCounters restores the counters from countersPath and makes
// it their persistence target. When no counters file exists yet (first boot
// with this feature, or an unreadable file) they are seeded ONCE from the
// audit files at auditPath and written out; from then on only Log bumps them,
// so the history the seed read is never counted a second time.
func (a *AuditLog) loadPRThroughputCounters(countersPath, auditPath string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.prCountersPath = countersPath
	if data, err := os.ReadFile(countersPath); err == nil {
		var c PRThroughputCounters
		if json.Unmarshal(data, &c) == nil {
			a.prCounters = c
			if a.hydratePRThroughputTerminalKeysLocked(auditPath) {
				a.persistPRThroughputCountersLocked()
			}
			return
		}
	}
	var seeded PRThroughputCounters
	for _, e := range a.actionsSince(time.Time{}, func(action string) bool { return prThroughputActions[action] }, nil, auditPath) {
		seeded.add(e)
	}
	a.prCounters = seeded
	a.persistPRThroughputCountersLocked()
}

func (a *AuditLog) hydratePRThroughputTerminalKeysLocked(auditPath string) bool {
	changed := false
	for _, e := range a.actionsSince(time.Time{}, func(action string) bool { return prThroughputTerminalAction(action) }, nil, auditPath) {
		key := prThroughputPRKey(e)
		if key == "" {
			continue
		}
		if a.prCounters.Terminal == nil {
			a.prCounters.Terminal = map[string]string{}
		}
		if _, exists := a.prCounters.Terminal[key]; exists {
			continue
		}
		a.prCounters.Terminal[key] = e.Action
		if repo, ok := collect.AuditEntryRepo(e); ok {
			if a.prCounters.ByRepo == nil {
				a.prCounters.ByRepo = map[string]PRThroughputRepoCounters{}
			}
			repoKey := strings.ToLower(repo)
			rc := a.prCounters.ByRepo[repoKey]
			if rc.Terminal == nil {
				rc.Terminal = map[string]string{}
			}
			rc.Terminal[key] = e.Action
			a.prCounters.ByRepo[repoKey] = rc
		}
		changed = true
	}
	return changed
}

// notePRThroughput bumps and persists the counters for a PR throughput audit
// entry. Callers must hold a.mu.
func (a *AuditLog) notePRThroughput(e AuditEntry) bool {
	if !a.prCounters.add(e) {
		return false
	}
	a.persistPRThroughputCountersLocked()
	return true
}

// persistPRThroughputCountersLocked writes the counters atomically (temp file
// + rename) so a crash mid-write cannot leave a torn file that would reseed.
// Callers must hold a.mu.
func (a *AuditLog) persistPRThroughputCountersLocked() {
	if a.prCountersPath == "" {
		return
	}
	data, err := json.Marshal(a.prCounters)
	if err != nil {
		return
	}
	tmp := a.prCountersPath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		slog.Error("pr throughput counters write failed", "error", err)
		return
	}
	if err := os.Rename(tmp, a.prCountersPath); err != nil {
		slog.Error("pr throughput counters rename failed", "error", err)
	}
}

// PRThroughputCounters returns a copy of the all-time PR throughput totals.
func (a *AuditLog) PRThroughputCounters() PRThroughputCounters {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.prCounters.clone()
}
