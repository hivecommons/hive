package adminmcp

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// review.settings changes the review settings (the top-level review block,
// Settings → Review Gate) through PUT /api/config/review, the endpoint the
// dashboard's Review Gate controls save with (handleReviewConfigPut). Every
// field of that endpoint's body is optional and an absent one is left alone,
// so this op sends only the fields the caller names. It never reads the
// section and sends it back whole, so a dashboard edit to any other field is
// never overwritten. The endpoint itself replaces two objects whole,
// verdict_labels and recommendations, and the preview shows every key of
// them that a change clears.
//
// The preview reads GET /api/config/review (settings_read section review) to
// show each named field before and after; a field already at the requested
// value is not sent. ConfirmGuard reads again at write_confirm and refuses
// when a named field moved after the preview. The endpoint answers only
// {"status":"updated"}, so ConfirmResult reads the section back and
// write_confirm reports the named fields as the hive now holds them.
//
// The endpoint validates the change (an autoscale min above its max and the
// like), saves it, records a config_review audit entry naming the caller, and
// is owner only. Its refusal comes back to the caller verbatim as a hive
// refusal; this op does not repeat its checks.
const (
	WriteOpReviewSettings = "review.settings"

	reviewSettingsPath    = "/api/config/review"
	reviewSettingsSection = "review"
)

func reviewSettingsWriteOps() []WriteOp { return []WriteOp{reviewSettingsOp{}} }

type reviewSettingsOp struct{}

type reviewSettingsKind int

const (
	rsBool      reviewSettingsKind = iota
	rsInt                          // a whole number
	rsNumber                       // any number
	rsString                       // a string
	rsList                         // a list of strings, replaced whole
	rsMap                          // an object of strings, replaced whole
	rsTriBool                      // stored as unset, true or false; the body takes true or false
	rsEventMode                    // event_driven_mode: auto, on or off, stored as review.event_driven
	rsWhole                        // an object the endpoint replaces whole; sub names its keys
)

// reviewSettingsField is one field of the PUT /api/config/review body.
type reviewSettingsField struct {
	// key is the body path. A dotted key is a field of a sub-object the
	// endpoint applies field by field (severity, backlog, reviewer_autoscale,
	// contributor_prs, review_bots).
	key string
	// read is where the settings_read section reports the field, dotted;
	// empty means the same path as key.
	read string
	// section is the settings_read section read reports it in; empty means
	// review.
	section string
	kind    reviewSettingsKind
	// sub names the keys of an rsWhole object, relative to it.
	sub []reviewSettingsField
	// keep: within an rsWhole object, a key left out keeps its value,
	// because the endpoint carries it over.
	keep bool
}

