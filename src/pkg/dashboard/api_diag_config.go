package dashboard

import (
	_ "embed"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/logread"
	"gopkg.in/yaml.v3"
)

// diagConfigValueKeys is the documented list of configuration keys known not
// to hold secrets (docs/admin-mcp.md). The effective configuration read shows
// a value only for these keys; every other key is shown only as set or not
// set. A "*" segment matches one map key (an agent name) and "[*]" any list
// index. Adding a key here is a security decision: it must never name a
// field that can carry a credential, a key or a URL with embedded login.
//
// The keys live in diag_config_value_keys.txt rather than in Go source so
// that naming a hive.yaml key here does not count as the Settings UI
// referencing it (see settings_parity_test.go).
//
//go:embed diag_config_value_keys.txt
var diagConfigValueKeysFile string

var diagConfigValueKeys = diagParseKeyList(diagConfigValueKeysFile)

// diagParseKeyList reads one key per line, ignoring blank lines and lines
// starting with "#".
func diagParseKeyList(text string) []string {
	var keys []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		keys = append(keys, line)
	}
	return keys
}

// diagEnvValueNames is the documented list of environment names known not to
// hold secrets. A name the log-read secret rule treats as secret
// (logread.SecretEnvName) is never shown, even if listed.
var diagEnvValueNames = []string{
	"HIVE_CONFIG",
	"HIVE_ADMIN_MCP_ENABLE_WRITES",
	"HIVE_ADMIN_MCP_PENDING_FILE",
	"HOME",
	"HOSTNAME",
	"LANG",
	"PATH",
	"TZ",
	"KUBERNETES_SERVICE_HOST",
	"KUBERNETES_SERVICE_PORT",
}

const (
	// diagConfigMaxKeys bounds the keys one read enumerates per layer.
	diagConfigMaxKeys = 20000
	// diagConfigMaxEnv bounds the environment names one read lists.
	diagConfigMaxEnv = 500
	// diagConfigValueBytes cuts a shown value.
	diagConfigValueBytes = 1024
	// diagConfigDefaultLayer names a key set by no layer file: a built-in
	// default or a value the loader derived.
	diagConfigDefaultLayer = "default"

	diagHiddenNotListed = "not_on_value_list"
	diagHiddenURLLogin  = "url_with_embedded_login"
)

var (
	diagIndexRE      = regexp.MustCompile(`\[\d+\]`)
	diagURLLoginRE   = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^/\s@]*@`)
	diagYAMLQuotedRE = regexp.MustCompile("`[^`]*`")
)

type diagConfigKey struct {
	Key        string   `json:"key"`
	Set        bool     `json:"set"`
	Layer      string   `json:"layer"`
	AlsoSetBy  []string `json:"also_set_by,omitempty"`
	ValueShown bool     `json:"value_shown"`
	Value      string   `json:"value,omitempty"`
	ValueCut   bool     `json:"value_cut,omitempty"`
	Hidden     string   `json:"value_hidden,omitempty"`
}

type diagEnvName struct {
	Name       string `json:"name"`
	Set        bool   `json:"set"`
	ValueShown bool   `json:"value_shown"`
	Value      string `json:"value,omitempty"`
	ValueCut   bool   `json:"value_cut,omitempty"`
	Hidden     string `json:"value_hidden,omitempty"`
}

type diagValueList struct {
	ConfigKeys []string `json:"config_keys"`
	EnvNames   []string `json:"env_names"`
}

type diagConfigProvenance struct {
	Layers              []layerInfo `json:"layers"`
	SeedPath            string      `json:"seed_path"`
	OverlayPath         string      `json:"overlay_path"`
	OverlayPresent      bool        `json:"overlay_present"`
	OverlayRejected     bool        `json:"overlay_rejected"`
	OverlayRejectReason string      `json:"overlay_reject_reason,omitempty"`
	LastGoodUsed        bool        `json:"last_good_used"`
	GitHubRatchetFired  bool        `json:"github_ratchet_fired"`
	IdentityIssues      []string    `json:"identity_issues,omitempty"`
}

type diagConfigResult struct {
	Type          string               `json:"type"`
	Read          string               `json:"read"`
	Note          string               `json:"note"`
	Keys          []diagConfigKey      `json:"keys"`
	KeyCount      int                  `json:"key_count"`
	KeysTotal     int                  `json:"keys_total"`
	Offset        int                  `json:"offset"`
	HasMore       bool                 `json:"has_more"`
	NextOffset    int                  `json:"next_offset,omitempty"`
	Env           []diagEnvName        `json:"env"`
	EnvTotal      int                  `json:"env_total"`
	EnvTruncated  bool                 `json:"env_truncated,omitempty"`
	ValueList     diagValueList        `json:"value_list"`
	Provenance    diagConfigProvenance `json:"provenance"`
	Redactions    []logread.Redaction  `json:"redactions"`
	ResultByteCap int                  `json:"result_byte_cap"`
}

const diagConfigNote = "Values are shown only for configuration keys and environment names on value_list, which lists keys known not to hold secrets; every other key or name is shown only as set or not set. Shown values are scrubbed as log lines are. The configuration file itself is never returned. layer names the layer that set each key (see provenance.layers); also_set_by lists lower layers it overrode."

