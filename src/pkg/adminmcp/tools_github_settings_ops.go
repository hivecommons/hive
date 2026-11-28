package adminmcp

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// github.settings changes the GitHub API budget settings on Settings → Forge
// App (github.api_reserve, api_critical, agent_reserve_floor,
// pr_detail_ttl_s, graphql_pr_batch_page_size, etag_cache_max_mb and
// etag_cache_max_entries) through PUT /api/config/github, the endpoint that
// card saves with (saveForgeGitHubSettings). Each of these seven is a
// pointer in the endpoint's body and an absent one is left alone
// (githubAPISettings.apply), so this op sends only the fields the caller names
// and never sends the card back whole. The fleet agent cap on the same card
// has its own op, github.agent_api_hourly_cap.
//
// They do not all apply at the same moment. agent_reserve_floor,
// pr_detail_ttl_s and graphql_pr_batch_page_size are read live, and the
// endpoint resizes the ETag cache itself. api_reserve and api_critical are
// copied into the GitHub client when it is built, so they apply when the
// save rebuilds the client (a hive running as a GitHub App with its key,
// reported as reinit ok in the endpoint's answer) and otherwise at the next
// restart. The preview says which.
//
// The preview reads GET /api/config/github/forge-apps (settings_read section
// github_apps) to show each named field before and after; a field already at
// the requested value is not sent. ConfirmGuard reads again at write_confirm
// and refuses when a named field moved after the preview, and ConfirmResult
// reads the section back so write_confirm reports what the hive now holds.
//
// The endpoint checks the values against the bounds the hive enforces at
// start (api_critical below api_reserve, the ETag cache bounds and the like),
// saves them, records a config_github audit entry naming the caller, and is
// owner only. Its refusal comes back to the caller verbatim as a hive
// refusal; this op does not repeat its checks.
const (
	githubSettingsPath    = "/api/config/github"
	githubSettingsSection = "github_apps"
)

func githubSettingsWriteOps() []WriteOp { return []WriteOp{githubSettingsOp{}} }

type githubSettingsOp struct{}

// githubSettingsField is one whole-number field of the PUT body. 0 means the
// default for every one of them.
type githubSettingsField struct {
	key string
	// def is the value 0 means, used when the read reports no default.
	def int
	// zeroReadsAsDefault: after a 0 is saved the settings read reports the
	// default, not 0. The endpoint writes the default itself for api_reserve
	// and api_critical, and the read reports agent_reserve_floor as its
	// effective value.
	zeroReadsAsDefault bool
	// atBuild: the GitHub client copies the field when it is built, so a
	// change applies when the client is rebuilt, not on the next request.
	atBuild bool
	// lowerCosts: a lower value makes the hive spend more API calls.
	lowerCosts string
	// lowerWidens: a lower value lets the hive spend deeper into the App
	// installation's hourly bucket before it sheds work; widens says how.
	lowerWidens bool
	widens      string
}

// githubSettingsFields is every PUT /api/config/github field github.settings
// accepts, in the order the Forge App card lists them.
// TestAdminMCPGitHubSettingsFieldsMatchHandlerBody (pkg/dashboard) fails when
// the endpoint takes a field that is neither in this list nor in its short
// list of fields this op leaves out (some of which no admin MCP op sets yet).
var githubSettingsFields = []githubSettingsField{
	{key: "api_reserve", def: config.DefaultGitHubAPIReserve, zeroReadsAsDefault: true, atBuild: true, lowerWidens: true,
		widens: "the hive keeps running its optional sweeps until fewer core REST calls are left before it enters conserve mode."},
	{key: "api_critical", def: config.DefaultGitHubAPICritical, zeroReadsAsDefault: true, atBuild: true, lowerWidens: true,
		widens: "the hive keeps its unstick steps and normal eval interval until fewer calls are left before it enters critical mode."},
	{key: "agent_reserve_floor", def: config.DefaultGitHubAgentReserveFloor, zeroReadsAsDefault: true, lowerWidens: true,
		widens: "agents may keep reading through the API proxy until fewer calls are left for the hive's own scan, merge and review loops."},
	{key: "pr_detail_ttl_s", def: int(config.DefaultGitHubPRDetailTTL / time.Second),
		lowerCosts: "a shorter PR detail cache TTL makes the hive fetch pull request details again sooner, so it spends more GitHub API calls."},
	{key: "graphql_pr_batch_page_size", def: config.DefaultGraphQLPRBatchPageSize,
		lowerCosts: "a smaller GraphQL PR batch page size splits each repository's open-PR scan into more queries, so it spends more GitHub API calls."},
	{key: "etag_cache_max_mb", def: config.DefaultGitHubETagCacheMaxMB},
	{key: "etag_cache_max_entries", def: config.DefaultGitHubETagCacheMaxEntries},
}