// reviewSettingsFields is every field handleReviewConfigPut reads, in the
// order its body declares them. It is the one list of what review.settings
// accepts; TestAdminMCPReviewSettingsFieldsMatchHandlerBody (pkg/dashboard)
// fails when the handler's body and this list differ.
var reviewSettingsFields = []reviewSettingsField{
	{key: "require_approval", kind: rsBool},
	{key: "shadow", kind: rsBool},
	{key: "fan_out", kind: rsBool},
	{key: "max_parallel_reviews", kind: rsInt},
	{key: "max_perspectives_per_pr", kind: rsInt},
	{key: "reviewer_agents", kind: rsList},
	{key: "fixer_agent", kind: rsString},
	{key: "fixer_fallback_agents", kind: rsList},
	{key: "fixer_fallback_after_undeliverable", kind: rsInt},
	{key: "all_authors", kind: rsBool},
	{key: "fix_human_prs", kind: rsTriBool},
	{key: "contributor_prs.base_sync", kind: rsBool},
	{key: "acknowledge_no_findings", kind: rsBool},
	{key: "human_decision_label", kind: rsString},
	{key: "self_authored_decision_policy", kind: rsString},
	{key: "confidence_score", kind: rsBool},
	{key: "priority_labels", kind: rsBool},
	{key: "require_labels", kind: rsList},
	{key: "verdict_labels", kind: rsWhole, sub: []reviewSettingsField{
		{key: "reviewing", kind: rsString},
		{key: "approve", kind: rsString},
		{key: "changes_requested", kind: rsString},
		{key: "requires_human", kind: rsString},
	}},
	{key: "max_reviews_per_head", kind: rsInt},
	{key: "post_comments", kind: rsBool},
	{key: "event_driven_mode", read: "event_driven", kind: rsEventMode},
	{key: "event_debounce_s", kind: rsInt},
	{key: "pending_ttl", kind: rsString},
	{key: "max_redispatch", kind: rsInt},
	{key: "head_max_retries", kind: rsInt},
	{key: "out_of_scope_backlog_disabled", kind: rsBool},
	{key: "max_out_of_scope_backlog_issues", kind: rsInt},
	{key: "perspectives", kind: rsList},
	{key: "perspective_prompts", kind: rsMap},
	{key: "combined_perspectives", kind: rsBool},
	{key: "revise_repos", kind: rsList},
	{key: "revise_verdicts_before", kind: rsString},
	{key: "recommendations", kind: rsWhole, sub: []reviewSettingsField{
		{key: "enabled", kind: rsBool},
		{key: "repos", kind: rsList},
		{key: "labels", kind: rsList, keep: true},
		{key: "min_ready_to_open", kind: rsInt},
	}},
	// The governor settings read reports the effective review-bot threshold
	// (classification.review_bots), which GET /api/config/review does not.
	{key: "review_bots.min_priority", section: SettingsSectionDefault, kind: rsString},
	{key: "severity.block_at", kind: rsString},
	{key: "severity.comment_below", kind: rsTriBool},
	{key: "severity.backlog_below", kind: rsTriBool},
	{key: "backlog.destination", kind: rsString},
	{key: "backlog.project_column_id", kind: rsString},
	{key: "backlog.linear_state", kind: rsString},
	{key: "backlog.jira_status", kind: rsString},
	{key: "backlog.labels", kind: rsList},
	{key: "backlog.max_per_pr_per_day", kind: rsInt},
	{key: "backlog.max_per_pr", kind: rsInt},
	{key: "backlog.min_severity", kind: rsString},
	{key: "reviewer_autoscale.enabled", read: "reviewers.autoscale.enabled", kind: rsBool},
	{key: "reviewer_autoscale.agent", read: "reviewers.autoscale.agent", kind: rsString},
	{key: "reviewer_autoscale.min", read: "reviewers.autoscale.min", kind: rsInt},
	{key: "reviewer_autoscale.max", read: "reviewers.autoscale.max", kind: rsInt},
	{key: "reviewer_autoscale.target_backlog_per_reviewer", read: "reviewers.autoscale.target_backlog_per_reviewer", kind: rsInt},
	{key: "reviewer_autoscale.scale_up_cycles", read: "reviewers.autoscale.scale_up_cycles", kind: rsInt},
	{key: "reviewer_autoscale.scale_down_cycles", read: "reviewers.autoscale.scale_down_cycles", kind: rsInt},
	{key: "reviewer_autoscale.cooldown", read: "reviewers.autoscale.cooldown", kind: rsString},
	{key: "reviewer_autoscale.cpu_per_reviewer", read: "reviewers.autoscale.cpu_per_reviewer", kind: rsNumber},
	{key: "reviewer_autoscale.mem_per_reviewer_mb", read: "reviewers.autoscale.mem_per_reviewer_mb", kind: rsInt},
}

// ReviewSettingsBodyFields lists every PUT /api/config/review body field
// review.settings accepts, dotted, with each key of an object the endpoint
// replaces whole listed under it (verdict_labels.approve). pkg/dashboard
// checks it against the handler's body type.
func ReviewSettingsBodyFields() []string {
	var out []string
	for _, f := range reviewSettingsFields {
		if f.kind != rsWhole {
			out = append(out, f.key)
			continue
		}
		for _, s := range f.sub {
			out = append(out, f.key+"."+s.key)
		}
	}
	return out
}

// reviewSettingsTopKeys are the top-level argument names, each sub-object
// once, in field order.
func reviewSettingsTopKeys() []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range reviewSettingsFields {
		top, _, _ := strings.Cut(f.key, ".")
		if !seen[top] {
			seen[top] = true
			out = append(out, top)
		}
	}
	return out
}

// reviewSettingsGroupKeys are the keys of one field-by-field sub-object.
func reviewSettingsGroupKeys(group string) []string {
	var out []string
	for _, f := range reviewSettingsFields {
		if g, name, nested := strings.Cut(f.key, "."); nested && g == group {
			out = append(out, name)
		}
	}
	return out
}

