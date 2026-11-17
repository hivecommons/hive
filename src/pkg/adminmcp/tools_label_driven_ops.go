package adminmcp

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// governor.label_driven: label-driven approval (hivecommons/hive#11313), the
// settings Settings → Repos saves through PUT /api/config/governor/repos —
// the project-wide trusted bots, default approval labels and org-member
// trust (labelDrivenApproval), and each repository's opt-in and own
// approval labels (repoLabelDriven). The endpoint leaves every field it is
// not sent alone and replaces a list it is sent, so the op merges list
// changes onto what the preview read and sends only what changes.
//
// projectbluefin's Prow adds labels as its App (mergeraptor[bot]); its
// /triage accepted counted only once that bot was in
// project.label_driven_trusted_bots, which until now only the dashboard
// could set.

const WriteOpGovernorLabelDriven = "governor.label_driven"

type labelDrivenOp struct{}

// labelDrivenRepoState is one repository's label-driven setting as the
// governor settings read reports it. ApprovalLabels is the repository's own
// list (empty: it inherits the project default).
type labelDrivenRepoState struct {
	Enabled        bool     `json:"enabled"`
	ApprovalLabels []string `json:"approval_labels"`
}

// labelDrivenState is label-driven approval as the preview read it. The
// exported fields are what the confirm fingerprints; the lists are the
// effective ones (built-in defaults resolved).
type labelDrivenState struct {
	ApprovalLabels  []string                        `json:"approval_labels"`
	TrustOrgMembers bool                            `json:"trust_org_members"`
	TrustedBots     []string                        `json:"trusted_bots"`
	Repos           map[string]labelDrivenRepoState `json:"repos"`
	projectRepos    []string
}

var defaultLabelDrivenApprovalLabels = []string{config.DefaultLabelDrivenApprovalLabel}

func readLabelDrivenState(ctx context.Context, provider Provider) (labelDrivenState, error) {
	const what = "label-driven approval settings"
	m, err := readGovernorSettings(ctx, provider, what)
	if err != nil {
		return labelDrivenState{}, err
	}
	var st labelDrivenState
	ld, _ := m["labelDrivenApproval"].(map[string]any)
	if ld == nil {
		return st, missingSetting(what, "labelDrivenApproval")
	}
	var ok bool
	if st.ApprovalLabels, ok = stringsField(ld, "approvalLabels"); !ok {
		return st, missingSetting(what, "labelDrivenApproval.approvalLabels")
	}
	if st.TrustOrgMembers, ok = boolField(ld, "trustOrgMembers"); !ok {
		return st, missingSetting(what, "labelDrivenApproval.trustOrgMembers")
	}
	if st.TrustedBots, ok = stringsField(ld, "trustedBots"); !ok {
		return st, missingSetting(what, "labelDrivenApproval.trustedBots")
	}
	st.ApprovalLabels = orDefault(st.ApprovalLabels, defaultLabelDrivenApprovalLabels)
	st.TrustedBots = orDefault(st.TrustedBots, config.DefaultLabelDrivenTrustedBots)
	repos, _ := m["repoLabelDriven"].(map[string]any)
	if repos == nil {
		return st, missingSetting(what, "repoLabelDriven")
	}
	st.Repos = map[string]labelDrivenRepoState{}
	for repo, raw := range repos {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			return st, missingSetting(what, "repoLabelDriven["+repo+"]")
		}
		var rs labelDrivenRepoState
		if rs.Enabled, ok = boolField(entry, "enabled"); !ok {
			return st, missingSetting(what, "repoLabelDriven["+repo+"].enabled")
		}
		if _, present := entry["approvalLabels"]; present {
			if rs.ApprovalLabels, ok = stringsField(entry, "approvalLabels"); !ok {
				return st, missingSetting(what, "repoLabelDriven["+repo+"].approvalLabels")
			}
		}
		if rs.ApprovalLabels == nil {
			rs.ApprovalLabels = []string{}
		}
		st.Repos[repo] = rs
	}
	st.projectRepos = stringListValueLoose(m["repos"])
	return st, nil
}

// labelDrivenRepoChange is one repository's requested change; nil in the
// request map clears the opt-in.
type labelDrivenRepoChange struct {
	enabled        *bool
	approvalLabels *listChange
}

type labelDrivenRequest struct {
	trustedBots     *listChange
	approvalLabels  *listChange
	trustOrgMembers *bool
	repos           map[string]*labelDrivenRepoChange
}

