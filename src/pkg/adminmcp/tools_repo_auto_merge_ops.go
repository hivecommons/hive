package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// repository.auto_merge switches one repository's auto-merge on or off
// (project.repo_policies[].auto_merge, resolved by
// config.RepoAutoMergeEnabled) through POST /api/repos/auto-merge, the
// endpoint the repository card's auto-merge switch saves with
// (toggleRepoAutoMerge in repos.js, handleRepoAutoMerge). It sends exactly
// what the card sends: {"repo": "owner/repo", "enabled": true | false}.
//
// The preview reads settings_read section auto_merge (GET
// /api/config/auto-merge, owner only), whose repo_auto_merge block reports
// the hive's autonomy level and each project repository's effective switch
// and stored override from the live config, and shows the switch before and
// after. A switch already as requested answers "nothing would change".
// ConfirmGuard reads again at write_confirm and refuses when the repository's
// switch moved after the preview (a dashboard toggle in between), and
// ConfirmResult reads it back so write_confirm reports what the hive now
// holds; the endpoint's own answer is only {"ok", "repo", "enabled",
// "changed", "persisted", "minStatusSeq"}.
//
// The endpoint refuses a repository that is not in project.repos and
// refuses switching auto-merge on below ACMM level 6. It saves the switch and
// records a repo_auto_merge audit entry naming the caller. Switching on needs
// a verified owner (requireOwnerRole); switching off also admits GitHub repo
// write, but the read this op makes first is owner only, so the op is owner
// only either way. The repository and level checks are made at the preview
// too, so a change the endpoint would refuse is never offered for confirm.
const (
	WriteOpRepoAutoMerge = "repository.auto_merge"

	repoAutoMergePath    = "/api/repos/auto-merge"
	repoAutoMergeSection = "auto_merge"
	repoAutoMergeKey     = "repo_auto_merge"
)

type repoAutoMergeOp struct{}

// RepoAutoMergeBodyFields lists the POST /api/repos/auto-merge body fields
// repository.auto_merge sends. pkg/dashboard checks it against the
// handler's body type.
func RepoAutoMergeBodyFields() []string { return []string{"repo", "enabled"} }

func (repoAutoMergeOp) Name() string { return WriteOpRepoAutoMerge }
func (repoAutoMergeOp) Description() string {
	return "Switch one repository's auto-merge on or off (project.repo_policies[].auto_merge) through POST " + repoAutoMergePath + ", the endpoint the repository card's auto-merge switch saves with. Owner only. Takes repo (owner/repo, or the bare name of a project repository) and enabled. The preview reads the current switch (settings_read section auto_merge, repo_auto_merge) and shows it before and after; a switch already as requested is refused with nothing would change. Switching on is disclosed as widening: the hive may merge PRs in that repository once its own gates and GitHub's rules pass. Switching off stops every Hive merge path there; agents still open, review and hold PRs. A repository not in project.repos is refused, and so is any change while the hive is below autonomy level 6, where auto-merge is off for every repository and moving to level 6 turns it on for every active one. write_confirm refuses if the switch changed after the preview, then reads it back and reports what the hive now holds. It merges nothing itself."
}

func (repoAutoMergeOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"repo":    map[string]any{"type": "string", "minLength": 1, "description": "owner/repo, or the bare name of a repository in project.repos."},
		"enabled": map[string]any{"type": "boolean", "description": "true switches the hive's auto-merge on for the repository, false switches it off."},
	}, "required": []string{"repo", "enabled"}, "additionalProperties": false}
}

func repoAutoMergeParseArgs(args map[string]any) (string, bool, error) {
	if err := rejectUnknownKeys(args, WriteOpRepoAutoMerge, RepoAutoMergeBodyFields()...); err != nil {
		return "", false, err
	}
	repo := stringArg(args, "repo")
	enabled, ok := boolArg(args, "enabled")
	if repo == "" || !ok {
		return "", false, fmt.Errorf("repo and enabled are required (enabled is true or false)")
	}
	return repo, enabled, nil
}

// repoAutoMergeRepoState is one repository's switch as the auto_merge
// section reports it. value is the stored override, nil when unset.
type repoAutoMergeRepoState struct {
	repo    string
	enabled bool
	value   *bool
}

type repoAutoMergeState struct {
	level    int
	minLevel int
	repos    []repoAutoMergeRepoState
}

