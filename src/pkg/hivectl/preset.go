package hivectl

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// A contribution preset is a plain, credential-free document a project
// publishes (at a public HTTPS URL or as a local file) to recommend Hives and
// repository selections to contributors. It is a recommendation only: it grants
// nothing, carries no secrets, and is deliberately unrelated to the encrypted,
// credential-bearing profile bundle in profile_bundle.go.
const (
	// PresetVersion is the only preset schema major this hivectl understands.
	PresetVersion = 1
	// PresetAllCurrent selects the current membership of a Hive or hub. It
	// never opts into future additions; that is a personal setting.
	PresetAllCurrent = "all-current"

	// MaxPresetBytes caps a preset document, local or fetched.
	MaxPresetBytes = 256 << 10
	// MaxPresetEntries caps the total number of hive, repository and hub
	// entries in one preset.
	MaxPresetEntries = 1024

	presetFetchTimeout   = 30 * time.Second
	presetMaxRedirects   = 5
	presetMaxNameLen     = 200
	presetMaxDescLen     = 2000
	presetMaxRevisionLen = 200
)

// Forge kinds a preset repository may name. They mirror forge.Kind in
// pkg/forge (pinned by TestPresetForgeKindsMatchForgePackage) without making
// hivectl depend on the forge adapters.
const (
	presetForgeGitHub = "github"
	presetForgeGitLab = "gitlab"
	presetForgeGitea  = "gitea"
)

// Preset is a parsed and validated contribution preset document.
type Preset struct {
	Version     int          `yaml:"version" json:"version"`
	Name        string       `yaml:"name" json:"name"`
	Description string       `yaml:"description,omitempty" json:"description,omitempty"`
	Revision    string       `yaml:"revision,omitempty" json:"revision,omitempty"`
	Hives       []PresetHive `yaml:"hives" json:"hives"`
	Hubs        []PresetHub  `yaml:"hubs,omitempty" json:"hubs,omitempty"`
}

// PresetHive recommends one Hive and which of its repositories to contribute to.
type PresetHive struct {
	URL          string             `yaml:"url" json:"url"`
	Description  string             `yaml:"description,omitempty" json:"description,omitempty"`
	Repositories PresetRepoSelector `yaml:"repositories" json:"repositories"`
}

// PresetRepoSelector is either the literal "all-current" or an explicit list.
type PresetRepoSelector struct {
	AllCurrent bool         `json:"all_current,omitempty"`
	Repos      []PresetRepo `json:"repos,omitempty"`
}

