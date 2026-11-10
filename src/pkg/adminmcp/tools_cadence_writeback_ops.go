package adminmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Write operations for two settings an owner changes on the dashboard that
// had no admin MCP operation: one agent's per-mode cadences
// (governor.modes.<mode>.<agent>, the agent's Cadences settings) and the
// write-back gate's per-kind overrides (writeback.kinds.<kind>, Settings →
// Features). Each wraps exactly the endpoint the dashboard saves through, so
// the endpoint's owner-only check, validation and audit entry apply unchanged.
//
// Both previews read the current values first, show each change before and
// after, and record a fingerprint of what they read
// (WritePreview.StateFingerprint); ConfirmGuard reads again at write_confirm,
// which refuses with ErrConfirmationStale when it no longer matches.
//
// The Features endpoint replaces the whole writebackKinds map it is sent, so
// writeback.kind_policy sends the current overrides with the change merged in:
// kinds the caller does not name keep their override.
const (
	WriteOpAgentCadences       = "agent.cadences"
	WriteOpWritebackKindPolicy = "writeback.kind_policy"
)

func cadenceWritebackWriteOps() []WriteOp {
	return []WriteOp{agentCadencesOp{}, writebackKindPolicyOp{}}
}

type agentCadencesOp struct{}
type writebackKindPolicyOp struct{}

// ---- agent.cadences ---------------------------------------------------------

// agentCadenceModes are the governor modes an agent cadence is set for, in
// ladder order.
var agentCadenceModes = []string{"surge", "busy", "quiet", "idle"}

// agentCadenceRequest is one requested cadence: seconds between kicks, 0 for
// pause.
type agentCadenceRequest struct {
	seconds int64
	label   string
}

// agentCadenceSecondsLabel spells seconds the way the cadence endpoint stores
// them (formatCadenceDuration in pkg/dashboard).
func agentCadenceSecondsLabel(seconds int64) string {
	switch {
	case seconds <= 0:
		return "pause"
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	case seconds%60 == 0:
		return fmt.Sprintf("%dm", seconds/60)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}

func parseAgentCadenceRequest(raw any, field string) (agentCadenceRequest, error) {
	s, ok := raw.(string)
	if !ok {
		return agentCadenceRequest{}, fmt.Errorf("%s must be a duration such as 1h or 90m, or pause", field)
	}
	s = strings.ToLower(strings.TrimSpace(s))
	var req agentCadenceRequest
	switch s {
	case "pause", "paused", "off":
		req = agentCadenceRequest{seconds: 0, label: "pause"}
	default:
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return agentCadenceRequest{}, fmt.Errorf("%s: %q is not a positive duration such as 1h or 90m, or pause", field, s)
		}
		if d%time.Second != 0 {
			return agentCadenceRequest{}, fmt.Errorf("%s: %q must be whole seconds", field, s)
		}
		secs := int64(d / time.Second)
		req = agentCadenceRequest{seconds: secs, label: agentCadenceSecondsLabel(secs)}
	}
	// The endpoint's own check (Cadence.Validate), run on the value it will
	// store.
	if err := config.Cadence(req.label).Validate(); err != nil {
		return agentCadenceRequest{}, fmt.Errorf("%s: %v", field, err)
	}
	return req, nil
}

// parseAgentCadencesArgs returns the agent and the requested cadence per mode.
func parseAgentCadencesArgs(args map[string]any) (string, map[string]agentCadenceRequest, error) {
	const where = WriteOpAgentCadences
	if err := rejectUnknownKeys(args, where, "agent", "modes"); err != nil {
		return "", nil, err
	}
	agent, err := requiredString(args, "agent")
	if err != nil {
		return "", nil, err
	}
	modes, err := objectArg(args, "modes", where)
	if err != nil {
		return "", nil, err
	}
	if len(modes) == 0 {
		return "", nil, fmt.Errorf("%s.modes names no mode: give one or more of %s", where, strings.Join(agentCadenceModes, ", "))
	}
	if err := rejectUnknownKeys(modes, where+".modes", agentCadenceModes...); err != nil {
		return "", nil, err
	}
	out := map[string]agentCadenceRequest{}
	for _, mode := range agentCadenceModes {
		raw, present := modes[mode]
		if !present {
			continue
		}
		req, err := parseAgentCadenceRequest(raw, where+".modes."+mode)
		if err != nil {
			return "", nil, err
		}
		out[mode] = req
	}
	return agent, out, nil
}

