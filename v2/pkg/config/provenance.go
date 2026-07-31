package config

// Field provenance — "which layer set this field, and can I even edit it?"
//
// This exists because that question previously had no answer short of reading
// pod boot logs. The cost was concrete and repeated:
//
//   - A GitHub Enterprise hive was repaired on the FOURTH attempt, after
//     patching the hub's meta.json, then clusters.json, then the spoke's
//     ConfigMap — three layers that are INERT for github.app_id — before
//     patching the dashboard overlay, the layer that actually wins.
//
//   - A half-applied GHE identity (app_id switched to the GHE App, api_url
//     left empty so it defaulted to api.github.com) produced
//     "404 Integration not found" on live hives. The two fields live in the
//     same layer, and seeing them side by side with their sources would have
//     made the split obvious immediately. See IdentitySetIssues.
//
// Provenance is derived, never authoritative: it records what MergeLayers did.
// It changes no behaviour.

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// FieldOrigin records where one field's effective value came from.
type FieldOrigin struct {
	// Field is the dotted config path, e.g. "github.app_id".
	Field string `json:"field"`
	// Layer is the layer that supplied the winning value.
	Layer string `json:"layer"`
	// Path is where to write to change it.
	Path string `json:"path"`
	// Writer names who can write that layer at runtime. For the ConfigMap
	// seed this is "nobody" — the fact that ends most misdirected repairs.
	Writer string `json:"writer"`
	// Writable is false when no running component can write the layer.
	Writable bool `json:"writable"`
	// Value is the effective value, rendered for display. Secrets are
	// redacted; see redactField.
	Value string `json:"value,omitempty"`
	// AlsoSetBy lists lower layers that set this field but lost. This is what
	// reveals a stale ConfigMap value that looks authoritative and is not.
	AlsoSetBy []string `json:"also_set_by,omitempty"`
}

// Provenance is the per-field origin map for one merge.
type Provenance struct {
	// Fields maps dotted field path → winning layer.
	Fields map[string]Layer `json:"-"`
	// alsoSet tracks every layer that supplied a value, winner included.
	alsoSet map[string][]Layer

	// OverlayRejected is true when a dashboard overlay existed but failed the
	// plausibility guard and was DISCARDED, so the hive is running on the bare
	// ConfigMap seed.
	//
	// On the live fleet seeds are 4–11% of the running config, so this state
	// can mean a hive booted with a fraction of its agent roster. It must
	// never be silent.
	OverlayRejected bool `json:"overlay_rejected"`
	// OverlayRejectReason names why, e.g. "overlay has no agents".
	OverlayRejectReason string `json:"overlay_reject_reason,omitempty"`
	// GitHubRatchetFired is true when the seed's github block was kept because
	// the overlay's looked like a placeholder (ratchet C in layers.go).
	GitHubRatchetFired bool `json:"github_ratchet_fired"`
}

// NewProvenance returns an empty Provenance.
func NewProvenance() *Provenance {
	return &Provenance{
		Fields:  make(map[string]Layer),
		alsoSet: make(map[string][]Layer),
	}
}

// Set records that layer supplied the winning value for field.
func (p *Provenance) Set(field string, layer Layer) {
	if p == nil {
		return
	}
	p.Fields[field] = layer
}

// SetPrefix re-points every already-recorded field under prefix at layer. Used
// when a whole block changes owner at once, as the github ratchet does.
func (p *Provenance) SetPrefix(prefix string, layer Layer) {
	if p == nil {
		return
	}
	for f := range p.Fields {
		if len(f) >= len(prefix) && f[:len(prefix)] == prefix {
			p.Fields[f] = layer
		}
	}
}

// LayerOf returns the layer that set field, or LayerUnset.
func (p *Provenance) LayerOf(field string) Layer {
	if p == nil {
		return LayerUnset
	}
	return p.Fields[field]
}

// note records that layer supplied a value for field, winner or not.
func (p *Provenance) note(field string, layer Layer) {
	if p == nil {
		return
	}
	for _, l := range p.alsoSet[field] {
		if l == layer {
			return
		}
	}
	p.alsoSet[field] = append(p.alsoSet[field], layer)
}

