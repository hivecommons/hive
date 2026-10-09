package compliance

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logscrub"
	"gopkg.in/yaml.v3"
)

// Posture check IDs. Stable: they key the history and the audit log.
const (
	CheckNonAuthorReview   = "non_author_review"
	CheckOwnerAutoMerge    = "owner_not_auto_merge_author"
	CheckAuditRetention    = "audit_retention"
	CheckAgentConfinement  = "agent_confinement"
	CheckSentinel          = "sentinel_enabled"
	CheckNoConfigSecrets   = "no_secrets_in_config"
	CheckDashboardAuth     = "dashboard_auth"
	CheckHoldLabels        = "hold_labels_exist"
	settingAuditRetention  = "audit.retention_days"
	evidenceAuditRetention = "docs/audit-log.md#rotation-and-retention"
)

// Confinement tiers, most to least confined. T1: the agent runs in the
// credential-free sandbox. T2: the agent runs on the host but the proxy
// injects the GitHub credential, so the agent never holds the real token.
// T3: unconfined.
const (
	ConfinementT1 = "T1"
	ConfinementT2 = "T2"
	ConfinementT3 = "T3"
	// DefaultMinConfinementTier is the floor the confinement check applies
	// until agent_backends.min_confinement_tier lands (hivecommons/hive#11077).
	DefaultMinConfinementTier = ConfinementT2
)

var postureChecks = []PostureCheck{
	{
		ID:         CheckNonAuthorReview,
		Title:      "Every merged PR in the window had a review by someone other than its author",
		ControlIDs: []string{"CC8.1", "CC6.3"},
		Run:        checkNonAuthorReview,
	},
	{
		ID:         CheckOwnerAutoMerge,
		Title:      "No dashboard owner authored a PR that was auto-merged in the window",
		ControlIDs: []string{"CC6.3"},
		Run:        checkOwnerAutoMerge,
	},
	{
		ID:         CheckAuditRetention,
		Title:      "Audit log retention meets the selected profiles' floor",
		ControlIDs: []string{"CC7.2"},
		Run:        checkAuditRetention,
	},
	{
		ID:         CheckAgentConfinement,
		Title:      "Every enabled agent runs at or below the minimum confinement tier",
		ControlIDs: []string{"CC6.6", "CC6.1"},
		Run:        checkAgentConfinement,
	},
	{
		ID:         CheckSentinel,
		Title:      "Sentinel is enabled with no behaviours disabled and its label exists on every repo",
		ControlIDs: []string{"CC6.8", "CC7.1"},
		Run:        checkSentinel,
	},
	{
		ID:         CheckNoConfigSecrets,
		Title:      "No secret-looking values in hive.yaml or its overlays",
		ControlIDs: []string{"CC6.1"},
		Run:        checkNoConfigSecrets,
	},
	{
		ID:         CheckDashboardAuth,
		Title:      "Dashboard authentication is enabled",
		ControlIDs: []string{"CC6.1", "CC6.2"},
		Run:        checkDashboardAuth,
	},
	{
		ID:         CheckHoldLabels,
		Title:      "Hold and needs-human labels exist on every repo",
		ControlIDs: []string{"CC8.1", "CC7.3"},
		Run:        checkHoldLabels,
	},
}

func skip(detail string) Result { return Result{Status: PostureSkip, Detail: detail} }
func failed(detail string, refs []string) Result {
	return Result{Status: PostureFail, Detail: detail, EvidenceRefs: refs}
}
func passed(detail string, refs []string) Result {
	return Result{Status: PosturePass, Detail: detail, EvidenceRefs: refs}
}
func errored(detail string) Result { return Result{Status: PostureError, Detail: detail} }

// mergedPRsInWindow gathers merged PRs from every repo. ok=false carries the
// skip or error result to return.
func mergedPRsInWindow(ctx context.Context, d PostureDeps) ([]PostureMergedPR, Result, bool) {
	if d.GitHub == nil {
		return nil, skip("no GitHub client: merged-PR evidence unavailable"), false
	}
	repos := d.qualifiedRepos()
	if len(repos) == 0 {
		return nil, skip("no repositories configured"), false
	}
	since := d.Now().Add(-d.Window)
	var all []PostureMergedPR
	for _, repo := range repos {
		prs, err := d.GitHub.MergedPRsSince(ctx, repo, since)
		if err != nil {
			return nil, errored(fmt.Sprintf("listing merged PRs for %s: %v", repo, err)), false
		}
		for _, pr := range prs {
			if pr.Repo == "" {
				pr.Repo = repo
			}
			if !pr.MergedAt.IsZero() && pr.MergedAt.Before(since) {
				continue
			}
			all = append(all, pr)
		}
	}
	return all, Result{}, true
}

