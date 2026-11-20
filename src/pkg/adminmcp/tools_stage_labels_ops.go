package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// governor.stage_labels: the stale contributor PR settings under
// governor.labels.stage that Settings → Labels saves through PUT
// /api/config/governor/labels — abandon_after (the idle clock) and
// abandon_close_repos (the repositories that close abandoned PRs at ACMM
// L6). Before this op no governor.labels.stage.* key had an admin-MCP
// write. The endpoint leaves every key it is not sent alone and replaces a
// list it is sent, so the op merges the list change onto what the preview
// read and sends only what changes.

const WriteOpGovernorStageLabels = "governor.stage_labels"

type stageLabelsOp struct{}

// stageLabelsState is the stage settings as the preview read them; the
// exported fields are what the confirm fingerprints.
type stageLabelsState struct {
	AbandonAfter      string   `json:"abandon_after"`
	AbandonCloseRepos []string `json:"abandon_close_repos"`
	projectRepos      []string
}

func readStageLabelsState(ctx context.Context, provider Provider) (stageLabelsState, error) {
	const what = "stage label settings"
	m, err := readGovernorSettings(ctx, provider, what)
	if err != nil {
		return stageLabelsState{}, err
	}
	var st stageLabelsState
	raw, present := m["stageAbandonAfter"]
	if !present {
		return st, missingSetting(what, "stageAbandonAfter")
	}
	if raw != nil {
		s, ok := raw.(string)
		if !ok {
			return st, missingSetting(what, "stageAbandonAfter")
		}
		st.AbandonAfter = strings.TrimSpace(s)
	}
	var ok bool
	if st.AbandonCloseRepos, ok = stringsField(m, "stageAbandonCloseRepos"); !ok {
		return st, missingSetting(what, "stageAbandonCloseRepos")
	}
	st.projectRepos, _ = stringsField(m, "repos")
	return st, nil
}

type stageLabelsRequest struct {
	abandonAfter *string
	closeRepos   *listChange
}

func parseStageLabelsArgs(args map[string]any) (stageLabelsRequest, error) {
	const where = WriteOpGovernorStageLabels
	var req stageLabelsRequest
	if err := rejectUnknownKeys(args, where, "abandon_after", "abandon_close_repos"); err != nil {
		return req, err
	}
	if raw, present := args["abandon_after"]; present && raw != nil {
		s, ok := raw.(string)
		if !ok {
			return req, fmt.Errorf("%s.abandon_after must be a Go duration string such as \"168h\", or \"0\" for off", where)
		}
		norm, err := normalizeAbandonAfterArg(s)
		if err != nil {
			return req, err
		}
		req.abandonAfter = &norm
	}
	if c, ok, err := listChangeArg(args, "abandon_close_repos", where); err != nil {
		return req, err
	} else if ok {
		req.closeRepos = &c
	}
	if req.abandonAfter == nil && req.closeRepos == nil {
		return req, fmt.Errorf("give at least one of abandon_after or abandon_close_repos")
	}
	return req, nil
}

// normalizeAbandonAfterArg mirrors the endpoint: "" or a zero duration is
// off (stored as ""), anything else must be a positive Go duration.
func normalizeAbandonAfterArg(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return "", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return "", fmt.Errorf("%s.abandon_after %q is not a Go duration such as \"168h\" (or \"0\" for off)", WriteOpGovernorStageLabels, s)
	}
	if d == 0 {
		return "", nil
	}
	return s, nil
}

// qualifyStageRepo resolves a repository named in abandon_close_repos to
// the owner/repo form the endpoint stores, using the project's repository
// list; a name outside project.repos is refused, as the endpoint would.
func qualifyStageRepo(name string, projectRepos []string) (string, error) {
	name = strings.TrimSpace(name)
	for _, r := range projectRepos {
		if strings.EqualFold(r, name) {
			return r, nil
		}
		if i := strings.LastIndex(r, "/"); i >= 0 && !strings.Contains(name, "/") && strings.EqualFold(r[i+1:], name) {
			return r, nil
		}
	}
	return "", fmt.Errorf("%s.abandon_close_repos: %q is not in project.repos (%s)", WriteOpGovernorStageLabels, name, listLabel(projectRepos))
}

func abandonAfterLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return "off"
	}
	return v
}

func (stageLabelsOp) Name() string { return WriteOpGovernorStageLabels }
func (stageLabelsOp) Description() string {
	return "Change the stale contributor PR settings under governor.labels.stage — abandon_after (the idle clock: a contributor PR parked at 071 needs-author or at an adoption-stalled 085 with no outside activity this long is flagged abandoned; \"0\" is off) and abandon_close_repos (repositories that close abandoned PRs after a notice and a 24h grace, at ACMM L6 only) — through PUT /api/config/governor/labels, the endpoint Settings → Labels saves through. abandon_close_repos takes {add, remove, clear}: the preview reads the current values, shows each before → after and sends the merged list, and write_confirm is refused if they changed since the preview. Owner only: the endpoint refuses any other role."
}
func (stageLabelsOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"abandon_after":       map[string]any{"type": "string", "description": "Go duration such as 168h after which an idle parked contributor PR is flagged abandoned; \"0\" or \"\" turns the idle clock off."},
		"abandon_close_repos": listChangeSchema("Repositories (in project.repos, owner/repo or bare name) whose abandoned contributor PRs the hive closes after a notice and a 24h grace, at ACMM L6 only; an empty list closes nothing."),
	}, "additionalProperties": false}
}