// recordAll walks cfg and records layer as a source for every field it sets,
// overwriting any previously recorded winner. Callers apply layers lowest →
// highest so the last write wins, matching the merge order.
func (p *Provenance) recordAll(cfg *Config, layer Layer) {
	if p == nil || cfg == nil {
		return
	}
	for _, f := range enumerateFields(cfg) {
		p.Fields[f.Field] = layer
		p.note(f.Field, layer)
	}
}

// enumerateFields lists the config fields provenance tracks, with their
// rendered values.
//
// This is a deliberately CURATED list, not reflection over the whole struct.
// It covers the fields an operator actually changes — the rows of the layering
// table — because an exhaustive dump of every nested field would bury the
// handful that matter. Fields absent from a config are skipped, so "not
// listed" means "no layer set it".
func enumerateFields(cfg *Config) []FieldOrigin {
	if cfg == nil {
		return nil
	}
	var out []FieldOrigin
	add := func(field, value string) {
		if value == "" {
			return
		}
		out = append(out, FieldOrigin{Field: field, Value: value})
	}

	add("project.org", cfg.Project.Org)
	add("project.primary_repo", cfg.Project.PrimaryRepo)
	if len(cfg.Project.Repos) > 0 {
		add("project.repos", strconv.Itoa(len(cfg.Project.Repos))+" repo(s)")
	}

	if cfg.ACMMLevel != nil {
		add("acmm_level", strconv.Itoa(*cfg.ACMMLevel))
	}

	// The GitHub identity set. These four fields must be internally
	// consistent; see IdentitySetIssues.
	if cfg.GitHub.AppID != 0 {
		add("github.app_id", strconv.FormatInt(cfg.GitHub.AppID, 10))
	}
	if cfg.GitHub.InstallationID != 0 {
		add("github.installation_id", strconv.FormatInt(cfg.GitHub.InstallationID, 10))
	}
	add("github.app_slug", cfg.GitHub.AppSlug)
	add("github.api_url", cfg.GitHub.APIURL)
	add("github.base_url", cfg.GitHub.BaseURL)
	add("github.key_file", cfg.GitHub.KeyFile)
	if cfg.GitHub.Token != "" {
		add("github.token", redacted)
	}

	add("hub.url", cfg.Hub.URL)
	add("hub.is_public", strconv.FormatBool(cfg.Hub.IsPublic))
	add("hive_id", cfg.HiveID)

	for name := range cfg.Agents {
		add("agents."+name, "configured")
	}
	return out
}

// redacted is the placeholder shown instead of secret values. Provenance is
// exposed over the dashboard API, so it must never carry credentials.
const redacted = "<redacted>"