func prRef(pr PostureMergedPR) string {
	if pr.URL != "" {
		return pr.URL
	}
	return fmt.Sprintf("https://github.com/%s/pull/%d", pr.Repo, pr.Number)
}

func windowLabel(w time.Duration) string {
	return fmt.Sprintf("%dd", int(w.Hours()/24))
}

func checkNonAuthorReview(ctx context.Context, d PostureDeps) Result {
	prs, res, ok := mergedPRsInWindow(ctx, d)
	if !ok {
		return res
	}
	var bad []string
	for _, pr := range prs {
		if !hasNonAuthorReview(pr) {
			bad = append(bad, prRef(pr))
		}
	}
	if len(bad) > 0 {
		return failed(fmt.Sprintf("%d of %d PR(s) merged in the last %s had no review from anyone other than the author", len(bad), len(prs), windowLabel(d.Window)), bad)
	}
	return passed(fmt.Sprintf("all %d PR(s) merged in the last %s had a non-author review", len(prs), windowLabel(d.Window)), nil)
}

func hasNonAuthorReview(pr PostureMergedPR) bool {
	author := strings.ToLower(strings.TrimSpace(pr.Author))
	for _, r := range pr.Reviewers {
		if r = strings.ToLower(strings.TrimSpace(r)); r != "" && r != author {
			return true
		}
	}
	return false
}

// isAutoMerged reports whether automation, not a person, merged pr: a GitHub
// App (bot account) or the hive's own author identity.
func isAutoMerged(pr PostureMergedPR, hiveLogin string) bool {
	if pr.MergedByBot {
		return true
	}
	by := strings.ToLower(strings.TrimSpace(pr.MergedBy))
	if by == "" {
		return false
	}
	if strings.HasSuffix(by, "[bot]") {
		return true
	}
	hive := strings.ToLower(strings.TrimSpace(hiveLogin))
	return hive != "" && by == hive
}

func checkOwnerAutoMerge(ctx context.Context, d PostureDeps) Result {
	dash := d.Config.Dashboard
	owners := 0
	for i, e := range dash.AuthorizedUsers {
		if entryRole(e, i == 0) == config.RoleOwner {
			owners++
		}
	}
	if owners == 0 {
		return skip("no dashboard owners configured (dashboard.authorized_users)")
	}
	prs, res, ok := mergedPRsInWindow(ctx, d)
	if !ok {
		return res
	}
	var bad []string
	who := map[string]bool{}
	auto := 0
	for _, pr := range prs {
		if !isAutoMerged(pr, d.Config.Project.AIAuthor) {
			continue
		}
		auto++
		if role, ok := dash.AuthorizedRole(pr.Author); ok && role == config.RoleOwner {
			bad = append(bad, prRef(pr))
			who[strings.ToLower(pr.Author)] = true
		}
	}
	if len(bad) > 0 {
		return failed(fmt.Sprintf("owner(s) %s authored %d of %d auto-merged PR(s) in the last %s", strings.Join(sortedKeys(who), ", "), len(bad), auto, windowLabel(d.Window)), bad)
	}
	return passed(fmt.Sprintf("none of %d auto-merged PR(s) in the last %s was authored by an owner", auto, windowLabel(d.Window)), nil)
}

// auditRetentionFloor is the highest at_least recommendation any selected
// profile makes for audit.retention_days; ok=false when none maps it.
func auditRetentionFloor(cfg *config.Config) (floor int, from []string, ok bool) {
	for _, id := range cfg.Compliance.SelectedFrameworks() {
		p, found := ProfileByID(id)
		if !found {
			continue
		}
		for _, c := range p.Controls {
			for _, m := range c.Mappings {
				if m.SettingPath != settingAuditRetention || m.Evaluator != EvalAtLeast {
					continue
				}
				n, err := strconv.Atoi(strings.TrimSpace(m.Recommended))
				if err != nil {
					continue
				}
				from = append(from, p.ID+" "+c.ID)
				if !ok || n > floor {
					floor = n
				}
				ok = true
			}
		}
	}
	return floor, from, ok
}

func checkAuditRetention(_ context.Context, d PostureDeps) Result {
	floor, from, ok := auditRetentionFloor(d.Config)
	if !ok {
		return skip("no selected profile sets an audit retention floor")
	}
	refs := []string{evidenceAuditRetention}
	cur := BuiltinAuditRetentionDays
	if cur >= floor {
		return passed(fmt.Sprintf("audit retention %d days meets the %d-day floor (%s)", cur, floor, strings.Join(from, ", ")), refs)
	}
	return failed(fmt.Sprintf("audit retention %d days is below the %d-day floor (%s)", cur, floor, strings.Join(from, ", ")), refs)
}