// handleDiagConfig serves GET /api/diag/config?offset=: the effective
// configuration and its provenance, as computed for GET /api/config/provenance
// (the seed merged with the dashboard overlay). Owner only, with the owner
// role verified by this hive, matching that endpoint.
func (s *Server) handleDiagConfig(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			jsonError(w, "offset must be a whole number of at least 0", http.StatusBadRequest)
			return
		}
		offset = n
	}
	seedPath := "/etc/hive/hive.yaml"
	if envCfg := os.Getenv("HIVE_CONFIG"); envCfg != "" {
		seedPath = envCfg
	}
	seedYAML, err := os.ReadFile(seedPath)
	if err != nil {
		jsonError(w, "config file not found", http.StatusNotFound)
		return
	}
	overlayYAML, overlayErr := os.ReadFile(config.DashboardOverlayFile)
	if overlayErr != nil {
		overlayYAML = nil
	}
	merged, prov, err := config.MergeLayersYAML(seedYAML, overlayYAML)
	if err != nil {
		jsonError(w, "config is not parsable", http.StatusInternalServerError)
		return
	}
	res := buildDiagConfig(diagConfigInput{
		merged: merged, prov: prov, seedYAML: seedYAML, overlayYAML: overlayYAML,
		seedPath: seedPath, overlayPath: config.DashboardOverlayFile,
		environ: os.Environ(), secrets: s.diagSecrets(), offset: offset,
	})
	diagWriteJSON(w, res)
}

type diagConfigInput struct {
	merged                *config.Config
	prov                  *config.Provenance
	seedYAML, overlayYAML []byte
	seedPath, overlayPath string
	environ               []string
	secrets               *logread.SecretScrubber
	offset                int
}

func buildDiagConfig(in diagConfigInput) diagConfigResult {
	counts := map[string]int{}
	scrub := func(text string) string {
		out := logread.ScrubText(text, in.secrets)
		for k, n := range out.Counts {
			counts[k] += n
		}
		return out.Text
	}
	prov := in.prov
	if prov == nil {
		prov = config.NewProvenance()
	}
	res := diagConfigResult{
		Type: "config_effective", Read: "config_effective", Note: diagConfigNote, Offset: in.offset,
		ValueList:     diagValueList{ConfigKeys: diagConfigValueKeys, EnvNames: diagEnvValueNames},
		ResultByteCap: adminmcp.DiagResultByteCap,
		Provenance: diagConfigProvenance{
			Layers: describeLayers(), SeedPath: in.seedPath, OverlayPath: in.overlayPath,
			OverlayPresent: len(in.overlayYAML) > 0, OverlayRejected: prov.OverlayRejected,
			OverlayRejectReason: scrub(diagYAMLQuotedRE.ReplaceAllString(prov.OverlayRejectReason, "`…`")),
			LastGoodUsed:        prov.LastGoodUsed, GitHubRatchetFired: prov.GitHubRatchetFired,
		},
	}
	if in.merged != nil {
		res.Provenance.IdentityIssues = config.IdentitySetIssues(in.merged.GitHub)
	}
	keys := diagConfigKeys(in, prov, scrub)
	res.Env = diagEnvNames(in.environ, scrub)
	res.EnvTotal = len(res.Env)
	if len(res.Env) > diagConfigMaxEnv {
		res.Env, res.EnvTruncated = res.Env[:diagConfigMaxEnv], true
	}
	res.KeysTotal = len(keys)
	res.Keys = []diagConfigKey{}
	used := diagJSONSize(res) + 64
	for i := min(in.offset, len(keys)); i < len(keys); i++ {
		size := diagJSONSize(keys[i]) + 1
		if used+size > adminmcp.DiagResultByteCap-diagFitMarginBytes {
			res.HasMore, res.NextOffset = true, i
			break
		}
		used += size
		res.Keys = append(res.Keys, keys[i])
	}
	res.KeyCount = len(res.Keys)
	res.Redactions = logread.RedactionList(counts)
	return res
}