func parseLabelDrivenArgs(args map[string]any) (labelDrivenRequest, error) {
	const where = WriteOpGovernorLabelDriven
	var req labelDrivenRequest
	if err := rejectUnknownKeys(args, where, "trusted_bots", "approval_labels", "trust_org_members", "repos"); err != nil {
		return req, err
	}
	for _, f := range []struct {
		key string
		dst **listChange
	}{
		{"trusted_bots", &req.trustedBots},
		{"approval_labels", &req.approvalLabels},
	} {
		c, ok, err := listChangeArg(args, f.key, where)
		if err != nil {
			return req, err
		}
		if ok {
			*f.dst = &c
		}
	}
	if raw, present := args["trust_org_members"]; present && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return req, fmt.Errorf("%s.trust_org_members must be true or false", where)
		}
		req.trustOrgMembers = &b
	}
	repos, err := objectArg(args, "repos", where)
	if err != nil {
		return req, err
	}
	if repos != nil {
		name := where + ".repos"
		if len(repos) == 0 {
			return req, fmt.Errorf("%s names no repository", name)
		}
		req.repos = map[string]*labelDrivenRepoChange{}
		for _, repo := range SortKeys(repos) {
			if strings.TrimSpace(repo) == "" {
				return req, fmt.Errorf("%s has an empty repository name", name)
			}
			if repos[repo] == nil {
				req.repos[repo] = nil
				continue
			}
			field := name + "[" + repo + "]"
			entry, ok := repos[repo].(map[string]any)
			if !ok {
				return req, fmt.Errorf("%s must be an object or null", field)
			}
			if err := rejectUnknownKeys(entry, field, "enabled", "approval_labels"); err != nil {
				return req, err
			}
			var rc labelDrivenRepoChange
			if raw, present := entry["enabled"]; present && raw != nil {
				b, ok := raw.(bool)
				if !ok {
					return req, fmt.Errorf("%s.enabled must be true or false", field)
				}
				rc.enabled = &b
			}
			c, ok, err := listChangeArg(entry, "approval_labels", field)
			if err != nil {
				return req, err
			}
			if ok {
				rc.approvalLabels = &c
			}
			if rc.enabled == nil && rc.approvalLabels == nil {
				return req, fmt.Errorf("%s names no change: give enabled or approval_labels, or null to clear the opt-in", field)
			}
			req.repos[repo] = &rc
		}
	}
	if req.trustedBots == nil && req.approvalLabels == nil && req.trustOrgMembers == nil && req.repos == nil {
		return req, fmt.Errorf("give at least one of trusted_bots, approval_labels, trust_org_members or repos")
	}
	return req, nil
}

func (labelDrivenOp) Name() string { return WriteOpGovernorLabelDriven }
func (labelDrivenOp) Description() string {
	return "Change label-driven approval (project.label_driven_trusted_bots, project.label_driven_approval_labels, project.label_driven_trust_org_members, and each repository's project.repo_policies[].label_driven opt-in and label_driven_approval_labels) through PUT /api/config/governor/repos, the endpoint Settings → Repos saves through. trusted_bots and approval_labels take {add, remove, clear}; clear restores the built-in default (kubestellar-prow[bot], triage/accepted). repos takes {\"owner/repo\": {\"enabled\": bool, \"approval_labels\": {add, remove, clear}} | null}; null or enabled false clears the repository's opt-in and its own labels. The preview reads the current settings, shows each before → after, says plainly when a change lets more people or bots release parked work, and write_confirm is refused if the settings changed since the preview. Owner only: the endpoint refuses any other role. It adds or removes no label and releases nothing by itself."
}
func (labelDrivenOp) InputSchema() map[string]any {
	repo := map[string]any{"type": "object", "properties": map[string]any{
		"enabled":         map[string]any{"type": "boolean", "description": "Opt the repository in to label-driven approval (true) or out (false, which also drops its own approval labels)."},
		"approval_labels": listChangeSchema("This repository's own approval labels; an empty list inherits the project default."),
	}, "additionalProperties": false}
	return map[string]any{"type": "object", "properties": map[string]any{
		"trusted_bots":      listChangeSchema("Bot logins whose label additions count as a trusted triager's on label-driven repositories; an empty list restores the built-in default kubestellar-prow[bot]."),
		"approval_labels":   listChangeSchema("Default approval labels for label-driven repositories without their own list; an empty list restores the built-in default triage/accepted."),
		"trust_org_members": map[string]any{"type": "boolean", "description": "Whether a public member of the repository's owning organisation who adds an approval label is trusted (Prow's own rule; on by default)."},
		"repos": map[string]any{"type": "object", "description": "owner/repo (or a bare name in this hive's org) to its change, or null to clear the repository's opt-in. Repositories you do not name keep their settings.",
			"additionalProperties": map[string]any{"anyOf": []any{repo, map[string]any{"type": "null"}}}},
	}, "additionalProperties": false}
}