// PresetRepo names one repository by its full forge identity.
type PresetRepo struct {
	Repo        string `yaml:"repo" json:"repo"`
	Forge       string `yaml:"forge,omitempty" json:"forge"`
	Host        string `yaml:"host,omitempty" json:"host"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// PresetHub recommends a hub and all or some of the Hives it advertises.
type PresetHub struct {
	URL         string             `yaml:"url" json:"url"`
	Description string             `yaml:"description,omitempty" json:"description,omitempty"`
	Hives       PresetHiveSelector `yaml:"hives" json:"hives"`
}

// PresetHiveSelector is either the literal "all-current" or a list of Hive URLs.
type PresetHiveSelector struct {
	AllCurrent bool     `json:"all_current,omitempty"`
	URLs       []string `json:"urls,omitempty"`
}

// Identity is the normalized {forge, host, owner/name} key used for set
// semantics. It is only meaningful after Validate.
func (r PresetRepo) Identity() string {
	return strings.ToLower(r.Forge) + "|" + strings.ToLower(r.Host) + "|" + strings.ToLower(r.Repo)
}

// UnmarshalYAML accepts exactly "all-current" or a sequence of repository
// mappings. The sequence is re-decoded strictly because yaml.v3 does not carry
// KnownFields into a nested Node.Decode.
func (s *PresetRepoSelector) UnmarshalYAML(node *yaml.Node) error {
	*s = PresetRepoSelector{}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" || node.Value != PresetAllCurrent {
			return fmt.Errorf("line %d: repositories must be %q or a list of repositories (got %q)", node.Line, PresetAllCurrent, node.Value)
		}
		s.AllCurrent = true
		return nil
	case yaml.SequenceNode:
		var repos []PresetRepo
		if err := strictDecodeNode(node, &repos); err != nil {
			return err
		}
		if repos == nil {
			repos = []PresetRepo{}
		}
		s.Repos = repos
		return nil
	default:
		return fmt.Errorf("line %d: repositories must be %q or a list of repositories", node.Line, PresetAllCurrent)
	}
}

// UnmarshalYAML accepts exactly "all-current" or a sequence of Hive URLs.
func (s *PresetHiveSelector) UnmarshalYAML(node *yaml.Node) error {
	*s = PresetHiveSelector{}
	switch node.Kind {
	case yaml.ScalarNode:
		if node.Tag != "!!str" || node.Value != PresetAllCurrent {
			return fmt.Errorf("line %d: hub hives must be %q or a list of Hive URLs (got %q)", node.Line, PresetAllCurrent, node.Value)
		}
		s.AllCurrent = true
		return nil
	case yaml.SequenceNode:
		var urls []string
		if err := strictDecodeNode(node, &urls); err != nil {
			return err
		}
		if urls == nil {
			urls = []string{}
		}
		s.URLs = urls
		return nil
	default:
		return fmt.Errorf("line %d: hub hives must be %q or a list of Hive URLs", node.Line, PresetAllCurrent)
	}
}

func strictDecodeNode(node *yaml.Node, out any) error {
	raw, err := yaml.Marshal(node)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	return dec.Decode(out)
}

// presetDeniedKeys are fields a preset must never carry. Strict decoding
// already rejects them as unknown; this list exists so the error names the
// field and says why instead of a bare "field not found".
var presetDeniedKeys = map[string]string{
	"token":                        "credentials",
	"tokens":                       "credentials",
	"registration_token":           "credentials",
	"access_token":                 "credentials",
	"api_key":                      "credentials",
	"apikey":                       "credentials",
	"secret":                       "credentials",
	"secrets":                      "credentials",
	"password":                     "credentials",
	"passphrase":                   "credentials",
	"credential":                   "credentials",
	"credentials":                  "credentials",
	"auth":                         "credentials",
	"authorization":                "credentials",
	"cookie":                       "credentials",
	"cookies":                      "credentials",
	"contributor_id":               "credentials",
	"session":                      "local execution settings",
	"env":                          "local execution settings",
	"environment":                  "local execution settings",
	"command":                      "shell commands",
	"commands":                     "shell commands",
	"cmd":                          "shell commands",
	"exec":                         "shell commands",
	"run":                          "shell commands",
	"script":                       "shell commands",
	"scripts":                      "shell commands",
	"shell":                        "shell commands",
	"hook":                         "shell commands",
	"hooks":                        "shell commands",
	"backend":                      "local execution settings",
	"backends":                     "local execution settings",
	"model":                        "local execution settings",
	"models":                       "local execution settings",
	"provider":                     "local execution settings",
	"providers":                    "local execution settings",
	"strategy":                     "local execution settings",
	"commons_strategy":             "local execution settings",
	"quota":                        "local execution settings",
	"permission":                   "permission changes",
	"permissions":                  "permission changes",
	"allow":                        "permission changes",
	"allowed_tools":                "permission changes",
	"allowedtools":                 "permission changes",
	"tools":                        "permission changes",
	"sandbox":                      "permission changes",
	"trust":                        "permission changes",
	"trust_tier":                   "permission changes",
	"auto_approve":                 "permission changes",
	"autoapprove":                  "permission changes",
	"dangerously_skip_permissions": "permission changes",
}

// presetDeniedKeyReason classifies a key as forbidden, matching exact names and
// any key that obviously carries a secret or permission grant.
func presetDeniedKeyReason(key string) (string, bool) {
	k := strings.ToLower(strings.TrimSpace(key))
	k = strings.NewReplacer("-", "_", ".", "_", " ", "_").Replace(k)
	if reason, ok := presetDeniedKeys[k]; ok {
		return reason, true
	}
	for _, sub := range []string{"token", "secret", "password", "credential", "api_key", "private_key"} {
		if strings.Contains(k, sub) {
			return "credentials", true
		}
	}
	if strings.Contains(k, "permission") {
		return "permission changes", true
	}
	return "", false
}

// checkPresetDeniedKeys walks every mapping key in the document and refuses
// the first forbidden one, naming its path.
func checkPresetDeniedKeys(node *yaml.Node, path string) error {
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for i, child := range node.Content {
			p := path
			if node.Kind == yaml.SequenceNode {
				p = fmt.Sprintf("%s[%d]", path, i)
			}
			if err := checkPresetDeniedKeys(child, p); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			field := key.Value
			if path != "" {
				field = path + "." + key.Value
			}
			if reason, denied := presetDeniedKeyReason(key.Value); denied {
				return fmt.Errorf("preset field %q is not allowed: a contribution preset may not carry %s (line %d)", field, reason, key.Line)
			}
			if err := checkPresetDeniedKeys(node.Content[i+1], field); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return fmt.Errorf("preset may not use YAML aliases (line %d)", node.Line)
	}
	return nil
}

// ParsePreset decodes and validates a contribution preset document. Unknown
// fields, forbidden fields, unknown schema majors and unsafe URLs are errors.
func ParsePreset(data []byte) (*Preset, error) {
	if len(data) > MaxPresetBytes {
		return nil, fmt.Errorf("preset is %d bytes, larger than the %d byte limit", len(data), MaxPresetBytes)
	}
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("preset is not valid YAML: %w", err)
	}
	if root.Kind != yaml.DocumentNode || len(root.Content) != 1 || root.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("preset must be a single YAML mapping document")
	}
	if err := checkPresetDeniedKeys(&root, ""); err != nil {
		return nil, err
	}
	// Check the version before strict decoding so a newer document is refused
	// as "unsupported version" rather than with a confusing unknown-field error.
	if err := checkPresetVersionNode(root.Content[0]); err != nil {
		return nil, err
	}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var p Preset
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("preset: %w", err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("preset must contain exactly one YAML document")
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func checkPresetVersionNode(m *yaml.Node) error {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value != "version" {
			continue
		}
		v := m.Content[i+1]
		if v.Kind != yaml.ScalarNode || v.Tag != "!!int" {
			return fmt.Errorf("preset version must be an integer (got %q)", v.Value)
		}
		if v.Value != fmt.Sprint(PresetVersion) {
			return fmt.Errorf("preset version %s is not supported (this hivectl understands version %d); upgrade hivectl", v.Value, PresetVersion)
		}
		return nil
	}
	return fmt.Errorf("preset has no version; expected version: %d", PresetVersion)
}

// Validate enforces the preset rules and normalizes the document in place:
// forge and host defaults are filled in and duplicate repositories and hub
// Hive URLs are collapsed with set semantics.
func (p *Preset) Validate() error {
	if p.Version != PresetVersion {
		return fmt.Errorf("preset version %d is not supported (this hivectl understands version %d)", p.Version, PresetVersion)
	}
	if err := validatePresetText("name", p.Name, presetMaxNameLen, true); err != nil {
		return err
	}
	if err := validatePresetText("description", p.Description, presetMaxDescLen, false); err != nil {
		return err
	}
	if err := validatePresetText("revision", p.Revision, presetMaxRevisionLen, false); err != nil {
		return err
	}
	if len(p.Hives) == 0 && len(p.Hubs) == 0 {
		return errors.New("preset recommends nothing: add at least one entry under hives or hubs")
	}

	entries := 0
	seenHives := map[string]bool{}
	for i := range p.Hives {
		h := &p.Hives[i]
		field := fmt.Sprintf("hives[%d]", i)
		norm, err := validatePresetURL(field+".url", h.URL)
		if err != nil {
			return err
		}
		if seenHives[norm] {
			return fmt.Errorf("%s.url %q is listed more than once; merge the entries", field, h.URL)
		}
		seenHives[norm] = true
		if err := validatePresetText(field+".description", h.Description, presetMaxDescLen, false); err != nil {
			return err
		}
		if !h.Repositories.AllCurrent && h.Repositories.Repos == nil {
			return fmt.Errorf("%s.repositories is required: use %q or a list of repositories", field, PresetAllCurrent)
		}
		if h.Repositories.AllCurrent && len(h.Repositories.Repos) > 0 {
			return fmt.Errorf("%s.repositories cannot be both %q and a list", field, PresetAllCurrent)
		}
		entries++
		seenRepos := map[string]bool{}
		repos := make([]PresetRepo, 0, len(h.Repositories.Repos))
		for j, r := range h.Repositories.Repos {
			rf := fmt.Sprintf("%s.repositories[%d]", field, j)
			if err := normalizePresetRepo(rf, &r); err != nil {
				return err
			}
			id := r.Identity()
			if seenRepos[id] {
				continue
			}
			seenRepos[id] = true
			repos = append(repos, r)
			entries++
		}
		if h.Repositories.Repos != nil {
			h.Repositories.Repos = repos
		}
		if entries > MaxPresetEntries {
			return fmt.Errorf("preset has more than %d entries", MaxPresetEntries)
		}
	}

	seenHubs := map[string]bool{}
	for i := range p.Hubs {
		h := &p.Hubs[i]
		field := fmt.Sprintf("hubs[%d]", i)
		norm, err := validatePresetURL(field+".url", h.URL)
		if err != nil {
			return err
		}
		if seenHubs[norm] {
			return fmt.Errorf("%s.url %q is listed more than once; merge the entries", field, h.URL)
		}
		seenHubs[norm] = true
		if err := validatePresetText(field+".description", h.Description, presetMaxDescLen, false); err != nil {
			return err
		}
		if !h.Hives.AllCurrent && h.Hives.URLs == nil {
			return fmt.Errorf("%s.hives is required: use %q or a list of Hive URLs", field, PresetAllCurrent)
		}
		entries++
		seen := map[string]bool{}
		urls := make([]string, 0, len(h.Hives.URLs))
		for j, u := range h.Hives.URLs {
			n, err := validatePresetURL(fmt.Sprintf("%s.hives[%d]", field, j), u)
			if err != nil {
				return err
			}
			if seen[n] {
				continue
			}
			seen[n] = true
			urls = append(urls, u)
			entries++
		}
		if h.Hives.URLs != nil {
			h.Hives.URLs = urls
		}
		if entries > MaxPresetEntries {
			return fmt.Errorf("preset has more than %d entries", MaxPresetEntries)
		}
	}
	return nil
}

func validatePresetText(field, value string, maxLen int, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("preset %s is required", field)
	}
	if len(value) > maxLen {
		return fmt.Errorf("preset %s is longer than %d bytes", field, maxLen)
	}
	for _, r := range value {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return fmt.Errorf("preset %s may not contain control characters", field)
		}
	}
	return nil
}

// validatePresetURL accepts https URLs, and http only for loopback hosts. It
// returns a normalized form for duplicate detection.
func validatePresetURL(field, raw string) (string, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return "", fmt.Errorf("%s %q must be non-empty and free of surrounding whitespace", field, raw)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("%s %q is not a valid URL: %w", field, raw, err)
	}
	if u.Host == "" || u.Hostname() == "" {
		return "", fmt.Errorf("%s %q has no host", field, raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("%s %q may not embed credentials", field, raw)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return "", fmt.Errorf("%s %q must use https:// (http:// is allowed only for loopback hosts)", field, raw)
		}
	default:
		return "", fmt.Errorf("%s %q must use https:// (got %q)", field, raw, u.Scheme)
	}
	norm := strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.EscapedPath(), "/")
	if u.RawQuery != "" {
		norm += "?" + u.RawQuery
	}
	return norm, nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func presetForgeDefaultHost(kind string) string {
	switch kind {
	case presetForgeGitHub:
		return "github.com"
	case presetForgeGitLab:
		return "gitlab.com"
	}
	return ""
}

// normalizePresetRepo fills forge/host defaults and refuses anything that is
// not a full owner/name on a known forge.
func normalizePresetRepo(field string, r *PresetRepo) error {
	if r.Forge == "" {
		r.Forge = presetForgeGitHub
	}
	kind := strings.ToLower(r.Forge)
	switch kind {
	case presetForgeGitHub, presetForgeGitLab, presetForgeGitea:
	default:
		return fmt.Errorf("%s.forge %q is not a known forge (use %s, %s or %s)", field, r.Forge, presetForgeGitHub, presetForgeGitLab, presetForgeGitea)
	}
	r.Forge = kind

	if r.Host == "" {
		r.Host = presetForgeDefaultHost(kind)
		if r.Host == "" {
			return fmt.Errorf("%s.host is required for forge %q", field, kind)
		}
	}
	if err := validatePresetHost(r.Host); err != nil {
		return fmt.Errorf("%s.host %q: %w", field, r.Host, err)
	}
	r.Host = strings.ToLower(r.Host)

	if err := validatePresetRepoName(r.Repo, kind); err != nil {
		return fmt.Errorf("%s.repo %q: %w", field, r.Repo, err)
	}
	return validatePresetText(field+".description", r.Description, presetMaxDescLen, false)
}

func validatePresetHost(host string) error {
	errBare := errors.New("must be a bare host name, optionally with :port")
	if host == "" || strings.ContainsAny(host, "/@?#[] \t") {
		return errBare
	}
	name := host
	if strings.Contains(host, ":") {
		h, port, err := net.SplitHostPort(host)
		if err != nil || h == "" || port == "" {
			return errBare
		}
		for _, r := range port {
			if r < '0' || r > '9' {
				return errBare
			}
		}
		name = h
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return errBare
		}
	}
	return nil
}

// validatePresetRepoName requires a full owner/name path. GitLab also allows
// nested groups (group/subgroup/name). Display names are refused outright.
func validatePresetRepoName(repo string, kind string) error {
	parts := strings.Split(repo, "/")
	if len(parts) < 2 {
		return errors.New("must be a full owner/name repository, not a display name")
	}
	if len(parts) > 2 && kind != presetForgeGitLab {
		return errors.New("must be exactly owner/name")
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return errors.New("must be a full owner/name repository, not a display name")
		}
		for _, r := range part {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			default:
				return errors.New("must be a full owner/name repository, not a display name")
			}
		}
	}
	return nil
}

// LoadPreset reads a preset from an https:// URL or a local file path and
// returns it parsed and validated. Every other scheme (including file://) is
// refused. Nothing is written anywhere.
func LoadPreset(ctx context.Context, source string) (*Preset, error) {
	var transport http.RoundTripper
	if t, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = t.Clone()
	}
	return loadPreset(ctx, source, newPresetHTTPClient(transport))
}

func loadPreset(ctx context.Context, source string, client *http.Client) (*Preset, error) {
	if source == "" {
		return nil, errors.New("preset source is empty")
	}
	if i := strings.Index(source, "://"); i >= 0 {
		scheme := strings.ToLower(source[:i])
		if scheme != "https" && scheme != "http" {
			return nil, fmt.Errorf("preset source %q: only https:// URLs and local file paths are supported (got %q)", source, scheme)
		}
		data, err := fetchPreset(ctx, source, client)
		if err != nil {
			return nil, err
		}
		return ParsePreset(data)
	}
	data, err := readPresetFile(source)
	if err != nil {
		return nil, err
	}
	return ParsePreset(data)
}

// newPresetHTTPClient builds the client used to fetch presets: bounded
// timeout, no cookie jar, and a redirect policy that never leaves https.
func newPresetHTTPClient(transport http.RoundTripper) *http.Client {
	return &http.Client{
		Timeout:   presetFetchTimeout,
		Transport: transport,
		Jar:       nil,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= presetMaxRedirects {
				return fmt.Errorf("preset fetch stopped after %d redirects", presetMaxRedirects)
			}
			if strings.EqualFold(via[0].URL.Scheme, "https") && !strings.EqualFold(req.URL.Scheme, "https") {
				return fmt.Errorf("preset fetch refused a redirect from https to %s://%s", req.URL.Scheme, req.URL.Host)
			}
			if _, err := validatePresetURL("redirect target", req.URL.String()); err != nil {
				return fmt.Errorf("preset fetch refused a redirect: %w", err)
			}
			req.Header.Del("Authorization")
			req.Header.Del("Cookie")
			return nil
		},
	}
}

func fetchPreset(ctx context.Context, source string, client *http.Client) ([]byte, error) {
	if _, err := validatePresetURL("preset source", source); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return nil, fmt.Errorf("preset source %q: %w", source, err)
	}
	req.Header.Set("Accept", "application/yaml, text/yaml, text/plain;q=0.9, */*;q=0.1")
	req.Header.Set("User-Agent", "hivectl")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch preset %q: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch preset %q: HTTP %d", source, resp.StatusCode)
	}
	if resp.ContentLength > MaxPresetBytes {
		return nil, fmt.Errorf("preset %q is %d bytes, larger than the %d byte limit", source, resp.ContentLength, MaxPresetBytes)
	}
	return readPresetLimited(resp.Body, source)
}

func readPresetFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read preset: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("read preset: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("preset %q is not a regular file", path)
	}
	if info.Size() > MaxPresetBytes {
		return nil, fmt.Errorf("preset %q is %d bytes, larger than the %d byte limit", path, info.Size(), MaxPresetBytes)
	}
	return readPresetLimited(f, path)
}

func readPresetLimited(r io.Reader, source string) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxPresetBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read preset %q: %w", source, err)
	}
	if len(data) > MaxPresetBytes {
		return nil, fmt.Errorf("preset %q is larger than the %d byte limit", source, MaxPresetBytes)
	}
	return data, nil
}