func repoAutoMergeRead(ctx context.Context, provider Provider) (repoAutoMergeState, error) {
	if provider == nil {
		return repoAutoMergeState{}, fmt.Errorf("%s reads the current auto-merge switch first, and this connection cannot read it", WriteOpRepoAutoMerge)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": repoAutoMergeSection})
	if err != nil {
		return repoAutoMergeState{}, fmt.Errorf("%s needs the current merge settings (settings_read section %s), which only the hive's verified owner can read: %w", WriteOpRepoAutoMerge, repoAutoMergeSection, err)
	}
	m, _ := data.(map[string]any)
	block, ok := m[repoAutoMergeKey].(map[string]any)
	if !ok {
		return repoAutoMergeState{}, fmt.Errorf("this hive does not report %s in settings_read section %s (it runs an older build), so %s cannot show what would change; upgrade it, or use the auto-merge switch on the repository card", repoAutoMergeKey, repoAutoMergeSection, WriteOpRepoAutoMerge)
	}
	var st repoAutoMergeState
	if st.level, ok = numberArg(block["acmm_level"]); !ok {
		return repoAutoMergeState{}, fmt.Errorf("settings_read section %s does not report %s.acmm_level, so %s cannot show what would change", repoAutoMergeSection, repoAutoMergeKey, WriteOpRepoAutoMerge)
	}
	if st.minLevel, ok = numberArg(block["min_acmm_level"]); !ok || st.minLevel <= 0 {
		return repoAutoMergeState{}, fmt.Errorf("settings_read section %s does not report %s.min_acmm_level, so %s cannot show what would change", repoAutoMergeSection, repoAutoMergeKey, WriteOpRepoAutoMerge)
	}
	items, _ := block["repos"].([]any)
	for _, item := range items {
		entry, _ := item.(map[string]any)
		repo, _ := entry["repo"].(string)
		enabled, hasEnabled := entry["enabled"].(bool)
		if strings.TrimSpace(repo) == "" || !hasEnabled {
			return repoAutoMergeState{}, fmt.Errorf("settings_read section %s reports a %s.repos entry without repo and enabled, so %s cannot show what would change", repoAutoMergeSection, repoAutoMergeKey, WriteOpRepoAutoMerge)
		}
		rs := repoAutoMergeRepoState{repo: repo, enabled: enabled}
		if v, ok := entry["value"].(bool); ok {
			rs.value = &v
		}
		st.repos = append(st.repos, rs)
	}
	return st, nil
}

// find resolves the argument to the repository as the hive lists it: an
// owner/repo or a bare name of a project repository, case-folded.
func (st repoAutoMergeState) find(arg string) (repoAutoMergeRepoState, bool) {
	names := make([]string, 0, len(st.repos))
	for _, rs := range st.repos {
		names = append(names, rs.repo)
	}
	want := qualifyProjectRepo(arg, names)
	for _, rs := range st.repos {
		if strings.EqualFold(rs.repo, want) {
			return rs, true
		}
	}
	return repoAutoMergeRepoState{}, false
}

type repoAutoMergePlan struct {
	state       repoAutoMergeState
	repo        repoAutoMergeRepoState
	enabled     bool
	fingerprint string
}

func repoAutoMergePlanFor(ctx context.Context, args map[string]any, provider Provider) (repoAutoMergePlan, error) {
	arg, enabled, err := repoAutoMergeParseArgs(args)
	if err != nil {
		return repoAutoMergePlan{}, err
	}
	st, err := repoAutoMergeRead(ctx, provider)
	if err != nil {
		return repoAutoMergePlan{}, err
	}
	rs, ok := st.find(arg)
	if !ok {
		return repoAutoMergePlan{}, fmt.Errorf("%s: %q is not a repository in project.repos; the hive has an auto-merge switch only for the repositories it manages, so nothing would be updated", WriteOpRepoAutoMerge, arg)
	}
	if st.level < st.minLevel {
		level := strconv.Itoa(st.minLevel)
		if enabled {
			return repoAutoMergePlan{}, fmt.Errorf("auto-merge requires autonomy level %s, and this hive is at level %d: auto-merge is off for every repository, so the endpoint refuses to switch it on for %s. Raise the level first (%s); moving to level %s turns auto-merge on for every active repository", level, st.level, rs.repo, WriteOpFleetAutonomyLevel, level)
		}
		return repoAutoMergePlan{}, fmt.Errorf("nothing would change: auto-merge is already off for %s, as for every repository while this hive is below autonomy level %s (it is at level %d). Moving to level %s turns it on for every active repository, so switch %s off after that", rs.repo, level, st.level, level, rs.repo)
	}
	if rs.enabled == enabled {
		return repoAutoMergePlan{}, fmt.Errorf("nothing would change: auto-merge is already %s for %s", repoAutoMergeOnOff(enabled), rs.repo)
	}
	before := map[string]any{"repo": rs.repo, "enabled": rs.enabled, "value": rs.value, "acmm_level": st.level}
	return repoAutoMergePlan{state: st, repo: rs, enabled: enabled, fingerprint: settingsFingerprint(before)}, nil
}

func repoAutoMergeOnOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// repoAutoMergeBeforeLabel is the switch as the preview read it, naming an
// unset override as the level 6 default.
func repoAutoMergeBeforeLabel(rs repoAutoMergeRepoState) string {
	if rs.value == nil && rs.enabled {
		return "on (not set, the level 6 default)"
	}
	return repoAutoMergeOnOff(rs.enabled)
}

// Preview cannot read the current switch, so it only checks the arguments.
// write_preview uses PreviewWithProvider; write_confirm calls Preview to check
// the request still names this endpoint, and then ConfirmGuard reads again.
func (repoAutoMergeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	repo, enabled, err := repoAutoMergeParseArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpRepoAutoMerge,
		Summary:             "Switch auto-merge " + repoAutoMergeOnOff(enabled) + " for " + repo + " (current switch not read)",
		Target:              repo,
		Request:             WriteRequest{Method: http.MethodPost, Path: repoAutoMergePath},
		WideningDisclosure:  "Potential widening: the current switch was not read, so this preview cannot say what would change. Switching auto-merge on lets the hive merge PRs in the repository once its own gates and GitHub's rules pass.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}, nil
}

func (repoAutoMergeOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	plan, err := repoAutoMergePlanFor(ctx, args, provider)
	if err != nil {
		return WritePreview{}, err
	}
	repo := plan.repo.repo
	after := repoAutoMergeOnOff(plan.enabled)
	line := "auto-merge for " + repo + ": " + repoAutoMergeBeforeLabel(plan.repo) + " → " + after
	body := map[string]any{"repo": repo, "enabled": plan.enabled}
	effects := []string{
		line,
		"Hive sends POST " + repoAutoMergePath + " with {\"repo\": \"" + repo + "\", \"enabled\": " + strconv.FormatBool(plan.enabled) + "}, the same request as the repository card's auto-merge switch. It sets project.repo_policies[].auto_merge for " + repo + " only; every other repository keeps its switch.",
		"The endpoint saves the switch and records a repo_auto_merge audit entry with you as the actor. It applies without a restart.",
	}
	var disclosure string
	if plan.enabled {
		effects = append(effects, "Hive's merge paths (the hive-merge relay, the App's self-authored sweep and direct merges through the proxy) may merge PRs in "+repo+" again, each behind its own gates.")
		if plan.repo.value == nil || *plan.repo.value {
			// At level 6 the switch reads off with no false override only
			// when the repository's merge_strategy is not recognised
			// (config.RepoAutoMergeEnabled).
			effects = append(effects, repo+" has no auto_merge: false recorded, yet its switch reads off. The hive reads it off when the repository's merge_strategy is not recognised, and then refuses every merge there until merge_strategy is fixed, so this write alone may leave it off; write_confirm reports what the hive holds after it.")
		}
		disclosure = "Widening: the hive may merge PRs in " + repo + " once its own gates and GitHub's rules pass. Its gates are the merge lanes' own checks (CI, review, holds, human-merge paths); GitHub's rules are the branch protection or rulesets on " + repo + ", which this preview does not read, and whatever approvals they require still apply. This operation merges nothing itself."
	} else {
		effects = append(effects, "Hive stops merging PRs in "+repo+": the hive-merge relay refuses it, the App's self-authored sweep skips it and the proxy refuses direct merge attempts. Agents may still open, review and hold PRs there, existing hold labels stay in place, and a person merges them.")
		disclosure = "Narrowing: the hive stops merging PRs in " + repo + "; nothing else about " + repo + " changes, and this operation merges and closes nothing."
	}
	effects = append(effects, "write_confirm reads the switch again and refuses if it changed after this preview, then reads it back and reports what the hive now holds.")
	return WritePreview{
		Operation:           WriteOpRepoAutoMerge,
		Summary:             "Switch auto-merge " + after + " for " + repo,
		Target:              repo,
		Request:             WriteRequest{Method: http.MethodPost, Path: repoAutoMergePath, Body: body},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm switching auto-merge " + after + " for " + repo + ": " + line + ".",
		Details:             map[string]any{"repo": repo, "before": plan.repo.enabled, "after": plan.enabled, "acmm_level": plan.state.level},
		StateFingerprint:    plan.fingerprint,
	}, nil
}

// ConfirmGuard reads the switch again and returns the fingerprint of the
// repository's switch and the hive's level.
func (repoAutoMergeOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	plan, err := repoAutoMergePlanFor(ctx, args, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the auto-merge switch again before writing, and that check refused this change; preview it again: %w", err)
	}
	return plan.fingerprint, nil
}

// ConfirmResult reads the switch back after the write and returns it as the
// hive now holds it.
func (repoAutoMergeOp) ConfirmResult(ctx context.Context, args map[string]any, provider Provider) (any, error) {
	arg, _, err := repoAutoMergeParseArgs(args)
	if err != nil {
		return nil, err
	}
	st, err := repoAutoMergeRead(ctx, provider)
	if err != nil {
		return nil, err
	}
	rs, ok := st.find(arg)
	if !ok {
		return nil, fmt.Errorf("%s is no longer in project.repos", arg)
	}
	out := map[string]any{"repo": rs.repo, "enabled": rs.enabled}
	if rs.value != nil {
		out["value"] = *rs.value
	}
	return out, nil
}