func (reviewSettingsOp) Name() string { return WriteOpReviewSettings }
func (reviewSettingsOp) Description() string {
	return "Change review settings (the review block, Settings → Review Gate) through PUT " + reviewSettingsPath + ", the endpoint the dashboard's Review Gate controls save with. Owner only. Pass only what changes, shaped like the endpoint's body, for example {\"reviewer_autoscale\": {\"enabled\": true, \"max\": 2}}; only those fields are sent and every other review setting keeps its value. severity, backlog, reviewer_autoscale, contributor_prs and review_bots are changed field by field. A list or perspective_prompts is replaced by the value given. verdict_labels and recommendations are replaced whole by the endpoint, so a key left out of them is cleared (recommendations.labels is kept). An unknown field is refused and the refusal lists the accepted ones; null is refused, because the endpoint reads it as no change. The preview reads the current settings and shows each named field before and after. write_confirm refuses if any of them changed after the preview, then reads them back and reports what the hive now holds. The endpoint's own checks (an autoscale min above its max and the like) come back as its error."
}

func (reviewSettingsOp) InputSchema() map[string]any {
	props := map[string]any{}
	groups := map[string]map[string]any{}
	for i := range reviewSettingsFields {
		f := &reviewSettingsFields[i]
		group, name, nested := strings.Cut(f.key, ".")
		if !nested {
			props[f.key] = f.schema()
			continue
		}
		if groups[group] == nil {
			groups[group] = map[string]any{}
		}
		groups[group][name] = f.schema()
	}
	for group, sub := range groups {
		props[group] = map[string]any{"type": "object", "properties": sub, "additionalProperties": false, "description": "Changed field by field: keys you leave out keep their value."}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{}, "additionalProperties": false}
}

func (f *reviewSettingsField) schema() map[string]any {
	switch f.kind {
	case rsBool, rsTriBool:
		return map[string]any{"type": "boolean"}
	case rsInt:
		return map[string]any{"type": "integer"}
	case rsNumber:
		return map[string]any{"type": "number"}
	case rsEventMode:
		return map[string]any{"type": "string", "enum": []string{"auto", "on", "off"}, "description": "review.event_driven: auto (on when signed webhooks arrive), on or off."}
	case rsList:
		return map[string]any{"type": "array", "items": stringSchema(), "description": "The whole new list."}
	case rsMap:
		return map[string]any{"type": "object", "additionalProperties": stringSchema(), "description": "The whole new map."}
	case rsWhole:
		sub := map[string]any{}
		for i := range f.sub {
			sub[f.sub[i].key] = f.sub[i].schema()
		}
		return map[string]any{"type": "object", "properties": sub, "additionalProperties": false, "description": "Replaced whole by the endpoint: a key you leave out is cleared."}
	default:
		return stringSchema()
	}
}

// reviewSettingsValue is one named field and the value to send for it.
type reviewSettingsValue struct {
	field *reviewSettingsField
	value any
}

func reviewSettingsParseArgs(args map[string]any) ([]reviewSettingsValue, error) {
	const where = WriteOpReviewSettings
	if len(args) == 0 {
		return nil, fmt.Errorf("give at least one review setting to change (accepted: %s)", strings.Join(reviewSettingsTopKeys(), ", "))
	}
	if err := rejectUnknownKeys(args, where, reviewSettingsTopKeys()...); err != nil {
		return nil, err
	}
	groups := map[string]map[string]any{}
	for _, group := range reviewSettingsTopKeys() {
		keys := reviewSettingsGroupKeys(group)
		raw, present := args[group]
		if len(keys) == 0 || !present {
			continue
		}
		obj, ok := raw.(map[string]any)
		if !ok || len(obj) == 0 {
			return nil, fmt.Errorf("%s.%s must be an object naming at least one of: %s", where, group, strings.Join(keys, ", "))
		}
		if err := rejectUnknownKeys(obj, where+"."+group, keys...); err != nil {
			return nil, err
		}
		groups[group] = obj
	}
	var out []reviewSettingsValue
	for i := range reviewSettingsFields {
		f := &reviewSettingsFields[i]
		src, key := args, f.key
		if group, name, nested := strings.Cut(f.key, "."); nested {
			src, key = groups[group], name
		}
		raw, present := src[key]
		if !present {
			continue
		}
		v, err := f.parse(raw, where+"."+f.key)
		if err != nil {
			return nil, err
		}
		out = append(out, reviewSettingsValue{field: f, value: v})
	}
	return out, nil
}