// agentCadenceCell is one mode's cadence as the agent's settings report it.
type agentCadenceCell struct {
	kind    string // unset, interval, continuous, schedule
	seconds int64  // interval only; 0 is paused
	label   string
}

func readAgentCadenceCell(raw any, present bool) agentCadenceCell {
	if !present {
		return agentCadenceCell{kind: "unset", label: "not set"}
	}
	if n, ok := numberArg(raw); ok {
		return agentCadenceCell{kind: "interval", seconds: int64(n), label: agentCadenceSecondsLabel(int64(n))}
	}
	if s, ok := raw.(string); ok {
		if strings.EqualFold(strings.TrimSpace(s), config.CadenceModeContinuous) {
			return agentCadenceCell{kind: "continuous", label: config.CadenceModeContinuous}
		}
		return agentCadenceCell{kind: "schedule", label: s}
	}
	data, _ := json.Marshal(raw)
	return agentCadenceCell{kind: "schedule", label: "schedule " + string(data)}
}

// agentCadenceState is what the preview read. The exported fields are what
// the confirm fingerprints.
type agentCadenceState struct {
	Cadences map[string]any `json:"cadences"`
	Modes    []string       `json:"modes"`
}

// readAgentCadenceState reads the agent's settings (settings_read section
// agent, GET /api/config/agent/{name}): its cadence per mode, and the modes
// this hive configures.
func readAgentCadenceState(ctx context.Context, provider Provider, agent string) (agentCadenceState, error) {
	if provider == nil {
		return agentCadenceState{}, fmt.Errorf("%s reads agent %s's current cadences first, and this connection cannot read them; nothing was previewed", WriteOpAgentCadences, agent)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": "agent", "agent": agent})
	if err != nil {
		return agentCadenceState{}, fmt.Errorf("%s could not read agent %s's settings (settings_read section agent): %w; nothing was previewed", WriteOpAgentCadences, agent, err)
	}
	m, _ := data.(map[string]any)
	if m == nil {
		return agentCadenceState{}, fmt.Errorf("%s could not read agent %s's settings: settings_read section agent returned no object; nothing was previewed", WriteOpAgentCadences, agent)
	}
	raw, present := m["cadences"]
	if !present {
		return agentCadenceState{}, missingSetting("agent "+agent+"'s cadences", "cadences")
	}
	st := agentCadenceState{Cadences: map[string]any{}}
	if cad, ok := raw.(map[string]any); ok {
		st.Cadences = cad
	} else if raw != nil {
		return agentCadenceState{}, missingSetting("agent "+agent+"'s cadences", "cadences")
	}
	// The endpoint skips a mode the hive does not configure, so a mode this
	// hive lacks is refused here instead of being silently dropped. models
	// carries one key per configured mode.
	models, ok := m["models"].(map[string]any)
	if !ok {
		return agentCadenceState{}, missingSetting("the hive's governor modes", "models")
	}
	for mode := range models {
		st.Modes = append(st.Modes, mode)
	}
	sort.Strings(st.Modes)
	return st, nil
}

func (agentCadencesOp) Name() string { return WriteOpAgentCadences }
func (agentCadencesOp) Description() string {
	return "Set one agent's cadence in one or more governor modes (governor.modes.<mode>.<agent>: surge, busy, quiet, idle) through PUT /api/config/agent/{name}/cadences, the endpoint the agent's Cadences settings save through. Each mode takes a duration such as 1h or 90m, or pause; modes you do not name keep their cadence. The preview reads the current cadences, shows each mode before → after, and says when the agent would run more often (more spend). Each mode set becomes operator-owned, as a dashboard edit does, so a later ACMM level change or pack apply no longer resets it. write_confirm is refused if the agent's cadences changed since the preview. Owner only: the endpoint refuses any other role. Time-of-day and cron schedules stay on the dashboard."
}
func (agentCadencesOp) InputSchema() map[string]any {
	modes := map[string]any{}
	for _, m := range agentCadenceModes {
		modes[m] = map[string]any{"type": "string", "description": "Time between kicks in " + m + " mode: a duration such as 15m, 1h or 90m, or pause."}
	}
	return map[string]any{"type": "object", "properties": map[string]any{
		"agent": map[string]any{"type": "string", "minLength": 1},
		"modes": map[string]any{"type": "object", "properties": modes, "additionalProperties": false},
	}, "required": []string{"agent", "modes"}, "additionalProperties": false}
}

func agentCadencesPath(agent string) string {
	return "/api/config/agent/" + url.PathEscape(agent) + "/cadences"
}

// Preview without a provider checks the arguments and names the endpoint;
// write_preview always goes through PreviewWithProvider, and write_confirm
// uses this only to check the method and path.
func (agentCadencesOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, _, err := parseAgentCadencesArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpAgentCadences, agent, agentCadencesPath(agent), "cadences of agent "+agent), nil
}

