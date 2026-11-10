package adminmcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Write operations for two Settings surfaces whose lists an owner edits on
// the dashboard: the issue filter (project.issue_filter, Settings → Labels)
// and Sentinel (sentinel.*, Settings → Security). Each wraps exactly the
// endpoint the dashboard saves through, so the endpoint's owner-only check,
// validation and audit entry apply unchanged.
//
// Lists are edited by change, not replaced: {add, remove, clear}. The preview
// reads the current settings, shows each list before and after, and sends the
// merged list, because both endpoints replace a list they are sent. It also
// records a fingerprint of what it read (WritePreview.StateFingerprint), and
// ConfirmGuard reads again at write_confirm, which refuses with
// ErrConfirmationStale when it no longer matches, so a confirm never
// overwrites a change somebody made in between.
//
// decisions.delegate ("Let hive decide": enabled, per-repo, llm,
// effect_timeout) is written through PUT /api/config/decisions/delegate by
// its own operation, decisions.delegate_settings (tools_decisions_ops.go).
const (
	WriteOpProjectIssueFilter = "project.issue_filter"
	WriteOpSentinelSettings   = "sentinel.settings"
)

func configSurfaceWriteOps() []WriteOp {
	return []WriteOp{issueFilterOp{}, sentinelSettingsOp{}}
}

type issueFilterOp struct{}
type sentinelSettingsOp struct{}

// ---- list changes ----------------------------------------------------------

// listChange is one list field's requested edit.
type listChange struct {
	add    []string
	remove []string
	clear  bool
}

func listChangeSchema(desc string) map[string]any {
	return map[string]any{
		"type":        "object",
		"description": desc + " Give add, remove or clear (remove every entry; add still applies after it).",
		"properties": map[string]any{
			"add":    map[string]any{"type": "array", "items": stringSchema()},
			"remove": map[string]any{"type": "array", "items": stringSchema()},
			"clear":  map[string]any{"type": "boolean"},
		},
		"additionalProperties": false,
	}
}

// rejectUnknownKeys refuses any key of m not in allowed, naming where it was.
func rejectUnknownKeys(m map[string]any, where string, allowed ...string) error {
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	var bad []string
	for k := range m {
		if !ok[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("unsupported field %s in %s (allowed: %s)", strings.Join(quoteAll(bad), ", "), where, strings.Join(allowed, ", "))
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = fmt.Sprintf("%q", s)
	}
	return out
}

// objectArg returns args[key] as an object, or nil when it is absent.
func objectArg(args map[string]any, key, where string) (map[string]any, error) {
	raw, present := args[key]
	if !present || raw == nil {
		return nil, nil
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s must be an object", where, key)
	}
	return m, nil
}

// listChangeArg parses args[key] as a list change. It reports false when the
// field is absent.
func listChangeArg(args map[string]any, key, where string) (listChange, bool, error) {
	m, err := objectArg(args, key, where)
	if err != nil || m == nil {
		return listChange{}, false, err
	}
	name := where + "." + key
	if err := rejectUnknownKeys(m, name, "add", "remove", "clear"); err != nil {
		return listChange{}, false, err
	}
	var c listChange
	if c.add, err = namedStringList(m["add"], name+".add"); err != nil {
		return listChange{}, false, err
	}
	if c.remove, err = namedStringList(m["remove"], name+".remove"); err != nil {
		return listChange{}, false, err
	}
	if raw, present := m["clear"]; present && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return listChange{}, false, fmt.Errorf("%s.clear must be true or false", name)
		}
		c.clear = b
	}
	if c.clear && len(c.remove) > 0 {
		return listChange{}, false, fmt.Errorf("%s: give clear or remove, not both", name)
	}
	if !c.clear && len(c.add) == 0 && len(c.remove) == 0 {
		return listChange{}, false, fmt.Errorf("%s names no change: give add, remove or clear", name)
	}
	return c, true, nil
}

func namedStringList(raw any, name string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	var items []any
	switch v := raw.(type) {
	case []any:
		items = v
	case []string:
		for _, s := range v {
			items = append(items, s)
		}
	default:
		return nil, fmt.Errorf("%s must be a list of strings", name)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings", name)
		}
		if s = strings.TrimSpace(s); s == "" {
			return nil, fmt.Errorf("%s must not contain an empty entry", name)
		}
		out = append(out, s)
	}
	return out, nil
}

// fold functions decide when two entries are the same one.
func foldCase(s string) string  { return strings.ToLower(strings.TrimSpace(s)) }
func foldExact(s string) string { return strings.TrimSpace(s) }

func containsFold(list []string, s string, fold func(string) string) bool {
	for _, v := range list {
		if fold(v) == fold(s) {
			return true
		}
	}
	return false
}

// apply merges the change onto before, keeping every entry it does not name
// in its place. Removing an entry that is not there is refused, so a typo
// cannot pass for a removal.
func (c listChange) apply(field string, before []string, fold func(string) string) ([]string, error) {
	after := []string{}
	if !c.clear {
		after = append(after, before...)
	}
	for _, r := range c.remove {
		if !containsFold(after, r, fold) {
			return nil, fmt.Errorf("%s: cannot remove %q, it is not in the current list (%s)", field, r, listLabel(before))
		}
		kept := after[:0:0]
		for _, v := range after {
			if fold(v) != fold(r) {
				kept = append(kept, v)
			}
		}
		after = kept
	}
	for _, a := range c.add {
		if !containsFold(after, a, fold) {
			after = append(after, a)
		}
	}
	return after, nil
}

// listDiff returns the entries of after missing from before, and the entries
// of before missing from after.
func listDiff(before, after []string, fold func(string) string) (added, removed []string) {
	for _, v := range after {
		if !containsFold(before, v, fold) {
			added = append(added, v)
		}
	}
	for _, v := range before {
		if !containsFold(after, v, fold) {
			removed = append(removed, v)
		}
	}
	return added, removed
}

func sameList(a, b []string, fold func(string) string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if fold(a[i]) != fold(b[i]) {
			return false
		}
	}
	return true
}

