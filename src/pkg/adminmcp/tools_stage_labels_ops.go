package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// governor.stage_labels: the stale contributor PR settings under
// governor.labels.stage that Settings → Labels saves through PUT
// /api/config/governor/labels — abandon_after (the idle clock) and
// abandon_close_repos (the repositories that close abandoned PRs at ACMM
// L6) — and the person-PR takeover timer under
// governor.labels.stage.adoption (hivecommons/hive#19395): person_after,
// person_grace, person_repos and person_human_repos. Before this op no
// governor.labels.stage.* key had an admin-MCP write. The endpoint leaves
// every key it is not sent alone and replaces a list it is sent, so the op
// merges each list change onto what the preview read and sends only what
// changes.

const WriteOpGovernorStageLabels = "governor.stage_labels"

type stageLabelsOp struct{}

// stageLabelsState is the stage settings as the preview read them; the
// exported fields are what the confirm fingerprints.
type stageLabelsState struct {
	AbandonAfter      string   `json:"abandon_after"`
	AbandonCloseRepos []string `json:"abandon_close_repos"`
	PersonAfter       string   `json:"person_after"`
	PersonGrace       string   `json:"person_grace"`
	PersonRepos       []string `json:"person_repos"`
	PersonHumanRepos  []string `json:"person_human_repos"`
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
	for _, f := range []struct {
		key string
		dst *string
	}{{"stageAdoptionPersonAfter", &st.PersonAfter}, {"stageAdoptionPersonGrace", &st.PersonGrace}} {
		raw, present := m[f.key]
		if !present {
			return st, missingSetting(what, f.key)
		}
		if raw != nil {
			v, ok := raw.(string)
			if !ok {
				return st, missingSetting(what, f.key)
			}
			*f.dst = strings.TrimSpace(v)
		}
	}
	if st.PersonRepos, ok = stringsField(m, "stageAdoptionPersonRepos"); !ok {
		return st, missingSetting(what, "stageAdoptionPersonRepos")
	}
	if st.PersonHumanRepos, ok = stringsField(m, "stageAdoptionPersonHumanRepos"); !ok {
		return st, missingSetting(what, "stageAdoptionPersonHumanRepos")
	}
	st.projectRepos, _ = stringsField(m, "repos")
	return st, nil
}

type stageLabelsRequest struct {
	abandonAfter     *string
	closeRepos       *listChange
	personAfter      *string
	personGrace      *string
	personRepos      *listChange
	personHumanRepos *listChange
}

func parseStageLabelsArgs(args map[string]any) (stageLabelsRequest, error) {
	const where = WriteOpGovernorStageLabels
	var req stageLabelsRequest
	if err := rejectUnknownKeys(args, where, "abandon_after", "abandon_close_repos", "person_after", "person_grace", "person_repos", "person_human_repos"); err != nil {
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
	for _, f := range []struct {
		key, zero string
		dst       **string
	}{{"person_after", "for off", &req.personAfter}, {"person_grace", "for the 24h default", &req.personGrace}} {
		raw, present := args[f.key]
		if !present || raw == nil {
			continue
		}
		s, ok := raw.(string)
		if !ok {
			return req, fmt.Errorf("%s.%s must be a Go duration string such as \"24h\", or \"0\" %s", where, f.key, f.zero)
		}
		norm, err := normalizeStageDurationArg(f.key, s, f.zero)
		if err != nil {
			return req, err
		}
		*f.dst = &norm
	}
	if c, ok, err := listChangeArg(args, "person_repos", where); err != nil {
		return req, err
	} else if ok {
		req.personRepos = &c
	}
	if c, ok, err := listChangeArg(args, "person_human_repos", where); err != nil {
		return req, err
	} else if ok {
		req.personHumanRepos = &c
	}
	if req.abandonAfter == nil && req.closeRepos == nil && req.personAfter == nil && req.personGrace == nil && req.personRepos == nil && req.personHumanRepos == nil {
		return req, fmt.Errorf("give at least one of abandon_after, abandon_close_repos, person_after, person_grace, person_repos or person_human_repos")
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

// normalizeStageDurationArg mirrors the endpoint for person_after and
// person_grace: "" or a zero duration is stored as "" (person_after off,
// person_grace the 24h default), anything else must be a positive Go
// duration. zero says what "0" means, for the error.
func normalizeStageDurationArg(field, s, zero string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return "", nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return "", fmt.Errorf("%s.%s %q is not a Go duration such as \"24h\" (or \"0\" %s)", WriteOpGovernorStageLabels, field, s, zero)
	}
	if d == 0 {
		return "", nil
	}
	return s, nil
}

// qualifyStageRepo resolves a repository named in a repository list field
// (abandon_close_repos, person_repos, person_human_repos) to the
// owner/repo form the endpoint stores, using the project's repository list;
// a name outside project.repos is refused, as the endpoint would.
func qualifyStageRepo(field, name string, projectRepos []string) (string, error) {
	name = strings.TrimSpace(name)
	for _, r := range projectRepos {
		if strings.EqualFold(r, name) {
			return r, nil
		}
		if i := strings.LastIndex(r, "/"); i >= 0 && !strings.Contains(name, "/") && strings.EqualFold(r[i+1:], name) {
			return r, nil
		}
	}
	return "", fmt.Errorf("%s.%s: %q is not in project.repos (%s)", WriteOpGovernorStageLabels, field, name, listLabel(projectRepos))
}

// mergeStageRepos applies a repository list change for field onto before,
// qualifying added names against project.repos.
func mergeStageRepos(field string, change listChange, before, projectRepos []string) ([]string, error) {
	qualified := change
	qualified.add = nil
	for _, a := range change.add {
		full, err := qualifyStageRepo(field, a, projectRepos)
		if err != nil {
			return nil, err
		}
		qualified.add = append(qualified.add, full)
	}
	qualified.remove = nil
	for _, r := range change.remove {
		// A stored entry may sit outside project.repos; remove it as named.
		if full, err := qualifyStageRepo(field, r, projectRepos); err == nil {
			r = full
		}
		qualified.remove = append(qualified.remove, r)
	}
	return qualified.apply(field, before, foldCase)
}

func abandonAfterLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return "off"
	}
	return v
}

