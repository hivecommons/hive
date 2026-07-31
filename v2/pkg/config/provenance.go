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
	"sort"
	"strconv"
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
// This function only DETECTS and NAMES the invalid state — it changes no
// behaviour and rejects nothing. Surfacing it in provenance is what makes the
// split visible at a glance instead of via a 404 in agent logs.
func IdentitySetIssues(gh GitHubConfig) []string {
	var issues []string
	apiIsGHE := containsFold(gh.APIURL, gheAPIURLMarker)
	baseIsGHE := gh.BaseURL != "" && !containsFold(gh.BaseURL, "github.com")
	slugIsGHE := containsFold(gh.AppSlug, "ghe")

	// The live regression: an App is configured and the forge markers
	// disagree with each other.
	if gh.AppID != 0 {
		if slugIsGHE && !apiIsGHE {
			issues = append(issues, "app_slug looks like a GitHub Enterprise slug ("+gh.AppSlug+
				") but api_url is not a GHE API URL ("+displayOrEmpty(gh.APIURL)+
				") — a GHE App ID presented to api.github.com returns 404 Integration not found")
		}
		if baseIsGHE && !apiIsGHE {
			issues = append(issues, "base_url is "+gh.BaseURL+" but api_url is "+
				displayOrEmpty(gh.APIURL)+" — base_url and api_url must name the same forge")
		}
		if apiIsGHE && gh.BaseURL != "" && !baseIsGHE {
			issues = append(issues, "api_url is a GHE API URL but base_url is "+gh.BaseURL+
				" — base_url and api_url must name the same forge")
		}
	}
	return issues
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