// Preview without a provider checks the arguments and names the endpoint.
func (stageLabelsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseStageLabelsArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpGovernorStageLabels, "governor.labels.stage", "/api/config/governor/labels", "stage label settings"), nil
}

// ConfirmGuard reads the settings again at write_confirm and returns their
// fingerprint, which must still match the preview's.
func (op stageLabelsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// PreviewWithProvider reads the current values, merges the change onto
// them and discloses what the result lets the hive do.
func (stageLabelsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	req, err := parseStageLabelsArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	before, err := readStageLabelsState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	after := before
	body := map[string]any{}
	var changes []settingChange
	if req.abandonAfter != nil {
		after.AbandonAfter = *req.abandonAfter
		body["stage_abandon_after"] = after.AbandonAfter
		if after.AbandonAfter != before.AbandonAfter {
			c := settingChange{Field: "abandon_after", Before: abandonAfterLabel(before.AbandonAfter), After: abandonAfterLabel(after.AbandonAfter)}
			if after.AbandonAfter == "" {
				c.Note = "the idle clock is off: no PR is flagged abandoned and no abandoned PR is closed"
			}
			changes = append(changes, c)
		}
	}
	if req.closeRepos != nil {
		qualified := *req.closeRepos
		qualified.add = nil
		for _, a := range req.closeRepos.add {
			full, err := qualifyStageRepo(a, before.projectRepos)
			if err != nil {
				return WritePreview{}, err
			}
			qualified.add = append(qualified.add, full)
		}
		qualified.remove = nil
		for _, r := range req.closeRepos.remove {
			// A stored entry may sit outside project.repos; remove it as named.
			if full, err := qualifyStageRepo(r, before.projectRepos); err == nil {
				r = full
			}
			qualified.remove = append(qualified.remove, r)
		}
		list, err := qualified.apply("abandon_close_repos", before.AbandonCloseRepos, foldCase)
		if err != nil {
			return WritePreview{}, err
		}
		after.AbandonCloseRepos = list
		body["stage_abandon_close_repos"] = list
		if !sameList(before.AbandonCloseRepos, list, foldCase) {
			c := settingChange{Field: "abandon_close_repos", Before: before.AbandonCloseRepos, After: list}
			if len(list) == 0 {
				c.Note = "empty: the hive closes no abandoned PR"
			}
			changes = append(changes, c)
		}
	}
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("%s changes nothing: the settings already read that way", WriteOpGovernorStageLabels)
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
		"Hive saves it through Settings → Labels (PUT /api/config/governor/labels), records a config_governor_labels audit entry, and applies it on the next stage-label pass, with no restart. Fields not named here keep their values.",
		"Flagging a PR abandoned changes no stage and no label; the abandoned issue lock stays (withheld covering_pr_stalled). Closing happens only in a listed repository at ACMM L6, after one notice and a 24h grace with no activity, no hold and no hive/keep-open label.",
		"This write itself closes, comments on and labels nothing.",
	)
	return WritePreview{
		Operation:           WriteOpGovernorStageLabels,
		Summary:             "Change the stale contributor PR settings: " + strings.Join(fields, ", "),
		Target:              "governor.labels.stage",
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/labels", Body: body},
		Effects:             effects,
		WideningDisclosure:  stageLabelsDisclosure(before, after),
		ConfirmationMessage: "Confirm changing the stale contributor PR settings: " + strings.Join(fields, ", ") + ".",
		Details:             map[string]any{"changes": changeDetails(changes)},
		StateFingerprint:    fingerprint,
	}, nil
}

// stageLabelsDisclosure: adding a repository to abandon_close_repos lets the
// hive close other people's PRs there; turning the idle clock on (or
// shortening it) where repositories are listed brings those closes sooner.
func stageLabelsDisclosure(before, after stageLabelsState) string {
	added, _ := listDiff(before.AbandonCloseRepos, after.AbandonCloseRepos, foldCase)
	var widen []string
	if len(added) > 0 {
		line := "the hive may now close abandoned contributor PRs in " + listLabel(added) + " (after a notice and a 24h grace, at ACMM L6 only)"
		if after.AbandonAfter == "" {
			line += " once abandon_after is set"
		}
		widen = append(widen, line)
	}
	if len(after.AbandonCloseRepos) > 0 && after.AbandonAfter != "" && abandonAfterShorter(before.AbandonAfter, after.AbandonAfter) {
		widen = append(widen, "contributor PRs in "+listLabel(after.AbandonCloseRepos)+" become closable after "+after.AbandonAfter+" idle instead of "+abandonAfterLabel(before.AbandonAfter))
	}
	if len(widen) > 0 {
		return "Widening: " + strings.Join(widen, "; ") + "."
	}
	return "No widening: the hive closes no PR it could not close before. Flagging a PR abandoned only changes how the dashboard groups it."
}

// abandonAfterShorter reports whether after makes the idle clock fire
// sooner than before ("" is off, the longest).
func abandonAfterShorter(before, after string) bool {
	a, errA := time.ParseDuration(after)
	if errA != nil || a <= 0 {
		return false
	}
	if strings.TrimSpace(before) == "" {
		return true
	}
	b, errB := time.ParseDuration(before)
	return errB != nil || a < b
}