func listLabel(list []string) string {
	if len(list) == 0 {
		return "none"
	}
	return strings.Join(list, ", ")
}

// orDefault is the list a defaulted setting uses: the list, or def when the
// list is empty.
func orDefault(list, def []string) []string {
	if len(list) == 0 {
		return append([]string(nil), def...)
	}
	return list
}

// settingChange is one before → after line of a preview.
type settingChange struct {
	Field  string `json:"field"`
	Before any    `json:"before"`
	After  any    `json:"after"`
	Note   string `json:"note,omitempty"`
}

func (c settingChange) effect() string {
	line := fmt.Sprintf("%s: %s → %s", c.Field, changeValueLabel(c.Before), changeValueLabel(c.After))
	if c.Note != "" {
		line += " (" + c.Note + ")"
	}
	return line
}

func changeValueLabel(v any) string {
	switch x := v.(type) {
	case []string:
		return "[" + listLabel(x) + "]"
	case bool:
		if x {
			return "on"
		}
		return "off"
	}
	return fmt.Sprint(v)
}

func changeDetails(changes []settingChange) []map[string]any {
	out := make([]map[string]any, 0, len(changes))
	for _, c := range changes {
		row := map[string]any{"field": c.Field, "before": c.Before, "after": c.After}
		if c.Note != "" {
			row["note"] = c.Note
		}
		out = append(out, row)
	}
	return out
}