// parse checks the value's JSON type and returns what to send. It checks no
// range or format: the endpoint does, and its refusal reaches the caller.
func (f *reviewSettingsField) parse(raw any, name string) (any, error) {
	if raw == nil {
		return nil, fmt.Errorf("%s: null is not accepted, because the endpoint reads it as no change; leave the field out to keep its value", name)
	}
	switch f.kind {
	case rsBool, rsTriBool:
		if _, ok := raw.(bool); !ok {
			return nil, fmt.Errorf("%s must be true or false", name)
		}
	case rsInt:
		if n, ok := reviewSettingsNumber(raw); !ok || n != math.Trunc(n) {
			return nil, fmt.Errorf("%s must be a whole number", name)
		}
	case rsNumber:
		if _, ok := reviewSettingsNumber(raw); !ok {
			return nil, fmt.Errorf("%s must be a number", name)
		}
	case rsString, rsEventMode:
		if _, ok := raw.(string); !ok {
			return nil, fmt.Errorf("%s must be a string", name)
		}
	case rsList:
		list, ok := reviewSettingsStrings(raw)
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings", name)
		}
		return list, nil
	case rsMap:
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object of strings", name)
		}
		for _, k := range SortKeys(m) {
			if _, ok := m[k].(string); !ok {
				return nil, fmt.Errorf("%s[%s] must be a string", name, k)
			}
		}
	case rsWhole:
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object", name)
		}
		keys := make([]string, 0, len(f.sub))
		for _, s := range f.sub {
			keys = append(keys, s.key)
		}
		if err := rejectUnknownKeys(m, name, keys...); err != nil {
			return nil, err
		}
		out := map[string]any{}
		for i := range f.sub {
			s := &f.sub[i]
			v, present := m[s.key]
			if !present {
				continue
			}
			pv, err := s.parse(v, name+"."+s.key)
			if err != nil {
				return nil, err
			}
			out[s.key] = pv
		}
		return out, nil
	}
	return raw, nil
}

func reviewSettingsNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func reviewSettingsStrings(v any) ([]string, bool) {
	switch list := v.(type) {
	case []string:
		return append([]string{}, list...), true
	case []any:
		out := make([]string, 0, len(list))
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

// current is the value as the section reports it, normalized for display
// and comparison: a key the section leaves out (omitempty) reads as its zero
// value, an unset tri-state as "unset".
func (f *reviewSettingsField) current(raw any) any {
	switch f.kind {
	case rsBool:
		b, _ := raw.(bool)
		return b
	case rsInt, rsNumber:
		n, _ := reviewSettingsNumber(raw)
		return n
	case rsString:
		s, _ := raw.(string)
		return s
	case rsList:
		list, _ := reviewSettingsStrings(raw)
		if list == nil {
			list = []string{}
		}
		return list
	case rsMap:
		m, _ := raw.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		return m
	case rsTriBool:
		if b, ok := raw.(bool); ok {
			return b
		}
		return "unset"
	case rsEventMode:
		if b, ok := raw.(bool); ok {
			if b {
				return "on"
			}
			return "off"
		}
		return "auto"
	}
	return raw
}

// requested is a sent value normalized the way current normalizes a read one.
func (f *reviewSettingsField) requested(v any) any {
	if f.kind == rsEventMode {
		s, _ := v.(string)
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			return s
		}
		return "auto"
	}
	return f.current(v)
}

// reviewSettingsState is what the preview read: the review section, and the
// governor section when a field is reported there.
type reviewSettingsState map[string]map[string]any

func (st reviewSettingsState) lookup(f *reviewSettingsField) any {
	section := f.section
	if section == "" {
		section = reviewSettingsSection
	}
	path := f.read
	if path == "" {
		path = f.key
	}
	var cur any = st[section]
	for _, part := range strings.Split(path, ".") {
		m, _ := cur.(map[string]any)
		if m == nil {
			return nil
		}
		cur = m[part]
	}
	return cur
}

func reviewSettingsRead(ctx context.Context, provider Provider, values []reviewSettingsValue) (reviewSettingsState, error) {
	if provider == nil {
		return nil, fmt.Errorf("%s reads the current review settings first, and this connection cannot read them", WriteOpReviewSettings)
	}
	sections := []string{reviewSettingsSection}
	for _, v := range values {
		if v.field.section != "" && v.field.section != reviewSettingsSection {
			sections = append(sections, v.field.section)
			break
		}
	}
	st := reviewSettingsState{}
	for _, section := range sections {
		data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": section})
		if err != nil {
			return nil, fmt.Errorf("%s needs the current review settings (settings_read section %s): %w", WriteOpReviewSettings, section, err)
		}
		m, ok := data.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s could not read the current review settings: settings_read section %s returned no object", WriteOpReviewSettings, section)
		}
		st[section] = m
	}
	return st, nil
}