// ConfirmGuard reads the agent's cadences again at write_confirm and returns
// their fingerprint, which must still match the preview's.
func (op agentCadencesOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// PreviewWithProvider reads the agent's cadences, compares each named mode
// and discloses every mode where the agent would run more often.
func (agentCadencesOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	agent, req, err := parseAgentCadencesArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	st, err := readAgentCadenceState(ctx, provider, agent)
	if err != nil {
		return WritePreview{}, err
	}
	configured := map[string]bool{}
	for _, m := range st.Modes {
		configured[m] = true
	}
	body := map[string]any{}
	var changes []settingChange
	var widen, narrow, owned []string
	for _, mode := range agentCadenceModes {
		want, ok := req[mode]
		if !ok {
			continue
		}
		if !configured[mode] {
			return WritePreview{}, fmt.Errorf("%s: this hive configures no %s mode (configured: %s), so the endpoint would skip it; nothing was previewed", WriteOpAgentCadences, mode, listLabel(st.Modes))
		}
		raw, present := st.Cadences[mode]
		before := readAgentCadenceCell(raw, present)
		if before.kind == "interval" && before.seconds == want.seconds {
			continue
		}
		body[mode] = want.seconds
		owned = append(owned, mode)
		c := settingChange{Field: mode, Before: before.label, After: want.label}
		switch {
		case want.seconds == 0:
			c.Note = "paused: the governor no longer kicks " + agent + " on a cadence in " + mode
			narrow = append(narrow, agent+" stops running on a cadence in "+mode)
		case before.kind == "interval" && before.seconds == 0:
			widen = append(widen, agent+" runs more often in "+mode+": more spend (every "+want.label+", was paused)")
		case before.kind == "interval" && before.seconds > want.seconds:
			widen = append(widen, agent+" runs more often in "+mode+": more spend (every "+want.label+", was every "+before.label+")")
		case before.kind == "interval":
			narrow = append(narrow, agent+" runs less often in "+mode+" (every "+want.label+", was every "+before.label+")")
		case before.kind == "continuous":
			narrow = append(narrow, agent+" runs every "+want.label+" in "+mode+" instead of continuously")
		case before.kind == "unset":
			widen = append(widen, agent+" runs more often in "+mode+": more spend (it had no cadence of its own there and now runs every "+want.label+")")
		default:
			widen = append(widen, agent+" runs more often in "+mode+": more spend (every "+want.label+" replaces "+before.label+", which may have run less often)")
		}
		changes = append(changes, c)
	}
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("%s changes nothing: agent %s's cadences already read that way", WriteOpAgentCadences, agent)
	}
	fingerprint, err := stateFingerprint(st)
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
		"Hive saves it through agent "+agent+"'s Cadences settings (PUT "+agentCadencesPath(agent)+"), the same save the dashboard makes, and records a config_agent_cadences audit entry. Modes not named here keep their cadence.",
		"Each mode set here becomes operator-owned for "+agent+" (governor.cadence_owners.<mode>."+agent+": operator), as a dashboard edit does: a later ACMM level change (fleet.autonomy_level) or pack apply no longer resets "+listLabel(owned)+" to the pack default. The hive does not report whether these cells were operator-owned already.",
		"This write itself kicks, merges, approves and labels nothing.",
	)
	narrowNote := ""
	if len(narrow) > 0 {
		narrowNote = " Narrowing: " + strings.Join(narrow, "; ") + "."
	}
	disclosure := "No widening: " + agent + " runs no more often in any mode." + narrowNote
	if len(widen) > 0 {
		disclosure = "Widening: " + strings.Join(widen, "; ") + "." + narrowNote
	}
	return WritePreview{
		Operation:           WriteOpAgentCadences,
		Summary:             "Change agent " + agent + "'s cadence in " + strings.Join(fields, ", "),
		Target:              agent,
		Request:             WriteRequest{Method: http.MethodPut, Path: agentCadencesPath(agent), Body: body},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing agent " + agent + "'s cadence in " + strings.Join(fields, ", ") + ".",
		Details:             map[string]any{"agent": agent, "changes": changeDetails(changes), "operator_owned_after": owned},
		StateFingerprint:    fingerprint,
	}, nil
}