// GitHubSettingsBodyFields lists every PUT /api/config/github body field
// github.settings accepts. pkg/dashboard checks it against the handler's body
// type.
func GitHubSettingsBodyFields() []string {
	out := make([]string, 0, len(githubSettingsFields))
	for _, f := range githubSettingsFields {
		out = append(out, f.key)
	}
	return out
}

func (githubSettingsOp) Name() string { return WriteOpGitHubSettings }
func (githubSettingsOp) Description() string {
	return "Change the GitHub API budget settings on Settings → Forge App through PUT " + githubSettingsPath + ", the endpoint that card saves with. Owner only. Pass only the fields that change, each a whole number where 0 restores the default: " + strings.Join(GitHubSettingsBodyFields(), ", ") + ". Only those fields are sent and every other setting keeps its value; the fleet agent cap is github.agent_api_hourly_cap. The preview reads the current settings and shows each named field before and after. Lowering api_reserve, api_critical or agent_reserve_floor is named as widening, because the hive then spends deeper into its GitHub App installation's hourly bucket before it sheds work; raising an ETag cache bound says how much more memory the cache may use. agent_reserve_floor, pr_detail_ttl_s, graphql_pr_batch_page_size and the ETag cache bounds apply without a restart; api_reserve and api_critical apply when the hive rebuilds its GitHub client, which a hive running as a GitHub App does on this save (the write result shows reinit ok), and otherwise at the next restart. write_confirm refuses if any named field changed after the preview, then reads them back and reports what the hive now holds. The endpoint's own checks (api_critical below api_reserve, the ETag cache bounds and the like) come back as its error."
}

func (githubSettingsOp) InputSchema() map[string]any {
	props := map[string]any{}
	for _, f := range githubSettingsFields {
		props[f.key] = map[string]any{"type": "integer", "description": "github." + f.key + "; 0 restores the default (" + strconv.Itoa(f.def) + ")."}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{}, "additionalProperties": false}
}

// githubSettingsValue is one named field and the value to send for it.
type githubSettingsValue struct {
	field *githubSettingsField
	value int
}

// githubSettingsParseArgs checks each named field is a whole number. It
// checks no range: the endpoint does, and its refusal reaches the caller.
func githubSettingsParseArgs(args map[string]any) ([]githubSettingsValue, error) {
	const where = WriteOpGitHubSettings
	if len(args) == 0 {
		return nil, fmt.Errorf("give at least one GitHub API setting to change (accepted: %s)", strings.Join(GitHubSettingsBodyFields(), ", "))
	}
	if _, ok := args["agent_github_api_hourly_cap"]; ok {
		return nil, fmt.Errorf("%s does not set agent_github_api_hourly_cap; use the %s operation", where, WriteOpGitHubAgentAPIHourlyCap)
	}
	if err := rejectUnknownKeys(args, where, GitHubSettingsBodyFields()...); err != nil {
		return nil, err
	}
	var out []githubSettingsValue
	for i := range githubSettingsFields {
		f := &githubSettingsFields[i]
		raw, present := args[f.key]
		if !present {
			continue
		}
		if raw == nil {
			return nil, fmt.Errorf("%s.%s: null is not accepted; leave the field out to keep its value, or give 0 to restore the default", where, f.key)
		}
		n, ok := reviewSettingsNumber(raw)
		if !ok || n != math.Trunc(n) || math.Abs(n) > math.MaxInt32 {
			return nil, fmt.Errorf("%s.%s must be a whole number", where, f.key)
		}
		out = append(out, githubSettingsValue{field: f, value: int(n)})
	}
	return out, nil
}