// Preview without a provider checks the arguments and names the endpoint.
func (labelDrivenOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseLabelDrivenArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpGovernorLabelDriven, "project.label_driven", "/api/config/governor/repos", "label-driven approval settings"), nil
}

// ConfirmGuard reads the settings again at write_confirm and returns their
// fingerprint, which must still match the preview's.
func (op labelDrivenOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// PreviewWithProvider reads the current settings, merges the requested
// changes onto them and discloses who can release parked work afterwards.
func (labelDrivenOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	req, err := parseLabelDrivenArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	before, err := readLabelDrivenState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	after := before
	after.Repos = maps.Clone(before.Repos)
	body := map[string]any{}
	approval := map[string]any{}
	var changes []settingChange

	for _, f := range []struct {
		field  string
		key    string
		change *listChange
		before []string
		def    []string
		dst    *[]string
	}{
		{"trusted_bots", "trustedBots", req.trustedBots, before.TrustedBots, config.DefaultLabelDrivenTrustedBots, &after.TrustedBots},
		{"approval_labels", "approvalLabels", req.approvalLabels, before.ApprovalLabels, defaultLabelDrivenApprovalLabels, &after.ApprovalLabels},
	} {
		if f.change == nil {
			continue
		}
		list, err := f.change.apply(f.field, f.before, foldCase)
		if err != nil {
			return WritePreview{}, err
		}
		effective := orDefault(list, f.def)
		*f.dst = effective
		if sameList(f.before, effective, foldCase) {
			continue
		}
		approval[f.key] = list
		sc := settingChange{Field: f.field, Before: f.before, After: effective}
		if len(list) == 0 {
			sc.Note = "restores the built-in default (" + listLabel(f.def) + ")"
		}
		changes = append(changes, sc)
	}
	if req.trustOrgMembers != nil && *req.trustOrgMembers != before.TrustOrgMembers {
		after.TrustOrgMembers = *req.trustOrgMembers
		approval["trustOrgMembers"] = *req.trustOrgMembers
		changes = append(changes, settingChange{Field: "trust_org_members", Before: before.TrustOrgMembers, After: after.TrustOrgMembers})
	}
	if len(approval) > 0 {
		body["labelDrivenApproval"] = approval
	}
	repoChanges, err := applyLabelDrivenRepos(&after, before, req.repos, body)
	if err != nil {
		return WritePreview{}, err
	}
	changes = append(changes, repoChanges...)
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("%s changes nothing: label-driven approval already reads that way", WriteOpGovernorLabelDriven)
	}
	fingerprint, err := stateFingerprint(before)
	if err != nil {
		return WritePreview{}, err
	}

	fields := make([]string, 0, len(changes))
	effects := make([]string, 0, len(changes)+3)
	for _, c := range changes {
		fields = append(fields, c.Field)
		effects = append(effects, c.effect())
	}
	effects = append(effects,
		"After this change: "+labelDrivenRule(after),
		"Hive saves it through Settings → Repos (PUT /api/config/governor/repos), records a config_governor_repos audit entry, and applies it on the next un-park sweep, with no restart. Fields and repositories not named here keep their values.",
		"This write itself adds or removes no label and releases, merges, approves and closes nothing.",
	)
	return WritePreview{
		Operation:           WriteOpGovernorLabelDriven,
		Summary:             "Change label-driven approval: " + strings.Join(fields, ", "),
		Target:              "project.label_driven",
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/repos", Body: body},
		Effects:             effects,
		WideningDisclosure:  labelDrivenDisclosure(before, after),
		ConfirmationMessage: "Confirm changing label-driven approval: " + strings.Join(fields, ", ") + ".",
		Details:             map[string]any{"changes": changeDetails(changes), "after_rule": labelDrivenRule(after)},
		StateFingerprint:    fingerprint,
	}, nil
}