// AgentConfinementTier reports the tier an agent runs at under cfg.
func AgentConfinementTier(cfg *config.Config, a config.AgentConfig, getenv func(string) string) string {
	if a.SandboxEnabled(cfg.AgentSandbox) {
		return ConfinementT1
	}
	if getenv != nil && config.ResolveProxyInjectGHAuth(getenv).Enabled {
		return ConfinementT2
	}
	return ConfinementT3
}

func checkAgentConfinement(_ context.Context, d PostureDeps) Result {
	agents := d.Config.EnabledAgents()
	if len(agents) == 0 {
		return skip("no enabled agents")
	}
	floor := DefaultMinConfinementTier
	over := map[string]bool{}
	for name, a := range agents {
		tier := AgentConfinementTier(d.Config, a, d.Getenv)
		if tier > floor {
			label := name + " (" + tier
			if b := strings.TrimSpace(a.Backend); b != "" {
				label += ", " + b
			}
			over[label+")"] = true
		}
	}
	if len(over) > 0 {
		return failed(fmt.Sprintf("%d of %d enabled agent(s) run above %s: %s", len(over), len(agents), floor, strings.Join(sortedKeys(over), "; ")), nil)
	}
	return passed(fmt.Sprintf("all %d enabled agent(s) run at %s or better", len(agents), floor), nil)
}

// missingLabels checks every repo for every wanted label. ok=false carries
// an error result.
func missingLabels(ctx context.Context, d PostureDeps, want []string) (missing []string, repos int, res Result, ok bool) {
	for _, repo := range d.qualifiedRepos() {
		have, err := d.GitHub.RepoLabels(ctx, repo)
		if err != nil {
			return nil, 0, errored(fmt.Sprintf("listing labels for %s: %v", repo, err)), false
		}
		set := map[string]bool{}
		for _, l := range have {
			set[strings.ToLower(strings.TrimSpace(l))] = true
		}
		for _, w := range want {
			if !set[strings.ToLower(strings.TrimSpace(w))] {
				missing = append(missing, repo+": "+w)
			}
		}
		repos++
	}
	return missing, repos, Result{}, true
}

func checkSentinel(ctx context.Context, d PostureDeps) Result {
	s := d.Config.Sentinel
	if !s.IsEnabled() {
		return failed("sentinel is disabled (sentinel.enabled: false)", nil)
	}
	if disabled := nonNilList(s.DisabledBehaviors); len(disabled) > 0 {
		return failed("sentinel behaviours disabled: "+strings.Join(disabled, ", "), nil)
	}
	label := s.LabelOrDefault()
	if d.GitHub == nil || len(d.qualifiedRepos()) == 0 {
		return passed("sentinel enabled with every behaviour on; label presence not verified (no GitHub client or repositories)", nil)
	}
	missing, repos, res, ok := missingLabels(ctx, d, []string{label})
	if !ok {
		return res
	}
	if len(missing) > 0 {
		return failed(fmt.Sprintf("sentinel label %q is missing on %d of %d repo(s): %s", label, len(missing), repos, strings.Join(missing, "; ")), nil)
	}
	return passed(fmt.Sprintf("sentinel enabled with every behaviour on; label %q present on all %d repo(s)", label, repos), nil)
}

func checkHoldLabels(ctx context.Context, d PostureDeps) Result {
	if d.GitHub == nil {
		return skip("no GitHub client: repository labels unavailable")
	}
	if len(d.qualifiedRepos()) == 0 {
		return skip("no repositories configured")
	}
	want := append([]string{d.HoldLabel}, d.Config.Project.IssueFilter.HardSuppressLabels.EffectiveNeedsHuman()...)
	missing, repos, res, ok := missingLabels(ctx, d, want)
	if !ok {
		return res
	}
	if len(missing) > 0 {
		return failed(fmt.Sprintf("%d label(s) missing across %d repo(s): %s", len(missing), repos, strings.Join(missing, "; ")), nil)
	}
	return passed(fmt.Sprintf("labels %s present on all %d repo(s)", strings.Join(want, ", "), repos), nil)
}

func checkDashboardAuth(_ context.Context, d PostureDeps) Result {
	dash := d.Config.Dashboard
	switch {
	case dash.HubProxied:
		return passed("dashboard is behind the hub's authenticating proxy (dashboard.hub_proxied)", nil)
	case dash.IsDirectRouteAuthzEnabled():
		return passed("dashboard enforces per-user device-flow login (dashboard.authorized_users)", nil)
	case strings.TrimSpace(dash.AuthToken) != "":
		return passed("dashboard requires the shared auth token (dashboard.auth_token / DASHBOARD_AUTH_TOKEN)", nil)
	}
	return failed("dashboard authentication is off: no auth token, no authorized_users allowlist and not hub-proxied", nil)
}