// diagConfigKeys lists every key any layer set, plus every non-default key of
// the effective configuration, sorted, each with the layer that set it.
func diagConfigKeys(in diagConfigInput, prov *config.Provenance, scrub func(string) string) []diagConfigKey {
	effective := map[string]string{}
	if in.merged != nil {
		if b, err := yaml.Marshal(in.merged); err == nil {
			effective = diagYAMLLeaves(b)
		}
	}
	seed := diagYAMLLeaves(in.seedYAML)
	overlay := diagYAMLLeaves(in.overlayYAML)
	overlayAccepted := len(in.overlayYAML) > 0 && !prov.OverlayRejected

	names := map[string]bool{}
	for k, v := range effective {
		if !diagZeroValue(v) || seed[k] != "" || overlay[k] != "" {
			names[k] = true
		}
	}
	for k := range seed {
		names[k] = true
	}
	for k := range overlay {
		names[k] = true
	}
	sorted := make([]string, 0, len(names))
	for k := range names {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)

	out := make([]diagConfigKey, 0, len(sorted))
	for _, key := range sorted {
		_, inEffective := effective[key]
		_, inSeed := seed[key]
		_, inOverlay := overlay[key]
		layer := config.LayerUnset.String()
		switch {
		case prov.LayerOf(key) != config.LayerUnset:
			layer = prov.LayerOf(key).String()
		case overlayAccepted && inOverlay:
			layer = config.LayerDashboardOverlay.String()
		case inSeed && (!overlayAccepted || prov.GitHubRatchetFired && strings.HasPrefix(key, "github.")):
			layer = config.LayerSeed.String()
		case inEffective:
			layer = diagConfigDefaultLayer
		}
		entry := diagConfigKey{Key: scrub(key), Set: layer != config.LayerUnset.String(), Layer: layer}
		if inSeed && layer != config.LayerSeed.String() {
			entry.AlsoSetBy = append(entry.AlsoSetBy, config.LayerSeed.String())
		}
		if inOverlay && layer != config.LayerDashboardOverlay.String() {
			entry.AlsoSetBy = append(entry.AlsoSetBy, config.LayerDashboardOverlay.String())
		}
		switch {
		case !entry.Set:
		case !diagConfigValueKey(key):
			entry.Hidden = diagHiddenNotListed
		default:
			entry.Value, entry.ValueCut, entry.Hidden = diagShownValue(effective[key], scrub)
			entry.ValueShown = entry.Hidden == ""
		}
		out = append(out, entry)
	}
	return out
}

// diagShownValue renders an allowlisted value: scrubbed, cut, and withheld
// if it is a URL carrying a login.
func diagShownValue(v string, scrub func(string) string) (string, bool, string) {
	if diagURLLoginRE.MatchString(v) {
		return "", false, diagHiddenURLLogin
	}
	v = scrub(v)
	if len(v) > diagConfigValueBytes {
		return logread.HeadText(v, diagConfigValueBytes), true, ""
	}
	return v, false, ""
}

// diagConfigValueKey reports whether key is on diagConfigValueKeys.
func diagConfigValueKey(key string) bool {
	keyParts := strings.Split(diagIndexRE.ReplaceAllString(key, "[*]"), ".")
	for _, pattern := range diagConfigValueKeys {
		if diagKeyMatch(strings.Split(pattern, "."), keyParts) {
			return true
		}
	}
	return false
}

// diagKeyMatch matches dotted key segments; a "*" pattern segment matches
// exactly one non-empty segment.
func diagKeyMatch(pattern, key []string) bool {
	if len(pattern) != len(key) {
		return false
	}
	for i, p := range pattern {
		if p == "*" && key[i] != "" {
			continue
		}
		if p != key[i] {
			return false
		}
	}
	return true
}

func diagZeroValue(v string) bool {
	switch v {
	case "", "0", "false", "null", "~", "[]", "{}":
		return true
	}
	return false
}

// diagYAMLLeaves flattens a YAML document to dotted leaf keys and scalar
// values ("a.b", "a.list[0]"). Empty maps and lists are not leaves. At most
// diagConfigMaxKeys leaves are read; an unparsable document has none.
func diagYAMLLeaves(data []byte) map[string]string {
	out := map[string]string{}
	if len(data) == 0 {
		return out
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil || len(doc.Content) == 0 {
		return out
	}
	var walk func(n *yaml.Node, prefix string)
	walk = func(n *yaml.Node, prefix string) {
		if len(out) >= diagConfigMaxKeys || n == nil {
			return
		}
		switch n.Kind {
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				key := n.Content[i].Value
				if prefix != "" {
					key = prefix + "." + key
				}
				walk(n.Content[i+1], key)
			}
		case yaml.SequenceNode:
			for i, child := range n.Content {
				walk(child, prefix+"["+strconv.Itoa(i)+"]")
			}
		case yaml.ScalarNode:
			if prefix != "" {
				out[prefix] = n.Value
			}
		case yaml.AliasNode:
			walk(n.Alias, prefix)
		}
	}
	walk(doc.Content[0], "")
	return out
}

// diagEnvNames lists every environment name, set, plus every listed name that
// is not set. Values are shown only for listed names that the secret rule
// does not claim.
func diagEnvNames(environ []string, scrub func(string) string) []diagEnvName {
	listed := map[string]bool{}
	for _, name := range diagEnvValueNames {
		listed[name] = true
	}
	seen := map[string]bool{}
	var out []diagEnvName
	for _, kv := range environ {
		name, value, _ := strings.Cut(kv, "=")
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		entry := diagEnvName{Name: scrub(name), Set: true}
		if listed[name] && !logread.SecretEnvName(name) {
			entry.Value, entry.ValueCut, entry.Hidden = diagShownValue(value, scrub)
			entry.ValueShown = entry.Hidden == ""
		} else {
			entry.Hidden = diagHiddenNotListed
		}
		out = append(out, entry)
	}
	for _, name := range diagEnvValueNames {
		if !seen[name] {
			out = append(out, diagEnvName{Name: name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