// ---- writeback.kind_policy -------------------------------------------------

// writebackKindFields are the fields of writeback.kinds.<kind>.
var writebackKindFields = []string{"min_level", "require_label", "unlabeled_min_level", "own_only"}

// writebackKindChange is one kind's requested change: remove the whole
// override, or set and unset single fields.
type writebackKindChange struct {
	remove bool
	set    map[string]any // field → int or bool
	unset  []string
}

func writebackKindNames() []string {
	kinds := writeback.Kinds()
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

func parseWritebackKindPolicyArgs(args map[string]any) (map[string]writebackKindChange, error) {
	const where = WriteOpWritebackKindPolicy
	if err := rejectUnknownKeys(args, where, "kinds"); err != nil {
		return nil, err
	}
	raw, present := args["kinds"]
	kinds, ok := raw.(map[string]any)
	if !present || !ok {
		return nil, fmt.Errorf("%s.kinds must be an object of write-back kind → override (or null to remove the override)", where)
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("%s.kinds names no kind", where)
	}
	out := map[string]writebackKindChange{}
	for name, rawChange := range kinds {
		kind := strings.ToLower(strings.TrimSpace(name))
		if !writeback.IsKind(kind) {
			return nil, fmt.Errorf("%s.kinds: %q is not a write-back kind (known: %s)", where, name, strings.Join(writebackKindNames(), ", "))
		}
		if _, dup := out[kind]; dup {
			return nil, fmt.Errorf("%s.kinds names %s twice", where, kind)
		}
		field := where + ".kinds." + kind
		if rawChange == nil {
			out[kind] = writebackKindChange{remove: true}
			continue
		}
		m, ok := rawChange.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s must be an object with %s, or null to remove the override", field, strings.Join(writebackKindFields, ", "))
		}
		if err := rejectUnknownKeys(m, field, writebackKindFields...); err != nil {
			return nil, err
		}
		if len(m) == 0 {
			return nil, fmt.Errorf("%s names no field: give %s, or null to remove the override", field, strings.Join(writebackKindFields, ", "))
		}
		c := writebackKindChange{set: map[string]any{}}
		for _, f := range writebackKindFields {
			v, present := m[f]
			if !present {
				continue
			}
			if v == nil {
				c.unset = append(c.unset, f)
				continue
			}
			switch f {
			case "min_level", "unlabeled_min_level":
				n, ok := numberArg(v)
				if !ok || n > config.MaxACMMLevel {
					return nil, fmt.Errorf("%s.%s must be a level from 0 to %d, or null to drop it", field, f, config.MaxACMMLevel)
				}
				c.set[f] = n
			default:
				b, ok := v.(bool)
				if !ok {
					return nil, fmt.Errorf("%s.%s must be true or false, or null to drop it", field, f)
				}
				c.set[f] = b
			}
		}
		out[kind] = c
	}
	return out, nil
}

// writebackKindState is the write-back settings the preview read. Kinds is
// what the confirm fingerprints.
type writebackKindState struct {
	Kinds         map[string]writeback.KindPolicyOverride `json:"writeback_kinds"`
	requireLabels bool
	optInLabels   []string
}

func readWritebackKindState(ctx context.Context, provider Provider) (writebackKindState, error) {
	const what = "write-back kind overrides"
	m, err := readGovernorSettings(ctx, provider, what)
	if err != nil {
		return writebackKindState{}, err
	}
	features, _ := m["features"].(map[string]any)
	if features == nil {
		return writebackKindState{}, missingSetting(what, "features")
	}
	raw, present := features["writebackKinds"]
	if !present {
		return writebackKindState{}, missingSetting(what, "features.writebackKinds")
	}
	st := writebackKindState{Kinds: map[string]writeback.KindPolicyOverride{}, requireLabels: true}
	if raw != nil {
		data, err := json.Marshal(raw)
		if err != nil {
			return writebackKindState{}, missingSetting(what, "features.writebackKinds")
		}
		var kinds map[string]writeback.KindPolicyOverride
		if err := json.Unmarshal(data, &kinds); err != nil {
			return writebackKindState{}, missingSetting(what, "features.writebackKinds")
		}
		for k, ov := range kinds {
			st.Kinds[strings.ToLower(strings.TrimSpace(k))] = ov
		}
	}
	if b, ok := boolField(features, "writebackRequireLabels"); ok {
		st.requireLabels = b
	}
	st.optInLabels, _ = stringsField(features, "writebackOptInLabels")
	return st, nil
}