// applyLabelDrivenRepos sets or clears the named repositories' opt-in and
// own labels on after, adds the per-repository patch to body
// (repoLabelDriven, the shape Settings → Repos sends), and returns the
// preview rows. The endpoint takes an entry as the whole setting: enabled
// with the repository's own list, which it replaces, or null to clear.
func applyLabelDrivenRepos(after *labelDrivenState, before labelDrivenState, req map[string]*labelDrivenRepoChange, body map[string]any) ([]settingChange, error) {
	const field = "repos"
	patch := map[string]any{}
	var changes []settingChange
	touched := map[string]bool{}
	for _, key := range slices.Sorted(maps.Keys(req)) {
		repo := qualifyProjectRepo(strings.TrimSpace(key), before.projectRepos)
		watched := false
		for _, full := range before.projectRepos {
			if strings.EqualFold(full, repo) {
				repo, watched = full, true
			}
		}
		if !watched {
			return nil, fmt.Errorf("%s: %q is not a repository in project.repos", field, key)
		}
		if touched[strings.ToLower(repo)] {
			return nil, fmt.Errorf("%s names %s twice", field, repo)
		}
		touched[strings.ToLower(repo)] = true
		cur := before.Repos[repo]
		next := labelDrivenRepoState{Enabled: cur.Enabled, ApprovalLabels: cur.ApprovalLabels}
		rc := req[key]
		row := field + "[" + repo + "]"
		if rc == nil || (rc.enabled != nil && !*rc.enabled) {
			if rc != nil && rc.approvalLabels != nil {
				return nil, fmt.Errorf("%s: approval_labels needs the repository opted in; enabled false clears its own labels", row)
			}
			if !cur.Enabled {
				return nil, fmt.Errorf("%s is not label-driven; there is no opt-in to clear", row)
			}
			changes = append(changes, settingChange{Field: row + ".enabled", Before: true, After: false, Note: "its own approval labels are dropped with it"})
			after.Repos[repo] = labelDrivenRepoState{ApprovalLabels: []string{}}
			patch[repo] = nil
			continue
		}
		if rc.enabled != nil {
			next.Enabled = *rc.enabled
		}
		if rc.approvalLabels != nil {
			if !next.Enabled {
				return nil, fmt.Errorf("%s: approval_labels needs the repository opted in; add \"enabled\": true", row)
			}
			list, err := rc.approvalLabels.apply(row+".approval_labels", cur.ApprovalLabels, foldCase)
			if err != nil {
				return nil, err
			}
			next.ApprovalLabels = list
		}
		if next.Enabled != cur.Enabled {
			changes = append(changes, settingChange{Field: row + ".enabled", Before: cur.Enabled, After: next.Enabled})
		}
		if !sameList(cur.ApprovalLabels, next.ApprovalLabels, foldCase) {
			sc := settingChange{Field: row + ".approval_labels", Before: repoLabelsLabel(cur.ApprovalLabels), After: repoLabelsLabel(next.ApprovalLabels)}
			if len(next.ApprovalLabels) == 0 {
				sc.Note = "inherits the project default"
			}
			changes = append(changes, sc)
		}
		if next.Enabled == cur.Enabled && sameList(cur.ApprovalLabels, next.ApprovalLabels, foldCase) {
			continue
		}
		after.Repos[repo] = next
		patch[repo] = map[string]any{"enabled": true, "approvalLabels": next.ApprovalLabels}
	}
	if len(patch) > 0 {
		body["repoLabelDriven"] = patch
	}
	return changes, nil
}

// repoLabelsLabel names a repository's own approval labels for a preview
// row: the list, or that it inherits the project default.
func repoLabelsLabel(own []string) string {
	if len(own) == 0 {
		return "project default"
	}
	return "[" + listLabel(own) + "]"
}

// labelDrivenEffectiveLabels is the approval labels in force on repo.
func labelDrivenEffectiveLabels(st labelDrivenState, repo string) []string {
	if own := st.Repos[repo].ApprovalLabels; len(own) > 0 {
		return own
	}
	return st.ApprovalLabels
}

// labelDrivenRepos is the label-driven repositories, sorted.
func labelDrivenRepos(st labelDrivenState) []string {
	var out []string
	for repo, rs := range st.Repos {
		if rs.Enabled {
			out = append(out, repo)
		}
	}
	slices.Sort(out)
	return out
}