type reviewSettingsRow struct {
	field         string
	before, after any
}

type reviewSettingsPlan struct {
	rows        []reviewSettingsRow
	unchanged   []string
	body        map[string]any
	whole       []string // rsWhole fields sent
	fingerprint string
}

func reviewSettingsPlanFor(ctx context.Context, args map[string]any, provider Provider) (reviewSettingsPlan, error) {
	values, err := reviewSettingsParseArgs(args)
	if err != nil {
		return reviewSettingsPlan{}, err
	}
	st, err := reviewSettingsRead(ctx, provider, values)
	if err != nil {
		return reviewSettingsPlan{}, err
	}
	plan := reviewSettingsPlan{body: map[string]any{}}
	before := map[string]any{}
	for _, v := range values {
		f := v.field
		raw := st.lookup(f)
		before[f.key] = raw
		var rows []reviewSettingsRow
		if f.kind == rsWhole {
			sent, _ := v.value.(map[string]any)
			cur, _ := raw.(map[string]any)
			for i := range f.sub {
				s := &f.sub[i]
				b := s.current(cur[s.key])
				a := b
				if nv, ok := sent[s.key]; ok {
					a = s.requested(nv)
				} else if !s.keep {
					a = s.current(nil)
				}
				if compactJSON(a) != compactJSON(b) {
					rows = append(rows, reviewSettingsRow{field: f.key + "." + s.key, before: b, after: a})
				}
			}
		} else if b, a := f.current(raw), f.requested(v.value); compactJSON(a) != compactJSON(b) {
			rows = append(rows, reviewSettingsRow{field: f.key, before: b, after: a})
		}
		if len(rows) == 0 {
			plan.unchanged = append(plan.unchanged, f.key)
			continue
		}
		plan.rows = append(plan.rows, rows...)
		if f.kind == rsWhole {
			plan.whole = append(plan.whole, f.key)
		}
		if group, name, nested := strings.Cut(f.key, "."); nested {
			sub, _ := plan.body[group].(map[string]any)
			if sub == nil {
				sub = map[string]any{}
				plan.body[group] = sub
			}
			sub[name] = v.value
		} else {
			plan.body[f.key] = v.value
		}
	}
	if len(plan.rows) == 0 {
		return reviewSettingsPlan{}, fmt.Errorf("nothing would change: %s already as requested", strings.Join(plan.unchanged, ", "))
	}
	plan.fingerprint = settingsFingerprint(before)
	return plan, nil
}

// Preview cannot read the current settings, so it only checks the
// arguments. write_preview uses PreviewWithProvider; write_confirm calls
// Preview to check the request still names this endpoint, and then
// ConfirmGuard reads again.
func (reviewSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := reviewSettingsParseArgs(args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpReviewSettings,
		Summary:             "Change review settings (current settings not read)",
		Target:              "review settings (review)",
		Request:             WriteRequest{Method: http.MethodPut, Path: reviewSettingsPath},
		WideningDisclosure:  "Potential widening: the current review settings were not read, so this preview cannot say what would change.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}, nil
}

func (reviewSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	plan, err := reviewSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return WritePreview{}, err
	}
	fields := make([]string, 0, len(plan.rows))
	changes := make([]map[string]any, 0, len(plan.rows))
	lines := make([]string, 0, len(plan.rows))
	for _, row := range plan.rows {
		fields = append(fields, row.field)
		changes = append(changes, map[string]any{"field": row.field, "before": row.before, "after": row.after})
		lines = append(lines, row.field+": "+compactJSON(row.before)+" → "+compactJSON(row.after))
	}
	effects := append([]string{}, lines...)
	effects = append(effects, "Hive sends PUT "+reviewSettingsPath+" with only these fields. Every other review setting keeps its value, including one changed on the dashboard after this preview.")
	if len(plan.whole) > 0 {
		effects = append(effects, "The endpoint replaces "+strings.Join(plan.whole, " and ")+" whole, so a key left out of it is cleared (recommendations.labels is kept); the changes above include those keys.")
	}
	effects = append(effects,
		"The endpoint validates the change, saves it, records a config_review audit entry with you as the actor, and applies it as a dashboard save does. It is owner only, and a value it refuses comes back as its own error.",
		"write_confirm reads these settings again and refuses if any of them changed after this preview, then reads them back and reports what the hive now holds.",
	)
	details := map[string]any{"fields": fields, "changes": changes}
	if len(plan.unchanged) > 0 {
		effects = append(effects, "Already as requested, so not sent: "+strings.Join(plan.unchanged, ", ")+".")
		details["unchanged"] = plan.unchanged
	}
	return WritePreview{
		Operation:           WriteOpReviewSettings,
		Summary:             "Change review settings: " + strings.Join(fields, ", "),
		Target:              "review settings (review)",
		Request:             WriteRequest{Method: http.MethodPut, Path: reviewSettingsPath, Body: plan.body},
		Effects:             effects,
		WideningDisclosure:  reviewSettingsDisclosure(plan.rows),
		ConfirmationMessage: "Confirm changing review settings: " + strings.Join(lines, "; ") + ".",
		Details:             details,
		StateFingerprint:    plan.fingerprint,
	}, nil
}