// writebackKindOverrideBody is one override as the endpoint takes it: only
// the fields that are set.
func writebackKindOverrideBody(ov writeback.KindPolicyOverride) map[string]any {
	out := map[string]any{}
	if ov.MinLevel != nil {
		out["min_level"] = *ov.MinLevel
	}
	if ov.RequireLabel != nil {
		out["require_label"] = *ov.RequireLabel
	}
	if ov.UnlabeledMinLevel != nil {
		out["unlabeled_min_level"] = *ov.UnlabeledMinLevel
	}
	if ov.OwnOnly != nil {
		out["own_only"] = *ov.OwnOnly
	}
	return out
}

func writebackKindOverrideEmpty(ov writeback.KindPolicyOverride) bool {
	return ov.MinLevel == nil && ov.RequireLabel == nil && ov.UnlabeledMinLevel == nil && ov.OwnOnly == nil
}

func writebackKindOverrideLabel(ov writeback.KindPolicyOverride, present bool) string {
	if !present || writebackKindOverrideEmpty(ov) {
		return "no override"
	}
	var parts []string
	for _, f := range writebackKindFields {
		if v, ok := writebackKindOverrideBody(ov)[f]; ok {
			parts = append(parts, fmt.Sprintf("%s: %v", f, v))
		}
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// applyWritebackKindChange merges c onto the kind's current override.
// Removing an override or a field that is not set is refused, so a typo
// cannot pass for a removal.
func applyWritebackKindChange(kind string, cur writeback.KindPolicyOverride, present bool, c writebackKindChange) (writeback.KindPolicyOverride, error) {
	if c.remove {
		if !present {
			return cur, fmt.Errorf("%s: cannot remove the override for %s, it has none", WriteOpWritebackKindPolicy, kind)
		}
		return writeback.KindPolicyOverride{}, nil
	}
	next := cur
	set := writebackKindOverrideBody(cur)
	for _, f := range c.unset {
		if _, ok := set[f]; !ok {
			return cur, fmt.Errorf("%s: cannot drop %s.%s, the override does not set it (%s)", WriteOpWritebackKindPolicy, kind, f, writebackKindOverrideLabel(cur, present))
		}
		switch f {
		case "min_level":
			next.MinLevel = nil
		case "require_label":
			next.RequireLabel = nil
		case "unlabeled_min_level":
			next.UnlabeledMinLevel = nil
		case "own_only":
			next.OwnOnly = nil
		}
	}
	for f, v := range c.set {
		switch f {
		case "min_level":
			n := v.(int)
			next.MinLevel = &n
		case "require_label":
			b := v.(bool)
			next.RequireLabel = &b
		case "unlabeled_min_level":
			n := v.(int)
			next.UnlabeledMinLevel = &n
		case "own_only":
			b := v.(bool)
			next.OwnOnly = &b
		}
	}
	return next, nil
}

// writebackKindNever is the level a kind never reaches.
const writebackKindNever = 99

// writebackKindReach is the lowest level at which kind writes on an item the
// hive did not author, with and without the opt-in label (Gate.Allowed).
func writebackKindReach(p writeback.KindPolicy) (withLabel, withoutLabel int) {
	if p.OwnOnly {
		return writebackKindNever, writebackKindNever
	}
	switch {
	case !p.RequireLabel:
		return p.MinLevel, p.MinLevel
	case p.UnlabeledMinLevel > 0:
		return p.MinLevel, p.UnlabeledMinLevel
	default:
		return p.MinLevel, writebackKindNever
	}
}

func writebackKindPolicyLabel(p writeback.KindPolicy) string {
	with, without := writebackKindReach(p)
	switch {
	case with == writebackKindNever:
		return "hive-authored items only"
	case without == with:
		return fmt.Sprintf("L%d, any item", with)
	case without == writebackKindNever:
		return fmt.Sprintf("L%d with the opt-in label", with)
	default:
		return fmt.Sprintf("L%d with the opt-in label, L%d without", with, without)
	}
}

func writebackKindLevelLabel(level int) string {
	if level == writebackKindNever {
		return "never"
	}
	return fmt.Sprintf("L%d", level)
}

func (writebackKindPolicyOp) Name() string { return WriteOpWritebackKindPolicy }
func (writebackKindPolicyOp) Description() string {
	return "Change the write-back gate's per-kind overrides (writeback.kinds.<kind>: min_level, require_label, unlabeled_min_level, own_only) through PUT /api/config/governor/features, the endpoint Settings → Features saves through. kinds maps a write-back kind to the fields to set (null drops one field), or to null to remove that kind's override. That endpoint replaces the whole override map, so the preview reads the current overrides and sends them all with the change merged in: kinds you do not name keep their override. The preview shows each kind's override and effective policy before → after and says when a kind would write at a lower level or without the opt-in label. write_confirm is refused if the overrides changed since the preview. Owner only: the endpoint refuses any other role. It posts, labels and merges nothing itself."
}
func (writebackKindPolicyOp) InputSchema() map[string]any {
	level := map[string]any{"anyOf": []any{map[string]any{"type": "integer", "minimum": 0, "maximum": config.MaxACMMLevel}, map[string]any{"type": "null"}}}
	flag := map[string]any{"anyOf": []any{map[string]any{"type": "boolean"}, map[string]any{"type": "null"}}}
	override := map[string]any{"type": "object", "properties": map[string]any{
		"min_level":           level,
		"require_label":       flag,
		"unlabeled_min_level": level,
		"own_only":            flag,
	}, "additionalProperties": false}
	kinds := map[string]any{}
	for _, k := range writebackKindNames() {
		kinds[k] = map[string]any{"anyOf": []any{override, map[string]any{"type": "null"}}}
	}
	return map[string]any{"type": "object", "properties": map[string]any{
		"kinds": map[string]any{"type": "object", "description": "Write-back kind → the override fields to change (null drops a field), or null to remove the kind's override. Kinds you do not name keep their override.", "properties": kinds, "additionalProperties": false},
	}, "required": []string{"kinds"}, "additionalProperties": false}
}

// Preview without a provider checks the arguments and names the endpoint;
// see agentCadencesOp.Preview.
func (writebackKindPolicyOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseWritebackKindPolicyArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpWritebackKindPolicy, "writeback.kinds", "/api/config/governor/features", "write-back kind overrides"), nil
}

// ConfirmGuard reads the overrides again at write_confirm and returns their
// fingerprint, which must still match the preview's.
func (op writebackKindPolicyOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// PreviewWithProvider reads the current overrides, merges the change onto
// them and discloses every kind that would write on more items.
func (writebackKindPolicyOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	req, err := parseWritebackKindPolicyArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	st, err := readWritebackKindState(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	after := map[string]writeback.KindPolicyOverride{}
	for k, ov := range st.Kinds {
		after[k] = ov
	}
	named := make([]string, 0, len(req))
	for k := range req {
		named = append(named, k)
	}
	sort.Strings(named)

	requireLabels := st.requireLabels
	policyOf := func(kinds map[string]writeback.KindPolicyOverride, kind string) writeback.KindPolicy {
		return writeback.Config{Kinds: kinds, Unsolicited: writeback.UnsolicitedConfig{RequireLabels: &requireLabels}}.Policy(writeback.Kind(kind))
	}
	optIn := listLabel(st.optInLabels)
	if len(st.optInLabels) == 0 {
		optIn = "the review opt-in label"
	}

	var changes []settingChange
	var widen, narrow, changed []string
	for _, kind := range named {
		cur, present := st.Kinds[kind]
		next, err := applyWritebackKindChange(kind, cur, present, req[kind])
		if err != nil {
			return WritePreview{}, err
		}
		if writebackKindOverrideEmpty(next) {
			delete(after, kind)
		} else {
			after[kind] = next
		}
		_, nextPresent := after[kind]
		beforeLabel := writebackKindOverrideLabel(cur, present)
		afterLabel := writebackKindOverrideLabel(next, nextPresent)
		if beforeLabel == afterLabel {
			continue
		}
		changed = append(changed, kind)
		pb, pa := policyOf(st.Kinds, kind), policyOf(after, kind)
		changes = append(changes, settingChange{
			Field:  "writeback.kinds." + kind,
			Before: beforeLabel,
			After:  afterLabel,
			Note:   "effective: " + writebackKindPolicyLabel(pb) + " → " + writebackKindPolicyLabel(pa),
		})
		bw, bwo := writebackKindReach(pb)
		aw, awo := writebackKindReach(pa)
		switch {
		case bw == writebackKindNever && aw != writebackKindNever:
			widen = append(widen, kind+" now writes on items the hive did not author ("+writebackKindPolicyLabel(pa)+"); before, only on its own")
		case aw == writebackKindNever && bw != writebackKindNever:
			narrow = append(narrow, kind+" now writes only on items the hive authored (was "+writebackKindPolicyLabel(pb)+")")
		default:
			switch {
			case aw < bw:
				widen = append(widen, kind+" now writes on opted-in items from "+writebackKindLevelLabel(aw)+" (was "+writebackKindLevelLabel(bw)+")")
			case aw > bw:
				narrow = append(narrow, kind+" writes on opted-in items from "+writebackKindLevelLabel(aw)+" only (was "+writebackKindLevelLabel(bw)+")")
			}
			switch {
			case awo < bwo && bwo == writebackKindNever:
				widen = append(widen, kind+" now writes on items without the opt-in label ("+optIn+") from "+writebackKindLevelLabel(awo)+"; before, it needed the label")
			case awo < bwo:
				widen = append(widen, kind+" now writes on items without the opt-in label from "+writebackKindLevelLabel(awo)+" (was "+writebackKindLevelLabel(bwo)+")")
			case awo > bwo && awo == writebackKindNever:
				narrow = append(narrow, kind+" no longer writes on items without the opt-in label (was from "+writebackKindLevelLabel(bwo)+")")
			case awo > bwo:
				narrow = append(narrow, kind+" writes on items without the opt-in label from "+writebackKindLevelLabel(awo)+" only (was "+writebackKindLevelLabel(bwo)+")")
			}
		}
	}
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("%s changes nothing: the overrides already read that way", WriteOpWritebackKindPolicy)
	}
	fingerprint, err := stateFingerprint(st)
	if err != nil {
		return WritePreview{}, err
	}

	// The endpoint replaces the whole map: send every override, changed or
	// not. An empty map clears them all.
	kindsBody := map[string]any{}
	var kept []string
	for k, ov := range after {
		kindsBody[k] = writebackKindOverrideBody(ov)
		if _, isNamed := req[k]; !isNamed {
			kept = append(kept, k)
		}
	}
	sort.Strings(kept)

	effects := make([]string, 0, len(changes)+5)
	for _, c := range changes {
		effects = append(effects, c.effect())
	}
	keptNote := "No other kind has an override."
	if len(kept) > 0 {
		keptNote = "The overrides of kinds not named here (" + strings.Join(kept, ", ") + ") are sent unchanged and kept."
	}
	effects = append(effects,
		"Hive saves it through Settings → Features (PUT /api/config/governor/features, writebackKinds), records a config_governor_features audit entry, and the write-back gate applies it from its next decision, with no restart. That endpoint replaces the whole override map, so this write sends every override with the change merged in. "+keptNote,
		"The effective policy shown ignores writeback.unsolicited.min_level, which when set still raises every kind's level, and writeback.per_repo, which still sets each repository's mode.",
	)
	effects = append(effects, "This write itself posts, labels, closes and merges nothing.")

	narrowNote := ""
	if len(narrow) > 0 {
		narrowNote = " Narrowing: " + strings.Join(narrow, "; ") + "."
	}
	disclosure := "No widening: no kind writes at a lower level or without the opt-in label." + narrowNote
	if len(widen) > 0 {
		disclosure = "Widening: hive writes on more items it did not open. " + strings.Join(widen, "; ") + "." + narrowNote
	}
	return WritePreview{
		Operation:           WriteOpWritebackKindPolicy,
		Summary:             "Change the write-back kind overrides: " + strings.Join(changed, ", "),
		Target:              "writeback.kinds",
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/features", Body: map[string]any{"writebackKinds": kindsBody}},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing the write-back kind overrides: " + strings.Join(changed, ", ") + ".",
		Details:             map[string]any{"changes": changeDetails(changes), "kept": kept},
		StateFingerprint:    fingerprint,
	}, nil
}