// githubSettingsState is the settings object of the github_apps section.
type githubSettingsState map[string]any

func githubSettingsRead(ctx context.Context, provider Provider) (githubSettingsState, error) {
	if provider == nil {
		return nil, fmt.Errorf("%s reads the current GitHub API settings first, and this connection cannot read them", WriteOpGitHubSettings)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": githubSettingsSection})
	if err != nil {
		return nil, fmt.Errorf("%s needs the current GitHub API settings (settings_read section %s): %w", WriteOpGitHubSettings, githubSettingsSection, err)
	}
	m, _ := data.(map[string]any)
	settings, ok := m["settings"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s could not read the current GitHub API settings: settings_read section %s returned no settings object", WriteOpGitHubSettings, githubSettingsSection)
	}
	return githubSettingsState(settings), nil
}

// current is the field's value as the section reports it.
func (st githubSettingsState) current(f *githubSettingsField) (int, error) {
	n, ok := numberArg(st[f.key])
	if !ok {
		return 0, fmt.Errorf("settings_read section %s does not report %s, so %s cannot show what would change", githubSettingsSection, f.key, WriteOpGitHubSettings)
	}
	return n, nil
}

// def is the value 0 means for f, as the hive reports it when it does.
func (st githubSettingsState) def(f *githubSettingsField) int {
	defaults, _ := st["defaults"].(map[string]any)
	if n, ok := numberArg(defaults[f.key]); ok && n > 0 {
		return n
	}
	return f.def
}

// effective is the value v puts in force: 0 means the default.
func (st githubSettingsState) effective(f *githubSettingsField, v int) int {
	if v <= 0 {
		return st.def(f)
	}
	return v
}

func (st githubSettingsState) label(f *githubSettingsField, v int) string {
	if v == 0 {
		return "0 (default " + strconv.Itoa(st.def(f)) + ")"
	}
	return strconv.Itoa(v)
}

type githubSettingsRow struct {
	field         *githubSettingsField
	before, after int
}

type githubSettingsPlan struct {
	state       githubSettingsState
	rows        []githubSettingsRow
	unchanged   []string
	body        map[string]any
	fingerprint string
}

func githubSettingsPlanFor(ctx context.Context, args map[string]any, provider Provider) (githubSettingsPlan, error) {
	values, err := githubSettingsParseArgs(args)
	if err != nil {
		return githubSettingsPlan{}, err
	}
	st, err := githubSettingsRead(ctx, provider)
	if err != nil {
		return githubSettingsPlan{}, err
	}
	plan := githubSettingsPlan{state: st, body: map[string]any{}}
	before := map[string]any{}
	for _, v := range values {
		f := v.field
		cur, err := st.current(f)
		if err != nil {
			return githubSettingsPlan{}, err
		}
		before[f.key] = cur
		after := v.value
		if after == 0 && f.zeroReadsAsDefault {
			after = st.def(f)
		}
		if after == cur {
			plan.unchanged = append(plan.unchanged, f.key)
			continue
		}
		plan.rows = append(plan.rows, githubSettingsRow{field: f, before: cur, after: after})
		plan.body[f.key] = v.value
	}
	if len(plan.rows) == 0 {
		return githubSettingsPlan{}, fmt.Errorf("nothing would change: %s already as requested", strings.Join(plan.unchanged, ", "))
	}
	plan.fingerprint = settingsFingerprint(before)
	return plan, nil
}

// Preview cannot read the current settings, so it only checks the
// arguments. write_preview uses PreviewWithProvider; write_confirm calls
// Preview to check the request still names this endpoint, and then
// ConfirmGuard reads again.
func (githubSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := githubSettingsParseArgs(args); err != nil {
		return WritePreview{}, err
	}
	return WritePreview{
		Operation:           WriteOpGitHubSettings,
		Summary:             "Change GitHub API settings (current settings not read)",
		Target:              "GitHub API settings (github)",
		Request:             WriteRequest{Method: http.MethodPut, Path: githubSettingsPath},
		WideningDisclosure:  "Potential widening: the current GitHub API settings were not read, so this preview cannot say what would change. Lowering api_reserve, api_critical or agent_reserve_floor lets the hive spend deeper into its GitHub API budget before it sheds work.",
		ConfirmationMessage: "Preview this change through write_preview to see what it would do.",
	}, nil
}

func (githubSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	plan, err := githubSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return WritePreview{}, err
	}
	st := plan.state
	fields := make([]string, 0, len(plan.rows))
	changes := make([]map[string]any, 0, len(plan.rows))
	lines := make([]string, 0, len(plan.rows))
	shrinks := false
	var atBuild, live, costs []string
	for _, row := range plan.rows {
		f := row.field
		fields = append(fields, f.key)
		changes = append(changes, map[string]any{"field": f.key, "before": row.before, "after": row.after})
		lines = append(lines, f.key+": "+st.label(f, row.before)+" → "+st.label(f, row.after))
		lower := st.effective(f, row.after) < st.effective(f, row.before)
		if (f.key == "etag_cache_max_mb" || f.key == "etag_cache_max_entries") && lower {
			shrinks = true
		}
		if f.lowerCosts != "" && lower {
			costs = append(costs, f.key+" goes down: "+f.lowerCosts)
		}
		if f.atBuild {
			atBuild = append(atBuild, f.key)
		} else {
			live = append(live, f.key)
		}
	}
	effects := append([]string{}, lines...)
	effects = append(effects,
		"Hive sends PUT "+githubSettingsPath+" with only these fields. Every other setting on the Forge App card keeps its value, including one changed on the dashboard after this preview.",
		"The endpoint checks the values against the bounds the hive enforces at start, saves them to hive.yaml and the dashboard overlay, and records a config_github audit entry with you as the actor. It is owner only, and a value it refuses comes back as its own error.",
	)
	if len(live) > 0 {
		how := " apply without a restart: the hive reads them live."
		if strings.Contains(strings.Join(live, ","), "etag_cache_") {
			how = " apply without a restart: the hive reads them live, and the endpoint resizes the ETag cache itself on save."
		}
		effects = append(effects, strings.Join(live, ", ")+how)
	}
	if len(atBuild) > 0 {
		effects = append(effects, strings.Join(atBuild, " and ")+" apply only when the hive rebuilds its GitHub client. A hive running as a GitHub App with its key in place rebuilds it on this save, and they apply then: the write result shows reinit ok. On a hive that uses a personal access token, or when that rebuild fails (reinit failed, with reinit_error), they apply after the next restart.")
	}
	if shrinks {
		effects = append(effects, "A smaller ETag cache evicts at once, so some GETs that a free 304 answered before are charged again until the cache refills.")
	}
	effects = append(effects, costs...)
	effects = append(effects, "write_confirm reads these settings again and refuses if any of them changed after this preview, then reads them back and reports what the hive now holds.")
	details := map[string]any{"fields": fields, "changes": changes}
	if len(plan.unchanged) > 0 {
		effects = append(effects, "Already as requested, so not sent: "+strings.Join(plan.unchanged, ", ")+".")
		details["unchanged"] = plan.unchanged
	}
	return WritePreview{
		Operation:           WriteOpGitHubSettings,
		Summary:             "Change GitHub API settings: " + strings.Join(fields, ", "),
		Target:              "GitHub API settings (github)",
		Request:             WriteRequest{Method: http.MethodPut, Path: githubSettingsPath, Body: plan.body},
		Effects:             effects,
		WideningDisclosure:  githubSettingsDisclosure(st, plan.rows),
		ConfirmationMessage: "Confirm changing GitHub API settings: " + strings.Join(lines, "; ") + ".",
		Details:             details,
		StateFingerprint:    plan.fingerprint,
	}, nil
}