// ConfirmGuard reads the settings again and returns the fingerprint of the
// named fields' values.
func (reviewSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	plan, err := reviewSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the review settings again before writing, and that check refused this change; preview it again: %w", err)
	}
	return plan.fingerprint, nil
}

// ConfirmResult reads the settings back after the write and returns each
// named field as the hive now holds it.
func (reviewSettingsOp) ConfirmResult(ctx context.Context, args map[string]any, provider Provider) (any, error) {
	values, err := reviewSettingsParseArgs(args)
	if err != nil {
		return nil, err
	}
	st, err := reviewSettingsRead(ctx, provider, values)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, v := range values {
		f := v.field
		raw := st.lookup(f)
		if f.kind != rsWhole {
			out[f.key] = f.current(raw)
			continue
		}
		cur, _ := raw.(map[string]any)
		obj := map[string]any{}
		for i := range f.sub {
			obj[f.sub[i].key] = f.sub[i].current(cur[f.sub[i].key])
		}
		out[f.key] = obj
	}
	return out, nil
}

// reviewSettingsDisclosure names each change that loosens the review gate or
// lets the hive add reviewers on its own.
func reviewSettingsDisclosure(rows []reviewSettingsRow) string {
	var notes []string
	for _, row := range rows {
		switch row.field {
		case "require_approval":
			if row.before == true && row.after == false {
				notes = append(notes, "require_approval turns off: a pull request no longer needs an approving review verdict to pass the review gate.")
			}
		case "severity.block_at":
			if b, a := reviewSettingsBlockRank(row.before), reviewSettingsBlockRank(row.after); a > 0 && a < b {
				notes = append(notes, fmt.Sprintf("severity.block_at moves from %s to %s: findings that blocked a merge before are posted without blocking.", compactJSON(row.before), compactJSON(row.after)))
			}
		case "reviewer_autoscale.enabled":
			if row.before == false && row.after == true {
				notes = append(notes, "reviewer_autoscale turns on: the hive adds and retires reviewer replicas on its own, within min and max, which can raise spend.")
			}
		case "reviewer_autoscale.max":
			// 0 or less means the default ceiling.
			b, a := reviewSettingsAutoscaleMax(row.before), reviewSettingsAutoscaleMax(row.after)
			if a > b {
				notes = append(notes, fmt.Sprintf("reviewer_autoscale.max rises from %v to %v: when autoscale is on, the hive may run more reviewer replicas at once, which can raise spend.", b, a))
			}
		}
	}
	never := "This operation merges, approves and closes nothing itself."
	if len(notes) == 0 {
		return "No widening: none of these changes loosens the review gate or lets the hive add reviewers. " + never
	}
	return "Widening: " + strings.Join(notes, " ") + " " + never
}

// reviewSettingsAutoscaleMax is the reviewer ceiling a max value means.
func reviewSettingsAutoscaleMax(v any) float64 {
	if n, _ := v.(float64); n > 0 {
		return n
	}
	return config.DefaultReviewerAutoscaleMax
}

// reviewSettingsBlockRank orders block_at from least to most strict: P1, P2,
// P3. Unset (no blocking line configured) is 0 and is not compared.
func reviewSettingsBlockRank(v any) int {
	s, _ := v.(string)
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "P1":
		return 1
	case "P2":
		return 2
	case "P3":
		return 3
	}
	return 0
}