// secretKeyPattern matches YAML keys whose value is a credential.
var secretKeyPattern = regexp.MustCompile(`(?i)(^|_)(token|secret|password|passwd|api_?key|private_?key|client_secret)$`)

// literalSecretValue reports whether v under a credential-named key is a
// literal rather than a reference: env expansions ($VAR, ${VAR}), paths and
// Kubernetes "secret/key" references, and short placeholders are fine.
func literalSecretValue(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 8 || strings.HasPrefix(v, "$") || strings.Contains(v, "/") {
		return false
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return false
	}
	lower := strings.ToLower(v)
	for _, p := range []string{"redacted", "changeme", "<", "xxx", "***"} {
		if strings.Contains(lower, p) {
			return false
		}
	}
	return true
}

var redactedKindPattern = regexp.MustCompile(`<redacted:([a-z0-9-]+)>`)

// redactedKinds returns the logscrub categories ScrubString masked in s.
func redactedKinds(s string) []string {
	scrubbed := logscrub.ScrubString(s, logscrub.WithMarkers())
	if scrubbed == s {
		return nil
	}
	before := map[string]bool{}
	for _, m := range redactedKindPattern.FindAllStringSubmatch(s, -1) {
		before[m[1]] = true
	}
	kinds := map[string]bool{}
	for _, m := range redactedKindPattern.FindAllStringSubmatch(scrubbed, -1) {
		if !before[m[1]] {
			kinds[m[1]] = true
		}
	}
	return sortedKeys(kinds)
}

// scanConfigSecrets returns one finding per secret-looking value in raw,
// naming the file, line and kind but never the value. It reuses the
// pkg/logscrub credential shapes (GitHub tokens, JWTs, AWS keys, bearer
// tokens, private keys) and adds a YAML pass that flags a literal value under
// a credential-named key (token, secret, password, api_key, ...).
func scanConfigSecrets(file string, raw []byte) []string {
	var out []string
	lines := map[int]bool{}
	kindsSeen := map[string]bool{}
	for i, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		for _, k := range redactedKinds(line) {
			kindsSeen[k] = true
			if !lines[i+1] {
				lines[i+1] = true
				out = append(out, fmt.Sprintf("%s:%d (%s)", file, i+1, k))
			}
		}
	}
	// Multi-line shapes (PEM private-key blocks) only match the whole file.
	for _, k := range redactedKinds(string(raw)) {
		if !kindsSeen[k] {
			kindsSeen[k] = true
			out = append(out, fmt.Sprintf("%s (%s)", file, k))
		}
	}
	var root yaml.Node
	if err := yaml.Unmarshal(raw, &root); err == nil {
		walkYAMLSecrets(&root, "", func(path string, line int) {
			if !lines[line] {
				lines[line] = true
				out = append(out, fmt.Sprintf("%s:%d (literal value for %s)", file, line, path))
			}
		})
	}
	return out
}

func walkYAMLSecrets(n *yaml.Node, path string, hit func(path string, line int)) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for i, c := range n.Content {
			p := path
			if n.Kind == yaml.SequenceNode {
				p = fmt.Sprintf("%s[%d]", path, i)
			}
			walkYAMLSecrets(c, p, hit)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, v := n.Content[i], n.Content[i+1]
			p := k.Value
			if path != "" {
				p = path + "." + k.Value
			}
			if v.Kind == yaml.ScalarNode && secretKeyPattern.MatchString(k.Value) && literalSecretValue(v.Value) {
				hit(p, v.Line)
				continue
			}
			walkYAMLSecrets(v, p, hit)
		}
	}
}

func checkNoConfigSecrets(_ context.Context, d PostureDeps) Result {
	scanned := 0
	var findings []string
	seen := map[string]bool{}
	for _, f := range d.ConfigFiles {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		raw, err := d.ReadFile(f)
		if err != nil {
			continue
		}
		scanned++
		findings = append(findings, scanConfigSecrets(f, raw)...)
	}
	if scanned == 0 {
		return skip("no config files readable to scan")
	}
	if len(findings) > 0 {
		return failed(fmt.Sprintf("%d secret-looking value(s) in %d config file(s); move them to env or a mounted Secret", len(findings), scanned), findings)
	}
	return passed(fmt.Sprintf("no secret-looking values in %d config file(s)", scanned), nil)
}