// labelDrivenRule states in plain words who releases parked work by label.
func labelDrivenRule(st labelDrivenState) string {
	repos := labelDrivenRepos(st)
	if len(repos) == 0 {
		return "no repository is label-driven, so no label releases parked work; these settings apply once a repository opts in."
	}
	who := "collaborators with the triage, write, maintain or admin role"
	if st.TrustOrgMembers {
		who += ", public members of the repository's organisation"
	}
	who += " and the bots " + listLabel(st.TrustedBots)
	parts := make([]string, 0, len(repos))
	for _, repo := range repos {
		parts = append(parts, repo+" ("+listLabel(labelDrivenEffectiveLabels(st, repo))+")")
	}
	return "on " + strings.Join(parts, "; ") + ", an approval label added by " + who + " releases the issue from the hive's park labels and counts as human sign-off."
}

// githubActionsBotWarning is the extra caution for trusting the shared
// workflow identity (operator-reference: project.label_driven_trusted_bots).
const githubActionsBotWarning = "github-actions[bot] is the shared identity of every workflow in the repository, including workflows started by untrusted events (for example a pull_request_target or issue_comment trigger): any of them that adds an approval label would release parked work as a trusted triager"

// labelDrivenDisclosure names every way the change lets more people or bots
// release parked work by label, and says so plainly when it lets none.
func labelDrivenDisclosure(before, after labelDrivenState) string {
	var widen, narrow []string
	anyRepo := len(labelDrivenRepos(after)) > 0
	scope := ""
	if !anyRepo {
		scope = " (once a repository is label-driven)"
	}
	botsAdded, botsRemoved := listDiff(before.TrustedBots, after.TrustedBots, foldCase)
	if len(botsAdded) > 0 {
		line := "label additions by " + listLabel(botsAdded) + " now count as a trusted triager's: an approval label they add releases parked issues and counts as human sign-off" + scope
		if containsFold(botsAdded, "github-actions[bot]", foldCase) {
			line += ". " + githubActionsBotWarning
		}
		widen = append(widen, line)
	}
	if len(botsRemoved) > 0 {
		narrow = append(narrow, "label additions by "+listLabel(botsRemoved)+" no longer count as a trusted triager's")
	}
	labelsAdded, labelsRemoved := listDiff(before.ApprovalLabels, after.ApprovalLabels, foldCase)
	if len(labelsAdded) > 0 {
		widen = append(widen, "a trusted triager adding "+listLabel(labelsAdded)+" now approves on every label-driven repository without its own labels"+scope)
	}
	if len(labelsRemoved) > 0 {
		narrow = append(narrow, listLabel(labelsRemoved)+" no longer approves on label-driven repositories without their own labels")
	}
	switch {
	case !before.TrustOrgMembers && after.TrustOrgMembers:
		widen = append(widen, "public members of each repository's organisation may now release parked work by adding an approval label"+scope)
	case before.TrustOrgMembers && !after.TrustOrgMembers:
		narrow = append(narrow, "public members of the organisation who are not triage-or-above collaborators can no longer approve by label")
	}
	repos := map[string]bool{}
	for repo := range before.Repos {
		repos[repo] = true
	}
	for repo := range after.Repos {
		repos[repo] = true
	}
	for _, repo := range slices.Sorted(maps.Keys(repos)) {
		b, a := before.Repos[repo], after.Repos[repo]
		switch {
		case !b.Enabled && a.Enabled:
			widen = append(widen, repo+" opts in: an approval label ("+listLabel(labelDrivenEffectiveLabels(after, repo))+") added by a trusted triager or bot releases its parked issues and counts as human sign-off, and triage-role collaborators may issue /hive approve, decision and done there")
		case b.Enabled && !a.Enabled:
			narrow = append(narrow, repo+" opts out: labels no longer release its parked issues; /hive approve is needed again")
		case b.Enabled && a.Enabled:
			added, removed := listDiff(labelDrivenEffectiveLabels(before, repo), labelDrivenEffectiveLabels(after, repo), foldCase)
			if len(added) > 0 {
				widen = append(widen, "on "+repo+", "+listLabel(added)+" now approves too")
			}
			if len(removed) > 0 {
				narrow = append(narrow, "on "+repo+", "+listLabel(removed)+" no longer approves")
			}
		}
	}
	narrowNote := ""
	if len(narrow) > 0 {
		narrowNote = " Narrowing: " + strings.Join(narrow, "; ") + "."
	}
	if len(widen) > 0 {
		return "Widening: more people or bots can release parked work. " + capitalize(strings.Join(widen, "; ")) + "." + narrowNote
	}
	return "No widening: nobody can release parked work by label who cannot today." + narrowNote
}