// stateFingerprint is the digest a preview records of the settings it read.
func stateFingerprint(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// unreadSettingsPreview is the preview of an op that could not read the
// settings it merges onto: it names the endpoint and sends nothing.
func unreadSettingsPreview(op, target, path, what string) WritePreview {
	return WritePreview{
		Operation:           op,
		Summary:             "Change the " + what + " (current settings not read)",
		Target:              target,
		Request:             WriteRequest{Method: http.MethodPut, Path: path},
		WideningDisclosure:  "Potential widening: the current " + what + " were not read, so this preview cannot say what would change.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}
}

// confirmGuardFingerprint turns a fresh preview into the fingerprint
// ConfirmWrite compares. A fresh preview that fails (the change no longer
// applies, or the settings cannot be read) refuses the confirm.
func confirmGuardFingerprint(preview WritePreview, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the settings again before writing, and that check refused this change; preview it again: %w", err)
	}
	return preview.StateFingerprint, nil
}

// readGovernorSettings reads GET /api/config/governor (settings_read section
// governor), where both surfaces' current values live.
func readGovernorSettings(ctx context.Context, provider Provider, what string) (map[string]any, error) {
	if provider == nil {
		return nil, fmt.Errorf("the current %s could not be read, so the change cannot be merged onto it; nothing was previewed", what)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": SettingsSectionDefault})
	if err != nil {
		return nil, fmt.Errorf("the current %s could not be read (settings_read section %s): %v; nothing was previewed", what, SettingsSectionDefault, err)
	}
	m, _ := data.(map[string]any)
	if m == nil {
		return nil, fmt.Errorf("the current %s could not be read: settings_read section %s returned no object; nothing was previewed", what, SettingsSectionDefault)
	}
	return m, nil
}

// stringsField reads a list of strings from m[key]; null reads as empty. It
// reports false when the key is missing or not a list of strings.
func stringsField(m map[string]any, key string) ([]string, bool) {
	raw, present := m[key]
	if !present {
		return nil, false
	}
	if raw == nil {
		return []string{}, true
	}
	items, ok := raw.([]any)
	if !ok {
		if ss, ok := raw.([]string); ok {
			return append([]string{}, ss...), true
		}
		return nil, false
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func boolField(m map[string]any, key string) (bool, bool) {
	b, ok := m[key].(bool)
	return b, ok
}

func missingSetting(what, key string) error {
	return fmt.Errorf("the hive's settings do not include %s (%s), so the change cannot be merged onto it; nothing was previewed. The hive may be older than this operation", key, what)
}

// ---- project.issue_filter --------------------------------------------------

// hardSuppressBuckets maps each hard_suppress_labels bucket to the key the
// governor settings read uses and the bucket's built-in default label.
var hardSuppressBuckets = []struct{ key, readKey, def string }{
	{"needs_human", "needsHuman", config.DefaultNeedsHumanLabel},
	{"needs_direction", "needsDirection", config.DefaultNeedsDirectionLabel},
	{"needs_decision", "needsDecision", config.DefaultNeedsDecisionLabel},
	{"needs_spec", "needsSpec", config.DefaultNeedsSpecLabel},
}

// issueFilterState is the issue filter as the preview read it. The exported
// fields are what the confirm fingerprints; hubManaged is display only.
type issueFilterState struct {
	RequireLabels  []string            `json:"require_labels"`
	HardSuppress   map[string][]string `json:"hard_suppress_labels"`
	RTEnabled      bool                `json:"reporter_trust_enabled"`
	RTAssociations []string            `json:"reporter_trust_trusted_associations"`
	RTLogins       []string            `json:"reporter_trust_trusted_logins"`
	RTUntrusted    []string            `json:"reporter_trust_untrusted_require_labels"`
	hubManaged     bool
}

func readIssueFilterState(ctx context.Context, provider Provider) (issueFilterState, error) {
	const what = "issue filter"
	m, err := readGovernorSettings(ctx, provider, what)
	if err != nil {
		return issueFilterState{}, err
	}
	var st issueFilterState
	var ok bool
	if st.RequireLabels, ok = stringsField(m, "requireLabels"); !ok {
		return st, missingSetting(what, "requireLabels")
	}
	hs, _ := m["hardSuppressLabels"].(map[string]any)
	if hs == nil {
		return st, missingSetting(what, "hardSuppressLabels")
	}
	st.HardSuppress = map[string][]string{}
	for _, b := range hardSuppressBuckets {
		list, ok := stringsField(hs, b.readKey)
		if !ok {
			return st, missingSetting(what, "hardSuppressLabels."+b.readKey)
		}
		st.HardSuppress[b.key] = orDefault(list, []string{b.def})
	}
	rt, _ := m["reporterTrust"].(map[string]any)
	if rt == nil {
		return st, missingSetting(what, "reporterTrust")
	}
	if st.RTEnabled, ok = boolField(rt, "enabled"); !ok {
		return st, missingSetting(what, "reporterTrust.enabled")
	}
	for _, f := range []struct {
		key string
		dst *[]string
	}{
		{"trustedAssociations", &st.RTAssociations},
		{"trustedLogins", &st.RTLogins},
		{"untrustedRequireLabels", &st.RTUntrusted},
	} {
		list, ok := stringsField(rt, f.key)
		if !ok {
			return st, missingSetting(what, "reporterTrust."+f.key)
		}
		*f.dst = list
	}
	st.RTAssociations = orDefault(st.RTAssociations, config.DefaultTrustedAssociations)
	st.RTUntrusted = orDefault(st.RTUntrusted, config.DefaultUntrustedRequireLabels)
	if hub, _ := m["hub"].(map[string]any); hub != nil {
		if settings, _ := hub["settings"].(map[string]any); settings != nil {
			st.hubManaged, _ = boolField(settings, "hub_managed")
		}
	}
	return st, nil
}

// issueFilterRequest is the parsed op arguments.
type issueFilterRequest struct {
	require      *listChange
	hardSuppress map[string]listChange
	rtEnabled    *bool
	rtAssoc      *listChange
	rtLogins     *listChange
	rtUntrusted  *listChange
}

func parseIssueFilterArgs(args map[string]any) (issueFilterRequest, error) {
	var req issueFilterRequest
	if err := rejectUnknownKeys(args, "project.issue_filter", "require_labels", "hard_suppress_labels", "reporter_trust"); err != nil {
		return req, err
	}
	if c, ok, err := listChangeArg(args, "require_labels", "project.issue_filter"); err != nil {
		return req, err
	} else if ok {
		req.require = &c
	}
	hs, err := objectArg(args, "hard_suppress_labels", "project.issue_filter")
	if err != nil {
		return req, err
	}
	if hs != nil {
		keys := make([]string, 0, len(hardSuppressBuckets))
		for _, b := range hardSuppressBuckets {
			keys = append(keys, b.key)
		}
		if err := rejectUnknownKeys(hs, "project.issue_filter.hard_suppress_labels", keys...); err != nil {
			return req, err
		}
		req.hardSuppress = map[string]listChange{}
		for _, b := range hardSuppressBuckets {
			c, ok, err := listChangeArg(hs, b.key, "project.issue_filter.hard_suppress_labels")
			if err != nil {
				return req, err
			}
			if ok {
				req.hardSuppress[b.key] = c
			}
		}
		if len(req.hardSuppress) == 0 {
			return req, fmt.Errorf("project.issue_filter.hard_suppress_labels names no bucket")
		}
	}
	rt, err := objectArg(args, "reporter_trust", "project.issue_filter")
	if err != nil {
		return req, err
	}
	if rt != nil {
		const where = "project.issue_filter.reporter_trust"
		if err := rejectUnknownKeys(rt, where, "enabled", "trusted_associations", "trusted_logins", "untrusted_require_labels"); err != nil {
			return req, err
		}
		if raw, present := rt["enabled"]; present && raw != nil {
			b, ok := raw.(bool)
			if !ok {
				return req, fmt.Errorf("%s.enabled must be true or false", where)
			}
			req.rtEnabled = &b
		}
		for _, f := range []struct {
			key string
			dst **listChange
		}{
			{"trusted_associations", &req.rtAssoc},
			{"trusted_logins", &req.rtLogins},
			{"untrusted_require_labels", &req.rtUntrusted},
		} {
			c, ok, err := listChangeArg(rt, f.key, where)
			if err != nil {
				return req, err
			}
			if ok {
				*f.dst = &c
			}
		}
		if req.rtEnabled == nil && req.rtAssoc == nil && req.rtLogins == nil && req.rtUntrusted == nil {
			return req, fmt.Errorf("%s names no change", where)
		}
	}
	if req.require == nil && req.hardSuppress == nil && rt == nil {
		return req, fmt.Errorf("give at least one of require_labels, hard_suppress_labels or reporter_trust")
	}
	if req.rtAssoc != nil {
		for i, a := range req.rtAssoc.add {
			up := strings.ToUpper(a)
			if !containsFold(config.KnownAuthorAssociations, up, foldExact) {
				return req, fmt.Errorf("project.issue_filter.reporter_trust.trusted_associations: %q is not a GitHub author association (one of %s)", a, strings.Join(config.KnownAuthorAssociations, ", "))
			}
			req.rtAssoc.add[i] = up
		}
	}
	return req, nil
}

func (issueFilterOp) Name() string { return WriteOpProjectIssueFilter }
func (issueFilterOp) Description() string {
	return "Change the issue filter (project.issue_filter: require_labels, hard_suppress_labels buckets, and reporter_trust enabled, trusted_associations, trusted_logins and untrusted_require_labels) through PUT /api/config/governor/labels, the endpoint Settings → Labels saves through. Each list takes {add, remove, clear}: the preview reads the current lists, shows each before → after and sends the merged list, and write_confirm is refused if the filter changed since the preview. Owner only: the endpoint refuses any other role. On a hub-managed hive the hub may replace the whole filter on a later heartbeat."
}
func (issueFilterOp) InputSchema() map[string]any {
	buckets := map[string]any{}
	for _, b := range hardSuppressBuckets {
		buckets[b.key] = listChangeSchema("Labels that park an issue in the " + b.key + " bucket; an emptied bucket goes back to its default label " + b.def + ".")
	}
	return map[string]any{"type": "object", "properties": map[string]any{
		"require_labels":       listChangeSchema("Labels an issue must carry (at least one) before agents work it; an empty list turns the label gate off."),
		"hard_suppress_labels": map[string]any{"type": "object", "properties": buckets, "additionalProperties": false},
		"reporter_trust": map[string]any{"type": "object", "properties": map[string]any{
			"enabled":                  map[string]any{"type": "boolean"},
			"trusted_associations":     listChangeSchema("GitHub author associations whose issues need no triage label; an empty list goes back to OWNER, MEMBER, COLLABORATOR."),
			"trusted_logins":           listChangeSchema("Logins whose issues need no triage label."),
			"untrusted_require_labels": listChangeSchema("Labels that admit an untrusted reporter's issue; an empty list goes back to triage/accepted."),
		}, "additionalProperties": false},
	}, "additionalProperties": false}
}

// Preview without a provider checks the arguments and names the endpoint;
// write_preview always goes through PreviewWithProvider, and write_confirm
// uses this only to check the method and path.
func (issueFilterOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseIssueFilterArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpProjectIssueFilter, "project.issue_filter", "/api/config/governor/labels", "issue filter"), nil
}

// ConfirmGuard reads the filter again at write_confirm and returns its
// fingerprint, which must still match the preview's.
func (op issueFilterOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// PreviewWithProvider reads the current filter, merges the requested list
// changes onto it and discloses what the result admits.
func (issueFilterOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	req, err := parseIssueFilterArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	before, err := readIssueFilterState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	after := before
	after.HardSuppress = map[string][]string{}
	for k, v := range before.HardSuppress {
		after.HardSuppress[k] = v
	}
	body := map[string]any{}
	var changes []settingChange

	if req.require != nil {
		list, err := req.require.apply("require_labels", before.RequireLabels, foldCase)
		if err != nil {
			return WritePreview{}, err
		}
		after.RequireLabels = list
		body["require_labels"] = list
		if !sameList(before.RequireLabels, list, foldCase) {
			c := settingChange{Field: "require_labels", Before: before.RequireLabels, After: list}
			if len(list) == 0 {
				c.Note = "empty: the label gate is off"
			}
			changes = append(changes, c)
		}
	}
	if req.hardSuppress != nil {
		hsBody := map[string]any{}
		for _, b := range hardSuppressBuckets {
			c, ok := req.hardSuppress[b.key]
			if !ok {
				continue
			}
			field := "hard_suppress_labels." + b.key
			list, err := c.apply(field, before.HardSuppress[b.key], foldCase)
			if err != nil {
				return WritePreview{}, err
			}
			hsBody[b.key] = list
			effective := orDefault(list, []string{b.def})
			after.HardSuppress[b.key] = effective
			if !sameList(before.HardSuppress[b.key], effective, foldCase) {
				sc := settingChange{Field: field, Before: before.HardSuppress[b.key], After: effective}
				if len(list) == 0 {
					sc.Note = "emptied, so it goes back to the default " + b.def
				}
				changes = append(changes, sc)
			}
		}
		body["hard_suppress_labels"] = hsBody
	}
	if req.rtEnabled != nil || req.rtAssoc != nil || req.rtLogins != nil || req.rtUntrusted != nil {
		rtBody := map[string]any{}
		if req.rtEnabled != nil {
			after.RTEnabled = *req.rtEnabled
			rtBody["enabled"] = *req.rtEnabled
			if before.RTEnabled != after.RTEnabled {
				changes = append(changes, settingChange{Field: "reporter_trust.enabled", Before: before.RTEnabled, After: after.RTEnabled})
			}
		}
		for _, f := range []struct {
			field  string
			key    string
			change *listChange
			before []string
			def    []string
			dst    *[]string
		}{
			{"reporter_trust.trusted_associations", "trusted_associations", req.rtAssoc, before.RTAssociations, config.DefaultTrustedAssociations, &after.RTAssociations},
			{"reporter_trust.trusted_logins", "trusted_logins", req.rtLogins, before.RTLogins, nil, &after.RTLogins},
			{"reporter_trust.untrusted_require_labels", "untrusted_require_labels", req.rtUntrusted, before.RTUntrusted, config.DefaultUntrustedRequireLabels, &after.RTUntrusted},
		} {
			if f.change == nil {
				continue
			}
			list, err := f.change.apply(f.field, f.before, foldCase)
			if err != nil {
				return WritePreview{}, err
			}
			rtBody[f.key] = list
			effective := list
			if f.def != nil {
				effective = orDefault(list, f.def)
			}
			*f.dst = effective
			if !sameList(f.before, effective, foldCase) {
				sc := settingChange{Field: f.field, Before: f.before, After: effective}
				if f.def != nil && len(list) == 0 {
					sc.Note = "emptied, so it goes back to the default " + listLabel(f.def)
				}
				changes = append(changes, sc)
			}
		}
		body["reporter_trust"] = rtBody
	}
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("project.issue_filter changes nothing: the filter already reads that way")
	}
	fingerprint, err := stateFingerprint(before)
	if err != nil {
		return WritePreview{}, err
	}

	fields := make([]string, 0, len(changes))
	effects := make([]string, 0, len(changes)+4)
	for _, c := range changes {
		fields = append(fields, c.Field)
		effects = append(effects, c.effect())
	}
	effects = append(effects,
		"After this change: "+issueAdmissionRule(after),
		"Hive saves it through Settings → Labels (PUT /api/config/governor/labels), records a config_governor_labels audit entry, and applies it on the next issue scan, with no restart. Fields not named here keep their values.",
		"This write itself adds or removes no label and merges, approves and closes nothing.",
	)
	if before.hubManaged {
		effects = append(effects, hubManagedIssueFilterNote)
	}
	return WritePreview{
		Operation:           WriteOpProjectIssueFilter,
		Summary:             "Change the issue filter: " + strings.Join(fields, ", "),
		Target:              "project.issue_filter",
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/labels", Body: body},
		Effects:             effects,
		WideningDisclosure:  issueFilterDisclosure(before, after),
		ConfirmationMessage: "Confirm changing the issue filter: " + strings.Join(fields, ", ") + ".",
		Details:             map[string]any{"changes": changeDetails(changes), "hub_managed": before.hubManaged, "after_rule": issueAdmissionRule(after)},
		StateFingerprint:    fingerprint,
	}, nil
}

// hubManagedIssueFilterNote: on a hub-managed spoke, a hub record that
// carries an issue filter replaces the spoke's whole project.issue_filter on
// the next heartbeat (cmd/hive heartbeat reconcile). The spoke cannot see
// whether its record carries one.
const hubManagedIssueFilterNote = "Hub-managed: may be overwritten. This hive is managed by a hub. If the hub's record for this hive carries an issue filter, the next heartbeat replaces the whole project.issue_filter (require_labels, hard_suppress_labels and reporter_trust) with the hub's copy, undoing this change. The hive cannot see from here whether its hub record carries one; ask the hub operator."

// issueAdmissionRule states in plain words which issues the filter admits.
func issueAdmissionRule(st issueFilterState) string {
	trusted := "associations " + listLabel(st.RTAssociations)
	if len(st.RTLogins) > 0 {
		trusted += "; logins " + listLabel(st.RTLogins)
	}
	switch {
	case st.RTEnabled && len(st.RequireLabels) == 0:
		return "issues filed by trusted reporters (" + trusted + ") are worked with no label; issues filed by anyone else need one of [" + listLabel(st.RTUntrusted) + "] first. Issues filed by the hive or a bot are not judged by reporter trust."
	case st.RTEnabled:
		return "every issue needs one of [" + listLabel(st.RequireLabels) + "]; issues filed by people outside the trusted reporters (" + trusted + ") also need one of [" + listLabel(st.RTUntrusted) + "]."
	case len(st.RequireLabels) == 0:
		return "every open issue that is not held, exempt or hard-suppressed can be worked; no label is needed."
	default:
		return "only issues carrying one of [" + listLabel(st.RequireLabels) + "] can be worked."
	}
}

// issueFilterDisclosure names every way the change admits more issues
// without triage, and says so plainly when it admits none.
func issueFilterDisclosure(before, after issueFilterState) string {
	var widen, narrow []string
	reqAdded, reqRemoved := listDiff(before.RequireLabels, after.RequireLabels, foldCase)
	switch {
	case len(before.RequireLabels) > 0 && len(after.RequireLabels) == 0:
		widen = append(widen, "require_labels is cleared, so the label gate turns off: issues no longer need one of ["+listLabel(before.RequireLabels)+"] to be worked")
	case len(before.RequireLabels) == 0 && len(after.RequireLabels) > 0:
		narrow = append(narrow, "require_labels turns the label gate on: only issues carrying one of ["+listLabel(after.RequireLabels)+"] can be worked, and every other issue stops being picked up")
	default:
		if len(reqAdded) > 0 {
			widen = append(widen, "issues carrying "+listLabel(reqAdded)+" now pass require_labels too")
		}
		if len(reqRemoved) > 0 {
			narrow = append(narrow, "issues carrying only "+listLabel(reqRemoved)+" no longer pass require_labels")
		}
	}
	for _, b := range hardSuppressBuckets {
		added, removed := listDiff(before.HardSuppress[b.key], after.HardSuppress[b.key], foldCase)
		if len(removed) > 0 {
			widen = append(widen, "issues labelled "+listLabel(removed)+" are no longer parked out of the queue as "+b.key)
		}
		if len(added) > 0 {
			narrow = append(narrow, "issues labelled "+listLabel(added)+" are now parked as "+b.key)
		}
	}
	switch {
	case before.RTEnabled && !after.RTEnabled:
		widen = append(widen, "reporter trust is turned off: issues filed by untrusted reporters no longer need one of ["+listLabel(before.RTUntrusted)+"] before agents work them, and the reporter-trust merge hold stops applying unless github.reporter_trust_hold is set")
	case !before.RTEnabled && after.RTEnabled:
		narrow = append(narrow, "reporter trust is turned on: issues filed by people outside the trusted reporters now need one of ["+listLabel(after.RTUntrusted)+"] before agents work them; trusted reporters' issues still pass require_labels as before. Later scans mark each held issue filed by a person as waiting for triage, with a label and a one-time comment (reporter_trust.comment, on by default). Unless github.reporter_trust_hold is set, the reporter-trust merge hold turns on with it")
	}
	gate := ""
	if !after.RTEnabled {
		gate = " (once reporter trust is on)"
	}
	if added, _ := listDiff(before.RTLogins, after.RTLogins, foldCase); len(added) > 0 {
		line := "issues filed by " + listLabel(added) + " skip the reporter-trust triage label" + gate
		if bots := botLogins(added); len(bots) > 0 {
			line += "; " + listLabel(bots) + " look like bot accounts, and reporter trust judges only issues filed by people, so for those logins this changes neither admission nor the merge hold, only the other checks that ask whether a reporter is trusted (such as the park re-check)"
		}
		widen = append(widen, line)
	}
	if added, _ := listDiff(before.RTAssociations, after.RTAssociations, foldCase); len(added) > 0 {
		widen = append(widen, "issues filed by reporters GitHub marks "+listLabel(added)+" skip the reporter-trust triage label"+gate)
	}
	if added, _ := listDiff(before.RTUntrusted, after.RTUntrusted, foldCase); len(added) > 0 {
		widen = append(widen, "an untrusted reporter's issue carrying "+listLabel(added)+" is admitted too"+gate)
	}
	if _, removed := listDiff(before.RTLogins, after.RTLogins, foldCase); len(removed) > 0 {
		narrow = append(narrow, "issues filed by "+listLabel(removed)+" need the triage label again"+gate)
	}
	narrowNote := ""
	if len(narrow) > 0 {
		narrowNote = " Narrowing: " + strings.Join(narrow, "; ") + "."
	}
	if len(widen) > 0 {
		return "Widening: more issues admitted without triage. " + capitalize(strings.Join(widen, "; ")) + "." + narrowNote
	}
	return "No widening: this change admits no issue that is not admitted today." + narrowNote
}

func botLogins(logins []string) []string {
	var out []string
	for _, l := range logins {
		if strings.HasSuffix(strings.ToLower(strings.TrimSpace(l)), "[bot]") {
			out = append(out, l)
		}
	}
	return out
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// ---- sentinel.settings -----------------------------------------------------

// sentinelState is Sentinel as the preview read it. The exported fields are
// what the confirm fingerprints.
type sentinelState struct {
	SensitivePaths        []string `json:"sensitive_paths"`
	SensitivePathsDefault bool     `json:"sensitive_paths_default"`
	DisabledBehaviors     []string `json:"disabled_behaviors"`
	ExemptLogins          []string `json:"exempt_logins"`
	TrustedAuthorsBlock   bool     `json:"trusted_authors_block"`
	// RepoOverrides are the repositories whose own list
	// (repo_policies[].sentinel_sensitive_paths) replaces sensitive_paths. A
	// change to sensitive_paths does not reach them, and an override added,
	// changed or removed after the preview refuses the confirm.
	RepoOverrides []sentinelRepoPathsOverride `json:"repo_sensitive_paths"`
	label         string
	defaultPaths  []string
	// repoPathsSettable is whether the hive takes repoSensitivePaths on the
	// security PUT; projectRepos are its watched owner/repo names.
	repoPathsSettable bool
	projectRepos      []string
}

// sentinelRepoPathsOverride is one repository's own Sentinel sensitive-path
// list, as Settings → Security reports it.
type sentinelRepoPathsOverride struct {
	Repo           string   `json:"repo"`
	SensitivePaths []string `json:"sensitive_paths"`
}

// readSentinelRepoOverrides parses security.sentinel.repoSensitivePaths. A
// hive older than per-repo overrides sends none, which reads as no override.
func readSentinelRepoOverrides(raw any) []sentinelRepoPathsOverride {
	items, _ := raw.([]any)
	var out []sentinelRepoPathsOverride
	for _, item := range items {
		m, _ := item.(map[string]any)
		repo, _ := m["repo"].(string)
		paths, ok := stringsField(m, "sensitivePaths")
		if strings.TrimSpace(repo) == "" || !ok {
			continue
		}
		out = append(out, sentinelRepoPathsOverride{Repo: repo, SensitivePaths: paths})
	}
	return out
}

// sentinelOverrideRepos names the repositories with their own list.
func sentinelOverrideRepos(overrides []sentinelRepoPathsOverride) []string {
	out := make([]string, 0, len(overrides))
	for _, o := range overrides {
		out = append(out, o.Repo)
	}
	return out
}

func readSentinelState(ctx context.Context, provider Provider) (sentinelState, error) {
	const what = "Sentinel settings"
	m, err := readGovernorSettings(ctx, provider, what)
	if err != nil {
		return sentinelState{}, err
	}
	security, _ := m["security"].(map[string]any)
	if security == nil {
		return sentinelState{}, missingSetting(what, "security")
	}
	sc, _ := security["sentinel"].(map[string]any)
	if sc == nil {
		return sentinelState{}, missingSetting(what, "security.sentinel")
	}
	var st sentinelState
	var ok bool
	if st.SensitivePaths, ok = stringsField(sc, "sensitivePaths"); !ok {
		return st, missingSetting(what, "security.sentinel.sensitivePaths")
	}
	st.SensitivePathsDefault, _ = boolField(sc, "sensitivePathsDefault")
	if st.defaultPaths, ok = stringsField(sc, "defaultSensitivePaths"); !ok || len(st.defaultPaths) == 0 {
		st.defaultPaths = config.DefaultSentinelSensitivePaths()
	}
	if st.ExemptLogins, ok = stringsField(sc, "exemptLogins"); !ok {
		return st, missingSetting(what, "security.sentinel.exemptLogins")
	}
	if st.TrustedAuthorsBlock, ok = boolField(sc, "trustedAuthorsBlock"); !ok {
		return st, missingSetting(what, "security.sentinel.trustedAuthorsBlock")
	}
	behaviors, ok := sc["behaviors"].([]any)
	if !ok {
		return st, missingSetting(what, "security.sentinel.behaviors")
	}
	st.DisabledBehaviors = []string{}
	for _, raw := range behaviors {
		b, _ := raw.(map[string]any)
		name, _ := b["name"].(string)
		enabled, known := boolField(b, "enabled")
		if name == "" || !known {
			return st, missingSetting(what, "security.sentinel.behaviors[].name/enabled")
		}
		if !enabled {
			st.DisabledBehaviors = append(st.DisabledBehaviors, name)
		}
	}
	st.RepoOverrides = readSentinelRepoOverrides(sc["repoSensitivePaths"])
	st.repoPathsSettable, _ = boolField(sc, "repoSensitivePathsSettable")
	st.projectRepos = stringListValueLoose(m["repos"])
	st.label, _ = sc["label"].(string)
	if strings.TrimSpace(st.label) == "" {
		st.label = config.DefaultSentinelLabel
	}
	return st, nil
}

// sentinelBehaviorDescriptions maps each Sentinel rule to its one-line
// description.
func sentinelBehaviorDescriptions() (map[string]string, []string) {
	out := map[string]string{}
	var names []string
	for _, b := range config.SentinelBehaviors() {
		out[b.Name] = b.Description
		names = append(names, b.Name)
	}
	return out, names
}

func (sentinelSettingsOp) Name() string { return WriteOpSentinelSettings }
func (sentinelSettingsOp) Description() string {
	return "Change Sentinel's sensitive_paths, disabled_behaviors, exempt_logins and trusted_authors_block through PUT /api/config/governor/security, the endpoint Settings → Security saves through. Each list takes {add, remove, clear}: the preview reads the current lists, shows each before → after and sends the merged list, and write_confirm is refused if the settings changed since the preview. A change that flags fewer pull requests is disclosed as widening and names which pull requests would no longer be held. A repository with its own list (project.repo_policies[].sentinel_sensitive_paths) keeps it when sensitive_paths changes: the preview names those repositories and what their lists leave out. repo_sensitive_paths sets or clears one repository's own list: {\"owner/repo\": [every glob for it]}, [] to flag nothing by path there, or null to use the hive-wide list again; only the named repositories change, and a list that flags fewer paths in a repository is disclosed as widening. Owner only: the endpoint refuses any other role. It removes no label from any pull request."
}
func (sentinelSettingsOp) InputSchema() map[string]any {
	_, names := sentinelBehaviorDescriptions()
	return map[string]any{"type": "object", "properties": map[string]any{
		"sensitive_paths":       listChangeSchema("Glob patterns whose change always raises the sensitive_path finding. An emptied list goes back to the shipped defaults; to stop path checks, disable the sensitive_path behavior instead."),
		"disabled_behaviors":    listChangeSchema("Sentinel rules switched off (add disables a rule, remove turns it back on). Known rules: " + strings.Join(names, ", ") + "."),
		"exempt_logins":         listChangeSchema("Pull request authors Sentinel never flags (exact login, case-insensitive)."),
		"trusted_authors_block": map[string]any{"type": "boolean", "description": "Whether a finding on a trusted author's pull request (the Hive App, configured trusted bots, authorized owners) gets the blocking label; off posts an informational notice only."},
		"repo_sensitive_paths":  sentinelRepoPathsSchema(),
	}, "additionalProperties": false}
}

// Preview without a provider checks the arguments and names the endpoint;
// see issueFilterOp.Preview.
func (sentinelSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseSentinelArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpSentinelSettings, "sentinel", "/api/config/governor/security", "Sentinel settings"), nil
}

// ConfirmGuard reads the Sentinel settings again at write_confirm and returns
// their fingerprint, which must still match the preview's.
func (op sentinelSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// sentinelRequest is the parsed sentinel.settings arguments.
type sentinelRequest struct {
	paths, behaviors, exempt          listChange
	hasPaths, hasBehaviors, hasExempt bool
	block                             *bool
	// repoPaths are the per-repository lists to set (nil value: clear).
	repoPaths map[string]*[]string
}

func parseSentinelArgs(args map[string]any) (sentinelRequest, error) {
	const where = "sentinel.settings"
	var req sentinelRequest
	if err := rejectUnknownKeys(args, where, "sensitive_paths", "disabled_behaviors", "exempt_logins", "trusted_authors_block", "repo_sensitive_paths"); err != nil {
		return req, err
	}
	var err error
	if req.paths, req.hasPaths, err = listChangeArg(args, "sensitive_paths", where); err != nil {
		return req, err
	}
	if req.behaviors, req.hasBehaviors, err = listChangeArg(args, "disabled_behaviors", where); err != nil {
		return req, err
	}
	if req.exempt, req.hasExempt, err = listChangeArg(args, "exempt_logins", where); err != nil {
		return req, err
	}
	if raw, present := args["trusted_authors_block"]; present && raw != nil {
		b, ok := raw.(bool)
		if !ok {
			return req, fmt.Errorf("%s.trusted_authors_block must be true or false", where)
		}
		req.block = &b
	}
	if req.repoPaths, err = parseSentinelRepoPathsArg(args, where); err != nil {
		return req, err
	}
	if !req.hasPaths && !req.hasBehaviors && !req.hasExempt && req.block == nil && req.repoPaths == nil {
		return req, fmt.Errorf("give at least one of sensitive_paths, disabled_behaviors, exempt_logins, trusted_authors_block or repo_sensitive_paths")
	}
	if req.hasBehaviors {
		descriptions, names := sentinelBehaviorDescriptions()
		for _, list := range []*[]string{&req.behaviors.add, &req.behaviors.remove} {
			for i, name := range *list {
				lower := strings.ToLower(name)
				if _, ok := descriptions[lower]; !ok {
					return req, fmt.Errorf("%s.disabled_behaviors: unknown behavior %q (known: %s)", where, name, strings.Join(names, ", "))
				}
				(*list)[i] = lower
			}
		}
	}
	return req, nil
}

// PreviewWithProvider reads the current Sentinel settings, merges the
// requested list changes onto them and names which pull requests the result
// would stop holding.
func (sentinelSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	req, err := parseSentinelArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	paths, hasPaths := req.paths, req.hasPaths
	behaviors, hasBehaviors := req.behaviors, req.hasBehaviors
	exempt, hasExempt := req.exempt, req.hasExempt
	block := req.block
	descriptions, _ := sentinelBehaviorDescriptions()

	before, err := readSentinelState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	after := before
	sentinelBody := map[string]any{}
	var changes []settingChange
	var notes []string

	if req.repoPaths != nil {
		repoChanges, repoNotes, err := applySentinelRepoPaths(&after, before, req.repoPaths, sentinelBody)
		if err != nil {
			return WritePreview{}, err
		}
		changes = append(changes, repoChanges...)
		notes = append(notes, repoNotes...)
	}
	if hasPaths {
		list, err := paths.apply("sensitive_paths", before.SensitivePaths, foldExact)
		if err != nil {
			return WritePreview{}, err
		}
		sentinelBody["sensitivePaths"] = list
		effective := list
		after.SensitivePathsDefault = false
		if len(list) == 0 {
			// The endpoint stores an empty list as unset, which is the
			// shipped defaults.
			effective = append([]string(nil), before.defaultPaths...)
			after.SensitivePathsDefault = true
			notes = append(notes, fmt.Sprintf("An emptied sensitive_paths list goes back to the shipped defaults (%d globs). To stop path checks, disable the sensitive_path behavior instead.", len(before.defaultPaths)))
		} else if before.SensitivePathsDefault {
			notes = append(notes, "sensitive_paths was following the shipped defaults; it is now saved as this explicit list, so globs a later Hive release adds to the defaults will not apply to this hive.")
		}
		if len(after.RepoOverrides) > 0 {
			notes = append(notes, "Not changed by this write: "+listLabel(sentinelOverrideRepos(after.RepoOverrides))+" use their own sensitive-path list (hive.yaml repo_policies[].sentinel_sensitive_paths), so the hive-wide sensitive_paths does not apply to them.")
		}
		after.SensitivePaths = effective
		if !sameList(before.SensitivePaths, effective, foldExact) || before.SensitivePathsDefault != after.SensitivePathsDefault {
			changes = append(changes, settingChange{Field: "sentinel.sensitive_paths", Before: before.SensitivePaths, After: effective})
		}
	}
	if hasBehaviors {
		list, err := behaviors.apply("disabled_behaviors", before.DisabledBehaviors, foldCase)
		if err != nil {
			return WritePreview{}, err
		}
		sentinelBody["disabledBehaviors"] = list
		after.DisabledBehaviors = list
		if !sameList(before.DisabledBehaviors, list, foldCase) {
			changes = append(changes, settingChange{Field: "sentinel.disabled_behaviors", Before: before.DisabledBehaviors, After: list})
		}
	}
	if hasExempt {
		list, err := exempt.apply("exempt_logins", before.ExemptLogins, foldCase)
		if err != nil {
			return WritePreview{}, err
		}
		sentinelBody["exemptLogins"] = list
		after.ExemptLogins = list
		if !sameList(before.ExemptLogins, list, foldCase) {
			changes = append(changes, settingChange{Field: "sentinel.exempt_logins", Before: before.ExemptLogins, After: list})
		}
	}
	if block != nil {
		sentinelBody["trustedAuthorsBlock"] = *block
		after.TrustedAuthorsBlock = *block
		if before.TrustedAuthorsBlock != *block {
			changes = append(changes, settingChange{Field: "sentinel.trusted_authors_block", Before: before.TrustedAuthorsBlock, After: *block})
		}
	}
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("sentinel.settings changes nothing: Sentinel already reads that way")
	}
	fingerprint, err := stateFingerprint(before)
	if err != nil {
		return WritePreview{}, err
	}
	fields := make([]string, 0, len(changes))
	effects := make([]string, 0, len(changes)+len(notes)+2)
	for _, c := range changes {
		fields = append(fields, c.Field)
		effects = append(effects, c.effect())
	}
	effects = append(effects, notes...)
	effects = append(effects,
		"Hive saves it through Settings → Security (PUT /api/config/governor/security), records a config_governor_security audit entry, and Sentinel's next evaluation uses it, with no restart. Fields not named here keep their values.",
		"This write removes no label: a pull request already carrying "+before.label+" keeps it until a maintainer removes it. It merges, approves and closes nothing.",
	)
	return WritePreview{
		Operation:           WriteOpSentinelSettings,
		Summary:             "Change Sentinel settings: " + strings.Join(fields, ", "),
		Target:              "sentinel",
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/security", Body: map[string]any{"sentinel": sentinelBody}},
		Effects:             effects,
		WideningDisclosure:  sentinelDisclosure(before, after, descriptions),
		ConfirmationMessage: "Confirm changing Sentinel settings: " + strings.Join(fields, ", ") + ".",
		Details:             map[string]any{"changes": changeDetails(changes)},
		StateFingerprint:    fingerprint,
	}, nil
}

// sentinelDisclosure names, for every way the change makes Sentinel flag
// fewer pull requests, which pull requests would no longer be held.
func sentinelDisclosure(before, after sentinelState, descriptions map[string]string) string {
	var fewer, more []string
	pathsAdded, pathsRemoved := listDiff(before.SensitivePaths, after.SensitivePaths, foldExact)
	where := ""
	if len(after.RepoOverrides) > 0 {
		where = " (except in " + listLabel(sentinelOverrideRepos(after.RepoOverrides)) + ", which keep their own list)"
	}
	repoFewer, repoMore := sentinelRepoPathsDisclosure(before, after)
	fewer = append(fewer, repoFewer...)
	more = append(more, repoMore...)
	if len(pathsRemoved) > 0 {
		fewer = append(fewer, "a pull request"+where+" whose only sensitive-path match is "+listLabel(pathsRemoved)+" is no longer flagged by the sensitive_path rule (other rules still look at it)")
	}
	if len(pathsAdded) > 0 {
		more = append(more, "pull requests"+where+" touching "+listLabel(pathsAdded)+" are flagged by the sensitive_path rule")
	}
	disabled, enabled := listDiff(before.DisabledBehaviors, after.DisabledBehaviors, foldCase)
	for _, name := range disabled {
		fewer = append(fewer, "the "+name+" rule is switched off, so a pull request that only trips it (\""+descriptions[strings.ToLower(name)]+"\") is no longer flagged")
	}
	if len(enabled) > 0 {
		more = append(more, "the "+listLabel(enabled)+" rule(s) run again")
	}
	exemptAdded, exemptRemoved := listDiff(before.ExemptLogins, after.ExemptLogins, foldCase)
	if len(exemptAdded) > 0 {
		fewer = append(fewer, "pull requests opened by "+listLabel(exemptAdded)+" are never flagged, whatever they change")
	}
	if len(exemptRemoved) > 0 {
		more = append(more, "pull requests opened by "+listLabel(exemptRemoved)+" are evaluated again")
	}
	if before.TrustedAuthorsBlock && !after.TrustedAuthorsBlock {
		fewer = append(fewer, "a finding on a pull request from a trusted author (the Hive App, configured trusted bots and authorized owners) gets an informational notice only, with no "+before.label+" label")
	}
	if !before.TrustedAuthorsBlock && after.TrustedAuthorsBlock {
		more = append(more, "findings on trusted authors' pull requests get the blocking label")
	}
	moreNote := ""
	if len(more) > 0 {
		moreNote = " Also: " + strings.Join(more, "; ") + "."
	}
	moreNote += sentinelRepoOverrideStanding(sentinelRepoOverridesUntouched(before, after), after.SensitivePaths)
	if len(fewer) > 0 {
		return "Widening: fewer pull requests flagged by Sentinel, which narrows its protection. " + capitalize(strings.Join(fewer, "; ")) + ". The " + before.label + " label holds a pull request from Hive approval and every auto-merge lane until a maintainer removes it, so these pull requests would no longer be held for a maintainer." + moreNote
	}
	return "No widening: Sentinel flags every pull request it flags today." + moreNote
}

// sentinelRepoOverrideStanding names, for each repository with its own
// sensitive-path list, the hive-wide globs that list leaves out: a widening
// already in force there, which this write does not change.
func sentinelRepoOverrideStanding(overrides []sentinelRepoPathsOverride, hiveWide []string) string {
	var parts []string
	for _, o := range overrides {
		_, leftOut := listDiff(hiveWide, o.SensitivePaths, foldExact)
		if len(leftOut) > 0 {
			parts = append(parts, o.Repo+" does not flag "+listLabel(leftOut))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " Already in force and not changed by this write (per-repository lists): " + strings.Join(parts, "; ") + "."
}