// Report renders the provenance of cfg as a stable, sorted list suitable for
// the /api/config/provenance response.
func (p *Provenance) Report(cfg *Config) []FieldOrigin {
	fields := enumerateFields(cfg)
	out := make([]FieldOrigin, 0, len(fields))
	for _, f := range fields {
		layer := p.LayerOf(f.Field)
		f.Layer = layer.String()
		f.Path = layer.Path()
		f.Writer = layer.Writer()
		f.Writable = layer.Writable()
		if p != nil {
			for _, l := range p.alsoSet[f.Field] {
				if l != layer {
					f.AlsoSetBy = append(f.AlsoSetBy, l.String())
				}
			}
			sort.Strings(f.AlsoSetBy)
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// GitHub identity-set invariant
// ─────────────────────────────────────────────────────────────────────────

// gheAPIURLMarker identifies a GitHub Enterprise API URL. Public GitHub uses
// api.github.com (or an empty api_url, which defaults to it).
const gheAPIURLMarker = "/api/v3"

// The two valid GitHub identity sets, as named constants. Every one of these
// values appears somewhere in the tree as a bare literal today; the rules below
// must never grow one.
const (
	// PublicGitHubAppID is the numeric App ID of the public github.com
	// kubestellar-hive GitHub App.
	PublicGitHubAppID int64 = 3568013
	// PublicGitHubAppSlug is that App's URL slug (== DefaultGitHubAppSlug).
	PublicGitHubAppSlug = DefaultGitHubAppSlug

	// EnterpriseGitHubAppID is the numeric App ID of the github.ibm.com
	// kubestellar-hive-ghe GitHub App. This is the value that was pushed onto
	// seven public-GitHub hives on 2026-07-31.
	EnterpriseGitHubAppID int64 = 5686
	// EnterpriseGitHubAppSlug is the slug the config/dashboard paths use for
	// that App.
	//
	// NOTE: app_slug is NOT derivable from app_id, and no rule here treats it
	// as such. App ID 5686 appears in this tree under TWO slugs — this one and
	// "ibm-hive", which is what the vllm-d cluster fixture carries (see
	// pkg/hub/cluster_app_key_test.go). Both are legitimate: the slug is an
	// operator-chosen App URL name, so a rule asserting "app_id 5686 implies
	// slug kubestellar-hive-ghe" refuses a real production cluster. Only the
	// app_id → FORGE mapping below is safe to assert.
	EnterpriseGitHubAppSlug = "kubestellar-hive-ghe"
	// EnterpriseGitHubAPIURL and EnterpriseGitHubBaseURL are the forge URLs the
	// enterprise App lives on.
	EnterpriseGitHubAPIURL  = "https://github.ibm.com/api/v3"
	EnterpriseGitHubBaseURL = "https://github.ibm.com"
)

// forgeOfAppID classifies an app_id as belonging to a KNOWN forge.
//
// This is the only identity marker that is reliably populated on the real
// fleet. api_url and base_url are empty on ~41 of ~51 spokes (empty is the
// public default and nothing ever backfilled it), and app_slug is empty on many
// more — so every rule keyed on those three strings is unreachable in
// production for exactly the hives that need checking. app_id is required by
// config validation and is therefore always present.
//
// Returns "" for an App ID this build does not recognise (a third forge, a
// self-hosted deployment, the placeholder sentinel): unknown must never be
// treated as a mismatch, or a legitimate deployment is refused.
func forgeOfAppID(appID int64) string {
	switch appID {
	case PublicGitHubAppID:
		return DefaultGitHubBaseURL[len("https://"):] // "github.com"
	case EnterpriseGitHubAppID:
		return EnterpriseGitHubBaseURL[len("https://"):] // "github.ibm.com"
	}
	return ""
}

// IdentitySetIssues reports inconsistencies within the GitHub identity set:
// app_id, app_slug, api_url and base_url are ONE atomic identity and no valid
// mixture of forges exists.
//
//	GHE:    app_id=<ghe app>    slug=<ghe slug>    api_url=https://<host>/api/v3
//	Public: app_id=<public app> slug=<public slug> api_url="" (or api.github.com)
//
// WHY THIS EXISTS. A cluster-config change pushed a GHE app_id to a set of
// hives WITHOUT also pushing api_url. The spokes ended up with a GHE App ID
// and an empty api_url, which defaults to public GitHub, and every token
// request failed:
//
//	POST https://api.github.com/app/installations/<id>/access_tokens
//	404 Integration not found
//
// The hives that also received api_url=https://github.ibm.com/api/v3 were
// fine — the failure split exactly on that one field. #2360's forge endpoint
// refuses half-applied identities by construction; the cluster-config push
// path has no such guard.
//
// This function DETECTS and NAMES the invalid state. Callers on the WRITE path
// (see RejectIdentitySet) refuse a config carrying any of these issues rather
// than applying it partially; the provenance API renders the same list for
// display.
func IdentitySetIssues(gh GitHubConfig) []string {
	var issues []string
	apiIsGHE := containsFold(gh.APIURL, gheAPIURLMarker)
	baseIsGHE := gh.BaseURL != "" && !containsFold(gh.BaseURL, "github.com")
	slugIsGHE := containsFold(gh.AppSlug, "ghe")

	// RULE 0 — the app_id rule. Keyed on the ONE identity field that is always
	// populated on the real fleet, so unlike the three string-marker rules below
	// it actually fires on the shape the incident produced:
	//
	//	app_id:   5686                  <- GHE App
	//	app_slug: kubestellar-hive-ghe
	//	api_url:  ""                    <- EMPTY, resolves to api.github.com
	//	base_url: ""
	//
	// HostLabel() resolves an empty api_url/base_url to "github.com", which is
	// what makes this comparison meaningful: it asks "does the forge this App
	// belongs to match the forge this hive will actually talk to?", where the
	// second half already accounts for empty-means-public. A healthy public hive
	// (app_id 3568013, both URLs empty) compares github.com against github.com
	// and passes — which it must, because ~41 spokes run exactly that way.
	//
	// Unknown App IDs (a third forge, self-hosted, the placeholder sentinel) are
	// skipped: forgeOfAppID returns "" and no claim is made.
	if appForge := forgeOfAppID(gh.AppID); appForge != "" {
		if host := gh.HostLabel(); !strings.EqualFold(host, appForge) {
			issues = append(issues, "app_id "+strconv.FormatInt(gh.AppID, 10)+
				" is the GitHub App on "+appForge+", but api_url ("+displayOrEmpty(gh.APIURL)+
				") and base_url ("+displayOrEmpty(gh.BaseURL)+") resolve to "+host+
				" — an App ID presented to the wrong forge returns 404 Integration not found")
		}
	}

	// The live regression: an App is configured and the forge markers
	// disagree with each other.
	if gh.AppID != 0 {
		if slugIsGHE && !apiIsGHE {
			issues = append(issues, "app_slug looks like a GitHub Enterprise slug ("+gh.AppSlug+
				") but api_url is not a GHE API URL ("+displayOrEmpty(gh.APIURL)+
				") — a GHE App ID presented to api.github.com returns 404 Integration not found")
		}
		// base_url and api_url must name the same forge — but ONLY when both are
		// actually set.
		//
		// An empty one is not a public one. ResolvedBaseURL and HostLabel are
		// explicit that a GHE hive legitimately carries
		// api_url: https://github.ibm.com/api/v3 with base_url: "" (pooled and
		// placeholder GHE hives all do), and the symmetric case — a declared GHE
		// base_url with api_url unset — is the shape of the vllm-d cluster
		// record. Treating either silence as "public github.com" makes these two
		// rules fire on VALID configs.
		//
		// That was harmless while this function only rendered a dashboard panel.
		// It is not harmless now that RejectIdentitySet refuses writes: reading
		// an empty api_url as a contradiction would refuse the GHE App push to
		// every GHE hive that has not backfilled both URLs. Requiring both to be
		// populated before comparing them is what keeps the rule true rather
		// than merely loud. The app_id rule above still catches the incident,
		// which is the case where BOTH are empty.
		if gh.APIURL != "" && gh.BaseURL != "" {
			if baseIsGHE && !apiIsGHE {
				issues = append(issues, "base_url is "+gh.BaseURL+" but api_url is "+
					gh.APIURL+" — base_url and api_url must name the same forge")
			}
			if apiIsGHE && !baseIsGHE {
				issues = append(issues, "api_url is a GHE API URL but base_url is "+gh.BaseURL+
					" — base_url and api_url must name the same forge")
			}
		}
	}
	return issues
}

// ErrIdentitySetMismatch is the sentinel returned by RejectIdentitySet. Callers
// on the write path test for it with errors.Is to distinguish "this push is
// internally inconsistent" from any other failure.
var ErrIdentitySetMismatch = errors.New("github identity set is internally inconsistent")

// RejectIdentitySet is the WRITE-PATH guard. It returns a non-nil error when
// applying gh would leave a hive with a half-applied GitHub identity.
//
// It is the caller IdentitySetIssues never had. Detection alone is what let the
// 2026-07-31 push land: the mismatch was rendered on a dashboard panel while
// every spoke applied it field by field, adopting the GHE app_id and leaving
// api_url empty because "empty means unchanged".
//
// Callers must apply NOTHING when this returns an error. Applying the
// consistent subset is the failure mode, not the remedy.
func RejectIdentitySet(gh GitHubConfig) error {
	issues := IdentitySetIssues(gh)
	if len(issues) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrIdentitySetMismatch, strings.Join(issues, "; "))
}

// displayOrEmpty renders an empty string as an explicit marker, so a report
// never shows a blank where a value is expected. An empty api_url is exactly
// the condition that caused the outage, so it must be visible, not invisible.
func displayOrEmpty(s string) string {
	if s == "" {
		return "<empty — defaults to api.github.com>"
	}
	return s
}