// personGraceLabel renders person_grace, whose empty value is the default.
func personGraceLabel(v string) string {
	if strings.TrimSpace(v) == "" {
		return fmt.Sprintf("%dh (default)", int(config.DefaultPersonTakeoverGrace/time.Hour))
	}
	return v
}

func (stageLabelsOp) Name() string { return WriteOpGovernorStageLabels }
func (stageLabelsOp) Description() string {
	return "Change the stale contributor PR settings under governor.labels.stage — abandon_after (the idle clock: a contributor PR parked at 071 needs-author or at an adoption-stalled 085 with no outside activity this long is flagged abandoned; \"0\" is off) and abandon_close_repos (repositories that close abandoned PRs after a notice and a 24h grace, at ACMM L6 only) — and the person-PR takeover timer under governor.labels.stage.adoption — person_after (how long a person's PR sent back to its author at 071 / 072 for work the hive can do may sit idle before the hive posts a takeover notice; \"0\" is off), person_grace (the wait after that notice before the hive takes the PR over through the Adopt now path; \"0\" or \"\" is the 24h default), person_repos (repositories where it applies to the agent-written PRs of this hive's owners) and person_human_repos (repositories where it applies to author/human PRs), ACMM L6 only — through PUT /api/config/governor/labels, the endpoint Settings → Labels saves through. Each repository list takes {add, remove, clear}: the preview reads the current values, shows each before → after and sends the merged list, and write_confirm is refused if they changed since the preview. Owner only: the endpoint refuses any other role."
}
func (stageLabelsOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"abandon_after":       map[string]any{"type": "string", "description": "Go duration such as 168h after which an idle parked contributor PR is flagged abandoned; \"0\" or \"\" turns the idle clock off."},
		"abandon_close_repos": listChangeSchema("Repositories (in project.repos, owner/repo or bare name) whose abandoned contributor PRs the hive closes after a notice and a 24h grace, at ACMM L6 only; an empty list closes nothing."),
		"person_after":        map[string]any{"type": "string", "description": "Go duration such as 24h a person's PR sent back to its author may sit idle before the hive posts a takeover notice; \"0\" or \"\" turns the takeover timer off."},
		"person_grace":        map[string]any{"type": "string", "description": "Go duration the hive waits after the takeover notice before it takes the PR over; \"0\" or \"\" is the 24h default."},
		"person_repos":        listChangeSchema("Repositories (in project.repos, owner/repo or bare name) where the takeover timer applies to the agent-written PRs of this hive's owners, at ACMM L6 only; an empty list applies it nowhere."),
		"person_human_repos":  listChangeSchema("Repositories (in project.repos, owner/repo or bare name) where the takeover timer applies to hand-written author/human PRs, at ACMM L6 only; an empty list applies it nowhere."),
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
		list, err := mergeStageRepos("abandon_close_repos", *req.closeRepos, before.AbandonCloseRepos, before.projectRepos)
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
	if req.personAfter != nil {
		after.PersonAfter = *req.personAfter
		body["stage_adoption_person_after"] = after.PersonAfter
		if after.PersonAfter != before.PersonAfter {
			c := settingChange{Field: "person_after", Before: abandonAfterLabel(before.PersonAfter), After: abandonAfterLabel(after.PersonAfter)}
			if after.PersonAfter == "" {
				c.Note = "the takeover timer is off: the hive takes over no person's PR on its own"
			}
			changes = append(changes, c)
		}
	}
	if req.personGrace != nil {
		after.PersonGrace = *req.personGrace
		body["stage_adoption_person_grace"] = after.PersonGrace
		if after.PersonGrace != before.PersonGrace {
			changes = append(changes, settingChange{Field: "person_grace", Before: personGraceLabel(before.PersonGrace), After: personGraceLabel(after.PersonGrace)})
		}
	}
	for _, f := range []struct {
		field, key string
		change     *listChange
		before     []string
		dst        *[]string
		empty      string
	}{
		{"person_repos", "stage_adoption_person_repos", req.personRepos, before.PersonRepos, &after.PersonRepos, "empty: the hive takes over no owner's agent PR on its own"},
		{"person_human_repos", "stage_adoption_person_human_repos", req.personHumanRepos, before.PersonHumanRepos, &after.PersonHumanRepos, "empty: the hive takes over no hand-written PR on its own"},
	} {
		if f.change == nil {
			continue
		}
		list, err := mergeStageRepos(f.field, *f.change, f.before, before.projectRepos)
		if err != nil {
			return WritePreview{}, err
		}
		*f.dst = list
		body[f.key] = list
		if !sameList(f.before, list, foldCase) {
			c := settingChange{Field: f.field, Before: f.before, After: list}
			if len(list) == 0 {
				c.Note = f.empty
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
	)
	if req.personAfter != nil || req.personGrace != nil || req.personRepos != nil || req.personHumanRepos != nil {
		effects = append(effects, "The takeover timer acts only on a person's PR at 071 / 072 in a listed repository at ACMM L6, sent back for a merge conflict, a failed check or a review bot's change request (never a person's change request or an 085 decision), idle past person_after: one notice, then person_grace with no push, comment, review, hold, do-not-merge, needs-human or hive/keep-open label, then the operator's Adopt now path. Let hive decide still never adopts a person's PR.")
	}
	effects = append(effects, "This write itself closes, takes over, comments on and labels nothing.")
	return WritePreview{
		Operation:           WriteOpGovernorStageLabels,
		Summary:             "Change the stale and person PR settings: " + strings.Join(fields, ", "),
		Target:              "governor.labels.stage",
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/labels", Body: body},
		Effects:             effects,
		WideningDisclosure:  stageLabelsDisclosure(before, after),
		ConfirmationMessage: "Confirm changing the stale and person PR settings: " + strings.Join(fields, ", ") + ".",
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
	for _, l := range []struct {
		before, after []string
		what          string
	}{
		{before.PersonRepos, after.PersonRepos, "this hive's owners' agent-written PRs"},
		{before.PersonHumanRepos, after.PersonHumanRepos, "hand-written author/human PRs"},
	} {
		if added, _ := listDiff(l.before, l.after, foldCase); len(added) > 0 {
			line := "the hive may now take over " + l.what + " in " + listLabel(added) + " on its own (after the idle time, a notice and the grace, at ACMM L6 only)"
			if after.PersonAfter == "" {
				line += " once person_after is set"
			}
			widen = append(widen, line)
		}
	}
	personListed := append(append([]string(nil), after.PersonRepos...), after.PersonHumanRepos...)
	if len(personListed) > 0 && after.PersonAfter != "" && abandonAfterShorter(before.PersonAfter, after.PersonAfter) {
		widen = append(widen, "a person's idle PR in "+listLabel(personListed)+" gets the takeover notice after "+after.PersonAfter+" instead of "+abandonAfterLabel(before.PersonAfter))
	}
	if len(personListed) > 0 && after.PersonAfter != "" && personGraceShorter(before.PersonGrace, after.PersonGrace) {
		widen = append(widen, "the takeover follows the notice after "+personGraceLabel(after.PersonGrace)+" instead of "+personGraceLabel(before.PersonGrace))
	}
	if len(widen) > 0 {
		return "Widening: " + strings.Join(widen, "; ") + "."
	}
	return "No widening: the hive closes or takes over no PR it could not before. Flagging a PR abandoned only changes how the dashboard groups it."
}

// personGraceShorter reports whether after makes the takeover follow the
// notice sooner than before ("" is the 24h default).
func personGraceShorter(before, after string) bool {
	parse := func(v string) time.Duration {
		if d, err := time.ParseDuration(strings.TrimSpace(v)); err == nil && d > 0 {
			return d
		}
		return config.DefaultPersonTakeoverGrace
	}
	return parse(after) < parse(before)
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