// ConfirmGuard reads the settings again and returns the fingerprint of the
// named fields' values.
func (githubSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	plan, err := githubSettingsPlanFor(ctx, args, provider)
	if err != nil {
		return "", fmt.Errorf("write_confirm checks the GitHub API settings again before writing, and that check refused this change; preview it again: %w", err)
	}
	return plan.fingerprint, nil
}

// ConfirmResult reads the settings back after the write and returns each
// named field as the hive now holds it.
func (githubSettingsOp) ConfirmResult(ctx context.Context, args map[string]any, provider Provider) (any, error) {
	values, err := githubSettingsParseArgs(args)
	if err != nil {
		return nil, err
	}
	st, err := githubSettingsRead(ctx, provider)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, v := range values {
		cur, err := st.current(v.field)
		if err != nil {
			return nil, err
		}
		out[v.field.key] = cur
	}
	return out, nil
}

// githubSettingsDisclosure names each lowered floor as widening, and says how
// much more memory a raised ETag cache bound lets the cache use.
func githubSettingsDisclosure(st githubSettingsState, rows []githubSettingsRow) string {
	// The MB bound in force after the change, for the entries note.
	mbField := githubSettingsFieldFor("etag_cache_max_mb")
	mbCur, _ := numberArg(st[mbField.key])
	mbBound := st.effective(mbField, mbCur)
	for _, row := range rows {
		if row.field.key == mbField.key {
			mbBound = st.effective(mbField, row.after)
		}
	}
	var widen, memory []string
	widenAtBuild := false
	for _, row := range rows {
		f := row.field
		from, to := st.effective(f, row.before), st.effective(f, row.after)
		switch {
		case f.lowerWidens && to < from:
			widen = append(widen, fmt.Sprintf("%s drops from %d to %d: %s", f.key, from, to, f.widens))
			widenAtBuild = widenAtBuild || f.atBuild
		case f.key == "etag_cache_max_mb" && to > from:
			memory = append(memory, fmt.Sprintf("etag_cache_max_mb rises from %d to %d: the ETag cache may use up to %d MB more memory (%d MB in all) for the gzip-compressed response bodies it keeps.", from, to, to-from, to))
		case f.key == "etag_cache_max_entries" && to > from:
			memory = append(memory, fmt.Sprintf("etag_cache_max_entries rises from %d to %d: the ETag cache may keep up to %d more responses, and so use more memory, up to its etag_cache_max_mb bound of %d MB.", from, to, to-from, mbBound))
		}
	}
	tail := ""
	if len(memory) > 0 {
		tail = " More memory: " + strings.Join(memory, " ")
	}
	tail += " This operation changes only these settings; it starts, merges and closes nothing."
	if len(widen) == 0 {
		return "No widening: none of these changes lets the hive spend deeper into its GitHub API budget before it sheds work." + tail
	}
	when := ""
	if widenAtBuild {
		when = " A lower api_reserve or api_critical takes effect when the hive rebuilds its GitHub client: on this save for a hive running as a GitHub App whose rebuild succeeds (reinit ok), otherwise at the next restart."
	}
	return "Widening: " + strings.Join(widen, " ") + " These floors guard the one hourly rate-limit bucket of this hive's GitHub App installation, which agents and the hive's own loops share, so the hive now spends deeper into it before it sheds work." + when + tail
}

func githubSettingsFieldFor(key string) *githubSettingsField {
	for i := range githubSettingsFields {
		if githubSettingsFields[i].key == key {
			return &githubSettingsFields[i]
		}
	}
	return nil
}
